package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAssetCacheColdResponseForwardsPrefixBeforeTail(t *testing.T) {
	cache := newAssetCache(t.TempDir())
	site := Site{ID: 77, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/file/*"}
	request := httptest.NewRequest(http.MethodGet, "https://proxy.example/file/poster.jpg", nil)
	target, _ := url.Parse("https://media.example/file/poster.jpg")
	cacheReq := cache.request(site, request, target)
	tracker := newRequestDiagnosticsTracker(time.Now(), request)
	ctx := context.WithValue(request.Context(), requestDiagnosticsContextKey{}, tracker)
	request = request.WithContext(context.WithValue(ctx, assetCacheContextKey{}, cacheReq))
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/jpeg"}}, ContentLength: -1, Body: reader, Request: request}
	prepared := make(chan error, 1)
	go func() { prepared <- prepareAssetCacheResponse(response, cache, site) }()
	select {
	case err := <-prepared:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		_ = reader.Close()
		<-prepared
		t.Fatal("cache preparation waited for an upstream body instead of returning a streaming response")
	}
	defer response.Body.Close()
	prefixWritten := make(chan struct{})
	go func() { _, _ = io.WriteString(writer, "prefix"); close(prefixWritten) }()
	prefix := make([]byte, 6)
	if _, err := io.ReadFull(response.Body, prefix); err != nil || string(prefix) != "prefix" {
		t.Fatalf("prefix was not forwarded: %q err=%v", prefix, err)
	}
	<-prefixWritten
	if hit, err := cache.read(cacheReq, time.Now()); err != nil || hit != nil {
		t.Fatalf("partial cache entry became visible: hit=%v err=%v", hit != nil, err)
	}
	if value := tracker.snapshot(time.Now(), http.StatusOK, nil, nil); value.CacheWrite == "stored" {
		t.Fatal("partial response was reported stored")
	}
	go func() { _, _ = io.WriteString(writer, "tail"); _ = writer.Close() }()
	tail, err := io.ReadAll(response.Body)
	if err != nil || string(tail) != "tail" || response.ContentLength != -1 {
		t.Fatalf("tail/framing changed: %q length=%d err=%v", tail, response.ContentLength, err)
	}
	hit, err := cache.read(cacheReq, time.Now())
	if err != nil || hit == nil || string(hit.body) != "prefixtail" {
		t.Fatalf("complete stream was not published: hit=%v err=%v", hit != nil, err)
	}
	if value := tracker.snapshot(time.Now(), http.StatusOK, nil, nil); value.CacheWrite != "stored" {
		t.Fatalf("completed fill diagnostics=%+v", value)
	}
}

func newAssetCacheStreamFixture(t *testing.T, length int64, body io.ReadCloser) (*assetCache, Site, *assetCacheRequest, *http.Response, *requestDiagnosticsTracker) {
	t.Helper()
	cache := newAssetCache(t.TempDir())
	site := Site{ID: 78, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/file/*"}
	request := httptest.NewRequest(http.MethodGet, "https://proxy.example/file/poster.jpg", nil)
	target, _ := url.Parse("https://media.example/file/poster.jpg")
	cacheReq := cache.request(site, request, target)
	tracker := newRequestDiagnosticsTracker(time.Now(), request)
	ctx := context.WithValue(request.Context(), requestDiagnosticsContextKey{}, tracker)
	request = request.WithContext(context.WithValue(ctx, assetCacheContextKey{}, cacheReq))
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/jpeg"}}, ContentLength: length, Body: body, Request: request}
	return cache, site, cacheReq, response, tracker
}

func TestAssetCacheStreamRequiresCompleteDeclaredLength(t *testing.T) {
	for _, length := range []int64{4, 6} {
		t.Run(strings.Repeat("x", int(length)), func(t *testing.T) {
			cache, site, req, response, tracker := newAssetCacheStreamFixture(t, length, io.NopCloser(strings.NewReader("image")))
			if err := prepareAssetCacheResponse(response, cache, site); err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || string(data) != "image" || response.ContentLength != length {
				t.Fatalf("cache changed upstream bytes/framing: %q length=%d err=%v", data, response.ContentLength, err)
			}
			if hit, err := cache.read(req, time.Now()); err != nil || hit != nil {
				t.Fatalf("mismatched length was cached: hit=%v err=%v", hit != nil, err)
			}
			if got := tracker.snapshot(time.Now(), http.StatusOK, nil, nil); got.CacheWriteReason != "length_mismatch" {
				t.Fatalf("mismatched length diagnostics=%+v", got)
			}
		})
	}
}

func assertAssetCacheNoTemporaryFiles(t *testing.T, cache *assetCache) {
	t.Helper()
	err := filepath.WalkDir(cache.dir, func(name string, entry fs.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.Contains(entry.Name(), ".tmp") {
			t.Errorf("abandoned cache temporary file remains: %s", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAssetCacheStreamAbandonsIncompleteCanceledAndFailedFills(t *testing.T) {
	for _, decision := range []string{"close", "cancel", "sink_error", "clear", "generation"} {
		t.Run(decision, func(t *testing.T) {
			cache, site, req, response, tracker := newAssetCacheStreamFixture(t, 5, io.NopCloser(strings.NewReader("image")))
			ctx, cancel := context.WithCancel(response.Request.Context())
			defer cancel()
			response.Request = response.Request.WithContext(ctx)
			if err := prepareAssetCacheResponse(response, cache, site); err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			prefix := make([]byte, 2)
			if _, err := io.ReadFull(response.Body, prefix); err != nil || string(prefix) != "im" {
				t.Fatal("stream prefix changed", err)
			}
			wantReason := "incomplete"
			wantWrite := "skipped"
			switch decision {
			case "close":
				_ = response.Body.Close()
			case "cancel":
				cancel()
				wantReason = "canceled"
			case "sink_error":
				// Inject a filesystem write failure after the response started.
				fill := response.Body.(*assetCacheFillBody)
				if err := fill.file.Close(); err != nil {
					t.Fatal(err)
				}
				wantWrite, wantReason = "write_error", "io"
			case "clear":
				if err := cache.clear(); err != nil {
					t.Fatal(err)
				}
				wantReason = "generation_changed"
			case "generation":
				if err := cache.gcSiteGenerations(assetCacheSitePrefix(site), assetCacheSitePrefix(site)+"-config-next"); err != nil {
					t.Fatal(err)
				}
				wantReason = "generation_changed"
			}
			if decision != "close" {
				tail, err := io.ReadAll(response.Body)
				if err != nil || string(tail) != "age" {
					t.Fatalf("cache lifecycle changed the upstream tail: body=%q err=%v", tail, err)
				}
				_ = response.Body.Close()
			}
			if hit, err := cache.read(req, time.Now()); err != nil || hit != nil {
				t.Fatalf("abandoned fill was published: hit=%v err=%v", hit != nil, err)
			}
			if got := tracker.snapshot(time.Now(), http.StatusOK, nil, nil); got.CacheWrite != wantWrite || got.CacheWriteReason != wantReason {
				t.Fatalf("abandoned fill diagnostics=%+v want %s/%s", got, wantWrite, wantReason)
			}
			assertAssetCacheNoTemporaryFiles(t, cache)
		})
	}
}

type cacheEOFWithDataBody struct {
	err    error
	read   bool
	closed atomic.Int32
}

func (b *cacheEOFWithDataBody) Read(p []byte) (int, error) {
	if b.read {
		return 0, io.EOF
	}
	b.read = true
	return copy(p, "image"), b.err
}

func (b *cacheEOFWithDataBody) Close() error { b.closed.Add(1); return nil }

func TestAssetCacheStreamHandlesFinalDataAndReadErrorWithoutChangingUpstream(t *testing.T) {
	for _, succeeds := range []bool{false, true} {
		t.Run(fmt.Sprintf("eof=%t", succeeds), func(t *testing.T) {
			readErr := errors.New("fixture upstream interruption")
			if succeeds {
				readErr = io.EOF
			}
			original := &cacheEOFWithDataBody{err: readErr}
			cache, site, req, response, _ := newAssetCacheStreamFixture(t, 5, original)
			if err := prepareAssetCacheResponse(response, cache, site); err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			_ = response.Body.Close()
			if string(data) != "image" || succeeds && err != nil || !succeeds && !errors.Is(err, readErr) || original.closed.Load() != 1 {
				t.Fatalf("upstream lifecycle changed: body=%q err=%v closes=%d", data, err, original.closed.Load())
			}
			hit, err := cache.read(req, time.Now())
			if err != nil || (hit != nil) != succeeds {
				t.Fatalf("EOF/error publication wrong: hit=%v err=%v", hit != nil, err)
			}
			assertAssetCacheNoTemporaryFiles(t, cache)
		})
	}
}

func TestAssetCacheStreamCloseInterruptsPendingRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	cache, site, _, response, _ := newAssetCacheStreamFixture(t, -1, reader)
	if err := prepareAssetCacheResponse(response, cache, site); err != nil {
		t.Fatal(err)
	}
	readDone := make(chan struct{})
	go func() { _, _ = response.Body.Read(make([]byte, 8)); close(readDone) }()
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("cache body Close did not interrupt the upstream Read")
	}
	assertAssetCacheNoTemporaryFiles(t, cache)
}

func TestAssetCacheStreamMetadataFailureKeepsPreviousCompleteEntry(t *testing.T) {
	cache, site, req, response, tracker := newAssetCacheStreamFixture(t, 5, io.NopCloser(strings.NewReader("image")))
	if err := cache.write(site, req, response, []byte("old"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := prepareAssetCacheResponse(response, cache, site); err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	fill := response.Body.(*assetCacheFillBody)
	if err := fill.root.Mkdir(fill.tempName+".json", 0700); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil || string(data) != "image" {
		t.Fatalf("metadata sink failure changed response: body=%q err=%v", data, err)
	}
	hit, err := cache.read(req, time.Now())
	if err != nil || hit == nil || string(hit.body) != "old" {
		t.Fatalf("failed fill damaged previous complete entry: hit=%v err=%v", hit, err)
	}
	if got := tracker.snapshot(time.Now(), http.StatusOK, nil, nil); got.CacheWrite != "write_error" {
		t.Fatalf("metadata failure did not record final cache write failure: %+v", got)
	}
	assertAssetCacheNoTemporaryFiles(t, cache)
}

func TestAssetCacheConcurrentColdFillsPublishOnlyCompleteObjects(t *testing.T) {
	cache, site, req, first, _ := newAssetCacheStreamFixture(t, 5, io.NopCloser(strings.NewReader("first")))
	second := *first
	second.Body = io.NopCloser(strings.NewReader("second"))
	second.ContentLength = 6
	for _, response := range []*http.Response{first, &second} {
		if err := prepareAssetCacheResponse(response, cache, site); err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if _, err := io.ReadFull(response.Body, make([]byte, 2)); err != nil {
			t.Fatal(err)
		}
	}
	for _, response := range []*http.Response{first, &second} {
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
	}
	hit, err := cache.read(req, time.Now())
	if err != nil || hit == nil || string(hit.body) != "second" || hit.meta.Size != 6 {
		t.Fatalf("concurrent fill publication mixed objects: hit=%v err=%v", hit, err)
	}
	assertAssetCacheNoTemporaryFiles(t, cache)
}

func TestAssetCacheProxyColdFirstBodyDoesNotWaitForTail(t *testing.T) {
	tail := make(chan struct{})
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=60")
		_, _ = io.WriteString(w, "prefix")
		_ = http.NewResponseController(w).Flush()
		select {
		case <-tail:
			_, _ = io.WriteString(w, "tail")
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	app := newTestApp(t)
	app.pm.SetAssetCache(newAssetCache(t.TempDir()))
	port := freePort(t)
	releasePort(port)
	site := Site{ID: 778, Name: "streaming-cold-cache", ListenPort: port, IngressMode: ingressModePort, TargetURL: upstream.URL, PlaybackMode: "direct", StreamHosts: "[]", UAMode: passthroughUAMode, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/file/*"}
	if err := app.pm.StartSite(site); err != nil {
		t.Fatal(err)
	}
	defer app.pm.StopSite(site.ID)
	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	target := "http://127.0.0.1:" + strconv.Itoa(port) + "/file/poster.jpg"
	response, err := client.Get(target)
	if err != nil {
		t.Fatal("cold response headers waited for the tail:", err)
	}
	defer response.Body.Close()
	prefix := make([]byte, 6)
	if _, err := io.ReadFull(response.Body, prefix); err != nil || string(prefix) != "prefix" {
		t.Fatal("cold response prefix waited for tail", err)
	}
	close(tail)
	if data, err := io.ReadAll(response.Body); err != nil || string(data) != "tail" || response.ContentLength != -1 {
		t.Fatalf("stream body or framing changed: body=%q length=%d err=%v", data, response.ContentLength, err)
	}
	warm, err := client.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer warm.Body.Close()
	data, err := io.ReadAll(warm.Body)
	if err != nil || string(data) != "prefixtail" || warm.Header.Get("X-Meridian-Cache") != "HIT" || requests.Load() != 1 {
		t.Fatalf("complete cold response did not become a warm HIT: body=%q hit=%q requests=%d err=%v", data, warm.Header.Get("X-Meridian-Cache"), requests.Load(), err)
	}
}
