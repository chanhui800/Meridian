package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestControllerAgentHTTPConfigApplyAndReportE2E exercises the production
// HTTP contract end to end: enrollment, config serialization, Agent envelope
// validation/application, and a report containing both authorized and
// unauthorized site telemetry. It uses SQLite and real HTTP handlers while
// keeping DNS/Cloudflare outside the test.
func TestControllerAgentHTTPConfigApplyAndReportE2E(t *testing.T) {
	app := newTestApp(t)
	app.dynamicRouteKey = bytes.Repeat([]byte{0x31}, 32)
	installReviewCertificate(t, app)
	now := time.Now().UTC()
	nodeA, enrollmentA, err := app.db.CreateControlNode(NodeCreateInput{Name: "e2e-a", Address: "203.0.113.80", Port: 19090}, now)
	if err != nil {
		t.Fatal(err)
	}
	nodeB, enrollmentB, err := app.db.CreateControlNode(NodeCreateInput{Name: "e2e-b", Address: "203.0.113.81", Port: 19091}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, tokenA, err := app.db.EnrollControlNode(enrollmentA, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNode(enrollmentB, now); err != nil {
		t.Fatal(err)
	}
	installReviewCertificateForNode(t, app, nodeA.GUID)
	installReviewCertificateForNode(t, app, nodeB.GUID)
	siteAPort := freePort(t)
	releasePort(siteAPort)
	siteA, err := app.db.CreateSiteRecord(Site{Name: "e2e-a-site", ListenPort: siteAPort, PublicHost: "a.e2e.example", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:1", PlaybackMode: "direct", MainVideoStreamMode: "proxy", StreamHosts: "[]", UAMode: passthroughUAMode})
	if err != nil {
		t.Fatal(err)
	}
	siteBPort := freePort(t)
	releasePort(siteBPort)
	siteB, err := app.db.CreateSiteRecord(Site{Name: "e2e-b-site", ListenPort: siteBPort, PublicHost: "b.e2e.example", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:1", PlaybackMode: "direct", MainVideoStreamMode: "proxy", StreamHosts: "[]", UAMode: passthroughUAMode})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(siteA.ID, true, "fixed", nodeA.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(siteB.ID, true, "fixed", nodeB.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.RecordNodeReport(tokenA, NodeReport{BootID: "e2e-bootstrap", ReportSessionID: "e2e-bootstrap", CounterEpoch: "e2e-kernel:eth0", Sequence: 1, InterfaceName: "eth0", AgentVersion: "test"}, now); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/config", app.withAgentPreAuth(app.handleAgentConfig))
	mux.HandleFunc("/api/agent/report", app.withAgentPreAuth(app.handleAgentReport))
	server := httptest.NewServer(mux)
	defer server.Close()
	request, err := http.NewRequest(http.MethodGet, server.URL+"/api/agent/config", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+tokenA)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("config status=%d", response.StatusCode)
	}
	var config AgentRuntimeConfig
	if err := json.NewDecoder(response.Body).Decode(&config); err != nil {
		t.Fatal(err)
	}
	if len(config.Routes) != 1 || config.Routes[0].SiteID != siteA.ID {
		t.Fatalf("Agent received unauthorized routes: %#v", config.Routes)
	}
	port := freePort(t)
	releasePort(port)
	config.HTTPSPort = port
	config.ConfigHash, err = agentConfigLegacyHash(config)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &edgeAgentRuntime{stateDir: t.TempDir()}
	if err := runtime.apply(config); err != nil {
		t.Fatalf("Agent failed to apply Controller config: %v", err)
	}
	runtime.close()
	unauthorizedUID := strings.Repeat("b", 32)
	report := NodeReport{BootID: "e2e-session", ReportSessionID: "e2e-session", CounterEpoch: "e2e-kernel:eth0", Sequence: 1, InterfaceName: "eth0", AppliedConfigHash: config.ConfigHash,
		Events: []NodeRequestEvent{{EventID: 1, EventUID: strings.Repeat("a", 32), SiteID: siteA.ID, Host: siteA.PublicHost, Method: http.MethodGet, Path: "/ok", StatusCode: 200, RecordedAtMS: now.UnixMilli()}, {EventID: 2, EventUID: unauthorizedUID, SiteID: siteB.ID, Host: siteB.PublicHost, Method: http.MethodGet, Path: "/no", StatusCode: 200, RecordedAtMS: now.UnixMilli()}}}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	reportRequest, err := http.NewRequest(http.MethodPost, server.URL+"/api/agent/report", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	reportRequest.Header.Set("Authorization", "Bearer "+tokenA)
	reportRequest.Header.Set("Content-Type", "application/json")
	reportResponse, err := server.Client().Do(reportRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer reportResponse.Body.Close()
	if reportResponse.StatusCode != http.StatusOK {
		t.Fatalf("report status=%d", reportResponse.StatusCode)
	}
	var acknowledgement struct {
		AcceptedEventIDs   []int64  `json:"accepted_event_ids"`
		DiscardedEventIDs  []int64  `json:"discarded_event_ids"`
		DiscardedEventUIDs []string `json:"discarded_event_uids"`
	}
	if err := json.NewDecoder(reportResponse.Body).Decode(&acknowledgement); err != nil {
		t.Fatal(err)
	}
	if len(acknowledgement.AcceptedEventIDs) != 1 || acknowledgement.AcceptedEventIDs[0] != 1 || len(acknowledgement.DiscardedEventIDs) != 1 || acknowledgement.DiscardedEventIDs[0] != 2 || len(acknowledgement.DiscardedEventUIDs) != 1 || acknowledgement.DiscardedEventUIDs[0] != unauthorizedUID {
		t.Fatalf("unexpected report acknowledgement: %#v", acknowledgement)
	}
	var logs int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM request_logs WHERE site_id=?", siteB.ID).Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if logs != 0 {
		t.Fatalf("unauthorized Site B event created %d request logs", logs)
	}
}
