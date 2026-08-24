package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeTMDBRegressionEvent(t *testing.T, database *DB, event watchHistoryEvent) int64 {
	t.Helper()
	skipped, err := database.writeWatchHistoryBatch([]watchHistoryEvent{event})
	if err != nil {
		t.Fatalf("write watch history event: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("watch history event skipped=%d, want 0", skipped)
	}
	var mediaItemID int64
	if err := database.db.QueryRow(
		"SELECT id FROM media_items WHERE site_id=? AND upstream_item_id=?",
		event.SiteID, event.UpstreamItemID,
	).Scan(&mediaItemID); err != nil {
		t.Fatalf("read media item: %v", err)
	}
	return mediaItemID
}

func TestTMDBRegressionStaleRunningRevisionCannotCommit(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	observedAtMS := time.Now().Add(-time.Minute).UnixMilli()

	event := watchHistoryTestEvent(site.ID, "stale-revision", observedAtMS, 100)
	event.MediaType = "episode"
	event.Title = "本地单集标题"
	event.OriginalTitle = "Local Episode Title"
	event.SeriesName = ""
	event.SeasonNumber = 1
	event.EpisodeNumber = 2
	event.TMDBType = ""
	mediaItemID := writeTMDBRegressionEvent(t, database, event)

	claimed, found, err := database.claimTMDBJob(observedAtMS + 1)
	if err != nil {
		t.Fatalf("claim TMDB job: %v", err)
	}
	if !found || claimed.ID != mediaItemID {
		t.Fatalf("claimed job = %+v found=%v, want media item %d", claimed, found, mediaItemID)
	}

	improved := event
	improved.ObservedAtMS++
	improved.PositionTicks++
	improved.SeriesName = "补齐后的剧集名"
	writeTMDBRegressionEvent(t, database, improved)

	stale := tmdbMetadata{
		TMDBType:      "tv",
		TMDBID:        77,
		Title:         "旧任务剧集名",
		OriginalTitle: "Stale Series",
		Overview:      "旧任务简介",
		PosterPath:    "/stale.jpg",
		MatchMethod:   "title",
	}
	if err := database.completeTMDBJob(mediaItemID, claimed.JobRevision, stale, "zh-CN", improved.ObservedAtMS+1); err != nil {
		t.Fatalf("complete stale TMDB job: %v", err)
	}

	var title, seriesName, overview, posterPath, jobState string
	var revision int64
	if err := database.db.QueryRow(`SELECT m.title, m.series_name, m.overview, m.poster_path, j.state, j.revision
		FROM media_items m JOIN tmdb_jobs j ON j.media_item_id=m.id WHERE m.id=?`, mediaItemID).Scan(
		&title, &seriesName, &overview, &posterPath, &jobState, &revision,
	); err != nil {
		t.Fatalf("read stale completion result: %v", err)
	}
	if title != event.Title || seriesName != improved.SeriesName {
		t.Fatalf("media metadata after stale completion = title %q series %q", title, seriesName)
	}
	if overview != "" || posterPath != "" {
		t.Fatalf("stale metadata committed overview=%q poster=%q", overview, posterPath)
	}
	if jobState != "pending" || revision != claimed.JobRevision+1 {
		t.Fatalf("job after stale completion = state %q revision %d, want pending revision %d", jobState, revision, claimed.JobRevision+1)
	}
	var staleCacheRows int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM tmdb_cache WHERE tmdb_type='tv' AND tmdb_id=77").Scan(&staleCacheRows); err != nil {
		t.Fatal(err)
	}
	if staleCacheRows != 0 {
		t.Fatalf("stale completion inserted %d cache rows", staleCacheRows)
	}
}

