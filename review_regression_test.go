package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func reviewRoute(id int64, host, target string) AgentSiteRoute {
	return AgentSiteRoute{SiteID: id, Host: host, TargetURL: target, PlaybackMode: "direct",
		Site:            Site{Name: host, PublicHost: host, IngressMode: ingressModeHost, TargetURL: target, PlaybackMode: "direct", MainVideoStreamMode: "proxy", StreamHosts: "[]", UAMode: passthroughUAMode, ClientIPMode: clientIPModeBoth, DynamicProfile: dynamicProfileSafe},
		FailoverTargets: "[]", StreamHostsRaw: "[]", DynamicSources: `["redirect","playback_info"]`, DynamicRules: "[]"}
}

func installReviewCertificate(t *testing.T, app *App) {
	t.Helper()
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(tlsServer.Close)
	certificate := tlsServer.TLS.Certificates[0]
	keyDER, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	app.panelCertificates = &panelCertificateManager{certFile: certFile, keyFile: keyFile, edgeCertFile: certFile, edgeKeyFile: keyFile, accountDir: dir}
}

func installReviewCertificateForNode(t *testing.T, app *App, guid string) {
	t.Helper()
	certPEM, err := readBoundedPrivateFile(app.panelCertificates.certFile)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := readBoundedPrivateFile(app.panelCertificates.keyFile)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile, err := app.panelCertificates.nodeEdgeTLSPaths(guid)
	if err != nil {
		t.Fatal(err)
	}
	if err := installCertificatePairAtomic(certFile, keyFile, []byte(certPEM), []byte(keyPEM)); err != nil {
		t.Fatal(err)
	}
}

func reviewCertificatePEM(t *testing.T) (string, string) {
	t.Helper()
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(tlsServer.Close)
	certificate := tlsServer.TLS.Certificates[0]
	keyDER, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func TestReviewOversizeChunkedBodyPreserved(t *testing.T) {
	runtime := &edgeAgentRuntime{}
	payload := strings.Repeat("x", edgeEventBodyLimit+20)
	request := httptest.NewRequest(http.MethodPost, "https://media.example.test/Sessions/Playing/Progress", strings.NewReader(payload))
	request.ContentLength = -1
	request.TransferEncoding = []string{"chunked"}
	var received []byte
	handler := runtime.observe(map[string]int64{"media.example.test": 101}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		received, err = io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if string(received) != payload {
		t.Fatalf("upstream received %d bytes, original body was %d bytes", len(received), len(payload))
	}
}

func TestReviewCacheMultiValueHeaders(t *testing.T) {
	for name, values := range map[string][]string{"Cache-Control": {"public", "no-store"}, "Vary": {"Accept-Encoding", "X-User-ID"}} {
		t.Run(name, func(t *testing.T) {
			header := http.Header{"Content-Type": {"image/jpeg"}}
			header[name] = values
			if assetCacheResponseEligible(&http.Response{StatusCode: http.StatusOK, Header: header}, []byte("private image")) {
				t.Fatal("response with restrictive second header line was accepted for caching")
			}
		})
	}
}

func TestReviewCacheSiteIdentityAcrossRebuild(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = io.WriteString(w, r.Header.Get("X-Origin-Secret"))
	}))
	defer upstream.Close()
	runtime := &edgeAgentRuntime{stateDir: t.TempDir()}
	config := AgentRuntimeConfig{SchemaVersion: agentConfigSchemaVersion, NodeGUID: "review-node", HTTPSPort: 9090, DynamicKey: testEdgeRuntimeKey(t)}
	for i, name := range []string{"a.example.test", "b.example.test"} {
		route := reviewRoute(int64(100+i), name, upstream.URL)
		route.Headers = map[string][]string{"X-Origin-Secret": {name}}
		route.Site.AssetCacheEnabled = true
		route.Site.AssetCacheTTLSec = 3600
		route.Site.AssetCacheMaxBytes = 16 << 20
		route.Site.AssetCacheRules = "*/web/*"
		config.Routes = []AgentSiteRoute{route}
		bundle, err := buildEdgeProxy(config, runtime)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		bundle.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://"+name+"/web/private.jpg", nil))
		bundle.close()
		if response.Code != http.StatusOK || response.Body.String() != name {
			t.Fatalf("site %s received %q, upstream calls=%d", name, response.Body.String(), calls)
		}
	}
}

