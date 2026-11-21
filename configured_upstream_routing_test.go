package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestConfiguredPlaybackURLControllerAndAgentFailover(t *testing.T) {
	for _, actor := range []string{"controller", "agent"} {
		for _, test := range []struct {
			name, primaryBase, playbackBase string
			streamHost, separateAuthority   bool
			wantFailover                    bool
		}{
			{name: "omitted", wantFailover: true},
			{name: "identical", playbackBase: "/api?line=main&variant=one", wantFailover: true},
			{name: "trailing-base-slash", playbackBase: "/api/?line=main&variant=one", wantFailover: true},
			{name: "equivalent-stream-host", playbackBase: "/api?line=main&variant=one", streamHost: true, wantFailover: true},
			{name: "different-query", playbackBase: "/api?line=playback&variant=one"},
			{name: "different-query-order", playbackBase: "/api?variant=one&line=main"},
			{name: "different-stream-host-query", playbackBase: "/api?line=playback&variant=one", streamHost: true},
			{name: "different-base", playbackBase: "/playback?line=main&variant=one"},
			{name: "identical-escaped-base", primaryBase: "/api%2Fone?line=main&variant=one", playbackBase: "/api%2Fone?line=main&variant=one", wantFailover: true},
			{name: "different-escaped-base", primaryBase: "/api%2Fone?line=main&variant=one", playbackBase: "/api/one?line=main&variant=one"},
			{name: "different-escaped-spelling", primaryBase: "/%61pi?line=main&variant=one", playbackBase: "/api?line=main&variant=one"},
			{name: "separate-authority", playbackBase: "/api?line=main&variant=one", separateAuthority: true},
		} {
			t.Run(actor+"/"+test.name, func(t *testing.T) {
				primaryBase := test.primaryBase
				if primaryBase == "" {
					primaryBase = "/api?line=main&variant=one"
				}
				selectedBase := primaryBase
				if test.playbackBase != "" {
					selectedBase = test.playbackBase
				}
				selected, err := url.Parse(selectedBase)
				if err != nil {
					t.Fatal(err)
				}
				wantPath := strings.TrimSuffix(selected.EscapedPath(), "/") + "/Videos/1/stream.mkv"
				wantQuery := selected.RawQuery + "&quality=original"
				var primaryCalls, playbackCalls, backupCalls atomic.Int32
				failedPlayback := func(w http.ResponseWriter, r *http.Request) {
					if r.URL.EscapedPath() != wantPath || r.URL.RawQuery != wantQuery {
						t.Errorf("selected playback path/query = %q/%q, want %q/%q", r.URL.EscapedPath(), r.URL.RawQuery, wantPath, wantQuery)
					}
					w.WriteHeader(http.StatusBadGateway)
				}
				primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					primaryCalls.Add(1)
					failedPlayback(w, r)
				}))
				t.Cleanup(primary.Close)
				playback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					playbackCalls.Add(1)
					failedPlayback(w, r)
				}))
				t.Cleanup(playback.Close)
				backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					backupCalls.Add(1)
					if r.URL.Path != "/backup/Videos/1/stream.mkv" || r.URL.RawQuery != "line=backup&quality=original" {
						t.Errorf("backup path/query = %q/%q", r.URL.Path, r.URL.RawQuery)
					}
					_, _ = io.WriteString(w, "backup media")
				}))
				t.Cleanup(backup.Close)
				site := Site{
					ID: 73, Name: test.name, PublicHost: "relay.example", IngressMode: ingressModeHost,
					TargetURL: primary.URL + primaryBase, FailoverTargetList: []string{backup.URL + "/backup?line=backup"},
					PlaybackMode: "direct", MainVideoStreamMode: mainVideoStreamModeProxy,
					StreamHosts: "[]", UAMode: passthroughUAMode,
				}
				if test.playbackBase != "" {
					playbackURL := primary.URL + test.playbackBase
					if test.separateAuthority {
						playbackURL = playback.URL + test.playbackBase
					}
					if test.streamHost {
						site.StreamHosts = `["` + playbackURL + `"]`
					} else {
						site.PlaybackTargetURL = playbackURL
					}
				}
				handler, _, runtime := newFailoverProxyHandler(t, actor, site)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://relay.example/Videos/1/stream.mkv?quality=original", nil))
				wantStatus, wantBody, wantBackup := http.StatusBadGateway, "", int32(0)
				wantBackend := primary.URL
				if test.wantFailover {
					wantStatus, wantBody, wantBackup, wantBackend = http.StatusOK, "backup media", 1, backup.URL
				} else if test.separateAuthority {
					wantBackend = playback.URL
				}
				wantPrimary, wantPlayback := int32(1), int32(0)
				if test.separateAuthority {
					wantPrimary, wantPlayback = 0, 1
				}
				if response.Code != wantStatus || response.Body.String() != wantBody || primaryCalls.Load() != wantPrimary || playbackCalls.Load() != wantPlayback || backupCalls.Load() != wantBackup {
					t.Fatalf("playback response status/body = %d/%q, primary/playback/backup calls = %d/%d/%d; want %d/%q, %d/%d/%d", response.Code, response.Body.String(), primaryCalls.Load(), playbackCalls.Load(), backupCalls.Load(), wantStatus, wantBody, wantPrimary, wantPlayback, wantBackup)
				}
				if runtime != nil {
					events := runtime.events.snapshot()
					if len(events) != 1 || events[0].StatusCode != wantStatus || events[0].BackendAddress != wantBackend {
						t.Fatalf("Agent playback events = %+v, want status/backend %d/%q", events, wantStatus, wantBackend)
					}
				}
			})
		}
	}
}

