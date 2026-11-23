package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCapabilityResourceCannotRelaxBrowserIsolation(t *testing.T) {
	for _, contentType := range []string{"", "text/plain", "application/octet-stream", "video/mp4"} {
		t.Run(contentType, func(t *testing.T) {
			issuer := newStructuredDiscoveryTestIssuer(t)
			base := mustStructuredURL(t, "https://origin.example.com/master.m3u8")
			authority := redirectHostKey(base)
			issuer.configuredAuthorities = map[string]bool{authority: true}
			issuer.primaryAuthority = authority
			// A media endpoint can mislabel active bytes. They must retain the
			// capability sandbox even when no active Content-Type was declared.
			const payload = "\x00\xff<script>window.mediaScriptExecuted=true</script>"
			requests := 0
			issuer.configuredTransport = structuredRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
				requests++
				header := make(http.Header)
				if contentType != "" {
					header.Set("Content-Type", contentType)
				}
				header.Add("Content-Security-Policy", "script-src * 'unsafe-inline'")
				header.Add("Content-Security-Policy", "sandbox allow-scripts allow-same-origin")
				header.Set("X-Content-Type-Options", "upstream-can-sniff")
				header.Set("X-Frame-Options", "SAMEORIGIN")
				header.Set("Referrer-Policy", "unsafe-url")
				header.Set("Set-Cookie", "media-session=untrusted; Path=/")
				return &http.Response{
					StatusCode: http.StatusOK, Header: header,
					Body: io.NopCloser(strings.NewReader(payload)), ContentLength: int64(len(payload)), Request: request,
				}, nil
			})
			session := &dynamicRewriteSession{ctx: context.Background(), issuer: issuer, base: base, source: dynamicDiscoverySourceHLS}
			route, err := session.rewrite("segment.bin")
			if err != nil || !session.commit() {
				t.Fatalf("mint resource capability: route=%q err=%v", route, err)
			}
			recorder := httptest.NewRecorder()
			issuer.serve(recorder, httptest.NewRequest(http.MethodGet, "https://site.example.com"+route, nil))
			if requests != 1 || recorder.Code != http.StatusOK || recorder.Body.String() != payload {
				t.Fatalf("media bytes must survive unchanged: requests=%d status=%d body=%q", requests, recorder.Code, recorder.Body.String())
			}
			const sandbox = "sandbox; default-src 'none'; base-uri 'none'; form-action 'none'"
			if values := recorder.Header().Values("Content-Security-Policy"); len(values) != 1 || values[0] != sandbox {
				t.Fatalf("upstream relaxed the capability sandbox: %q", values)
			}
			for name, want := range map[string]string{
				"X-Content-Type-Options": "nosniff",
				"X-Frame-Options":        "DENY",
				"Referrer-Policy":        "no-referrer",
				"Set-Cookie":             "",
			} {
				if got := recorder.Header().Get(name); got != want {
					t.Errorf("response %s=%q, want %q", name, got, want)
				}
			}
		})
	}
}

func TestPathIngressRedirectPreservesOriginBoundaries(t *testing.T) {
	upstream := &url.URL{Scheme: "https", Host: "origin.example.com", Path: "/source"}
	client := &url.URL{Scheme: "https", Host: "site.example.com"}
	for _, test := range []struct {
		name, location string
		external       bool
	}{
		{name: "absolute origin with slash-shaped path", location: "https://origin.example.com//other.example/video"},
		{name: "three leading slashes", location: "///other.example/video"},
		{name: "backslash path", location: `/\other.example/video`},
		{name: "encoded slashes", location: "/%2f%2fother.example/video"},
		{name: "encoded backslash", location: "/%5cother.example/video"},
		{name: "already prefixed", location: "/sntp//other.example/video"},
		{name: "embedded application prefix", location: "/emby/sntp//other.example/video"},
		{name: "encoded filename and query", location: "/Movies/My%20Movie?next=//other.example"},
		{name: "upstream scheme-relative redirect", location: "//cdn.example.com/video", external: true},
		{name: "upstream absolute redirect", location: "https://cdn.example.com/video", external: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{Header: http.Header{"Location": {test.location}}, Request: &http.Request{URL: upstream}}
			prefixPathIngressResponse(response, "/sntp", "")
			location := response.Header.Get("Location")
			if test.external {
				// Existing external redirects are part of the upstream contract;
				// the prefix mapper must neither synthesize nor reinterpret them.
				if location != test.location {
					t.Fatalf("upstream redirect changed: %q -> %q", test.location, location)
				}
				return
			}
			parsed, err := url.Parse(location)
			if err != nil {
				t.Fatalf("parse mapped Location %q: %v", location, err)
			}
			target := client.ResolveReference(parsed)
			if parsed.IsAbs() || parsed.Host != "" || target.Scheme != client.Scheme || target.Host != client.Host || !strings.HasPrefix(target.Path, "/sntp/") {
				t.Fatalf("local path escaped its ingress: %q -> %q", test.location, target)
			}
		})
	}
}

func TestPlaybackPathLearningDoesNotRewriteRedirectLocation(t *testing.T) {
	issuer := newStructuredDiscoveryTestIssuer(t)
	request := httptest.NewRequest(http.MethodGet, "https://origin.example.com/Items/1/PlaybackInfo", nil)
	request = request.WithContext(context.WithValue(request.Context(), dynamicPlaybackInfoBaseContextKey{}, request.URL))
	const payload = `{"MediaSources":[{"DirectStreamUrl":"/vendor/published-playback?next=//other.example"}]}`
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": {"application/json"},
			"Location":     {"/continue?allowed=yes"},
		},
		Body: io.NopCloser(strings.NewReader(payload)), ContentLength: int64(len(payload)), Request: request,
	}
	if err := rewriteDynamicStructuredResponse(response, issuer, false); err != nil {
		t.Fatalf("rewrite PlaybackInfo response: %v", err)
	}
	defer response.Body.Close()
	if !issuer.state.hasLearnedPlaybackPath("/vendor/published-playback", time.Now()) {
		t.Fatal("test did not publish the playback path")
	}
	if got := response.Header.Get("Location"); got != "/continue?allowed=yes" {
		t.Fatalf("body path changed the independent redirect header: %q", got)
	}
	prefixPathIngressResponse(response, "/sntp", "")
	if got := response.Header.Get("Location"); got != "/sntp/continue?allowed=yes" {
		t.Fatalf("mapped Location used the learned body path: %q", got)
	}
	if request.URL.Scheme != "https" || request.URL.Host != "origin.example.com" {
		t.Fatalf("learning changed the request authority: %s", request.URL)
	}
}