func TestReviewSchedulerSkipsListenerFailure(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	var healthyID int64
	for i, name := range []string{"broken", "healthy"} {
		node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: name, Priority: 200 - i*100, Address: "203.0.113.10", Port: 9090}, now)
		if err != nil {
			t.Fatal(err)
		}
		_, token, err := app.db.EnrollControlNode(enrollment, now)
		if err != nil {
			t.Fatal(err)
		}
		report := NodeReport{BootID: name, Sequence: 1, InterfaceName: "eth0"}
		if i == 0 {
			report.ListenerError = "bind: address already in use"
		} else {
			healthyID = node.ID
		}
		if _, err := app.db.RecordNodeReport(token, report, now); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := app.db.NodeControlSnapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Scheduler.ActiveNodeID != healthyID {
		t.Fatalf("selected node %d with listener failure, healthy node is %d", snapshot.Scheduler.ActiveNodeID, healthyID)
	}
}

func TestReviewOldNodeRetainsRouteUntilDNSCommit(t *testing.T) {
	app := newTestApp(t)
	installReviewCertificate(t, app)
	now := time.Now()
	old, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "old", Address: "203.0.113.10", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	installReviewCertificateForNode(t, app, old.GUID)
	_, oldToken, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.RecordNodeReport(oldToken, NodeReport{BootID: "old", Sequence: 1, InterfaceName: "eth0"}, now); err != nil {
		t.Fatal(err)
	}
	next, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "new", Address: "203.0.113.11", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNode(enrollment, now); err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(reviewRoute(0, "media.example.test", "https://origin.example.test").Site)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", next.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET applied_node_id=?,cf_record_id='still-points-to-old',dns_status='active' WHERE site_id=?", old.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	config, err := app.buildAgentConfig(oldToken, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Routes) == 0 {
		t.Fatal("old node loses all routes while DNS still points to it")
	}
}

