package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const pathTargetNodeTestVersion = "v1.11.10-pre.20261123.3"

type pathTargetNodeFixture struct {
	app      *App
	node     ControlNode
	token    string
	now      time.Time
	generic  Site
	fixed    Site
	upstream *httptest.Server
}

func newPathTargetNodeFixture(t *testing.T, version string) *pathTargetNodeFixture {
	t.Helper()
	fixture := &pathTargetNodeFixture{app: newTestApp(t), now: time.Now()}
	fixture.app.dynamicRouteKey = bytes.Repeat([]byte{0x71}, 32)
	fixture.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fixed-ping" {
			_, _ = io.WriteString(w, "fixed-ok")
			return
		}
		if r.URL.Path != "/media/movie.mkv" || r.URL.Query().Get("marker") != "source" || r.Header.Get("Range") != "bytes=0-3" {
			http.Error(w, "invalid synthetic media request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "video/x-matroska")
		w.Header().Set("Content-Range", "bytes 0-3/8")
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "abcd")
	}))
	t.Cleanup(fixture.upstream.Close)
	node, enrollment, err := fixture.app.db.CreateControlNode(NodeCreateInput{
		Name: "path-target-node", Address: "203.0.113.50", Port: 19090, Priority: 20,
	}, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	fixture.node, fixture.token, err = fixture.app.db.EnrollControlNode(enrollment, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	installReviewCertificate(t, fixture.app)
	installReviewCertificateForNode(t, fixture.app, node.GUID)
	if _, err := fixture.app.db.RecordNodeReport(fixture.token, NodeReport{
		BootID: "path-target-bootstrap", ReportSessionID: "path-target-bootstrap", CounterEpoch: "synthetic:eth0",
		Sequence: 1, InterfaceName: "eth0", AgentVersion: version,
	}, fixture.now); err != nil {
		t.Fatal(err)
	}
	fixed, err := fixture.app.db.CreateSiteRecord(Site{
		Name: "fixed-node-site", ListenPort: freePort(t), PublicHost: "fixed-node.example.test", IngressMode: ingressModeHost,
		TargetURL: fixture.upstream.URL, PlaybackMode: "direct", StreamHosts: "[]", UAMode: passthroughUAMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.fixed = *fixed
	generic, err := fixture.app.db.CreateSiteRecord(Site{
		Name: "generic-node-site", ListenPort: freePort(t), PublicHost: "generic-node.example.test", IngressMode: ingressModeHost,
		UpstreamMode: upstreamModePathTarget, PlaybackMode: "direct", StreamHosts: "[]", UAMode: passthroughUAMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.generic = *generic
	for _, site := range []Site{fixture.fixed, fixture.generic} {
		if _, err := fixture.app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", fixture.node.ID, fixture.now); err != nil {
			t.Fatalf("schedule %s: %v", site.Name, err)
		}
	}
	return fixture
}

func (fixture *pathTargetNodeFixture) httpConfig(t *testing.T, version string) AgentRuntimeConfig {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://controller.example.test/api/agent/config", nil)
	request.Header.Set("Authorization", "Bearer "+fixture.token)
	request.Header.Set(agentVersionHeader, version)
	recorder := httptest.NewRecorder()
	fixture.app.handleAgentConfig(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("agent config HTTP status=%d", recorder.Code)
	}
	var config AgentRuntimeConfig
	if err := json.NewDecoder(recorder.Body).Decode(&config); err != nil {
		t.Fatal(err)
	}
	return config
}

type pathTargetNodeReportAck struct {
	AcceptedEventIDs   []int64  `json:"accepted_event_ids"`
	AcceptedEventUIDs  []string `json:"accepted_event_uids"`
	DiscardedEventIDs  []int64  `json:"discarded_event_ids"`
	DiscardedEventUIDs []string `json:"discarded_event_uids"`
}

func (fixture *pathTargetNodeFixture) httpReport(t *testing.T, report NodeReport) pathTargetNodeReportAck {
	t.Helper()
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://controller.example.test/api/agent/report", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+fixture.token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	fixture.app.handleAgentReport(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("agent report HTTP status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var result pathTargetNodeReportAck
	if err := json.NewDecoder(recorder.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPathTargetNodeScheduledFixedSiteRequiresAppliedAgentUpgradeBeforeModeChange(t *testing.T) {
	const oldVersion = "v1.11.9-pre.20261123.7"
	fixture := newPathTargetNodeFixture(t, oldVersion)
	if _, err := fixture.app.db.SaveSiteNodeSchedule(fixture.generic.ID, false, "global", 0, fixture.now); err != nil {
		t.Fatal(err)
	}
	port := freePort(t)
	var err error
	fixture.node, err = fixture.app.db.UpdateControlNode(fixture.node.ID, NodeCreateInput{
		Name: fixture.node.Name, Address: fixture.node.Address, Port: port, Priority: fixture.node.Priority,
		TrafficQuota: fixture.node.TrafficQuota, BillingMode: fixture.node.BillingMode, ResetDay: fixture.node.ResetDay,
		TrafficManualOffsetBytes: fixture.node.TrafficManualOffset,
	}, true, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	oldConfig := fixture.httpConfig(t, oldVersion)
	if len(oldConfig.Routes) != 1 || oldConfig.Routes[0].SiteID != fixture.fixed.ID || siteUsesPathTarget(oldConfig.Routes[0].Site) {
		t.Fatal("old Agent did not receive the scheduled fixed route")
	}
	runtime := &edgeAgentRuntime{stateDir: t.TempDir()}
	if err := runtime.events.initWithRawKey(runtime.stateDir, bytes.Repeat([]byte{0x73}, 32), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.close() })
	releasePort(port)
	if err := runtime.apply(oldConfig); err != nil {
		t.Fatalf("apply old fixed config: %v", err)
	}
	report := NodeReport{
		BootID: "path-target-bootstrap", ReportSessionID: "path-target-bootstrap", CounterEpoch: "synthetic:eth0",
		Sequence: 2, InterfaceName: "eth0", AgentVersion: oldVersion,
		AppliedConfigHash: oldConfig.ConfigHash, AppliedConfigRevision: oldConfig.ConfigRevision,
	}
	fixture.httpReport(t, report)
	if _, err := fixture.app.db.db.Exec(`UPDATE site_node_schedules SET applied_node_id=?,applied_address=?,dns_status='active' WHERE site_id=?`, fixture.node.ID, fixture.node.Address, fixture.fixed.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.pm.StartSite(fixture.fixed); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.app.pm.StopSite(fixture.fixed.ID) })
	beforeSite, err := fixture.app.db.GetSite(fixture.fixed.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeSchedule, err := fixture.app.db.siteNodeSchedule(fixture.fixed.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeNode, err := fixture.app.db.controlNodeByID(fixture.node.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if beforeSchedule.AppliedNodeID != fixture.node.ID || beforeSchedule.DNSStatus != "active" || !siteScheduleConfigReady(beforeSchedule, beforeNode) || beforeNode.ConfigDirty {
		t.Fatal("fixed-site fixture was not applied and ready before the mode change")
	}
	beforeRequests, beforeStartedAt, beforeRunning, beforePortListening := fixture.app.pm.GetSiteRuntime(fixture.fixed.ID)
	if !beforeRunning {
		t.Fatal("fixed-site controller runtime was not running before the mode change")
	}
	body, err := json.Marshal(map[string]any{"name": fixture.fixed.Name, "upstream_mode": upstreamModePathTarget})
	if err != nil {
		t.Fatal(err)
	}
	put := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		fixture.app.handleSiteByID(response, httptest.NewRequest(http.MethodPut, "/api/sites/"+jsonNumber64(fixture.fixed.ID), bytes.NewReader(body)))
		return response
	}
	rejected := put()
	if rejected.Code != http.StatusConflict || !strings.Contains(rejected.Body.String(), pathTargetAgentMinimumVersion) {
		t.Fatalf("old applied Agent mode-change status=%d body=%s, want upgrade conflict", rejected.Code, rejected.Body.String())
	}
	afterSite, err := fixture.app.db.GetSite(fixture.fixed.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterSchedule, err := fixture.app.db.siteNodeSchedule(fixture.fixed.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterNode, err := fixture.app.db.controlNodeByID(fixture.node.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeSite, afterSite) || beforeSchedule != afterSchedule || !reflect.DeepEqual(beforeNode, afterNode) {
		t.Fatal("rejected generic mode change mutated the fixed site, schedule or Agent config state")
	}
	afterRequests, afterStartedAt, afterRunning, afterPortListening := fixture.app.pm.GetSiteRuntime(fixture.fixed.ID)
	if afterRequests != beforeRequests || !afterStartedAt.Equal(beforeStartedAt) || afterRunning != beforeRunning || afterPortListening != beforePortListening {
		t.Fatal("rejected generic mode change replaced or stopped the controller runtime")
	}
	assertFixedRoute := func(handler http.Handler) {
		t.Helper()
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://"+fixture.fixed.PublicHost+"/fixed-ping", nil))
		if response.Code != http.StatusOK || response.Body.String() != "fixed-ok" {
			t.Fatalf("fixed route after rejection status/body=%d/%q", response.Code, response.Body.String())
		}
	}
	controllerHandler, found := fixture.app.pm.PublicHostHandler(fixture.fixed.PublicHost)
	if !found {
		t.Fatal("rejected generic mode change removed the fixed controller host route")
	}
	assertFixedRoute(controllerHandler)
	assertFixedRoute(runtime)
	if hash, revision, listenerErr, applyErr, _, _ := runtime.status(); hash != oldConfig.ConfigHash || revision != oldConfig.ConfigRevision || listenerErr != "" || applyErr != "" {
		t.Fatal("rejected generic mode change altered the applied fixed Agent runtime")
	}
	report.Sequence++
	report.AgentVersion = pathTargetNodeTestVersion
	fixture.httpReport(t, report)
	upgradedNode, err := fixture.app.db.controlNodeByID(fixture.node.ID, time.Now())
	if err != nil || upgradedNode.AgentVersion != pathTargetNodeTestVersion {
		t.Fatalf("Agent upgrade report was not recorded: err=%v version=%q", err, upgradedNode.AgentVersion)
	}
	accepted := put()
	if accepted.Code != http.StatusOK {
		t.Fatalf("upgraded applied Agent mode-change status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	updatedSite, err := fixture.app.db.GetSite(fixture.fixed.ID)
	if err != nil || !siteUsesPathTarget(*updatedSite) || updatedSite.TargetURL != "" {
		t.Fatalf("successful mode change did not persist the generic site: err=%v", err)
	}
	updatedSchedule, err := fixture.app.db.siteNodeSchedule(fixture.fixed.ID)
	if err != nil {
		t.Fatal(err)
	}
	updatedNode, err := fixture.app.db.controlNodeByID(fixture.node.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if updatedSchedule.ConfigHash != "" || updatedSchedule.ConfigPendingSinceMS <= 0 || updatedSchedule.ScheduleRevision != beforeSchedule.ScheduleRevision+1 || updatedSchedule.AppliedNodeID != fixture.node.ID || updatedSchedule.DesiredNodeID != fixture.node.ID {
		t.Fatal("successful generic mode change did not invalidate the existing schedule for re-application")
	}
	if !updatedNode.ConfigDirty || updatedNode.DesiredConfigHash != "" || updatedNode.DesiredConfigRevision != 0 || updatedNode.ConfigRevision != upgradedNode.ConfigRevision+1 {
		t.Fatal("successful generic mode change did not invalidate the assigned Agent config")
	}
	newConfig := fixture.httpConfig(t, pathTargetNodeTestVersion)
	if len(newConfig.Routes) != 1 || newConfig.Routes[0].SiteID != fixture.fixed.ID || !siteUsesPathTarget(newConfig.Routes[0].Site) || newConfig.Routes[0].TargetURL != "" || newConfig.ConfigHash == oldConfig.ConfigHash {
		t.Fatal("upgraded Agent did not receive the replacement generic route")
	}
	if err := runtime.apply(newConfig); err != nil {
		t.Fatalf("apply replacement generic config: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://"+fixture.fixed.PublicHost+"/"+fixture.upstream.URL+"/media/movie.mkv?marker=source", nil)
	request.Header.Set("Range", "bytes=0-3")
	response := httptest.NewRecorder()
	runtime.ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent || response.Body.String() != "abcd" || response.Header().Get("Content-Range") != "bytes 0-3/8" {
		t.Fatalf("replacement Agent generic Range status/body=%d/%q", response.Code, response.Body.String())
	}
	report.Sequence++
	report.AppliedConfigHash, report.AppliedConfigRevision = newConfig.ConfigHash, newConfig.ConfigRevision
	fixture.httpReport(t, report)
	readySchedule, err := fixture.app.db.siteNodeSchedule(fixture.fixed.ID)
	if err != nil {
		t.Fatal(err)
	}
	readyNode, err := fixture.app.db.controlNodeByID(fixture.node.ID, time.Now())
	if err != nil || readyNode.ConfigDirty || !siteScheduleConfigReady(readySchedule, readyNode) {
		t.Fatalf("replacement generic Agent config never became ready after apply acknowledgement: err=%v", err)
	}
}

func TestPathTargetNodeControllerConfigAndAgentRangeEndToEnd(t *testing.T) {
	fixture := newPathTargetNodeFixture(t, pathTargetNodeTestVersion)
	config := fixture.httpConfig(t, pathTargetNodeTestVersion)
	if len(config.Routes) != 2 {
		t.Fatalf("mixed agent route count=%d, want 2", len(config.Routes))
	}
	if err := validateAgentConfigEnvelope(config); err != nil {
		t.Fatalf("Agent rejected controller config: %v", err)
	}
	var genericRoute, fixedRoute *AgentSiteRoute
	for index := range config.Routes {
		route := &config.Routes[index]
		if route.SiteID == fixture.generic.ID {
			genericRoute = route
		} else if route.SiteID == fixture.fixed.ID {
			fixedRoute = route
		}
	}
	if genericRoute == nil || fixedRoute == nil || !siteUsesPathTarget(genericRoute.Site) || genericRoute.TargetURL != "" || genericRoute.Site.TargetURL != "" || len(genericRoute.Headers) != 0 {
		t.Fatal("controller did not preserve the generic route without fixed targets")
	}
	if siteUsesPathTarget(fixedRoute.Site) || fixedRoute.TargetURL != fixture.upstream.URL {
		t.Fatal("fixed route changed in mixed generic config")
	}
	encoded, err := json.Marshal(fixedRoute)
	if err != nil || strings.Contains(string(encoded), "upstream_mode") {
		t.Fatal("fixed route must retain the existing Agent wire shape")
	}
	configBytes, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var decoded AgentRuntimeConfig
	if err := json.Unmarshal(configBytes, &decoded); err != nil {
		t.Fatal(err)
	}
	runtime := &edgeAgentRuntime{stateDir: t.TempDir()}
	if err := runtime.events.initWithRawKey(runtime.stateDir, bytes.Repeat([]byte{0x72}, 32), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.events.close() })
	bundle, err := buildEdgeProxy(decoded, runtime)
	if err != nil {
		t.Fatalf("Agent failed to build mixed routes: %v", err)
	}
	t.Cleanup(bundle.close)
	gateway := httptest.NewServer(bundle.handler)
	t.Cleanup(gateway.Close)
	for _, route := range []string{
		"/" + fixture.upstream.URL + "/media/movie.mkv?marker=source",
		pathTargetRoutePrefix + "http/" + strings.TrimPrefix(fixture.upstream.URL, "http://") + "/media/movie.mkv?marker=source",
	} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, gateway.URL+route, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = fixture.generic.PublicHost
		request.Header.Set("Range", "bytes=0-3")
		request.Header.Set("User-Agent", "SyntheticNodePlayer/1")
		response, err := gateway.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusPartialContent || string(body) != "abcd" || response.Header.Get("Content-Range") != "bytes 0-3/8" {
			t.Fatalf("Agent generic Range status/body=%d/%q err=%v", response.StatusCode, body, readErr)
		}
	}
	fixedRequest := httptest.NewRequest(http.MethodGet, "http://"+fixture.fixed.PublicHost+"/fixed-ping", nil)
	fixedResponse := httptest.NewRecorder()
	bundle.handler.ServeHTTP(fixedResponse, fixedRequest)
	if fixedResponse.Code != http.StatusOK || fixedResponse.Body.String() != "fixed-ok" {
		t.Fatalf("Agent fixed route status/body=%d/%q", fixedResponse.Code, fixedResponse.Body.String())
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		// Ordinary request logs are batched by the journal for up to two
		// seconds. Force a durability barrier instead of making this fixture
		// wait on that timer before inspecting the reportable event snapshot.
		if err := runtime.events.flush(); err != nil {
			t.Fatalf("persist Agent generic request logs: %v", err)
		}
		logs := runtime.events.snapshot()
		genericLogs := 0
		for _, event := range logs {
			if event.SiteID != fixture.generic.ID || event.SkipRequestLog {
				continue
			}
			if event.StatusCode != http.StatusPartialContent || event.Path != "/media/movie.mkv" || event.Query != "" || event.BackendAddress != fixture.upstream.URL {
				t.Fatal("Agent generic log did not identify its central site, path and actual upstream")
			}
			genericLogs++
		}
		if genericLogs == 2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("Agent generic range requests were not recorded in the central event spool")
}

func TestPathTargetNodePlaybackHistoryEventsSeparateSourcesEndToEnd(t *testing.T) {
	fixture := newPathTargetNodeFixture(t, pathTargetNodeTestVersion)
	fixture.generic.WatchHistoryEnabled = true
	if err := fixture.app.db.UpdateSiteRecord(fixture.generic); err != nil {
		t.Fatal(err)
	}
	newOrigin := func() *httptest.Server {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
			if request.Method != http.MethodPost || request.URL.Path != "/emby/Sessions/Playing/Progress" {
				http.NotFound(w, request)
				return
			}
			_, _ = io.Copy(io.Discard, request.Body)
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(server.Close)
		return server
	}
	originA, originB := newOrigin(), newOrigin()
	config := fixture.httpConfig(t, pathTargetNodeTestVersion)
	var genericRoute *AgentSiteRoute
	for index := range config.Routes {
		if config.Routes[index].SiteID == fixture.generic.ID {
			genericRoute = &config.Routes[index]
		}
	}
	if genericRoute == nil || !genericRoute.Site.WatchHistoryEnabled || !siteUsesPathTarget(genericRoute.Site) {
		t.Fatal("controller config lost the generic route's watch-history setting")
	}
	runtime := &edgeAgentRuntime{stateDir: t.TempDir()}
	if err := runtime.events.initWithRawKey(runtime.stateDir, bytes.Repeat([]byte{0x74}, 32), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.events.close() })
	bundle, err := buildEdgeProxy(config, runtime)
	if err != nil {
		t.Fatalf("build generic Agent history route: %v", err)
	}
	t.Cleanup(bundle.close)
	gateway := httptest.NewServer(bundle.handler)
	t.Cleanup(gateway.Close)
	progress := func(origin *httptest.Server, title string, position int64) {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"ItemId": "shared-item", "PlaySessionId": "same-session", "UserId": "same-viewer",
			"PositionTicks": position, "RunTimeTicks": 10000,
			"Item": map[string]any{"Id": "shared-item", "Name": title, "Type": "Movie"},
		})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, gateway.URL+"/"+origin.URL+"/emby/Sessions/Playing/Progress", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Host = fixture.generic.PublicHost
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Emby-Authorization", `MediaBrowser Token="synthetic-node-history-token", Client="TestPlayer", DeviceId="same-device"`)
		response, err := gateway.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			t.Fatalf("Agent playback progress status=%d, want 204", response.StatusCode)
		}
	}
	progress(originA, "Origin A Movie", 100)
	progress(originB, "Origin B Movie", 500)
	progress(originA, "Origin A Movie", 3000)
	if err := runtime.events.flush(); err != nil {
		t.Fatalf("persist Agent playback events: %v", err)
	}
	var historyEvents []NodeRequestEvent
	sourceKeys := make(map[string]string)
	for _, event := range runtime.events.snapshot() {
		if event.WatchHistory == nil {
			continue
		}
		history := event.WatchHistory
		if event.SiteID != fixture.generic.ID || history.SiteID != fixture.generic.ID || event.Host != fixture.generic.PublicHost || event.Path != "/Sessions/Playing/Progress" || event.StatusCode != http.StatusNoContent || !event.SkipRequestLog || event.Priority != nodeEventPriorityCritical {
			t.Fatal("Agent playback event lost its central site or structured event identity")
		}
		if len(history.SourceKey) != sha256.Size*2 || !isHexString(history.SourceKey) || history.UpstreamItemID != "shared-item" || history.PlaySessionID != "same-session" || history.TokenCiphertext != "" {
			t.Fatal("Agent playback event lost its source namespace or forwarded a local token")
		}
		if previous := sourceKeys[history.Title]; previous != "" && previous != history.SourceKey {
			t.Fatal("same-origin Agent playback progress changed its source namespace")
		}
		sourceKeys[history.Title] = history.SourceKey
		historyEvents = append(historyEvents, event)
	}
	if len(historyEvents) != 3 || len(sourceKeys) != 2 || sourceKeys["Origin A Movie"] == sourceKeys["Origin B Movie"] {
		t.Fatalf("Agent two-source playback events/namespaces=%d/%d, want 3/2 distinct", len(historyEvents), len(sourceKeys))
	}
	report := NodeReport{
		BootID: "path-target-bootstrap", ReportSessionID: "path-target-bootstrap", CounterEpoch: "synthetic:eth0",
		Sequence: 2, InterfaceName: "eth0", AgentVersion: pathTargetNodeTestVersion,
		AppliedConfigHash: config.ConfigHash, AppliedConfigRevision: config.ConfigRevision, Events: historyEvents,
	}
	encoded, err := json.Marshal(report)
	if err != nil || !bytes.Contains(encoded, []byte(`"source_key"`)) || bytes.Contains(encoded, []byte("synthetic-node-history-token")) {
		t.Fatalf("Agent report wire payload lost source keys or exposed a token: err=%v", err)
	}
	ack := fixture.httpReport(t, report)
	if len(ack.AcceptedEventIDs) != len(historyEvents) || len(ack.AcceptedEventUIDs) != len(historyEvents) || len(ack.DiscardedEventIDs) != 0 || len(ack.DiscardedEventUIDs) != 0 {
		t.Fatalf("controller did not accept every source-scoped Agent event: accepted=%d/%d discarded=%d/%d", len(ack.AcceptedEventIDs), len(ack.AcceptedEventUIDs), len(ack.DiscardedEventIDs), len(ack.DiscardedEventUIDs))
	}
	history, err := fixture.app.db.ListWatchHistory(WatchHistoryFilter{SiteID: fixture.generic.ID, Limit: 10})
	if err != nil || len(history) != 2 {
		t.Fatalf("controller two-source history count=%d, want 2: err=%v", len(history), err)
	}
	byTitle := make(map[string]WatchHistoryEntry)
	for _, entry := range history {
		if entry.UpstreamItemID != "shared-item" || entry.PlaySessionID != "same-session" || entry.SourceKey != sourceKeys[entry.Title] {
			t.Fatal("controller history lost the original playback identity or Agent source namespace")
		}
		byTitle[entry.Title] = entry
	}
	if byTitle["Origin A Movie"].PositionTicks != 3000 || byTitle["Origin B Movie"].PositionTicks != 500 {
		t.Fatalf("Agent source metadata/progress merged in the controller: A=%d B=%d", byTitle["Origin A Movie"].PositionTicks, byTitle["Origin B Movie"].PositionTicks)
	}
	var sessions int
	if err := fixture.app.db.db.QueryRow("SELECT COUNT(*) FROM watch_sessions WHERE site_id=?", fixture.generic.ID).Scan(&sessions); err != nil || sessions != 2 {
		t.Fatalf("same-session two-source controller storage count=%d, want 2: err=%v", sessions, err)
	}
}

func TestPathTargetNodeOldAgentConfigRemainsUpgradeable(t *testing.T) {
	fixture := newPathTargetNodeFixture(t, "v1.11.9-pre.20261123.7")
	previousVersion := appVersion
	appVersion = pathTargetNodeTestVersion
	t.Cleanup(func() { appVersion = previousVersion })
	agentPath := filepath.Join(t.TempDir(), "synthetic-agent")
	agentBytes := []byte("synthetic Agent update bytes")
	if err := os.WriteFile(agentPath, agentBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MERIDIAN_AGENT_BINARY_LINUX_AMD64", agentPath)
	config, err := fixture.app.buildAgentConfigForRequest(context.Background(), fixture.token, fixture.now, "linux/amd64", "v1.11.9-pre.20261123.7")
	if err != nil {
		t.Fatalf("old Agent could not fetch its update config: %v", err)
	}
	if len(config.Routes) != 1 || config.Routes[0].SiteID != fixture.fixed.ID || siteUsesPathTarget(config.Routes[0].Site) {
		t.Fatal("unsupported Agent received a generic route instead of its compatible fixed config")
	}
	if err := validateAgentConfigEnvelope(config); err != nil {
		t.Fatalf("compatible old Agent config cannot reach its update path: %v", err)
	}
	digest := sha256.Sum256(agentBytes)
	if config.AgentVersion != pathTargetNodeTestVersion || config.AgentSHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("withholding a generic route also withheld the co-built Agent update metadata")
	}
	schedule, err := fixture.app.db.siteNodeSchedule(fixture.generic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.ConfigHash != "" || schedule.DesiredNodeID != fixture.node.ID || !strings.Contains(schedule.LastError, "Agent") {
		t.Fatal("old Agent's excluded generic route was published as ready")
	}
	probeCalls := 0
	err = fixture.app.reconcileOneSiteScheduleWithProbeLocked(t.Context(), schedule, fixture.now, func(context.Context, ControlNode, string, []byte) error {
		probeCalls++
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "Agent") || probeCalls != 0 {
		t.Fatalf("old Agent passed generic cutover readiness: err=%v probes=%d", err, probeCalls)
	}
}

func TestPathTargetNodeModeChangesHash(t *testing.T) {
	config := AgentRuntimeConfig{SchemaVersion: 1, NodeGUID: "synthetic-hash-node", HTTPSPort: 9090, Routes: []AgentSiteRoute{{
		SiteID: 1, Host: "generic.example.test", Site: Site{UpstreamMode: upstreamModePathTarget},
	}}}
	genericHash, err := agentConfigHash(config)
	if err != nil {
		t.Fatal(err)
	}
	config.Routes[0].Site.UpstreamMode = ""
	fixedHash, err := agentConfigHash(config)
	if err != nil {
		t.Fatal(err)
	}
	if genericHash == fixedHash {
		t.Fatal("Agent runtime hash does not distinguish fixed and generic proxy behavior")
	}
}

func TestPathTargetNodeVersionCompatibility(t *testing.T) {
	for _, test := range []struct {
		version string
		want    bool
	}{
		{"", false}, {"dev", false}, {"v1.11.8", false},
		{"v1.11.9-pre.20261123.7", false}, {"v1.11.9-pre.20261122.20", false},
		{"v1.11.9-unknown.1", false}, {"v1.11.9-pre.20261123.bad", false},
		{"v1.11.9", false}, {"v1.11.10-pre.20261123.2", false}, {"v1.11.10-pre.20261122.30", false},
		{"v1.11.10-unknown.1", false}, {"v1.11.10-pre.20261123.bad", false},
		{pathTargetNodeTestVersion, true}, {"v1.11.10-pre.20261124.1", true},
		{"v1.11.10", true}, {"v1.12.0", true}, {"v2.0.0", true},
		{"v1.11.10-pre.20261123.3+build.1", true},
	} {
		t.Run(test.version, func(t *testing.T) {
			if got := agentSupportsPathTargetUpstream(test.version); got != test.want {
				t.Fatalf("generic support for %q=%t, want %t", test.version, got, test.want)
			}
		})
	}
}

func TestPathTargetNodeAutoSchedulingPrefersCompatibleAgent(t *testing.T) {
	fixture := newPathTargetNodeFixture(t, "v1.11.9-pre.20261123.7")
	compatible, enrollment, err := fixture.app.db.CreateControlNode(NodeCreateInput{
		Name: "compatible-low-priority", Address: "203.0.113.51", Port: 19091, Priority: 10,
	}, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := fixture.app.db.EnrollControlNode(enrollment, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.db.RecordNodeReport(token, NodeReport{
		BootID: "compatible-bootstrap", ReportSessionID: "compatible-bootstrap", CounterEpoch: "synthetic:eth0",
		Sequence: 1, InterfaceName: "eth0", AgentVersion: pathTargetNodeTestVersion,
	}, fixture.now); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.db.SaveSiteNodeSchedule(fixture.generic.ID, true, "global", 0, fixture.now); err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.refreshSiteAssignments(fixture.now); err != nil {
		t.Fatal(err)
	}
	schedule, err := fixture.app.db.siteNodeSchedule(fixture.generic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.DesiredNodeID != compatible.ID {
		t.Fatalf("generic auto assignment=%d, want compatible node %d", schedule.DesiredNodeID, compatible.ID)
	}
}

func TestPathTargetNodeUnknownAgentGetsCompatibleEmptyConfig(t *testing.T) {
	fixture := newPathTargetNodeFixture(t, "")
	if _, err := fixture.app.db.SaveSiteNodeSchedule(fixture.fixed.ID, false, "global", 0, fixture.now); err != nil {
		t.Fatal(err)
	}
	config := fixture.httpConfig(t, "")
	if len(config.Routes) != 0 || config.ConfigHash == "" || config.SchemaVersion != agentConfigSchemaVersion {
		t.Fatal("unknown Agent must receive a valid empty config before its upgrade")
	}
	wantHash, err := agentConfigHashForVersion(config, "")
	if err != nil || config.ConfigHash != wantHash {
		t.Fatalf("unknown Agent compatibility hash mismatch: err=%v", err)
	}
	schedule, err := fixture.app.db.siteNodeSchedule(fixture.generic.ID)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.ConfigHash != "" || !strings.Contains(schedule.LastError, "Agent") {
		t.Fatal("an unrecognized Agent version was declared ready for generic proxying")
	}
}
