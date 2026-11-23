package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func watchHistorySourceAddressTestRequest(t *testing.T, authority, path string) *http.Request {
	t.Helper()
	target, err := url.Parse(authority)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, nil)
	return request.WithContext(context.WithValue(request.Context(), pathTargetContextKey{}, target))
}

func watchHistorySourceAddressTestEvent(t *testing.T, siteID int64, authority, mount, itemID string, now time.Time) watchHistoryEvent {
	t.Helper()
	request := watchHistorySourceAddressTestRequest(t, authority, (&url.URL{Path: mount + "/Sessions/Playing/Progress"}).EscapedPath())
	event := watchHistoryTestEvent(siteID, "same-session", now.UnixMilli(), 100)
	event.SourceKey = watchHistorySourceKeyFromRequest(request)
	event.SourceAddress = watchHistorySourceAddressFromRequest(request)
	event.UpstreamItemID = itemID
	event.SessionHash = watchHistorySessionDigest(siteID, "same-session", "same-viewer", itemID, now, event.SourceKey)
	if !validWatchHistoryEvent(event) || event.SourceAddress == "" {
		t.Fatal("source address fixture is invalid")
	}
	return event
}

func TestWatchHistorySourceAddressCanonicalizesAuthorityAndStripsCredentials(t *testing.T) {
	for _, test := range []struct{ authority, path, address string }{
		{"https://synthetic-user:synthetic-password@ORIGIN.example./unused?api_key=synthetic-target-token#synthetic-fragment", "/emby/Users/viewer/Items/movie?api_key=synthetic-request-token", "https://origin.example:443/emby"},
		{"http://192.168.1.20", "/Sessions/Playing/Progress", "http://192.168.1.20:80"},
		{"http://localhost:8096", "/tenant/emby/Items/movie", "http://localhost:8096/tenant/emby"},
		{"http://[0:0:0:0:0:0:0:1]", "/emby/Sessions/Playing", "http://[::1]:80/emby"},
		{"https://[fd00:0000:0000:0000:0000:0000:0000:0002]:8443", "/a/emby/Users/u/PlayingItems/movie/Progress", "https://[fd00::2]:8443/a/emby"},
		{"https://origin.example:443", "/tenant%20two/emby/Shows/show/Episodes", "https://origin.example:443/tenant%20two/emby"},
	} {
		t.Run(test.address, func(t *testing.T) {
			request := watchHistorySourceAddressTestRequest(t, test.authority, test.path)
			address := watchHistorySourceAddressFromRequest(request)
			if address != test.address || !validWatchHistorySourceAddress(watchHistorySourceKeyFromRequest(request), address) {
				t.Fatalf("source address=%q, want %q", address, test.address)
			}
			if strings.Contains(address, "synthetic-") {
				t.Fatal("source address retained a request credential or fragment")
			}
		})
	}
	request := watchHistorySourceAddressTestRequest(t, "https://ORIGIN.example:443", "/emby/Sessions/Playing/Progress?api_key=synthetic-token")
	legacyDigest := sha256.Sum256([]byte("meridian-watch-history-source-v1\x00https://origin.example:443\x00/emby"))
	if got := watchHistorySourceKeyFromRequest(request); got != fmt.Sprintf("%x", legacyDigest) {
		t.Fatal("adding source addresses changed a preexisting source digest")
	}
	fixed := httptest.NewRequest(http.MethodPost, "/emby/Sessions/Playing", nil)
	if watchHistorySourceAddressFromRequest(fixed) != "" || watchHistorySourceKeyFromRequest(fixed) != "" {
		t.Fatal("fixed-upstream request gained generic source fields")
	}
}

