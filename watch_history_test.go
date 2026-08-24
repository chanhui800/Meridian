package main

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWatchHistoryMigrationAddsTMDBJobRevisionIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-watch-history.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE tmdb_jobs (
		media_item_id INTEGER PRIMARY KEY,
		state TEXT NOT NULL DEFAULT 'pending',
		attempts INTEGER NOT NULL DEFAULT 0,
		next_attempt_at_ms INTEGER NOT NULL DEFAULT 0,
		lease_until_ms INTEGER NOT NULL DEFAULT 0,
		last_error_code TEXT NOT NULL DEFAULT '',
		updated_at_ms INTEGER NOT NULL DEFAULT 0
	)`); err != nil {
		raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.migrate(); err != nil {
		t.Fatalf("repeat migrate: %v", err)
	}
	var revisionColumn int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('tmdb_jobs') WHERE name='revision'").Scan(&revisionColumn); err != nil {
		t.Fatal(err)
	}
	if revisionColumn != 1 {
		t.Fatalf("revision column count=%d, want 1", revisionColumn)
	}
}

func openWatchHistoryTestDB(t *testing.T) *DB {
	t.Helper()
	database, err := openDB(filepath.Join(t.TempDir(), "watch-history.db"))
	if err != nil {
		t.Fatalf("open watch history database: %v", err)
	}
	t.Cleanup(database.Close)
	return database
}

func createWatchHistoryTestSite(t *testing.T, database *DB, enabled bool) *Site {
	t.Helper()
	site, err := database.CreateSiteRecord(Site{
		Name:                "watch-history",
		ListenPort:          freePort(t),
		IngressMode:         ingressModePort,
		TargetURL:           "http://127.0.0.1:8096",
		PlaybackMode:        "direct",
		StreamHosts:         "[]",
		UAMode:              passthroughUAMode,
		WatchHistoryEnabled: enabled,
	})
	if err != nil {
		t.Fatalf("create watch history site: %v", err)
	}
	return site
}

func watchHistoryTestEvent(siteID int64, session string, observedAtMS, positionTicks int64) watchHistoryEvent {
	return watchHistoryEvent{
		SiteID:         siteID,
		SessionHash:    fmt.Sprintf("%064x", session),
		UpstreamItemID: "item-1",
		EventType:      "progress",
		ObservedAtMS:   observedAtMS,
		PositionTicks:  positionTicks,
		RunTimeTicks:   1_000,
		PlayMethod:     "DirectPlay",
		MediaType:      "movie",
		Title:          "Example Movie",
		ProductionYear: 2026,
		SeasonNumber:   -1,
		EpisodeNumber:  -1,
		TMDBType:       "movie",
	}
}

func TestWatchHistoryMigrationAndSiteDefault(t *testing.T) {
	database := openWatchHistoryTestDB(t)

	for _, table := range []string{"media_items", "watch_sessions", "tmdb_jobs", "tmdb_cache", "tmdb_settings"} {
		var count int
		if err := database.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&count); err != nil {
			t.Fatalf("inspect table %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("table %s was not migrated", table)
		}
	}
	var revisionColumn int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('tmdb_jobs') WHERE name='revision'").Scan(&revisionColumn); err != nil {
		t.Fatalf("inspect tmdb_jobs revision: %v", err)
	}
	if revisionColumn != 1 {
		t.Fatal("tmdb_jobs revision was not migrated")
	}

	var enabled int
	var language, credentialState string
	var retentionDays, lastTestedAtMS int
	if err := database.db.QueryRow(`SELECT enabled, language, history_retention_days, credential_state, last_tested_at_ms
		FROM tmdb_settings WHERE id=1`).Scan(&enabled, &language, &retentionDays, &credentialState, &lastTestedAtMS); err != nil {
		t.Fatalf("read default TMDB settings: %v", err)
	}
	if enabled != 0 || language != "zh-CN" || retentionDays != 90 || credentialState != "unconfigured" || lastTestedAtMS != 0 {
		t.Fatalf("unexpected TMDB defaults: enabled=%d language=%q retention=%d state=%q tested=%d", enabled, language, retentionDays, credentialState, lastTestedAtMS)
	}

	site := createWatchHistoryTestSite(t, database, false)
	if site.WatchHistoryEnabled {
		t.Fatal("watch history must default to disabled")
	}
}

func TestWatchHistoryLegacyProgressUsesOnlyWhitelistedQueryValues(t *testing.T) {
	site := Site{ID: 77, WatchHistoryEnabled: true}
	request := httptest.NewRequest(http.MethodPost,
		"http://media.example/emby/Users/raw-user-id/PlayingItems/item-legacy/Progress?PositionTicks=420000000&RunTimeTicks=1000000000&PlaySessionId=raw-play-session&PlayMethod=DirectPlay&api_key=raw-token",
		http.NoBody)
	event, ok := watchHistoryEventFromCapture(nil, nil, site, request, nil, http.StatusNoContent, time.UnixMilli(1_700_000_000_000))
	if !ok {
		t.Fatal("legacy progress route was not recorded")
	}
	if event.UpstreamItemID != "item-legacy" || event.PositionTicks != 420000000 || event.RunTimeTicks != 1000000000 || event.PlayMethod != "DirectPlay" {
		t.Fatalf("legacy event = %+v", event)
	}
	serialized := fmt.Sprintf("%+v", event)
	for _, secret := range []string{"raw-user-id", "raw-play-session", "raw-token", "api_key"} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("legacy event retained private value %q: %s", secret, serialized)
		}
	}

	malformed := httptest.NewRequest(http.MethodPost,
		"http://media.example/Users/user/PlayingItems/item/Progress?PositionTicks=not-a-number", http.NoBody)
	if _, ok := watchHistoryEventFromCapture(nil, nil, site, malformed, nil, http.StatusNoContent, time.Now()); ok {
		t.Fatal("malformed legacy progress query was recorded")
	}
	if _, ok := watchHistoryEventFromCapture(nil, nil, site, request, nil, http.StatusBadGateway, time.Now()); ok {
		t.Fatal("failed legacy progress response was recorded")
	}
}

func TestWatchHistoryMetadataResponseEnrichesLegacyProgress(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	metadataRequest := httptest.NewRequest(http.MethodGet, "http://media.example/emby/Users/user/Items/item-legacy", http.NoBody)
	metadataBody := `{"Id":"item-legacy","Name":"Example Movie","OriginalTitle":"Example Original","Type":"Movie","ProductionYear":2026,"ProviderIds":{"Tmdb":"123"}}`
	metadataResponse := &http.Response{
		StatusCode:    http.StatusOK,
		Request:       metadataRequest,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(metadataBody)),
		ContentLength: int64(len(metadataBody)),
	}
	metadataResponse.Header.Set("Content-Type", "application/json")
	if err := captureWatchHistoryMetadata(metadataResponse, database, 77); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(metadataResponse.Body); err != nil {
		t.Fatal(err)
	}
	legacy := httptest.NewRequest(http.MethodPost,
		"http://media.example/emby/Users/user/PlayingItems/item-legacy/Progress?PositionTicks=420&RunTimeTicks=1000",
		http.NoBody)
	event, ok := watchHistoryEventFromCapture(nil, database, Site{ID: 77, WatchHistoryEnabled: true}, legacy, nil, http.StatusNoContent, time.UnixMilli(1_700_000_000_000))
	if !ok {
		t.Fatal("legacy progress with cached metadata was not recorded")
	}
	if event.MediaType != "movie" || event.Title != "Example Movie" || event.ProductionYear != 2026 {
		t.Fatalf("metadata was not merged into legacy event: %+v", event)
	}
}

func TestWatchHistoryMetadataAcceptsGzipResponsesWithoutChangingBody(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	plain := []byte(`{"Id":"gzip-item","Name":"压缩影片","Type":"Movie","ProductionYear":2026}`)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://media.example/emby/Users/user/Items/gzip-item", http.NoBody)
	response := &http.Response{
		StatusCode:    http.StatusOK,
		Request:       request,
		Header:        make(http.Header),
		Body:          io.NopCloser(bytes.NewReader(compressed.Bytes())),
		ContentLength: int64(len(compressed.Bytes())),
	}
	response.Header.Set("Content-Type", "application/json")
	response.Header.Set("Content-Encoding", "gzip")
	if err := captureWatchHistoryMetadata(response, database, 81); err != nil {
		t.Fatal(err)
	}
	forwarded, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(forwarded, compressed.Bytes()) {
		t.Fatal("gzip metadata observer changed the forwarded response body")
	}
	if item, ok := database.watchHistoryMetadataFor(81, "gzip-item"); !ok || item.Name != "压缩影片" {
		t.Fatalf("gzip metadata was not cached: ok=%v item=%+v", ok, item)
	}
}

func TestWatchHistoryStandardProgressUsesCachedMetadata(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	metadataRequest := httptest.NewRequest(http.MethodGet, "http://media.example/emby/Users/user/Items/item-standard", http.NoBody)
	metadataBody := `{"Id":"item-standard","Name":"Cached Movie","OriginalTitle":"Cached Original","Type":"Movie","ProductionYear":2025,"RunTimeTicks":9000,"ProviderIds":{"Tmdb":"456","Imdb":"tt00456"}}`
	metadataResponse := &http.Response{
		StatusCode:    http.StatusOK,
		Request:       metadataRequest,
		Header:        make(http.Header),
		Body:          io.NopCloser(strings.NewReader(metadataBody)),
		ContentLength: int64(len(metadataBody)),
	}
	metadataResponse.Header.Set("Content-Type", "application/json")
	if err := captureWatchHistoryMetadata(metadataResponse, database, 77); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(metadataResponse.Body); err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"ItemId":"item-standard","PlaySessionId":"session-standard","PositionTicks":420}`)
	request := httptest.NewRequest(http.MethodPost, "http://media.example/emby/Sessions/Playing/Progress", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	capture := startWatchHistoryCapture(Site{ID: 77, WatchHistoryEnabled: true}, request, requestLogCategoryPlaybackSync, database)
	if capture == nil {
		t.Fatal("standard playback sync was not captured")
	}
	if _, err := io.ReadAll(request.Body); err != nil {
		t.Fatal(err)
	}
	event, ok := watchHistoryEventFromCapture(capture, database, Site{ID: 77, WatchHistoryEnabled: true}, request, nil, http.StatusNoContent, time.UnixMilli(1_700_000_000_000))
	if !ok {
		t.Fatal("minimal standard playback sync was not recorded")
	}
	if event.Title != "Cached Movie" || event.MediaType != "movie" || event.ProductionYear != 2025 ||
		event.RunTimeTicks != 9000 || event.TMDBType != "movie" || event.TMDBID != 456 || event.IMDBID != "tt00456" {
		t.Fatalf("cached standard metadata was not applied: %+v", event)
	}
}

