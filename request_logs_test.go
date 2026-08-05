package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestRequestLogQueueFiltersAndClear(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "request-logs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	site, err := db.CreateSite("edge-one", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	for _, event := range []requestLogEvent{
		{SiteID: site.ID, SiteName: site.Name, ResourceCategory: requestLogCategoryPlayback, StatusCode: 200, ClientIP: "203.0.113.10", UserAgent: "CapyPlayer/1.1.3", Method: http.MethodPost, Path: "/Items/abc/PlaybackInfo"},
		{SiteID: site.ID, SiteName: site.Name, ResourceCategory: requestLogCategoryImage, StatusCode: 404, ClientIP: "203.0.113.11", UserAgent: "Hills/1.8", Method: http.MethodGet, Path: "/Items/abc/Images/Primary"},
		{SiteID: site.ID, SiteName: site.Name, ResourceCategory: requestLogCategoryAuth, StatusCode: 401, ClientIP: "203.0.113.12", UserAgent: "Hills/1.8", Method: http.MethodPost, Path: "/Users/AuthenticateByName"},
		{SiteID: site.ID, SiteName: site.Name, ResourceCategory: requestLogCategoryAPI, StatusCode: 503, ClientIP: "203.0.113.13", UserAgent: "Browser/1.0", Method: http.MethodGet, Path: "/System/Info"},
	} {
		db.EnqueueRequestLog(event)
	}

	all, err := db.ListRequestLogs(RequestLogFilter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("logs=%d, want 4: %#v", len(all), all)
	}
	playback, err := db.ListRequestLogs(RequestLogFilter{Category: requestLogCategoryPlayback})
	if err != nil || len(playback) != 1 || playback[0].Path != "/Items/abc/PlaybackInfo" {
		t.Fatalf("playback logs=%#v err=%v", playback, err)
	}
	clientErrors, err := db.ListRequestLogs(RequestLogFilter{StatusGroup: "4xx"})
	if err != nil || len(clientErrors) != 2 {
		t.Fatalf("4xx logs=%#v err=%v", clientErrors, err)
	}
	searched, err := db.ListRequestLogs(RequestLogFilter{Query: "capyplayer"})
	if err != nil || len(searched) != 1 || searched[0].StatusCode != 200 {
		t.Fatalf("searched logs=%#v err=%v", searched, err)
	}
	pathSearch, err := db.ListRequestLogs(RequestLogFilter{Query: "playbackinfo"})
	if err != nil || len(pathSearch) != 1 {
		t.Fatalf("path logs=%#v err=%v", pathSearch, err)
	}
	now := time.Now().UnixMilli()
	ranged, err := db.ListRequestLogs(RequestLogFilter{FromMS: now - int64(time.Minute/time.Millisecond), ToMS: now + int64(time.Minute/time.Millisecond)})
	if err != nil || len(ranged) != 4 {
		t.Fatalf("ranged logs=%#v err=%v", ranged, err)
	}

	beforeDropped := db.DroppedRequestLogs()
	db.EnqueueRequestLog(requestLogEvent{SiteID: site.ID, ResourceCategory: "unknown"})
	if db.DroppedRequestLogs() != beforeDropped+1 {
		t.Fatalf("dropped logs=%d, want %d", db.DroppedRequestLogs(), beforeDropped+1)
	}
	if err := db.ClearRequestLogs(); err != nil {
		t.Fatal(err)
	}
	empty, err := db.ListRequestLogs(RequestLogFilter{})
	if err != nil || len(empty) != 0 {
		t.Fatalf("logs after clear=%#v err=%v", empty, err)
	}
}

func TestRequestLogClassificationAndSanitization(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{path: "/Items/abc/PlaybackInfo", want: requestLogCategoryPlayback},
		{path: "/Items/abc/Images/Primary", want: requestLogCategoryImage},
		{path: "/Users/AuthenticateByName", want: requestLogCategoryAuth},
		{path: "/System/Info", want: requestLogCategoryAPI},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "http://media.example"+tc.path+"?api_key=secret", nil)
		req.RemoteAddr = "203.0.113.20:12345"
		req.Header.Set("User-Agent", "Capy\r\nPlayer")
		event := newRequestLogEvent(Site{ID: 7, Name: "node"}, req, nil)
		if event.ResourceCategory != tc.want {
			t.Fatalf("path %s category=%s want=%s", tc.path, event.ResourceCategory, tc.want)
		}
		if event.Path != tc.path || event.ClientIP != "203.0.113.20" || event.UserAgent != "CapyPlayer" {
			t.Fatalf("event=%#v", event)
		}
	}
}

func TestRequestLogResponseWriterAndAPI(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := &requestLogResponseWriter{ResponseWriter: recorder}
	writer.WriteHeader(http.StatusNotFound)
	_, _ = writer.Write([]byte("missing"))
	writer.WriteHeader(http.StatusOK)
	if writer.StatusCode() != http.StatusNotFound || recorder.Code != http.StatusNotFound {
		t.Fatalf("status writer=%d recorder=%d", writer.StatusCode(), recorder.Code)
	}

	app := newTestApp(t)
	site, err := app.db.CreateSite("api-node", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	app.db.EnqueueRequestLog(requestLogEvent{SiteID: site.ID, SiteName: site.Name, ResourceCategory: requestLogCategoryAPI, StatusCode: 200, ClientIP: "203.0.113.30", UserAgent: "Test/1", Method: http.MethodGet, Path: "/System/Info"})

	response := httptest.NewRecorder()
	app.handleRequestLogs(response, httptest.NewRequest(http.MethodGet, "/api/request-logs?status=all&limit=10", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", response.Code, response.Body.String())
	}
	var payload RequestLogsResponse
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || len(payload.Logs) != 1 {
		t.Fatalf("payload=%#v err=%v", payload, err)
	}

	cleared := httptest.NewRecorder()
	app.handleRequestLogs(cleared, httptest.NewRequest(http.MethodDelete, "/api/request-logs", nil))
	if cleared.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d body=%s", cleared.Code, cleared.Body.String())
	}
}
