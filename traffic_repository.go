package main

import (
	"database/sql"
	"errors"
	"log"
	"sort"
	"strings"
	"time"
)

type TrafficLog struct {
	ID         int64  `json:"id"`
	SiteID     int64  `json:"site_id"`
	BytesIn    int64  `json:"bytes_in"`
	BytesOut   int64  `json:"bytes_out"`
	Requests   int64  `json:"requests"`
	RecordedAt string `json:"recorded_at"`
	// RecordedAtMS is the local wall-clock bucket represented as an epoch in
	// the server's local timezone. traffic_logs intentionally stores wall-clock
	// text for backwards compatibility, so clients must not interpret the
	// driver's RFC3339/Z rendering as a UTC instant.
	RecordedAtMS int64 `json:"recorded_at_ms,omitempty"`
}

// SiteTraffic is the authoritative per-site traffic state: the persisted
// baseline plus in-memory pending bytes. TrafficUsed is always
// the configured billing projection of persisted and pending raw directions.
type SiteTraffic struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Running            bool   `json:"running"`
	TrafficQuota       int64  `json:"traffic_quota"`
	PersistedTraffic   int64  `json:"persisted_traffic"`
	BytesIn            int64  `json:"bytes_in"`
	BytesOut           int64  `json:"bytes_out"`
	CumulativeBytesIn  int64  `json:"cumulative_bytes_in"`
	CumulativeBytesOut int64  `json:"cumulative_bytes_out"`
	TrafficUsed        int64  `json:"traffic_used"`
	MonthlyTraffic     int64  `json:"monthly_traffic"`
	Requests           int64  `json:"requests"`
	SampledAtMS        int64  `json:"sampled_at_ms,omitempty"`
	CacheSizeBytes     int64  `json:"cache_size_bytes,omitempty"`
	AgentRuntime       bool   `json:"-"`
}

// TrafficSnapshot is the single authoritative global traffic payload shared by
// /api/dashboard, /api/traffic/overview and SSE events.
type TrafficSnapshot struct {
	TotalSites      int                  `json:"total_sites"`
	OnlineSites     int                  `json:"online_sites"`
	RunningSites    int                  `json:"running_sites"`
	TotalTraffic    int64                `json:"total_traffic"`
	MonthlyTraffic  int64                `json:"monthly_traffic"`
	BillingMode     string               `json:"billing_mode"`
	TrafficResetDay int                  `json:"traffic_reset_day"`
	TotalRequests   int64                `json:"total_requests"`
	UptimeSeconds   int64                `json:"uptime_seconds"`
	PanelDomain     string               `json:"panel_domain,omitempty"`
	PanelAccessURL  string               `json:"panel_access_url,omitempty"`
	GeneratedAtMS   int64                `json:"generated_at_ms"`
	LiveSites       []SiteTraffic        `json:"live_sites"`
	RealtimeTrend   *dashboardTrendPoint `json:"realtime_trend,omitempty"`
}

// NodeSiteLiveTraffic is the last persisted runtime sample from an applied
// Agent. The dashboard overlays it after taking the local proxy snapshot.
type NodeSiteLiveTraffic struct {
	SiteID             int64
	NodeID             int64
	Running            bool
	CumulativeBytesIn  int64
	CumulativeBytesOut int64
	Requests           int64
	SampledAtMS        int64
	CacheSizeBytes     int64
}

// TrafficHistory is the single-site envelope returned by
// /api/traffic/{id}/snapshot: an atomically captured live snapshot plus the
// log window with pending bytes and requests merged into the current-minute bucket.
type TrafficHistory struct {
	Snapshot    SiteTraffic  `json:"snapshot"`
	Logs        []TrafficLog `json:"logs"`
	BillingMode string       `json:"billing_mode"`
}

func (d *DB) AddTraffic(siteID, bytesIn, bytesOut int64) {
	if err := d.addTraffic(siteID, bytesIn, bytesOut); err != nil {
		log.Printf("[traffic] failed to persist usage for site %d: %v", siteID, err)
	}
}

func (d *DB) addTraffic(siteID, bytesIn, bytesOut int64) error {
	return d.addTrafficWithRequests(siteID, bytesIn, bytesOut, 0)
}