func TestWatchHistoryStandardPayloadUsesBodyAndMergesNowPlayingItem(t *testing.T) {
	body := []byte(`{"ItemId":"item-body","PlaySessionId":"session-body","PositionTicks":10,"Item":{"Id":"item-body"},"NowPlayingItem":{"Id":"item-body","Name":"Body Movie","Type":"Movie","ProductionYear":2026}}`)
	request := httptest.NewRequest(http.MethodPost, "http://media.example/emby/Sessions/Playing", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	capture := startWatchHistoryCapture(Site{ID: 78, WatchHistoryEnabled: true}, request, requestLogCategoryPlaybackSync, nil)
	if capture == nil {
		t.Fatal("standard start sync was not captured")
	}
	_, _ = io.ReadAll(request.Body)
	event, ok := watchHistoryEventFromCapture(capture, nil, Site{ID: 78, WatchHistoryEnabled: true}, request, nil, http.StatusNoContent, time.UnixMilli(1_700_000_000_000))
	if !ok || event.Title != "Body Movie" || event.MediaType != "movie" || event.ProductionYear != 2026 {
		t.Fatalf("Item/NowPlayingItem fields were not merged: ok=%v event=%+v", ok, event)
	}
}

func TestWatchHistoryAcceptsGzipAndNonPostPlaybackSyncWithoutChangingBody(t *testing.T) {
	plain := []byte(`{"ItemId":"gzip-item","PlaySessionId":"gzip-session","PositionTicks":100}`)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPatch, "http://media.example/emby/Sessions/Playing/Progress", bytes.NewReader(compressed.Bytes()))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "gzip")
	capture := startWatchHistoryCapture(Site{ID: 80, WatchHistoryEnabled: true}, request, requestLogCategoryPlaybackSync, nil)
	if capture == nil {
		t.Fatal("gzip PATCH playback sync was not captured")
	}
	forwarded, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(forwarded, compressed.Bytes()) {
		t.Fatal("gzip observer changed the forwarded request body")
	}
	event, ok := watchHistoryEventFromCapture(capture, nil, Site{ID: 80, WatchHistoryEnabled: true}, request, nil, http.StatusNoContent, time.UnixMilli(1_700_000_000_000))
	if !ok || event.UpstreamItemID != "gzip-item" {
		t.Fatalf("gzip PATCH event = ok:%v event:%+v", ok, event)
	}
}

