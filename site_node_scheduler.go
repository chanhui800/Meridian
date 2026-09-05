package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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
	"time"

	"golang.org/x/net/publicsuffix"
)

const agentConfigSchemaVersion = 1

type SiteNodeSchedule struct {
	SiteID               int64  `json:"site_id"`
	SiteName             string `json:"site_name"`
	PublicHost           string `json:"public_host"`
	Enabled              bool   `json:"enabled"`
	Mode                 string `json:"mode"`
	FixedNodeID          int64  `json:"fixed_node_id"`
	DesiredNodeID        int64  `json:"desired_node_id"`
	AppliedNodeID        int64  `json:"applied_node_id"`
	AppliedAddress       string `json:"applied_address"`
	DNSStatus            string `json:"dns_status"`
	ConfigHash           string `json:"config_hash"`
	LastError            string `json:"last_error"`
	DesiredNodeName      string `json:"desired_node_name"`
	AppliedNodeName      string `json:"applied_node_name"`
	AppliedNodePort      int    `json:"applied_node_port"`
	AgentBootID          string `json:"agent_boot_id,omitempty"`
	AgentRequestCount    int64  `json:"agent_request_count"`
	AgentLastRequestAtMS int64  `json:"agent_last_request_at_ms"`
	AgentLastStatus      int    `json:"agent_last_status"`
	UpdatedAtMS          int64  `json:"updated_at_ms"`
	cfZoneID             string
	cfRecordID           string
	cfRecordType         string
}

type AgentSiteRoute struct {
	SiteID            int64               `json:"site_id"`
	Host              string              `json:"host"`
	TargetURL         string              `json:"target_url"`
	PlaybackTargetURL string              `json:"playback_target_url,omitempty"`
	StreamHosts       []string            `json:"stream_hosts,omitempty"`
	PlaybackMode      string              `json:"playback_mode,omitempty"`
	Headers           map[string][]string `json:"headers,omitempty"`
	Site              Site                `json:"site"`
	FailoverTargets   string              `json:"failover_targets_raw,omitempty"`
	FailoverLines     string              `json:"failover_lines_raw,omitempty"`
	StreamHostsRaw    string              `json:"stream_hosts_raw,omitempty"`
	UpstreamHeaders   string              `json:"upstream_headers_raw,omitempty"`
	DynamicSources    string              `json:"dynamic_sources_raw,omitempty"`
	DynamicRules      string              `json:"dynamic_rules_raw,omitempty"`
}

type AgentRuntimeConfig struct {
	SchemaVersion  int              `json:"schema_version"`
	ConfigHash     string           `json:"config_hash"`
	NodeGUID       string           `json:"node_guid"`
	EntryMode      string           `json:"entry_mode"`
	HTTPPort       int              `json:"http_port"`
	HTTPSPort      int              `json:"https_port"`
	CertificatePEM string           `json:"certificate_pem,omitempty"`
	PrivateKeyPEM  string           `json:"private_key_pem,omitempty"`
	DynamicKey     string           `json:"dynamic_key,omitempty"`
	AgentVersion   string           `json:"agent_version,omitempty"`
	AgentSHA256    string           `json:"agent_sha256,omitempty"`
	Routes         []AgentSiteRoute `json:"routes"`
}

func deriveNodeRuntimeKey(master []byte, nodeGUID, purpose string) []byte {
	if len(master) == 0 || strings.TrimSpace(nodeGUID) == "" {
		return nil
	}
	mac := hmac.New(sha256.New, master)
	_, _ = mac.Write([]byte("meridian-node-runtime-v1\x00" + purpose + "\x00" + nodeGUID))
	return mac.Sum(nil)
}

