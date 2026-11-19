package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"
)

const (
	edgeAgentUpdateTimeout       = 2 * time.Minute
	edgeAgentUpdateRetryInterval = 5 * time.Minute
	edgeAgentUpdateMaxBytes      = 128 << 20
)

func edgeControllerBinaryURL(controller string) (*url.URL, error) {
	target, err := url.Parse(strings.TrimSpace(controller))
	if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.Opaque != "" || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" {
		return nil, errors.New("invalid Controller binary URL")
	}
	target.Path = strings.TrimRight(target.Path, "/") + "/api/agent/binary"
	if target.RawPath != "" {
		target.RawPath = strings.TrimRight(target.RawPath, "/") + "/api/agent/binary"
	}
	return target, nil
}

func edgeDownloadAgentUpdate(ctx context.Context, client *http.Client, controller, token, downloadURL, version, platform string) (*http.Response, error) {
	asset, err := agentReleaseAssetName(platform)
	if err != nil {
		return nil, err
	}
	controllerBinary, err := edgeControllerBinaryURL(controller)
	if err != nil {
		return nil, err
	}
	target, err := url.Parse(strings.TrimSpace(downloadURL))
	if err != nil || target.Scheme != "https" || target.Hostname() == "" || target.Opaque != "" || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" {
		return nil, errors.New("invalid Agent download URL")
	}
	authenticated := sameRedirectAuthority(controllerBinary, target) && controllerBinary.EscapedPath() == target.EscapedPath()
	if err := validateAgentReleaseDownloadURL(downloadURL, strings.TrimSpace(version), asset); err != nil {
		if !authenticated {
			return nil, err
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	if authenticated {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set(agentPlatformHeader, platform)
	releaseClient := *client
	credentialsAllowed := authenticated
	releaseClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many Agent release redirects")
		}
		if req.URL.Scheme != "https" || req.URL.User != nil {
			return errors.New("refusing non-HTTPS Agent release redirect")
		}
		if !sameRedirectAuthority(controllerBinary, req.URL) {
			credentialsAllowed = false
		}
		if !credentialsAllowed {
			req.Header.Del("Authorization")
		}
		return nil
	}
	response, err := releaseClient.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, fmt.Errorf("Agent update returned %s", response.Status)
	}
	return response, nil
}

// edgeAgentUpdate is a verified, durable download. Only the supervisor may
// commit it; a background request cannot replace a revoked or superseded
// process's executable.
type edgeAgentUpdate struct {
	executable string
	temporary  string
	syncParent func(string) error
}

func (update *edgeAgentUpdate) discard() {
	if update != nil && update.temporary != "" {
		_ = os.Remove(update.temporary)
		update.temporary = ""
	}
}

func (update *edgeAgentUpdate) commit(ctx context.Context) error {
	if update == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		update.discard()
		return err
	}
	if err := os.Rename(update.temporary, update.executable); err != nil {
		update.discard()
		return err
	}
	update.temporary = ""
	// The rename has committed. Even if its durability sync fails, the
	// supervisor must restart instead of continuing with the old loaded code.
	syncParent := update.syncParent
	if syncParent == nil {
		syncParent = syncDirectory
	}
	if err := syncParent(filepath.Dir(update.executable)); err != nil {
		fmt.Fprintf(os.Stderr, "Meridian Agent update directory sync failed (replacement already applied): %v\n", err)
	}
	return errEdgeAgentUpdated
}

func edgePrepareAgentUpdate(ctx context.Context, client *http.Client, controller, token string, config AgentRuntimeConfig) (*edgeAgentUpdate, error) {
	if len(config.AgentSHA256) != sha256.Size*2 {
		return nil, nil
	}
	if _, err := hex.DecodeString(config.AgentSHA256); err != nil {
		return nil, errors.New("invalid Agent update checksum")
	}
	executable, current, err := edgeExecutableDigest()
	if err != nil || strings.EqualFold(current, config.AgentSHA256) {
		return nil, err
	}
	downloadURL := strings.TrimSpace(config.AgentDownloadURL)
	if downloadURL == "" {
		manifest, err := edgeFetchAgentManifest(ctx, client, controller, token)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(manifest.SHA256, config.AgentSHA256) || manifest.Version != strings.TrimSpace(config.AgentVersion) {
			return nil, errors.New("Agent release manifest differs from controller configuration")
		}
		downloadURL = manifest.DownloadURL
	}
	response, err := edgeDownloadAgentUpdate(ctx, client, controller, token, downloadURL, config.AgentVersion, goruntime.GOOS+"/"+goruntime.GOARCH)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.ContentLength > edgeAgentUpdateMaxBytes {
		return nil, errors.New("Agent update download is too large")
	}
	return edgeStageAgentUpdate(ctx, executable, response.Body, config.AgentSHA256)
}

func edgeStageAgentUpdate(ctx context.Context, executable string, body io.Reader, expectedChecksum string) (*edgeAgentUpdate, error) {
	temporary, err := os.CreateTemp(filepath.Dir(executable), ".meridian-agent-update-*") // #nosec G304 G703 -- directory of the current Agent executable selected by the kernel.
	if err != nil {
		return nil, err
	}
	update := &edgeAgentUpdate{executable: executable, temporary: temporary.Name()}
	prepared := false
	defer func() {
		_ = temporary.Close()
		if !prepared {
			update.discard()
		}
	}()
	digest := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, digest), io.LimitReader(body, edgeAgentUpdateMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("Agent update download failed: %w", err)
	}
	if written <= 0 || written > edgeAgentUpdateMaxBytes {
		return nil, errors.New("Agent update download has an invalid size")
	}
	if !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), expectedChecksum) {
		return nil, errors.New("Agent update checksum mismatch")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := temporary.Chmod(0o755); err != nil {
		return nil, err
	}
	if err := temporary.Sync(); err != nil {
		return nil, err
	}
	if err := temporary.Close(); err != nil {
		return nil, err
	}
	prepared = true
	return update, nil
}

