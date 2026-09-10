package main

import (
	"testing"
	"time"
)

func TestNodeLiveReportOverlaysDashboardWithoutPersistingTraffic(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "live-node", Address: "203.0.113.70", Port: 19070}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "live-site", ListenPort: freePort(t), PublicHost: "live.example", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:1", PlaybackMode: "direct", MainVideoStreamMode: "proxy", StreamHosts: "[]", UAMode: passthroughUAMode})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET applied_node_id=?, dns_status='active' WHERE site_id=?", node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	full := NodeReport{
		BootID: "live-boot", ReportSessionID: "live-session", CounterEpoch: "kernel:eth0", SiteCounterEpoch: "1",
		Sequence: 1, InterfaceName: "eth0", SiteStats: []NodeSiteStat{{SiteID: site.ID, Host: site.PublicHost, CumulativeBytesIn: 100, CumulativeBytesOut: 200, RequestCount: 3, CounterEpoch: 1}},
	}
	if _, err := app.db.RecordNodeReport(token, full, now); err != nil {
		t.Fatal(err)
	}
	var logCountBefore int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM node_site_traffic_logs WHERE node_id=? AND site_id=?", node.ID, site.ID).Scan(&logCountBefore); err != nil {
		t.Fatal(err)
	}
	accepted, discarded, err := app.db.recordNodeLiveReport(node.ID, NodeLiveReport{
		ReportSessionID: "live-session", CounterEpoch: "kernel:eth0", Sequence: 2, SampledAtMS: now.Add(time.Second).UnixMilli(),
		SiteStats: []NodeLiveSiteTraffic{{SiteID: site.ID, Host: site.PublicHost, CumulativeBytesIn: 900, CumulativeBytesOut: 1200, Requests: 17}},
	}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted) != 1 || accepted[0] != site.ID || len(discarded) != 0 {
		t.Fatalf("live acknowledgement accepted=%v discarded=%v", accepted, discarded)
	}
	snapshot, err := app.db.NodeSiteLiveTrafficSnapshot(now.Add(2 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	value, ok := snapshot[site.ID]
	if !ok || value.CumulativeBytesIn != 900 || value.CumulativeBytesOut != 1200 || value.Requests != 17 {
		t.Fatalf("live overlay did not reach dashboard snapshot: %#v", snapshot)
	}
	var logCountAfter int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM node_site_traffic_logs WHERE node_id=? AND site_id=?", node.ID, site.ID).Scan(&logCountAfter); err != nil {
		t.Fatal(err)
	}
	if logCountAfter != logCountBefore {
		t.Fatalf("live sample unexpectedly persisted traffic logs: before=%d after=%d", logCountBefore, logCountAfter)
	}
}

