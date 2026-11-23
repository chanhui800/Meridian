package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type stopCancelResponseWriter struct {
	http.ResponseWriter
	cancel context.CancelFunc
}

func (writer *stopCancelResponseWriter) WriteHeader(status int) {
	if status >= 200 {
		writer.cancel()
	}
	writer.ResponseWriter.WriteHeader(status)
}

func stopTestHostController(t *testing.T, target string) (*App, *Site, http.Handler) {
	t.Helper()
	app := newTestApp(t)
	site, err := app.db.CreateSiteRecord(Site{
		Name: "stop-confirm-controller", PublicHost: "stop-confirm.example.test", IngressMode: ingressModeHost,
		TargetURL: target, PlaybackMode: "direct", StreamHosts: "[]", UAMode: passthroughUAMode, WatchHistoryEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	handler, ok := app.pm.PublicHostHandler(site.PublicHost)
	if !ok || handler == nil {
		t.Fatal("controller proxy handler unavailable")
	}
	return app, site, handler
}

func stopTestRequest(t *testing.T, ctx context.Context, host, path, token string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "https://"+host+path, strings.NewReader(`{"ItemId":"item-1","PlaySessionId":"session-1","UserId":"user-a","PositionTicks":100,"RunTimeTicks":1000}`)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Emby-Authorization", fmt.Sprintf(`MediaBrowser Token="%s", Client="Origami", DeviceId="phone-a"`, token))
	return request.WithContext(context.WithValue(request.Context(), publicHostIngressContextKey{}, true))
}

func waitStopTestOutcome(t *testing.T, database *DB, siteID int64, outcome string) WatchHistoryEntry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, err := database.ListWatchHistory(WatchHistoryFilter{SiteID: siteID})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 1 && entries[0].ClientStopOutcome == outcome {
			return entries[0]
		}
		if time.Now().After(deadline) {
			t.Fatalf("stop outcome never became %q: %+v", outcome, entries)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestProxyStopIntentActual204SurvivesClientCancellation(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	app, site, handler := stopTestHostController(t, upstream.URL)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, stopTestRequest(t, t.Context(), site.PublicHost, "/Sessions/Playing/Progress", "confirm-secret"))
	if response.Code != http.StatusNoContent {
		t.Fatalf("initial playback status=%d", response.Code)
	}
	if entries, err := app.db.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID}); err != nil || len(entries) != 1 {
		t.Fatalf("initial playback count=%d err=%v", len(entries), err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	response = httptest.NewRecorder()
	handler.ServeHTTP(&stopCancelResponseWriter{ResponseWriter: response, cancel: cancel}, stopTestRequest(t, ctx, site.PublicHost, "/Sessions/Playing/Stopped", "confirm-secret"))
	if ctx.Err() != context.Canceled || response.Code != http.StatusNoContent {
		t.Fatal("test did not cancel the client after the actual origin 204")
	}
	entry := waitStopTestOutcome(t, app.db, site.ID, "confirmed")
	if entry.StoppedAtMS == 0 || entry.ClientStopRequestedAtMS == 0 {
		t.Fatalf("actual origin 204 was discarded because the client canceled: %+v", entry)
	}
	assertStopTestActive(t, app.db, site.ID, time.Now(), 0)
}

func TestProxyStopIntentOriginDenialsKeepPlaybackActive(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if strings.HasSuffix(r.URL.Path, "/Stopped") {
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()
			app, site, handler := stopTestHostController(t, upstream.URL)
			handler.ServeHTTP(httptest.NewRecorder(), stopTestRequest(t, t.Context(), site.PublicHost, "/Sessions/Playing/Progress", "denial-secret"))
			if entries, err := app.db.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID}); err != nil || len(entries) != 1 {
				t.Fatalf("initial playback count=%d err=%v", len(entries), err)
			}
			handler.ServeHTTP(httptest.NewRecorder(), stopTestRequest(t, t.Context(), site.PublicHost, "/Sessions/Playing/Stopped", "denial-secret"))
			entry := waitStopTestOutcome(t, app.db, site.ID, "denied")
			if entry.StoppedAtMS != 0 || entry.Completed {
				t.Fatal("rejected stop fabricated successful playback completion")
			}
			assertStopTestActive(t, app.db, site.ID, time.Now(), 1)
		})
	}
}

