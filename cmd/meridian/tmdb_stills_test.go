package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This uses TMDB's image response shape, including the null language that most
// backdrops/stills use. The server applies the documented image-language filter
// so omitting null recreates the one-picture gallery seen with zh-CN details.
const tmdbGalleryDetailFixture = `{
  "id":42,"title":"Example Movie","original_title":"Example Movie",
  "name":"Example Show","original_name":"Example Show",
  "release_date":"2024-01-02","first_air_date":"2024-01-02",
  "poster_path":"/poster.jpg","backdrop_path":"/localized.jpg",
  "images":{"backdrops":[
    {"aspect_ratio":1.778,"height":1080,"iso_639_1":"en","file_path":"/english.jpg","vote_average":5.4,"vote_count":3,"width":1920},
    {"aspect_ratio":1.778,"height":1080,"iso_639_1":null,"file_path":"/neutral-1.jpg","vote_average":5.4,"vote_count":3,"width":1920},
    {"aspect_ratio":1.778,"height":1080,"iso_639_1":"zh","file_path":"/localized.jpg","vote_average":5.3,"vote_count":2,"width":1920},
    {"aspect_ratio":1.778,"height":1080,"iso_639_1":null,"file_path":"/neutral-2.jpg","vote_average":5.3,"vote_count":2,"width":1920},
    {"aspect_ratio":1.778,"height":1080,"iso_639_1":null,"file_path":"/neutral-3.jpg","vote_average":5.3,"vote_count":2,"width":1920},
    {"aspect_ratio":1.778,"height":1080,"iso_639_1":null,"file_path":"/neutral-4.jpg","vote_average":5.3,"vote_count":2,"width":1920},
    {"aspect_ratio":1.778,"height":1080,"iso_639_1":null,"file_path":"/neutral-5.jpg","vote_average":5.3,"vote_count":2,"width":1920},
    {"aspect_ratio":1.778,"height":1080,"iso_639_1":null,"file_path":"/neutral-6.jpg","vote_average":5.3,"vote_count":2,"width":1920},
    {"aspect_ratio":1.778,"height":1080,"iso_639_1":null,"file_path":"/neutral-1.jpg","vote_average":5.3,"vote_count":2,"width":1920},
    {"aspect_ratio":1.778,"height":1080,"iso_639_1":"fr","file_path":"/french.jpg","vote_average":5.3,"vote_count":2,"width":1920},
    {"iso_639_1":null,"file_path":"https://untrusted.invalid/image.jpg"},
    {"iso_639_1":null,"file_path":"/../invalid.jpg"}
  ]},"credits":{"cast":[{"name":"Actor One","character":"Lead"}]}
}`

func writeTMDBFilteredGalleryFixture(t *testing.T, w http.ResponseWriter, r *http.Request, id int64) {
	t.Helper()
	var detail map[string]any
	if err := json.Unmarshal([]byte(tmdbGalleryDetailFixture), &detail); err != nil {
		t.Error(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	detail["id"] = id
	allowed := make(map[string]bool)
	allowed[strings.Split(r.URL.Query().Get("language"), "-")[0]] = true
	for _, language := range strings.Split(r.URL.Query().Get("include_image_language"), ",") {
		if language == "null" {
			language = ""
		}
		if language != "" || strings.Contains(r.URL.Query().Get("include_image_language"), "null") {
			allowed[strings.Split(language, "-")[0]] = true
		}
	}
	images := detail["images"].(map[string]any)
	filtered := make([]any, 0)
	for _, value := range images["backdrops"].([]any) {
		image := value.(map[string]any)
		language, _ := image["iso_639_1"].(string)
		if allowed[language] {
			filtered = append(filtered, image)
		}
	}
	images["backdrops"] = filtered
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(detail); err != nil {
		t.Error(err)
	}
}

func TestTMDBGalleryIncludesNeutralStillsInOneDetailRequest(t *testing.T) {
	want := []string{
		"/localized.jpg", "/neutral-1.jpg", "/neutral-2.jpg", "/neutral-3.jpg",
		"/neutral-4.jpg", "/neutral-5.jpg", "/neutral-6.jpg", "/english.jpg",
	}
	for _, testCase := range []struct {
		name string
		item tmdbLookupMedia
		path string
	}{
		{name: "movie", item: tmdbLookupMedia{MediaType: "movie", TMDBID: 42}, path: "/movie/42"},
		{name: "series", item: tmdbLookupMedia{MediaType: "series", TMDBID: 77}, path: "/tv/77"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var calls atomic.Int32
			token := strings.Repeat("fixture-token-", 4)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != testCase.path {
					t.Errorf("detail path=%q, want %q", r.URL.Path, testCase.path)
				}
				if strings.Contains(r.URL.String(), token) || r.Header.Get("Authorization") != "Bearer "+token {
					t.Error("TMDB bearer credential was not confined to the authorization header")
				}
				if r.URL.Query().Get("language") != "zh-CN" || r.URL.Query().Get("append_to_response") != "credits,images" {
					t.Error("details must retain the selected language and appended credits/images")
				}
				writeTMDBFilteredGalleryFixture(t, w, r, testCase.item.TMDBID)
			}))
			defer server.Close()
			client := newTMDBClientForTest(server.Client(), server.URL, server.URL)
			metadata, err := client.lookupMedia(t.Context(), token, "zh-CN", testCase.item)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(metadata.Stills, want) {
				t.Fatalf("gallery=%v, want all %d localized/neutral/fallback stills %v", metadata.Stills, len(want), want)
			}
			if calls.Load() != 1 {
				t.Fatalf("detail calls=%d, want one appended-images request", calls.Load())
			}
		})
	}
}

