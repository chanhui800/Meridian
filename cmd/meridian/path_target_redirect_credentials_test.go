package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func setPathTargetRegressionCredentials(request *http.Request) {
	request.Header.Set("Authorization", "Bearer origin-a")
	request.Header.Set("X-Emby-Authorization", `MediaBrowser Client="player", Token="origin-a"`)
	request.Header.Set("X-Emby-Token", "origin-a")
	request.Header.Set("X-MediaBrowser-Token", "origin-a")
	request.Header.Set("Proxy-Authorization", "Basic origin-proxy")
	request.Header.Set("Cookie", "meridian_session=panel; origin_session=origin-a")
	request.Header.Set("X-Api-Key", "origin-a")
	request.Header.Set("User-Agent", "regression-player/1.0")
	request.Header.Set("Range", "bytes=2-5")
}

func assertPathTargetRegressionCredentials(t *testing.T, header http.Header, present bool) {
	t.Helper()
	for _, name := range []string{"Authorization", "X-Emby-Authorization", "X-Emby-Token", "X-MediaBrowser-Token", "Cookie", "X-Api-Key"} {
		if got := header.Get(name); (got != "") != present {
			t.Errorf("%s presence=%t, want %t; value=%q", name, got != "", present, got)
		}
	}
	if got := header.Get("Proxy-Authorization"); got != "" {
		t.Errorf("proxy credentials reached the origin: %q", got)
	}
	if header.Get("Range") != "bytes=2-5" || header.Get("User-Agent") != "regression-player/1.0" {
		t.Errorf("media headers changed: Range=%q UA=%q", header.Get("Range"), header.Get("User-Agent"))
	}
}

func TestPathTargetRedirectRestrictionSurvivesCanonicalTransitions(t *testing.T) {
	for _, locationForm := range []string{"canonical", "absolute gateway"} {
		t.Run(locationForm, func(t *testing.T) {
			gateway := newPathTargetRegressionGateway(t)
			observed := make(chan *http.Request, 3)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observed <- r.Clone(context.Background())
				if r.URL.Path == "/media/start" {
					location := "/_proxy/v1/http/" + r.Host + "/media/final?" + r.URL.RawQuery
					if locationForm == "absolute gateway" {
						location = gateway.URL + location
					}
					w.Header().Set("Location", location)
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				}
				w.Header().Set("Content-Type", "video/mp4")
				w.Header().Set("Content-Range", "bytes 2-5/8")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, "cdef")
			}))
			t.Cleanup(backend.Close)
			frontend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertPathTargetRegressionCredentials(t, r.Header, true)
				w.Header().Set("Location", backend.URL+"/media/start?api_key=backend-token&sig=%2Fencoded%20value")
				w.WriteHeader(http.StatusFound)
			}))
			t.Cleanup(frontend.Close)
			frontendURL, _ := url.Parse(frontend.URL)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
				gateway.URL+"/_proxy/v1/http/"+frontendURL.Host+"/emya/video", nil)
			if err != nil {
				t.Fatal(err)
			}
			setPathTargetRegressionCredentials(request)
			response, err := gateway.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != http.StatusPartialContent || string(body) != "cdef" || response.Header.Get("Content-Range") != "bytes 2-5/8" {
				t.Fatalf("redirected media: status=%d body=%q range=%q err=%v", response.StatusCode, body, response.Header.Get("Content-Range"), err)
			}
			for range 2 {
				received := <-observed
				assertPathTargetRegressionCredentials(t, received.Header, false)
				if received.URL.RawQuery != "api_key=backend-token&sig=%2Fencoded%20value" {
					t.Errorf("signed media query changed: %q", received.URL.RawQuery)
				}
			}
			// A later explicit request to B retains the client's own credentials;
			// the prior transition does not taint unrelated requests or the site.
			backendURL, _ := url.Parse(backend.URL)
			request, err = http.NewRequestWithContext(t.Context(), http.MethodGet,
				gateway.URL+"/_proxy/v1/http/"+backendURL.Host+"/media/final", nil)
			if err != nil {
				t.Fatal(err)
			}
			setPathTargetRegressionCredentials(request)
			response, err = gateway.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			drainProxyRegressionResponse(t, response)
			assertPathTargetRegressionCredentials(t, (<-observed).Header, true)
		})
	}
}

func TestPathTargetNestedCredentiallessRoutesKeepRestriction(t *testing.T) {
	observed := make(chan http.Header, 4)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.Header.Clone()
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(backend.Close)
	backendURL, _ := url.Parse(backend.URL)
	gateway := newPathTargetRegressionGateway(t)
	for _, route := range []string{
		"/_proxy/v1/_redirect/http/" + backendURL.Host + "/media",
		"/http://frontend.invalid/emby/_proxy/v1/_redirect/http/" + backendURL.Host + "/media",
		"/_proxy/v1/http/frontend.invalid/emby/_proxy/v1/_redirect/http/" + backendURL.Host + "/media",
		"/_proxy/v1/_redirect/http/frontend.invalid/emby/_proxy/v1/http/" + backendURL.Host + "/media",
	} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, gateway.URL+route, nil)
		if err != nil {
			t.Fatal(err)
		}
		setPathTargetRegressionCredentials(request)
		response, err := gateway.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		drainProxyRegressionResponse(t, response)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("nested route %s status=%d", route, response.StatusCode)
		}
		assertPathTargetRegressionCredentials(t, <-observed, false)
	}
}

