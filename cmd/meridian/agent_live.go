package main

import (
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"time"
)

const (
	agentFullReportInterval   = 5 * time.Second
	agentLiveReportInterval   = 2 * time.Second
	agentLiveFallbackWindow   = 2 * agentLiveReportInterval
	maxNodeLiveSitesPerReport = maxNodeSiteStatsPerReport
)

type nodeLiveTrafficKey struct {
	nodeID int64
	siteID int64
}

type nodeLiveTrafficState struct {
	NodeLiveSiteTraffic
	reportSessionID      string
	counterEpoch         string
	sequence             int64
	fullSequence         int64
	telemetrySequence    int64
	rateFromLive         bool
	implicitCounterReset bool
	updatedAt            time.Time
	lastReportAt         time.Time
	liveGauge            nodeLiveGaugeIdentity
}

type nodeLiveGaugeIdentity struct {
	sampledAtMS       int64
	sequence          int64
	telemetrySequence int64
	counterEpoch      uint64
	counterDomain     uint64
}

var agentCounterDomainSequence atomic.Uint64

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
		invalidRate := stat.RateValid && (math.IsNaN(stat.DownloadBPS) || math.IsInf(stat.DownloadBPS, 0) || stat.DownloadBPS < 0 || math.IsNaN(stat.UploadBPS) || math.IsInf(stat.UploadBPS, 0) || stat.UploadBPS < 0)
		if stat.SiteID <= 0 || strings.TrimSpace(stat.Host) == "" || len(stat.Host) > 255 || stat.CumulativeBytesIn < 0 || stat.CumulativeBytesOut < 0 || stat.Requests < 0 || invalidRate {
			return errors.New("invalid live site traffic")
		}
	}
	return nil
}

func telemetrySequenceIsStale(previous nodeLiveTrafficState, sessionID string, sequence int64) bool {
	if sequence <= 0 || previous.telemetrySequence <= 0 {
		return false
	}
	// The shared sequence restarts with a new Agent process. Only compare it
	// inside the same process session. A NIC epoch can change without restarting
	// the process or its site counters, and cannot reset this shared sequence.
	return previous.reportSessionID == sessionID && sequence <= previous.telemetrySequence
}

func nodeTrafficReportDomainMatches(previous nodeLiveTrafficState, sessionID, counterEpoch string) bool {
	return previous.reportSessionID == sessionID && previous.counterEpoch == counterEpoch
}

