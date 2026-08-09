package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMainVideoStreamRequestClassification(t *testing.T) {
	tests := []struct {
		name   string
		method string
		target string
		want   bool
	}{
		{"mp4", http.MethodGet, "/media/movie.mp4", true},
		{"mixed-case mkv", http.MethodHead, "/media/Movie.MKV", true},
		{"video stream", http.MethodGet, "/Videos/1/stream", true},
		{"video stream opaque extension", http.MethodGet, "/Videos/1/stream.bin", true},
		{"emby original", http.MethodGet, "/emby/Videos/1/original", true},
		{"video download", http.MethodGet, "/Videos/1/download", true},
		{"item download", http.MethodGet, "/Items/1/download", true},
		{"static query", http.MethodGet, "/Videos/1?Static=true", true},
		{"hls manifest", http.MethodGet, "/Videos/1/stream.m3u8", false},
		{"dash manifest", http.MethodGet, "/Videos/1/manifest.mpd", false},
		{"hls segment", http.MethodGet, "/media/segment.ts", false},
		{"subtitle", http.MethodGet, "/Videos/1/Subtitles/2/Stream.ass", false},
		{"image", http.MethodGet, "/Items/1/Images/Primary.jpg", false},
		{"static asset", http.MethodGet, "/web/app.js", false},
		{"playback info", http.MethodGet, "/Items/1/PlaybackInfo", false},
		{"ordinary api", http.MethodGet, "/System/Info/Public", false},
		{"post video", http.MethodPost, "/Videos/1/stream", false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(test.method, "http://meridian.example"+test.target, nil)
			if got := isMainVideoStreamRequest(r); got != test.want {
				t.Fatalf("isMainVideoStreamRequest(%s %s) = %v, want %v", test.method, test.target, got, test.want)
			}
		})
	}
}

func TestMainVideoDirectModeRedirectsOnlyMainVideo(t *testing.T) {
	app := newTestApp(t)
	var upstreamHits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		_, _ = io.WriteString(w, "proxied:"+r.URL.Path)
	}))
	defer upstream.Close()

	port := freePort(t)
	site, err := app.db.CreateSiteRecord(Site{
		Name:                "main-video-direct",
		ListenPort:          port,
		IngressMode:         ingressModePort,
		TargetURL:           upstream.URL + "/base?origin_secret=must-not-leak",
		PlaybackMode:        "direct",
		MainVideoStreamMode: mainVideoStreamModeDirect,
		StreamHosts:         "[]",
		UAMode:              passthroughUAMode,
	})
	if err != nil {
		t.Fatalf("create direct site: %v", err)
	}
	releasePort(port)
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatalf("start direct site: %v", err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	resp, err := client.Get(baseURL + "/Videos/1/stream.mkv?api_key=client-visible")
	if err != nil {
		t.Fatalf("request direct main video: %v", err)
	}
	_ = resp.Body.Close()
	location := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusTemporaryRedirect || !strings.HasSuffix(location, "/base/Videos/1/stream.mkv?api_key=client-visible") || strings.Contains(location, "origin_secret") {
		t.Fatalf("direct response status=%d Location=%q", resp.StatusCode, location)
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Fatalf("direct main video reached upstream %d times", got)
	}

	for _, path := range []string{"/media/segment.ts", "/Videos/1/Subtitles/2/Stream.ass", "/System/Info/Public"} {
		resp, err = client.Get(baseURL + path)
		if err != nil {
			t.Fatalf("request proxied path %s: %v", path, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil || resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(body), "proxied:") {
			t.Fatalf("proxied path %s status=%d read=%v body=%q", path, resp.StatusCode, readErr, body)
		}
	}
	if got := upstreamHits.Load(); got != 3 {
		t.Fatalf("non-main-video upstream hits = %d, want 3", got)
	}
}

func TestMainVideoStreamModeDefaultsToProxyAndRejectsUnknownValues(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSiteRecord(Site{
		Name:         "main-video-default",
		ListenPort:   freePort(t),
		IngressMode:  ingressModePort,
		TargetURL:    "http://127.0.0.1:8096",
		PlaybackMode: "direct",
		StreamHosts:  "[]",
		UAMode:       passthroughUAMode,
	})
	if err != nil {
		t.Fatalf("create default site: %v", err)
	}
	if site.MainVideoStreamMode != mainVideoStreamModeProxy {
		t.Fatalf("default main_video_stream_mode = %q, want proxy", site.MainVideoStreamMode)
	}

	invalid := *site
	invalid.MainVideoStreamMode = "automatic"
	if err := app.db.UpdateSiteRecord(invalid); err == nil || !strings.Contains(err.Error(), "main_video_stream_mode") {
		t.Fatalf("unknown mode error = %v", err)
	}
}