func TestPathTargetEncodedCredentiallessCompatibilityRoutes(t *testing.T) {
	observed := make(chan *http.Request, 4)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.Clone(context.Background())
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 2-5/8")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "cdef")
	}))
	t.Cleanup(backend.Close)
	upstream, _ := url.Parse(backend.URL)
	gateway := newPathTargetRegressionGateway(t)
	compatibility := "http://frontend.invalid/emby/_proxy/v1/_redirect/http/" + upstream.Host + "/media/video%20name%2Fpart.mkv"
	encoded := url.QueryEscape(compatibility)
	for _, test := range []struct {
		name, route string
		wantStatus  int
	}{
		{"plain compatibility", "/" + compatibility, http.StatusPartialContent},
		{"encoded compatibility", "/" + encoded, http.StatusPartialContent},
		{"double encoded unsupported", "/" + url.QueryEscape(encoded), http.StatusBadRequest},
		{"triple encoded unsupported", "/" + url.QueryEscape(url.QueryEscape(encoded)), http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
				gateway.URL+test.route+"?api_key=backend-token&sig=%2F%2B", nil)
			if err != nil {
				t.Fatal(err)
			}
			setPathTargetRegressionCredentials(request)
			response, err := gateway.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != test.wantStatus {
				t.Fatalf("encoded route status=%d, want=%d body=%s err=%v", response.StatusCode, test.wantStatus, body, err)
			}
			if test.wantStatus == http.StatusBadRequest {
				if len(observed) != 0 {
					t.Fatal("unsupported additional URL decoding contacted an upstream")
				}
				return
			}
			received := <-observed
			assertPathTargetRegressionCredentials(t, received.Header, false)
			if string(body) != "cdef" || received.URL.EscapedPath() != "/media/video%20name%2Fpart.mkv" ||
				received.URL.RawQuery != "api_key=backend-token&sig=%2F%2B" {
				t.Fatalf("encoded compatibility changed media path/query: path=%s query=%s body=%s", received.URL.EscapedPath(), received.URL.RawQuery, body)
			}
		})
	}
}

func TestPathTargetProducerRejectsBrowserDotSegments(t *testing.T) {
	for _, dot := range []string{".", "..", "%2e", "%2E", "%2e%2e", "%2E%2E", "%2E%2e", "%2e%2E", ".%2e", ".%2E", "%2e.", "%2E."} {
		t.Run(dot, func(t *testing.T) {
			target, err := url.Parse("http://cdn.example/" + strings.Repeat(dot+"/", 5) + "http%3A%2F%2Fcdn.example%2Fvideo")
			if err != nil {
				t.Fatal(err)
			}
			for _, credentialless := range []bool{false, true} {
				if route := pathTargetRouteURLWithCredentials(target, credentialless); route != "" {
					t.Fatalf("producer emitted dot segments that can remove gateway routing prefixes: %s", route)
				}
			}
		})
	}
	for _, path := range []string{"/a%2Fb/video", "/a%2F../video", "/%252e%252e/video", "/%2efoo/video", "/a%5C..%5Cvideo"} {
		target, err := url.Parse("http://[::1]:8096" + path + "?sig=%2F%2B&walk=%2e%2e")
		if err != nil {
			t.Fatal(err)
		}
		if route, want := pathTargetRouteURLWithCredentials(target, true), "/_proxy/v1/_redirect/http/[::1]:8096"+path+"?sig=%2F%2B&walk=%2e%2e"; route != want {
			t.Fatalf("safe encoded single segment/query changed: route=%q want=%q", route, want)
		}
	}
}

func TestPathTargetProducerRejectsDotSegmentsInManifestChildren(t *testing.T) {
	base, _ := url.Parse("http://cdn.example/media/manifest")
	unsafe := "http://cdn.example/" + strings.Repeat("%2e%2e/", 5) + "http%3A%2F%2Fcdn.example%2Fvideo"
	hls := &dynamicRewriteSession{ctx: t.Context(), pathTargetMode: true, pathTargetCredentialless: true, base: base, source: dynamicDiscoverySourceHLS}
	if _, err := rewriteHLSResponse([]byte("#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:1,\n"+unsafe+"\n#EXT-X-ENDLIST\n"), hls); err == nil {
		t.Fatal("HLS child bypassed the shared safe route producer")
	}
	dash := &dynamicRewriteSession{ctx: t.Context(), pathTargetMode: true, pathTargetCredentialless: true, base: base, source: dynamicDiscoverySourceDASH}
	if route, err := rewriteDASHTemplate(unsafe+"?number=$Number$", base, dash, dashTemplateBindings{representationID: "main", bandwidth: "1000"}); err == nil {
		t.Fatalf("DASH template emitted a prefix-escaping dot path: %q", route)
	}
}

