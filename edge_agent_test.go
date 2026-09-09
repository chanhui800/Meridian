package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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

func TestEdgeAgentRollbackSurfacesListenerRestoreFailure(t *testing.T) {
	cause := errors.New("clear asset cache failed")
	listenErr := errors.New("address already in use")
	runtime := &edgeAgentRuntime{listen: func(string, string) (net.Listener, error) {
		return nil, listenErr
	}}
	oldServer := &http.Server{}
	err := runtime.rollbackApply(edgeAgentRuntimeState{port: 9090, server: oldServer}, true, false, cause)
	if !errors.Is(err, cause) || !errors.Is(err, listenErr) {
		t.Fatalf("rollback error=%v, want cause and listener error", err)
	}
	if runtime.server != nil {
		t.Fatal("runtime retained a server after listener restore failed")
	}
	_, _, listenerError, applyError, _, _ := runtime.status()
	if !strings.Contains(listenerError, "restore old listener on port 9090") {
		t.Fatalf("listener error=%q, want restore failure", listenerError)
	}
	if applyError != "" {
		t.Fatalf("unexpected apply error=%q", applyError)
	}
}

func TestEdgeAgentApplyFailureIsExposedInRuntimeStatus(t *testing.T) {
	runtime := &edgeAgentRuntime{stateDir: t.TempDir()}
	err := runtime.apply(AgentRuntimeConfig{})
	if err == nil {
		t.Fatal("invalid configuration unexpectedly applied")
	}
	_, _, _, applyError, _, _ := runtime.status()
	if applyError == "" || !strings.Contains(applyError, "Agent configuration is invalid") {
		t.Fatalf("apply error=%q, want the runtime apply failure", applyError)
	}
}

func TestAgentConfigIdentityRequiresRevisionAndHash(t *testing.T) {
	base := AgentRuntimeConfig{ConfigHash: "same", ConfigRevision: 7}
	if !agentConfigIdentityEqual(base, AgentRuntimeConfig{ConfigHash: "same", ConfigRevision: 7}) {
		t.Fatal("matching config hash and revision were not recognized")
	}
	if agentConfigIdentityEqual(base, AgentRuntimeConfig{ConfigHash: "same", ConfigRevision: 8}) {
		t.Fatal("different config revision was incorrectly acknowledged")
	}
	if agentConfigIdentityEqual(base, AgentRuntimeConfig{ConfigHash: "other", ConfigRevision: 7}) {
		t.Fatal("different config hash was incorrectly acknowledged")
	}
}

func TestEdgeTrafficCounterSurvivesHotApply(t *testing.T) {
	runtime := &edgeAgentRuntime{}
	first := runtime.trafficCounterFor(42)
	if first == nil {
		t.Fatal("missing stable traffic counter")
	}
	first.cumulativeOut.Store(100)
	second := runtime.trafficCounterFor(42)
	if second != first {
		t.Fatal("hot apply created a new counter for the same site")
	}
	if got := second.cumulativeOut.Load(); got != 100 {
		t.Fatalf("stable counter cumulative bytes = %d, want 100", got)
	}
}

func TestEdgeSiteStatsRetainRemovedRouteUntilControllerAcknowledgesIt(t *testing.T) {
	runtime := &edgeAgentRuntime{}
	counter := runtime.trafficCounterFor(42, "tail.example.test")
	counter.cumulativeIn.Store(123)
	counter.cumulativeOut.Store(456)
	counter.requests.Store(7)
	pending := runtime.prepareSiteStats()
	if len(pending.stats) != 1 || pending.stats[0].SiteID != 42 || pending.stats[0].CumulativeBytesOut != 456 {
		t.Fatalf("tail route stats=%#v, want stable removed-route report", pending.stats)
	}
	runtime.commitSiteStatsWithACK(pending, nil)
	if _, ok := runtime.siteReported[42]; ok {
		t.Fatal("unacknowledged tail stats advanced local watermark")
	}
	runtime.commitSiteStatsWithACK(pending, map[int64]bool{42: true})
	if got := runtime.siteReported[42].CumulativeBytesOut; got != 456 {
		t.Fatalf("acknowledged tail watermark=%d, want 456", got)
	}
}