func TestWatchHistoryAcceptsFormEncodedPlaybackSync(t *testing.T) {
	body := []byte("ItemId=form-item&PlaySessionId=form-session&PositionTicks=120&RunTimeTicks=240&PlayMethod=DirectPlay")
	request := httptest.NewRequest(http.MethodPost, "http://media.example/emby/Sessions/Playing/Progress", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	capture := startWatchHistoryCapture(Site{ID: 81, WatchHistoryEnabled: true}, request, requestLogCategoryPlaybackSync, nil)
	if capture == nil {
		t.Fatal("form playback sync was not captured")
	}
	if _, err := io.ReadAll(request.Body); err != nil {
		t.Fatal(err)
	}
	event, ok := watchHistoryEventFromCapture(capture, nil, Site{ID: 81, WatchHistoryEnabled: true}, request, nil, http.StatusNoContent, time.UnixMilli(1_700_000_000_000))
	if !ok || event.UpstreamItemID != "form-item" || event.SessionHash == "" || event.PositionTicks != 120 || event.RunTimeTicks != 240 || event.PlayMethod != "DirectPlay" {
		t.Fatalf("form playback event = ok:%v event:%+v", ok, event)
	}
}

func TestWatchHistoryMetadataCollectionPathsCaptureItems(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	paths := []string{
		"/emby/Users/user/Items?ParentId=library",
		"/emby/Users/user/Items/Resume",
		"/emby/Shows/show/Episodes",
		"/emby/Shows/NextUp",
		"/emby/Items/Latest",
	}
	for index, path := range paths {
		itemID := fmt.Sprintf("collection-item-%d", index)
		request := httptest.NewRequest(http.MethodGet, "http://media.example"+path, http.NoBody)
		body := fmt.Sprintf(`{"Items":[{"Id":%q,"Name":"Collection Movie","Type":"Movie"}]}`, itemID)
		response := &http.Response{StatusCode: http.StatusOK, Request: request, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
		response.Header.Set("Content-Type", "application/json")
		if err := captureWatchHistoryMetadata(response, database, 79); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(response.Body); err != nil {
			t.Fatal(err)
		}
		if _, ok := database.watchHistoryMetadataFor(79, itemID); !ok {
			t.Fatalf("metadata collection path %s did not cache item", path)
		}
	}
	playbackInfo := httptest.NewRequest(http.MethodGet, "http://media.example/emby/Items/item-1/PlaybackInfo", http.NoBody)
	response := &http.Response{StatusCode: http.StatusOK, Request: playbackInfo, Header: make(http.Header), Body: http.NoBody, ContentLength: 0}
	response.Header.Set("Content-Type", "application/json")
	if err := captureWatchHistoryMetadata(response, database, 79); err != nil {
		t.Fatal(err)
	}
	if _, ok := response.Body.(*watchHistoryMetadataBodyObserver); ok {
		t.Fatal("PlaybackInfo must not be treated as item metadata")
	}
}

func TestWatchHistoryMetadataCaptureRejectsNonItemResponses(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "http://media.example/emby/Items/Counts", http.NoBody)
	response := &http.Response{StatusCode: http.StatusOK, Request: request, Header: make(http.Header), Body: http.NoBody, ContentLength: 0}
	response.Header.Set("Content-Type", "application/json")
	if err := captureWatchHistoryMetadata(response, &DB{}, 1); err != nil {
		t.Fatal(err)
	}
	if _, ok := response.Body.(*watchHistoryMetadataBodyObserver); ok {
		t.Fatal("counts endpoint must not be captured as item metadata")
	}
	collection := httptest.NewRequest(http.MethodGet, "http://media.example/emby/Users/user/Items?Ids=item-1", http.NoBody)
	collectionResponse := &http.Response{StatusCode: http.StatusOK, Request: collection, Header: make(http.Header), Body: http.NoBody, ContentLength: 0}
	collectionResponse.Header.Set("Content-Type", "application/json")
	if err := captureWatchHistoryMetadata(collectionResponse, &DB{}, 1); err != nil {
		t.Fatal(err)
	}
	if _, ok := collectionResponse.Body.(*watchHistoryMetadataBodyObserver); !ok {
		t.Fatal("an explicit Items query should be observed")
	}
}

func TestWatchHistoryMetadataUpdateRequeuesPreviousUnmatchedTMDBJob(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	initial := watchHistoryTestEvent(site.ID, "metadata-retry", 1_000, 100)
	initial.MediaType = ""
	initial.Title = ""
	initial.TMDBType = ""
	if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{initial}); err != nil {
		t.Fatal(err)
	}
	var mediaItemID int64
	if err := database.db.QueryRow("SELECT id FROM media_items WHERE site_id=?", site.ID).Scan(&mediaItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec("UPDATE tmdb_jobs SET state='done', last_error_code='unsupported_media_type' WHERE media_item_id=?", mediaItemID); err != nil {
		t.Fatal(err)
	}
	richer := initial
	richer.ObservedAtMS = 2_000
	richer.MediaType = "movie"
	richer.Title = "Recovered Movie"
	richer.TMDBType = "movie"
	if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{richer}); err != nil {
		t.Fatal(err)
	}
	var state string
	var revision int64
	if err := database.db.QueryRow("SELECT state, revision FROM tmdb_jobs WHERE media_item_id=?", mediaItemID).Scan(&state, &revision); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || revision != 1 {
		t.Fatalf("richer metadata did not requeue job: state=%q revision=%d", state, revision)
	}
}