// mergeNodeTrafficSample orders counters independently from the speed gauge.
// Full reports must advance the shared watermark even while a lightweight
// measurement still owns the gauge. Their database receive times cannot be
// used to decide which counter snapshot was captured last.
func mergeNodeTrafficSample(previous nodeLiveTrafficState, exists bool, sample NodeLiveSiteTraffic, sessionID, counterEpoch string, sequence, telemetrySequence int64, live bool, now time.Time) nodeLiveTrafficState {
	reportedCounterEpoch := sample.CounterEpoch
	sameSiteEpoch := sample.CounterEpoch > 0 && sample.CounterEpoch == previous.CounterEpoch
	sameReportDomain := exists && previous.reportSessionID == sessionID && (previous.counterEpoch == counterEpoch || sameSiteEpoch)
	if !sameReportDomain {
		previous = nodeLiveTrafficState{}
	}
	next := previous
	next.reportSessionID, next.counterEpoch = sessionID, counterEpoch
	next.lastReportAt = now
	if telemetrySequence > next.telemetrySequence {
		next.telemetrySequence = telemetrySequence
	}
	if live {
		next.sequence = sequence
	} else {
		next.fullSequence = sequence
	}

	epochChanged := sameReportDomain && previous.CounterEpoch > 0 && sample.CounterEpoch > 0 && previous.CounterEpoch != sample.CounterEpoch
	countersDecreased := sameReportDomain && (sample.CumulativeBytesIn < previous.CumulativeBytesIn || sample.CumulativeBytesOut < previous.CumulativeBytesOut || sample.Requests < previous.Requests)
	// A legacy live sample cannot name its new counter epoch. If its reset was
	// already observed, the next full report supplies that name rather than
	// starting a second reset and counting its initial bytes twice.
	promoteImplicitReset := epochChanged && previous.implicitCounterReset && !countersDecreased
	if sameReportDomain && sample.CounterEpoch > 0 && previous.CounterEpoch > sample.CounterEpoch && !previous.implicitCounterReset {
		return next
	}
	if countersDecreased && !epochChanged && sample.CounterEpoch > 0 && sample.CounterEpoch == previous.CounterEpoch {
		// Current Agents never reset counters without changing their epoch.
		// An older snapshot in the same named epoch must not look like a reset.
		return next
	}
	reset := !sameReportDomain || (epochChanged && !promoteImplicitReset) || countersDecreased
	if reset {
		next.CounterDomain = agentCounterDomainSequence.Add(1)
		next.DownloadBPS, next.UploadBPS, next.RateValid, next.RateReceivedAtMS = 0, 0, false, 0
		next.rateFromLive = false
		next.liveGauge = nodeLiveGaugeIdentity{}
		next.implicitCounterReset = sameReportDomain && countersDecreased && (sample.CounterEpoch == 0 || previous.CounterEpoch == 0)
	} else if promoteImplicitReset {
		next.implicitCounterReset = false
	}
	if sample.CounterEpoch == 0 && sameReportDomain {
		sample.CounterEpoch = previous.CounterEpoch
	}
	sample.CounterDomain = next.CounterDomain
	download, upload, rateValid, rateReceived := next.DownloadBPS, next.UploadBPS, next.RateValid, next.RateReceivedAtMS
	if live && sample.RateValid {
		download, upload, rateValid, rateReceived = sample.DownloadBPS, sample.UploadBPS, true, now.UnixMilli()
		next.rateFromLive = true
	} else {
		keepLiveRate := !live && !reset && previous.rateFromLive && previous.RateValid &&
			previous.RateReceivedAtMS > 0 &&
			now.UnixMilli()-previous.RateReceivedAtMS <= agentLiveFallbackWindow.Milliseconds()
		if !keepLiveRate {
			download, upload, rateValid, rateReceived = 0, 0, false, 0
			// Reports from older Agents lack explicit rates. Full-report
			// fallback rates use captured timestamps and the same counter domain.
			if !reset && sample.AgentSampledAtMS > previous.AgentSampledAtMS && previous.AgentSampledAtMS > 0 {
				seconds := float64(sample.AgentSampledAtMS-previous.AgentSampledAtMS) / 1000
				download = float64(sample.CumulativeBytesOut-previous.CumulativeBytesOut) / seconds
				upload = float64(sample.CumulativeBytesIn-previous.CumulativeBytesIn) / seconds
				rateValid = true
				rateReceived = now.UnixMilli()
			}
			next.rateFromLive = live
		}
	}
	sample.DownloadBPS, sample.UploadBPS, sample.RateValid, sample.RateReceivedAtMS = download, upload, rateValid, rateReceived
	next.NodeLiveSiteTraffic = sample
	next.updatedAt = now
	if live {
		next.liveGauge = nodeLiveGaugeIdentity{
			sampledAtMS: sample.AgentSampledAtMS, sequence: sequence, telemetrySequence: telemetrySequence,
			counterEpoch: reportedCounterEpoch, counterDomain: next.CounterDomain,
		}
	}
	return next
}

