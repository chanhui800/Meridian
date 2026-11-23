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
	Epoch         string
	Generation    int64
	DataVersion   int64
	SchemaVersion int64
}

type trafficTotalsCacheEntry struct {
	key     trafficTotalsKey
	version trafficTotalsVersion
	values  map[int64]int64
}

// Only accounting changes invalidate the connection-local generation. Keeping
// these triggers in TEMP avoids persistent cache state in backups/schema and
// also covers raw SQL, backfills, cascades and rolled-back transactions without
// requiring every writer to remember a Go invalidation hook.
const trafficTotalsVersionDDL = `
CREATE TEMP TABLE IF NOT EXISTS meridian_traffic_totals_version (
	id INTEGER PRIMARY KEY CHECK (id=1), epoch TEXT NOT NULL, generation INTEGER NOT NULL
);
CREATE TEMP TRIGGER IF NOT EXISTS meridian_traffic_totals_local_insert AFTER INSERT ON main.traffic_logs
BEGIN UPDATE meridian_traffic_totals_version SET generation=generation+1 WHERE id=1; END;
CREATE TEMP TRIGGER IF NOT EXISTS meridian_traffic_totals_local_delete AFTER DELETE ON main.traffic_logs
BEGIN UPDATE meridian_traffic_totals_version SET generation=generation+1 WHERE id=1; END;
CREATE TEMP TRIGGER IF NOT EXISTS meridian_traffic_totals_local_update
AFTER UPDATE OF site_id,bytes_in,bytes_out,recorded_at,recorded_at_ms ON main.traffic_logs
WHEN OLD.site_id IS NOT NEW.site_id OR OLD.bytes_in IS NOT NEW.bytes_in OR OLD.bytes_out IS NOT NEW.bytes_out
 OR OLD.recorded_at IS NOT NEW.recorded_at OR OLD.recorded_at_ms IS NOT NEW.recorded_at_ms
BEGIN UPDATE meridian_traffic_totals_version SET generation=generation+1 WHERE id=1; END;
CREATE TEMP TRIGGER IF NOT EXISTS meridian_traffic_totals_node_insert AFTER INSERT ON main.node_site_traffic_logs
BEGIN UPDATE meridian_traffic_totals_version SET generation=generation+1 WHERE id=1; END;
CREATE TEMP TRIGGER IF NOT EXISTS meridian_traffic_totals_node_delete AFTER DELETE ON main.node_site_traffic_logs
BEGIN UPDATE meridian_traffic_totals_version SET generation=generation+1 WHERE id=1; END;
CREATE TEMP TRIGGER IF NOT EXISTS meridian_traffic_totals_node_update
AFTER UPDATE OF site_id,bytes_in,bytes_out,recorded_at_ms ON main.node_site_traffic_logs
WHEN OLD.site_id IS NOT NEW.site_id OR OLD.bytes_in IS NOT NEW.bytes_in OR OLD.bytes_out IS NOT NEW.bytes_out
 OR OLD.recorded_at_ms IS NOT NEW.recorded_at_ms
BEGIN UPDATE meridian_traffic_totals_version SET generation=generation+1 WHERE id=1; END;
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
	err := conn.QueryRowContext(ctx, `SELECT epoch,generation,
		(SELECT data_version FROM pragma_data_version), (SELECT schema_version FROM pragma_schema_version)
		FROM temp.meridian_traffic_totals_version WHERE id=1`).Scan(
		&version.Epoch, &version.Generation, &version.DataVersion, &version.SchemaVersion)
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO temp.meridian_traffic_totals_version(id,epoch,generation)
		VALUES(1,?,0) ON CONFLICT(id) DO UPDATE SET epoch=excluded.epoch,generation=0`, hex.EncodeToString(identity[:])); err != nil {
		return trafficTotalsVersion{}, err
	}
	if err := tx.Commit(); err != nil {
		return trafficTotalsVersion{}, err
	}
	return readTrafficTotalsVersion(ctx, conn)
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
		version, versionErr = initializeTrafficTotalsVersion(ctx, conn)
		if versionErr != nil {
			// Read-only snapshots/embedders can still read authoritative totals
			// even if SQLite disallows installing TEMP tracking state.
			return queryTrafficSinceBySite(ctx, conn, start, billingMode)
		}
		d.trafficTotalsEpoch, d.trafficTotalsSchema = version.Epoch, version.SchemaVersion
	}
	if cached := d.trafficTotals; cached != nil && cached.key == key && cached.version == version {
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
	if err == nil && version == after {
		d.trafficTotals = &trafficTotalsCacheEntry{key: key, version: version, values: values}
	} else {
		d.trafficTotals = nil
	}
	return copyTrafficTotals(values), nil
}
