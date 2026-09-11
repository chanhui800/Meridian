package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

// agentInstallerScript is the single canonical installer served by the panel
// and published under scripts/. Keep the generated response and install
// command on the same implementation.
//
//go:embed scripts/agent-install.sh
var agentInstallerScript string

var agentBinaryIdentityCache struct {
	mu      sync.Mutex
	entries map[string]*agentBinaryIdentityEntry
}

type agentBinaryIdentityEntry struct {
	sync.Once
	version string
	digest  string
	err     error
}

func configuredAgentBinaryPath() string {
	executable := strings.TrimSpace(os.Getenv("MERIDIAN_AGENT_BINARY"))
	if executable == "" {
		// Docker sets MERIDIAN_AGENT_BINARY explicitly. For standalone installs,
		// prefer the installer path and retain the historical image fallback.
		if _, err := os.Stat("/usr/local/bin/meridian-agent"); err == nil {
			return "/usr/local/bin/meridian-agent"
		}
		executable = "/app/meridian-agent"
	}
	return executable
}

const (
	agentPlatformHeader = "X-Meridian-Agent-Platform"
	agentVersionHeader  = "X-Meridian-Agent-Version"
	agentSessionHeader  = "X-Meridian-Agent-Session"
	agentEpochHeader    = "X-Meridian-Agent-Session-Epoch"
)

func requestedAgentSession(r *http.Request) (agentSessionRequest, error) {
	if r == nil {
		return agentSessionRequest{}, nil
	}
	id := strings.TrimSpace(r.Header.Get(agentSessionHeader))
	epochText := strings.TrimSpace(r.Header.Get(agentEpochHeader))
	if id == "" && epochText == "" {
		return agentSessionRequest{}, nil
	}
	if id == "" || len(id) > 128 {
		return agentSessionRequest{}, errors.New("invalid agent session")
	}
	epoch, err := strconv.ParseInt(epochText, 10, 64)
	if err != nil || epoch <= 0 {
		return agentSessionRequest{}, errors.New("invalid agent session epoch")
	}
	return agentSessionRequest{ID: id, Epoch: epoch}, nil
}

func normalizeAgentPlatform(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "linux/amd64", "linux-amd64", "amd64", "x86_64":
		return "linux/amd64"
	case "linux/arm64", "linux-arm64", "arm64", "aarch64":
		return "linux/arm64"
	default:
		return ""
	}
}

func requestedAgentPlatform(r *http.Request) (string, error) {
	if r == nil {
		return "", nil
	}
	raw := strings.TrimSpace(r.Header.Get(agentPlatformHeader))
	if raw == "" {
		return "", nil
	}
	platform := normalizeAgentPlatform(raw)
	if platform == "" {
		return "", errors.New("unsupported agent platform")
	}
	return platform, nil
}

