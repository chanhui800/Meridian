package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSchema57BackfillsFixedUpstreamAndKeepsAgentWireShape(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "schema56.db")
	db, err := openDB(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	site, err := db.CreateSiteRecord(Site{
		Name:         "legacy-fixed",
		ListenPort:   freePort(t),
		IngressMode:  ingressModePort,
		TargetURL:    "http://127.0.0.1:8096",
		PlaybackMode: "direct",
		StreamHosts:  "[]",
		UAMode:       passthroughUAMode,
	})
	if err != nil {
		t.Fatalf("create fixed site: %v", err)
	}
	if _, err := db.db.Exec("ALTER TABLE sites DROP COLUMN upstream_mode"); err != nil {
		t.Fatalf("prepare schema 56 fixture: %v", err)
	}
	if _, err := db.db.Exec("PRAGMA user_version=56"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = openDB(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	restored, err := db.GetSite(site.ID)
	if err != nil {
		t.Fatalf("load migrated site: %v", err)
	}
	if restored.UpstreamMode != "" || restored.TargetURL != site.TargetURL {
		t.Fatalf("migrated fixed site changed: upstream_mode=%q target_url=%q", restored.UpstreamMode, restored.TargetURL)
	}
	encoded, err := json.Marshal(restored)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["upstream_mode"]; ok {
		t.Fatalf("fixed mode changed the legacy JSON wire shape: %s", encoded)
	}
	var schemaVersion int
	if err := db.db.QueryRow("PRAGMA user_version").Scan(&schemaVersion); err != nil || schemaVersion != databaseSchemaVersion {
		t.Fatalf("schema version=%d err=%v, want %d", schemaVersion, err, databaseSchemaVersion)
	}
}

func TestParsePathTargetRequestAllowsPrivateAndLoopbackAuthorities(t *testing.T) {
	tests := []struct {
		path       string
		wantScheme string
		wantHost   string
		wantPath   string
	}{
		{path: "/http://line.xml.org:80/emby/Users/AuthenticateByName", wantScheme: "http", wantHost: "line.xml.org:80", wantPath: "/emby/Users/AuthenticateByName"},
		{path: "/https://[::1]:8920/jellyfin/Items", wantScheme: "https", wantHost: "[::1]:8920", wantPath: "/jellyfin/Items"},
		{path: "/_proxy/v1/http/127.0.0.1:8096/emby/Items", wantScheme: "http", wantHost: "127.0.0.1:8096", wantPath: "/emby/Items"},
		{path: "/_proxy/v1/https/[::1]:8920/jellyfin/Items", wantScheme: "https", wantHost: "[::1]:8920", wantPath: "/jellyfin/Items"},
		{path: "/_proxy/v1/http/localhost:5000", wantScheme: "http", wantHost: "localhost:5000", wantPath: ""},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			target, upstreamPath, _, err := parsePathTargetRequest(request.URL)
			if err != nil {
				t.Fatalf("parsePathTargetRequest: %v", err)
			}
			if target.Scheme != test.wantScheme || target.Host != test.wantHost || upstreamPath != test.wantPath {
				t.Fatalf("target/path = %s://%s%q, want %s://%s%q", target.Scheme, target.Host, upstreamPath, test.wantScheme, test.wantHost, test.wantPath)
			}
		})
	}
}

