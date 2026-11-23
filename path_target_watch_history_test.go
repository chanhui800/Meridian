package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestWatchHistoryPathTargetSourceKeyScopesAuthorityAndAPIMount(t *testing.T) {
	key := func(authority, path string) string {
		t.Helper()
		target, err := url.Parse(authority)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request = request.WithContext(context.WithValue(request.Context(), pathTargetContextKey{}, target))
		return watchHistorySourceKeyFromRequest(request)
	}
	originKey := key("https://ORIGIN.example:443", "/emby/Items/item?api_key=synthetic-one")
	if len(originKey) != 64 || !isHexString(originKey) {
		t.Fatal("generic source key is not a bounded digest")
	}
	if got := key("https://origin.example", "/emby/Sessions/Playing/Progress?api_key=synthetic-two"); got != originKey {
		t.Fatal("source key changed with the API route, default port, hostname casing or query credential")
	}
	for _, other := range []string{
		key("https://other.example", "/emby/Sessions/Playing/Progress"),
		key("https://origin.example", "/other/emby/Sessions/Playing/Progress"),
		key("http://origin.example:443", "/emby/Sessions/Playing/Progress"),
	} {
		if other == originKey {
			t.Fatal("different authority or API mount reused a watch source key")
		}
	}
	for _, route := range []string{
		"/emby/Users/user/Items/item", "/emby/Users/user/Items", "/emby/Items/Resume",
		"/emby/Shows/show/Episodes", "/emby/Shows/NextUp", "/emby/Users/user/PlayingItems/item/Progress",
	} {
		if got := key("https://origin.example", route); got != originKey {
			t.Fatalf("API route %q changed the media server namespace", route)
		}
	}
	if got := watchHistorySourceKeyFromRequest(httptest.NewRequest(http.MethodGet, "/emby/Items/item", nil)); got != "" {
		t.Fatal("a fixed-upstream request gained a source namespace")
	}
}

func TestWatchHistorySourceNamespacedStorageKeepsOriginalIDAndFixedDigests(t *testing.T) {
	original := "opaque:item:id"
	sourceKey := strings.Repeat("a", 64)
	storage := watchHistoryStoredItemID(watchHistoryEvent{SourceKey: sourceKey, UpstreamItemID: original})
	decodedSource, decodedID := splitWatchHistoryStoredItemID(storage)
	if decodedSource != sourceKey || decodedID != original {
		t.Fatal("generic storage namespace did not round-trip the original item ID")
	}
	if got := watchHistoryStoredItemID(watchHistoryEvent{UpstreamItemID: original}); got != original {
		t.Fatal("fixed item ID changed in storage")
	}
	for _, malformed := range []string{original, watchHistorySourceItemPrefix + "invalid", watchHistorySourceItemPrefix + strings.Repeat("z", 64) + ":item"} {
		if source, item := splitWatchHistoryStoredItemID(malformed); source != "" || item != malformed {
			t.Fatal("ordinary or malformed storage ID was decoded as a generic source")
		}
	}
	observedAt := time.Unix(1000, 0)
	fixedDigest := watchHistorySessionDigest(1, "session", "viewer", original, observedAt)
	if fixedDigest != watchHistorySessionDigest(1, "session", "viewer", original, observedAt, "") {
		t.Fatal("fixed session digest changed with the optional source field")
	}
	if genericDigest := watchHistorySessionDigest(1, "session", "viewer", original, observedAt, sourceKey); genericDigest == fixedDigest {
		t.Fatal("generic session identity was not scoped to its source")
	}
}

func TestWatchHistorySourceKeySurvivesAgentEventJSONAndStaysOptional(t *testing.T) {
	event := watchHistoryTestEvent(7, "wire-session", time.Now().UnixMilli(), 100)
	event.SourceKey = strings.Repeat("a", 64)
	encoded, err := json.Marshal(NodeRequestEvent{SiteID: event.SiteID, WatchHistory: &event})
	if err != nil {
		t.Fatal(err)
	}
	var decoded NodeRequestEvent
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.WatchHistory == nil || decoded.WatchHistory.SourceKey != event.SourceKey || !validWatchHistoryEvent(*decoded.WatchHistory) {
		t.Fatal("the generic source key was lost or invalid after Agent event serialization")
	}
	event.SourceKey = ""
	fixedEncoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(fixedEncoded), "source_key") {
		t.Fatal("fixed Agent events acquired a new nonempty source field")
	}
	for _, invalid := range []string{"origin.example", strings.Repeat("a", 65), strings.Repeat("z", 64)} {
		event.SourceKey = invalid
		if validWatchHistoryEvent(event) {
			t.Fatal("watch history accepted a malformed source namespace")
		}
	}
}

