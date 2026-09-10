package main

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

type dashboardTrendCacheEntry struct {
	ExpiresAt time.Time
	Response  *dashboardTrendsResponse
}

const maxDashboardTrendCacheEntries = 128

func (pm *ProxyManager) dashboardTrendCached(key string, now time.Time) *dashboardTrendsResponse {
	pm.trendCacheMu.Lock()
	defer pm.trendCacheMu.Unlock()
	entry, ok := pm.trendCache[key]
	if !ok || entry.Response == nil || !now.Before(entry.ExpiresAt) {
		if ok {
			delete(pm.trendCache, key)
		}
		return nil
	}
	return entry.Response
}

func (pm *ProxyManager) cacheDashboardTrend(key string, response *dashboardTrendsResponse, expiresAt time.Time) {
	pm.trendCacheMu.Lock()
	defer pm.trendCacheMu.Unlock()
	if pm.trendCache == nil {
		pm.trendCache = make(map[string]dashboardTrendCacheEntry)
	}
	now := time.Now()
	for cacheKey, entry := range pm.trendCache {
		if !now.Before(entry.ExpiresAt) {
			delete(pm.trendCache, cacheKey)
		}
	}
	if len(pm.trendCache) >= maxDashboardTrendCacheEntries {
		oldestKey := ""
		var oldestExpiry time.Time
		for cacheKey, entry := range pm.trendCache {
			if oldestKey == "" || entry.ExpiresAt.Before(oldestExpiry) {
				oldestKey, oldestExpiry = cacheKey, entry.ExpiresAt
			}
		}
		if oldestKey != "" {
			delete(pm.trendCache, oldestKey)
		}
	}
	pm.trendCache[key] = dashboardTrendCacheEntry{ExpiresAt: expiresAt, Response: response}
}

type dashboardTrendPoint struct {
	TimestampMS int64   `json:"timestamp_ms"`
	Traffic     int64   `json:"traffic_bytes"`
	BytesIn     int64   `json:"bytes_in"`
	BytesOut    int64   `json:"bytes_out"`
	Requests    int64   `json:"requests"`
	DownloadBPS float64 `json:"download_bps"`
	UploadBPS   float64 `json:"upload_bps"`
}

// dashboardTrendBaseline is the last cumulative Agent counter that is
// durably represented by node_site_traffic_logs. Realtime samples after this
// watermark can be converted to deltas without using the Controller wall
// clock as a proxy for persisted history.
type dashboardTrendBaseline struct {
	BytesIn     int64 `json:"bytes_in"`
	BytesOut    int64 `json:"bytes_out"`
	Requests    int64 `json:"requests"`
	SampledAtMS int64 `json:"sampled_at_ms"`
}

type dashboardTrendsResponse struct {
	SiteID         string `json:"site_id"`
	Range          string `json:"range"`
	BillingMode    string `json:"billing_mode"`
	TimezoneOffset int    `json:"timezone_offset_minutes"`
	StartMS        int64  `json:"start_ms"`
	EndMS          int64  `json:"end_ms"`
	// AsOfMS is the history snapshot cutoff. Realtime points newer than this
	// instant can be merged without double-counting the current bucket.
	AsOfMS int64 `json:"as_of_ms"`
	// Keep this field in every response, including an empty object. The
	// dashboard uses its presence to distinguish a request-time as_of_ms
	// fallback from a durable live baseline when merging the client tail.
	LiveBaselines map[int64]dashboardTrendBaseline `json:"live_baselines"`
	BucketSeconds int64                            `json:"bucket_seconds"`
	Points        []dashboardTrendPoint            `json:"points"`
	SiteSeries    []dashboardTrendSite             `json:"site_series"`
}

// dashboardTrendSite carries the same time buckets as the aggregate chart,
// allowing the hover card to explain which sites contributed to a point when
// the selector is set to “全部站点”.
type dashboardTrendSite struct {
	SiteID   int64                 `json:"site_id"`
	SiteName string                `json:"site_name"`
	Points   []dashboardTrendPoint `json:"points"`
}

type dashboardPendingTraffic struct {
	BytesIn  int64
	BytesOut int64
	Requests int64
}

