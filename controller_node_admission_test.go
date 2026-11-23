package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

type controllerNodeAdmissionFixture struct {
	*schedulerDNSFixture
	upstream  *httptest.Server
	hits      atomic.Int64
	transport http.RoundTripper
	client    *http.Client
	router    http.Handler
}

func newControllerNodeAdmissionFixture(t *testing.T, generic bool, ingress string, handler http.Handler) *controllerNodeAdmissionFixture {
	t.Helper()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	f := newSchedulerDNSFixture(t, false)
	fixture := &controllerNodeAdmissionFixture{schedulerDNSFixture: f, transport: transport, client: &http.Client{Transport: transport, Timeout: 5 * time.Second}}
	fixture.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture.hits.Add(1)
		if handler != nil {
			handler.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/Sessions/Playing") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, "controller-upstream")
	}))
	t.Cleanup(fixture.upstream.Close)
	f.site.TargetURL = fixture.upstream.URL
	f.site.IngressMode = ingress
	f.site.ListenPort = freePort(t)
	f.site.WatchHistoryEnabled = true
	if ingress == ingressModePath {
		f.site.PathPrefix = "/owned"
	}
	storedMode := upstreamModeFixed
	if generic {
		storedMode = upstreamModePathTarget
		f.site.UpstreamMode = upstreamModePathTarget
		f.site.TargetURL = ""
		if _, err := f.app.db.db.Exec("UPDATE control_nodes SET agent_version=? WHERE id=?", pathTargetNodeTestVersion, f.node.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Scheduling publishes a DNS host. Keep that valid durable route while an
	// alternate Controller port/path entry for the same site is exercised; a
	// cached local entry must obey site ownership regardless of its ingress.
	storedIngress, storedPrefix := ingress, f.site.PathPrefix
	if ingress == ingressModePort || ingress == ingressModePath {
		storedIngress, storedPrefix = ingressModeHost, ""
		f.site.PublicHost = ""
	}
	if _, err := f.app.db.db.Exec(`UPDATE sites SET target_url=?,upstream_mode=?,ingress_mode=?,path_prefix=?,listen_port=?,watch_history_enabled=1 WHERE id=?`,
		f.site.TargetURL, storedMode, storedIngress, storedPrefix, f.site.ListenPort, f.site.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.db.db.Exec(`UPDATE site_node_schedules SET applied_node_id=NULL,dns_status='pending',config_pending_since_ms=? WHERE site_id=?`, f.now.UnixMilli(), f.site.ID); err != nil {
		t.Fatal(err)
	}
	fixture.start(t)
	t.Cleanup(func() { _ = fixture.app.pm.StopSite(fixture.site.ID) })
	fixture.router = f.app.publicHostRouter(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	return fixture
}

func (f *controllerNodeAdmissionFixture) start(t *testing.T) {
	t.Helper()
	schedulerTransport := http.DefaultTransport
	http.DefaultTransport = f.transport
	defer func() { http.DefaultTransport = schedulerTransport }()
	if ingressUsesPort(f.site.IngressMode) {
		releasePort(f.site.ListenPort)
	}
	err := f.app.pm.StartSite(f.site)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *controllerNodeAdmissionFixture) sourcePath(path string) string {
	if siteUsesPathTarget(f.site) {
		return "/" + f.upstream.URL + path
	}
	return path
}

func (f *controllerNodeAdmissionFixture) request(t *testing.T, method, path, entry string, body io.Reader, upgrade bool) *httptest.ResponseRecorder {
	t.Helper()
	path = f.sourcePath(path)
	host := f.host
	if entry == "alias" {
		host = "cached-alias.example.test"
	}
	if entry == "path" {
		path = f.site.PathPrefix + path
		host = "panel.example.test"
	}
	request := httptest.NewRequest(method, "http://"+host+path, body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Emby-Token", "synthetic-client-token")
	request.Header.Set("X-Emby-Device-Id", "synthetic-client-device")
	if upgrade {
		request.Header.Set("Connection", "Upgrade")
		request.Header.Set("Upgrade", "websocket")
		request.Header.Set("Sec-WebSocket-Version", "13")
		request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	}
	response := httptest.NewRecorder()
	if entry != "port" {
		f.router.ServeHTTP(response, request)
		return response
	}
	request.URL.Host = fmt.Sprintf("127.0.0.1:%d", f.site.ListenPort)
	request.Host = "cached-or-unrelated-host.example.test"
	request.RequestURI = ""
	result, err := f.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Body.Close()
	for name, values := range result.Header {
		response.Header()[name] = append([]string(nil), values...)
	}
	response.WriteHeader(result.StatusCode)
	if _, err := io.Copy(response, result.Body); err != nil {
		t.Fatal(err)
	}
	return response
}

func (f *controllerNodeAdmissionFixture) apply(t *testing.T) {
	t.Helper()
	if _, err := f.app.db.db.Exec(`UPDATE site_node_schedules SET config_pending_since_ms=0 WHERE site_id=?`, f.site.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(f.now.Add(time.Second)); err != nil {
		t.Fatalf("commit applied node ownership: %v", err)
	}
	f.hits.Store(0)
}

func assertControllerNodeAdmissionDenied(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("controller status=%d body=%s, want 421", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Location") != "" || !strings.Contains(response.Body.String(), "site_node_owned") {
		t.Fatalf("ownership rejection headers=%v body=%s", response.Header(), response.Body.String())
	}
}

func TestControllerNodeAdmissionCutsOverOnlyAfterAppliedCommit(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		generic bool
		ingress string
		entry   string
	}{
		{"fixed-host-and-port", false, ingressModeBoth, "host"},
		{"fixed-path", false, ingressModePath, "path"},
		{"generic-host", true, ingressModeHost, "host"},
		{"generic-port", true, ingressModePort, "port"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newControllerNodeAdmissionFixture(t, scenario.generic, scenario.ingress, nil)
			if err := f.reconcile(f.now); err == nil {
				t.Fatal("pending Agent configuration unexpectedly committed")
			}
			if response := f.request(t, http.MethodGet, "/Videos/movie/stream.mkv", scenario.entry, nil, false); response.Code != http.StatusOK || f.hits.Load() != 1 {
				t.Fatalf("pending controller status=%d hits=%d", response.Code, f.hits.Load())
			}
			// Probe failure after Agent readiness must leave a first-time site on
			// the Controller until DNS and the applied assignment are committed.
			if _, err := f.app.db.db.Exec("UPDATE site_node_schedules SET config_pending_since_ms=0 WHERE site_id=?", f.site.ID); err != nil {
				t.Fatal(err)
			}
			f.probeFails.Store(true)
			if err := f.reconcile(f.now); err == nil {
				t.Fatal("unhealthy first-time node unexpectedly committed")
			}
			if response := f.request(t, http.MethodGet, "/ping", scenario.entry, nil, false); response.Code != http.StatusOK {
				t.Fatalf("unhealthy first-time controller status=%d", response.Code)
			}
			f.probeFails.Store(false)
			f.apply(t)
			entries := []string{scenario.entry}
			if ingressUsesPort(f.site.IngressMode) && scenario.entry != "port" {
				entries = append(entries, "port")
			}
			if ingressUsesHost(f.site.IngressMode) {
				f.app.pm.mu.Lock()
				f.app.pm.publicHosts["cached-alias.example.test"] = f.site.ID
				f.app.pm.publicHostModes["cached-alias.example.test"] = f.site.IngressMode
				f.app.pm.mu.Unlock()
				entries = append(entries, "alias")
			}
			for _, entry := range entries {
				for _, path := range []string{"/Videos/movie/stream.mkv", "/System/Info/Public", "/_meridian/dynamic/expired", "/socket"} {
					assertControllerNodeAdmissionDenied(t, f.request(t, http.MethodGet, path, entry, nil, path == "/socket"))
				}
				body := strings.NewReader(`{"ItemId":"movie","PlaySessionId":"blocked-session","PositionTicks":100,"NowPlayingItem":{"Id":"movie","Name":"Blocked Movie","Type":"Movie","RunTimeTicks":1000}}`)
				assertControllerNodeAdmissionDenied(t, f.request(t, http.MethodPost, "/Sessions/Playing/Progress", entry, body, false))
			}
			if scenario.entry == "path" {
				response := httptest.NewRecorder()
				f.router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://panel.example.test"+f.site.PathPrefix, nil))
				assertControllerNodeAdmissionDenied(t, response)
			}
			if f.hits.Load() != 0 {
				t.Fatalf("applied controller contacted upstream %d times", f.hits.Load())
			}
			if err := f.app.db.flushDynamicObservations(); err != nil {
				t.Fatal(err)
			}
			var sessions, blockedLogs int
			if err := f.app.db.db.QueryRow("SELECT COUNT(*) FROM watch_sessions WHERE site_id=?", f.site.ID).Scan(&sessions); err != nil {
				t.Fatal(err)
			}
			if err := f.app.db.db.QueryRow("SELECT COUNT(*) FROM request_logs WHERE site_id=? AND status_code=421", f.site.ID).Scan(&blockedLogs); err != nil {
				t.Fatal(err)
			}
			if sessions != 0 || blockedLogs != 0 {
				t.Fatalf("rejected ingress produced watch sessions=%d logs=%d", sessions, blockedLogs)
			}
		})
	}
}

func TestControllerNodeAdmissionKeepsOwnerThroughFailoverAndCleanup(t *testing.T) {
	f := newControllerNodeAdmissionFixture(t, false, ingressModeHost, nil)
	f.apply(t)
	next, enrollment, err := f.app.db.CreateControlNode(NodeCreateInput{Name: "not-ready-replacement", Address: "203.0.113.12", Port: 19012}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := f.app.db.EnrollControlNode(enrollment, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.db.RecordNodeReport(token, NodeReport{BootID: "replacement", Sequence: 1, InterfaceName: "eth0", AgentVersion: pathTargetNodeTestVersion}, f.now); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.db.SaveSiteNodeSchedule(f.site.ID, true, "fixed", next.ID, f.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := f.app.refreshSiteAssignments(f.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(f.now.Add(2 * time.Second)); err == nil {
		t.Fatal("replacement without applied configuration unexpectedly committed")
	}
	assertControllerNodeAdmissionDenied(t, f.request(t, http.MethodGet, "/ping", "host", nil, false))
	if _, err := f.app.db.db.Exec("UPDATE control_nodes SET agent_apply_error='synthetic replacement failure' WHERE id=?", next.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(f.now.Add(3 * time.Second)); err == nil {
		t.Fatal("replacement apply failure unexpectedly committed")
	}
	assertControllerNodeAdmissionDenied(t, f.request(t, http.MethodGet, "/ping", "host", nil, false))
	schedule, err := f.app.db.SaveSiteNodeSchedule(f.site.ID, false, "fixed", 0, f.now.Add(4*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	assertControllerNodeAdmissionDenied(t, f.request(t, http.MethodGet, "/ping", "host", nil, false))
	if schedule.AppliedNodeID != f.node.ID {
		t.Fatal("disabling a pending replacement lost the committed owner")
	}
	schedulerTransport := http.DefaultTransport
	http.DefaultTransport = schedulerDNSRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodDelete {
			return nil, errors.New("synthetic DNS deletion failure")
		}
		return schedulerTransport.RoundTrip(request)
	})
	err = f.app.deleteTrackedSiteDNS(t.Context(), schedule)
	http.DefaultTransport = schedulerTransport
	if err == nil {
		t.Fatal("DNS deletion failure unexpectedly completed cleanup")
	}
	assertControllerNodeAdmissionDenied(t, f.request(t, http.MethodGet, "/ping", "host", nil, false))
	if err := f.app.deleteTrackedSiteDNS(t.Context(), schedule); err != nil {
		t.Fatalf("retry disabled cleanup: %v", err)
	}
	if response := f.request(t, http.MethodGet, "/ping", "host", nil, false); response.Code != http.StatusOK || f.hits.Load() != 1 {
		t.Fatalf("cleanup did not reopen controller: status=%d hits=%d", response.Code, f.hits.Load())
	}
}

func TestControllerNodeAdmissionDoesNotPublishFailedCutover(t *testing.T) {
	f := newControllerNodeAdmissionFixture(t, false, ingressModeHost, nil)
	if _, err := f.app.db.db.Exec("UPDATE site_node_schedules SET config_pending_since_ms=0 WHERE site_id=?", f.site.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.db.db.Exec(`CREATE TRIGGER reject_admission_cutover BEFORE UPDATE OF applied_node_id ON site_node_schedules
		WHEN NEW.applied_node_id IS NOT NULL BEGIN SELECT RAISE(ABORT,'synthetic cutover persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(f.now); err == nil {
		t.Fatal("cutover persistence failure unexpectedly committed ownership")
	}
	if response := f.request(t, http.MethodGet, "/ping", "host", nil, false); response.Code != http.StatusOK || f.hits.Load() != 1 {
		t.Fatalf("failed first cutover closed controller admission: status=%d hits=%d", response.Code, f.hits.Load())
	}
	if _, err := f.app.db.db.Exec("DROP TRIGGER reject_admission_cutover"); err != nil {
		t.Fatal(err)
	}
	f.apply(t)
	assertControllerNodeAdmissionDenied(t, f.request(t, http.MethodGet, "/ping", "host", nil, false))
}

func TestControllerNodeAdmissionReopensAfterNodeDeletionCleanup(t *testing.T) {
	f := newControllerNodeAdmissionFixture(t, false, ingressModeHost, nil)
	f.apply(t)
	schedulerTransport := http.DefaultTransport
	http.DefaultTransport = schedulerDNSRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodDelete {
			return nil, errors.New("synthetic node deletion DNS failure")
		}
		return schedulerTransport.RoundTrip(request)
	})
	err := f.app.prepareNodeDeletion(t.Context(), f.node.ID)
	http.DefaultTransport = schedulerTransport
	if err == nil {
		t.Fatal("failed remote cleanup unexpectedly completed node deletion")
	}
	schedule, err := f.app.db.siteNodeSchedule(f.site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.Enabled || schedule.AppliedNodeID != f.node.ID {
		t.Fatal("failed node deletion did not preserve the disabled cleanup owner")
	}
	assertControllerNodeAdmissionDenied(t, f.request(t, http.MethodGet, "/ping", "host", nil, false))
	if err := f.app.prepareNodeDeletion(t.Context(), f.node.ID); err != nil {
		t.Fatalf("retry node cleanup: %v", err)
	}
	if err := f.app.db.DeleteControlNode(f.node.ID); err != nil {
		t.Fatal(err)
	}
	if response := f.request(t, http.MethodGet, "/ping", "host", nil, false); response.Code != http.StatusOK || f.hits.Load() != 1 {
		t.Fatalf("node cleanup did not reopen controller: status=%d hits=%d", response.Code, f.hits.Load())
	}
}

func TestControllerNodeAdmissionRefreshesAfterScheduleRemoval(t *testing.T) {
	f := newControllerNodeAdmissionFixture(t, false, ingressModeHost, nil)
	f.apply(t)
	if err := f.app.removeSiteNodeSchedule(t.Context(), f.site.ID); err != nil {
		t.Fatal(err)
	}
	if response := f.request(t, http.MethodGet, "/ping", "host", nil, false); response.Code != http.StatusOK || f.hits.Load() != 1 {
		t.Fatalf("schedule removal retained stale controller admission: status=%d hits=%d", response.Code, f.hits.Load())
	}
}

func TestControllerNodeAdmissionConcurrentPublication(t *testing.T) {
	f := newControllerNodeAdmissionFixture(t, false, ingressModeHost, nil)
	f.apply(t)
	// First takeover dirties the Agent config. Restore its simulated ACK so
	// later maintenance commits exercise publication rather than readiness.
	if _, err := f.app.db.db.Exec("UPDATE control_nodes SET desired_config_hash=applied_config_hash WHERE id=?", f.node.ID); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var workers sync.WaitGroup
	workerErrors := make(chan string, 4)
	for range 4 {
		workers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				response := httptest.NewRecorder()
				f.router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://"+f.host+"/ping", nil))
				if response.Code != http.StatusMisdirectedRequest {
					workerErrors <- fmt.Sprintf("published owner briefly reopened controller: %d", response.Code)
					return
				}
			}
		})
	}
	var publicationError error
	for round := range 12 {
		f.app.forgetSiteDNSMaintenance(f.site.ID)
		if err := f.reconcile(f.now.Add(time.Duration(round+2) * time.Second)); err != nil {
			publicationError = err
			break
		}
	}
	close(stop)
	workers.Wait()
	if publicationError != nil {
		t.Fatal(publicationError)
	}
	select {
	case message := <-workerErrors:
		t.Fatal(message)
	default:
	}
	if f.hits.Load() != 0 {
		t.Fatal("concurrent snapshot publication admitted a new upstream request")
	}
}

func TestControllerNodeAdmissionPreservesAdmittedHTTPAndWebSockets(t *testing.T) {
	for _, generic := range []bool{false, true} {
		t.Run(fmt.Sprintf("generic=%t", generic), func(t *testing.T) {
			release := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			mux := http.NewServeMux()
			mux.HandleFunc("/Videos/movie/stream.mkv", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "video/x-matroska")
				_, _ = io.WriteString(w, "before")
				w.(http.Flusher).Flush()
				select {
				case <-release:
					_, _ = io.WriteString(w, "after")
				case <-r.Context().Done():
				}
			})
			mux.Handle("/socket", websocket.Handler(func(conn *websocket.Conn) {
				defer conn.Close()
				for {
					var message string
					if websocket.Message.Receive(conn, &message) != nil {
						return
					}
					if websocket.Message.Send(conn, message) != nil {
						return
					}
				}
			}))
			f := newControllerNodeAdmissionFixture(t, generic, ingressModePort, mux)
			url := fmt.Sprintf("http://127.0.0.1:%d", f.site.ListenPort)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, url+f.sourcePath("/Videos/movie/stream.mkv"), nil)
			if err != nil {
				t.Fatal(err)
			}
			stream, err := f.client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Body.Close()
			before := make([]byte, len("before"))
			if _, err := io.ReadFull(stream.Body, before); err != nil || string(before) != "before" {
				t.Fatalf("admit HTTP stream: bytes=%s err=%v", before, err)
			}
			config, err := websocket.NewConfig("ws"+strings.TrimPrefix(url, "http")+f.sourcePath("/socket"), url)
			if err != nil {
				t.Fatal(err)
			}
			conn, err := websocket.DialConfig(config)
			if err != nil {
				t.Fatalf("admit websocket: %v", err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			echo := func(message string) {
				t.Helper()
				if err := websocket.Message.Send(conn, message); err != nil {
					t.Fatal(err)
				}
				var received string
				if err := websocket.Message.Receive(conn, &received); err != nil || received != message {
					t.Fatalf("WebSocket interrupted: got=%q err=%v", received, err)
				}
			}
			echo("before")
			f.apply(t)
			assertControllerNodeAdmissionDenied(t, f.request(t, http.MethodGet, "/socket", "port", nil, true))
			echo("after")
			releaseOnce.Do(func() { close(release) })
			after, err := io.ReadAll(stream.Body)
			if err != nil || string(after) != "after" {
				t.Fatalf("HTTP stream interrupted by ownership commit: bytes=%s err=%v", after, err)
			}
			if f.hits.Load() != 0 {
				t.Fatalf("new connection reached upstream after ownership commit: hits=%d", f.hits.Load())
			}
		})
	}
}

func TestControllerNodeAdmissionSurvivesRestartAndDoesNotQuerySQLite(t *testing.T) {
	f := newControllerNodeAdmissionFixture(t, false, ingressModeHost, nil)
	f.apply(t)
	if _, err := f.app.db.SaveSiteNodeSchedule(f.site.ID, false, "fixed", 0, f.now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := f.app.pm.StopSite(f.site.ID); err != nil {
		t.Fatal(err)
	}
	path := f.app.db.dbPath
	f.app.db.Close()
	schedulerTransport := http.DefaultTransport
	http.DefaultTransport = f.transport
	database, err := openDB(path)
	http.DefaultTransport = schedulerTransport
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = f.app.pm.StopSite(f.site.ID)
		database.Close()
	})
	f.app.db = database
	f.app.pm = NewProxyManager(database, bytes.Repeat([]byte{0x42}, 32))
	f.app.pm.SetHostOnlyIngressSafe(true)
	f.start(t)
	assertControllerNodeAdmissionDenied(t, f.request(t, http.MethodGet, "/ping", "host", nil, false))
	// Hold the only SQLite connection. A request that performs even one owner
	// query cannot finish until this transaction releases it.
	transaction, err := database.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		f.router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://"+f.host+"/ping", nil))
		done <- response
	}()
	select {
	case response := <-done:
		assertControllerNodeAdmissionDenied(t, response)
	case <-time.After(time.Second):
		t.Fatal("controller admission waited for SQLite instead of reading its in-memory owner")
	}
	if f.hits.Load() != 0 {
		t.Fatal("restarted disabled schedule with pending cleanup reached upstream")
	}
}

func TestControllerNodeAdmissionDoesNotAffectPanelOtherSitesOrAgentRuntime(t *testing.T) {
	for _, generic := range []bool{false, true} {
		t.Run(fmt.Sprintf("generic=%t", generic), func(t *testing.T) {
			f := newControllerNodeAdmissionFixture(t, generic, ingressModeHost, nil)
			f.apply(t)
			response := httptest.NewRecorder()
			f.router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://panel.example.test/api/sites", nil))
			if response.Code != http.StatusNoContent {
				t.Fatalf("site ownership affected panel API: %d", response.Code)
			}
			other := Site{ID: f.site.ID + 1, Name: "controller-owned-other-site", PublicHost: "other.example.test", IngressMode: ingressModeHost, TargetURL: f.upstream.URL, PlaybackMode: "direct", StreamHosts: "[]", UAMode: passthroughUAMode}
			schedulerTransport := http.DefaultTransport
			http.DefaultTransport = f.transport
			err := f.app.pm.StartSite(other)
			http.DefaultTransport = schedulerTransport
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.app.pm.StopSite(other.ID) })
			response = httptest.NewRecorder()
			f.router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://"+other.PublicHost+"/ping", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("site ownership affected another site: %d", response.Code)
			}
			runtime := &edgeAgentRuntime{stateDir: t.TempDir()}
			config := AgentRuntimeConfig{SchemaVersion: agentConfigSchemaVersion, NodeGUID: f.node.GUID, HTTPSPort: 19090, DynamicKey: testEdgeRuntimeKey(t), Routes: []AgentSiteRoute{{SiteID: f.site.ID, Host: f.host, TargetURL: f.site.TargetURL, Site: f.site, FailoverTargets: "[]", StreamHostsRaw: "[]"}}}
			http.DefaultTransport = f.transport
			bundle, err := buildEdgeProxy(config, runtime)
			http.DefaultTransport = schedulerTransport
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(bundle.close)
			// The same core may see an ownership snapshot, but the ephemeral Agent
			// database is an explicit bypass and never acts as the Controller.
			owners := make(map[int64]int64)
			for localID := range bundle.localSites {
				owners[localID] = f.node.ID
			}
			bundle.database.controllerNodeAdmission.Store(&controllerNodeAdmissionSnapshot{owners: owners})
			runtime.handler, runtime.bundle = bundle.handler, bundle
			response = httptest.NewRecorder()
			runtime.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://"+f.host+f.sourcePath("/ping"), nil))
			if response.Code != http.StatusOK {
				t.Fatalf("controller admission blocked Agent runtime: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestControllerNodeAdmissionStartupFailsClosedOnInvalidOwnership(t *testing.T) {
	f := newControllerNodeAdmissionFixture(t, false, ingressModeHost, nil)
	if _, err := f.app.db.db.Exec("UPDATE site_node_schedules SET applied_node_id='invalid-owner' WHERE site_id=?", f.site.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.app.pm.StopSite(f.site.ID); err != nil {
		t.Fatal(err)
	}
	path := f.app.db.dbPath
	f.app.db.Close()
	database, err := openDB(path)
	if err == nil {
		database.Close()
		t.Fatal("invalid durable ownership silently opened an empty admission snapshot")
	}
	if !strings.Contains(err.Error(), "controller node admission") {
		t.Fatalf("unexpected startup error: %v", err)
	}
}