func TestTMDBRegressionExpiredDerivedMetadataIsClearedAndRequeued(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	now := time.Now()
	event := watchHistoryTestEvent(site.ID, "expired-metadata", now.UnixMilli(), 100)
	mediaItemID := writeTMDBRegressionEvent(t, database, event)
	expiredAtMS := now.Add(-tmdbCacheLifetime - time.Hour).UnixMilli()

	if _, err := database.db.Exec(`UPDATE media_items SET tmdb_type='movie', tmdb_id=101,
		overview='expired overview', poster_path='/expired.jpg', match_status='title', metadata_updated_at_ms=? WHERE id=?`,
		expiredAtMS, mediaItemID); err != nil {
		t.Fatalf("seed expired media metadata: %v", err)
	}
	if _, err := database.db.Exec(`UPDATE tmdb_jobs SET state='done', attempts=3, next_attempt_at_ms=123,
		lease_until_ms=456, last_error_code='old', revision=4 WHERE media_item_id=?`, mediaItemID); err != nil {
		t.Fatalf("seed completed TMDB job: %v", err)
	}
	if _, err := database.db.Exec(`INSERT INTO tmdb_cache
		(tmdb_type, tmdb_id, language, title, original_title, overview, release_year, poster_path, updated_at_ms, expires_at_ms)
		VALUES ('movie',101,'zh-CN','Expired','','expired overview',2026,'/expired.jpg',?,?)`, expiredAtMS, now.Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatalf("seed expired TMDB cache: %v", err)
	}
	if _, err := database.db.Exec(`INSERT INTO tmdb_cache
		(tmdb_type, tmdb_id, language, title, original_title, overview, release_year, poster_path, updated_at_ms, expires_at_ms)
		VALUES ('movie',999,'zh-CN','Orphan','','orphan overview',2026,'/orphan.jpg',?,?)`, now.UnixMilli(), now.Add(time.Hour).UnixMilli()); err != nil {
		t.Fatalf("seed orphan TMDB cache: %v", err)
	}

	if err := database.pruneWatchHistory(); err != nil {
		t.Fatalf("prune watch history: %v", err)
	}

	var overview, posterPath, matchStatus, state, lastError string
	var metadataUpdatedAtMS, revision, attempts, nextAttemptAtMS, leaseUntilMS int64
	if err := database.db.QueryRow(`SELECT m.overview, m.poster_path, m.match_status, m.metadata_updated_at_ms,
		j.state, j.revision, j.attempts, j.next_attempt_at_ms, j.lease_until_ms, j.last_error_code
		FROM media_items m JOIN tmdb_jobs j ON j.media_item_id=m.id WHERE m.id=?`, mediaItemID).Scan(
		&overview, &posterPath, &matchStatus, &metadataUpdatedAtMS,
		&state, &revision, &attempts, &nextAttemptAtMS, &leaseUntilMS, &lastError,
	); err != nil {
		t.Fatalf("read pruned metadata: %v", err)
	}
	if overview != "" || posterPath != "" || matchStatus != "pending" || metadataUpdatedAtMS != 0 {
		t.Fatalf("expired derived metadata remained: overview=%q poster=%q status=%q updated=%d", overview, posterPath, matchStatus, metadataUpdatedAtMS)
	}
	if state != "pending" || revision != 5 || attempts != 0 || nextAttemptAtMS != 0 || leaseUntilMS != 0 || lastError != "" {
		t.Fatalf("requeued job = state %q revision %d attempts %d next %d lease %d error %q", state, revision, attempts, nextAttemptAtMS, leaseUntilMS, lastError)
	}
	for _, tmdbID := range []int64{101, 999} {
		var count int
		if err := database.db.QueryRow("SELECT COUNT(*) FROM tmdb_cache WHERE tmdb_type='movie' AND tmdb_id=?", tmdbID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("TMDB cache %d remained with %d rows", tmdbID, count)
		}
	}
}

