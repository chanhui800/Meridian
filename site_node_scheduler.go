package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
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
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	agentConfigSchemaVersion = 1
	// siteNodeDrainWindow is an emergency upper bound only. Normal drains are
	// retired by a final SiteStats snapshot (NodeSiteStat.Final) and its ACK;
	// the long TTL prevents a long-lived playback from losing billable traffic
	// while still bounding stale authorization after an Agent disappears.
	siteNodeDrainWindow = 24 * time.Hour
)

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
	ConfigPendingSinceMS int64  `json:"config_pending_since_ms"`
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
	// TrafficCycleUsage is the Controller's authoritative usage for the
	// currently active billing cycle. It is intentionally excluded from the
	// runtime config hash because it changes with telemetry, while the quota
	// itself remains part of the route identity.
	TrafficCycleUsage   int64  `json:"traffic_cycle_usage,omitempty"`
	TrafficCycleStartMS int64  `json:"traffic_cycle_start_ms,omitempty"`
	TrafficBillingMode  string `json:"traffic_billing_mode,omitempty"`
}

type AgentRuntimeConfig struct {
	SchemaVersion        int              `json:"schema_version"`
	ConfigHash           string           `json:"config_hash"`
	ConfigRevision       int64            `json:"config_revision,omitempty"`
	NodeGUID             string           `json:"node_guid"`
	EntryMode            string           `json:"entry_mode"`
	HTTPPort             int              `json:"http_port"`
	HTTPSPort            int              `json:"https_port"`
	CertificatePEM       string           `json:"certificate_pem,omitempty"`
	PrivateKeyPEM        string           `json:"private_key_pem,omitempty"`
	DynamicKey           string           `json:"dynamic_key,omitempty"`
	ProbeSecret          string           `json:"probe_secret,omitempty"`
	AgentVersion         string           `json:"agent_version,omitempty"`
	AgentSHA256          string           `json:"agent_sha256,omitempty"`
	AgentDownloadURL     string           `json:"agent_download_url,omitempty"`
	CacheClearGeneration int64            `json:"cache_clear_generation,omitempty"`
	ForceStopSiteIDs     []int64          `json:"force_stop_site_ids,omitempty"`
	Routes               []AgentSiteRoute `json:"routes"`
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
		&value.AgentBootID, &value.AgentRequestCount, &value.AgentLastRequestAtMS, &value.AgentLastStatus, &value.ConfigPendingSinceMS, &value.UpdatedAtMS)
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
	COALESCE(n.agent_boot_id,''),COALESCE(n.agent_request_count,0),COALESCE(n.agent_last_request_at_ms,0),COALESCE(n.agent_last_status,0),COALESCE(n.config_pending_since_ms,0),COALESCE(n.updated_at_ms,0)
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
	var oldEnabled int
	var oldMode string
	var oldFixed, oldDesired, oldApplied sql.NullInt64
	var oldPendingSince int64
	lookupErr := d.db.QueryRow("SELECT enabled,mode,fixed_node_id,desired_node_id,applied_node_id,config_pending_since_ms FROM site_node_schedules WHERE site_id=?", siteID).
		Scan(&oldEnabled, &oldMode, &oldFixed, &oldDesired, &oldApplied, &oldPendingSince)
	if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
		return SiteNodeSchedule{}, lookupErr
	}
	oldFixedID := int64(0)
	if oldFixed.Valid {
		oldFixedID = oldFixed.Int64
	}
	unchanged := lookupErr == nil && oldEnabled == sqliteBool(enabled) && oldMode == mode && oldFixedID == fixedNodeID
	if unchanged {
		// Re-saving the same scheduler form is a no-op. In particular, do not
		// turn an already active DNS record back into pending or clear its
		// readiness fields.
		return d.siteNodeSchedule(siteID)
	}
	pendingSince := int64(0)
	if enabled {
		pendingSince = now.UnixMilli()
		if lookupErr == nil && oldEnabled != 0 && oldMode == mode && oldFixedID == fixedNodeID {
			// Saving an unchanged form must not restart the configuration
			// application deadline. Only a real assignment/config change starts it.
			pendingSince = oldPendingSince
		}
	}
	status := "disabled"
	if enabled {
		status = "pending"
	}
	tx, err := d.db.Begin()
	if err != nil {
		return SiteNodeSchedule{}, err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO site_node_schedules
		(site_id,enabled,mode,fixed_node_id,dns_status,config_pending_since_ms,created_at_ms,updated_at_ms)
		VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(site_id) DO UPDATE SET enabled=excluded.enabled,mode=excluded.mode,
		fixed_node_id=excluded.fixed_node_id,dns_status=excluded.dns_status,config_pending_since_ms=excluded.config_pending_since_ms,
		last_error='',updated_at_ms=excluded.updated_at_ms`,
		siteID, sqliteBool(enabled), mode, nullableNodeID(fixedNodeID), status, pendingSince, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return SiteNodeSchedule{}, err
	}
	nodeIDs := []int64{fixedNodeID}
	if oldFixed.Valid {
		nodeIDs = append(nodeIDs, oldFixed.Int64)
	}
	if oldDesired.Valid {
		nodeIDs = append(nodeIDs, oldDesired.Int64)
	}
	if oldApplied.Valid {
		nodeIDs = append(nodeIDs, oldApplied.Int64)
	}
	if err := markAgentConfigsDirtyForNodeIDsTx(tx, nodeIDs...); err != nil {
		return SiteNodeSchedule{}, err
	}
	if err := tx.Commit(); err != nil {
		return SiteNodeSchedule{}, err
	}
	return d.siteNodeSchedule(siteID)
}

const siteNodeProbeCooldown = 2 * time.Minute

const (
	readinessCertificate = "certificate"
	readinessConfig      = "config"
	readinessListener    = "listener"
	readinessProbe       = "probe"
)

type nodeReadinessError struct {
	Kind string
	Err  error
}

func (e *nodeReadinessError) Error() string {
	if e == nil || e.Err == nil {
		return "node is not ready"
	}
	return e.Err.Error()
}

func (e *nodeReadinessError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func readinessError(kind string, err error) error {
	if err == nil {
		return nil
	}
	return &nodeReadinessError{Kind: kind, Err: err}
}

// siteScheduleConfigReady keeps DNS cutover tied to the route that is about
// to receive traffic. A node-level hash can still match while a newly moved
// site is waiting for the Agent to fetch and apply its next configuration.
func siteScheduleConfigReady(schedule SiteNodeSchedule, node ControlNode) bool {
	if schedule.ConfigPendingSinceMS > 0 {
		return false
	}
	scheduleHash := strings.TrimSpace(schedule.ConfigHash)
	desiredHash := strings.TrimSpace(node.DesiredConfigHash)
	appliedHash := strings.TrimSpace(node.AppliedConfigHash)
	if scheduleHash == "" || desiredHash == "" || appliedHash == "" {
		return false
	}
	return scheduleHash == desiredHash && scheduleHash == appliedHash
}

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
	// Keep the currently running Agent authorized until a replacement enrollment
	// commits. A pending one-time enrollment token is allowed to expire without
	// revoking the old long-lived token; this makes regenerating a script safe
	// even when the operator does not run it immediately.
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
		eligible[node.ID] = nodeAssignmentEligible(node)
	}
	values, err := a.db.ListSiteNodeSchedules()
	if err != nil {
		return err
	}
	type assignmentUpdate struct {
		siteID        int64
		desired       int64
		status        string
		lastError     string
		pendingSince  int64
		desiredChange bool
	}
	updates := make([]assignmentUpdate, 0)
	changedNodeIDs := make([]int64, 0)
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
			fallback := int64(0)
			for _, candidate := range snapshot.Nodes {
				if !nodeAssignmentEligible(candidate) {
					continue
				}
				// A probe cooldown means "retry this node later"; it must not
				// make an otherwise online node disappear from the assignment.
				// Keep a fallback so a single-node deployment continues probing
				// and can recover as soon as the Agent comes back.
				if fallback == 0 {
					fallback = candidate.ID
				}
				if !cooldowns[candidate.ID] {
					desired = candidate.ID
					break
				}
			}
			if desired == 0 {
				desired = fallback
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
		if desired != value.DesiredNodeID {
			status = "pending"
		}
		desiredChanged := desired != value.DesiredNodeID
		statusChanged := status != value.DNSStatus || lastError != value.LastError
		if desiredChanged || statusChanged {
			pendingSince := value.ConfigPendingSinceMS
			if desiredChanged {
				pendingSince = now.UnixMilli()
			}
			updates = append(updates, assignmentUpdate{
				siteID:        value.SiteID,
				desired:       desired,
				status:        status,
				lastError:     lastError,
				pendingSince:  pendingSince,
				desiredChange: desiredChanged,
			})
			if desiredChanged {
				changedNodeIDs = append(changedNodeIDs, value.DesiredNodeID, desired)
			}
		}
	}
	if len(updates) == 0 {
		return nil
	}
	tx, err := a.db.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	assignmentsChanged := false
	for _, update := range updates {
		if update.desiredChange {
			if _, err := tx.Exec(`UPDATE site_node_schedules SET desired_node_id=?,dns_status=?,last_error=?,config_pending_since_ms=?,updated_at_ms=? WHERE site_id=?`,
				nullableNodeID(update.desired), update.status, update.lastError, update.pendingSince, now.UnixMilli(), update.siteID); err != nil {
				return err
			}
			assignmentsChanged = true
		} else if _, err := tx.Exec(`UPDATE site_node_schedules SET dns_status=?,last_error=?,updated_at_ms=? WHERE site_id=?`,
			update.status, update.lastError, now.UnixMilli(), update.siteID); err != nil {
			return err
		}
	}
	if assignmentsChanged {
		if err := markAgentConfigsDirtyForNodeIDsTx(tx, changedNodeIDs...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// agentSiteHashBase lets the runtime hash use the Site wire shape without
// making the UI-only icon fields part of the Agent contract. Older Agents
// ignore icon_name/icon_url while decoding a route, so including those fields
// in the Controller hash would make them reject otherwise valid configs.
type agentSiteHashBase Site

type agentSiteHash struct {
	agentSiteHashBase
	IconName string `json:"icon_name,omitempty"`
	IconURL  string `json:"icon_url,omitempty"`
}

type agentSiteRouteHash struct {
	SiteID            int64               `json:"site_id"`
	Host              string              `json:"host"`
	TargetURL         string              `json:"target_url"`
	PlaybackTargetURL string              `json:"playback_target_url,omitempty"`
	StreamHosts       []string            `json:"stream_hosts,omitempty"`
	PlaybackMode      string              `json:"playback_mode,omitempty"`
	Headers           map[string][]string `json:"headers,omitempty"`
	Site              agentSiteHash       `json:"site"`
	FailoverTargets   string              `json:"failover_targets_raw,omitempty"`
	FailoverLines     string              `json:"failover_lines_raw,omitempty"`
	StreamHostsRaw    string              `json:"stream_hosts_raw,omitempty"`
	UpstreamHeaders   string              `json:"upstream_headers_raw,omitempty"`
	DynamicSources    string              `json:"dynamic_sources_raw,omitempty"`
	DynamicRules      string              `json:"dynamic_rules_raw,omitempty"`
}

type agentRuntimeConfigHash struct {
	SchemaVersion int    `json:"schema_version"`
	ConfigHash    string `json:"config_hash"`
	// ConfigRevision is an ordering/acknowledgement field, not runtime
	// behavior, so deliberately keep it outside both legacy and modern hashes.
	NodeGUID             string               `json:"node_guid"`
	EntryMode            string               `json:"entry_mode"`
	HTTPPort             int                  `json:"http_port"`
	HTTPSPort            int                  `json:"https_port"`
	CertificatePEM       string               `json:"certificate_pem,omitempty"`
	PrivateKeyPEM        string               `json:"private_key_pem,omitempty"`
	DynamicKey           string               `json:"dynamic_key,omitempty"`
	ProbeSecret          string               `json:"probe_secret,omitempty"`
	AgentVersion         string               `json:"agent_version,omitempty"`
	AgentSHA256          string               `json:"agent_sha256,omitempty"`
	AgentDownloadURL     string               `json:"agent_download_url,omitempty"`
	CacheClearGeneration int64                `json:"cache_clear_generation,omitempty"`
	ForceStopSiteIDs     []int64              `json:"force_stop_site_ids,omitempty"`
	Routes               []agentSiteRouteHash `json:"routes"`
}

func agentConfigHashPayload(config AgentRuntimeConfig, legacy bool) agentRuntimeConfigHash {
	config.ConfigHash = ""
	if !legacy {
		config.AgentVersion = ""
		config.AgentSHA256 = ""
	}
	config.AgentDownloadURL = ""
	config.ForceStopSiteIDs = append([]int64(nil), config.ForceStopSiteIDs...)
	sort.Slice(config.ForceStopSiteIDs, func(i, j int) bool { return config.ForceStopSiteIDs[i] < config.ForceStopSiteIDs[j] })
	if legacy {
		config.CacheClearGeneration = 0
		config.ForceStopSiteIDs = nil
		// ProbeSecret was added after the legacy Agent contract. Older Agents
		// ignore the field, so omit it from the compatibility hash while current
		// Agents use the runtime hash above and authenticate their health probes.
		config.ProbeSecret = ""
	}
	routes := make([]agentSiteRouteHash, len(config.Routes))
	for i, route := range config.Routes {
		hashSite := route.Site
		// Traffic usage is a live accounting value. It is delivered in the
		// route so an Agent can enforce the quota, but must not participate in
		// the runtime identity or every report would force a full proxy apply.
		hashSite.TrafficUsed = 0
		hashSite.TrafficUsedIn = 0
		hashSite.TrafficUsedOut = 0
		routes[i] = agentSiteRouteHash{
			SiteID:            route.SiteID,
			Host:              route.Host,
			TargetURL:         route.TargetURL,
			PlaybackTargetURL: route.PlaybackTargetURL,
			StreamHosts:       route.StreamHosts,
			PlaybackMode:      route.PlaybackMode,
			Headers:           route.Headers,
			Site:              agentSiteHash{agentSiteHashBase: agentSiteHashBase(hashSite)},
			FailoverTargets:   route.FailoverTargets,
			FailoverLines:     route.FailoverLines,
			StreamHostsRaw:    route.StreamHostsRaw,
			UpstreamHeaders:   route.UpstreamHeaders,
			DynamicSources:    route.DynamicSources,
			DynamicRules:      route.DynamicRules,
		}
	}
	return agentRuntimeConfigHash{
		SchemaVersion:        config.SchemaVersion,
		ConfigHash:           config.ConfigHash,
		NodeGUID:             config.NodeGUID,
		EntryMode:            config.EntryMode,
		HTTPPort:             config.HTTPPort,
		HTTPSPort:            config.HTTPSPort,
		CertificatePEM:       config.CertificatePEM,
		PrivateKeyPEM:        config.PrivateKeyPEM,
		DynamicKey:           config.DynamicKey,
		ProbeSecret:          config.ProbeSecret,
		AgentVersion:         config.AgentVersion,
		AgentSHA256:          config.AgentSHA256,
		AgentDownloadURL:     config.AgentDownloadURL,
		CacheClearGeneration: config.CacheClearGeneration,
		ForceStopSiteIDs:     config.ForceStopSiteIDs,
		Routes:               routes,
	}
}

func hashAgentConfigPayload(config AgentRuntimeConfig, legacy bool) (string, error) {
	data, err := json.Marshal(agentConfigHashPayload(config, legacy))
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

// agentConfigHash covers only the runtime behavior applied by the Agent.
// Release metadata and UI-only site icon fields are intentionally excluded so
// a GitHub outage, a new checksum, or an icon change cannot cause a route
// config restart by itself.
func agentConfigHash(config AgentRuntimeConfig) (string, error) {
	return hashAgentConfigPayload(config, false)
}

// agentConfigLegacyHash preserves the v1.9.29 wire/hash contract for Agents
// that have not started sending their version header yet. v1.9.29 knew about
// AgentVersion and AgentSHA256, but not AgentDownloadURL, ProbeSecret, or
// CacheClearGeneration.
func agentConfigLegacyHash(config AgentRuntimeConfig) (string, error) {
	return hashAgentConfigPayload(config, true)
}

func agentUsesRuntimeConfigHash(version string) bool {
	version = strings.TrimSpace(strings.TrimPrefix(version, "v"))
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	patch, errPatch := strconv.Atoi(parts[2])
	if errMajor != nil || errMinor != nil || errPatch != nil {
		return false
	}
	if major != 1 {
		return major > 1
	}
	if minor != 9 {
		return minor > 9
	}
	return patch >= 30
}

// agentSupportsProbeSecret reports whether the Agent understands the
// probe_secret field that was added to the runtime configuration contract in
// v1.9.43. Older Agents ignore unknown JSON fields, but their configuration
// hash cannot include a field they never saw. Keep that field out of the hash
// during the rolling upgrade window so they can validate and apply the config
// before downloading the current Agent binary.
func agentSupportsProbeSecret(version string) bool {
	version = strings.TrimSpace(strings.TrimPrefix(version, "v"))
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	patch, errPatch := strconv.Atoi(parts[2])
	if errMajor != nil || errMinor != nil || errPatch != nil {
		return false
	}
	if major != 1 {
		return major > 1
	}
	if minor != 9 {
		return minor > 9
	}
	return patch >= 43
}

// agentSupportsCacheClear reports whether the Agent understands the
// cache_clear_generation field in the runtime config. This field was added in
// the next controller/Agent rollout after the v1.9.49 wire contract. Older
// Agents ignore the JSON field, so the controller must calculate the hash over
// the shape they actually know until they upgrade.
func agentSupportsCacheClear(version string) bool {
	version = strings.TrimSpace(strings.TrimPrefix(version, "v"))
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	major, errMajor := strconv.Atoi(parts[0])
	minor, errMinor := strconv.Atoi(parts[1])
	patch, errPatch := strconv.Atoi(parts[2])
	if errMajor != nil || errMinor != nil || errPatch != nil {
		return false
	}
	if major != 1 {
		return major > 1
	}
	if minor != 9 {
		return minor > 9
	}
	return patch >= 50
}

func agentConfigHashForVersion(config AgentRuntimeConfig, version string) (string, error) {
	if !agentSupportsProbeSecret(version) {
		config.ProbeSecret = ""
	}
	if !agentSupportsCacheClear(version) {
		config.CacheClearGeneration = 0
	}
	if agentUsesRuntimeConfigHash(version) {
		return agentConfigHash(config)
	}
	return agentConfigLegacyHash(config)
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
	return a.buildAgentConfigForPlatform(token, now, "")
}

func (a *App) buildAgentConfigForPlatform(token string, now time.Time, platform string) (AgentRuntimeConfig, error) {
	return a.buildAgentConfigForRequest(context.Background(), token, now, platform, "")
}

func (a *App) buildAgentConfigForRequest(ctx context.Context, token string, now time.Time, platform, clientVersion string) (AgentRuntimeConfig, error) {
	var node ControlNode
	var err error
	if identity, ok := agentCredentialFromContext(ctx); ok && identity.HasNode && identity.Token == token {
		node = identity.Node
	} else {
		node, err = a.db.nodeByAgentToken(token, now)
		if err != nil {
			return AgentRuntimeConfig{}, err
		}
	}
	if err := a.refreshSiteAssignments(now); err != nil {
		return AgentRuntimeConfig{}, err
	}
	settings := a.db.currentSystemSettings()
	cycleStart := trafficCycleStart(now, settings.TrafficResetDay, timezoneLocation(settings.ScheduleTimezone))
	cycleMode := trafficBillingModeLabel(settings.TrafficBillingMode)
	type pendingRoute struct {
		route                      AgentSiteRoute
		storedHeaders, streamHosts string
	}
	pending := make([]pendingRoute, 0)
	dynamicKey := deriveNodeRuntimeKey(a.dynamicRouteKey, node.GUID, "dynamic-routes")
	probeSecretText, probeErr := dNodeProbeSecret(a.db, node)
	if probeErr != nil {
		return AgentRuntimeConfig{}, probeErr
	}
	if _, probeErr := decodeNodeProbeSecret(probeSecretText); probeErr != nil {
		return AgentRuntimeConfig{}, probeErr
	}
	// Hold one SQLite read/write transaction for the route snapshot and the
	// desired-hash publication below. This prevents a site edit from being
	// observed half-way through config construction.
	tx, err := a.db.db.BeginTx(ctx, nil)
	if err != nil {
		return AgentRuntimeConfig{}, err
	}
	defer tx.Rollback()
	node, err = scanControlNode(tx.QueryRow(controlNodeSelect+" WHERE id=?", node.ID), now)
	if err != nil {
		return AgentRuntimeConfig{}, err
	}
	rows, err := tx.Query(`SELECT s.id,s.public_host,s.target_url,s.playback_target_url,s.playback_mode,s.stream_hosts,s.upstream_headers
		FROM site_node_schedules n JOIN sites s ON s.id=n.site_id
		WHERE n.enabled=1 AND s.enabled=1 AND (n.desired_node_id=? OR n.applied_node_id=?)
		ORDER BY s.id`, node.ID, node.ID)
	if err != nil {
		return AgentRuntimeConfig{}, err
	}
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
		site, getErr := getSiteTx(tx, route.SiteID)
		if getErr != nil {
			return AgentRuntimeConfig{}, getErr
		}
		route.Site = *site
		// Site icons are presentation-only metadata for the Controller UI. Do
		// not send them to Agents or let a later icon-pack change invalidate the
		// runtime route configuration.
		route.Site.IconName = ""
		route.Site.IconURL = ""
		route.Site.MediaMovieCount = 0
		route.Site.MediaSeriesCount = 0
		route.Site.MediaEpisodeCount = 0
		route.Site.MediaCountUpdatedMS = 0
		route.Site.CreatedAt = ""
		route.Site.UpdatedAt = ""
		route.Site.UpstreamHeaders = nil
		route.TrafficCycleUsage, err = sumTrafficSinceForSiteTx(tx, route.SiteID, cycleStart, cycleMode)
		if err != nil {
			return AgentRuntimeConfig{}, fmt.Errorf("site %d traffic usage: %w", route.SiteID, err)
		}
		if !cycleStart.IsZero() {
			route.TrafficCycleStartMS = cycleStart.UnixMilli()
		}
		route.TrafficBillingMode = cycleMode
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
	config := AgentRuntimeConfig{SchemaVersion: agentConfigSchemaVersion, ConfigRevision: node.ConfigRevision, NodeGUID: node.GUID, EntryMode: "direct",
		HTTPPort: 0, HTTPSPort: node.Port, DynamicKey: encodeRuntimeKey(dynamicKey), ProbeSecret: probeSecretText,
		CacheClearGeneration: node.CacheClearGeneration, Routes: routes}
	forceRows, forceErr := tx.Query("SELECT site_id FROM agent_route_revocations WHERE node_id=? ORDER BY site_id", node.ID)
	if forceErr != nil {
		return AgentRuntimeConfig{}, forceErr
	}
	for forceRows.Next() {
		var siteID int64
		if err := forceRows.Scan(&siteID); err != nil {
			_ = forceRows.Close()
			return AgentRuntimeConfig{}, err
		}
		if siteID > 0 {
			config.ForceStopSiteIDs = append(config.ForceStopSiteIDs, siteID)
		}
	}
	if err := forceRows.Close(); err != nil {
		return AgentRuntimeConfig{}, err
	}
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
	config.ConfigHash, err = agentConfigHashForVersion(config, clientVersion)
	if err != nil {
		return AgentRuntimeConfig{}, err
	}
	// Publish the snapshot with a compare-and-swap. If a site/node mutation
	// increments config_revision while this response was being assembled, do
	// not let the stale route set become the desired configuration.
	result, err := tx.Exec(`UPDATE control_nodes SET
		desired_config_hash=?, desired_config_revision=?,
		config_dirty=1, updated_at_ms=?
		WHERE id=? AND config_revision=?`, config.ConfigHash, config.ConfigRevision, now.UnixMilli(), node.ID, node.ConfigRevision)
	if err != nil {
		return AgentRuntimeConfig{}, err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return AgentRuntimeConfig{}, err
	}
	if rowsAffected != 1 {
		return AgentRuntimeConfig{}, errors.New("agent configuration changed while it was being built; retry")
	}
	if _, err := tx.Exec(`UPDATE site_node_schedules SET
		config_pending_since_ms=CASE WHEN config_hash<>? THEN ? ELSE config_pending_since_ms END,
		config_hash=?,updated_at_ms=? WHERE enabled=1 AND desired_node_id=?`,
		config.ConfigHash, now.UnixMilli(), config.ConfigHash, now.UnixMilli(), node.ID); err != nil {
		return AgentRuntimeConfig{}, err
	}
	if err := tx.Commit(); err != nil {
		return AgentRuntimeConfig{}, err
	}
	// Release metadata is advisory and deliberately excluded from the runtime
	// hash. Fetch it after the SQLite transaction so a slow/unavailable GitHub
	// request never monopolizes the controller's single database connection.
	// Legacy Agents do not send a platform header and therefore receive no
	// update metadata, preserving cross-architecture rollout safety.
	if platform != "" {
		if manifest, manifestErr := agentReleaseManifestForPlatform(ctx, platform); manifestErr == nil {
			config.AgentVersion = manifest.Version
			config.AgentSHA256 = manifest.SHA256
		}
	}
	return config, nil
}

func (a *App) handleAgentConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	platform, platformErr := requestedAgentPlatform(r)
	if platformErr != nil {
		a.jsonErr(w, http.StatusBadRequest, platformErr.Error())
		return
	}
	config, err := a.buildAgentConfigForRequest(r.Context(), requestBearerToken(r), time.Now(), platform, r.Header.Get(agentVersionHeader))
	if errors.Is(err, errInvalidAgentToken) {
		writeAgentAuthFailure(a, w, r)
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
	if err := deleteTrackedSiteDNSRemote(ctx, cf, schedule); err != nil {
		return err
	}
	_, err = a.db.db.Exec(`UPDATE site_node_schedules SET cf_zone_id='',cf_record_id='',cf_record_type='',applied_node_id=NULL,
		applied_address='',dns_status='disabled',last_error='',updated_at_ms=? WHERE site_id=?`, time.Now().UnixMilli(), schedule.SiteID)
	return err
}

func deleteTrackedSiteDNSRemote(ctx context.Context, cf *cloudflareClient, schedule SiteNodeSchedule) error {
	if schedule.cfRecordID == "" {
		return nil
	}
	if cf == nil {
		return errors.New("Cloudflare DNS client is unavailable")
	}
	if schedule.cfZoneID == "" {
		return errors.New("tracked DNS zone is missing")
	}
	return cf.deleteRecord(ctx, schedule.cfZoneID, schedule.cfRecordID)
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

func probeScheduledNode(ctx context.Context, node ControlNode, host string, probeSecret []byte) error {
	return probeScheduledNodeWithRoots(ctx, node, host, probeSecret, nil)
}

func probeScheduledNodeWithRoots(ctx context.Context, node ControlNode, host string, probeSecret []byte, roots *x509.CertPool) error {
	if len(probeSecret) == 0 {
		return errors.New("node health probe secret is unavailable")
	}
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
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host, RootCAs: roots},
		DisableKeepAlives: true,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+"/.well-known/meridian-agent-health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Meridian-Probe", encodeRuntimeKey(probeSecret))
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
		return readinessError(readinessListener, err)
	}
	if applyErr := strings.TrimSpace(node.AgentApplyError); applyErr != "" {
		return readinessError(readinessConfig, fmt.Errorf("Agent configuration apply failed: %s", applyErr))
	}
	if listenerErr := strings.TrimSpace(node.AgentListenerError); listenerErr != "" {
		return readinessError(readinessListener, errors.New(listenerErr))
	}
	if !siteScheduleConfigReady(schedule, node) {
		return readinessError(readinessConfig, errors.New("Agent has not applied the site configuration"))
	}
	if a.panelCertificates == nil {
		return readinessError(readinessCertificate, errors.New("edge TLS certificate is unavailable"))
	}
	edgeCertFile, _, pathErr := a.panelCertificates.nodeEdgeTLSPaths(node.GUID)
	if pathErr != nil {
		return readinessError(readinessCertificate, pathErr)
	}
	if err := certificateCoversHost(edgeCertFile, schedule.PublicHost); err != nil {
		return readinessError(readinessCertificate, err)
	}
	probeSecretText, err := dNodeProbeSecret(a.db, node)
	if err != nil {
		return readinessError(readinessProbe, err)
	}
	probeSecret, err := decodeNodeProbeSecret(probeSecretText)
	if err != nil {
		return readinessError(readinessProbe, errors.New("node health probe secret is invalid"))
	}
	if err := probeScheduledNode(ctx, node, schedule.PublicHost, probeSecret); err != nil {
		return readinessError(readinessProbe, fmt.Errorf("entry health check: %w", err))
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
	cf, err := a.cloudflareForScheduling()
	if err != nil {
		return err
	}
	zoneID := schedule.cfZoneID
	if zoneID == "" {
		zoneID, err = cf.findZone(ctx, schedule.PublicHost)
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
	if err != nil && schedule.cfRecordID != "" && isCloudflareRecordNotFoundError(err) {
		// A tracked record may have been removed outside Meridian. Re-resolve
		// the zone and recreate only when the exact name is still unoccupied;
		// never overwrite an operator-created untracked record.
		zoneID, err = cf.findZone(ctx, schedule.PublicHost)
		if err == nil {
			var records []cloudflareAddressRecord
			records, err = cf.exactAddressRecords(ctx, zoneID, schedule.PublicHost)
			if err == nil && len(records) > 0 {
				err = errors.New("an untracked exact A/AAAA record already exists; Meridian will not overwrite it")
			}
		}
		if err == nil {
			recordID, err = cf.writeAddressRecord(ctx, zoneID, "", recordType, schedule.PublicHost, ip.String())
		}
	}
	if err != nil {
		return err
	}
	// DNS is an external side effect and is complete at this point. Publish the
	// replacement and both Agent invalidations atomically. The previous applied
	// node retains only a bounded drain route so existing streams can report a
	// final counter; it is not left to the next sixty-second config poll.
	tx, beginErr := a.db.db.BeginTx(ctx, nil)
	if beginErr != nil {
		return beginErr
	}
	defer tx.Rollback()
	if schedule.AppliedNodeID != node.ID {
		if schedule.AppliedNodeID > 0 {
			if _, err = tx.Exec(`INSERT INTO site_node_drains(site_id,node_id,public_host,expires_at_ms,created_at_ms) VALUES(?,?,?,?,?)
				ON CONFLICT(site_id,node_id) DO UPDATE SET public_host=excluded.public_host,expires_at_ms=excluded.expires_at_ms,created_at_ms=excluded.created_at_ms`,
				schedule.SiteID, schedule.AppliedNodeID, strings.ToLower(strings.TrimSpace(schedule.PublicHost)), now.Add(siteNodeDrainWindow).UnixMilli(), now.UnixMilli()); err != nil {
				return err
			}
		}
		if err = markAgentConfigsDirtyForNodeIDsTx(tx, schedule.AppliedNodeID, node.ID); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`UPDATE site_node_schedules SET applied_node_id=?,cf_zone_id=?,cf_record_id=?,cf_record_type=?,
		applied_address=?,dns_status='active',last_error='',updated_at_ms=? WHERE site_id=?`, node.ID, zoneID, recordID,
		recordType, ip.String(), now.UnixMilli(), schedule.SiteID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (a *App) reconcileSiteNodeScheduling(ctx context.Context) {
	now := time.Now()
	if _, err := a.db.db.Exec("DELETE FROM site_node_drains WHERE expires_at_ms<=?", now.UnixMilli()); err != nil {
		log.Printf("[node-scheduler] expire node drains: %v", err)
	}
	if _, err := a.db.db.Exec("DELETE FROM site_node_host_aliases WHERE expires_at_ms<=?", now.UnixMilli()); err != nil {
		log.Printf("[node-scheduler] expire site host aliases: %v", err)
	}
	if err := a.db.retryNodeTLSCleanup(""); err != nil {
		log.Printf("[node-scheduler] managed Edge TLS cleanup retry failed: %v", err)
	}
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
			var readiness *nodeReadinessError
			if value.Mode == "global" && value.DesiredNodeID > 0 && errors.As(err, &readiness) {
				var schedulerMode string
				if modeErr := a.db.db.QueryRow("SELECT mode FROM node_scheduler_settings WHERE id=1").Scan(&schedulerMode); modeErr == nil && schedulerMode == "auto" {
					shouldCooldown := readiness.Kind == readinessCertificate || readiness.Kind == readinessListener || readiness.Kind == readinessProbe
					if readiness.Kind == readinessConfig && value.ConfigPendingSinceMS > 0 {
						shouldCooldown = now.Sub(time.UnixMilli(value.ConfigPendingSinceMS)) >= 90*time.Second
					}
					if shouldCooldown {
						if cooldownErr := a.db.recordSiteNodeProbeFailure(value.SiteID, value.DesiredNodeID, err, now); cooldownErr != nil {
							log.Printf("[node-scheduler] record site %d node %d cooldown failed: %v", value.SiteID, value.DesiredNodeID, cooldownErr)
						}
					}
				}
			}
			if value.DNSStatus != "waiting" || value.LastError != err.Error() {
				_, _ = a.db.db.Exec("UPDATE site_node_schedules SET dns_status='waiting',last_error=?,updated_at_ms=? WHERE site_id=?", err.Error(), now.UnixMilli(), value.SiteID)
			}
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

func siteNodeRevocationIDsTx(tx *sql.Tx, siteID int64, nodeIDs ...int64) ([]int64, error) {
	seen := make(map[int64]struct{})
	result := make([]int64, 0, len(nodeIDs)+2)
	for _, nodeID := range nodeIDs {
		if nodeID > 0 {
			if _, ok := seen[nodeID]; !ok {
				seen[nodeID] = struct{}{}
				result = append(result, nodeID)
			}
		}
	}
	rows, err := tx.Query("SELECT node_id FROM site_node_drains WHERE site_id=?", siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID int64
		if err := rows.Scan(&nodeID); err != nil {
			return nil, err
		}
		if nodeID > 0 {
			if _, ok := seen[nodeID]; !ok {
				seen[nodeID] = struct{}{}
				result = append(result, nodeID)
			}
		}
	}
	return result, rows.Err()
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
	if err != nil {
		return nil
	}
	// Freeze the local schedule before touching Cloudflare. Invalidate every
	// Agent that currently owns the site before clearing its node references;
	// otherwise the old Agent would not learn that the route was removed until
	// its periodic config poll.
	tx, err := a.db.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	revocationIDs, err := siteNodeRevocationIDsTx(tx, siteID, value.FixedNodeID, value.DesiredNodeID, value.AppliedNodeID)
	if err != nil {
		return err
	}
	for _, nodeID := range revocationIDs {
		if nodeID > 0 {
			if _, err := tx.Exec("INSERT OR IGNORE INTO agent_route_revocations(node_id,site_id,public_host,created_at_ms) VALUES(?,?,?,?)", nodeID, siteID, strings.ToLower(strings.TrimSpace(value.PublicHost)), time.Now().UnixMilli()); err != nil {
				return err
			}
		}
	}
	if err := markAgentConfigsDirtyForNodeIDsTx(tx, revocationIDs...); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE site_node_schedules SET enabled=0,dns_status='waiting',last_error='',updated_at_ms=? WHERE site_id=?`, time.Now().UnixMilli(), siteID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if value.cfRecordID != "" {
		cf, err := a.cloudflareForScheduling()
		if err != nil {
			_, _ = a.db.db.Exec(`UPDATE site_node_schedules SET last_error=?,updated_at_ms=? WHERE site_id=?`, err.Error(), time.Now().UnixMilli(), siteID)
			return err
		}
		if err := deleteTrackedSiteDNSRemote(ctx, cf, value); err != nil {
			_, _ = a.db.db.Exec(`UPDATE site_node_schedules SET dns_status='waiting',last_error=?,updated_at_ms=? WHERE site_id=?`, err.Error(), time.Now().UnixMilli(), siteID)
			return err
		}
	}
	// Keep the child row as the durable deletion handle until DeleteSite's
	// database transaction commits. This avoids the partial-commit window where
	// Cloudflare has been cleaned but a later site-row deletion fails.
	_, err = a.db.db.Exec(`UPDATE site_node_schedules SET enabled=0,fixed_node_id=NULL,desired_node_id=NULL,
		applied_node_id=NULL,cf_zone_id='',cf_record_id='',cf_record_type='',applied_address='',dns_status='disabled',last_error='',updated_at_ms=? WHERE site_id=?`, time.Now().UnixMilli(), siteID)
	return err
}

