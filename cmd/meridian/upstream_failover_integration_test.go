package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func newFailoverProxyHandler(t *testing.T, actor string, site Site) (http.Handler, *ProxyManager, *edgeAgentRuntime) {
	t.Helper()
	if actor == "controller" {
		app := newTestApp(t)
		if err := app.pm.ConfigureDynamicDiscovery(bytes.Repeat([]byte{0x31}, 32), "", 19090, nil); err != nil {
			t.Fatal(err)
		}
		if err := app.pm.StartSite(site); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
		return app.publicHostRouter(http.NotFoundHandler()), app.pm, nil
	}
	if actor != "agent" {
		t.Fatalf("unknown actor %q", actor)
	}
	targets, err := json.Marshal(site.FailoverTargetList)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &edgeAgentRuntime{stateDir: t.TempDir()}
	bundle, err := buildEdgeProxy(AgentRuntimeConfig{
		SchemaVersion: 1, NodeGUID: "failover-node", HTTPSPort: 19090, DynamicKey: testEdgeRuntimeKey(t),
		Routes: []AgentSiteRoute{{
			SiteID: site.ID, Host: site.PublicHost, Site: site, TargetURL: site.TargetURL,
			PlaybackTargetURL: site.PlaybackTargetURL, PlaybackMode: site.PlaybackMode,
			FailoverTargets: string(targets), StreamHostsRaw: site.StreamHosts, Headers: site.RuntimeUpstreamHeaders,
		}},
	}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bundle.close)
	return bundle.handler, bundle.manager, runtime
}

