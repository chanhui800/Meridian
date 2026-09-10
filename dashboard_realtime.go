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
)

type dashboardRealtimeCounter struct {
	BytesIn     int64
	BytesOut    int64
	Requests    int64
	SampledAtMS int64
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
	defer pm.dashboardRealtimeMu.Unlock()
	if sampledAtMS <= pm.dashboardRealtimeLastMS {
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
	pm.dashboardRealtimePrev = next
	pm.dashboardRealtimeLastMS = sampledAtMS
	pm.dashboardRealtimePoints = appendDashboardRealtimePoint(pm.dashboardRealtimePoints, point)
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
			BytesIn:     maxNonNegativeInt64(site.CumulativeBytesIn),
			BytesOut:    maxNonNegativeInt64(site.CumulativeBytesOut),
			Requests:    maxNonNegativeInt64(site.Requests),
			SampledAtMS: sampledAtMS,
		}
		prior, exists := previous[site.ID]
		deltaIn, deltaOut, deltaRequests := int64(0), int64(0), int64(0)
		seconds := dashboardRealtimeServerInterval.Seconds()
		if exists {
			deltaIn = counterDelta(current.BytesIn, prior.BytesIn)
			deltaOut = counterDelta(current.BytesOut, prior.BytesOut)
			deltaRequests = counterDelta(current.Requests, prior.Requests)
			if elapsed := time.Duration(sampledAtMS-prior.SampledAtMS) * time.Millisecond; elapsed > 0 {
				seconds = elapsed.Seconds()
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
		// Keep a per-site traffic value so the frontend can render a selected
		// site from the same authoritative aggregate sample.
		sitePoint.Traffic = trafficBillableBytes(snapshot.BillingMode, deltaIn, deltaOut)
		point.SiteContributions[site.ID] = sitePoint
		point.BytesIn += deltaIn
		point.BytesOut += deltaOut
		point.Requests += deltaRequests
		next[site.ID] = current
	}
	point.Traffic = trafficBillableBytes(snapshot.BillingMode, point.BytesIn, point.BytesOut)
	point.DownloadBPS = float64(point.BytesOut) / dashboardRealtimeElapsedSeconds(sampledAtMS, previous)
	point.UploadBPS = float64(point.BytesIn) / dashboardRealtimeElapsedSeconds(sampledAtMS, previous)
	if len(point.SiteContributions) == 0 {
		point.SiteContributions = nil
	}
	return point, next
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