func dashboardTrendWindowWithCustom(name string, now, customStart, customEnd time.Time) (string, time.Time, time.Time, time.Duration, error) {
	return dashboardTrendWindowWithLocation(name, now, customStart, customEnd, time.Local)
}

func dashboardTrendWindowWithLocation(name string, now, customStart, customEnd time.Time, location *time.Location) (string, time.Time, time.Time, time.Duration, error) {
	if location == nil {
		location = time.Local
	}
	var duration, bucket time.Duration
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "realtime":
		name, duration, bucket = "realtime", 30*time.Minute, time.Minute
	case "hour", "1h":
		name, duration, bucket = "hour", time.Hour, time.Minute
	case "6h", "6-hour":
		name, duration, bucket = "6h", 6*time.Hour, 5*time.Minute
	case "day", "24h":
		name, duration, bucket = "day", 24*time.Hour, time.Hour
	case "7d", "week":
		name, duration, bucket = "7d", 7*24*time.Hour, time.Hour
	case "month", "monthly":
		// “本月” means the natural calendar month: from the first day at
		// 00:00 through the current moment. It intentionally does not use the
		// configurable traffic billing reset day.
		name = "month"
		localNow := now.In(location)
		start := time.Date(localNow.Year(), localNow.Month(), 1, 0, 0, 0, 0, location)
		elapsed := localNow.Sub(start)
		if elapsed < time.Minute {
			elapsed = time.Minute
		}
		candidates := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour}
		bucket = 24 * time.Hour
		for _, candidate := range candidates {
			count := (elapsed + candidate - time.Nanosecond) / candidate
			if count <= 720 {
				bucket = candidate
				break
			}
		}
		count := (elapsed + bucket - time.Nanosecond) / bucket
		if count < 1 {
			count = 1
		}
		return name, start, start.Add(count * bucket), bucket, nil
	case "custom":
		name = "custom"
		start := customStart.In(location).Truncate(time.Minute)
		end := customEnd.In(location).Truncate(time.Minute)
		if start.IsZero() || end.IsZero() {
			return "", time.Time{}, time.Time{}, 0, errors.New("custom trend range requires start and end")
		}
		if !end.After(start) {
			return "", time.Time{}, time.Time{}, 0, errors.New("custom trend range end must be after start")
		}
		const maxCustomDuration = 366 * 24 * time.Hour
		if end.Sub(start) > maxCustomDuration {
			return "", time.Time{}, time.Time{}, 0, errors.New("custom trend range cannot exceed 366 days")
		}
		// Keep the response compact while preserving minute precision for short
		// windows. The largest range returns at most roughly 720 points.
		candidates := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour}
		duration = end.Sub(start)
		bucket = 24 * time.Hour
		for _, candidate := range candidates {
			if (duration+candidate-time.Nanosecond)/candidate <= 720 {
				bucket = candidate
				break
			}
		}
		return name, start, end, bucket, nil
	default:
		return "", time.Time{}, time.Time{}, 0, errors.New("invalid dashboard trend range")
	}
	localNow := now.In(location)
	seconds := int64(bucket / time.Second)
	bucketStart := time.Unix((localNow.Unix()/seconds)*seconds, 0).In(location)
	end := bucketStart.Add(bucket)
	start := end.Add(-duration)
	return name, start, end, bucket, nil
}

func (pm *ProxyManager) pendingDashboardTraffic(siteID *int64) map[int64]dashboardPendingTraffic {
	result, _, unlock := pm.lockLocalDashboardTrendSnapshot(siteID, time.Now())
	unlock()
	return result
}

