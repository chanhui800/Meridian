package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPathTargetMountedPlaybackInfoRewritesMediaBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = io.WriteString(w, "synthetic-video")
	}))
	t.Cleanup(backend.Close)
	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	frontend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"MediaSources":[{"Protocol":"Http","DirectStreamUrl":"%s/media/file.mp4?token=synthetic"}]}`, backend.URL)
	}))
	t.Cleanup(frontend.Close)
	frontendURL, err := url.Parse(frontend.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, mount := range []string{"/emby", "/jellyfin", "/alpha/emby"} {
		t.Run(mount, func(t *testing.T) {
			gateway := newPathTargetRegressionGateway(t)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
				gateway.URL+pathTargetRoutePrefix+"http/"+frontendURL.Host+mount+"/Items/1/PlaybackInfo", nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := gateway.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("PlaybackInfo status=%d", response.StatusCode)
			}
			want := pathTargetRedirectRoutePrefix + "http/" + backendURL.Host + "/media/file.mp4?token=synthetic"
			if !strings.Contains(string(body), want) {
				t.Fatalf("generic API mount %s left DirectStreamUrl outside the gateway: body=%s; want routed backend=%s", mount, body, want)
			}
		})
	}
}

func TestPathTargetCrossAuthorityPlaybackInfoRouteDropsClientCredentials(t *testing.T) {
	observed := make(chan *http.Request, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- r.Clone(context.Background())
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = io.WriteString(w, "synthetic-video")
	}))
	t.Cleanup(backend.Close)
	frontend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertPathTargetRegressionCredentials(t, r.Header, true)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"MediaSources":[{"Protocol":"Http","DirectStreamUrl":"%s/media/file.mp4?token=synthetic"}]}`, backend.URL)
	}))
	t.Cleanup(frontend.Close)
	frontendURL, err := url.Parse(frontend.URL)
	if err != nil {
		t.Fatal(err)
	}
	gateway := newPathTargetRegressionGateway(t)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		gateway.URL+pathTargetRoutePrefix+"http/"+frontendURL.Host+"/emby/Items/1/PlaybackInfo", nil)
	if err != nil {
		t.Fatal(err)
	}
	setPathTargetRegressionCredentials(request)
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("PlaybackInfo response status=%d err=%v body=%s", response.StatusCode, err, body)
	}
	var payload struct {
		MediaSources []struct {
			DirectStreamURL string `json:"DirectStreamUrl"`
		} `json:"MediaSources"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.MediaSources) != 1 {
		t.Fatalf("decode rewritten PlaybackInfo: err=%v body=%s", err, body)
	}
	route := payload.MediaSources[0].DirectStreamURL
	if !strings.HasPrefix(route, pathTargetRedirectRoutePrefix) {
		t.Fatalf("cross-authority media URL was not marked credentialless: %q", route)
	}
	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, gateway.URL+route, nil)
	if err != nil {
		t.Fatal(err)
	}
	setPathTargetRegressionCredentials(request)
	response, err = gateway.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	mediaBody, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(mediaBody) != "synthetic-video" {
		t.Fatalf("cross-authority media status=%d body=%q err=%v", response.StatusCode, mediaBody, err)
	}
	assertPathTargetRegressionCredentials(t, (<-observed).Header, false)
}

func TestPathTargetMountedClassifierLeavesOtherMethodsAndPaths(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"MediaSources":[{"Protocol":"Http","DirectStreamUrl":"http://media.invalid/movie.mp4"}]}`)
	}))
	t.Cleanup(origin.Close)
	upstream, _ := url.Parse(origin.URL)
	gateway := newPathTargetRegressionGateway(t)
	for _, test := range []struct{ method, path string }{
		{http.MethodGet, "/alpha/Items/1/PlaybackInfo/extra"},
		{http.MethodGet, "/alpha/Items//PlaybackInfo"},
		{http.MethodDelete, "/alpha/Items/1/PlaybackInfo"},
	} {
		request, err := http.NewRequestWithContext(t.Context(), test.method,
			gateway.URL+"/_proxy/v1/http/"+upstream.Host+test.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := gateway.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), "http://media.invalid/movie.mp4") {
			t.Fatalf("unclassified %s %s changed: status=%d body=%s err=%v", test.method, test.path, response.StatusCode, body, err)
		}
	}
}

func TestPathTargetMountedClassifierDoesNotBroadenFixedSites(t *testing.T) {
	payload := `{"MediaSources":[{"Protocol":"Http","DirectStreamUrl":"http://media.invalid/movie.mp4"}]}`
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(origin.Close)
	app := newTestApp(t)
	site, err := app.db.CreateSiteRecord(Site{
		Name: "fixed classifier regression", ListenPort: 8103,
		PublicHost: "fixed-classifier.example.test", IngressMode: ingressModeHost,
		TargetURL: origin.URL, PlaybackMode: "direct", StreamHosts: "[]", UAMode: passthroughUAMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	handler, _ := app.pm.PublicHostHandler(site.PublicHost)
	gateway := httptest.NewServer(handler)
	t.Cleanup(gateway.Close)
	response, err := gateway.Client().Get(gateway.URL + "/alpha/Items/1/PlaybackInfo")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(body) != payload {
		t.Fatalf("generic mount classification changed fixed-site behavior: status=%d body=%s err=%v", response.StatusCode, body, err)
	}
}
