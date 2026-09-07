package main

import (
	"testing"
	"time"
)

func TestTrafficCycleStartUsesConfiguredResetDay(t *testing.T) {
	location := time.FixedZone("UTC+08:00", 8*60*60)
	date := func(year int, month time.Month, day int) time.Time {
		return time.Date(year, month, day, 12, 0, 0, 0, location)
	}
	for _, tc := range []struct {
		name     string
		now      time.Time
		resetDay int
		want     time.Time
	}{
		{name: "first day", now: date(2026, time.August, 18), resetDay: 1, want: date(2026, time.August, 1)},
		{name: "disabled", now: date(2026, time.August, 18), resetDay: 0, want: time.Time{}},
		{name: "before reset", now: date(2026, time.August, 10), resetDay: 15, want: date(2026, time.July, 15)},
		{name: "after reset", now: date(2026, time.August, 18), resetDay: 15, want: date(2026, time.August, 15)},
		{name: "short leap month", now: date(2028, time.March, 1), resetDay: 31, want: date(2028, time.February, 29)},
		{name: "short current month", now: date(2027, time.February, 28), resetDay: 31, want: date(2027, time.February, 28)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := trafficCycleStart(tc.now, tc.resetDay, location)
			if !got.Equal(tc.want) && (got.Year() != tc.want.Year() || got.Month() != tc.want.Month() || got.Day() != tc.want.Day() || got.Hour() != 0) {
				t.Fatalf("trafficCycleStart(%s, %d) = %s, want %s", tc.now, tc.resetDay, got, tc.want)
			}
		})
	}
}

func TestTrafficCycleStartUsesSchedulingTimezoneInsteadOfServerTimezone(t *testing.T) {
	beijing := time.FixedZone("UTC+08:00", 8*60*60)
	nowUTC := time.Date(2026, time.September, 30, 16, 30, 0, 0, time.UTC)
	got := trafficCycleStart(nowUTC, 1, beijing)
	want := time.Date(2026, time.October, 1, 0, 0, 0, 0, beijing)
	if !got.Equal(want) || got.Location().String() != beijing.String() {
		t.Fatalf("traffic cycle start = %s, want %s", got, want)
	}
}

func TestTrafficCycleSnapshotsUseSchedulingTimezoneAgainstUTCStorage(t *testing.T) {
	originalLocal := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = originalLocal })

	app := newTestApp(t)
	settings := app.db.currentSystemSettings()
	settings.ScheduleTimezone = beijingTimezoneOffsetMinutes
	settings.TrafficResetDay = 1
	settings.TrafficBillingMode = trafficBillingModeBidirectional
	if err := app.db.saveSystemSettings(settings); err != nil {
		t.Fatalf("save scheduling timezone: %v", err)
	}

	site, err := app.db.CreateSite("timezone-cycle", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	cycleStart := trafficCycleStart(time.Now(), 1, timezoneLocation(beijingTimezoneOffsetMinutes))
	if err := app.db.addTrafficWithRequestsAt(site.ID, 10, 20, 1, cycleStart.Add(-time.Minute)); err != nil {
		t.Fatalf("add pre-cycle traffic: %v", err)
	}
	if err := app.db.addTrafficWithRequestsAt(site.ID, 30, 40, 1, cycleStart.Add(time.Minute)); err != nil {
		t.Fatalf("add current-cycle traffic: %v", err)
	}

	snapshot, err := app.pm.TrafficSnapshot()
	if err != nil {
		t.Fatalf("TrafficSnapshot: %v", err)
	}
	if snapshot.MonthlyTraffic != 140 {
		t.Fatalf("monthly traffic = %d, want only post-boundary traffic 140", snapshot.MonthlyTraffic)
	}
	if live := findLiveSite(t, snapshot, site.ID); live.MonthlyTraffic != 140 {
		t.Fatalf("site monthly traffic = %d, want 140", live.MonthlyTraffic)
	}

	usage, err := app.pm.currentTrafficCycleUsage(&ProxyInstance{Site: *site}, time.Now())
	if err != nil {
		t.Fatalf("currentTrafficCycleUsage: %v", err)
	}
	if usage != 140 {
		t.Fatalf("quota cycle usage = %d, want 140", usage)
	}
}

