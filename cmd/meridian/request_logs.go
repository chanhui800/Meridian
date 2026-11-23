package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	requestLogQueueCapacity               = 4096
	requestLogBatchSize                   = 128
	requestLogGlobalRowLimit              = 20000
	requestLogRetention                   = 30 * 24 * time.Hour
	requestLogMaxSiteNameBytes            = 100
	requestLogMaxClientIPBytes            = 64
	requestLogMaxUserAgentBytes           = 512
	requestLogMaxPathBytes                = 1024
	requestLogCategoryPlayback            = "playback"
	requestLogCategoryPlaybackSync        = "playback_sync"
	requestLogCategoryVideo               = "video" // legacy grouped value retained for old rows and filters
	requestLogCategoryStream              = "stream"
	requestLogCategoryManifest            = "manifest"
	requestLogCategorySegment             = "segment"
	requestLogCategoryImage               = "image"
	requestLogCategoryMetadata            = "metadata"
	requestLogCategorySubtitle            = "subtitle"
	requestLogCategoryAsset               = "asset"
	requestLogCategoryWebSocket           = "websocket"
	requestLogCategoryAPI                 = "api"
	requestLogCategoryAuth                = "auth"
	clientClosedRequestStatus             = 499
	requestLogLegacyPlaybackSyncPredicate = `(lower(request_logs.path)='/sessions/playing' OR lower(request_logs.path) LIKE '/sessions/playing/%' OR lower(request_logs.path)='/emby/sessions/playing' OR lower(request_logs.path) LIKE '/emby/sessions/playing/%')`
)

// requestLogEvent is the bounded hot-path payload accepted from a site proxy.
// Query strings, request/response headers other than the sanitized User-Agent,
// cookies, tokens and bodies are deliberately excluded.
type requestLogEvent struct {
	RequestDiagnostics
	SiteID            int64
	SiteName          string
	FinalNode         string
	ResourceCategory  string
	StatusCode        int
	ClientIP          string
	InboundColo       string
	OutboundColo      string
	UserAgent         string
	UpstreamUserAgent string
	BackendAddress    string
	Method            string
	Path              string
	timelineAtMS      *int64
}

type RequestLog struct {
	RequestDiagnostics
	ID                int64  `json:"id"`
	SiteID            int64  `json:"site_id"`
	SiteName          string `json:"site_name"`
	FinalNode         string `json:"final_node"`
	ResourceCategory  string `json:"resource_category"`
	StatusCode        int    `json:"status_code"`
	ClientIP          string `json:"client_ip"`
	ClientRegion      string `json:"client_region"`
	InboundColo       string `json:"inbound_colo"`
	OutboundColo      string `json:"outbound_colo"`
	UserAgent         string `json:"user_agent"`
	UpstreamUserAgent string `json:"upstream_user_agent"`
	BackendAddress    string `json:"backend_address"`
	Method            string `json:"method"`
	Path              string `json:"path"`
	RecordedAtMS      int64  `json:"recorded_at_ms"`
	CursorAtMS        int64  `json:"cursor_at_ms,omitempty"`
}

// AgentRequestLogPolicy is an additive wire snapshot of the Controller's
// write policy. It refreshes without changing the proxy route config hash.
type AgentRequestLogPolicy struct {
	Version                int    `json:"version"`
	LogEnabled             bool   `json:"log_enabled"`
	LogLevel               string `json:"log_level"`
	LogWriteImage          bool   `json:"log_write_image"`
	LogWritePlayback       bool   `json:"log_write_playback"`
	LogWriteMetadata       bool   `json:"log_write_metadata"`
	LogWriteVideo          bool   `json:"log_write_video"`
	LogWriteSubtitle       bool   `json:"log_write_subtitle"`
	LogWriteAsset          bool   `json:"log_write_asset"`
	LogWriteWebSocket      bool   `json:"log_write_websocket"`
	LogWriteAPI            bool   `json:"log_write_api"`
	LogWriteAuth           bool   `json:"log_write_auth"`
	LogWriteNode           bool   `json:"log_write_node"`
	LogWriteCategory       bool   `json:"log_write_category"`
	LogWriteStatus         bool   `json:"log_write_status"`
	LogWriteClientIP       bool   `json:"log_write_client_ip"`
	LogWriteColo           bool   `json:"log_write_colo"`
	LogWriteUA             bool   `json:"log_write_ua"`
	LogWriteUpstreamUA     bool   `json:"log_write_upstream_ua"`
	LogWriteBackendAddress bool   `json:"log_write_backend_address"`
	LogWriteTimeline       bool   `json:"log_write_timeline"`
}

func newAgentRequestLogPolicy(settings SystemSettings) *AgentRequestLogPolicy {
	return &AgentRequestLogPolicy{
		Version: 1, LogEnabled: settings.LogEnabled, LogLevel: settings.LogLevel,
		LogWriteImage: settings.LogWriteImage, LogWritePlayback: settings.LogWritePlayback,
		LogWriteMetadata: settings.LogWriteMetadata, LogWriteVideo: settings.LogWriteVideo,
		LogWriteSubtitle: settings.LogWriteSubtitle, LogWriteAsset: settings.LogWriteAsset,
		LogWriteWebSocket: settings.LogWriteWebSocket, LogWriteAPI: settings.LogWriteAPI, LogWriteAuth: settings.LogWriteAuth,
		LogWriteNode: settings.LogWriteNode, LogWriteCategory: settings.LogWriteCategory, LogWriteStatus: settings.LogWriteStatus,
		LogWriteClientIP: settings.LogWriteClientIP, LogWriteColo: settings.LogWriteColo, LogWriteUA: settings.LogWriteUA,
		LogWriteUpstreamUA: settings.LogWriteUpstreamUA, LogWriteBackendAddress: settings.LogWriteBackendAddress,
		LogWriteTimeline: settings.LogWriteTimeline,
	}
}

