package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTMDBReadTokenEncryptionUsesSeparateSecretDomain(t *testing.T) {
	secret := []byte(strings.Repeat("s", 32))
	token := strings.Repeat("token", 20)
	ciphertext, err := encryptTMDBReadTokenWithSecret(token, secret)
	if err != nil {
		t.Fatalf("encryptTMDBReadTokenWithSecret: %v", err)
	}
	if strings.Contains(ciphertext, token) {
		t.Fatal("ciphertext contains plaintext token")
	}
	plain, err := decryptTMDBReadTokenWithSecret(ciphertext, secret)
	if err != nil || plain != token {
		t.Fatalf("decrypt = %q, %v", plain, err)
	}
	if _, err := decryptTMDBReadTokenWithSecret(ciphertext, []byte(strings.Repeat("x", 32))); err == nil {
		t.Fatal("ciphertext decrypted with a different secret")
	}
	if _, err := decryptTelegramBotTokenWithSecret(ciphertext, secret); err == nil {
		t.Fatal("TMDB ciphertext crossed the Telegram key domain")
	}
}

func TestTMDBClientUsesBearerWithoutTokenInURL(t *testing.T) {
	token := strings.Repeat("read-token-", 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.String(), token) {
			t.Error("token leaked into URL")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"images": map[string]string{"secure_base_url": "https://image.tmdb.org/t/p/"}})
	}))
	defer server.Close()
	client := newTMDBClientForTest(server.Client(), server.URL, server.URL)
	if err := client.testCredentials(context.Background(), token); err != nil {
		t.Fatalf("testCredentials: %v", err)
	}
}

func TestTMDBClientRejectsRedirectAndOversizedJSON(t *testing.T) {
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":{"secure_base_url":"https://image.tmdb.org/t/p/"}}`))
	}))
	defer redirectTarget.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("large") == "1" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"padding":"` + strings.Repeat("x", tmdbJSONMaxBytes) + `"}`))
			return
		}
		http.Redirect(w, r, redirectTarget.URL, http.StatusFound)
	}))
	defer server.Close()

	noRedirect := server.Client()
	noRedirect.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	client := newTMDBClientForTest(noRedirect, server.URL, server.URL)
	if err := client.testCredentials(context.Background(), strings.Repeat("t", 40)); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect error = %v", err)
	}

	var dst map[string]any
	err := client.doJSON(context.Background(), strings.Repeat("t", 40), "/configuration", mapValues("large", "1"), &dst)
	if err == nil || !strings.Contains(err.Error(), "response_too_large") {
		t.Fatalf("oversized response error = %v", err)
	}
}

func TestTMDBClientClassifiesAuthRateLimitServerAndPermanentClientErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, _ := strconv.Atoi(r.URL.Query().Get("status"))
		if status == 0 {
			status = http.StatusInternalServerError
		}
		if status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "120")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":"error"}`))
	}))
	defer server.Close()
	client := newTMDBClientForTest(server.Client(), server.URL, server.URL)

	for _, testCase := range []struct {
		name       string
		status     int
		code       string
		retryAfter time.Duration
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, code: "auth"},
		{name: "forbidden", status: http.StatusForbidden, code: "auth"},
		{name: "rate limited", status: http.StatusTooManyRequests, code: "rate_limited", retryAfter: 2 * time.Minute},
		{name: "not found", status: http.StatusNotFound, code: "client_error"},
		{name: "server", status: http.StatusServiceUnavailable, code: "server"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var response map[string]any
			err := client.doJSON(t.Context(), strings.Repeat("t", 40), "/configuration", mapValues("status", strconv.Itoa(testCase.status)), &response)
			var apiErr *tmdbAPIError
			if !errors.As(err, &apiErr) || apiErr.Code != testCase.code || apiErr.StatusCode != testCase.status || apiErr.RetryAfter != testCase.retryAfter {
				t.Fatalf("error=%v api=%+v", err, apiErr)
			}
		})
	}
}

