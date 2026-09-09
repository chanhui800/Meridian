package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

type panicBody struct{}

func (panicBody) Read([]byte) (int, error) { panic("request body was decoded before authentication") }
func (panicBody) Close() error             { return nil }

func TestAgentPreAuthAuthenticatesBeforeBodyDecode(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "preauth", Address: "203.0.113.80"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, agentToken, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	handler := app.withAgentPreAuth(func(w http.ResponseWriter, r *http.Request) {
		called = true
		identity, ok := agentCredentialFromContext(r.Context())
		if !ok || !identity.HasNode || identity.Node.ID != node.ID || identity.Token != agentToken {
			t.Errorf("missing authenticated Agent identity: %#v, %v", identity, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "/api/agent/report", panicBody{})
	request.Header.Set("Authorization", "Bearer "+agentToken)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || !called {
		t.Fatalf("authenticated request = status %d called=%v", response.Code, called)
	}

	invalid := httptest.NewRequest(http.MethodPost, "/api/agent/report", panicBody{})
	invalid.Header.Set("Authorization", "Bearer invalid")
	invalidResponse := httptest.NewRecorder()
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("invalid credentials caused body decode: %v", recovered)
			}
		}()
		handler.ServeHTTP(invalidResponse, invalid)
	}()
	if invalidResponse.Code != http.StatusUnauthorized {
		t.Fatalf("invalid credentials status = %d, want 401", invalidResponse.Code)
	}
}

func TestAgentPreAuthDatabaseFailureIsRetryableNotRevoked(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	_, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "preauth-db-failure", Address: "203.0.113.81"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.db.db.Close(); err != nil {
		t.Fatal(err)
	}
	handler := app.withAgentPreAuth(func(http.ResponseWriter, *http.Request) {
		t.Fatal("request reached endpoint after authentication storage failure")
	})
	req := httptest.NewRequest(http.MethodGet, "/api/agent/config", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("database failure status=%d, want 503", response.Code)
	}
	if state := response.Header().Get("X-Meridian-Agent-State"); state != "" {
		t.Fatalf("database failure emitted Agent state %q, want no revocation", state)
	}
}

