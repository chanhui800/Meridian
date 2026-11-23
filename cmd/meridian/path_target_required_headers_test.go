package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPathTargetRelativePlaybackPreservesRequiredHeadersAndMount(t *testing.T) {
	for _, test := range []struct {
		name, mount, field, reference, expectedPath string
		withSubtitle                               bool
	}{
		{"root stream", "/emby", "DirectStreamUrl", "/videos/item/stream", "/videos/item/stream", false},
		{"Emos mount", "/emby", "DirectStreamUrl", "/emya/video", "/emby/emya/video", false},
		{"nested mount transcode", "/alpha/emby", "TranscodingUrl", "/emya/video", "/alpha/emby/emya/video", false},
		{"Jellyfin subtitle", "/jellyfin", "DirectStreamUrl", "/emya/video", "/jellyfin/emya/video", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			gateway := newPathTargetRegressionGateway(t)
			const signedQuery = "api_key=backend-token&sig=%2F%2B"
			headers := map[string]string{"User-Agent": "required-player/1.0", "X-Backend-Token": "synthetic-backend-token"}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == test.mount+"/Items/item/PlaybackInfo" {
					source := map[string]any{
						"Protocol": "Http", test.field: test.reference + "?" + signedQuery,
						"RequiredHttpHeaders": headers,
					}
					if test.withSubtitle {
						source["MediaStreams"] = []any{map[string]any{"IsExternalUrl": true, "DeliveryUrl": "/subs/movie.vtt?" + signedQuery}}
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"MediaSources": []any{source}})
					return
				}
				if r.URL.Path != test.expectedPath && r.URL.Path != "/subs/movie.vtt" {
					http.NotFound(w, r)
					return
				}
				if r.URL.RawQuery != signedQuery || r.Header.Get("Range") != "bytes=2-5" || r.Header.Get("User-Agent") != headers["User-Agent"] || r.Header.Get("X-Backend-Token") != headers["X-Backend-Token"] {
					t.Errorf("media request lost its signed query, required headers or Range: path=%s query=%s headers=%v", r.URL.Path, r.URL.RawQuery, r.Header)
					http.Error(w, "missing media requirements", http.StatusForbidden)
					return
				}
				w.Header().Set("Content-Type", "video/mp4")
				w.Header().Set("Content-Range", "bytes 2-5/8")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, "cdef")
			}))
			t.Cleanup(upstream.Close)
			upstreamURL, _ := url.Parse(upstream.URL)
			response, err := gateway.Client().Get(gateway.URL + "/_proxy/v1/http/" + upstreamURL.Host + test.mount + "/Items/item/PlaybackInfo")
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				MediaSources []map[string]json.RawMessage
			}
			err = json.NewDecoder(response.Body).Decode(&payload)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK || len(payload.MediaSources) != 1 {
				t.Fatalf("PlaybackInfo status=%d sources=%d err=%v", response.StatusCode, len(payload.MediaSources), err)
			}
			var route string
			if err := json.Unmarshal(payload.MediaSources[0][test.field], &route); err != nil {
				t.Fatal(err)
			}
			if want := "/_proxy/v1/http/" + upstreamURL.Host + test.expectedPath + "?" + signedQuery; route != want {
				t.Fatalf("relative media route=%q, want %q", route, want)
			}
			var returnedHeaders map[string]string
			if err := json.Unmarshal(payload.MediaSources[0]["RequiredHttpHeaders"], &returnedHeaders); err != nil {
				t.Fatal(err)
			}
			for name, value := range headers {
				if returnedHeaders[name] != value {
					t.Fatalf("required header %s changed in PlaybackInfo", name)
				}
			}
			// Clients can append the rewritten route to their configured API base.
			for _, clientRoute := range []string{route, "/" + upstream.URL + test.mount + route} {
				request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, gateway.URL+clientRoute, nil)
				if err != nil {
					t.Fatal(err)
				}
				for name, value := range returnedHeaders {
					request.Header.Set(name, value)
				}
				request.Header.Set("Range", "bytes=2-5")
				media, err := gateway.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(media.Body)
				_ = media.Body.Close()
				if err != nil || media.StatusCode != http.StatusPartialContent || string(body) != "cdef" || media.Header.Get("Content-Range") != "bytes 2-5/8" {
					t.Fatalf("media status/body/range=%d/%q/%q err=%v", media.StatusCode, body, media.Header.Get("Content-Range"), err)
				}
			}
			if test.withSubtitle {
				var streams []struct {
					DeliveryURL string `json:"DeliveryUrl"`
				}
				if err := json.Unmarshal(payload.MediaSources[0]["MediaStreams"], &streams); err != nil {
					t.Fatal(err)
				}
				if len(streams) != 1 || !strings.HasPrefix(streams[0].DeliveryURL, "/_proxy/v1/http/"+upstreamURL.Host+"/subs/movie.vtt?") {
					t.Fatalf("external relative subtitle lost its target: %+v", streams)
				}
			}
		})
	}
}
