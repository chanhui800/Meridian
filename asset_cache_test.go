package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestAssetCacheEligibilityExcludesPlaybackAndSensitiveRequests(t *testing.T) {
	site := Site{AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/file/*\n*/emby/Items/*/Images/*\n*/web/*"}
	base, _ := url.Parse("https://media.example")
	cases := []struct {
		path        string
		rangeHeader string
		want        bool
	}{
		{path: "/emby/Items/1/Images/Primary.jpg", want: true},
		{path: "/web/app.js", want: true},
		{path: "/Videos/1/stream.mp4", want: false},
		{path: "/live/master.m3u8", want: false},
		{path: "/_meridian/d/token", want: false},
		{path: "/emby/Items/1/Images/Primary.jpg", rangeHeader: "bytes=0-9", want: false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "https://proxy.example"+tc.path, nil)
		req.Header.Set("Range", tc.rangeHeader)
		if got := assetCacheRequestEligible(site, req, assetCacheTargetURL(req, base)); got != tc.want {
			t.Fatalf("path=%s range=%q eligible=%v want=%v", tc.path, tc.rangeHeader, got, tc.want)
		}
	}
}

func TestAssetCacheReadWriteAndIdentityIsolation(t *testing.T) {
	cache := newAssetCache(t.TempDir())
	site := Site{ID: 7, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/emby/Items/*/Images/*"}
	target, _ := url.Parse("https://media.example/emby/Items/1/Images/Primary.jpg")
	request := httptest.NewRequest(http.MethodGet, "https://proxy.example/emby/Items/1/Images/Primary.jpg", nil)
	request.Header.Set("Authorization", "Bearer user-one")
	cacheReq := cache.request(site, request, target)
	response := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"image/jpeg"}, "Cache-Control": []string{"public, max-age=60"}},
		ContentLength: 5,
	}
	now := time.Now()
	if err := cache.write(site, cacheReq, response, []byte("image"), now); err != nil {
		t.Fatal(err)
	}
	hit, err := cache.read(cacheReq, now.Add(time.Second))
	if err != nil || hit == nil || string(hit.body) != "image" {
		t.Fatalf("hit=%#v err=%v", hit, err)
	}
	other := httptest.NewRequest(http.MethodGet, request.URL.String(), nil)
	other.Header.Set("Authorization", "Bearer user-two")
	if cache.request(site, other, target).key == cacheReq.key {
		t.Fatal("cache key did not isolate authenticated identities")
	}
}

func TestAssetCacheRejectsPrivateCookieAndMediaResponses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header http.Header
	}{
		{name: "private", header: http.Header{"Content-Type": []string{"image/jpeg"}, "Cache-Control": []string{"private"}}},
		{name: "cookie", header: http.Header{"Content-Type": []string{"image/jpeg"}, "Set-Cookie": []string{"session=secret"}}},
		{name: "video", header: http.Header{"Content-Type": []string{"video/mp4"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: http.StatusOK, Header: tc.header}
			if assetCacheResponseEligible(resp, []byte("body")) {
				t.Fatal("unsafe response was cacheable")
			}
		})
	}
}

func TestAssetCacheEvictsLeastRecentlyUsedEntryPerSite(t *testing.T) {
	cache := newAssetCache(t.TempDir())
	site := Site{ID: 9, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 7, AssetCacheRules: "*/web/*"}
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/css"}}}
	now := time.Now()

	makeRequest := func(name string) *assetCacheRequest {
		target, _ := url.Parse("https://media.example/web/" + name + ".css")
		request := httptest.NewRequest(http.MethodGet, "https://proxy.example/web/"+name+".css", nil)
		return cache.request(site, request, target)
	}
	oldest := makeRequest("oldest")
	newest := makeRequest("newest")
	if err := cache.write(site, oldest, response, []byte("old"), now); err != nil {
		t.Fatal(err)
	}
	if err := cache.write(site, newest, response, []byte("newer"), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldest.bodyPath); !os.IsNotExist(err) {
		t.Fatalf("oldest body should be evicted, stat err=%v", err)
	}
	if _, err := os.Stat(newest.bodyPath); err != nil {
		t.Fatalf("newest body should remain: %v", err)
	}
}

func TestAssetCacheProxyHitAndMediaBypass(t *testing.T) {
	var imageRequests atomic.Int64
	var mediaRequests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Ext(r.URL.Path) {
		case ".jpg":
			imageRequests.Add(1)
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("Content-Length", "5")
			_, _ = w.Write([]byte("image"))
		case ".mp4":
			mediaRequests.Add(1)
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Length", "5")
			_, _ = w.Write([]byte("video"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	app := newTestApp(t)
	app.pm.SetAssetCache(newAssetCache(t.TempDir()))
	port := freePort(t)
	releasePort(port)
	site := Site{
		ID: 19, Name: "cache-proxy", ListenPort: port, IngressMode: ingressModePort,
		TargetURL: upstream.URL, PlaybackMode: "direct", StreamHosts: "[]", UAMode: passthroughUAMode,
		AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/file/*\n*/emby/Items/*/Images/*",
	}
	if err := app.pm.StartSite(site); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	if app.pm.assetCache == nil {
		t.Fatal("proxy manager cache is nil")
	}
	probeRequest := httptest.NewRequest(http.MethodGet, "http://proxy.example/emby/Items/1/Images/Primary.jpg", nil)
	upstreamURL, _ := url.Parse(upstream.URL)
	probeTarget := assetCacheTargetURL(probeRequest, upstreamURL)
	if cacheReq := app.pm.assetCache.request(site, probeRequest, probeTarget); cacheReq == nil {
		t.Fatalf("image request was not eligible: target=%s rule=%v reserved=%v redirect=%v info=%v structured=%q ext=%q", probeTarget, assetCacheRuleMatches(site.AssetCacheRules, probeTarget), isReservedDynamicRoute(probeRequest.URL.Path), isPlaybackRedirectEndpoint(probeRequest.URL.Path), isPlaybackInfoRequest(probeRequest.URL.Path), dynamicStructuredRequestSource(probeRequest.URL.Path), filepath.Ext(probeTarget.Path))
	}

	client := &http.Client{Timeout: 3 * time.Second}
	get := func(path string) *http.Response {
		response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return response
	}
	if response := get("/emby/Items/1/Images/Primary.jpg"); response.Header.Get("X-Meridian-Cache") != "MISS" {
		t.Fatalf("first image cache header=%q want MISS", response.Header.Get("X-Meridian-Cache"))
	}
	if response := get("/emby/Items/1/Images/Primary.jpg"); response.Header.Get("X-Meridian-Cache") != "HIT" {
		t.Fatalf("second image cache header=%q want HIT", response.Header.Get("X-Meridian-Cache"))
	}
	if got := imageRequests.Load(); got != 1 {
		t.Fatalf("image upstream requests=%d want 1", got)
	}
	get("/file/movie.mp4")
	get("/file/movie.mp4")
	if got := mediaRequests.Load(); got != 2 {
		t.Fatalf("media upstream requests=%d want 2", got)
	}
}