func configuredProxyTransportForHost(t *testing.T, manager *ProxyManager, host string) *http.Transport {
	t.Helper()
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	for _, instance := range manager.proxies {
		if instance.Site.PublicHost == host && instance.transport != nil {
			return instance.transport
		}
	}
	t.Fatal("configured proxy transport missing")
	return nil
}

func TestConfiguredPlaybackURLAuthorityEquivalenceControllerAndAgent(t *testing.T) {
	for _, actor := range []string{"controller", "agent"} {
		for _, test := range []struct {
			name, primary, playback string
			wantFailover            bool
		}{
			{name: "host-case", primary: "http://ORIGIN.example/api", playback: "http://origin.example/api", wantFailover: true},
			{name: "default-http-port", primary: "http://origin.example/api", playback: "http://ORIGIN.example:80/api", wantFailover: true},
			{name: "default-https-port", primary: "https://origin.example/api", playback: "https://ORIGIN.example:443/api", wantFailover: true},
			{name: "different-scheme", primary: "http://origin.example/api", playback: "https://origin.example/api"},
			{name: "different-port", primary: "http://origin.example:8096/api", playback: "http://origin.example:8097/api"},
			{name: "repeated-base-slash", primary: "http://origin.example/api/", playback: "http://origin.example/api//"},
		} {
			t.Run(actor+"/"+test.name, func(t *testing.T) {
				site := Site{
					ID: 73, Name: test.name, PublicHost: "relay.example", IngressMode: ingressModeHost,
					TargetURL: test.primary, PlaybackTargetURL: test.playback,
					FailoverTargetList: []string{"http://backup.example/backup"},
					PlaybackMode:       "direct", MainVideoStreamMode: mainVideoStreamModeProxy,
					StreamHosts: "[]", UAMode: passthroughUAMode,
				}
				handler, manager, _ := newFailoverProxyHandler(t, actor, site)
				playback, err := normalizeTargetURL(test.playback)
				if err != nil {
					t.Fatal(err)
				}
				var primaryCalls, backupCalls atomic.Int32
				// Both schemes are intercepted before dialing. The authority tests
				// exercise the installed Controller/Agent proxy without DNS or TLS.
				fixture := roundTripFunc(func(r *http.Request) (*http.Response, error) {
					status, body := http.StatusBadGateway, ""
					if r.URL.Hostname() == "backup.example" {
						backupCalls.Add(1)
						status, body = http.StatusOK, "backup media"
						if r.URL.Path != "/backup/Videos/1/stream.mkv" {
							t.Errorf("backup request path = %q", r.URL.Path)
						}
					} else {
						primaryCalls.Add(1)
						if r.URL.Scheme != playback.Scheme || r.URL.Host != playback.Host || r.URL.EscapedPath() != strings.TrimSuffix(playback.EscapedPath(), "/")+"/Videos/1/stream.mkv" {
							t.Errorf("configured playback request URL = %s", r.URL)
						}
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
				})
				transport := configuredProxyTransportForHost(t, manager, site.PublicHost)
				transport.RegisterProtocol("http", fixture)
				transport.RegisterProtocol("https", fixture)
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://relay.example/Videos/1/stream.mkv", nil))
				wantStatus, wantBackup := http.StatusBadGateway, int32(0)
				if test.wantFailover {
					wantStatus, wantBackup = http.StatusOK, 1
				}
				if response.Code != wantStatus || primaryCalls.Load() != 1 || backupCalls.Load() != wantBackup {
					t.Fatalf("authority response status/primary/backup = %d/%d/%d, want %d/1/%d", response.Code, primaryCalls.Load(), backupCalls.Load(), wantStatus, wantBackup)
				}
			})
		}
	}
}

type configuredRedirectTestBases struct {
	name, primary, playback, backup, intermediate, response string
}

func configuredRedirectBaseCases() []configuredRedirectTestBases {
	return []configuredRedirectTestBases{
		{name: "playback-base", primary: "/primary", playback: "/playback", intermediate: "/playback", response: "/playback"},
		{name: "original-api-base", primary: "/primary", playback: "/playback", intermediate: "/primary", response: "/primary"},
		{name: "return-to-api-base", primary: "/primary", playback: "/playback", intermediate: "/playback", response: "/primary"},
		{name: "nested-playback-base", primary: "/service", playback: "/service/playback", intermediate: "/service/playback", response: "/service/playback"},
		{name: "playback-path-boundary", primary: "/primary", playback: "/playback", intermediate: "/playback-archive", response: "/primary"},
		{name: "configured-backup-base", primary: "/primary", playback: "/playback", backup: "/backup", intermediate: "/backup", response: "/backup"},
	}
}

func newConfiguredRedirectBaseUpstream(t *testing.T, bases configuredRedirectTestBases, requireMediaCookie bool) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name, want := range map[string]string{
			"Authorization": "Bearer test-client", "X-Test-Private": "client-only", "X-Origin-Key": "configured",
		} {
			if got := r.Header.Get(name); got != want {
				t.Errorf("same-authority request %s header %s = %q, want %q", r.URL.Path, name, got, want)
			}
		}
		if cookie, err := r.Cookie("client_session"); err != nil || cookie.Value != "ready" {
			t.Errorf("same-authority request %s lost client cookie", r.URL.Path)
		}
		for _, segment := range []string{"1", "2"} {
			switch r.URL.Path {
			case bases.primary + "/Videos/" + segment + "/stream.mkv":
				w.Header().Set("Location", bases.intermediate+"/media/start/"+segment)
				w.WriteHeader(http.StatusFound)
				return
			case bases.intermediate + "/media/start/" + segment:
				w.Header().Set("Location", bases.response+"/media/segment/"+segment+"?quality=original")
				w.WriteHeader(http.StatusTemporaryRedirect)
				return
			case bases.response + "/media/segment/" + segment:
				if r.URL.Query().Get("quality") != "original" {
					t.Errorf("redirected segment query = %q", r.URL.RawQuery)
				}
				if requireMediaCookie && segment == "2" {
					if cookie, err := r.Cookie("media_session"); err != nil || cookie.Value != "ready" {
						http.Error(w, "media session missing", http.StatusUnauthorized)
						return
					}
				}
				w.Header().Set("Location", bases.response+"/account?x=1")
				http.SetCookie(w, &http.Cookie{Name: "media_session", Value: "ready", Path: bases.response + "/", HttpOnly: true})
				_, _ = io.WriteString(w, "segment "+segment)
				return
			}
		}
		if r.URL.Path == bases.primary+"/account" && r.URL.Query().Get("x") == "1" {
			if cookie, err := r.Cookie("media_session"); err != nil || cookie.Value != "ready" {
				http.Error(w, "account session missing", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(w, "account page")
			return
		}
		t.Errorf("unexpected configured redirect path = %q", r.URL.Path)
		http.NotFound(w, r)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func configuredRedirectBaseSite(bases configuredRedirectTestBases, upstream *httptest.Server) Site {
	site := Site{
		ID: 73, Name: bases.name, PublicHost: "relay.example", IngressMode: ingressModeHost,
		TargetURL: upstream.URL + bases.primary, PlaybackTargetURL: upstream.URL + bases.playback,
		PlaybackMode: "redirect", MainVideoStreamMode: mainVideoStreamModeProxy,
		StreamHosts: "[]", UAMode: passthroughUAMode,
		RuntimeUpstreamHeaders: map[string][]string{"X-Origin-Key": {"configured"}},
	}
	if bases.backup != "" {
		site.FailoverTargetList = []string{upstream.URL + bases.backup}
	}
	return site
}

func TestConfiguredRedirectControllerAndAgentSelectResponseBase(t *testing.T) {
	for _, actor := range []string{"controller", "agent"} {
		for _, bases := range configuredRedirectBaseCases() {
			t.Run(actor+"/"+bases.name, func(t *testing.T) {
				upstream := newConfiguredRedirectBaseUpstream(t, bases, false)
				site := configuredRedirectBaseSite(bases, upstream)
				handler, manager, _ := newFailoverProxyHandler(t, actor, site)
				transport := configuredProxyTransportForHost(t, manager, site.PublicHost)
				original := transport.Clone()
				t.Cleanup(original.CloseIdleConnections)
				var finalRequests atomic.Int32
				transport.RegisterProtocol("http", roundTripFunc(func(r *http.Request) (*http.Response, error) {
					wantBase := bases.primary
					if strings.HasPrefix(r.URL.Path, bases.playback+"/") {
						wantBase = bases.playback
					}
					if bases.backup != "" && strings.HasPrefix(r.URL.Path, bases.backup+"/") {
						wantBase = bases.backup
					}
					selected, _ := r.Context().Value(configuredUpstreamContextKey{}).(*url.URL)
					if selected == nil || selected.Path != wantBase || !sameRedirectAuthority(selected, r.URL) {
						t.Errorf("request %s configured base = %v, want %s", r.URL.Path, selected, wantBase)
					}
					if r.URL.Path == bases.response+"/media/segment/1" {
						finalRequests.Add(1)
					}
					return original.RoundTrip(r)
				}))
				request := httptest.NewRequest(http.MethodGet, "http://relay.example/Videos/1/stream.mkv", nil)
				request.Header.Set("Authorization", "Bearer test-client")
				request.Header.Set("X-Test-Private", "client-only")
				request.AddCookie(&http.Cookie{Name: "client_session", Value: "ready"})
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusOK || response.Body.String() != "segment 1" || finalRequests.Load() != 1 {
					t.Fatalf("redirect response status/body/final requests = %d/%q/%d", response.Code, response.Body.String(), finalRequests.Load())
				}
			})
		}
	}
}

func TestConfiguredSameAuthorityRedirectKeepsPathIngressCookieAndLocation(t *testing.T) {
	for _, dynamicAvailable := range []bool{false, true} {
		mode := "without-dynamic-key"
		if dynamicAvailable {
			mode = "with-dynamic-key"
		}
		for _, bases := range configuredRedirectBaseCases() {
			t.Run(mode+"/"+bases.name, func(t *testing.T) {
				upstream := newConfiguredRedirectBaseUpstream(t, bases, true)
				site := configuredRedirectBaseSite(bases, upstream)
				site.PublicHost, site.IngressMode, site.PathPrefix = "", ingressModePath, "/relay"
				app := newTestApp(t)
				if dynamicAvailable {
					if err := app.pm.ConfigureDynamicDiscovery(bytes.Repeat([]byte{0x31}, 32), "", 19090, nil); err != nil {
						t.Fatal(err)
					}
				}
				if err := app.pm.StartSite(site); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
				proxy := httptest.NewServer(app.publicHostRouter(http.NotFoundHandler()))
				t.Cleanup(proxy.Close)
				proxyURL, err := url.Parse(proxy.URL)
				if err != nil {
					t.Fatal(err)
				}
				jar, err := cookiejar.New(nil)
				if err != nil {
					t.Fatal(err)
				}
				jar.SetCookies(proxyURL, []*http.Cookie{{Name: "client_session", Value: "ready", Path: "/relay/"}})
				client := proxy.Client()
				client.Jar = jar
				serve := func(path, wantBody string) *http.Response {
					t.Helper()
					request, err := http.NewRequest(http.MethodGet, proxy.URL+path, nil)
					if err != nil {
						t.Fatal(err)
					}
					request.Header.Set("Authorization", "Bearer test-client")
					request.Header.Set("X-Test-Private", "client-only")
					response, err := client.Do(request)
					if err != nil {
						t.Fatal(err)
					}
					body, readErr := io.ReadAll(response.Body)
					_ = response.Body.Close()
					if response.StatusCode != http.StatusOK || readErr != nil || string(body) != wantBody {
						t.Fatalf("path %s status/body = %d/%q, %v; want 200/%q", path, response.StatusCode, body, readErr, wantBody)
					}
					return response
				}
				first := serve("/relay/Videos/1/stream.mkv", "segment 1")
				if cookies := first.Cookies(); len(cookies) != 1 || cookies[0].Path != "/relay/" {
					t.Errorf("configured response cookie = %v, want Path=/relay/", cookies)
				}
				if location := first.Header.Get("Location"); location != "/relay/account?x=1" {
					t.Errorf("configured response Location = %q, want /relay/account?x=1", location)
				}
				serve("/relay/Videos/2/stream.mkv", "segment 2")
				serve(first.Header.Get("Location"), "account page")
			})
		}
	}
}
