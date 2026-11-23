package main

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

type nodeTrafficReportIdentity struct {
	sessionID    string
	source       string
	runtimeEpoch string
	sequence     int64
}

type nodeTrafficWatermark struct {
	exists    bool
	epoch     string
	sessionID string
	sequence  int64
	bytesIn   int64
	bytesOut  int64
	requests  int64
	cacheSize int64
}

func loadNodeTrafficWatermarkTx(tx *sql.Tx, nodeID, siteID int64) (nodeTrafficWatermark, error) {
	var value nodeTrafficWatermark
	err := tx.QueryRow(`SELECT boot_id,last_bytes_in,last_bytes_out,last_request_count,cache_size_bytes,report_session_id,report_sequence
		FROM node_site_counters WHERE node_id=? AND site_id=?`, nodeID, siteID).Scan(
		&value.epoch, &value.bytesIn, &value.bytesOut, &value.requests, &value.cacheSize, &value.sessionID, &value.sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return value, nil
	}
	value.exists = err == nil
	return value, err
}

func loadNodeTrafficEpochTx(tx *sql.Tx, nodeID, siteID int64, epoch string) (nodeTrafficWatermark, error) {
	value := nodeTrafficWatermark{epoch: epoch}
	err := tx.QueryRow(`SELECT last_bytes_in,last_bytes_out,last_request_count,cache_size_bytes,report_session_id,report_sequence
		FROM node_site_counter_epochs WHERE node_id=? AND site_id=? AND boot_id=?`, nodeID, siteID, epoch).Scan(
		&value.bytesIn, &value.bytesOut, &value.requests, &value.cacheSize, &value.sessionID, &value.sequence)
	if errors.Is(err, sql.ErrNoRows) {
		return value, nil
	}
	value.exists = err == nil
	return value, err
}

func nodeTrafficEpochMayBecomeCurrent(current, period nodeTrafficWatermark, epoch string, stat NodeSiteStat, identity nodeTrafficReportIdentity, delayed bool) bool {
	if !current.exists || current.epoch == epoch {
		return true
	}
	if identity.runtimeEpoch != "" {
		// Older Agents incremented a process-wide site runtime generation on
		// hot apply. A first delivery from its drain may have a new report
		// sequence and the same per-site epoch, so fence that outer generation
		// as well. Current Agents keep it stable for the process.
		if suffix, ok := strings.CutPrefix(current.epoch, identity.sessionID+":"); ok {
			previousRuntime, _, _ := strings.Cut(suffix, ":")
			previousGeneration, previousErr := strconv.ParseUint(previousRuntime, 10, 64)
			generation, currentErr := strconv.ParseUint(identity.runtimeEpoch, 10, 64)
			if previousErr == nil && currentErr == nil && previousGeneration != generation {
				return generation > previousGeneration
			}
		}
	}
	if stat.CounterEpoch > 0 {
		// Explicit generations order this site's periods independently of the
		// node-wide sequence. A newer period may already have landed without
		// becoming current on an older Controller, so compare before treating a
		// known period as a drain. This also works with migrated sequence-zero rows.
		if suffix, ok := strings.CutPrefix(current.epoch, identity.source+":"); ok {
			if previousEpoch, err := strconv.ParseUint(suffix, 10, 64); err == nil && previousEpoch != stat.CounterEpoch {
				return stat.CounterEpoch > previousEpoch
			}
		}
		// Without comparable generations, retain the conservative tombstone
		// rule: a known non-current named period cannot replace current state.
		if period.exists {
			return false
		}
	}
	if current.sessionID == identity.sessionID && current.sequence > 0 {
		return identity.sequence > current.sequence
	}
	// On an upgraded database the old row has no per-site sequence. The full
	// report's existing process watermark can still fence an older epoch;
	// it does not suppress accounting or partial reports for other sites.
	return !delayed || current.sessionID != identity.sessionID
}