func TestParsePathTargetRequestFindsNestedCanonicalMediaRoute(t *testing.T) {
	for _, test := range []struct {
		name        string
		url         string
		wantTarget  string
		wantPath    string
		wantRaw     string
		wantEscaped string
		wantQuery   string
	}{
		{
			name:        "base path before canonical route",
			url:         "http://gateway.example/http://frontend.example:80/emby/_proxy/v1/https/media.example:443/videos/1/original.mkv?api_key=private",
			wantTarget:  "https://media.example:443",
			wantPath:    "/videos/1/original.mkv",
			wantEscaped: "/videos/1/original.mkv",
			wantQuery:   "api_key=private",
		},
		{
			name:        "IPv6 target and escaped media path",
			url:         "http://gateway.example/http://frontend.example:80/emby/_proxy/v1/https/[::1]:8920/videos/%E4%B8%AD%20film.mkv",
			wantTarget:  "https://[::1]:8920",
			wantPath:    "/videos/\xe4\xb8\xad film.mkv",
			wantEscaped: "/videos/%E4%B8%AD%20film.mkv",
		},
		{
			name:        "canonical route before nested base path",
			url:         "http://gateway.example/_proxy/v1/http/frontend.example:80/emby/_proxy/v1/https/media.example:443/videos/1/original.mkv?api_key=private",
			wantTarget:  "https://media.example:443",
			wantPath:    "/videos/1/original.mkv",
			wantEscaped: "/videos/1/original.mkv",
			wantQuery:   "api_key=private",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.url, nil)
			target, upstreamPath, rawPath, err := parsePathTargetRequest(request.URL)
			if err != nil {
				t.Fatalf("parse nested canonical route: %v", err)
			}
			if got := target.String(); got != test.wantTarget {
				t.Fatalf("target = %q, want %q", got, test.wantTarget)
			}
			if got := upstreamPath; got != test.wantPath {
				t.Fatalf("upstream path = %q, want %q", got, test.wantPath)
			}
			if got := rawPath; got != test.wantRaw {
				t.Fatalf("raw path = %q, want %q", got, test.wantRaw)
			}
			if got := (&url.URL{Path: upstreamPath, RawPath: rawPath}).EscapedPath(); got != test.wantEscaped {
				t.Fatalf("escaped path = %q, want %q", got, test.wantEscaped)
			}
			if got := request.URL.RawQuery; got != test.wantQuery {
				t.Fatalf("query = %q, want %q", got, test.wantQuery)
			}
		})
	}
}

func TestParsePathTargetRequestRejectsUnsupportedOrAmbiguousAuthority(t *testing.T) {
	for _, path := range []string{
		"/not-a-target/path",
		"/ftp://origin.example:21/media",
		"/http://user:pass@origin.example/emby",
		"/_proxy/v1/ftp/host/file",
		"/_proxy/v1/http/user:pass@host/file",
		"/_proxy/v1/http/host:bad/file",
	} {
		t.Run(path, func(t *testing.T) {
			if _, _, _, err := parsePathTargetRequest(httptest.NewRequest(http.MethodGet, path, nil).URL); err == nil {
				t.Fatal("parsePathTargetRequest accepted an invalid route")
			}
		})
	}
}

func TestRewritePathTargetResponseKeepsRedirectsAndCookiesInTargetRoute(t *testing.T) {
	upstream, err := url.Parse("http://127.0.0.1:8123/base/playlist.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	response := &http.Response{
		Request: &http.Request{URL: upstream},
		Header: http.Header{
			"Location":   {"../next.m3u8?token=private"},
			"Set-Cookie": {"emby_session=keep; Path=/emby; Domain=example.com; HttpOnly", "meridian_session=drop; Path=/"},
		},
	}
	stripPanelSessionSetCookies(response.Header)
	rewritePathTargetResponse(response)
	if got, want := response.Header.Get("Location"), "/_proxy/v1/http/127.0.0.1:8123/next.m3u8?token=private"; got != want {
		t.Fatalf("Location = %q, want %q", got, want)
	}
	cookies := response.Header.Values("Set-Cookie")
	if len(cookies) != 2 || !strings.Contains(cookies[0], "Path=/_proxy/v1/http/127.0.0.1:8123/emby") ||
		!strings.Contains(cookies[1], "Path=/http://127.0.0.1:8123/emby") ||
		strings.Contains(strings.Join(cookies, "\n"), "Domain=") || strings.Contains(strings.Join(cookies, "\n"), "meridian_session") {
		t.Fatalf("rewritten cookies = %#v", cookies)
	}
}

