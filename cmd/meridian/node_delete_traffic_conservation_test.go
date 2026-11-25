package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type deletedNodeTrafficFixture struct {
	app      *App
	sites    [2]*Site
	nodes    [2]ControlNode
	base     time.Time
	adminID  int64
	mode     string
	resetDay int
}

func newDeletedNodeTrafficFixture(t *testing.T, mode string, resetDay int) deletedNodeTrafficFixture {
	t.Helper()
	for _, name := range []string{"TLS_STATE_DIR", "PANEL_TLS_CERT_FILE", "PANEL_TLS_KEY_FILE", "EDGE_TLS_CERT_FILE", "EDGE_TLS_KEY_FILE"} {
		t.Setenv(name, "")
	}
	// SQLite's OS localtime can differ from Go's location. Transferred wall
	// text must follow the same location as the normal controller writer.
	previousLocation := time.Local
	time.Local = timezoneLocation(345)
	t.Cleanup(func() { time.Local = previousLocation })
	app := newTestApp(t)
	settings := app.db.currentSystemSettings()
	settings.TrafficBillingMode, settings.TrafficResetDay = mode, resetDay
	if err := app.db.saveSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	f := deletedNodeTrafficFixture{app: app, mode: mode, resetDay: resetDay,
		base: time.Date(now.Year(), now.Month(), 15, 12, 0, 0, 0, time.UTC)}
	tokens := [2]string{}
	for index := range f.sites {
		var err error
		f.sites[index], err = app.db.CreateSiteRecord(Site{Name: fmt.Sprintf("ledger site %d", index+1),
			ListenPort: -101 - index, PublicHost: fmt.Sprintf("ledger-%d.example.test", index+1), IngressMode: ingressModeHost,
			TargetURL: "http://127.0.0.1:8096", StreamHosts: "[]", UAMode: passthroughUAMode, TrafficQuota: 350})
		if err != nil {
			t.Fatal(err)
		}
		node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: fmt.Sprintf("ledger node %d", index+1),
			Address: fmt.Sprintf("203.0.113.%d", 24+index), Port: 19091}, f.base)
		if err != nil {
			t.Fatal(err)
		}
		f.nodes[index], tokens[index], err = app.db.EnrollControlNode(enrollment, f.base)
		if err != nil || f.nodes[index].ID != node.ID {
			t.Fatalf("node enrollment: %v", err)
		}
	}
	if _, err := app.db.db.Exec(`INSERT INTO site_node_schedules(site_id,enabled,mode,desired_node_id,applied_node_id,dns_status)
		VALUES(?,1,'global',?,?,'active'),(?,1,'global',?,?,'active')`,
		f.sites[0].ID, f.nodes[0].ID, f.nodes[1].ID, f.sites[1].ID, f.nodes[0].ID, f.nodes[0].ID); err != nil {
		t.Fatal(err)
	}
	for _, point := range []struct {
		siteIndex, minute int
		in, out, requests int64
	}{{0, 0, 3, 7, 1}, {0, 1, 5, 11, 2}, {1, 0, 1, 2, 1}} {
		if err := app.db.addTrafficWithRequestsAt(f.sites[point.siteIndex].ID, point.in, point.out, point.requests,
			f.base.Add(time.Duration(point.minute)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	for nodeIndex, token := range tokens {
		for minute := range 2 {
			stats := []NodeSiteStat{}
			if nodeIndex == 0 {
				if minute == 0 {
					stats = []NodeSiteStat{
						{SiteID: f.sites[0].ID, Host: f.sites[0].PublicHost, BytesIn: 10, BytesOut: 100, CumulativeBytesIn: 10, CumulativeBytesOut: 100, RequestCount: 2, CounterEpoch: 1},
						{SiteID: f.sites[1].ID, Host: f.sites[1].PublicHost, BytesIn: 4, BytesOut: 40, CumulativeBytesIn: 4, CumulativeBytesOut: 40, RequestCount: 1, CounterEpoch: 1},
					}
				} else {
					stats = []NodeSiteStat{
						{SiteID: f.sites[0].ID, Host: f.sites[0].PublicHost, BytesIn: 20, BytesOut: 50, CumulativeBytesIn: 30, CumulativeBytesOut: 150, RequestCount: 5, CounterEpoch: 1},
						{SiteID: f.sites[1].ID, Host: f.sites[1].PublicHost, BytesIn: 6, BytesOut: 60, CumulativeBytesIn: 10, CumulativeBytesOut: 100, RequestCount: 3, CounterEpoch: 1},
					}
				}
			} else if minute == 0 {
				stats = []NodeSiteStat{{SiteID: f.sites[0].ID, Host: f.sites[0].PublicHost,
					BytesIn: 40, BytesOut: 200, CumulativeBytesIn: 40, CumulativeBytesOut: 200, RequestCount: 4, CounterEpoch: 1}}
			} else {
				stats = []NodeSiteStat{{SiteID: f.sites[0].ID, Host: f.sites[0].PublicHost,
					BytesIn: 5, BytesOut: 25, CumulativeBytesIn: 45, CumulativeBytesOut: 225, RequestCount: 5, CounterEpoch: 1}}
			}
			report := NodeReport{BootID: fmt.Sprintf("ledger-kernel-%d", nodeIndex), Sequence: int64(minute + 1),
				InterfaceName: "synthetic0", RXBytes: int64(10 * minute), TXBytes: int64(10 * minute), AgentVersion: appVersion, SiteStats: stats}
			result, err := app.db.RecordNodeReportResult(token, report, f.base.Add(time.Duration(minute)*time.Minute))
			if err != nil || len(result.AcceptedSiteIDs) != len(stats) {
				t.Fatalf("node %d minute %d accepted=%v err=%v", nodeIndex, minute, result.AcceptedSiteIDs, err)
			}
			// An acknowledged retry must not bill the same minute again before
			// or after it is transferred to the controller history.
			if _, err := app.db.RecordNodeReportResult(token, report, f.base.Add(time.Duration(minute)*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
	}
	var err error
	f.adminID, err = app.db.CreateInitialUser("ledger-admin", "synthetic-ledger-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f deletedNodeTrafficFixture) delete(t *testing.T, nodeID int64) int {
	t.Helper()
	token, err := generateTokenWithVersion(f.adminID, "ledger-admin", 1)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("https://panel.example.test/api/nodes/%d", nodeID), nil)
	request.Header.Set("Origin", "https://panel.example.test")
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	f.app.authMiddleware(f.app.handleNodeByID)(response, request)
	return response.Code
}

func (f deletedNodeTrafficFixture) assertTraffic(t *testing.T) {
	t.Helper()
	rows, err := f.app.db.GetTrafficTrendLogsGrouped(nil, f.base.Add(-time.Minute), f.base.Add(3*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	type key struct{ siteID, ms int64 }
	type value struct{ in, out, requests int64 }
	want := map[key]value{
		{f.sites[0].ID, f.base.UnixMilli()}:                  {53, 307, 7},
		{f.sites[0].ID, f.base.Add(time.Minute).UnixMilli()}: {30, 86, 6},
		{f.sites[1].ID, f.base.UnixMilli()}:                  {5, 42, 2},
		{f.sites[1].ID, f.base.Add(time.Minute).UnixMilli()}: {6, 60, 2},
	}
	if len(rows) != len(want) {
		t.Errorf("minute rows=%d, want %d", len(rows), len(want))
	}
	for _, row := range rows {
		k := key{row.SiteID, row.RecordedAtMS}
		expected, ok := want[k]
		if !ok || (value{row.BytesIn, row.BytesOut, row.Requests}) != expected {
			t.Errorf("site=%d ms=%d traffic=%d/%d requests=%d; want %v present=%t",
				row.SiteID, row.RecordedAtMS, row.BytesIn, row.BytesOut, row.Requests, expected, ok)
		}
	}
	for index, expected := range []struct{ in, out, outbound, bidirectional int64 }{{83, 393, 393, 952}, {11, 102, 102, 226}} {
		site, err := f.app.db.GetSite(f.sites[index].ID)
		if err != nil {
			t.Fatal(err)
		}
		if site.TrafficUsedIn != expected.in || site.TrafficUsedOut != expected.out || site.TrafficUsed != expected.in+expected.out {
			t.Errorf("lifetime changed during history transfer: site=%d total=%d in/out=%d/%d", site.ID, site.TrafficUsed, site.TrafficUsedIn, site.TrafficUsedOut)
		}
		wantBillable := expected.outbound
		if f.mode == trafficBillingModeBidirectional {
			wantBillable = expected.bidirectional
		}
		start := f.base.Add(-time.Minute)
		if f.resetDay == 0 {
			start = time.Time{}
		}
		used, err := f.app.db.SumTrafficSinceForSite(site.ID, start, f.mode)
		if err != nil || used != wantBillable {
			t.Errorf("cycle usage=%d, want %d err=%v", used, wantBillable, err)
		}
		// This also executes after the connection-local TEMP traffic triggers
		// were installed by the first pre-deletion cached aggregate.
		bySite, err := f.app.db.SumTrafficSinceBySite(start, f.mode)
		if err != nil || bySite[site.ID] != wantBillable {
			t.Errorf("cached cycle usage=%d, want %d err=%v", bySite[site.ID], wantBillable, err)
		}
	}
	if err := quotaUsageDecision(f.app.pm, &ProxyInstance{Site: *f.sites[0]}, f.sites[0].TrafficQuota, f.base.Add(3*time.Minute)); err != errTrafficQuotaExceeded {
		t.Errorf("deleting an accounted node rearmed a depleted site quota: %v", err)
	}
}

func TestDeletingControlNodePreservesBilledSiteMinutes(t *testing.T) {
	for _, mode := range []string{trafficBillingModeOutbound, trafficBillingModeBidirectional} {
		for _, resetDay := range []int{1, 0} {
			t.Run(fmt.Sprintf("%s reset %d", mode, resetDay), func(t *testing.T) {
				f := newDeletedNodeTrafficFixture(t, mode, resetDay)
				f.assertTraffic(t)
				for _, node := range f.nodes {
					if status := f.delete(t, node.ID); status != http.StatusOK {
						t.Fatalf("authenticated deletion request status=%d", status)
					}
					f.assertTraffic(t)
					if err := f.app.db.ForceDeletePendingNode(node.ID); err != nil {
						t.Fatalf("force-remove pending node after verifying its traffic ledger: %v", err)
					}
					f.assertTraffic(t)
					if status := f.delete(t, node.ID); status != http.StatusNotFound {
						t.Fatalf("repeated deletion status=%d, want not found", status)
					}
					f.assertTraffic(t)
				}
			})
		}
	}
}