func TestWatchHistorySourceAddressAgentRoundTripAndValidation(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	event := watchHistorySourceAddressTestEvent(t, site.ID, "https://origin.example", "/emby", "agent-item", time.Now())
	wire := NodeRequestEvent{SiteID: site.ID, StatusCode: http.StatusNoContent, WatchHistory: &event}
	encoded, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	var decoded NodeRequestEvent
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.WatchHistory == nil || decoded.WatchHistory.SourceAddress != event.SourceAddress || !validWatchHistoryEvent(*decoded.WatchHistory) {
		t.Fatal("Agent source address did not survive structured event serialization")
	}
	if err := database.processNodeEventFromSource(decoded, 9); err != nil {
		t.Fatalf("persist structured Agent source address: %v", err)
	}
	for _, address := range []string{
		"https://synthetic-user:synthetic-pass@origin.example:443/emby",
		"https://origin.example:443/emby?api_key=synthetic-secret",
		"https://origin.example:443/emby?",
		"https://origin.example:443/emby#fragment",
		"https://origin.example/emby",
		"https://ORIGIN.example:443/emby",
		"https://other.example:443/emby",
		"https://origin.example:443/other/emby",
		"https://origin.example:443/emby/Items/agent-item",
		"ftp://origin.example:443/emby",
		"https://origin.example:443/emby%0A",
		"https://origin.example:443/" + strings.Repeat("a", watchHistoryMaxSourceAddressBytes),
	} {
		invalid := event
		invalid.SourceAddress = address
		if validWatchHistoryEvent(invalid) {
			t.Fatalf("Agent event accepted an unsafe, noncanonical or mismatched address %q", address)
		}
	}
	oldAgent := event
	oldAgent.SourceAddress = ""
	if !validWatchHistoryEvent(oldAgent) {
		t.Fatal("old Agent digest-only event became invalid")
	}
	if err := database.processNodeEventFromSource(NodeRequestEvent{SiteID: site.ID, StatusCode: http.StatusNoContent, WatchHistory: &oldAgent}, 9); err != nil {
		t.Fatalf("old Agent source event: %v", err)
	}
	entries, err := database.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID})
	if err != nil || len(entries) != 1 || entries[0].SourceAddress != "https://origin.example:443" {
		t.Fatalf("old Agent erased a learned source address: count=%d err=%v", len(entries), err)
	}
	var storedAddress string
	if err := database.db.QueryRow("SELECT source_address FROM watch_history_sources WHERE site_id=? AND source_key=?", site.ID, event.SourceKey).Scan(&storedAddress); err != nil || storedAddress != event.SourceAddress {
		t.Fatalf("Agent event lost its internal API mount: address=%q err=%v", storedAddress, err)
	}
	fixed := watchHistoryTestEvent(site.ID, "fixed", time.Now().UnixMilli(), 100)
	fixed.SourceAddress = event.SourceAddress
	if validWatchHistoryEvent(fixed) {
		t.Fatal("an address without a source digest was accepted")
	}
	fixed.SourceAddress = ""
	encoded, err = json.Marshal(fixed)
	if err != nil || strings.Contains(string(encoded), "source_address") {
		t.Fatal("fixed-upstream Agent event changed its optional wire fields")
	}
}

func TestWatchHistorySourceAddressCapturedByModernAndLegacyPlayback(t *testing.T) {
	site := Site{ID: 7, UpstreamMode: upstreamModePathTarget, WatchHistoryEnabled: true}
	request := watchHistorySourceAddressTestRequest(t, "https://origin.example", "/tenant/emby/Sessions/Playing/Progress?api_key=synthetic-token")
	request.Body = io.NopCloser(strings.NewReader(`{"ItemId":"movie","PlaySessionId":"session","PositionTicks":100,"RunTimeTicks":1000}`))
	request.ContentLength = -1
	request.Header.Set("Content-Type", "application/json")
	capture := startWatchHistoryCapture(site, request, requestLogCategoryPlaybackSync, nil)
	if capture == nil {
		t.Fatal("modern generic playback was not observed")
	}
	_, _ = io.Copy(io.Discard, capture)
	modern, ok := watchHistoryEventFromCapture(capture, nil, site, request, nil, http.StatusNoContent, time.Now())
	if !ok || modern.SourceAddress != "https://origin.example:443/tenant/emby" {
		t.Fatal("modern playback omitted the source address")
	}
	legacyRequest := watchHistorySourceAddressTestRequest(t, "https://origin.example:443", "/tenant/emby/Users/viewer/PlayingItems/movie/Progress?PlaySessionId=session&PositionTicks=100&RunTimeTicks=1000&api_key=synthetic-token")
	legacy, ok := watchHistoryLegacyProgressEvent(site, legacyRequest, nil, time.Now())
	if !ok || legacy.SourceKey != modern.SourceKey || legacy.SourceAddress != modern.SourceAddress {
		t.Fatal("legacy playback used a different source address or omitted it")
	}
}

