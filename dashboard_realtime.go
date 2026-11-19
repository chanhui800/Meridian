package main

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"
)

var dashboardRealtimeEpochSequence atomic.Uint64

// The Controller owns one fixed-cadence realtime sequence. Browsers consume
// this sequence through SSE and the dashboard trend endpoint instead of
// sampling independently, so every browser sees the same five-minute tail.
const (
	dashboardRealtimeServerInterval  = 2 * time.Second
	dashboardRealtimeServerWindow    = 5 * time.Minute
	dashboardRealtimeServerMaxPoints = 150
	agentRealtimeRateHoldWindow      = 3 * dashboardRealtimeServerInterval
	agentRealtimeBackfillMaxPoints   = 8
)

type dashboardRealtimeCounter struct {
	BytesIn               int64
	BytesOut              int64
	Requests              int64
	SampledAtMS           int64
	AgentSampledAtMS      int64
	AgentReceivedAtMS     int64
	AgentRateReceivedAtMS int64
	AgentCounterDomain    uint64
	AgentNodeID           int64
	AgentReportSessionID  string
	AgentCounterEpoch     uint64
	AgentRuntime          bool
	AgentDownloadBPS      float64
	AgentUploadBPS        float64
	AgentRateValid        bool
	LastDownloadBPS       float64
	LastUploadBPS         float64
	LastRateValid         bool
	LastAgentAdvanceMS    int64
}

// startDashboardRealtimeSampler starts one process-wide sampler. It is
// intentionally independent from the number of connected SSE clients.
func (pm *ProxyManager) startDashboardRealtimeSampler(ctx context.Context) {
	if pm == nil || ctx == nil || !pm.dashboardRealtimeStarted.CompareAndSwap(false, true) {
		return
	}
	pm.captureDashboardRealtimeSample()
	go func() {
		ticker := time.NewTicker(dashboardRealtimeServerInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				pm.captureDashboardRealtimeSample()
			case <-ctx.Done():
				return
			}
		}
	}()
}

func (pm *ProxyManager) captureDashboardRealtimeSample() {
	if pm == nil {
		return
	}
	snapshot, err := pm.TrafficSnapshot()
	if err != nil {
		log.Printf("[dashboard-trend] realtime sample unavailable: %v", err)
		return
	}
	sampledAtMS := snapshot.GeneratedAtMS
	if sampledAtMS <= 0 {
		sampledAtMS = time.Now().UnixMilli()
	}

	pm.dashboardRealtimeMu.Lock()
	if sampledAtMS <= pm.dashboardRealtimeLastMS {
		pm.dashboardRealtimeMu.Unlock()
		return
	}
	if pm.dashboardRealtimePrev == nil {
		pm.dashboardRealtimePrev = make(map[int64]dashboardRealtimeCounter)
	}
	if pm.dashboardRealtimeEpoch == "" {
		pm.dashboardRealtimeEpoch = fmt.Sprintf("%x-%x", startTime.UnixNano(), dashboardRealtimeEpochSequence.Add(1))
	}
	if pm.dashboardRealtimeBillingMode != "" && pm.dashboardRealtimeBillingMode != snapshot.BillingMode {
		// Billing policy changes make old traffic_bytes incomparable with new
		// samples. Start a fresh visible window while keeping historical logs.
		pm.dashboardRealtimePoints = nil
		pm.dashboardRealtimePrev = make(map[int64]dashboardRealtimeCounter)
		pm.dashboardRealtimeEpoch = fmt.Sprintf("%x-%x", startTime.UnixNano(), dashboardRealtimeEpochSequence.Add(1))
		pm.dashboardRealtimeRevision = 0
		pm.trendCacheMu.Lock()
		pm.trendCache = nil
		pm.trendCacheMu.Unlock()
	}
	pm.dashboardRealtimeBillingMode = snapshot.BillingMode
	point, next := dashboardRealtimePointFromSnapshot(snapshot, pm.dashboardRealtimePrev, sampledAtMS)
	updates := backfillDashboardRealtimeAgentTraffic(pm.dashboardRealtimePoints, &point, snapshot.BillingMode, pm.dashboardRealtimePrev, next)
	baseRevision := pm.dashboardRealtimeRevision
	pm.dashboardRealtimeRevision++
	pm.dashboardRealtimePrev = next
	pm.dashboardRealtimeLastMS = sampledAtMS
	pm.dashboardRealtimePoints = appendDashboardRealtimePoint(pm.dashboardRealtimePoints, point)
	latest := cloneDashboardTrendPoint(point)

	// Publish one immutable snapshot after the trend point has been appended so
	// SSE, dashboard and trend consumers observe the same revision. Keep the
	// realtime lock until publication so a concurrent capture cannot publish an
	// older snapshot after the newer one.
	snapshot.RealtimeTrend = &latest
	snapshot.RealtimeEpoch = pm.dashboardRealtimeEpoch
	snapshot.RealtimeBaseRevision = baseRevision
	snapshot.RealtimeRevision = pm.dashboardRealtimeRevision
	snapshot.RealtimeUpdates = append(updates, cloneDashboardTrendPoint(point))
	pm.dashboardSnapshotMu.Lock()
	pm.dashboardSnapshot = cloneTrafficSnapshot(snapshot)
	pm.dashboardSnapshotMu.Unlock()
	pm.dashboardRealtimeMu.Unlock()
}

