package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func withStableTMDBTestSecret(t *testing.T) {
	t.Helper()
	originalSecret := append([]byte(nil), jwtSecret...)
	originalEphemeral := jwtSecretEphemeral
	jwtSecret = []byte(strings.Repeat("j", 32))
	jwtSecretEphemeral = false
	t.Cleanup(func() {
		jwtSecret = originalSecret
		jwtSecretEphemeral = originalEphemeral
	})
}

func TestTMDBSettingsAPIIsWriteOnly(t *testing.T) {
	withStableTMDBTestSecret(t)
	app := newTestApp(t)
	token := strings.Repeat("tmdb-token-", 8)
	body, _ := json.Marshal(map[string]any{
		"enabled": true, "token": token, "language": "zh-CN", "history_retention_days": 120,
	})
	response := httptest.NewRecorder()
	app.handleTMDBSettings(response, httptest.NewRequest(http.MethodPost, "/api/tmdb-settings", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), token) || strings.Contains(response.Body.String(), "token_ciphertext") {
		t.Fatal("TMDB settings response exposed a token")
	}

	response = httptest.NewRecorder()
	app.handleTMDBSettings(response, httptest.NewRequest(http.MethodGet, "/api/tmdb-settings", nil))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), token) {
		t.Fatalf("GET status=%d body=%s", response.Code, response.Body.String())
	}
	var public TMDBSettings
	if err := json.Unmarshal(response.Body.Bytes(), &public); err != nil {
		t.Fatalf("decode public settings: %v", err)
	}
	if !public.Configured || !public.Enabled || public.HistoryRetentionDays != 120 {
		t.Fatalf("public settings = %+v", public)
	}
}

