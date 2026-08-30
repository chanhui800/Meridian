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
	errManualNodeUnavailable = errors.New("manual node is unavailable")
)

type ControlNode struct {
	ID                  int64  `json:"id"`
	GUID                string `json:"guid"`
	Name                string `json:"name"`
	Address             string `json:"address"`
	Port                int    `json:"port"`
	Enabled             bool   `json:"enabled"`
	Priority            int    `json:"priority"`
	TrafficQuota        int64  `json:"traffic_quota"`
	TrafficManualOffset int64  `json:"traffic_manual_offset_bytes"`
	BillingMode         string `json:"billing_mode"`
	ResetDay            int    `json:"reset_day"`
	CycleStartedAtMS    int64  `json:"cycle_started_at_ms"`
	PeriodRXBytes       int64  `json:"period_rx_bytes"`
	PeriodTXBytes       int64  `json:"period_tx_bytes"`
	LifetimeRXBytes     int64  `json:"lifetime_rx_bytes"`
	LifetimeTXBytes     int64  `json:"lifetime_tx_bytes"`
	InterfaceName       string `json:"interface_name"`
	AgentVersion        string `json:"agent_version"`
	DesiredConfigHash   string `json:"desired_config_hash"`
	AppliedConfigHash   string `json:"applied_config_hash"`
	AgentListenerError  string `json:"agent_listener_error"`
	EnrolledAtMS        int64  `json:"enrolled_at_ms"`
	LastSeenAtMS        int64  `json:"last_seen_at_ms"`
	CreatedAtMS         int64  `json:"created_at_ms"`
	UpdatedAtMS         int64  `json:"updated_at_ms"`
	Status              string `json:"status"`
	TrafficUsed         int64  `json:"traffic_used"`
	TrafficRemaining    int64  `json:"traffic_remaining"`
	EnrollmentAvailable bool   `json:"enrollment_available"`
	Depleted            bool   `json:"depleted"`
	Active              bool   `json:"active"`

	lastRawRXBytes      int64
	lastRawTXBytes      int64
	lastBootID          string
	lastSequence        int64
	enrollmentTokenHash string
	agentTokenHash      string
	enrollmentExpiresMS int64
}

type NodeSchedulerSettings struct {
	Mode         string `json:"mode"`
	ManualNodeID int64  `json:"manual_node_id"`
	ActiveNodeID int64  `json:"active_node_id"`
	UpdatedAtMS  int64  `json:"updated_at_ms"`
}

type NodeControlSnapshot struct {
	Nodes     []ControlNode         `json:"nodes"`
	Scheduler NodeSchedulerSettings `json:"scheduler"`
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
	BootID            string                   `json:"boot_id"`
	Sequence          int64                    `json:"sequence"`
	InterfaceName     string                   `json:"interface_name"`
	RXBytes           int64                    `json:"rx_bytes"`
	TXBytes           int64                    `json:"tx_bytes"`
	AgentVersion      string                   `json:"agent_version"`
	AppliedConfigHash string                   `json:"applied_config_hash"`
	ListenerError     string                   `json:"listener_error"`
	SiteStats         []NodeSiteStat           `json:"site_stats,omitempty"`
	MediaCounts       []NodeMediaCount         `json:"media_counts,omitempty"`
	Retention         []NodeRetentionStatus    `json:"retention,omitempty"`
	Observations      []NodeDynamicObservation `json:"observations,omitempty"`
	Events            []NodeRequestEvent       `json:"events,omitempty"`
}

type NodeSiteStat struct {
	Host               string `json:"host"`
	RequestCount       int64  `json:"request_count"`
	LastRequestAtMS    int64  `json:"last_request_at_ms"`
	LastStatus         int    `json:"last_status"`
	BytesIn            int64  `json:"bytes_in"`
	BytesOut           int64  `json:"bytes_out"`
	CumulativeBytesIn  int64  `json:"cumulative_bytes_in"`
	CumulativeBytesOut int64  `json:"cumulative_bytes_out"`
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
}