func cloneTrafficSnapshot(snapshot *TrafficSnapshot) *TrafficSnapshot {
	if snapshot == nil {
		return nil
	}
	cloned := *snapshot
	if snapshot.LiveSites != nil {
		cloned.LiveSites = append([]SiteTraffic(nil), snapshot.LiveSites...)
	}
	if snapshot.RealtimeTrend != nil {
		trend := cloneDashboardTrendPoint(*snapshot.RealtimeTrend)
		cloned.RealtimeTrend = &trend
	}
	cloned.RealtimeUpdates = cloneDashboardTrendPoints(snapshot.RealtimeUpdates)
	if snapshot.RealtimeTelemetry != nil {
		status := *snapshot.RealtimeTelemetry
		cloned.RealtimeTelemetry = &status
	}
	return &cloned
}

func (pm *ProxyManager) latestDashboardSnapshot() *TrafficSnapshot {
	if pm == nil {
		return nil
	}
	pm.dashboardSnapshotMu.RLock()
	defer pm.dashboardSnapshotMu.RUnlock()
	return cloneTrafficSnapshot(pm.dashboardSnapshot)
}

func dashboardRealtimePointFromSnapshot(snapshot *TrafficSnapshot, previous map[int64]dashboardRealtimeCounter, sampledAtMS int64) (dashboardTrendPoint, map[int64]dashboardRealtimeCounter) {
	point := dashboardTrendPoint{
		TimestampMS:       sampledAtMS,
		SiteContributions: make(map[int64]dashboardTrendPoint),
	}
	next := make(map[int64]dashboardRealtimeCounter)
	if snapshot == nil {
		point.SiteContributions = nil
		return point, next
	}
	for _, site := range snapshot.LiveSites {
		if site.ID <= 0 {
			continue
		}
		current := dashboardRealtimeCounter{
			BytesIn:               maxNonNegativeInt64(site.CumulativeBytesIn),
			BytesOut:              maxNonNegativeInt64(site.CumulativeBytesOut),
			Requests:              maxNonNegativeInt64(site.Requests),
			SampledAtMS:           sampledAtMS,
			AgentSampledAtMS:      site.AgentSampledAtMS,
			AgentReceivedAtMS:     site.AgentReceivedAtMS,
			AgentRateReceivedAtMS: site.AgentRateReceivedAtMS,
			AgentCounterDomain:    site.AgentCounterDomain,
			AgentNodeID:           site.AgentNodeID,
			AgentReportSessionID:  site.AgentReportSessionID,
			AgentCounterEpoch:     site.AgentCounterEpoch,
			AgentRuntime:          site.AgentRuntime,
			AgentDownloadBPS:      site.AgentDownloadBPS,
			AgentUploadBPS:        site.AgentUploadBPS,
			AgentRateValid:        site.AgentRateValid,
		}
		prior, exists := previous[site.ID]
		if exists && current.AgentRuntime && prior.AgentRuntime && current.AgentNodeID == prior.AgentNodeID &&
			current.AgentCounterDomain == 0 && prior.AgentCounterDomain > 0 {
			// A claimed session which has not reported yet, or an expired live
			// cache, can expose an older durable counter. It is unavailable data,
			// not evidence of a runtime reset. Retain the difference watermark
			// until an authoritative counter sample arrives.
			current.BytesIn, current.BytesOut, current.Requests = prior.BytesIn, prior.BytesOut, prior.Requests
			current.AgentCounterDomain = prior.AgentCounterDomain
			current.AgentReportSessionID, current.AgentCounterEpoch = prior.AgentReportSessionID, prior.AgentCounterEpoch
			current.AgentSampledAtMS = prior.AgentSampledAtMS
		}
		sourceChanged := exists && (current.AgentRuntime != prior.AgentRuntime ||
			current.AgentRuntime && current.AgentNodeID != prior.AgentNodeID)
		counterDomainChanged := !sourceChanged && exists && current.AgentRuntime && current.AgentCounterDomain > 0 &&
			prior.AgentCounterDomain > 0 && current.AgentCounterDomain != prior.AgentCounterDomain &&
			!dashboardAgentCounterIdentityMatches(prior, current)
		if current.AgentRuntime {
			if !exists || sourceChanged || counterDomainChanged || current.AgentSampledAtMS > prior.AgentSampledAtMS {
				current.LastAgentAdvanceMS = sampledAtMS
			} else {
				current.LastAgentAdvanceMS = prior.LastAgentAdvanceMS
			}
		}
		deltaIn, deltaOut, deltaRequests := int64(0), int64(0), int64(0)
		seconds := dashboardRealtimeServerInterval.Seconds()
		if exists {
			deltaIn = counterDelta(current.BytesIn, prior.BytesIn)
			deltaOut = counterDelta(current.BytesOut, prior.BytesOut)
			deltaRequests = counterDelta(current.Requests, prior.Requests)
			if counterDomainChanged {
				deltaIn, deltaOut, deltaRequests = current.BytesIn, current.BytesOut, current.Requests
			}
			if sourceChanged {
				// A replacement node may already have large runtime counters.
				// They establish its baseline rather than becoming this window's
				// traffic. A reset on the same node is handled separately above.
				deltaIn, deltaOut, deltaRequests = 0, 0, 0
			}
			if current.AgentRuntime && current.AgentSampledAtMS > prior.AgentSampledAtMS && prior.AgentSampledAtMS > 0 {
				if elapsed := time.Duration(current.AgentSampledAtMS-prior.AgentSampledAtMS) * time.Millisecond; elapsed > 0 {
					seconds = elapsed.Seconds()
				}
			} else if !current.AgentRuntime {
				if elapsed := time.Duration(sampledAtMS-prior.SampledAtMS) * time.Millisecond; elapsed > 0 {
					seconds = elapsed.Seconds()
				}
			}
		}
		sitePoint := dashboardTrendPoint{
			TimestampMS: sampledAtMS,
			BytesIn:     deltaIn,
			BytesOut:    deltaOut,
			Requests:    deltaRequests,
			DownloadBPS: float64(deltaOut) / seconds,
			UploadBPS:   float64(deltaIn) / seconds,
		}
		rateReceivedAtMS := current.AgentRateReceivedAtMS
		if rateReceivedAtMS <= 0 && current.AgentCounterDomain == 0 {
			// Compatibility for snapshots from older Agents/embedders which
			// did not separate gauge freshness from counter receive time.
			rateReceivedAtMS = current.AgentReceivedAtMS
		}
		remoteFresh := rateReceivedAtMS > 0 && sampledAtMS >= rateReceivedAtMS && sampledAtMS-rateReceivedAtMS <= agentRealtimeRateHoldWindow.Milliseconds()
		if current.AgentRuntime && remoteFresh && current.AgentRateValid {
			sitePoint.DownloadBPS = current.AgentDownloadBPS
			sitePoint.UploadBPS = current.AgentUploadBPS
		} else if exists && current.AgentRuntime && remoteFresh && current.AgentSampledAtMS <= prior.AgentSampledAtMS && deltaIn == 0 && deltaOut == 0 && prior.LastRateValid {
			// A delayed/repeated remote sample is not proof that traffic stopped.
			// Keep the last measured rate for this short controller tick; freshness
			// still clears it when the Agent truly goes stale.
			sitePoint.DownloadBPS = prior.LastDownloadBPS
			sitePoint.UploadBPS = prior.LastUploadBPS
		} else if current.AgentRuntime {
			sitePoint.SpeedUnavailable = true
			sitePoint.DownloadBPS = 0
			sitePoint.UploadBPS = 0
		}
		// Keep a per-site traffic value so the frontend can render a selected
		// site from the same authoritative aggregate sample.
		sitePoint.Traffic = trafficBillableBytes(snapshot.BillingMode, deltaIn, deltaOut)
		point.SiteContributions[site.ID] = sitePoint
		point.BytesIn += deltaIn
		point.BytesOut += deltaOut
		point.Requests += deltaRequests
		point.DownloadBPS += sitePoint.DownloadBPS
		point.UploadBPS += sitePoint.UploadBPS
		if sitePoint.SpeedUnavailable {
			point.SpeedUnavailable = true
		}
		current.LastDownloadBPS = sitePoint.DownloadBPS
		current.LastUploadBPS = sitePoint.UploadBPS
		current.LastRateValid = !sitePoint.SpeedUnavailable
		next[site.ID] = current
	}
	point.Traffic = trafficBillableBytes(snapshot.BillingMode, point.BytesIn, point.BytesOut)
	if len(point.SiteContributions) == 0 {
		point.SiteContributions = nil
	}
	return point, next
}

