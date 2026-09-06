package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const agentReleaseRepository = "chanhui800/Meridian"

// AgentBinaryManifest describes the immutable release asset an Agent should
// download. The URL is pinned to the controller's release version; it never
// follows the mutable GitHub "latest" alias.
type AgentBinaryManifest struct {
	Version     string `json:"version"`
	Platform    string `json:"platform"`
	DownloadURL string `json:"download_url"`
	SHA256      string `json:"sha256"`
}

var agentReleaseChecksums = struct {
	sync.Mutex
	version string
	values  map[string]string
}{values: make(map[string]string)}

var agentReleaseChecksumFlight singleflight.Group

func agentReleaseAssetName(platform string) (string, error) {
	switch normalizeAgentPlatform(platform) {
	case "linux/amd64":
		return "meridian-agent-linux-amd64", nil
	case "linux/arm64":
		return "meridian-agent-linux-arm64", nil
	default:
		return "", errors.New("unsupported agent platform")
	}
}

func validAgentReleaseVersion(version string) bool {
	if !strings.HasPrefix(version, "v") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func agentReleaseDownloadURL(version, asset string) string {
	return "https://github.com/" + agentReleaseRepository + "/releases/download/" + url.PathEscape(version) + "/" + url.PathEscape(asset)
}

func validateAgentReleaseDownloadURL(raw, version, asset string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("agent release URL must be an HTTPS GitHub URL")
	}
	expected := "/" + agentReleaseRepository + "/releases/download/" + version + "/" + asset
	if path.Clean(parsed.EscapedPath()) != expected {
		return errors.New("agent release URL is not pinned to the expected asset")
	}
	return nil
}

func parseAgentReleaseChecksums(data io.Reader) (map[string]string, error) {
	values := make(map[string]string)
	scanner := bufio.NewScanner(io.LimitReader(data, 1<<20))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || len(fields[0]) != 64 {
			continue
		}
		digest := strings.ToLower(fields[0])
		valid := true
		for _, r := range digest {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		values[name] = digest
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func fetchAgentReleaseChecksums(ctx context.Context, version string) (map[string]string, error) {
	if !validAgentReleaseVersion(version) {
		return nil, errors.New("agent release manifest is unavailable for a development build")
	}
	agentReleaseChecksums.Lock()
	if agentReleaseChecksums.version == version && len(agentReleaseChecksums.values) > 0 {
		values := make(map[string]string, len(agentReleaseChecksums.values))
		for key, value := range agentReleaseChecksums.values {
			values[key] = value
		}
		agentReleaseChecksums.Unlock()
		return values, nil
	}
	agentReleaseChecksums.Unlock()

	result, err, _ := agentReleaseChecksumFlight.Do(version, func() (any, error) {
		// Re-check after joining the singleflight group; another caller may
		// have completed the immutable release lookup while we were waiting.
		agentReleaseChecksums.Lock()
		if agentReleaseChecksums.version == version && len(agentReleaseChecksums.values) > 0 {
			values := make(map[string]string, len(agentReleaseChecksums.values))
			for key, value := range agentReleaseChecksums.values {
				values[key] = value
			}
			agentReleaseChecksums.Unlock()
			return values, nil
		}
		agentReleaseChecksums.Unlock()

		requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		endpoint := agentReleaseDownloadURL(version, "SHA256SUMS")
		request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			if req.URL.Scheme != "https" {
				return errors.New("refusing non-HTTPS GitHub redirect")
			}
			return nil
		}}
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GitHub checksum manifest returned %s", response.Status)
		}
		values, err := parseAgentReleaseChecksums(response.Body)
		if err != nil {
			return nil, err
		}
		if _, ok := values["meridian-agent-linux-amd64"]; !ok {
			return nil, errors.New("GitHub checksum manifest has no amd64 Agent")
		}
		if _, ok := values["meridian-agent-linux-arm64"]; !ok {
			return nil, errors.New("GitHub checksum manifest has no arm64 Agent")
		}
		agentReleaseChecksums.Lock()
		agentReleaseChecksums.version = version
		agentReleaseChecksums.values = values
		agentReleaseChecksums.Unlock()
		return values, nil
	})
	if err != nil {
		// A successful lookup is immutable for this Controller release. Keep
		// serving it if a later caller somehow observes a transient fetch error.
		agentReleaseChecksums.Lock()
		if agentReleaseChecksums.version == version && len(agentReleaseChecksums.values) > 0 {
			values := make(map[string]string, len(agentReleaseChecksums.values))
			for key, value := range agentReleaseChecksums.values {
				values[key] = value
			}
			agentReleaseChecksums.Unlock()
			return values, nil
		}
		agentReleaseChecksums.Unlock()
		return nil, err
	}
	values, ok := result.(map[string]string)
	if !ok {
		return nil, errors.New("invalid cached Agent release manifest")
	}
	return values, nil
}

func agentReleaseManifestForPlatform(ctx context.Context, platform string) (AgentBinaryManifest, error) {
	normalized := normalizeAgentPlatform(platform)
	asset, err := agentReleaseAssetName(normalized)
	if err != nil {
		return AgentBinaryManifest{}, err
	}
	version := strings.TrimSpace(appVersion)
	if !validAgentReleaseVersion(version) {
		return AgentBinaryManifest{}, errors.New("agent release manifest is unavailable for a development build")
	}
	values, err := fetchAgentReleaseChecksums(ctx, version)
	if err != nil {
		return AgentBinaryManifest{}, err
	}
	manifest := AgentBinaryManifest{Version: version, Platform: normalized, DownloadURL: agentReleaseDownloadURL(version, asset), SHA256: values[asset]}
	if len(manifest.SHA256) != 64 {
		return AgentBinaryManifest{}, errors.New("agent release checksum is unavailable")
	}
	if err := validateAgentReleaseDownloadURL(manifest.DownloadURL, version, asset); err != nil {
		return AgentBinaryManifest{}, err
	}
	return manifest, nil
}
