package main

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testEdgeRuntimeKey(t *testing.T) string {
	t.Helper()
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func TestEdgeEventSpoolUsesIndependentKeyAcrossReenrollment(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	var first edgeEventStore
	if err := first.initWithRawKey(dir, key, nil); err != nil {
		t.Fatal(err)
	}
	if err := first.add(NodeRequestEvent{SiteID: 1, Host: "media.example.test", Method: "GET", Path: "/Sessions/Playing", StatusCode: 200, RecordedAtMS: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	var restored edgeEventStore
	if err := restored.initWithRawKey(dir, key, nil); err != nil {
		t.Fatal(err)
	}
	if len(restored.snapshot()) != 1 {
		t.Fatal("event spool did not survive an agent token change")
	}
}

func TestEdgeEventQueueRetainsCriticalEventsWhenFull(t *testing.T) {
	var store edgeEventStore
	for i := 0; i < edgeEventQueueLimit; i++ {
		if err := store.add(NodeRequestEvent{SiteID: 1, Host: "media.example.test", Method: "GET", Path: "/api/Items", ResourceCategory: requestLogCategoryAPI, StatusCode: 200, RecordedAtMS: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.add(NodeRequestEvent{SiteID: 1, Host: "media.example.test", Method: "POST", Path: "/Sessions/Playing/Progress", ResourceCategory: requestLogCategoryPlaybackSync, StatusCode: 204, RecordedAtMS: edgeEventQueueLimit + 1}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	foundCritical := false
	for _, event := range store.items {
		if event.ResourceCategory == requestLogCategoryPlaybackSync {
			foundCritical = true
			break
		}
	}
	if !foundCritical {
		t.Fatal("critical playback event was dropped when queue was full")
	}
}

func TestEdgeObserverMarksReplayEventsCritical(t *testing.T) {
	dir := t.TempDir()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	runtime := &edgeAgentRuntime{}
	if err := runtime.events.initWithRawKey(dir, key, nil); err != nil {
		t.Fatal(err)
	}
	handler := runtime.observe(map[string]int64{"media.example.test": 7}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "https://media.example.test/Sessions/Playing", strings.NewReader(`{"ItemId":"1"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	events := runtime.events.snapshot()
	if len(events) != 1 || events[0].Priority != nodeEventPriorityCritical {
		t.Fatalf("observer event priority = %#v, want critical", events)
	}
}

func TestEdgeEventSpoolCorruptionCanBeQuarantined(t *testing.T) {
	dir := t.TempDir()
	spoolDir := filepath.Join(dir, "events")
	if err := os.MkdirAll(spoolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(spoolDir, "events.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"nonce":"bad","ciphertext":"bad"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	var store edgeEventStore
	if err := store.initWithRawKey(spoolDir, key, nil); err == nil {
		t.Fatal("corrupt spool unexpectedly loaded")
	}
	if err := quarantineEventSpool(path); err != nil {
		t.Fatal(err)
	}
	if err := store.initWithRawKey(spoolDir, key, nil); err != nil {
		t.Fatalf("reinitialize after quarantine: %v", err)
	}
}

func TestBuildNodeInstallCommandUsesControllerInstaller(t *testing.T) {
	command := buildNodeInstallCommand("https://panel.example.test:9090", "token")
	if strings.Contains(command, "raw.githubusercontent.com") || strings.Contains(command, "/main/") {
		t.Fatalf("installer command still uses mutable GitHub main: %s", command)
	}
	if !strings.Contains(command, "https://panel.example.test:9090/api/agent/install.sh") {
		t.Fatalf("installer command does not use controller endpoint: %s", command)
	}
}

func TestEdgeProxyRewritesAndServesDynamicPlaybackBackend(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/Items/1/PlaybackInfo" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"MediaSources":[{"DirectStreamUrl":"https://cdn.example.com/movie.mp4?sig=edge-secret"}]}`)
	}))
	defer upstream.Close()
	captures := make(chan redirectRuntimeDialCapture, 1)
	runtime := &edgeAgentRuntime{
		stateDir: t.TempDir(),
		resolver: dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
		}),
		transport: redirectRuntimeFactory(captures, func(*http.Request) string {
			return "HTTP/1.1 200 OK\r\nContent-Type: video/mp4\r\nContent-Length: 8\r\n\r\nedge-ok!"
		}),
	}
	config := AgentRuntimeConfig{
		SchemaVersion: 1, NodeGUID: "edge-node", HTTPSPort: 9090, DynamicKey: testEdgeRuntimeKey(t),
		Routes: []AgentSiteRoute{{
			SiteID: 51, Host: "dynamic.example.test", TargetURL: upstream.URL, PlaybackMode: "direct",
			Site: Site{Name: "dynamic", PublicHost: "dynamic.example.test", IngressMode: ingressModeHost,
				TargetURL: upstream.URL, PlaybackMode: "direct", MainVideoStreamMode: "proxy", StreamHosts: "[]",
				UAMode: passthroughUAMode, ClientIPMode: clientIPModeBoth, DynamicDiscoveryEnabled: true,
				DynamicProfile: dynamicProfileCompatible, DynamicDiscoverySources: allDynamicDiscoverySources()},
			FailoverTargets: "[]", StreamHostsRaw: "[]", DynamicSources: `["redirect","playback_info","hls","dash"]`, DynamicRules: "[]",
		}},
	}
	bundle, err := buildEdgeProxy(config, runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer bundle.close()
	playbackInfo := httptest.NewRecorder()
	bundle.handler.ServeHTTP(playbackInfo, httptest.NewRequest(http.MethodGet, "https://dynamic.example.test/Items/1/PlaybackInfo", nil))
	body := playbackInfo.Body.String()
	if playbackInfo.Code != http.StatusOK || strings.Contains(body, "cdn.example.com") || strings.Contains(body, "edge-secret") {
		t.Fatalf("dynamic backend leaked: status=%d body=%s", playbackInfo.Code, body)
	}
	start := strings.Index(body, dynamicRoutePrefix)
	if start < 0 {
		t.Fatalf("PlaybackInfo has no dynamic capability: %s", body)
	}
	end := strings.IndexByte(body[start:], '"')
	if end < 0 {
		t.Fatalf("dynamic capability is not JSON-delimited: %s", body)
	}
	media := httptest.NewRecorder()
	bundle.handler.ServeHTTP(media, httptest.NewRequest(http.MethodGet, "https://dynamic.example.test"+body[start:start+end], nil))
	if media.Code != http.StatusOK || media.Body.String() != "edge-ok!" {
		t.Fatalf("dynamic media = %d %q", media.Code, media.Body.String())
	}
	select {
	case capture := <-captures:
		if capture.err != nil || capture.address != "1.1.1.1:443" || capture.request.URL.RequestURI() != "/movie.mp4?sig=edge-secret" {
			t.Fatalf("dynamic edge request = %#v", capture)
		}
	case <-time.After(time.Second):
		t.Fatal("dynamic edge request did not use the pinned transport")
	}
}

func TestEdgeProxyReusesPrimaryPlaybackAndHeaderPolicies(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Origin-Secret") != "configured" {
			t.Errorf("primary request missing runtime header")
		}
		_, _ = io.WriteString(w, "api")
	}))
	defer api.Close()
	playback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Origin-Secret") != "" {
			t.Errorf("origin-bound header leaked to playback authority")
		}
		_, _ = io.WriteString(w, "media")
	}))
	defer playback.Close()
	runtime := &edgeAgentRuntime{stateDir: t.TempDir()}
	config := AgentRuntimeConfig{
		SchemaVersion: 1, NodeGUID: "edge-node", HTTPSPort: 9090, DynamicKey: testEdgeRuntimeKey(t),
		Routes: []AgentSiteRoute{{
			SiteID: 41, Host: "edge.example.test", TargetURL: api.URL, PlaybackTargetURL: playback.URL, PlaybackMode: "direct",
			Headers: map[string][]string{"X-Origin-Secret": {"configured"}},
			Site: Site{Name: "edge", PublicHost: "edge.example.test", IngressMode: ingressModeHost,
				TargetURL: api.URL, PlaybackTargetURL: playback.URL, PlaybackMode: "direct", MainVideoStreamMode: "proxy",
				StreamHosts: "[]", UAMode: passthroughUAMode, ClientIPMode: clientIPModeBoth,
				DynamicProfile: dynamicProfileSafe, DynamicDiscoverySources: defaultDynamicDiscoverySources(), DynamicDomainRules: []DynamicDomainRule{}},
			FailoverTargets: "[]", StreamHostsRaw: "[]", DynamicSources: `["redirect","playback_info","hls","dash"]`, DynamicRules: "[]",
		}},
	}
	bundle, err := buildEdgeProxy(config, runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer bundle.close()
	for _, test := range []struct{ path, want string }{
		{path: "/Items/abc", want: "api"},
		{path: "/Videos/abc/file", want: "media"},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "https://edge.example.test"+test.path, nil)
		bundle.handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || response.Body.String() != test.want {
			t.Fatalf("%s = %d %q, want %q", test.path, response.Code, response.Body.String(), test.want)
		}
	}
	events := runtime.events.snapshot()
	if len(events) != 2 || events[0].SiteID != 41 || events[1].SiteID != 41 {
		t.Fatalf("central Agent events = %#v", events)
	}
	if events[0].ResourceCategory != requestLogCategoryMetadata || events[1].ResourceCategory != requestLogCategoryStream || events[1].BackendAddress == "" {
		t.Fatalf("Agent did not preserve proxy log classification/backend: %#v", events)
	}
}

