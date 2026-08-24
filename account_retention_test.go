package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openAccountRetentionTestDB(t *testing.T) *DB {
	t.Helper()
	database, err := openDB(filepath.Join(t.TempDir(), "account-retention.db"))
	if err != nil {
		t.Fatalf("open retention database: %v", err)
	}
	t.Cleanup(database.Close)
	return database
}

func createAccountRetentionTestSite(t *testing.T, database *DB, name string, days int) *Site {
	t.Helper()
	site, err := database.CreateSiteRecord(Site{
		Name:                 name,
		ListenPort:           freePort(t),
		IngressMode:          ingressModePort,
		TargetURL:            "http://127.0.0.1:8096",
		PlaybackMode:         "direct",
		StreamHosts:          "[]",
		UAMode:               passthroughUAMode,
		AccountRetentionDays: days,
	})
	if err != nil {
		t.Fatalf("create retention site: %v", err)
	}
	return site
}

func TestAccountRetentionCompletesAfterFivePlaybackSyncsWithoutVideoTiming(t *testing.T) {
	database := openAccountRetentionTestDB(t)
	site := createAccountRetentionTestSite(t, database, "retention", 30)
	tracker := newAccountRetentionTracker(database)
	base := time.UnixMilli(site.AccountRetentionStartedMS).Add(time.Second)

	syncRequest := httptest.NewRequest(http.MethodPost, "http://media.example/Sessions/Playing/Progress", nil)
	syncRequest.RemoteAddr = "203.0.113.8:42000"
	syncRequest.Header.Set("User-Agent", "Meridian Retention Test")
	syncRequest.Header.Set("X-Emby-Token", "shared-token")

	for i := 0; i < accountRetentionRequiredPlaybackSyncs-1; i++ {
		observedAt := base.Add(time.Duration(i) * time.Second)
		tracker.Observe(*site, syncRequest, nil, requestLogCategoryPlaybackSync, http.StatusNoContent, observedAt, observedAt)
	}
	if err := database.flushDynamicObservations(); err != nil {
		t.Fatalf("flush before fifth playback sync: %v", err)
	}
	before, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.AccountRetentionCompletedMS != 0 || before.AccountRetentionStartedMS != site.AccountRetentionStartedMS {
		t.Fatalf("retention completed before fifth playback sync: %+v", before)
	}

	completedAt := base.Add(time.Duration(accountRetentionRequiredPlaybackSyncs-1) * time.Second)
	tracker.Observe(*site, syncRequest, nil, requestLogCategoryPlaybackSync, http.StatusNoContent, completedAt, completedAt)
	if err := database.flushDynamicObservations(); err != nil {
		t.Fatalf("flush completion: %v", err)
	}
	completed, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.AccountRetentionCompletedMS != completedAt.UnixMilli() || completed.AccountRetentionStartedMS != completed.AccountRetentionCompletedMS {
		t.Fatalf("retention completion = start %d complete %d, want %d", completed.AccountRetentionStartedMS, completed.AccountRetentionCompletedMS, completedAt.UnixMilli())
	}

	tracker.Observe(*site, syncRequest, nil, requestLogCategoryPlaybackSync, http.StatusNoContent, completedAt.Add(time.Second), completedAt.Add(time.Second))
	if err := database.flushDynamicObservations(); err != nil {
		t.Fatalf("flush playback sync after completion: %v", err)
	}
	after, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.AccountRetentionStartedMS != completed.AccountRetentionStartedMS || after.AccountRetentionCompletedMS != completed.AccountRetentionCompletedMS {
		t.Fatalf("playback sync after completion reset the cycle again: before=%+v after=%+v", completed, after)
	}
}

