package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPathIngressConsumesOnlyItsEntryBeforeApplicationPath(t *testing.T) {
	for _, test := range []struct{ prefix, applicationPath string }{
		{"/front", "/videos/item/stream"},
		{"/videos", "/videos/item/stream"},
		{"/emby", "/emby/videos/item/stream"},
		{"/videos", "/videos/item%2Fpart/stream"},
	} {
		t.Run(test.prefix+test.applicationPath, func(t *testing.T) {
			app := newTestApp(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.EscapedPath() != test.applicationPath || r.URL.RawQuery != "api_key=backend&sig=%2F%2B" || r.Header.Get("Range") != "bytes=2-5" {
					t.Errorf("application route changed after path entry: URI=%s Range=%s", r.URL.RequestURI(), r.Header.Get("Range"))
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "video/mp4")
				w.Header().Set("Content-Range", "bytes 2-5/8")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, "cdef")
			}))
			t.Cleanup(upstream.Close)
			site, err := app.db.CreateSiteRecord(Site{Name: "application path boundary", ListenPort: 8100,
				IngressMode: ingressModePath, PathPrefix: test.prefix, TargetURL: upstream.URL, UAMode: passthroughUAMode, StreamHosts: "[]"})
			if err != nil {
				t.Fatal(err)
			}
			if err := app.pm.StartSite(*site); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
			gateway := httptest.NewServer(app.publicHostRouter(http.NotFoundHandler()))
			t.Cleanup(gateway.Close)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, gateway.URL+test.prefix+test.applicationPath+"?api_key=backend&sig=%2F%2B", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Range", "bytes=2-5")
			response, err := gateway.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != http.StatusPartialContent || string(body) != "cdef" || response.Header.Get("Content-Range") != "bytes 2-5/8" {
				t.Fatalf("application response status/body/range=%d/%q/%q err=%v", response.StatusCode, body, response.Header.Get("Content-Range"), err)
			}
		})
	}
}

func TestPathIngressRepeatedCapabilityEntryKeepsMediaTarget(t *testing.T) {
	app := newTestApp(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/media/manifest.m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:1,\nsegment.ts?api_key=backend&sig=%2F%2B\n#EXT-X-ENDLIST\n")
		case "/media/segment.ts":
			if r.URL.RawQuery != "api_key=backend&sig=%2F%2B" || r.Header.Get("Range") != "bytes=2-5" {
				t.Errorf("capability lost signed query or Range: %s %v", r.URL, r.Header)
			}
			w.Header().Set("Content-Type", "video/mp2t")
			w.Header().Set("Content-Range", "bytes 2-5/8")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "cdef")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	if err := app.pm.ConfigureDynamicDiscovery([]byte(strings.Repeat("p", 32)), "panel.example.test", 9090, nil); err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "capability path compatibility", ListenPort: 8100,
		IngressMode: ingressModePath, PathPrefix: "/emos", TargetURL: upstream.URL, UAMode: passthroughUAMode, StreamHosts: "[]"})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	gateway := httptest.NewServer(app.publicHostRouter(http.NotFoundHandler()))
	t.Cleanup(gateway.Close)
	response, err := gateway.Client().Get(gateway.URL + "/emos/media/manifest.m3u8")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("manifest status=%d err=%v", response.StatusCode, err)
	}
	var child string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "/emos/_meridian/d/") {
			child = line
		}
	}
	if child == "" {
		t.Fatalf("manifest lacks a path-prefixed capability: %s", body)
	}
	for _, route := range []string{child, "/emos" + child, "/emos/emos/emby" + strings.TrimPrefix(child, "/emos")} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, gateway.URL+route, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Range", "bytes=2-5")
		media, err := gateway.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(media.Body)
		_ = media.Body.Close()
		if err != nil || media.StatusCode != http.StatusPartialContent || string(body) != "cdef" {
			t.Fatalf("capability route %s returned status=%d body=%q err=%v", route, media.StatusCode, body, err)
		}
	}
}