func TestAgentPreAuthCannotBeBypassedByEndpointRotation(t *testing.T) {
	app := &App{}
	handler := app.withAgentPreAuth(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	paths := []string{"/api/agent/enroll", "/api/agent/config", "/api/agent/manifest", "/api/agent/binary", "/api/agent/report"}
	for i := 0; i < agentPreAuthBurst; i++ {
		req := httptest.NewRequest(http.MethodGet, paths[i%len(paths)], nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusNoContent {
			t.Fatalf("request %d was rejected before burst exhausted: %d", i, response.Code)
		}
	}
	req := httptest.NewRequest(http.MethodGet, paths[0], nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("rotated endpoint bypassed shared pre-auth limiter: %d", response.Code)
	}
}

func TestAgentSecurityAuditStateIsBoundedByNodeAndCategory(t *testing.T) {
	db := &DB{}
	for siteID := int64(1); siteID <= 100000; siteID++ {
		db.recordAgentSecurityRejection(7, siteID, "request-event")
	}
	db.agentSecurityMu.Lock()
	entries := len(db.agentSecurityLastLog)
	db.agentSecurityMu.Unlock()
	if entries != 1 {
		t.Fatalf("security audit limiter retained %d site-specific entries, want 1", entries)
	}
	if got := db.agentSecurityRejected.Load(); got != 100000 {
		t.Fatalf("rejection counter=%d, want 100000", got)
	}
}

func TestAgentPreAuthAdmissionRateAndConcurrency(t *testing.T) {
	admission := newAgentPreAuthAdmission()
	now := time.Now()
	releases := make([]func(), 0, agentPreAuthConcurrency)
	for i := 0; i < agentPreAuthConcurrency; i++ {
		release, _, ok := admission.admit("client-"+string(rune('a'+i)), now)
		if !ok {
			t.Fatalf("pre-auth admission %d was rejected before concurrency ceiling", i)
		}
		releases = append(releases, release)
	}
	if _, _, ok := admission.admit("other-client", now); ok {
		t.Fatal("pre-auth concurrency ceiling was bypassed")
	}
	for _, release := range releases {
		release()
	}
	// A separate client has its own token bucket and is admitted immediately;
	// a single noisy client is rate limited without affecting it.
	for i := 0; i < agentPreAuthBurst; i++ {
		release, _, ok := admission.admit("noisy", now)
		if !ok {
			t.Fatalf("burst admission %d was rejected", i)
		}
		release()
	}
	if _, _, ok := admission.admit("noisy", now); ok {
		t.Fatal("pre-auth token bucket did not reject an exhausted client")
	}
	if release, _, ok := admission.admit("independent", now); !ok {
		t.Fatal("one client's pre-auth limiter affected another client")
	} else {
		release()
	}
}

func TestNodeReportAdmissionReclaimsIdleEntries(t *testing.T) {
	admission := newNodeReportAdmission()
	now := time.Now()
	release, _, ok := admission.admit(42, now)
	if !ok {
		t.Fatal("node report was rejected")
	}
	release()
	admission.mu.Lock()
	admission.prune(now.Add(nodeReportEntryTTL + time.Second))
	_, exists := admission.entries[42]
	admission.mu.Unlock()
	if exists {
		t.Fatal("idle node report limiter entry was not reclaimed")
	}
}

func TestAgentReportCannotAcknowledgeFutureCacheGeneration(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "cache-generation-guard", Address: "203.0.113.90"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE control_nodes SET cache_clear_generation=3 WHERE id=?", node.ID); err != nil {
		t.Fatal(err)
	}
	for sequence, generation := range []int64{4, 3} {
		if _, err := app.db.RecordNodeReportResult(token, NodeReport{
			BootID: "cache-guard", ReportSessionID: "cache-guard", CounterEpoch: "kernel:eth0", Sequence: int64(sequence + 1),
			InterfaceName: "eth0", CacheClearGeneration: generation,
		}, now.Add(time.Duration(sequence)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	var applied int64
	if err := app.db.db.QueryRow("SELECT cache_clear_applied_generation FROM control_nodes WHERE id=?", node.ID).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != 3 {
		t.Fatalf("cache clear applied generation=%d, want only the controller-issued generation 3", applied)
	}
}

func TestAgentPreAuthAdmissionHasHardEntryLimit(t *testing.T) {
	admission := newAgentPreAuthAdmission()
	now := time.Now()
	for i := 0; i < maxTrackedAgentPreAuthClients+100000; i++ {
		release, _, ok := admission.admit("client-"+strconv.Itoa(i), now)
		if ok {
			release()
		}
	}
	admission.mu.Lock()
	entries := len(admission.entries)
	admission.mu.Unlock()
	if entries > maxTrackedAgentPreAuthClients {
		t.Fatalf("pre-auth entries=%d, want <=%d", entries, maxTrackedAgentPreAuthClients)
	}
}

func TestAgentPreAuthAdmissionDoesNotEvictActiveEntries(t *testing.T) {
	admission := newAgentPreAuthAdmission()
	now := time.Now()
	release, _, ok := admission.admit("active", now)
	if !ok {
		t.Fatal("active client was rejected")
	}
	t.Cleanup(release)
	admission.mu.Lock()
	admission.maxEntries = 1
	admission.mu.Unlock()
	if _, _, ok := admission.admit("new-client", now); ok {
		t.Fatal("new client admitted while the only tracked entry was active")
	}
	admission.mu.Lock()
	_, exists := admission.entries["active"]
	admission.mu.Unlock()
	if !exists {
		t.Fatal("active pre-auth entry was evicted")
	}
}

func BenchmarkAgentPreAuthAdmissionLargeMap(b *testing.B) {
	admission := newAgentPreAuthAdmission()
	now := time.Now()
	for i := 0; i < maxTrackedAgentPreAuthClients; i++ {
		admission.entries[strconv.Itoa(i)] = &agentPreAuthEntry{tokens: agentPreAuthBurst, last: now, lastSeen: now}
	}
	for i := 0; i < b.N; i++ {
		release, _, ok := admission.admit("steady-client", now.Add(time.Duration(i)*time.Millisecond))
		if ok {
			release()
		}
	}
}