func applyAgentRequestLogPolicy(database *DB, policy *AgentRequestLogPolicy) {
	if database == nil {
		return
	}
	settings := database.currentSystemSettings()
	if policy == nil || policy.Version != 1 || policy.LogLevel != "info" && policy.LogLevel != "error" {
		// Legacy Controllers did not transmit a policy. Forward all categories
		// to their authoritative write gate instead of losing images/metadata
		// under the ephemeral database's unrelated default settings.
		settings.LogEnabled, settings.LogLevel = true, "info"
		settings.LogWriteImage, settings.LogWriteMetadata = true, true
		settings.LogWritePlayback, settings.LogWriteVideo, settings.LogWriteSubtitle, settings.LogWriteAsset = true, true, true, true
		settings.LogWriteWebSocket, settings.LogWriteAPI, settings.LogWriteAuth = true, true, true
		settings.LogWriteNode, settings.LogWriteCategory, settings.LogWriteStatus = true, true, true
		settings.LogWriteClientIP, settings.LogWriteColo, settings.LogWriteUA, settings.LogWriteUpstreamUA = true, true, true, true
		settings.LogWriteBackendAddress, settings.LogWriteTimeline = true, true
	} else {
		settings.LogEnabled, settings.LogLevel = policy.LogEnabled, policy.LogLevel
		settings.LogWriteImage, settings.LogWritePlayback, settings.LogWriteMetadata = policy.LogWriteImage, policy.LogWritePlayback, policy.LogWriteMetadata
		settings.LogWriteVideo, settings.LogWriteSubtitle, settings.LogWriteAsset = policy.LogWriteVideo, policy.LogWriteSubtitle, policy.LogWriteAsset
		settings.LogWriteWebSocket, settings.LogWriteAPI, settings.LogWriteAuth = policy.LogWriteWebSocket, policy.LogWriteAPI, policy.LogWriteAuth
		settings.LogWriteNode, settings.LogWriteCategory, settings.LogWriteStatus = policy.LogWriteNode, policy.LogWriteCategory, policy.LogWriteStatus
		settings.LogWriteClientIP, settings.LogWriteColo, settings.LogWriteUA = policy.LogWriteClientIP, policy.LogWriteColo, policy.LogWriteUA
		settings.LogWriteUpstreamUA, settings.LogWriteBackendAddress, settings.LogWriteTimeline = policy.LogWriteUpstreamUA, policy.LogWriteBackendAddress, policy.LogWriteTimeline
	}
	database.systemSettings.Store(&settings)
}

type RequestLogFilter struct {
	FromMS      int64
	ToMS        int64
	AfterMS     int64
	AfterID     int64
	BeforeMS    int64
	BeforeID    int64
	Category    string
	StatusGroup string
	Query       string
	Limit       int
}

type RequestLogsResponse struct {
	Logs        []RequestLog `json:"logs"`
	DroppedLogs uint64       `json:"dropped_logs"`
	NextCursor  string       `json:"next_cursor,omitempty"`
	HasMore     bool         `json:"has_more,omitempty"`
}

func encodeRequestLogCursor(recordedAtMS, id int64) string {
	if recordedAtMS <= 0 || id <= 0 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d:%d", recordedAtMS, id)))
}

func decodeRequestLogCursor(value string) (int64, int64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, 0, nil
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) > 64 {
		return 0, 0, fmt.Errorf("invalid cursor")
	}
	parts := strings.Split(string(decoded), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid cursor")
	}
	recordedAtMS, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || recordedAtMS <= 0 {
		return 0, 0, fmt.Errorf("invalid cursor")
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return 0, 0, fmt.Errorf("invalid cursor")
	}
	return recordedAtMS, id, nil
}

type queuedRequestLog struct {
	event        requestLogEvent
	recordedAtMS int64
	timelineAtMS int64
}

func validRequestLogCategory(category string) bool {
	switch category {
	case requestLogCategoryPlayback, requestLogCategoryPlaybackSync, requestLogCategoryVideo, requestLogCategoryStream,
		requestLogCategoryManifest, requestLogCategorySegment, requestLogCategoryImage,
		requestLogCategoryMetadata, requestLogCategorySubtitle, requestLogCategoryAsset,
		requestLogCategoryWebSocket, requestLogCategoryAPI, requestLogCategoryAuth:
		return true
	default:
		return false
	}
}