type edgeAgentUpdateTarget struct {
	configHash     string
	configRevision int64
	nodeGUID       string
	leaseID        string
	sessionEpoch   int64
	version        string
	checksum       string
}

func agentUpdateTarget(config AgentRuntimeConfig, sessionEpoch int64) edgeAgentUpdateTarget {
	return edgeAgentUpdateTarget{
		configHash: strings.TrimSpace(config.ConfigHash), configRevision: config.ConfigRevision,
		nodeGUID: strings.TrimSpace(config.NodeGUID), leaseID: strings.TrimSpace(config.AgentLeaseID),
		sessionEpoch: sessionEpoch, version: strings.TrimSpace(config.AgentVersion),
		checksum: strings.ToLower(strings.TrimSpace(config.AgentSHA256)),
	}
}

type edgeAgentUpdateResult struct {
	generation uint64
	update     *edgeAgentUpdate
	err        error
}

// The supervisor owns this state. Its only background goroutine prepares one
// download and sends one result; it never mutates worker state or commits files.
type edgeAgentUpdateWorker struct {
	ctx         context.Context
	prepare     func(context.Context, AgentRuntimeConfig) (*edgeAgentUpdate, error)
	results     chan edgeAgentUpdateResult
	target      edgeAgentUpdateTarget
	config      AgentRuntimeConfig
	allowed     bool
	generation  uint64
	lastTarget  edgeAgentUpdateTarget
	lastAttempt time.Time
	cancel      context.CancelFunc
	done        chan struct{}
	closed      bool
}

func newEdgeAgentUpdateWorker(ctx context.Context, prepare func(context.Context, AgentRuntimeConfig) (*edgeAgentUpdate, error)) *edgeAgentUpdateWorker {
	return &edgeAgentUpdateWorker{ctx: ctx, prepare: prepare, results: make(chan edgeAgentUpdateResult, 1)}
}

func (worker *edgeAgentUpdateWorker) matches(config AgentRuntimeConfig, sessionEpoch int64) bool {
	return worker.allowed && worker.target == agentUpdateTarget(config, sessionEpoch)
}

func (worker *edgeAgentUpdateWorker) invalidate() {
	worker.allowed = false
	worker.generation++
	if worker.cancel != nil {
		worker.cancel()
	}
}

func (worker *edgeAgentUpdateWorker) allow(config AgentRuntimeConfig, sessionEpoch int64) {
	if worker.closed || len(config.AgentSHA256) != sha256.Size*2 || strings.TrimSpace(config.AgentVersion) == "" {
		worker.invalidate()
		return
	}
	if _, err := hex.DecodeString(config.AgentSHA256); err != nil {
		worker.invalidate()
		return
	}
	if !worker.matches(config, sessionEpoch) {
		worker.invalidate()
		worker.target = agentUpdateTarget(config, sessionEpoch)
	}
	// Retain only update metadata, never a route/TLS snapshot in the worker.
	worker.config = AgentRuntimeConfig{AgentVersion: config.AgentVersion, AgentSHA256: config.AgentSHA256, AgentDownloadURL: config.AgentDownloadURL}
	worker.allowed = true
}

func (worker *edgeAgentUpdateWorker) start(now time.Time) bool {
	if worker.closed || !worker.allowed || worker.done != nil || worker.ctx.Err() != nil {
		return false
	}
	if worker.target == worker.lastTarget && !worker.lastAttempt.IsZero() && now.Sub(worker.lastAttempt) < edgeAgentUpdateRetryInterval {
		return false
	}
	worker.lastTarget, worker.lastAttempt = worker.target, now
	attemptCtx, cancel := context.WithTimeout(worker.ctx, edgeAgentUpdateTimeout) // #nosec G118 -- supervisor finish/close release the attempt; invalidation also cancels it.
	worker.cancel = cancel
	done := make(chan struct{})
	worker.done = done
	generation, config := worker.generation, worker.config
	go func() {
		defer close(done)
		update, err := worker.prepare(attemptCtx, config)
		if attemptCtx.Err() != nil {
			update.discard()
			update = nil
			if err == nil {
				err = attemptCtx.Err()
			}
		}
		worker.results <- edgeAgentUpdateResult{generation: generation, update: update, err: err}
	}()
	return true
}

func (worker *edgeAgentUpdateWorker) finish(result edgeAgentUpdateResult) (*edgeAgentUpdate, error) {
	if worker.done != nil {
		<-worker.done
		worker.done = nil
	}
	if worker.cancel != nil {
		worker.cancel()
		worker.cancel = nil
	}
	if worker.closed || worker.ctx.Err() != nil {
		result.update.discard()
		return nil, nil
	}
	if !worker.allowed || result.generation != worker.generation {
		result.update.discard()
		if isEdgeAgentRevoked(result.err) || isEdgeAgentStale(result.err) {
			return nil, result.err
		}
		return nil, nil
	}
	if result.err != nil {
		result.update.discard()
		return nil, result.err
	}
	return result.update, nil
}

func (worker *edgeAgentUpdateWorker) close() {
	worker.closed = true
	worker.invalidate()
	if worker.done != nil {
		<-worker.done
		result := <-worker.results
		_, _ = worker.finish(result)
	}
}

func syncDirectory(path string) error {
	if goruntime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path) // #nosec G304 G703 -- parent directory of the Agent executable selected by Meridian.
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
