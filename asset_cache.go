package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var assetCacheBlockedExtensions = map[string]bool{
	".m3u8": true, ".m3u": true, ".mpd": true, ".ts": true, ".m4s": true,
	".mp4": true, ".m4v": true, ".mkv": true, ".webm": true, ".mov": true,
	".avi": true, ".flv": true, ".mp3": true, ".aac": true, ".m4a": true,
	".flac": true, ".ogg": true, ".opus": true, ".wav": true,
}

func assetCacheRuleMatches(rules string, target *url.URL) bool {
	if target == nil {
		return false
	}
	candidate := strings.ToLower(target.Host + target.EscapedPath())
	for _, line := range strings.Split(strings.ReplaceAll(rules, "\r\n", "\n"), "\n") {
		line = strings.ToLower(strings.TrimSpace(line))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pattern := "^" + regexp.QuoteMeta(line) + "$"
		pattern = strings.ReplaceAll(pattern, `\*`, ".*")
		if matched, _ := regexp.MatchString(pattern, candidate); matched {
			return true
		}
	}
	return false
}

type assetCacheMeta struct {
	Status       int                 `json:"status"`
	Headers      map[string][]string `json:"headers"`
	CreatedAtMS  int64               `json:"created_at_ms"`
	ExpiresAtMS  int64               `json:"expires_at_ms"`
	AccessedAtMS int64               `json:"accessed_at_ms"`
	Size         int64               `json:"size"`
}

type assetCacheHit struct {
	meta assetCacheMeta
	body []byte
}

type assetCache struct {
	dir                string
	mu                 sync.Mutex
	maintenanceMu      sync.Mutex
	sizeBySiteMap      map[int64]int64
	sizeByNamespace    map[string]int64
	entriesByNamespace map[string]map[string]struct{}
	epoch              uint64
	activeNamespaces   map[string]string
	fills              map[*assetCacheFillBody]struct{}
	clearing           bool
	totalBytes         int64
	sizeLoaded         bool
	entries            map[string]assetCacheIndexedEntry
}

type assetCacheIndexedEntry struct {
	siteID            int64
	namespace         string
	metaName          string
	bodyName          string
	accessed          int64
	persistedAccessed int64
	size              int64
	bodyInfo          os.FileInfo
}

const assetCacheAccessPersistInterval = 30 * time.Second

var errAssetCacheInvalidEntry = errors.New("invalid cache entry")

type assetCacheReadSnapshot struct {
	meta     assetCacheMeta
	body     *os.File
	bodyInfo os.FileInfo
}

type assetCacheContextKey struct{}

type assetCacheRequest struct {
	key       string
	metaName  string
	bodyName  string
	metaPath  string
	bodyPath  string
	method    string
	epoch     uint64
	namespace string
}

func newAssetCache(dir string) *assetCache {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	return &assetCache{dir: dir, sizeBySiteMap: make(map[int64]int64), sizeByNamespace: make(map[string]int64), entriesByNamespace: make(map[string]map[string]struct{}), activeNamespaces: make(map[string]string), fills: make(map[*assetCacheFillBody]struct{}), entries: make(map[string]assetCacheIndexedEntry)}
}

func assetCacheTargetURL(r *http.Request, upstream *url.URL) *url.URL {
	if r == nil || r.URL == nil || upstream == nil {
		return nil
	}
	target := *r.URL
	applyUpstreamURL(&target, upstream)
	target.Fragment = ""
	target.RawFragment = ""
	return &target
}

func assetCacheRequestEligible(site Site, r *http.Request, target *url.URL) bool {
	return assetCacheRequestReason(site, r, target) == ""
}

func assetCacheRequestReason(site Site, r *http.Request, target *url.URL) string {
	if !site.AssetCacheEnabled {
		return "disabled"
	}
	if r == nil || r.URL == nil || target == nil {
		return "rule_no_match"
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return "method"
	}
	if r.Header.Get("Range") != "" {
		return "range"
	}
	if hasUpgradeIntent(r) || isReservedDynamicRoute(r.URL.Path) || isPlaybackRedirectEndpoint(r.URL.Path) || isPlaybackInfoRequest(r.URL.Path) || dynamicStructuredRequestSource(r.URL.Path) != "" {
		return "dynamic"
	}
	ext := strings.ToLower(filepath.Ext(target.Path))
	if assetCacheBlockedExtensions[ext] {
		return "dynamic"
	}
	if !assetCacheRuleMatches(site.AssetCacheRules, target) {
		return "rule_no_match"
	}
	return ""
}