func trafficMinuteBucket(now time.Time) string {
	nowLocal := now.In(time.Local)
	return time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), nowLocal.Hour(), nowLocal.Minute(), 0, 0, time.Local).Format("2006-01-02 15:04:05")
}

// trafficWallClockMillis converts the wall-clock bucket text stored in
// traffic_logs into an epoch using the server's local timezone. The SQLite
// driver may render the same text as RFC3339 with a trailing Z, but that Z is
// not the original instant; it is only the driver's representation of a
// timezone-less DATETIME value.
func trafficWallClockMillis(recordedAt string) int64 {
	t, err := time.Parse(time.RFC3339Nano, recordedAt)
	if err != nil {
		t, err = time.ParseInLocation("2006-01-02 15:04:05", recordedAt, time.Local)
	}
	if err != nil {
		return 0
	}
	wall := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
	return wall.UnixMilli()
}

func (d *DB) addTrafficWithRequests(siteID, bytesIn, bytesOut, requests int64) error {
	return d.addTrafficWithRequestsAt(siteID, bytesIn, bytesOut, requests, time.Now())
}

func (d *DB) addTrafficWithRequestsAt(siteID, bytesIn, bytesOut, requests int64, now time.Time) error {
	minute := trafficMinuteBucket(now)
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO traffic_logs (site_id, bytes_in, bytes_out, requests, recorded_at, recorded_at_ms)
		 VALUES (?,?,?,?,?,?)
		 ON CONFLICT(site_id, recorded_at) DO UPDATE SET
			bytes_in = traffic_logs.bytes_in + excluded.bytes_in,
			bytes_out = traffic_logs.bytes_out + excluded.bytes_out,
			requests = traffic_logs.requests + excluded.requests`,
		siteID, bytesIn, bytesOut, requests, minute, trafficWallClockMillis(minute),
	); err != nil {
		return err
	}

	// Request-only checkpoints must not issue a no-op traffic_used update. In
	// addition to avoiding unnecessary writes, lifecycle shutdown relies on the
	// pre-close request-count checkpoint remaining independent from a later
	// byte-persistence failure after ingress has irreversibly closed.
	if bytesIn != 0 || bytesOut != 0 {
		if _, err := tx.Exec(
			"UPDATE sites SET traffic_used=traffic_used+?+?, traffic_used_in=traffic_used_in+?, traffic_used_out=traffic_used_out+?, updated_at=CURRENT_TIMESTAMP WHERE id=?",
			bytesIn, bytesOut, bytesIn, bytesOut, siteID,
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (d *DB) GetTrafficLogs(siteID int64, hours int) ([]TrafficLog, error) {
	rows, err := d.db.Query(
		"SELECT id, site_id, bytes_in, bytes_out, requests, recorded_at, recorded_at_ms FROM traffic_logs WHERE site_id=? AND (recorded_at_ms>=? OR (recorded_at_ms=0 AND recorded_at>=?)) ORDER BY recorded_at_ms, recorded_at",
		siteID, time.Now().Add(-time.Duration(hours)*time.Hour).UnixMilli(), time.Now().Add(-time.Duration(hours)*time.Hour).Format("2006-01-02 15:04:05"),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var logs []TrafficLog
	for rows.Next() {
		var l TrafficLog
		if err := rows.Scan(&l.ID, &l.SiteID, &l.BytesIn, &l.BytesOut, &l.Requests, &l.RecordedAt, &l.RecordedAtMS); err != nil {
			return nil, err
		}
		if l.RecordedAtMS == 0 {
			l.RecordedAtMS = trafficWallClockMillis(l.RecordedAt)
		}
		logs = append(logs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if logs == nil {
		logs = []TrafficLog{}
	}
	nodeRows, err := d.db.Query("SELECT id,site_id,bytes_in,bytes_out,requests,recorded_at_ms FROM node_site_traffic_logs WHERE site_id=? AND recorded_at_ms>=? ORDER BY recorded_at_ms", siteID, time.Now().Add(-time.Duration(hours)*time.Hour).UnixMilli())
	if err != nil {
		return nil, err
	}
	defer nodeRows.Close()
	for nodeRows.Next() {
		var l TrafficLog
		if err := nodeRows.Scan(&l.ID, &l.SiteID, &l.BytesIn, &l.BytesOut, &l.Requests, &l.RecordedAtMS); err != nil {
			return nil, err
		}
		l.RecordedAt = time.UnixMilli(l.RecordedAtMS).In(time.Local).Format("2006-01-02 15:04:05")
		logs = append(logs, l)
	}
	if err := nodeRows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(logs, func(i, j int) bool { return logs[i].RecordedAtMS < logs[j].RecordedAtMS })
	return logs, nil
}

// SumTrafficSince returns persisted bidirectional traffic from the supplied
// local wall-clock boundary. The traffic log table intentionally stores local
// wall-clock buckets for backwards compatibility, so the comparison uses the
// same text format as trafficMinuteBucket instead of UTC serialization.
func (d *DB) SumTrafficSince(start time.Time, billingMode string) (int64, error) {
	var bytesIn, bytesOut int64
	startMS := start.UnixMilli()
	startText := start.In(time.Local).Format("2006-01-02 15:04:05")
	err := d.db.QueryRow(`
		WITH source AS (
			SELECT bytes_in, bytes_out
			FROM traffic_logs
			WHERE recorded_at_ms >= ? OR (recorded_at_ms = 0 AND recorded_at >= ?)
			UNION ALL
			SELECT bytes_in, bytes_out
			FROM node_site_traffic_logs
			WHERE recorded_at_ms >= ?
		)
		SELECT COALESCE(SUM(bytes_in), 0), COALESCE(SUM(bytes_out), 0)
		FROM source
	`, startMS, startText, startMS).Scan(&bytesIn, &bytesOut)
	return trafficBillableBytes(billingMode, bytesIn, bytesOut), err
}

// SumTrafficSinceBySite returns the same billing-mode projection as
// SumTrafficSince, grouped by site so dashboards can show each node's current
// month usage without issuing one query per site.
func (d *DB) SumTrafficSinceBySite(start time.Time, billingMode string) (map[int64]int64, error) {
	startMS := start.UnixMilli()
	startText := start.In(time.Local).Format("2006-01-02 15:04:05")
	rows, err := d.db.Query(`
		WITH source AS (
			SELECT site_id, bytes_in, bytes_out
			FROM traffic_logs
			WHERE recorded_at_ms >= ? OR (recorded_at_ms = 0 AND recorded_at >= ?)
			UNION ALL
			SELECT site_id, bytes_in, bytes_out
			FROM node_site_traffic_logs
			WHERE recorded_at_ms >= ?
		)
		SELECT site_id, COALESCE(SUM(bytes_in), 0), COALESCE(SUM(bytes_out), 0)
		FROM source
		GROUP BY site_id
	`, startMS, startText, startMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64]int64)
	for rows.Next() {
		var siteID, bytesIn, bytesOut int64
		if err := rows.Scan(&siteID, &bytesIn, &bytesOut); err != nil {
			return nil, err
		}
		result[siteID] = trafficBillableBytes(billingMode, bytesIn, bytesOut)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// SumTrafficSinceForSite returns the billable bytes for one site and cycle.
// Directional columns remain raw so changing the billing mode never destroys
// the information needed to recalculate the current cycle.
func (d *DB) SumTrafficSinceForSite(siteID int64, start time.Time, billingMode string) (int64, error) {
	return sumTrafficSinceForSiteQuery(d.db, siteID, start, billingMode)
}

// sumTrafficSinceForSiteTx is the transaction-scoped equivalent used while
// building an Agent route snapshot. Keeping the usage read in that snapshot
// transaction makes the quota baseline consistent with the site assignment
// and avoids a second connection racing a telemetry flush.
func sumTrafficSinceForSiteTx(tx *sql.Tx, siteID int64, start time.Time, billingMode string) (int64, error) {
	if tx == nil {
		return 0, errors.New("nil traffic transaction")
	}
	return sumTrafficSinceForSiteQuery(tx, siteID, start, billingMode)
}

type trafficQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func sumTrafficSinceForSiteQuery(queryer trafficQueryer, siteID int64, start time.Time, billingMode string) (int64, error) {
	var bytesIn, bytesOut int64
	startMS := start.UnixMilli()
	startText := start.In(time.Local).Format("2006-01-02 15:04:05")
	err := queryer.QueryRow(`
		WITH source AS (
			SELECT bytes_in, bytes_out
			FROM traffic_logs
			WHERE site_id=? AND (recorded_at_ms >= ? OR (recorded_at_ms = 0 AND recorded_at >= ?))
			UNION ALL
			SELECT bytes_in, bytes_out
			FROM node_site_traffic_logs
			WHERE site_id=? AND recorded_at_ms >= ?
		)
		SELECT COALESCE(SUM(bytes_in), 0), COALESCE(SUM(bytes_out), 0)
		FROM source
		`, siteID, startMS, startText, siteID, startMS).Scan(&bytesIn, &bytesOut)
	return trafficBillableBytes(billingMode, bytesIn, bytesOut), err
}

// NodeSiteLiveTrafficSnapshot returns the last Agent sample for each site
// whose applied node is currently serving it. During a scheduler transition
// the applied node remains authoritative until DNS and config converge.
func (d *DB) NodeSiteLiveTrafficSnapshot(now time.Time) (map[int64]NodeSiteLiveTraffic, error) {
	rows, err := d.db.Query(`
		SELECT
			sch.site_id,
			sch.applied_node_id,
			COALESCE(c.last_bytes_in, 0),
			COALESCE(c.last_bytes_out, 0),
			COALESCE(c.last_request_count, sch.agent_request_count, 0),
			COALESCE(c.cache_size_bytes, 0),
			COALESCE(c.updated_at_ms, 0),
			COALESCE(n.last_seen_at_ms, 0),
			COALESCE(n.enabled, 0),
			COALESCE(n.agent_listener_error, ''),
			COALESCE(sch.dns_status, '')
		FROM site_node_schedules sch
		JOIN sites s ON s.id=sch.site_id
		JOIN control_nodes n ON n.id=sch.applied_node_id
		LEFT JOIN node_site_counters c
			ON c.site_id=sch.site_id AND c.node_id=sch.applied_node_id
		WHERE sch.enabled=1 AND s.enabled=1 AND sch.applied_node_id IS NOT NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64]NodeSiteLiveTraffic)
	liveOverlay := d.nodeLiveTrafficOverlay(now)
	for rows.Next() {
		var value NodeSiteLiveTraffic
		var nodeLastSeenMS int64
		var nodeEnabled int
		var listenerError, dnsStatus string
		if err := rows.Scan(&value.SiteID, &value.NodeID, &value.CumulativeBytesIn, &value.CumulativeBytesOut, &value.Requests, &value.CacheSizeBytes, &value.SampledAtMS, &nodeLastSeenMS, &nodeEnabled, &listenerError, &dnsStatus); err != nil {
			return nil, err
		}
		liveKey := nodeLiveTrafficKey{nodeID: value.NodeID, siteID: value.SiteID}
		if live, ok := liveOverlay[liveKey]; ok && live.SampledAtMS >= value.SampledAtMS {
			value.CumulativeBytesIn = live.CumulativeBytesIn
			value.CumulativeBytesOut = live.CumulativeBytesOut
			value.Requests = live.Requests
			value.SampledAtMS = live.SampledAtMS
		}
		nodeFresh := nodeLastSeenMS > 0 && now.Sub(time.UnixMilli(nodeLastSeenMS)) <= nodeOnlineWindow
		siteFresh := value.SampledAtMS > 0 && now.Sub(time.UnixMilli(value.SampledAtMS)) <= nodeOnlineWindow
		value.Running = nodeEnabled != 0 && nodeFresh && siteFresh && strings.TrimSpace(listenerError) == "" && strings.EqualFold(strings.TrimSpace(dnsStatus), "active")
		result[value.SiteID] = value
	}
	return result, rows.Err()
}

