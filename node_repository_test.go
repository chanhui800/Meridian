package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestControlNodeEnrollmentTrafficAndDelete(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	node, enrollmentToken, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "Tokyo 01", Address: "203.0.113.10", Priority: 100,
		TrafficQuota: 10_000, BillingMode: "outbound", ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatalf("CreateControlNode: %v", err)
	}
	if enrollmentToken == "" || node.EnrollmentAvailable != true {
		t.Fatalf("created node missing enrollment token state: %#v", node)
	}
	if node.Port != 443 {
		t.Fatalf("default port = %d, want 443", node.Port)
	}
	if strings.Contains(node.String(), enrollmentToken) {
		t.Fatal("node representation exposed enrollment token")
	}

	enrolled, agentToken, err := app.db.EnrollControlNode(enrollmentToken, now.Add(time.Second))
	if err != nil {
		t.Fatalf("EnrollControlNode: %v", err)
	}
	if enrolled.ID != node.ID || agentToken == "" || enrolled.EnrollmentAvailable {
		t.Fatalf("unexpected enrollment result: %#v", enrolled)
	}
	if _, _, err := app.db.EnrollControlNode(enrollmentToken, now.Add(2*time.Second)); !errors.Is(err, errInvalidNodeToken) {
		t.Fatalf("reused enrollment token error = %v", err)
	}

	first := NodeReport{BootID: "boot-a:run-a", Sequence: 1, InterfaceName: "ens5", RXBytes: 1000, TXBytes: 2000, AgentVersion: "test"}
	if _, err := app.db.RecordNodeReport(agentToken, first, now.Add(3*time.Second)); err != nil {
		t.Fatalf("first report: %v", err)
	}
	second := first
	second.Sequence = 2
	second.RXBytes = 1300
	second.TXBytes = 2600
	second.AppliedConfigHash = "config-v1"
	second.ListenerError = ""
	reported, err := app.db.RecordNodeReport(agentToken, second, now.Add(4*time.Second))
	if err != nil {
		t.Fatalf("second report: %v", err)
	}
	if reported.PeriodRXBytes != 300 || reported.PeriodTXBytes != 600 || reported.TrafficUsed != 600 {
		t.Fatalf("unexpected traffic totals: %#v", reported)
	}
	if reported.AppliedConfigHash != "config-v1" || reported.AgentListenerError != "" {
		t.Fatalf("agent config report not persisted: %#v", reported)
	}
	duplicate, err := app.db.RecordNodeReport(agentToken, second, now.Add(5*time.Second))
	if err != nil {
		t.Fatalf("duplicate report: %v", err)
	}
	if duplicate.PeriodRXBytes != 300 || duplicate.PeriodTXBytes != 600 {
		t.Fatalf("duplicate report counted twice: %#v", duplicate)
	}
	updated, err := app.db.UpdateControlNode(node.ID, NodeCreateInput{Name: node.Name, Address: node.Address, Priority: node.Priority, TrafficQuota: node.TrafficQuota, BillingMode: node.BillingMode, ResetDay: 15}, true, now.Add(5500*time.Millisecond))
	if err != nil || updated.PeriodRXBytes != 0 || updated.PeriodTXBytes != 0 || updated.ResetDay != 15 {
		t.Fatalf("billing cycle update = %#v, %v", updated, err)
	}

	manual, err := app.db.UpdateNodeScheduler("manual", node.ID, now.Add(6*time.Second))
	if err != nil || manual.Scheduler.ManualNodeID != node.ID || manual.Scheduler.ActiveNodeID != node.ID {
		t.Fatalf("manual scheduler = %#v, %v", manual.Scheduler, err)
	}
	if err := app.db.DeleteControlNode(node.ID); err != nil {
		t.Fatalf("DeleteControlNode: %v", err)
	}
	afterDelete, err := app.db.NodeControlSnapshot(now.Add(7 * time.Second))
	if err != nil || len(afterDelete.Nodes) != 0 || afterDelete.Scheduler.ManualNodeID != 0 || afterDelete.Scheduler.ActiveNodeID != 0 {
		t.Fatalf("snapshot after delete = %#v, %v", afterDelete, err)
	}
}

func TestControlNodePortValidation(t *testing.T) {
	custom, err := normalizeNodeInput(NodeCreateInput{Name: "custom", Port: 9090})
	if err != nil || custom.Port != 9090 {
		t.Fatalf("custom node port = %#v, %v", custom, err)
	}
	defaulted, err := normalizeNodeInput(NodeCreateInput{Name: "default"})
	if err != nil || defaulted.Port != 443 {
		t.Fatalf("API default node port = %#v, %v", defaulted, err)
	}
	if _, err := normalizeNodeInput(NodeCreateInput{Name: "invalid", Port: 65536}); err == nil {
		t.Fatal("node accepted a port above 65535")
	}
}

