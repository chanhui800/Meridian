package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type edgeAgentUpdateTransportFunc func(*http.Request) (*http.Response, error)

func (transport edgeAgentUpdateTransportFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestEdgeAgentUpdateDownloadCredentialIsolation(t *testing.T) {
	const controller = "https://controller.example.test:9443/base"
	const syntheticToken = "synthetic-agent-credential"
	intercepted := errors.New("intercepted download")
	for _, test := range []struct {
		name       string
		controller string
		download   string
		wantAuth   bool
		wantCall   bool
	}{
		{
			name:       "public release",
			controller: controller,
			download:   agentReleaseDownloadURL("v9.9.9", "meridian-agent-linux-amd64"),
			wantCall:   true,
		},
		{
			name:       "protected Controller binary",
			controller: controller,
			download:   controller + "/api/agent/binary",
			wantAuth:   true,
			wantCall:   true,
		},
		{
			name:       "canonical Controller origin",
			controller: "https://CONTROLLER.example.test:443/base",
			download:   "https://controller.example.test/base/api/agent/binary",
			wantAuth:   true,
			wantCall:   true,
		},
		{
			name:       "binary endpoint prefix impostor",
			controller: controller,
			download:   controller + "/api/agent/binary-other",
		},
		{
			name:       "binary endpoint query",
			controller: controller,
			download:   controller + "/api/agent/binary?target=other",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			client := &http.Client{Transport: edgeAgentUpdateTransportFunc(func(request *http.Request) (*http.Response, error) {
				called = true
				authorization := request.Header.Get("Authorization")
				if test.wantAuth && authorization != "Bearer "+syntheticToken {
					t.Error("protected Controller request lost its credential")
				}
				if !test.wantAuth && authorization != "" {
					t.Error("public or invalid download received a Controller credential")
				}
				return nil, intercepted
			})}
			_, err := edgeDownloadAgentUpdate(context.Background(), client, test.controller, syntheticToken, test.download, "v9.9.9", "linux/amd64")
			if called != test.wantCall {
				t.Fatalf("download attempted = %t, want %t", called, test.wantCall)
			}
			if test.wantCall && !errors.Is(err, intercepted) {
				t.Fatal("valid download did not reach its transport")
			}
			if !test.wantCall && err == nil {
				t.Fatal("invalid download was accepted")
			}
		})
	}
}

func TestEdgeAgentUpdateRedirectCredentialIsolation(t *testing.T) {
	const controller = "https://controller.example.test:9443/base"
	for _, redirectedURL := range []string{
		"https://downloads.controller.example.test:9443/agent",
		"https://controller.example.test:10443/agent",
	} {
		t.Run(redirectedURL, func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: edgeAgentUpdateTransportFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				if requests == 1 {
					if request.Header.Get("Authorization") == "" {
						t.Error("protected Controller request has no credential")
					}
					return &http.Response{
						StatusCode: http.StatusFound,
						Header:     http.Header{"Location": []string{redirectedURL}},
						Body:       io.NopCloser(strings.NewReader("")),
						Request:    request,
					}, nil
				}
				if request.Header.Get("Authorization") != "" {
					t.Error("redirect to a different origin received a Controller credential")
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("synthetic Agent binary")),
					Request:    request,
				}, nil
			})}
			response, err := edgeDownloadAgentUpdate(context.Background(), client, controller, "synthetic-agent-credential", controller+"/api/agent/binary", "v9.9.9", "linux/amd64")
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			if requests != 2 {
				t.Fatalf("download requests = %d, want 2", requests)
			}
		})
	}
}

func edgeUpdateTestConfig() AgentRuntimeConfig {
	return AgentRuntimeConfig{
		ConfigHash: "synthetic-approved-config", ConfigRevision: 7, NodeGUID: "synthetic-node",
		AgentLeaseID: "synthetic-lease", AgentVersion: "v9.9.9", AgentSHA256: strings.Repeat("a", 64),
		AgentDownloadURL: agentReleaseDownloadURL("v9.9.9", "meridian-agent-linux-amd64"),
	}
}

func waitEdgeUpdateResult(t *testing.T, worker *edgeAgentUpdateWorker) edgeAgentUpdateResult {
	t.Helper()
	select {
	case result := <-worker.results:
		return result
	case <-time.After(time.Second):
		t.Fatal("Agent update worker did not finish")
		return edgeAgentUpdateResult{}
	}
}