// filterAndSanitizeRequestLogEvent is the common write policy for Controller
// proxies, Agent sinks and report transactions. Suppression affects only the
// optional log row, never the report ledger, traffic or derived watch effects.
func filterAndSanitizeRequestLogEvent(settings SystemSettings, event requestLogEvent) (requestLogEvent, bool, error) {
	if !settings.LogEnabled || (settings.LogLevel == "error" && event.StatusCode > 0 && event.StatusCode < 400 && !requestDiagnosticsFailed(event.RequestDiagnostics)) ||
		(!settings.LogWriteImage && event.ResourceCategory == requestLogCategoryImage) ||
		(!settings.LogWritePlayback && (event.ResourceCategory == requestLogCategoryPlayback || event.ResourceCategory == requestLogCategoryPlaybackSync)) ||
		(!settings.LogWriteMetadata && event.ResourceCategory == requestLogCategoryMetadata) ||
		(!settings.LogWriteVideo && (event.ResourceCategory == requestLogCategoryVideo || event.ResourceCategory == requestLogCategoryStream || event.ResourceCategory == requestLogCategoryManifest || event.ResourceCategory == requestLogCategorySegment)) ||
		(!settings.LogWriteSubtitle && event.ResourceCategory == requestLogCategorySubtitle) ||
		(!settings.LogWriteAsset && event.ResourceCategory == requestLogCategoryAsset) ||
		(!settings.LogWriteWebSocket && event.ResourceCategory == requestLogCategoryWebSocket) ||
		(!settings.LogWriteAPI && event.ResourceCategory == requestLogCategoryAPI) ||
		(!settings.LogWriteAuth && event.ResourceCategory == requestLogCategoryAuth) {
		return requestLogEvent{}, false, nil
	}
	if !settings.LogWriteClientIP {
		event.ClientIP = ""
	}
	if !settings.LogWriteUA {
		event.UserAgent = ""
	}
	if !settings.LogWriteUpstreamUA {
		event.UpstreamUserAgent = ""
	}
	if !settings.LogWriteBackendAddress {
		event.BackendAddress = ""
	}
	if !settings.LogWriteNode {
		event.SiteName = ""
		event.FinalNode = ""
	}
	if !settings.LogWriteCategory {
		event.ResourceCategory = ""
	}
	if !settings.LogWriteStatus {
		event.StatusCode = 0
	}
	if !settings.LogWriteColo {
		event.InboundColo, event.OutboundColo = "", ""
	}
	event.RequestDiagnostics = filterRequestDiagnosticsFields(event.RequestDiagnostics, settings.LogWriteStatus, settings.LogWriteTimeline, settings.LogWriteCategory, settings.LogWriteBackendAddress)
	event.SiteName = requestLogSafeText(event.SiteName, requestLogMaxSiteNameBytes)
	event.FinalNode = requestLogSafeText(event.FinalNode, requestLogMaxSiteNameBytes)
	event.UserAgent = requestLogSafeText(event.UserAgent, requestLogMaxUserAgentBytes)
	event.UpstreamUserAgent = requestLogSafeText(event.UpstreamUserAgent, requestLogMaxUserAgentBytes)
	event.BackendAddress = requestLogSafeBackendAddress(event.BackendAddress)
	event.ClientIP = requestLogSafeText(event.ClientIP, requestLogMaxClientIPBytes)
	event.InboundColo = requestLogSafeColo(event.InboundColo)
	event.OutboundColo = requestLogSafeColo(event.OutboundColo)
	event.Method = requestLogSafeText(strings.ToUpper(event.Method), 16)
	event.Path, _, _ = strings.Cut(event.Path, "?")
	event.Path, _, _ = strings.Cut(event.Path, "#")
	event.Path = requestLogSafeText(event.Path, requestLogMaxPathBytes)
	if event.SiteID <= 0 || event.ResourceCategory != "" && !validRequestLogCategory(event.ResourceCategory) ||
		event.StatusCode != 0 && (event.StatusCode < 100 || event.StatusCode > 599) ||
		len(event.SiteName) > requestLogMaxSiteNameBytes || len(event.FinalNode) > requestLogMaxSiteNameBytes || len(event.ClientIP) > requestLogMaxClientIPBytes ||
		len(event.UserAgent) > requestLogMaxUserAgentBytes || len(event.UpstreamUserAgent) > requestLogMaxUserAgentBytes || len(event.BackendAddress) > maxDynamicTargetURLBytes || event.Method == "" || len(event.Method) > 16 || event.Path == "" || len(event.Path) > requestLogMaxPathBytes {
		return requestLogEvent{}, false, fmt.Errorf("invalid request log event")
	}
	return event, true, nil
}

func requestDiagnosticsFailed(value RequestDiagnostics) bool {
	if value.DiagnosticsVersion != requestDiagnosticsVersion {
		return false
	}
	return requestDiagnosticsEnum(value.TerminationReason, "client_canceled", "client_deadline", "upstream_error", "downstream_error", "proxy_aborted", "site_stopping") != ""
}

