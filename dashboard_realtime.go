package main

import (
	"context"
	"log"
	"time"
)

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
	BytesIn            int64
	BytesOut           int64
	Requests           int64
	SampledAtMS        int64
	AgentSampledAtMS   int64
	AgentReceivedAtMS  int64
	AgentRuntime       bool
	AgentDownloadBPS   float64
	AgentUploadBPS     float64
	AgentRateValid     bool
	LastDownloadBPS    float64
	LastUploadBPS      float64
	LastRateValid      bool
	LastAgentAdvanceMS int64
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
	if pm.dashboardRealtimeBillingMode != "" && pm.dashboardRealtimeBillingMode != snapshot.BillingMode {
		// Billing policy changes make old traffic_bytes incomparable with new
		// samples. Start a fresh visible window while keeping historical logs.
		pm.dashboardRealtimePoints = nil
		pm.dashboardRealtimePrev = make(map[int64]dashboardRealtimeCounter)
	}
	pm.dashboardRealtimeBillingMode = snapshot.BillingMode
	point, next := dashboardRealtimePointFromSnapshot(snapshot, pm.dashboardRealtimePrev, sampledAtMS)
	backfillDashboardRealtimeAgentTraffic(pm.dashboardRealtimePoints, &point, snapshot.BillingMode, pm.dashboardRealtimePrev, next)
	pm.dashboardRealtimePrev = next
	pm.dashboardRealtimeLastMS = sampledAtMS
	pm.dashboardRealtimePoints = appendDashboardRealtimePoint(pm.dashboardRealtimePoints, point)
	latest := cloneDashboardTrendPoint(point)
	pm.dashboardRealtimeMu.Unlock()

	// Publish one immutable snapshot after the trend point has been appended so
	// SSE, dashboard and trend consumers observe the same sample sequence.
	snapshot.RealtimeTrend = &latest
	pm.dashboardSnapshotMu.Lock()
	pm.dashboardSnapshot = cloneTrafficSnapshot(snapshot)
	pm.dashboardSnapshotMu.Unlock()
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
			BytesIn:           maxNonNegativeInt64(site.CumulativeBytesIn),
			BytesOut:          maxNonNegativeInt64(site.CumulativeBytesOut),
			Requests:          maxNonNegativeInt64(site.Requests),
			SampledAtMS:       sampledAtMS,
			AgentSampledAtMS:  site.AgentSampledAtMS,
			AgentReceivedAtMS: site.AgentReceivedAtMS,
			AgentRuntime:      site.AgentRuntime,
			AgentDownloadBPS:  site.AgentDownloadBPS,
			AgentUploadBPS:    site.AgentUploadBPS,
			AgentRateValid:    site.AgentRateValid,
		}
		prior, exists := previous[site.ID]
		if current.AgentRuntime {
			if !exists || current.AgentSampledAtMS > prior.AgentSampledAtMS {
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
		remoteFresh := current.AgentReceivedAtMS > 0 && sampledAtMS >= current.AgentReceivedAtMS && sampledAtMS-current.AgentReceivedAtMS <= agentRealtimeRateHoldWindow.Milliseconds()
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

// backfillDashboardRealtimeAgentTraffic redistributes a short Agent reporting
// gap over the controller buckets it actually covered. The cumulative counter
// remains the source of truth, and integer remainders are retained exactly, so
// the visible bucket sum always equals the accepted counter delta.
func backfillDashboardRealtimeAgentTraffic(points []dashboardTrendPoint, current *dashboardTrendPoint, billingMode string, previous, next map[int64]dashboardRealtimeCounter) {
	if current == nil || len(points) == 0 {
		return
	}
	for siteID, nextCounter := range next {
		prior, ok := previous[siteID]
		if !ok || !nextCounter.AgentRuntime || nextCounter.AgentSampledAtMS <= prior.AgentSampledAtMS || prior.LastAgentAdvanceMS <= 0 {
			continue
		}
		currentSite, ok := current.SiteContributions[siteID]
		if !ok || (currentSite.BytesIn == 0 && currentSite.BytesOut == 0) {
			continue
		}
		targets := make([]*dashboardTrendPoint, 0, agentRealtimeBackfillMaxPoints)
		for index := range points {
			if points[index].TimestampMS <= prior.LastAgentAdvanceMS {
				continue
			}
			if _, exists := points[index].SiteContributions[siteID]; exists {
				targets = append(targets, &points[index])
			}
		}
		targets = append(targets, current)
		if len(targets) <= 1 || len(targets) > agentRealtimeBackfillMaxPoints {
			continue
		}
		for index, target := range targets {
			bytesIn := distributedCounterPart(currentSite.BytesIn, len(targets), index)
			bytesOut := distributedCounterPart(currentSite.BytesOut, len(targets), index)
			setDashboardRealtimeSiteTraffic(target, siteID, billingMode, bytesIn, bytesOut)
		}
	}
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
	if pm == nil {
		return nil
	}
	pm.dashboardRealtimeMu.RLock()
	defer pm.dashboardRealtimeMu.RUnlock()
	if siteID == nil {
		return cloneDashboardTrendPoints(pm.dashboardRealtimePoints)
	}
	points := make([]dashboardTrendPoint, 0, len(pm.dashboardRealtimePoints))
	for _, aggregate := range pm.dashboardRealtimePoints {
		if site, ok := aggregate.SiteContributions[*siteID]; ok {
			site.TimestampMS = aggregate.TimestampMS
			points = append(points, cloneDashboardTrendPoint(site))
		}
	}
	return points
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
