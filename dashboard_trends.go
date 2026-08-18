package main

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type dashboardTrendPoint struct {
	TimestampMS int64   `json:"timestamp_ms"`
	Traffic     int64   `json:"traffic_bytes"`
	BytesIn     int64   `json:"bytes_in"`
	BytesOut    int64   `json:"bytes_out"`
	Requests    int64   `json:"requests"`
	DownloadBPS float64 `json:"download_bps"`
	UploadBPS   float64 `json:"upload_bps"`
}

type dashboardTrendsResponse struct {
	SiteID        string                `json:"site_id"`
	Range         string                `json:"range"`
	BillingMode   string                `json:"billing_mode"`
	BucketSeconds int64                 `json:"bucket_seconds"`
	Points        []dashboardTrendPoint `json:"points"`
}

type dashboardPendingTraffic struct {
	BytesIn  int64
	BytesOut int64
	Requests int64
}

func dashboardTrendWindow(name string, now time.Time) (string, time.Time, time.Time, time.Duration, error) {
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
	default:
		return "", time.Time{}, time.Time{}, 0, errors.New("invalid dashboard trend range")
	}
	localNow := now.In(time.Local)
	seconds := int64(bucket / time.Second)
	bucketStart := time.Unix((localNow.Unix()/seconds)*seconds, 0).In(time.Local)
	end := bucketStart.Add(bucket)
	start := end.Add(-duration)
	return name, start, end, bucket, nil
}

func (pm *ProxyManager) pendingDashboardTraffic(siteID *int64) map[int64]dashboardPendingTraffic {
	result := make(map[int64]dashboardPendingTraffic)
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	for id, inst := range pm.proxies {
		if siteID != nil && *siteID != id {
			continue
		}
		inst.trafficMu.Lock()
		result[id] = dashboardPendingTraffic{
			BytesIn:  inst.bytesIn.Load(),
			BytesOut: inst.bytesOut.Load(),
			Requests: inst.pendingRequests.Load(),
		}
		inst.trafficMu.Unlock()
	}
	return result
}

func (pm *ProxyManager) dashboardTrends(siteID *int64, rangeName string) (*dashboardTrendsResponse, error) {
	billingMode := pm.database.currentSystemSettings().TrafficBillingMode
	name, start, end, bucket, err := dashboardTrendWindow(rangeName, time.Now())
	if err != nil {
		return nil, err
	}
	logs, err := pm.database.GetTrafficTrendLogs(siteID, start, end)
	if err != nil {
		return nil, err
	}
	count := int(end.Sub(start) / bucket)
	points := make([]dashboardTrendPoint, count)
	for i := range points {
		points[i].TimestampMS = start.Add(time.Duration(i) * bucket).UnixMilli()
	}
	bucketMS := bucket.Milliseconds()
	for _, logRow := range logs {
		index := int((logRow.RecordedAtMS - start.UnixMilli()) / bucketMS)
		if index < 0 || index >= len(points) {
			continue
		}
		points[index].BytesIn += logRow.BytesIn
		points[index].BytesOut += logRow.BytesOut
		points[index].Requests += logRow.Requests
	}
	// Include traffic that is still in memory and will be flushed into the
	// current bucket later. This keeps the realtime chart responsive without
	// forcing a database write from a read-only dashboard request.
	if pending := pm.pendingDashboardTraffic(siteID); len(pending) > 0 && len(points) > 0 {
		last := &points[len(points)-1]
		for _, value := range pending {
			last.BytesIn += value.BytesIn
			last.BytesOut += value.BytesOut
			last.Requests += value.Requests
		}
	}
	for i := range points {
		points[i].Traffic = trafficBillableBytes(billingMode, points[i].BytesIn, points[i].BytesOut)
		seconds := bucket.Seconds()
		points[i].DownloadBPS = float64(points[i].BytesOut) / seconds
		points[i].UploadBPS = float64(points[i].BytesIn) / seconds
	}
	return &dashboardTrendsResponse{
		SiteID: func() string {
			if siteID == nil {
				return "all"
			}
			return strconv.FormatInt(*siteID, 10)
		}(),
		Range:         name,
		BillingMode:   trafficBillingModeLabel(billingMode),
		BucketSeconds: int64(bucket / time.Second),
		Points:        points,
	}, nil
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
	trend, err := a.pm.dashboardTrends(siteID, rangeName)
	if err != nil {
		a.jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.jsonOK(w, trend)
}