func TestEdgeTelemetryNeedsCategoryAcknowledgement(t *testing.T) {
	runtime := &edgeAgentRuntime{mediaCounts: map[int64]NodeMediaCount{7: {SiteID: 7, MovieCount: 1, ObservedAtMS: 1}}}
	pending := runtime.prepareTelemetry()
	runtime.commitTelemetryWithACK(pending, nil, nil, nil)
	if _, ok := runtime.mediaCounts[7]; !ok {
		t.Fatal("unacknowledged telemetry was discarded")
	}
	runtime.commitTelemetryWithACK(pending, map[int64]bool{7: true}, nil, nil)
	if _, ok := runtime.mediaCounts[7]; ok {
		t.Fatal("acknowledged telemetry remained pending")
	}
}

func TestEdgeRevocationMarkerFailureStaysQuiescent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := edgeQuiesceRevoked(ctx, &edgeAgentRuntime{}, filepath.Join(t.TempDir(), "missing", "agent.revoked")); err != nil {
		t.Fatalf("quiesce revoked: %v", err)
	}
}

func TestEdgeHotReloadDrainDoesNotForceCloseActiveRequest(t *testing.T) {
	instance := &ProxyInstance{}
	instance.activeRequests.Add(1)
	bundle := &edgeProxyBundle{manager: &ProxyManager{proxies: map[int64]*ProxyInstance{1: instance}}}
	runtime := &edgeAgentRuntime{}
	runtime.beginBundleDrain(bundle)
	deadline := time.Now().Add(time.Second)
	for {
		runtime.mu.RLock()
		_, draining := runtime.drainingBundles[bundle]
		runtime.mu.RUnlock()
		if draining || time.Now().After(deadline) {
			if !draining {
				t.Fatal("hot reload drain ended before its active request completed")
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	instance.activeRequests.Done()
	for time.Now().Before(deadline) {
		runtime.mu.RLock()
		_, draining := runtime.drainingBundles[bundle]
		runtime.mu.RUnlock()
		if !draining {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("hot reload drain did not finish after active request completed")
}

func TestCommitSiteStatsRebasesAuthoritativeQuotaImmediately(t *testing.T) {
	counter := &edgeSiteTrafficCounter{}
	inst := &ProxyInstance{
		Site:                      Site{ID: 1},
		trafficCounter:            counter,
		trafficCycleAuthoritative: true,
		trafficCycleMode:          trafficBillingModeOutbound,
		trafficCycleUsage:         100,
	}
	bundle := &edgeProxyBundle{
		manager:    &ProxyManager{proxies: map[int64]*ProxyInstance{1: inst}},
		localSites: map[int64]edgeSiteIdentity{1: {centralID: 42}},
	}
	runtime := &edgeAgentRuntime{
		bundle:       bundle,
		siteReported: make(map[int64]ProxyRuntimeStat),
	}
	counter.cumulativeOut.Store(10)
	runtime.commitSiteStats(edgeSiteStatsPending{current: map[int64]ProxyRuntimeStat{
		42: {SiteID: 42, CumulativeBytesOut: 10},
	}})
	if got := inst.trafficCycleUsage; got != 110 {
		t.Fatalf("quota baseline after report ACK = %d, want 110", got)
	}
	if got := inst.trafficAckedCumulativeOut; got != 10 {
		t.Fatalf("ACK watermark = %d, want 10", got)
	}
	usage, err := (&ProxyManager{}).currentTrafficCycleUsage(inst, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if usage != 110 {
		t.Fatalf("effective quota usage after report ACK = %d, want 110", usage)
	}
}

func TestEdgeAPIRequestSurfacesRevokedAgentState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Meridian-Agent-State", "revoked")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid agent token","agent_state":"revoked"}`)
	}))
	defer server.Close()
	err := edgeAPIRequest(context.Background(), server.Client(), http.MethodGet, server.URL, "stale-token", nil, nil)
	if !isEdgeAgentRevoked(err) {
		t.Fatalf("revoked response error = %v, want revoked agent state", err)
	}
}

func TestEdgeEnrollmentTokenAvailabilityUsesFileContents(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing-token")
	if edgeEnrollmentTokenAvailable(missing) {
		t.Fatal("missing enrollment token was treated as available")
	}
	empty := filepath.Join(dir, "empty-token")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if edgeEnrollmentTokenAvailable(empty) {
		t.Fatal("empty enrollment token was treated as available")
	}
	valid := filepath.Join(dir, "valid-token")
	if err := os.WriteFile(valid, []byte("one-time-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !edgeEnrollmentTokenAvailable(valid) {
		t.Fatal("non-empty enrollment token was not detected")
	}
}

func TestBuildEdgeProxyIgnoresControllerOnlySiteIconMetadata(t *testing.T) {
	runtime := &edgeAgentRuntime{stateDir: t.TempDir()}
	config := AgentRuntimeConfig{
		SchemaVersion: 1,
		NodeGUID:      "edge-icon-node",
		HTTPSPort:     19090,
		DynamicKey:    testEdgeRuntimeKey(t),
		Routes: []AgentSiteRoute{{
			SiteID:    17,
			Host:      "icon.example.test",
			TargetURL: "http://127.0.0.1:18096",
			Site: Site{
				Name: "Icon site", PublicHost: "icon.example.test", IngressMode: ingressModeHost,
				TargetURL: "http://127.0.0.1:18096", PlaybackMode: "direct", MainVideoStreamMode: "proxy",
				StreamHosts: "[]", UAMode: passthroughUAMode, ClientIPMode: clientIPModeBoth,
				IconName: "Emby", IconURL: "https://icons.example.test/emby.png",
			},
			FailoverTargets: "[]", StreamHostsRaw: "[]", DynamicSources: "[]", DynamicRules: "[]",
		}},
	}
	bundle, err := buildEdgeProxy(config, runtime)
	if err != nil {
		t.Fatalf("build edge proxy with UI icon metadata: %v", err)
	}
	bundle.close()
}

func TestEdgeProxyReportsMediaCountsWithCentralSiteID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isMediaLibraryCountsPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"MovieCount":12,"SeriesCount":34,"EpisodeCount":56}`)
	}))
	defer upstream.Close()
	runtime := &edgeAgentRuntime{stateDir: t.TempDir()}
	config := AgentRuntimeConfig{
		SchemaVersion: 1, NodeGUID: "media-count-node", HTTPSPort: 19090, DynamicKey: testEdgeRuntimeKey(t),
		Routes: []AgentSiteRoute{{
			SiteID: 73, Host: "counts.example.test", TargetURL: upstream.URL,
			Site: Site{
				Name: "counts", PublicHost: "counts.example.test", IngressMode: ingressModeHost,
				TargetURL: upstream.URL, PlaybackMode: "direct", MainVideoStreamMode: "proxy",
				StreamHosts: "[]", UAMode: passthroughUAMode, ClientIPMode: clientIPModeBoth,
			},
			FailoverTargets: "[]", StreamHostsRaw: "[]", DynamicSources: "[]", DynamicRules: "[]",
		}},
	}
	bundle, err := buildEdgeProxy(config, runtime)
	if err != nil {
		t.Fatalf("build edge proxy: %v", err)
	}
	defer bundle.close()
	response := httptest.NewRecorder()
	bundle.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://counts.example.test/Items/Counts", nil))
	if response.Code != http.StatusOK || response.Body.String() != `{"MovieCount":12,"SeriesCount":34,"EpisodeCount":56}` {
		t.Fatalf("media count response = %d %q", response.Code, response.Body.String())
	}
	media, _, _ := runtime.telemetrySnapshot()
	if len(media) != 1 {
		t.Fatalf("media telemetry = %#v, want one event", media)
	}
	if media[0].SiteID != 73 || media[0].MovieCount != 12 || media[0].SeriesCount != 34 || media[0].EpisodeCount != 56 {
		t.Fatalf("media telemetry = %#v, want central site 73 with counts", media[0])
	}
}