// lockLocalDashboardTrendSnapshot pins every selected local proxy while the
// caller takes the SQLite history snapshot. Holding trafficMu across that
// read makes the in-memory cumulative/pending split and traffic_logs observe
// one consistent point in time; the returned unlock function must be called
// on every path.
func (pm *ProxyManager) lockLocalDashboardTrendSnapshot(siteID *int64, sampledAt time.Time) (map[int64]dashboardPendingTraffic, map[int64]dashboardTrendBaseline, func()) {
	pendingResult := make(map[int64]dashboardPendingTraffic)
	baselineResult := make(map[int64]dashboardTrendBaseline)
	if pm == nil {
		return pendingResult, baselineResult, func() {}
	}
	pm.mu.RLock()
	ids := make([]int64, 0, len(pm.proxies))
	instances := make(map[int64]*ProxyInstance)
	for id, inst := range pm.proxies {
		if siteID != nil && *siteID != id {
			continue
		}
		ids = append(ids, id)
		instances[id] = inst
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		inst := instances[id]
		inst.trafficMu.Lock()
		pendingIn := inst.trafficBytesIn().Load()
		pendingOut := inst.trafficBytesOut().Load()
		pendingRequests := inst.trafficPendingRequests().Load()
		baselineIn := inst.trafficCumulativeIn().Load() - pendingIn
		baselineOut := inst.trafficCumulativeOut().Load() - pendingOut
		baselineRequests := inst.trafficRequests().Load() - pendingRequests
		if baselineIn < 0 {
			baselineIn = 0
		}
		if baselineOut < 0 {
			baselineOut = 0
		}
		if baselineRequests < 0 {
			baselineRequests = 0
		}
		pendingResult[id] = dashboardPendingTraffic{
			BytesIn: pendingIn, BytesOut: pendingOut, Requests: pendingRequests,
		}
		baselineResult[id] = dashboardTrendBaseline{
			BytesIn: baselineIn, BytesOut: baselineOut, Requests: baselineRequests,
			SampledAtMS: sampledAt.UnixMilli(),
		}
	}
	return pendingResult, baselineResult, func() {
		for index := len(ids) - 1; index >= 0; index-- {
			instances[ids[index]].trafficMu.Unlock()
		}
		pm.mu.RUnlock()
	}
}

// localDashboardTrendBaselines returns the controller-local cumulative
// counters that are already represented by traffic_logs at the same snapshot
// as pendingDashboardTraffic. Agent baselines come from SQLite; local proxy
// baselines are derived by subtracting the unflushed counters from the
// process-stable cumulative counters.
func (pm *ProxyManager) localDashboardTrendBaselines(siteID *int64, sampledAt time.Time) map[int64]dashboardTrendBaseline {
	_, baselines, unlock := pm.lockLocalDashboardTrendSnapshot(siteID, sampledAt)
	unlock()
	return baselines
}

func dashboardTrendPoints(start, end time.Time, bucket time.Duration, rangeName string, billingMode string, logs []TrafficLog, pending dashboardPendingTraffic, now ...time.Time) []dashboardTrendPoint {
	count := int((end.Sub(start) + bucket - time.Nanosecond) / bucket)
	if count < 1 {
		count = 1
	}
	points := make([]dashboardTrendPoint, count)
	for i := range points {
		points[i].TimestampMS = start.Add(time.Duration(i) * bucket).UnixMilli()
	}
	bucketMS := bucket.Milliseconds()
	if bucketMS <= 0 {
		bucketMS = 1
	}
	for _, logRow := range logs {
		index := int((logRow.RecordedAtMS - start.UnixMilli()) / bucketMS)
		if index < 0 || index >= len(points) {
			continue
		}
		points[index].BytesIn += logRow.BytesIn
		points[index].BytesOut += logRow.BytesOut
		points[index].Requests += logRow.Requests
	}
	// Pending controller-local counters belong to the current wall-clock
	// bucket only. Historical windows must remain immutable, and a window that
	// ends in the future must not receive today's pending bytes at its final
	// (future) bucket.
	current := time.Now()
	if len(now) > 0 && !now[0].IsZero() {
		current = now[0]
	}
	if !current.Before(start) && current.Before(end) {
		index := int((current.UnixMilli() - start.UnixMilli()) / bucketMS)
		if index >= 0 && index < len(points) {
			points[index].BytesIn += pending.BytesIn
			points[index].BytesOut += pending.BytesOut
			points[index].Requests += pending.Requests
		}
	}
	for i := range points {
		points[i].Traffic = trafficBillableBytes(billingMode, points[i].BytesIn, points[i].BytesOut)
		seconds := bucket.Seconds()
		if rangeName == "custom" {
			bucketStart := start.Add(time.Duration(i) * bucket)
			bucketEnd := start.Add(time.Duration(i+1) * bucket)
			if bucketEnd.After(end) {
				seconds = end.Sub(bucketStart).Seconds()
			}
		}
		if seconds <= 0 {
			seconds = bucket.Seconds()
		}
		points[i].DownloadBPS = float64(points[i].BytesOut) / seconds
		points[i].UploadBPS = float64(points[i].BytesIn) / seconds
	}
	return points
}