func TestWatchHistoryCapturePassesBodyThroughAndHashesSession(t *testing.T) {
	body := []byte(`{
		"ItemId":"item-episode-7",
		"PlaySessionId":"raw-secret-session-id",
		"PositionTicks":420000000,
		"RunTimeTicks":1000000000,
		"PlayMethod":"DirectPlay",
		"NowPlayingItem":{
			"Id":"item-episode-7","Name":"Episode Seven","OriginalTitle":"Original Seven","Type":"Episode",
			"ProductionYear":2026,"SeriesName":"Example Series","ParentIndexNumber":1,"IndexNumber":7,
			"ProviderIds":{"Tmdb":"12345","Imdb":"tt1234567","Tvdb":"7654321"}
		}
	}`)
	request := httptest.NewRequest(http.MethodPost, "http://media.example/emby/Sessions/Playing/Progress", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("X-Emby-Token", "raw-secret-token")
	capture := startWatchHistoryCapture(Site{ID: 42, WatchHistoryEnabled: true}, request, requestLogCategoryPlaybackSync, nil)
	if capture == nil {
		t.Fatal("eligible playback sync was not observed")
	}
	forwarded, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(forwarded, body) {
		t.Fatal("observer changed the forwarded request body")
	}

	event, ok := watchHistoryEventFromCapture(capture, nil, Site{ID: 42, WatchHistoryEnabled: true}, request, nil, http.StatusNoContent, time.UnixMilli(1_700_000_000_000))
	if !ok {
		t.Fatal("complete successful playback sync did not create an event")
	}
	if event.UpstreamItemID != "item-episode-7" || event.MediaType != "episode" || event.Title != "Episode Seven" ||
		event.SeriesName != "Example Series" || event.SeasonNumber != 1 || event.EpisodeNumber != 7 || event.TMDBID != 0 ||
		event.IMDBID != "" || event.TVDBID != "" {
		t.Fatalf("unexpected whitelisted event: %+v", event)
	}
	if len(event.SessionHash) != 64 || strings.Contains(event.SessionHash, "raw-secret") {
		t.Fatalf("session identifier was not safely digested: %q", event.SessionHash)
	}
	if fmt.Sprintf("%+v", event) == string(body) || strings.Contains(fmt.Sprintf("%+v", event), "raw-secret-token") || strings.Contains(fmt.Sprintf("%+v", event), "raw-secret-session-id") {
		t.Fatal("event retained a raw credential, session identifier, or request body")
	}
}

func TestWatchHistorySessionDigestSeparatesItemsWithinReusedPlaySession(t *testing.T) {
	observedAt := time.UnixMilli(1_700_000_000_000)
	first := watchHistorySessionDigest(42, "shared-play-session", "viewer", "item-a", observedAt)
	second := watchHistorySessionDigest(42, "shared-play-session", "viewer", "item-b", observedAt)
	if first == second {
		t.Fatal("a reused PlaySessionId merged two different media items")
	}
	if first != watchHistorySessionDigest(42, "shared-play-session", "different-viewer", "item-a", observedAt.Add(time.Hour)) {
		t.Fatal("stable PlaySessionId and item should remain one session across progress updates")
	}
}

func TestWatchHistoryCaptureRejectsUnsafeOrIncompleteBodies(t *testing.T) {
	validBody := []byte(`{"ItemId":"item-1","PlaySessionId":"session-1"}`)
	tests := []struct {
		name     string
		site     Site
		category string
		method   string
		body     []byte
		status   int
		wantCap  bool
		want     bool
	}{
		{name: "disabled", site: Site{ID: 1}, category: requestLogCategoryPlaybackSync, method: http.MethodPost, body: validBody, status: 204},
		{name: "wrong category", site: Site{ID: 1, WatchHistoryEnabled: true}, category: requestLogCategoryAPI, method: http.MethodPost, body: validBody, status: 204},
		{name: "wrong method", site: Site{ID: 1, WatchHistoryEnabled: true}, category: requestLogCategoryPlaybackSync, method: http.MethodGet, body: validBody, status: 204},
		{name: "failed response", site: Site{ID: 1, WatchHistoryEnabled: true}, category: requestLogCategoryPlaybackSync, method: http.MethodPost, body: validBody, status: 502, wantCap: true},
		{name: "malformed json", site: Site{ID: 1, WatchHistoryEnabled: true}, category: requestLogCategoryPlaybackSync, method: http.MethodPost, body: []byte(`{"ItemId":`), status: 204, wantCap: true},
		{name: "over limit", site: Site{ID: 1, WatchHistoryEnabled: true}, category: requestLogCategoryPlaybackSync, method: http.MethodPost, body: bytes.Repeat([]byte("x"), watchHistoryBodyLimit+1), status: 204},
		{name: "eligible", site: Site{ID: 1, WatchHistoryEnabled: true}, category: requestLogCategoryPlaybackSync, method: http.MethodPost, body: validBody, status: 204, wantCap: true, want: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, "http://media.example/Sessions/Playing/Progress", bytes.NewReader(testCase.body))
			request.Header.Set("Content-Type", "application/json")
			capture := startWatchHistoryCapture(testCase.site, request, testCase.category, nil)
			if (capture != nil) != testCase.wantCap {
				t.Fatalf("capture present=%v, want %v", capture != nil, testCase.wantCap)
			}
			if capture == nil {
				return
			}
			_, _ = io.ReadAll(request.Body)
			_, got := watchHistoryEventFromCapture(capture, nil, testCase.site, request, nil, testCase.status, time.Now())
			if got != testCase.want {
				t.Fatalf("event present=%v, want %v", got, testCase.want)
			}
		})
	}

	chunked := httptest.NewRequest(http.MethodPost, "http://media.example/Sessions/Playing/Progress", nil)
	chunked.Body = io.NopCloser(bytes.NewReader(validBody))
	chunked.ContentLength = -1
	chunked.Header.Set("Content-Type", "application/json")
	capture := startWatchHistoryCapture(Site{ID: 2, WatchHistoryEnabled: true}, chunked, requestLogCategoryPlaybackSync, nil)
	if capture == nil {
		t.Fatal("bounded unknown-length JSON should be observed")
	}
	_, _ = io.ReadAll(chunked.Body)
	if _, ok := watchHistoryEventFromCapture(capture, nil, Site{ID: 2, WatchHistoryEnabled: true}, chunked, nil, 204, time.Now()); !ok {
		t.Fatal("complete bounded unknown-length JSON was rejected")
	}
}