func TestWatchHistorySourceAddressSeparatesSourcesAndFillsOlderItems(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	now := time.Now()
	old := watchHistorySourceAddressTestEvent(t, site.ID, "https://origin-a.example", "/emby", "old-item", now)
	old.SourceAddress = ""
	b := watchHistorySourceAddressTestEvent(t, site.ID, "https://origin-b.example", "/emby", "shared-item", now)
	otherMount := watchHistorySourceAddressTestEvent(t, site.ID, "https://origin-a.example", "/other/emby", "shared-item", now)
	fixed := watchHistoryTestEvent(site.ID, "fixed", now.UnixMilli(), 100)
	fixed.UpstreamItemID = "fixed-item"
	if skipped, err := database.writeWatchHistoryBatch([]watchHistoryEvent{old, b, otherMount, fixed}); err != nil || skipped != 0 {
		t.Fatalf("write sources: skipped=%d err=%v", skipped, err)
	}
	a := watchHistorySourceAddressTestEvent(t, site.ID, "https://origin-a.example:443", "/emby", "shared-item", now.Add(time.Millisecond))
	if skipped, err := database.writeWatchHistoryBatch([]watchHistoryEvent{a}); err != nil || skipped != 0 {
		t.Fatalf("learn existing source address: skipped=%d err=%v", skipped, err)
	}
	if _, err := database.db.Exec("INSERT INTO watch_history_sources(site_id,source_key,source_address) VALUES (?,'',?)", site.ID, a.SourceAddress); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{a.SourceKey: "https://origin-a.example:443", b.SourceKey: "https://origin-b.example:443", otherMount.SourceKey: "https://origin-a.example:443", "": ""}
	for _, list := range []func(WatchHistoryFilter) ([]WatchHistoryEntry, error){database.ListWatchHistory, database.ListActiveWatchHistory} {
		entries, err := list(WatchHistoryFilter{SiteID: site.ID})
		if err != nil || len(entries) != 5 {
			t.Fatalf("source history count=%d err=%v, want 5", len(entries), err)
		}
		for _, entry := range entries {
			if entry.SourceAddress != want[entry.SourceKey] {
				t.Fatalf("wrong source address for item %q: %q", entry.UpstreamItemID, entry.SourceAddress)
			}
			parsed, err := url.Parse(entry.SourceAddress)
			if err != nil || parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
				t.Fatal("history API exposed an API mount, query or fragment")
			}
			if entry.SourceKey == "" {
				encoded, err := json.Marshal(entry)
				if err != nil || strings.Contains(string(encoded), "source_address") || strings.Contains(string(encoded), "source_key") {
					t.Fatal("fixed-upstream history gained source fields from an invalid empty-key mapping")
				}
			}
		}
	}
	newerOldAgent := a
	newerOldAgent.ObservedAtMS++
	newerOldAgent.SourceAddress = ""
	if skipped, err := database.writeWatchHistoryBatch([]watchHistoryEvent{newerOldAgent, a}); err != nil || skipped != 0 {
		t.Fatalf("merge old/new Agent events: skipped=%d err=%v", skipped, err)
	}
	if _, err := database.db.Exec("UPDATE watch_history_sources SET source_address=? WHERE site_id=? AND source_key=?", a.SourceAddress+"?api_key=synthetic-corruption", site.ID, a.SourceKey); err != nil {
		t.Fatal(err)
	}
	entries, err := database.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.SourceKey == a.SourceKey && entry.SourceAddress != "" {
			t.Fatal("history response exposed an invalid persisted source address")
		}
	}
}

func insertWatchHistorySourceAddressTestLog(t *testing.T, database *DB, siteID int64, category, backend, path string, status int, atMS int64) {
	t.Helper()
	_, err := database.db.Exec(`INSERT INTO request_logs
		(site_id,site_name,resource_category,status_code,client_ip,user_agent,method,path,backend_address,recorded_at_ms,timeline_at_ms)
		VALUES (?,'source-address',?,?,'127.0.0.1','','POST',?,?,?,?)`, siteID, category, status, path, backend, atMS, atMS)
	if err != nil {
		t.Fatal(err)
	}
}