func (pm *ProxyManager) dashboardTrends(siteID *int64, rangeName string, customWindow ...time.Time) (*dashboardTrendsResponse, error) {
	// Coalesce identical cold-cache requests. Dashboard tabs frequently mount
	// together, and the database is intentionally kept on a single writer
	// connection; only one caller should execute the trend query.
	customKey := ""
	for _, value := range customWindow {
		customKey += fmt.Sprintf("|%d", value.UnixNano())
	}
	siteKey := "all"
	if siteID != nil {
		siteKey = strconv.FormatInt(*siteID, 10)
	}
	settings := pm.database.currentSystemSettings()
	flightKey := fmt.Sprintf("%p|%s|%s|%s|%d%s", pm.database, siteKey, strings.ToLower(strings.TrimSpace(rangeName)), trafficBillingModeLabel(settings.TrafficBillingMode), settings.ScheduleTimezone, customKey)
	value, err, _ := pm.dashboardTrendGroup.Do(flightKey, func() (interface{}, error) {
		return pm.dashboardTrendsUncoalesced(siteID, rangeName, customWindow...)
	})
	if err != nil {
		return nil, err
	}
	return value.(*dashboardTrendsResponse), nil
}

func (pm *ProxyManager) dashboardTrendsUncoalesced(siteID *int64, rangeName string, customWindow ...time.Time) (*dashboardTrendsResponse, error) {
	settings := pm.database.currentSystemSettings()
	billingMode := settings.TrafficBillingMode
	trendLocation := timezoneLocation(settings.ScheduleTimezone)
	var customStart, customEnd time.Time
	if len(customWindow) > 0 {
		if len(customWindow) != 2 {
			return nil, errors.New("custom trend range requires start and end")
		}
		customStart, customEnd = customWindow[0], customWindow[1]
	}
	name, start, end, bucket, err := dashboardTrendWindowWithLocation(rangeName, time.Now(), customStart, customEnd, trendLocation)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	siteKey := "all"
	if siteID != nil {
		siteKey = strconv.FormatInt(*siteID, 10)
	}
	cacheKey := fmt.Sprintf("%p|%s|%s|%s|%d|%d|%d|%d", pm.database, siteKey, name, trafficBillingModeLabel(billingMode), start.UnixMilli(), end.UnixMilli(), bucket/time.Second, settings.ScheduleTimezone)
	cacheTTL := 30 * time.Second
	if strings.EqualFold(name, "realtime") {
		cacheTTL = 3 * time.Second
	}
	if cached := pm.dashboardTrendCached(cacheKey, now); cached != nil {
		return cached, nil
	}
	pendingBySite, localBaselines, unlockLocal := pm.lockLocalDashboardTrendSnapshot(siteID, now)
	defer unlockLocal()
	logs, liveBaselines, err := pm.database.GetTrafficTrendLogsGroupedSnapshot(siteID, start, end, bucket)
	if err != nil {
		return nil, err
	}
	sites, err := pm.database.ListSites()
	if err != nil {
		return nil, err
	}
	selectedSites := make([]Site, 0, len(sites))
	for _, site := range sites {
		if siteID == nil || site.ID == *siteID {
			selectedSites = append(selectedSites, site)
		}
	}
	logsBySite := make(map[int64][]TrafficLog)
	for _, logRow := range logs {
		logsBySite[logRow.SiteID] = append(logsBySite[logRow.SiteID], logRow)
	}
	for localSiteID, baseline := range localBaselines {
		if _, exists := liveBaselines[localSiteID]; !exists {
			liveBaselines[localSiteID] = baseline
		}
	}
	var aggregatePending dashboardPendingTraffic
	for _, value := range pendingBySite {
		aggregatePending.BytesIn += value.BytesIn
		aggregatePending.BytesOut += value.BytesOut
		aggregatePending.Requests += value.Requests
	}
	points := dashboardTrendPoints(start, end, bucket, name, billingMode, logs, aggregatePending, now)
	siteSeries := make([]dashboardTrendSite, 0, len(selectedSites))
	for _, site := range selectedSites {
		siteSeries = append(siteSeries, dashboardTrendSite{
			SiteID:   site.ID,
			SiteName: site.Name,
			Points:   dashboardTrendPoints(start, end, bucket, name, billingMode, logsBySite[site.ID], pendingBySite[site.ID], now),
		})
	}
	// AsOfMS is a real persisted-history watermark whenever every selected
	// site is backed by an Agent counter baseline. Mixed local/Agent views keep
	// the request-time fallback because no single timestamp can describe both
	// sources; their per-site baselines are still returned for exact merges.
	asOfMS := now.UnixMilli()
	if len(selectedSites) > 0 {
		complete := true
		earliest := int64(0)
		for _, site := range selectedSites {
			baseline, ok := liveBaselines[site.ID]
			if !ok || baseline.SampledAtMS <= 0 {
				complete = false
				break
			}
			if earliest == 0 || baseline.SampledAtMS < earliest {
				earliest = baseline.SampledAtMS
			}
		}
		if complete && earliest > 0 {
			asOfMS = earliest
		}
	}
	if siteID != nil {
		if baseline, ok := liveBaselines[*siteID]; ok && baseline.SampledAtMS > 0 {
			asOfMS = baseline.SampledAtMS
		}
	}
	response := &dashboardTrendsResponse{
		SiteID: func() string {
			if siteID == nil {
				return "all"
			}
			return strconv.FormatInt(*siteID, 10)
		}(),
		Range:          name,
		BillingMode:    trafficBillingModeLabel(billingMode),
		TimezoneOffset: settings.ScheduleTimezone,
		StartMS:        start.UnixMilli(),
		EndMS:          end.UnixMilli(),
		AsOfMS:         asOfMS,
		LiveBaselines:  liveBaselines,
		BucketSeconds:  int64(bucket / time.Second),
		Points:         points,
		SiteSeries:     siteSeries,
	}
	pm.cacheDashboardTrend(cacheKey, response, now.Add(cacheTTL))
	return response, nil
}

