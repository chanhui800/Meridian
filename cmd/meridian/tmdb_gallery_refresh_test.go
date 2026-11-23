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

func seedLegacyTMDBGallery(t *testing.T, database *DB, siteID int64, upstreamItem, mediaType string, tmdbID int64, version int, nowMS int64) int64 {
	t.Helper()
	event := watchHistoryTestEvent(siteID, upstreamItem, nowMS, 100)
	event.UpstreamItemID = upstreamItem
	event.MediaType = mediaType
	event.TMDBType = tmdbTypeForMedia(mediaType)
	if mediaType == "episode" {
		event.Title = "Local episode " + upstreamItem
		event.SeriesName = "Example Show"
		event.SeasonNumber = 1
		event.EpisodeNumber = 2
	}
	mediaID := writeTMDBRegressionEvent(t, database, event)
	if _, err := database.db.Exec(`UPDATE media_items SET tmdb_type=?, tmdb_id=?,
		overview='legacy overview', poster_path='/poster.jpg', backdrop_path='/localized.jpg',
		stills_json='["/localized.jpg"]', cast_json='[{"name":"Legacy Actor"}]',
		details_version=?, metadata_updated_at_ms=?, match_status='tmdb_id' WHERE id=?`,
		tmdbTypeForMedia(mediaType), tmdbID, version, nowMS, mediaID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`UPDATE tmdb_jobs SET state='done', attempts=3,
		next_attempt_at_ms=0, lease_until_ms=0, last_error_code='', revision=7 WHERE media_item_id=?`, mediaID); err != nil {
		t.Fatal(err)
	}
	return mediaID
}

func seedTMDBGalleryCache(t *testing.T, database *DB, tmdbType string, tmdbID int64, version int, nowMS int64) {
	t.Helper()
	if _, err := database.db.Exec(`INSERT INTO tmdb_cache
		(tmdb_type, tmdb_id, language, title, overview, poster_path, backdrop_path, stills_json,
		details_version, updated_at_ms, expires_at_ms)
		VALUES (?,?,'zh-CN','Legacy title','legacy overview','/poster.jpg','/localized.jpg',
		'["/localized.jpg"]',?,?,?)`, tmdbType, tmdbID, version, nowMS, nowMS+tmdbCacheLifetime.Milliseconds()); err != nil {
		t.Fatal(err)
	}
}

func configureTMDBGalleryWorker(t *testing.T, database *DB) string {
	t.Helper()
	token := strings.Repeat("gallery-fixture-token-", 3)
	ciphertext, err := encryptTMDBReadToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`UPDATE tmdb_settings SET enabled=1, token_ciphertext=?, credential_state=? WHERE id=1`,
		ciphertext, tmdbCredentialReady); err != nil {
		t.Fatal(err)
	}
	return token
}

func TestTMDBGalleryUpgradeMigrationPreservesUsableDetails(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	nowMS := time.Now().Add(-time.Minute).UnixMilli()
	legacyID := seedLegacyTMDBGallery(t, database, site.ID, "upgrade-legacy", "movie", 42, 2, nowMS)
	freshID := seedLegacyTMDBGallery(t, database, site.ID, "upgrade-fresh", "movie", 77, tmdbDetailsVersion, nowMS)
	seedTMDBGalleryCache(t, database, "movie", 42, 2, nowMS)
	seedTMDBGalleryCache(t, database, "movie", 77, tmdbDetailsVersion, nowMS)
	if _, err := database.db.Exec("UPDATE tmdb_settings SET details_schema_version=2 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := database.migrate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		id      int64
		version int
	}{{legacyID, 2}, {freshID, tmdbDetailsVersion}} {
		var overview, poster, stills, state, status string
		var version int
		var updatedMS, revision int64
		if err := database.db.QueryRow(`SELECT m.overview,m.poster_path,m.stills_json,m.details_version,
			m.metadata_updated_at_ms,m.match_status,j.state,j.revision FROM media_items m
			JOIN tmdb_jobs j ON j.media_item_id=m.id WHERE m.id=?`, item.id).Scan(
			&overview, &poster, &stills, &version, &updatedMS, &status, &state, &revision); err != nil {
			t.Fatal(err)
		}
		if overview != "legacy overview" || poster != "/poster.jpg" || stills != `["/localized.jpg"]` ||
			version != item.version || updatedMS != nowMS || status != "tmdb_id" || state != "done" || revision != 7 {
			t.Fatalf("migration reset usable details: overview=%q poster=%q stills=%q version=%d updated=%d status=%q state=%q revision=%d",
				overview, poster, stills, version, updatedMS, status, state, revision)
		}
	}
	var cacheRows, marker, schema int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM tmdb_cache").Scan(&cacheRows); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRow("SELECT details_schema_version FROM tmdb_settings WHERE id=1").Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRow("PRAGMA user_version").Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if cacheRows != 2 || marker != 2 || schema != databaseSchemaVersion {
		t.Fatalf("upgrade state cache=%d marker=%d schema=%d", cacheRows, marker, schema)
	}
}