func assetCacheIdentity(r *http.Request) string {
	if r == nil {
		return ""
	}
	identity := strings.Join([]string{
		r.Header.Get("Authorization"),
		r.Header.Get("X-Emby-Authorization"),
		r.Header.Get("X-Emby-Token"),
		r.Header.Get("X-MediaBrowser-Token"),
		r.Header.Get("Cookie"),
	}, "\n")
	if identity == "\n\n\n\n" {
		return "anonymous"
	}
	digest := sha256.Sum256([]byte(identity))
	return fmt.Sprintf("%x", digest[:])
}

// Agent runtimes supply a stable central-site generation rather than their
// ephemeral SQLite row ID. Controller runtimes derive a generation from the
// upstream/content and cache settings so edits cannot reuse prior content.
// Hash encrypted fixed headers along with the settings; never put their values
// or ciphertext directly in a filesystem path.
func assetCacheNamespace(site Site) string {
	namespace := strings.TrimSpace(site.AssetCacheNamespace)
	if namespace != "" {
		return namespace
	}
	payload := struct {
		TargetURL, PlaybackTargetURL, PlaybackMode, MainVideoStreamMode string
		IngressMode, PublicHost, PathPrefix, StreamHosts                string
		FailoverTargets                                                 []string
		UAMode, CustomUserAgent, CustomClient, CustomVersion            string
		ClientIPMode, StoredUpstreamHeaders                             string
		RuntimeUpstreamHeaders                                          map[string][]string
		CacheEnabled                                                    bool
		CacheTTLSec                                                     int
		CacheMaxBytes                                                   int64
		CacheRules                                                      string
	}{
		TargetURL: site.TargetURL, PlaybackTargetURL: site.PlaybackTargetURL, PlaybackMode: site.PlaybackMode, MainVideoStreamMode: site.MainVideoStreamMode,
		IngressMode: site.IngressMode, PublicHost: site.PublicHost, PathPrefix: site.PathPrefix,
		FailoverTargets: site.FailoverTargetList, StreamHosts: site.StreamHosts,
		UAMode: site.UAMode, CustomUserAgent: site.CustomUserAgent, CustomClient: site.CustomClient, CustomVersion: site.CustomVersion,
		ClientIPMode: site.ClientIPMode, StoredUpstreamHeaders: site.StoredUpstreamHeaders, RuntimeUpstreamHeaders: site.RuntimeUpstreamHeaders,
		CacheEnabled: site.AssetCacheEnabled, CacheTTLSec: site.AssetCacheTTLSec, CacheMaxBytes: site.AssetCacheMaxBytes, CacheRules: site.AssetCacheRules,
	}
	data, _ := json.Marshal(payload)
	digest := sha256.Sum256(data)
	return fmt.Sprintf("site-%d-config-%x", site.ID, digest[:16])
}

func assetCacheNamespacePrefix(namespace string) string {
	namespace = strings.TrimSpace(namespace)
	if strings.HasPrefix(namespace, "site-") {
		if index := strings.Index(namespace, "-config-"); index > 0 {
			return namespace[:index]
		}
	}
	return namespace
}

func assetCacheSitePrefix(site Site) string {
	return assetCacheNamespacePrefix(assetCacheNamespace(site))
}

func (c *assetCache) request(site Site, r *http.Request, target *url.URL) *assetCacheRequest {
	if c == nil || !assetCacheRequestEligible(site, r, target) {
		return nil
	}
	namespace := assetCacheNamespace(site)
	raw := strings.Join([]string{
		// The cache namespace is supplied by the Controller and remains stable
		// across the Agent's in-memory SQLite rebuilds. Do not include the local
		// SQLite row ID in the cache key: it is ephemeral and can change when a
		// route is added, removed, or reordered.
		namespace,
		target.String(),
		r.Header.Get("Accept"),
		r.Header.Get("Accept-Encoding"),
		assetCacheIdentity(r),
	}, "\n")
	digest := sha256.Sum256([]byte(raw))
	key := fmt.Sprintf("%x", digest[:])
	relDir := filepath.Join(namespace, key[:2])
	metaName := filepath.Join(relDir, key+".json")
	bodyName := filepath.Join(relDir, key+".body")
	c.mu.Lock()
	epoch := c.epoch
	c.mu.Unlock()
	return &assetCacheRequest{
		key:       key,
		method:    r.Method,
		metaName:  metaName,
		bodyName:  bodyName,
		metaPath:  filepath.Join(c.dir, metaName),
		bodyPath:  filepath.Join(c.dir, bodyName),
		epoch:     epoch,
		namespace: namespace,
	}
}