func parseDashboardTrendCustomTime(value string, location *time.Location) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("custom trend time is required")
	}
	if location == nil {
		location = time.Local
	}
	parsed, err := time.ParseInLocation("2006-01-02T15:04", value, location)
	if err != nil {
		return time.Time{}, errors.New("custom trend time must use YYYY-MM-DDTHH:MM")
	}
	return parsed, nil
}

func (a *App) handleDashboardTrends(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rangeName := r.URL.Query().Get("range")
	siteText := strings.TrimSpace(r.URL.Query().Get("site_id"))
	var siteID *int64
	if siteText != "" && !strings.EqualFold(siteText, "all") {
		id, err := strconv.ParseInt(siteText, 10, 64)
		if err != nil || id <= 0 {
			a.jsonErr(w, http.StatusBadRequest, "invalid site id")
			return
		}
		if _, err := a.db.GetSite(id); err != nil {
			a.jsonErr(w, http.StatusNotFound, "site not found")
			return
		}
		siteID = &id
	}
	var customWindow []time.Time
	if strings.EqualFold(strings.TrimSpace(rangeName), "custom") {
		location := timezoneLocation(a.db.currentSystemSettings().ScheduleTimezone)
		start, startErr := parseDashboardTrendCustomTime(r.URL.Query().Get("start"), location)
		end, endErr := parseDashboardTrendCustomTime(r.URL.Query().Get("end"), location)
		if startErr != nil || endErr != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid custom trend time; use YYYY-MM-DDTHH:MM")
			return
		}
		customWindow = []time.Time{start, end}
	}
	started := time.Now()
	trend, err := a.pm.dashboardTrends(siteID, rangeName, customWindow...)
	if err != nil {
		a.jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		logDashboardSlow("/api/dashboard-trends", elapsed)
	}
	w.Header().Set("Server-Timing", "trend;dur="+formatTimingDuration(time.Since(started)))
	a.jsonOK(w, trend)
}
