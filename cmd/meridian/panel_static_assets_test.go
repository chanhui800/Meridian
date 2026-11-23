package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"meridian/web"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

func TestPanelStaticFingerprintCompressionAndValidation(t *testing.T) {
	source := []byte(strings.Repeat("console.log('fixture');\n", 200))
	files := fstest.MapFS{
		"index.html":  {Data: []byte(`<html><script src="/js/app.js?v=dev"></script><link href="/css/app.css?v=dev" rel="stylesheet"></html>`)},
		"js/app.js":   {Data: source},
		"css/app.css": {Data: []byte(strings.Repeat("body { color: black; }\n", 100))},
	}
	handler := staticHandler(files)
	get := func(target, encoding, tag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		r.Header.Set("Accept-Encoding", encoding)
		if tag != "" {
			r.Header.Set("If-None-Match", tag)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	index := get("/", "gzip", "")
	if index.Code != 200 || !strings.Contains(index.Header().Get("Cache-Control"), "no-store") || strings.Contains(index.Body.String(), "v=dev") {
		t.Fatalf("index did not replace fingerprints safely: status=%d headers=%v body=%s", index.Code, index.Header(), index.Body.String())
	}
	match := regexp.MustCompile(`/js/app\.js\?v=([a-f0-9]{64})`).FindStringSubmatch(index.Body.String())
	if len(match) != 2 {
		t.Fatal("missing content-addressed script URL")
	}
	plain := get(match[0], "identity", "")
	if plain.Code != 200 || !bytes.Equal(plain.Body.Bytes(), source) || !strings.Contains(plain.Header().Get("Cache-Control"), "immutable") || plain.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("plain response mismatch: %d %v", plain.Code, plain.Header())
	}
	compressed := get(match[0], "br, gzip;q=0.8", "")
	if compressed.Header().Get("Content-Encoding") != "gzip" || compressed.Body.Len() >= len(source) {
		t.Fatal("precompressed representation was not selected")
	}
	reader, err := gzip.NewReader(compressed.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	decoded, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(decoded, source) {
		t.Fatal("gzip changed response bytes")
	}
	if compressed.Header().Get("ETag") == plain.Header().Get("ETag") {
		t.Fatal("different representations reused the same strong validator")
	}
	if got := get(match[0], "gzip", compressed.Header().Get("ETag")); got.Code != http.StatusNotModified || got.Body.Len() != 0 {
		t.Fatalf("conditional compressed response status=%d", got.Code)
	}
	for _, target := range []string{"/js/app.js", "/js/app.js?v=dev", "/js/app.js?v=older", match[0] + "&v=" + match[1]} {
		if got := get(target, "", ""); !strings.Contains(got.Header().Get("Cache-Control"), "no-store") {
			t.Fatalf("unversioned/stale/ambiguous URL became immutable: %s", target)
		}
	}
	rangeRequest := httptest.NewRequest(http.MethodGet, match[0], nil)
	rangeRequest.Header.Set("Range", "bytes=0-6")
	rangeRequest.Header.Set("Accept-Encoding", "gzip")
	rangeResponse := httptest.NewRecorder()
	handler.ServeHTTP(rangeResponse, rangeRequest)
	if rangeResponse.Code != http.StatusPartialContent || rangeResponse.Header().Get("Content-Encoding") != "" || !bytes.Equal(rangeResponse.Body.Bytes(), source[:7]) {
		t.Fatal("Range did not address original representation")
	}
	head := httptest.NewRecorder()
	headRequest := httptest.NewRequest(http.MethodHead, match[0], nil)
	headRequest.Header.Set("Accept-Encoding", "gzip")
	handler.ServeHTTP(head, headRequest)
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("HEAD did not match compressed GET headers")
	}
	if fallback := get("/legacy-client-path", "", ""); fallback.Body.String() != index.Body.String() || !strings.Contains(fallback.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("SPA fallback did not use current fingerprinted HTML")
	}
	files["js/app.js"] = &fstest.MapFile{Data: []byte("changed()")}
	updated := staticHandler(files)
	oldURL := httptest.NewRecorder()
	updated.ServeHTTP(oldURL, httptest.NewRequest(http.MethodGet, match[0], nil))
	if oldURL.Header().Get("Cache-Control") != "no-store, no-cache, must-revalidate" || oldURL.Body.String() != "changed()" {
		t.Fatal("old fingerprint incorrectly cached updated bytes")
	}
}

func TestPanelAcceptsGzipQuality(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   bool
	}{
		{"", false}, {"br", false}, {"gzip", true}, {"gzip;q=0", false}, {"*;q=1,gzip;q=0", false},
		{"gzip;q=0.5, br", true}, {"*;q=0.5", true}, {"gzip;q=invalid", false}, {"gzip;q=NaN", false}, {"gzip;q=2", false},
	} {
		if got := panelAcceptsGzip(tc.header); got != tc.want {
			t.Errorf("header %q got %v want %v", tc.header, got, tc.want)
		}
	}
}

func TestPanelStaticFilesStillUseCurrentSources(t *testing.T) {
	files, err := fs.Sub(web.StaticFiles, "static")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	staticHandler(files).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "?v=dev") {
		t.Fatal("embedded HTML did not fingerprint current source assets")
	}
}