func prepareAssetCacheRequest(cache *assetCache, site Site, r *http.Request, target *url.URL) (*http.Request, *assetCacheHit) {
	if r == nil {
		return r, nil
	}
	tracker := requestDiagnosticsFromContext(r.Context())
	tracker.SetCacheWrite("not_attempted", "")
	if cache == nil || !site.AssetCacheEnabled {
		tracker.SetCacheLookup("disabled", "disabled")
		tracker.SetCacheWrite("not_attempted", "disabled")
		return r, nil
	}
	if reason := assetCacheRequestReason(site, r, target); reason != "" {
		tracker.SetCacheLookup("bypass", reason)
		tracker.SetCacheWrite("not_attempted", reason)
		return r, nil
	}
	cacheReq := cache.request(site, r, target)
	hit, err := cache.read(cacheReq, time.Now())
	if err != nil {
		tracker.SetCacheLookup("read_error", assetCacheErrorReason(err))
	} else if hit != nil {
		tracker.SetCacheLookup("hit", "")
		tracker.SetResponseType(http.Header(hit.meta.Headers).Get("Content-Type"))
		return r, hit
	} else {
		tracker.SetCacheLookup("miss", "")
	}
	if cacheReq.method == http.MethodHead {
		tracker.SetCacheWrite("not_attempted", "head_no_fill")
	}
	return r.WithContext(context.WithValue(r.Context(), assetCacheContextKey{}, cacheReq)), nil
}

func (c *assetCache) openRoot(create bool) (*os.Root, error) {
	if create {
		cleanDir, err := filepath.Abs(filepath.Clean(c.dir))
		if err != nil {
			return nil, err
		}
		parent := filepath.Dir(cleanDir)
		base := filepath.Base(cleanDir)
		parentRoot, err := os.OpenRoot(parent)
		if err != nil {
			return nil, err
		}
		err = parentRoot.Mkdir(base, 0700)
		_ = parentRoot.Close()
		if err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
	}
	return os.OpenRoot(c.dir)
}

// sizeBySite reports the bytes occupied by cached response bodies. Metadata
// files are intentionally excluded so the panel matches the configured cache
// budget, which is also enforced against response body sizes.
func assetCacheSiteIDFromPath(path string) (int64, bool) {
	parts := strings.SplitN(filepath.ToSlash(path), "/", 2)
	if len(parts) != 2 {
		return 0, false
	}
	directory := parts[0]
	if strings.HasPrefix(directory, "site-") {
		directory = strings.TrimPrefix(directory, "site-")
		if dash := strings.Index(directory, "-"); dash >= 0 {
			directory = directory[:dash]
		}
	}
	siteID, err := strconv.ParseInt(directory, 10, 64)
	return siteID, err == nil && siteID > 0
}

// assetCacheNamespaceFromPath returns the stable central-site namespace
// prefix (for example site-100) rather than an Agent's ephemeral SQLite row
// ID. Generation suffixes are intentionally ignored for budget accounting so
// old generations remain part of the same site's quota.
func assetCacheNamespaceFromPath(path string) string {
	parts := strings.SplitN(filepath.ToSlash(path), "/", 2)
	if len(parts) != 2 {
		return ""
	}
	return assetCacheNamespacePrefix(parts[0])
}