func TestFailoverControllerAndAgentPlaybackAPI(t *testing.T) {
	for _, actor := range []string{"controller", "agent"} {
		for _, mode := range []string{"main-video-direct", "legacy-redirect", "separate-playback"} {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				t.Run(actor+"/"+mode+"/"+method, func(t *testing.T) {
					const payload = `{"DeviceProfile":{"MaxStreamingBitrate":120000000}}`
					var primaryAPI, primaryInfo, primaryMedia, backupAPI, backupInfo, backupMedia, playbackMedia atomic.Int32
					playback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						playbackMedia.Add(1)
						if r.URL.Path != "/playback/Videos/1/stream.mkv" {
							t.Errorf("playback path = %q", r.URL.Path)
						}
						_, _ = io.WriteString(w, "configured playback")
					}))
					t.Cleanup(playback.Close)
					primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/primary/Items/1/PlaybackInfo":
							primaryInfo.Add(1)
							if body, err := io.ReadAll(r.Body); err != nil || method == http.MethodPost && string(body) != payload {
								t.Errorf("primary PlaybackInfo body = %q, %v", body, err)
							}
						case "/primary/System/Info":
							primaryAPI.Add(1)
						case "/primary/Videos/1/stream.mkv":
							primaryMedia.Add(1)
							if mode == "legacy-redirect" {
								w.Header().Set("Location", playback.URL+"/playback/Videos/1/stream.mkv")
								w.WriteHeader(http.StatusFound)
								return
							}
						default:
							t.Errorf("unexpected primary path = %q", r.URL.Path)
						}
						w.WriteHeader(http.StatusBadGateway)
					}))
					t.Cleanup(primary.Close)
					var backup *httptest.Server
					backup = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/backup/Items/1/PlaybackInfo":
							backupInfo.Add(1)
							if body, err := io.ReadAll(r.Body); err != nil || r.Method != method || method == http.MethodPost && string(body) != payload {
								t.Errorf("backup PlaybackInfo method/body = %s/%q, %v", r.Method, body, err)
							}
							w.Header().Set("Content-Type", "application/json")
							_ = json.NewEncoder(w).Encode(map[string]any{"MediaSources": []map[string]string{{"DirectStreamUrl": backup.URL + "/backup/media/movie.mp4"}}})
						case "/backup/System/Info":
							backupAPI.Add(1)
							_, _ = io.WriteString(w, "backup API")
						case "/backup/media/movie.mp4":
							backupMedia.Add(1)
							_, _ = io.WriteString(w, "backup media")
						default:
							t.Errorf("unexpected backup path = %q", r.URL.Path)
							http.NotFound(w, r)
						}
					}))
					t.Cleanup(backup.Close)
					site := Site{
						ID: 73, Name: mode, PublicHost: "relay.example", IngressMode: ingressModeHost,
						TargetURL: primary.URL + "/primary", FailoverTargetList: []string{backup.URL + "/backup"},
						PlaybackMode: "direct", MainVideoStreamMode: mainVideoStreamModeProxy, StreamHosts: "[]", UAMode: passthroughUAMode,
					}
					switch mode {
					case "main-video-direct":
						site.MainVideoStreamMode = mainVideoStreamModeDirect
					case "legacy-redirect":
						site.PlaybackMode = "redirect"
						site.PlaybackTargetURL = playback.URL + "/playback"
					case "separate-playback":
						site.PlaybackTargetURL = playback.URL + "/playback"
					}
					handler, _, runtime := newFailoverProxyHandler(t, actor, site)
					serve := func(method, path string, body io.Reader) *httptest.ResponseRecorder {
						rec := httptest.NewRecorder()
						handler.ServeHTTP(rec, httptest.NewRequest(method, "https://relay.example"+path, body))
						return rec
					}
					var requestBody io.Reader
					if method == http.MethodPost {
						requestBody = strings.NewReader(payload)
					}
					info := serve(method, "/Items/1/PlaybackInfo", requestBody)
					var result struct {
						MediaSources []struct {
							DirectStreamURL string `json:"DirectStreamUrl"`
						}
					}
					if info.Code != http.StatusOK || json.Unmarshal(info.Body.Bytes(), &result) != nil || len(result.MediaSources) != 1 || !strings.HasPrefix(result.MediaSources[0].DirectStreamURL, dynamicRoutePrefix) {
						t.Fatalf("PlaybackInfo status/body = %d/%q", info.Code, info.Body.String())
					}
					if api := serve(http.MethodGet, "/System/Info", nil); api.Code != http.StatusOK || api.Body.String() != "backup API" {
						t.Fatalf("API status/body = %d/%q", api.Code, api.Body.String())
					}
					if primaryInfo.Load() != 1 || backupInfo.Load() != 1 || primaryAPI.Load() != 0 || backupAPI.Load() != 1 {
						t.Fatalf("API calls primary/backup = %d/%d, PlaybackInfo = %d/%d", primaryAPI.Load(), backupAPI.Load(), primaryInfo.Load(), backupInfo.Load())
					}
					media := serve(http.MethodGet, "/Videos/1/stream.mkv", nil)
					if mode == "main-video-direct" {
						if media.Code != http.StatusTemporaryRedirect || media.Header().Get("Location") != primary.URL+"/primary/Videos/1/stream.mkv" || primaryMedia.Load() != 1 || playbackMedia.Load() != 0 {
							t.Fatalf("direct route status/location = %d/%q, primary/playback = %d/%d", media.Code, media.Header().Get("Location"), primaryMedia.Load(), playbackMedia.Load())
						}
					} else if media.Code != http.StatusOK || media.Body.String() != "configured playback" || playbackMedia.Load() != 1 || mode == "legacy-redirect" && primaryMedia.Load() != 1 || mode == "separate-playback" && primaryMedia.Load() != 0 {
						t.Fatalf("configured media status/body = %d/%q, primary/playback = %d/%d", media.Code, media.Body.String(), primaryMedia.Load(), playbackMedia.Load())
					}
					capability := serve(http.MethodGet, result.MediaSources[0].DirectStreamURL, nil)
					if mode == "main-video-direct" {
						if capability.Code != http.StatusTemporaryRedirect || capability.Header().Get("Location") != backup.URL+"/backup/media/movie.mp4" {
							t.Fatalf("backup media capability status/location = %d/%q", capability.Code, capability.Header().Get("Location"))
						}
					} else if capability.Code != http.StatusOK || capability.Body.String() != "backup media" {
						t.Fatalf("backup media capability status/body = %d/%q", capability.Code, capability.Body.String())
					}
					if backupMedia.Load() > 1 {
						t.Fatalf("media was retried on a backup: %d calls", backupMedia.Load())
					}
					if runtime != nil {
						events := runtime.events.snapshot()
						if len(events) != 4 {
							t.Fatalf("Agent event count = %d, want 4", len(events))
						}
						for _, event := range events[:2] {
							if event.SiteID != site.ID || event.StatusCode != http.StatusOK || event.BackendAddress != backup.URL {
								t.Errorf("Agent API event site/status/backend = %d/%d/%q", event.SiteID, event.StatusCode, event.BackendAddress)
							}
						}
					}
				})
			}
		}
	}
}

