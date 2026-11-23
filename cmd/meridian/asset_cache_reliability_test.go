package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAssetCacheFixedHeaderUpdateThroughAPIInvalidatesContent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte(r.Header.Get("X-Theme")))
	}))
	defer upstream.Close()
	app := newTestApp(t)
	app.pm.SetAssetCache(newAssetCache(t.TempDir()))
	port := freePort(t)
	releasePort(port)
	payload := map[string]any{
		"name": "cache-config-update", "listen_port": port, "target_url": upstream.URL,
		"asset_cache_enabled": true, "asset_cache_ttl_sec": 3600, "asset_cache_max_bytes": 16 << 20, "asset_cache_rules": "*/file/*",
		"upstream_headers": []map[string]string{{"name": "X-Theme", "value": "blue"}},
	}
	body, _ := json.Marshal(payload)
	created := httptest.NewRecorder()
	app.handleSites(created, httptest.NewRequest(http.MethodPost, "/api/sites", bytes.NewReader(body)))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var site Site
	if err := json.Unmarshal(created.Body.Bytes(), &site); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	client := &http.Client{Timeout: 3 * time.Second}
	get := func(wantBody, wantCache string) {
		t.Helper()
		resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/file/theme.css")
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || string(data) != wantBody || resp.Header.Get("X-Meridian-Cache") != wantCache {
			t.Fatalf("body=%q cache=%q err=%v, want %q/%s", data, resp.Header.Get("X-Meridian-Cache"), err, wantBody, wantCache)
		}
	}
	get("blue", "MISS")
	get("blue", "HIT")
	payload["upstream_headers"] = []map[string]string{{"name": "X-Theme", "value": "red"}}
	body, _ = json.Marshal(payload)
	updated := httptest.NewRecorder()
	app.handleSiteByID(updated, httptest.NewRequest(http.MethodPut, "/api/sites/"+strconv.FormatInt(site.ID, 10), bytes.NewReader(body)))
	if updated.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updated.Code, updated.Body.String())
	}
	get("red", "MISS")
	get("red", "HIT")
}

