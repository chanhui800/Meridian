package main

import (
	"fmt"
	"testing"
	"time"
)

func TestNodeReportsKeepQuotaUntilCalendarReset(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		year, resetDay, beforeDay, edgeDay int
		offsetMinutes                      int
		previousMonthDay                   int
	}{
		{name: "non-leap reset 31", year: 2026, resetDay: 31, beforeDay: 28, edgeDay: 29, offsetMinutes: 480, previousMonthDay: 28},
		{name: "non-leap reset 30", year: 2026, resetDay: 30, beforeDay: 28, edgeDay: 29, offsetMinutes: -300, previousMonthDay: 28},
		{name: "leap reset 31", year: 2028, resetDay: 31, beforeDay: 29, edgeDay: 30, offsetMinutes: 345, previousMonthDay: 29},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := openWatchHistoryTestDB(t)
			settings := database.currentSystemSettings()
			settings.ScheduleTimezone = tc.offsetMinutes
			if err := database.saveSystemSettings(settings); err != nil {
				t.Fatal(err)
			}
			location := timezoneLocation(tc.offsetMinutes)
			date := func(day int) time.Time { return time.Date(tc.year, time.March, day, 12, 0, 0, 0, location) }
			node, enrollment, err := database.CreateControlNode(NodeCreateInput{
				Name: "calendar node", Address: "203.0.113.23", Port: 19091,
				TrafficQuota: 1000, BillingMode: trafficBillingModeOutbound, ResetDay: tc.resetDay,
			}, date(1))
			if err != nil {
				t.Fatal(err)
			}
			_, token, err := database.EnrollControlNode(enrollment, date(1))
			if err != nil {
				t.Fatal(err)
			}
			report := NodeReport{BootID: "calendar-kernel", Sequence: 1, InterfaceName: "synthetic0", RXBytes: 10, TXBytes: 10, AgentVersion: appVersion}
			if _, err := database.RecordNodeReport(token, report, date(1)); err != nil {
				t.Fatal(err)
			}
			report.Sequence, report.RXBytes, report.TXBytes = 2, 1210, 1210
			before, err := database.RecordNodeReport(token, report, date(tc.beforeDay))
			if err != nil || before.TrafficUsed != 1200 || !before.Depleted {
				t.Fatalf("before reset: used=%d depleted=%t err=%v", before.TrafficUsed, before.Depleted, err)
			}
			report.Sequence = 3
			edge, err := database.RecordNodeReport(token, report, date(tc.edgeDay))
			if err != nil {
				t.Fatal(err)
			}
			wantPrevious := time.Date(tc.year, time.February, tc.previousMonthDay, 0, 0, 0, 0, location)
			if edge.TrafficUsed != 1200 || !edge.Depleted || edge.CycleStartedAtMS != wantPrevious.UnixMilli() {
				t.Errorf("pre-reset heartbeat: used=%d depleted=%t start=%s; want used=1200, depleted and %s",
					edge.TrafficUsed, edge.Depleted, time.UnixMilli(edge.CycleStartedAtMS).In(location), wantPrevious)
			}
			snapshot, err := database.NodeControlSnapshot(date(tc.edgeDay))
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Scheduler.ActiveNodeID == node.ID {
				t.Error("scheduler selected a node whose current billing cycle is still depleted")
			}
			report.Sequence, report.RXBytes, report.TXBytes = 4, 1260, 1260
			wantReset := time.Date(tc.year, time.March, tc.resetDay, 0, 0, 0, 0, location)
			if _, err := database.RecordNodeReport(token, report, wantReset.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			report.Sequence = 5
			reset, err := database.RecordNodeReport(token, report, date(tc.resetDay))
			if err != nil {
				t.Fatal(err)
			}
			if reset.TrafficUsed != 0 || reset.Depleted || reset.CycleStartedAtMS != wantReset.UnixMilli() {
				t.Errorf("actual reset: used=%d depleted=%t start=%s; want zero and %s",
					reset.TrafficUsed, reset.Depleted, time.UnixMilli(reset.CycleStartedAtMS).In(location), wantReset)
			}
		})
	}
}

func TestControlNodeInitialCycleUsesLastDayOfShortMonth(t *testing.T) {
	location := timezoneLocation(480)
	for _, tc := range []struct{ year, month, day, resetDay, wantMonth, wantDay int }{
		{2026, 3, 29, 30, 2, 28}, {2026, 3, 30, 31, 2, 28}, {2028, 3, 30, 31, 2, 29},
		{2026, 3, 31, 31, 3, 31}, {2026, 4, 30, 31, 4, 30},
	} {
		t.Run(fmt.Sprintf("%d-%02d-%02d reset %d", tc.year, tc.month, tc.day, tc.resetDay), func(t *testing.T) {
			database := openWatchHistoryTestDB(t)
			node, _, err := database.CreateControlNode(NodeCreateInput{Name: "initial calendar node", ResetDay: tc.resetDay},
				time.Date(tc.year, time.Month(tc.month), tc.day, 12, 0, 0, 0, location))
			if err != nil {
				t.Fatal(err)
			}
			want := time.Date(tc.year, time.Month(tc.wantMonth), tc.wantDay, 0, 0, 0, 0, location)
			if node.CycleStartedAtMS != want.UnixMilli() {
				t.Fatalf("initial cycle=%s, want %s", time.UnixMilli(node.CycleStartedAtMS).In(location), want)
			}
		})
	}
}