func TestTMDBGalleryRefreshWalkIsBoundedAndSkipsIneligibleJobs(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	nowMS := time.Now().UnixMilli()
	ids := make([]int64, 0, 76)
	for i := 0; i < 76; i++ {
		version := tmdbDetailsVersion
		if i < 9 || i >= 50 {
			version = 1 + i%2
		}
		ids = append(ids, seedLegacyTMDBGallery(t, database, site.ID, fmt.Sprintf("bounded-%d", i), "movie", 42, version, nowMS))
	}
	for _, change := range []struct {
		query string
		id    int64
	}{
		{`UPDATE media_items SET media_type='' WHERE id=?`, ids[0]},
		{`UPDATE media_items SET tmdb_id=0 WHERE id=?`, ids[1]},
		{`UPDATE tmdb_jobs SET last_error_code='no_confident_match' WHERE media_item_id=?`, ids[2]},
		{`UPDATE tmdb_jobs SET state='retry',next_attempt_at_ms=9999999999999 WHERE media_item_id=?`, ids[3]},
		{`UPDATE tmdb_jobs SET state='running',lease_until_ms=9999999999999 WHERE media_item_id=?`, ids[4]},
		{`UPDATE tmdb_jobs SET state='pending' WHERE media_item_id=?`, ids[5]},
		{`UPDATE media_items SET metadata_updated_at_ms=0 WHERE id=?`, ids[6]},
		{`UPDATE media_items SET match_status='no_confident_match' WHERE id=?`, ids[7]},
		{`UPDATE media_items SET match_status='insufficient_metadata' WHERE id=?`, ids[8]},
		{`DELETE FROM tmdb_jobs WHERE media_item_id=?`, ids[75]},
	} {
		if _, err := database.db.Exec(change.query, change.id); err != nil {
			t.Fatal(err)
		}
	}
	service := newTMDBService(database, nil)
	for wake, expected := range []int{0, 10, 10, 6, 0} {
		queued, err := service.refreshLegacyDetails(nowMS)
		if err != nil {
			t.Fatal(err)
		}
		if queued != expected || queued > tmdbJobsPerWake {
			t.Fatalf("wake %d queued=%d, want %d with maximum %d", wake, queued, expected, tmdbJobsPerWake)
		}
		if wake == 0 && (service.detailsRefreshCursor != ids[49] || service.detailsRefreshComplete) {
			t.Fatalf("first scan cursor=%d complete=%v, want exactly the first %d rows", service.detailsRefreshCursor, service.detailsRefreshComplete, tmdbDetailsRefreshScanLimit)
		}
	}
	var queuedRows, changedIneligible int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM tmdb_jobs WHERE media_item_id>=? AND state='pending'", ids[50]).Scan(&queuedRows); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRow("SELECT COUNT(*) FROM tmdb_jobs WHERE media_item_id<=? AND revision<>7", ids[8]).Scan(&changedIneligible); err != nil {
		t.Fatal(err)
	}
	if queuedRows != 26 || changedIneligible != 0 || !service.detailsRefreshComplete {
		t.Fatalf("queued=%d ineligible changes=%d complete=%v", queuedRows, changedIneligible, service.detailsRefreshComplete)
	}
	var preserved int
	if err := database.db.QueryRow(`SELECT COUNT(*) FROM media_items WHERE overview='legacy overview'
		AND poster_path='/poster.jpg' AND stills_json='["/localized.jpg"]'`).Scan(&preserved); err != nil {
		t.Fatal(err)
	}
	if preserved != len(ids) {
		t.Fatalf("only %d of %d existing copies survived queuing", preserved, len(ids))
	}
}

