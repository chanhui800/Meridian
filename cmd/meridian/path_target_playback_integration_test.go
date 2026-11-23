package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type pathTargetPlaybackFixture struct {
	app                *App
	site               Site
	gateway            *httptest.Server
	frontend           *httptest.Server
	backend            *httptest.Server
	frontendMediaCalls atomic.Int64
	backendMediaCalls  atomic.Int64
}

func newPathTargetPlaybackFixture(t *testing.T) *pathTargetPlaybackFixture {
	t.Helper()
	fixture := &pathTargetPlaybackFixture{app: newTestApp(t)}
	fixture.backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.backendMediaCalls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/media/movie.mkv" || r.Header.Get("Range") != "bytes=0-3" {
			http.Error(w, "unexpected media request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "video/x-matroska")
		w.Header().Set("Content-Range", "bytes 0-3/8")
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "abcd")
	}))
	t.Cleanup(fixture.backend.Close)
	fixture.frontend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/emby/Items/3520299/PlaybackInfo":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"MediaSources": []map[string]any{{
					"Id": "test-media-source", "Protocol": "Http",
					"DirectStreamUrl": fixture.frontend.URL + "/videos/3520299/original.mkv",
				}},
			})
		case "/emby/Items/emos/PlaybackInfo":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"MediaSources":[{"Protocol":"Http","DirectStreamUrl":"/emya/video?server=emos&media_id=test-media"}]}`)
		case "/emya/video":
			http.NotFound(w, r)
		case "/emby/emya/video":
			if r.URL.Query().Get("server") != "emos" || r.URL.Query().Get("media_id") != "test-media" {
				http.Error(w, "missing playback query", http.StatusBadRequest)
				return
			}
			w.Header().Set("Location", fixture.backend.URL+"/media/movie.mkv")
			w.WriteHeader(http.StatusPermanentRedirect)
		case "/videos/3520299/original.mkv":
			fixture.frontendMediaCalls.Add(1)
			w.Header().Set("Location", fixture.backend.URL+"/media/movie.mkv")
			w.WriteHeader(http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.frontend.Close)
	created, err := fixture.app.db.CreateSiteRecord(Site{
		Name: "generic playback integration", ListenPort: 8100,
		PublicHost: "generic-playback.example.test", IngressMode: ingressModeHost,
		UpstreamMode: upstreamModePathTarget, PlaybackMode: "direct",
		MainVideoStreamMode: mainVideoStreamModeProxy, UAMode: passthroughUAMode,
		StreamHosts: "[]",
	})
	if err != nil {
		t.Fatalf("create generic site: %v", err)
	}
	fixture.site = *created
	if err := fixture.app.pm.StartSite(fixture.site); err != nil {
		t.Fatalf("start generic site: %v", err)
	}
	t.Cleanup(func() { _ = fixture.app.pm.StopSite(fixture.site.ID) })
	handler, configured := fixture.app.pm.PublicHostHandler(fixture.site.PublicHost)
	if !configured || handler == nil {
		t.Fatal("generic host route was not registered")
	}
	fixture.gateway = httptest.NewServer(handler)
	t.Cleanup(fixture.gateway.Close)
	return fixture
}

func TestPathTargetPlaybackIntegrationPreservesEmosBasePrefixAndFollowsBackend(t *testing.T) {
	fixture := newPathTargetPlaybackFixture(t)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		fixture.gateway.URL+"/"+fixture.frontend.URL+"/emby/Items/emos/PlaybackInfo", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "CapyPlayer/1.1.8")
	response, err := fixture.gateway.Client().Do(request)
	if err != nil {
		t.Fatalf("get Emos PlaybackInfo: %v", err)
	}
	var payload struct {
		MediaSources []struct {
			DirectStreamURL string `json:"DirectStreamUrl"`
		} `json:"MediaSources"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		response.Body.Close()
		t.Fatalf("decode Emos PlaybackInfo: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(payload.MediaSources) != 1 {
		t.Fatalf("Emos PlaybackInfo status/sources=%d/%d, want 200/1", response.StatusCode, len(payload.MediaSources))
	}
	mediaRoute := payload.MediaSources[0].DirectStreamURL
	if !strings.Contains(mediaRoute, "/_proxy/v1/http/") || !strings.Contains(mediaRoute, "/emby/emya/video?server=emos&media_id=test-media") {
		t.Fatalf("Emos DirectStreamUrl=%q, want canonical /emby/emya/video route", mediaRoute)
	}
	// CapyPlayer appends the canonical URL after the configured Emby API base.
	nestedMediaPath := "/" + fixture.frontend.URL + "/emby" + mediaRoute
	status, body, contentRange := fixture.requestMedia(t, nestedMediaPath)
	if status != http.StatusPartialContent || body != "abcd" || contentRange != "bytes 0-3/8" {
		t.Fatalf("Emos media status/body/range=%d/%q/%q, want 206/abcd/bytes 0-3/8", status, body, contentRange)
	}
	if fixture.frontendMediaCalls.Load() != 0 || fixture.backendMediaCalls.Load() != 1 {
		t.Fatalf("Emos frontend old-path/backend calls=%d/%d, want 0/1", fixture.frontendMediaCalls.Load(), fixture.backendMediaCalls.Load())
	}
	logs := fixture.requestLogs(t, 3)
	var frontendRedirect, backendRange bool
	for _, entry := range logs {
		frontendRedirect = frontendRedirect || entry.Path == "/emby/emya/video" && entry.StatusCode == http.StatusPermanentRedirect && entry.BackendAddress == fixture.frontend.URL
		backendRange = backendRange || entry.Path == "/media/movie.mkv" && entry.StatusCode == http.StatusPartialContent && entry.BackendAddress == fixture.backend.URL
	}
	if !frontendRedirect || !backendRange {
		t.Fatalf("Emos base-path redirect/range logs frontend308/backend206=%t/%t", frontendRedirect, backendRange)
	}
}

