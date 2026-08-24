package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func insertTMDBPosterRegressionMedia(t *testing.T, app *App, posterPath string) int64 {
	t.Helper()

	result, err := app.db.db.Exec(
		"INSERT INTO sites (name, listen_port, target_url) VALUES (?, ?, ?)",
		"poster-regression", 19081, "http://127.0.0.1:8096",
	)
	if err != nil {
		t.Fatalf("insert poster regression site: %v", err)
	}
	siteID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read poster regression site id: %v", err)
	}

	nowMS := time.Now().UnixMilli()
	result, err = app.db.db.Exec(`INSERT INTO media_items
		(site_id, upstream_item_id, media_type, title, poster_path, created_at_ms, updated_at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		siteID, "poster-regression-item", "movie", "Poster Regression", posterPath, nowMS, nowMS,
	)
	if err != nil {
		t.Fatalf("insert poster regression media: %v", err)
	}
	mediaItemID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("read poster regression media id: %v", err)
	}
	return mediaItemID
}

func configureTMDBPosterRegressionService(app *App, server *httptest.Server) {
	client := server.Client()
	client.Timeout = 5 * time.Second
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	app.tmdb = newTMDBService(app.db, newTMDBClientForTest(client, server.URL, server.URL))
}

func requestTMDBPosterRegression(t *testing.T, app *App, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	app.handleWatchHistoryPoster(response, httptest.NewRequest(method, path, nil))
	return response
}

func TestWatchHistoryPosterHandlerGETAndHEADSuccess(t *testing.T) {
	var upstreamCalls atomic.Int32
	payload := []byte("jpeg-regression-payload")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/poster.jpg" {
			t.Errorf("poster upstream request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	app := newTestApp(t)
	mediaItemID := insertTMDBPosterRegressionMedia(t, app, "/poster.jpg")
	configureTMDBPosterRegressionService(app, server)
	path := "/api/watch-history/posters/" + strconv.FormatInt(mediaItemID, 10)

	getResponse := requestTMDBPosterRegression(t, app, http.MethodGet, path)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%q", getResponse.Code, getResponse.Body.String())
	}
	if !bytes.Equal(getResponse.Body.Bytes(), payload) {
		t.Fatalf("GET body=%q, want %q", getResponse.Body.Bytes(), payload)
	}
	if got := getResponse.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Fatalf("GET Content-Type=%q", got)
	}
	if got := getResponse.Header().Get("Content-Length"); got != strconv.Itoa(len(payload)) {
		t.Fatalf("GET Content-Length=%q, want %d", got, len(payload))
	}
	if got := getResponse.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("GET X-Content-Type-Options=%q", got)
	}
	if got := getResponse.Header().Get("Cache-Control"); !strings.Contains(got, "max-age=86400") {
		t.Fatalf("GET Cache-Control=%q", got)
	}

	headResponse := requestTMDBPosterRegression(t, app, http.MethodHead, path)
	if headResponse.Code != http.StatusOK {
		t.Fatalf("HEAD status=%d body=%q", headResponse.Code, headResponse.Body.String())
	}
	if headResponse.Body.Len() != 0 {
		t.Fatalf("HEAD unexpectedly returned %d body bytes", headResponse.Body.Len())
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("GET plus HEAD made %d upstream calls, want only the GET call", got)
	}
}

func TestWatchHistoryPosterHandlerRejectsRedirectAndInvalidContent(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		var finalCalls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/poster.jpg":
				http.Redirect(w, r, "/final.jpg", http.StatusFound)
			case "/final.jpg":
				finalCalls.Add(1)
				w.Header().Set("Content-Type", "image/jpeg")
				_, _ = w.Write([]byte("redirected-poster"))
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()

		app := newTestApp(t)
		mediaItemID := insertTMDBPosterRegressionMedia(t, app, "/poster.jpg")
		configureTMDBPosterRegressionService(app, server)
		response := requestTMDBPosterRegression(t, app, http.MethodGet, "/api/watch-history/posters/"+strconv.FormatInt(mediaItemID, 10))
		if response.Code != http.StatusNotFound {
			t.Fatalf("redirect status=%d body=%q", response.Code, response.Body.String())
		}
		if got := finalCalls.Load(); got != 0 {
			t.Fatalf("poster client followed redirect %d times", got)
		}
	})

	t.Run("unsupported MIME", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<html>not an image</html>"))
		}))
		defer server.Close()

		app := newTestApp(t)
		mediaItemID := insertTMDBPosterRegressionMedia(t, app, "/poster.jpg")
		configureTMDBPosterRegressionService(app, server)
		response := requestTMDBPosterRegression(t, app, http.MethodGet, "/api/watch-history/posters/"+strconv.FormatInt(mediaItemID, 10))
		if response.Code != http.StatusNotFound {
			t.Fatalf("unsupported MIME status=%d body=%q", response.Code, response.Body.String())
		}
		if got := response.Header().Get("Cache-Control"); got != "private, no-store" {
			t.Fatalf("unsupported MIME Cache-Control=%q", got)
		}
	})
}

func TestWatchHistoryPosterHandlerRejectsOversizedResponses(t *testing.T) {
	t.Run("known content length", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("Content-Length", strconv.Itoa(tmdbPosterMaxBytes+1))
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		app := newTestApp(t)
		mediaItemID := insertTMDBPosterRegressionMedia(t, app, "/poster.jpg")
		configureTMDBPosterRegressionService(app, server)
		response := requestTMDBPosterRegression(t, app, http.MethodGet, "/api/watch-history/posters/"+strconv.FormatInt(mediaItemID, 10))
		if response.Code != http.StatusNotFound {
			t.Fatalf("known-length oversized status=%d body=%q", response.Code, response.Body.String())
		}
	})

	t.Run("unknown content length", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "image/jpeg")
			w.WriteHeader(http.StatusOK)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			_, _ = w.Write(bytes.Repeat([]byte{0x42}, tmdbPosterMaxBytes+1))
		}))
		defer server.Close()

		app := newTestApp(t)
		mediaItemID := insertTMDBPosterRegressionMedia(t, app, "/poster.jpg")
		configureTMDBPosterRegressionService(app, server)
		response := requestTMDBPosterRegression(t, app, http.MethodGet, "/api/watch-history/posters/"+strconv.FormatInt(mediaItemID, 10))
		if response.Code != http.StatusNotFound {
			t.Fatalf("unknown-length oversized status=%d body=%q", response.Code, response.Body.String())
		}
		if got := response.Header().Get("Cache-Control"); got != "private, no-store" {
			t.Fatalf("unknown-length oversized Cache-Control=%q", got)
		}
	})
}

func TestWatchHistoryPosterHandlerRejectsIllegalPaths(t *testing.T) {
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("must-not-be-requested"))
	}))
	defer server.Close()

	app := newTestApp(t)
	invalidMediaItemID := insertTMDBPosterRegressionMedia(t, app, "/../evil.jpg")
	configureTMDBPosterRegressionService(app, server)

	for _, path := range []string{
		"/api/watch-history/posters/",
		"/api/watch-history/posters/not-a-number",
		"/api/watch-history/posters/-1",
		"/api/watch-history/posters/1/../../evil.jpg",
		"/api/watch-history/posters/" + strconv.FormatInt(invalidMediaItemID, 10),
	} {
		response := requestTMDBPosterRegression(t, app, http.MethodGet, path)
		if response.Code != http.StatusNotFound {
			t.Errorf("illegal path %q status=%d body=%q", path, response.Code, response.Body.String())
		}
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("illegal poster paths reached upstream %d times", got)
	}
}

func TestWatchHistoryAdditionalImagesUseStoredTMDBPaths(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backdrop.jpg" && r.URL.Path != "/still-1.jpg" && r.URL.Path != "/actor.jpg" {
			t.Errorf("unexpected image path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("image-payload"))
	}))
	defer server.Close()

	app := newTestApp(t)
	result, err := app.db.db.Exec(`INSERT INTO sites (name, listen_port, target_url) VALUES ('image-history', 19083, 'http://127.0.0.1:8096')`)
	if err != nil {
		t.Fatal(err)
	}
	siteID, _ := result.LastInsertId()
	result, err = app.db.db.Exec(`INSERT INTO media_items
		(site_id, upstream_item_id, media_type, title, poster_path, backdrop_path, stills_json, created_at_ms, updated_at_ms)
		VALUES (?, ?, 'movie', 'Image history', '/poster.jpg', '/backdrop.jpg', '["/still-1.jpg"]', ?, ?)`, siteID, "image-item", time.Now().UnixMilli(), time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	mediaItemID, _ := result.LastInsertId()
	if _, err := app.db.db.Exec(`UPDATE media_items SET cast_json=? WHERE id=?`, `[{"name":"Actor","profile_path":"/actor.jpg"}]`, mediaItemID); err != nil {
		t.Fatal(err)
	}
	configureTMDBPosterRegressionService(app, server)

	backdrop := httptest.NewRecorder()
	app.handleWatchHistoryBackdrop(backdrop, httptest.NewRequest(http.MethodGet, "/api/watch-history/backdrops/"+strconv.FormatInt(mediaItemID, 10), nil))
	if backdrop.Code != http.StatusOK || backdrop.Body.String() != "image-payload" {
		t.Fatalf("backdrop status=%d body=%q", backdrop.Code, backdrop.Body.String())
	}
	still := httptest.NewRecorder()
	app.handleWatchHistoryStill(still, httptest.NewRequest(http.MethodGet, "/api/watch-history/stills/"+strconv.FormatInt(mediaItemID, 10)+"/0", nil))
	if still.Code != http.StatusOK || still.Body.String() != "image-payload" {
		t.Fatalf("still status=%d body=%q", still.Code, still.Body.String())
	}
	invalid := httptest.NewRecorder()
	app.handleWatchHistoryStill(invalid, httptest.NewRequest(http.MethodGet, "/api/watch-history/stills/"+strconv.FormatInt(mediaItemID, 10)+"/12", nil))
	if invalid.Code != http.StatusNotFound {
		t.Fatalf("invalid still status=%d body=%q", invalid.Code, invalid.Body.String())
	}
	cast := httptest.NewRecorder()
	app.handleWatchHistoryCast(cast, httptest.NewRequest(http.MethodGet, "/api/watch-history/cast/"+strconv.FormatInt(mediaItemID, 10)+"/0", nil))
	if cast.Code != http.StatusOK || cast.Body.String() != "image-payload" {
		t.Fatalf("cast status=%d body=%q", cast.Code, cast.Body.String())
	}
}
