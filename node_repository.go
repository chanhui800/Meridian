package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	nodeOnlineWindow       = 45 * time.Second
	nodeEnrollmentLifetime = 24 * time.Hour
)

var (
	errNodeNotFound          = errors.New("node not found")
	errNodeNameConflict      = errors.New("node name already exists")
	errInvalidNodeToken      = errors.New("invalid or expired node token")
	errInvalidAgentToken     = errors.New("invalid agent token")
	errStaleAgentSession     = errors.New("stale agent session; fetch a fresh agent configuration")
	errManualNodeUnavailable = errors.New("manual node is unavailable")
	errPersistentJWTRequired = errors.New("Agent nodes require a persistent JWT_SECRET")
)

type ControlNode struct {
	ID                          int64  `json:"id"`
	GUID                        string `json:"guid"`
	Name                        string `json:"name"`
	Address                     string `json:"address"`
	Port                        int    `json:"port"`
	Enabled                     bool   `json:"enabled"`
	Priority                    int    `json:"priority"`
	TrafficQuota                int64  `json:"traffic_quota"`
	TrafficManualOffset         int64  `json:"traffic_manual_offset_bytes"`
	BillingMode                 string `json:"billing_mode"`
	ResetDay                    int    `json:"reset_day"`
	CycleStartedAtMS            int64  `json:"cycle_started_at_ms"`
	PeriodRXBytes               int64  `json:"period_rx_bytes"`
	PeriodTXBytes               int64  `json:"period_tx_bytes"`
	LifetimeRXBytes             int64  `json:"lifetime_rx_bytes"`
	LifetimeTXBytes             int64  `json:"lifetime_tx_bytes"`
	InterfaceName               string `json:"interface_name"`
	AgentVersion                string `json:"agent_version"`
	DesiredConfigHash           string `json:"desired_config_hash"`
	ConfigDirty                 bool   `json:"config_dirty"`
	AppliedConfigHash           string `json:"applied_config_hash"`
	ConfigRevision              int64  `json:"config_revision"`
	DesiredConfigRevision       int64  `json:"desired_config_revision"`
	AppliedConfigRevision       int64  `json:"applied_config_revision"`
	AgentApplyError             string `json:"agent_apply_error"`
	AgentApplyErrorAtMS         int64  `json:"agent_apply_error_at_ms"`
	AgentApplyFailures          int64  `json:"agent_apply_failures"`
	AgentListenerError          string `json:"agent_listener_error"`
	EventSpoolError             string `json:"event_spool_error"`
	EventQueueDepth             int    `json:"event_queue_depth"`
	EventDropped                int64  `json:"event_dropped"`
	CacheClearGeneration        int64  `json:"cache_clear_generation"`
	CacheClearAppliedGeneration int64  `json:"cache_clear_applied_generation"`
	EnrolledAtMS                int64  `json:"enrolled_at_ms"`
	LastSeenAtMS                int64  `json:"last_seen_at_ms"`
	CreatedAtMS                 int64  `json:"created_at_ms"`
	UpdatedAtMS                 int64  `json:"updated_at_ms"`
	Status                      string `json:"status"`
	TrafficUsed                 int64  `json:"traffic_used"`
	TrafficRemaining            int64  `json:"traffic_remaining"`
	EnrollmentAvailable         bool   `json:"enrollment_available"`
	Depleted                    bool   `json:"depleted"`
	Active                      bool   `json:"active"`

	lastRawRXBytes        int64
	lastRawTXBytes        int64
	lastBootID            string // counter epoch (kernel boot + interface)
	lastReportSessionID   string
	activeAgentSessionID  string
	agentSessionEpoch     int64
	agentLeaseID          string
	lastSequence          int64
	enrollmentTokenHash   string
	agentTokenHash        string
	enrollmentExpiresMS   int64
	probeSecretCiphertext string
}

type NodeSchedulerSettings struct {
	Mode         string `json:"mode"`
	ManualNodeID int64  `json:"manual_node_id"`
	ActiveNodeID int64  `json:"active_node_id"`
	UpdatedAtMS  int64  `json:"updated_at_ms"`
}

type NodeControlSnapshot struct {
	Nodes         []ControlNode            `json:"nodes"`
	Scheduler     NodeSchedulerSettings    `json:"scheduler"`
	AgentSecurity AgentSecurityDiagnostics `json:"agent_security"`
}

// AgentSecurityDiagnostics contains bounded, aggregate counters for report
// authorization failures. It intentionally excludes tokens, headers and site
// payloads so the diagnostics response is safe for administrators to inspect.
type AgentSecurityDiagnostics struct {
	RejectedTotal  uint64 `json:"rejected_total"`
	RequestEvent   uint64 `json:"request_event"`
	SiteStat       uint64 `json:"site_stat"`
	MediaCount     uint64 `json:"media_count"`
	Retention      uint64 `json:"retention"`
	Observation    uint64 `json:"observation"`
	LastRejectedAt string `json:"last_rejected_at,omitempty"`
}

type NodeCreateInput struct {
	Name                     string
	Address                  string
	Port                     int
	Priority                 int
	TrafficQuota             int64
	BillingMode              string
	ResetDay                 int
	TrafficManualOffsetBytes int64
}

type NodeReport struct {
	BootID           string `json:"boot_id"`
	ReportSessionID  string `json:"report_session_id,omitempty"`
	SessionEpoch     int64  `json:"session_epoch,omitempty"`
	AgentLeaseID     string `json:"agent_lease_id,omitempty"`
	CounterEpoch     string `json:"counter_epoch,omitempty"`
	SiteCounterEpoch string `json:"site_counter_epoch,omitempty"`
	Sequence         int64  `json:"sequence"`
	// TelemetrySequence is shared by the full and lightweight report channels.
	// It lets the Controller reject a delayed sample from either channel.
	TelemetrySequence     int64                    `json:"telemetry_sequence,omitempty"`
	InterfaceName         string                   `json:"interface_name"`
	RXBytes               int64                    `json:"rx_bytes"`
	TXBytes               int64                    `json:"tx_bytes"`
	AgentVersion          string                   `json:"agent_version"`
	AppliedConfigHash     string                   `json:"applied_config_hash"`
	AppliedConfigRevision int64                    `json:"applied_config_revision,omitempty"`
	ApplyError            string                   `json:"apply_error,omitempty"`
	ApplyErrorAtMS        int64                    `json:"apply_error_at_ms,omitempty"`
	ApplyFailures         int64                    `json:"apply_failures,omitempty"`
	ListenerError         string                   `json:"listener_error"`
	EventSpoolError       string                   `json:"event_spool_error,omitempty"`
	EventQueueDepth       int                      `json:"event_queue_depth,omitempty"`
	EventDropped          int64                    `json:"event_dropped,omitempty"`
	CacheClearGeneration  int64                    `json:"cache_clear_generation,omitempty"`
	SiteStats             []NodeSiteStat           `json:"site_stats,omitempty"`
	MediaCounts           []NodeMediaCount         `json:"media_counts,omitempty"`
	Retention             []NodeRetentionStatus    `json:"retention,omitempty"`
	Observations          []NodeDynamicObservation `json:"observations,omitempty"`
	Events                []NodeRequestEvent       `json:"events,omitempty"`
}

// NodeLiveReport is the lightweight runtime sample used by the dashboard.
// It carries only process-stable per-site counters; the full NodeReport remains
// responsible for site state, media, observations, events, and persistence.
type NodeLiveReport struct {
	ReportSessionID   string                `json:"report_session_id"`
	SessionEpoch      int64                 `json:"session_epoch,omitempty"`
	AgentLeaseID      string                `json:"agent_lease_id,omitempty"`
	CounterEpoch      string                `json:"counter_epoch,omitempty"`
	Sequence          int64                 `json:"sequence"`
	TelemetrySequence int64                 `json:"telemetry_sequence,omitempty"`
	SampledAtMS       int64                 `json:"sampled_at_ms"`
	SiteStats         []NodeLiveSiteTraffic `json:"site_stats,omitempty"`
}

type NodeLiveSiteTraffic struct {
	SiteID             int64  `json:"site_id"`
	Host               string `json:"host"`
	CumulativeBytesIn  int64  `json:"cumulative_bytes_in"`
	CumulativeBytesOut int64  `json:"cumulative_bytes_out"`
	Requests           int64  `json:"requests"`
	SampledAtMS        int64  `json:"sampled_at_ms,omitempty"`
}

// NodeReportResult keeps the protocol acknowledgement tied to the exact
// validation/commit decision made by the controller. Handlers must not infer
// ACKs from their original request slice after invalid events are filtered.
type NodeReportResult struct {
	Node                        ControlNode
	AcceptedSiteIDs             []int64
	DiscardedSiteIDs            []int64
	AcceptedMediaSiteIDs        []int64
	DiscardedMediaSiteIDs       []int64
	AcceptedRetentionSiteIDs    []int64
	DiscardedRetentionSiteIDs   []int64
	AcceptedObservationSiteIDs  []int64
	DiscardedObservationSiteIDs []int64
	AcceptedEventIDs            []int64
	AcceptedEventUIDs           []string
	DiscardedEventIDs           []int64
	DiscardedEventUIDs          []string
}

type NodeSiteStat struct {
	// SiteID is the Controller-stable identity. Host is retained for protocol
	// compatibility and must agree with SiteID when both are supplied.
	SiteID             int64  `json:"site_id,omitempty"`
	Host               string `json:"host"`
	RequestCount       int64  `json:"request_count"`
	LastRequestAtMS    int64  `json:"last_request_at_ms"`
	LastStatus         int    `json:"last_status"`
	BytesIn            int64  `json:"bytes_in"`
	BytesOut           int64  `json:"bytes_out"`
	CumulativeBytesIn  int64  `json:"cumulative_bytes_in"`
	CumulativeBytesOut int64  `json:"cumulative_bytes_out"`
	CacheSizeBytes     int64  `json:"cache_size_bytes,omitempty"`
	// CacheSizeValid distinguishes a failed cache directory read from a real
	// zero-byte cache. Older Agents omit the field and therefore never erase a
	// previously known Controller value by reporting an unknown size.
	CacheSizeValid bool `json:"cache_size_valid,omitempty"`
	// CounterEpoch changes when an Agent recreates a retired site counter. It
	// lets the Controller distinguish an A→B→A route transition from a
	// monotonic continuation of the old cumulative values.
	CounterEpoch uint64 `json:"counter_epoch,omitempty"`
	// Final marks the last snapshot for a route removed from an Agent bundle.
	// The Controller retires only that node's drain generation after accepting it.
	Final bool `json:"final,omitempty"`
}

type NodeMediaCount struct {
	SiteID       int64 `json:"site_id"`
	MovieCount   int64 `json:"movie_count"`
	SeriesCount  int64 `json:"series_count"`
	EpisodeCount int64 `json:"episode_count"`
	ObservedAtMS int64 `json:"observed_at_ms"`
}

type NodeRetentionStatus struct {
	SiteID              int64 `json:"site_id"`
	ExpectedStartedAtMS int64 `json:"expected_started_at_ms"`
	CompletedAtMS       int64 `json:"completed_at_ms"`
	Done                bool  `json:"done"`
}

type NodeDynamicObservation struct {
	SiteID             int64  `json:"site_id"`
	CanonicalAuthority string `json:"canonical_authority"`
	Source             string `json:"source"`
	Decision           string `json:"decision"`
	ReasonCode         string `json:"reason_code"`
	ObservedAtMS       int64  `json:"observed_at_ms"`
}