func TestTMDBSettingsAPIReturnsCacheStats(t *testing.T) {
	withStableTMDBTestSecret(t)
	app := newTestApp(t)
	if _, err := app.db.db.Exec(`INSERT INTO tmdb_cache
		(tmdb_type, tmdb_id, language, title, overview, poster_path, updated_at_ms, expires_at_ms)
		VALUES ('movie', 123, 'zh-CN', '缓存影片', '简介', '/poster.jpg', ?, ?)`, time.Now().UnixMilli(), time.Now().Add(time.Hour).UnixMilli()); err != nil {
		t.Fatalf("insert cache row: %v", err)
	}
	response := httptest.NewRecorder()
	app.handleTMDBSettings(response, httptest.NewRequest(http.MethodGet, "/api/tmdb-settings", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", response.Code, response.Body.String())
	}
	var public TMDBSettings
	if err := json.Unmarshal(response.Body.Bytes(), &public); err != nil {
		t.Fatalf("decode public settings: %v", err)
	}
	if public.CacheEntries != 1 || public.CacheSizeBytes <= 0 {
		t.Fatalf("cache stats = %d entries, %d bytes; want one non-empty entry", public.CacheEntries, public.CacheSizeBytes)
	}
}

func TestTMDBSettingsRejectSavingWithEphemeralSecret(t *testing.T) {
	originalEphemeral := jwtSecretEphemeral
	jwtSecretEphemeral = true
	t.Cleanup(func() { jwtSecretEphemeral = originalEphemeral })
	app := newTestApp(t)
	body := []byte(`{"enabled":true,"token":"` + strings.Repeat("x", 40) + `"}`)
	response := httptest.NewRecorder()
	app.handleTMDBSettings(response, httptest.NewRequest(http.MethodPost, "/api/tmdb-settings", bytes.NewReader(body)))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "JWT_SECRET") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTMDBWorkerReleasesSQLiteBeforeNetwork(t *testing.T) {
	withStableTMDBTestSecret(t)
	app := newTestApp(t)
	nowMS := time.Now().UnixMilli()
	result, err := app.db.db.Exec("INSERT INTO sites (name, listen_port, target_url, watch_history_enabled) VALUES ('history', 19001, 'http://127.0.0.1:8096', 1)")
	if err != nil {
		t.Fatalf("insert site: %v", err)
	}
	siteID, _ := result.LastInsertId()
	result, err = app.db.db.Exec(`INSERT INTO media_items
		(site_id, upstream_item_id, media_type, title, production_year, created_at_ms, updated_at_ms)
		VALUES (?,?,?,?,?,?,?)`, siteID, "item-1", "movie", "Example Movie", 2024, nowMS, nowMS)
	if err != nil {
		t.Fatalf("insert media item: %v", err)
	}
	mediaID, _ := result.LastInsertId()
	if _, err := app.db.db.Exec("INSERT INTO tmdb_jobs (media_item_id, state, updated_at_ms) VALUES (?, 'pending', ?)", mediaID, nowMS); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	ciphertext, err := encryptTMDBReadToken(strings.Repeat("read-token-", 8))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE tmdb_settings SET enabled=1, token_ciphertext=?, credential_state='unknown' WHERE id=1", ciphertext); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var count int
		if err := app.db.db.QueryRow("SELECT COUNT(*) FROM sites").Scan(&count); err != nil || count != 1 {
			t.Errorf("database was unavailable during TMDB network call: count=%d err=%v", count, err)
		}
		if r.URL.Path != "/search/movie" && r.URL.Path != "/movie/42" {
			t.Errorf("path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/movie/42" {
			_, _ = w.Write([]byte(`{"id":42,"title":"Example Movie","original_title":"Example Movie","release_date":"2024-01-02","overview":"Summary","poster_path":"/poster.jpg","backdrop_path":"/backdrop.jpg","vote_average":8.4,"genres":[{"name":"Drama"}],"images":{"backdrops":[{"file_path":"/still-1.jpg"},{"file_path":"/still-2.jpg"}]},"credits":{"cast":[{"name":"Actor One","character":"Lead"}]}}`))
		} else {
			_, _ = w.Write([]byte(`{"results":[{"id":42,"title":"Example Movie","original_title":"Example Movie","release_date":"2024-01-02","overview":"Summary","poster_path":"/poster.jpg"}]}`))
		}
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 2 * time.Second
	service := newTMDBService(app.db, newTMDBClientForTest(client, server.URL, server.URL))
	service.runOne(t.Context(), time.Now())

	var tmdbID int64
	var posterPath, backdropPath, releaseDate, genresJSON, stillsJSON, castJSON, status string
	var rating float64
	if err := app.db.db.QueryRow("SELECT tmdb_id, poster_path, backdrop_path, release_date, vote_average, genres_json, stills_json, cast_json, match_status FROM media_items WHERE id=?", mediaID).Scan(&tmdbID, &posterPath, &backdropPath, &releaseDate, &rating, &genresJSON, &stillsJSON, &castJSON, &status); err != nil {
		t.Fatal(err)
	}
	if tmdbID != 42 || posterPath != "/poster.jpg" || status != "title" {
		t.Fatalf("metadata id=%d poster=%q status=%q cast=%q", tmdbID, posterPath, status, castJSON)
	}
	if backdropPath != "/backdrop.jpg" || releaseDate != "2024-01-02" || rating != 8.4 || !strings.Contains(genresJSON, "Drama") || !strings.Contains(stillsJSON, "/still-1.jpg") {
		t.Fatalf("extended metadata backdrop=%q date=%q rating=%v genres=%q stills=%q", backdropPath, releaseDate, rating, genresJSON, stillsJSON)
	}
	if !strings.Contains(castJSON, "Actor One") || !strings.Contains(castJSON, "Lead") {
		t.Fatalf("cast was not persisted: %q", castJSON)
	}
}

func TestTMDBPosterRejectsStoredArbitraryURL(t *testing.T) {
	app := newTestApp(t)
	nowMS := time.Now().UnixMilli()
	result, err := app.db.db.Exec("INSERT INTO sites (name, listen_port, target_url) VALUES ('history', 19002, 'http://127.0.0.1:8096')")
	if err != nil {
		t.Fatal(err)
	}
	siteID, _ := result.LastInsertId()
	result, err = app.db.db.Exec(`INSERT INTO media_items
		(site_id, upstream_item_id, media_type, title, poster_path, created_at_ms, updated_at_ms)
		VALUES (?,?,?,?,?,?,?)`, siteID, "item-2", "movie", "Unsafe", "https://example.com/evil.jpg", nowMS, nowMS)
	if err != nil {
		t.Fatal(err)
	}
	mediaID, _ := result.LastInsertId()
	service := newTMDBService(app.db, newTMDBClient(nil))
	if _, _, err := service.fetchPoster(t.Context(), mediaID); err == nil {
		t.Fatal("arbitrary poster URL was accepted")
	}
}
