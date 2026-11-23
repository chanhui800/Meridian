package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestPathTargetHistorySettingSurvivesAPICreateEditAndRuntime(t *testing.T) {
	app := newTestApp(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/emby/Sessions/Playing/Progress" || request.Method != http.MethodPost {
			http.NotFound(w, request)
			return
		}
		_, _ = io.Copy(io.Discard, request.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(upstream.Close)

	create := httptest.NewRecorder()
	app.handleSites(create, httptest.NewRequest(http.MethodPost, "/api/sites", bytes.NewBufferString(`{
		"name":"generic history features","public_host":"generic-history.example.test",
		"ingress_mode":"host","upstream_mode":"path_target","watch_history_enabled":true
	}`)))
	if create.Code != http.StatusCreated {
		t.Fatalf("create generic history site status=%d body=%s", create.Code, create.Body.String())
	}
	var site Site
	if err := json.Unmarshal(create.Body.Bytes(), &site); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	if !site.WatchHistoryEnabled || !siteUsesPathTarget(site) || site.TargetURL != "" {
		t.Fatalf("create lost generic history configuration: watch=%t mode=%q target_empty=%t", site.WatchHistoryEnabled, site.UpstreamMode, site.TargetURL == "")
	}

	update := func(history *bool) {
		t.Helper()
		payload := map[string]any{"name": "generic history edited", "upstream_mode": "path_target"}
		if history != nil {
			payload["watch_history_enabled"] = *history
		}
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		app.handleSiteByID(recorder, httptest.NewRequest(http.MethodPut, "/api/sites/"+strconv.FormatInt(site.ID, 10), bytes.NewReader(body)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("update history setting status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
	play := func(session string) {
		t.Helper()
		handler, available := app.pm.PublicHostHandler(site.PublicHost)
		if !available {
			t.Fatal("generic history host route unavailable")
		}
		payload, _ := json.Marshal(map[string]any{
			"ItemId": "generic-item", "PlaySessionId": session, "PositionTicks": 100, "RunTimeTicks": 1000,
			"Item": map[string]any{"Id": "generic-item", "Name": "Generic Movie", "Type": "Movie"},
		})
		request := httptest.NewRequest(http.MethodPost, "http://"+site.PublicHost+"/"+upstream.URL+"/emby/Sessions/Playing/Progress", bytes.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("generic playback progress status=%d", recorder.Code)
		}
	}
	assertHistoryCount := func(want int) {
		t.Helper()
		entries, err := app.db.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		var sessions int
		if err := app.db.db.QueryRow("SELECT COUNT(*) FROM watch_sessions WHERE site_id=?", site.ID).Scan(&sessions); err != nil {
			t.Fatal(err)
		}
		if sessions != want || len(entries) != 1 {
			t.Fatalf("history sessions/visible media=%d/%d, want %d/1", sessions, len(entries), want)
		}
		for _, entry := range entries {
			if entry.UpstreamItemID != "generic-item" || entry.Title != "Generic Movie" {
				t.Fatalf("generic history metadata changed: item=%q title=%q", entry.UpstreamItemID, entry.Title)
			}
		}
	}
	play("initial-session")
	assertHistoryCount(1)
	update(nil)
	updated, err := app.db.GetSite(site.ID)
	if err != nil || !updated.WatchHistoryEnabled {
		t.Fatalf("editing generic site cleared history flag: err=%v", err)
	}
	play("edited-session")
	assertHistoryCount(2)
	off := false
	update(&off)
	play("disabled-session")
	assertHistoryCount(2)
	on := true
	update(&on)
	play("reenabled-session")
	assertHistoryCount(3)
}