type NodeRequestEvent struct {
	EventUID                string `json:"event_uid,omitempty"`
	EventID                 int64  `json:"event_id"`
	SiteID                  int64  `json:"site_id"`
	Host                    string `json:"host"`
	Method                  string `json:"method"`
	Path                    string `json:"path"`
	Query                   string `json:"query,omitempty"`
	StatusCode              int    `json:"status_code"`
	ClientIP                string `json:"client_ip,omitempty"`
	UserAgent               string `json:"user_agent,omitempty"`
	Authorization           string `json:"authorization,omitempty"`
	Body                    string `json:"body,omitempty"`
	ContentType             string `json:"content_type,omitempty"`
	ContentEncoding         string `json:"content_encoding,omitempty"`
	ResponseBody            string `json:"response_body,omitempty"`
	ResponseContentType     string `json:"response_content_type,omitempty"`
	ResponseContentEncoding string `json:"response_content_encoding,omitempty"`
	RecordedAtMS            int64  `json:"recorded_at_ms"`
	ResourceCategory        string `json:"resource_category,omitempty"`
	UpstreamUserAgent       string `json:"upstream_user_agent,omitempty"`
	BackendAddress          string `json:"backend_address,omitempty"`
	InboundColo             string `json:"inbound_colo,omitempty"`
	OutboundColo            string `json:"outbound_colo,omitempty"`
	SkipRequestLog          bool   `json:"skip_request_log,omitempty"`
	Priority                string `json:"priority,omitempty"`
	// WatchHistory carries a fully parsed playback event from an Agent.  It is
	// used when the Agent has no durable Controller database to run the normal
	// request-body parser against.
	WatchHistory *watchHistoryEvent `json:"watch_history,omitempty"`
}

const (
	nodeEventPriorityCritical   = "critical"
	nodeEventPriorityBestEffort = "best_effort"
)

// Metadata responses from Emby-compatible backends can include provider and
// image fields large enough to exceed the old 8 KiB event cap. Keep the event
// bounded, but leave enough room for a complete item response.
const maxNodeRequestEventBodyBytes = 8 << 10
const maxNodeRequestEventResponseBodyBytes = 64 << 10
const maxAgentReportBodyBytes = 2 << 20
const maxAgentLiveReportBodyBytes = 256 << 10

// Leave headroom below the Controller's hard 2 MiB limit so an event batch is
// accepted consistently by both the JSON HTTP fallback and WebSocket path.
const targetAgentReportBytes = 1536 << 10
const maxNodeRequestEventsPerReport = 128
const maxNodeTelemetryItemsPerReport = 128
const maxNodeSiteStatsPerReport = 512