// GetTrafficTrendLogs returns traffic rows in the requested wall-clock window.
// Rows are bounded to the selected range and grouped into the dashboard bucket
// by GetTrafficTrendLogsGrouped without changing the persisted schema.
func (d *DB) GetTrafficTrendLogs(siteID *int64, start, end time.Time) ([]TrafficLog, error) {
	return d.GetTrafficTrendLogsGrouped(siteID, start, end, time.Minute)
}

type trafficTrendRowsQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func queryTrafficTrendLogsGrouped(queryer trafficTrendRowsQueryer, siteID *int64, start, end time.Time, bucket time.Duration) ([]TrafficLog, error) {
	bucketMS := bucket.Milliseconds()
	if bucketMS < 1 {
		bucketMS = time.Minute.Milliseconds()
	}
	startMS, endMS := start.UnixMilli(), end.UnixMilli()
	query := `WITH source AS (
		SELECT site_id, bytes_in, bytes_out, requests, recorded_at_ms FROM traffic_logs WHERE recorded_at_ms>=? AND recorded_at_ms<?
		UNION ALL
		SELECT site_id, bytes_in, bytes_out, requests, recorded_at_ms FROM node_site_traffic_logs WHERE recorded_at_ms>=? AND recorded_at_ms<?
	), bucketed AS (
		SELECT site_id, bytes_in, bytes_out, requests,
			(? + ((recorded_at_ms - ?) / ?) * ?) AS bucket_ms
		FROM source
	)
	SELECT site_id, COALESCE(SUM(bytes_in),0), COALESCE(SUM(bytes_out),0), COALESCE(SUM(requests),0), bucket_ms
	FROM bucketed`
	args := []any{startMS, endMS, startMS, endMS, startMS, startMS, bucketMS, bucketMS}
	if siteID != nil {
		query += " WHERE site_id=?"
		args = append(args, *siteID)
	}
	query += " GROUP BY site_id, bucket_ms ORDER BY bucket_ms, site_id"
	rows, err := queryer.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs := make([]TrafficLog, 0)
	for rows.Next() {
		var l TrafficLog
		if err := rows.Scan(&l.SiteID, &l.BytesIn, &l.BytesOut, &l.Requests, &l.RecordedAtMS); err != nil {
			return nil, err
		}
		l.RecordedAt = time.UnixMilli(l.RecordedAtMS).In(time.Local).Format("2006-01-02 15:04:05")
		logs = append(logs, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return logs, nil
}

// GetTrafficTrendLogsGrouped aggregates both controller and node traffic at
// the requested bucket in SQLite, keeping long dashboard ranges out of the Go
// heap. The returned rows are already ordered and can be fed directly to the
// dashboard bucket merger.
func (d *DB) GetTrafficTrendLogsGrouped(siteID *int64, start, end time.Time, bucket time.Duration) ([]TrafficLog, error) {
	return queryTrafficTrendLogsGrouped(d.db, siteID, start, end, bucket)
}

// GetTrafficTrendLogsGroupedSnapshot reads trend rows and the Agent counter
// baselines from one SQLite read transaction. The baseline timestamp is the
// exact moment the last full report was committed, rather than the HTTP
// request wall clock, so the dashboard can merge the live tail without a gap.
func (d *DB) GetTrafficTrendLogsGroupedSnapshot(siteID *int64, start, end time.Time, bucket time.Duration) ([]TrafficLog, map[int64]dashboardTrendBaseline, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	logs, err := queryTrafficTrendLogsGrouped(tx, siteID, start, end, bucket)
	if err != nil {
		return nil, nil, err
	}
	query := `SELECT c.site_id, c.last_bytes_in, c.last_bytes_out, c.last_request_count, c.updated_at_ms
		FROM node_site_counters c
		JOIN site_node_schedules sch ON sch.site_id=c.site_id AND sch.applied_node_id=c.node_id
		JOIN sites s ON s.id=c.site_id
		WHERE sch.enabled=1 AND s.enabled=1 AND c.updated_at_ms>0`
	args := make([]any, 0, 1)
	if siteID != nil {
		query += " AND c.site_id=?"
		args = append(args, *siteID)
	}
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, nil, err
	}
	baselines := make(map[int64]dashboardTrendBaseline)
	for rows.Next() {
		var siteIDValue int64
		var baseline dashboardTrendBaseline
		if err := rows.Scan(&siteIDValue, &baseline.BytesIn, &baseline.BytesOut, &baseline.Requests, &baseline.SampledAtMS); err != nil {
			rows.Close()
			return nil, nil, err
		}
		baselines[siteIDValue] = baseline
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return logs, baselines, nil
}