func TestAssetCacheControllerGenerationIgnoresPresentationAndCounters(t *testing.T) {
	site := Site{
		ID: 41, Name: "first", TargetURL: "https://origin.example", PlaybackMode: "direct", StreamHosts: "[]",
		UAMode: passthroughUAMode, StoredUpstreamHeaders: "[]", AssetCacheEnabled: true,
		AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/file/*",
	}
	first := assetCacheNamespace(site)
	copy := site
	copy.Name, copy.IconName, copy.IconURL = "renamed", "new-icon", "https://icons.example/new.png"
	copy.SortOrder, copy.TrafficUsed, copy.MediaCountUpdatedMS = 8, 12345, time.Now().UnixMilli()
	copy.UpdatedAt = time.Now().String()
	if got := assetCacheNamespace(copy); got != first {
		t.Fatalf("presentation/counter update invalidated cache: %q => %q", first, got)
	}
	copy = site
	copy.UAMode, copy.CustomUserAgent, copy.CustomClient, copy.CustomVersion = customUAMode, "custom-agent", "Custom", "1.0"
	if assetCacheNamespace(copy) == first {
		t.Fatal("upstream UA configuration update reused prior content generation")
	}
	copy = site
	copy.AssetCacheTTLSec = 1
	if assetCacheNamespace(copy) == first {
		t.Fatal("TTL reduction reused entries with prior expiry")
	}
	if assetCacheNamespacePrefix(first) != "site-41" {
		t.Fatalf("generation changed stable budget namespace: %q", first)
	}
}

type assetCacheObservedBody struct {
	io.Reader
	reads  int
	closed bool
}

func (body *assetCacheObservedBody) Read(p []byte) (int, error) {
	body.reads++
	return body.Reader.Read(p)
}

func (body *assetCacheObservedBody) Close() error {
	body.closed = true
	return nil
}

func TestPrepareAssetCacheResponseBypassesBeforeReadingUnsafeBodies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		header http.Header
		reason string
	}{
		{name: "no-store", status: http.StatusOK, header: http.Header{"Content-Type": {"image/jpeg"}, "Cache-Control": {"public", "no-store"}}, reason: "no_store"},
		{name: "private", status: http.StatusOK, header: http.Header{"Content-Type": {"text/css"}, "Cache-Control": {"private"}}, reason: "private"},
		{name: "no-cache", status: http.StatusOK, header: http.Header{"Content-Type": {"image/jpeg"}, "Cache-Control": {"no-cache"}}, reason: "no_cache"},
		{name: "set-cookie", status: http.StatusOK, header: http.Header{"Content-Type": {"image/jpeg"}, "Set-Cookie": {"session=secret"}}, reason: "set_cookie"},
		{name: "json", status: http.StatusOK, header: http.Header{"Content-Type": {"application/json"}}, reason: "mime"},
		{name: "video", status: http.StatusOK, header: http.Header{"Content-Type": {"video/mp4"}}, reason: "mime"},
		{name: "unsupported-vary", status: http.StatusOK, header: http.Header{"Content-Type": {"text/css"}, "Vary": {"Accept-Encoding", "X-User-ID"}}, reason: "vary"},
		{name: "error", status: http.StatusBadGateway, header: http.Header{"Content-Type": {"text/css"}}, reason: "response_status"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := newAssetCache(t.TempDir())
			site := Site{ID: 31, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/file/*"}
			target, _ := url.Parse("https://media.example/file/asset")
			inbound := httptest.NewRequest(http.MethodGet, "https://proxy.example/file/asset", nil)
			cacheReq := cache.request(site, inbound, target)
			tracker := newRequestDiagnosticsTracker(time.Now(), inbound)
			ctx := context.WithValue(inbound.Context(), requestDiagnosticsContextKey{}, tracker)
			request := inbound.WithContext(context.WithValue(ctx, assetCacheContextKey{}, cacheReq))
			original := &assetCacheObservedBody{Reader: strings.NewReader("response body")}
			response := &http.Response{StatusCode: tc.status, Header: tc.header, ContentLength: -1, Body: original, Request: request}
			if err := prepareAssetCacheResponse(response, cache, site); err != nil {
				t.Fatal(err)
			}
			if original.reads != 0 || original.closed || response.Body != original || response.ContentLength != -1 {
				t.Fatalf("uncacheable stream was buffered/replaced: reads=%d closed=%v same_body=%v length=%d", original.reads, original.closed, response.Body == original, response.ContentLength)
			}
			if got := response.Header.Get("X-Meridian-Cache"); got != "BYPASS" {
				t.Fatalf("cache=%q, want BYPASS", got)
			}
			diagnostics := tracker.snapshot(time.Now(), tc.status, nil, nil)
			if diagnostics.CacheWrite != "skipped" || diagnostics.CacheWriteReason != tc.reason {
				t.Fatalf("unsafe response lacks its fixed reason: write=%q write_reason=%q want=%q", diagnostics.CacheWrite, diagnostics.CacheWriteReason, tc.reason)
			}
			data, err := io.ReadAll(response.Body)
			if err != nil || string(data) != "response body" {
				t.Fatalf("stream no longer readable: body=%q err=%v", data, err)
			}
		})
	}
}

func TestAssetCacheHEADHitSkipsBodyAndPreservesLength(t *testing.T) {
	cache := newAssetCache(t.TempDir())
	site := Site{ID: 51, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/file/*"}
	target, _ := url.Parse("https://media.example/file/poster.jpg")
	request := httptest.NewRequest(http.MethodGet, "https://proxy.example/file/poster.jpg", nil)
	cacheReq := cache.request(site, request, target)
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/jpeg"}}}
	body := bytes.Repeat([]byte{'x'}, 2<<20)
	now := time.Now()
	if err := cache.write(site, cacheReq, response, body, now); err != nil {
		t.Fatal(err)
	}
	head := httptest.NewRequest(http.MethodHead, request.URL.String(), nil)
	headReq := cache.request(site, head, target)
	hit, err := cache.read(headReq, now.Add(time.Second))
	if err != nil || hit == nil {
		t.Fatalf("HEAD hit=%v err=%v", hit != nil, err)
	}
	if len(hit.body) != 0 {
		t.Fatalf("HEAD unnecessarily read %d cached body bytes", len(hit.body))
	}
	recorder := httptest.NewRecorder()
	serveAssetCacheHit(recorder, head, hit)
	if recorder.Body.Len() != 0 || recorder.Header().Get("Content-Length") != strconv.Itoa(len(body)) || recorder.Header().Get("X-Meridian-Cache") != "HIT" {
		t.Fatalf("HEAD body=%d headers=%v", recorder.Body.Len(), recorder.Header())
	}
	if err := os.Truncate(cacheReq.bodyPath, int64(len(body)-1)); err != nil {
		t.Fatal(err)
	}
	if invalid, err := cache.read(headReq, now.Add(2*time.Second)); err == nil || invalid != nil {
		t.Fatalf("HEAD must reject mismatched body length with a cache read error: hit=%v err=%v", invalid != nil, err)
	}
}

func TestAssetCacheHotEntryKeepsRecencyAcrossReconcileWithoutPerHitMetadataWrite(t *testing.T) {
	cache := newAssetCache(t.TempDir())
	site := Site{ID: 52, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 6, AssetCacheRules: "*/file/*"}
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/css"}}}
	now := time.Now()
	makeReq := func(name string) *assetCacheRequest {
		target, _ := url.Parse("https://media.example/file/" + name + ".css")
		return cache.request(site, httptest.NewRequest(http.MethodGet, "https://proxy.example/file/"+name+".css", nil), target)
	}
	hot, cold, newest := makeReq("hot"), makeReq("cold"), makeReq("new")
	if err := cache.write(site, hot, response, []byte("css"), now); err != nil {
		t.Fatal(err)
	}
	if err := cache.write(site, cold, response, []byte("css"), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(hot.metaPath)
	if err != nil {
		t.Fatal(err)
	}
	if hit, err := cache.read(hot, now.Add(2*time.Second)); err != nil || hit == nil {
		t.Fatalf("hot hit=%v err=%v", hit != nil, err)
	}
	after, err := os.ReadFile(hot.metaPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("one hit rewrote on-disk metadata: changed=%v err=%v", !bytes.Equal(before, after), err)
	}
	if err := cache.reconcileSizeIndex(); err != nil {
		t.Fatal(err)
	}
	if err := cache.write(site, newest, response, []byte("css"), now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cold.bodyPath); !os.IsNotExist(err) {
		t.Fatalf("reconcile lost in-memory recency: cold stat=%v", err)
	}
	if _, err := os.Stat(hot.bodyPath); err != nil {
		t.Fatalf("hot entry disappeared after reconcile: %v", err)
	}
}

func TestAssetCacheReadSnapshotAndFailedOldReaderKeepReplacement(t *testing.T) {
	cache := newAssetCache(t.TempDir())
	site := Site{ID: 53, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/file/*"}
	target, _ := url.Parse("https://media.example/file/poster.jpg")
	request := httptest.NewRequest(http.MethodGet, "https://proxy.example/file/poster.jpg", nil)
	cacheReq := cache.request(site, request, target)
	now := time.Now()
	write := func(value string) {
		t.Helper()
		response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/jpeg"}, "Etag": {value}}}
		if err := cache.write(site, cacheReq, response, []byte(value), now); err != nil {
			t.Fatal(err)
		}
	}
	write("old")
	old, err := cache.openReadSnapshot(cacheReq, now.Add(time.Second))
	if err != nil || old == nil || old.body == nil {
		t.Fatalf("open snapshot=%v err=%v", old != nil, err)
	}
	defer old.body.Close()
	write("new")
	body, err := io.ReadAll(old.body)
	if err != nil || string(body) != "old" || strings.Join(old.meta.Headers["ETag"], "") != "old" {
		t.Fatalf("replacement mixed old metadata/body: body=%q etag=%q err=%v", body, old.meta.Headers["ETag"], err)
	}
	_ = old.body.Close()
	if _, err := io.ReadAll(old.body); err == nil {
		t.Fatal("closed old descriptor unexpectedly readable")
	}
	cache.removeReadEntryIfCurrent(cacheReq, old.bodyInfo)
	hit, err := cache.read(cacheReq, now.Add(2*time.Second))
	if err != nil || hit == nil || string(hit.body) != "new" || strings.Join(hit.meta.Headers["ETag"], "") != "new" {
		t.Fatalf("failed old reader removed replacement: hit=%#v err=%v", hit, err)
	}
	if _, total, err := cache.sizeBySite(); err != nil || total != 3 {
		t.Fatalf("replacement accounting total=%d err=%v", total, err)
	}
}

func TestAssetCacheMetadataAccessPersistsPeriodically(t *testing.T) {
	cache := newAssetCache(t.TempDir())
	site := Site{ID: 54, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/file/*"}
	target, _ := url.Parse("https://media.example/file/theme.css")
	request := httptest.NewRequest(http.MethodHead, "https://proxy.example/file/theme.css", nil)
	cacheReq := cache.request(site, request, target)
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/css"}}}
	now := time.Now()
	if err := cache.write(site, cacheReq, response, []byte("css"), now); err != nil {
		t.Fatal(err)
	}
	access := now.Add(assetCacheAccessPersistInterval + time.Second)
	if hit, err := cache.read(cacheReq, access); err != nil || hit == nil {
		t.Fatalf("periodic hit=%v err=%v", hit != nil, err)
	}
	data, err := os.ReadFile(cacheReq.metaPath)
	if err != nil {
		t.Fatal(err)
	}
	var meta assetCacheMeta
	if err := json.Unmarshal(data, &meta); err != nil || meta.AccessedAtMS != access.UnixMilli() {
		t.Fatalf("recency was not persisted: accessed=%d want=%d err=%v", meta.AccessedAtMS, access.UnixMilli(), err)
	}
	restarted := newAssetCache(cache.dir)
	if hit, err := restarted.read(cacheReq, access.Add(time.Second)); err != nil || hit == nil {
		t.Fatalf("restarted cache hit=%v err=%v", hit != nil, err)
	}
	after, err := os.ReadFile(cacheReq.metaPath)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatalf("restart caused another immediate metadata write: changed=%v err=%v", !bytes.Equal(data, after), err)
	}
}

func TestAssetCacheConcurrentReadWriteClearAndReconcile(t *testing.T) {
	cache := newAssetCache(t.TempDir())
	site := Site{ID: 55, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 128 << 10, AssetCacheRules: "*/file/*"}
	target, _ := url.Parse("https://media.example/file/poster.jpg")
	get := cache.request(site, httptest.NewRequest(http.MethodGet, "https://proxy.example/file/poster.jpg", nil), target)
	head := cache.request(site, httptest.NewRequest(http.MethodHead, "https://proxy.example/file/poster.jpg", nil), target)
	first, second := bytes.Repeat([]byte{'a'}, 64<<10), bytes.Repeat([]byte{'b'}, (64<<10)+1)
	errors := make(chan error, 10)
	start := make(chan struct{})
	var workers sync.WaitGroup
	run := func(fn func() error) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			if err := fn(); err != nil {
				errors <- err
			}
		}()
	}
	run(func() error {
		for i := range 40 {
			body, tag := first, "a"
			if i%2 != 0 {
				body, tag = second, "b"
			}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/jpeg"}, "Etag": {tag}}}
			if err := cache.write(site, get, resp, body, time.Now()); err != nil {
				return err
			}
		}
		return nil
	})
	for range 3 {
		run(func() error {
			for range 80 {
				hit, err := cache.read(get, time.Now())
				if err != nil {
					return err
				}
				if hit == nil {
					continue
				}
				tag := strings.Join(hit.meta.Headers["ETag"], "")
				if (tag != "a" || !bytes.Equal(hit.body, first)) && (tag != "b" || !bytes.Equal(hit.body, second)) {
					return fmt.Errorf("concurrent hit mixed body/header: etag=%q size=%d", tag, len(hit.body))
				}
			}
			return nil
		})
	}
	run(func() error {
		for range 80 {
			hit, err := cache.read(head, time.Now())
			if err != nil {
				return err
			}
			if hit != nil && (len(hit.body) != 0 || (hit.meta.Size != int64(len(first)) && hit.meta.Size != int64(len(second)))) {
				return fmt.Errorf("invalid concurrent HEAD hit: body=%d size=%d", len(hit.body), hit.meta.Size)
			}
		}
		return nil
	})
	run(func() error {
		for range 16 {
			if err := cache.clear(); err != nil {
				return err
			}
		}
		return nil
	})
	run(func() error {
		for range 16 {
			if err := cache.reconcileSizeIndex(); err != nil {
				return err
			}
		}
		return nil
	})
	close(start)
	workers.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	if t.Failed() {
		return
	}
	if err := cache.clear(); err != nil {
		t.Fatal(err)
	}
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/jpeg"}}}
	if err := cache.write(site, get, response, first, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := cache.reconcileSizeIndex(); err != nil {
		t.Fatal(err)
	}
	if sizes, total, err := cache.sizeBySite(); err != nil || total != int64(len(first)) || sizes[site.ID] != total {
		t.Fatalf("final size accounting sizes=%v total=%d err=%v", sizes, total, err)
	}
}

func BenchmarkAssetCacheHEADHit(b *testing.B) {
	cache := newAssetCache(b.TempDir())
	site := Site{ID: 56, AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 16 << 20, AssetCacheRules: "*/file/*"}
	target, _ := url.Parse("https://media.example/file/poster.jpg")
	request := httptest.NewRequest(http.MethodHead, "https://proxy.example/file/poster.jpg", nil)
	cacheReq := cache.request(site, request, target)
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/jpeg"}}}
	now := time.Now()
	if err := cache.write(site, cacheReq, response, bytes.Repeat([]byte{'x'}, 2<<20), now); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if hit, err := cache.read(cacheReq, now.Add(time.Second)); err != nil || hit == nil {
			b.Fatalf("HEAD hit=%v err=%v", hit != nil, err)
		}
	}
}