func TestAccountRetentionIgnoresVideoAndFailedPlaybackSyncRequests(t *testing.T) {
	database := openAccountRetentionTestDB(t)
	site := createAccountRetentionTestSite(t, database, "incomplete", 30)
	tracker := newAccountRetentionTracker(database)
	base := time.UnixMilli(site.AccountRetentionStartedMS).Add(time.Second)
	videoRequest := httptest.NewRequest(http.MethodGet, "http://media.example/Videos/1/stream.mkv", nil)
	videoRequest.RemoteAddr = "203.0.113.9:42000"
	videoRequest.Header.Set("User-Agent", "Meridian Retention Test")
	syncRequest := httptest.NewRequest(http.MethodPost, "http://media.example/Sessions/Playing/Progress", nil)
	syncRequest.RemoteAddr = videoRequest.RemoteAddr
	syncRequest.Header.Set("User-Agent", videoRequest.Header.Get("User-Agent"))

	tracker.Observe(*site, videoRequest, nil, requestLogCategoryStream, http.StatusPartialContent, base, base.Add(2*time.Hour))
	tracker.Observe(*site, syncRequest, nil, requestLogCategoryPlaybackSync, http.StatusBadGateway, base.Add(time.Second), base.Add(time.Second))
	for i := 0; i < accountRetentionRequiredPlaybackSyncs-1; i++ {
		observedAt := base.Add(time.Duration(i+2) * time.Second)
		tracker.Observe(*site, syncRequest, nil, requestLogCategoryPlaybackSync, http.StatusNoContent, observedAt, observedAt)
	}
	if err := database.flushDynamicObservations(); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccountRetentionCompletedMS != 0 || stored.AccountRetentionStartedMS != site.AccountRetentionStartedMS {
		t.Fatalf("non-sync, failed sync, or four successful syncs changed retention cycle: %+v", stored)
	}
}

func TestAccountRetentionCompletionAlwaysAdvancesTheCycle(t *testing.T) {
	database := openAccountRetentionTestDB(t)
	site := createAccountRetentionTestSite(t, database, "same-millisecond", 30)
	tracker := newAccountRetentionTracker(database)
	observedAt := time.UnixMilli(site.AccountRetentionStartedMS)
	request := httptest.NewRequest(http.MethodPost, "http://media.example/Sessions/Playing/Progress", nil)
	request.RemoteAddr = "203.0.113.10:42000"
	request.Header.Set("User-Agent", "Meridian Retention Test")
	request.Header.Set("X-Emby-Token", "same-millisecond-token")

	for i := 0; i < accountRetentionRequiredPlaybackSyncs; i++ {
		tracker.Observe(*site, request, nil, requestLogCategoryPlaybackSync, http.StatusNoContent, observedAt, observedAt)
	}
	if err := database.flushDynamicObservations(); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantCompletedAt := site.AccountRetentionStartedMS + 1
	if stored.AccountRetentionStartedMS != wantCompletedAt || stored.AccountRetentionCompletedMS != wantCompletedAt {
		t.Fatalf("completion did not advance cycle: got start=%d completed=%d want=%d", stored.AccountRetentionStartedMS, stored.AccountRetentionCompletedMS, wantCompletedAt)
	}
}