func TestTrafficCycleUsageRespectsSiteAndBillingMode(t *testing.T) {
	app := newTestApp(t)
	first, err := app.db.CreateSite("cycle-first", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite first: %v", err)
	}
	second, err := app.db.CreateSite("cycle-second", freePort(t), "http://127.0.0.1:8097", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite second: %v", err)
	}
	now := time.Now().In(time.Local)
	start := now.Add(-24 * time.Hour)
	if err := app.db.addTrafficWithRequestsAt(first.ID, 100, 300, 1, now.Add(-time.Hour)); err != nil {
		t.Fatalf("add first traffic: %v", err)
	}
	if err := app.db.addTrafficWithRequestsAt(second.ID, 900, 700, 1, now.Add(-time.Hour)); err != nil {
		t.Fatalf("add second traffic: %v", err)
	}
	dual, err := app.db.SumTrafficSinceForSite(first.ID, start, trafficBillingModeBidirectional)
	if err != nil {
		t.Fatalf("dual cycle usage: %v", err)
	}
	outbound, err := app.db.SumTrafficSinceForSite(first.ID, start, trafficBillingModeOutbound)
	if err != nil {
		t.Fatalf("outbound cycle usage: %v", err)
	}
	if dual != 800 || outbound != 300 {
		t.Fatalf("cycle usage = dual %d outbound %d, want 800/300", dual, outbound)
	}
}

func TestTrafficResetDayValidation(t *testing.T) {
	settings := defaultSystemSettings()
	settings.TrafficResetDay = 0
	if _, err := normalizeSystemSettings(settings); err != nil {
		t.Fatalf("disabled reset rejected: %v", err)
	}
	settings.TrafficResetDay = 31
	if _, err := normalizeSystemSettings(settings); err != nil {
		t.Fatalf("day 31 rejected: %v", err)
	}
	for _, invalid := range []int{-1, 32} {
		settings.TrafficResetDay = invalid
		if _, err := normalizeSystemSettings(settings); err == nil {
			t.Fatalf("invalid reset day %d accepted", invalid)
		}
	}
}