func filterRequestDiagnosticsFields(value RequestDiagnostics, status, timeline, category, backend bool) RequestDiagnostics {
	value = sanitizeRequestDiagnostics(value)
	if !status {
		value.ResponseStatusCode, value.TerminationReason = 0, ""
	}
	if !timeline {
		value.DurationMS, value.UpstreamFirstByteMS, value.BodyFirstByteMS, value.BodyBytesWritten = nil, nil, nil, nil
		value.DNSDurationMS, value.TCPDurationMS, value.TLSDurationMS = nil, nil, nil
	}
	if !category {
		value.RangeMode = 0
		value.CacheLookup, value.CacheWrite, value.CacheReason, value.ResponseType = "", "", "", ""
		value.CacheLookupReason, value.CacheWriteReason = "", ""
	}
	if !backend {
		value.ConnectionState, value.FinalConnectionState = "", ""
		value.RedirectCount, value.RetryCount = nil, nil
		value.UpstreamAttemptsTruncated = false
		value.DNSDurationMS, value.TCPDurationMS, value.TLSDurationMS = nil, nil, nil
	}
	if !status && !timeline && !category && !backend {
		value.UpstreamAttempts = nil
		return value
	}
	for index := range value.UpstreamAttempts {
		attempt := &value.UpstreamAttempts[index]
		if !status {
			attempt.ResponseStatusCode, attempt.Outcome = 0, ""
		}
		if !timeline {
			attempt.StartedMS, attempt.FirstByteMS, attempt.ResponseHeadersMS, attempt.DurationMS = nil, nil, nil, nil
			attempt.DNSDurationMS, attempt.TCPDurationMS, attempt.TLSDurationMS = nil, nil, nil
		}
		if !category {
			attempt.SourceRole = ""
		}
		if !backend {
			attempt.Authority, attempt.ConnectionState = "", ""
			attempt.DNSDurationMS, attempt.TCPDurationMS, attempt.TLSDurationMS = nil, nil, nil
		}
	}
	return value
}

func requestLogSafeBackendAddress(value string) string {
	value = requestLogSafeText(value, maxDynamicTargetURLBytes)
	if strings.HasPrefix(value, "node:") {
		if id, err := strconv.ParseInt(strings.TrimPrefix(value, "node:"), 10, 64); err == nil && id > 0 {
			return fmt.Sprintf("node:%d", id)
		}
		return ""
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return ""
	}
	if authority := dynamicCanonicalAuthority(parsed); authority != "" {
		return authority
	}
	return redactUpstreamURL(parsed)
}

func requestLogSafeColo(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	if len(value) < 3 || len(value) > 8 {
		return ""
	}
	for _, letter := range value {
		if letter < 'A' || letter > 'Z' {
			return ""
		}
	}
	return value
}

// EnqueueRequestLog never waits for SQLite. Request logging is optional
// operator telemetry and must not add latency or backpressure to media traffic.
func (d *DB) EnqueueRequestLog(event requestLogEvent) {
	if d == nil {
		return
	}
	settings := d.currentSystemSettings()
	event, accepted, err := filterAndSanitizeRequestLogEvent(settings, event)
	if err != nil {
		d.droppedRequestLogs.Add(1)
		return
	}
	if !accepted {
		return
	}
	nowMS := time.Now().UnixMilli()
	timelineAtMS := int64(0)
	if settings.LogWriteTimeline {
		timelineAtMS = nowMS
	}
	event.timelineAtMS = &timelineAtMS
	if d.edgeEphemeral {
		if d.edgeRequestLogSink != nil {
			d.edgeRequestLogSink(event)
		}
		return
	}
	command := dynamicObservationCommand{
		kind: dynamicObservationCommandRequestLogWrite,
		requestLog: queuedRequestLog{
			event:        event,
			recordedAtMS: nowMS,
			timelineAtMS: timelineAtMS,
		},
	}
	if !d.dynamicObservationGate.TryRLock() {
		d.droppedRequestLogs.Add(1)
		return
	}
	defer d.dynamicObservationGate.RUnlock()
	if d.dynamicObservationClosed.Load() || d.dynamicObservationQueue == nil {
		d.droppedRequestLogs.Add(1)
		return
	}
	select {
	case d.dynamicObservationQueue <- command:
	default:
		d.droppedRequestLogs.Add(1)
	}
}

func (d *DB) DroppedRequestLogs() uint64 {
	if d == nil {
		return 0
	}
	return d.droppedRequestLogs.Load()
}

func (d *DB) ClearRequestLogs() error {
	return d.sendDynamicObservationControl(dynamicObservationCommandRequestLogClear, 0)
}