func TestPathTargetSiteRewritesOrDropsUpstreamRedirectLocation(t *testing.T) {
	tests := []struct {
		name         string
		locations    []string
		wantLocation string
	}{
		{name: "malformed network authority", locations: []string{`//\evil.test/path`}},
		{name: "userinfo", locations: []string{"https://user@evil.test/path"}},
		{name: "unsupported scheme", locations: []string{"javascript:alert(1)"}},
		{name: "empty first location", locations: []string{"", "https://evil.test/path"}},
		{name: "relative", locations: []string{"../next.m3u8?token=synthetic"}, wantLocation: "/_proxy/v1/http/{upstream}/next.m3u8?token=synthetic"},
		{name: "protocol relative", locations: []string{"//evil.test/path"}, wantLocation: "/_proxy/v1/_redirect/http/evil.test/path"},
		{name: "path backslash", locations: []string{`/\evil.test/path`}, wantLocation: "/_proxy/v1/http/{upstream}/%5Cevil.test/path"},
		{name: "private target with double slash path", locations: []string{"https://192.168.1.20:8096//media/movie.mkv"}, wantLocation: "/_proxy/v1/_redirect/https/192.168.1.20:8096//media/movie.mkv"},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, test := range tests {
			if r.URL.Query().Get("case") != test.name {
				continue
			}
			for _, location := range test.locations {
				w.Header().Add("Location", location)
			}
			w.WriteHeader(http.StatusFound)
			_, _ = io.WriteString(w, "upstream redirect")
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp(t)
	site, err := app.db.CreateSiteRecord(Site{
		Name: "generic redirect boundary", ListenPort: 8101,
		PublicHost: "generic-redirect.example.test", IngressMode: ingressModeHost,
		UpstreamMode: upstreamModePathTarget, PlaybackMode: "direct",
		StreamHosts: "[]", UAMode: passthroughUAMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	handler, ok := app.pm.PublicHostHandler(site.PublicHost)
	if !ok || handler == nil {
		t.Fatal("generic host route was not registered")
	}
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
				gateway.URL+pathTargetRoutePrefix+"http/"+upstreamURL.Host+"/base/playlist.m3u8?case="+url.QueryEscape(test.name), nil)
			if err != nil {
				t.Fatal(err)
			}
			// RoundTrip preserves the wire response without interpreting or following Location.
			response, err := transport.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusFound || string(body) != "upstream redirect" {
				t.Fatalf("redirect status/body = %d/%q", response.StatusCode, body)
			}
			locations := response.Header.Values("Location")
			if test.wantLocation == "" {
				if len(locations) != 0 {
					t.Fatalf("unsafe upstream Location reached the client: %q", locations)
				}
				return
			}
			want := strings.ReplaceAll(test.wantLocation, "{upstream}", upstreamURL.Host)
			if len(locations) != 1 || locations[0] != want {
				t.Fatalf("Location = %q, want only %q", locations, want)
			}
		})
	}
}