func TestDashboardTrendSnapshotReturnsPersistedAgentBaseline(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "trend-baseline-node", Address: "203.0.113.74", Port: 19074}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "trend-baseline-site", ListenPort: freePort(t), PublicHost: "trend-baseline.example", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:1", PlaybackMode: "direct", MainVideoStreamMode: "proxy", StreamHosts: "[]", UAMode: passthroughUAMode})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET applied_node_id=?, dns_status='active' WHERE site_id=?", node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.RecordNodeReport(token, NodeReport{
		BootID: "trend-baseline-boot", ReportSessionID: "trend-baseline-session", CounterEpoch: "kernel:eth0", SiteCounterEpoch: "1",
		Sequence: 1, TelemetrySequence: 1, InterfaceName: "eth0", SiteStats: []NodeSiteStat{{SiteID: site.ID, Host: site.PublicHost, BytesIn: 10, BytesOut: 20, CumulativeBytesIn: 110, CumulativeBytesOut: 220, RequestCount: 11, CounterEpoch: 1}},
	}, now); err != nil {
		t.Fatal(err)
	}
	_, baselines, err := app.db.GetTrafficTrendLogsGroupedSnapshot(&site.ID, now.Add(-time.Minute), now.Add(time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	baseline, ok := baselines[site.ID]
	if !ok {
		t.Fatalf("missing Agent trend baseline: %#v", baselines)
	}
	if baseline.BytesIn != 110 || baseline.BytesOut != 220 || baseline.Requests != 11 || baseline.SampledAtMS != now.UnixMilli() {
		t.Fatalf("baseline=%+v, want counters 110/220/11 at %d", baseline, now.UnixMilli())
	}
	trend, err := app.pm.dashboardTrends(&site.ID, "realtime")
	if err != nil {
		t.Fatal(err)
	}
	if trend.AsOfMS != now.UnixMilli() {
		t.Fatalf("dashboard AsOfMS=%d, want persisted Agent watermark %d", trend.AsOfMS, now.UnixMilli())
	}
	if got := trend.LiveBaselines[site.ID]; got != baseline {
		t.Fatalf("dashboard live baseline=%+v, want %+v", got, baseline)
	}
}

func TestNodeTelemetrySequenceOrdersFullAndLiveChannels(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "ordered-live", Address: "203.0.113.73", Port: 19073}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "ordered-site", ListenPort: freePort(t), PublicHost: "ordered.example", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:1", PlaybackMode: "direct", MainVideoStreamMode: "proxy", StreamHosts: "[]", UAMode: passthroughUAMode})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET applied_node_id=?, dns_status='active' WHERE site_id=?", node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	fullReport := func(sequence, telemetry int64, in, out, requests int64, at time.Time) {
		t.Helper()
		if _, err := app.db.RecordNodeReport(token, NodeReport{
			BootID: "ordered-boot", ReportSessionID: "ordered-session", CounterEpoch: "kernel:eth0",
			Sequence: sequence, TelemetrySequence: telemetry, InterfaceName: "eth0",
			SiteStats: []NodeSiteStat{{SiteID: site.ID, Host: site.PublicHost, CumulativeBytesIn: in, CumulativeBytesOut: out, RequestCount: requests}},
		}, at); err != nil {
			t.Fatal(err)
		}
	}
	fullReport(1, 100, 100, 200, 3, now)
	accepted, _, err := app.db.recordNodeLiveReport(node.ID, NodeLiveReport{
		ReportSessionID: "ordered-session", CounterEpoch: "kernel:eth0", Sequence: 1, TelemetrySequence: 101,
		SampledAtMS: now.Add(time.Second).UnixMilli(),
		SiteStats:   []NodeLiveSiteTraffic{{SiteID: site.ID, Host: site.PublicHost, CumulativeBytesIn: 900, CumulativeBytesOut: 1200, Requests: 17}},
	}, now.Add(time.Second))
	if err != nil || len(accepted) != 1 {
		t.Fatalf("live report accepted=%v err=%v", accepted, err)
	}
	fullReport(2, 102, 950, 1250, 19, now.Add(2*time.Second))
	// A delayed live request with an older shared sequence must not overlay
	// the newer committed full report.
	accepted, _, err = app.db.recordNodeLiveReport(node.ID, NodeLiveReport{
		ReportSessionID: "ordered-session", CounterEpoch: "kernel:eth0", Sequence: 2, TelemetrySequence: 101,
		SampledAtMS: now.Add(3 * time.Second).UnixMilli(),
		SiteStats:   []NodeLiveSiteTraffic{{SiteID: site.ID, Host: site.PublicHost, CumulativeBytesIn: 910, CumulativeBytesOut: 1210, Requests: 18}},
	}, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted) != 0 {
		t.Fatalf("delayed live report was accepted: %v", accepted)
	}
	snapshot, err := app.db.NodeSiteLiveTrafficSnapshot(now.Add(4 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	value, ok := snapshot[site.ID]
	if !ok || value.CumulativeBytesIn != 950 || value.CumulativeBytesOut != 1250 || value.Requests != 19 {
		t.Fatalf("newer full report was overwritten: %#v", snapshot)
	}
	// A restarted Agent begins a fresh telemetry sequence and must not be
	// blocked by the previous process's watermark.
	accepted, _, err = app.db.recordNodeLiveReport(node.ID, NodeLiveReport{
		ReportSessionID: "restarted-session", CounterEpoch: "kernel:eth0", Sequence: 1, TelemetrySequence: 1,
		SampledAtMS: now.Add(5 * time.Second).UnixMilli(),
		SiteStats:   []NodeLiveSiteTraffic{{SiteID: site.ID, Host: site.PublicHost, CumulativeBytesIn: 1000, CumulativeBytesOut: 1300, Requests: 20}},
	}, now.Add(5*time.Second))
	if err != nil || len(accepted) != 1 {
		t.Fatalf("restarted live report accepted=%v err=%v", accepted, err)
	}
}

func TestNodeLiveReportRejectsUnauthorizedSite(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	nodeA, enrollmentA, err := app.db.CreateControlNode(NodeCreateInput{Name: "live-a", Address: "203.0.113.71", Port: 19071}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNode(enrollmentA, now); err != nil {
		t.Fatal(err)
	}
	nodeB, enrollmentB, err := app.db.CreateControlNode(NodeCreateInput{Name: "live-b", Address: "203.0.113.72", Port: 19072}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = app.db.EnrollControlNode(enrollmentB, now)
	if err != nil {
		t.Fatal(err)
	}
	siteB, err := app.db.CreateSiteRecord(Site{Name: "other-site", ListenPort: freePort(t), PublicHost: "other.example", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:1", PlaybackMode: "direct", MainVideoStreamMode: "proxy", StreamHosts: "[]", UAMode: passthroughUAMode})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(siteB.ID, true, "fixed", nodeB.ID, now); err != nil {
		t.Fatal(err)
	}
	accepted, discarded, err := app.db.recordNodeLiveReport(nodeA.ID, NodeLiveReport{
		ReportSessionID: "spoof", CounterEpoch: "kernel:eth0", Sequence: 1, SampledAtMS: now.UnixMilli(),
		SiteStats: []NodeLiveSiteTraffic{{SiteID: siteB.ID, Host: siteB.PublicHost, CumulativeBytesIn: 1, CumulativeBytesOut: 2, Requests: 1}},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted) != 0 || len(discarded) != 1 || discarded[0] != siteB.ID {
		t.Fatalf("unauthorized live report accepted=%v discarded=%v", accepted, discarded)
	}
}
