package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProxyStopIntentRemovesActiveBeforeOriginCompletes(t *testing.T) {
	stopReceived := make(chan struct{})
	releaseStop := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if strings.HasSuffix(r.URL.Path, "/Stopped") {
			close(stopReceived)
			select {
			case <-releaseStop:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	t.Cleanup(func() { close(releaseStop) })
	app := newTestApp(t)
	port := freePort(t)
	site, err := app.db.CreateSiteRecord(Site{
		Name: "stop-intent-proxy", ListenPort: port, IngressMode: ingressModePort,
		TargetURL: upstream.URL, PlaybackMode: "direct", StreamHosts: "[]",
		UAMode: passthroughUAMode, WatchHistoryEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	releasePort(port)
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", port)
	const body = `{"ItemId":"stop-item","PlaySessionId":"stop-session","PositionTicks":100,"RunTimeTicks":1000}`
	post := func(ctx context.Context, path string) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+path, bytes.NewBufferString(body))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Emby-Authorization", `MediaBrowser Token="stop-test-token", Client="Origami", DeviceId="phone"`)
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
		}
		return err
	}
	if err := post(t.Context(), "/Sessions/Playing/Progress"); err != nil {
		t.Fatal(err)
	}
	if entries, err := app.db.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID}); err != nil || len(entries) != 1 {
		t.Fatalf("successful playback history count=%d err=%v", len(entries), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- post(ctx, "/Sessions/Playing/Stopped") }()
	select {
	case <-stopReceived:
	case <-time.After(time.Second):
		t.Fatal("origin did not consume stopped body")
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for {
		active, err := app.db.ListActiveWatchHistory(WatchHistoryFilter{SiteID: site.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(active) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("complete client stop intent remained active while origin was still pending")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("stop finished before the blocked-origin probe")
	default:
	}
	entries, err := app.db.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID})
	if err != nil || len(entries) != 1 || entries[0].ClientStopOutcome != "pending" || entries[0].ClientStopRequestedAtMS == 0 || entries[0].StoppedAtMS != 0 {
		t.Fatalf("pending origin stop was misrepresented: entries=%+v err=%v", entries, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled stop did not release the proxy")
	}
	deadline = time.Now().Add(time.Second)
	for {
		entries, err := app.db.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 1 && entries[0].ClientStopOutcome == "unconfirmed" {
			if entries[0].StoppedAtMS != 0 {
				t.Fatal("cancelled origin stop fabricated a confirmed stop time")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelled stop did not publish an unconfirmed outcome")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func capturedStopTestPlayback(t *testing.T, site Site, token string, observedAt time.Time) watchHistoryEvent {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://media.example/Sessions/Playing/Progress", strings.NewReader(`{"ItemId":"item-1","PlaySessionId":"session-1","UserId":"user-a","PositionTicks":100,"RunTimeTicks":1000}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Emby-Authorization", fmt.Sprintf(`MediaBrowser Token="%s", Client="Origami", DeviceId="phone-a"`, token))
	capture := startWatchHistoryCapture(site, request, requestLogCategoryPlaybackSync, nil)
	if capture == nil {
		t.Fatal("playback capture unavailable")
	}
	_, _ = io.Copy(io.Discard, request.Body)
	event, ok := watchHistoryEventFromCapture(capture, nil, site, request, nil, http.StatusNoContent, observedAt)
	if !ok || event.ClientIdentityHash == "" {
		t.Fatal("successful playback did not establish a credential proof")
	}
	event.ResponseStatusCode = http.StatusNoContent
	return event
}

func stopTestEvent(playback watchHistoryEvent, atMS int64, outcome string, status int) watchHistoryEvent {
	event := playback
	event.EventType, event.ObservedAtMS = "stopped", atMS
	event.TokenCiphertext = ""
	event.ClientStopRequestedAtMS, event.ClientStopOutcome = atMS, outcome
	event.ResponseStatusCode = status
	if outcome == "confirmed" {
		event.StopResponseAtMS = atMS + 10
	}
	return event
}

func assertStopTestActive(t *testing.T, database *DB, siteID int64, now time.Time, count int) {
	t.Helper()
	entries, err := database.listActiveWatchHistoryAt(WatchHistoryFilter{SiteID: siteID}, now)
	if err != nil || len(entries) != count {
		t.Fatalf("active count=%d, want %d, err=%v", len(entries), count, err)
	}
}

func TestWatchHistoryStopIntentMatchesOnlyObservedClient(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	otherSite := createWatchHistoryTestSite(t, database, true)
	now := time.Now()
	playback := capturedStopTestPlayback(t, *site, "account-a-secret", now.Add(-time.Second))
	if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{playback}); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*watchHistoryEvent)
	}{
		{"other account", func(event *watchHistoryEvent) {
			event.ClientIdentityHash = watchHistoryRequestIdentityHash(site.ID, "account-b-secret")
		}},
		{"other site", func(event *watchHistoryEvent) { event.SiteID = otherSite.ID }},
		{"other item", func(event *watchHistoryEvent) { event.UpstreamItemID = "other-item" }},
		{"other session hash", func(event *watchHistoryEvent) { event.SessionHash = strings.Repeat("a", 64) }},
		{"known hash wrong raw session", func(event *watchHistoryEvent) { event.PlaySessionID = "third-party-session" }},
		{"other user", func(event *watchHistoryEvent) { event.UserID = "user-b" }},
		{"other device", func(event *watchHistoryEvent) { event.DeviceID = "phone-b" }},
		{"other client", func(event *watchHistoryEvent) { event.ClientName = "other-client" }},
		{"missing credential", func(event *watchHistoryEvent) { event.ClientIdentityHash = "" }},
		{"other source node", func(event *watchHistoryEvent) { event.sourceNodeID = 17 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			event := stopTestEvent(playback, now.UnixMilli(), "pending", 0)
			tc.mutate(&event)
			if database.watchHistoryStops.submit(event) {
				t.Fatal("unmatched stop was admitted to the local overlay")
			}
			if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{event}); err != nil {
				t.Fatal(err)
			}
			assertStopTestActive(t, database, site.ID, now, 1)
		})
	}
	for _, table := range []string{"media_items", "watch_sessions"} {
		var count int
		if err := database.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("unmatched stop created %s: count=%d err=%v", table, count, err)
		}
	}
}

func TestWatchHistoryStopIntentProgressOrderingAndConfirmedReplay(t *testing.T) {
	for _, outcome := range []string{"unconfirmed", "confirmed", "denied"} {
		t.Run(outcome, func(t *testing.T) {
			database := openWatchHistoryTestDB(t)
			site := createWatchHistoryTestSite(t, database, true)
			now := time.Now()
			playback := capturedStopTestPlayback(t, *site, "ordering-secret", now.Add(-time.Second))
			if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{playback}); err != nil {
				t.Fatal(err)
			}
			status := 0
			if outcome == "confirmed" {
				status = http.StatusNoContent
			} else if outcome == "denied" {
				status = http.StatusForbidden
			}
			stop := stopTestEvent(playback, playback.ObservedAtMS+100, outcome, status)
			if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{stop}); err != nil {
				t.Fatal(err)
			}
			wantActive := 0
			if outcome == "denied" {
				wantActive = 1
			}
			assertStopTestActive(t, database, site.ID, now, wantActive)
			for _, progressMS := range []int64{playback.ObservedAtMS + 50, stop.ObservedAtMS} {
				old := playback
				old.ObservedAtMS = progressMS
				if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{old}); err != nil {
					t.Fatal(err)
				}
				assertStopTestActive(t, database, site.ID, now, wantActive)
			}
			newer := playback
			newer.EventType, newer.ObservedAtMS = "started", stop.ObservedAtMS+100
			if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{newer}); err != nil {
				t.Fatal(err)
			}
			assertStopTestActive(t, database, site.ID, now, 1)
			staleConfirmed := stopTestEvent(playback, stop.ObservedAtMS, "confirmed", http.StatusNoContent)
			staleConfirmed.StopResponseAtMS = newer.ObservedAtMS + 1000
			if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{staleConfirmed}); err != nil {
				t.Fatal(err)
			}
			entries, err := database.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID})
			if err != nil || len(entries) != 1 || entries[0].ClientStopRequestedAtMS != 0 || entries[0].ClientStopOutcome != "" || entries[0].StoppedAtMS != 0 {
				t.Fatalf("newer successful playback did not clear the old stop: entries=%+v err=%v", entries, err)
			}
		})
	}
}

func TestWatchHistoryStopIntentFirstSuccessfulBatch(t *testing.T) {
	for _, outcome := range []string{"pending", "confirmed"} {
		t.Run(outcome, func(t *testing.T) {
			database := openWatchHistoryTestDB(t)
			site := createWatchHistoryTestSite(t, database, true)
			now := time.Now()
			playback := capturedStopTestPlayback(t, *site, "first-batch-secret", now.Add(-time.Second))
			status := 0
			if outcome == "confirmed" {
				status = http.StatusNoContent
			}
			stop := stopTestEvent(playback, playback.ObservedAtMS+100, outcome, status)
			if skipped, err := database.writeWatchHistoryBatch([]watchHistoryEvent{playback, stop}); err != nil || skipped != 0 {
				t.Fatalf("first successful progress/stop batch skipped=%d err=%v", skipped, err)
			}
			assertStopTestActive(t, database, site.ID, now, 0)
			entries, err := database.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID})
			if err != nil || len(entries) != 1 || entries[0].ClientStopOutcome != outcome || (outcome == "confirmed") != (entries[0].StoppedAtMS > 0) {
				t.Fatalf("first batch stop outcome=%+v err=%v", entries, err)
			}
			encoded, err := json.Marshal(entries)
			if err != nil || strings.Contains(string(encoded), "client_identity_hash") || strings.Contains(string(encoded), "first-batch-secret") || strings.Contains(string(encoded), "token_ciphertext") {
				t.Fatal("watch-history response exposed stop matching credentials")
			}
		})
	}
}

func TestWatchHistoryStopIntentMalformedCaptureKeepsActive(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	now := time.Now()
	playback := capturedStopTestPlayback(t, *site, "malformed-secret", now.Add(-time.Second))
	if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{playback}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body, encoding string
		partial              bool
	}{
		{name: "malformed JSON", body: `{"ItemId":"item-1","PlaySessionId":"session-1",`},
		{name: "malformed numeric JSON", body: `{"ItemId":"item-1","PlaySessionId":"session-1","PositionTicks":"invalid"}`},
		{name: "malformed numeric form", body: "ItemId=item-1&PlaySessionId=session-1&PositionTicks=invalid"},
		{name: "mismatched item", body: `{"ItemId":"item-1","PlaySessionId":"session-1","Item":{"Id":"other-item"}}`},
		{name: "partial body", body: `{"ItemId":"item-1","PlaySessionId":"session-1"}`, partial: true},
		{name: "unsupported encoding", body: `{"ItemId":"item-1","PlaySessionId":"session-1"}`, encoding: "br"},
		{name: "oversized body", body: strings.Repeat("x", watchHistoryBodyLimit+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://media.example/Sessions/Playing/Stopped", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Content-Encoding", tc.encoding)
			request.Header.Set("X-Emby-Authorization", `MediaBrowser Token="malformed-secret", Client="Origami", DeviceId="phone-a"`)
			capture := startWatchHistoryCapture(*site, request, requestLogCategoryPlaybackSync, database)
			attachWatchHistoryStopIntentCapture(capture, database, *site, request, nil, now)
			if tc.partial {
				_, _ = request.Body.Read(make([]byte, 8))
			} else {
				_, _ = io.Copy(io.Discard, request.Body)
			}
			finishWatchHistoryCapture(capture, database, *site, request, nil, http.StatusBadRequest, now)
			assertStopTestActive(t, database, site.ID, now, 1)
		})
	}
}

func TestWatchHistoryStopIntentQueueFullAndClosedCannotConfirm(t *testing.T) {
	site := Site{ID: 11, Enabled: true, WatchHistoryEnabled: true}
	now := time.Now()
	playback := capturedStopTestPlayback(t, site, "queue-secret", now.Add(-time.Second))
	state := &watchHistoryStopState{sessions: make(map[watchHistorySessionKey]watchHistoryTrustedSession), queue: make(chan watchHistoryEvent, 1), done: make(chan struct{})}
	state.rememberPlayback(playback)
	state.queue <- watchHistoryEvent{}
	started := time.Now()
	if state.submit(stopTestEvent(playback, now.UnixMilli(), "confirmed", http.StatusNoContent)) {
		t.Fatal("full stop queue accepted a confirmation")
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("full stop queue blocked the proxy hot path")
	}
	entry := WatchHistoryEntry{SiteID: site.ID, sessionHash: playback.SessionHash, LastSeenAtMS: playback.ObservedAtMS}
	state.overlay(&entry)
	if entry.ClientStopOutcome != "unconfirmed" || entry.StoppedAtMS != 0 {
		t.Fatalf("failed enqueue fabricated source confirmation: %+v", entry)
	}
	close(state.done)
	state.close()
	if state.submit(stopTestEvent(playback, now.UnixMilli()+1, "pending", 0)) {
		t.Fatal("closed stop queue accepted work")
	}
}

func TestWatchHistoryStopIntentMigration54PreservesExistingHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema54.db")
	database, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	site := createWatchHistoryTestSite(t, database, true)
	playback := capturedStopTestPlayback(t, *site, "migration-secret", time.Now())
	if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{playback}); err != nil {
		t.Fatal(err)
	}
	database.Close()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"ALTER TABLE watch_sessions DROP COLUMN client_stop_requested_at_ms", "ALTER TABLE watch_sessions DROP COLUMN client_stop_outcome", "PRAGMA user_version=54"} {
		if _, err := raw.Exec(statement); err != nil {
			_ = raw.Close()
			t.Fatal(err)
		}
	}
	_ = raw.Close()
	database, err = openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.migrate(); err != nil {
		t.Fatal(err)
	}
	entries, err := database.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID})
	if err != nil || len(entries) != 1 || entries[0].ClientStopRequestedAtMS != 0 || entries[0].ClientStopOutcome != "" || entries[0].LastSeenAtMS != playback.ObservedAtMS {
		t.Fatalf("schema54 upgrade changed existing playback: entries=%+v err=%v", entries, err)
	}
	var version int
	if err := database.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != databaseSchemaVersion {
		t.Fatalf("stop intent migration version=%d err=%v", version, err)
	}
}

func TestWatchHistoryStopIntentConfirmedSavesFinalProgressOnly(t *testing.T) {
	for _, outcome := range []string{"pending", "unconfirmed", "denied", "confirmed"} {
		t.Run(outcome, func(t *testing.T) {
			database := openWatchHistoryTestDB(t)
			site := createWatchHistoryTestSite(t, database, true)
			playback := capturedStopTestPlayback(t, *site, "final-progress-secret", time.Now().Add(-time.Second))
			if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{playback}); err != nil {
				t.Fatal(err)
			}
			status := 0
			if outcome == "confirmed" {
				status = http.StatusNoContent
			} else if outcome == "denied" {
				status = http.StatusForbidden
			}
			stop := stopTestEvent(playback, playback.ObservedAtMS+100, outcome, status)
			stop.PositionTicks, stop.RunTimeTicks, stop.PlayMethod = 1000, 1000, "Transcode"
			if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{stop}); err != nil {
				t.Fatal(err)
			}
			entries, err := database.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID})
			if err != nil || len(entries) != 1 {
				t.Fatalf("final progress entry count=%d err=%v", len(entries), err)
			}
			entry := entries[0]
			if outcome == "confirmed" {
				if entry.PositionTicks != 1000 || entry.RunTimeTicks != 1000 || entry.PlayMethod != "Transcode" || !entry.Completed || entry.LastSeenAtMS != stop.ObservedAtMS || entry.StoppedAtMS != stop.StopResponseAtMS {
					t.Fatalf("confirmed stop lost final playback data: %+v", entry)
				}
			} else if entry.PositionTicks != playback.PositionTicks || entry.LastSeenAtMS != playback.ObservedAtMS || entry.Completed || entry.StoppedAtMS != 0 {
				t.Fatalf("unconfirmed stop fabricated final playback progress: %+v", entry)
			}
		})
	}
}

func TestWatchHistoryStopIntentNewSourceInSameBatchSupersedesOldProof(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	now := time.Now()
	old := capturedStopTestPlayback(t, *site, "source-migration-secret", now.Add(-time.Second))
	old.sourceNodeID, old.TokenCiphertext = 11, ""
	if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{old}); err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.sourceNodeID, newer.ObservedAtMS = 12, old.ObservedAtMS+100
	oldStop := stopTestEvent(old, newer.ObservedAtMS+100, "pending", 0)
	if skipped, err := database.writeWatchHistoryBatch([]watchHistoryEvent{newer, oldStop}); err != nil || skipped != 1 {
		t.Fatalf("source migration stop skipped=%d err=%v", skipped, err)
	}
	assertStopTestActive(t, database, site.ID, now, 1)
	entries, err := database.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID})
	if err != nil || len(entries) != 1 || entries[0].ClientStopRequestedAtMS != 0 {
		t.Fatalf("old source stop hid the new source: entries=%+v err=%v", entries, err)
	}
}

func TestWatchHistoryStopIntentCloseDrainsAdmittedConfirmation(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	playback := capturedStopTestPlayback(t, *site, "close-drain-secret", time.Now().Add(-time.Second))
	if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{playback}); err != nil {
		t.Fatal(err)
	}
	database.watchHistoryWriteMu.Lock()
	locked := true
	defer func() {
		if locked {
			database.watchHistoryWriteMu.Unlock()
		}
	}()
	stop := stopTestEvent(playback, playback.ObservedAtMS+100, "confirmed", http.StatusNoContent)
	if !database.watchHistoryStops.submit(stop) {
		t.Fatal("valid confirmed stop was not admitted")
	}
	done := make(chan struct{})
	go func() {
		database.Close()
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		database.watchHistoryStops.mu.Lock()
		closed := database.watchHistoryStops.closed
		database.watchHistoryStops.mu.Unlock()
		if closed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("close never stopped intent admission")
		}
		time.Sleep(time.Millisecond)
	}
	database.watchHistoryWriteMu.Unlock()
	locked = false
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not finish draining playback events")
	}
	reopened, err := openDB(database.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	entry := waitStopTestOutcome(t, reopened, site.ID, "confirmed")
	if entry.StoppedAtMS != stop.StopResponseAtMS {
		t.Fatal("shutdown discarded an admitted origin confirmation")
	}
	assertStopTestActive(t, reopened, site.ID, time.Now(), 0)
	if reopened.watchHistoryStops.submit(stopTestEvent(playback, stop.ObservedAtMS+10, "pending", 0)) {
		t.Fatal("a restarted process accepted an incoming stop without observed client proof")
	}
}

func TestWatchHistoryStopIntentBridgesAdjacentFallbackBucket(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	seconds := int64(accountRetentionSessionTTL / time.Second)
	boundary := time.Unix((time.Now().Unix()/seconds+1)*seconds, 0)
	request := httptest.NewRequest(http.MethodPost, "http://media.example/Sessions/Playing/Progress", strings.NewReader(`{"ItemId":"item-1","PositionTicks":100,"RunTimeTicks":1000}`))
	request.Header.Set("X-Emby-Authorization", `MediaBrowser Token="bucket-secret", Client="Origami", DeviceId="phone-a"`)
	capture := startWatchHistoryCapture(*site, request, requestLogCategoryPlaybackSync, database)
	_, _ = io.Copy(io.Discard, request.Body)
	playback, ok := watchHistoryEventFromCapture(capture, database, *site, request, nil, http.StatusNoContent, boundary.Add(-10*time.Second))
	if !ok {
		t.Fatal("fallback playback was not parsed")
	}
	if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{playback}); err != nil {
		t.Fatal(err)
	}
	stopRequest := httptest.NewRequest(http.MethodPost, "http://media.example/Sessions/Playing/Stopped", strings.NewReader(`{"ItemId":"item-1","PositionTicks":110}`))
	stopRequest.Header = request.Header.Clone()
	stopCapture := startWatchHistoryCapture(*site, stopRequest, requestLogCategoryPlaybackSync, database)
	_, _ = io.Copy(io.Discard, stopRequest.Body)
	stop, ok := watchHistoryStopEventFromCapture(stopCapture, *site, stopRequest, nil, boundary.Add(10*time.Second))
	if !ok || stop.SessionHash == playback.SessionHash {
		t.Fatal("test did not cross the fallback digest bucket")
	}
	if skipped, err := database.writeWatchHistoryBatch([]watchHistoryEvent{stop}); err != nil || skipped != 0 {
		t.Fatalf("adjacent fallback stop skipped=%d err=%v", skipped, err)
	}
	assertStopTestActive(t, database, site.ID, boundary.Add(10*time.Second), 0)
	var count int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM watch_sessions").Scan(&count); err != nil || count != 1 {
		t.Fatalf("fallback stop created a session: count=%d err=%v", count, err)
	}
}
