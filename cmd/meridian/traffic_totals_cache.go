package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"time"
)

type trafficTotalsKey struct {
	Lifetime    bool
	StartMS     int64
	LegacyStart string
	BillingMode string
}

type trafficTotalsVersion struct {
	Epoch             string
	Generation        int64
	RequestGeneration int64
	DataVersion       int64
	SchemaVersion     int64
}

type trafficTotalsCacheEntry struct {
	key     trafficTotalsKey
	version trafficTotalsVersion
	values  map[int64]int64
}

type trafficRequestTotalsCacheEntry struct {
	startMS     int64
	legacyStart string
	version     trafficTotalsVersion
	value       int64
}

// Only accounting changes invalidate the connection-local generation. Keeping
// these triggers in TEMP avoids persistent cache state in backups/schema and
// also covers raw SQL, backfills, cascades and rolled-back transactions without
// requiring every writer to remember a Go invalidation hook.
const trafficTotalsVersionDDL = `
CREATE TEMP TABLE IF NOT EXISTS meridian_traffic_totals_version (
	id INTEGER PRIMARY KEY CHECK (id=1), epoch TEXT NOT NULL, generation INTEGER NOT NULL, request_generation INTEGER NOT NULL
);
CREATE TEMP TRIGGER IF NOT EXISTS meridian_traffic_totals_local_insert AFTER INSERT ON main.traffic_logs
BEGIN UPDATE meridian_traffic_totals_version SET generation=generation+1,request_generation=request_generation+CASE WHEN NEW.requests<>0 THEN 1 ELSE 0 END WHERE id=1; END;
CREATE TEMP TRIGGER IF NOT EXISTS meridian_traffic_totals_local_delete AFTER DELETE ON main.traffic_logs
BEGIN UPDATE meridian_traffic_totals_version SET generation=generation+1,request_generation=request_generation+CASE WHEN OLD.requests<>0 THEN 1 ELSE 0 END WHERE id=1; END;
CREATE TEMP TRIGGER IF NOT EXISTS meridian_traffic_totals_local_update
AFTER UPDATE OF site_id,bytes_in,bytes_out,recorded_at,recorded_at_ms ON main.traffic_logs
WHEN OLD.site_id IS NOT NEW.site_id OR OLD.bytes_in IS NOT NEW.bytes_in OR OLD.bytes_out IS NOT NEW.bytes_out
 OR OLD.recorded_at IS NOT NEW.recorded_at OR OLD.recorded_at_ms IS NOT NEW.recorded_at_ms
BEGIN UPDATE meridian_traffic_totals_version SET generation=generation+1,request_generation=request_generation+CASE WHEN OLD.requests<>0 OR NEW.requests<>0 THEN 1 ELSE 0 END WHERE id=1; END;
CREATE TEMP TRIGGER IF NOT EXISTS meridian_traffic_requests_local_update AFTER UPDATE OF requests ON main.traffic_logs
WHEN OLD.requests IS NOT NEW.requests
BEGIN UPDATE meridian_traffic_totals_version SET request_generation=request_generation+1 WHERE id=1; END;
CREATE TEMP TRIGGER IF NOT EXISTS meridian_traffic_totals_node_insert AFTER INSERT ON main.node_site_traffic_logs
BEGIN UPDATE meridian_traffic_totals_version SET generation=generation+1,request_generation=request_generation+CASE WHEN NEW.requests<>0 THEN 1 ELSE 0 END WHERE id=1; END;
CREATE TEMP TRIGGER IF NOT EXISTS meridian_traffic_totals_node_delete AFTER DELETE ON main.node_site_traffic_logs
BEGIN UPDATE meridian_traffic_totals_version SET generation=generation+1,request_generation=request_generation+CASE WHEN OLD.requests<>0 THEN 1 ELSE 0 END WHERE id=1; END;
CREATE TEMP TRIGGER IF NOT EXISTS meridian_traffic_totals_node_update
AFTER UPDATE OF site_id,bytes_in,bytes_out,recorded_at_ms ON main.node_site_traffic_logs
WHEN OLD.site_id IS NOT NEW.site_id OR OLD.bytes_in IS NOT NEW.bytes_in OR OLD.bytes_out IS NOT NEW.bytes_out
 OR OLD.recorded_at_ms IS NOT NEW.recorded_at_ms
BEGIN UPDATE meridian_traffic_totals_version SET generation=generation+1,request_generation=request_generation+CASE WHEN OLD.requests<>0 OR NEW.requests<>0 THEN 1 ELSE 0 END WHERE id=1; END;
CREATE TEMP TRIGGER IF NOT EXISTS meridian_traffic_requests_node_update AFTER UPDATE OF requests ON main.node_site_traffic_logs
WHEN OLD.requests IS NOT NEW.requests
BEGIN UPDATE meridian_traffic_totals_version SET request_generation=request_generation+1 WHERE id=1; END;
CREATE TEMP TRIGGER IF NOT EXISTS meridian_traffic_totals_site_insert AFTER INSERT ON main.sites
BEGIN UPDATE meridian_traffic_totals_version SET generation=generation+1 WHERE id=1; END;
CREATE TEMP TRIGGER IF NOT EXISTS meridian_traffic_totals_site_delete AFTER DELETE ON main.sites
BEGIN UPDATE meridian_traffic_totals_version SET generation=generation+1 WHERE id=1; END;
CREATE TEMP TRIGGER IF NOT EXISTS meridian_traffic_totals_site_update
AFTER UPDATE OF id,traffic_used,traffic_used_in,traffic_used_out ON main.sites
WHEN OLD.id IS NOT NEW.id OR OLD.traffic_used IS NOT NEW.traffic_used
 OR OLD.traffic_used_in IS NOT NEW.traffic_used_in OR OLD.traffic_used_out IS NOT NEW.traffic_used_out
BEGIN UPDATE meridian_traffic_totals_version SET generation=generation+1 WHERE id=1; END;`