func TestProxyStopIntentUnmatchedIncomingRequestsKeepActive(t *testing.T) {
	received := make(chan struct{}, 1)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if strings.HasSuffix(r.URL.Path, "/Stopped") {
			received <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	defer close(release)
	app, site, handler := stopTestHostController(t, upstream.URL)
	handler.ServeHTTP(httptest.NewRecorder(), stopTestRequest(t, t.Context(), site.PublicHost, "/Sessions/Playing/Progress", "original-account-secret"))
	if entries, err := app.db.ListWatchHistory(WatchHistoryFilter{SiteID: site.ID}); err != nil || len(entries) != 1 {
		t.Fatalf("original playback count=%d err=%v", len(entries), err)
	}
	otherSite, err := app.db.CreateSiteRecord(Site{Name: "unmatched-other", ListenPort: freePort(t), PublicHost: "unmatched-other.example.test", IngressMode: ingressModeHost, TargetURL: upstream.URL, WatchHistoryEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.pm.StartSite(*otherSite); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(otherSite.ID) })
	otherHandler, _ := app.pm.PublicHostHandler(otherSite.PublicHost)
	for _, tc := range []struct {
		name, token, body string
		otherSite         bool
	}{
		{name: "other account same known session", token: "other-account-secret", body: `{"ItemId":"item-1","PlaySessionId":"session-1"}`},
		{name: "other site same known session", token: "original-account-secret", body: `{"ItemId":"item-1","PlaySessionId":"session-1"}`, otherSite: true},
		{name: "replayed wrong session", token: "original-account-secret", body: `{"ItemId":"item-1","PlaySessionId":"wrong-session"}`},
		{name: "malformed playback body", token: "original-account-secret", body: `{"ItemId":"item-1","PlaySessionId":"session-1","PositionTicks":"invalid"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			targetHandler, host := handler, site.PublicHost
			if tc.otherSite {
				targetHandler, host = otherHandler, otherSite.PublicHost
			}
			request := httptest.NewRequest(http.MethodPost, "https://"+host+"/Sessions/Playing/Stopped", strings.NewReader(tc.body)).WithContext(context.WithValue(ctx, publicHostIngressContextKey{}, true))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Emby-Authorization", fmt.Sprintf(`MediaBrowser Token="%s", Client="Origami", DeviceId="phone-a"`, tc.token))
			done := make(chan struct{})
			go func() {
				targetHandler.ServeHTTP(httptest.NewRecorder(), request)
				close(done)
			}()
			select {
			case <-received:
			case <-time.After(time.Second):
				t.Fatal("origin never consumed the unmatched stop")
			}
			assertStopTestActive(t, app.db, site.ID, time.Now(), 1)
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("unmatched stop did not obey client cancellation")
			}
		})
	}
	var count int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM watch_sessions").Scan(&count); err != nil || count != 1 {
		t.Fatalf("unmatched incoming stops created sessions: count=%d err=%v", count, err)
	}
}

func TestEdgeStopIntentAuthenticatedReportAndACKWithLoggingDisabled(t *testing.T) {
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
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "stop-agent", Address: "203.0.113.45", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, agentToken, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	// The Agent's local site is ID 1; allocate a different Controller ID so
	// the test catches accidental recomputation of its process-local proof.
	if _, err := app.db.CreateSiteRecord(Site{Name: "other-central-site", ListenPort: freePort(t), PublicHost: "other-central.example.test", IngressMode: ingressModeHost, TargetURL: upstream.URL}); err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "stop-agent-site", PublicHost: "stop-agent.example.test", IngressMode: ingressModeHost, TargetURL: upstream.URL, WatchHistoryEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET desired_node_id=? WHERE site_id=?", node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	settings := app.db.currentSystemSettings()
	settings.LogEnabled = false
	if err := app.db.saveSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	runtime := &edgeAgentRuntime{stateDir: t.TempDir()}
	if err := runtime.events.initWithRawKey(runtime.stateDir, bytes.Repeat([]byte("s"), 32), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.events.close() })
	config := AgentRuntimeConfig{
		SchemaVersion: 1, NodeGUID: "stop-agent-guid", HTTPSPort: 19090, DynamicKey: testEdgeRuntimeKey(t), RequestLogPolicy: newAgentRequestLogPolicy(settings),
		Routes: []AgentSiteRoute{{
			SiteID: site.ID, Host: site.PublicHost, TargetURL: upstream.URL, PlaybackMode: "direct", FailoverTargets: "[]", StreamHostsRaw: "[]",
			Site: Site{Name: "stop-agent", TargetURL: upstream.URL, PlaybackMode: "direct", MainVideoStreamMode: "proxy", StreamHosts: "[]", UAMode: passthroughUAMode, ClientIPMode: clientIPModeBoth, WatchHistoryEnabled: true},
		}},
	}
	bundle, err := buildEdgeProxy(config, runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer bundle.close()
	bundle.handler.ServeHTTP(httptest.NewRecorder(), stopTestRequest(t, t.Context(), site.PublicHost, "/Sessions/Playing/Progress", "edge-stop-secret"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		bundle.handler.ServeHTTP(httptest.NewRecorder(), stopTestRequest(t, ctx, site.PublicHost, "/Sessions/Playing/Stopped", "edge-stop-secret"))
		close(done)
	}()
	select {
	case <-stopReceived:
	case <-time.After(time.Second):
		t.Fatal("Agent origin never received the stop body")
	}
	deadline := time.Now().Add(time.Second)
	var pendingEvents []NodeRequestEvent
	for {
		pendingEvents = runtime.events.snapshot()
		if len(pendingEvents) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Agent did not spool progress and immediate stop intent: count=%d", len(pendingEvents))
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, event := range pendingEvents {
		if event.Priority != nodeEventPriorityCritical || !event.SkipRequestLog || event.WatchHistory == nil || event.Authorization != "" || event.Query != "" || event.Body != "" || event.WatchHistory.TokenCiphertext != "" {
			t.Fatal("Agent stop did not use the bounded structured critical event chain")
		}
		if event.WatchHistory.ClientIdentityHash == watchHistoryRequestIdentityHash(site.ID, "edge-stop-secret", "phone-a", "Origami") {
			t.Fatal("test did not isolate the Agent's local credential proof from the Controller")
		}
	}
	encoded, err := json.Marshal(pendingEvents)
	if err != nil || strings.Contains(string(encoded), "edge-stop-secret") {
		t.Fatal("Agent report contains a plaintext playback credential")
	}
	report := NodeReport{BootID: "stop-report-session", ReportSessionID: "stop-report-session", CounterEpoch: "kernel:eth0", Sequence: 1, InterfaceName: "eth0", Events: pendingEvents}
	result, err := app.db.RecordNodeReportResult(agentToken, report, time.Now())
	if err != nil || len(result.AcceptedEventUIDs) != 2 {
		t.Fatalf("authenticated first progress/stop report ACK=%d err=%v", len(result.AcceptedEventUIDs), err)
	}
	entry := waitStopTestOutcome(t, app.db, site.ID, "pending")
	if entry.StoppedAtMS != 0 {
		t.Fatal("Agent pending stop was presented as origin-confirmed")
	}
	assertStopTestActive(t, app.db, site.ID, time.Now(), 0)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Agent stop did not obey client cancellation")
	}
	deadline = time.Now().Add(time.Second)
	var completedEvents []NodeRequestEvent
	for {
		completedEvents = runtime.events.snapshot()
		if len(completedEvents) == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Agent did not spool the unconfirmed stop result")
		}
		time.Sleep(5 * time.Millisecond)
	}
	report.Events, report.Sequence = completedEvents, 2
	result, err = app.db.RecordNodeReportResult(agentToken, report, time.Now())
	if err != nil || len(result.AcceptedEventUIDs) != 3 {
		t.Fatalf("Agent duplicate/result ACK=%d err=%v", len(result.AcceptedEventUIDs), err)
	}
	entry = waitStopTestOutcome(t, app.db, site.ID, "unconfirmed")
	if entry.StoppedAtMS != 0 {
		t.Fatal("canceled Agent stop fabricated origin confirmation")
	}
	if err := runtime.events.ack(completedEvents); err != nil || runtime.events.depth() != 0 {
		t.Fatalf("accepted critical events were not ACKed: depth=%d err=%v", runtime.events.depth(), err)
	}
	report.Sequence = 3
	if _, err := app.db.RecordNodeReportResult(agentToken, report, time.Now()); err != nil {
		t.Fatal(err)
	}
	var sessionCount, logCount int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM watch_sessions WHERE site_id=?", site.ID).Scan(&sessionCount); err != nil {
		t.Fatal(err)
	}
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM request_logs WHERE site_id=?", site.ID).Scan(&logCount); err != nil {
		t.Fatal(err)
	}
	if sessionCount != 1 || logCount != 0 {
		t.Fatalf("Agent replay/log policy changed playback effects: sessions=%d logs=%d", sessionCount, logCount)
	}
}