func (c *assetCache) rebuildSizeIndexLocked() error {
	if c == nil {
		return nil
	}
	sizes := make(map[int64]int64)
	namespaceSizes := make(map[string]int64)
	namespaceEntries := make(map[string]map[string]struct{})
	entries := make(map[string]assetCacheIndexedEntry)
	root, err := c.openRoot(false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c.sizeBySiteMap, c.entries, c.totalBytes, c.sizeLoaded = sizes, entries, 0, true
			c.sizeByNamespace, c.entriesByNamespace = namespaceSizes, namespaceEntries
			return nil
		}
		return err
	}
	defer root.Close()

	var total int64
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".body") {
			return nil
		}
		siteID, ok := assetCacheSiteIDFromPath(path)
		if !ok {
			return nil
		}
		bodyName := filepath.FromSlash(path)
		// Root.Stat captures the identity while the file still occupies this
		// path, including on platforms whose directory entries defer that work.
		info, err := root.Stat(bodyName)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.Size() < 0 {
			return fmt.Errorf("cache file %q has an invalid size", path)
		}
		sizes[siteID] += info.Size()
		total += info.Size()
		metaName := strings.TrimSuffix(bodyName, ".body") + ".json"
		accessed := int64(0)
		if data, readErr := root.ReadFile(metaName); readErr == nil {
			var meta assetCacheMeta
			if json.Unmarshal(data, &meta) == nil {
				accessed = meta.AccessedAtMS
			}
		}
		indexed := assetCacheIndexedEntry{siteID: siteID, namespace: assetCacheNamespaceFromPath(path), metaName: metaName, bodyName: bodyName, accessed: accessed, persistedAccessed: accessed, size: info.Size(), bodyInfo: info}
		if previous, ok := c.entries[bodyName]; ok && previous.bodyInfo != nil && os.SameFile(previous.bodyInfo, info) {
			// Hits update RAM on every access, but the metadata is persisted less
			// often. A periodic disk reconciliation must keep that newer recency
			// only while this is still the same body file.
			indexed.accessed = max(indexed.accessed, previous.accessed)
		}
		entries[bodyName] = indexed
		namespaceSizes[indexed.namespace] += indexed.size
		if namespaceEntries[indexed.namespace] == nil {
			namespaceEntries[indexed.namespace] = make(map[string]struct{})
		}
		namespaceEntries[indexed.namespace][bodyName] = struct{}{}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	c.sizeBySiteMap, c.entries, c.totalBytes, c.sizeLoaded = sizes, entries, total, true
	c.sizeByNamespace, c.entriesByNamespace = namespaceSizes, namespaceEntries
	return nil
}

func (c *assetCache) ensureSizeIndexLocked() error {
	if c == nil || c.sizeLoaded {
		return nil
	}
	return c.rebuildSizeIndexLocked()
}

func (c *assetCache) sizeBySite() (map[int64]int64, int64, error) {
	if c == nil {
		return map[int64]int64{}, 0, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureSizeIndexLocked(); err != nil {
		return nil, 0, err
	}
	sizes := make(map[int64]int64, len(c.sizeBySiteMap))
	for siteID, size := range c.sizeBySiteMap {
		sizes[siteID] = size
	}
	return sizes, c.totalBytes, nil
}

func (c *assetCache) reconcileSizeIndex() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rebuildSizeIndexLocked()
}