func TestSchema58WatchHistorySourceAddressesBackfillOnlyExactAPISources(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	otherSite := createWatchHistoryTestSite(t, database, true)
	now := time.Now()
	a := watchHistorySourceAddressTestEvent(t, site.ID, "https://origin.example", "/emby", "old-a", now)
	b := watchHistorySourceAddressTestEvent(t, site.ID, "http://[fd00::2]:8096", "/tenant two/emby", "old-b", now)
	wrongBackend := watchHistorySourceAddressTestEvent(t, site.ID, "https://frontend.example", "/emby", "wrong-backend", now)
	wrongMount := watchHistorySourceAddressTestEvent(t, site.ID, "https://origin.example", "/other/emby", "wrong-mount", now)
	denied := watchHistorySourceAddressTestEvent(t, site.ID, "https://denied.example", "/emby", "denied", now)
	wrongSite := watchHistorySourceAddressTestEvent(t, site.ID, "https://wrong-site.example", "/emby", "wrong-site", now)
	events := []watchHistoryEvent{a, b, wrongBackend, wrongMount, denied, wrongSite}
	for index := range events {
		events[index].SourceAddress = ""
	}
	if skipped, err := database.writeWatchHistoryBatch(events); err != nil || skipped != 0 {
		t.Fatalf("write digest-only history: skipped=%d err=%v", skipped, err)
	}
	insertWatchHistorySourceAddressTestLog(t, database, site.ID, requestLogCategoryPlaybackSync, "https://synthetic-user:synthetic-pass@ORIGIN.example./ignored?api_key=synthetic-backend-token#synthetic-fragment", "/emby/Sessions/Playing/Progress?api_key=synthetic-path-token", 204, now.UnixMilli())
	insertWatchHistorySourceAddressTestLog(t, database, site.ID, requestLogCategoryMetadata, "http://[fd00::2]:8096", "/tenant%20two/emby/Items/old-b", 200, now.UnixMilli())
	insertWatchHistorySourceAddressTestLog(t, database, site.ID, requestLogCategoryPlaybackSync, "https://cdn.example:443", "/emby/Sessions/Playing/Progress", 204, now.UnixMilli())
	insertWatchHistorySourceAddressTestLog(t, database, site.ID, requestLogCategoryStream, "https://frontend.example:443", "/emby/Sessions/Playing/Progress", 200, now.UnixMilli())
	insertWatchHistorySourceAddressTestLog(t, database, site.ID, requestLogCategoryPlaybackSync, "https://denied.example:443", "/emby/Sessions/Playing/Progress", 403, now.UnixMilli())
	insertWatchHistorySourceAddressTestLog(t, database, otherSite.ID, requestLogCategoryPlaybackSync, "https://wrong-site.example:443", "/emby/Sessions/Playing/Progress", 204, now.UnixMilli())
	for _, statement := range []string{"DROP TABLE watch_history_sources", "PRAGMA user_version=57"} {
		if _, err := database.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.migrate(); err != nil {
		t.Fatalf("migrate source addresses: %v", err)
	}
	entries, err := database.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID})
	if err != nil || len(entries) != len(events) {
		t.Fatalf("migrated history count=%d err=%v", len(entries), err)
	}
	want := map[string]string{a.SourceKey: "https://origin.example:443", b.SourceKey: "http://[fd00::2]:8096"}
	for _, entry := range entries {
		if entry.SourceAddress != want[entry.SourceKey] {
			t.Fatalf("migration assigned an inexact source address to %q: %q", entry.UpstreamItemID, entry.SourceAddress)
		}
		if strings.Contains(entry.SourceAddress, "synthetic-") {
			t.Fatal("backfill persisted a credential or fragment")
		}
	}
	for _, event := range []watchHistoryEvent{a, b} {
		var storedAddress string
		if err := database.db.QueryRow("SELECT source_address FROM watch_history_sources WHERE site_id=? AND source_key=?", site.ID, event.SourceKey).Scan(&storedAddress); err != nil || storedAddress != event.SourceAddress {
			t.Fatalf("backfill lost the internal API mount: address=%q err=%v", storedAddress, err)
		}
	}
	if err := database.migrate(); err != nil {
		t.Fatalf("repeat source-address migration: %v", err)
	}
	var sourceCount, schemaVersion int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM watch_history_sources").Scan(&sourceCount); err != nil || sourceCount != 2 {
		t.Fatalf("backfilled source count=%d err=%v, want 2", sourceCount, err)
	}
	if err := database.db.QueryRow("PRAGMA user_version").Scan(&schemaVersion); err != nil || schemaVersion != databaseSchemaVersion {
		t.Fatalf("schema version=%d err=%v", schemaVersion, err)
	}
}