func TestDerivedNodeRuntimeKeysAreStableAndNodeScoped(t *testing.T) {
	master := make([]byte, 32)
	if _, err := rand.Read(master); err != nil {
		t.Fatal(err)
	}
	first := deriveNodeRuntimeKey(master, "node-a", "dynamic-routes")
	second := deriveNodeRuntimeKey(master, "node-a", "dynamic-routes")
	other := deriveNodeRuntimeKey(master, "node-b", "dynamic-routes")
	if len(first) != 32 || string(first) != string(second) || string(first) == string(other) {
		t.Fatalf("derived keys are not stable and node-scoped")
	}
}

func TestBuildAgentConfigCarriesCompleteDynamicSiteWithoutNestedQueryDeadlock(t *testing.T) {
	app := newTestApp(t)
	app.dynamicRouteKey = make([]byte, 32)
	if _, err := rand.Read(app.dynamicRouteKey); err != nil {
		t.Fatal(err)
	}
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tlsServer.Close()
	certificate := tlsServer.TLS.Certificates[0]
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})
	keyDER, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	tlsDir := t.TempDir()
	certFile, keyFile := filepath.Join(tlsDir, "fullchain.pem"), filepath.Join(tlsDir, "privkey.pem")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	app.panelCertificates = &panelCertificateManager{certFile: certFile, keyFile: keyFile, edgeCertFile: certFile, edgeKeyFile: keyFile, accountDir: tlsDir}
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "edge", Address: "203.0.113.10", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	edgeCertFile, edgeKeyFile, err := app.panelCertificates.nodeEdgeTLSPaths(node.GUID)
	if err != nil {
		t.Fatal(err)
	}
	if err := installCertificatePairAtomic(edgeCertFile, edgeKeyFile, certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.RecordNodeReport(token, NodeReport{BootID: "boot", Sequence: 1, InterfaceName: "eth0"}, now); err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{
		Name: "dynamic", PublicHost: "dynamic.example.test", IngressMode: ingressModeHost, TargetURL: "https://origin.example.test",
		PlaybackMode: "direct", MainVideoStreamMode: "proxy", StreamHosts: "[]", UAMode: passthroughUAMode,
		DynamicDiscoveryEnabled: true, DynamicProfile: dynamicProfileCompatible,
		DynamicDiscoverySources: allDynamicDiscoverySources(), DynamicDomainRules: []DynamicDomainRule{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	result := make(chan AgentRuntimeConfig, 1)
	errorsCh := make(chan error, 1)
	go func() {
		config, buildErr := app.buildAgentConfig(token, now.Add(time.Second))
		if buildErr != nil {
			errorsCh <- buildErr
			return
		}
		result <- config
	}()
	select {
	case err := <-errorsCh:
		t.Fatal(err)
	case config := <-result:
		if len(config.Routes) != 1 || !config.Routes[0].Site.DynamicDiscoveryEnabled || config.DynamicKey == "" {
			t.Fatalf("incomplete runtime config: %#v", config)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("buildAgentConfig deadlocked while expanding routes")
	}
}
