package main

import (
	"errors"
	"strings"
	"time"
)

const (
	agentFullReportInterval   = 5 * time.Second
	agentLiveReportInterval   = 2 * time.Second
	maxNodeLiveSitesPerReport = maxNodeSiteStatsPerReport
)

type nodeLiveTrafficKey struct {
	nodeID int64
	siteID int64
}

type nodeLiveTrafficState struct {
	NodeLiveSiteTraffic
	reportSessionID string
	counterEpoch    string
	sequence        int64
	updatedAt       time.Time
}

func validateNodeLiveReport(report NodeLiveReport, now time.Time) error {
	report.ReportSessionID = strings.TrimSpace(report.ReportSessionID)
	report.CounterEpoch = strings.TrimSpace(report.CounterEpoch)
	if report.ReportSessionID == "" || len(report.ReportSessionID) > 128 || len(report.CounterEpoch) > 128 {
		return errors.New("invalid live report session")
	}
	if report.Sequence <= 0 || report.SampledAtMS <= 0 || len(report.SiteStats) > maxNodeLiveSitesPerReport {
		return errors.New("invalid live report metadata")
	}
	// The timestamp is only a diagnostic hint. The server receive time is used
	// as the freshness boundary, but reject wildly future samples so a peer
	// cannot pin an apparently fresh value indefinitely.
	if report.SampledAtMS > now.Add(2*time.Minute).UnixMilli() {
		return errors.New("invalid live report timestamp")
	}
	for _, stat := range report.SiteStats {
		if stat.SiteID <= 0 || strings.TrimSpace(stat.Host) == "" || len(stat.Host) > 255 || stat.CumulativeBytesIn < 0 || stat.CumulativeBytesOut < 0 || stat.Requests < 0 {
			return errors.New("invalid live site traffic")
		}
	}
	return nil
}

// recordNodeLiveReport validates site ownership in a read transaction and
// stores accepted counters only in memory. It deliberately performs no
// SQLite writes, so a 2-second dashboard sample cannot amplify persistence
// load or alter billing history.
func (d *DB) recordNodeLiveReport(nodeID int64, report NodeLiveReport, now time.Time) (accepted, discarded []int64, err error) {
	if d == nil || d.db == nil || nodeID <= 0 {
		return nil, nil, errInvalidAgentToken
	}
	tx, err := d.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	allowed, err := authorizedNodeSitesTx(tx, nodeID, now.UnixMilli())
	if err != nil {
		return nil, nil, err
	}

	accepted = make([]int64, 0, len(report.SiteStats))
	discarded = make([]int64, 0)
	// Use receive time for freshness. An Agent clock can be skewed, while the
	// Controller must ensure a delayed sample cannot look newer than a current
	// heartbeat.
	serverSampledAt := now.UnixMilli()
	d.agentLiveMu.Lock()
	if d.agentLive == nil {
		d.agentLive = make(map[nodeLiveTrafficKey]nodeLiveTrafficState)
	}
	for key, state := range d.agentLive {
		if now.Sub(state.updatedAt) > 2*nodeOnlineWindow {
			delete(d.agentLive, key)
		}
	}
	for _, stat := range report.SiteStats {
		site, ok := allowed[stat.SiteID]
		host := requestPublicHost(stat.Host)
		if !ok || host == "" || host != site.PublicHost {
			discarded = appendUniqueInt64(discarded, stat.SiteID)
			continue
		}
		key := nodeLiveTrafficKey{nodeID: nodeID, siteID: site.ID}
		previous, exists := d.agentLive[key]
		if exists && previous.reportSessionID == report.ReportSessionID && report.Sequence <= previous.sequence {
			continue
		}
		d.agentLive[key] = nodeLiveTrafficState{
			NodeLiveSiteTraffic: NodeLiveSiteTraffic{
				SiteID: stat.SiteID, Host: host,
				CumulativeBytesIn:  stat.CumulativeBytesIn,
				CumulativeBytesOut: stat.CumulativeBytesOut,
				Requests:           stat.Requests,
				SampledAtMS:        serverSampledAt,
			},
			reportSessionID: report.ReportSessionID,
			counterEpoch:    report.CounterEpoch,
			sequence:        report.Sequence,
			updatedAt:       now,
		}
		accepted = appendUniqueInt64(accepted, stat.SiteID)
	}
	d.agentLiveMu.Unlock()
	return accepted, discarded, nil
}

func appendUniqueInt64(values []int64, value int64) []int64 {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// nodeLiveTrafficOverlay returns accepted samples keyed by site ID. A sample
// is never allowed to resurrect an offline node; NodeSiteLiveTrafficSnapshot
// still derives Running from the persisted Agent heartbeat and DNS state.
func (d *DB) nodeLiveTrafficOverlay(now time.Time) map[nodeLiveTrafficKey]nodeLiveTrafficState {
	result := make(map[nodeLiveTrafficKey]nodeLiveTrafficState)
	if d == nil {
		return result
	}
	d.agentLiveMu.RLock()
	defer d.agentLiveMu.RUnlock()
	for key, state := range d.agentLive {
		if state.updatedAt.IsZero() || now.Sub(state.updatedAt) > nodeOnlineWindow {
			continue
		}
		// Keep both node and site in the key: during a scheduler transition two
		// nodes can report the same site, and a site-only map would choose one
		// nondeterministically.
		result[key] = state
	}
	return result
}