func TestNodeMetadataEventResponseLimit(t *testing.T) {
	event := NodeRequestEvent{
		EventID: 1, SiteID: 1, Host: "media.example.test", Method: "GET",
		Path: "/Items/123", StatusCode: 200, ResponseBody: strings.Repeat("x", 32<<10),
		RecordedAtMS: time.Now().UnixMilli(),
	}
	if err := validateNodeRequestEvent(event); err != nil {
		t.Fatalf("metadata response below limit rejected: %v", err)
	}
	event.ResponseBody = strings.Repeat("x", maxNodeRequestEventResponseBodyBytes+1)
	if err := validateNodeRequestEvent(event); err == nil {
		t.Fatal("metadata response above limit accepted")
	}
}

func TestControlNodeManualTrafficOffsetAndSiteStats(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "stats", Address: "203.0.113.20", ResetDay: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "stats-site", PublicHost: "stats.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	siteID := site.ID
	if _, err := app.db.SaveSiteNodeSchedule(siteID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET desired_node_id=? WHERE site_id=?", node.ID, siteID); err != nil {
		t.Fatal(err)
	}
	_, err = app.db.RecordNodeReport(token, NodeReport{BootID: "boot", Sequence: 1, InterfaceName: "eth0", RXBytes: 100, TXBytes: 200, SiteStats: []NodeSiteStat{{Host: "stats.example.test", RequestCount: 3, LastRequestAtMS: now.UnixMilli(), LastStatus: 200}}}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.db.RecordNodeReport(token, NodeReport{BootID: "boot", Sequence: 2, InterfaceName: "eth0", RXBytes: 100, TXBytes: 300, SiteStats: []NodeSiteStat{{Host: "stats.example.test", RequestCount: 4, LastRequestAtMS: now.Add(time.Second).UnixMilli(), LastStatus: 206}}}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := app.db.siteNodeSchedule(siteID)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.AgentRequestCount != 4 || schedule.AgentLastStatus != 206 {
		t.Fatalf("unexpected agent stats: %#v", schedule)
	}
	requestEvent := NodeRequestEvent{EventID: 1, SiteID: siteID, Host: "stats.example.test", Method: "GET", Path: "/System/Info", StatusCode: 200, UserAgent: "TestClient/1", UpstreamUserAgent: "Upstream/1", ResourceCategory: requestLogCategoryMetadata, BackendAddress: "https://origin.example.test:443", RecordedAtMS: now.Add(2 * time.Second).UnixMilli()}
	if _, err := app.db.RecordNodeReport(token, NodeReport{BootID: "boot", Sequence: 3, InterfaceName: "eth0", RXBytes: 100, TXBytes: 300, Events: []NodeRequestEvent{requestEvent}}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.RecordNodeReport(token, NodeReport{BootID: "boot", Sequence: 4, InterfaceName: "eth0", RXBytes: 100, TXBytes: 300, Events: []NodeRequestEvent{requestEvent}}, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	logs, err := app.db.ListRequestLogs(RequestLogFilter{Limit: 20})
	if err != nil || len(logs) != 1 || logs[0].Path != "/System/Info" || logs[0].ResourceCategory != requestLogCategoryMetadata || logs[0].BackendAddress != "https://origin.example.test:443" || logs[0].UpstreamUserAgent != "Upstream/1" {
		t.Fatalf("node event logs = %#v, %v", logs, err)
	}
	updated, err := app.db.UpdateControlNode(node.ID, NodeCreateInput{Name: node.Name, Address: node.Address, Port: node.Port, Priority: node.Priority, TrafficQuota: node.TrafficQuota, BillingMode: node.BillingMode, ResetDay: node.ResetDay, TrafficManualOffsetBytes: 1024}, true, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if updated.TrafficUsed != 1124 {
		t.Fatalf("traffic offset not applied: %#v", updated)
	}
}

func TestNodeHTTPSProbePortFollowsNodePort(t *testing.T) {
	if got := nodeHTTPSProbePort(ControlNode{Port: 9090}); got != 9090 {
		t.Fatalf("probe port = %d, want 9090", got)
	}
}

func TestLegacyControlNodeMigratesToControllerPort(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	node, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "legacy", Port: 443}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE control_nodes SET entry_mode='shared',http_port=18443,https_port=0 WHERE id=?", node.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.db.MigrateControlNodesToSinglePort(9090, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	migrated, err := app.db.controlNodeByID(node.ID, now.Add(2*time.Second))
	if err != nil || migrated.Port != 9090 {
		t.Fatalf("migrated node = %#v, %v", migrated, err)
	}
	var mode string
	var httpPort int
	if err := app.db.db.QueryRow("SELECT entry_mode,http_port FROM control_nodes WHERE id=?", node.ID).Scan(&mode, &httpPort); err != nil {
		t.Fatal(err)
	}
	if mode != "direct" || httpPort != 0 {
		t.Fatalf("legacy storage = %s/%d, want direct/0", mode, httpPort)
	}
}

func TestSiteNodeSchedulingIsOptInAndCanFollowGlobalNode(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 8, 30, 13, 0, 0, 0, time.UTC)
	site, err := app.db.CreateSiteRecord(Site{Name: "scheduled", ListenPort: 18090, PublicHost: "media.example.com", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096", PlaybackMode: "direct", StreamHosts: "[]", UAMode: "passthrough"})
	if err != nil {
		t.Fatal(err)
	}
	values, err := app.db.ListSiteNodeSchedules()
	if err != nil || len(values) != 1 || values[0].Enabled || values[0].DNSStatus != "disabled" {
		t.Fatalf("default site schedule = %#v, %v", values, err)
	}
	node, token, err := app.db.CreateControlNode(NodeCreateInput{Name: "edge", Address: "203.0.113.10", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, agentToken, err := app.db.EnrollControlNode(token, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.RecordNodeReport(agentToken, NodeReport{BootID: "boot", Sequence: 1, InterfaceName: "ens5", AgentVersion: "test"}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	schedule, err := app.db.SaveSiteNodeSchedule(site.ID, true, "global", 0, now.Add(2*time.Second))
	if err != nil || !schedule.Enabled {
		t.Fatalf("SaveSiteNodeSchedule = %#v, %v", schedule, err)
	}
	if err := app.refreshSiteAssignments(now.Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	schedule, err = app.db.siteNodeSchedule(site.ID)
	if err != nil || schedule.DesiredNodeID != node.ID || schedule.DNSStatus == "active" {
		t.Fatalf("assigned schedule = %#v, %v", schedule, err)
	}
}

func TestSiteNodeSchedulingCanBeDisabledWithoutFixedNode(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 8, 30, 14, 0, 0, 0, time.UTC)
	site, err := app.db.CreateSiteRecord(Site{Name: "toggle", ListenPort: 18091, PublicHost: "toggle.example.com", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096", PlaybackMode: "direct", StreamHosts: "[]", UAMode: "passthrough"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", 0, now); err == nil {
		t.Fatal("enabled fixed schedule accepted without a node")
	}
	schedule, err := app.db.SaveSiteNodeSchedule(site.ID, false, "fixed", 0, now)
	if err != nil {
		t.Fatalf("disabled fixed schedule = %#v, %v", schedule, err)
	}
	if schedule.Enabled || schedule.DNSStatus != "disabled" {
		t.Fatalf("disabled schedule = %#v", schedule)
	}
}

func TestControlNodeAutoSchedulerSkipsDepletedAndOffline(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	depleted, tokenA, err := app.db.CreateControlNode(NodeCreateInput{Name: "A", Priority: 200, TrafficQuota: 100, BillingMode: "outbound", ResetDay: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	ready, tokenB, err := app.db.CreateControlNode(NodeCreateInput{Name: "B", Priority: 100, TrafficQuota: 1000, BillingMode: "outbound", ResetDay: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, agentA, _ := app.db.EnrollControlNode(tokenA, now)
	_, agentB, _ := app.db.EnrollControlNode(tokenB, now)
	for token, report := range map[string]NodeReport{
		agentA: {BootID: "a", Sequence: 1, InterfaceName: "eth0", TXBytes: 100},
		agentB: {BootID: "b", Sequence: 1, InterfaceName: "eth0", TXBytes: 100},
	} {
		if _, err := app.db.RecordNodeReport(token, report, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.db.RecordNodeReport(agentA, NodeReport{BootID: "a", Sequence: 2, InterfaceName: "eth0", TXBytes: 250}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := app.db.NodeControlSnapshot(now.Add(3 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Scheduler.ActiveNodeID != ready.ID {
		t.Fatalf("active node = %d, want %d; depleted node %d", snapshot.Scheduler.ActiveNodeID, ready.ID, depleted.ID)
	}
}

func TestBuildNodeInstallScriptDoesNotPersistTokenInService(t *testing.T) {
	script := buildNodeInstallScript("https://panel.example.com", "enrollment-secret")
	if !strings.Contains(script, "enrollment-secret") || !strings.Contains(script, "default") && strings.Contains(script, "eth0") {
		t.Fatalf("unexpected install script: %s", script)
	}
	service := script[strings.Index(script, "[Unit]"):]
	if strings.Contains(service, "enrollment-secret") {
		t.Fatal("systemd service contains the one-time enrollment token")
	}
	if !strings.Contains(script, "Linux x86_64 only") {
		t.Fatal("install script does not declare architecture limit")
	}
}

func TestNormalizeControllerURLRequiresHTTPSForRemoteHosts(t *testing.T) {
	if _, err := normalizeControllerURL("http://panel.example.com"); err == nil {
		t.Fatal("remote HTTP controller URL accepted")
	}
	for _, input := range []string{"https://panel.example.com/", "http://localhost:9090", "http://127.0.0.1:9090"} {
		if _, err := normalizeControllerURL(input); err != nil {
			t.Fatalf("normalizeControllerURL(%q): %v", input, err)
		}
	}
}