func dashboardAgentCounterIdentityMatches(previous, current dashboardRealtimeCounter) bool {
	if previous.AgentNodeID != current.AgentNodeID || previous.AgentReportSessionID == "" ||
		previous.AgentReportSessionID != current.AgentReportSessionID || previous.AgentCounterEpoch != current.AgentCounterEpoch {
		return false
	}
	if current.AgentCounterEpoch > 0 {
		return true
	}
	// Legacy samples cannot name a site epoch. A re-created cache with
	// monotonic counters in the same session retains its existing baseline.
	return current.BytesIn >= previous.BytesIn && current.BytesOut >= previous.BytesOut && current.Requests >= previous.Requests
}

// backfillDashboardRealtimeAgentTraffic redistributes a short Agent reporting
// gap over the controller buckets it actually covered. The cumulative counter
// remains the source of truth, and integer remainders are retained exactly, so
// the visible bucket sum always equals the accepted counter delta.
func backfillDashboardRealtimeAgentTraffic(points []dashboardTrendPoint, current *dashboardTrendPoint, billingMode string, previous, next map[int64]dashboardRealtimeCounter) []dashboardTrendPoint {
	if current == nil || len(points) == 0 {
		return nil
	}
	changed := make(map[int]bool)
	for siteID, nextCounter := range next {
		prior, ok := previous[siteID]
		if !ok || !prior.AgentRuntime || !nextCounter.AgentRuntime || prior.AgentNodeID != nextCounter.AgentNodeID ||
			nextCounter.AgentSampledAtMS <= prior.AgentSampledAtMS || prior.LastAgentAdvanceMS <= 0 ||
			current.TimestampMS-prior.LastAgentAdvanceMS > int64(agentRealtimeBackfillMaxPoints)*dashboardRealtimeServerInterval.Milliseconds() ||
			(prior.AgentCounterDomain > 0 && nextCounter.AgentCounterDomain > 0 && prior.AgentCounterDomain != nextCounter.AgentCounterDomain &&
				!dashboardAgentCounterIdentityMatches(prior, nextCounter)) {
			continue
		}
		currentSite, ok := current.SiteContributions[siteID]
		if !ok || (currentSite.BytesIn == 0 && currentSite.BytesOut == 0) {
			continue
		}
		type targetPoint struct {
			point *dashboardTrendPoint
			index int
		}
		targets := make([]targetPoint, 0, agentRealtimeBackfillMaxPoints)
		for index := range points {
			if points[index].TimestampMS <= prior.LastAgentAdvanceMS {
				continue
			}
			if contribution, exists := points[index].SiteContributions[siteID]; exists && contribution.BytesIn == 0 && contribution.BytesOut == 0 {
				targets = append(targets, targetPoint{point: &points[index], index: index})
			}
		}
		targets = append(targets, targetPoint{point: current, index: -1})
		if len(targets) <= 1 || len(targets) > agentRealtimeBackfillMaxPoints {
			continue
		}
		for index, target := range targets {
			bytesIn := distributedCounterPart(currentSite.BytesIn, len(targets), index)
			bytesOut := distributedCounterPart(currentSite.BytesOut, len(targets), index)
			setDashboardRealtimeSiteTraffic(target.point, siteID, billingMode, bytesIn, bytesOut)
			if target.index >= 0 && (bytesIn > 0 || bytesOut > 0) {
				changed[target.index] = true
			}
		}
	}
	updates := make([]dashboardTrendPoint, 0, len(changed))
	for index := range points {
		if changed[index] {
			updates = append(updates, cloneDashboardTrendPoint(points[index]))
		}
	}
	return updates
}