func TestEdgeProxyEnforcesControllerTrafficQuotaAfterConfigRefresh(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "should-not-be-reached")
	}))
	defer upstream.Close()
	runtime := &edgeAgentRuntime{stateDir: t.TempDir()}
	settings := defaultSystemSettings()
	cycleStart := trafficCycleStart(time.Now(), settings.TrafficResetDay, timezoneLocation(settings.ScheduleTimezone))
	config := AgentRuntimeConfig{
		SchemaVersion: 1, NodeGUID: "quota-node", HTTPSPort: 19090, DynamicKey: testEdgeRuntimeKey(t),
		Routes: []AgentSiteRoute{{
			SiteID: 74, Host: "quota.example.test", TargetURL: upstream.URL,
			TrafficCycleUsage: 100, TrafficCycleStartMS: cycleStart.UnixMilli(), TrafficBillingMode: trafficBillingModeBidirectional,
			Site: Site{
				Name: "quota", PublicHost: "quota.example.test", IngressMode: ingressModeHost,
				TargetURL: upstream.URL, PlaybackMode: "direct", MainVideoStreamMode: "proxy",
				StreamHosts: "[]", UAMode: passthroughUAMode, ClientIPMode: clientIPModeBoth,
				TrafficQuota: 100, TrafficUsed: 100,
			},
			FailoverTargets: "[]", StreamHostsRaw: "[]", DynamicSources: "[]", DynamicRules: "[]",
		}},
	}
	bundle, err := buildEdgeProxy(config, runtime)
	if err != nil {
		t.Fatalf("build edge proxy: %v", err)
	}
	defer bundle.close()
	runtime.mu.Lock()
	runtime.bundle = bundle
	runtime.mu.Unlock()
	response := httptest.NewRecorder()
	bundle.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://quota.example.test/Items", nil))
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "traffic quota exceeded") {
		t.Fatalf("quota response = %d %q, want 403 quota exceeded", response.Code, response.Body.String())
	}
}