func TestPathTargetSiteProxiesLocalRangeRequestAndMetersIt(t *testing.T) {
	app := newTestApp(t)
	type receivedRequest struct {
		path, query, rangeHeader, cookie string
	}
	received := make(chan receivedRequest, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- receivedRequest{path: r.URL.Path, query: r.URL.RawQuery, rangeHeader: r.Header.Get("Range"), cookie: r.Header.Get("Cookie")}
		w.Header().Set("Content-Range", "bytes 2-5/8")
		w.Header().Add("Set-Cookie", "emby_session=ok; Path=/emby")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "cdef")
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{
		Name:         "path-target-local",
		ListenPort:   8099,
		PublicHost:   "path-target.example.test",
		IngressMode:  ingressModeHost,
		UpstreamMode: upstreamModePathTarget,
		PlaybackMode: "direct",
		StreamHosts:  "[]",
		UAMode:       passthroughUAMode,
	})
	if err != nil {
		t.Fatalf("create path-target site: %v", err)
	}
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatalf("start path-target site: %v", err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	handler, ok := app.pm.PublicHostHandler(site.PublicHost)
	if !ok || handler == nil {
		t.Fatal("path-target host route was not registered")
	}
	requestURL := "http://" + site.PublicHost + pathTargetRoutePrefix + "http/" + target.Host + "/emby/Videos/1/stream?api_key=private"
	request := httptest.NewRequest(http.MethodGet, requestURL, nil)
	request.Header.Set("Range", "bytes=2-5")
	request.Header.Set("Cookie", "meridian_session=panel; emby_session=client")
	request = request.WithContext(context.WithValue(request.Context(), publicHostIngressContextKey{}, true))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusPartialContent || string(body) != "cdef" {
		t.Fatalf("proxy response = %d %q", response.Code, body)
	}
	if got, want := response.Header().Get("Set-Cookie"), "emby_session=ok; Path=/_proxy/v1/http/"+target.Host+"/emby"; got != want {
		t.Fatalf("Set-Cookie = %q, want %q", got, want)
	}
	select {
	case got := <-received:
		if got.path != "/emby/Videos/1/stream" || got.query != "api_key=private" || got.rangeHeader != "bytes=2-5" || got.cookie != "emby_session=client" {
			t.Fatalf("upstream request = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream was not called")
	}
	directURLRequest := httptest.NewRequest(http.MethodGet, "http://"+site.PublicHost+"/http://"+target.Host+"/emby/Videos/1/stream?api_key=private", nil)
	directURLRequest = directURLRequest.WithContext(context.WithValue(directURLRequest.Context(), publicHostIngressContextKey{}, true))
	directURLResponse := httptest.NewRecorder()
	handler.ServeHTTP(directURLResponse, directURLRequest)
	if directURLResponse.Code != http.StatusPartialContent || directURLResponse.Body.String() != "cdef" {
		t.Fatalf("direct URL path response = %d %q", directURLResponse.Code, directURLResponse.Body.String())
	}
	select {
	case got := <-received:
		if got.path != "/emby/Videos/1/stream" || got.query != "api_key=private" {
			t.Fatalf("direct URL path upstream request = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("direct URL path was not forwarded")
	}
	reservedPathURL := "http://" + site.PublicHost + pathTargetRoutePrefix + "http/" + target.Host + "/_meridian/d/resource-token"
	reservedRequest := httptest.NewRequest(http.MethodGet, reservedPathURL, nil)
	reservedRequest = reservedRequest.WithContext(context.WithValue(reservedRequest.Context(), publicHostIngressContextKey{}, true))
	reservedResponse := httptest.NewRecorder()
	handler.ServeHTTP(reservedResponse, reservedRequest)
	if reservedResponse.Code != http.StatusPartialContent {
		t.Fatalf("reserved upstream path response status = %d", reservedResponse.Code)
	}
	select {
	case got := <-received:
		if got.path != "/_meridian/d/resource-token" {
			t.Fatalf("reserved upstream path was intercepted locally: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("reserved upstream path was not forwarded")
	}
	if got := app.pm.LiveSiteTraffic([]Site{*site})[site.ID].TrafficUsed; got == 0 {
		t.Fatal("path-target traffic was not accounted to its site")
	}
}

func TestPathTargetSiteRequiresPublicHostForNodeScheduling(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSiteRecord(Site{
		Name:         "path-target-unscheduled",
		ListenPort:   freePort(t),
		IngressMode:  ingressModePort,
		UpstreamMode: upstreamModePathTarget,
		PlaybackMode: "direct",
		StreamHosts:  "[]",
		UAMode:       passthroughUAMode,
	})
	if err != nil {
		t.Fatalf("create path-target site: %v", err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "global", 0, time.Now()); err == nil || !strings.Contains(err.Error(), "public host") {
		t.Fatalf("schedule error = %v, want public host requirement", err)
	}
}