func (fixture *pathTargetPlaybackFixture) playbackRoute(t *testing.T) string {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		fixture.gateway.URL+"/"+fixture.frontend.URL+"/emby/Items/3520299/PlaybackInfo", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "Hills/1.11.0")
	response, err := fixture.gateway.Client().Do(request)
	if err != nil {
		t.Fatalf("get generic PlaybackInfo: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("PlaybackInfo status=%d", response.StatusCode)
	}
	var payload struct {
		MediaSources []struct {
			DirectStreamURL string `json:"DirectStreamUrl"`
		} `json:"MediaSources"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode generic PlaybackInfo: %v", err)
	}
	if len(payload.MediaSources) != 1 || !strings.HasPrefix(payload.MediaSources[0].DirectStreamURL, pathTargetRoutePrefix) {
		t.Fatal("PlaybackInfo did not expose a canonical generic media route")
	}
	return payload.MediaSources[0].DirectStreamURL
}

func (fixture *pathTargetPlaybackFixture) requestMedia(t *testing.T, path string) (int, string, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fixture.gateway.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Range", "bytes=0-3")
	request.Header.Set("User-Agent", "Hills/1.11.0")
	response, err := fixture.gateway.Client().Do(request)
	if err != nil {
		t.Fatalf("request generic media: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read generic media: %v", err)
	}
	return response.StatusCode, string(body), response.Header.Get("Content-Range")
}

func (fixture *pathTargetPlaybackFixture) requestLogs(t *testing.T, minimum int) []RequestLog {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := fixture.app.db.ListRequestLogs(RequestLogFilter{Limit: 20})
		if err != nil {
			t.Fatalf("read generic request logs: %v", err)
		}
		var logs []RequestLog
		for _, entry := range entries {
			if entry.SiteID == fixture.site.ID {
				logs = append(logs, entry)
			}
		}
		if len(logs) >= minimum {
			return logs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("generic requests did not produce at least %d logs", minimum)
	return nil
}

func TestPathTargetPlaybackIntegrationCanonicalRouteFollows302AndLogsBackend(t *testing.T) {
	fixture := newPathTargetPlaybackFixture(t)
	mediaRoute := fixture.playbackRoute(t)
	status, body, contentRange := fixture.requestMedia(t, mediaRoute)
	if status != http.StatusPartialContent || body != "abcd" || contentRange != "bytes 0-3/8" {
		t.Fatalf("media response status/body/range=%d/%q/%q, want 206/abcd/bytes 0-3/8", status, body, contentRange)
	}
	if fixture.frontendMediaCalls.Load() != 1 || fixture.backendMediaCalls.Load() != 1 {
		t.Fatalf("frontend/backend media calls=%d/%d, want 1/1", fixture.frontendMediaCalls.Load(), fixture.backendMediaCalls.Load())
	}
	logs := fixture.requestLogs(t, 3)
	var frontendRedirect, backendRange bool
	for _, entry := range logs {
		frontendRedirect = frontendRedirect || entry.Path == "/videos/3520299/original.mkv" && entry.StatusCode == http.StatusFound && entry.BackendAddress == fixture.frontend.URL
		backendRange = backendRange || entry.Path == "/media/movie.mkv" && entry.StatusCode == http.StatusPartialContent && entry.BackendAddress == fixture.backend.URL
	}
	if !frontendRedirect || !backendRange {
		t.Fatalf("generic redirect/range backend attribution frontend302/backend206=%t/%t", frontendRedirect, backendRange)
	}
}

func TestPathTargetPlaybackIntegrationHillsNestedBaseRouteReachesMediaBackend(t *testing.T) {
	fixture := newPathTargetPlaybackFixture(t)
	mediaRoute := fixture.playbackRoute(t)
	// Hills appends the root-relative canonical route after its configured
	// direct-URL base plus /emby instead of resolving it at the gateway root.
	nestedMediaPath := "/" + fixture.frontend.URL + "/emby" + mediaRoute
	status, body, contentRange := fixture.requestMedia(t, nestedMediaPath)
	logs := fixture.requestLogs(t, 2)
	var frontendRedirect, backendRange bool
	for _, entry := range logs {
		frontendRedirect = frontendRedirect || entry.Path == "/videos/3520299/original.mkv" && entry.StatusCode == http.StatusFound && entry.BackendAddress == fixture.frontend.URL && entry.RedirectCount != nil && *entry.RedirectCount == 0
		backendRange = backendRange || entry.Path == "/media/movie.mkv" && entry.StatusCode == http.StatusPartialContent && entry.BackendAddress == fixture.backend.URL
	}
	if !frontendRedirect {
		t.Errorf("nested-base request was not logged at the frontend with a 302 and redirect_count=0; frontend calls=%d", fixture.frontendMediaCalls.Load())
	}
	if status != http.StatusPartialContent || body != "abcd" || contentRange != "bytes 0-3/8" {
		t.Errorf("Hills nested-base media status=%d, want 206; frontend/backend media calls=%d/%d", status, fixture.frontendMediaCalls.Load(), fixture.backendMediaCalls.Load())
	}
	if !backendRange {
		t.Errorf("Hills nested-base media did not reach/log its backend; backend media calls=%d", fixture.backendMediaCalls.Load())
	}
}

func TestPathTargetPlaybackIntegrationCanonicalRouteBeforeNestedBasePath(t *testing.T) {
	fixture := newPathTargetPlaybackFixture(t)
	mediaRoute := fixture.playbackRoute(t)
	frontendAuthority := strings.TrimPrefix(fixture.frontend.URL, "http://")
	nestedCanonicalPath := pathTargetRoutePrefix + "http/" + frontendAuthority + "/emby" + mediaRoute
	status, body, contentRange := fixture.requestMedia(t, nestedCanonicalPath)
	if status != http.StatusPartialContent || body != "abcd" || contentRange != "bytes 0-3/8" {
		t.Fatalf("nested canonical response status/body/range=%d/%q/%q, want 206/abcd/bytes 0-3/8", status, body, contentRange)
	}
	if fixture.frontendMediaCalls.Load() != 1 || fixture.backendMediaCalls.Load() != 1 {
		t.Fatalf("frontend/backend media calls=%d/%d, want 1/1", fixture.frontendMediaCalls.Load(), fixture.backendMediaCalls.Load())
	}
	logs := fixture.requestLogs(t, 3)
	var frontendRedirect, backendRange bool
	for _, entry := range logs {
		frontendRedirect = frontendRedirect || entry.Path == "/videos/3520299/original.mkv" && entry.StatusCode == http.StatusFound && entry.BackendAddress == fixture.frontend.URL
		backendRange = backendRange || entry.Path == "/media/movie.mkv" && entry.StatusCode == http.StatusPartialContent && entry.BackendAddress == fixture.backend.URL
	}
	if !frontendRedirect || !backendRange {
		t.Fatalf("nested canonical redirect/range backend attribution frontend302/backend206=%t/%t", frontendRedirect, backendRange)
	}
}