func TestEdgeTelemetryMapsRetentionAndObservationToCentralSiteID(t *testing.T) {
	localSites := map[int64]edgeSiteIdentity{
		3: {centralID: 91, host: "mapped.example.test"},
	}
	retention, ok := edgeTelemetryEventSiteID(edgeTelemetryEvent{
		Kind:      "retention",
		Retention: accountRetentionCompletionEvent{SiteID: 3, ExpectedStartedAtMS: 10, CompletedAtMS: 20},
	}, localSites)
	if !ok || retention.Retention.SiteID != 91 {
		t.Fatalf("retention mapping = %#v, ok=%t", retention.Retention, ok)
	}
	observation, ok := edgeTelemetryEventSiteID(edgeTelemetryEvent{
		Kind:        "observation",
		Observation: dynamicObservationEvent{SiteID: 3, CanonicalAuthority: "mapped.example.test", Source: dynamicObservationSourceHLS, Decision: "allow", ReasonCode: dynamicObservationReasonCandidateAllowed},
	}, localSites)
	if !ok || observation.Observation.SiteID != 91 {
		t.Fatalf("observation mapping = %#v, ok=%t", observation.Observation, ok)
	}
}

func TestEdgeTelemetryDropsUnknownLocalSiteID(t *testing.T) {
	event := edgeTelemetryEvent{Kind: "retention", Retention: accountRetentionCompletionEvent{SiteID: 404}}
	if _, ok := edgeTelemetryEventSiteID(event, map[int64]edgeSiteIdentity{3: {centralID: 91}}); ok {
		t.Fatal("unknown local site ID was accepted")
	}
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

func TestEdgeAgentHealthProbeBypassesRouteAssignment(t *testing.T) {
	runtime := &edgeAgentRuntime{}
	probeSecret := []byte("probe-secret")
	handler := runtime.observe(map[string]int64{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Meridian-Node", "edge-node")
		w.WriteHeader(http.StatusOK)
	}), probeSecret)

	request := httptest.NewRequest(http.MethodGet, "https://unassigned.example.test/.well-known/meridian-agent-health", nil)
	request.Header.Set("X-Meridian-Probe", encodeRuntimeKey(probeSecret))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("health probe status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := response.Header().Get("X-Meridian-Node"); got != "edge-node" {
		t.Fatalf("health probe node header = %q, want edge-node", got)
	}
}

