package main

import (
	"fmt"
	"log"
	"net/http"
	"time"
)

func formatTimingDuration(duration time.Duration) string {
	return fmt.Sprintf("%.1f", float64(duration.Microseconds())/1000)
}

func logDashboardSlow(path string, duration time.Duration) {
	log.Printf("[dashboard] slow request path=%s duration=%s", path, duration.Round(time.Millisecond))
}

// dashboardSiteView is the intentionally small site representation used by
// the dashboard. Management-only fields (credentials, upstream headers and
// discovery rules) never need to cross the dashboard API boundary.
type dashboardSiteView struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	TargetURL         string `json:"target_url"`
	UAMode            string `json:"ua_mode"`
	PublicHost        string `json:"public_host"`
	PathPrefix        string `json:"path_prefix"`
	IngressMode       string `json:"ingress_mode"`
	ListenPort        int    `json:"listen_port"`
	Running           bool   `json:"running"`
	TrafficUsed       int64  `json:"traffic_used"`
	MonthlyTraffic    int64  `json:"monthly_traffic"`
	CacheSizeBytes    int64  `json:"cache_size_bytes"`
	MediaMovieCount   int64  `json:"media_movie_count"`
	MediaSeriesCount  int64  `json:"media_series_count"`
	MediaEpisodeCount int64  `json:"media_episode_count"`
}

type dashboardBootstrapResponse struct {
	Snapshot      *TrafficSnapshot    `json:"snapshot"`
	Sites         []dashboardSiteView `json:"sites"`
	Insights      dashboardInsights   `json:"insights"`
	GeneratedAtMS int64               `json:"generated_at_ms"`
}

func (a *App) dashboardBootstrap() (*dashboardBootstrapResponse, error) {
	started := time.Now()
	now := started
	settings := a.db.currentSystemSettings()
	sites, err := a.db.ListSites()
	if err != nil {
		return nil, err
	}
	snapshot, err := a.pm.dashboardSnapshotWithSites(sites, settings, now)
	if err != nil {
		return nil, err
	}
	snapshot.PanelDomain = a.panelHost
	snapshot.PanelAccessURL = a.panelAccessURL()
	cacheSizes := map[int64]int64{}
	if sizes, _, cacheErr := a.pm.AssetCacheSizes(); cacheErr != nil {
		// Cache statistics are supplementary dashboard data. A transient
		// filesystem error must not hide the authoritative site/traffic snapshot.
		log.Printf("[dashboard] cache statistics unavailable: %v", cacheErr)
	} else {
		cacheSizes = sizes
	}
	liveBySite := make(map[int64]SiteTraffic, len(snapshot.LiveSites))
	for _, live := range snapshot.LiveSites {
		liveBySite[live.ID] = live
	}
	views := make([]dashboardSiteView, 0, len(sites))
	for _, site := range sites {
		live := liveBySite[site.ID]
		cacheSize := cacheSizes[site.ID]
		if live.AgentRuntime {
			cacheSize = live.CacheSizeBytes
		}
		views = append(views, dashboardSiteView{
			ID: site.ID, Name: site.Name, TargetURL: site.TargetURL, UAMode: site.UAMode,
			PublicHost: site.PublicHost, PathPrefix: site.PathPrefix, IngressMode: site.IngressMode,
			ListenPort: site.ListenPort, Running: live.Running, TrafficUsed: live.TrafficUsed,
			MonthlyTraffic: live.MonthlyTraffic, CacheSizeBytes: cacheSize,
			MediaMovieCount: site.MediaMovieCount, MediaSeriesCount: site.MediaSeriesCount,
			MediaEpisodeCount: site.MediaEpisodeCount,
		})
	}
	response := &dashboardBootstrapResponse{
		Snapshot:      snapshot,
		Sites:         views,
		Insights:      a.dashboardInsightsSnapshot(),
		GeneratedAtMS: snapshot.GeneratedAtMS,
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		// Keep this diagnostic intentionally free of site names, URLs and
		// credentials so slow-path logs cannot become a data-exfiltration vector.
		logDashboardSlow("/api/dashboard/bootstrap", elapsed)
	}
	return response, nil
}

func (a *App) handleDashboardBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	started := time.Now()
	response, err := a.dashboardBootstrap()
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "dashboard bootstrap unavailable")
		return
	}
	w.Header().Set("Server-Timing", "dashboard;dur="+formatTimingDuration(time.Since(started)))
	a.jsonOK(w, response)
}