func configuredAgentBinaryPathForPlatform(platform string) (string, error) {
	platform = normalizeAgentPlatform(platform)
	if platform == "" {
		return configuredAgentBinaryPath(), nil
	}
	var envName string
	var candidates []string
	switch platform {
	case "linux/amd64":
		envName = "MERIDIAN_AGENT_BINARY_LINUX_AMD64"
		candidates = []string{"/app/meridian-agent-linux-amd64", "/usr/local/bin/meridian-agent-linux-amd64"}
	case "linux/arm64":
		envName = "MERIDIAN_AGENT_BINARY_LINUX_ARM64"
		candidates = []string{"/app/meridian-agent-linux-arm64", "/usr/local/bin/meridian-agent-linux-arm64"}
	}
	if configured := strings.TrimSpace(os.Getenv(envName)); configured != "" {
		return configured, nil
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	if platform == runtime.GOOS+"/"+runtime.GOARCH {
		return configuredAgentBinaryPath(), nil
	}
	return "", fmt.Errorf("agent binary for %s is unavailable", platform)
}

func agentBinaryIdentity() (string, string, error) {
	return agentBinaryIdentityForPlatform("")
}

func agentBinaryIdentityForPlatform(platform string) (string, string, error) {
	platform = normalizeAgentPlatform(platform)
	agentBinaryIdentityCache.mu.Lock()
	if agentBinaryIdentityCache.entries == nil {
		agentBinaryIdentityCache.entries = make(map[string]*agentBinaryIdentityEntry)
	}
	entry, ok := agentBinaryIdentityCache.entries[platform]
	if !ok {
		entry = &agentBinaryIdentityEntry{}
		agentBinaryIdentityCache.entries[platform] = entry
	}
	agentBinaryIdentityCache.mu.Unlock()
	entry.Do(func() {
		executable, pathErr := configuredAgentBinaryPathForPlatform(platform)
		if pathErr != nil {
			entry.err = pathErr
			return
		}
		file, err := os.Open(executable) // #nosec G304 -- fixed image path or administrator-controlled environment value.
		if err != nil {
			entry.err = err
			return
		}
		defer file.Close()
		digest := sha256.New()
		if _, err := io.Copy(digest, file); err != nil {
			entry.err = err
			return
		}
		entry.version = appVersion
		entry.digest = hex.EncodeToString(digest.Sum(nil))
	})
	return entry.version, entry.digest, entry.err
}

type nodeAPIInput struct {
	Name                     string `json:"name"`
	Address                  string `json:"address"`
	Port                     int    `json:"port"`
	HTTPSPort                int    `json:"https_port"` // Deprecated compatibility for cached pre-single-port pages.
	Enabled                  *bool  `json:"enabled"`
	Priority                 int    `json:"priority"`
	TrafficQuota             int64  `json:"traffic_quota"`
	TrafficManualOffsetBytes int64  `json:"traffic_manual_offset_bytes"`
	BillingMode              string `json:"billing_mode"`
	ResetDay                 int    `json:"reset_day"`
	ControllerURL            string `json:"controller_url"`
}

func nodeCreateInput(input nodeAPIInput) NodeCreateInput {
	port := input.Port
	if port == 0 {
		port = input.HTTPSPort
	}
	return NodeCreateInput{
		Name: input.Name, Address: input.Address, Port: port, Priority: input.Priority,
		TrafficQuota: input.TrafficQuota, BillingMode: input.BillingMode, ResetDay: input.ResetDay,
		TrafficManualOffsetBytes: input.TrafficManualOffsetBytes,
	}
}

func normalizeControllerURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.Scheme != "https" {
		return "", errors.New("controller_url must be an absolute HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("controller_url cannot contain credentials, query, or fragment")
	}
	if parsed.EscapedPath() != "" && parsed.EscapedPath() != "/" {
		return "", errors.New("controller_url must not contain a path")
	}
	if parsed.RawPath != "" && parsed.RawPath != "/" {
		return "", errors.New("controller_url must not contain a path")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func buildNodeInstallScript(controllerURL, enrollmentToken string) string {
	return buildNodeInstallScriptWithOptions(controllerURL, enrollmentToken, false)
}

func buildNodeInstallScriptWithOptions(controllerURL, enrollmentToken string, reenroll bool) string {
	controller := shellSingleQuote(controllerURL)
	token := shellSingleQuote(enrollmentToken)
	reenrollArg := ""
	if reenroll {
		reenrollArg = " --reenroll"
	}
	return fmt.Sprintf("#!/bin/sh\nset -eu\nexec /bin/sh -s -- -e %s -t %s%s <<'MERIDIAN_CANONICAL_INSTALLER'\n%sMERIDIAN_CANONICAL_INSTALLER\n", controller, token, reenrollArg, agentInstallerScript)
}

func buildNodeInstallCommand(controllerURL, enrollmentToken string) string {
	return buildNodeInstallCommandWithOptions(controllerURL, enrollmentToken, false)
}

func buildNodeInstallCommandWithOptions(controllerURL, enrollmentToken string, reenroll bool) string {
	endpoint := strings.TrimRight(controllerURL, "/") + "/api/agent/install.sh"
	reenrollArg := ""
	if reenroll {
		reenrollArg = " --reenroll"
	}
	return fmt.Sprintf("curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL %s | sudo bash -s -- -e %s -t %s%s",
		shellSingleQuote(endpoint),
		shellSingleQuote(controllerURL), shellSingleQuote(enrollmentToken), reenrollArg)
}

func (a *App) handleAgentInstaller(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, agentInstallerScript)
}

func writeNodeAPIError(a *App, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errNodeNotFound):
		a.jsonErr(w, http.StatusNotFound, "node not found")
	case errors.Is(err, errNodeNameConflict):
		a.jsonErr(w, http.StatusConflict, "node name already exists")
	case errors.Is(err, errManualNodeUnavailable):
		a.jsonErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, errPersistentJWTRequired):
		a.jsonErr(w, http.StatusConflict, "请先配置持久 JWT_SECRET，再创建或注册 Agent 节点")
	default:
		a.jsonErr(w, http.StatusBadRequest, err.Error())
	}
}