func TestTMDBRegressionManualCacheClearInvalidatesCopiedHistoryMetadata(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	defer database.Close()
	site := createWatchHistoryTestSite(t, database, true)
	nowMS := time.Now().UnixMilli()
	mediaItemID := writeTMDBRegressionEvent(t, database, watchHistoryTestEvent(site.ID, "manual-clear", nowMS, 100))
	if _, err := database.db.Exec(`UPDATE media_items SET tmdb_type='movie', tmdb_id=505, title='本地播放标题',
		overview='cached overview', poster_path='/cached.jpg', backdrop_path='/backdrop.jpg', stills_json='["/still.jpg"]',
		cast_json='[{"name":"Actor","profile_path":"/actor.jpg"}]', match_status='title', details_version=?, metadata_updated_at_ms=? WHERE id=?`,
		tmdbDetailsVersion, nowMS, mediaItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`UPDATE tmdb_jobs SET state='done', attempts=2, last_error_code='', revision=4 WHERE media_item_id=?`, mediaItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO tmdb_cache
		(tmdb_type, tmdb_id, language, title, poster_path, updated_at_ms, expires_at_ms, details_version)
		VALUES ('movie',505,'zh-CN','Cached','/cached.jpg',?,?,?)`, nowMS, nowMS+time.Hour.Milliseconds(), tmdbDetailsVersion); err != nil {
		t.Fatal(err)
	}
	if err := database.clearTMDBCache("all"); err != nil {
		t.Fatalf("clear TMDB cache: %v", err)
	}
	var title, overview, posterPath, backdropPath, stillsJSON, castJSON, matchStatus, state string
	var detailsVersion, metadataUpdated, revision int64
	if err := database.db.QueryRow(`SELECT m.title, m.overview, m.poster_path, m.backdrop_path, m.stills_json, m.cast_json,
		m.match_status, m.details_version, m.metadata_updated_at_ms, j.state, j.revision
		FROM media_items m JOIN tmdb_jobs j ON j.media_item_id=m.id WHERE m.id=?`, mediaItemID).Scan(
		&title, &overview, &posterPath, &backdropPath, &stillsJSON, &castJSON, &matchStatus, &detailsVersion,
		&metadataUpdated, &state, &revision); err != nil {
		t.Fatal(err)
	}
	if title != "本地播放标题" || overview != "" || posterPath != "" || backdropPath != "" || stillsJSON != "[]" || castJSON != "[]" ||
		matchStatus != "pending" || detailsVersion != 0 || metadataUpdated != 0 || state != "pending" || revision != 5 {
		t.Fatalf("cleared metadata = title=%q overview=%q poster=%q backdrop=%q stills=%q cast=%q status=%q version=%d updated=%d state=%q revision=%d",
			title, overview, posterPath, backdropPath, stillsJSON, castJSON, matchStatus, detailsVersion, metadataUpdated, state, revision)
	}
	var cacheRows int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM tmdb_cache").Scan(&cacheRows); err != nil || cacheRows != 0 {
		t.Fatalf("cache rows after clear=%d err=%v", cacheRows, err)
	}
}

func TestTMDBPublicSettingsCountsLegacyCopiedMetadataWhenCacheRowIsMissing(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	defer database.Close()
	site := createWatchHistoryTestSite(t, database, true)
	nowMS := time.Now().UnixMilli()
	mediaItemID := writeTMDBRegressionEvent(t, database, watchHistoryTestEvent(site.ID, "legacy-cache", nowMS, 100))
	if _, err := database.db.Exec(`UPDATE media_items SET tmdb_type='movie', tmdb_id=606,
		overview='legacy overview', poster_path='/legacy.jpg', match_status='title',
		metadata_updated_at_ms=? WHERE id=?`, nowMS, mediaItemID); err != nil {
		t.Fatalf("seed copied legacy metadata: %v", err)
	}
	settings, err := database.tmdbPublicSettings()
	if err != nil {
		t.Fatalf("read TMDB public settings: %v", err)
	}
	if settings.CacheEntries != 1 || settings.CacheSizeBytes <= 0 {
		t.Fatalf("legacy cache stats = %d entries, %d bytes; want one non-empty entry", settings.CacheEntries, settings.CacheSizeBytes)
	}
}

func TestTMDBPublicSettingsCountsCopiedMetadataWithoutTimestamp(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	defer database.Close()
	site := createWatchHistoryTestSite(t, database, true)
	mediaItemID := writeTMDBRegressionEvent(t, database, watchHistoryTestEvent(site.ID, "legacy-cache-no-timestamp", time.Now().UnixMilli(), 100))
	if _, err := database.db.Exec(`UPDATE media_items SET tmdb_type='movie', tmdb_id=0,
		overview='copied without marker', poster_path='/legacy-no-marker.jpg', metadata_updated_at_ms=0 WHERE id=?`, mediaItemID); err != nil {
		t.Fatalf("seed copied metadata without marker: %v", err)
	}
	settings, err := database.tmdbPublicSettings()
	if err != nil {
		t.Fatalf("read TMDB public settings: %v", err)
	}
	if settings.CacheEntries != 1 || settings.CacheSizeBytes <= 0 {
		t.Fatalf("copied cache stats without marker = %d entries, %d bytes; want one non-empty entry", settings.CacheEntries, settings.CacheSizeBytes)
	}
}

func TestTMDBRegressionEpisodeKeepsLocalTitle(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	nowMS := time.Now().UnixMilli()
	event := watchHistoryTestEvent(site.ID, "episode-title", nowMS, 100)
	event.MediaType = "episode"
	event.Title = "本地单集标题"
	event.OriginalTitle = "Local Episode Title"
	event.SeriesName = "本地剧集名"
	event.SeasonNumber = 3
	event.EpisodeNumber = 4
	event.TMDBType = ""
	mediaItemID := writeTMDBRegressionEvent(t, database, event)

	claimed, found, err := database.claimTMDBJob(nowMS + 1)
	if err != nil || !found {
		t.Fatalf("claim TMDB job: found=%v err=%v", found, err)
	}
	metadata := tmdbMetadata{
		TMDBType:      "tv",
		TMDBID:        202,
		Title:         "TMDB 剧集标题",
		OriginalTitle: "TMDB Series Title",
		Overview:      "series overview",
		PosterPath:    "/series.jpg",
		MatchMethod:   "title",
	}
	if err := database.completeTMDBJob(mediaItemID, claimed.JobRevision, metadata, "zh-CN", nowMS+2); err != nil {
		t.Fatalf("complete episode TMDB job: %v", err)
	}

	var title, originalTitle, seriesName, overview, posterPath, state string
	var tmdbID int64
	if err := database.db.QueryRow(`SELECT m.title, m.original_title, m.series_name, m.overview, m.poster_path,
		m.tmdb_id, j.state FROM media_items m JOIN tmdb_jobs j ON j.media_item_id=m.id WHERE m.id=?`, mediaItemID).Scan(
		&title, &originalTitle, &seriesName, &overview, &posterPath, &tmdbID, &state,
	); err != nil {
		t.Fatalf("read completed episode metadata: %v", err)
	}
	if title != event.Title || originalTitle != event.OriginalTitle || seriesName != event.SeriesName {
		t.Fatalf("episode identity overwritten: title=%q original=%q series=%q", title, originalTitle, seriesName)
	}
	if overview != metadata.Overview || posterPath != metadata.PosterPath || tmdbID != metadata.TMDBID || state != "done" {
		t.Fatalf("episode enrichment not committed: overview=%q poster=%q tmdb=%d state=%q", overview, posterPath, tmdbID, state)
	}
}

func TestTMDBRegressionClientErrorJobDoesNotRetry(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	now := time.Now()
	event := watchHistoryTestEvent(site.ID, "client-error", now.UnixMilli(), 100)
	mediaItemID := writeTMDBRegressionEvent(t, database, event)

	tokenCiphertext, err := encryptTMDBReadToken(strings.Repeat("tmdb-read-token-", 3))
	if err != nil {
		t.Fatalf("encrypt TMDB token: %v", err)
	}
	if _, err := database.db.Exec(`UPDATE tmdb_settings SET enabled=1, token_ciphertext=?, credential_state=? WHERE id=1`,
		tokenCiphertext, tmdbCredentialReady); err != nil {
		t.Fatalf("configure TMDB settings: %v", err)
	}

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer server.Close()
	service := newTMDBService(database, newTMDBClientForTest(server.Client(), server.URL, server.URL))

	service.runOne(t.Context(), now.Add(time.Millisecond))
	var state, lastError, matchStatus string
	var attempts, nextAttemptAtMS, leaseUntilMS int64
	if err := database.db.QueryRow(`SELECT j.state, j.attempts, j.next_attempt_at_ms, j.lease_until_ms,
		j.last_error_code, m.match_status FROM tmdb_jobs j JOIN media_items m ON m.id=j.media_item_id
		WHERE j.media_item_id=?`, mediaItemID).Scan(&state, &attempts, &nextAttemptAtMS, &leaseUntilMS, &lastError, &matchStatus); err != nil {
		t.Fatalf("read client-error job: %v", err)
	}
	if state != "done" || attempts != 1 || nextAttemptAtMS != 0 || leaseUntilMS != 0 || lastError != "client_error" || matchStatus != "client_error" {
		t.Fatalf("client-error job = state %q attempts %d next %d lease %d error %q match %q", state, attempts, nextAttemptAtMS, leaseUntilMS, lastError, matchStatus)
	}
	service.runOne(t.Context(), now.Add(24*time.Hour))
	if got := requests.Load(); got != 1 {
		t.Fatalf("permanent client error was retried: requests=%d", got)
	}
}