func TestTMDBGalleryPreservesRealImageCountsAndFallbacks(t *testing.T) {
	for _, testCase := range []struct {
		name, body, backdrop string
		stills               []string
	}{
		{name: "missing images retains backdrop fallback", body: `{"id":42,"backdrop_path":"/fallback.jpg"}`, backdrop: "/fallback.jpg", stills: []string{}},
		{name: "empty images retains backdrop fallback", body: `{"id":42,"backdrop_path":"/fallback.jpg","images":{"backdrops":[]}}`, backdrop: "/fallback.jpg", stills: []string{}},
		{name: "missing language is neutral", body: `{"id":42,"images":{"backdrops":[{"file_path":"/only.jpg"}]}}`, backdrop: "/only.jpg", stills: []string{"/only.jpg"}},
		{name: "null language is neutral", body: `{"id":42,"images":{"backdrops":[{"file_path":"/only.jpg","iso_639_1":null}]}}`, backdrop: "/only.jpg", stills: []string{"/only.jpg"}},
		{name: "no upstream pictures stays empty", body: `{"id":42,"images":{"backdrops":[]}}`, stills: []string{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(testCase.body))
			}))
			defer server.Close()
			metadata, err := newTMDBClientForTest(server.Client(), server.URL, server.URL).lookupMedia(
				t.Context(), strings.Repeat("fixture-token-", 4), "zh-CN", tmdbLookupMedia{MediaType: "movie", TMDBID: 42})
			if err != nil || !slices.Equal(metadata.Stills, testCase.stills) || metadata.BackdropPath != testCase.backdrop || calls.Load() != 1 {
				t.Fatalf("gallery=%v backdrop=%q calls=%d err=%v", metadata.Stills, metadata.BackdropPath, calls.Load(), err)
			}
		})
	}
}

func TestTMDBGalleryPrioritizesBeforeBoundingAndDeduplicatesPaths(t *testing.T) {
	images := &tmdbImages{Backdrops: []tmdbImage{
		{FilePath: "https://untrusted.invalid/remote.jpg"}, {FilePath: "/../invalid.jpg"},
		{FilePath: "/english.jpg", Language: "en"},
	}}
	want := []string{"/localized.jpg"}
	for i := 0; i < 15; i++ {
		imagePath := fmt.Sprintf("/neutral-%d.jpg", i)
		images.Backdrops = append(images.Backdrops, tmdbImage{FilePath: imagePath}, tmdbImage{FilePath: " " + imagePath + " "})
		if i < 11 {
			want = append(want, imagePath)
		}
	}
	images.Backdrops = append(images.Backdrops, tmdbImage{FilePath: "/localized.jpg", Language: "zh"})
	if got := normalizeTMDBImagesForLanguage(images, "zh-CN"); !slices.Equal(got, want) {
		t.Fatalf("bounded ranked gallery=%v, want %v", got, want)
	}
}

func TestTMDBGalleryWorkerStoresOnlyBoundedValidatedStills(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	nowMS := time.Now().UnixMilli()
	mediaID := writeTMDBRegressionEvent(t, database, watchHistoryTestEvent(site.ID, "bounded-gallery-store", nowMS, 100))
	job, found, err := database.claimTMDBJob(nowMS)
	if err != nil || !found || job.ID != mediaID {
		t.Fatalf("claim found=%v err=%v", found, err)
	}
	stills := []string{"https://untrusted.invalid/remote.jpg", "/../invalid.jpg"}
	want := make([]string, 0, 12)
	for i := 0; i < 15; i++ {
		imagePath := fmt.Sprintf("/stored-%d.jpg", i)
		stills = append(stills, imagePath, " "+imagePath+" ")
		if i < 12 {
			want = append(want, imagePath)
		}
	}
	if err := database.completeTMDBJob(mediaID, job.JobRevision, tmdbMetadata{
		TMDBType: "movie", TMDBID: 42, Stills: stills, MatchMethod: "tmdb_id",
	}, "zh-CN", nowMS); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"SELECT stills_json FROM media_items WHERE id=?", "SELECT stills_json FROM tmdb_cache WHERE tmdb_id=?"} {
		id := mediaID
		if strings.Contains(query, "tmdb_cache") {
			id = 42
		}
		var value string
		if err := database.db.QueryRow(query, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		var stored []string
		if err := json.Unmarshal([]byte(value), &stored); err != nil || !slices.Equal(stored, want) {
			t.Fatalf("stored gallery=%v err=%v, want %v", stored, err, want)
		}
	}
	entries, err := database.ListWatchHistory(WatchHistoryFilter{Limit: 10})
	if err != nil || len(entries) != 1 || !slices.Equal(entries[0].Stills, want) {
		t.Fatalf("history gallery entries=%d err=%v", len(entries), err)
	}
}