func (a *App) handleNodes(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Cache-Control", "no-store")
		snapshot, err := a.db.NodeControlSnapshot(now)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "node snapshot unavailable")
			return
		}
		a.jsonOK(w, snapshot)
	case http.MethodPost:
		var input nodeAPIInput
		if err := decodeJSONBody(w, r, &input); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid request")
			return
		}
		controllerURL, err := normalizeControllerURL(input.ControllerURL)
		if err != nil {
			writeNodeAPIError(a, w, err)
			return
		}
		node, token, err := a.db.CreateControlNode(nodeCreateInput(input), now)
		if err != nil {
			writeNodeAPIError(a, w, err)
			return
		}
		a.jsonOK(w, map[string]interface{}{
			"node": node, "install_script": buildNodeInstallScript(controllerURL, token),
			"install_command":          buildNodeInstallCommand(controllerURL, token),
			"enrollment_expires_at_ms": now.Add(nodeEnrollmentLifetime).UnixMilli(),
		})
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func nodeIDFromPath(path string) (int64, string, error) {
	path = strings.Trim(strings.TrimPrefix(path, "/api/nodes/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || len(parts) > 2 {
		return 0, "", errNodeNotFound
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		return 0, "", errNodeNotFound
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	return id, action, nil
}

func (a *App) handleNodeByID(w http.ResponseWriter, r *http.Request) {
	id, action, err := nodeIDFromPath(r.URL.Path)
	if err != nil {
		writeNodeAPIError(a, w, err)
		return
	}
	now := time.Now()
	if action == "enrollment" {
		if r.Method != http.MethodPost {
			a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var input struct {
			ControllerURL string `json:"controller_url"`
		}
		if err := decodeJSONBody(w, r, &input); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid request")
			return
		}
		controllerURL, err := normalizeControllerURL(input.ControllerURL)
		if err != nil {
			writeNodeAPIError(a, w, err)
			return
		}
		node, token, err := a.db.RefreshNodeEnrollment(id, now)
		if err != nil {
			writeNodeAPIError(a, w, err)
			return
		}
		a.jsonOK(w, map[string]interface{}{"node": node, "install_script": buildNodeInstallScriptWithOptions(controllerURL, token, true), "install_command": buildNodeInstallCommandWithOptions(controllerURL, token, true)})
		return
	}
	if action != "" {
		writeNodeAPIError(a, w, errNodeNotFound)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var input nodeAPIInput
		if err := decodeJSONBody(w, r, &input); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid request")
			return
		}
		enabled := true
		if input.Enabled != nil {
			enabled = *input.Enabled
		}
		node, err := a.db.UpdateControlNode(id, nodeCreateInput(input), enabled, now)
		if err != nil {
			writeNodeAPIError(a, w, err)
			return
		}
		a.jsonOK(w, node)
	case http.MethodDelete:
		if err := a.prepareNodeDeletion(r.Context(), id); err != nil {
			a.jsonErr(w, http.StatusBadGateway, "node DNS cleanup failed: "+err.Error())
			return
		}
		if err := a.db.DeleteControlNode(id); err != nil {
			writeNodeAPIError(a, w, err)
			return
		}
		a.jsonOK(w, map[string]bool{"deleted": true})
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *App) handleNodeScheduler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var input struct {
		Mode         string `json:"mode"`
		ManualNodeID int64  `json:"manual_node_id"`
	}
	if err := decodeJSONBody(w, r, &input); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	snapshot, err := a.db.UpdateNodeScheduler(input.Mode, input.ManualNodeID, time.Now())
	if err != nil {
		writeNodeAPIError(a, w, err)
		return
	}
	a.jsonOK(w, snapshot)
}

func requestBearerToken(r *http.Request) string {
	value := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(value) < 8 || !strings.EqualFold(value[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(value[7:])
}

func (a *App) handleAgentBinary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	identity, authErr := agentIdentityForRequest(a, r)
	if authErr != nil {
		writeAgentAuthenticationError(a, w, r, authErr)
		return
	}
	if !identity.HasNode && !identity.Enrollment {
		writeAgentAuthFailure(a, w, r)
		return
	}
	platform, err := requestedAgentPlatform(r)
	if err != nil {
		a.jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// A platform-aware legacy Agent can still use this endpoint. Prefer a
	// locally installed binary for old standalone Controllers, then proxy the
	// pinned Release asset when the Controller image no longer carries one.
	if platform == "" {
		if serveAgentBinaryFile(w, configuredAgentBinaryPath(), "") {
			return
		}
		a.jsonErr(w, http.StatusConflict, "legacy Agent must send X-Meridian-Agent-Platform; reinstall it to enable updates")
		return
	}
	// Resolve the immutable Release checksum before considering any local
	// legacy bundle. Standalone v1.9.29 Controllers may still have stale
	// platform binaries on disk; serving one would make an old Agent download
	// the same version it already runs and fail its checksum verification.
	manifest, manifestErr := agentReleaseManifestForPlatform(r.Context(), platform)
	if manifestErr != nil {
		log.Printf("Agent release manifest unavailable for %s: %v", platform, manifestErr)
		a.jsonErr(w, http.StatusServiceUnavailable, "agent binary unavailable")
		return
	}
	if executable, pathErr := configuredAgentBinaryPathForPlatform(platform); pathErr == nil {
		if localAgentBinaryMatchesManifest(executable, manifest) && serveAgentBinaryFile(w, executable, platform) {
			return
		}
	}
	started, err := proxyAgentReleaseBinaryWithManifest(w, r, manifest)
	if err != nil {
		log.Printf("legacy Agent binary proxy failed for %s: %v", platform, err)
		if !started {
			a.jsonErr(w, http.StatusServiceUnavailable, "agent binary unavailable")
		}
	}
}

func agentBinarySHA256(executable string) (string, error) {
	file, err := os.Open(executable) // #nosec G304 -- the path is fixed by the image or an administrator-controlled environment variable.
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	written, err := io.Copy(digest, io.LimitReader(file, 128<<20+1))
	if err != nil || written <= 0 || written > 128<<20 {
		if err != nil {
			return "", err
		}
		return "", errors.New("Agent binary is too large")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func localAgentBinaryMatchesManifest(executable string, manifest AgentBinaryManifest) bool {
	localSHA, err := agentBinarySHA256(executable)
	return err == nil && strings.EqualFold(localSHA, manifest.SHA256)
}

func serveAgentBinaryFile(w http.ResponseWriter, executable, platform string) bool {
	file, err := os.Open(executable) // #nosec G304 -- the path is fixed by the image or an administrator-controlled environment variable.
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > 128<<20 {
		return false
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return false
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="meridian-agent"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Meridian-Agent-SHA256", hex.EncodeToString(digest.Sum(nil)))
	if platform != "" {
		w.Header().Set(agentPlatformHeader, platform)
	}
	_, _ = io.Copy(w, file)
	return true
}

func proxyAgentReleaseBinary(w http.ResponseWriter, r *http.Request, platform string) (bool, error) {
	manifest, err := agentReleaseManifestForPlatform(r.Context(), platform)
	if err != nil {
		return false, err
	}
	return proxyAgentReleaseBinaryWithManifest(w, r, manifest)
}

func proxyAgentReleaseBinaryWithManifest(w http.ResponseWriter, r *http.Request, manifest AgentBinaryManifest) (bool, error) {
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, manifest.DownloadURL, nil)
	if err != nil {
		return false, err
	}
	client := &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("refusing non-HTTPS GitHub redirect")
		}
		return nil
	}}
	response, err := client.Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("GitHub Agent release returned %s", response.Status)
	}
	if response.ContentLength > 128<<20 {
		return false, errors.New("Agent release binary is too large")
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="meridian-agent"`)
	w.Header().Set("Cache-Control", "no-store")
	if response.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(response.ContentLength, 10))
	}
	w.Header().Set("X-Meridian-Agent-SHA256", manifest.SHA256)
	w.Header().Set("X-Meridian-Agent-Version", manifest.Version)
	w.Header().Set(agentPlatformHeader, manifest.Platform)
	digest := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(w, digest), io.LimitReader(response.Body, 128<<20+1))
	if copyErr != nil {
		return true, copyErr
	}
	if written <= 0 || written > 128<<20 || !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), manifest.SHA256) {
		return true, errors.New("GitHub Agent release checksum mismatch")
	}
	return true, nil
}

// handleAgentManifest authenticates the node exactly like the legacy binary
// endpoint, but returns a release-pinned GitHub asset so Controllers do not
// need to carry every Agent architecture in their image or installation.
func (a *App) handleAgentManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	identity, authErr := agentIdentityForRequest(a, r)
	if authErr != nil {
		writeAgentAuthenticationError(a, w, r, authErr)
		return
	}
	if !identity.HasNode && !identity.Enrollment {
		writeAgentAuthFailure(a, w, r)
		return
	}
	platform, err := requestedAgentPlatform(r)
	if err != nil {
		a.jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if platform == "" {
		a.jsonErr(w, http.StatusBadRequest, "agent platform is required")
		return
	}
	manifest, err := agentReleaseManifestForPlatform(r.Context(), platform)
	if err != nil {
		log.Printf("agent release manifest unavailable for %s: %v", platform, err)
		a.jsonErr(w, http.StatusServiceUnavailable, "agent release manifest unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Meridian-Agent-Platform", manifest.Platform)
	w.Header().Set("X-Meridian-Agent-Version", manifest.Version)
	w.Header().Set("X-Meridian-Agent-SHA256", manifest.SHA256)
	w.Header().Set("X-Meridian-Agent-Download-URL", manifest.DownloadURL)
	a.jsonOK(w, manifest)
}

// handleAgentInstallScript is intentionally public: it contains no node
// credential. The enrollment token is supplied as a command-line argument so
// operators can use the same curl|bash workflow as other node agents.
func (a *App) handleAgentInstallScript(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, agentInstallerScript)
}

func (a *App) handleAgentEnroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	identity, authErr := agentIdentityForRequest(a, r)
	if authErr != nil {
		writeAgentAuthenticationError(a, w, r, authErr)
		return
	}
	if !identity.Enrollment {
		writeAgentAuthFailure(a, w, r)
		return
	}
	node, agentToken, err := a.db.EnrollControlNode(identity.Token, time.Now())
	if err != nil {
		if errors.Is(err, errPersistentJWTRequired) {
			a.jsonErr(w, http.StatusConflict, "请先配置持久 JWT_SECRET，再注册 Agent 节点")
			return
		}
		a.jsonErr(w, http.StatusUnauthorized, "invalid enrollment token")
		return
	}
	// Queue provisioning so enrollment returns immediately and each job gets
	// its own ACME timeout/retry budget instead of waiting behind another node.
	if a.certificateWorker != nil {
		a.certificateWorker.enqueue(node.ID)
	} else if a.panelCertificates != nil {
		go func(enrolled ControlNode) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := provisionEdgeCertificateForNode(ctx, a.db, a.panelCertificates, enrolled); err != nil {
				log.Printf("[edge-certificate] initial provision failed for node %s: %v", enrolled.Name, err)
			}
		}(node)
	}
	a.jsonOK(w, map[string]interface{}{"node_guid": node.GUID, "agent_token": agentToken, "report_interval_seconds": int(agentFullReportInterval / time.Second)})
}

func (a *App) handleAgentReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	identity, authErr := agentNodeIdentityForRequest(a, r)
	if authErr != nil {
		writeAgentAuthenticationError(a, w, r, authErr)
		return
	}
	token := identity.Token
	node := identity.Node
	release, retryAfter, admitted := a.agentReports().admit(node.ID, time.Now())
	if !admitted {
		seconds := int(retryAfter.Seconds())
		if retryAfter-time.Duration(seconds)*time.Second > 0 {
			seconds++
		}
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		a.jsonErr(w, http.StatusTooManyRequests, "agent report rate or concurrency limit exceeded")
		return
	}
	defer release()
	var report NodeReport
	if err := decodeJSONBodyWithLimit(w, r, &report, maxAgentReportBodyBytes); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	result, err := a.db.RecordNodeReportResult(token, report, time.Now())
	if errors.Is(err, errInvalidAgentToken) {
		writeAgentAuthFailure(a, w, r)
		return
	}
	if errors.Is(err, errStaleAgentSession) {
		w.Header().Set("X-Meridian-Agent-State", "stale")
		a.jsonErr(w, http.StatusConflict, "stale agent session; retry configuration fetch")
		return
	}
	if err != nil {
		a.jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	configChanged := nodeReportConfigChanged(result.Node, report.AppliedConfigHash, report.AppliedConfigRevision)
	a.jsonOK(w, map[string]interface{}{
		"accepted": true, "node_id": result.Node.ID, "next_report_seconds": int(agentFullReportInterval / time.Second),
		"accepted_site_ids": result.AcceptedSiteIDs, "discarded_site_ids": result.DiscardedSiteIDs,
		"accepted_media_site_ids": result.AcceptedMediaSiteIDs, "discarded_media_site_ids": result.DiscardedMediaSiteIDs,
		"accepted_retention_site_ids": result.AcceptedRetentionSiteIDs, "discarded_retention_site_ids": result.DiscardedRetentionSiteIDs,
		"accepted_observation_site_ids": result.AcceptedObservationSiteIDs, "discarded_observation_site_ids": result.DiscardedObservationSiteIDs,
		"accepted_event_ids": result.AcceptedEventIDs, "accepted_event_uids": result.AcceptedEventUIDs,
		"discarded_event_ids": result.DiscardedEventIDs, "discarded_event_uids": result.DiscardedEventUIDs,
		"config_hash":    result.Node.DesiredConfigHash,
		"config_changed": configChanged,
	})
}

// handleAgentLive accepts the small, high-frequency traffic sample used by
// the dashboard. It has an independent per-node admission gate from the full
// HTTP/WebSocket report paths, and performs no SQLite writes.
func (a *App) handleAgentLive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	identity, authErr := agentNodeIdentityForRequest(a, r)
	if authErr != nil {
		writeAgentAuthenticationError(a, w, r, authErr)
		return
	}
	release, retryAfter, admitted := a.agentLiveReports().admit(identity.Node.ID, time.Now())
	if !admitted {
		seconds := int(retryAfter.Seconds())
		if retryAfter-time.Duration(seconds)*time.Second > 0 {
			seconds++
		}
		if seconds < 1 {
			seconds = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		a.jsonErr(w, http.StatusTooManyRequests, "agent report rate or concurrency limit exceeded")
		return
	}
	defer release()
	var report NodeLiveReport
	if err := decodeJSONBodyWithLimit(w, r, &report, maxAgentLiveReportBodyBytes); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	now := time.Now()
	if err := validateNodeLiveReport(report, now); err != nil {
		a.jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	accepted, discarded, err := a.db.recordNodeLiveReport(identity.Node.ID, report, now)
	if errors.Is(err, errInvalidAgentToken) {
		writeAgentAuthFailure(a, w, r)
		return
	}
	if errors.Is(err, errStaleAgentSession) {
		w.Header().Set("X-Meridian-Agent-State", "stale")
		a.jsonErr(w, http.StatusConflict, "stale agent session; retry configuration fetch")
		return
	}
	if err != nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "live report temporarily unavailable")
		return
	}
	for _, siteID := range discarded {
		a.db.recordAgentSecurityRejection(identity.Node.ID, siteID, "site-stats")
	}
	a.jsonOK(w, map[string]interface{}{
		"accepted":            true,
		"node_id":             identity.Node.ID,
		"next_report_seconds": int(agentLiveReportInterval / time.Second),
		"accepted_site_ids":   accepted,
		"discarded_site_ids":  discarded,
	})
}

// nodeReportConfigChanged tells an Agent to fetch immediately when the
// controller has invalidated its runtime snapshot. A blank desired hash is a
// deliberate invalidation marker, so it must also trigger a refresh.
func nodeReportConfigChanged(node ControlNode, appliedHash string, appliedRevision int64) bool {
	desired := strings.TrimSpace(node.DesiredConfigHash)
	applied := strings.TrimSpace(appliedHash)
	if appliedRevision > 0 && node.DesiredConfigRevision > 0 && appliedRevision != node.DesiredConfigRevision {
		return true
	}
	return node.ConfigDirty || desired == "" || desired != applied
}

type agentWebSocketNodeContextKey struct{}

func withAgentWebSocketNode(r *http.Request, node ControlNode) *http.Request {
	if r == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), agentWebSocketNodeContextKey{}, node))
}

// handleAgentWebSocket is a control-plane heartbeat channel. It deliberately
// shares RecordNodeReport with the POST fallback so sequence/event idempotency,
// node traffic accounting, and metadata replay have one implementation.
func (a *App) handleAgentWebSocket(ws *websocket.Conn) {
	if ws == nil || ws.Request() == nil {
		return
	}
	token := requestBearerToken(ws.Request())
	node, ok := ws.Request().Context().Value(agentWebSocketNodeContextKey{}).(ControlNode)
	if !ok || node.ID <= 0 {
		// Keep direct handler tests and embedders functional when they do not
		// install the main router's handshake context. The production router
		// always supplies the authenticated node and therefore performs no
		// second lookup for a connection.
		var authErr error
		node, authErr = a.db.nodeByAgentToken(token, time.Now())
		if authErr != nil {
			return
		}
	}
	ws.MaxPayloadBytes = maxAgentReportBodyBytes
	defer ws.Close()
	for {
		if err := ws.SetReadDeadline(time.Now().Add(nodeOnlineWindow)); err != nil {
			return
		}
		// Read only the bounded WebSocket frame before admission. The frame is
		// capped by MaxPayloadBytes; admitting after receive prevents an idle
		// WebSocket connection from monopolizing the node's slot and blocking the
		// independent 2-second live traffic channel.
		var payload []byte
		if err := websocket.Message.Receive(ws, &payload); err != nil {
			return
		}
		release, retryAfter, admitted := a.agentReports().admit(node.ID, time.Now())
		if !admitted {
			_ = ws.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_ = websocket.JSON.Send(ws, map[string]interface{}{
				"accepted":            false,
				"error":               "agent report rate or concurrency limit exceeded",
				"retry_after_seconds": max(1, int(retryAfter.Seconds()+0.5)),
			})
			return
		}
		var report NodeReport
		if err := json.Unmarshal(payload, &report); err != nil {
			release()
			_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
			_ = websocket.JSON.Send(ws, map[string]interface{}{
				"accepted": false,
				"error":    "invalid request",
			})
			continue
		}
		result, err := a.db.RecordNodeReportResult(token, report, time.Now())
		release()
		if err != nil {
			if errors.Is(err, errStaleAgentSession) {
				_ = ws.SetWriteDeadline(time.Now().Add(2 * time.Second))
				_ = websocket.JSON.Send(ws, map[string]interface{}{"accepted": false, "agent_state": "stale", "error": "stale agent session; retry configuration fetch"})
			}
			return
		}
		ack := map[string]interface{}{
			"accepted": true, "node_id": result.Node.ID, "next_report_seconds": int(agentFullReportInterval / time.Second),
			"accepted_site_ids": result.AcceptedSiteIDs, "discarded_site_ids": result.DiscardedSiteIDs,
			"accepted_media_site_ids": result.AcceptedMediaSiteIDs, "discarded_media_site_ids": result.DiscardedMediaSiteIDs,
			"accepted_retention_site_ids": result.AcceptedRetentionSiteIDs, "discarded_retention_site_ids": result.DiscardedRetentionSiteIDs,
			"accepted_observation_site_ids": result.AcceptedObservationSiteIDs, "discarded_observation_site_ids": result.DiscardedObservationSiteIDs,
			"accepted_event_ids": result.AcceptedEventIDs, "accepted_event_uids": result.AcceptedEventUIDs,
			"discarded_event_ids": result.DiscardedEventIDs, "discarded_event_uids": result.DiscardedEventUIDs,
			"config_hash":    result.Node.DesiredConfigHash,
			"config_changed": nodeReportConfigChanged(result.Node, report.AppliedConfigHash, report.AppliedConfigRevision),
		}
		if err := ws.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return
		}
		if err := websocket.JSON.Send(ws, ack); err != nil {
			return
		}
	}
}