func TestAccountRetentionConfigurationPreservesAndResetsRuntimeState(t *testing.T) {
	database := openAccountRetentionTestDB(t)
	site := createAccountRetentionTestSite(t, database, "configuration", 30)
	completedAt := site.AccountRetentionStartedMS + time.Hour.Milliseconds()
	if _, err := database.db.Exec(`UPDATE sites SET account_retention_started_at_ms=?, account_retention_last_completed_at_ms=? WHERE id=?`, completedAt, completedAt, site.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.Name = "configuration-renamed"
	stored.AccountRetentionDays = 45
	if err := database.UpdateSiteRecord(*stored); err != nil {
		t.Fatalf("update enabled retention: %v", err)
	}
	preserved, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.AccountRetentionStartedMS != completedAt || preserved.AccountRetentionCompletedMS != completedAt || preserved.AccountRetentionDays != 45 {
		t.Fatalf("ordinary edit reset runtime state: %+v", preserved)
	}

	preserved.AccountRetentionDays = 0
	if err := database.UpdateSiteRecord(*preserved); err != nil {
		t.Fatalf("disable retention: %v", err)
	}
	disabled, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if disabled.AccountRetentionStartedMS != 0 || disabled.AccountRetentionDays != 0 {
		t.Fatalf("disabled retention still has a cycle: %+v", disabled)
	}

	disabled.AccountRetentionDays = 30
	if err := database.UpdateSiteRecord(*disabled); err != nil {
		t.Fatalf("reenable retention: %v", err)
	}
	reenabled, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reenabled.AccountRetentionStartedMS <= 0 || reenabled.AccountRetentionCompletedMS != 0 || reenabled.AccountRetentionDays != 30 {
		t.Fatalf("reenabled retention did not start a clean cycle: %+v", reenabled)
	}
}

func TestCaptureMediaLibraryCountsPersistsChunkedResponseWithoutChangingBody(t *testing.T) {
	database := openAccountRetentionTestDB(t)
	site := createAccountRetentionTestSite(t, database, "library", 0)
	payload := `{"MovieCount":19778,"SeriesCount":28127,"EpisodeCount":339044}`
	request := httptest.NewRequest(http.MethodGet, "http://media.example/emby/Items/Counts", nil)
	response := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:          io.NopCloser(strings.NewReader(payload)),
		ContentLength: -1,
		Request:       request,
	}
	if err := captureMediaLibraryCounts(response, database, site.ID); err != nil {
		t.Fatalf("capture counts: %v", err)
	}
	replayed, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(replayed) != payload || response.ContentLength != -1 {
		t.Fatalf("response body changed: len=%d body=%q", response.ContentLength, replayed)
	}
	if err := database.flushDynamicObservations(); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.MediaMovieCount != 19778 || stored.MediaSeriesCount != 28127 || stored.MediaEpisodeCount != 339044 || stored.MediaCountUpdatedMS <= 0 {
		t.Fatalf("stored media counts = %+v", stored)
	}
}

func TestCaptureMediaLibraryCountsDoesNotTruncateOversizedChunkedResponse(t *testing.T) {
	database := openAccountRetentionTestDB(t)
	site := createAccountRetentionTestSite(t, database, "library-large", 0)
	payload := `{"MovieCount":19778,"SeriesCount":28127,"EpisodeCount":339044,"Padding":"` +
		strings.Repeat("x", mediaLibraryCountBodyLimit) + `"}`
	request := httptest.NewRequest(http.MethodGet, "http://media.example/Items/Counts", nil)
	response := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(strings.NewReader(payload)),
		ContentLength: -1,
		Request:       request,
	}
	if err := captureMediaLibraryCounts(response, database, site.ID); err != nil {
		t.Fatalf("capture counts: %v", err)
	}
	replayed, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(replayed) != payload || response.ContentLength != -1 {
		t.Fatalf("oversized response changed: len=%d got=%d want=%d", response.ContentLength, len(replayed), len(payload))
	}
	if err := database.flushDynamicObservations(); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.MediaCountUpdatedMS != 0 {
		t.Fatalf("oversized count response must not be persisted: %+v", stored)
	}
}

func TestAccountRetentionStatusUsesConfiguredReportTimezone(t *testing.T) {
	location := time.FixedZone("UTC+8", 8*60*60)
	now := time.Date(2026, time.August, 24, 0, 30, 0, 0, location)
	completed := now.Add(-15 * time.Minute)
	status := accountRetentionStatusAt(Site{
		AccountRetentionDays:        30,
		AccountRetentionStartedMS:   completed.UnixMilli(),
		AccountRetentionCompletedMS: completed.UnixMilli(),
	}, now, location)
	if !status.Enabled || !status.CompletedToday || status.RemainingDays != 30 {
		t.Fatalf("retention status = %+v", status)
	}
}
