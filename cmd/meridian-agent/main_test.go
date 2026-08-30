package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestNormalizeControllerSecurityBoundary(t *testing.T) {
	if _, err := normalizeController("http://controller.example.com"); err == nil {
		t.Fatal("remote HTTP controller accepted")
	}
	for _, value := range []string{"https://controller.example.com/", "http://localhost:9090", "http://127.0.0.1:9090"} {
		if _, err := normalizeController(value); err != nil {
			t.Fatalf("normalizeController(%q): %v", value, err)
		}
	}
}

func TestRouteHandlerHealthAndProxyHeaders(t *testing.T) {
	var upstreamURL string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != strings.TrimPrefix(upstreamURL, "http://") {
			t.Errorf("upstream Host = %q", r.Host)
		}
		if r.Header.Get("X-Origin-Secret") != "configured" {
			t.Errorf("custom header missing")
		}
		_, _ = io.WriteString(w, "proxied")
	}))
	defer upstream.Close()
	upstreamURL = upstream.URL
	handler, err := buildRouteHandler(runtimeConfig{NodeGUID: "node-guid", Routes: []siteRoute{{
		SiteID: 1, Host: "media.example.com", TargetURL: upstream.URL, Headers: map[string][]string{"X-Origin-Secret": {"configured"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	health := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "https://media.example.com/.well-known/meridian-agent-health", nil)
	handler.ServeHTTP(health, req)
	if health.Code != http.StatusOK || health.Header().Get("X-Meridian-Node") != "node-guid" {
		t.Fatalf("health = %d %#v", health.Code, health.Header())
	}
	response := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "https://media.example.com/Items", nil)
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK || response.Body.String() != "proxied" {
		t.Fatalf("proxy = %d %q", response.Code, response.Body.String())
	}
}

func TestRouteHandlerMultiplexesSitesByHost(t *testing.T) {
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "first")
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "second")
	}))
	defer second.Close()
	handler, err := buildRouteHandler(runtimeConfig{NodeGUID: "node-guid", Routes: []siteRoute{
		{SiteID: 1, Host: "first.example.com", TargetURL: first.URL},
		{SiteID: 2, Host: "second.example.com", TargetURL: second.URL},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]string{"first.example.com": "first", "second.example.com": "second"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://"+host+"/Items", nil))
		if response.Code != http.StatusOK || response.Body.String() != want {
			t.Fatalf("host %s = %d %q, want 200 %q", host, response.Code, response.Body.String(), want)
		}
	}
}

func TestRouteHandlerUsesPlaybackTargetForMediaRequests(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/Videos/") {
			t.Fatalf("media request reached API target: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, "api")
	}))
	defer api.Close()
	playback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Videos/abc/file" {
			t.Fatalf("unexpected request reached playback target: %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, "media")
	}))
	defer playback.Close()
	handler, err := buildRouteHandler(runtimeConfig{Routes: []siteRoute{{
		SiteID: 7, Host: "split.example.com", TargetURL: api.URL, PlaybackTargetURL: playback.URL,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		path, want string
	}{
		{path: "/Items/abc", want: "api"},
		{path: "/Videos/abc/file", want: "media"},
		{path: "/Items/abc/PlaybackInfo", want: "api"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://split.example.com"+test.path, nil))
		if response.Code != http.StatusOK || response.Body.String() != test.want {
			t.Fatalf("%s = %d %q, want %q", test.path, response.Code, response.Body.String(), test.want)
		}
	}
}

func TestAgentMetadataResponsePathExcludesMediaSubresources(t *testing.T) {
	for _, test := range []struct {
		path string
		want bool
	}{
		{path: "/Items/abc", want: true},
		{path: "/emby/Items", want: true},
		{path: "/Items/abc/Images/Primary", want: false},
		{path: "/Items/abc/PlaybackInfo", want: false},
		{path: "/Shows/abc/Episodes", want: true},
		{path: "/Videos/abc/file", want: false},
	} {
		if got := agentMetadataResponsePath(http.MethodGet, test.path); got != test.want {
			t.Errorf("metadata path %q = %v, want %v", test.path, got, test.want)
		}
	}
}

func TestHTTPSRuntimeReportsOccupiedPortWithoutTakingItOver(t *testing.T) {
	occupied, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port
	runtime := &agentRuntime{stateDir: t.TempDir()}
	config := runtimeConfig{EntryMode: "direct", HTTPPort: 0, HTTPSPort: port, Routes: []siteRoute{{
		SiteID: 1, Host: "media.example.com", TargetURL: "https://upstream.example.com",
	}}}
	if _, err := runtime.startListeners(config); err == nil {
		t.Fatal("occupied HTTPS port was accepted")
	}
	if replacement, err := net.Listen("tcp", occupied.Addr().String()); err == nil {
		_ = replacement.Close()
		t.Fatal("original listener was closed by Agent")
	}
}

func TestRuntimeRejectsLegacySharedOrHTTPListeners(t *testing.T) {
	runtime := &agentRuntime{stateDir: t.TempDir()}
	route := []siteRoute{{SiteID: 1, Host: "media.example.com", TargetURL: "https://upstream.example.com"}}
	if _, err := runtime.startListeners(runtimeConfig{EntryMode: "shared", HTTPPort: 18443, Routes: route}); err == nil {
		t.Fatal("legacy shared listener was accepted")
	}
	if _, err := runtime.startListeners(runtimeConfig{EntryMode: "direct", HTTPPort: 8080, HTTPSPort: 9090, Routes: route}); err == nil {
		t.Fatal("HTTP listener was accepted")
	}
}

func TestListenerMatchingIncludesRouteTransition(t *testing.T) {
	empty := runtimeConfig{HTTPSPort: 9090}
	assigned := runtimeConfig{HTTPSPort: 9090, Routes: []siteRoute{{SiteID: 1}}}
	for _, test := range []struct {
		name        string
		serverCount int
		config      runtimeConfig
		want        bool
	}{
		{name: "idle remains idle", serverCount: 0, config: empty, want: true},
		{name: "first assignment starts listener", serverCount: 0, config: assigned, want: false},
		{name: "assignment keeps listener", serverCount: 1, config: assigned, want: true},
		{name: "last removal stops listener", serverCount: 1, config: empty, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := listenersMatchConfig(9090, test.serverCount, test.config); got != test.want {
				t.Fatalf("listenersMatchConfig = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDirectRuntimeSupportsHTTPSOnlyCustomPort(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reservation.Addr().(*net.TCPAddr).Port
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	runtime := &agentRuntime{stateDir: t.TempDir()}
	servers, err := runtime.startListeners(runtimeConfig{EntryMode: "direct", HTTPPort: 0, HTTPSPort: port, Routes: []siteRoute{{
		SiteID: 1, Host: "media.example.com", TargetURL: "https://upstream.example.com",
	}}})
	if err != nil {
		t.Fatalf("start HTTPS-only listener: %v", err)
	}
	defer shutdownServers(servers)
	if len(servers) != 1 {
		t.Fatalf("server count = %d, want only HTTPS", len(servers))
	}
	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("custom HTTPS port is not listening: %v", err)
	}
	_ = conn.Close()
}
