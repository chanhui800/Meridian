package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
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

func agentBinaryIdentity() (string, string, error) {
	agentBinaryIdentityCache.Do(func() {
		file, err := os.Open(configuredAgentBinaryPath()) // #nosec G304 -- fixed image path or administrator-controlled environment value.
		if err != nil {
			agentBinaryIdentityCache.err = err
			return
		}
		defer file.Close()
		digest := sha256.New()
		if _, err := io.Copy(digest, file); err != nil {
			agentBinaryIdentityCache.err = err
			return
		}
		agentBinaryIdentityCache.version = appVersion
		agentBinaryIdentityCache.digest = hex.EncodeToString(digest.Sum(nil))
	})
	return agentBinaryIdentityCache.version, agentBinaryIdentityCache.digest, agentBinaryIdentityCache.err
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
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return "", errors.New("controller_url must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("controller_url cannot contain credentials, query, or fragment")
	}
	hostIP := net.ParseIP(parsed.Hostname())
	if parsed.Scheme != "https" && parsed.Hostname() != "localhost" && (hostIP == nil || !hostIP.IsLoopback()) {
		return "", errors.New("public controller_url must use HTTPS")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func buildNodeInstallScript(controllerURL, enrollmentToken string) string {
	controller := shellSingleQuote(controllerURL)
	token := shellSingleQuote(enrollmentToken)
	return fmt.Sprintf("#!/bin/sh\nset -eu\nexec /bin/sh -s -- -e %s -t %s <<'MERIDIAN_CANONICAL_INSTALLER'\n%sMERIDIAN_CANONICAL_INSTALLER\n", controller, token, agentInstallerScript)
}

func buildNodeInstallCommand(controllerURL, enrollmentToken string) string {
	endpoint := strings.TrimRight(controllerURL, "/") + "/api/agent/install.sh"
	return fmt.Sprintf("curl --proto '=https' --proto-redir '=https' --tlsv1.2 -fsSL %s | sudo bash -s -- -e %s -t %s",
		shellSingleQuote(endpoint),
		shellSingleQuote(controllerURL), shellSingleQuote(enrollmentToken))
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
		a.jsonOK(w, map[string]interface{}{"node": node, "install_script": buildNodeInstallScript(controllerURL, token), "install_command": buildNodeInstallCommand(controllerURL, token)})
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
	token := requestBearerToken(r)
	if err := a.db.AuthorizeEnrollmentToken(token, time.Now()); err != nil {
		if _, agentErr := a.db.nodeByAgentToken(token, time.Now()); agentErr != nil {
			a.jsonErr(w, http.StatusUnauthorized, "invalid agent token")
			return
		}
	}
	executable := configuredAgentBinaryPath()
	file, err := os.Open(executable) // #nosec G304 -- the path is fixed by the image or an administrator-controlled environment variable.
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "agent binary unavailable")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "agent binary unavailable")
		return
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "agent binary unavailable")
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "agent binary unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="meridian-agent"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.Header().Set("X-Meridian-Agent-SHA256", hex.EncodeToString(digest.Sum(nil)))
	_, _ = io.Copy(w, file)
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
	node, agentToken, err := a.db.EnrollControlNode(requestBearerToken(r), time.Now())
	if err != nil {
		a.jsonErr(w, http.StatusUnauthorized, "invalid enrollment token")
		return
	}
	// Provision this node immediately instead of waiting for the 12-hour
	// renewal scheduler. The operation is bounded and asynchronous so enroll
	// remains responsive; the normal scheduler retries any transient failure.
	if a.panelCertificates != nil {
		go func(enrolled ControlNode) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := provisionEdgeCertificateForNode(ctx, a.db, a.panelCertificates, enrolled); err != nil {
				log.Printf("[edge-certificate] initial provision failed for node %s: %v", enrolled.Name, err)
			}
		}(node)
	}
	a.jsonOK(w, map[string]interface{}{"node_guid": node.GUID, "agent_token": agentToken, "report_interval_seconds": 15})
}

func (a *App) handleAgentReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var report NodeReport
	if err := decodeJSONBodyWithLimit(w, r, &report, maxAgentReportBodyBytes); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	result, err := a.db.RecordNodeReportResult(requestBearerToken(r), report, time.Now())
	if errors.Is(err, errInvalidAgentToken) {
		a.jsonErr(w, http.StatusUnauthorized, "invalid agent token")
		return
	}
	if err != nil {
		a.jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	configChanged := result.Node.DesiredConfigHash != "" && result.Node.DesiredConfigHash != report.AppliedConfigHash
	a.jsonOK(w, map[string]interface{}{
		"accepted": true, "node_id": result.Node.ID, "next_report_seconds": 15,
		"accepted_event_ids": result.AcceptedEventIDs, "accepted_event_uids": result.AcceptedEventUIDs,
		"discarded_event_ids": result.DiscardedEventIDs, "discarded_event_uids": result.DiscardedEventUIDs,
		"config_hash":    result.Node.DesiredConfigHash,
		"config_changed": configChanged,
	})
}

// handleAgentWebSocket is a control-plane heartbeat channel. It deliberately
// shares RecordNodeReport with the POST fallback so sequence/event idempotency,
// node traffic accounting, and metadata replay have one implementation.
func (a *App) handleAgentWebSocket(ws *websocket.Conn) {
	if ws == nil || ws.Request() == nil {
		return
	}
	token := requestBearerToken(ws.Request())
	if _, err := a.db.nodeByAgentToken(token, time.Now()); err != nil {
		return
	}
	ws.MaxPayloadBytes = maxAgentReportBodyBytes
	defer ws.Close()
	for {
		if err := ws.SetReadDeadline(time.Now().Add(nodeOnlineWindow)); err != nil {
			return
		}
		var report NodeReport
		if err := websocket.JSON.Receive(ws, &report); err != nil {
			return
		}
		result, err := a.db.RecordNodeReportResult(token, report, time.Now())
		if err != nil {
			return
		}
		ack := map[string]interface{}{
			"accepted": true, "node_id": result.Node.ID, "next_report_seconds": 15,
			"accepted_event_ids": result.AcceptedEventIDs, "accepted_event_uids": result.AcceptedEventUIDs,
			"discarded_event_ids": result.DiscardedEventIDs, "discarded_event_uids": result.DiscardedEventUIDs,
			"config_hash":    result.Node.DesiredConfigHash,
			"config_changed": result.Node.DesiredConfigHash != "" && result.Node.DesiredConfigHash != report.AppliedConfigHash,
		}
		if err := ws.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return
		}
		if err := websocket.JSON.Send(ws, ack); err != nil {
			return
		}
	}
}
