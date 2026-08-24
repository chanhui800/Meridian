package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

func insertWatchHistoryRegressionSite(t *testing.T, app *App) int64 {
	t.Helper()
	result, err := app.db.db.Exec(
		"INSERT INTO sites (name, listen_port, target_url, watch_history_enabled) VALUES (?, ?, ?, ?)",
		"cursor-regression", 19082, "http://127.0.0.1:8096", 1,
	)
	if err != nil {
		t.Fatalf("insert watch history regression site: %v", err)
	}
	siteID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read watch history regression site id: %v", err)
	}
	return siteID
}

func decodeWatchHistoryRegressionResponse(t *testing.T, response *httptest.ResponseRecorder) watchHistoryResponse {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("watch history status=%d body=%s", response.Code, response.Body.String())
	}
	var page watchHistoryResponse
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode watch history response: %v", err)
	}
	return page
}

func TestWatchHistoryAPIRejectsReversedTimeRangeAsBadRequest(t *testing.T) {
	app := newTestApp(t)
	response := httptest.NewRecorder()
	app.handleWatchHistory(response, httptest.NewRequest(http.MethodGet, "/api/watch-history?from_ms=200&to_ms=100", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("reversed range status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestWatchHistoryAPICursorDoesNotSkipEqualTimestamps(t *testing.T) {
	app := newTestApp(t)
	siteID := insertWatchHistoryRegressionSite(t, app)
	const observedAtMS = int64(1_780_000_000_000)

	for index := 0; index < 3; index++ {
		event := watchHistoryEvent{
			SiteID:         siteID,
			SessionHash:    fmt.Sprintf("%064x", index+1),
			UpstreamItemID: fmt.Sprintf("same-time-item-%d", index+1),
			EventType:      "progress",
			ObservedAtMS:   observedAtMS,
			PositionTicks:  int64((index + 1) * 100),
			RunTimeTicks:   1_000,
			PlayMethod:     "DirectPlay",
			MediaType:      "movie",
			Title:          fmt.Sprintf("Same Time Movie %d", index+1),
			SeasonNumber:   -1,
			EpisodeNumber:  -1,
			TMDBType:       "movie",
		}
		if skipped, err := app.db.writeWatchHistoryBatch([]watchHistoryEvent{event}); err != nil || skipped != 0 {
			t.Fatalf("write same-timestamp event %d: skipped=%d err=%v", index+1, skipped, err)
		}
	}

	firstQuery := url.Values{
		"site_id": {strconv.FormatInt(siteID, 10)},
		"limit":   {"2"},
	}
	firstResponse := httptest.NewRecorder()
	app.handleWatchHistory(firstResponse, httptest.NewRequest(http.MethodGet, "/api/watch-history?"+firstQuery.Encode(), nil))
	firstPage := decodeWatchHistoryRegressionResponse(t, firstResponse)
	if len(firstPage.Items) != 2 || !firstPage.HasMore || firstPage.NextCursor == "" {
		t.Fatalf("first same-timestamp page=%+v", firstPage)
	}

	secondQuery := url.Values{
		"site_id": {strconv.FormatInt(siteID, 10)},
		"limit":   {"2"},
		"cursor":  {firstPage.NextCursor},
	}
	secondResponse := httptest.NewRecorder()
	app.handleWatchHistory(secondResponse, httptest.NewRequest(http.MethodGet, "/api/watch-history?"+secondQuery.Encode(), nil))
	secondPage := decodeWatchHistoryRegressionResponse(t, secondResponse)
	if len(secondPage.Items) != 1 || secondPage.HasMore || secondPage.NextCursor != "" {
		t.Fatalf("second same-timestamp page=%+v", secondPage)
	}

	seen := make(map[int64]struct{}, 3)
	for _, item := range append(firstPage.Items, secondPage.Items...) {
		if item.LastSeenAtMS != observedAtMS {
			t.Errorf("history id=%d timestamp=%d, want %d", item.ID, item.LastSeenAtMS, observedAtMS)
		}
		if _, duplicate := seen[item.ID]; duplicate {
			t.Fatalf("history id=%d appeared on more than one cursor page", item.ID)
		}
		seen[item.ID] = struct{}{}
	}
	if len(seen) != 3 {
		t.Fatalf("cursor returned %d unique same-timestamp rows, want 3", len(seen))
	}
}