func encodeRuntimeKey(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func scanSiteNodeSchedule(scanner interface{ Scan(...any) error }) (SiteNodeSchedule, error) {
	var value SiteNodeSchedule
	var enabled int
	var fixed, desired, applied sql.NullInt64
	err := scanner.Scan(&value.SiteID, &value.SiteName, &value.PublicHost, &enabled, &value.Mode, &fixed, &desired, &applied,
		&value.cfZoneID, &value.cfRecordID, &value.cfRecordType, &value.AppliedAddress, &value.DNSStatus,
		&value.ConfigHash, &value.LastError, &value.DesiredNodeName, &value.AppliedNodeName, &value.AppliedNodePort,
		&value.AgentBootID, &value.AgentRequestCount, &value.AgentLastRequestAtMS, &value.AgentLastStatus, &value.UpdatedAtMS)
	value.Enabled = enabled != 0
	if fixed.Valid {
		value.FixedNodeID = fixed.Int64
	}
	if desired.Valid {
		value.DesiredNodeID = desired.Int64
	}
	if applied.Valid {
		value.AppliedNodeID = applied.Int64
	}
	return value, err
}

const siteNodeScheduleSelect = `SELECT s.id,s.name,s.public_host,COALESCE(n.enabled,0),COALESCE(n.mode,'global'),
	n.fixed_node_id,n.desired_node_id,n.applied_node_id,COALESCE(n.cf_zone_id,''),COALESCE(n.cf_record_id,''),
	COALESCE(n.cf_record_type,''),COALESCE(n.applied_address,''),COALESCE(n.dns_status,'disabled'),
	COALESCE(n.config_hash,''),COALESCE(n.last_error,''),COALESCE(d.name,''),COALESCE(an.name,''),COALESCE(an.https_port,0),
	COALESCE(n.agent_boot_id,''),COALESCE(n.agent_request_count,0),COALESCE(n.agent_last_request_at_ms,0),COALESCE(n.agent_last_status,0),COALESCE(n.updated_at_ms,0)
	FROM sites s LEFT JOIN site_node_schedules n ON n.site_id=s.id
	LEFT JOIN control_nodes d ON d.id=n.desired_node_id LEFT JOIN control_nodes an ON an.id=n.applied_node_id`

func (d *DB) ListSiteNodeSchedules() ([]SiteNodeSchedule, error) {
	rows, err := d.db.Query(siteNodeScheduleSelect + " ORDER BY s.sort_order,s.id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]SiteNodeSchedule, 0)
	for rows.Next() {
		value, err := scanSiteNodeSchedule(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (d *DB) siteNodeSchedule(siteID int64) (SiteNodeSchedule, error) {
	value, err := scanSiteNodeSchedule(d.db.QueryRow(siteNodeScheduleSelect+" WHERE s.id=?", siteID))
	if errors.Is(err, sql.ErrNoRows) {
		return SiteNodeSchedule{}, errNodeNotFound
	}
	return value, err
}

func normalizeSiteScheduleMode(mode string) (string, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "global"
	}
	if mode != "global" && mode != "fixed" {
		return "", errors.New("site schedule mode must be global or fixed")
	}
	return mode, nil
}

func (d *DB) SaveSiteNodeSchedule(siteID int64, enabled bool, mode string, fixedNodeID int64, now time.Time) (SiteNodeSchedule, error) {
	mode, err := normalizeSiteScheduleMode(mode)
	if err != nil {
		return SiteNodeSchedule{}, err
	}
	site, err := d.GetSite(siteID)
	if err != nil {
		return SiteNodeSchedule{}, err
	}
	if enabled && strings.TrimSpace(site.PublicHost) == "" {
		return SiteNodeSchedule{}, errors.New("site requires a public host before node scheduling can be enabled")
	}
	if enabled && mode == "fixed" {
		if fixedNodeID <= 0 {
			return SiteNodeSchedule{}, errors.New("fixed mode requires a node")
		}
		if _, err := d.controlNodeByID(fixedNodeID, now); err != nil {
			return SiteNodeSchedule{}, err
		}
	} else if mode != "fixed" {
		fixedNodeID = 0
	}
	status := "disabled"
	if enabled {
		status = "pending"
	}
	_, err = d.db.Exec(`INSERT INTO site_node_schedules
		(site_id,enabled,mode,fixed_node_id,dns_status,created_at_ms,updated_at_ms)
		VALUES(?,?,?,?,?,?,?) ON CONFLICT(site_id) DO UPDATE SET enabled=excluded.enabled,mode=excluded.mode,
		fixed_node_id=excluded.fixed_node_id,dns_status=excluded.dns_status,last_error='',updated_at_ms=excluded.updated_at_ms`,
		siteID, sqliteBool(enabled), mode, nullableNodeID(fixedNodeID), status, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return SiteNodeSchedule{}, err
	}
	return d.siteNodeSchedule(siteID)
}

const siteNodeProbeCooldown = 2 * time.Minute

func (d *DB) siteNodeProbeCooldowns(siteID int64, now time.Time) (map[int64]bool, error) {
	rows, err := d.db.Query("SELECT node_id FROM site_node_probe_failures WHERE site_id=? AND failed_until_ms>?", siteID, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make(map[int64]bool)
	for rows.Next() {
		var nodeID int64
		if err := rows.Scan(&nodeID); err != nil {
			return nil, err
		}
		values[nodeID] = true
	}
	return values, rows.Err()
}

func (d *DB) recordSiteNodeProbeFailure(siteID, nodeID int64, probeErr error, now time.Time) error {
	if siteID <= 0 || nodeID <= 0 || probeErr == nil {
		return nil
	}
	_, err := d.db.Exec(`INSERT INTO site_node_probe_failures(site_id,node_id,failed_until_ms,last_error,updated_at_ms)
		VALUES(?,?,?,?,?) ON CONFLICT(site_id,node_id) DO UPDATE SET failed_until_ms=excluded.failed_until_ms,last_error=excluded.last_error,updated_at_ms=excluded.updated_at_ms`,
		siteID, nodeID, now.Add(siteNodeProbeCooldown).UnixMilli(), probeErr.Error(), now.UnixMilli())
	return err
}

func (d *DB) clearSiteNodeProbeFailure(siteID, nodeID int64) error {
	if siteID <= 0 || nodeID <= 0 {
		return nil
	}
	_, err := d.db.Exec("DELETE FROM site_node_probe_failures WHERE site_id=? AND node_id=?", siteID, nodeID)
	return err
}

func (d *DB) nodeByAgentToken(token string, now time.Time) (ControlNode, error) {
	if strings.TrimSpace(token) == "" {
		return ControlNode{}, errInvalidAgentToken
	}
	node, err := scanControlNode(d.db.QueryRow(controlNodeSelect+" WHERE agent_token_hash=?", hashNodeToken(token)), now)
	if errors.Is(err, sql.ErrNoRows) {
		return ControlNode{}, errInvalidAgentToken
	}
	return node, err
}

func (a *App) refreshSiteAssignments(now time.Time) error {
	snapshot, err := a.db.NodeControlSnapshot(now)
	if err != nil {
		return err
	}
	eligible := make(map[int64]bool, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		eligible[node.ID] = nodeEligible(node)
	}
	values, err := a.db.ListSiteNodeSchedules()
	if err != nil {
		return err
	}
	for _, value := range values {
		if !value.Enabled {
			continue
		}
		desired := snapshot.Scheduler.ActiveNodeID
		// Keep the last reconciliation error visible between UI refreshes. The
		// health/DNS pass clears it only after a successful commit; otherwise a
		// GET /api/node-scheduler/sites must not hide the reason for waiting.
		lastError := strings.TrimSpace(value.LastError)
		if value.Mode == "fixed" {
			desired = value.FixedNodeID
		} else if snapshot.Scheduler.Mode == "auto" {
			cooldowns, cooldownErr := a.db.siteNodeProbeCooldowns(value.SiteID, now)
			if cooldownErr != nil {
				return cooldownErr
			}
			desired = 0
			for _, candidate := range snapshot.Nodes {
				if nodeEligible(candidate) && !cooldowns[candidate.ID] {
					desired = candidate.ID
					break
				}
			}
		}
		if desired <= 0 || !eligible[desired] {
			desired = 0
			lastError = "no eligible node is available"
		} else if desired != value.DesiredNodeID {
			lastError = ""
		}
		status := value.DNSStatus
		if status == "" || status == "disabled" {
			status = "pending"
		}
		if _, err := a.db.db.Exec(`UPDATE site_node_schedules SET desired_node_id=?,dns_status=?,last_error=?,updated_at_ms=? WHERE site_id=?`,
			nullableNodeID(desired), status, lastError, now.UnixMilli(), value.SiteID); err != nil {
			return err
		}
	}
	return nil
}

func agentConfigHash(config AgentRuntimeConfig) (string, error) {
	config.ConfigHash = ""
	data, err := json.Marshal(config)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func readBoundedPrivateFile(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("TLS file path is unavailable")
	}
	data, err := os.ReadFile(path) // #nosec G304 -- path is captured from administrator-controlled panel TLS configuration.
	if err != nil {
		return "", err
	}
	if len(data) == 0 || len(data) > 2<<20 {
		return "", errors.New("TLS file has an invalid size")
	}
	return string(data), nil
}

func (a *App) buildAgentConfig(token string, now time.Time) (AgentRuntimeConfig, error) {
	node, err := a.db.nodeByAgentToken(token, now)
	if err != nil {
		return AgentRuntimeConfig{}, err
	}
	if err := a.refreshSiteAssignments(now); err != nil {
		return AgentRuntimeConfig{}, err
	}
	rows, err := a.db.db.Query(`SELECT s.id,s.public_host,s.target_url,s.playback_target_url,s.playback_mode,s.stream_hosts,s.upstream_headers
		FROM site_node_schedules n JOIN sites s ON s.id=n.site_id
		WHERE n.enabled=1 AND (n.desired_node_id=? OR n.applied_node_id=?) AND s.enabled=1 ORDER BY s.id`, node.ID, node.ID)
	if err != nil {
		return AgentRuntimeConfig{}, err
	}
	type pendingRoute struct {
		route                      AgentSiteRoute
		storedHeaders, streamHosts string
	}
	pending := make([]pendingRoute, 0)
	dynamicKey := deriveNodeRuntimeKey(a.dynamicRouteKey, node.GUID, "dynamic-routes")
	for rows.Next() {
		var value pendingRoute
		if err := rows.Scan(&value.route.SiteID, &value.route.Host, &value.route.TargetURL, &value.route.PlaybackTargetURL, &value.route.PlaybackMode, &value.streamHosts, &value.storedHeaders); err != nil {
			_ = rows.Close()
			return AgentRuntimeConfig{}, err
		}
		pending = append(pending, value)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return AgentRuntimeConfig{}, err
	}
	if err := rows.Close(); err != nil {
		return AgentRuntimeConfig{}, err
	}
	routes := make([]AgentSiteRoute, 0, len(pending))
	for _, value := range pending {
		route := value.route
		storedHeaders, streamHosts := value.storedHeaders, value.streamHosts
		target, err := normalizeTargetURL(route.TargetURL)
		if err != nil {
			return AgentRuntimeConfig{}, fmt.Errorf("site %d target: %w", route.SiteID, err)
		}
		route.TargetURL = target.String()
		playbackTarget, _, err := resolvePlaybackConfiguration(route.PlaybackTargetURL, streamHosts)
		if err != nil {
			return AgentRuntimeConfig{}, fmt.Errorf("site %d playback target: %w", route.SiteID, err)
		}
		if playbackTarget != nil {
			route.PlaybackTargetURL = playbackTarget.String()
		}
		if strings.TrimSpace(streamHosts) != "" {
			if err := json.Unmarshal([]byte(streamHosts), &route.StreamHosts); err != nil {
				return AgentRuntimeConfig{}, fmt.Errorf("site %d stream hosts: %w", route.SiteID, err)
			}
		}
		if route.StreamHosts == nil {
			route.StreamHosts = []string{}
		}
		policy, err := resolveUpstreamHeaderPolicy(storedHeaders, a.pm.upstreamHeaderKey, target)
		if err != nil {
			return AgentRuntimeConfig{}, fmt.Errorf("site %d headers: %w", route.SiteID, err)
		}
		if len(policy.values) > 0 {
			route.Headers = map[string][]string(policy.values)
		}
		site, getErr := a.db.GetSite(route.SiteID)
		if getErr != nil {
			return AgentRuntimeConfig{}, getErr
		}
		route.Site = *site
		route.Site.TrafficQuota = 0
		route.Site.TrafficUsed = 0
		route.Site.TrafficUsedIn = 0
		route.Site.TrafficUsedOut = 0
		route.Site.MediaMovieCount = 0
		route.Site.MediaSeriesCount = 0
		route.Site.MediaEpisodeCount = 0
		route.Site.MediaCountUpdatedMS = 0
		route.Site.CreatedAt = ""
		route.Site.UpdatedAt = ""
		route.Site.UpstreamHeaders = nil
		route.FailoverTargets = site.FailoverTargets
		route.FailoverLines = site.StoredFailoverLines
		route.StreamHostsRaw = site.StreamHosts
		route.DynamicSources = site.StoredDynamicDiscoverySources
		route.DynamicRules = site.StoredDynamicDomainRules
		route.Host = strings.ToLower(strings.TrimSpace(route.Host))
		routes = append(routes, route)
	}
	// Keep the legacy field names in the Agent wire contract during rolling
	// upgrades. Their values now describe one HTTPS-only listener.
	config := AgentRuntimeConfig{SchemaVersion: agentConfigSchemaVersion, NodeGUID: node.GUID, EntryMode: "direct",
		HTTPPort: 0, HTTPSPort: node.Port, DynamicKey: encodeRuntimeKey(dynamicKey), Routes: routes}
	config.AgentVersion, config.AgentSHA256, _ = agentBinaryIdentity()
	if len(routes) > 0 {
		if a.panelCertificates == nil {
			return AgentRuntimeConfig{}, errors.New("edge TLS certificate is unavailable")
		}
		edgeCertFile, edgeKeyFile, pathErr := a.panelCertificates.nodeEdgeTLSPaths(node.GUID)
		if pathErr != nil {
			return AgentRuntimeConfig{}, pathErr
		}
		config.CertificatePEM, err = readBoundedPrivateFile(edgeCertFile)
		if err != nil {
			return AgentRuntimeConfig{}, fmt.Errorf("read edge TLS certificate: %w", err)
		}
		config.PrivateKeyPEM, err = readBoundedPrivateFile(edgeKeyFile)
		if err != nil {
			return AgentRuntimeConfig{}, fmt.Errorf("read edge TLS private key: %w", err)
		}
		if _, err := tls.X509KeyPair([]byte(config.CertificatePEM), []byte(config.PrivateKeyPEM)); err != nil {
			return AgentRuntimeConfig{}, fmt.Errorf("edge TLS certificate/key pair is invalid: %w", err)
		}
	}
	config.ConfigHash, err = agentConfigHash(config)
	if err != nil {
		return AgentRuntimeConfig{}, err
	}
	_, err = a.db.db.Exec("UPDATE control_nodes SET desired_config_hash=?,updated_at_ms=? WHERE id=?", config.ConfigHash, now.UnixMilli(), node.ID)
	if err != nil {
		return AgentRuntimeConfig{}, err
	}
	_, err = a.db.db.Exec("UPDATE site_node_schedules SET config_hash=?,updated_at_ms=? WHERE enabled=1 AND desired_node_id=?", config.ConfigHash, now.UnixMilli(), node.ID)
	return config, err
}

func (a *App) handleAgentConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	config, err := a.buildAgentConfig(requestBearerToken(r), time.Now())
	if errors.Is(err, errInvalidAgentToken) {
		a.jsonErr(w, http.StatusUnauthorized, "invalid agent token")
		return
	}
	if err != nil {
		a.jsonErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	a.jsonOK(w, config)
}

func (a *App) handleSiteNodeSchedules(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if err := a.refreshSiteAssignments(time.Now()); err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "site schedules unavailable")
		return
	}
	values, err := a.db.ListSiteNodeSchedules()
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "site schedules unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	a.jsonOK(w, map[string]any{"sites": values})
}

func siteScheduleID(path string) (int64, error) {
	value := strings.Trim(strings.TrimPrefix(path, "/api/node-scheduler/sites/"), "/")
	if value == "" || strings.Contains(value, "/") {
		return 0, errNodeNotFound
	}
	id, err := parsePositiveInt64(value)
	if err != nil {
		return 0, errNodeNotFound
	}
	return id, nil
}

func parsePositiveInt64(value string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

func (a *App) handleSiteNodeScheduleByID(w http.ResponseWriter, r *http.Request) {
	id, err := siteScheduleID(r.URL.Path)
	if err != nil {
		a.jsonErr(w, http.StatusNotFound, "site not found")
		return
	}
	if r.Method != http.MethodPut {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var input struct {
		Enabled     bool   `json:"enabled"`
		Mode        string `json:"mode"`
		FixedNodeID int64  `json:"fixed_node_id"`
	}
	if err := decodeJSONBody(w, r, &input); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	if input.Enabled {
		if site, siteErr := a.db.GetSite(id); siteErr == nil {
			if settings, settingsErr := a.db.PanelSettings(); settingsErr == nil && strings.TrimSpace(settings.RouteDomain) != "" && !wildcardDomainCoversHost(settings.RouteDomain, site.PublicHost) {
				a.jsonErr(w, http.StatusBadRequest, fmt.Sprintf("edge wildcard certificate does not cover host %q", site.PublicHost))
				return
			}
			if input.Mode == "fixed" && input.FixedNodeID > 0 && a.panelCertificates != nil {
				if node, nodeErr := a.db.controlNodeByID(input.FixedNodeID, time.Now()); nodeErr == nil {
					if certFile, _, pathErr := a.panelCertificates.nodeEdgeTLSPaths(node.GUID); pathErr == nil {
						if _, statErr := os.Stat(certFile); statErr == nil {
							if certErr := certificateCoversHost(certFile, site.PublicHost); certErr != nil {
								a.jsonErr(w, http.StatusBadRequest, certErr.Error())
								return
							}
						}
					}
				}
			}
		}
	}
	previous, _ := a.db.siteNodeSchedule(id)
	value, err := a.db.SaveSiteNodeSchedule(id, input.Enabled, input.Mode, input.FixedNodeID, time.Now())
	if err != nil {
		writeNodeAPIError(a, w, err)
		return
	}
	if !input.Enabled && previous.cfRecordID != "" {
		if err := a.deleteTrackedSiteDNS(r.Context(), previous); err != nil {
			_, _ = a.db.SaveSiteNodeSchedule(id, true, previous.Mode, previous.FixedNodeID, time.Now())
			a.jsonErr(w, http.StatusBadGateway, "DNS cleanup failed: "+err.Error())
			return
		}
		value, _ = a.db.siteNodeSchedule(id)
	}
	a.jsonOK(w, value)
}

func (a *App) cloudflareForScheduling() (*cloudflareClient, error) {
	settings, err := a.db.PanelSettings()
	if err != nil {
		return nil, err
	}
	if settings.ACMEDNSProvider != "cloudflare" || settings.ACMETokenCiphertext == "" {
		return nil, errors.New("Cloudflare DNS credentials are not configured")
	}
	token, err := decryptPanelACMEToken(settings.ACMETokenCiphertext)
	if err != nil {
		return nil, err
	}
	return &cloudflareClient{token: token, httpClient: &http.Client{Timeout: 15 * time.Second}, apiBase: "https://api.cloudflare.com/client/v4"}, nil
}

type cloudflareAddressRecord struct{ ID, Type, Name, Content string }

func (c *cloudflareClient) exactAddressRecords(ctx context.Context, zoneID, name string) ([]cloudflareAddressRecord, error) {
	result, err := c.request(ctx, http.MethodGet, "/zones/"+url.PathEscape(zoneID)+"/dns_records?name="+url.QueryEscape(name)+"&per_page=100", nil)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(result, &raw); err != nil {
		return nil, errors.New("Cloudflare DNS returned invalid records")
	}
	values := make([]cloudflareAddressRecord, 0, len(raw))
	for _, item := range raw {
		if (item.Type == "A" || item.Type == "AAAA") && strings.EqualFold(item.Name, name) {
			values = append(values, cloudflareAddressRecord{ID: item.ID, Type: item.Type, Name: item.Name, Content: item.Content})
		}
	}
	return values, nil
}

func (c *cloudflareClient) writeAddressRecord(ctx context.Context, zoneID, recordID, recordType, name, address string) (string, error) {
	body, _ := json.Marshal(map[string]any{"type": recordType, "name": name, "content": address, "ttl": 60, "proxied": false})
	method, path := http.MethodPost, "/zones/"+url.PathEscape(zoneID)+"/dns_records"
	if recordID != "" {
		method, path = http.MethodPut, path+"/"+url.PathEscape(recordID)
	}
	result, err := c.request(ctx, method, path, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	var record struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(result, &record); err != nil || record.ID == "" {
		return "", errors.New("Cloudflare DNS did not return a record ID")
	}
	return record.ID, nil
}

func (a *App) deleteTrackedSiteDNS(ctx context.Context, schedule SiteNodeSchedule) error {
	if schedule.cfRecordID == "" {
		return nil
	}
	cf, err := a.cloudflareForScheduling()
	if err != nil {
		return err
	}
	if schedule.cfZoneID == "" {
		return errors.New("tracked DNS zone is missing")
	}
	if err := cf.deleteRecord(ctx, schedule.cfZoneID, schedule.cfRecordID); err != nil {
		return err
	}
	_, err = a.db.db.Exec(`UPDATE site_node_schedules SET cf_zone_id='',cf_record_id='',cf_record_type='',applied_node_id=NULL,
		applied_address='',dns_status='disabled',last_error='',updated_at_ms=? WHERE site_id=?`, time.Now().UnixMilli(), schedule.SiteID)
	return err
}

func nodeDialAddress(address string, port int) (string, error) {
	host := strings.TrimSpace(address)
	if parsed := net.ParseIP(host); parsed == nil {
		return "", errors.New("node address must be an IPv4 or IPv6 address")
	}
	return net.JoinHostPort(host, fmt.Sprint(port)), nil
}

func nodeHTTPSProbePort(node ControlNode) int {
	return node.Port
}

func probeScheduledNode(ctx context.Context, node ControlNode, host string) error {
	address, err := nodeDialAddress(node.Address, nodeHTTPSProbePort(node))
	if err != nil {
		return err
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		},
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host},
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/.well-known/meridian-agent-health", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Meridian-Node") != node.GUID {
		return errors.New("node entry health check did not identify the selected Agent")
	}
	return nil
}

func (a *App) reconcileOneSiteSchedule(ctx context.Context, schedule SiteNodeSchedule, now time.Time) error {
	if !schedule.Enabled {
		return nil
	}
	site, err := a.db.GetSite(schedule.SiteID)
	if err != nil {
		return err
	}
	if !site.Enabled {
		if schedule.cfRecordID != "" {
			return a.deleteTrackedSiteDNS(ctx, schedule)
		}
		_, err := a.db.db.Exec("UPDATE site_node_schedules SET dns_status='disabled',last_error='',updated_at_ms=? WHERE site_id=?", now.UnixMilli(), schedule.SiteID)
		return err
	}
	if schedule.DesiredNodeID <= 0 {
		return errors.New("no eligible node is available")
	}
	node, err := a.db.controlNodeByID(schedule.DesiredNodeID, now)
	if err != nil {
		return err
	}
	if a.panelCertificates == nil {
		return errors.New("edge TLS certificate is unavailable")
	}
	edgeCertFile, _, pathErr := a.panelCertificates.nodeEdgeTLSPaths(node.GUID)
	if pathErr != nil {
		return pathErr
	}
	if err := certificateCoversHost(edgeCertFile, schedule.PublicHost); err != nil {
		return err
	}
	if node.DesiredConfigHash == "" || node.AppliedConfigHash != node.DesiredConfigHash {
		return errors.New("Agent has not applied the desired configuration")
	}
	if node.AgentListenerError != "" {
		return errors.New(node.AgentListenerError)
	}
	if err := probeScheduledNode(ctx, node, schedule.PublicHost); err != nil {
		return fmt.Errorf("entry health check: %w", err)
	}
	if err := a.db.clearSiteNodeProbeFailure(schedule.SiteID, node.ID); err != nil {
		return fmt.Errorf("clear entry health cooldown: %w", err)
	}
	ip := net.ParseIP(strings.TrimSpace(node.Address))
	if ip == nil {
		return errors.New("node address must be an IP address for DNS scheduling")
	}
	recordType := "A"
	if ip.To4() == nil {
		recordType = "AAAA"
	}
	zoneName, err := publicsuffix.EffectiveTLDPlusOne(schedule.PublicHost)
	if err != nil {
		return err
	}
	cf, err := a.cloudflareForScheduling()
	if err != nil {
		return err
	}
	zoneID := schedule.cfZoneID
	if zoneID == "" {
		zoneID, err = cf.findZone(ctx, zoneName)
		if err != nil {
			return err
		}
	}
	if schedule.cfRecordID == "" {
		records, err := cf.exactAddressRecords(ctx, zoneID, schedule.PublicHost)
		if err != nil {
			return err
		}
		if len(records) > 0 {
			return errors.New("an untracked exact A/AAAA record already exists; Meridian will not overwrite it")
		}
	}
	recordID, err := cf.writeAddressRecord(ctx, zoneID, schedule.cfRecordID, recordType, schedule.PublicHost, ip.String())
	if err != nil {
		return err
	}
	_, err = a.db.db.Exec(`UPDATE site_node_schedules SET applied_node_id=?,cf_zone_id=?,cf_record_id=?,cf_record_type=?,
		applied_address=?,dns_status='active',last_error='',updated_at_ms=? WHERE site_id=?`, node.ID, zoneID, recordID,
		recordType, ip.String(), now.UnixMilli(), schedule.SiteID)
	return err
}

func (a *App) reconcileSiteNodeScheduling(ctx context.Context) {
	now := time.Now()
	if err := a.refreshSiteAssignments(now); err != nil {
		log.Printf("[node-scheduler] refresh assignments failed: %v", err)
		return
	}
	values, err := a.db.ListSiteNodeSchedules()
	if err != nil {
		log.Printf("[node-scheduler] list site schedules failed: %v", err)
		return
	}
	for _, value := range values {
		if !value.Enabled {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := a.reconcileOneSiteSchedule(probeCtx, value, now)
		cancel()
		if err != nil {
			log.Printf("[node-scheduler] site %d waiting: %v", value.SiteID, err)
			if value.Mode == "global" && value.DesiredNodeID > 0 && strings.Contains(err.Error(), "entry health check") {
				var schedulerMode string
				if modeErr := a.db.db.QueryRow("SELECT mode FROM node_scheduler_settings WHERE id=1").Scan(&schedulerMode); modeErr == nil && schedulerMode == "auto" {
					if cooldownErr := a.db.recordSiteNodeProbeFailure(value.SiteID, value.DesiredNodeID, err, now); cooldownErr != nil {
						log.Printf("[node-scheduler] record site %d probe cooldown failed: %v", value.SiteID, cooldownErr)
					}
				}
			}
			_, _ = a.db.db.Exec("UPDATE site_node_schedules SET dns_status='waiting',last_error=?,updated_at_ms=? WHERE site_id=?", err.Error(), now.UnixMilli(), value.SiteID)
		}
	}
}

func runSiteNodeScheduler(ctx context.Context, app *App) {
	if app == nil {
		return
	}
	app.reconcileSiteNodeScheduling(ctx)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			app.reconcileSiteNodeScheduling(ctx)
		}
	}
}

func (a *App) removeSiteNodeSchedule(ctx context.Context, siteID int64) error {
	var exists int
	if err := a.db.db.QueryRow("SELECT COUNT(*) FROM site_node_schedules WHERE site_id=?", siteID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	value, err := a.db.siteNodeSchedule(siteID)
	if err != nil && !errors.Is(err, errNodeNotFound) {
		return err
	}
	if err == nil && value.cfRecordID != "" {
		if err := a.deleteTrackedSiteDNS(ctx, value); err != nil {
			return err
		}
	}
	_, err = a.db.db.Exec("DELETE FROM site_node_schedules WHERE site_id=?", siteID)
	return err
}

func (a *App) prepareNodeDeletion(ctx context.Context, nodeID int64) error {
	values, err := a.db.ListSiteNodeSchedules()
	if err != nil {
		return err
	}
	for _, value := range values {
		if value.FixedNodeID != nodeID && value.DesiredNodeID != nodeID && value.AppliedNodeID != nodeID {
			continue
		}
		if value.cfRecordID != "" {
			if err := a.deleteTrackedSiteDNS(ctx, value); err != nil {
				return err
			}
		}
		if _, err := a.db.db.Exec(`UPDATE site_node_schedules SET enabled=0,fixed_node_id=NULL,desired_node_id=NULL,
			applied_node_id=NULL,dns_status='disabled',last_error='',updated_at_ms=? WHERE site_id=?`, time.Now().UnixMilli(), value.SiteID); err != nil {
			return err
		}
	}
	return nil
}
