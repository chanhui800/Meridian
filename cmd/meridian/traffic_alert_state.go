package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"time"
)

const trafficAlertStateCyclesKept = 2

const trafficAlertDeliveryLease = time.Minute

type telegramTrafficAlertClaim struct {
	leaseID  string
	attempts int
}

func ensureTelegramTrafficAlertStateSchema(ctx context.Context, conn *sql.Conn, previousVersion int) error {
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS telegram_traffic_alert_state (
		kind TEXT NOT NULL CHECK(kind IN ('node','site')),
		resource_id INTEGER NOT NULL CHECK(resource_id>0),
		cycle_start_ms INTEGER NOT NULL CHECK(cycle_start_ms>=0),
		highest_bucket INTEGER NOT NULL DEFAULT 0 CHECK(highest_bucket>=0),
		lease_id TEXT NOT NULL DEFAULT '',
		lease_until_ms INTEGER NOT NULL DEFAULT 0,
		retry_attempts INTEGER NOT NULL DEFAULT 0,
		next_attempt_at_ms INTEGER NOT NULL DEFAULT 0,
		updated_at_ms INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY(kind,resource_id,cycle_start_ms)
	) WITHOUT ROWID;
	CREATE TRIGGER IF NOT EXISTS telegram_alert_state_node_deleted AFTER DELETE ON control_nodes
	BEGIN DELETE FROM telegram_traffic_alert_state WHERE kind='node' AND resource_id=OLD.id; END;
	CREATE TRIGGER IF NOT EXISTS telegram_alert_state_site_deleted AFTER DELETE ON sites
	BEGIN DELETE FROM telegram_traffic_alert_state WHERE kind='site' AND resource_id=OLD.id; END;`); err != nil {
		return err
	}
	if previousVersion >= 59 {
		return nil
	}
	// Preserve the strongest evidence still available in the old bounded
	// history. Deleted history cannot reconstruct a prior delivery watermark.
	if _, err := conn.ExecContext(ctx, `INSERT INTO telegram_traffic_alert_state
		(kind,resource_id,cycle_start_ms,highest_bucket,updated_at_ms)
		SELECT kind,resource_id,cycle_start_ms,MAX(alert_bucket),MAX(sent_at_ms)
		FROM telegram_traffic_alerts
		WHERE alert_bucket>0 AND cycle_start_ms>=0 AND (
			(kind='node' AND EXISTS(SELECT 1 FROM control_nodes n WHERE n.id=resource_id)) OR
			(kind='site' AND EXISTS(SELECT 1 FROM sites s WHERE s.id=resource_id)))
		GROUP BY kind,resource_id,cycle_start_ms
		ON CONFLICT(kind,resource_id,cycle_start_ms) DO UPDATE SET
			highest_bucket=MAX(highest_bucket,excluded.highest_bucket),
			updated_at_ms=MAX(updated_at_ms,excluded.updated_at_ms)`); err != nil {
		return err
	}
	var resetDay, timezone int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(traffic_reset_day,0),COALESCE(schedule_timezone_offset,480) FROM system_settings WHERE id=1`).Scan(&resetDay, &timezone); err != nil {
		return err
	}
	currentSiteCycle := trafficCycleStart(time.Now(), resetDay, timezoneLocation(timezone))
	currentSiteCycleMS := int64(0)
	if !currentSiteCycle.IsZero() {
		currentSiteCycleMS = currentSiteCycle.UnixMilli()
	}
	_, err := conn.ExecContext(ctx, `DELETE FROM telegram_traffic_alert_state WHERE
		(kind,resource_id,cycle_start_ms) IN (
		SELECT kind,resource_id,cycle_start_ms FROM (
			SELECT a.kind,a.resource_id,a.cycle_start_ms,
			ROW_NUMBER() OVER(PARTITION BY a.kind,a.resource_id ORDER BY
				CASE WHEN (a.kind='node' AND a.cycle_start_ms=COALESCE(n.cycle_started_at_ms,0)) OR
					(a.kind='site' AND a.cycle_start_ms=?) THEN 0 ELSE 1 END,
				a.cycle_start_ms DESC,a.updated_at_ms DESC) AS cycle_rank
			FROM telegram_traffic_alert_state a LEFT JOIN control_nodes n ON a.kind='node' AND n.id=a.resource_id
		) WHERE cycle_rank>?)`, currentSiteCycleMS, trafficAlertStateCyclesKept)
	return err
}