func TestWatchHistoryBatchDoesNotRegressOutOfOrderProgress(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)

	newer := watchHistoryTestEvent(site.ID, "session-1", 2_000, 950)
	older := watchHistoryTestEvent(site.ID, "session-1", 1_000, 100)
	older.EventType = "stopped"
	if skipped, err := database.writeWatchHistoryBatch([]watchHistoryEvent{newer}); err != nil || skipped != 0 {
		t.Fatalf("write newer event: skipped=%d err=%v", skipped, err)
	}
	if skipped, err := database.writeWatchHistoryBatch([]watchHistoryEvent{older}); err != nil || skipped != 0 {
		t.Fatalf("write older event: skipped=%d err=%v", skipped, err)
	}

	entries, err := database.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("history length=%d, want 1", len(entries))
	}
	entry := entries[0]
	if entry.StartedAtMS != 1_000 || entry.LastSeenAtMS != 2_000 || entry.PositionTicks != 950 || entry.RunTimeTicks != 1_000 || !entry.Completed {
		t.Fatalf("out-of-order event regressed session: %+v", entry)
	}
}

func TestWatchHistoryBatchDoesNotRegressSameMillisecondProgress(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	high := watchHistoryTestEvent(site.ID, "session-same-ms", 2_000, 900)
	low := watchHistoryTestEvent(site.ID, "session-same-ms", 2_000, 100)
	if skipped, err := database.writeWatchHistoryBatch([]watchHistoryEvent{high, low}); err != nil || skipped != 0 {
		t.Fatalf("write same-batch events: skipped=%d err=%v", skipped, err)
	}
	if skipped, err := database.writeWatchHistoryBatch([]watchHistoryEvent{low}); err != nil || skipped != 0 {
		t.Fatalf("write later low event: skipped=%d err=%v", skipped, err)
	}
	entries, err := database.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].PositionTicks != 900 || entries[0].LastSeenAtMS != 2_000 {
		t.Fatalf("same-millisecond progress regressed: %+v", entries)
	}
}