func TestTMDBClientClassifiesTimeoutAsNetworkError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"images":{"secure_base_url":"https://image.tmdb.org/t/p/"}}`))
	}))
	defer server.Close()
	httpClient := server.Client()
	httpClient.Timeout = 10 * time.Millisecond
	client := newTMDBClientForTest(httpClient, server.URL, server.URL)
	err := client.testCredentials(t.Context(), strings.Repeat("t", 40))
	var apiErr *tmdbAPIError
	if !errors.As(err, &apiErr) || apiErr.Code != "network" {
		t.Fatalf("timeout error=%v api=%+v", err, apiErr)
	}
}

func TestBoundedRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	if got := boundedRetryAfter("1", now); got != time.Minute {
		t.Fatalf("short Retry-After = %s", got)
	}
	if got := boundedRetryAfter("999999", now); got != 6*time.Hour {
		t.Fatalf("long Retry-After = %s", got)
	}
}

func TestTMDBEpisodeSearchDoesNotUseEpisodeYearAsSeriesYear(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/tv" && r.URL.Path != "/tv/77" {
			t.Fatalf("path = %s, want /search/tv", r.URL.Path)
		}
		if r.URL.Path == "/tv/77" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":77,"name":"Long Running Show","original_name":"Long Running Show","first_air_date":"2012-01-01","poster_path":"/show.jpg","credits":{"cast":[{"name":"Actor One","character":"Lead"}]}}`))
			return
		}
		if r.URL.Path == "/search/tv" {
			if got := r.URL.Query().Get("query"); got != "Long Running Show" {
				t.Fatalf("query = %q", got)
			}
			if got := r.URL.Query().Get("first_air_date_year"); got != "" {
				t.Fatalf("episode year leaked into series search: %q", got)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":77,"name":"Long Running Show","original_name":"Long Running Show","first_air_date":"2012-01-01","poster_path":"/show.jpg"}]}`))
	}))
	defer server.Close()
	client := newTMDBClientForTest(server.Client(), server.URL, server.URL)
	metadata, err := client.lookupMedia(t.Context(), strings.Repeat("t", 40), "zh-CN", tmdbLookupMedia{
		MediaType: "episode", Title: "Finale", SeriesName: "Long Running Show", ProductionYear: 2026,
	})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.TMDBType != "tv" || metadata.TMDBID != 77 || metadata.PosterPath != "/show.jpg" || len(metadata.Cast) != 1 || metadata.Cast[0].Name != "Actor One" {
		t.Fatalf("metadata = %+v", metadata)
	}
}

func TestTMDBExternalMatchFetchesDetailCreditsAndImages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/find/tt123":
			_, _ = w.Write([]byte(`{"movie_results":[{"id":42,"title":"Example Movie","release_date":"2024-01-02","poster_path":"/poster.jpg"}]}`))
		case "/movie/42":
			_, _ = w.Write([]byte(`{"id":42,"title":"Example Movie","release_date":"2024-01-02","poster_path":"/poster.jpg","backdrop_path":"/backdrop.jpg","vote_average":8.2,"genres":[{"name":"Drama"}],"images":{"backdrops":[{"file_path":"/still.jpg"}]},"credits":{"cast":[{"name":"Actor One","character":"Lead"}]}}`))
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client := newTMDBClientForTest(server.Client(), server.URL, server.URL)
	metadata, err := client.lookupMedia(t.Context(), strings.Repeat("t", 40), "zh-CN", tmdbLookupMedia{
		MediaType: "movie", Title: "Example Movie", IMDBID: "tt123",
	})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.TMDBID != 42 || metadata.BackdropPath != "/backdrop.jpg" || metadata.VoteAverage != 8.2 || len(metadata.Cast) != 1 || metadata.Cast[0].Name != "Actor One" || len(metadata.Stills) != 1 {
		t.Fatalf("metadata = %+v", metadata)
	}
}

func mapValues(key, value string) url.Values {
	return url.Values{key: {value}}
}