func TestEdgeAgentHealthProbeRequiresSecret(t *testing.T) {
	handler := (&edgeAgentRuntime{}).observe(map[string]int64{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unauthorized health probe reached router")
	}), []byte("probe-secret"))
	request := httptest.NewRequest(http.MethodGet, "https://unassigned.example.test/.well-known/meridian-agent-health", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("health probe status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func TestScheduledProbeUsesWireProbeSecret(t *testing.T) {
	probeSecret := make([]byte, sha256.Size)
	if _, err := rand.Read(probeSecret); err != nil {
		t.Fatal(err)
	}
	const nodeGUID = "scheduled-probe-node"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("X-Meridian-Probe"), encodeRuntimeKey(probeSecret); got != want {
			t.Fatalf("probe header=%q, want %q", got, want)
		}
		w.Header().Set("X-Meridian-Node", nodeGUID)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	certificate, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	host := "127.0.0.1"
	if len(certificate.DNSNames) > 0 {
		host = certificate.DNSNames[0]
	}
	address, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	node := ControlNode{GUID: nodeGUID, Address: address, Port: port}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	if err := probeScheduledNodeWithRoots(context.Background(), node, host, probeSecret, roots); err != nil {
		t.Fatalf("scheduled probe failed: %v", err)
	}
}

func TestNodeProbeSecretWireRoundTrip(t *testing.T) {
	plaintext, err := newNodeProbeSecret()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := decodeNodeProbeSecret(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != sha256.Size {
		t.Fatalf("decoded probe secret length=%d, want %d", len(raw), sha256.Size)
	}
	wire := encodeRuntimeKey(raw)
	decoded, err := edgeDecodeKey(wire)
	if err != nil || len(decoded) != sha256.Size || string(decoded) != string(raw) {
		t.Fatalf("probe secret wire round trip failed: len=%d err=%v", len(decoded), err)
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
		decodedProbe, decodeErr := edgeDecodeKey(config.ProbeSecret)
		if decodeErr != nil || len(decodedProbe) != sha256.Size {
			t.Fatalf("Agent config probe secret is not a 32-byte wire key: len=%d err=%v", len(decodedProbe), decodeErr)
		}
		// Exercise the same wire envelope through the production Agent runtime,
		// rather than only decoding a hand-built value. A free listener port keeps
		// this Controller -> Config -> Agent regression test hermetic.
		port := freePort(t)
		releasePort(port)
		config.HTTPSPort = port
		config.ConfigHash, err = agentConfigLegacyHash(config)
		if err != nil {
			t.Fatalf("recompute test config hash: %v", err)
		}
		runtime := &edgeAgentRuntime{stateDir: t.TempDir()}
		if err := runtime.apply(config); err != nil {
			runtime.close()
			t.Fatalf("current Agent rejected Controller config: %v", err)
		}
		runtime.close()
	case <-time.After(3 * time.Second):
		t.Fatal("buildAgentConfig deadlocked while expanding routes")
	}
}