func TestListWatchHistoryMergesEpisodesBySeriesAndPrefersLatestInProgress(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	episode := func(session, itemID, seriesName, title string, season, number int, observedAtMS, positionTicks int64) watchHistoryEvent {
		event := watchHistoryTestEvent(site.ID, session, observedAtMS, positionTicks)
		event.UpstreamItemID = itemID
		event.MediaType = "episode"
		event.Title = title
		event.SeriesName = seriesName
		event.SeasonNumber = season
		event.EpisodeNumber = number
		event.TMDBType = ""
		return event
	}
	events := []watchHistoryEvent{
		episode("series-a-completed", "series-a-15", "Example Series", "Completed Episode", 2, 15, 4_000, 950),
		episode("series-a-progress-old", "series-a-14", "Example Series", "Older In Progress", 2, 14, 1_000, 200),
		episode("series-a-progress-new", "series-a-16", "Example Series", "Latest In Progress", 2, 16, 2_000, 500),
		episode("series-b-completed", "series-b-1", "Other Series", "Other Completed", 1, 1, 3_000, 950),
	}
	movie := watchHistoryTestEvent(site.ID, "movie-session", 3_500, 400)
	movie.UpstreamItemID = "movie-item"
	events = append(events, movie)
	if skipped, err := database.writeWatchHistoryBatch(events); err != nil || skipped != 0 {
		t.Fatalf("write history: skipped=%d err=%v", skipped, err)
	}

	entries, err := database.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("history length=%d, want 3: %+v", len(entries), entries)
	}
	var example, other *WatchHistoryEntry
	for index := range entries {
		switch entries[index].SeriesName {
		case "Example Series":
			example = &entries[index]
		case "Other Series":
			other = &entries[index]
		}
	}
	if example == nil || example.Title != "Latest In Progress" || example.EpisodeNumber != 16 || example.Completed {
		t.Fatalf("preferred series entry = %+v", example)
	}
	if other == nil || other.Title != "Other Completed" || !other.Completed {
		t.Fatalf("completed-only series entry = %+v", other)
	}
}

func TestListWatchHistorySeriesGroupingStaysScopedToSite(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	firstSite := createWatchHistoryTestSite(t, database, true)
	secondSite := createWatchHistoryTestSite(t, database, true)
	for index, site := range []*Site{firstSite, secondSite} {
		event := watchHistoryTestEvent(site.ID, fmt.Sprintf("shared-series-%d", index), int64(1_000+index), 500)
		event.UpstreamItemID = fmt.Sprintf("shared-item-%d", index)
		event.MediaType = "episode"
		event.Title = fmt.Sprintf("Site %d Episode", index+1)
		event.SeriesName = "Shared Series Name"
		event.SeasonNumber = 1
		event.EpisodeNumber = index + 1
		event.TMDBType = ""
		if skipped, err := database.writeWatchHistoryBatch([]watchHistoryEvent{event}); err != nil || skipped != 0 {
			t.Fatalf("write site %d: skipped=%d err=%v", index, skipped, err)
		}
	}
	entries, err := database.ListWatchHistory(WatchHistoryFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].SiteID == entries[1].SiteID {
		t.Fatalf("cross-site series were merged: %+v", entries)
	}
}

func TestListWatchHistoryUnknownDurationDoesNotHideNewerCompletedEpisode(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	unknown := watchHistoryTestEvent(site.ID, "unknown-duration", 1_000, 200)
	unknown.UpstreamItemID = "unknown-episode"
	unknown.MediaType = "episode"
	unknown.Title = "Old Unknown Duration"
	unknown.SeriesName = "Duration Boundary Series"
	unknown.SeasonNumber = 1
	unknown.EpisodeNumber = 1
	unknown.RunTimeTicks = 0
	completed := watchHistoryTestEvent(site.ID, "new-completed", 2_000, 1_000)
	completed.UpstreamItemID = "completed-episode"
	completed.MediaType = "episode"
	completed.Title = "New Completed Episode"
	completed.SeriesName = "Duration Boundary Series"
	completed.SeasonNumber = 1
	completed.EpisodeNumber = 2
	completed.RunTimeTicks = 1_000
	if skipped, err := database.writeWatchHistoryBatch([]watchHistoryEvent{unknown, completed}); err != nil || skipped != 0 {
		t.Fatalf("write history: skipped=%d err=%v", skipped, err)
	}
	entries, err := database.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Title != "New Completed Episode" || !entries[0].Completed {
		t.Fatalf("unknown-duration row hid newer completed episode: %+v", entries)
	}
}

