package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type nodeTrafficHTTPFixture struct {
	telemetryFixture
	lease string
}

func newNodeTrafficHTTPFixture(t *testing.T) nodeTrafficHTTPFixture {
	t.Helper()
	fixture := newTelemetryFixture(t)
	tx, err := fixture.app.db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	lease, err := claimAgentLeaseTx(tx, fixture.node.ID, "telemetry-session", 1, fixture.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return nodeTrafficHTTPFixture{telemetryFixture: fixture, lease: lease}
}

func (fixture nodeTrafficHTTPFixture) report(frame ledgerFrame, cacheSize int64) NodeReport {
	report := ledgerFrameReport(fixture.telemetryFixture, frame)
	report.AgentVersion = "v1.11.4"
	report.SessionEpoch, report.AgentLeaseID = 1, fixture.lease
	report.SiteStats[0].CacheSizeValid, report.SiteStats[0].CacheSizeBytes = true, cacheSize
	return report
}

func (fixture nodeTrafficHTTPFixture) post(t *testing.T, report NodeReport) {
	t.Helper()
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "https://controller.example/api/agent/report", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+fixture.token)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	fixture.app.handleAgentReport(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("Agent report sequence %d returned HTTP %d: %s", report.Sequence, response.Code, response.Body.String())
	}
	var ack struct {
		Accepted        bool    `json:"accepted"`
		AcceptedSiteIDs []int64 `json:"accepted_site_ids"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &ack); err != nil {
		t.Fatal(err)
	}
	if !ack.Accepted || (len(report.SiteStats) > 0 && (len(ack.AcceptedSiteIDs) != 1 || ack.AcceptedSiteIDs[0] != fixture.site.ID)) {
		t.Fatalf("authorized Agent report must be acknowledged: %+v", ack)
	}
}

func (fixture nodeTrafficHTTPFixture) assertCurrent(t *testing.T, report NodeReport) {
	t.Helper()
	want := report.SiteStats[0]
	snapshot, err := fixture.app.db.NodeSiteLiveTrafficSnapshot(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := snapshot[fixture.site.ID]
	if got.CumulativeBytesIn != want.CumulativeBytesIn || got.CumulativeBytesOut != want.CumulativeBytesOut || got.Requests != want.RequestCount || got.CacheSizeBytes != want.CacheSizeBytes {
		t.Fatalf("newer period must own the visible traffic/cache counters: got %+v, want %d/%d/%d cache %d", got, want.CumulativeBytesIn, want.CumulativeBytesOut, want.RequestCount, want.CacheSizeBytes)
	}
	schedules, err := fixture.app.db.ListSiteNodeSchedules()
	if err != nil {
		t.Fatal(err)
	}
	if len(schedules) != 1 || schedules[0].AgentRequestCount != want.RequestCount || schedules[0].AgentLastRequestAtMS != want.LastRequestAtMS || schedules[0].AgentLastStatus != want.LastStatus {
		t.Fatalf("newer period must refresh current schedule metadata: %+v", schedules)
	}
}

func (fixture nodeTrafficHTTPFixture) restart(t *testing.T) {
	t.Helper()
	path := fixture.app.db.dbPath
	fixture.app.db.Close()
	reopened, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	fixture.app.db, fixture.app.pm.database = reopened, reopened
}

func (fixture nodeTrafficHTTPFixture) strandCurrent(t *testing.T, old NodeReport, unknownSequence bool) {
	t.Helper()
	stat := old.SiteStats[0]
	currentEpoch := old.ReportSessionID + ":" + old.SiteCounterEpoch
	if stat.CounterEpoch > 0 {
		currentEpoch += fmt.Sprintf(":%d", stat.CounterEpoch)
	}
	sequence := old.Sequence
	if unknownSequence {
		sequence = 0
	}
	// Recreate v1.11.4's persisted stranded current row, retaining the accepted
	// newer period and its already-billed history across Controller restart.
	if _, err := fixture.app.db.db.Exec(`UPDATE node_site_counters SET
		boot_id=?,last_bytes_in=?,last_bytes_out=?,last_request_count=?,cache_size_bytes=?,
		report_sequence=?,updated_at_ms=? WHERE node_id=? AND site_id=?`,
		currentEpoch, stat.CumulativeBytesIn, stat.CumulativeBytesOut, stat.RequestCount, stat.CacheSizeBytes,
		sequence, fixture.now.UnixMilli(), fixture.node.ID, fixture.site.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.db.db.Exec(`UPDATE site_node_schedules SET
		agent_request_count=?,agent_last_request_at_ms=?,agent_last_status=? WHERE site_id=?`,
		stat.RequestCount, stat.LastRequestAtMS, stat.LastStatus, fixture.site.ID); err != nil {
		t.Fatal(err)
	}
	fixture.restart(t)
}

func TestNodeTrafficLedgerNewerPeriodBecomesCurrentHTTP(t *testing.T) {
	for _, scenario := range []struct {
		name                   string
		alreadyAccounted       bool
		unknownCurrentSequence bool
		currentSiteEpoch       uint64
		newSiteEpoch           uint64
		newRuntimeEpoch        string
	}{
		{"delayed_first_delivery", false, false, 1, 2, "1"},
		{"accounted_before_controller_restart", true, false, 1, 2, "1"},
		{"migrated_unknown_sequence", true, true, 1, 2, "1"},
		{"new_runtime_with_lower_site_epoch", true, false, 2, 1, "2"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newNodeTrafficHTTPFixture(t)
			old := fixture.report(ledgerFrame{1, 10, 20, 10, 20, 1, scenario.currentSiteEpoch, false}, 100)
			fixture.post(t, old)
			// A later page can advance the node's sequence without this site.
			otherPage := fixture.report(ledgerFrame{10, 0, 0, 0, 0, 0, scenario.currentSiteEpoch, false}, 0)
			otherPage.SiteStats = nil
			fixture.post(t, otherPage)
			late := fixture.report(ledgerFrame{2, 30, 50, 30, 50, 2, scenario.newSiteEpoch, false}, 200)
			late.SiteCounterEpoch = scenario.newRuntimeEpoch
			if scenario.alreadyAccounted {
				if _, err := fixture.app.db.RecordNodeReportResult(fixture.token, late, fixture.now.Add(3*time.Second)); err != nil {
					t.Fatal(err)
				}
				fixture.strandCurrent(t, old, scenario.unknownCurrentSequence)
			}
			// An exact retry must also repair already-accounted newer periods.
			fixture.post(t, late)
			fixture.assertCurrent(t, late)
			next := fixture.report(ledgerFrame{11, 10, 20, 40, 70, 3, scenario.newSiteEpoch, false}, 300)
			next.SiteCounterEpoch = scenario.newRuntimeEpoch
			fixture.post(t, next)
			fixture.assertCurrent(t, next)
			fixture.restart(t)
			fixture.assertCurrent(t, next)
			for mode, want := range map[string]int64{"bidirectional": 280, "outbound": 90} {
				got, err := fixture.app.db.SumTrafficSinceForSite(fixture.site.ID, fixture.now.Add(-time.Minute), mode)
				if err != nil || got != want {
					t.Fatalf("%s billing must count each accepted period exactly once: got %d want %d err %v", mode, got, want, err)
				}
			}
			site, err := fixture.app.db.GetSite(fixture.site.ID)
			if err != nil {
				t.Fatal(err)
			}
			if site.TrafficUsedIn != 50 || site.TrafficUsedOut != 90 || site.TrafficUsed != 140 {
				t.Fatalf("lifetime directional usage must count each period once: got %d/%d total %d", site.TrafficUsedIn, site.TrafficUsedOut, site.TrafficUsed)
			}
		})
	}
}

func TestNodeTrafficLedgerStrandedPeriodIgnoresObsoleteRetryHTTP(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		frame ledgerFrame
	}{
		{"older_sequence_with_same_counters", ledgerFrame{2, 30, 50, 30, 50, 2, 2, false}},
		{"same_sequence_with_different_counters", ledgerFrame{4, 40, 60, 40, 60, 3, 2, false}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newNodeTrafficHTTPFixture(t)
			old := fixture.report(ledgerFrame{1, 10, 20, 10, 20, 1, 1, false}, 100)
			fixture.post(t, old)
			otherPage := fixture.report(ledgerFrame{10, 0, 0, 0, 0, 0, 1, false}, 0)
			otherPage.SiteStats = nil
			fixture.post(t, otherPage)
			latest := fixture.report(ledgerFrame{4, 30, 50, 30, 50, 2, 2, false}, 200)
			if _, err := fixture.app.db.RecordNodeReportResult(fixture.token, latest, fixture.now.Add(3*time.Second)); err != nil {
				t.Fatal(err)
			}
			fixture.strandCurrent(t, old, false)
			obsolete := fixture.report(scenario.frame, 150)
			obsolete.SiteStats[0].LastStatus = 503
			fixture.post(t, obsolete)
			fixture.assertCurrent(t, old)
			fixture.post(t, latest)
			fixture.assertCurrent(t, latest)
			got, err := fixture.app.db.SumTrafficSinceForSite(fixture.site.ID, fixture.now.Add(-time.Minute), "outbound")
			if err != nil || got != 70 {
				t.Fatalf("covered retry must not add unaccounted traffic: got %d want 70 err %v", got, err)
			}
		})
	}
}
