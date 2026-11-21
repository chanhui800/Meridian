package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newRequestDiagnosticsProxy(t *testing.T, upstream string, cache *assetCache) (*App, Site, *httptest.Server) {
	t.Helper()
	app := newTestApp(t)
	if cache != nil {
		app.pm.SetAssetCache(cache)
	}
	site, err := app.db.CreateSiteRecord(Site{
		Name: "diagnostics", PublicHost: "diagnostics.example.test", IngressMode: ingressModeHost,
		TargetURL: upstream, PlaybackMode: "direct", UAMode: passthroughUAMode, StreamHosts: "[]",
		AssetCacheEnabled: cache != nil, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/file/*",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	app.pm.mu.RLock()
	handler := app.pm.proxies[site.ID].handler
	app.pm.mu.RUnlock()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return app, *site, server
}

func waitRequestDiagnosticsLog(t *testing.T, app *App, siteID int64, path string) RequestLog {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		logs, err := app.db.ListRequestLogs(RequestLogFilter{Limit: 20})
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range logs {
			if entry.SiteID == siteID && entry.Path == path {
				return entry
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("request log for %s was not recorded", path)
	return RequestLog{}
}

func TestProxyRequestDiagnosticsCompletionEarlyHintsAndReuse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-2" {
			http.Error(w, "range missing", 400)
			return
		}
		w.Header().Set("Link", "</style.css>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-2/12")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "abc")
	}))
	defer upstream.Close()
	app, site, server := newRequestDiagnosticsProxy(t, upstream.URL, nil)
	for index, wantConnection := range []string{"new", "reused"} {
		path := "/Videos/complete-" + string(rune('a'+index)) + "/stream"
		var hints []int
		trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, _ textproto.MIMEHeader) error { hints = append(hints, code); return nil }}
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(t.Context(), trace), http.MethodGet, server.URL+path+"?api_key=never-persist", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Range", "bytes=0-2")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != 206 || string(body) != "abc" || len(hints) != 1 || hints[0] != 103 {
			t.Fatalf("response=%d %q hints=%v error=%v", resp.StatusCode, body, hints, err)
		}
		entry := waitRequestDiagnosticsLog(t, app, site.ID, path)
		if entry.StatusCode != 206 || entry.ResponseStatusCode != 206 || entry.TerminationReason != "completed" || entry.DiagnosticsVersion != 1 || entry.RangeMode != 2 || entry.ConnectionState != wantConnection || entry.ResponseType != "video" {
			t.Fatalf("request diagnostics=%+v", entry)
		}
		if entry.DurationMS == nil || entry.UpstreamFirstByteMS == nil || entry.BodyFirstByteMS == nil || entry.BodyBytesWritten == nil || *entry.BodyBytesWritten != 3 || *entry.UpstreamFirstByteMS > *entry.DurationMS || *entry.BodyFirstByteMS > *entry.DurationMS {
			t.Fatalf("missing or impossible transfer measurements: %+v", entry.RequestDiagnostics)
		}
		if entry.DNSDurationMS != nil || entry.TLSDurationMS != nil || index == 0 && entry.TCPDurationMS == nil || index == 1 && entry.TCPDurationMS != nil {
			t.Fatalf("unobserved/reused phase was fabricated: %+v", entry.RequestDiagnostics)
		}
		if strings.Contains(entry.Path, "?") || entry.CacheLookup != "disabled" {
			t.Fatalf("request diagnostic cache/path=%+v", entry)
		}
	}
}

func TestProxyRequestDiagnostics206CancellationPreservesFilter(t *testing.T) {
	upstreamStopped := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamStopped)
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "1048576")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, strings.Repeat("x", 64<<10))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	app, site, server := newRequestDiagnosticsProxy(t, upstream.URL, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/Videos/cancel/stream", nil)
	req.Header.Set("Range", "bytes=0-")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = resp.Body.Close()
	entry := waitRequestDiagnosticsLog(t, app, site.ID, "/Videos/cancel/stream")
	if entry.StatusCode != clientClosedRequestStatus || entry.ResponseStatusCode != 206 || entry.TerminationReason != "client_canceled" || entry.BodyBytesWritten == nil || *entry.BodyBytesWritten <= 0 || *entry.BodyBytesWritten > 64<<10 || entry.BodyFirstByteMS == nil {
		t.Fatalf("canceled transfer diagnostics=%+v", entry)
	}
	filtered, err := app.db.ListRequestLogs(RequestLogFilter{StatusGroup: "4xx", Limit: 10})
	if err != nil || len(filtered) != 1 || filtered[0].ID != entry.ID {
		t.Fatalf("499 filter lost canceled 206: %+v error=%v", filtered, err)
	}
	select {
	case <-upstreamStopped:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not stop after cancellation")
	}
}

func TestProxyRequestDiagnosticsUpstreamBodyErrorIsLoggedAtErrorLevel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "32768")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, strings.Repeat("x", 8192))
	}))
	defer upstream.Close()
	app, site, server := newRequestDiagnosticsProxy(t, upstream.URL, nil)
	settings := app.db.currentSystemSettings()
	settings.LogLevel = "error"
	if err := app.db.saveSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	resp, err := server.Client().Get(server.URL + "/Videos/truncated/stream")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 206 || !errors.Is(err, io.ErrUnexpectedEOF) || len(body) == 0 {
		t.Fatalf("truncated response=%d bytes=%d err=%v", resp.StatusCode, len(body), err)
	}
	entry := waitRequestDiagnosticsLog(t, app, site.ID, "/Videos/truncated/stream")
	if entry.StatusCode != 206 || entry.ResponseStatusCode != 206 || entry.TerminationReason != "upstream_error" || entry.BodyBytesWritten == nil || *entry.BodyBytesWritten != 8192 {
		t.Fatalf("truncated diagnostics=%+v", entry)
	}
}

func TestProxyRequestDiagnosticsGatewayFailureDoesNotFabricateFirstByte(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	upstreamURL := upstream.URL
	app, site, server := newRequestDiagnosticsProxy(t, upstreamURL, nil)
	upstream.Close()
	resp, err := server.Client().Get(server.URL + "/System/Unavailable")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || resp.StatusCode != 502 {
		t.Fatalf("gateway response=%d %q err=%v", resp.StatusCode, body, err)
	}
	entry := waitRequestDiagnosticsLog(t, app, site.ID, "/System/Unavailable")
	if entry.ResponseStatusCode != 502 || entry.TerminationReason != "upstream_error" || entry.UpstreamFirstByteMS != nil || entry.TCPDurationMS == nil || entry.BodyBytesWritten == nil || *entry.BodyBytesWritten != int64(len(body)) {
		t.Fatalf("gateway failure diagnostics=%+v", entry)
	}
}

func TestProxyRequestDiagnosticsCacheHitAndDiskFailures(t *testing.T) {
	for _, diskFailure := range []bool{false, true} {
		name := "hit"
		if diskFailure {
			name = "disk_failure"
		}
		t.Run(name, func(t *testing.T) {
			var upstreamRequests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamRequests.Add(1)
				w.Header().Set("Content-Type", "text/css")
				_, _ = io.WriteString(w, "body")
			}))
			defer upstream.Close()
			cacheDir := filepath.Join(t.TempDir(), "cache")
			if diskFailure {
				if err := os.WriteFile(cacheDir, []byte("blocked root"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			app, site, server := newRequestDiagnosticsProxy(t, upstream.URL, newAssetCache(cacheDir))
			for index := 0; index < 2; index++ {
				if err := app.db.ClearRequestLogs(); err != nil {
					t.Fatal(err)
				}
				resp, err := server.Client().Get(server.URL + "/file/theme.css")
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || resp.StatusCode != 200 || string(body) != "body" {
					t.Fatalf("cache HTTP failed=%d %q err=%v", resp.StatusCode, body, err)
				}
				entry := waitRequestDiagnosticsLog(t, app, site.ID, "/file/theme.css")
				if entry.BodyBytesWritten == nil || *entry.BodyBytesWritten != 4 || entry.ResponseType != "style" || entry.TerminationReason != "completed" {
					t.Fatalf("cache transfer diagnostics=%+v", entry)
				}
				if diskFailure {
					if entry.CacheLookup != "read_error" || entry.CacheWrite != "write_error" || entry.CacheLookupReason == "" || entry.CacheWriteReason == "" || entry.ResponseStatusCode != 200 {
						t.Fatalf("disk failure diagnostics=%+v", entry)
					}
				} else if index == 0 {
					if entry.CacheLookup != "miss" || entry.CacheWrite != "stored" || entry.UpstreamFirstByteMS == nil {
						t.Fatalf("fill diagnostics=%+v", entry)
					}
				} else {
					if entry.CacheLookup != "hit" || entry.CacheWrite != "not_attempted" || entry.ConnectionState != "" || entry.UpstreamFirstByteMS != nil || entry.DNSDurationMS != nil || entry.TCPDurationMS != nil || entry.TLSDurationMS != nil {
						t.Fatalf("cache hit fabricated upstream activity=%+v", entry)
					}
				}
			}
			wantRequests := int32(1)
			if diskFailure {
				wantRequests = 2
			}
			if upstreamRequests.Load() != wantRequests {
				t.Fatalf("upstream requests=%d want=%d", upstreamRequests.Load(), wantRequests)
			}
		})
	}
}

type diagnosticsPartialWriter struct{ http.ResponseWriter }

func (writer diagnosticsPartialWriter) Write([]byte) (int, error) { return 2, io.ErrClosedPipe }

func TestRequestDiagnosticsActualWritesAndBoundedConcurrentUpdates(t *testing.T) {
	start := time.Now()
	tracker := newRequestDiagnosticsTracker(start, httptest.NewRequest(http.MethodGet, "/", nil))
	writer := &requestLogResponseWriter{ResponseWriter: diagnosticsPartialWriter{httptest.NewRecorder()}, diagnostics: tracker}
	writer.WriteHeader(http.StatusEarlyHints)
	if writer.ResponseStatusCode() != 0 {
		t.Fatal("informational status became final")
	}
	writer.WriteHeader(206)
	if n, err := writer.Write([]byte("abcd")); n != 2 || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write=%d err=%v", n, err)
	}
	var workers sync.WaitGroup
	for index := 0; index < 8; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for count := 0; count < 100; count++ {
				tracker.SetCacheLookup("read_error", "permission")
				tracker.SetCacheWrite("write_error", "no_space")
				tracker.SetResponseType("image/png; token=never-persist")
				observeDynamicDNSDuration(context.WithValue(context.Background(), requestDiagnosticsContextKey{}, tracker), 0)
				_ = tracker.snapshot(time.Now(), 206, nil, nil)
			}
		}()
	}
	workers.Wait()
	result := tracker.snapshot(time.Now(), writer.ResponseStatusCode(), nil, nil)
	if result.ResponseStatusCode != 206 || result.TerminationReason != "downstream_error" || result.BodyBytesWritten == nil || *result.BodyBytesWritten != 2 || result.DNSDurationMS == nil || *result.DNSDurationMS != 0 || result.CacheLookupReason != "permission" || result.CacheWriteReason != "no_space" || result.ResponseType != "image" {
		t.Fatalf("actual write/concurrent diagnostics=%+v", result)
	}
	encoded := requestDiagnosticsJSON(result)
	if strings.Contains(encoded, "never-persist") || len(encoded) > requestDiagnosticsMaxJSONBytes {
		t.Fatalf("unbounded/sensitive diagnostics JSON=%q", encoded)
	}
}

func TestRequestDiagnosticsMigrationAndLegacyRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	site, err := db.CreateSiteRecord(Site{Name: "legacy diagnostics", PublicHost: "legacy.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatal(err)
	}
	db.EnqueueRequestLog(requestLogEvent{SiteID: site.ID, SiteName: site.Name, ResourceCategory: requestLogCategoryAPI, StatusCode: 200, Method: http.MethodGet, Path: "/legacy"})
	if _, err := db.ListRequestLogs(RequestLogFilter{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("ALTER TABLE request_logs DROP COLUMN diagnostics_json"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("PRAGMA user_version=53"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != databaseSchemaVersion {
		t.Fatalf("migration schema=%d err=%v", version, err)
	}
	logs, err := db.ListRequestLogs(RequestLogFilter{})
	if err != nil || len(logs) != 1 || logs[0].DiagnosticsVersion != 0 || logs[0].DurationMS != nil || logs[0].ResponseStatusCode != 0 || logs[0].Path != "/legacy" {
		t.Fatalf("legacy record fabricated diagnostics=%+v err=%v", logs, err)
	}
	encoded, _ := json.Marshal(logs[0])
	if strings.Contains(string(encoded), "duration_ms") || strings.Contains(string(encoded), "diagnostics_version") {
		t.Fatalf("legacy JSON reported measured zero=%s", encoded)
	}
	for _, raw := range []string{`{"diagnostics_version":2,"duration_ms":0}`, `{"diagnostics_version":1,"duration_ms":-1,"cache_reason":"token=secret","connection_state":"https://secret","response_type":"<svg>"}`, `{"diagnostics_version":1,`} {
		value := parseRequestDiagnosticsJSON(raw)
		if value.DurationMS != nil || value.CacheReason != "" || value.ConnectionState != "" || value.ResponseType != "" {
			t.Fatalf("invalid diagnostics survived: %+v", value)
		}
	}
	var diagnosticsColumn string
	if err := db.db.QueryRow("SELECT type FROM pragma_table_info('request_logs') WHERE name='diagnostics_json'").Scan(&diagnosticsColumn); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if diagnosticsColumn != "TEXT" {
		t.Fatalf("additive diagnostics column=%q", diagnosticsColumn)
	}
}

func TestRequestDiagnosticsWriteAndDisplaySettingsMaskRelatedFields(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSiteRecord(Site{Name: "diagnostic privacy", PublicHost: "diagnostic-privacy.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatal(err)
	}
	zero, bytesWritten := int64(0), int64(1234)
	event := requestLogEvent{
		RequestDiagnostics: RequestDiagnostics{DiagnosticsVersion: 1, ResponseStatusCode: 206, TerminationReason: "client_canceled",
			DurationMS: &zero, UpstreamFirstByteMS: &zero, BodyFirstByteMS: &zero, BodyBytesWritten: &bytesWritten,
			RangeMode: 2, ConnectionState: "new", DNSDurationMS: &zero, TCPDurationMS: &zero, TLSDurationMS: &zero,
			CacheLookup: "read_error", CacheWrite: "write_error", CacheLookupReason: "permission", CacheWriteReason: "no_space", ResponseType: "video"},
		SiteID: site.ID, SiteName: site.Name, ResourceCategory: requestLogCategoryStream, StatusCode: 499,
		UserAgent: "private-UA", UpstreamUserAgent: "private-upstream-UA", Method: http.MethodGet, Path: "/written",
	}
	app.db.EnqueueRequestLog(event)
	logs, err := app.db.ListRequestLogs(RequestLogFilter{})
	if err != nil || len(logs) != 1 || logs[0].ResponseStatusCode != 206 || logs[0].DurationMS == nil {
		t.Fatalf("initial diagnostics=%+v err=%v", logs, err)
	}
	settings := app.db.currentSystemSettings()
	settings.LogDisplayStatus, settings.LogDisplayTimeline, settings.LogDisplayCategory, settings.LogDisplayBackendAddress = false, false, false, false
	settings.LogDisplayUA, settings.LogDisplayUpstreamUA = false, false
	if err := app.db.saveSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	logs, err = app.db.ListRequestLogs(RequestLogFilter{})
	if err != nil || len(logs) != 1 {
		t.Fatalf("masked display=%+v err=%v", logs, err)
	}
	entry := logs[0]
	if entry.StatusCode != 0 || entry.ResponseStatusCode != 0 || entry.TerminationReason != "" || entry.DurationMS != nil || entry.BodyBytesWritten != nil || entry.DNSDurationMS != nil || entry.TCPDurationMS != nil || entry.TLSDurationMS != nil || entry.RangeMode != 0 || entry.CacheLookup != "" || entry.CacheWrite != "" || entry.CacheLookupReason != "" || entry.CacheWriteReason != "" || entry.ResponseType != "" || entry.ConnectionState != "" || entry.UserAgent != "hidden" || entry.UpstreamUserAgent != "hidden" {
		t.Fatalf("display settings leaked related diagnostics=%+v", entry)
	}
	var storedJSON string
	if err := app.db.db.QueryRow("SELECT diagnostics_json FROM request_logs WHERE path='/written'").Scan(&storedJSON); err != nil {
		t.Fatal(err)
	}
	if stored := parseRequestDiagnosticsJSON(storedJSON); stored.ResponseStatusCode != 206 || stored.DurationMS == nil {
		t.Fatal("display mask changed stored observations")
	}
	settings.LogWriteStatus, settings.LogWriteTimeline, settings.LogWriteCategory, settings.LogWriteBackendAddress = false, false, false, false
	settings.LogWriteUA, settings.LogWriteUpstreamUA = false, false
	if err := app.db.saveSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	event.Path = "/masked-write"
	app.db.EnqueueRequestLog(event)
	if _, err := app.db.ListRequestLogs(RequestLogFilter{}); err != nil {
		t.Fatal(err)
	}
	if err := app.db.db.QueryRow("SELECT diagnostics_json FROM request_logs WHERE path='/masked-write'").Scan(&storedJSON); err != nil {
		t.Fatal(err)
	}
	if storedJSON != `{"diagnostics_version":1}` {
		t.Fatalf("write settings leaked related diagnostics=%s", storedJSON)
	}
}

func TestRequestDiagnosticsSiteStopCauseIsNotUpstreamFailure(t *testing.T) {
	tracker := newRequestDiagnosticsTracker(time.Now(), nil)
	tracker.markUpstreamError() // Canceling the site also aborts its upstream read.
	if result := tracker.snapshot(time.Now(), 206, nil, context.Canceled); result.TerminationReason != "site_stopping" || result.ResponseStatusCode != 206 {
		t.Fatalf("site stop was mislabeled as an upstream fault: %+v", result)
	}
	if result := tracker.snapshot(time.Now(), 206, context.Canceled, context.Canceled); result.TerminationReason != "client_canceled" {
		t.Fatalf("original client cancellation was lost: %+v", result)
	}
	if result := newRequestDiagnosticsTracker(time.Now(), nil).snapshot(time.Now(), 0, nil, nil); result.ResponseStatusCode != 200 || result.BodyBytesWritten == nil || *result.BodyBytesWritten != 0 || result.BodyFirstByteMS != nil {
		t.Fatalf("empty normal response measurements=%+v", result)
	}
}