func TestEdgeAgentUpdateWorkerDoesNotBlockOrOverlap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan time.Time, 1)
	var calls atomic.Int64
	worker := newEdgeAgentUpdateWorker(ctx, func(attemptCtx context.Context, _ AgentRuntimeConfig) (*edgeAgentUpdate, error) {
		calls.Add(1)
		deadline, _ := attemptCtx.Deadline()
		entered <- deadline
		<-attemptCtx.Done()
		return nil, attemptCtx.Err()
	})
	defer worker.close()
	config := edgeUpdateTestConfig()
	worker.allow(config, 3)
	startedAt := time.Now()
	returned := make(chan bool, 1)
	go func() { returned <- worker.start(startedAt) }()
	deadline := <-entered
	select {
	case started := <-returned:
		if !started {
			t.Fatal("approved update did not start")
		}
	case <-time.After(time.Second):
		cancel()
		<-returned
		t.Fatal("blocked download prevented the supervisor's next step")
	}
	if deadline.IsZero() || deadline.After(startedAt.Add(edgeAgentUpdateTimeout+time.Second)) {
		t.Fatal("update attempt has no bounded deadline")
	}
	worker.allow(config, 3)
	if worker.start(startedAt.Add(time.Second)) {
		t.Fatal("a second download started while the first was blocked")
	}
	worker.invalidate()
	if worker.start(startedAt.Add(2 * time.Second)) {
		t.Fatal("invalidated configuration started an update")
	}
	update, err := worker.finish(waitEdgeUpdateResult(t, worker))
	if update != nil || err != nil {
		t.Fatal("cancelled attempt remained eligible for installation")
	}
	worker.allow(config, 3)
	if worker.start(startedAt.Add(time.Minute)) {
		t.Fatal("a same-identity refresh reset the update retry interval")
	}
	if calls.Load() != 1 {
		t.Fatalf("download attempts = %d, want 1", calls.Load())
	}
}

func TestEdgeAgentUpdateWorkerRequiresValidMetadata(t *testing.T) {
	var attempts atomic.Int64
	worker := newEdgeAgentUpdateWorker(context.Background(), func(context.Context, AgentRuntimeConfig) (*edgeAgentUpdate, error) {
		attempts.Add(1)
		return nil, nil
	})
	defer worker.close()
	if worker.start(time.Now()) {
		t.Fatal("update started without approved configuration")
	}
	config := edgeUpdateTestConfig()
	config.AgentSHA256 = strings.Repeat("z", 64)
	worker.allow(config, 3)
	if worker.start(time.Now()) || attempts.Load() != 0 {
		t.Fatal("invalid update metadata started a download")
	}
}

func TestEdgeAgentUpdateWorkerDiscardsSupersededDownload(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "agent")
	temporary := executable + ".download"
	if err := os.WriteFile(executable, []byte("previous binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temporary, []byte("obsolete download"), 0o600); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	entered := make(chan struct{})
	var calls atomic.Int64
	worker := newEdgeAgentUpdateWorker(context.Background(), func(context.Context, AgentRuntimeConfig) (*edgeAgentUpdate, error) {
		if calls.Add(1) == 1 {
			close(entered)
			// Model a download that completed just as cancellation arrived.
			<-release
			return &edgeAgentUpdate{executable: executable, temporary: temporary}, nil
		}
		return nil, nil
	})
	defer func() {
		releaseOnce.Do(func() { close(release) })
		worker.close()
	}()
	config := edgeUpdateTestConfig()
	worker.allow(config, 3)
	now := time.Now()
	if !worker.start(now) {
		t.Fatal("first approved download did not start")
	}
	<-entered
	config.ConfigRevision++
	config.AgentLeaseID = "synthetic-new-lease"
	worker.allow(config, 3)
	if worker.start(now.Add(time.Second)) {
		t.Fatal("new target overlapped an outstanding cancelled download")
	}
	releaseOnce.Do(func() { close(release) })
	update, err := worker.finish(waitEdgeUpdateResult(t, worker))
	if update != nil || err != nil {
		t.Fatal("obsolete update result was accepted")
	}
	if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("obsolete download was not removed")
	}
	contents, err := os.ReadFile(executable)
	if err != nil || string(contents) != "previous binary" {
		t.Fatal("background completion replaced the running target")
	}
	if !worker.start(now.Add(2 * time.Second)) {
		t.Fatal("new approved identity was blocked by the old target's retry interval")
	}
	_, _ = worker.finish(waitEdgeUpdateResult(t, worker))
}