// A full report may capture newer counters before an earlier lightweight
// measurement is delivered. Gauge ordering follows the live channel separately,
// but only a measurement from the current site counter domain is eligible.
func mergeDelayedNodeLiveGauge(previous nodeLiveTrafficState, sample NodeLiveSiteTraffic, report NodeLiveReport, now time.Time) (nodeLiveTrafficState, bool) {
	if !sample.RateValid || previous.reportSessionID != report.ReportSessionID || previous.CounterDomain == 0 {
		return previous, false
	}
	sameNamedEpoch := sample.CounterEpoch > 0 && sample.CounterEpoch == previous.CounterEpoch
	sameLegacyEpoch := sample.CounterEpoch == 0 && previous.liveGauge.counterEpoch == 0 &&
		previous.liveGauge.counterDomain == previous.CounterDomain && previous.counterEpoch == report.CounterEpoch
	if !sameNamedEpoch && !sameLegacyEpoch {
		return previous, false
	}
	newer := report.TelemetrySequence > previous.liveGauge.telemetrySequence
	if report.TelemetrySequence <= 0 || previous.liveGauge.telemetrySequence <= 0 {
		newer = report.SampledAtMS > previous.liveGauge.sampledAtMS && report.Sequence > previous.liveGauge.sequence
	}
	if !newer {
		return previous, false
	}
	next := previous
	next.DownloadBPS, next.UploadBPS, next.RateValid = sample.DownloadBPS, sample.UploadBPS, true
	next.RateReceivedAtMS = now.UnixMilli()
	next.rateFromLive = true
	next.sequence = report.Sequence
	next.liveGauge = nodeLiveGaugeIdentity{
		sampledAtMS: report.SampledAtMS, sequence: report.Sequence, telemetrySequence: report.TelemetrySequence,
		counterEpoch: sample.CounterEpoch, counterDomain: next.CounterDomain,
	}
	return next, true
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
		lastReportAt := state.lastReportAt
		if lastReportAt.IsZero() {
			lastReportAt = state.updatedAt
		}
		if now.Sub(lastReportAt) > 2*nodeOnlineWindow {
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
		if exists && telemetrySequenceIsStale(previous, report.ReportSessionID, report.TelemetrySequence) {
			gauge, applied := mergeDelayedNodeLiveGauge(previous, stat, report, now)
			previous = gauge
			previous.lastReportAt = now
			d.agentLive[key] = previous
			if applied {
				accepted = appendUniqueInt64(accepted, stat.SiteID)
			}
			continue
		}
		if exists && report.TelemetrySequence <= 0 && nodeTrafficReportDomainMatches(previous, report.ReportSessionID, report.CounterEpoch) && report.Sequence <= previous.sequence {
			continue
		}
		sample := stat
		sample.Host = host
		sample.SampledAtMS, sample.AgentSampledAtMS, sample.ReceivedAtMS = serverSampledAt, report.SampledAtMS, serverSampledAt
		d.agentLive[key] = mergeNodeTrafficSample(previous, exists, sample, report.ReportSessionID, report.CounterEpoch, report.Sequence, report.TelemetrySequence, true, now)
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
	if d == nil || nodeID <= 0 || len(acceptedSiteIDs) == 0 {
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
	agentSampledAtMS := report.SampledAtMS
	if agentSampledAtMS <= 0 || agentSampledAtMS > now.Add(2*time.Minute).UnixMilli() {
		agentSampledAtMS = now.UnixMilli()
	}
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
		if exists && telemetrySequenceIsStale(previous, sessionID, report.TelemetrySequence) {
			previous.lastReportAt = now
			d.agentLive[key] = previous
			continue
		}
		if exists && report.TelemetrySequence <= 0 && report.Sequence > 0 && previous.fullSequence > 0 && nodeTrafficReportDomainMatches(previous, sessionID, counterEpoch) && report.Sequence <= previous.fullSequence {
			continue
		}
		sample := NodeLiveSiteTraffic{
			SiteID: stat.SiteID, Host: host, CounterEpoch: stat.CounterEpoch,
			CumulativeBytesIn: currentIn, CumulativeBytesOut: currentOut, Requests: stat.RequestCount,
			SampledAtMS: now.UnixMilli(), AgentSampledAtMS: agentSampledAtMS, ReceivedAtMS: now.UnixMilli(),
		}
		d.agentLive[key] = mergeNodeTrafficSample(previous, exists, sample, sessionID, counterEpoch, report.Sequence, report.TelemetrySequence, false, now)
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
		lastReportAt := state.lastReportAt
		if lastReportAt.IsZero() {
			lastReportAt = state.updatedAt
		}
		if lastReportAt.IsZero() || now.Sub(lastReportAt) > 2*nodeOnlineWindow {
			continue
		}
		// Keep both node and site in the key: during a scheduler transition two
		// nodes can report the same site, and a site-only map would choose one
		// nondeterministically.
		result[key] = state
	}
	return result
}