func TestDisabledTrafficResetUsesAllPersistedUsage(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSite("no-reset", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if err := app.db.addTrafficWithRequestsAt(site.ID, 120, 280, 1, time.Date(2024, time.January, 1, 0, 0, 0, 0, time.Local)); err != nil {
		t.Fatalf("add historical traffic: %v", err)
	}
	usage, err := app.db.SumTrafficSinceForSite(site.ID, trafficCycleStart(time.Now(), 0, time.UTC), trafficBillingModeBidirectional)
	if err != nil {
		t.Fatalf("sum no-reset usage: %v", err)
	}
	if usage != 800 {
		t.Fatalf("no-reset usage = %d, want all-time 800", usage)
	}
}

func TestTrafficBillingModeAppliesToAllTrafficSnapshots(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSite("billing", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if err := app.db.addTrafficWithRequests(site.ID, 100, 300, 4); err != nil {
		t.Fatalf("add traffic: %v", err)
	}
	dual, err := app.pm.TrafficSnapshot()
	if err != nil {
		t.Fatalf("dual snapshot: %v", err)
	}
	if dual.TotalTraffic != 800 || dual.BillingMode != trafficBillingModeBidirectional {
		t.Fatalf("dual snapshot = %+v, want 800 bidirectional", dual)
	}

	settings := app.db.currentSystemSettings()
	settings.TrafficBillingMode = trafficBillingModeOutbound
	if err := app.db.saveSystemSettings(settings); err != nil {
		t.Fatalf("save outbound mode: %v", err)
	}
	outbound, err := app.pm.TrafficSnapshot()
	if err != nil {
		t.Fatalf("outbound snapshot: %v", err)
	}
	if outbound.TotalTraffic != 300 || outbound.BillingMode != trafficBillingModeOutbound || outbound.MonthlyTraffic != 300 {
		t.Fatalf("outbound snapshot = %+v, want 300 outbound", outbound)
	}
	updatedSite, err := app.db.GetSite(site.ID)
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}
	history, err := app.pm.SiteTrafficHistory(*updatedSite, 24)
	if err != nil {
		t.Fatalf("outbound history: %v", err)
	}
	if history.Snapshot.TrafficUsed != 300 || history.BillingMode != trafficBillingModeOutbound {
		t.Fatalf("outbound history = %+v, want billed traffic 300", history)
	}
}

func TestTrafficAggregatesIncludeControllerAndAgentSources(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSite("mixed-sources", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	now := time.Now().In(time.Local)
	start := now.Add(-time.Hour)
	if err := app.db.addTrafficWithRequestsAt(site.ID, 100, 200, 1, now.Add(-20*time.Minute)); err != nil {
		t.Fatalf("controller traffic: %v", err)
	}
	if _, err := app.db.db.Exec(`INSERT INTO node_site_traffic_logs(node_id,site_id,bytes_in,bytes_out,requests,recorded_at_ms) VALUES(?,?,?,?,?,?)`, 77, site.ID, 300, 400, 2, now.Add(-10*time.Minute).UnixMilli()); err != nil {
		t.Fatalf("agent traffic: %v", err)
	}
	if got, err := app.db.SumTrafficSince(start, trafficBillingModeBidirectional); err != nil || got != 2000 {
		t.Fatalf("SumTrafficSince = %d, %v; want 2000", got, err)
	}
	bySite, err := app.db.SumTrafficSinceBySite(start, trafficBillingModeBidirectional)
	if err != nil || bySite[site.ID] != 2000 {
		t.Fatalf("SumTrafficSinceBySite = %v, %v; want site 2000", bySite, err)
	}
	if got, err := app.db.SumTrafficSinceForSite(site.ID, start, trafficBillingModeOutbound); err != nil || got != 600 {
		t.Fatalf("SumTrafficSinceForSite = %d, %v; want 600 outbound", got, err)
	}
}

func TestDashboardIncludesScheduledAgentTraffic(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "dashboard-agent", Address: "203.0.113.90", Port: 9090}, now)
	if err != nil {
		t.Fatalf("CreateControlNode: %v", err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatalf("EnrollControlNode: %v", err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "dashboard-scheduled", PublicHost: "dashboard-scheduled.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatalf("CreateSiteRecord: %v", err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatalf("SaveSiteNodeSchedule: %v", err)
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=?,dns_status='active' WHERE site_id=?`, node.ID, node.ID, site.ID); err != nil {
		t.Fatalf("apply schedule: %v", err)
	}
	report := NodeReport{
		BootID:          "dashboard-boot",
		ReportSessionID: "dashboard-session",
		CounterEpoch:    "dashboard-counter",
		Sequence:        1,
		InterfaceName:   "eth0",
		SiteStats:       []NodeSiteStat{{Host: site.PublicHost, RequestCount: 7, LastRequestAtMS: now.UnixMilli(), LastStatus: 200, BytesIn: 300, BytesOut: 400, CumulativeBytesIn: 300, CumulativeBytesOut: 400}},
	}
	if _, err := app.db.RecordNodeReport(token, report, now); err != nil {
		t.Fatalf("RecordNodeReport: %v", err)
	}
	sites, err := app.db.ListSites()
	if err != nil {
		t.Fatalf("ListSites: %v", err)
	}
	settings := app.db.currentSystemSettings()
	snapshot, err := app.pm.dashboardSnapshotWithSites(sites, settings, now.Add(time.Second))
	if err != nil {
		t.Fatalf("dashboard snapshot: %v", err)
	}
	live := findLiveSite(t, snapshot, site.ID)
	if !live.Running || live.CumulativeBytesIn != 300 || live.CumulativeBytesOut != 400 || live.Requests != 7 {
		t.Fatalf("scheduled Agent live site = %+v", live)
	}
	if live.MonthlyTraffic != 1400 || snapshot.MonthlyTraffic != 1400 || snapshot.TotalTraffic != 1400 || snapshot.TotalRequests != 7 {
		t.Fatalf("scheduled Agent dashboard totals = snapshot=%+v live=%+v", snapshot, live)
	}
	if snapshot.GeneratedAtMS != now.Add(time.Second).UnixMilli() {
		t.Fatalf("snapshot GeneratedAtMS=%d, want %d", snapshot.GeneratedAtMS, now.Add(time.Second).UnixMilli())
	}
	bootstrap, err := app.dashboardBootstrap()
	if err != nil {
		t.Fatalf("dashboard bootstrap: %v", err)
	}
	if len(bootstrap.Sites) != 1 || bootstrap.Sites[0].TrafficUsed != 1400 || !bootstrap.Sites[0].Running {
		t.Fatalf("dashboard bootstrap site = %+v", bootstrap.Sites)
	}
}

func TestLegacyTrafficDirectionsAreInitializedFromTotal(t *testing.T) {
	in, out := legacyTrafficDirections(101)
	if in != 50 || out != 51 || in+out != 101 {
		t.Fatalf("legacy directions = %d/%d, want 50/51", in, out)
	}
}

func TestCurrentTrafficCycleUsageCachesPersistedAndTracksFlushes(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSite("quota-cycle", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 1000, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	now := time.Now().In(time.Local)
	if err := app.db.addTrafficWithRequestsAt(site.ID, 100, 200, 1, now.Add(-time.Minute)); err != nil {
		t.Fatalf("add persisted traffic: %v", err)
	}
	inst := &ProxyInstance{Site: *site}
	inst.bytesIn.Store(10)
	inst.bytesOut.Store(20)

	usage, err := app.pm.currentTrafficCycleUsage(inst, now)
	if err != nil {
		t.Fatalf("current usage: %v", err)
	}
	if usage != 660 {
		t.Fatalf("current usage = %d, want 660", usage)
	}
	if err := app.pm.flushProxyTraffic(inst); err != nil {
		t.Fatalf("flush traffic: %v", err)
	}
	usage, err = app.pm.currentTrafficCycleUsage(inst, now)
	if err != nil {
		t.Fatalf("usage after flush: %v", err)
	}
	if usage != 660 {
		t.Fatalf("usage after flush = %d, want 660 without double counting", usage)
	}

	settings := app.db.currentSystemSettings()
	settings.TrafficBillingMode = trafficBillingModeOutbound
	if err := app.db.saveSystemSettings(settings); err != nil {
		t.Fatalf("save outbound mode: %v", err)
	}
	usage, err = app.pm.currentTrafficCycleUsage(inst, now)
	if err != nil {
		t.Fatalf("outbound usage: %v", err)
	}
	if usage != 220 {
		t.Fatalf("outbound usage = %d, want 220", usage)
	}
}

func TestTrafficBillableBytesCountsBothVPSNetworkLegs(t *testing.T) {
	videoBytes := int64(784) * (1 << 30) / 100
	if got := trafficBillableBytes(trafficBillingModeBidirectional, 0, videoBytes); got != videoBytes*2 {
		t.Fatalf("bidirectional 7.84 GiB video = %d, want %d", got, videoBytes*2)
	}
	if got := trafficBillableBytes(trafficBillingModeOutbound, 0, videoBytes); got != videoBytes {
		t.Fatalf("outbound 7.84 GiB video = %d, want %d", got, videoBytes)
	}
}

func TestTrafficBillableBytesClampsInvalidAndOverflowingCounters(t *testing.T) {
	if got := trafficBillableBytes(trafficBillingModeBidirectional, -10, 5); got != 10 {
		t.Fatalf("negative input clamp = %d, want 10", got)
	}
	maxInt64 := int64(^uint64(0) >> 1)
	if got := trafficBillableBytes(trafficBillingModeBidirectional, maxInt64, 1); got != maxInt64 {
		t.Fatalf("overflow result = %d, want saturated %d", got, maxInt64)
	}
}
