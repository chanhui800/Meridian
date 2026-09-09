package main

import (
	"context"
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

func TestReviewSchedulerKeepsAssignmentOnApplyFailure(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "apply-failed", Priority: 100, Address: "203.0.113.11", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.RecordNodeReport(token, NodeReport{BootID: "apply-failed", Sequence: 1, InterfaceName: "eth0", ApplyError: "invalid route"}, now); err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(reviewRoute(0, "apply-failed.example.test", "https://origin.example.test").Site)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "global", 0, now); err != nil {
		t.Fatal(err)
	}
	if err := app.refreshSiteAssignments(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	schedule, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.DesiredNodeID != node.ID {
		t.Fatalf("apply failure cleared desired assignment: got %d want %d", schedule.DesiredNodeID, node.ID)
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
	if gotIn != 500 || gotOut != 600 || gotRequests != 14 {
		t.Fatalf("site traffic after Agent restart = in %d out %d requests %d, want 500/600/14 (including first report)", gotIn, gotOut, gotRequests)
	}
}

func TestReviewSiteTrafficCounterSeparatesRepeatedConfigEpochs(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "site-counter-epochs", Address: "203.0.113.23", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "site-counter-epochs", PublicHost: "site-counter-epochs.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18081"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET desired_node_id=? WHERE site_id=?", node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	report := func(sequence int64, epoch string, in, out, requests int64, at time.Time) {
		t.Helper()
		_, reportErr := app.db.RecordNodeReport(token, NodeReport{
			BootID: "same-kernel", ReportSessionID: "same-session", CounterEpoch: "kernel:eth0", SiteCounterEpoch: epoch,
			Sequence: sequence, InterfaceName: "eth0", SiteStats: []NodeSiteStat{{
				Host: site.PublicHost, BytesIn: in, BytesOut: out, CumulativeBytesIn: in, CumulativeBytesOut: out,
				RequestCount: requests, LastRequestAtMS: at.UnixMilli(), LastStatus: 200,
			}},
		}, at)
		if reportErr != nil {
			t.Fatal(reportErr)
		}
	}
	report(1, "1", 1000, 2000, 10, now)
	// The runtime returns to an earlier configuration hash after a second
	// transition. Each monotonic Agent-side epoch must still count its first
	// post-apply sample rather than treating it as a duplicate baseline.
	report(2, "2", 100, 200, 5, now.Add(time.Second))
	report(3, "3", 50, 80, 3, now.Add(2*time.Second))
	var gotIn, gotOut, gotRequests int64
	if err := app.db.db.QueryRow("SELECT COALESCE(SUM(bytes_in),0),COALESCE(SUM(bytes_out),0),COALESCE(SUM(requests),0) FROM node_site_traffic_logs WHERE node_id=? AND site_id=?", node.ID, site.ID).Scan(&gotIn, &gotOut, &gotRequests); err != nil {
		t.Fatal(err)
	}
	if gotIn != 1150 || gotOut != 2280 || gotRequests != 18 {
		t.Fatalf("repeated config epochs lost first deltas: in=%d out=%d requests=%d, want 1150/2280/18", gotIn, gotOut, gotRequests)
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
		SiteStats:    []NodeSiteStat{{SiteID: siteB.ID, Host: siteB.PublicHost, RequestCount: 1, LastRequestAtMS: now.UnixMilli(), LastStatus: 200}},
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
	if len(result.DiscardedSiteIDs) != 1 || result.DiscardedSiteIDs[0] != siteB.ID || len(result.DiscardedMediaSiteIDs) != 1 || result.DiscardedMediaSiteIDs[0] != siteB.ID || len(result.DiscardedObservationSiteIDs) != 1 || result.DiscardedObservationSiteIDs[0] != siteB.ID {
		t.Fatalf("controller did not return explicit telemetry dispositions: %#v", result)
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

func TestReviewCompletedDNSMoveKeepsTailTrafficAuthorizedOnlyDuringDrain(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	oldNode, oldEnrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "drain-old", Address: "203.0.113.70", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	newNode, newEnrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "drain-new", Address: "203.0.113.71", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, oldToken, err := app.db.EnrollControlNode(oldEnrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNode(newEnrollment, now); err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "drain-site", PublicHost: "drain.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", newNode.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=? WHERE site_id=?`, newNode.ID, newNode.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`INSERT INTO site_node_drains(site_id,node_id,expires_at_ms,created_at_ms) VALUES(?,?,?,?)`, site.ID, oldNode.ID, now.Add(time.Minute).UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	report := NodeReport{BootID: "drain", ReportSessionID: "drain", CounterEpoch: "epoch", SiteCounterEpoch: "drain:1", Sequence: 1, InterfaceName: "eth0", SiteStats: []NodeSiteStat{{SiteID: site.ID, Host: site.PublicHost, RequestCount: 1, LastRequestAtMS: now.UnixMilli(), LastStatus: 200, BytesOut: 100, CumulativeBytesOut: 100, Final: true}}}
	result, err := app.db.RecordNodeReportResult(oldToken, report, now)
	if err != nil || len(result.AcceptedSiteIDs) != 1 || result.AcceptedSiteIDs[0] != site.ID {
		t.Fatalf("draining tail report=%#v err=%v, want accepted", result, err)
	}
	var activeDrains int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM site_node_drains WHERE site_id=? AND node_id=?", site.ID, oldNode.ID).Scan(&activeDrains); err != nil {
		t.Fatal(err)
	}
	if activeDrains != 1 {
		t.Fatalf("Final SiteStat retired drain before tail queues were flushed: %d", activeDrains)
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_drains SET acked_at_ms=?,finalization_expires_at_ms=? WHERE site_id=?`, now.UnixMilli(), now.Add(-time.Second).UnixMilli(), site.ID); err != nil {
		t.Fatal(err)
	}
	report.Sequence = 2
	result, err = app.db.RecordNodeReportResult(oldToken, report, now.Add(time.Second))
	if err != nil || len(result.DiscardedSiteIDs) != 1 || result.DiscardedSiteIDs[0] != site.ID {
		t.Fatalf("expired drain report=%#v err=%v, want discarded", result, err)
	}
}

func TestReviewRevocationAndDrainExpiryStartAfterAgentAck(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "ack-lifecycle", Address: "203.0.113.240", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNode(enrollment, now); err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "ack-lifecycle-site", PublicHost: "ack-lifecycle.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE sites SET enabled=0 WHERE id=?`, site.ID); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-48 * time.Hour).UnixMilli()
	if _, err := app.db.db.Exec(`INSERT INTO agent_route_revocations(node_id,site_id,public_host,created_at_ms) VALUES(?,?,?,?)`, node.ID, site.ID, site.PublicHost, old); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`INSERT INTO site_node_drains(site_id,node_id,public_host,expires_at_ms,created_at_ms) VALUES(?,?,?,?,?)`, site.ID, node.ID, site.PublicHost, old, old); err != nil {
		t.Fatal(err)
	}
	lookup := func(at time.Time) map[int64]authorizedNodeSite {
		t.Helper()
		tx, txErr := app.db.db.Begin()
		if txErr != nil {
			t.Fatal(txErr)
		}
		defer tx.Rollback()
		allowed, lookupErr := authorizedNodeSitesTx(tx, node.ID, at.UnixMilli())
		if lookupErr != nil {
			t.Fatal(lookupErr)
		}
		return allowed
	}
	if _, ok := lookup(now)[site.ID]; !ok {
		t.Fatal("unacknowledged revocation/drain was lost based on its old creation time")
	}
	ack := now.UnixMilli()
	finalize := now.Add(siteNodeDrainWindow).UnixMilli()
	if _, err := app.db.db.Exec(`UPDATE agent_route_revocations SET acked_at_ms=?,finalization_expires_at_ms=? WHERE node_id=? AND site_id=?`, ack, finalize, node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_drains SET acked_at_ms=?,finalization_expires_at_ms=? WHERE node_id=? AND site_id=?`, ack, finalize, node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookup(now.Add(23 * time.Hour))[site.ID]; !ok {
		t.Fatal("acked revocation/drain disappeared before finalization window")
	}
	if _, ok := lookup(now.Add(25 * time.Hour))[site.ID]; ok {
		t.Fatal("acked revocation/drain remained after finalization window")
	}
}

func TestReviewLegacyForceStopFallbackRevokesOldCredential(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "legacy-force-stop", Address: "203.0.113.241", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "legacy-force-stop-site", PublicHost: "legacy-force-stop.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET agent_version=? WHERE id=?`, "v1.9.64", node.ID); err != nil {
		t.Fatal(err)
	}
	created := now.Add(-legacyForceStopGrace - time.Second).UnixMilli()
	if _, err := app.db.db.Exec(`INSERT INTO agent_route_revocations(node_id,site_id,public_host,created_at_ms) VALUES(?,?,?,?)`, node.ID, site.ID, site.PublicHost, created); err != nil {
		t.Fatal(err)
	}
	if err := app.revokeLegacyAgentsWithPendingForceStops(now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.nodeByAgentToken(token, now); !errors.Is(err, errInvalidAgentToken) {
		t.Fatalf("legacy Agent credential remained valid after fallback: %v", err)
	}
}

func TestReviewDeletingDrainOnlyNodeLeavesCurrentAssignmentUntouched(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	oldNode, oldEnrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "delete-drain-only-old", Address: "203.0.113.180", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	newNode, newEnrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "delete-drain-only-new", Address: "203.0.113.181", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNode(oldEnrollment, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNode(newEnrollment, now); err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "delete-drain-only-site", PublicHost: "delete-drain-only.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", newNode.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=?,dns_status='active' WHERE site_id=?`, newNode.ID, newNode.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`INSERT INTO site_node_drains(site_id,node_id,expires_at_ms,created_at_ms) VALUES(?,?,?,?)`, site.ID, oldNode.ID, now.Add(time.Hour).UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := app.prepareNodeDeletion(context.Background(), oldNode.ID); err != nil {
		t.Fatalf("prepare drain-only deletion: %v", err)
	}
	schedule, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !schedule.Enabled || schedule.DesiredNodeID != newNode.ID || schedule.AppliedNodeID != newNode.ID || schedule.DNSStatus != "active" {
		t.Fatalf("drain-only deletion changed current schedule: %#v", schedule)
	}
	var revocations int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM agent_route_revocations WHERE site_id=? AND node_id=?", site.ID, newNode.ID).Scan(&revocations); err != nil {
		t.Fatal(err)
	}
	if revocations != 0 {
		t.Fatalf("drain-only deletion created a revocation for current node: %d", revocations)
	}
}

func TestReviewDisablingSiteDirtiesCurrentAndDrainNodes(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	oldNode, oldEnrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "disable-drain-old", Address: "203.0.113.182", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	newNode, newEnrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "disable-drain-new", Address: "203.0.113.183", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNode(oldEnrollment, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNode(newEnrollment, now); err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "disable-drain-site", PublicHost: "disable-drain.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", newNode.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=? WHERE site_id=?`, newNode.ID, newNode.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`INSERT INTO site_node_drains(site_id,node_id,expires_at_ms,created_at_ms) VALUES(?,?,?,?)`, site.ID, oldNode.ID, now.Add(time.Hour).UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE control_nodes SET config_dirty=0 WHERE id IN (?,?)", oldNode.ID, newNode.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.db.SetSiteEnabled(site.ID, false); err != nil {
		t.Fatal(err)
	}
	for _, nodeID := range []int64{oldNode.ID, newNode.ID} {
		var dirty int
		if err := app.db.db.QueryRow("SELECT config_dirty FROM control_nodes WHERE id=?", nodeID).Scan(&dirty); err != nil {
			t.Fatal(err)
		}
		if dirty != 1 {
			t.Fatalf("node %d was not dirtied when disabling site", nodeID)
		}
	}
}

func TestReviewDisabledNodeForceStopInvalidation(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "disabled-force-stop", Address: "203.0.113.242", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNode(enrollment, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET enabled=0,config_revision=7,config_dirty=0 WHERE id=?`, node.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := app.db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := markAgentConfigsDirtyForNodeIDsTx(tx, node.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var revision, dirty int64
	if err := app.db.db.QueryRow(`SELECT config_revision,config_dirty FROM control_nodes WHERE id=?`, node.ID).Scan(&revision, &dirty); err != nil {
		t.Fatal(err)
	}
	if revision != 8 || dirty != 1 {
		t.Fatalf("disabled enrolled Agent was not invalidated: revision=%d dirty=%d", revision, dirty)
	}
}

func TestReviewLegacyDisabledNodeForceStopFallback(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "legacy-disabled-force-stop", Address: "203.0.113.243", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "legacy-disabled-site", PublicHost: "legacy-disabled.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	created := now.Add(-legacyForceStopGrace - time.Second).UnixMilli()
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET enabled=0,agent_version=? WHERE id=?`, "v1.9.64", node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`INSERT INTO agent_route_revocations(node_id,site_id,public_host,created_at_ms) VALUES(?,?,?,?)`, node.ID, site.ID, site.PublicHost, created); err != nil {
		t.Fatal(err)
	}
	if err := app.revokeLegacyAgentsWithPendingForceStops(now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.nodeByAgentToken(token, now); !errors.Is(err, errInvalidAgentToken) {
		t.Fatalf("disabled legacy Agent credential remained valid after fallback: %v", err)
	}
}

func TestReviewDrainLifecycleResetsOnABABTransition(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "drain-reused", Address: "203.0.113.244", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "drain-reused-site", PublicHost: "drain-reused.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	ack := now.Add(-23 * time.Hour)
	if _, err := app.db.db.Exec(`INSERT INTO site_node_drains(site_id,node_id,public_host,expires_at_ms,created_at_ms,acked_at_ms,finalization_expires_at_ms) VALUES(?,?,?,?,?,?,?)`, site.ID, node.ID, site.PublicHost, ack.Add(siteNodeDrainWindow).UnixMilli(), now.Add(-24*time.Hour).UnixMilli(), ack.UnixMilli(), ack.Add(siteNodeDrainWindow).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	tx, err := app.db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertSiteNodeDrainTx(tx, site.ID, node.ID, site.PublicHost, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var gotAck, gotFinal int64
	if err := app.db.db.QueryRow(`SELECT acked_at_ms,finalization_expires_at_ms FROM site_node_drains WHERE site_id=? AND node_id=?`, site.ID, node.ID).Scan(&gotAck, &gotFinal); err != nil {
		t.Fatal(err)
	}
	if gotAck != 0 || gotFinal != 0 {
		t.Fatalf("reused drain inherited prior lifecycle: ack=%d final=%d", gotAck, gotFinal)
	}
}

func TestReviewDrainPreservesMultipleHostGenerations(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "drain-generations", Address: "203.0.113.246", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "drain-generations-site", PublicHost: "h3.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=? WHERE site_id=?`, node.ID, node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	upsert := func(host string, at time.Time) {
		t.Helper()
		tx, txErr := app.db.db.Begin()
		if txErr != nil {
			t.Fatal(txErr)
		}
		if err := upsertSiteNodeDrainTx(tx, site.ID, node.ID, host, at); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	upsert("h1.example.test", now)
	upsert("h2.example.test", now.Add(time.Minute))
	upsert("h3.example.test", now.Add(2*time.Minute))

	var drainHost string
	if err := app.db.db.QueryRow("SELECT public_host FROM site_node_drains WHERE site_id=? AND node_id=?", site.ID, node.ID).Scan(&drainHost); err != nil {
		t.Fatal(err)
	}
	if drainHost != "h3.example.test" {
		t.Fatalf("latest drain host=%q, want h3.example.test", drainHost)
	}
	rows, err := app.db.db.Query("SELECT public_host,acked_at_ms,finalization_expires_at_ms FROM site_node_host_aliases WHERE site_id=? AND node_id=? ORDER BY public_host", site.ID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := make(map[string][2]int64)
	for rows.Next() {
		var host string
		var acked, finalized int64
		if err := rows.Scan(&host, &acked, &finalized); err != nil {
			t.Fatal(err)
		}
		seen[host] = [2]int64{acked, finalized}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"h1.example.test", "h2.example.test"} {
		lifecycle, ok := seen[host]
		if !ok {
			t.Fatalf("missing preserved drain generation %q", host)
		}
		if lifecycle != [2]int64{} {
			t.Fatalf("generation %q unexpectedly acknowledged: %#v", host, lifecycle)
		}
	}
	tx, err := app.db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	allowed, err := authorizedNodeSitesTx(tx, node.ID, now.Add(3*time.Minute).UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"h1.example.test", "h2.example.test", "h3.example.test"} {
		if !authorizedNodeSiteHost(allowed, site.ID, host) {
			t.Fatalf("host generation %q was not authorized", host)
		}
	}
}

func TestReviewSchema46RepairsLegacyAliasLifecycle(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "alias-migration", Address: "203.0.113.247", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "alias-migration-site", PublicHost: "new-alias.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	created := now.Add(-time.Hour).UnixMilli()
	expires := now.Add(-time.Minute).UnixMilli()
	if _, err := app.db.db.Exec(`INSERT INTO site_node_host_aliases(site_id,node_id,public_host,expires_at_ms,created_at_ms,acked_at_ms,finalization_expires_at_ms) VALUES(?,?,?,?,?,?,?)`, site.ID, node.ID, "old-alias.example.test", expires, created, created, expires); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("PRAGMA user_version = 45"); err != nil {
		t.Fatal(err)
	}
	if err := app.db.migrate(); err != nil {
		t.Fatal(err)
	}
	var acked, finalized int64
	if err := app.db.db.QueryRow(`SELECT acked_at_ms,finalization_expires_at_ms FROM site_node_host_aliases WHERE site_id=? AND node_id=?`, site.ID, node.ID).Scan(&acked, &finalized); err != nil {
		t.Fatal(err)
	}
	if acked != 0 || finalized != 0 {
		t.Fatalf("schema 46 did not repair legacy alias lifecycle: ack=%d final=%d", acked, finalized)
	}
}

func TestReviewHostAliasLifecycleStartsOnConfigAck(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "host-alias-lifecycle", Address: "203.0.113.245", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "host-alias-site", PublicHost: "new-host.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=?,config_hash=? WHERE site_id=?`, node.ID, node.ID, "alias-config", site.ID); err != nil {
		t.Fatal(err)
	}
	oldHost := "old-host.example.test"
	if _, err := app.db.db.Exec(`INSERT INTO site_node_host_aliases(site_id,node_id,public_host,expires_at_ms,created_at_ms,acked_at_ms,finalization_expires_at_ms) VALUES(?,?,?,?,?,?,?)`, site.ID, node.ID, oldHost, now.Add(-time.Hour).UnixMilli(), now.Add(-48*time.Hour).UnixMilli(), 0, 0); err != nil {
		t.Fatal(err)
	}
	lookup := func(at time.Time) bool {
		t.Helper()
		tx, txErr := app.db.db.Begin()
		if txErr != nil {
			t.Fatal(txErr)
		}
		defer tx.Rollback()
		allowed, lookupErr := authorizedNodeSitesTx(tx, node.ID, at.UnixMilli())
		if lookupErr != nil {
			t.Fatal(lookupErr)
		}
		return authorizedNodeSiteHost(allowed, site.ID, oldHost)
	}
	if !lookup(now.Add(48 * time.Hour)) {
		t.Fatal("unacknowledged host alias expired from creation-time TTL")
	}
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET config_revision=3,desired_config_revision=3,desired_config_hash=?,applied_config_hash='' WHERE id=?`, "alias-config", node.ID); err != nil {
		t.Fatal(err)
	}
	report := NodeReport{BootID: "alias-boot", ReportSessionID: "alias-session", CounterEpoch: "alias-epoch", AppliedConfigHash: "alias-config", AppliedConfigRevision: 3, AgentVersion: "v1.9.69", Sequence: 1, InterfaceName: "eth0"}
	if _, err := app.db.RecordNodeReportResult(token, report, now); err != nil {
		t.Fatal(err)
	}
	var acked, finalized int64
	if err := app.db.db.QueryRow(`SELECT acked_at_ms,finalization_expires_at_ms FROM site_node_host_aliases WHERE site_id=? AND node_id=?`, site.ID, node.ID).Scan(&acked, &finalized); err != nil {
		t.Fatal(err)
	}
	if acked != now.UnixMilli() || finalized != now.Add(siteNodeDrainWindow).UnixMilli() {
		t.Fatalf("host alias lifecycle did not start on ACK: ack=%d final=%d", acked, finalized)
	}
	if !lookup(now.Add(23 * time.Hour)) {
		t.Fatal("acked host alias disappeared before finalization")
	}
	if lookup(now.Add(25 * time.Hour)) {
		t.Fatal("acked host alias remained after finalization")
	}
}

func TestReviewFailedSiteHostUpdateRestoresAliasSnapshot(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "rollback-alias-node", Address: "203.0.113.246", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "rollback-alias-site", PublicHost: "stable.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	created, expires, acked, finalized := now.Add(-time.Hour).UnixMilli(), now.Add(time.Hour).UnixMilli(), now.Add(-time.Minute).UnixMilli(), now.Add(30*time.Minute).UnixMilli()
	if _, err := app.db.db.Exec(`INSERT INTO site_node_host_aliases(site_id,node_id,public_host,expires_at_ms,created_at_ms,acked_at_ms,finalization_expires_at_ms) VALUES(?,?,?,?,?,?,?)`, site.ID, node.ID, "legacy.example.test", expires, created, acked, finalized); err != nil {
		t.Fatal(err)
	}
	snapshot, err := app.db.snapshotSiteUpdate(*site)
	if err != nil {
		t.Fatal(err)
	}
	candidate := *site
	candidate.PublicHost = "candidate.example.test"
	if err := app.db.UpdateSiteRecord(candidate); err != nil {
		t.Fatal(err)
	}
	if err := app.db.restoreSiteSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	restored, err := app.db.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.PublicHost != "stable.example.test" {
		t.Fatalf("restored public host=%q, want stable.example.test", restored.PublicHost)
	}
	var count int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM site_node_host_aliases WHERE site_id=? AND public_host=?", site.ID, "candidate.example.test").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed candidate host left %d alias rows", count)
	}
	var gotNode int64
	var gotHost string
	var gotExpires, gotCreated, gotAcked, gotFinalized int64
	if err := app.db.db.QueryRow(`SELECT node_id,public_host,expires_at_ms,created_at_ms,acked_at_ms,finalization_expires_at_ms FROM site_node_host_aliases WHERE site_id=?`, site.ID).Scan(&gotNode, &gotHost, &gotExpires, &gotCreated, &gotAcked, &gotFinalized); err != nil {
		t.Fatal(err)
	}
	if gotNode != node.ID || gotHost != "legacy.example.test" || gotExpires != expires || gotCreated != created || gotAcked != acked || gotFinalized != finalized {
		t.Fatalf("alias lifecycle changed during rollback: node=%d host=%q expires=%d created=%d acked=%d finalized=%d", gotNode, gotHost, gotExpires, gotCreated, gotAcked, gotFinalized)
	}
}

func TestReviewScheduledSiteRemovalInvalidatesOwningAgent(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "remove-owner", Address: "203.0.113.91", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNode(enrollment, now); err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "remove-site", PublicHost: "remove.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=?,config_hash='applied' WHERE site_id=?`, node.ID, node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.db.db.QueryRow("SELECT config_dirty FROM control_nodes WHERE id=?", node.ID).Scan(new(int)); err != nil {
		t.Fatal(err)
	}
	if err := app.removeSiteNodeSchedule(context.Background(), site.ID); err != nil {
		t.Fatal(err)
	}
	var dirty int
	var desiredHash string
	if err := app.db.db.QueryRow("SELECT config_dirty,desired_config_hash FROM control_nodes WHERE id=?", node.ID).Scan(&dirty, &desiredHash); err != nil {
		t.Fatal(err)
	}
	if dirty != 1 || desiredHash != "" {
		t.Fatalf("owning Agent was not invalidated on site removal: dirty=%d hash=%q", dirty, desiredHash)
	}
	var enabled int
	if err := app.db.db.QueryRow("SELECT enabled FROM site_node_schedules WHERE site_id=?", site.ID).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 {
		t.Fatalf("site schedule remained enabled after removal: %d", enabled)
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

func TestLegacyTLSMigrationRejectsIntermediateSymlinks(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meridian.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	t.Setenv("PANEL_TLS_CERT_FILE", "")
	t.Setenv("PANEL_TLS_KEY_FILE", "")
	t.Setenv("EDGE_TLS_CERT_FILE", filepath.Join(dir, "external", "edge.pem"))
	t.Setenv("EDGE_TLS_KEY_FILE", filepath.Join(dir, "external", "edge.key"))
	stateDir := filepath.Join(dir, "owned-tls")
	t.Setenv("TLS_STATE_DIR", stateDir)
	node, _, err := db.CreateControlNode(NodeCreateInput{Name: "symlink-edge", Address: "203.0.113.72"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	legacyRoot := filepath.Join(dir, "external", "edge-nodes")
	if err := os.MkdirAll(legacyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM := reviewCertificatePEM(t)
	outside := filepath.Join(dir, "outside", "current")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "fullchain.pem"), []byte(certPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "privkey.pem"), []byte(keyPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(legacyRoot, node.GUID)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := migrateLegacyEdgeTLSState(db, dbPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "edge-nodes", node.GUID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlinked GUID directory was migrated: %v", err)
	}
	// Replace the GUID link with a real directory and make only current a link.
	if err := os.Remove(filepath.Join(legacyRoot, node.GUID)); err != nil {
		t.Fatal(err)
	}
	currentLinkRoot := filepath.Join(legacyRoot, node.GUID, "current")
	if err := os.MkdirAll(filepath.Dir(currentLinkRoot), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, currentLinkRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := migrateLegacyEdgeTLSState(db, dbPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "edge-nodes", node.GUID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("symlinked current directory was migrated: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "fullchain.pem")); err != nil {
		t.Fatalf("outside TLS file was modified or removed: %v", err)
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