func (a *App) prepareNodeDeletion(ctx context.Context, nodeID int64) error {
	values, err := a.db.ListSiteNodeSchedules()
	if err != nil {
		return err
	}
	drainSites := make(map[int64]struct{})
	drainRows, drainErr := a.db.db.Query("SELECT site_id FROM site_node_drains WHERE node_id=?", nodeID)
	if drainErr != nil {
		return drainErr
	}
	for drainRows.Next() {
		var siteID int64
		if scanErr := drainRows.Scan(&siteID); scanErr != nil {
			drainRows.Close()
			return scanErr
		}
		drainSites[siteID] = struct{}{}
	}
	if drainErr := drainRows.Err(); drainErr != nil {
		drainRows.Close()
		return drainErr
	}
	if drainErr := drainRows.Close(); drainErr != nil {
		return drainErr
	}
	affected := make([]SiteNodeSchedule, 0)
	for _, value := range values {
		if value.FixedNodeID != nodeID && value.DesiredNodeID != nodeID && value.AppliedNodeID != nodeID {
			if _, draining := drainSites[value.SiteID]; !draining {
				continue
			}
		}
		affected = append(affected, value)
	}
	if len(affected) == 0 {
		return nil
	}
	needsCloudflare := false
	for _, value := range affected {
		if strings.TrimSpace(value.cfRecordID) != "" {
			needsCloudflare = true
			break
		}
	}
	var cf *cloudflareClient
	if needsCloudflare {
		cf, err = a.cloudflareForScheduling()
		if err != nil {
			return err
		}
	}
	// Freeze every affected row in one local transaction before the remote phase.
	// This makes a partial Cloudflare failure safe: all rows are disabled and
	// retain their record IDs for retry, so the scheduler cannot recreate a
	// record that was already deleted earlier in the batch.
	tx, err := a.db.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, value := range affected {
		revocationIDs, revocationErr := siteNodeRevocationIDsTx(tx, value.SiteID, value.FixedNodeID, value.DesiredNodeID, value.AppliedNodeID)
		if revocationErr != nil {
			return revocationErr
		}
		for _, nodeID := range revocationIDs {
			if nodeID > 0 {
				if _, err := tx.Exec("INSERT OR IGNORE INTO agent_route_revocations(node_id,site_id,public_host,created_at_ms) VALUES(?,?,?,?)", nodeID, value.SiteID, strings.ToLower(strings.TrimSpace(value.PublicHost)), time.Now().UnixMilli()); err != nil {
					return err
				}
			}
		}
		if err := markAgentConfigsDirtyForNodeIDsTx(tx, revocationIDs...); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE site_node_schedules SET enabled=0,dns_status='waiting',last_error='',updated_at_ms=? WHERE site_id=?`, time.Now().UnixMilli(), value.SiteID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, value := range affected {
		if strings.TrimSpace(value.cfRecordID) == "" {
			continue
		}
		if err := deleteTrackedSiteDNSRemote(ctx, cf, value); err != nil {
			// Keep every row disabled and retain tracked IDs so a later delete
			// request can resume the remote phase idempotently.
			_, _ = a.db.db.Exec(`UPDATE site_node_schedules SET enabled=0,dns_status='waiting',last_error=?,updated_at_ms=? WHERE site_id=?`, err.Error(), time.Now().UnixMilli(), value.SiteID)
			return err
		}
	}
	tx, err = a.db.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, value := range affected {
		if _, err := tx.Exec(`UPDATE site_node_schedules SET enabled=0,fixed_node_id=NULL,desired_node_id=NULL,
			applied_node_id=NULL,cf_zone_id='',cf_record_id='',cf_record_type='',applied_address='',dns_status='disabled',last_error='',updated_at_ms=? WHERE site_id=?`, time.Now().UnixMilli(), value.SiteID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