const maxNodeRequestEventBodyBytes = 8 << 10
const maxNodeRequestEventResponseBodyBytes = 8 << 10
const maxNodeRequestEventsPerReport = 32
const maxNodeTelemetryItemsPerReport = 128

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
	nowMS := now.UnixMilli()
	cycleStart := nodeCycleStart(now, input.ResetDay, d.currentSystemSettings().ScheduleTimezone)
	result, err := d.db.Exec(`INSERT INTO control_nodes
		(guid,name,address,entry_mode,http_port,https_port,priority,traffic_quota,billing_mode,reset_day,cycle_started_at_ms,
		 enrollment_token_hash,enrollment_expires_at_ms,created_at_ms,updated_at_ms)
		VALUES(?,?,?,'direct',0,?,?,?,?,?,?,?,?,?,?)`, guid, input.Name, input.Address, input.Port, input.Priority, input.TrafficQuota,
		input.BillingMode, input.ResetDay, cycleStart, hashNodeToken(enrollmentToken), now.Add(nodeEnrollmentLifetime).UnixMilli(), nowMS, nowMS)
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
	last_raw_rx_bytes,last_raw_tx_bytes,last_boot_id,last_sequence,interface_name,agent_version,desired_config_hash,applied_config_hash,agent_listener_error,
	enrollment_token_hash,enrollment_expires_at_ms,agent_token_hash,enrolled_at_ms,last_seen_at_ms,created_at_ms,updated_at_ms
	FROM control_nodes`

func scanControlNode(scanner rowScanner, now time.Time) (ControlNode, error) {
	var node ControlNode
	var enabled int
	err := scanner.Scan(&node.ID, &node.GUID, &node.Name, &node.Address, &node.Port, &enabled, &node.Priority, &node.TrafficQuota,
		&node.BillingMode, &node.ResetDay, &node.CycleStartedAtMS, &node.PeriodRXBytes, &node.PeriodTXBytes,
		&node.LifetimeRXBytes, &node.LifetimeTXBytes, &node.TrafficManualOffset, &node.lastRawRXBytes, &node.lastRawTXBytes, &node.lastBootID,
		&node.lastSequence, &node.InterfaceName, &node.AgentVersion, &node.DesiredConfigHash, &node.AppliedConfigHash, &node.AgentListenerError, &node.enrollmentTokenHash, &node.enrollmentExpiresMS,
		&node.agentTokenHash, &node.EnrolledAtMS, &node.LastSeenAtMS, &node.CreatedAtMS, &node.UpdatedAtMS)
	if err != nil {
		return ControlNode{}, err
	}
	node.Enabled = enabled != 0
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

func nodeEligible(node ControlNode) bool {
	return node.Enabled && node.Status == "online" && !node.Depleted
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
	return NodeControlSnapshot{Nodes: nodes, Scheduler: scheduler}, nil
}

func nullableNodeID(id int64) interface{} {
	if id <= 0 {
		return nil
	}
	return id
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
		if _, err := d.controlNodeByID(manualNodeID, now); err != nil {
			return NodeControlSnapshot{}, err
		}
	} else {
		manualNodeID = 0
	}
	if _, err := d.db.Exec("UPDATE node_scheduler_settings SET mode=?,manual_node_id=?,updated_at_ms=? WHERE id=1", mode, nullableNodeID(manualNodeID), now.UnixMilli()); err != nil {
		return NodeControlSnapshot{}, err
	}
	return d.NodeControlSnapshot(now)
}

func (d *DB) UpdateControlNode(id int64, input NodeCreateInput, enabled bool, now time.Time) (ControlNode, error) {
	input, err := normalizeNodeInput(input)
	if err != nil {
		return ControlNode{}, err
	}
	current, err := d.controlNodeByID(id, now)
	if err != nil {
		return ControlNode{}, err
	}
	var result sql.Result
	if current.ResetDay != input.ResetDay {
		cycleStart := nodeCycleStart(now, input.ResetDay, d.currentSystemSettings().ScheduleTimezone)
		result, err = d.db.Exec(`UPDATE control_nodes SET name=?,address=?,entry_mode='direct',http_port=0,https_port=?,enabled=?,priority=?,traffic_quota=?,billing_mode=?,reset_day=?,traffic_manual_offset_bytes=?,
			cycle_started_at_ms=?,period_rx_bytes=0,period_tx_bytes=0,updated_at_ms=? WHERE id=?`, input.Name, input.Address,
			input.Port, sqliteBool(enabled), input.Priority, input.TrafficQuota, input.BillingMode, input.ResetDay, input.TrafficManualOffsetBytes, cycleStart, now.UnixMilli(), id)
	} else {
		result, err = d.db.Exec(`UPDATE control_nodes SET name=?,address=?,entry_mode='direct',http_port=0,https_port=?,enabled=?,priority=?,traffic_quota=?,billing_mode=?,traffic_manual_offset_bytes=?,updated_at_ms=? WHERE id=?`,
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
	return d.controlNodeByID(id, now)
}

func (d *DB) RefreshNodeEnrollment(id int64, now time.Time) (ControlNode, string, error) {
	token, err := newNodeToken()
	if err != nil {
		return ControlNode{}, "", err
	}
	result, err := d.db.Exec(`UPDATE control_nodes SET enrollment_token_hash=?,enrollment_expires_at_ms=?,
		agent_token_hash='',enrolled_at_ms=0,last_seen_at_ms=0,last_boot_id='',last_sequence=0,
		last_raw_rx_bytes=0,last_raw_tx_bytes=0,updated_at_ms=? WHERE id=?`, hashNodeToken(token),
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
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE node_scheduler_settings SET manual_node_id=NULL,active_node_id=NULL WHERE manual_node_id=? OR active_node_id=?", id, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE site_node_schedules SET enabled=0,fixed_node_id=NULL,desired_node_id=NULL,applied_node_id=NULL,
		dns_status='disabled',last_error='' WHERE fixed_node_id=? OR desired_node_id=? OR applied_node_id=?`, id, id, id); err != nil {
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
	return tx.Commit()
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
	agentToken, err := newNodeToken()
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
		agent_token_hash=?,enrolled_at_ms=?,updated_at_ms=? WHERE id=? AND enrollment_token_hash<>''`, hashNodeToken(agentToken), now.UnixMilli(), now.UnixMilli(), id)
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
	report.InterfaceName = strings.TrimSpace(report.InterfaceName)
	if report.BootID == "" || len(report.BootID) > 128 {
		return errors.New("invalid boot_id")
	}
	if report.Sequence <= 0 || report.RXBytes < 0 || report.TXBytes < 0 {
		return errors.New("invalid traffic counters")
	}
	if report.InterfaceName == "" || len(report.InterfaceName) > 64 || len(report.AgentVersion) > 128 || len(report.AppliedConfigHash) > 128 || len(report.ListenerError) > 1024 {
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
		if strings.TrimSpace(stat.Host) == "" || len(stat.Host) > 255 || stat.RequestCount < 0 || stat.LastRequestAtMS < 0 || stat.LastStatus < 0 || stat.LastStatus > 999 || stat.BytesIn < 0 || stat.BytesOut < 0 || stat.CumulativeBytesIn < 0 || stat.CumulativeBytesOut < 0 {
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
	if event.EventID <= 0 || event.SiteID <= 0 || len(event.Host) > 255 || len(event.Method) > 16 || len(event.Path) > 2048 || len(event.Query) > 4096 || event.StatusCode < 0 || event.StatusCode > 999 || len(event.ClientIP) > 64 || len(event.UserAgent) > 512 || len(event.Authorization) > 8192 || len(event.Body) > maxNodeRequestEventBodyBytes || len(event.ContentType) > 128 || len(event.ContentEncoding) > 64 || len(event.ResponseBody) > maxNodeRequestEventResponseBodyBytes || len(event.ResponseContentType) > 128 || len(event.ResponseContentEncoding) > 64 || len(event.ResourceCategory) > 32 || len(event.UpstreamUserAgent) > 512 || len(event.BackendAddress) > 2048 || len(event.InboundColo) > 64 || len(event.OutboundColo) > 64 || event.RecordedAtMS <= 0 {
		return errors.New("invalid request event")
	}
	return nil
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

func recordNodeSiteTrafficTx(tx *sql.Tx, nodeID int64, bootID string, stat NodeSiteStat, nowMS int64) error {
	var siteID int64
	host := strings.ToLower(strings.TrimSpace(stat.Host))
	if err := tx.QueryRow(`SELECT s.id FROM site_node_schedules n JOIN sites s ON s.id=n.site_id WHERE n.enabled=1 AND n.desired_node_id=? AND lower(s.public_host)=?`, nodeID, host).Scan(&siteID); err != nil {
		return nil
	}
	currentIn, currentOut := stat.CumulativeBytesIn, stat.CumulativeBytesOut
	if currentIn == 0 && stat.BytesIn > 0 {
		currentIn = stat.BytesIn
	}
	if currentOut == 0 && stat.BytesOut > 0 {
		currentOut = stat.BytesOut
	}
	var previousBoot string
	var previousIn, previousOut, previousRequests int64
	err := tx.QueryRow(`SELECT boot_id,last_bytes_in,last_bytes_out,last_request_count FROM node_site_counters WHERE node_id=? AND site_id=?`, nodeID, siteID).Scan(&previousBoot, &previousIn, &previousOut, &previousRequests)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.Exec(`INSERT INTO node_site_counters(node_id,site_id,boot_id,last_bytes_in,last_bytes_out,last_request_count,updated_at_ms) VALUES(?,?,?,?,?,?,?)`, nodeID, siteID, bootID, currentIn, currentOut, stat.RequestCount, nowMS)
		return err
	}
	if err != nil {
		return err
	}
	var deltaIn, deltaOut, deltaRequests int64
	if previousBoot == bootID && currentIn >= previousIn && currentOut >= previousOut && stat.RequestCount >= previousRequests {
		deltaIn, deltaOut, deltaRequests = currentIn-previousIn, currentOut-previousOut, stat.RequestCount-previousRequests
	}
	if deltaIn > 0 || deltaOut > 0 || deltaRequests > 0 {
		bucket := (nowMS / 60000) * 60000
		_, err = tx.Exec(`INSERT INTO node_site_traffic_logs(node_id,site_id,bytes_in,bytes_out,requests,recorded_at_ms) VALUES(?,?,?,?,?,?) ON CONFLICT(node_id,site_id,recorded_at_ms) DO UPDATE SET bytes_in=bytes_in+excluded.bytes_in,bytes_out=bytes_out+excluded.bytes_out,requests=requests+excluded.requests`, nodeID, siteID, deltaIn, deltaOut, deltaRequests, bucket)
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(`UPDATE node_site_counters SET boot_id=?,last_bytes_in=?,last_bytes_out=?,last_request_count=?,updated_at_ms=? WHERE node_id=? AND site_id=?`, bootID, currentIn, currentOut, stat.RequestCount, nowMS, nodeID, siteID)
	return err
}

func (d *DB) RecordNodeReport(agentToken string, report NodeReport, now time.Time) (ControlNode, error) {
	// A malformed/stale event must not make the whole heartbeat unprocessable.
	// Keep the node online and drop only the offending event; core counters and
	// valid events remain durable.
	validEvents := report.Events[:0]
	for _, event := range report.Events {
		if validateNodeRequestEvent(event) == nil {
			validEvents = append(validEvents, event)
		}
	}
	report.Events = validEvents
	if err := validateNodeReport(report); err != nil {
		return ControlNode{}, err
	}
	if err := d.resetDueNodeCycles(now); err != nil {
		return ControlNode{}, err
	}
	tx, err := d.db.Begin()
	if err != nil {
		return ControlNode{}, err
	}
	defer tx.Rollback()
	acceptedEvents := make([]NodeRequestEvent, 0, len(report.Events))
	var id, lastSequence, lastRX, lastTX int64
	var lastBootID string
	err = tx.QueryRow(`SELECT id,last_sequence,last_raw_rx_bytes,last_raw_tx_bytes,last_boot_id FROM control_nodes WHERE agent_token_hash=?`, hashNodeToken(agentToken)).Scan(
		&id, &lastSequence, &lastRX, &lastTX, &lastBootID)
	if errors.Is(err, sql.ErrNoRows) {
		return ControlNode{}, errInvalidAgentToken
	}
	if err != nil {
		return ControlNode{}, err
	}
	deltaRX, deltaTX := int64(0), int64(0)
	if report.BootID == lastBootID && report.Sequence > lastSequence && report.RXBytes >= lastRX && report.TXBytes >= lastTX {
		deltaRX = report.RXBytes - lastRX
		deltaTX = report.TXBytes - lastTX
	}
	if report.BootID == lastBootID && report.Sequence <= lastSequence {
		deltaRX, deltaTX = 0, 0
		report.RXBytes, report.TXBytes, report.Sequence = lastRX, lastTX, lastSequence
	}
	_, err = tx.Exec(`UPDATE control_nodes SET period_rx_bytes=period_rx_bytes+?,period_tx_bytes=period_tx_bytes+?,
		lifetime_rx_bytes=lifetime_rx_bytes+?,lifetime_tx_bytes=lifetime_tx_bytes+?,last_raw_rx_bytes=?,last_raw_tx_bytes=?,
		last_boot_id=?,last_sequence=?,interface_name=?,agent_version=?,applied_config_hash=?,agent_listener_error=?,last_seen_at_ms=?,updated_at_ms=? WHERE id=?`,
		deltaRX, deltaTX, deltaRX, deltaTX, report.RXBytes, report.TXBytes, report.BootID, report.Sequence,
		strings.TrimSpace(report.InterfaceName), strings.TrimSpace(report.AgentVersion), strings.TrimSpace(report.AppliedConfigHash), strings.TrimSpace(report.ListenerError), now.UnixMilli(), now.UnixMilli(), id)
	if err != nil {
		return ControlNode{}, err
	}
	for _, stat := range report.SiteStats {
		host := strings.ToLower(strings.TrimSpace(stat.Host))
		if host == "" || len(host) > 255 || stat.RequestCount < 0 || stat.LastRequestAtMS < 0 || stat.LastStatus < 0 || stat.LastStatus > 999 {
			continue
		}
		var scheduleID int64
		var previousBoot string
		var previousCount int64
		err := tx.QueryRow(`SELECT n.site_id,n.agent_boot_id,n.agent_request_count FROM site_node_schedules n JOIN sites s ON s.id=n.site_id WHERE n.enabled=1 AND n.desired_node_id=? AND lower(s.public_host)=?`, id, host).Scan(&scheduleID, &previousBoot, &previousCount)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return ControlNode{}, err
		}
		count := stat.RequestCount
		if previousBoot != report.BootID || count < previousCount {
			previousCount = 0
		}
		if count < previousCount {
			count = previousCount
		}
		_, err = tx.Exec(`UPDATE site_node_schedules SET agent_boot_id=?,agent_request_count=?,agent_last_request_at_ms=?,agent_last_status=?,updated_at_ms=? WHERE site_id=?`,
			report.BootID, count, stat.LastRequestAtMS, stat.LastStatus, now.UnixMilli(), scheduleID)
		if err != nil {
			return ControlNode{}, err
		}
		if err := recordNodeSiteTrafficTx(tx, id, report.BootID, stat, now.UnixMilli()); err != nil {
			return ControlNode{}, err
		}
	}
	for _, count := range report.MediaCounts {
		_, err := tx.Exec(`UPDATE sites SET media_movie_count=?,media_series_count=?,media_episode_count=?,media_count_updated_at_ms=? WHERE id=? AND media_count_updated_at_ms<?`, count.MovieCount, count.SeriesCount, count.EpisodeCount, count.ObservedAtMS, count.SiteID, count.ObservedAtMS)
		if err != nil {
			return ControlNode{}, err
		}
	}
	for _, status := range report.Retention {
		_, err := tx.Exec(`UPDATE sites SET account_retention_started_at_ms=?,account_retention_last_completed_at_ms=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND account_retention_days>0 AND account_retention_started_at_ms=?`, status.CompletedAtMS, status.CompletedAtMS, status.SiteID, status.ExpectedStartedAtMS)
		if err != nil {
			return ControlNode{}, err
		}
	}
	for _, observation := range report.Observations {
		_, err := tx.Exec(`INSERT INTO dynamic_observations(site_id,canonical_authority,source,decision,reason_code,first_seen_ms,last_seen_ms,count) VALUES(?,?,?,?,?,?,?,1) ON CONFLICT(site_id,canonical_authority,source,decision,reason_code) DO UPDATE SET last_seen_ms=excluded.last_seen_ms,count=count+1`, observation.SiteID, observation.CanonicalAuthority, observation.Source, observation.Decision, observation.ReasonCode, observation.ObservedAtMS, observation.ObservedAtMS)
		if err != nil {
			return ControlNode{}, err
		}
	}
	for _, event := range report.Events {
		result, err := tx.Exec(`INSERT OR IGNORE INTO node_request_events(node_id,agent_boot_id,event_id,received_at_ms) VALUES(?,?,?,?)`, id, report.BootID, event.EventID, now.UnixMilli())
		if err != nil {
			return ControlNode{}, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return ControlNode{}, err
		}
		if rows == 0 {
			continue
		}
		if !event.SkipRequestLog {
			if err := d.recordNodeRequestEventTx(tx, id, event); err != nil {
				return ControlNode{}, err
			}
		}
		acceptedEvents = append(acceptedEvents, event)
	}
	if err := tx.Commit(); err != nil {
		return ControlNode{}, err
	}
	// Metadata responses must be replayed first. A playback sync event often
	// arrives in the same heartbeat and relies on this cache for its title.
	for _, event := range acceptedEvents {
		if event.ResponseBody != "" {
			d.recordNodeMetadataEvent(event)
		}
	}
	for _, event := range acceptedEvents {
		d.recordNodeWatchHistoryEvent(event)
	}
	node, err := d.controlNodeByID(id, now)
	if err != nil {
		return ControlNode{}, err
	}
	_, _ = d.NodeControlSnapshot(now)
	return node, nil
}

func (d *DB) recordNodeWatchHistoryEvent(event NodeRequestEvent) {
	if d == nil || event.Body == "" || event.StatusCode < 200 || event.StatusCode >= 300 {
		return
	}
	site, err := d.GetSite(event.SiteID)
	if err != nil || !site.WatchHistoryEnabled {
		return
	}
	parsed, err := url.Parse("https://" + event.Host + event.Path)
	if err != nil {
		return
	}
	parsed.RawQuery = event.Query
	req, err := http.NewRequest(event.Method, parsed.String(), strings.NewReader(event.Body))
	if err != nil {
		return
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
		return
	}
	_, _ = io.Copy(io.Discard, capture)
	if history, ok := watchHistoryEventFromCapture(capture, d, *site, req, nil, event.StatusCode, time.UnixMilli(event.RecordedAtMS)); ok {
		_ = d.EnqueueWatchHistory(history)
	}
}

// recordNodeMetadataEvent replays the Agent's bounded JSON metadata response
// through the same in-process observer used by the controller proxy. This
// keeps media enrichment identical for direct-node and controller traffic.
func (d *DB) recordNodeMetadataEvent(event NodeRequestEvent) {
	if d == nil || event.ResponseBody == "" || event.StatusCode < 200 || event.StatusCode >= 300 {
		return
	}
	site, err := d.GetSite(event.SiteID)
	if err != nil || !site.WatchHistoryEnabled {
		return
	}
	parsed, err := url.Parse("https://" + event.Host + event.Path)
	if err != nil {
		return
	}
	parsed.RawQuery = event.Query
	req, err := http.NewRequest(http.MethodGet, parsed.String(), nil)
	if err != nil {
		return
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
		return
	}
	_, _ = io.Copy(io.Discard, response.Body)
}

func (node ControlNode) String() string {
	return fmt.Sprintf("%s(%d)", node.Name, node.ID)
}
