package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// A fill writes only chunks already read for the downstream response. There is
// no background queue or second body-sized buffer. Slow disks can pace reads,
// while a cache write failure abandons the fill without changing upstream I/O.
type assetCacheFillBody struct {
	original  io.ReadCloser
	ctx       context.Context
	cache     *assetCache
	site      Site
	request   *assetCacheRequest
	tracker   *requestDiagnosticsTracker
	meta      assetCacheMeta
	declared  int64
	root      *os.Root
	file      *os.File
	tempName  string
	read      int64
	mu        sync.Mutex
	done      bool
	closing   atomic.Bool
	closeOnce sync.Once
	closeErr  error
}

func (c *assetCache) fillCurrentLocked(req *assetCacheRequest) bool {
	active := c.activeNamespaces[assetCacheNamespacePrefix(req.namespace)]
	return !c.clearing && req.epoch == c.epoch && (active == "" || active == req.namespace)
}

func (c *assetCache) newFillBody(site Site, req *assetCacheRequest, resp *http.Response, tracker *requestDiagnosticsTracker) (*assetCacheFillBody, string, error) {
	c.mu.Lock()
	current := c.fillCurrentLocked(req)
	c.mu.Unlock()
	if !current {
		return nil, "generation_changed", nil
	}
	if resp.Request.Context().Err() != nil {
		return nil, "canceled", nil
	}
	root, err := c.openRoot(true)
	if err != nil {
		return nil, "", err
	}
	if err := root.MkdirAll(filepath.Dir(req.bodyName), 0700); err != nil {
		_ = root.Close()
		return nil, "", err
	}
	tempName := req.bodyName + "." + rand.Text() + ".tmp"
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fillCurrentLocked(req) {
		_ = root.Close()
		return nil, "generation_changed", nil
	}
	file, err := root.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		_ = root.Close()
		return nil, "", err
	}
	body := &assetCacheFillBody{
		original: resp.Body, ctx: resp.Request.Context(), cache: c, site: site, request: req, tracker: tracker,
		meta:     assetCacheMeta{Status: resp.StatusCode, Headers: cacheableResponseHeaders(resp.Header)},
		declared: resp.ContentLength, root: root, file: file, tempName: tempName,
	}
	c.fills[body] = struct{}{}
	return body, "", nil
}

func (b *assetCacheFillBody) discardLocked(write, reason string) {
	if b.done {
		return
	}
	b.done = true
	if b.file != nil {
		_ = b.file.Close()
		b.file = nil
	}
	if b.root != nil {
		_ = b.root.Remove(b.tempName)
		_ = b.root.Close()
		b.root = nil
	}
	b.tracker.SetCacheWrite(write, reason)
	b.cache.mu.Lock()
	delete(b.cache.fills, b)
	b.cache.mu.Unlock()
}

func (b *assetCacheFillBody) abort(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.discardLocked("skipped", reason)
}

func (b *assetCacheFillBody) Read(payload []byte) (int, error) {
	if b.closing.Load() {
		return 0, io.ErrClosedPipe
	}
	n, upstreamErr := b.original.Read(payload)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return n, upstreamErr
	}
	if b.ctx.Err() != nil {
		b.discardLocked("skipped", "canceled")
		return n, upstreamErr
	}
	if b.closing.Load() {
		b.discardLocked("skipped", "incomplete")
		return n, upstreamErr
	}
	if n > 0 {
		b.read += int64(n)
		if b.read > maxAssetCacheObject {
			b.discardLocked("skipped", "too_large")
			return n, upstreamErr
		}
		if b.declared >= 0 && b.read > b.declared {
			b.discardLocked("skipped", "length_mismatch")
			return n, upstreamErr
		}
		written, err := b.file.Write(payload[:n])
		if err == nil && written != n {
			err = io.ErrShortWrite
		}
		if err != nil {
			b.discardLocked("write_error", assetCacheErrorReason(err))
			return n, upstreamErr
		}
	}
	if upstreamErr != nil {
		if errors.Is(upstreamErr, io.EOF) {
			b.completeLocked()
		} else {
			b.discardLocked("skipped", "io")
		}
	}
	return n, upstreamErr
}