func TestTMDBGalleryRefreshReusesNewCacheForExistingHistory(t *testing.T) {
	withStableTMDBTestSecret(t)
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	now := time.Now()
	ids := []int64{
		seedLegacyTMDBGallery(t, database, site.ID, "movie-one", "movie", 42, 1, now.UnixMilli()),
		seedLegacyTMDBGallery(t, database, site.ID, "movie-two", "movie", 42, 2, now.UnixMilli()),
		seedLegacyTMDBGallery(t, database, site.ID, "movie-three", "movie", 42, 2, now.UnixMilli()),
		seedLegacyTMDBGallery(t, database, site.ID, "episode-one", "episode", 77, 2, now.UnixMilli()),
		seedLegacyTMDBGallery(t, database, site.ID, "episode-two", "episode", 77, 2, now.UnixMilli()),
	}
	seedTMDBGalleryCache(t, database, "movie", 42, 2, now.UnixMilli())
	seedTMDBGalleryCache(t, database, "tv", 77, 2, now.UnixMilli())
	if _, found, err := database.cachedTMDBMetadata(tmdbLookupMedia{MediaType: "movie", TMDBID: 42}, "zh-CN", now.UnixMilli()); err != nil || found {
		t.Fatalf("old gallery cache remained eligible: found=%v err=%v", found, err)
	}
	token := configureTMDBGalleryWorker(t, database)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.Contains(r.URL.String(), token) || r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("TMDB credential was not confined to its bearer header")
		}
		switch r.URL.Path {
		case "/movie/42":
			writeTMDBFilteredGalleryFixture(t, w, r, 42)
		case "/tv/77":
			writeTMDBFilteredGalleryFixture(t, w, r, 77)
		default:
			t.Errorf("refresh searched or misrouted a resolved media identity: %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	service := newTMDBService(database, newTMDBClientForTest(server.Client(), server.URL, server.URL))
	if queued, err := service.refreshLegacyDetails(now.UnixMilli()); err != nil || queued != len(ids) {
		t.Fatalf("refresh queued=%d err=%v", queued, err)
	}
	for range ids {
		if processed, err := service.runOne(t.Context(), now.Add(time.Millisecond)); err != nil || !processed {
			t.Fatalf("worker processed=%v err=%v", processed, err)
		}
	}
	if processed, err := service.runOne(t.Context(), now.Add(time.Second)); err != nil || processed {
		t.Fatalf("worker repeated an already upgraded gallery: processed=%v err=%v", processed, err)
	}
	if queued, err := newTMDBService(database, nil).refreshLegacyDetails(now.UnixMilli()); err != nil || queued != 0 {
		t.Fatalf("restart requeued upgraded galleries: queued=%d err=%v", queued, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("shared movie/series gallery made %d API calls, want one per title", calls.Load())
	}
	want := []string{"/localized.jpg", "/neutral-1.jpg", "/neutral-2.jpg", "/neutral-3.jpg", "/neutral-4.jpg", "/neutral-5.jpg", "/neutral-6.jpg", "/english.jpg"}
	for _, id := range ids {
		var version int
		var state, stills string
		if err := database.db.QueryRow(`SELECT m.details_version,m.stills_json,j.state FROM media_items m
			JOIN tmdb_jobs j ON j.media_item_id=m.id WHERE m.id=?`, id).Scan(&version, &stills, &state); err != nil {
			t.Fatal(err)
		}
		if version != tmdbDetailsVersion || state != "done" || !slices.Equal(decodeWatchHistoryStills(stills), want) {
			t.Fatalf("item %d version=%d state=%q stills=%s", id, version, state, stills)
		}
	}
	entries, err := database.ListWatchHistory(WatchHistoryFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	// The two episodes remain grouped under their existing series identity.
	if len(entries) != 4 {
		t.Fatalf("history entries=%d, want three movies and one grouped series", len(entries))
	}
	var storedSessions int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM watch_sessions").Scan(&storedSessions); err != nil || storedSessions != len(ids) {
		t.Fatalf("history sessions=%d, want %d: %v", storedSessions, len(ids), err)
	}
	for _, entry := range entries {
		if !slices.Equal(entry.Stills, want) || entry.PositionTicks != 100 {
			t.Fatalf("history lost gallery or playback progress: stills=%v position=%d", entry.Stills, entry.PositionTicks)
		}
		if entry.MediaType == "episode" && (!strings.HasPrefix(entry.Title, "Local episode ") || entry.SeriesName != "Example Show") {
			t.Fatalf("episode identity changed: title=%q series=%q", entry.Title, entry.SeriesName)
		}
	}
	public, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), token) || strings.Contains(string(public), "untrusted.invalid") {
		t.Fatal("history exposed a TMDB credential or arbitrary remote image URL")
	}
}

