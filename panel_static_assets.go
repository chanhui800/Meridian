package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type panelStaticAsset struct {
	body        []byte
	gzipBody    []byte
	fingerprint string
	contentType string
}

type panelStaticHandler struct {
	files    fs.FS
	fallback http.Handler
	assets   map[string]panelStaticAsset
	index    []byte
}

var panelAssetReference = regexp.MustCompile(`((?:src|href)="/)([^"?#]+)(?:\?[^"\r\n]*)?(")`)

// Immutable bytes and compressed representations are prepared once, outside
// the request path. Only an exact content fingerprint enables long caching;
// legacy and mismatched URLs remain safe across rolling version updates.
func newPanelStaticHandler(files fs.FS) http.Handler {
	handler := &panelStaticHandler{files: files, fallback: http.FileServer(http.FS(files)), assets: make(map[string]panelStaticAsset)}
	_ = fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return nil
		}
		extension := strings.ToLower(path.Ext(name))
		if extension != ".js" && extension != ".css" && extension != ".svg" {
			return nil
		}
		body, err := fs.ReadFile(files, name)
		if err != nil {
			return nil
		}
		digest := sha256.Sum256(body)
		asset := panelStaticAsset{body: body, fingerprint: hex.EncodeToString(digest[:]), contentType: mime.TypeByExtension(extension)}
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err = writer.Write(body); err == nil {
			err = writer.Close()
		}
		if err == nil && compressed.Len() < len(body) {
			asset.gzipBody = compressed.Bytes()
		}
		handler.assets["/"+name] = asset
		return nil
	})
	if index, err := fs.ReadFile(files, "index.html"); err == nil {
		handler.index = panelAssetReference.ReplaceAllFunc(index, func(reference []byte) []byte {
			parts := panelAssetReference.FindSubmatch(reference)
			asset, ok := handler.assets["/"+string(parts[2])]
			if !ok {
				return reference
			}
			return []byte(string(parts[1]) + string(parts[2]) + "?v=" + asset.fingerprint + string(parts[3]))
		})
	}
	return handler
}

func panelStaticNoStore(header http.Header) {
	header.Set("Cache-Control", "no-store, no-cache, must-revalidate")
	header.Set("Pragma", "no-cache")
	header.Set("Expires", "0")
}

func panelAcceptsGzip(value string) bool {
	explicit, wildcard := -1.0, 0.0
	for _, encoding := range strings.Split(value, ",") {
		parts := strings.Split(encoding, ";")
		name := strings.ToLower(strings.TrimSpace(parts[0]))
		if name != "gzip" && name != "*" {
			continue
		}
		quality := 1.0
		for _, option := range parts[1:] {
			key, number, found := strings.Cut(strings.TrimSpace(option), "=")
			if found && strings.EqualFold(strings.TrimSpace(key), "q") {
				parsed, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
				if err != nil || parsed != parsed || parsed < 0 || parsed > 1 {
					quality = 0
				} else {
					quality = parsed
				}
			}
		}
		if name == "gzip" {
			explicit = quality
		} else {
			wildcard = quality
		}
	}
	if explicit >= 0 {
		return explicit > 0
	}
	return wildcard > 0
}

func (handler *panelStaticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if asset, ok := handler.assets[r.URL.Path]; ok {
		versions := r.URL.Query()["v"]
		if len(versions) == 1 && versions[0] == asset.fingerprint {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			panelStaticNoStore(w.Header())
		}
		w.Header().Set("Content-Type", asset.contentType)
		w.Header().Add("Vary", "Accept-Encoding")
		payload, tag := asset.body, asset.fingerprint
		// Byte ranges address the original representation. Do not combine
		// compressed offsets and uncompressed Range/If-Range validators.
		if len(asset.gzipBody) > 0 && r.Header.Get("Range") == "" && panelAcceptsGzip(r.Header.Get("Accept-Encoding")) {
			payload, tag = asset.gzipBody, tag+"-gzip"
			w.Header().Set("Content-Encoding", "gzip")
		}
		w.Header().Set("ETag", `"`+tag+`"`)
		http.ServeContent(w, r, path.Base(r.URL.Path), time.Time{}, bytes.NewReader(payload))
		return
	}
	panelStaticNoStore(w.Header())
	if (r.URL.Path == "/" || r.URL.Path == "/index.html") && handler.index != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(handler.index))
		return
	}
	if file, err := handler.files.Open(strings.TrimPrefix(r.URL.Path, "/")); err == nil {
		_ = file.Close()
		handler.fallback.ServeHTTP(w, r)
		return
	}
	if handler.index != nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(handler.index))
		return
	}
	handler.fallback.ServeHTTP(w, r)
}