func TestEdgeAgentUpdateWorkerCloseDiscardsUncommittedResult(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "agent")
	temporary := executable + ".download"
	if err := os.WriteFile(executable, []byte("previous binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temporary, []byte("prepared binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker := newEdgeAgentUpdateWorker(context.Background(), func(context.Context, AgentRuntimeConfig) (*edgeAgentUpdate, error) {
		return &edgeAgentUpdate{executable: executable, temporary: temporary}, nil
	})
	defer worker.close()
	worker.allow(edgeUpdateTestConfig(), 3)
	if !worker.start(time.Now()) {
		t.Fatal("approved update did not start")
	}
	select {
	case <-worker.done:
	case <-time.After(time.Second):
		t.Fatal("prepared result was not published")
	}
	worker.close()
	worker.close()
	if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("closing the worker left an uncommitted download")
	}
	contents, err := os.ReadFile(executable)
	if err != nil || string(contents) != "previous binary" {
		t.Fatal("closing the worker committed an update")
	}
}

func TestEdgeAgentUpdateWorkerPreservesRetryIntervalOnRefresh(t *testing.T) {
	worker := newEdgeAgentUpdateWorker(context.Background(), func(context.Context, AgentRuntimeConfig) (*edgeAgentUpdate, error) {
		return nil, errors.New("synthetic download failure")
	})
	defer worker.close()
	config := edgeUpdateTestConfig()
	worker.allow(config, 3)
	now := time.Now()
	worker.start(now)
	_, _ = worker.finish(waitEdgeUpdateResult(t, worker))
	config.AgentSHA256 = strings.ToUpper(config.AgentSHA256)
	worker.allow(config, 3)
	if worker.start(now.Add(time.Minute)) {
		t.Fatal("same target refresh started a premature retry")
	}
	if !worker.start(now.Add(edgeAgentUpdateRetryInterval)) {
		t.Fatal("update was not retried after the interval")
	}
	_, _ = worker.finish(waitEdgeUpdateResult(t, worker))
}

func TestEdgeAgentUpdateWorkerReturnsAuthenticationFailures(t *testing.T) {
	for _, failure := range []*edgeAPIError{
		{StatusCode: http.StatusUnauthorized, AgentState: "revoked"},
		{StatusCode: http.StatusConflict, AgentState: "stale"},
	} {
		t.Run(failure.AgentState, func(t *testing.T) {
			worker := newEdgeAgentUpdateWorker(context.Background(), func(context.Context, AgentRuntimeConfig) (*edgeAgentUpdate, error) {
				return nil, failure
			})
			defer worker.close()
			worker.allow(edgeUpdateTestConfig(), 3)
			worker.start(time.Now())
			result := waitEdgeUpdateResult(t, worker)
			worker.invalidate()
			_, err := worker.finish(result)
			if !errors.Is(err, failure) {
				t.Fatal("update authentication failure did not reach the supervisor")
			}
		})
	}
}

func TestEdgeAgentUpdateStagesBeforeSupervisorCommit(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "agent")
	if err := os.WriteFile(executable, []byte("previous binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	payload := []byte("verified replacement")
	digest := sha256.Sum256(payload)
	update, err := edgeStageAgentUpdate(context.Background(), executable, strings.NewReader(string(payload)), hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	defer update.discard()
	contents, err := os.ReadFile(executable)
	if err != nil || string(contents) != "previous binary" {
		t.Fatal("staging replaced the executable before supervisor approval")
	}
	synced := false
	update.syncParent = func(string) error {
		synced = true
		contents, err := os.ReadFile(executable)
		if err != nil || string(contents) != string(payload) {
			t.Error("directory sync ran before the verified replacement was committed")
		}
		return errors.New("synthetic directory sync failure")
	}
	if err := update.commit(context.Background()); !errors.Is(err, errEdgeAgentUpdated) {
		t.Fatal("committed replacement did not demand restart after a sync failure")
	}
	if !synced {
		t.Fatal("committed replacement skipped directory durability sync")
	}
}

func TestEdgeAgentUpdateCancelledCommitKeepsPreviousBinary(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "agent")
	temporary := executable + ".download"
	if err := os.WriteFile(executable, []byte("previous binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temporary, []byte("prepared binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	update := &edgeAgentUpdate{executable: executable, temporary: temporary}
	if err := update.commit(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled supervisor committed the executable")
	}
	if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled commit left a prepared download")
	}
	contents, err := os.ReadFile(executable)
	if err != nil || string(contents) != "previous binary" {
		t.Fatal("cancelled commit changed the previous executable")
	}
}

func TestEdgeAgentUpdateBadChecksumLeavesNoStagedFile(t *testing.T) {
	directory := t.TempDir()
	executable := filepath.Join(directory, "agent")
	update, err := edgeStageAgentUpdate(context.Background(), executable, strings.NewReader("unverified binary"), strings.Repeat("a", 64))
	if err == nil || update != nil {
		t.Fatal("bad checksum produced an installable update")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed verification left a staged executable")
	}
}
