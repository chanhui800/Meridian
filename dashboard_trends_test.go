package main

import (
	"testing"
	"time"
)

func sumDashboardTrend(points []dashboardTrendPoint) (traffic, requests int64) {
	for _, point := range points {
		traffic += point.Traffic
		requests += point.Requests
	}
	return traffic, requests
}

func TestDashboardTrendsAggregateAllAndSingleSite(t *testing.T) {
	app := newTestApp(t)
	first, err := app.db.CreateSite("first", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite first: %v", err)
	}
	second, err := app.db.CreateSite("second", freePort(t), "http://127.0.0.1:8097", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite second: %v", err)
	}
	now := time.Now().In(time.Local)
	if err := app.db.addTrafficWithRequestsAt(first.ID, 100, 200, 3, now.Add(-10*time.Minute)); err != nil {
		t.Fatalf("add first traffic: %v", err)
	}
	if err := app.db.addTrafficWithRequestsAt(second.ID, 50, 50, 2, now.Add(-5*time.Minute)); err != nil {
		t.Fatalf("add second traffic: %v", err)
	}
	inst := &ProxyInstance{Site: *first}
	inst.bytesIn.Store(10)
	inst.bytesOut.Store(20)
	inst.pendingRequests.Store(1)
	app.pm.proxies[first.ID] = inst

	all, err := app.pm.dashboardTrends(nil, "hour")
	if err != nil {
		t.Fatalf("dashboardTrends all: %v", err)
	}
	if all.SiteID != "all" || all.Range != "hour" || all.BucketSeconds != 60 || len(all.Points) != 60 {
		t.Fatalf("all metadata = %+v", all)
	}
	traffic, requests := sumDashboardTrend(all.Points)
	if traffic != 430 || requests != 6 {
		t.Fatalf("all totals = traffic %d requests %d, want 430/6", traffic, requests)
	}

	single, err := app.pm.dashboardTrends(&first.ID, "hour")
	if err != nil {
		t.Fatalf("dashboardTrends single: %v", err)
	}
	traffic, requests = sumDashboardTrend(single.Points)
	if traffic != 330 || requests != 4 {
		t.Fatalf("single totals = traffic %d requests %d, want 330/4", traffic, requests)
	}
	last := single.Points[len(single.Points)-1]
	if last.Traffic < 30 || last.Requests < 1 {
		t.Fatalf("current bucket = %+v, want pending traffic and request", last)
	}
}

func TestDashboardTrendRangesAndMonthlyTraffic(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSite("monthly", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	now := time.Now().In(time.Local)
	if err := app.db.addTrafficWithRequestsAt(site.ID, 70, 30, 2, now.Add(-time.Minute)); err != nil {
		t.Fatalf("add traffic: %v", err)
	}
	inst := &ProxyInstance{Site: *site}
	inst.Site.TrafficUsed = 100
	inst.persistedTraffic.Store(100)
	inst.bytesIn.Store(5)
	inst.bytesOut.Store(15)
	app.pm.proxies[site.ID] = inst

	for rangeName, expectedPoints := range map[string]int{"realtime": 30, "hour": 60, "6h": 72, "day": 24, "7d": 168} {
		trend, err := app.pm.dashboardTrends(nil, rangeName)
		if err != nil {
			t.Fatalf("range %s: %v", rangeName, err)
		}
		if len(trend.Points) != expectedPoints {
			t.Fatalf("range %s points = %d, want %d", rangeName, len(trend.Points), expectedPoints)
		}
	}
	if _, err := app.pm.dashboardTrends(nil, "invalid"); err == nil {
		t.Fatal("invalid range accepted")
	}

	snapshot, err := app.pm.TrafficSnapshot()
	if err != nil {
		t.Fatalf("TrafficSnapshot: %v", err)
	}
	if snapshot.MonthlyTraffic != 120 || snapshot.TotalTraffic != 120 {
		t.Fatalf("snapshot traffic = monthly %d total %d, want 120/120", snapshot.MonthlyTraffic, snapshot.TotalTraffic)
	}
	if len(snapshot.LiveSites) != 1 || snapshot.LiveSites[0].MonthlyTraffic != 120 {
		t.Fatalf("site monthly traffic = %+v, want 120", snapshot.LiveSites)
	}
}