// startReconcile keeps the in-memory accounting correct if an operator or a
// separate process changes the cache directory behind Meridian's back. It is
// deliberately off the dashboard request path.
func (c *assetCache) startReconcile(ctx context.Context) {
	if c == nil || ctx == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := c.reconcileSizeIndex(); err != nil {
					log.Printf("[asset-cache] reconcile failed: %s", assetCacheErrorReason(err))
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

// clear removes only entries beneath the configured asset-cache root. The
// root directory itself remains available for subsequent cache writes.
func (c *assetCache) clear() error {
	if c == nil {
		return nil
	}
	c.maintenanceMu.Lock()
	defer c.maintenanceMu.Unlock()
	c.mu.Lock()
	c.epoch++
	c.clearing = true
	fills := make([]*assetCacheFillBody, 0, len(c.fills))
	for fill := range c.fills {
		fills = append(fills, fill)
	}
	c.mu.Unlock()
	for _, fill := range fills {
		fill.abort("generation_changed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	defer func() { c.clearing = false }()
	root, err := c.openRoot(false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if err := root.RemoveAll(entry.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	c.sizeBySiteMap = make(map[int64]int64)
	c.sizeByNamespace = make(map[string]int64)
	c.entriesByNamespace = make(map[string]map[string]struct{})
	c.entries = make(map[string]assetCacheIndexedEntry)
	c.totalBytes = 0
	c.sizeLoaded = true
	return nil
}

// gcSiteGenerations removes cache generations for one stable central site
// after a new configuration generation has been successfully materialized.
// It never uses the Agent's local SQLite ID and never touches another site's
// namespace.
func (c *assetCache) gcSiteGenerations(sitePrefix, keepNamespace string) error {
	if c == nil || strings.TrimSpace(sitePrefix) == "" {
		return nil
	}
	c.maintenanceMu.Lock()
	defer c.maintenanceMu.Unlock()
	c.mu.Lock()
	c.activeNamespaces[sitePrefix] = keepNamespace
	fills := make([]*assetCacheFillBody, 0)
	for fill := range c.fills {
		if assetCacheNamespacePrefix(fill.request.namespace) == sitePrefix && fill.request.namespace != keepNamespace {
			fills = append(fills, fill)
		}
	}
	c.mu.Unlock()
	for _, fill := range fills {
		fill.abort("generation_changed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	root, err := c.openRoot(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	prefix := strings.TrimSuffix(sitePrefix, "-") + "-"
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) || entry.Name() == keepNamespace {
			continue
		}
		if err := root.RemoveAll(entry.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return c.rebuildSizeIndexLocked()
}

func (c *assetCache) addIndexedEntryLocked(entry assetCacheIndexedEntry) {
	if old, ok := c.entries[entry.bodyName]; ok {
		c.removeIndexedEntryLocked(old.bodyName)
	}
	c.entries[entry.bodyName] = entry
	c.sizeBySiteMap[entry.siteID] += entry.size
	c.sizeByNamespace[entry.namespace] += entry.size
	if c.entriesByNamespace[entry.namespace] == nil {
		c.entriesByNamespace[entry.namespace] = make(map[string]struct{})
	}
	c.entriesByNamespace[entry.namespace][entry.bodyName] = struct{}{}
	c.totalBytes += entry.size
}

func (c *assetCache) removeIndexedEntryLocked(bodyName string) {
	entry, ok := c.entries[bodyName]
	if !ok {
		return
	}
	delete(c.entries, bodyName)
	c.sizeByNamespace[entry.namespace] -= entry.size
	delete(c.entriesByNamespace[entry.namespace], bodyName)
	if len(c.entriesByNamespace[entry.namespace]) == 0 {
		delete(c.entriesByNamespace, entry.namespace)
		delete(c.sizeByNamespace, entry.namespace)
	}
	c.sizeBySiteMap[entry.siteID] -= entry.size
	if c.sizeBySiteMap[entry.siteID] <= 0 {
		delete(c.sizeBySiteMap, entry.siteID)
	}
	c.totalBytes -= entry.size
	if c.totalBytes < 0 {
		c.totalBytes = 0
	}
}

func (c *assetCache) removeEntryLocked(root *os.Root, metaName, bodyName string) error {
	metaErr := root.Remove(metaName)
	bodyErr := root.Remove(bodyName)
	if bodyErr == nil || errors.Is(bodyErr, os.ErrNotExist) {
		c.removeIndexedEntryLocked(bodyName)
	}
	if errors.Is(metaErr, os.ErrNotExist) {
		metaErr = nil
	}
	if errors.Is(bodyErr, os.ErrNotExist) {
		bodyErr = nil
	}
	return errors.Join(metaErr, bodyErr)
}

func (c *assetCache) read(req *assetCacheRequest, now time.Time) (*assetCacheHit, error) {
	if c == nil || req == nil {
		return nil, nil
	}
	snapshot, err := c.openReadSnapshot(req, now)
	if err != nil || snapshot == nil {
		return nil, err
	}
	if snapshot.body == nil {
		return &assetCacheHit{meta: snapshot.meta}, nil
	}
	defer snapshot.body.Close()
	// The descriptor and metadata were paired under the lock. Writers replace
	// body files atomically, so an in-flight reader can finish its own snapshot
	// without serializing every site's disk read behind the global cache lock.
	body, err := io.ReadAll(io.LimitReader(snapshot.body, snapshot.meta.Size+1))
	if err != nil {
		c.removeReadEntryIfCurrent(req, snapshot.bodyInfo)
		return nil, err
	}
	if int64(len(body)) != snapshot.meta.Size {
		c.removeReadEntryIfCurrent(req, snapshot.bodyInfo)
		return nil, errAssetCacheInvalidEntry
	}
	return &assetCacheHit{meta: snapshot.meta, body: body}, nil
}

func (c *assetCache) openReadSnapshot(req *assetCacheRequest, now time.Time) (*assetCacheReadSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureSizeIndexLocked(); err != nil {
		return nil, err
	}
	root, err := c.openRoot(false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer root.Close()
	metaBytes, err := root.ReadFile(req.metaName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var meta assetCacheMeta
	if json.Unmarshal(metaBytes, &meta) != nil {
		c.removeEntryLocked(root, req.metaName, req.bodyName)
		return nil, errAssetCacheInvalidEntry
	}
	if meta.ExpiresAtMS <= now.UnixMilli() {
		c.removeEntryLocked(root, req.metaName, req.bodyName)
		return nil, nil
	}
	if meta.Size <= 0 || meta.Size > maxAssetCacheObject || !assetCacheResponseHeadersEligible(&http.Response{StatusCode: meta.Status, Header: http.Header(meta.Headers)}) {
		c.removeEntryLocked(root, req.metaName, req.bodyName)
		return nil, errAssetCacheInvalidEntry
	}
	snapshot := &assetCacheReadSnapshot{meta: meta}
	if req.method == http.MethodHead {
		snapshot.bodyInfo, err = root.Stat(req.bodyName)
	} else {
		snapshot.body, err = root.Open(req.bodyName)
		if err == nil {
			snapshot.bodyInfo, err = snapshot.body.Stat()
		}
	}
	if err != nil {
		if snapshot.body != nil {
			_ = snapshot.body.Close()
		}
		if errors.Is(err, os.ErrNotExist) {
			c.removeEntryLocked(root, req.metaName, req.bodyName)
			return nil, errAssetCacheInvalidEntry
		}
		return nil, err
	}
	if !snapshot.bodyInfo.Mode().IsRegular() || snapshot.bodyInfo.Size() != meta.Size {
		if snapshot.body != nil {
			_ = snapshot.body.Close()
		}
		c.removeEntryLocked(root, req.metaName, req.bodyName)
		return nil, errAssetCacheInvalidEntry
	}
	c.recordAccessLocked(root, req, &snapshot.meta, snapshot.bodyInfo, now)
	return snapshot, nil
}

func (c *assetCache) recordAccessLocked(root *os.Root, req *assetCacheRequest, meta *assetCacheMeta, info os.FileInfo, now time.Time) {
	entry, ok := c.entries[req.bodyName]
	if !ok || entry.bodyInfo == nil || !os.SameFile(entry.bodyInfo, info) {
		siteID, valid := assetCacheSiteIDFromPath(req.bodyName)
		if !valid {
			return
		}
		entry = assetCacheIndexedEntry{siteID: siteID, namespace: assetCacheNamespaceFromPath(req.bodyName), metaName: req.metaName, bodyName: req.bodyName, accessed: meta.AccessedAtMS, persistedAccessed: meta.AccessedAtMS, size: meta.Size, bodyInfo: info}
		c.addIndexedEntryLocked(entry)
	}
	entry.accessed = max(entry.accessed, now.UnixMilli())
	meta.AccessedAtMS = entry.accessed
	if entry.accessed-entry.persistedAccessed >= assetCacheAccessPersistInterval.Milliseconds() {
		if updated, err := json.Marshal(meta); err == nil {
			if err := root.WriteFile(req.metaName, updated, 0600); err == nil {
				entry.persistedAccessed = entry.accessed
			}
		}
	}
	c.entries[req.bodyName] = entry
}

func (c *assetCache) removeReadEntryIfCurrent(req *assetCacheRequest, original os.FileInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	root, err := c.openRoot(false)
	if err != nil {
		return
	}
	defer root.Close()
	current, err := root.Stat(req.bodyName)
	if err == nil && os.SameFile(original, current) {
		// A failed old read must not remove a replacement written after its
		// descriptor was opened, even when that replacement has the same size.
		c.removeEntryLocked(root, req.metaName, req.bodyName)
	}
}

func assetCacheResponseHeadersEligible(resp *http.Response) bool {
	return assetCacheResponseHeadersReason(resp) == ""
}

func assetCacheResponseHeadersReason(resp *http.Response) string {
	if resp == nil || resp.StatusCode != http.StatusOK {
		return "response_status"
	}
	if len(resp.Header.Values("Set-Cookie")) > 0 {
		return "set_cookie"
	}
	cacheControl := strings.ToLower(strings.Join(resp.Header.Values("Cache-Control"), ","))
	for _, directive := range strings.Split(cacheControl, ",") {
		name := strings.TrimSpace(strings.SplitN(directive, "=", 2)[0])
		switch name {
		case "no-store":
			return "no_store"
		case "no-cache":
			return "no_cache"
		case "private":
			return "private"
		}
	}
	vary := strings.ToLower(strings.Join(resp.Header.Values("Vary"), ","))
	for _, name := range strings.Split(vary, ",") {
		name = strings.TrimSpace(name)
		if name == "*" || name != "" && name != "accept" && name != "accept-encoding" {
			return "vary"
		}
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return "mime"
	}
	mediaType = strings.ToLower(mediaType)
	if strings.HasPrefix(mediaType, "video/") || strings.HasPrefix(mediaType, "audio/") || strings.Contains(mediaType, "mpegurl") || mediaType == "application/dash+xml" {
		return "mime"
	}
	if strings.HasPrefix(mediaType, "image/") || strings.HasPrefix(mediaType, "font/") || mediaType == "text/css" || strings.Contains(mediaType, "javascript") || mediaType == "application/wasm" || mediaType == "application/font-woff" {
		return ""
	}
	return "mime"
}

func assetCacheResponseEligible(resp *http.Response, body []byte) bool {
	return len(body) > 0 && int64(len(body)) <= maxAssetCacheObject && assetCacheResponseHeadersEligible(resp)
}

func cacheableResponseHeaders(header http.Header) map[string][]string {
	result := make(map[string][]string)
	for _, name := range []string{"Content-Type", "Content-Language", "Content-Encoding", "ETag", "Last-Modified", "Cache-Control", "Expires", "Vary"} {
		if values := header.Values(name); len(values) > 0 {
			result[name] = append([]string(nil), values...)
		}
	}
	return result
}

func (c *assetCache) write(site Site, req *assetCacheRequest, resp *http.Response, body []byte, now time.Time) error {
	_, err := c.writeStored(site, req, resp, body, now)
	return err
}

func (c *assetCache) writeStored(site Site, req *assetCacheRequest, resp *http.Response, body []byte, now time.Time) (bool, error) {
	if c == nil || req == nil || !assetCacheResponseEligible(resp, body) {
		return false, nil
	}
	meta := assetCacheMeta{
		Status:       resp.StatusCode,
		Headers:      cacheableResponseHeaders(resp.Header),
		CreatedAtMS:  now.UnixMilli(),
		ExpiresAtMS:  now.Add(time.Duration(site.AssetCacheTTLSec) * time.Second).UnixMilli(),
		AccessedAtMS: now.UnixMilli(),
		Size:         int64(len(body)),
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureSizeIndexLocked(); err != nil {
		return false, err
	}
	root, err := c.openRoot(true)
	if err != nil {
		return false, err
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.Dir(req.bodyName), 0700); err != nil {
		return false, err
	}
	bodyTmp := req.bodyName + ".tmp"
	metaTmp := req.metaName + ".tmp"
	if err := root.WriteFile(bodyTmp, body, 0600); err != nil {
		return false, err
	}
	if err := root.WriteFile(metaTmp, metaBytes, 0600); err != nil {
		_ = root.Remove(bodyTmp)
		return false, err
	}
	if err := root.Rename(bodyTmp, req.bodyName); err != nil {
		_ = root.Remove(bodyTmp)
		_ = root.Remove(metaTmp)
		return false, err
	}
	if err := root.Rename(metaTmp, req.metaName); err != nil {
		_ = root.Remove(req.bodyName)
		_ = root.Remove(metaTmp)
		c.removeIndexedEntryLocked(req.bodyName)
		return false, err
	}
	bodyInfo, err := root.Stat(req.bodyName)
	if err != nil {
		c.removeEntryLocked(root, req.metaName, req.bodyName)
		return false, err
	}
	if siteID, ok := assetCacheSiteIDFromPath(req.bodyName); ok {
		c.addIndexedEntryLocked(assetCacheIndexedEntry{siteID: siteID, namespace: assetCacheNamespaceFromPath(req.bodyName), metaName: req.metaName, bodyName: req.bodyName, accessed: meta.AccessedAtMS, persistedAccessed: meta.AccessedAtMS, size: meta.Size, bodyInfo: bodyInfo})
	}
	if err := c.enforceBudgetLocked(root, site); err != nil {
		return false, err
	}
	current, err := root.Stat(req.bodyName)
	if errors.Is(err, os.ErrNotExist) {
		c.removeIndexedEntryLocked(req.bodyName)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !os.SameFile(bodyInfo, current) || current.Size() != meta.Size {
		return false, errors.New("cache body changed during write")
	}
	return true, nil
}

type assetCacheFile struct {
	metaName string
	bodyName string
	accessed int64
	size     int64
}

func (c *assetCache) enforceBudgetLocked(root *os.Root, site Site) error {
	targetNamespace := assetCacheNamespacePrefix(assetCacheNamespace(site))
	total := c.sizeByNamespace[targetNamespace]
	if total <= site.AssetCacheMaxBytes {
		return nil
	}
	files := make([]assetCacheFile, 0, len(c.entriesByNamespace[targetNamespace]))
	for bodyName := range c.entriesByNamespace[targetNamespace] {
		entry := c.entries[bodyName]
		files = append(files, assetCacheFile{metaName: entry.metaName, bodyName: entry.bodyName, accessed: entry.accessed, size: entry.size})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].accessed < files[j].accessed })
	for _, file := range files {
		if total <= site.AssetCacheMaxBytes {
			break
		}
		if err := c.removeEntryLocked(root, file.metaName, file.bodyName); err != nil {
			return err
		}
		total -= file.size
	}
	return nil
}

func assetCacheErrorReason(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, errAssetCacheInvalidEntry) {
		return "invalid_entry"
	}
	if errors.Is(err, syscall.EROFS) {
		return "readonly"
	}
	if errors.Is(err, syscall.ENOSPC) {
		return "no_space"
	}
	if errors.Is(err, os.ErrPermission) {
		return "permission"
	}
	if runtime.GOOS == "windows" {
		// Windows reports native error codes instead of the portable syscall
		// errno values for write-protected volumes and exhausted disks.
		if errors.Is(err, syscall.Errno(19)) {
			return "readonly" // ERROR_WRITE_PROTECT
		}
		if errors.Is(err, syscall.Errno(39)) || errors.Is(err, syscall.Errno(112)) {
			return "no_space" // ERROR_HANDLE_DISK_FULL / ERROR_DISK_FULL
		}
	}
	return "io"
}

func prepareAssetCacheResponse(resp *http.Response, cache *assetCache, site Site) error {
	if resp == nil || resp.Request == nil {
		return nil
	}
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}
	tracker := requestDiagnosticsFromContext(resp.Request.Context())
	tracker.SetResponseType(resp.Header.Get("Content-Type"))
	if cache == nil {
		return nil
	}
	cacheReq, _ := resp.Request.Context().Value(assetCacheContextKey{}).(*assetCacheRequest)
	if cacheReq == nil {
		return nil
	}
	if cacheReq.method == http.MethodHead {
		tracker.SetCacheWrite("not_attempted", "head_no_fill")
		return nil
	}
	if reason := assetCacheResponseHeadersReason(resp); reason != "" {
		tracker.SetCacheWrite("skipped", reason)
		resp.Header.Set("X-Meridian-Cache", "BYPASS")
		return nil
	}
	if resp.Body == nil || resp.Body == http.NoBody || resp.ContentLength == 0 {
		tracker.SetCacheWrite("skipped", "empty_body")
		resp.Header.Set("X-Meridian-Cache", "BYPASS")
		return nil
	}
	if resp.ContentLength > maxAssetCacheObject {
		tracker.SetCacheWrite("skipped", "too_large")
		resp.Header.Set("X-Meridian-Cache", "BYPASS")
		return nil
	}
	body, reason, err := cache.newFillBody(site, cacheReq, resp, tracker)
	if err != nil {
		tracker.SetCacheWrite("write_error", assetCacheErrorReason(err))
		resp.Header.Set("X-Meridian-Cache", "BYPASS")
		return nil
	}
	if body == nil {
		tracker.SetCacheWrite("skipped", reason)
		resp.Header.Set("X-Meridian-Cache", "BYPASS")
		return nil
	}
	resp.Body = body
	// Headers describe the lookup. The body records the final cache write
	// outcome before the request handler takes its completion snapshot.
	resp.Header.Set("X-Meridian-Cache", "MISS")
	return nil
}

func serveAssetCacheHit(w http.ResponseWriter, r *http.Request, hit *assetCacheHit) {
	for name, values := range hit.meta.Headers {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.Header().Set("X-Meridian-Cache", "HIT")
	w.Header().Set("Age", strconv.FormatInt(max(0, (time.Now().UnixMilli()-hit.meta.CreatedAtMS)/1000), 10))
	w.Header().Set("Content-Length", strconv.FormatInt(hit.meta.Size, 10))
	w.WriteHeader(hit.meta.Status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(hit.body) // #nosec G705 -- bytes are served only after cache metadata/content-type eligibility checks.
	}
}