func (d *DB) ListRequestLogs(filter RequestLogFilter) ([]RequestLog, error) {
	if filter.FromMS < 0 || filter.ToMS < 0 || filter.FromMS > 0 && filter.ToMS > 0 && filter.FromMS > filter.ToMS {
		return nil, fmt.Errorf("invalid request log time range")
	}
	if filter.Category != "" && filter.Category != "all" && !validRequestLogCategory(filter.Category) {
		return nil, fmt.Errorf("invalid request log category")
	}
	switch filter.StatusGroup {
	case "", "all", "4xx", "5xx":
	default:
		return nil, fmt.Errorf("invalid request log status filter")
	}
	filter.Query = strings.TrimSpace(filter.Query)
	if len(filter.Query) > 200 {
		return nil, fmt.Errorf("request log search must not exceed 200 bytes")
	}
	if filter.Limit == 0 {
		filter.Limit = 200
	}
	if filter.Limit < 1 || filter.Limit > 500 {
		return nil, fmt.Errorf("request log limit must be between 1 and 500")
	}
	if filter.AfterMS < 0 || filter.AfterID < 0 || filter.BeforeMS < 0 || filter.BeforeID < 0 {
		return nil, fmt.Errorf("invalid request log cursor")
	}
	if err := d.flushDynamicObservationsIfSmall(); err != nil {
		return nil, err
	}
	conditions := make([]string, 0, 6)
	args := make([]interface{}, 0, 12)
	if filter.FromMS > 0 {
		conditions = append(conditions, "recorded_at_ms>=?")
		args = append(args, filter.FromMS)
	}
	if filter.ToMS > 0 {
		conditions = append(conditions, "recorded_at_ms<=?")
		args = append(args, filter.ToMS)
	}
	if filter.AfterMS > 0 {
		conditions = append(conditions, "(request_logs.recorded_at_ms>? OR (request_logs.recorded_at_ms=? AND request_logs.id>?))")
		args = append(args, filter.AfterMS, filter.AfterMS, filter.AfterID)
	} else if filter.AfterID > 0 {
		conditions = append(conditions, "request_logs.id>?")
		args = append(args, filter.AfterID)
	}
	if filter.BeforeMS > 0 {
		conditions = append(conditions, "(request_logs.recorded_at_ms<? OR (request_logs.recorded_at_ms=? AND request_logs.id<?))")
		args = append(args, filter.BeforeMS, filter.BeforeMS, filter.BeforeID)
	}
	if filter.Category != "" && filter.Category != "all" {
		switch filter.Category {
		case requestLogCategoryVideo:
			conditions = append(conditions, "resource_category IN (?,?,?,?)")
			args = append(args, requestLogCategoryVideo, requestLogCategoryStream, requestLogCategoryManifest, requestLogCategorySegment)
		case requestLogCategoryPlaybackSync:
			conditions = append(conditions, "(resource_category=? OR (resource_category=? AND "+requestLogLegacyPlaybackSyncPredicate+"))")
			args = append(args, requestLogCategoryPlaybackSync, requestLogCategoryPlayback)
		case requestLogCategoryPlayback:
			conditions = append(conditions, "(resource_category=? AND NOT "+requestLogLegacyPlaybackSyncPredicate+")")
			args = append(args, requestLogCategoryPlayback)
		default:
			conditions = append(conditions, "resource_category=?")
			args = append(args, filter.Category)
		}
	}
	switch filter.StatusGroup {
	case "4xx":
		conditions = append(conditions, "status_code BETWEEN 400 AND 499")
	case "5xx":
		conditions = append(conditions, "status_code BETWEEN 500 AND 599")
	}
	if filter.Query != "" {
		conditions = append(conditions, `(instr(lower(request_logs.site_name), lower(?))>0 OR instr(lower(COALESCE(sites.name, '')), lower(?))>0 OR instr(lower(request_logs.client_ip), lower(?))>0 OR instr(lower(request_logs.user_agent), lower(?))>0 OR instr(lower(request_logs.upstream_user_agent), lower(?))>0 OR instr(lower(request_logs.backend_address), lower(?))>0 OR instr(lower(request_logs.path), lower(?))>0 OR CAST(request_logs.status_code AS TEXT)=?)`)
		for range 8 {
			args = append(args, filter.Query)
		}
	}
	query := `SELECT request_logs.id, request_logs.site_id, request_logs.site_name, request_logs.final_node, request_logs.resource_category, request_logs.status_code, request_logs.client_ip, request_logs.user_agent, request_logs.upstream_user_agent, request_logs.backend_address, request_logs.method, request_logs.path, request_logs.recorded_at_ms, request_logs.timeline_at_ms, request_logs.inbound_colo, request_logs.outbound_colo, request_logs.diagnostics_json FROM request_logs LEFT JOIN sites ON sites.id=request_logs.site_id`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ") // #nosec G202 -- conditions are fixed SQL fragments selected from validated filters; values remain parameters.
	}
	query += " ORDER BY request_logs.recorded_at_ms DESC, request_logs.id DESC LIMIT ?"
	args = append(args, filter.Limit)
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs := make([]RequestLog, 0)
	for rows.Next() {
		var entry RequestLog
		var timelineAtMS int64
		var diagnosticsJSON string
		if err := rows.Scan(&entry.ID, &entry.SiteID, &entry.SiteName, &entry.FinalNode, &entry.ResourceCategory, &entry.StatusCode, &entry.ClientIP, &entry.UserAgent, &entry.UpstreamUserAgent, &entry.BackendAddress, &entry.Method, &entry.Path, &entry.CursorAtMS, &timelineAtMS, &entry.InboundColo, &entry.OutboundColo, &diagnosticsJSON); err != nil {
			return nil, err
		}
		entry.RecordedAtMS = timelineAtMS
		entry.RequestDiagnostics = parseRequestDiagnosticsJSON(diagnosticsJSON)
		if entry.ResourceCategory == requestLogCategoryPlayback && isRequestLogPlaybackActivityPath(entry.Path) {
			entry.ResourceCategory = requestLogCategoryPlaybackSync
		}
		logs = append(logs, entry)
	}
	settings := d.currentSystemSettings()
	for i := range logs {
		logs[i].RequestDiagnostics = filterRequestDiagnosticsFields(logs[i].RequestDiagnostics, settings.LogDisplayStatus, settings.LogDisplayTimeline, settings.LogDisplayCategory, settings.LogDisplayBackendAddress)
		if !settings.LogDisplayStatus {
			logs[i].StatusCode = 0
		}
		if !settings.LogDisplayCategory {
			logs[i].ResourceCategory = ""
		}
		if !settings.LogDisplayTimeline {
			logs[i].RecordedAtMS = 0
		}
		if !settings.LogDisplayClientIP {
			logs[i].ClientIP = "hidden"
		}
		if !settings.LogDisplayUA {
			logs[i].UserAgent = "hidden"
		}
		if !settings.LogDisplayUpstreamUA {
			logs[i].UpstreamUserAgent = "hidden"
		}
		if !settings.LogDisplayBackendAddress {
			logs[i].BackendAddress = "hidden"
		}
		if !settings.LogDisplayNode {
			logs[i].FinalNode = "hidden"
		}
		if !settings.LogDisplayColo {
			logs[i].InboundColo, logs[i].OutboundColo = "hidden", "hidden"
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return logs, nil
}

func (d *DB) writeRequestLogBatch(batch []queuedRequestLog) (int, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	statement, err := tx.Prepare(`
		INSERT INTO request_logs
			(site_id, site_name, final_node, resource_category, status_code, client_ip, user_agent, upstream_user_agent, backend_address, inbound_colo, outbound_colo, method, path, recorded_at_ms, timeline_at_ms, diagnostics_json)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM sites WHERE id=?)`)
	if err != nil {
		return 0, err
	}
	defer statement.Close()
	skipped := 0
	for _, queued := range batch {
		event := queued.event
		result, err := statement.Exec(
			event.SiteID,
			event.SiteName,
			event.FinalNode,
			event.ResourceCategory,
			event.StatusCode,
			event.ClientIP,
			event.UserAgent,
			event.UpstreamUserAgent,
			event.BackendAddress,
			event.InboundColo,
			event.OutboundColo,
			event.Method,
			event.Path,
			queued.recordedAtMS,
			queued.timelineAtMS,
			requestDiagnosticsJSON(event.RequestDiagnostics),
			event.SiteID,
		)
		if err != nil {
			return 0, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if rows == 0 {
			skipped++
		}
	}
	if err := pruneRequestLogsTx(tx, time.Now(), time.Duration(d.currentSystemSettings().LogRetentionDays)*24*time.Hour); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return skipped, nil
}

// nodeRequestEventPendingLimit bounds unprocessed agent-event ledger rows;
// it matches the Agent's own event spool capacity.
const nodeRequestEventPendingLimit = 8192

func pruneRequestLogsTx(tx *sql.Tx, now time.Time, retention time.Duration) error {
	cutoffMS := now.Add(-retention).UnixMilli()
	if _, err := tx.Exec("DELETE FROM request_logs WHERE recorded_at_ms<?", cutoffMS); err != nil {
		return err
	}
	if retention < 24*time.Hour {
		retention = 24 * time.Hour
	}
	// The event ledger is only a replay/deduplication window. Retire
	// acknowledged identities after the request-log retention plus one extra
	// day so an offline Agent can replay safely without allowing the table to
	// grow without bound.
	if _, err := tx.Exec(`DELETE FROM node_request_events WHERE processed_at_ms>0 AND received_at_ms<?`, now.Add(-retention-24*time.Hour).UnixMilli()); err != nil {
		return err
	}
	// Pending rows were previously kept forever. An event whose derived effect
	// keeps failing would pin its row indefinitely, so cap the unprocessed
	// ledger (the Agent spool is the authoritative redelivery source) and
	// drop the oldest pending rows beyond the cap.
	if _, err := tx.Exec(`DELETE FROM node_request_events WHERE processed_at_ms=0 AND (node_id, agent_boot_id, event_id) IN (
		SELECT node_id, agent_boot_id, event_id FROM node_request_events WHERE processed_at_ms=0
		ORDER BY received_at_ms DESC, node_id, agent_boot_id, event_id LIMIT -1 OFFSET ?
	)`, nodeRequestEventPendingLimit); err != nil {
		return err
	}
	_, err := tx.Exec(`
		DELETE FROM request_logs
		WHERE id IN (
			SELECT id FROM request_logs
			ORDER BY recorded_at_ms DESC, id DESC
			LIMIT -1 OFFSET ?
		)`, requestLogGlobalRowLimit)
	return err
}

func (d *DB) pruneRequestLogs() error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := pruneRequestLogsTx(tx, time.Now(), time.Duration(d.currentSystemSettings().LogRetentionDays)*24*time.Hour); err != nil {
		return err
	}
	return tx.Commit()
}

func requestLogSafeText(value string, maxBytes int) string {
	value = strings.ToValidUTF8(value, "")
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	value = strings.TrimSpace(value)
	for len(value) > maxBytes {
		_, size := utf8.DecodeLastRuneInString(value)
		if size <= 0 {
			return ""
		}
		value = value[:len(value)-size]
	}
	return value
}

func classifyRequestLogResource(r *http.Request) string {
	if r == nil || r.URL == nil {
		return requestLogCategoryAPI
	}
	path := strings.ToLower(r.URL.Path)
	switch {
	case hasUpgradeIntent(r):
		return requestLogCategoryWebSocket
	case strings.Contains(path, "/authenticate"), strings.Contains(path, "/quickconnect"):
		return requestLogCategoryAuth
	case isPlaybackInfoRequest(path):
		return requestLogCategoryPlayback
	case isRequestLogPlaybackActivityPath(path):
		return requestLogCategoryPlaybackSync
	case requestLogPathHasSuffix(path, ".m3u8", ".m3u", ".mpd"):
		return requestLogCategoryManifest
	case requestLogPathHasSuffix(path, ".ts", ".m4s"):
		return requestLogCategorySegment
	case strings.Contains(path, "/subtitles/"), strings.HasSuffix(path, "/subtitles"), strings.Contains(path, "/captions/"), strings.Contains(path, "/emya/subtitle"), requestLogPathHasSuffix(path, ".srt", ".ass", ".ssa", ".vtt", ".sub", ".idx", ".sup"):
		return requestLogCategorySubtitle
	case strings.Contains(path, "/images/"), strings.HasSuffix(path, "/images"), strings.Contains(path, "/image/"), strings.Contains(path, "/icons/"), strings.Contains(path, "/branding/"), strings.Contains(path, "/covers/"), requestLogPathHasSuffix(path, ".jpg", ".jpeg", ".gif", ".png", ".svg", ".ico", ".webp", ".avif"):
		return requestLogCategoryImage
	case requestLogPathHasSuffix(path, ".js", ".css", ".woff", ".woff2", ".ttf", ".otf", ".map", ".webmanifest"):
		return requestLogCategoryAsset
	case isPlaybackRequest(path), isPlaybackRedirectEndpoint(path), isReservedDynamicRoute(path), requestLogPathHasSuffix(path, ".mp4", ".m4v", ".ogv", ".webm", ".mkv", ".mov", ".avi", ".wmv", ".flv", ".mp3", ".m4a", ".aac", ".flac", ".wav", ".ogg", ".opus"):
		return requestLogCategoryStream
	case isRequestLogMetadataPath(path):
		return requestLogCategoryMetadata
	default:
		return requestLogCategoryAPI
	}
}

func requestLogPathHasSuffix(path string, suffixes ...string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

func isRequestLogPlaybackActivityPath(path string) bool {
	path = strings.TrimSuffix(strings.ToLower(path), "/")
	_, legacyProgress := legacyPlaybackProgressItemID(path)
	return path == "/sessions/playing" || strings.HasPrefix(path, "/sessions/playing/") ||
		path == "/emby/sessions/playing" || strings.HasPrefix(path, "/emby/sessions/playing/") || legacyProgress
}

// legacyPlaybackProgressItemID recognizes the old Emby/Jellyfin progress route
// without returning or retaining its user-id segment.
func legacyPlaybackProgressItemID(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) > 0 && strings.EqualFold(parts[0], "emby") {
		parts = parts[1:]
	}
	if len(parts) != 5 || !strings.EqualFold(parts[0], "users") || strings.TrimSpace(parts[1]) == "" ||
		!strings.EqualFold(parts[2], "playingitems") || strings.TrimSpace(parts[3]) == "" || !strings.EqualFold(parts[4], "progress") {
		return "", false
	}
	return parts[3], true
}

func isRequestLogMetadataPath(path string) bool {
	parts := strings.Split(strings.Trim(strings.ToLower(path), "/"), "/")
	if len(parts) > 0 && parts[0] == "emby" {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return false
	}
	switch parts[0] {
	case "items", "shows", "movies", "users":
		return true
	default:
		return false
	}
}

func newRequestLogEvent(site Site, r *http.Request, trustedProxies []*net.IPNet, policies ...UAHeaderPolicy) requestLogEvent {
	path := "/"
	method := http.MethodGet
	userAgent := ""
	clientIP := "unknown"
	inboundColo := ""
	if r != nil {
		method = strings.ToUpper(strings.TrimSpace(r.Method))
		userAgent = r.Header.Get("User-Agent")
		inboundColo = requestLogColo(r.Header.Get("CF-Ray"))
		if r.URL != nil && r.URL.EscapedPath() != "" {
			path = r.URL.EscapedPath()
		}
		if ip := net.ParseIP(requestClientKey(r, trustedProxies)); ip != nil {
			clientIP = ip.String()
		}
	}
	if method == "" {
		method = http.MethodGet
	}
	upstreamUserAgent := userAgent
	if len(policies) > 0 && policies[0].Rewrite {
		upstreamUserAgent = policies[0].Profile.UserAgent
	}
	return requestLogEvent{
		SiteID:            site.ID,
		SiteName:          requestLogSafeText(site.Name, requestLogMaxSiteNameBytes),
		FinalNode:         requestLogSafeText("主控", requestLogMaxSiteNameBytes),
		ResourceCategory:  classifyRequestLogResource(r),
		ClientIP:          clientIP,
		InboundColo:       inboundColo,
		UserAgent:         requestLogSafeText(userAgent, requestLogMaxUserAgentBytes),
		UpstreamUserAgent: requestLogSafeText(upstreamUserAgent, requestLogMaxUserAgentBytes),
		Method:            requestLogSafeText(method, 16),
		Path:              requestLogSafeText(path, requestLogMaxPathBytes),
	}
}

func requestLogColo(cfRay string) string {
	cfRay = strings.TrimSpace(cfRay)
	index := strings.LastIndexByte(cfRay, '-')
	if index < 0 || index == len(cfRay)-1 {
		return ""
	}
	colo := strings.ToUpper(strings.TrimSpace(cfRay[index+1:]))
	if len(colo) < 3 || len(colo) > 8 {
		return ""
	}
	for _, char := range colo {
		if char < 'A' || char > 'Z' {
			return ""
		}
	}
	return colo
}

type requestLogResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	diagnostics *requestDiagnosticsTracker
}