func TestEnqueueWatchHistoryNeverBlocksWhenQueueIsFull(t *testing.T) {
	database := &DB{dynamicObservationQueue: make(chan dynamicObservationCommand, 1)}
	event := watchHistoryTestEvent(1, "session", time.Now().UnixMilli(), 100)
	if !database.EnqueueWatchHistory(event) {
		t.Fatal("first enqueue failed")
	}
	done := make(chan bool, 1)
	go func() { done <- database.EnqueueWatchHistory(event) }()
	select {
	case enqueued := <-done:
		if enqueued {
			t.Fatal("full queue unexpectedly accepted an event")
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("watch history enqueue blocked the proxy path")
	}
	if database.DroppedWatchHistory() != 1 {
		t.Fatalf("dropped=%d, want 1", database.DroppedWatchHistory())
	}
}

func TestClearAndDeleteWatchHistoryRemoveChildrenWithoutForeignKeys(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	event := watchHistoryTestEvent(site.ID, "session-clear", time.Now().UnixMilli(), 100)
	if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{event}); err != nil {
		t.Fatal(err)
	}
	nowMS := time.Now().UnixMilli()
	if _, err := database.db.Exec(`INSERT INTO tmdb_cache
		(tmdb_type, tmdb_id, language, title, updated_at_ms, expires_at_ms)
		VALUES ('movie',999,'zh-CN','orphan',?,?)`, nowMS, nowMS+time.Hour.Milliseconds()); err != nil {
		t.Fatal(err)
	}
	if err := database.ClearWatchHistory(site.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"watch_sessions", "media_items", "tmdb_jobs", "tmdb_cache"} {
		var count int
		if err := database.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s after clear: count=%d err=%v", table, count, err)
		}
	}

	event.SessionHash = fmt.Sprintf("%064x", "session-delete")
	if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{event}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`UPDATE media_items SET tmdb_type='movie', tmdb_id=1000 WHERE site_id=?`, site.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO tmdb_cache
		(tmdb_type, tmdb_id, language, title, updated_at_ms, expires_at_ms)
		VALUES ('movie',1000,'zh-CN','site orphan',?,?)`, nowMS, nowMS+time.Hour.Milliseconds()); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteSite(site.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"watch_sessions", "media_items", "tmdb_jobs", "tmdb_cache"} {
		var count int
		if err := database.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s after site deletion: count=%d err=%v", table, count, err)
		}
	}
}

func TestDeleteWatchHistoryRemovesOnlyOrphanedTMDBCache(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	nowMS := time.Now().UnixMilli()
	for _, session := range []string{"session-one", "session-two"} {
		event := watchHistoryTestEvent(site.ID, session, nowMS, 100)
		if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{event}); err != nil {
			t.Fatal(err)
		}
	}
	var mediaItemID int64
	if err := database.db.QueryRow("SELECT id FROM media_items WHERE site_id=?", site.ID).Scan(&mediaItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec("UPDATE media_items SET tmdb_type='movie', tmdb_id=123 WHERE id=?", mediaItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(`INSERT INTO tmdb_cache
		(tmdb_type, tmdb_id, language, title, updated_at_ms, expires_at_ms)
		VALUES ('movie',123,'zh-CN','shared',?,?)`, nowMS, nowMS+time.Hour.Milliseconds()); err != nil {
		t.Fatal(err)
	}
	var firstHistoryID int64
	if err := database.db.QueryRow("SELECT id FROM watch_sessions ORDER BY id LIMIT 1").Scan(&firstHistoryID); err != nil {
		t.Fatal(err)
	}
	if err := database.deleteWatchHistoryRow(firstHistoryID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM media_items WHERE id=?", mediaItemID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("media item removed while still referenced: count=%d err=%v", count, err)
	}
	if err := database.db.QueryRow("SELECT COUNT(*) FROM tmdb_cache WHERE tmdb_type='movie' AND tmdb_id=123").Scan(&count); err != nil || count != 1 {
		t.Fatalf("shared cache removed too early: count=%d err=%v", count, err)
	}
	var secondHistoryID int64
	if err := database.db.QueryRow("SELECT id FROM watch_sessions LIMIT 1").Scan(&secondHistoryID); err != nil {
		t.Fatal(err)
	}
	if err := database.deleteWatchHistoryRow(secondHistoryID); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"SELECT COUNT(*) FROM watch_sessions",
		"SELECT COUNT(*) FROM media_items",
		"SELECT COUNT(*) FROM tmdb_jobs",
		"SELECT COUNT(*) FROM tmdb_cache",
	} {
		if err := database.db.QueryRow(query).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count=%d err=%v", query, count, err)
		}
	}
}

func TestPruneWatchHistoryKeepsNewestFiftyThousandRows(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	nowMS := time.Now().UnixMilli()
	result, err := database.db.Exec(`INSERT INTO media_items
		(site_id, upstream_item_id, created_at_ms, updated_at_ms) VALUES (?, 'bulk-item', ?, ?)`, site.ID, nowMS, nowMS)
	if err != nil {
		t.Fatal(err)
	}
	mediaItemID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.db.Exec(`WITH RECURSIVE sequence(value) AS (
		SELECT 1 UNION ALL SELECT value+1 FROM sequence WHERE value<=50000
	) INSERT INTO watch_sessions
		(site_id, media_item_id, session_hash, started_at_ms, last_seen_at_ms, position_ticks, runtime_ticks)
		SELECT ?, ?, printf('%064d', value), ?, ?+value, value, 100000
		FROM sequence`, site.ID, mediaItemID, nowMS, nowMS)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.pruneWatchHistory(); err != nil {
		t.Fatal(err)
	}
	var count int
	var oldest int64
	if err := database.db.QueryRow("SELECT COUNT(*), MIN(last_seen_at_ms) FROM watch_sessions").Scan(&count, &oldest); err != nil {
		t.Fatal(err)
	}
	if count != watchHistoryGlobalRowLimit || oldest != nowMS+2 {
		t.Fatalf("pruned rows=%d oldest=%d, want rows=%d oldest=%d", count, oldest, watchHistoryGlobalRowLimit, nowMS+2)
	}
}

func TestProxyRecordsSuccessfulPlaybackSyncWithoutChangingBody(t *testing.T) {
	requestBody := []byte(`{"ItemId":"proxy-item","PlaySessionId":"proxy-session","PositionTicks":500,"RunTimeTicks":1000,"Item":{"Id":"proxy-item","Name":"Proxy Movie","Type":"Movie"}}`)
	receivedBody := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		receivedBody <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	app := newTestApp(t)
	port := freePort(t)
	site, err := app.db.CreateSiteRecord(Site{
		Name:                "watch-history-proxy",
		ListenPort:          port,
		IngressMode:         ingressModePort,
		TargetURL:           upstream.URL,
		PlaybackMode:        "direct",
		StreamHosts:         "[]",
		UAMode:              passthroughUAMode,
		WatchHistoryEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	releasePort(port)
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })

	request, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/Sessions/Playing/Progress", port), bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("proxy status=%d", response.StatusCode)
	}
	select {
	case body := <-receivedBody:
		if !bytes.Equal(body, requestBody) {
			t.Fatal("proxy observation changed the upstream body")
		}
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive the request")
	}

	entries, err := app.db.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Title != "Proxy Movie" || entries[0].PositionTicks != 500 {
		t.Fatalf("proxy history=%+v", entries)
	}
}

func TestProxyEnrichesMinimalStandardPlaybackAfterMetadataResponse(t *testing.T) {
	receivedProgress := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/Items/proxy-minimal" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Id":"proxy-minimal","Name":"Proxy Cached Movie","Type":"Movie","ProductionYear":2024,"RunTimeTicks":8000,"ProviderIds":{"Tmdb":"789"}}`)
			return
		}
		if request.URL.Path == "/Sessions/Playing/Progress" {
			receivedProgress <- struct{}{}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	app := newTestApp(t)
	port := freePort(t)
	site, err := app.db.CreateSiteRecord(Site{
		Name:                "watch-history-minimal-proxy",
		ListenPort:          port,
		IngressMode:         ingressModePort,
		TargetURL:           upstream.URL,
		PlaybackMode:        "direct",
		StreamHosts:         "[]",
		UAMode:              passthroughUAMode,
		WatchHistoryEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	releasePort(port)
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })

	metadataRequest, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/Items/proxy-minimal", port), http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	metadataResponse, err := http.DefaultClient.Do(metadataRequest)
	if err != nil {
		t.Fatal(err)
	}
	metadataBody, readErr := io.ReadAll(metadataResponse.Body)
	_ = metadataResponse.Body.Close()
	if readErr != nil || metadataResponse.StatusCode != http.StatusOK || !bytes.Contains(metadataBody, []byte("Proxy Cached Movie")) {
		t.Fatalf("metadata response status=%d body=%s err=%v", metadataResponse.StatusCode, metadataBody, readErr)
	}

	progressBody := []byte(`{"ItemId":"proxy-minimal","PlaySessionId":"proxy-minimal-session","PositionTicks":500}`)
	progressRequest, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/Sessions/Playing/Progress", port), bytes.NewReader(progressBody))
	if err != nil {
		t.Fatal(err)
	}
	progressRequest.Header.Set("Content-Type", "application/json")
	progressResponse, err := http.DefaultClient.Do(progressRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = progressResponse.Body.Close()
	if progressResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("progress status=%d", progressResponse.StatusCode)
	}
	select {
	case <-receivedProgress:
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive minimal progress")
	}

	entries, err := app.db.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Title != "Proxy Cached Movie" || entries[0].MediaType != "movie" {
		t.Fatalf("minimal proxy history=%+v", entries)
	}
}
