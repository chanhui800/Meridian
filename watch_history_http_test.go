package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWatchHistoryAPIListsPaginatesAndClears(t *testing.T) {
	app := newTestApp(t)
	result, err := app.db.db.Exec("INSERT INTO sites (name, listen_port, target_url, watch_history_enabled) VALUES ('站点甲', 19011, 'http://127.0.0.1:8096', 1)")
	if err != nil {
		t.Fatal(err)
	}
	siteID, _ := result.LastInsertId()
	nowMS := time.Now().UnixMilli()
	for index, title := range []string{"较早影片", "最新影片"} {
		event := watchHistoryEvent{
			SiteID: siteID, SessionHash: strings.Repeat(string(rune('a'+index)), 64), UpstreamItemID: title,
			ObservedAtMS: nowMS + int64(index), PositionTicks: 90, RunTimeTicks: 100, MediaType: "movie", Title: title,
		}
		if _, err := app.db.writeWatchHistoryBatch([]watchHistoryEvent{event}); err != nil {
			t.Fatalf("write history: %v", err)
		}
	}

	response := httptest.NewRecorder()
	app.handleWatchHistory(response, httptest.NewRequest(http.MethodGet, "/api/watch-history?site_id=all&limit=1", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", response.Code, response.Body.String())
	}
	var page watchHistoryResponse
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Title != "最新影片" || !page.HasMore || page.NextCursor == "" || page.Items[0].ProgressPercent != 90 {
		t.Fatalf("first page = %+v", page)
	}
	if strings.Contains(response.Body.String(), "session_hash") || strings.Contains(response.Body.String(), "imdb_id") {
		t.Fatal("watch history response exposed private matching identifiers")
	}

	response = httptest.NewRecorder()
	app.handleWatchHistory(response, httptest.NewRequest(http.MethodGet, "/api/watch-history?limit=1&cursor="+page.NextCursor, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("second GET status=%d body=%s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Title != "较早影片" || page.HasMore {
		t.Fatalf("second page = %+v", page)
	}

	response = httptest.NewRecorder()
	app.handleWatchHistory(response, httptest.NewRequest(http.MethodDelete, "/api/watch-history?site_id="+strconv.FormatInt(siteID, 10), nil))
	if response.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", response.Code, response.Body.String())
	}
	var count int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM watch_sessions WHERE site_id=?", siteID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("watch sessions after clear=%d err=%v", count, err)
	}
}

func TestWatchHistoryAPIRejectsInvalidCursor(t *testing.T) {
	app := newTestApp(t)
	response := httptest.NewRecorder()
	app.handleWatchHistory(response, httptest.NewRequest(http.MethodGet, "/api/watch-history?cursor=not-a-cursor", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