func TestTMDBGalleryRefreshTransientFailureRetainsDetails(t *testing.T) {
	withStableTMDBTestSecret(t)
	for _, mediaType := range []string{"movie", "episode"} {
		for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests} {
			t.Run(fmt.Sprintf("%s-%d", mediaType, status), func(t *testing.T) {
				database := openWatchHistoryTestDB(t)
				site := createWatchHistoryTestSite(t, database, true)
				now := time.Now()
				id := seedLegacyTMDBGallery(t, database, site.ID, "transient-gallery", mediaType, 42, 2, now.UnixMilli())
				seedTMDBGalleryCache(t, database, tmdbTypeForMedia(mediaType), 42, 2, now.UnixMilli())
				token := configureTMDBGalleryWorker(t, database)
				var calls, responseStatus atomic.Int32
				responseStatus.Store(int32(status))
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.Path != "/"+tmdbTypeForMedia(mediaType)+"/42" {
						t.Errorf("refresh route=%q", r.URL.Path)
					}
					if code := responseStatus.Load(); code != 0 {
						w.Header().Set("Content-Type", "application/json")
						w.Header().Set("Retry-After", "120")
						w.WriteHeader(int(code))
						_ = json.NewEncoder(w).Encode(map[string]any{"status_message": "upstream error " + token})
						return
					}
					writeTMDBFilteredGalleryFixture(t, w, r, 42)
				}))
				defer server.Close()
				client := newTMDBClientForTest(server.Client(), server.URL, server.URL)
				service := newTMDBService(database, client)
				if queued, err := service.refreshLegacyDetails(now.UnixMilli()); err != nil || queued != 1 {
					t.Fatalf("queued=%d err=%v", queued, err)
				}
				if processed, err := service.runOne(t.Context(), now); err != nil || !processed {
					t.Fatalf("processed=%v err=%v", processed, err)
				}
				var overview, poster, stills, state, code string
				var version int
				var metadataUpdated, nextAttempt int64
				if err := database.db.QueryRow(`SELECT m.overview,m.poster_path,m.stills_json,m.details_version,
					m.metadata_updated_at_ms,j.state,j.last_error_code,j.next_attempt_at_ms FROM media_items m
					JOIN tmdb_jobs j ON j.media_item_id=m.id WHERE m.id=?`, id).Scan(
					&overview, &poster, &stills, &version, &metadataUpdated, &state, &code, &nextAttempt); err != nil {
					t.Fatal(err)
				}
				wantCode, wantDelay := "rate_limited", 2*time.Minute
				if status == http.StatusForbidden {
					wantCode, wantDelay = "auth", 6*time.Hour
				}
				if overview != "legacy overview" || poster != "/poster.jpg" || stills != `["/localized.jpg"]` || version != 2 ||
					metadataUpdated != now.UnixMilli() || state != "retry" || code != wantCode || nextAttempt != now.Add(wantDelay).UnixMilli() {
					t.Fatalf("retry lost usable details: overview=%q poster=%q stills=%q version=%d updated=%d state=%q code=%q next=%d",
						overview, poster, stills, version, metadataUpdated, state, code, nextAttempt)
				}
				if queued, err := newTMDBService(database, client).refreshLegacyDetails(now.UnixMilli()); err != nil || queued != 0 {
					t.Fatalf("retry bypassed backoff during refresh: queued=%d err=%v", queued, err)
				}
				if processed, err := service.runOne(t.Context(), now.Add(time.Second)); err != nil || processed || calls.Load() != 1 {
					t.Fatalf("retry bypassed backoff: processed=%v err=%v calls=%d", processed, err, calls.Load())
				}
				responseStatus.Store(0)
				if _, err := database.db.Exec("UPDATE tmdb_settings SET credential_state=? WHERE id=1", tmdbCredentialReady); err != nil {
					t.Fatal(err)
				}
				if processed, err := service.runOne(t.Context(), now.Add(wantDelay+time.Second)); err != nil || !processed {
					t.Fatalf("recovery processed=%v err=%v", processed, err)
				}
				if err := database.db.QueryRow("SELECT details_version,stills_json FROM media_items WHERE id=?", id).Scan(&version, &stills); err != nil {
					t.Fatal(err)
				}
				if version != tmdbDetailsVersion || len(decodeWatchHistoryStills(stills)) != 8 || calls.Load() != 2 {
					t.Fatalf("recovered gallery version=%d stills=%q calls=%d", version, stills, calls.Load())
				}
			})
		}
	}
}