func distributedCounterPart(total int64, count, index int) int64 {
	if total <= 0 || count <= 0 || index < 0 || index >= count {
		return 0
	}
	value := total / int64(count)
	if int64(index) < total%int64(count) {
		value++
	}
	return value
}

func setDashboardRealtimeSiteTraffic(point *dashboardTrendPoint, siteID int64, billingMode string, bytesIn, bytesOut int64) {
	if point == nil || point.SiteContributions == nil {
		return
	}
	contribution, ok := point.SiteContributions[siteID]
	if !ok {
		return
	}
	point.BytesIn += bytesIn - contribution.BytesIn
	point.BytesOut += bytesOut - contribution.BytesOut
	contribution.BytesIn = bytesIn
	contribution.BytesOut = bytesOut
	contribution.Traffic = trafficBillableBytes(billingMode, bytesIn, bytesOut)
	point.SiteContributions[siteID] = contribution
	point.Traffic = trafficBillableBytes(billingMode, point.BytesIn, point.BytesOut)
}

func dashboardRealtimeElapsedSeconds(sampledAtMS int64, previous map[int64]dashboardRealtimeCounter) float64 {
	if len(previous) > 0 {
		oldest := int64(0)
		for _, counter := range previous {
			if counter.SampledAtMS > 0 && (oldest == 0 || counter.SampledAtMS < oldest) {
				oldest = counter.SampledAtMS
			}
		}
		if oldest > 0 && sampledAtMS > oldest {
			return float64(sampledAtMS-oldest) / 1000
		}
	}
	return dashboardRealtimeServerInterval.Seconds()
}

