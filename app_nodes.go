package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
	return fmt.Sprintf(`#!/bin/sh
set -eu
controller_url=%s
enrollment_token=%s
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c|--controller) [ "$#" -ge 2 ] || { echo 'missing controller URL' >&2; exit 2; }; controller_url="$2"; shift 2 ;;
    -t|--token) [ "$#" -ge 2 ] || { echo 'missing enrollment token' >&2; exit 2; }; enrollment_token="$2"; shift 2 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
if [ -z "$controller_url" ] || [ -z "$enrollment_token" ]; then
  echo 'usage: install.sh -c https://panel.example.com -t ENROLLMENT_TOKEN' >&2
  exit 2
fi
install_dir=/opt/meridian-agent
state_dir=/var/lib/meridian-agent
token_dir=/etc/meridian-agent
token_file="$token_dir/enrollment-token"
binary_tmp="$install_dir/meridian-agent.tmp"

if [ "$(id -u)" -ne 0 ]; then
  echo "Please run this script as root." >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64|amd64) ;;
  *) echo "This test Agent currently supports Linux x86_64 only." >&2; exit 1 ;;
esac

install -d -m 0755 "$install_dir"
install -d -m 0700 "$state_dir"
install -d -m 0700 "$token_dir"
if command -v systemctl >/dev/null 2>&1; then
  systemctl stop meridian-agent.service 2>/dev/null || true
fi
rm -f "$state_dir/state.json"
umask 077
printf '%%s' "$enrollment_token" > "$token_file"
curl -fsSL -H "Authorization: Bearer $enrollment_token" \
  "$controller_url/api/agent/binary" -o "$binary_tmp"
chmod 0755 "$binary_tmp"
mv -f "$binary_tmp" "$install_dir/meridian-agent"

cat > /etc/systemd/system/meridian-agent.service <<EOF
[Unit]
Description=Meridian node agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$install_dir/meridian-agent --controller $controller_url --state $state_dir/state.json --enroll-token-file $token_file
Restart=always
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ReadWritePaths=$state_dir $install_dir $token_dir

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now meridian-agent.service
# Keep the generated installer non-interactive: status may invoke a pager on
# distributions that ignore --no-pager in non-login shells.
if ! systemctl is-active --quiet meridian-agent.service; then
  systemctl --no-pager --full --lines=20 status meridian-agent.service >&2 || true
  exit 1
fi
printf '%%s\n' 'Meridian Agent installed and running.'
`, controller, token)
}

func buildNodeInstallCommand(controllerURL, enrollmentToken string) string {
	return fmt.Sprintf("wget -qO- 'https://raw.githubusercontent.com/chanhui800/Meridian/main/scripts/agent-install.sh' | sudo bash -s -- -e %s -t %s",
		shellSingleQuote(controllerURL), shellSingleQuote(enrollmentToken))
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
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="meridian-agent"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
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
	_, _ = io.WriteString(w, buildNodeInstallScript("", ""))
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
	node, err := a.db.RecordNodeReport(requestBearerToken(r), report, time.Now())
	if errors.Is(err, errInvalidAgentToken) {
		a.jsonErr(w, http.StatusUnauthorized, "invalid agent token")
		return
	}
	if err != nil {
		a.jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	accepted := make([]int64, 0, len(report.Events))
	for _, event := range report.Events {
		accepted = append(accepted, event.EventID)
	}
	a.jsonOK(w, map[string]interface{}{"accepted": true, "node_id": node.ID, "next_report_seconds": 15, "accepted_event_ids": accepted})
}