type backendAddressContextKey struct{}

type backendAddressTracker struct {
	mu    sync.RWMutex
	value string
}

func (t *backendAddressTracker) SetURL(target *url.URL) {
	if t == nil || target == nil {
		return
	}
	value := dynamicCanonicalAuthority(target)
	if value == "" {
		value = redactUpstreamURL(target)
	}
	t.mu.Lock()
	t.value = value
	t.mu.Unlock()
}

func (t *backendAddressTracker) Get() string {
	if t == nil {
		return ""
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.value
}

func backendAddressTrackerFromContext(ctx context.Context) *backendAddressTracker {
	if ctx == nil {
		return nil
	}
	tracker, _ := ctx.Value(backendAddressContextKey{}).(*backendAddressTracker)
	return tracker
}

func (w *requestLogResponseWriter) WriteHeader(statusCode int) {
	if w.statusCode != 0 {
		return
	}
	if statusCode >= http.StatusOK || statusCode == http.StatusSwitchingProtocols {
		w.statusCode = statusCode
		w.diagnostics.SetResponseType(w.Header().Get("Content-Type"))
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *requestLogResponseWriter) Write(payload []byte) (int, error) {
	if w.statusCode == 0 {
		w.WriteHeader(http.StatusOK)
	}
	writtenAt := time.Now()
	n, err := w.ResponseWriter.Write(payload)
	w.diagnostics.observeBodyWrite(n, writtenAt, err != nil)
	return n, err
}

func (w *requestLogResponseWriter) StatusCode() int {
	if w.statusCode == 0 {
		return http.StatusOK
	}
	return w.statusCode
}

func (w *requestLogResponseWriter) ResponseStatusCode() int {
	return w.statusCode
}

func (w *requestLogResponseWriter) Flush() {
	_ = w.FlushError()
}

func (w *requestLogResponseWriter) FlushError() error {
	if w.statusCode == 0 {
		w.WriteHeader(http.StatusOK)
	}
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err != nil && err != http.ErrNotSupported {
		w.diagnostics.observeBodyWrite(0, time.Now(), true)
	}
	return err
}

func (w *requestLogResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("hijack not supported")
	}
	conn, buffer, err := hijacker.Hijack()
	if err == nil && w.statusCode == 0 {
		w.statusCode = http.StatusSwitchingProtocols
	}
	return conn, buffer, err
}

func (w *requestLogResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *requestLogResponseWriter) Push(target string, options *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, options)
	}
	return http.ErrNotSupported
}

