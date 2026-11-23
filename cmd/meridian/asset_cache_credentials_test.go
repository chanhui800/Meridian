package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func cacheCredentialPNG(t *testing.T, red byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	avatar := image.NewRGBA(image.Rect(0, 0, 2, 2))
	avatar.SetRGBA(0, 0, color.RGBA{R: red, G: 25, A: 255})
	if err := png.Encode(&encoded, avatar); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func newCacheCredentialGateway(t *testing.T, origin string) (*httptest.Server, *assetCache, *Site) {
	t.Helper()
	app := newTestApp(t)
	cache := newAssetCache(t.TempDir())
	app.pm.SetAssetCache(cache)
	target, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{
		Name: "credential cache", ListenPort: 8102,
		PublicHost: "credential-cache.example.test", IngressMode: ingressModeHost,
		TargetURL: origin, PlaybackMode: "direct", StreamHosts: "[]", UAMode: passthroughUAMode,
		AssetCacheEnabled: true, AssetCacheTTLSec: 60, AssetCacheMaxBytes: 1 << 20, AssetCacheRules: target.Host + "/*",
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
		t.Fatal("cache route missing")
	}
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)
	return gateway, cache, site
}

func TestAssetCacheSeparatesAllRepeatedAuthenticationFields(t *testing.T) {
	for _, name := range []string{"Authorization", "X-Emby-Authorization", "X-Emby-Token", "X-MediaBrowser-Token"} {
		t.Run(name, func(t *testing.T) {
			alice, bob := cacheCredentialPNG(t, 100), cacheCredentialPNG(t, 150)
			var calls atomic.Int64
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				values := r.Header.Values(name)
				if len(values) != 2 {
					http.Error(w, "missing credentials", http.StatusUnauthorized)
					return
				}
				body := alice
				if strings.Contains(values[1], "bob") {
					body = bob
				}
				w.Header().Set("Content-Type", "image/png")
				_, _ = w.Write(body)
			}))
			t.Cleanup(origin.Close)
			gateway, _, _ := newCacheCredentialGateway(t, origin.URL)
			for _, user := range []string{"alice", "bob", "alice"} {
				request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, gateway.URL+"/avatar.png", nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header[name] = []string{"shared", user}
				response, err := gateway.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(response.Body)
				_ = response.Body.Close()
				want := alice
				if user == "bob" {
					want = bob
				}
				if err != nil || response.StatusCode != http.StatusOK || !bytes.Equal(body, want) {
					t.Fatalf("%s user %s cache=%s status=%d err=%v returned another identity's image", name, user, response.Header.Get("X-Meridian-Cache"), response.StatusCode, err)
				}
			}
			if calls.Load() != 2 {
				t.Fatalf("repeated identity did not reuse its cache: calls=%d", calls.Load())
			}
		})
	}
}

func TestAssetCacheUpgradeDoesNotServeLegacyAnonymousPollution(t *testing.T) {
	legacyPrivate, current := cacheCredentialPNG(t, 100), cacheCredentialPNG(t, 150)
	var calls atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(current)
	}))
	t.Cleanup(origin.Close)
	gateway, cache, site := newCacheCredentialGateway(t, origin.URL)
	// The released v1 cache key allowed an empty first Cookie field to conceal
	// an authenticated second field in the anonymous namespace. Provision that
	// historical on-disk object as an upgrade fixture, then use only HTTP to
	// verify it cannot be served by the new runtime.
	namespace := assetCacheNamespace(*site)
	legacyRaw := strings.Join([]string{namespace, origin.URL + "/avatar.png", "", "", "anonymous"}, "\n")
	digest := sha256.Sum256([]byte(legacyRaw))
	key := fmt.Sprintf("%x", digest[:])
	directory := filepath.Join(namespace, key[:2])
	legacy := &assetCacheRequest{key: key, method: http.MethodGet, namespace: namespace,
		metaName: filepath.Join(directory, key+".json"), bodyName: filepath.Join(directory, key+".body")}
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/png"}}, ContentLength: int64(len(legacyPrivate))}
	if err := cache.write(*site, legacy, response, legacyPrivate, time.Now()); err != nil {
		t.Fatal(err)
	}
	for index := range 2 {
		response, err := gateway.Client().Get(gateway.URL + "/avatar.png")
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		wantCache := "MISS"
		if index == 1 {
			wantCache = "HIT"
		}
		if err != nil || !bytes.Equal(body, current) || response.Header.Get("X-Meridian-Cache") != wantCache || calls.Load() != 1 {
			t.Fatalf("legacy private object crossed upgrade boundary: cache=%s calls=%d err=%v", response.Header.Get("X-Meridian-Cache"), calls.Load(), err)
		}
	}
}
