package main

import (
	"crypto/x509"
	"encoding/pem"
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
	_, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "event-dedupe", Address: "203.0.113.20", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	event := NodeRequestEvent{EventUID: strings.Repeat("a", 32), EventID: 1, SiteID: 1, Host: "media.example.test", Method: http.MethodPost, Path: "/Sessions/Playing/Progress", StatusCode: 204, RecordedAtMS: now.UnixMilli(), SkipRequestLog: true}
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