func persistNodeTrafficEpochTx(tx *sql.Tx, nodeID, siteID int64, epoch string, stat NodeSiteStat, identity nodeTrafficReportIdentity, currentIn, currentOut, cacheSize, nowMS int64) error {
	_, err := tx.Exec(`INSERT INTO node_site_counter_epochs
		(node_id,site_id,boot_id,last_bytes_in,last_bytes_out,last_request_count,cache_size_bytes,report_session_id,report_sequence,updated_at_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(node_id,site_id,boot_id) DO UPDATE SET
			last_bytes_in=excluded.last_bytes_in,last_bytes_out=excluded.last_bytes_out,
			last_request_count=excluded.last_request_count,cache_size_bytes=excluded.cache_size_bytes,
			report_session_id=excluded.report_session_id,report_sequence=MAX(report_sequence,excluded.report_sequence),
			updated_at_ms=excluded.updated_at_ms`,
		nodeID, siteID, epoch, currentIn, currentOut, stat.RequestCount, cacheSize, identity.sessionID, identity.sequence, nowMS)
	return err
}

func persistCurrentNodeTrafficTx(tx *sql.Tx, nodeID, siteID int64, epoch string, stat NodeSiteStat, identity nodeTrafficReportIdentity, currentIn, currentOut, cacheSize, nowMS int64) error {
	_, err := tx.Exec(`INSERT INTO node_site_counters
		(node_id,site_id,boot_id,last_bytes_in,last_bytes_out,last_request_count,cache_size_bytes,report_session_id,report_sequence,updated_at_ms)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(node_id,site_id) DO UPDATE SET
			boot_id=excluded.boot_id,last_bytes_in=excluded.last_bytes_in,last_bytes_out=excluded.last_bytes_out,
			last_request_count=excluded.last_request_count,cache_size_bytes=excluded.cache_size_bytes,
			report_session_id=excluded.report_session_id,report_sequence=excluded.report_sequence,
			updated_at_ms=excluded.updated_at_ms`,
		nodeID, siteID, epoch, currentIn, currentOut, stat.RequestCount, cacheSize, identity.sessionID, identity.sequence, nowMS)
	return err
}