func counterDelta(current, previous int64) int64 {
	if current >= previous {
		return current - previous
	}
	return current
}

func maxNonNegativeInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func appendDashboardRealtimePoint(points []dashboardTrendPoint, point dashboardTrendPoint) []dashboardTrendPoint {
	point = cloneDashboardTrendPoint(point)
	if len(points) > 0 && points[len(points)-1].TimestampMS == point.TimestampMS {
		points[len(points)-1] = point
	} else {
		points = append(points, point)
	}
	cutoff := point.TimestampMS - dashboardRealtimeServerWindow.Milliseconds()
	first := 0
	for first < len(points) && points[first].TimestampMS < cutoff {
		first++
	}
	if first > 0 {
		points = append([]dashboardTrendPoint(nil), points[first:]...)
	}
	if len(points) > dashboardRealtimeServerMaxPoints {
		points = append([]dashboardTrendPoint(nil), points[len(points)-dashboardRealtimeServerMaxPoints:]...)
	}
	return points
}

func (pm *ProxyManager) dashboardRealtimeTrendLatest() *dashboardTrendPoint {
	if pm == nil {
		return nil
	}
	pm.dashboardRealtimeMu.RLock()
	defer pm.dashboardRealtimeMu.RUnlock()
	if len(pm.dashboardRealtimePoints) == 0 {
		return nil
	}
	point := cloneDashboardTrendPoint(pm.dashboardRealtimePoints[len(pm.dashboardRealtimePoints)-1])
	return &point
}

