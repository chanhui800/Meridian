package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newPathTargetRegressionGateway(t *testing.T) *httptest.Server {
	t.Helper()
	app := newTestApp(t)
	site, err := app.db.CreateSiteRecord(Site{
		Name: "generic boundary regression", ListenPort: 8101,
		PublicHost: "generic-boundary.example.test", IngressMode: ingressModeHost,
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
		t.Fatal("generic route missing")
	}
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)
	return gateway
}

func TestAssetCacheSeparatesRepeatedCookieAuthentication(t *testing.T) {
	avatars := map[string][]byte{}
	for index, name := range []string{"alice", "bob"} {
		var encoded bytes.Buffer
		avatar := image.NewRGBA(image.Rect(0, 0, 2, 2))
		avatar.SetRGBA(0, 0, color.RGBA{R: uint8(100 + index*50), G: 25, A: 255})
		if err := png.Encode(&encoded, avatar); err != nil {
			t.Fatal(err)
		}
		avatars[name] = encoded.Bytes()
	}
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		cookie, err := r.Cookie("origin_session")
		if err != nil {
			http.Error(w, "missing session", http.StatusUnauthorized)
			return
		}
		body, ok := avatars[cookie.Value]
		if !ok {
			http.Error(w, "invalid session", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(upstream.Close)
	app := newTestApp(t)
	app.pm.SetAssetCache(newAssetCache(t.TempDir()))
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{
		Name: "cookie cache regression", ListenPort: 8102,
		PublicHost: "cache-boundary.example.test", IngressMode: ingressModeHost,
		TargetURL: upstream.URL, PlaybackMode: "direct", StreamHosts: "[]", UAMode: passthroughUAMode,
		AssetCacheEnabled: true, AssetCacheTTLSec: 60, AssetCacheMaxBytes: 1 << 20,
		AssetCacheRules: target.Host + "/*",
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
		t.Fatal("fixed route missing")
	}
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)
	client := &http.Client{Timeout: 5 * time.Second}
	for _, user := range []string{"alice", "bob"} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, gateway.URL+"/avatar.png", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header["Cookie"] = []string{"prefs=shared", "origin_session=" + user}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("user %s status=%d", user, response.StatusCode)
		}
		if !bytes.Equal(body, avatars[user]) {
			t.Fatalf("user %s received another authenticated user's PNG; cache=%s upstream calls=%d (Cookie headers: prefs=shared and origin_session=%s)", user, response.Header.Get("X-Meridian-Cache"), calls.Load(), user)
		}
	}
	for _, cookie := range []string{"prefs=shared; origin_session=alice", "meridian_session=panel-session; prefs=shared; origin_session=alice"} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, gateway.URL+"/avatar.png", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Cookie", cookie)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || !bytes.Equal(body, avatars["alice"]) || response.Header.Get("X-Meridian-Cache") != "HIT" || calls.Load() != 2 {
			t.Fatalf("equivalent Cookie representation missed Alice's cached image: status=%d cache=%s calls=%d err=%v", response.StatusCode, response.Header.Get("X-Meridian-Cache"), calls.Load(), err)
		}
	}
}

func drainProxyRegressionResponse(t *testing.T, response *http.Response) {
	t.Helper()
	_, err := io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
}

// The direct-origin control strips Authorization at an actual cross-host
// redirect. Only the equivalent gateway redirect leaks the same credential.
func TestPathTargetRedirectDoesNotForwardOriginAuthorization(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			authorizations := make(chan string, 2)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authorizations <- r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "video/mp4")
				_, _ = io.WriteString(w, "synthetic-media")
			}))
			t.Cleanup(backend.Close)
			backendURL := "http://localhost:" + strings.Split(strings.TrimPrefix(backend.URL, "http://"), ":")[1] + "/media/file.mp4"
			frontend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", backendURL)
				w.WriteHeader(status)
			}))
			t.Cleanup(frontend.Close)
			client := &http.Client{Timeout: 5 * time.Second}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, frontend.URL+"/Videos/1/original.mkv", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer synthetic-origin-a")
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			drainProxyRegressionResponse(t, response)
			if got := <-authorizations; got != "" {
				t.Fatalf("direct-origin control retained authorization: %q", got)
			}
			gateway := newPathTargetRegressionGateway(t)
			frontendTarget, err := url.Parse(frontend.URL)
			if err != nil {
				t.Fatal(err)
			}
			request, err = http.NewRequestWithContext(t.Context(), http.MethodGet,
				gateway.URL+pathTargetRoutePrefix+"http/"+frontendTarget.Host+"/Videos/1/original.mkv", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer synthetic-origin-a")
			response, err = client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			drainProxyRegressionResponse(t, response)
			if got := <-authorizations; got != "" {
				t.Fatalf("passive %d gateway redirect forwarded origin A's Authorization to different host B: %q (direct control stripped it)", status, got)
			}
		})
	}
}

func TestPathTargetCookieLoginAcrossRouteFormats(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			http.SetCookie(w, &http.Cookie{Name: "origin_session", Value: "synthetic-login", Path: "/", HttpOnly: true})
			w.WriteHeader(http.StatusOK)
			return
		}
		cookie, err := r.Cookie("origin_session")
		if err != nil || cookie.Value != "synthetic-login" {
			http.Error(w, "session missing", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "authenticated")
	}))
	t.Cleanup(upstream.Close)
	upstreamTarget, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"canonical", "compatibility"} {
		t.Run(format, func(t *testing.T) {
			gateway := newPathTargetRegressionGateway(t)
			prefix := pathTargetRoutePrefix + "http/" + upstreamTarget.Host
			if format == "compatibility" {
				prefix = "/http://" + upstreamTarget.Host
			}
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Jar: jar, Timeout: 5 * time.Second}
			response, err := client.Get(gateway.URL + prefix + "/login")
			if err != nil {
				t.Fatal(err)
			}
			setCookie := response.Header.Get("Set-Cookie")
			drainProxyRegressionResponse(t, response)
			response, err = client.Get(gateway.URL + prefix + "/protected")
			if err != nil {
				t.Fatal(err)
			}
			gotStatus := response.StatusCode
			drainProxyRegressionResponse(t, response)
			if gotStatus != http.StatusOK {
				t.Fatalf("%s route login followed by protected request returned %d; cookie path emitted was %q", format, gotStatus, setCookie)
			}
			otherPrefix := "/http://" + upstreamTarget.Host
			if format == "compatibility" {
				otherPrefix = pathTargetRoutePrefix + "http/" + upstreamTarget.Host
			}
			response, err = client.Get(gateway.URL + otherPrefix + "/protected")
			if err != nil {
				t.Fatal(err)
			}
			gotStatus = response.StatusCode
			drainProxyRegressionResponse(t, response)
			if gotStatus != http.StatusOK {
				t.Fatalf("switching from %s to the other documented format lost the session: %d", format, gotStatus)
			}
		})
	}
}
