package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareAssetCacheOversizeResponsePreservesBodyAndClose(t *testing.T) {
	for _, drain := range []bool{true, false} {
		name := "close_before_drain"
		if drain {
			name = "drain_then_close"
		}
		t.Run(name, func(t *testing.T) {
			cache := newAssetCache(t.TempDir())
			site := Site{ID: 91, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 32 << 20, AssetCacheRules: "*/file/*"}
			target, _ := url.Parse("https://media.example/file/poster.jpg")
			inbound := httptest.NewRequest(http.MethodGet, "https://proxy.example/file/poster.jpg", nil)
			tracker := newRequestDiagnosticsTracker(time.Now(), inbound)
			request := inbound.WithContext(context.WithValue(inbound.Context(), requestDiagnosticsContextKey{}, tracker))
			request, hit := prepareAssetCacheRequest(cache, site, request, target)
			if hit != nil {
				t.Fatal("oversize fixture unexpectedly had a cache hit")
			}
			length := maxAssetCacheObject + 128
			original := &assetCacheObservedBody{Reader: strings.NewReader(strings.Repeat("i", int(length)))}
			response := &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/jpeg"}},
				ContentLength: -1, Body: original, Request: request,
			}
			if err := prepareAssetCacheResponse(response, cache, site); err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if original.closed || original.reads != 0 || response.Header.Get("X-Meridian-Cache") != "MISS" || response.ContentLength != -1 {
				t.Fatalf("oversize stream lifecycle changed: closed=%v cache=%q length=%d", original.closed, response.Header.Get("X-Meridian-Cache"), response.ContentLength)
			}
			if drain {
				n, err := io.Copy(io.Discard, response.Body)
				if err != nil || n != length {
					t.Fatalf("oversize response was truncated: copied=%d want=%d err=%v", n, length, err)
				}
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if !original.closed {
				t.Fatal("closing the proxy response did not close the original upstream body")
			}
			diagnostics := tracker.snapshot(time.Now(), http.StatusOK, nil, nil)
			wantReason := "incomplete"
			if drain {
				wantReason = "too_large"
			}
			if diagnostics.CacheWrite != "skipped" || diagnostics.CacheWriteReason != wantReason || diagnostics.ResponseType != "image" {
				t.Fatalf("oversize/abandoned stream decision not reported: write=%q write_reason=%q type=%q", diagnostics.CacheWrite, diagnostics.CacheWriteReason, diagnostics.ResponseType)
			}
		})
	}
}