func (pm *ProxyManager) dashboardRealtimeTrendPoints(siteID *int64) []dashboardTrendPoint {
	return pm.dashboardRealtimeWindowSnapshot(siteID).Points
}

type dashboardRealtimeWindow struct {
	Epoch       string
	Revision    uint64
	BillingMode string
	Points      []dashboardTrendPoint
}

// Metadata and the complete window are cloned under the same lock. Cached
// historical responses may reuse their history, but never an older realtime
// revision or a window calculated under a different billing policy.
func (pm *ProxyManager) dashboardRealtimeWindowSnapshot(siteID *int64) dashboardRealtimeWindow {
	if pm == nil {
		return dashboardRealtimeWindow{}
	}
	pm.dashboardRealtimeMu.RLock()
	defer pm.dashboardRealtimeMu.RUnlock()
	window := dashboardRealtimeWindow{Epoch: pm.dashboardRealtimeEpoch, Revision: pm.dashboardRealtimeRevision, BillingMode: pm.dashboardRealtimeBillingMode}
	if siteID == nil {
		window.Points = cloneDashboardTrendPoints(pm.dashboardRealtimePoints)
		return window
	}
	points := make([]dashboardTrendPoint, 0, len(pm.dashboardRealtimePoints))
	for _, aggregate := range pm.dashboardRealtimePoints {
		if site, ok := aggregate.SiteContributions[*siteID]; ok {
			site.TimestampMS = aggregate.TimestampMS
			points = append(points, cloneDashboardTrendPoint(site))
		}
	}
	window.Points = points
	return window
}

func (pm *ProxyManager) dashboardRealtimePacketSnapshot() *TrafficSnapshot {
	if pm == nil {
		return nil
	}
	pm.dashboardSnapshotMu.RLock()
	defer pm.dashboardSnapshotMu.RUnlock()
	if pm.dashboardSnapshot == nil {
		return nil
	}
	packet := &TrafficSnapshot{
		RealtimeTrend:        pm.dashboardSnapshot.RealtimeTrend,
		RealtimeEpoch:        pm.dashboardSnapshot.RealtimeEpoch,
		RealtimeRevision:     pm.dashboardSnapshot.RealtimeRevision,
		RealtimeBaseRevision: pm.dashboardSnapshot.RealtimeBaseRevision,
		RealtimeUpdates:      pm.dashboardSnapshot.RealtimeUpdates,
	}
	return cloneTrafficSnapshot(packet)
}

func cloneDashboardTrendPoints(points []dashboardTrendPoint) []dashboardTrendPoint {
	if len(points) == 0 {
		return nil
	}
	cloned := make([]dashboardTrendPoint, len(points))
	for i, point := range points {
		cloned[i] = cloneDashboardTrendPoint(point)
	}
	return cloned
}

func cloneDashboardTrendPoint(point dashboardTrendPoint) dashboardTrendPoint {
	if len(point.SiteContributions) == 0 {
		point.SiteContributions = nil
		return point
	}
	contributions := make(map[int64]dashboardTrendPoint, len(point.SiteContributions))
	for siteID, sitePoint := range point.SiteContributions {
		contributions[siteID] = cloneDashboardTrendPoint(sitePoint)
	}
	point.SiteContributions = contributions
	return point
}
