package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestAssetCacheBelowBudgetDoesNotAllocateEvictionCandidates(t *testing.T) {
	cache := newAssetCache(t.TempDir())
	cache.sizeLoaded = true
	site := Site{ID: 1, AssetCacheNamespace: "site-1-config-a", AssetCacheMaxBytes: 512 << 20}
	for entry := range 10000 {
		name := fmt.Sprintf("site-1-config-a/%d.body", entry)
		cache.addIndexedEntryLocked(assetCacheIndexedEntry{siteID: 1, namespace: "site-1", bodyName: name, size: 4096})
	}
	allocations := testing.AllocsPerRun(10, func() {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if err := cache.enforceBudgetLocked(nil, site); err != nil {
			t.Fatal(err)
		}
	})
	if allocations != 0 {
		t.Fatalf("cache below budget allocated eviction candidates: %.0f allocations/check", allocations)
	}
}

func TestAssetCacheBudgetCountsStableNamespaceAcrossLocalIDsAndGenerations(t *testing.T) {
	cache := newAssetCache(t.TempDir())
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"image/jpeg"}}}
	target, _ := url.Parse("https://media.example/file/poster.jpg")
	request := httptest.NewRequest(http.MethodGet, "https://proxy.example/file/poster.jpg", nil)
	oldSite := Site{ID: 1, AssetCacheNamespace: "site-100-config-old", AssetCacheEnabled: true, AssetCacheTTLSec: 3600, AssetCacheMaxBytes: 6, AssetCacheRules: "*/file/*"}
	newSite := oldSite
	newSite.ID, newSite.AssetCacheNamespace = 2, "site-100-config-new"
	oldReq, newReq := cache.request(oldSite, request, target), cache.request(newSite, request, target)
	if err := cache.write(oldSite, oldReq, response, []byte("older"), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := cache.write(newSite, newReq, response, []byte("newer"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if hit, err := cache.read(oldReq, time.Now()); err != nil || hit != nil {
		t.Fatalf("old generation escaped stable-site byte budget: hit=%v err=%v", hit != nil, err)
	}
	if hit, err := cache.read(newReq, time.Now()); err != nil || hit == nil || string(hit.body) != "newer" {
		t.Fatalf("new generation was not retained: hit=%v err=%v", hit, err)
	}
	if err := cache.reconcileSizeIndex(); err != nil {
		t.Fatal(err)
	}
	if sizes, total, err := cache.sizeBySite(); err != nil || total != 5 || sizes[100] != 5 {
		t.Fatalf("stable namespace accounting=%v total=%d err=%v", sizes, total, err)
	}
}

func BenchmarkAssetCacheBelowBudget(b *testing.B) {
	for _, count := range []int{100, 10000, 50000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			cache := newAssetCache(b.TempDir())
			cache.sizeLoaded = true
			site := Site{ID: 1, AssetCacheNamespace: "site-1-config-benchmark", AssetCacheMaxBytes: 512 << 20}
			for entry := range count {
				name := fmt.Sprintf("site-1-config-benchmark/%d.body", entry)
				cache.addIndexedEntryLocked(assetCacheIndexedEntry{siteID: 1, namespace: "site-1", bodyName: name, accessed: int64((entry * 7919) % count), size: 4096})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				cache.mu.Lock()
				err := cache.enforceBudgetLocked(nil, site)
				cache.mu.Unlock()
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