func (d *DB) claimTelegramTrafficAlert(alert telegramTrafficAlert, now time.Time) (telegramTrafficAlertClaim, bool, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return telegramTrafficAlertClaim{}, false, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO telegram_traffic_alert_state
		(kind,resource_id,cycle_start_ms,updated_at_ms) SELECT ?,?,?,?
		WHERE EXISTS(SELECT 1 FROM control_nodes WHERE ?='node' AND id=?) OR
			EXISTS(SELECT 1 FROM sites WHERE ?='site' AND id=?)
		ON CONFLICT(kind,resource_id,cycle_start_ms) DO NOTHING`, alert.kind, alert.id, alert.cycleStartMS, now.UnixMilli(),
		alert.kind, alert.id, alert.kind, alert.id); err != nil {
		return telegramTrafficAlertClaim{}, false, err
	}
	claim := telegramTrafficAlertClaim{leaseID: rand.Text()}
	result, err := tx.Exec(`UPDATE telegram_traffic_alert_state SET lease_id=?,lease_until_ms=?,
		retry_attempts=MIN(retry_attempts+1,5),updated_at_ms=MAX(updated_at_ms,?)
		WHERE kind=? AND resource_id=? AND cycle_start_ms=? AND highest_bucket<?
			AND lease_until_ms<=? AND next_attempt_at_ms<=?`, claim.leaseID, now.Add(trafficAlertDeliveryLease).UnixMilli(),
		now.UnixMilli(), alert.kind, alert.id, alert.cycleStartMS, alert.bucket, now.UnixMilli(), now.UnixMilli())
	if err != nil {
		return telegramTrafficAlertClaim{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return telegramTrafficAlertClaim{}, false, err
	}
	if err := pruneTelegramTrafficAlertResourceTx(tx, alert, now); err != nil {
		return telegramTrafficAlertClaim{}, false, err
	}
	if err := tx.QueryRow(`SELECT retry_attempts FROM telegram_traffic_alert_state
		WHERE kind=? AND resource_id=? AND cycle_start_ms=? AND lease_id=?`, alert.kind, alert.id, alert.cycleStartMS, claim.leaseID).Scan(&claim.attempts); err != nil {
		if err == sql.ErrNoRows {
			// An obsolete cycle cannot displace the actual configured cycle.
			return telegramTrafficAlertClaim{}, false, nil
		}
		return telegramTrafficAlertClaim{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return telegramTrafficAlertClaim{}, false, err
	}
	return claim, true, nil
}

func telegramTrafficAlertRetryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 5 {
		attempts = 5
	}
	delay := time.Minute * time.Duration(1<<uint(attempts-1))
	if delay > 15*time.Minute {
		return 15 * time.Minute
	}
	return delay
}

func (d *DB) retryTelegramTrafficAlert(alert telegramTrafficAlert, claim telegramTrafficAlertClaim, now time.Time) error {
	_, err := d.db.Exec(`UPDATE telegram_traffic_alert_state SET lease_id='',lease_until_ms=0,
		next_attempt_at_ms=?,updated_at_ms=MAX(updated_at_ms,?)
		WHERE kind=? AND resource_id=? AND cycle_start_ms=? AND lease_id=?`,
		now.Add(telegramTrafficAlertRetryDelay(claim.attempts)).UnixMilli(), now.UnixMilli(),
		alert.kind, alert.id, alert.cycleStartMS, claim.leaseID)
	return err
}

func pruneTelegramTrafficAlertResourceTx(tx *sql.Tx, alert telegramTrafficAlert, now time.Time) error {
	currentCycleMS := int64(0)
	if alert.kind == "node" {
		if err := tx.QueryRow(`SELECT COALESCE(cycle_started_at_ms,0) FROM control_nodes WHERE id=?`, alert.id).Scan(&currentCycleMS); err != nil {
			return err
		}
	} else {
		var resetDay, timezone int
		if err := tx.QueryRow(`SELECT COALESCE(traffic_reset_day,0),COALESCE(schedule_timezone_offset,480)
			FROM system_settings WHERE id=1`).Scan(&resetDay, &timezone); err != nil {
			return err
		}
		cycle := trafficCycleStart(now, resetDay, timezoneLocation(timezone))
		if !cycle.IsZero() {
			currentCycleMS = cycle.UnixMilli()
		}
	}
	// Pin the actual configured cycle even when reset is disabled (cycle zero),
	// while retaining one recent cycle for late results. The primary key makes
	// storage at most two rows per live resource, independent of escalation.
	_, err := tx.Exec(`DELETE FROM telegram_traffic_alert_state WHERE kind=? AND resource_id=?
		AND cycle_start_ms NOT IN (SELECT cycle_start_ms FROM telegram_traffic_alert_state
			WHERE kind=? AND resource_id=? ORDER BY CASE WHEN cycle_start_ms=? THEN 0 ELSE 1 END,
			cycle_start_ms DESC,updated_at_ms DESC LIMIT ?)`, alert.kind, alert.id,
		alert.kind, alert.id, currentCycleMS, trafficAlertStateCyclesKept)
	return err
}