func TestTMDBGalleryFailedSearchEnrichmentCannotPoisonSharedCache(t *testing.T) {
	withStableTMDBTestSecret(t)
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(fmt.Sprintf("detail-%d", status), func(t *testing.T) {
			database := openWatchHistoryTestDB(t)
			site := createWatchHistoryTestSite(t, database, true)
			now := time.Now()
			legacyID := seedLegacyTMDBGallery(t, database, site.ID, "shared-legacy", "movie", 42, 2, now.UnixMilli())
			seedTMDBGalleryCache(t, database, "movie", 42, 2, now.UnixMilli())
			event := watchHistoryTestEvent(site.ID, "shared-fresh", now.UnixMilli(), 100)
			event.UpstreamItemID = "shared-fresh"
			event.ProductionYear = 2024
			freshID := writeTMDBRegressionEvent(t, database, event)
			token := configureTMDBGalleryWorker(t, database)
			var searches, details, responseStatus atomic.Int32
			responseStatus.Store(int32(status))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+token || strings.Contains(r.URL.String(), token) {
					t.Error("TMDB credential escaped the bearer header")
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/search/movie":
					searches.Add(1)
					_, _ = w.Write([]byte(`{"results":[{"id":42,"title":"Example Movie","release_date":"2024-01-02","poster_path":"/poster.jpg"}]}`))
				case "/movie/42":
					details.Add(1)
					if code := responseStatus.Load(); code != 0 {
						if code == http.StatusTooManyRequests {
							w.Header().Set("Retry-After", "120")
						}
						w.WriteHeader(int(code))
						_ = json.NewEncoder(w).Encode(map[string]string{"status_message": "upstream error " + token})
						return
					}
					writeTMDBFilteredGalleryFixture(t, w, r, 42)
				default:
					t.Errorf("unexpected TMDB endpoint=%q", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			service := newTMDBService(database, newTMDBClientForTest(server.Client(), server.URL, server.URL))
			if processed, err := service.runOne(t.Context(), now); err != nil || !processed {
				t.Fatalf("new search processed=%v err=%v", processed, err)
			}
			if queued, err := service.refreshLegacyDetails(now.UnixMilli()); err != nil || queued != 1 {
				t.Fatalf("legacy refresh queued=%d err=%v", queued, err)
			}
			if _, err := service.runOne(t.Context(), now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			var legacyStills, cacheStills, state, code string
			var legacyVersion, cacheVersion int
			var nextAttempt int64
			if err := database.db.QueryRow("SELECT details_version,stills_json FROM media_items WHERE id=?", legacyID).Scan(&legacyVersion, &legacyStills); err != nil {
				t.Fatal(err)
			}
			if legacyVersion != 2 || legacyStills != `["/localized.jpg"]` {
				t.Fatalf("failed new enrichment erased the old gallery: version=%d stills=%q", legacyVersion, legacyStills)
			}
			if err := database.db.QueryRow("SELECT details_version,stills_json FROM tmdb_cache WHERE tmdb_type='movie' AND tmdb_id=42").Scan(&cacheVersion, &cacheStills); err != nil {
				t.Fatal(err)
			}
			if cacheVersion != 2 || cacheStills != `["/localized.jpg"]` {
				t.Fatalf("failed detail overwrote shared cache: version=%d stills=%q", cacheVersion, cacheStills)
			}
			if err := database.db.QueryRow("SELECT state,last_error_code,next_attempt_at_ms FROM tmdb_jobs WHERE media_item_id=?", freshID).Scan(&state, &code, &nextAttempt); err != nil {
				t.Fatal(err)
			}
			wantCode, wantDelay := "server", time.Minute
			if status == http.StatusForbidden {
				wantCode, wantDelay = "auth", 6*time.Hour
			} else if status == http.StatusTooManyRequests {
				wantCode, wantDelay = "rate_limited", 2*time.Minute
			}
			if state != "retry" || code != wantCode || nextAttempt != now.Add(wantDelay).UnixMilli() {
				t.Fatalf("failed enrichment retry state=%q code=%q next=%d", state, code, nextAttempt)
			}
			before := searches.Load() + details.Load()
			if processed, err := service.runOne(t.Context(), now.Add(2*time.Second)); err != nil || processed || searches.Load()+details.Load() != before {
				t.Fatalf("failed enrichment bypassed retry backoff: processed=%v err=%v", processed, err)
			}
			responseStatus.Store(0)
			if _, err := database.db.Exec("UPDATE tmdb_settings SET credential_state=? WHERE id=1", tmdbCredentialReady); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if processed, err := service.runOne(t.Context(), now.Add(wantDelay+2*time.Second)); err != nil || !processed {
					t.Fatalf("recovery processed=%v err=%v", processed, err)
				}
			}
			for _, id := range []int64{legacyID, freshID} {
				var version int
				var stills string
				if err := database.db.QueryRow("SELECT details_version,stills_json FROM media_items WHERE id=?", id).Scan(&version, &stills); err != nil {
					t.Fatal(err)
				}
				if version != tmdbDetailsVersion || len(decodeWatchHistoryStills(stills)) != 8 {
					t.Fatalf("recovery gallery version=%d stills=%q", version, stills)
				}
			}
			if searches.Load() != 2 || details.Load() != 3 {
				t.Fatalf("bounded retry requests search=%d detail=%d, want 2 and 3", searches.Load(), details.Load())
			}
		})
	}
}

func TestTMDBGallerySuccessfulEmptyDetailsCompleteOnce(t *testing.T) {
	withStableTMDBTestSecret(t)
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	now := time.Now()
	id := seedLegacyTMDBGallery(t, database, site.ID, "empty-gallery", "movie", 42, 2, now.UnixMilli())
	seedTMDBGalleryCache(t, database, "movie", 42, 2, now.UnixMilli())
	configureTMDBGalleryWorker(t, database)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":42,"title":"Example Movie","images":{"backdrops":[]},"credits":{"cast":[]}}`))
	}))
	defer server.Close()
	service := newTMDBService(database, newTMDBClientForTest(server.Client(), server.URL, server.URL))
	if queued, err := service.refreshLegacyDetails(now.UnixMilli()); err != nil || queued != 1 {
		t.Fatalf("queued=%d err=%v", queued, err)
	}
	if processed, err := service.runOne(t.Context(), now); err != nil || !processed {
		t.Fatalf("processed=%v err=%v", processed, err)
	}
	var version, cacheVersion int
	var stills, state string
	if err := database.db.QueryRow(`SELECT m.details_version,m.stills_json,j.state FROM media_items m
		JOIN tmdb_jobs j ON j.media_item_id=m.id WHERE m.id=?`, id).Scan(&version, &stills, &state); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRow("SELECT details_version FROM tmdb_cache WHERE tmdb_type='movie' AND tmdb_id=42").Scan(&cacheVersion); err != nil {
		t.Fatal(err)
	}
	if version != tmdbDetailsVersion || cacheVersion != tmdbDetailsVersion || state != "done" || stills != "[]" {
		t.Fatalf("successful empty detail version=%d cache=%d state=%q stills=%q", version, cacheVersion, state, stills)
	}
	if queued, err := newTMDBService(database, nil).refreshLegacyDetails(now.UnixMilli()); err != nil || queued != 0 {
		t.Fatalf("successful empty gallery requeued: queued=%d err=%v", queued, err)
	}
	if processed, err := service.runOne(t.Context(), now.Add(time.Hour)); err != nil || processed || calls.Load() != 1 {
		t.Fatalf("successful empty gallery retried: processed=%v calls=%d err=%v", processed, calls.Load(), err)
	}
}
