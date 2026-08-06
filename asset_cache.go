package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	dir string
	mu  sync.Mutex
}

type assetCacheContextKey struct{}

type assetCacheRequest struct {
	key      string
	metaPath string
	bodyPath string
	method   string
}

func newAssetCache(dir string) *assetCache {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	return &assetCache{dir: dir}
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
	if !site.AssetCacheEnabled || r == nil || target == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	if r.Header.Get("Range") != "" || hasUpgradeIntent(r) || isReservedDynamicRoute(r.URL.Path) || isPlaybackRedirectEndpoint(r.URL.Path) || isPlaybackInfoRequest(r.URL.Path) || dynamicStructuredRequestSource(r.URL.Path) != "" {
		return false
	}
	ext := strings.ToLower(filepath.Ext(target.Path))
	return !assetCacheBlockedExtensions[ext] && assetCacheRuleMatches(site.AssetCacheRules, target)
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

func (c *assetCache) request(site Site, r *http.Request, target *url.URL) *assetCacheRequest {
	if c == nil || !assetCacheRequestEligible(site, r, target) {
		return nil
	}
	raw := strings.Join([]string{
		strconv.FormatInt(site.ID, 10),
		target.String(),
		r.Header.Get("Accept"),
		r.Header.Get("Accept-Encoding"),
		assetCacheIdentity(r),
	}, "\n")
	digest := sha256.Sum256([]byte(raw))
	key := fmt.Sprintf("%x", digest[:])
	dir := filepath.Join(c.dir, strconv.FormatInt(site.ID, 10), key[:2])
	return &assetCacheRequest{
		key:      key,
		method:   r.Method,
		metaPath: filepath.Join(dir, key+".json"),
		bodyPath: filepath.Join(dir, key+".body"),
	}
}

func (c *assetCache) read(req *assetCacheRequest, now time.Time) (*assetCacheHit, error) {
	if c == nil || req == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	metaBytes, err := os.ReadFile(req.metaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var meta assetCacheMeta
	if json.Unmarshal(metaBytes, &meta) != nil || meta.ExpiresAtMS <= now.UnixMilli() || meta.Size < 0 || meta.Size > maxAssetCacheObject {
		_ = os.Remove(req.metaPath)
		_ = os.Remove(req.bodyPath)
		return nil, nil
	}
	body, err := os.ReadFile(req.bodyPath)
	if err != nil || int64(len(body)) != meta.Size {
		_ = os.Remove(req.metaPath)
		_ = os.Remove(req.bodyPath)
		return nil, nil
	}
	meta.AccessedAtMS = now.UnixMilli()
	if updated, err := json.Marshal(meta); err == nil {
		_ = os.WriteFile(req.metaPath, updated, 0600)
	}
	return &assetCacheHit{meta: meta, body: body}, nil
}

func assetCacheResponseEligible(resp *http.Response, body []byte) bool {
	if resp == nil || resp.StatusCode != http.StatusOK || len(body) == 0 || int64(len(body)) > maxAssetCacheObject || len(resp.Header.Values("Set-Cookie")) > 0 {
		return false
	}
	cacheControl := strings.ToLower(resp.Header.Get("Cache-Control"))
	if strings.Contains(cacheControl, "no-store") || strings.Contains(cacheControl, "no-cache") || strings.Contains(cacheControl, "private") {
		return false
	}
	vary := strings.ToLower(resp.Header.Get("Vary"))
	for _, name := range strings.Split(vary, ",") {
		name = strings.TrimSpace(name)
		if name != "" && name != "accept" && name != "accept-encoding" {
			return false
		}
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	mediaType = strings.ToLower(mediaType)
	if strings.HasPrefix(mediaType, "video/") || strings.HasPrefix(mediaType, "audio/") || strings.Contains(mediaType, "mpegurl") || mediaType == "application/dash+xml" {
		return false
	}
	return strings.HasPrefix(mediaType, "image/") || strings.HasPrefix(mediaType, "font/") || mediaType == "text/css" || strings.Contains(mediaType, "javascript") || mediaType == "application/wasm" || mediaType == "application/font-woff"
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
	if c == nil || req == nil || !assetCacheResponseEligible(resp, body) {
		return nil
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
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(req.bodyPath), 0700); err != nil {
		return err
	}
	bodyTmp := req.bodyPath + ".tmp"
	metaTmp := req.metaPath + ".tmp"
	if err := os.WriteFile(bodyTmp, body, 0600); err != nil {
		return err
	}
	if err := os.WriteFile(metaTmp, metaBytes, 0600); err != nil {
		_ = os.Remove(bodyTmp)
		return err
	}
	if err := os.Rename(bodyTmp, req.bodyPath); err != nil {
		_ = os.Remove(bodyTmp)
		_ = os.Remove(metaTmp)
		return err
	}
	if err := os.Rename(metaTmp, req.metaPath); err != nil {
		_ = os.Remove(metaTmp)
		return err
	}
	return c.enforceBudgetLocked(site)
}

type assetCacheFile struct {
	metaPath string
	bodyPath string
	accessed int64
	size     int64
}

func (c *assetCache) enforceBudgetLocked(site Site) error {
	root := filepath.Join(c.dir, strconv.FormatInt(site.ID, 10))
	files := make([]assetCacheFile, 0)
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		var meta assetCacheMeta
		if json.Unmarshal(data, &meta) != nil {
			return nil
		}
		bodyPath := strings.TrimSuffix(path, ".json") + ".body"
		files = append(files, assetCacheFile{metaPath: path, bodyPath: bodyPath, accessed: meta.AccessedAtMS, size: meta.Size})
		total += meta.Size
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].accessed < files[j].accessed })
	for _, file := range files {
		if total <= site.AssetCacheMaxBytes {
			break
		}
		_ = os.Remove(file.metaPath)
		_ = os.Remove(file.bodyPath)
		total -= file.size
	}
	return nil
}

func prepareAssetCacheResponse(resp *http.Response, cache *assetCache, site Site) error {
	cacheReq, _ := resp.Request.Context().Value(assetCacheContextKey{}).(*assetCacheRequest)
	if cacheReq == nil || resp.Body == nil {
		return nil
	}
	if cacheReq.method == http.MethodHead {
		return nil
	}
	if resp.ContentLength <= 0 || resp.ContentLength > maxAssetCacheObject {
		resp.Header.Set("X-Meridian-Cache", "BYPASS")
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAssetCacheObject+1))
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	if int64(len(body)) <= maxAssetCacheObject {
		_ = cache.write(site, cacheReq, resp, body, time.Now())
	}
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
	w.Header().Set("Content-Length", strconv.Itoa(len(hit.body)))
	w.WriteHeader(hit.meta.Status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(hit.body)
	}
}