func newNodeToken() (string, error) {
	value := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func hashNodeToken(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func normalizeNodeInput(input NodeCreateInput) (NodeCreateInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Address = strings.TrimSpace(input.Address)
	// The browser supplies the controller's current port for new nodes. Keep a
	// 443 fallback for API callers that omit the optional convenience default.
	if input.Port == 0 {
		input.Port = 443
	}
	input.BillingMode = strings.ToLower(strings.TrimSpace(input.BillingMode))
	if input.BillingMode == "" {
		input.BillingMode = "outbound"
	}
	if input.Name == "" || len(input.Name) > 64 {
		return input, errors.New("node name must be 1-64 characters")
	}
	if len(input.Address) > 255 {
		return input, errors.New("node address is too long")
	}
	if input.Port < 1 || input.Port > 65535 {
		return input, errors.New("node port must be between 1 and 65535")
	}
	if input.Priority < 0 || input.Priority > 1000 {
		return input, errors.New("priority must be between 0 and 1000")
	}
	if input.TrafficQuota < 0 {
		return input, errors.New("traffic quota cannot be negative")
	}
	if input.ResetDay < 0 || input.ResetDay > 31 {
		return input, errors.New("reset day must be between 0 and 31")
	}
	if input.BillingMode != "outbound" && input.BillingMode != "bidirectional" {
		return input, errors.New("billing mode must be outbound or bidirectional")
	}
	return input, nil
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func nodeCycleStart(now time.Time, resetDay, offsetMinutes int) int64 {
	if resetDay == 0 {
		return now.UnixMilli()
	}
	local := now.UTC().Add(time.Duration(offsetMinutes) * time.Minute)
	day := resetDay
	if max := daysInMonth(local.Year(), local.Month()); day > max {
		day = max
	}
	candidateLocal := time.Date(local.Year(), local.Month(), day, 0, 0, 0, 0, time.UTC)
	if local.Before(candidateLocal) {
		previous := local.AddDate(0, -1, 0)
		day = resetDay
		if max := daysInMonth(previous.Year(), previous.Month()); day > max {
			day = max
		}
		candidateLocal = time.Date(previous.Year(), previous.Month(), day, 0, 0, 0, 0, time.UTC)
	}
	return candidateLocal.Add(-time.Duration(offsetMinutes) * time.Minute).UnixMilli()
}

func (d *DB) resetDueNodeCycles(now time.Time) error {
	settings := d.currentSystemSettings()
	rows, err := d.db.Query("SELECT id, reset_day, cycle_started_at_ms FROM control_nodes WHERE reset_day > 0")
	if err != nil {
		return err
	}
	type dueCycle struct{ id, start int64 }
	due := make([]dueCycle, 0)
	for rows.Next() {
		var id, started int64
		var resetDay int
		if err := rows.Scan(&id, &resetDay, &started); err != nil {
			_ = rows.Close()
			return err
		}
		start := nodeCycleStart(now, resetDay, settings.ScheduleTimezone)
		if started < start {
			due = append(due, dueCycle{id: id, start: start})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, cycle := range due {
		if _, err := d.db.Exec(`UPDATE control_nodes SET period_rx_bytes=0, period_tx_bytes=0,
			cycle_started_at_ms=?, updated_at_ms=? WHERE id=? AND cycle_started_at_ms < ?`, cycle.start, now.UnixMilli(), cycle.id, cycle.start); err != nil {
			return err
		}
		if _, err := d.db.Exec(`UPDATE site_node_schedules SET agent_boot_id='',agent_request_count=0,agent_last_request_at_ms=0,agent_last_status=0
			WHERE desired_node_id=?`, cycle.id); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) CreateControlNode(input NodeCreateInput, now time.Time) (ControlNode, string, error) {
	if jwtSecretEphemeral {
		return ControlNode{}, "", errPersistentJWTRequired
	}
	input, err := normalizeNodeInput(input)
	if err != nil {
		return ControlNode{}, "", err
	}
	guid, err := newNodeToken()
	if err != nil {
		return ControlNode{}, "", err
	}
	enrollmentToken, err := newNodeToken()
	if err != nil {
		return ControlNode{}, "", err
	}
	probeSecret, err := newNodeProbeSecret()
	if err != nil {
		return ControlNode{}, "", err
	}
	probeSecretCiphertext, err := encryptNodeProbeSecretWithSecret(probeSecret, jwtSecret)
	if err != nil {
		return ControlNode{}, "", err
	}
	nowMS := now.UnixMilli()
	cycleStart := nodeCycleStart(now, input.ResetDay, d.currentSystemSettings().ScheduleTimezone)
	result, err := d.db.Exec(`INSERT INTO control_nodes
		(guid,name,address,entry_mode,http_port,https_port,priority,traffic_quota,billing_mode,reset_day,cycle_started_at_ms,
		 enrollment_token_hash,enrollment_expires_at_ms,probe_secret_ciphertext,created_at_ms,updated_at_ms)
		VALUES(?,?,?,'direct',0,?,?,?,?,?,?,?,?,?,?,?)`, guid, input.Name, input.Address, input.Port, input.Priority, input.TrafficQuota,
		input.BillingMode, input.ResetDay, cycleStart, hashNodeToken(enrollmentToken), now.Add(nodeEnrollmentLifetime).UnixMilli(), probeSecretCiphertext, nowMS, nowMS)
	if err != nil {
		if isSQLiteUniqueConstraintError(err) {
			return ControlNode{}, "", errNodeNameConflict
		}
		return ControlNode{}, "", err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return ControlNode{}, "", err
	}
	node, err := d.controlNodeByID(id, now)
	return node, enrollmentToken, err
}

type rowScanner interface{ Scan(...interface{}) error }

const controlNodeSelect = `SELECT id,guid,name,address,https_port,enabled,priority,traffic_quota,billing_mode,reset_day,
	cycle_started_at_ms,period_rx_bytes,period_tx_bytes,lifetime_rx_bytes,lifetime_tx_bytes,traffic_manual_offset_bytes,
	last_raw_rx_bytes,last_raw_tx_bytes,last_boot_id,last_report_session_id,active_agent_session_id,agent_session_epoch,agent_lease_id,last_sequence,interface_name,agent_version,desired_config_hash,config_dirty,applied_config_hash,agent_apply_error,agent_apply_error_at_ms,agent_apply_failures,agent_listener_error,event_spool_error,event_queue_depth,event_dropped,
	config_revision,desired_config_revision,applied_config_revision,
	enrollment_token_hash,enrollment_expires_at_ms,probe_secret_ciphertext,agent_token_hash,cache_clear_generation,cache_clear_applied_generation,enrolled_at_ms,last_seen_at_ms,created_at_ms,updated_at_ms
	FROM control_nodes`

func scanControlNode(scanner rowScanner, now time.Time) (ControlNode, error) {
	var node ControlNode
	var enabled, configDirty int
	err := scanner.Scan(&node.ID, &node.GUID, &node.Name, &node.Address, &node.Port, &enabled, &node.Priority, &node.TrafficQuota,
		&node.BillingMode, &node.ResetDay, &node.CycleStartedAtMS, &node.PeriodRXBytes, &node.PeriodTXBytes,
		&node.LifetimeRXBytes, &node.LifetimeTXBytes, &node.TrafficManualOffset, &node.lastRawRXBytes, &node.lastRawTXBytes, &node.lastBootID, &node.lastReportSessionID, &node.activeAgentSessionID, &node.agentSessionEpoch, &node.agentLeaseID,
		&node.lastSequence, &node.InterfaceName, &node.AgentVersion, &node.DesiredConfigHash, &configDirty, &node.AppliedConfigHash, &node.AgentApplyError, &node.AgentApplyErrorAtMS, &node.AgentApplyFailures, &node.AgentListenerError, &node.EventSpoolError, &node.EventQueueDepth, &node.EventDropped,
		&node.ConfigRevision, &node.DesiredConfigRevision, &node.AppliedConfigRevision,
		&node.enrollmentTokenHash, &node.enrollmentExpiresMS, &node.probeSecretCiphertext,
		&node.agentTokenHash, &node.CacheClearGeneration, &node.CacheClearAppliedGeneration, &node.EnrolledAtMS, &node.LastSeenAtMS, &node.CreatedAtMS, &node.UpdatedAtMS)
	if err != nil {
		return ControlNode{}, err
	}
	node.Enabled = enabled != 0
	node.ConfigDirty = configDirty != 0
	node.EnrollmentAvailable = node.enrollmentTokenHash != "" && now.UnixMilli() < node.enrollmentExpiresMS
	if node.LastSeenAtMS > 0 && now.Sub(time.UnixMilli(node.LastSeenAtMS)) <= nodeOnlineWindow {
		node.Status = "online"
	} else if node.EnrolledAtMS > 0 {
		node.Status = "offline"
	} else {
		node.Status = "pending"
	}
	if node.BillingMode == "bidirectional" {
		node.TrafficUsed = node.PeriodRXBytes + node.PeriodTXBytes
	} else {
		node.TrafficUsed = node.PeriodTXBytes
	}
	node.TrafficUsed += node.TrafficManualOffset
	if node.TrafficUsed < 0 {
		node.TrafficUsed = 0
	}
	if node.TrafficQuota > 0 {
		node.TrafficRemaining = node.TrafficQuota - node.TrafficUsed
		if node.TrafficRemaining < 0 {
			node.TrafficRemaining = 0
		}
		node.Depleted = node.TrafficUsed >= node.TrafficQuota
	}
	return node, nil
}

func (d *DB) MigrateControlNodesToSinglePort(defaultPort int, now time.Time) error {
	if defaultPort < 1 || defaultPort > 65535 {
		return errors.New("default node port must be between 1 and 65535")
	}
	_, err := d.db.Exec(`UPDATE control_nodes
		SET entry_mode='direct',http_port=0,
			https_port=CASE WHEN https_port BETWEEN 1 AND 65535 THEN https_port ELSE ? END,
			updated_at_ms=?
		WHERE entry_mode!='direct' OR http_port!=0 OR https_port NOT BETWEEN 1 AND 65535`, defaultPort, now.UnixMilli())
	return err
}

func (d *DB) controlNodeByID(id int64, now time.Time) (ControlNode, error) {
	node, err := scanControlNode(d.db.QueryRow(controlNodeSelect+" WHERE id=?", id), now)
	if errors.Is(err, sql.ErrNoRows) {
		return ControlNode{}, errNodeNotFound
	}
	return node, err
}

func (d *DB) listControlNodes(now time.Time) ([]ControlNode, error) {
	rows, err := d.db.Query(controlNodeSelect + " ORDER BY priority DESC, id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes := make([]ControlNode, 0)
	for rows.Next() {
		node, err := scanControlNode(rows, now)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

// nodeAssignmentEligible answers whether a node may remain the desired
// assignment. Runtime readiness is checked separately during reconciliation;
// an apply failure must not make the scheduler erase the desired node and
// create a self-reinforcing "no eligible node" loop.
func nodeAssignmentEligible(node ControlNode) bool {
	return node.Enabled && node.Status == "online" && !node.Depleted &&
		strings.TrimSpace(node.AgentListenerError) == ""
}

func nodeRuntimeReady(node ControlNode) bool {
	return nodeAssignmentEligible(node) &&
		strings.TrimSpace(node.AgentListenerError) == "" &&
		strings.TrimSpace(node.AgentApplyError) == ""
}

// Keep the historical helper name for callers that only need assignment
// eligibility. Readiness-sensitive paths must call nodeRuntimeReady explicitly.
func nodeEligible(node ControlNode) bool {
	return nodeAssignmentEligible(node)
}

func (d *DB) NodeControlSnapshot(now time.Time) (NodeControlSnapshot, error) {
	if err := d.resetDueNodeCycles(now); err != nil {
		return NodeControlSnapshot{}, err
	}
	nodes, err := d.listControlNodes(now)
	if err != nil {
		return NodeControlSnapshot{}, err
	}
	var scheduler NodeSchedulerSettings
	var manualID, activeID sql.NullInt64
	if err := d.db.QueryRow("SELECT mode,manual_node_id,active_node_id,updated_at_ms FROM node_scheduler_settings WHERE id=1").Scan(
		&scheduler.Mode, &manualID, &activeID, &scheduler.UpdatedAtMS); err != nil {
		return NodeControlSnapshot{}, err
	}
	if manualID.Valid {
		scheduler.ManualNodeID = manualID.Int64
	}
	if activeID.Valid {
		scheduler.ActiveNodeID = activeID.Int64
	}
	desired := int64(0)
	if scheduler.Mode == "manual" {
		for _, node := range nodes {
			if node.ID == scheduler.ManualNodeID && nodeEligible(node) {
				desired = node.ID
				break
			}
		}
	} else {
		sort.SliceStable(nodes, func(i, j int) bool {
			if nodes[i].Priority != nodes[j].Priority {
				return nodes[i].Priority > nodes[j].Priority
			}
			return nodes[i].TrafficUsed < nodes[j].TrafficUsed
		})
		for _, node := range nodes {
			if nodeEligible(node) {
				desired = node.ID
				break
			}
		}
	}
	if desired != scheduler.ActiveNodeID {
		if _, err := d.db.Exec("UPDATE node_scheduler_settings SET active_node_id=?,updated_at_ms=? WHERE id=1", nullableNodeID(desired), now.UnixMilli()); err != nil {
			return NodeControlSnapshot{}, err
		}
		scheduler.ActiveNodeID = desired
		scheduler.UpdatedAtMS = now.UnixMilli()
	}
	for i := range nodes {
		nodes[i].Active = nodes[i].ID == scheduler.ActiveNodeID
	}
	return NodeControlSnapshot{Nodes: nodes, Scheduler: scheduler, AgentSecurity: d.agentSecuritySnapshot()}, nil
}

func nullableNodeID(id int64) interface{} {
	if id <= 0 {
		return nil
	}
	return id
}

// markAgentConfigsDirtyTx invalidates runtime snapshots for every enrolled
// Agent. Enrollment and scheduling eligibility are separate concerns: a
// disabled Agent may still need a route-removal or force-stop command.
func markAgentConfigsDirtyTx(tx *sql.Tx) error {
	if tx == nil {
		return errors.New("nil database transaction")
	}
	_, err := tx.Exec(`UPDATE control_nodes SET
		config_revision=config_revision+1,
		desired_config_hash='',
		desired_config_revision=0,
		config_dirty=1,
		updated_at_ms=?
		WHERE agent_token_hash<>''`, time.Now().UnixMilli())
	return err
}

func markAgentConfigsDirtyForNodeIDsTx(tx *sql.Tx, nodeIDs ...int64) error {
	if tx == nil {
		return errors.New("nil database transaction")
	}
	seen := make(map[int64]struct{}, len(nodeIDs))
	args := make([]interface{}, 0, len(nodeIDs)+1)
	placeholders := make([]string, 0, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if nodeID <= 0 {
			continue
		}
		if _, ok := seen[nodeID]; ok {
			continue
		}
		seen[nodeID] = struct{}{}
		placeholders = append(placeholders, "?")
		args = append(args, nodeID)
	}
	if len(placeholders) == 0 {
		return nil
	}
	args = append([]interface{}{time.Now().UnixMilli()}, args...)
	// #nosec G202 -- only the number of parameter placeholders is generated; all values remain bound arguments.
	_, err := tx.Exec(`UPDATE control_nodes SET
		config_revision=config_revision+1,
		desired_config_hash='',
		desired_config_revision=0,
		config_dirty=1,
		updated_at_ms=?
		WHERE agent_token_hash<>'' AND id IN (`+strings.Join(placeholders, ",")+")", args...)
	return err
}

func markAgentConfigsDirtyForSiteTx(tx *sql.Tx, siteID int64) error {
	if tx == nil || siteID <= 0 {
		return nil
	}
	rows, err := tx.Query("SELECT desired_node_id,applied_node_id FROM site_node_schedules WHERE site_id=?", siteID)
	if err != nil {
		return err
	}
	var nodeIDs []int64
	for rows.Next() {
		var desired, applied sql.NullInt64
		if err := rows.Scan(&desired, &applied); err != nil {
			_ = rows.Close()
			return err
		}
		if desired.Valid {
			nodeIDs = append(nodeIDs, desired.Int64)
		}
		if applied.Valid {
			nodeIDs = append(nodeIDs, applied.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	return markAgentConfigsDirtyForNodeIDsTx(tx, nodeIDs...)
}

// markAgentConfigDirty invalidates one enrolled Agent after an out-of-band
// runtime input (such as a renewed Edge certificate) changes its config.
func (d *DB) markAgentConfigDirty(nodeID int64) error {
	if d == nil || d.db == nil || nodeID <= 0 {
		return nil
	}
	result, err := d.db.Exec(`UPDATE control_nodes SET
		config_revision=config_revision+1,
		desired_config_hash='',
		desired_config_revision=0,
		config_dirty=1,
		updated_at_ms=?
		WHERE id=? AND enabled=1 AND agent_token_hash<>''`, time.Now().UnixMilli(), nodeID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errNodeNotFound
	}
	return nil
}

func (d *DB) UpdateNodeScheduler(mode string, manualNodeID int64, now time.Time) (NodeControlSnapshot, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode != "auto" && mode != "manual" {
		return NodeControlSnapshot{}, errors.New("scheduler mode must be auto or manual")
	}
	if mode == "manual" {
		if manualNodeID <= 0 {
			return NodeControlSnapshot{}, errors.New("manual mode requires a node")
		}
		node, err := d.controlNodeByID(manualNodeID, now)
		if err != nil {
			return NodeControlSnapshot{}, err
		}
		if !nodeAssignmentEligible(node) {
			return NodeControlSnapshot{}, errManualNodeUnavailable
		}
	} else {
		manualNodeID = 0
	}
	tx, err := d.db.Begin()
	if err != nil {
		return NodeControlSnapshot{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE node_scheduler_settings SET mode=?,manual_node_id=?,updated_at_ms=? WHERE id=1", mode, nullableNodeID(manualNodeID), now.UnixMilli()); err != nil {
		return NodeControlSnapshot{}, err
	}
	if err := markAgentConfigsDirtyTx(tx); err != nil {
		return NodeControlSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return NodeControlSnapshot{}, err
	}
	return d.NodeControlSnapshot(now)
}

func (d *DB) UpdateControlNode(id int64, input NodeCreateInput, enabled bool, now time.Time) (ControlNode, error) {
	input, err := normalizeNodeInput(input)
	if err != nil {
		return ControlNode{}, err
	}
	// Read the timezone before opening the write transaction. The database is
	// intentionally configured with a single SQLite connection, so issuing a
	// nested query while the transaction is open would deadlock.
	scheduleTimezone := d.currentSystemSettings().ScheduleTimezone
	tx, err := d.db.Begin()
	if err != nil {
		return ControlNode{}, err
	}
	defer tx.Rollback()
	var currentAddress string
	var currentPort, currentResetDay, currentEnabled int
	if err := tx.QueryRow("SELECT address,https_port,reset_day,enabled FROM control_nodes WHERE id=?", id).Scan(&currentAddress, &currentPort, &currentResetDay, &currentEnabled); errors.Is(err, sql.ErrNoRows) {
		return ControlNode{}, errNodeNotFound
	} else if err != nil {
		return ControlNode{}, err
	}

	portChanged := currentPort != input.Port
	enabledChanged := currentEnabled != sqliteBool(enabled)
	var result sql.Result
	if currentResetDay != input.ResetDay {
		cycleStart := nodeCycleStart(now, input.ResetDay, scheduleTimezone)
		result, err = tx.Exec(`UPDATE control_nodes SET name=?,address=?,entry_mode='direct',http_port=0,https_port=?,enabled=?,priority=?,traffic_quota=?,billing_mode=?,reset_day=?,traffic_manual_offset_bytes=?,
			cycle_started_at_ms=?,period_rx_bytes=0,period_tx_bytes=0,updated_at_ms=? WHERE id=?`, input.Name, input.Address,
			input.Port, sqliteBool(enabled), input.Priority, input.TrafficQuota, input.BillingMode, input.ResetDay, input.TrafficManualOffsetBytes, cycleStart, now.UnixMilli(), id)
	} else {
		result, err = tx.Exec(`UPDATE control_nodes SET name=?,address=?,entry_mode='direct',http_port=0,https_port=?,enabled=?,priority=?,traffic_quota=?,billing_mode=?,traffic_manual_offset_bytes=?,updated_at_ms=? WHERE id=?`,
			input.Name, input.Address, input.Port, sqliteBool(enabled), input.Priority, input.TrafficQuota, input.BillingMode, input.TrafficManualOffsetBytes, now.UnixMilli(), id)
	}
	if err != nil {
		if isSQLiteUniqueConstraintError(err) {
			return ControlNode{}, errNodeNameConflict
		}
		return ControlNode{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return ControlNode{}, err
	}
	if rows != 1 {
		return ControlNode{}, errNodeNotFound
	}
	if portChanged {
		if _, err := tx.Exec(`UPDATE control_nodes SET desired_config_hash='',desired_config_revision=0 WHERE id=?`, id); err != nil {
			return ControlNode{}, err
		}
		if _, err := tx.Exec(`UPDATE site_node_schedules SET
			config_hash='',
			config_pending_since_ms=CASE WHEN enabled=1 THEN ? ELSE 0 END,
			schedule_revision=schedule_revision+1,
			updated_at_ms=?
			WHERE desired_node_id=?`, now.UnixMilli(), now.UnixMilli(), id); err != nil {
			return ControlNode{}, err
		}
	}
	if enabledChanged {
		if err := markAgentConfigsDirtyTx(tx); err != nil {
			return ControlNode{}, err
		}
	} else if portChanged {
		if err := markAgentConfigsDirtyForNodeIDsTx(tx, id); err != nil {
			return ControlNode{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ControlNode{}, err
	}
	return d.controlNodeByID(id, now)
}

func (d *DB) RefreshNodeEnrollment(id int64, now time.Time) (ControlNode, string, error) {
	token, err := newNodeToken()
	if err != nil {
		return ControlNode{}, "", err
	}
	result, err := d.db.Exec(`UPDATE control_nodes SET enrollment_token_hash=?,enrollment_expires_at_ms=?,updated_at_ms=? WHERE id=?`, hashNodeToken(token),
		now.Add(nodeEnrollmentLifetime).UnixMilli(), now.UnixMilli(), id)
	if err != nil {
		return ControlNode{}, "", err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return ControlNode{}, "", err
	}
	if rows != 1 {
		return ControlNode{}, "", errNodeNotFound
	}
	node, err := d.controlNodeByID(id, now)
	return node, token, err
}

func (d *DB) DeleteControlNode(id int64) error {
	d.nodeTLSMutationMu.Lock()
	defer d.nodeTLSMutationMu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var guid string
	if err := tx.QueryRow("SELECT guid FROM control_nodes WHERE id=?", id).Scan(&guid); errors.Is(err, sql.ErrNoRows) {
		return errNodeNotFound
	} else if err != nil {
		return err
	}
	// Keep a durable cleanup handle before removing the node row. If the
	// managed TLS directory is temporarily unavailable, the scheduler can retry
	// cleanup without resurrecting the deleted control node.
	if validTLSNodeGUID(guid) {
		if _, err := tx.Exec(`INSERT INTO node_tls_cleanup_jobs(node_guid,created_at_ms,attempts,last_error,updated_at_ms)
			VALUES(?,?,0,'',?) ON CONFLICT(node_guid) DO UPDATE SET last_error='',updated_at_ms=excluded.updated_at_ms`, guid, time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("UPDATE node_scheduler_settings SET manual_node_id=NULL,active_node_id=NULL WHERE manual_node_id=? OR active_node_id=?", id, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE site_node_schedules SET enabled=0,fixed_node_id=NULL,desired_node_id=NULL,applied_node_id=NULL,
		dns_status='disabled',last_error='',schedule_revision=schedule_revision+1 WHERE fixed_node_id=? OR desired_node_id=? OR applied_node_id=?`, id, id, id); err != nil {
		return err
	}
	result, err := tx.Exec("DELETE FROM control_nodes WHERE id=?", id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return errNodeNotFound
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// The database deletion is authoritative. Complete the cleanup immediately
	// when possible; failures remain durable for the scheduler retry loop.
	if err := d.retryNodeTLSCleanupUnlocked(guid); err != nil {
		log.Printf("[node] removed node %d but managed Edge TLS cleanup is pending: %v", id, err)
	}
	return nil
}

func (d *DB) retryNodeTLSCleanup(onlyGUID string) error {
	d.nodeTLSMutationMu.Lock()
	defer d.nodeTLSMutationMu.Unlock()
	return d.retryNodeTLSCleanupUnlocked(onlyGUID)
}

func (d *DB) retryNodeTLSCleanupUnlocked(onlyGUID string) error {
	if d == nil || d.db == nil {
		return nil
	}
	query := "SELECT node_guid FROM node_tls_cleanup_jobs"
	args := []interface{}{}
	if strings.TrimSpace(onlyGUID) != "" {
		query += " WHERE node_guid=?"
		args = append(args, onlyGUID)
	}
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return err
	}
	guids := make([]string, 0)
	for rows.Next() {
		var guid string
		if err := rows.Scan(&guid); err != nil {
			_ = rows.Close()
			return err
		}
		guids = append(guids, guid)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	var failures []error
	for _, guid := range guids {
		if err := removeManagedEdgeNodeTLS(d.dbPath, guid); err != nil {
			failures = append(failures, fmt.Errorf("node %s: %w", guid, err))
			_, _ = d.db.Exec(`UPDATE node_tls_cleanup_jobs SET attempts=attempts+1,last_error=?,updated_at_ms=? WHERE node_guid=?`, err.Error(), time.Now().UnixMilli(), guid)
			continue
		}
		if _, err := d.db.Exec("DELETE FROM node_tls_cleanup_jobs WHERE node_guid=?", guid); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (d *DB) AuthorizeEnrollmentToken(token string, now time.Time) error {
	var count int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM control_nodes WHERE enrollment_token_hash=? AND enrollment_expires_at_ms>?`, hashNodeToken(token), now.UnixMilli()).Scan(&count)
	if err != nil {
		return err
	}
	if count != 1 {
		return errInvalidNodeToken
	}
	return nil
}

func (d *DB) EnrollControlNode(token string, now time.Time) (ControlNode, string, error) {
	if jwtSecretEphemeral {
		return ControlNode{}, "", errPersistentJWTRequired
	}
	agentToken, err := newNodeToken()
	if err != nil {
		return ControlNode{}, "", err
	}
	probeSecret, err := newNodeProbeSecret()
	if err != nil {
		return ControlNode{}, "", err
	}
	probeSecretCiphertext, err := encryptNodeProbeSecretWithSecret(probeSecret, jwtSecret)
	if err != nil {
		return ControlNode{}, "", err
	}
	tx, err := d.db.Begin()
	if err != nil {
		return ControlNode{}, "", err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRow(`SELECT id FROM control_nodes WHERE enrollment_token_hash=? AND enrollment_expires_at_ms>?`, hashNodeToken(token), now.UnixMilli()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ControlNode{}, "", errInvalidNodeToken
	}
	if err != nil {
		return ControlNode{}, "", err
	}
	result, err := tx.Exec(`UPDATE control_nodes SET enrollment_token_hash='',enrollment_expires_at_ms=0,
		agent_token_hash=?,probe_secret_ciphertext=?,enrolled_at_ms=?,updated_at_ms=? WHERE id=? AND enrollment_token_hash<>''`, hashNodeToken(agentToken), probeSecretCiphertext, now.UnixMilli(), now.UnixMilli(), id)
	if err != nil {
		return ControlNode{}, "", err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return ControlNode{}, "", errInvalidNodeToken
	}
	if err := tx.Commit(); err != nil {
		return ControlNode{}, "", err
	}
	node, err := d.controlNodeByID(id, now)
	return node, agentToken, err
}

func validateNodeReport(report NodeReport) error {
	report.BootID = strings.TrimSpace(report.BootID)
	report.ReportSessionID = strings.TrimSpace(report.ReportSessionID)
	report.CounterEpoch = strings.TrimSpace(report.CounterEpoch)
	report.InterfaceName = strings.TrimSpace(report.InterfaceName)
	if report.BootID == "" || len(report.BootID) > 128 || len(report.ReportSessionID) > 128 || len(report.AgentLeaseID) > 128 || report.SessionEpoch < 0 || len(report.CounterEpoch) > 128 || len(report.SiteCounterEpoch) > 128 {
		return errors.New("invalid boot_id")
	}
	if report.Sequence <= 0 || report.TelemetrySequence < 0 || report.RXBytes < 0 || report.TXBytes < 0 || report.CacheClearGeneration < 0 {
		return errors.New("invalid traffic counters")
	}
	if report.InterfaceName == "" || len(report.InterfaceName) > 64 || len(report.AgentVersion) > 128 || len(report.AppliedConfigHash) > 128 || len(report.ApplyError) > 1024 || len(report.ListenerError) > 1024 || len(report.EventSpoolError) > 1024 || report.EventQueueDepth < 0 || report.EventQueueDepth > edgeEventQueueLimit || report.EventDropped < 0 {
		return errors.New("invalid agent metadata")
	}
	if len(report.SiteStats) > 512 {
		return errors.New("too many site stats")
	}
	if len(report.MediaCounts) > maxNodeTelemetryItemsPerReport || len(report.Retention) > maxNodeTelemetryItemsPerReport || len(report.Observations) > maxNodeTelemetryItemsPerReport {
		return errors.New("too many telemetry items")
	}
	if len(report.Events) > maxNodeRequestEventsPerReport {
		return errors.New("too many request events")
	}
	for _, event := range report.Events {
		if err := validateNodeRequestEvent(event); err != nil {
			return err
		}
	}
	for _, stat := range report.SiteStats {
		if stat.SiteID < 0 || strings.TrimSpace(stat.Host) == "" || len(stat.Host) > 255 || stat.RequestCount < 0 || stat.LastRequestAtMS < 0 || stat.LastStatus < 0 || stat.LastStatus > 999 || stat.BytesIn < 0 || stat.BytesOut < 0 || stat.CumulativeBytesIn < 0 || stat.CumulativeBytesOut < 0 || stat.CacheSizeBytes < 0 {
			return errors.New("invalid site stats")
		}
	}
	for _, count := range report.MediaCounts {
		if count.SiteID <= 0 || count.MovieCount < 0 || count.SeriesCount < 0 || count.EpisodeCount < 0 || count.ObservedAtMS <= 0 || count.MovieCount > mediaLibraryCountMaxValue || count.SeriesCount > mediaLibraryCountMaxValue || count.EpisodeCount > mediaLibraryCountMaxValue {
			return errors.New("invalid media counts")
		}
	}
	for _, status := range report.Retention {
		if status.SiteID <= 0 || status.ExpectedStartedAtMS <= 0 || status.CompletedAtMS < status.ExpectedStartedAtMS {
			return errors.New("invalid retention status")
		}
	}
	for _, observation := range report.Observations {
		if observation.SiteID <= 0 || observation.ObservedAtMS <= 0 || !validDynamicObservationEnums(observation.Source, observation.Decision, observation.ReasonCode) || !isCanonicalDynamicObservationAuthority(observation.CanonicalAuthority) {
			return errors.New("invalid dynamic observation")
		}
	}
	return nil
}

func validateNodeRequestEvent(event NodeRequestEvent) error {
	if event.EventID <= 0 || (event.EventUID != "" && (len(event.EventUID) != 32 || !isHexString(event.EventUID))) || event.SiteID <= 0 || len(event.Host) > 255 || len(event.Method) > 16 || len(event.Path) > 2048 || len(event.Query) > 4096 || event.StatusCode < 0 || event.StatusCode > 999 || len(event.ClientIP) > 64 || len(event.UserAgent) > 512 || len(event.Authorization) > 8192 || len(event.Body) > maxNodeRequestEventBodyBytes || len(event.ContentType) > 128 || len(event.ContentEncoding) > 64 || len(event.ResponseBody) > maxNodeRequestEventResponseBodyBytes || len(event.ResponseContentType) > 128 || len(event.ResponseContentEncoding) > 64 || len(event.ResourceCategory) > 32 || len(event.UpstreamUserAgent) > 512 || len(event.BackendAddress) > 2048 || len(event.InboundColo) > 64 || len(event.OutboundColo) > 64 || event.RecordedAtMS <= 0 || (event.Priority != "" && event.Priority != nodeEventPriorityCritical && event.Priority != nodeEventPriorityBestEffort) {
		return errors.New("invalid request event")
	}
	if event.WatchHistory != nil {
		if event.WatchHistory.SiteID != event.SiteID || !validWatchHistoryEvent(*event.WatchHistory) {
			return errors.New("invalid watch history event")
		}
	}
	return nil
}

type authorizedNodeSite struct {
	ID          int64
	PublicHost  string
	HostAliases map[string]struct{}
}

// authorizedNodeSitesTx is the single source of truth for report-side site
// authorization. A site remains authorized while desired/applied points at the
// reporting node and, after a completed DNS move, through its bounded drain
// window so admitted streams can report their final traffic.
func authorizedNodeSitesTx(tx *sql.Tx, nodeID, nowMS int64) (map[int64]authorizedNodeSite, error) {
	rows, err := tx.Query(`
		SELECT s.id, LOWER(TRIM(s.public_host))
		FROM site_node_schedules n
		JOIN sites s ON s.id=n.site_id
		WHERE n.enabled=1
		  AND s.enabled=1
		  AND (n.desired_node_id=? OR n.applied_node_id=?)
	`, nodeID, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64]authorizedNodeSite)
	for rows.Next() {
		var site authorizedNodeSite
		if err := rows.Scan(&site.ID, &site.PublicHost); err != nil {
			return nil, err
		}
		site.HostAliases = map[string]struct{}{site.PublicHost: {}}
		result[site.ID] = site
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A disabled route remains authorized for the short finalization window
	// represented by its revocation tombstone. This lets the Agent flush its
	// last counters and queued events after ForceStop without reopening the
	// route for scheduling. Deleted sites have no durable application state to
	// update, so they are intentionally omitted here while the tombstone still
	// drives the Agent-side force stop.
	revocationRows, err := tx.Query(`SELECT r.site_id,
		LOWER(TRIM(COALESCE(NULLIF(r.public_host,''),s.public_host,'')))
		FROM agent_route_revocations r
		LEFT JOIN sites s ON s.id=r.site_id
		WHERE r.node_id=? AND s.id IS NOT NULL
		  AND (r.acked_at_ms<=0 OR r.finalization_expires_at_ms<=0 OR r.finalization_expires_at_ms > ?)`, nodeID, nowMS)
	if err != nil {
		return nil, err
	}
	for revocationRows.Next() {
		var siteID int64
		var host string
		if err := revocationRows.Scan(&siteID, &host); err != nil {
			revocationRows.Close()
			return nil, err
		}
		if siteID <= 0 || host == "" {
			continue
		}
		if _, exists := result[siteID]; !exists {
			result[siteID] = authorizedNodeSite{ID: siteID, PublicHost: host, HostAliases: map[string]struct{}{host: {}}}
		}
	}
	if err := revocationRows.Err(); err != nil {
		revocationRows.Close()
		return nil, err
	}
	if err := revocationRows.Close(); err != nil {
		return nil, err
	}
	// Public-host renames do not create DNS drain rows. Keep old hosts pending
	// until the replacement config is acknowledged, then retain them only for
	// the bounded finalization window so offline Agents do not lose buffered
	// events without widening authorization to another site.
	aliasRows, err := tx.Query(`SELECT site_id, LOWER(TRIM(public_host))
		FROM site_node_host_aliases
		WHERE node_id=?
		  AND (acked_at_ms<=0 OR finalization_expires_at_ms<=0 OR finalization_expires_at_ms>?)`, nodeID, nowMS)
	if err != nil {
		return nil, err
	}
	for aliasRows.Next() {
		var siteID int64
		var host string
		if err := aliasRows.Scan(&siteID, &host); err != nil {
			return nil, err
		}
		if host == "" {
			continue
		}
		if site, ok := result[siteID]; ok {
			if site.HostAliases == nil {
				site.HostAliases = make(map[string]struct{})
			}
			site.HostAliases[host] = struct{}{}
			result[siteID] = site
		}
	}
	if err := aliasRows.Err(); err != nil {
		aliasRows.Close()
		return nil, err
	}
	if err := aliasRows.Close(); err != nil {
		return nil, err
	}
	// A site may have multiple in-flight drain generations. Keep each former
	// host as an alias so late events from an admitted old bundle remain valid.
	drainRows, err := tx.Query(`SELECT d.site_id, LOWER(TRIM(d.public_host))
		FROM site_node_drains d JOIN sites s ON s.id=d.site_id
		WHERE d.node_id=? AND (d.acked_at_ms<=0 OR d.finalization_expires_at_ms<=0 OR d.finalization_expires_at_ms > ?)`, nodeID, nowMS)
	if err != nil {
		return nil, err
	}
	defer drainRows.Close()
	for drainRows.Next() {
		var siteID int64
		var host string
		if err := drainRows.Scan(&siteID, &host); err != nil {
			return nil, err
		}
		if site, ok := result[siteID]; ok {
			if site.HostAliases == nil {
				site.HostAliases = make(map[string]struct{})
			}
			if host != "" {
				site.HostAliases[host] = struct{}{}
			}
			result[siteID] = site
		} else {
			var currentHost string
			if err := tx.QueryRow("SELECT LOWER(TRIM(public_host)) FROM sites WHERE id=?", siteID).Scan(&currentHost); err == nil {
				result[siteID] = authorizedNodeSite{ID: siteID, PublicHost: currentHost, HostAliases: map[string]struct{}{host: {}}}
			}
		}
	}
	return result, drainRows.Err()
}

func authorizedNodeSiteHost(sites map[int64]authorizedNodeSite, siteID int64, host string) bool {
	site, ok := sites[siteID]
	if !ok {
		return false
	}
	normalized := requestPublicHost(strings.TrimSpace(host))
	if normalized == "" {
		return false
	}
	if strings.EqualFold(normalized, site.PublicHost) {
		return true
	}
	for alias := range site.HostAliases {
		if strings.EqualFold(normalized, alias) {
			return true
		}
	}
	return false
}

// authorizedNodeSiteForStat accepts legacy Host-only reports but requires the
// supplied host to match when a current Agent includes its stable SiteID.
func authorizedNodeSiteForStat(sites map[int64]authorizedNodeSite, stat NodeSiteStat) (authorizedNodeSite, bool) {
	if stat.SiteID > 0 {
		site, ok := sites[stat.SiteID]
		return site, ok && authorizedNodeSiteHost(sites, stat.SiteID, stat.Host)
	}
	host := requestPublicHost(strings.TrimSpace(stat.Host))
	for _, site := range sites {
		if authorizedNodeSiteHost(sites, site.ID, host) {
			return site, true
		}
	}
	return authorizedNodeSite{}, false
}

func (d *DB) authorizedNodeSitesForAgentToken(token string) (int64, map[int64]authorizedNodeSite, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()
	var nodeID int64
	if err := tx.QueryRow("SELECT id FROM control_nodes WHERE agent_token_hash=?", hashNodeToken(token)).Scan(&nodeID); errors.Is(err, sql.ErrNoRows) {
		return 0, nil, errInvalidAgentToken
	} else if err != nil {
		return 0, nil, err
	}
	sites, err := authorizedNodeSitesTx(tx, nodeID, time.Now().UnixMilli())
	if err != nil {
		return 0, nil, err
	}
	return nodeID, sites, nil
}

func (d *DB) recordAgentSecurityRejection(nodeID, siteID int64, category string) {
	if d == nil {
		return
	}
	d.agentSecurityRejected.Add(1)
	switch category {
	case "request-event":
		d.agentSecurityRequestEvent.Add(1)
	case "site-stats":
		d.agentSecuritySiteStat.Add(1)
	case "media-counts":
		d.agentSecurityMediaCount.Add(1)
	case "retention":
		d.agentSecurityRetention.Add(1)
	case "observation":
		d.agentSecurityObservation.Add(1)
	}
	d.agentSecurityLastRejectedMS.Store(time.Now().UnixMilli())
	// SiteID is deliberately excluded from the log key. It is attacker
	// controlled input and must not create an unbounded map entry per spoofed
	// site. One bounded key per node/category is sufficient for rate limiting.
	key := fmt.Sprintf("%d:%s", nodeID, category)
	now := time.Now()
	d.agentSecurityMu.Lock()
	if d.agentSecurityLastLog == nil {
		d.agentSecurityLastLog = make(map[string]time.Time)
	}
	for existingKey, timestamp := range d.agentSecurityLastLog {
		if now.Sub(timestamp) >= 10*time.Minute {
			delete(d.agentSecurityLastLog, existingKey)
		}
	}
	if len(d.agentSecurityLastLog) >= 4096 {
		var oldestKey string
		var oldest time.Time
		for existingKey, timestamp := range d.agentSecurityLastLog {
			if oldestKey == "" || timestamp.Before(oldest) {
				oldestKey, oldest = existingKey, timestamp
			}
		}
		if oldestKey != "" {
			delete(d.agentSecurityLastLog, oldestKey)
		}
	}
	last := d.agentSecurityLastLog[key]
	if last.IsZero() || now.Sub(last) >= time.Minute {
		d.agentSecurityLastLog[key] = now
		d.agentSecurityMu.Unlock()
		log.Printf("[agent-security] rejected unauthorized site report node_id=%d site_id=%d category=%s", nodeID, siteID, category)
		return
	}
	d.agentSecurityMu.Unlock()
}

func (d *DB) agentSecuritySnapshot() AgentSecurityDiagnostics {
	if d == nil {
		return AgentSecurityDiagnostics{}
	}
	result := AgentSecurityDiagnostics{
		RejectedTotal: d.agentSecurityRejected.Load(),
		RequestEvent:  d.agentSecurityRequestEvent.Load(),
		SiteStat:      d.agentSecuritySiteStat.Load(),
		MediaCount:    d.agentSecurityMediaCount.Load(),
		Retention:     d.agentSecurityRetention.Load(),
		Observation:   d.agentSecurityObservation.Load(),
	}
	if timestamp := d.agentSecurityLastRejectedMS.Load(); timestamp > 0 {
		result.LastRejectedAt = time.UnixMilli(timestamp).UTC().Format(time.RFC3339)
	}
	return result
}

func isHexString(value string) bool {
	if value == "" {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (d *DB) recordNodeRequestEventTx(tx *sql.Tx, nodeID int64, event NodeRequestEvent) error {
	var siteName string
	if err := tx.QueryRow("SELECT name FROM sites WHERE id=?", event.SiteID).Scan(&siteName); err != nil {
		return nil
	}
	rawPath := event.Path
	if event.Query != "" {
		rawPath += "?" + event.Query
	}
	category := event.ResourceCategory
	if !validRequestLogCategory(category) {
		category = requestLogCategoryAPI
		switch {
		case strings.Contains(strings.ToLower(rawPath), "/sessions/playing"):
			category = requestLogCategoryPlaybackSync
		case strings.Contains(strings.ToLower(rawPath), "/playbackinfo"):
			category = requestLogCategoryPlayback
		case strings.Contains(strings.ToLower(rawPath), "/videos/"):
			category = requestLogCategoryStream
		}
	}
	if !validRequestLogCategory(category) {
		category = requestLogCategoryAPI
	}
	var finalNode string
	if err := tx.QueryRow("SELECT name FROM control_nodes WHERE id=?", nodeID).Scan(&finalNode); err != nil {
		finalNode = fmt.Sprintf("节点 #%d", nodeID)
	}
	backendAddress := event.BackendAddress
	if backendAddress == "" {
		backendAddress = fmt.Sprintf("node:%d", nodeID)
	}
	_, err := tx.Exec(`INSERT INTO request_logs(site_id,site_name,final_node,resource_category,status_code,client_ip,user_agent,upstream_user_agent,backend_address,inbound_colo,outbound_colo,method,path,recorded_at_ms,timeline_at_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, event.SiteID, siteName, finalNode, category, event.StatusCode, event.ClientIP, event.UserAgent, event.UpstreamUserAgent, backendAddress, event.InboundColo, event.OutboundColo, event.Method, requestLogSafeText(event.Path, requestLogMaxPathBytes), event.RecordedAtMS, event.RecordedAtMS)
	return err
}

func recordNodeSiteTrafficTx(tx *sql.Tx, nodeID int64, counterEpoch string, stat NodeSiteStat, nowMS int64, allowedSites map[int64]authorizedNodeSite) error {
	site, ok := authorizedNodeSiteForStat(allowedSites, stat)
	if !ok {
		return nil
	}
	siteID := site.ID
	currentIn, currentOut := stat.CumulativeBytesIn, stat.CumulativeBytesOut
	if currentIn == 0 && stat.BytesIn > 0 {
		currentIn = stat.BytesIn
	}
	if currentOut == 0 && stat.BytesOut > 0 {
		currentOut = stat.BytesOut
	}
	var previousBoot string
	var previousIn, previousOut, previousRequests, previousCacheSize int64
	err := tx.QueryRow(`SELECT boot_id,last_bytes_in,last_bytes_out,last_request_count,cache_size_bytes FROM node_site_counters WHERE node_id=? AND site_id=?`, nodeID, siteID).Scan(&previousBoot, &previousIn, &previousOut, &previousRequests, &previousCacheSize)
	if errors.Is(err, sql.ErrNoRows) {
		// The first sample is the delta from the Agent runtime's initial zero
		// state, rather than merely a counter baseline. Persist it immediately
		// so the first report is visible in history and lifetime site totals.
		initialIn, initialOut := stat.BytesIn, stat.BytesOut
		// RequestCount is the cumulative count for this Agent runtime epoch. The
		// first report is the segment from zero, so persist it together with the
		// first byte deltas instead of silently dropping the first batch.
		initialRequests := stat.RequestCount
		if initialIn > 0 || initialOut > 0 || initialRequests > 0 {
			bucket := (nowMS / 60000) * 60000
			if _, err = tx.Exec(`INSERT INTO node_site_traffic_logs(node_id,site_id,bytes_in,bytes_out,requests,recorded_at_ms) VALUES(?,?,?,?,?,?) ON CONFLICT(node_id,site_id,recorded_at_ms) DO UPDATE SET bytes_in=bytes_in+excluded.bytes_in,bytes_out=bytes_out+excluded.bytes_out,requests=requests+excluded.requests`, nodeID, siteID, initialIn, initialOut, initialRequests, bucket); err != nil {
				return err
			}
			if _, err = tx.Exec(`UPDATE sites SET traffic_used=traffic_used+?+?,traffic_used_in=traffic_used_in+?,traffic_used_out=traffic_used_out+?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, initialIn, initialOut, initialIn, initialOut, siteID); err != nil {
				return err
			}
		}
		cacheSize := int64(0)
		if stat.CacheSizeValid {
			cacheSize = stat.CacheSizeBytes
		}
		_, err = tx.Exec(`INSERT INTO node_site_counters(node_id,site_id,boot_id,last_bytes_in,last_bytes_out,last_request_count,cache_size_bytes,updated_at_ms) VALUES(?,?,?,?,?,?,?,?)`, nodeID, siteID, counterEpoch, currentIn, currentOut, stat.RequestCount, cacheSize, nowMS)
		return err
	}
	if err != nil {
		return err
	}
	var deltaIn, deltaOut, deltaRequests int64
	if previousBoot != counterEpoch {
		// A process restart, config apply, or route transition starts a new
		// Agent-side counter epoch. BytesIn/BytesOut are already report deltas;
		// RequestCount is cumulative from zero for the same epoch.
		deltaIn, deltaOut, deltaRequests = stat.BytesIn, stat.BytesOut, stat.RequestCount
		// Older Agents only reported cumulative site counters. Preserve their
		// upgrade compatibility when the cumulative value is still monotonic;
		// a reset (the normal new-runtime case) falls back to the explicit
		// per-report delta above.
		if currentIn >= previousIn && stat.BytesIn == 0 {
			deltaIn = currentIn - previousIn
		}
		if currentOut >= previousOut && stat.BytesOut == 0 {
			deltaOut = currentOut - previousOut
		}
		// Explicit per-site epochs are emitted by current Agents and reset the
		// cumulative request counter to zero for the new route generation. Only
		// legacy reports without CounterEpoch may use the old monotonic
		// subtraction compatibility path; subtracting an older generation here
		// would silently drop the first requests of the new generation.
		if stat.CounterEpoch == 0 && stat.RequestCount >= previousRequests {
			deltaRequests = stat.RequestCount - previousRequests
		}
	} else if currentIn >= previousIn && currentOut >= previousOut && stat.RequestCount >= previousRequests {
		deltaIn, deltaOut, deltaRequests = currentIn-previousIn, currentOut-previousOut, stat.RequestCount-previousRequests
	}
	if deltaIn > 0 || deltaOut > 0 || deltaRequests > 0 {
		bucket := (nowMS / 60000) * 60000
		_, err = tx.Exec(`INSERT INTO node_site_traffic_logs(node_id,site_id,bytes_in,bytes_out,requests,recorded_at_ms) VALUES(?,?,?,?,?,?) ON CONFLICT(node_id,site_id,recorded_at_ms) DO UPDATE SET bytes_in=bytes_in+excluded.bytes_in,bytes_out=bytes_out+excluded.bytes_out,requests=requests+excluded.requests`, nodeID, siteID, deltaIn, deltaOut, deltaRequests, bucket)
		if err != nil {
			return err
		}
		if deltaIn > 0 || deltaOut > 0 {
			if _, err = tx.Exec(`UPDATE sites SET traffic_used=traffic_used+?+?,traffic_used_in=traffic_used_in+?,traffic_used_out=traffic_used_out+?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, deltaIn, deltaOut, deltaIn, deltaOut, siteID); err != nil {
				return err
			}
		}
	}
	cacheSize := previousCacheSize
	if stat.CacheSizeValid {
		cacheSize = stat.CacheSizeBytes
	}
	_, err = tx.Exec(`UPDATE node_site_counters SET boot_id=?,last_bytes_in=?,last_bytes_out=?,last_request_count=?,cache_size_bytes=?,updated_at_ms=? WHERE node_id=? AND site_id=?`, counterEpoch, currentIn, currentOut, stat.RequestCount, cacheSize, nowMS, nodeID, siteID)
	if err != nil {
		return err
	}
	// Final is an Agent-side counter watermark, not proof that every paginated
	// telemetry/event queue is empty. Keep the drain authorization tombstone
	// until its bounded expiry so later pages remain accepted.
	return nil
}

type nodeReportCommitResult struct {
	node                        ControlNode
	acceptedSiteIDs             []int64
	discardedSiteIDs            []int64
	acceptedMediaSiteIDs        []int64
	discardedMediaSiteIDs       []int64
	acceptedRetentionSiteIDs    []int64
	discardedRetentionSiteIDs   []int64
	acceptedObservationSiteIDs  []int64
	discardedObservationSiteIDs []int64
	acceptedNew                 []NodeRequestEvent
	acceptedDuplicate           []NodeRequestEvent
	pendingEffects              []NodeRequestEvent
	discardedIDs                []int64
	discardedUIDs               []string
}

func appendUniqueNodeSiteID(ids *[]int64, siteID int64) {
	if siteID <= 0 {
		return
	}
	for _, existing := range *ids {
		if existing == siteID {
			return
		}
	}
	*ids = append(*ids, siteID)
}

func appendNodeEventIdentity(ids *[]int64, uids *[]string, event NodeRequestEvent) {
	if event.EventID > 0 {
		*ids = append(*ids, event.EventID)
	}
	if len(event.EventUID) == 32 && isHexString(event.EventUID) {
		*uids = append(*uids, event.EventUID)
	}
}

// claimAgentLeaseTx binds a process session to the node before the first
// report is accepted. A newer session epoch rotates the lease; an older
// session can no longer overwrite state after a restart or failover.
func claimAgentLeaseTx(tx *sql.Tx, nodeID int64, sessionID string, sessionEpoch int64) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || sessionEpoch <= 0 {
		var lease string
		if err := tx.QueryRow("SELECT agent_lease_id FROM control_nodes WHERE id=?", nodeID).Scan(&lease); err != nil {
			return "", err
		}
		return strings.TrimSpace(lease), nil
	}
	var activeSession, lease string
	var activeEpoch int64
	if err := tx.QueryRow("SELECT active_agent_session_id,agent_session_epoch,agent_lease_id FROM control_nodes WHERE id=?", nodeID).Scan(&activeSession, &activeEpoch, &lease); err != nil {
		return "", err
	}
	activeSession = strings.TrimSpace(activeSession)
	lease = strings.TrimSpace(lease)
	if activeSession == sessionID && activeEpoch == sessionEpoch && lease != "" {
		return lease, nil
	}
	if activeSession != "" && sessionEpoch <= activeEpoch {
		return "", errStaleAgentSession
	}
	newLease, err := newNodeToken()
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec("UPDATE control_nodes SET active_agent_session_id=?,agent_session_epoch=?,agent_lease_id=?,updated_at_ms=? WHERE id=?", sessionID, sessionEpoch, newLease, time.Now().UnixMilli(), nodeID); err != nil {
		return "", err
	}
	return newLease, nil
}

func (d *DB) recordNodeReportCommit(agentToken string, report NodeReport, now time.Time) (nodeReportCommitResult, error) {
	result := nodeReportCommitResult{}
	validEvents := make([]NodeRequestEvent, 0, len(report.Events))
	for _, event := range report.Events {
		if validateNodeRequestEvent(event) == nil {
			validEvents = append(validEvents, event)
			continue
		}
		appendNodeEventIdentity(&result.discardedIDs, &result.discardedUIDs, event)
	}
	report.Events = validEvents
	if err := validateNodeReport(report); err != nil {
		return nodeReportCommitResult{}, err
	}
	if err := d.resetDueNodeCycles(now); err != nil {
		return nodeReportCommitResult{}, err
	}
	tx, err := d.db.Begin()
	if err != nil {
		return nodeReportCommitResult{}, err
	}
	defer tx.Rollback()

	var id, lastSequence, lastRX, lastTX int64
	var previousApplyFailures int64
	var cacheClearGeneration int64
	var lastBootID, lastSessionID, activeSessionID, desiredConfigHash, previousApplyError, previousAgentVersion, agentLeaseID string
	var activeSessionEpoch int64
	var configDirty, configRevision, desiredConfigRevision, appliedConfigRevision int64
	err = tx.QueryRow(`SELECT id,last_sequence,last_raw_rx_bytes,last_raw_tx_bytes,last_boot_id,last_report_session_id,active_agent_session_id,agent_session_epoch,agent_lease_id,cache_clear_generation,desired_config_hash,config_dirty,agent_apply_error,agent_apply_failures,config_revision,desired_config_revision,applied_config_revision,agent_version FROM control_nodes WHERE agent_token_hash=?`, hashNodeToken(agentToken)).Scan(
		&id, &lastSequence, &lastRX, &lastTX, &lastBootID, &lastSessionID, &activeSessionID, &activeSessionEpoch, &agentLeaseID, &cacheClearGeneration, &desiredConfigHash, &configDirty, &previousApplyError, &previousApplyFailures, &configRevision, &desiredConfigRevision, &appliedConfigRevision, &previousAgentVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nodeReportCommitResult{}, errInvalidAgentToken
	}
	if err != nil {
		return nodeReportCommitResult{}, err
	}
	if activeSessionID != "" && strings.TrimSpace(report.AgentLeaseID) == "" && report.SessionEpoch == 0 {
		return nodeReportCommitResult{}, errStaleAgentSession
	}
	if strings.TrimSpace(report.AgentLeaseID) != "" || report.SessionEpoch > 0 {
		if strings.TrimSpace(report.AgentLeaseID) == "" || report.SessionEpoch <= 0 || strings.TrimSpace(report.ReportSessionID) != strings.TrimSpace(activeSessionID) || report.SessionEpoch != activeSessionEpoch || strings.TrimSpace(report.AgentLeaseID) != strings.TrimSpace(agentLeaseID) {
			return nodeReportCommitResult{}, errStaleAgentSession
		}
	}

	// Authorization and every report mutation use one transaction. A scheduler
	// transition therefore resolves as either authorized-and-committed or
	// unauthorized-and-discarded; ACKs are never inferred from a preflight.
	allowedSites, err := authorizedNodeSitesTx(tx, id, now.UnixMilli())
	if err != nil {
		return nodeReportCommitResult{}, err
	}

	legacyBootID := strings.TrimSpace(report.BootID)
	sessionID := strings.TrimSpace(report.ReportSessionID)
	if sessionID == "" {
		sessionID = legacyBootID
	}
	siteEpoch := strings.TrimSpace(report.SiteCounterEpoch)
	counterEpoch := strings.TrimSpace(report.CounterEpoch)
	if counterEpoch == "" {
		counterEpoch = legacyBootID
	}
	// Site proxy counters are independent from the host network interface
	// counters above. Bind them to the Agent report session and the applied
	// runtime configuration so a process restart or hot apply starts a fresh
	// epoch even when the kernel boot/interface epoch is unchanged.
	siteCounterEpoch := sessionID
	if siteCounterEpoch == "" {
		siteCounterEpoch = legacyBootID
	}
	if siteEpoch != "" {
		// A monotonic Agent-side apply epoch distinguishes A→B→A route
		// transitions even when the configuration hash returns to an earlier
		// value within the same process session.
		siteCounterEpoch += ":" + siteEpoch
	} else if appliedHash := strings.TrimSpace(report.AppliedConfigHash); appliedHash != "" {
		siteCounterEpoch += ":" + appliedHash
	} else if counterEpoch != "" {
		siteCounterEpoch += ":" + counterEpoch
	}
	deltaRX, deltaTX := int64(0), int64(0)
	if counterEpoch == lastBootID && report.RXBytes >= lastRX && report.TXBytes >= lastTX && (sessionID != lastSessionID || report.Sequence > lastSequence) {
		deltaRX = report.RXBytes - lastRX
		deltaTX = report.TXBytes - lastTX
	}
	if sessionID == lastSessionID && report.Sequence <= lastSequence {
		deltaRX, deltaTX = 0, 0
		report.RXBytes, report.TXBytes, report.Sequence = lastRX, lastTX, lastSequence
	}
	reportedApplyError := strings.TrimSpace(report.ApplyError)
	applyError := reportedApplyError
	appliedHash := strings.TrimSpace(report.AppliedConfigHash)
	desiredHash := strings.TrimSpace(desiredConfigHash)
	clearAppliedConfig := false
	if report.AppliedConfigRevision > 0 {
		clearAppliedConfig = report.AppliedConfigRevision == configRevision &&
			report.AppliedConfigRevision == desiredConfigRevision &&
			appliedHash != "" && appliedHash == desiredHash
	} else {
		// Legacy Agents do not send a revision. They may acknowledge a
		// non-blank desired hash, but can never clear a deliberate blank
		// invalidation marker left by a newer configuration revision.
		clearAppliedConfig = appliedHash != "" && desiredHash != "" && appliedHash == desiredHash
	}
	legacyDiagnostic := false
	if applyError == "" && appliedHash == "" {
		// Older Agents do not know about apply_error. Preserve an existing
		// diagnostic until a report confirms that the desired configuration was
		// actually applied, instead of letting a legacy heartbeat erase it.
		applyError = strings.TrimSpace(previousApplyError)
		legacyDiagnostic = applyError != ""
	}
	applyErrorAtMS := int64(0)
	applyFailures := int64(0)
	if applyError != "" {
		if !legacyDiagnostic && (report.ApplyFailures > 0 || report.ApplyErrorAtMS > 0) {
			// Current Agents report the number and timestamp of the actual apply
			// attempt. Heartbeats must not increment these values.
			applyFailures = report.ApplyFailures
			if applyFailures < 1 {
				applyFailures = 1
			}
			applyErrorAtMS = report.ApplyErrorAtMS
			if applyErrorAtMS <= 0 {
				applyErrorAtMS = now.UnixMilli()
			}
		} else if legacyDiagnostic {
			// A legacy heartbeat only carries the previous error. Preserve its
			// counters rather than treating every heartbeat as another attempt.
			applyFailures = previousApplyFailures
			applyErrorAtMS = now.UnixMilli()
		} else if applyError == strings.TrimSpace(previousApplyError) {
			applyFailures = previousApplyFailures + 1
			applyErrorAtMS = now.UnixMilli()
		} else {
			applyFailures = 1
			applyErrorAtMS = now.UnixMilli()
		}
	} else if clearAppliedConfig {
		// A matching applied hash is the only successful completion signal. It
		// clears the previous failure and its counters atomically with the report.
		applyErrorAtMS = 0
		applyFailures = 0
	}
	if _, err = tx.Exec(`UPDATE control_nodes SET period_rx_bytes=period_rx_bytes+?,period_tx_bytes=period_tx_bytes+?,
			lifetime_rx_bytes=lifetime_rx_bytes+?,lifetime_tx_bytes=lifetime_tx_bytes+?,last_raw_rx_bytes=?,last_raw_tx_bytes=?,
			last_boot_id=?,last_report_session_id=?,last_sequence=?,interface_name=?,agent_version=?,applied_config_hash=?,applied_config_revision=?,agent_apply_error=?,agent_apply_error_at_ms=?,agent_apply_failures=?,agent_listener_error=?,event_spool_error=?,event_queue_depth=?,event_dropped=?,config_dirty=CASE WHEN ? THEN 0 ELSE config_dirty END,cache_clear_applied_generation=CASE WHEN ? > cache_clear_applied_generation AND ? <= ? THEN ? ELSE cache_clear_applied_generation END,last_seen_at_ms=?,updated_at_ms=? WHERE id=?`,
		deltaRX, deltaTX, deltaRX, deltaTX, report.RXBytes, report.TXBytes, counterEpoch, sessionID, report.Sequence,
		strings.TrimSpace(report.InterfaceName), strings.TrimSpace(report.AgentVersion), appliedHash, report.AppliedConfigRevision, applyError, applyErrorAtMS, applyFailures, strings.TrimSpace(report.ListenerError), strings.TrimSpace(report.EventSpoolError), report.EventQueueDepth, report.EventDropped,
		clearAppliedConfig,
		report.CacheClearGeneration, report.CacheClearGeneration, cacheClearGeneration, report.CacheClearGeneration,
		now.UnixMilli(), now.UnixMilli(), id); err != nil {
		return nodeReportCommitResult{}, err
	}
	// A cold-started Agent may report before it has successfully applied the
	// desired runtime config. Start the scheduler's pending clock from that
	// report even when the config hash itself did not change (for example after
	// an Agent process restart). Without this, a persistent apply failure leaves
	// config_pending_since_ms at zero and the automatic failover cooldown can
	// never begin.
	if desiredHash != "" && appliedHash != desiredHash {
		if _, err := tx.Exec(`UPDATE site_node_schedules SET
			config_pending_since_ms=CASE WHEN config_pending_since_ms=0 THEN ? ELSE config_pending_since_ms END,
			updated_at_ms=CASE WHEN config_pending_since_ms=0 THEN ? ELSE updated_at_ms END
			WHERE enabled=1 AND desired_node_id=? AND config_hash=?`, now.UnixMilli(), now.UnixMilli(), id, desiredHash); err != nil {
			return nodeReportCommitResult{}, err
		}
	} else if appliedHash != "" && appliedHash == desiredHash {
		if _, err := tx.Exec(`UPDATE site_node_schedules SET config_pending_since_ms=0,updated_at_ms=?
			WHERE enabled=1 AND desired_node_id=? AND config_hash=? AND config_pending_since_ms>0`, now.UnixMilli(), id, appliedHash); err != nil {
			return nodeReportCommitResult{}, err
		}
	}
	// A matching revision is the Agent's acknowledgement that it received the
	// complete transition payload. Start the finalization window only then;
	// commands and drain authorization must survive an offline Agent rather than
	// expiring from their creation timestamp. Legacy Agents that do not support
	// ForceStopSiteIDs stay pending and are handled by the scheduler fallback.
	if clearAppliedConfig && report.AppliedConfigRevision > 0 {
		ackMS := now.UnixMilli()
		finalizeMS := now.Add(siteNodeDrainWindow).UnixMilli()
		if _, err := tx.Exec(`UPDATE site_node_drains SET acked_at_ms=?,finalization_expires_at_ms=?
			WHERE node_id=? AND acked_at_ms<=0`, ackMS, finalizeMS, id); err != nil {
			return nodeReportCommitResult{}, err
		}
		effectiveAgentVersion := strings.TrimSpace(report.AgentVersion)
		if effectiveAgentVersion == "" {
			effectiveAgentVersion = strings.TrimSpace(previousAgentVersion)
		}
		if agentSupportsForceStop(effectiveAgentVersion) {
			if _, err := tx.Exec(`UPDATE agent_route_revocations SET acked_at_ms=?,finalization_expires_at_ms=?
				WHERE node_id=? AND acked_at_ms<=0`, ackMS, finalizeMS, id); err != nil {
				return nodeReportCommitResult{}, err
			}
		}
		if _, err := tx.Exec(`UPDATE site_node_host_aliases SET acked_at_ms=?,finalization_expires_at_ms=?
			WHERE node_id=? AND acked_at_ms<=0`, ackMS, finalizeMS, id); err != nil {
			return nodeReportCommitResult{}, err
		}
	}

	for _, stat := range report.SiteStats {
		host := strings.ToLower(strings.TrimSpace(stat.Host))
		if host == "" || len(host) > 255 || stat.RequestCount < 0 || stat.LastRequestAtMS < 0 || stat.LastStatus < 0 || stat.LastStatus > 999 {
			continue
		}
		authorizedSite, authorized := authorizedNodeSiteForStat(allowedSites, stat)
		if !authorized {
			d.recordAgentSecurityRejection(id, stat.SiteID, "site-stats")
			appendUniqueNodeSiteID(&result.discardedSiteIDs, stat.SiteID)
			continue
		}
		scheduleID := authorizedSite.ID
		var previousBoot string
		var previousCount int64
		err := tx.QueryRow(`SELECT agent_boot_id,agent_request_count FROM site_node_schedules WHERE site_id=?`, scheduleID).Scan(&previousBoot, &previousCount)
		if err != nil {
			return nodeReportCommitResult{}, err
		}
		count := stat.RequestCount
		if previousBoot != sessionID || count < previousCount {
			previousCount = 0
		}
		if count < previousCount {
			count = previousCount
		}
		if _, err = tx.Exec(`UPDATE site_node_schedules SET agent_boot_id=?,agent_request_count=?,agent_last_request_at_ms=?,agent_last_status=?,updated_at_ms=? WHERE site_id=?`,
			sessionID, count, stat.LastRequestAtMS, stat.LastStatus, now.UnixMilli(), scheduleID); err != nil {
			return nodeReportCommitResult{}, err
		}
		statCounterEpoch := siteCounterEpoch
		if stat.CounterEpoch > 0 {
			statCounterEpoch = fmt.Sprintf("%s:%d", siteCounterEpoch, stat.CounterEpoch)
		}
		if err := recordNodeSiteTrafficTx(tx, id, statCounterEpoch, stat, now.UnixMilli(), allowedSites); err != nil {
			return nodeReportCommitResult{}, err
		}
		appendUniqueNodeSiteID(&result.acceptedSiteIDs, scheduleID)
	}

	for _, count := range report.MediaCounts {
		if _, allowed := allowedSites[count.SiteID]; !allowed {
			d.recordAgentSecurityRejection(id, count.SiteID, "media-counts")
			appendUniqueNodeSiteID(&result.discardedMediaSiteIDs, count.SiteID)
			continue
		}
		if _, err := tx.Exec(`UPDATE sites SET media_movie_count=?,media_series_count=?,media_episode_count=?,media_count_updated_at_ms=? WHERE id=? AND media_count_updated_at_ms<?`, count.MovieCount, count.SeriesCount, count.EpisodeCount, count.ObservedAtMS, count.SiteID, count.ObservedAtMS); err != nil {
			return nodeReportCommitResult{}, err
		}
		appendUniqueNodeSiteID(&result.acceptedMediaSiteIDs, count.SiteID)
	}
	for _, status := range report.Retention {
		if _, allowed := allowedSites[status.SiteID]; !allowed {
			d.recordAgentSecurityRejection(id, status.SiteID, "retention")
			appendUniqueNodeSiteID(&result.discardedRetentionSiteIDs, status.SiteID)
			continue
		}
		if _, err := tx.Exec(`UPDATE sites SET account_retention_started_at_ms=?,account_retention_last_completed_at_ms=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND account_retention_days>0 AND account_retention_started_at_ms=?`, status.CompletedAtMS, status.CompletedAtMS, status.SiteID, status.ExpectedStartedAtMS); err != nil {
			return nodeReportCommitResult{}, err
		}
		appendUniqueNodeSiteID(&result.acceptedRetentionSiteIDs, status.SiteID)
	}
	for _, observation := range report.Observations {
		if _, allowed := allowedSites[observation.SiteID]; !allowed {
			d.recordAgentSecurityRejection(id, observation.SiteID, "observation")
			appendUniqueNodeSiteID(&result.discardedObservationSiteIDs, observation.SiteID)
			continue
		}
		if _, err := tx.Exec(`INSERT INTO dynamic_observations(site_id,canonical_authority,source,decision,reason_code,first_seen_ms,last_seen_ms,count) VALUES(?,?,?,?,?,?,?,1) ON CONFLICT(site_id,canonical_authority,source,decision,reason_code) DO UPDATE SET last_seen_ms=excluded.last_seen_ms,count=count+1`, observation.SiteID, observation.CanonicalAuthority, observation.Source, observation.Decision, observation.ReasonCode, observation.ObservedAtMS, observation.ObservedAtMS); err != nil {
			return nodeReportCommitResult{}, err
		}
		appendUniqueNodeSiteID(&result.acceptedObservationSiteIDs, observation.SiteID)
	}

	for _, event := range report.Events {
		if !authorizedNodeSiteHost(allowedSites, event.SiteID, event.Host) {
			d.recordAgentSecurityRejection(id, event.SiteID, "request-event")
			appendNodeEventIdentity(&result.discardedIDs, &result.discardedUIDs, event)
			continue
		}
		inserted, err := tx.Exec(`INSERT OR IGNORE INTO node_request_events(node_id,agent_boot_id,event_id,event_uid,received_at_ms,processed_at_ms) VALUES(?,?,?,?,?,0)`, id, sessionID, event.EventID, event.EventUID, now.UnixMilli())
		if err != nil {
			return nodeReportCommitResult{}, err
		}
		rows, err := inserted.RowsAffected()
		if err != nil {
			return nodeReportCommitResult{}, err
		}
		if rows == 0 {
			var existingBoot, existingUID string
			var existingID int64
			var processedAtMS int64
			lookupErr := tx.QueryRow(`SELECT agent_boot_id,event_id,event_uid,processed_at_ms FROM node_request_events WHERE node_id=? AND ((agent_boot_id=? AND event_id=?) OR (event_uid<>'' AND event_uid=?)) LIMIT 1`, id, sessionID, event.EventID, event.EventUID).Scan(&existingBoot, &existingID, &existingUID, &processedAtMS)
			if errors.Is(lookupErr, sql.ErrNoRows) {
				return nodeReportCommitResult{}, errors.New(`event insert was ignored without an existing identity`)
			}
			if lookupErr != nil {
				return nodeReportCommitResult{}, lookupErr
			}
			if (event.EventUID != "" && existingUID != "" && !strings.EqualFold(event.EventUID, existingUID)) ||
				(existingBoot != sessionID && existingUID == "") ||
				(existingBoot == sessionID && existingID != event.EventID) {
				appendNodeEventIdentity(&result.discardedIDs, &result.discardedUIDs, event)
				continue
			}
			if processedAtMS > 0 {
				result.acceptedDuplicate = append(result.acceptedDuplicate, event)
			} else {
				result.pendingEffects = append(result.pendingEffects, event)
			}
			continue
		}
		if !event.SkipRequestLog {
			if err := d.recordNodeRequestEventTx(tx, id, event); err != nil {
				return nodeReportCommitResult{}, err
			}
		}
		result.pendingEffects = append(result.pendingEffects, event)
	}

	if err := tx.Commit(); err != nil {
		return nodeReportCommitResult{}, err
	}
	// Publish the committed full-report sample to the dashboard overlay only
	// after the transaction succeeds. The shared telemetry sequence keeps this
	// overlay ordered with the lightweight live-report channel.
	d.recordNodeFullTelemetry(id, report, result.acceptedSiteIDs, now)
	result.node, err = d.controlNodeByID(id, now)
	if err != nil {
		return nodeReportCommitResult{}, err
	}
	// Derived effects are acknowledged only after they have been accepted by
	// their bounded queue/observer. If an effect fails, leave processed_at_ms at
	// zero so the Agent retries the event instead of deleting it permanently.
	for _, event := range result.pendingEffects {
		if err := d.processNodeEvent(event); err != nil {
			log.Printf("[node-event] deferred event node=%d site=%d: %v", id, event.SiteID, err)
			continue
		}
		if _, markErr := d.db.Exec(`UPDATE node_request_events SET processed_at_ms=? WHERE node_id=? AND ((agent_boot_id=? AND event_id=?) OR (event_uid<>'' AND event_uid=?))`, now.UnixMilli(), id, sessionID, event.EventID, event.EventUID); markErr != nil {
			log.Printf("[node-event] could not mark event processed node=%d site=%d: %v", id, event.SiteID, markErr)
			continue
		}
		result.acceptedNew = append(result.acceptedNew, event)
	}
	_, _ = d.NodeControlSnapshot(now)
	return result, nil
}

func (d *DB) RecordNodeReport(agentToken string, report NodeReport, now time.Time) (ControlNode, error) {
	result, err := d.recordNodeReportCommit(agentToken, report, now)
	if err != nil {
		return ControlNode{}, err
	}
	return result.node, nil
}

// RecordNodeReportResult is the acknowledgement-safe variant used by HTTP
// and WebSocket handlers. It authenticates, snapshots authorization, commits
// mutations, and derives ACK/discarded lists from that same transaction.
func (d *DB) RecordNodeReportResult(agentToken string, report NodeReport, now time.Time) (NodeReportResult, error) {
	committed, err := d.recordNodeReportCommit(agentToken, report, now)
	if err != nil {
		return NodeReportResult{}, err
	}
	result := NodeReportResult{Node: committed.node}
	result.AcceptedSiteIDs = committed.acceptedSiteIDs
	result.DiscardedSiteIDs = committed.discardedSiteIDs
	result.AcceptedMediaSiteIDs = committed.acceptedMediaSiteIDs
	result.DiscardedMediaSiteIDs = committed.discardedMediaSiteIDs
	result.AcceptedRetentionSiteIDs = committed.acceptedRetentionSiteIDs
	result.DiscardedRetentionSiteIDs = committed.discardedRetentionSiteIDs
	result.AcceptedObservationSiteIDs = committed.acceptedObservationSiteIDs
	result.DiscardedObservationSiteIDs = committed.discardedObservationSiteIDs
	for _, event := range committed.acceptedNew {
		appendNodeEventIdentity(&result.AcceptedEventIDs, &result.AcceptedEventUIDs, event)
	}
	for _, event := range committed.acceptedDuplicate {
		appendNodeEventIdentity(&result.AcceptedEventIDs, &result.AcceptedEventUIDs, event)
	}
	result.DiscardedEventIDs = committed.discardedIDs
	result.DiscardedEventUIDs = committed.discardedUIDs
	return result, nil
}

func (d *DB) recordNodeWatchHistoryEvent(event NodeRequestEvent) error {
	if d == nil || event.Body == "" || event.StatusCode < 200 || event.StatusCode >= 300 {
		return nil
	}
	site, err := d.GetSite(event.SiteID)
	if err != nil || !site.WatchHistoryEnabled {
		return nil
	}
	parsed, err := url.Parse("https://" + event.Host + event.Path)
	if err != nil {
		return err
	}
	parsed.RawQuery = event.Query
	req, err := http.NewRequest(event.Method, parsed.String(), strings.NewReader(event.Body))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", event.UserAgent)
	if event.ContentType != "" {
		req.Header.Set("Content-Type", event.ContentType)
	}
	if event.ContentEncoding != "" {
		req.Header.Set("Content-Encoding", event.ContentEncoding)
	}
	if event.Authorization != "" {
		req.Header.Set("Authorization", event.Authorization)
	}
	req.ContentLength = int64(len(event.Body))
	capture := startWatchHistoryCapture(*site, req, requestLogCategoryPlaybackSync, d)
	if capture == nil {
		return nil
	}
	_, _ = io.Copy(io.Discard, capture)
	if history, ok := watchHistoryEventFromCapture(capture, d, *site, req, nil, event.StatusCode, time.UnixMilli(event.RecordedAtMS)); ok {
		// Agent events are acknowledged only after the derived watch history is
		// committed. The proxy hot path may remain asynchronous, but replayed
		// control-plane events must not be ACKed while they only exist in memory.
		if _, err := d.writeWatchHistoryBatch([]watchHistoryEvent{history}); err != nil {
			return fmt.Errorf("persist watch history: %w", err)
		}
	}
	return nil
}

// recordNodeMetadataEvent replays the Agent's bounded JSON metadata response
// through the same in-process observer used by the controller proxy. This
// keeps media enrichment identical for direct-node and controller traffic.
func (d *DB) recordNodeMetadataEvent(event NodeRequestEvent) error {
	if d == nil || event.ResponseBody == "" || event.StatusCode < 200 || event.StatusCode >= 300 {
		return nil
	}
	site, err := d.GetSite(event.SiteID)
	if err != nil || !site.WatchHistoryEnabled {
		return nil
	}
	parsed, err := url.Parse("https://" + event.Host + event.Path)
	if err != nil {
		return err
	}
	parsed.RawQuery = event.Query
	req, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		return err
	}
	response := &http.Response{
		StatusCode:    event.StatusCode,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(event.ResponseBody)),
		ContentLength: int64(len(event.ResponseBody)),
		Request:       req,
	}
	if event.ResponseContentType != "" {
		response.Header.Set("Content-Type", event.ResponseContentType)
	}
	if event.ResponseContentEncoding != "" {
		response.Header.Set("Content-Encoding", event.ResponseContentEncoding)
	}
	if err := captureWatchHistoryMetadata(response, d, event.SiteID); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, response.Body)
	return nil
}

func (d *DB) processNodeEvent(event NodeRequestEvent) error {
	if event.ResponseBody != "" {
		if err := d.recordNodeMetadataEvent(event); err != nil {
			return err
		}
	}
	if event.WatchHistory != nil {
		if event.WatchHistory.SiteID != event.SiteID || !validWatchHistoryEvent(*event.WatchHistory) {
			return errors.New("watch history event site/host mismatch")
		}
		site, err := d.GetSite(event.SiteID)
		if err != nil || !site.WatchHistoryEnabled {
			return nil
		}
		// Token ciphertext is encrypted with the Controller's private JWT key
		// for direct requests. An Agent cannot legitimately produce that value,
		// so never persist attacker-supplied ciphertext from the wire event.
		history := *event.WatchHistory
		history.TokenCiphertext = ""
		if _, err := d.writeWatchHistoryBatch([]watchHistoryEvent{history}); err != nil {
			return fmt.Errorf("persist watch history: %w", err)
		}
		return nil
	}
	return d.recordNodeWatchHistoryEvent(event)
}

func (node ControlNode) String() string {
	return fmt.Sprintf("%s(%d)", node.Name, node.ID)
}