func TestPrepareAssetCacheResponseCacheWriteFailurePreservesUpstreamBody(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache-blocked-by-file")
	if err := os.WriteFile(cacheDir, []byte("not a cache directory"), 0600); err != nil {
		t.Fatal(err)
	}
	cache := newAssetCache(cacheDir)
	site := Site{ID: 93, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/file/*"}
	target, _ := url.Parse("https://media.example/file/poster.jpg")
	inbound := httptest.NewRequest(http.MethodGet, "https://proxy.example/file/poster.jpg", nil)
	tracker := newRequestDiagnosticsTracker(time.Now(), inbound)
	request := inbound.WithContext(context.WithValue(inbound.Context(), requestDiagnosticsContextKey{}, tracker))
	request, hit := prepareAssetCacheRequest(cache, site, request, target)
	if hit != nil {
		t.Fatal("unreadable cache directory unexpectedly yielded a hit")
	}
	response := &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/jpeg"}},
		ContentLength: -1, Body: io.NopCloser(strings.NewReader("upstream image")), Request: request,
	}
	if err := prepareAssetCacheResponse(response, cache, site); err != nil {
		t.Fatalf("a cache filesystem failure blocked normal forwarding: %v", err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(data) != "upstream image" || response.StatusCode != http.StatusOK {
		t.Fatalf("cache failure changed the upstream response: status=%d body=%q err=%v", response.StatusCode, data, err)
	}
	diagnostics := tracker.snapshot(time.Now(), http.StatusOK, nil, nil)
	if diagnostics.CacheLookup != "read_error" || diagnostics.CacheLookupReason != "io" || diagnostics.CacheWrite != "write_error" || diagnostics.CacheWriteReason != "io" || diagnostics.CacheReason != "io" {
		t.Fatalf("cache filesystem errors were hidden as a miss: lookup=%q lookup_reason=%q write=%q write_reason=%q reason=%q", diagnostics.CacheLookup, diagnostics.CacheLookupReason, diagnostics.CacheWrite, diagnostics.CacheWriteReason, diagnostics.CacheReason)
	}
}

func TestPrepareAssetCacheResponseDoesNotReportEvictedBodyAsStored(t *testing.T) {
	cache := newAssetCache(t.TempDir())
	site := Site{ID: 94, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 3, AssetCacheRules: "*/file/*"}
	target, _ := url.Parse("https://media.example/file/poster.jpg")
	inbound := httptest.NewRequest(http.MethodGet, "https://proxy.example/file/poster.jpg", nil)
	tracker := newRequestDiagnosticsTracker(time.Now(), inbound)
	ctx := context.WithValue(inbound.Context(), requestDiagnosticsContextKey{}, tracker)
	cacheReq := cache.request(site, inbound, target)
	request := inbound.WithContext(context.WithValue(ctx, assetCacheContextKey{}, cacheReq))
	response := &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/jpeg"}},
		ContentLength: 5, Body: io.NopCloser(strings.NewReader("image")), Request: request,
	}
	if err := prepareAssetCacheResponse(response, cache, site); err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	if hit, err := cache.read(cacheReq, time.Now()); err != nil || hit != nil {
		t.Fatalf("fixture did not evict its over-budget body: hit=%v err=%v", hit != nil, err)
	}
	diagnostics := tracker.snapshot(time.Now(), http.StatusOK, nil, nil)
	if diagnostics.CacheWrite != "skipped" || diagnostics.CacheReason != "evicted" {
		t.Fatalf("evicted response incorrectly reported as cached: write=%q reason=%q", diagnostics.CacheWrite, diagnostics.CacheReason)
	}
}

func TestPrepareAssetCacheRequestReportsEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, rules, rangeHeader string
		disabled, missingCache                 bool
		lookup, lookupReason, writeReason      string
	}{
		{name: "rule_no_match", method: http.MethodGet, path: "/file/poster.jpg", rules: "*/web/*", lookup: "bypass", lookupReason: "rule_no_match", writeReason: "rule_no_match"},
		{name: "disabled_site", method: http.MethodGet, path: "/file/poster.jpg", rules: "*/file/*", disabled: true, lookup: "disabled", lookupReason: "disabled", writeReason: "disabled"},
		{name: "missing_cache", method: http.MethodGet, path: "/file/poster.jpg", rules: "*/file/*", missingCache: true, lookup: "disabled", lookupReason: "disabled", writeReason: "disabled"},
		{name: "method", method: http.MethodPost, path: "/file/poster.jpg", rules: "*/file/*", lookup: "bypass", lookupReason: "method", writeReason: "method"},
		{name: "range", method: http.MethodGet, path: "/file/poster.jpg", rules: "*/file/*", rangeHeader: "bytes=0-9", lookup: "bypass", lookupReason: "range", writeReason: "range"},
		{name: "reserved_dynamic", method: http.MethodGet, path: "/_meridian/d/token", rules: "*", lookup: "bypass", lookupReason: "dynamic", writeReason: "dynamic"},
		{name: "media", method: http.MethodGet, path: "/file/movie.mp4", rules: "*/file/*", lookup: "bypass", lookupReason: "dynamic", writeReason: "dynamic"},
		{name: "miss", method: http.MethodGet, path: "/file/poster.jpg", rules: "*/file/*", lookup: "miss"},
		{name: "head_no_fill", method: http.MethodHead, path: "/file/poster.jpg", rules: "*/file/*", lookup: "miss", writeReason: "head_no_fill"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := newAssetCache(t.TempDir())
			if tc.missingCache {
				cache = nil
			}
			site := Site{ID: 95, AssetCacheEnabled: !tc.disabled, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: tc.rules}
			target, _ := url.Parse("https://media.example" + tc.path)
			inbound := httptest.NewRequest(tc.method, "https://proxy.example"+tc.path, nil)
			inbound.Header.Set("Range", tc.rangeHeader)
			tracker := newRequestDiagnosticsTracker(time.Now(), inbound)
			request := inbound.WithContext(context.WithValue(inbound.Context(), requestDiagnosticsContextKey{}, tracker))
			forwarded, hit := prepareAssetCacheRequest(cache, site, request, target)
			if hit != nil {
				t.Fatal("a request against an empty cache unexpectedly hit")
			}
			if tc.lookup != "miss" && forwarded != request {
				t.Fatal("a bypass changed the forwarded request")
			}
			diagnostics := tracker.snapshot(time.Now(), http.StatusOK, nil, nil)
			if diagnostics.CacheLookup != tc.lookup || diagnostics.CacheWrite != "not_attempted" || diagnostics.CacheLookupReason != tc.lookupReason || diagnostics.CacheWriteReason != tc.writeReason {
				t.Fatalf("eligibility decision not reported: lookup=%q write=%q lookup_reason=%q write_reason=%q", diagnostics.CacheLookup, diagnostics.CacheWrite, diagnostics.CacheLookupReason, diagnostics.CacheWriteReason)
			}
		})
	}
}

func TestPrepareAssetCacheRequestReportsBrokenEntryAndCanRefill(t *testing.T) {
	cache := newAssetCache(t.TempDir())
	site := Site{ID: 96, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/file/*"}
	target, _ := url.Parse("https://media.example/file/poster.jpg")
	inbound := httptest.NewRequest(http.MethodGet, "https://proxy.example/file/poster.jpg", nil)
	cacheReq := cache.request(site, inbound, target)
	initial := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/jpeg"}}}
	if err := cache.write(site, cacheReq, initial, []byte("old image"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheReq.metaPath, []byte("invalid metadata"), 0600); err != nil {
		t.Fatal(err)
	}
	tracker := newRequestDiagnosticsTracker(time.Now(), inbound)
	request := inbound.WithContext(context.WithValue(inbound.Context(), requestDiagnosticsContextKey{}, tracker))
	forwarded, hit := prepareAssetCacheRequest(cache, site, request, target)
	if hit != nil {
		t.Fatal("corrupt metadata was served as a cache hit")
	}
	before := tracker.snapshot(time.Now(), http.StatusOK, nil, nil)
	if before.CacheLookup != "read_error" || before.CacheLookupReason != "invalid_entry" {
		t.Fatalf("corrupt metadata was silently treated as a miss: lookup=%q lookup_reason=%q", before.CacheLookup, before.CacheLookupReason)
	}
	response := &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/jpeg"}},
		ContentLength: -1, Body: io.NopCloser(strings.NewReader("replacement")), Request: forwarded,
	}
	if err := prepareAssetCacheResponse(response, cache, site); err != nil {
		t.Fatalf("corrupt cache entry prevented forwarding/refill: %v", err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		t.Fatal(err)
	}
	after := tracker.snapshot(time.Now(), http.StatusOK, nil, nil)
	if after.CacheLookup != "read_error" || after.CacheLookupReason != "invalid_entry" || after.CacheWrite != "stored" || after.CacheReason != "invalid_entry" {
		t.Fatalf("successful refill erased the failed lookup: lookup=%q lookup_reason=%q write=%q reason=%q", after.CacheLookup, after.CacheLookupReason, after.CacheWrite, after.CacheReason)
	}
	refilled, err := cache.read(cacheReq, time.Now())
	if err != nil || refilled == nil || string(refilled.body) != "replacement" {
		t.Fatalf("corrupt entry did not become a valid refill: hit=%v err=%v", refilled != nil, err)
	}
}

func TestPrepareAssetCacheResponseReportsEmptyBodyAsSkipped(t *testing.T) {
	cache := newAssetCache(t.TempDir())
	site := Site{ID: 97, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/file/*"}
	target, _ := url.Parse("https://media.example/file/poster.jpg")
	inbound := httptest.NewRequest(http.MethodGet, "https://proxy.example/file/poster.jpg", nil)
	tracker := newRequestDiagnosticsTracker(time.Now(), inbound)
	request := inbound.WithContext(context.WithValue(inbound.Context(), requestDiagnosticsContextKey{}, tracker))
	request, hit := prepareAssetCacheRequest(cache, site, request, target)
	if hit != nil {
		t.Fatal("empty fixture unexpectedly had a cache hit")
	}
	response := &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/jpeg"}},
		ContentLength: 0, Body: http.NoBody, Request: request,
	}
	if err := prepareAssetCacheResponse(response, cache, site); err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	diagnostics := tracker.snapshot(time.Now(), http.StatusOK, nil, nil)
	if diagnostics.CacheLookup != "miss" || diagnostics.CacheWrite != "skipped" || diagnostics.CacheWriteReason != "empty_body" {
		t.Fatalf("empty response was mistaken for an attempted fill: lookup=%q write=%q write_reason=%q", diagnostics.CacheLookup, diagnostics.CacheWrite, diagnostics.CacheWriteReason)
	}
}

func TestPrepareAssetCacheResponseReadonlyFileReportsPermissionAndForwards(t *testing.T) {
	cache := newAssetCache(t.TempDir())
	site := Site{ID: 98, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/file/*"}
	target, _ := url.Parse("https://media.example/file/poster.jpg")
	inbound := httptest.NewRequest(http.MethodGet, "https://proxy.example/file/poster.jpg", nil)
	cacheReq := cache.request(site, inbound, target)
	if err := os.MkdirAll(filepath.Dir(cacheReq.bodyPath), 0700); err != nil {
		t.Fatal(err)
	}
	readonly := filepath.Dir(cacheReq.bodyPath)
	t.Cleanup(func() { _ = os.Chmod(readonly, 0700) })
	if err := os.Chmod(readonly, 0500); err != nil {
		t.Fatal(err)
	}
	probe, err := os.CreateTemp(readonly, "permission-probe-")
	if err == nil {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
		t.Skip("this test process can create files in read-only directories; permission fixture is unavailable")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("read-only fixture did not produce permission denial: %v", err)
	}
	tracker := newRequestDiagnosticsTracker(time.Now(), inbound)
	request := inbound.WithContext(context.WithValue(inbound.Context(), requestDiagnosticsContextKey{}, tracker))
	request, hit := prepareAssetCacheRequest(cache, site, request, target)
	if hit != nil {
		t.Fatal("read-only temporary file unexpectedly yielded a cache hit")
	}
	response := &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/jpeg"}},
		ContentLength: -1, Body: io.NopCloser(strings.NewReader("upstream image")), Request: request,
	}
	if err := prepareAssetCacheResponse(response, cache, site); err != nil {
		t.Fatalf("cache write permission denial blocked normal forwarding: %v", err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(data) != "upstream image" {
		t.Fatalf("cache permission denial changed response bytes: body=%q err=%v", data, err)
	}
	diagnostics := tracker.snapshot(time.Now(), http.StatusOK, nil, nil)
	if diagnostics.CacheLookup != "miss" || diagnostics.CacheWrite != "write_error" || diagnostics.CacheWriteReason != "permission" {
		t.Fatalf("cache permission denial was hidden: lookup=%q write=%q write_reason=%q", diagnostics.CacheLookup, diagnostics.CacheWrite, diagnostics.CacheWriteReason)
	}
}

func TestPrepareAssetCacheHEADMissDoesNotReadOrFillUpstreamBody(t *testing.T) {
	cache := newAssetCache(t.TempDir())
	site := Site{ID: 99, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/file/*"}
	target, _ := url.Parse("https://media.example/file/poster.jpg")
	inbound := httptest.NewRequest(http.MethodHead, "https://proxy.example/file/poster.jpg", nil)
	tracker := newRequestDiagnosticsTracker(time.Now(), inbound)
	request := inbound.WithContext(context.WithValue(inbound.Context(), requestDiagnosticsContextKey{}, tracker))
	request, hit := prepareAssetCacheRequest(cache, site, request, target)
	if hit != nil {
		t.Fatal("HEAD fixture unexpectedly had a cache hit")
	}
	original := &assetCacheObservedBody{Reader: strings.NewReader("image")}
	response := &http.Response{
		StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/jpeg"}, "Content-Length": {"5"}},
		ContentLength: 5, Body: original, Request: request,
	}
	if err := prepareAssetCacheResponse(response, cache, site); err != nil {
		t.Fatal(err)
	}
	if original.reads != 0 || original.closed || response.Body != original || response.ContentLength != 5 {
		t.Fatalf("HEAD miss touched/replaced the upstream body: reads=%d closed=%v same_body=%v length=%d", original.reads, original.closed, response.Body == original, response.ContentLength)
	}
	diagnostics := tracker.snapshot(time.Now(), http.StatusOK, nil, nil)
	if diagnostics.CacheLookup != "miss" || diagnostics.CacheWrite != "not_attempted" || diagnostics.CacheWriteReason != "head_no_fill" {
		t.Fatalf("HEAD fill decision was not reported: lookup=%q write=%q write_reason=%q", diagnostics.CacheLookup, diagnostics.CacheWrite, diagnostics.CacheWriteReason)
	}
}
