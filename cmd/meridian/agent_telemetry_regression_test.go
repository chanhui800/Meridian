package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type telemetryFixture struct {
	app   *App
	node  ControlNode
	site  Site
	token string
	now   time.Time
}

func newTelemetryFixture(t *testing.T) telemetryFixture {
	t.Helper()
	app := newTestApp(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "telemetry-node", Address: "203.0.113.40", Port: 19440,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{
		Name: "telemetry-site", ListenPort: freePort(t), PublicHost: "telemetry.example",
		IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:1",
		PlaybackMode: "direct", MainVideoStreamMode: "proxy",
		StreamHosts: "[]", UAMode: passthroughUAMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(
		"UPDATE site_node_schedules SET applied_node_id=?,dns_status='active' WHERE site_id=?",
		node.ID, site.ID,
	); err != nil {
		t.Fatal(err)
	}
	return telemetryFixture{app: app, node: node, site: *site, token: token, now: now}
}

func (fixture telemetryFixture) full(t *testing.T, sequence, telemetry, in, out, requests int64, sampled, received time.Duration) {
	t.Helper()
	report := NodeReport{
		BootID: "telemetry-boot", ReportSessionID: "telemetry-session",
		CounterEpoch: "kernel:eth0", SiteCounterEpoch: "1",
		Sequence: sequence, TelemetrySequence: telemetry,
		SampledAtMS: fixture.now.Add(sampled).UnixMilli(), InterfaceName: "eth0",
		SiteStats: []NodeSiteStat{{
			SiteID: fixture.site.ID, Host: fixture.site.PublicHost,
			CumulativeBytesIn: in, CumulativeBytesOut: out,
			RequestCount: requests, CounterEpoch: 1,
		}},
	}
	if _, err := fixture.app.db.RecordNodeReport(fixture.token, report, fixture.now.Add(received)); err != nil {
		t.Fatal(err)
	}
}

func (fixture telemetryFixture) live(t *testing.T, sequence, telemetry, in, out, requests int64, at time.Duration) {
	t.Helper()
	accepted, discarded, err := fixture.app.db.recordNodeLiveReport(fixture.node.ID, NodeLiveReport{
		ReportSessionID: "telemetry-session", CounterEpoch: "kernel:eth0",
		Sequence: sequence, TelemetrySequence: telemetry, SampledAtMS: fixture.now.Add(at).UnixMilli(),
		SiteStats: []NodeLiveSiteTraffic{{
			SiteID: fixture.site.ID, Host: fixture.site.PublicHost,
			CumulativeBytesIn: in, CumulativeBytesOut: out, Requests: requests,
			CounterEpoch: 1,
			DownloadBPS:  30, UploadBPS: 20, RateValid: true,
		}},
	}, fixture.now.Add(at))
	if err != nil || len(accepted) != 1 || len(discarded) != 0 {
		t.Fatalf("live report accepted=%v discarded=%v err=%v", accepted, discarded, err)
	}
}

func (fixture telemetryFixture) snapshot(t *testing.T, at time.Duration) *TrafficSnapshot {
	t.Helper()
	now := fixture.now.Add(at)
	live, err := fixture.app.db.NodeSiteLiveTrafficSnapshot(now)
	if err != nil {
		t.Fatal(err)
	}
	return fixture.app.pm.dashboardSnapshotFromSites(
		[]Site{fixture.site}, map[int64]int64{}, live, fixture.app.db.currentSystemSettings(), now,
	)
}

func TestFullReportPreservesHealthyLiveRateAfterPersistence(t *testing.T) {
	fixture := newTelemetryFixture(t)
	fixture.full(t, 1, 1, 100, 200, 3, 0, 0)
	fixture.live(t, 1, 2, 140, 260, 4, 4*time.Second)
	fixture.full(t, 2, 3, 150, 290, 5, 5*time.Second, 5*time.Second)

	snapshot := fixture.snapshot(t, 5*time.Second)
	point, _ := dashboardRealtimePointFromSnapshot(snapshot, nil, snapshot.GeneratedAtMS)
	if point.SpeedUnavailable || point.DownloadBPS != 30 || point.UploadBPS != 20 {
		t.Fatalf("healthy live rate after committed full report=%+v, want measured 30/20", point)
	}
	// The full channel still has to land its bytes in the durable ledger.
	persisted, err := fixture.app.db.SumTrafficSinceForSite(fixture.site.ID, fixture.now.Add(-time.Minute), "outbound")
	if err != nil {
		t.Fatal(err)
	}
	if persisted != 90 {
		t.Fatalf("persisted outbound=%d, want full-report delta 90", persisted)
	}
}

func TestDelayedFullReportCannotRegressNewerLiveCounters(t *testing.T) {
	fixture := newTelemetryFixture(t)
	fixture.full(t, 1, 1, 100, 200, 3, 0, 0)
	fixture.live(t, 1, 3, 180, 320, 6, 2*time.Second)
	fixture.full(t, 2, 2, 140, 260, 4, time.Second, 3*time.Second)

	got := fixture.snapshot(t, 3*time.Second).LiveSites[0]
	if got.CumulativeBytesIn != 180 || got.CumulativeBytesOut != 320 || got.Requests != 6 {
		t.Fatalf("delayed full report changed visible counters to %d/%d/%d, want 180/320/6",
			got.CumulativeBytesIn, got.CumulativeBytesOut, got.Requests)
	}
	persisted, err := fixture.app.db.SumTrafficSinceForSite(fixture.site.ID, fixture.now.Add(-time.Minute), "outbound")
	if err != nil {
		t.Fatal(err)
	}
	if persisted != 60 {
		t.Fatalf("delayed full report did not land durable bytes: got %d, want 60", persisted)
	}
}

func TestRealtimeBackfillAfterFullReportConservesAcceptedCounters(t *testing.T) {
	fixture := newTelemetryFixture(t)
	fixture.full(t, 1, 1, 100, 200, 3, 0, 0)
	fixture.live(t, 1, 2, 100, 200, 3, 0)
	var previous map[int64]dashboardRealtimeCounter
	var points []dashboardTrendPoint
	sample := func(at time.Duration) {
		snapshot := fixture.snapshot(t, at)
		point, next := dashboardRealtimePointFromSnapshot(snapshot, previous, snapshot.GeneratedAtMS)
		backfillDashboardRealtimeAgentTraffic(points, &point, snapshot.BillingMode, previous, next)
		points = appendDashboardRealtimePoint(points, point)
		previous = next
	}
	sample(0)
	fixture.live(t, 2, 3, 140, 260, 4, 2*time.Second)
	sample(2 * time.Second)
	sample(4 * time.Second)
	fixture.full(t, 2, 4, 150, 290, 5, 5*time.Second, 5*time.Second)
	sample(6 * time.Second)
	fixture.live(t, 3, 5, 180, 350, 6, 7*time.Second)
	sample(8 * time.Second)

	var bytesIn, bytesOut, requests int64
	for _, point := range points {
		bytesIn += point.BytesIn
		bytesOut += point.BytesOut
		requests += point.Requests
	}
	if bytesIn != 80 || bytesOut != 150 || requests != 3 {
		t.Fatalf("realtime sum=%d/%d/%d, want accepted cumulative delta 80/150/3", bytesIn, bytesOut, requests)
	}
}

func TestSSEImmediatelyPublishesBackfilledRealtimeBuckets(t *testing.T) {
	fixture := newTelemetryFixture(t)
	fixture.full(t, 1, 1, 100, 200, 3, 0, 0)
	fixture.live(t, 1, 2, 100, 200, 3, 0)
	type realtimeEvent struct {
		Point   dashboardTrendPoint   `json:"realtime_trend"`
		Updates []dashboardTrendPoint `json:"realtime_updates"`
	}
	publish := func() realtimeEvent {
		t.Helper()
		// The real sampler uses millisecond timestamps as point identities.
		time.Sleep(2 * time.Millisecond)
		fixture.app.pm.captureDashboardRealtimeSample()
		response := httptest.NewRecorder()
		if err := fixture.app.sendSSEEvent(response, response); err != nil {
			t.Fatal(err)
		}
		var event realtimeEvent
		body := strings.TrimSpace(strings.TrimPrefix(response.Body.String(), "data: "))
		if err := json.Unmarshal([]byte(body), &event); err != nil {
			t.Fatal(err)
		}
		return event
	}
	first := publish()
	fixture.live(t, 2, 3, 140, 260, 4, time.Since(fixture.now))
	second := publish()
	gap := publish()
	fixture.live(t, 3, 4, 180, 350, 6, time.Since(fixture.now))
	resumed := publish()

	visible := make(map[int64]dashboardTrendPoint)
	for _, event := range []realtimeEvent{first, second, gap, resumed} {
		for _, update := range event.Updates {
			visible[update.TimestampMS] = update
		}
		visible[event.Point.TimestampMS] = event.Point
	}
	var bytesIn, bytesOut, requests int64
	for _, point := range visible {
		bytesIn += point.BytesIn
		bytesOut += point.BytesOut
		requests += point.Requests
	}
	if bytesIn != 80 || bytesOut != 150 || requests != 3 {
		t.Fatalf("SSE consumer received %d/%d/%d, want corrected 80/150/3 without HTTP refresh",
			bytesIn, bytesOut, requests)
	}
	if got := visible[gap.Point.TimestampMS].BytesOut; got != 45 {
		t.Fatalf("SSE gap bucket outbound=%d, want immediate corrected value 45", got)
	}
}

func TestSwitchingAppliedNodeStartsARealtimeBaseline(t *testing.T) {
	fixture := newTelemetryFixture(t)
	fixture.full(t, 1, 1, 100, 200, 3, 0, 0)
	first := fixture.snapshot(t, 0)
	_, previous := dashboardRealtimePointFromSnapshot(first, nil, first.GeneratedAtMS)
	fixture.live(t, 1, 2, 140, 260, 4, 2*time.Second)
	second := fixture.snapshot(t, 2*time.Second)
	before, previous := dashboardRealtimePointFromSnapshot(second, previous, second.GeneratedAtMS)

	node, enrollment, err := fixture.app.db.CreateControlNode(NodeCreateInput{
		Name: "existing-counter-node", Address: "203.0.113.41", Port: 19441,
	}, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := fixture.app.db.EnrollControlNode(enrollment, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.db.RecordNodeReport(token, NodeReport{
		BootID: "telemetry-boot", ReportSessionID: "telemetry-session",
		CounterEpoch: "kernel:eth0", Sequence: 1, TelemetrySequence: 1, InterfaceName: "eth0",
	}, fixture.now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.db.SaveSiteNodeSchedule(fixture.site.ID, true, "fixed", node.ID, fixture.now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.refreshSiteAssignments(fixture.now.Add(4 * time.Second)); err != nil {
		t.Fatal(err)
	}
	replacement := fixture
	replacement.node, replacement.token = node, token
	replacement.full(t, 2, 2, 1_000, 2_000, 10, 4*time.Second, 4*time.Second)
	// The telemetry fixture supplies the successful DNS/application boundary.
	if _, err := fixture.app.db.db.Exec(
		"UPDATE site_node_schedules SET applied_node_id=?,dns_status='active' WHERE site_id=?",
		node.ID, fixture.site.ID,
	); err != nil {
		t.Fatal(err)
	}
	switched := fixture.snapshot(t, 5*time.Second)
	baseline, previous := dashboardRealtimePointFromSnapshot(switched, previous, switched.GeneratedAtMS)
	if baseline.BytesIn != 0 || baseline.BytesOut != 0 || baseline.Requests != 0 {
		t.Fatalf("replacement node's existing counters became new traffic: %+v", baseline)
	}
	replacement.live(t, 1, 3, 1_040, 2_060, 11, 6*time.Second)
	after := fixture.snapshot(t, 6*time.Second)
	point, _ := dashboardRealtimePointFromSnapshot(after, previous, after.GeneratedAtMS)
	if before.BytesIn+point.BytesIn != 80 || before.BytesOut+point.BytesOut != 120 || before.Requests+point.Requests != 2 {
		t.Fatalf("node switch did not preserve only new deltas: before=%+v after=%+v", before, point)
	}
}

func TestSameAppliedNodeCounterResetAccountsTheNewEpoch(t *testing.T) {
	fixture := newTelemetryFixture(t)
	fixture.full(t, 1, 1, 10, 20, 1, 0, 0)
	initial := fixture.snapshot(t, 0)
	baseline, previous := dashboardRealtimePointFromSnapshot(initial, nil, initial.GeneratedAtMS)
	if baseline.BytesIn != 0 || baseline.BytesOut != 0 || baseline.Requests != 0 {
		t.Fatalf("initial counters must establish a baseline: %+v", baseline)
	}
	fixture.live(t, 1, 2, 14, 26, 2, 2*time.Second)
	current := fixture.snapshot(t, 2*time.Second)
	before, previous := dashboardRealtimePointFromSnapshot(current, previous, current.GeneratedAtMS)
	fixture.full(t, 2, 3, 14, 26, 2, 3*time.Second, 3*time.Second)
	report := NodeReport{
		BootID: "telemetry-boot", ReportSessionID: "telemetry-session",
		CounterEpoch: "kernel:eth0", SiteCounterEpoch: "1",
		Sequence: 3, TelemetrySequence: 4, SampledAtMS: fixture.now.Add(4 * time.Second).UnixMilli(),
		InterfaceName: "eth0",
		SiteStats: []NodeSiteStat{{
			SiteID: fixture.site.ID, Host: fixture.site.PublicHost, CounterEpoch: 2,
			BytesIn: 60, BytesOut: 80, CumulativeBytesIn: 60, CumulativeBytesOut: 80, RequestCount: 4,
		}},
	}
	if _, err := fixture.app.db.RecordNodeReport(fixture.token, report, fixture.now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	reset := fixture.snapshot(t, 4*time.Second)
	after, _ := dashboardRealtimePointFromSnapshot(reset, previous, reset.GeneratedAtMS)
	if after.BytesIn != 60 || after.BytesOut != 80 || after.Requests != 4 || !after.SpeedUnavailable {
		t.Fatalf("same-node epoch reset must account its initial counters without the retired gauge: %+v", after)
	}
	if before.BytesIn+after.BytesIn != 64 || before.BytesOut+after.BytesOut != 86 || before.Requests+after.Requests != 5 {
		t.Fatalf("counter epochs did not conserve their separate deltas: before=%+v after=%+v", before, after)
	}
	persisted, err := fixture.app.db.SumTrafficSinceForSite(fixture.site.ID, fixture.now.Add(-time.Minute), "outbound")
	if err != nil || persisted != 86 {
		t.Fatalf("durable full reports must cover both counter epochs: outbound=%d err=%v", persisted, err)
	}
}

func TestFullReportsDoNotRefreshTheHeldLiveGauge(t *testing.T) {
	fixture := newTelemetryFixture(t)
	fixture.full(t, 1, 1, 100, 200, 3, 0, 0)
	fixture.live(t, 1, 2, 100, 200, 3, 0)
	for second := int64(1); second <= 4; second++ {
		at := time.Duration(second) * time.Second
		fixture.full(t, second+1, second+2, 100, 200, 3, at, at)
	}
	held := fixture.snapshot(t, 4*time.Second).LiveSites[0]
	if held.AgentRateReceivedAtMS != fixture.now.UnixMilli() || held.AgentDownloadBPS != 30 {
		t.Fatalf("full reports refreshed or replaced the held live gauge: %+v", held)
	}
	fixture.full(t, 6, 7, 100, 300, 4, 5*time.Second, 9*time.Second)
	fallback := fixture.snapshot(t, 9*time.Second).LiveSites[0]
	if fallback.AgentDownloadBPS != 100 || fallback.AgentUploadBPS != 0 || !fallback.AgentRateValid {
		t.Fatalf("fallback must use the one-second captured counter interval despite delivery delay: %+v", fallback)
	}
	if fallback.AgentRateReceivedAtMS != fixture.now.Add(9*time.Second).UnixMilli() {
		t.Fatalf("new fallback measurement did not receive its own freshness time: %+v", fallback)
	}
}

func TestDashboardTrendCacheReturnsTheCurrentRealtimeRevision(t *testing.T) {
	fixture := newTelemetryFixture(t)
	fixture.full(t, 1, 1, 100, 200, 3, 0, 0)
	fixture.live(t, 1, 2, 100, 200, 3, 0)
	fixture.app.pm.captureDashboardRealtimeSample()
	if _, err := fixture.app.pm.dashboardTrends(nil, "realtime"); err != nil {
		t.Fatal(err)
	}
	fixture.live(t, 2, 3, 140, 260, 4, time.Since(fixture.now))
	time.Sleep(2 * time.Millisecond)
	fixture.app.pm.captureDashboardRealtimeSample()
	response, err := fixture.app.pm.dashboardTrends(nil, "realtime")
	if err != nil {
		t.Fatal(err)
	}
	if len(response.RealtimePoints) != 2 || response.RealtimePoints[1].BytesOut != 60 {
		t.Fatalf("cached history returned an older realtime window: %+v", response.RealtimePoints)
	}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Epoch    string `json:"realtime_epoch"`
		Revision uint64 `json:"realtime_revision"`
	}
	if err := json.Unmarshal(body, &metadata); err != nil {
		t.Fatal(err)
	}
	current := fixture.app.pm.dashboardRealtimeWindowSnapshot(nil)
	if metadata.Epoch != current.Epoch || metadata.Revision != current.Revision {
		t.Fatalf("HTTP window metadata=%+v, want epoch=%q revision=%d", metadata, current.Epoch, current.Revision)
	}
}

func TestDashboardTrendBillingChangeDoesNotReuseTheOldRealtimeWindow(t *testing.T) {
	fixture := newTelemetryFixture(t)
	fixture.full(t, 1, 1, 100, 200, 3, 0, 0)
	fixture.live(t, 1, 2, 100, 200, 3, 0)
	fixture.app.pm.captureDashboardRealtimeSample()
	fixture.live(t, 2, 3, 140, 260, 4, time.Since(fixture.now))
	time.Sleep(2 * time.Millisecond)
	fixture.app.pm.captureDashboardRealtimeSample()
	if _, err := fixture.app.pm.dashboardTrends(nil, "realtime"); err != nil {
		t.Fatal(err)
	}
	oldEpoch := fixture.app.pm.dashboardRealtimeWindowSnapshot(nil).Epoch
	settings := fixture.app.db.currentSystemSettings()
	settings.TrafficBillingMode = trafficBillingModeOutbound
	if err := fixture.app.db.saveSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	response, err := fixture.app.pm.dashboardTrends(nil, "realtime")
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range response.RealtimePoints {
		if point.Traffic != point.BytesOut {
			t.Fatalf("outbound response contains a bidirectional realtime point: %+v", point)
		}
	}
	current := fixture.app.pm.dashboardRealtimeWindowSnapshot(nil)
	if current.Epoch == oldEpoch || current.BillingMode != response.BillingMode {
		t.Fatalf("billing change must create a matching authoritative epoch: window=%+v response billing=%q", current, response.BillingMode)
	}
}

func TestNetworkCounterEpochChangeDoesNotResetSiteTelemetry(t *testing.T) {
	fixture := newTelemetryFixture(t)
	fixture.full(t, 1, 1, 100, 200, 3, 0, 0)
	initial := fixture.snapshot(t, 0)
	_, previous := dashboardRealtimePointFromSnapshot(initial, nil, initial.GeneratedAtMS)
	fixture.live(t, 1, 2, 140, 260, 4, 2*time.Second)
	current := fixture.snapshot(t, 2*time.Second)
	_, previous = dashboardRealtimePointFromSnapshot(current, previous, current.GeneratedAtMS)
	if _, err := fixture.app.db.RecordNodeReport(fixture.token, NodeReport{
		BootID: "telemetry-boot", ReportSessionID: "telemetry-session", CounterEpoch: "kernel:eth1",
		SiteCounterEpoch: "1", Sequence: 2, TelemetrySequence: 3,
		SampledAtMS: fixture.now.Add(3 * time.Second).UnixMilli(), InterfaceName: "eth1",
		SiteStats: []NodeSiteStat{{
			SiteID: fixture.site.ID, Host: fixture.site.PublicHost, CounterEpoch: 1,
			CumulativeBytesIn: 150, CumulativeBytesOut: 290, RequestCount: 5,
		}},
	}, fixture.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	after := fixture.snapshot(t, 3*time.Second)
	point, _ := dashboardRealtimePointFromSnapshot(after, previous, after.GeneratedAtMS)
	if point.BytesIn != 10 || point.BytesOut != 30 || point.Requests != 1 || point.DownloadBPS != 30 || point.UploadBPS != 20 {
		t.Fatalf("a NIC epoch change reset stable site counters or gauge: %+v", point)
	}
	accepted, _, err := fixture.app.db.recordNodeLiveReport(fixture.node.ID, NodeLiveReport{
		ReportSessionID: "telemetry-session", CounterEpoch: "kernel:eth0",
		Sequence: 2, TelemetrySequence: 2, SampledAtMS: fixture.now.Add(2 * time.Second).UnixMilli(),
		SiteStats: []NodeLiveSiteTraffic{{
			SiteID: fixture.site.ID, Host: fixture.site.PublicHost, CounterEpoch: 1,
			CumulativeBytesIn: 140, CumulativeBytesOut: 260, Requests: 4,
			DownloadBPS: 30, UploadBPS: 20, RateValid: true,
		}},
	}, fixture.now.Add(4*time.Second))
	if err != nil || len(accepted) != 0 {
		t.Fatalf("a delayed old-NIC sample escaped the process watermark: accepted=%v err=%v", accepted, err)
	}
}

func TestLiveSequenceRestartKeepsSharedTelemetryOrdering(t *testing.T) {
	fixture := newTelemetryFixture(t)
	fixture.full(t, 1, 1, 100, 200, 3, 0, 0)
	fixture.live(t, 10, 2, 140, 260, 4, 2*time.Second)
	fixture.live(t, 1, 3, 180, 350, 6, 4*time.Second)
	got := fixture.snapshot(t, 4*time.Second).LiveSites[0]
	if got.CumulativeBytesIn != 180 || got.CumulativeBytesOut != 350 || got.Requests != 6 {
		t.Fatalf("restarted live channel lost a newer shared sample: %+v", got)
	}
}

func TestUnavailablePersistedFallbackDoesNotManufactureCounterReset(t *testing.T) {
	for _, scenario := range []string{"new-session-not-yet-reported", "overlay-expired"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newTelemetryFixture(t)
			fixture.full(t, 1, 1, 100, 200, 3, 0, 0)
			initial := fixture.snapshot(t, 0)
			_, previous := dashboardRealtimePointFromSnapshot(initial, nil, initial.GeneratedAtMS)
			fixture.live(t, 1, 2, 140, 260, 4, 2*time.Second)
			current := fixture.snapshot(t, 2*time.Second)
			_, previous = dashboardRealtimePointFromSnapshot(current, previous, current.GeneratedAtMS)
			at := 3 * time.Second
			if scenario == "new-session-not-yet-reported" {
				if _, err := fixture.app.db.db.Exec(
					"UPDATE control_nodes SET active_agent_session_id=?,agent_session_epoch=2,agent_lease_id=? WHERE id=?",
					"pending-session", "synthetic-lease", fixture.node.ID,
				); err != nil {
					t.Fatal(err)
				}
			} else {
				at = 93 * time.Second
			}
			unavailable := fixture.snapshot(t, at)
			point, next := dashboardRealtimePointFromSnapshot(unavailable, previous, unavailable.GeneratedAtMS)
			if point.BytesIn != 0 || point.BytesOut != 0 || point.Requests != 0 || !point.SpeedUnavailable {
				t.Fatalf("old durable fallback manufactured realtime counters: %+v", point)
			}
			if scenario == "overlay-expired" {
				fixture.live(t, 2, 3, 150, 280, 5, 94*time.Second)
				resumed := fixture.snapshot(t, 94*time.Second)
				delta, _ := dashboardRealtimePointFromSnapshot(resumed, next, resumed.GeneratedAtMS)
				if delta.BytesIn != 10 || delta.BytesOut != 20 || delta.Requests != 1 {
					t.Fatalf("same runtime after cache expiry did not resume from its previous watermark: %+v", delta)
				}
			}
		})
	}
}

func TestDelayedLiveGaugeCanAdvanceWithoutRegressingFullCounters(t *testing.T) {
	fixture := newTelemetryFixture(t)
	fixture.full(t, 1, 1, 100, 200, 3, 0, 0)
	fixture.live(t, 1, 2, 140, 260, 4, 2*time.Second)
	fixture.full(t, 2, 4, 180, 350, 6, 5*time.Second, 5*time.Second)
	accepted, _, err := fixture.app.db.recordNodeLiveReport(fixture.node.ID, NodeLiveReport{
		ReportSessionID: "telemetry-session", CounterEpoch: "kernel:eth0",
		Sequence: 2, TelemetrySequence: 3, SampledAtMS: fixture.now.Add(4 * time.Second).UnixMilli(),
		SiteStats: []NodeLiveSiteTraffic{{
			SiteID: fixture.site.ID, Host: fixture.site.PublicHost, CounterEpoch: 1,
			CumulativeBytesIn: 160, CumulativeBytesOut: 320, Requests: 5,
			DownloadBPS: 80, UploadBPS: 40, RateValid: true,
		}},
	}, fixture.now.Add(5500*time.Millisecond))
	if err != nil || len(accepted) != 1 {
		t.Fatalf("newer live gauge was discarded with its older counters: accepted=%v err=%v", accepted, err)
	}
	got := fixture.snapshot(t, 6*time.Second).LiveSites[0]
	if got.CumulativeBytesIn != 180 || got.CumulativeBytesOut != 350 || got.Requests != 6 ||
		got.AgentDownloadBPS != 80 || got.AgentUploadBPS != 40 ||
		got.AgentRateReceivedAtMS != fixture.now.Add(5500*time.Millisecond).UnixMilli() {
		t.Fatalf("counter/gauge ordering was not independent: %+v", got)
	}
}

func TestDelayedFullReportsKeepDurableBaselinesAndUnreportedSites(t *testing.T) {
	fixture := newTelemetryFixture(t)
	fixture.full(t, 1, 1, 100, 200, 3, 0, 0)
	fixture.full(t, 3, 3, 180, 320, 6, 3*time.Second, 3*time.Second)
	other, err := fixture.app.db.CreateSiteRecord(Site{
		Name: "partial-telemetry", ListenPort: freePort(t), PublicHost: "partial-telemetry.example",
		IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:1",
		PlaybackMode: "direct", MainVideoStreamMode: "proxy", StreamHosts: "[]", UAMode: passthroughUAMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.db.SaveSiteNodeSchedule(other.ID, true, "fixed", fixture.node.ID, fixture.now); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.db.db.Exec("UPDATE site_node_schedules SET applied_node_id=?,dns_status='active' WHERE site_id=?", fixture.node.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	late := NodeReport{
		BootID: "telemetry-boot", ReportSessionID: "telemetry-session", CounterEpoch: "kernel:eth0", SiteCounterEpoch: "1",
		Sequence: 2, TelemetrySequence: 2, SampledAtMS: fixture.now.Add(2 * time.Second).UnixMilli(), InterfaceName: "eth0",
		SiteStats: []NodeSiteStat{
			{SiteID: fixture.site.ID, Host: fixture.site.PublicHost, CounterEpoch: 1, CumulativeBytesIn: 140, CumulativeBytesOut: 260, RequestCount: 4},
			{SiteID: other.ID, Host: other.PublicHost, CounterEpoch: 1, BytesIn: 50, BytesOut: 70, CumulativeBytesIn: 50, CumulativeBytesOut: 70, RequestCount: 2},
		},
	}
	if _, err := fixture.app.db.RecordNodeReport(fixture.token, late, fixture.now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	fixture.full(t, 4, 4, 200, 350, 7, 5*time.Second, 5*time.Second)
	first, err := fixture.app.db.SumTrafficSinceForSite(fixture.site.ID, fixture.now.Add(-time.Minute), "outbound")
	if err != nil || first != 150 {
		t.Fatalf("out-of-order full report regressed durable baseline: got %d want150 err=%v", first, err)
	}
	second, err := fixture.app.db.SumTrafficSinceForSite(other.ID, fixture.now.Add(-time.Minute), "outbound")
	if err != nil || second != 70 {
		t.Fatalf("older global report lost an unreported site's bytes: got %d want70 err=%v", second, err)
	}
}

func TestDelayedLegacyRequestOnlyReportKeepsItsDurableWatermark(t *testing.T) {
	fixture := newTelemetryFixture(t)
	for index, sample := range []struct {
		sequence int64
		requests int64
	}{{1, 1}, {3, 3}, {2, 2}, {4, 4}} {
		if _, err := fixture.app.db.RecordNodeReport(fixture.token, NodeReport{
			BootID: "legacy-requests", ReportSessionID: "legacy-requests",
			CounterEpoch: "kernel:eth0", Sequence: sample.sequence,
			InterfaceName: "eth0",
			SiteStats: []NodeSiteStat{{
				SiteID: fixture.site.ID, Host: fixture.site.PublicHost, RequestCount: sample.requests,
			}},
		}, fixture.now.Add(time.Duration(index+1)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	logs, err := fixture.app.db.GetTrafficTrendLogs(&fixture.site.ID, fixture.now.Add(-time.Minute), fixture.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var requests int64
	for _, log := range logs {
		requests += log.Requests
	}
	if requests != 4 {
		t.Fatalf("request-only legacy replay counted %d requests, want cumulative4", requests)
	}
}

func TestCoveredFullReportCannotSeedAColdRealtimeOverlay(t *testing.T) {
	fixture := newTelemetryFixture(t)
	fixture.full(t, 1, 1, 100, 200, 3, 0, 0)
	fixture.full(t, 3, 3, 180, 320, 6, 3*time.Second, 3*time.Second)
	fixture.app.db.agentLiveMu.Lock()
	fixture.app.db.agentLive = nil
	fixture.app.db.agentLiveMu.Unlock()
	fixture.full(t, 2, 2, 140, 260, 4, 2*time.Second, 4*time.Second)
	got := fixture.snapshot(t, 4*time.Second).LiveSites[0]
	if got.CumulativeBytesIn != 180 || got.CumulativeBytesOut != 320 || got.Requests != 6 {
		t.Fatalf("ACKed stale full body re-seeded a cold overlay: %+v", got)
	}
}

func TestFullProcessingCannotClearALaterReceivedLiveGauge(t *testing.T) {
	fixture := newTelemetryFixture(t)
	fixture.full(t, 1, 1, 100, 200, 3, 0, 0)
	if _, _, err := fixture.app.db.recordNodeLiveReport(fixture.node.ID, NodeLiveReport{
		ReportSessionID: "telemetry-session", CounterEpoch: "kernel:eth0",
		Sequence: 1, TelemetrySequence: 2, SampledAtMS: fixture.now.Add(4 * time.Second).UnixMilli(),
		SiteStats: []NodeLiveSiteTraffic{{
			SiteID: fixture.site.ID, Host: fixture.site.PublicHost, CounterEpoch: 1,
			CumulativeBytesIn: 160, CumulativeBytesOut: 320, Requests: 5,
			DownloadBPS: 80, UploadBPS: 40, RateValid: true,
		}},
	}, fixture.now.Add(5500*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	// Receipt and completion can be ordered differently. The full report's
	// captured counters are newer, but its recorded receive time precedes the
	// independently received live gauge.
	fixture.full(t, 2, 3, 180, 350, 6, 5*time.Second, 5*time.Second)
	got := fixture.snapshot(t, 6*time.Second).LiveSites[0]
	if got.CumulativeBytesOut != 350 || got.AgentDownloadBPS != 80 || got.AgentUploadBPS != 40 {
		t.Fatalf("full completion cleared a later-received valid live gauge: %+v", got)
	}
}