func TestSchema58WatchHistorySourceBackfillBoundsRetainedLogCandidates(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	now := time.Now()
	event := watchHistorySourceAddressTestEvent(t, site.ID, "https://old-origin.example", "/emby", "old", now)
	old := event
	old.SourceAddress = ""
	if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{old}); err != nil {
		t.Fatal(err)
	}
	insertWatchHistorySourceAddressTestLog(t, database, site.ID, requestLogCategoryPlaybackSync, "https://old-origin.example", "/emby/Sessions/Playing", 204, 1)
	_, err := database.db.Exec(`WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i<?)
		INSERT INTO request_logs (site_id,site_name,resource_category,status_code,client_ip,user_agent,method,path,backend_address,recorded_at_ms,timeline_at_ms)
		SELECT ?,'bounded-source','playback_sync',204,'127.0.0.1','','POST','/emby/Sessions/Playing','https://unrelated.example',i+1,i+1 FROM seq`, watchHistorySourceBackfillPerCategoryLimit, site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec("PRAGMA user_version=57"); err != nil {
		t.Fatal(err)
	}
	if err := database.migrate(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM watch_history_sources").Scan(&count); err != nil || count != 0 {
		t.Fatalf("migration inspected logs beyond its candidate limit: count=%d err=%v", count, err)
	}
	if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{event}); err != nil {
		t.Fatal(err)
	}
	entries, err := database.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID})
	if err != nil || len(entries) != 1 || entries[0].SourceAddress != "https://old-origin.example:443" {
		t.Fatalf("later playback did not fill the bounded backfill gap: count=%d err=%v", len(entries), err)
	}
}

func TestWatchHistorySourceAddressesExpireWithTheirHistory(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	otherSite := createWatchHistoryTestSite(t, database, true)
	now := time.Now()
	one := watchHistorySourceAddressTestEvent(t, site.ID, "https://origin.example", "/emby", "one", now)
	two := watchHistorySourceAddressTestEvent(t, site.ID, "https://origin.example", "/emby", "two", now)
	other := watchHistorySourceAddressTestEvent(t, otherSite.ID, "https://other.example", "/emby", "other", now)
	if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{one, two, other}); err != nil {
		t.Fatal(err)
	}
	entries, err := database.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID})
	if err != nil || len(entries) != 2 {
		t.Fatalf("history count=%d err=%v", len(entries), err)
	}
	if err := database.deleteWatchHistoryRow(entries[0].ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM watch_history_sources WHERE site_id=?", site.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("deleting one item removed a shared address: count=%d err=%v", count, err)
	}
	if err := database.clearWatchHistoryRows(site.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRow("SELECT COUNT(*) FROM watch_history_sources").Scan(&count); err != nil || count != 1 {
		t.Fatalf("site clear retained its address or removed another site's address: count=%d err=%v", count, err)
	}
	if err := database.clearWatchHistoryRows(0); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRow("SELECT COUNT(*) FROM watch_history_sources").Scan(&count); err != nil || count != 0 {
		t.Fatalf("global clear retained source addresses: count=%d err=%v", count, err)
	}
}

func TestWatchHistorySourceAddressesRemovedWithDeletedSite(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	otherSite := createWatchHistoryTestSite(t, database, true)
	now := time.Now()
	deleted := watchHistorySourceAddressTestEvent(t, site.ID, "https://deleted.example", "/emby", "deleted", now)
	retained := watchHistorySourceAddressTestEvent(t, otherSite.ID, "https://retained.example", "/emby", "retained", now)
	if _, err := database.writeWatchHistoryBatch([]watchHistoryEvent{deleted, retained}); err != nil {
		t.Fatal(err)
	}
	if err := database.DeleteSite(site.ID); err != nil {
		t.Fatal(err)
	}
	var deletedCount, retainedCount int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM watch_history_sources WHERE site_id=?", site.ID).Scan(&deletedCount); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRow("SELECT COUNT(*) FROM watch_history_sources WHERE site_id=?", otherSite.ID).Scan(&retainedCount); err != nil {
		t.Fatal(err)
	}
	if deletedCount != 0 || retainedCount != 1 {
		t.Fatalf("source addresses after deleting one site: deleted=%d retained=%d, want 0/1", deletedCount, retainedCount)
	}
}