func readTrafficTotalsVersion(ctx context.Context, conn *sql.Conn) (trafficTotalsVersion, error) {
	var version trafficTotalsVersion
	err := conn.QueryRowContext(ctx, `SELECT epoch,generation,request_generation,
		(SELECT data_version FROM pragma_data_version), (SELECT schema_version FROM pragma_schema_version)
		FROM temp.meridian_traffic_totals_version WHERE id=1`).Scan(
		&version.Epoch, &version.Generation, &version.RequestGeneration, &version.DataVersion, &version.SchemaVersion)
	return version, err
}

func initializeTrafficTotalsVersion(ctx context.Context, conn *sql.Conn) (trafficTotalsVersion, error) {
	// A fresh random identity prevents generation=0 on a replacement SQL
	// connection from matching a cache filled by an earlier connection.
	var identity [16]byte
	if _, err := rand.Read(identity[:]); err != nil {
		return trafficTotalsVersion{}, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return trafficTotalsVersion{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, trafficTotalsVersionDDL); err != nil {
		return trafficTotalsVersion{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO temp.meridian_traffic_totals_version(id,epoch,generation,request_generation)
		VALUES(1,?,0,0) ON CONFLICT(id) DO UPDATE SET epoch=excluded.epoch,generation=0,request_generation=0`, hex.EncodeToString(identity[:])); err != nil {
		return trafficTotalsVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return trafficTotalsVersion{}, err
	}
	return readTrafficTotalsVersion(ctx, conn)
}

func sameTrafficByteTotalsVersion(a, b trafficTotalsVersion) bool {
	return a.Epoch == b.Epoch && a.Generation == b.Generation && a.DataVersion == b.DataVersion && a.SchemaVersion == b.SchemaVersion
}

func sameTrafficRequestTotalsVersion(a, b trafficTotalsVersion) bool {
	return a.Epoch == b.Epoch && a.RequestGeneration == b.RequestGeneration && a.DataVersion == b.DataVersion && a.SchemaVersion == b.SchemaVersion
}

func copyTrafficTotals(values map[int64]int64) map[int64]int64 {
	result := make(map[int64]int64, len(values))
	for siteID, value := range values {
		result[siteID] = value
	}
	return result
}

// cachedTrafficSinceBySite keeps exact persisted totals between accounting
// mutations. It does not cache live/pending counters, which the snapshot still
// overlays at its normal cadence. One pinned connection makes generation
// inspection, cache reuse and fills indivisible with respect to local writers.
func (d *DB) cachedTrafficSinceBySite(start time.Time, billingMode string) (map[int64]int64, error) {
	key := trafficTotalsKey{Lifetime: start.IsZero(), StartMS: start.UnixMilli(), BillingMode: trafficBillingModeLabel(billingMode)}
	if !key.Lifetime {
		// Preserve the legacy wall-clock predicate, including changes to the
		// server's location. The UTC boundary alone does not describe that SQL.
		key.LegacyStart = start.In(time.Local).Format("2006-01-02 15:04:05")
	}
	d.trafficTotalsMu.Lock()
	defer d.trafficTotalsMu.Unlock()
	ctx := context.Background()
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	version, versionErr := readTrafficTotalsVersion(ctx, conn)
	if versionErr != nil || version.Epoch != d.trafficTotalsEpoch || version.SchemaVersion != d.trafficTotalsSchema {
		d.trafficTotals = nil
		d.trafficRequestTotal = nil
		version, versionErr = initializeTrafficTotalsVersion(ctx, conn)
		if versionErr != nil {
			// Read-only snapshots/embedders can still read authoritative totals
			// even if SQLite disallows installing TEMP tracking state.
			return queryTrafficSinceBySite(ctx, conn, start, billingMode)
		}
		d.trafficTotalsEpoch, d.trafficTotalsSchema = version.Epoch, version.SchemaVersion
	}
	if cached := d.trafficTotals; cached != nil && cached.key == key && sameTrafficByteTotalsVersion(cached.version, version) {
		return copyTrafficTotals(cached.values), nil
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	values, err := queryTrafficSinceBySite(ctx, tx, start, billingMode)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// TEMP triggers see this connection's writes. data_version additionally
	// sees commits by other connections. Check after ending the read snapshot:
	// an external commit during the query must not label old totals as current.
	after, err := readTrafficTotalsVersion(ctx, conn)
	if err == nil && sameTrafficByteTotalsVersion(version, after) {
		d.trafficTotals = &trafficTotalsCacheEntry{key: key, version: version, values: values}
	} else {
		d.trafficTotals = nil
	}
	return copyTrafficTotals(values), nil
}

// cachedRequestTotalSince returns persisted request counts for a time window.
// The request cache uses the same TEMP accounting generation as traffic totals,
// but its own key so a natural-month dashboard summary does not evict the
// independently configured traffic quota cycle.
func (d *DB) cachedRequestTotalSince(start time.Time) (int64, error) {
	startMS := start.UnixMilli()
	legacyStart := ""
	if !start.IsZero() {
		legacyStart = start.In(time.Local).Format("2006-01-02 15:04:05")
	}
	d.trafficTotalsMu.Lock()
	defer d.trafficTotalsMu.Unlock()
	ctx := context.Background()
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	version, versionErr := readTrafficTotalsVersion(ctx, conn)
	if versionErr != nil || version.Epoch != d.trafficTotalsEpoch || version.SchemaVersion != d.trafficTotalsSchema {
		d.trafficTotals = nil
		d.trafficRequestTotal = nil
		version, versionErr = initializeTrafficTotalsVersion(ctx, conn)
		if versionErr != nil {
			return queryTrafficRequestTotal(ctx, conn, start)
		}
		d.trafficTotalsEpoch, d.trafficTotalsSchema = version.Epoch, version.SchemaVersion
	}
	if cached := d.trafficRequestTotal; cached != nil && cached.startMS == startMS && cached.legacyStart == legacyStart && sameTrafficRequestTotalsVersion(cached.version, version) {
		return cached.value, nil
	}
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	value, err := queryTrafficRequestTotal(ctx, tx, start)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	// Check after ending the read snapshot. A concurrent external commit means
	// this total is still valid for this response but cannot populate the cache.
	after, err := readTrafficTotalsVersion(ctx, conn)
	if err == nil && sameTrafficRequestTotalsVersion(version, after) {
		d.trafficRequestTotal = &trafficRequestTotalsCacheEntry{
			startMS: startMS, legacyStart: legacyStart, version: version, value: value,
		}
	} else {
		d.trafficRequestTotal = nil
	}
	return value, nil
}

func queryTrafficRequestTotal(ctx context.Context, queryer trafficTotalsQueryer, start time.Time) (int64, error) {
	query := `SELECT COALESCE(SUM(requests),0) FROM (
		SELECT requests FROM traffic_logs
		UNION ALL
		SELECT requests FROM node_site_traffic_logs
	)`
	var args []any
	if !start.IsZero() {
		startMS := start.UnixMilli()
		startText := start.In(time.Local).Format("2006-01-02 15:04:05")
		query = `WITH source AS (
			SELECT requests FROM traffic_logs
			WHERE recorded_at_ms >= ? OR (recorded_at_ms = 0 AND recorded_at >= ?)
			UNION ALL
			SELECT requests FROM node_site_traffic_logs WHERE recorded_at_ms >= ?
		)
		SELECT COALESCE(SUM(requests),0) FROM source`
		args = []any{startMS, startText, startMS}
	}
	rows, err := queryer.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return 0, nil
	}
	var total int64
	if err := rows.Scan(&total); err != nil {
		return 0, err
	}
	return total, rows.Err()
}