func TestPathTargetProducerDropsUnsafeUpstreamRedirectLocation(t *testing.T) {
	for _, dot := range []string{"%2e%2e", "%2E%2E", "%2E%2e", "%2e%2E", ".%2e", ".%2E", "%2e.", "%2E."} {
		from, _ := url.Parse("http://api.example/emby/video")
		response := &http.Response{StatusCode: http.StatusTemporaryRedirect, Request: &http.Request{URL: from},
			Header: http.Header{"Location": {"http://cdn.example/" + strings.Repeat(dot+"/", 5) + "http%3A%2F%2Fcdn.example%2Fvideo"}}}
		rewritePathTargetResponse(response)
		if route := response.Header.Get("Location"); route != "" {
			t.Fatalf("upstream redirect escaped restriction after browser normalization: %q", route)
		}
	}
}

func TestPathTargetSameAuthorityRedirectKeepsCredentials(t *testing.T) {
	observed := make(chan http.Header, 2)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.Header.Clone()
		if r.URL.Path == "/first" {
			w.Header().Set("Location", "/second?api_key=origin-token")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(origin.Close)
	upstream, _ := url.Parse(origin.URL)
	gateway := newPathTargetRegressionGateway(t)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, gateway.URL+"/http://"+upstream.Host+"/first", nil)
	if err != nil {
		t.Fatal(err)
	}
	setPathTargetRegressionCredentials(request)
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	drainProxyRegressionResponse(t, response)
	for range 2 {
		assertPathTargetRegressionCredentials(t, <-observed, true)
	}
}

func TestPathTargetCrossAuthority307PreservesRequestBody(t *testing.T) {
	observed := make(chan *http.Request, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := io.ReadAll(r.Body)
		if err != nil || r.Method != http.MethodPost || string(payload) != `{"play":true}` || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("redirected POST method=%s body=%q type=%s err=%v", r.Method, payload, r.Header.Get("Content-Type"), err)
		}
		observed <- r.Clone(context.Background())
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)
	frontend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Location", backend.URL+"/play?api_key=backend-token")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(frontend.Close)
	upstream, _ := url.Parse(frontend.URL)
	gateway := newPathTargetRegressionGateway(t)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		gateway.URL+"/_proxy/v1/http/"+upstream.Host+"/play", strings.NewReader(`{"play":true}`))
	if err != nil {
		t.Fatal(err)
	}
	setPathTargetRegressionCredentials(request)
	request.Header.Set("Content-Type", "application/json")
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	drainProxyRegressionResponse(t, response)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("POST status=%d", response.StatusCode)
	}
	received := <-observed
	assertPathTargetRegressionCredentials(t, received.Header, false)
	if received.URL.RawQuery != "api_key=backend-token" {
		t.Fatalf("POST query changed: %q", received.URL.RawQuery)
	}
}

func TestPathTargetRedirectAuthorityAndEscapedURLBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, from, to string
		credentialless bool
	}{
		{"same case and default port", "http://MEDIA.example:80/first", "http://media.example/second", false},
		{"different port", "http://media.example:8096/first", "http://media.example:8097/second", true},
		{"different scheme", "https://media.example/first", "http://media.example/second", true},
		{"same IPv6 default port", "http://[::1]:80/first", "http://[::1]/second", false},
		{"IPv6 escaped target", "http://127.0.0.1:8096/first", "https://[::1]:8920/media/file%20name%2Fpart.mkv?sig=%2F%2B&empty=", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			from, err := url.Parse(test.from)
			if err != nil {
				t.Fatal(err)
			}
			response := &http.Response{Request: &http.Request{URL: from}, Header: http.Header{"Location": {test.to}}, StatusCode: http.StatusTemporaryRedirect}
			rewritePathTargetResponse(response)
			location := response.Header.Get("Location")
			if strings.HasPrefix(location, "/_proxy/v1/_redirect/") != test.credentialless {
				t.Fatalf("redirect restriction=%q, want credentialless=%t", location, test.credentialless)
			}
			request := httptest.NewRequest(http.MethodGet, location, nil)
			target, path, rawPath, err := parsePathTargetRequest(request.URL)
			if err != nil {
				t.Fatal(err)
			}
			to, _ := url.Parse(test.to)
			wantHost := to.Host
			if !test.credentialless {
				wantHost = from.Host
			}
			if target.Scheme != to.Scheme || target.Host != wantHost || (&url.URL{Path: path, RawPath: rawPath}).EscapedPath() != to.EscapedPath() || request.URL.RawQuery != to.RawQuery {
				t.Fatalf("redirect changed escaped target: %s %s query=%s", target, rawPath, request.URL.RawQuery)
			}
		})
	}
}
