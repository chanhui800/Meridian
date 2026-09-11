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
	reportSessionID   string
	counterEpoch      string
	sequence          int64
	telemetrySequence int64
	updatedAt         time.Time
}

func validateNodeLiveReport(report NodeLiveReport, now time.Time) error {
	report.ReportSessionID = strings.TrimSpace(report.ReportSessionID)
	report.CounterEpoch = strings.TrimSpace(report.CounterEpoch)
	if report.ReportSessionID == "" || len(report.ReportSessionID) > 128 || len(report.CounterEpoch) > 128 {
		return errors.New("invalid live report session")
	}
	if report.AgentLeaseID != "" && (report.SessionEpoch <= 0 || len(report.AgentLeaseID) > 128) {
		return errors.New("invalid live report lease")
	}
	if report.Sequence <= 0 || report.TelemetrySequence < 0 || report.SampledAtMS <= 0 || len(report.SiteStats) > maxNodeLiveSitesPerReport {
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

func telemetrySequenceIsStale(previous nodeLiveTrafficState, sessionID, counterEpoch string, sequence int64) bool {
	if sequence <= 0 || previous.telemetrySequence <= 0 {
		return false
	}
	// The shared sequence restarts with a new Agent process. Only compare it
	// inside the same report session/counter epoch; a new session is a fresh
	// ordering domain and may legitimately begin at sequence 1.
	return previous.reportSessionID == sessionID && previous.counterEpoch == counterEpoch && sequence <= previous.telemetrySequence
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
	var activeSession, lease string
	var activeEpoch int64
	if err := tx.QueryRow("SELECT active_agent_session_id,agent_session_epoch,agent_lease_id FROM control_nodes WHERE id=?", nodeID).Scan(&activeSession, &activeEpoch, &lease); err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(activeSession) != "" && report.AgentLeaseID == "" && report.SessionEpoch == 0 {
		return nil, nil, errStaleAgentSession
	}
	if report.AgentLeaseID != "" || report.SessionEpoch > 0 {
		if report.AgentLeaseID == "" || report.SessionEpoch <= 0 || strings.TrimSpace(report.ReportSessionID) != strings.TrimSpace(activeSession) || report.SessionEpoch != activeEpoch || strings.TrimSpace(report.AgentLeaseID) != strings.TrimSpace(lease) {
			return nil, nil, errStaleAgentSession
		}
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
		// Full and lightweight reports share TelemetrySequence. A delayed
		// lightweight report must never overwrite a newer full report (or the
		// reverse), even though their channel-local sequences are independent.
		if exists && telemetrySequenceIsStale(previous, report.ReportSessionID, report.CounterEpoch, report.TelemetrySequence) {
			continue
		}
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
			reportSessionID:   report.ReportSessionID,
			counterEpoch:      report.CounterEpoch,
			sequence:          report.Sequence,
			telemetrySequence: report.TelemetrySequence,
			updatedAt:         now,
		}
		accepted = appendUniqueInt64(accepted, stat.SiteID)
	}
	d.agentLiveMu.Unlock()
	return accepted, discarded, nil
}

// recordNodeFullTelemetry publishes accepted full-report site counters into
// the same in-memory overlay used by /api/agent/live. The shared telemetry
// watermark prevents a delayed report from either channel from replacing a
// newer sample.
func (d *DB) recordNodeFullTelemetry(nodeID int64, report NodeReport, acceptedSiteIDs []int64, now time.Time) {
	if d == nil || nodeID <= 0 || report.TelemetrySequence <= 0 || len(acceptedSiteIDs) == 0 {
		return
	}
	allowed := make(map[int64]struct{}, len(acceptedSiteIDs))
	for _, siteID := range acceptedSiteIDs {
		if siteID > 0 {
			allowed[siteID] = struct{}{}
		}
	}
	d.agentLiveMu.Lock()
	defer d.agentLiveMu.Unlock()
	if d.agentLive == nil {
		d.agentLive = make(map[nodeLiveTrafficKey]nodeLiveTrafficState)
	}
	sessionID := strings.TrimSpace(report.ReportSessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(report.BootID)
	}
	counterEpoch := strings.TrimSpace(report.CounterEpoch)
	for _, stat := range report.SiteStats {
		if _, ok := allowed[stat.SiteID]; !ok {
			continue
		}
		host := requestPublicHost(stat.Host)
		if host == "" {
			continue
		}
		currentIn, currentOut := stat.CumulativeBytesIn, stat.CumulativeBytesOut
		if currentIn == 0 && stat.BytesIn > 0 {
			currentIn = stat.BytesIn
		}
		if currentOut == 0 && stat.BytesOut > 0 {
			currentOut = stat.BytesOut
		}
		key := nodeLiveTrafficKey{nodeID: nodeID, siteID: stat.SiteID}
		previous, exists := d.agentLive[key]
		if exists && telemetrySequenceIsStale(previous, sessionID, counterEpoch, report.TelemetrySequence) {
			continue
		}
		liveSequence := int64(0)
		if exists {
			liveSequence = previous.sequence
		}
		d.agentLive[key] = nodeLiveTrafficState{
			NodeLiveSiteTraffic: NodeLiveSiteTraffic{
				SiteID: stat.SiteID, Host: host,
				CumulativeBytesIn: currentIn, CumulativeBytesOut: currentOut,
				Requests: stat.RequestCount, SampledAtMS: now.UnixMilli(),
			},
			reportSessionID:   sessionID,
			counterEpoch:      counterEpoch,
			sequence:          liveSequence,
			telemetrySequence: report.TelemetrySequence,
			updatedAt:         now,
		}
	}
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