func TestFailoverPathIngressKeepsConfiguredResponseBase(t *testing.T) {
	for _, mode := range []string{"backup-redirect-chain", "backup-active-write", "separate-playback", "legacy-redirect", "legacy-redirect-without-dynamic"} {
		t.Run(mode, func(t *testing.T) {
			legacy := strings.HasPrefix(mode, "legacy-redirect")
			var playback *httptest.Server
			playback = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/playback/Videos/1/stream.mkv" {
					t.Errorf("playback path = %q", r.URL.Path)
				}
				for _, name := range []string{"Authorization", "Cookie", "X-Test-Private"} {
					if r.Header.Get(name) != "" {
						t.Errorf("client %s header reached the separate playback authority", name)
					}
				}
				w.Header().Set("Location", playback.URL+"/playback/account?x=1")
				w.Header().Add("Set-Cookie", "language=zh; Path=/playback/; HttpOnly")
				_, _ = io.WriteString(w, "configured playback")
			}))
			t.Cleanup(playback.Close)
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if legacy && r.URL.Path == "/primary/Videos/1/stream.mkv" {
					w.Header().Set("Location", playback.URL+"/playback/Videos/1/stream.mkv")
					w.WriteHeader(http.StatusFound)
					return
				}
				w.WriteHeader(http.StatusBadGateway)
			}))
			t.Cleanup(primary.Close)
			var backup *httptest.Server
			backup = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "backup-redirect-chain" && r.URL.Path == "/backup/System/Info" {
					w.Header().Set("Location", "/backup/internal/start/page")
					w.WriteHeader(http.StatusFound)
					return
				}
				w.Header().Set("Location", backup.URL+"/backup/account?x=1")
				w.Header().Add("Set-Cookie", "language=zh; Path=/backup/; HttpOnly")
				_, _ = io.WriteString(w, "backup API")
			}))
			t.Cleanup(backup.Close)
			site := Site{
				ID: 1, Name: mode, IngressMode: ingressModePath, PathPrefix: "/relay",
				TargetURL: primary.URL + "/primary", FailoverTargetList: []string{backup.URL + "/backup"},
				PlaybackMode: "direct", MainVideoStreamMode: mainVideoStreamModeProxy, StreamHosts: "[]", UAMode: passthroughUAMode,
			}
			if mode == "separate-playback" || legacy {
				site.PlaybackTargetURL = playback.URL + "/playback"
			}
			if legacy {
				site.PlaybackMode = "redirect"
			}
			var handler http.Handler
			if mode == "legacy-redirect-without-dynamic" {
				app := newTestApp(t)
				if err := app.pm.StartSite(site); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
				handler = app.publicHostRouter(http.NotFoundHandler())
			} else {
				handler, _, _ = newFailoverProxyHandler(t, "controller", site)
			}
			serve := func(method, path string, body io.Reader) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				request := httptest.NewRequest(method, "http://panel.example/relay"+path, body)
				request.Header.Set("Authorization", "Bearer client-token")
				request.Header.Set("Cookie", "client-session=private")
				request.Header.Set("X-Test-Private", "client-only")
				handler.ServeHTTP(rec, request)
				return rec
			}
			method, path, wantBody := http.MethodGet, "/System/Info", "backup API"
			var body io.Reader
			switch mode {
			case "backup-active-write":
				if initial := serve(http.MethodGet, path, nil); initial.Code != http.StatusOK {
					t.Fatalf("establish backup status = %d", initial.Code)
				}
				method, path, body = http.MethodPost, "/Sessions/Playing/Progress", strings.NewReader(`{"PositionTicks":1}`)
			case "separate-playback", "legacy-redirect", "legacy-redirect-without-dynamic":
				path, wantBody = "/Videos/1/stream.mkv", "configured playback"
			}
			response := serve(method, path, body)
			if mode == "backup-redirect-chain" {
				if response.Code != http.StatusFound || response.Header().Get("Location") != "/relay/internal/start/page" {
					t.Fatalf("redirect status/location = %d/%q", response.Code, response.Header().Get("Location"))
				}
				response = serve(http.MethodGet, "/internal/start/page", nil)
			}
			cookies := response.Result().Cookies()
			if response.Code != http.StatusOK || response.Body.String() != wantBody || response.Header().Get("Location") != "/relay/account?x=1" || len(cookies) != 1 || cookies[0].Path != "/relay/" {
				t.Fatalf("response status/body/location/cookies = %d/%q/%q/%v", response.Code, response.Body.String(), response.Header().Get("Location"), cookies)
			}
		})
	}
}