// recordNodeTrafficPeriodTx accounts a period independently from the current
// visible row. A retired period can advance its durable watermark without
// changing site metadata or seeding the full/live overlay.
func recordNodeTrafficPeriodTx(tx *sql.Tx, nodeID int64, epoch string, stat *NodeSiteStat, nowMS int64, allowedSites map[int64]authorizedNodeSite, delayedFullReport bool, identity nodeTrafficReportIdentity) (bool, error) {
	site, ok := authorizedNodeSiteForStat(allowedSites, *stat)
	if !ok {
		return false, nil
	}
	current, err := loadNodeTrafficWatermarkTx(tx, nodeID, site.ID)
	if err != nil {
		return false, err
	}
	period, err := loadNodeTrafficEpochTx(tx, nodeID, site.ID, epoch)
	if err != nil {
		return false, err
	}
	if !period.exists && current.exists && current.epoch == epoch {
		// A migrated current watermark is already accounted. Seed it lazily as
		// well as during migration so interrupted upgrades cannot recount it.
		period = current
	}
	applyCurrent := nodeTrafficEpochMayBecomeCurrent(current, period, epoch, *stat, identity, delayedFullReport)
	cumulativeReport := stat.CounterEpoch > 0 || stat.CumulativeBytesIn > 0 || stat.CumulativeBytesOut > 0
	currentIn, currentOut := stat.CumulativeBytesIn, stat.CumulativeBytesOut
	if currentIn == 0 && stat.BytesIn > 0 {
		currentIn = stat.BytesIn
	}
	if currentOut == 0 && stat.BytesOut > 0 {
		currentOut = stat.BytesOut
	}
	if period.exists && delayedFullReport && !cumulativeReport && stat.RequestCount < period.requests {
		// Preserve legacy cumulative request counts without throwing away an
		// interval byte report which may still need to land.
		if stat.BytesIn == 0 && stat.BytesOut == 0 {
			return false, nil
		}
		stat.RequestCount = period.requests
	}
	if period.exists && cumulativeReport && (stat.CounterEpoch > 0 || delayedFullReport) &&
		(currentIn < period.bytesIn || currentOut < period.bytesOut || stat.RequestCount < period.requests) {
		return false, nil
	}
	if period.exists && stat.CounterEpoch > 0 && identity.sequence <= period.sequence {
		// An exact retry can restore a newer period already accounted by an
		// older Controller without billing it again. Other covered snapshots
		// must not replace current metadata or lower a durable watermark.
		restoreCurrent := applyCurrent && current.exists && current.epoch != epoch &&
			identity.sequence == period.sequence && currentIn == period.bytesIn &&
			currentOut == period.bytesOut && stat.RequestCount == period.requests
		if !restoreCurrent {
			return false, nil
		}
	}

	deltaIn, deltaOut, deltaRequests := stat.BytesIn, stat.BytesOut, stat.RequestCount
	if period.exists {
		deltaIn, deltaOut, deltaRequests = 0, 0, 0
		if currentIn >= period.bytesIn && currentOut >= period.bytesOut && stat.RequestCount >= period.requests {
			deltaIn, deltaOut, deltaRequests = currentIn-period.bytesIn, currentOut-period.bytesOut, stat.RequestCount-period.requests
		}
	} else if current.exists {
		// Preserve first-report compatibility for Agents which carried only
		// monotonic counters over a runtime change instead of explicit deltas.
		if currentIn >= current.bytesIn && stat.BytesIn == 0 {
			deltaIn = currentIn - current.bytesIn
		}
		if currentOut >= current.bytesOut && stat.BytesOut == 0 {
			deltaOut = currentOut - current.bytesOut
		}
		if stat.CounterEpoch == 0 && stat.RequestCount >= current.requests {
			deltaRequests = stat.RequestCount - current.requests
		}
	}
	if deltaIn > 0 || deltaOut > 0 || deltaRequests > 0 {
		bucket := (nowMS / 60000) * 60000
		if _, err := tx.Exec(`INSERT INTO node_site_traffic_logs(node_id,site_id,bytes_in,bytes_out,requests,recorded_at_ms)
			VALUES(?,?,?,?,?,?) ON CONFLICT(node_id,site_id,recorded_at_ms) DO UPDATE SET
				bytes_in=bytes_in+excluded.bytes_in,bytes_out=bytes_out+excluded.bytes_out,requests=requests+excluded.requests`,
			nodeID, site.ID, deltaIn, deltaOut, deltaRequests, bucket); err != nil {
			return false, err
		}
		if deltaIn > 0 || deltaOut > 0 {
			if _, err := tx.Exec(`UPDATE sites SET traffic_used=traffic_used+?+?,traffic_used_in=traffic_used_in+?,
				traffic_used_out=traffic_used_out+?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, deltaIn, deltaOut, deltaIn, deltaOut, site.ID); err != nil {
				return false, err
			}
		}
	}
	cacheSize := period.cacheSize
	if !period.exists {
		cacheSize = current.cacheSize
	}
	if stat.CacheSizeValid {
		cacheSize = stat.CacheSizeBytes
	}
	if err := persistNodeTrafficEpochTx(tx, nodeID, site.ID, epoch, *stat, identity, currentIn, currentOut, cacheSize, nowMS); err != nil {
		return false, err
	}
	if !applyCurrent {
		return false, nil
	}
	if err := persistCurrentNodeTrafficTx(tx, nodeID, site.ID, epoch, *stat, identity, currentIn, currentOut, cacheSize, nowMS); err != nil {
		return false, err
	}
	// Final only ends this counter's data stream. Drain/event authorization is
	// still retained by its existing bounded tombstone lifecycle.
	return true, nil
}