func (b *assetCacheFillBody) completeLocked() {
	if b.read == 0 {
		b.discardLocked("skipped", "empty_body")
		return
	}
	if b.declared >= 0 && b.read != b.declared {
		b.discardLocked("skipped", "length_mismatch")
		return
	}
	if err := b.file.Close(); err != nil {
		b.file = nil
		b.discardLocked("write_error", assetCacheErrorReason(err))
		return
	}
	b.file = nil
	now := time.Now()
	b.meta.CreatedAtMS, b.meta.AccessedAtMS = now.UnixMilli(), now.UnixMilli()
	b.meta.ExpiresAtMS = now.Add(time.Duration(b.site.AssetCacheTTLSec) * time.Second).UnixMilli()
	b.meta.Size = b.read
	stored, reason, err := b.publish()
	if err != nil {
		b.discardLocked("write_error", assetCacheErrorReason(err))
	} else if !stored {
		b.discardLocked("skipped", reason)
	} else {
		b.discardLocked("stored", "")
	}
}

func (b *assetCacheFillBody) publish() (bool, string, error) {
	metaBytes, err := json.Marshal(b.meta)
	if err != nil {
		return false, "", err
	}
	c := b.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	if b.ctx.Err() != nil {
		return false, "canceled", nil
	}
	if b.closing.Load() {
		return false, "incomplete", nil
	}
	if !c.fillCurrentLocked(b.request) {
		return false, "generation_changed", nil
	}
	if err := c.ensureSizeIndexLocked(); err != nil {
		return false, "", err
	}
	metaTmp := b.tempName + ".json"
	defer b.root.Remove(metaTmp)
	if err := b.root.WriteFile(metaTmp, metaBytes, 0600); err != nil {
		return false, "", err
	}
	if err := b.root.Rename(b.tempName, b.request.bodyName); err != nil {
		return false, "", err
	}
	if err := b.root.Rename(metaTmp, b.request.metaName); err != nil {
		_ = b.root.Remove(b.request.bodyName)
		c.removeIndexedEntryLocked(b.request.bodyName)
		return false, "", err
	}
	info, err := b.root.Stat(b.request.bodyName)
	if err != nil {
		_ = c.removeEntryLocked(b.root, b.request.metaName, b.request.bodyName)
		return false, "", err
	}
	if info.Size() != b.read || !info.Mode().IsRegular() {
		_ = c.removeEntryLocked(b.root, b.request.metaName, b.request.bodyName)
		return false, "", errAssetCacheInvalidEntry
	}
	if siteID, ok := assetCacheSiteIDFromPath(b.request.bodyName); ok {
		c.addIndexedEntryLocked(assetCacheIndexedEntry{siteID: siteID, namespace: assetCacheNamespaceFromPath(b.request.bodyName), metaName: b.request.metaName, bodyName: b.request.bodyName, accessed: b.meta.AccessedAtMS, persistedAccessed: b.meta.AccessedAtMS, size: b.read, bodyInfo: info})
	}
	if err := c.enforceBudgetLocked(b.root, b.site); err != nil {
		return false, "", err
	}
	current, err := b.root.Stat(b.request.bodyName)
	if errors.Is(err, os.ErrNotExist) {
		c.removeIndexedEntryLocked(b.request.bodyName)
		return false, "evicted", nil
	}
	if err != nil {
		return false, "", err
	}
	if !os.SameFile(info, current) || current.Size() != b.read {
		return false, "", errAssetCacheInvalidEntry
	}
	return true, "", nil
}

func (b *assetCacheFillBody) Close() error {
	b.closeOnce.Do(func() {
		b.closing.Store(true)
		// Close must interrupt a pending network Read before taking the fill
		// lock; no lock is held while waiting for the upstream body.
		b.closeErr = b.original.Close()
		b.mu.Lock()
		defer b.mu.Unlock()
		reason := "incomplete"
		if b.ctx.Err() != nil {
			reason = "canceled"
		}
		b.discardLocked("skipped", reason)
	})
	return b.closeErr
}