// GET/DELETE /api/request-logs
func (a *App) handleRequestLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		query := r.URL.Query()
		filter := RequestLogFilter{
			Category:    strings.ToLower(strings.TrimSpace(query.Get("category"))),
			StatusGroup: strings.ToLower(strings.TrimSpace(query.Get("status"))),
			Query:       query.Get("q"),
		}
		for name, target := range map[string]*int64{"from_ms": &filter.FromMS, "to_ms": &filter.ToMS} {
			if raw := strings.TrimSpace(query.Get(name)); raw != "" {
				value, err := strconv.ParseInt(raw, 10, 64)
				if err != nil || value < 0 {
					a.jsonErr(w, http.StatusBadRequest, name+" must be a non-negative Unix millisecond timestamp")
					return
				}
				*target = value
			}
		}
		if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
			value, err := strconv.Atoi(raw)
			if err != nil {
				a.jsonErr(w, http.StatusBadRequest, "limit must be an integer")
				return
			}
			filter.Limit = value
		}
		if raw := strings.TrimSpace(query.Get("after_id")); raw != "" {
			value, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || value < 0 {
				a.jsonErr(w, http.StatusBadRequest, "after_id must be a non-negative integer")
				return
			}
			filter.AfterID = value
		}
		if raw := strings.TrimSpace(query.Get("after_cursor")); raw != "" {
			valueMS, valueID, err := decodeRequestLogCursor(raw)
			if err != nil {
				a.jsonErr(w, http.StatusBadRequest, "after_cursor is invalid")
				return
			}
			filter.AfterMS, filter.AfterID = valueMS, valueID
		}
		pageLimit := filter.Limit
		if pageLimit == 0 {
			pageLimit = 200
		}
		if raw := strings.TrimSpace(query.Get("cursor")); raw != "" {
			valueMS, valueID, err := decodeRequestLogCursor(raw)
			if err != nil {
				a.jsonErr(w, http.StatusBadRequest, "cursor is invalid")
				return
			}
			filter.BeforeMS, filter.BeforeID = valueMS, valueID
			if pageLimit < 500 {
				filter.Limit = pageLimit + 1
			}
		}
		logs, err := a.db.ListRequestLogs(filter)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		hasMore := len(logs) > pageLimit
		if hasMore {
			logs = logs[:pageLimit]
		}
		nextCursor := ""
		if hasMore && len(logs) > 0 {
			last := logs[len(logs)-1]
			nextCursor = encodeRequestLogCursor(last.CursorAtMS, last.ID)
		}
		a.clientIPRegions.enrich(logs)
		a.jsonOK(w, RequestLogsResponse{Logs: logs, DroppedLogs: a.db.DroppedRequestLogs(), NextCursor: nextCursor, HasMore: hasMore})
	case http.MethodDelete:
		if err := a.db.ClearRequestLogs(); err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "clear request logs failed")
			return
		}
		a.jsonOK(w, map[string]string{"status": "cleared"})
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