func TestFailoverControllerAndAgentKeepRequestRestrictions(t *testing.T) {
	for _, actor := range []string{"controller", "agent"} {
		for _, restriction := range []string{"fixed-headers", "unknown-length-PlaybackInfo", "unreplayable-read", "websocket"} {
			t.Run(actor+"/"+restriction, func(t *testing.T) {
				var primaryCalls, backupCalls atomic.Int32
				primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					primaryCalls.Add(1)
					if restriction == "fixed-headers" && r.Header.Get("X-Origin-Key") != "configured" {
						t.Error("configured primary header was not applied")
					}
					if body, err := io.ReadAll(r.Body); err != nil || r.Method == http.MethodPost && string(body) != "{}" || restriction == "unreplayable-read" && string(body) != "body" {
						t.Errorf("primary request body = %q, %v", body, err)
					}
					w.WriteHeader(http.StatusBadGateway)
				}))
				t.Cleanup(primary.Close)
				backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					backupCalls.Add(1)
					if r.Header.Get("X-Origin-Key") != "" {
						t.Error("configured primary header reached backup")
					}
					_, _ = io.WriteString(w, "backup API")
				}))
				t.Cleanup(backup.Close)
				site := Site{
					ID: 73, Name: restriction, PublicHost: "relay.example", IngressMode: ingressModeHost,
					TargetURL: primary.URL, FailoverTargetList: []string{backup.URL},
					PlaybackMode: "direct", MainVideoStreamMode: mainVideoStreamModeDirect, StreamHosts: "[]", UAMode: passthroughUAMode,
				}
				if restriction == "fixed-headers" {
					site.RuntimeUpstreamHeaders = map[string][]string{"X-Origin-Key": {"configured"}}
				}
				if restriction == "unknown-length-PlaybackInfo" {
					site.PlaybackMode = "redirect"
					site.PlaybackTargetURL = backup.URL
				}
				handler, _, _ := newFailoverProxyHandler(t, actor, site)
				if restriction == "websocket" {
					initial := httptest.NewRecorder()
					handler.ServeHTTP(initial, httptest.NewRequest(http.MethodGet, "http://relay.example/System/Info", nil))
					if initial.Code != http.StatusOK || primaryCalls.Load() != 1 || backupCalls.Load() != 1 {
						t.Fatalf("establish active backup status/calls = %d/%d/%d", initial.Code, primaryCalls.Load(), backupCalls.Load())
					}
					primaryCalls.Store(0)
					backupCalls.Store(0)
					proxy := httptest.NewServer(handler)
					t.Cleanup(proxy.Close)
					request, err := http.NewRequest(http.MethodGet, proxy.URL+"/socket", nil)
					if err != nil {
						t.Fatal(err)
					}
					request.Host = site.PublicHost
					request.Header.Set("Connection", "Upgrade")
					request.Header.Set("Upgrade", "websocket")
					request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
					request.Header.Set("Sec-WebSocket-Version", "13")
					response, err := proxy.Client().Do(request)
					if err != nil {
						t.Fatal(err)
					}
					response.Body.Close()
					if response.StatusCode != http.StatusBadGateway {
						t.Fatalf("websocket response status = %d", response.StatusCode)
					}
				} else {
					request := httptest.NewRequest(http.MethodGet, "http://relay.example/System/Info", nil)
					switch restriction {
					case "unknown-length-PlaybackInfo":
						request = httptest.NewRequest(http.MethodPost, "http://relay.example/Items/1/PlaybackInfo", io.NopCloser(strings.NewReader("{}")))
						request.ContentLength = -1
					case "unreplayable-read":
						request = httptest.NewRequest(http.MethodGet, "http://relay.example/System/Info", io.NopCloser(strings.NewReader("body")))
						request.ContentLength = 4
					}
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, request)
					if response.Code != http.StatusBadGateway {
						t.Fatalf("restricted request response status = %d", response.Code)
					}
				}
				if primaryCalls.Load() != 1 || backupCalls.Load() != 0 {
					t.Fatalf("restricted request calls primary/backup = %d/%d", primaryCalls.Load(), backupCalls.Load())
				}
			})
		}
	}
}