func TestWatchHistoryFixedSourceKeepsExistingHistoryAPI(t *testing.T) {
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	event := watchHistoryTestEvent(site.ID, "fixed-source", time.Now().UnixMilli(), 100)
	if !database.EnqueueWatchHistory(event) {
		t.Fatal("fixed history event was not accepted")
	}
	for _, list := range []func(WatchHistoryFilter) ([]WatchHistoryEntry, error){database.ListWatchHistory, database.ListActiveWatchHistory} {
		entries, err := list(WatchHistoryFilter{SiteID: site.ID})
		if err != nil || len(entries) != 1 || entries[0].SourceKey != "" || entries[0].UpstreamItemID != event.UpstreamItemID {
			t.Fatalf("fixed history identity changed: count=%d err=%v", len(entries), err)
		}
		encoded, err := json.Marshal(entries)
		if err != nil || strings.Contains(string(encoded), "source_key") {
			t.Fatal("fixed history responses changed their wire fields")
		}
	}
}

func TestPathTargetWatchHistorySeparatesOriginsWithSameItemAndSession(t *testing.T) {
	app := newTestApp(t)
	newOrigin := func(title string) *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/emby/Items/shared-item":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"Id": "shared-item", "Name": title, "Type": "Movie", "RunTimeTicks": 10000})
			case "/emby/Sessions/Playing", "/emby/Sessions/Playing/Progress", "/emby/Sessions/Playing/Stopped":
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusNoContent)
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(server.Close)
		return server
	}
	originA, originB := newOrigin("Origin A Movie"), newOrigin("Origin B Movie")
	site, err := app.db.CreateSiteRecord(Site{
		Name: "generic watch history", ListenPort: freePort(t),
		PublicHost: "generic-watch.example.test", IngressMode: ingressModeHost,
		UpstreamMode: upstreamModePathTarget, PlaybackMode: "direct", StreamHosts: "[]",
		UAMode: passthroughUAMode, WatchHistoryEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !site.WatchHistoryEnabled {
		t.Fatal("generic site lost the enabled watch-history setting")
	}
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	handler, found := app.pm.PublicHostHandler(site.PublicHost)
	if !found || handler == nil {
		t.Fatal("generic watch-history host handler is missing")
	}
	request := func(origin *httptest.Server, path string, body []byte) {
		t.Helper()
		method := http.MethodGet
		if body != nil {
			method = http.MethodPost
		}
		req := httptest.NewRequest(method, "/"+origin.URL+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Emby-Authorization", `MediaBrowser Token="synthetic-watch-token", Client="TestPlayer", DeviceId="same-device"`)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		want := http.StatusNoContent
		if body == nil {
			want = http.StatusOK
		}
		if recorder.Code != want {
			t.Fatalf("generic %s status=%d, want %d", path, recorder.Code, want)
		}
	}
	request(originA, "/emby/Items/shared-item", nil)
	request(originB, "/emby/Items/shared-item", nil)
	start := []byte(`{"ItemId":"shared-item","PlaySessionId":"same-session","PositionTicks":100,"RunTimeTicks":10000}`)
	request(originA, "/emby/Sessions/Playing", start)
	request(originB, "/emby/Sessions/Playing", start)
	progress := []byte(`{"ItemId":"shared-item","PlaySessionId":"same-session","PositionTicks":3000,"RunTimeTicks":10000}`)
	request(originA, "/emby/Sessions/Playing/Progress", progress)
	history, err := app.db.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("same-id two-origin history count=%d, want 2", len(history))
	}
	byTitle := make(map[string]WatchHistoryEntry, len(history))
	sources := make(map[string]bool, len(history))
	for _, entry := range history {
		if entry.UpstreamItemID != "shared-item" || entry.PlaySessionID != "same-session" {
			t.Fatal("watch history exposed a storage namespace or changed the original identity")
		}
		byTitle[entry.Title] = entry
		if len(entry.SourceKey) != 64 || !isHexString(entry.SourceKey) {
			t.Fatal("generic history did not expose a bounded source identity for background grouping")
		}
		sources[entry.SourceKey] = true
	}
	if len(sources) != 2 {
		t.Fatal("different origins shared the history grouping identity")
	}
	if byTitle["Origin A Movie"].PositionTicks != 3000 || byTitle["Origin B Movie"].PositionTicks != 100 {
		t.Fatalf("source metadata/progress mixed: A=%d B=%d", byTitle["Origin A Movie"].PositionTicks, byTitle["Origin B Movie"].PositionTicks)
	}
	request(originA, "/emby/Sessions/Playing/Stopped", progress)
	history, err = app.db.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID})
	if err != nil || len(history) != 2 {
		t.Fatalf("history after stopping one origin count=%d err=%v", len(history), err)
	}
	for _, entry := range history {
		if entry.Title == "Origin A Movie" && (entry.ClientStopOutcome != "confirmed" || entry.StoppedAtMS == 0) {
			t.Fatalf("origin A stop was not confirmed: outcome=%s stopped=%d", entry.ClientStopOutcome, entry.StoppedAtMS)
		}
		if entry.Title == "Origin B Movie" && (entry.ClientStopOutcome != "" || entry.StoppedAtMS != 0) {
			t.Fatal("origin A stop changed origin B's playback")
		}
	}
	active, err := app.db.ListActiveWatchHistory(WatchHistoryFilter{SiteID: site.ID})
	if err != nil || len(active) != 1 || active[0].Title != "Origin B Movie" || active[0].SourceKey != byTitle["Origin B Movie"].SourceKey {
		t.Fatalf("remaining active playback count=%d err=%v", len(active), err)
	}
	encoded, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "synthetic-watch-token") {
		t.Fatal("history response exposed a credential")
	}
	wantAddresses := map[string]string{
		byTitle["Origin A Movie"].SourceKey: originA.URL,
		byTitle["Origin B Movie"].SourceKey: originB.URL,
	}
	withoutAddresses := append([]WatchHistoryEntry(nil), history...)
	for index, entry := range history {
		if entry.SourceAddress != wantAddresses[entry.SourceKey] {
			t.Fatalf("source address=%q, want %q", entry.SourceAddress, wantAddresses[entry.SourceKey])
		}
		withoutAddresses[index].SourceAddress = ""
	}
	encoded, err = json.Marshal(withoutAddresses)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), originA.URL) || strings.Contains(string(encoded), originB.URL) {
		t.Fatal("history response exposed an upstream URL outside the source address field")
	}
}