func TestReviewConfigRefreshPreservesActiveRequest(t *testing.T) {
	runtime := &edgeAgentRuntime{stateDir: t.TempDir()}
	defer runtime.close()
	route := reviewRoute(101, "media.example.test", "http://127.0.0.1:8096")
	certificatePEM, privateKeyPEM := reviewCertificatePEM(t)
	config := AgentRuntimeConfig{SchemaVersion: agentConfigSchemaVersion, NodeGUID: "review", HTTPSPort: 9090, DynamicKey: testEdgeRuntimeKey(t), Routes: []AgentSiteRoute{route}, CertificatePEM: certificatePEM, PrivateKeyPEM: privateKeyPEM}
	var hashErr error
	config.ConfigHash, hashErr = agentConfigHash(config)
	if hashErr != nil {
		t.Fatal(hashErr)
	}
	old, err := buildEdgeProxy(config, runtime)
	if err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.bundle, runtime.handler, runtime.port, runtime.server, runtime.nodeGUID = old, old.handler, 9090, &http.Server{}, "review"
	runtime.mu.Unlock()
	var inst *ProxyInstance
	for _, value := range old.manager.proxies {
		inst = value
	}
	if !inst.beginRequest() {
		t.Fatal("request gate closed")
	}
	defer inst.endRequest()
	config.Routes[0].Site.Name = "renamed only"
	config.ConfigHash, err = agentConfigHash(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.apply(config); err != nil {
		t.Fatal(err)
	}
	select {
	case <-inst.ctx.Done():
		t.Fatal("configuration refresh cancelled an active request")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestReviewEventUIDDeduplicatesAcrossAgentRestart(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "event-dedupe", Address: "203.0.113.20", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "event-site", PublicHost: "media.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET desired_node_id=? WHERE site_id=?", node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	event := NodeRequestEvent{EventUID: strings.Repeat("a", 32), EventID: 1, SiteID: site.ID, Host: "media.example.test", Method: http.MethodPost, Path: "/Sessions/Playing/Progress", StatusCode: 204, RecordedAtMS: now.UnixMilli(), SkipRequestLog: true}
	first := NodeReport{BootID: "session-a", ReportSessionID: "session-a", CounterEpoch: "kernel:eth0", Sequence: 1, InterfaceName: "eth0", Events: []NodeRequestEvent{event}}
	second := first
	second.BootID = "session-b"
	second.ReportSessionID = "session-b"
	second.Sequence = 1
	if _, err := app.db.RecordNodeReport(token, first, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.RecordNodeReport(token, second, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM node_request_events WHERE event_uid=?", event.EventUID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("event UID was recorded %d times after session restart", count)
	}
}

func TestReviewTrafficCounterContinuesAcrossAgentRestart(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "counter-continuity", Address: "203.0.113.21", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	for i, report := range []NodeReport{
		{BootID: "session-a", ReportSessionID: "session-a", CounterEpoch: "kernel:eth0", Sequence: 1, InterfaceName: "eth0", RXBytes: 1000, TXBytes: 2000},
		{BootID: "session-b", ReportSessionID: "session-b", CounterEpoch: "kernel:eth0", Sequence: 1, InterfaceName: "eth0", RXBytes: 1500, TXBytes: 2600},
	} {
		if _, err := app.db.RecordNodeReport(token, report, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := app.db.NodeControlSnapshot(now.Add(2 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var got ControlNode
	for _, candidate := range snapshot.Nodes {
		if candidate.ID == node.ID {
			got = candidate
		}
	}
	if got.PeriodRXBytes != 500 || got.PeriodTXBytes != 600 {
		t.Fatalf("counter delta after Agent restart = rx %d tx %d, want 500/600", got.PeriodRXBytes, got.PeriodTXBytes)
	}
}

func TestReviewSiteTrafficCounterContinuesAcrossAgentRestart(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "site-counter-continuity", Address: "203.0.113.22", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "site-counter", PublicHost: "site-counter.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET desired_node_id=? WHERE site_id=?", node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	base := NodeSiteStat{Host: site.PublicHost, RequestCount: 10, LastRequestAtMS: now.UnixMilli(), LastStatus: 200, CumulativeBytesIn: 1000, CumulativeBytesOut: 2000}
	if _, err := app.db.RecordNodeReport(token, NodeReport{BootID: "session-a", ReportSessionID: "session-a", CounterEpoch: "kernel:eth0", Sequence: 1, InterfaceName: "eth0", SiteStats: []NodeSiteStat{base}}, now); err != nil {
		t.Fatal(err)
	}
	second := base
	second.RequestCount = 14
	second.CumulativeBytesIn = 1500
	second.CumulativeBytesOut = 2600
	second.LastRequestAtMS = now.Add(time.Second).UnixMilli()
	if _, err := app.db.RecordNodeReport(token, NodeReport{BootID: "session-b", ReportSessionID: "session-b", CounterEpoch: "kernel:eth0", Sequence: 1, InterfaceName: "eth0", SiteStats: []NodeSiteStat{second}}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var gotIn, gotOut, gotRequests int64
	if err := app.db.db.QueryRow("SELECT COALESCE(SUM(bytes_in),0),COALESCE(SUM(bytes_out),0),COALESCE(SUM(requests),0) FROM node_site_traffic_logs WHERE node_id=? AND site_id=?", node.ID, site.ID).Scan(&gotIn, &gotOut, &gotRequests); err != nil {
		t.Fatal(err)
	}
	if gotIn != 500 || gotOut != 600 || gotRequests != 4 {
		t.Fatalf("site traffic delta after Agent restart = in %d out %d requests %d, want 500/600/4", gotIn, gotOut, gotRequests)
	}
}

func TestReviewAgentReportCannotModifyUnauthorizedSite(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	nodeA, enrollA, err := app.db.CreateControlNode(NodeCreateInput{Name: "auth-a", Address: "203.0.113.30", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	nodeB, enrollB, err := app.db.CreateControlNode(NodeCreateInput{Name: "auth-b", Address: "203.0.113.31", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, tokenA, err := app.db.EnrollControlNode(enrollA, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNode(enrollB, now); err != nil {
		t.Fatal(err)
	}
	siteA, err := app.db.CreateSiteRecord(Site{Name: "auth-site-a", ListenPort: 18080, PublicHost: "auth-a.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	siteB, err := app.db.CreateSiteRecord(Site{Name: "auth-site-b", ListenPort: 18081, PublicHost: "auth-b.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18081"})
	if err != nil {
		t.Fatal(err)
	}
	for _, site := range []*Site{siteA, siteB} {
		if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", nodeA.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET desired_node_id=? WHERE site_id=?", nodeA.ID, siteA.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=? WHERE site_id=?", nodeB.ID, nodeB.ID, siteB.ID); err != nil {
		t.Fatal(err)
	}
	validUID := strings.Repeat("a", 32)
	unauthorizedUID := strings.Repeat("b", 32)
	result, err := app.db.RecordNodeReportResult(tokenA, NodeReport{
		BootID: "auth-session", ReportSessionID: "auth-session", CounterEpoch: "kernel:eth0", Sequence: 1, InterfaceName: "eth0",
		MediaCounts:  []NodeMediaCount{{SiteID: siteB.ID, MovieCount: 99, SeriesCount: 88, EpisodeCount: 77, ObservedAtMS: now.UnixMilli()}},
		Observations: []NodeDynamicObservation{{SiteID: siteB.ID, CanonicalAuthority: "https://auth-b.example.com:443", Source: dynamicObservationSourceRedirect, Decision: dynamicObservationDecisionAllowed, ReasonCode: dynamicObservationReasonRedirectAllowed, ObservedAtMS: now.UnixMilli()}},
		Events: []NodeRequestEvent{
			{EventID: 10, EventUID: validUID, SiteID: siteA.ID, Host: siteA.PublicHost, Method: http.MethodGet, Path: "/ok", StatusCode: 200, RecordedAtMS: now.UnixMilli()},
			{EventID: 11, EventUID: unauthorizedUID, SiteID: siteB.ID, Host: siteB.PublicHost, Method: http.MethodGet, Path: "/secret", StatusCode: 200, RecordedAtMS: now.UnixMilli()},
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.AcceptedEventIDs) != 1 || result.AcceptedEventIDs[0] != 10 || len(result.DiscardedEventIDs) != 1 || result.DiscardedEventIDs[0] != 11 {
		t.Fatalf("unexpected event acknowledgement: %#v", result)
	}
	if len(result.DiscardedEventUIDs) != 1 || result.DiscardedEventUIDs[0] != unauthorizedUID {
		t.Fatalf("unauthorized event UID was not discarded: %#v", result)
	}
	var movieCount int
	if err := app.db.db.QueryRow("SELECT media_movie_count FROM sites WHERE id=?", siteB.ID).Scan(&movieCount); err != nil {
		t.Fatal(err)
	}
	if movieCount != -1 {
		t.Fatalf("unauthorized media count modified Site B: %d", movieCount)
	}
	var observations, logs int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM dynamic_observations WHERE site_id=?", siteB.ID).Scan(&observations); err != nil {
		t.Fatal(err)
	}
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM request_logs WHERE site_id=?", siteB.ID).Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if observations != 0 || logs != 0 {
		t.Fatalf("unauthorized report produced Site B side effects: observations=%d logs=%d", observations, logs)
	}
	// The same site ID with another site's Host is also unauthorized.
	result, err = app.db.RecordNodeReportResult(tokenA, NodeReport{
		BootID: "auth-session", ReportSessionID: "auth-session", CounterEpoch: "kernel:eth0", Sequence: 2, InterfaceName: "eth0",
		Events: []NodeRequestEvent{{EventID: 12, EventUID: strings.Repeat("c", 32), SiteID: siteA.ID, Host: siteB.PublicHost, Method: http.MethodGet, Path: "/spoof", StatusCode: 200, RecordedAtMS: now.UnixMilli()}},
	}, now.Add(time.Second))
	if err != nil || len(result.DiscardedEventIDs) != 1 || result.DiscardedEventIDs[0] != 12 {
		t.Fatalf("mismatched Site/Host event was accepted: result=%#v err=%v", result, err)
	}
}

func TestReviewLegacyEdgeTLSMigratesOnlyKnownNodesIntoStateDir(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meridian.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t.Setenv("PANEL_TLS_CERT_FILE", "")
	t.Setenv("PANEL_TLS_KEY_FILE", "")
	legacyRoot := filepath.Join(dir, "external", "edge-nodes")
	stateDir := filepath.Join(dir, "owned-tls")
	t.Setenv("EDGE_TLS_CERT_FILE", filepath.Join(dir, "external", "edge.pem"))
	t.Setenv("EDGE_TLS_KEY_FILE", filepath.Join(dir, "external", "edge.key"))
	t.Setenv("TLS_STATE_DIR", stateDir)
	if err := os.MkdirAll(filepath.Dir(os.Getenv("EDGE_TLS_CERT_FILE")), 0o700); err != nil {
		t.Fatal(err)
	}
	known, _, err := db.CreateControlNode(NodeCreateInput{Name: "known-edge", Address: "203.0.113.40", Port: 9090}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(legacyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyRoot, "sentinel.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := reviewCertificatePEM(t)
	legacyCurrent := filepath.Join(legacyRoot, known.GUID, "current")
	if err := os.MkdirAll(legacyCurrent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyCurrent, "fullchain.pem"), []byte(certPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyCurrent, "privkey.pem"), []byte(keyPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	unknownCurrent := filepath.Join(legacyRoot, "unknown-node", "current")
	if err := os.MkdirAll(unknownCurrent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unknownCurrent, "fullchain.pem"), []byte("unknown"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyEdgeTLSState(db, dbPath); err != nil {
		t.Fatal(err)
	}
	manager := newPanelCertificateManager(dbPath, nil)
	certPath, keyPath, err := manager.nodeEdgeTLSPaths(known.GUID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Fatalf("migrated certificate pair is invalid: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "edge-nodes", "unknown-node")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unknown legacy node was migrated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacyRoot, "sentinel.txt")); err != nil {
		t.Fatalf("legacy sentinel was changed or removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacyCurrent, "fullchain.pem")); err != nil {
		t.Fatalf("legacy known certificate was removed: %v", err)
	}
}

func TestReviewEventSpoolEncryptsAndReportsPersistenceFailure(t *testing.T) {
	dir := t.TempDir()
	var store edgeEventStore
	if err := store.initWithKey(dir, []byte("agent-token")); err != nil {
		t.Fatal(err)
	}
	event := NodeRequestEvent{SiteID: 1, Host: "media.example.test", Method: http.MethodGet, Path: "/api", StatusCode: 200, Authorization: "Bearer secret", RecordedAtMS: time.Now().UnixMilli()}
	if err := store.add(event); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "events.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Bearer secret") {
		t.Fatal("encrypted event spool contains the Authorization value")
	}
	var restored edgeEventStore
	if err := restored.initWithKey(dir, []byte("agent-token")); err != nil {
		t.Fatal(err)
	}
	if len(restored.snapshot()) != 1 || restored.snapshot()[0].EventUID == "" {
		t.Fatal("encrypted event spool could not be restored")
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := store.add(event); err == nil {
		t.Fatal("event spool persistence failure was silently ignored")
	}
}

func TestReviewAgentRejectsConfigHashMismatch(t *testing.T) {
	runtime := &edgeAgentRuntime{stateDir: t.TempDir()}
	defer runtime.close()
	config := AgentRuntimeConfig{SchemaVersion: agentConfigSchemaVersion, ConfigHash: strings.Repeat("0", 64), NodeGUID: "review", HTTPSPort: 9090, DynamicKey: testEdgeRuntimeKey(t)}
	if err := runtime.apply(config); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("config hash mismatch was accepted: %v", err)
	}
}
