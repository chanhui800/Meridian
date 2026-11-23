package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleSitesFlushesPendingMediaCounts(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSiteRecord(Site{
		Name:         "queued-library-counts",
		ListenPort:   freePort(t),
		IngressMode:  ingressModePort,
		TargetURL:    "http://127.0.0.1:8096",
		PlaybackMode: "direct",
		StreamHosts:  "[]",
		UAMode:       passthroughUAMode,
	})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	if ok := app.db.EnqueueMediaLibraryCounts(mediaLibraryCountEvent{
		SiteID: site.ID, MovieCount: 12, SeriesCount: 34, EpisodeCount: 56, ObservedAtMS: 1_000,
	}); !ok {
		t.Fatal("enqueue pending media counts")
	}

	rr := httptest.NewRecorder()
	app.handleSites(rr, httptest.NewRequest(http.MethodGet, "/api/sites", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rr.Code, rr.Body.String())
	}
	var sites []struct {
		ID                int64 `json:"id"`
		MediaMovieCount   int64 `json:"media_movie_count"`
		MediaSeriesCount  int64 `json:"media_series_count"`
		MediaEpisodeCount int64 `json:"media_episode_count"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &sites); err != nil {
		t.Fatalf("decode sites: %v", err)
	}
	if len(sites) != 1 || sites[0].ID != site.ID || sites[0].MediaMovieCount != 12 || sites[0].MediaSeriesCount != 34 || sites[0].MediaEpisodeCount != 56 {
		t.Fatalf("site response did not include flushed library counts: %+v", sites)
	}
}