func TestPathTargetWatchHistorySeparatesAPIMountsAndSameNamedSeries(t *testing.T) {
	testPathTargetWatchHistorySeparateMounts(t, "/alpha/emby", "/beta/emby")
}

func TestPathTargetWatchHistoryMountCanContainAPIResourceNames(t *testing.T) {
	for _, resource := range []string{"items", "users", "sessions", "shows"} {
		t.Run(resource, func(t *testing.T) {
			testPathTargetWatchHistorySeparateMounts(t, "/services/"+resource+"/alpha/emby", "/services/"+resource+"/beta/emby")
		})
	}
}

func testPathTargetWatchHistorySeparateMounts(t *testing.T, mountA, mountB string) {
	t.Helper()
	app := newTestApp(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var mount, title string
		switch {
		case strings.HasPrefix(r.URL.Path, mountA+"/"):
			mount, title = mountA, "Alpha Episode"
		case strings.HasPrefix(r.URL.Path, mountB+"/"):
			mount, title = mountB, "Beta Episode"
		default:
			http.NotFound(w, r)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, mount)
		switch path {
		case "/Items/shared-item", "/Users/shared-user/Items/shared-item":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id": "shared-item", "Name": title, "Type": "Episode", "SeriesName": "Same Named Series",
				"ParentIndexNumber": 1, "IndexNumber": 1, "RunTimeTicks": 10000,
			})
		case "/Sessions/Playing/Progress", "/Sessions/Playing/Stopped":
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(origin.Close)
	site, err := app.db.CreateSiteRecord(Site{
		Name: "generic mounts", ListenPort: freePort(t),
		PublicHost: "generic-watch-mounts.example.test", IngressMode: ingressModeHost,
		UpstreamMode: upstreamModePathTarget, PlaybackMode: "direct", StreamHosts: "[]",
		UAMode: passthroughUAMode, WatchHistoryEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	handler, found := app.pm.PublicHostHandler(site.PublicHost)
	if !found || handler == nil {
		t.Fatal("generic mount handler is missing")
	}
	request := func(mount, path string, body []byte) {
		t.Helper()
		method := http.MethodGet
		if body != nil {
			method = http.MethodPost
		}
		target, _ := url.Parse(origin.URL)
		target.Path = mount + path
		req := httptest.NewRequest(method, pathTargetRouteURL(target), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Emby-Authorization", `MediaBrowser Token="synthetic-watch-token", Client="TestPlayer", DeviceId="same-device"`)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		want := http.StatusNoContent
		if body == nil {
			want = http.StatusOK
		}
		if recorder.Code != want {
			t.Fatalf("mounted request status=%d, want %d", recorder.Code, want)
		}
	}
	request(mountA, "/Users/shared-user/Items/shared-item", nil)
	request(mountB, "/Items/shared-item", nil)
	body := []byte(`{"ItemId":"shared-item","PlaySessionId":"same-session","PositionTicks":100,"RunTimeTicks":10000}`)
	request(mountA, "/Sessions/Playing/Progress", body)
	request(mountB, "/Sessions/Playing/Progress", body)
	history, err := app.db.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID})
	if err != nil || len(history) != 2 {
		t.Fatalf("same named series across API mounts history count=%d err=%v", len(history), err)
	}
	for _, entry := range history {
		if entry.UpstreamItemID != "shared-item" || entry.SeriesName != "Same Named Series" {
			t.Fatal("mounted metadata or displayed item identity was changed")
		}
	}
	if history[0].SourceKey == "" || history[0].SourceKey == history[1].SourceKey {
		t.Fatal("API mount history grouping did not preserve separate source identities")
	}
	request(mountA, "/Sessions/Playing/Stopped", body)
	if _, err := app.db.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID}); err != nil {
		t.Fatal(err)
	}
	active, err := app.db.ListActiveWatchHistory(WatchHistoryFilter{SiteID: site.ID})
	if err != nil || len(active) != 1 || active[0].Title != "Beta Episode" {
		t.Fatalf("stopping one API mount affected the other: active=%d err=%v", len(active), err)
	}
}
