package main

import (
	"errors"
	"testing"
	"time"
)

type ledgerFrame struct {
	sequence, bytesIn, bytesOut, totalIn, totalOut, requests int64
	epoch                                                    uint64
	final                                                    bool
}

func ledgerFrameReport(fixture telemetryFixture, frame ledgerFrame) NodeReport {
	return NodeReport{
		BootID: "telemetry-boot", ReportSessionID: "telemetry-session",
		CounterEpoch: "kernel:eth0", SiteCounterEpoch: "1", InterfaceName: "eth0",
		Sequence: frame.sequence, TelemetrySequence: frame.sequence,
		SampledAtMS: fixture.now.Add(time.Duration(frame.sequence) * time.Second).UnixMilli(),
		SiteStats: []NodeSiteStat{{
			SiteID: fixture.site.ID, Host: fixture.site.PublicHost,
			CounterEpoch: frame.epoch, Final: frame.final,
			BytesIn: frame.bytesIn, BytesOut: frame.bytesOut,
			CumulativeBytesIn: frame.totalIn, CumulativeBytesOut: frame.totalOut,
			RequestCount: frame.requests, LastRequestAtMS: fixture.now.Add(time.Duration(frame.sequence) * time.Second).UnixMilli(),
			LastStatus: 206,
		}},
	}
}

func recordLedgerFrame(t *testing.T, fixture telemetryFixture, frame ledgerFrame, received time.Duration) NodeReportResult {
	t.Helper()
	result, err := fixture.app.db.RecordNodeReportResult(fixture.token, ledgerFrameReport(fixture, frame), fixture.now.Add(received))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.AcceptedSiteIDs) != 1 || result.AcceptedSiteIDs[0] != fixture.site.ID {
		t.Fatalf("authorized counter report must be acknowledged: %#v", result)
	}
	return result
}

func assertLedgerTotals(t *testing.T, fixture telemetryFixture, wantIn, wantOut, wantRequests int64) {
	t.Helper()
	var bytesIn, bytesOut, requests int64
	if err := fixture.app.db.db.QueryRow(
		"SELECT COALESCE(SUM(bytes_in),0),COALESCE(SUM(bytes_out),0),COALESCE(SUM(requests),0) FROM node_site_traffic_logs WHERE node_id=? AND site_id=?",
		fixture.node.ID, fixture.site.ID,
	).Scan(&bytesIn, &bytesOut, &requests); err != nil {
		t.Fatal(err)
	}
	t.Logf("durable traffic=%d/%d/%d, want=%d/%d/%d", bytesIn, bytesOut, requests, wantIn, wantOut, wantRequests)
	if bytesIn != wantIn || bytesOut != wantOut || requests != wantRequests {
		t.Fatalf("durable traffic=%d/%d/%d, want=%d/%d/%d", bytesIn, bytesOut, requests, wantIn, wantOut, wantRequests)
	}
}

func TestNodeTrafficLedgerRetiredEpochReplayDoesNotCountAgain(t *testing.T) {
	fixture := newTelemetryFixture(t)
	frames := []ledgerFrame{
		{1, 10, 20, 10, 20, 1, 1, false},
		{2, 4, 6, 14, 26, 2, 1, true},
		{3, 30, 50, 30, 50, 2, 2, false},
		{2, 4, 6, 14, 26, 2, 1, true},
		{4, 10, 20, 40, 70, 3, 2, false},
	}
	for index, frame := range frames {
		recordLedgerFrame(t, fixture, frame, time.Duration(index+1)*time.Second)
	}
	assertLedgerTotals(t, fixture, 54, 96, 5)
}

func TestNodeTrafficLedgerRetiredDrainCountsOnlyItsUnaccountedDelta(t *testing.T) {
	fixture := newTelemetryFixture(t)
	frames := []ledgerFrame{
		{1, 10, 20, 10, 20, 1, 1, false},
		{2, 4, 6, 14, 26, 2, 1, false},
		{3, 30, 50, 30, 50, 2, 2, false},
		// A draining generation can be included in a newly captured report.
		{4, 4, 7, 18, 33, 3, 1, true},
		{4, 4, 7, 18, 33, 3, 1, true},
		{5, 10, 20, 40, 70, 3, 2, false},
	}
	for index, frame := range frames {
		recordLedgerFrame(t, fixture, frame, time.Duration(index+1)*time.Second)
		if index == 3 {
			live := fixture.snapshot(t, time.Duration(index+1)*time.Second).LiveSites[0]
			if live.CumulativeBytesIn != 30 || live.CumulativeBytesOut != 50 || live.Requests != 2 {
				t.Fatalf("retired drain replaced the current visible counters: %#v", live)
			}
			schedule, err := fixture.app.db.siteNodeSchedule(fixture.site.ID)
			if err != nil {
				t.Fatal(err)
			}
			if schedule.AgentRequestCount != 2 {
				t.Fatalf("retired drain replaced current schedule count: %d", schedule.AgentRequestCount)
			}
		}
	}
	assertLedgerTotals(t, fixture, 58, 103, 6)
}

func TestNodeTrafficLedgerColdOverlayIgnoresRetiredEpochReplay(t *testing.T) {
	fixture := newTelemetryFixture(t)
	for index, frame := range []ledgerFrame{
		{1, 10, 20, 10, 20, 1, 1, false},
		{2, 4, 6, 14, 26, 2, 1, true},
		{3, 30, 50, 30, 50, 2, 2, false},
	} {
		recordLedgerFrame(t, fixture, frame, time.Duration(index+1)*time.Second)
	}
	fixture.app.db.agentLiveMu.Lock()
	fixture.app.db.agentLive = nil
	fixture.app.db.agentLiveMu.Unlock()
	recordLedgerFrame(t, fixture, ledgerFrame{2, 4, 6, 14, 26, 2, 1, true}, 4*time.Second)
	live := fixture.snapshot(t, 4*time.Second).LiveSites[0]
	if live.CumulativeBytesIn != 30 || live.CumulativeBytesOut != 50 || live.Requests != 2 {
		t.Fatalf("retired replay seeded an old cold overlay: %#v", live)
	}
	assertLedgerTotals(t, fixture, 44, 76, 4)
}

func TestNodeTrafficLedgerMigrationPreservesAlreadyAccountedPeriod(t *testing.T) {
	fixture := newTelemetryFixture(t)
	first := ledgerFrame{1, 10, 20, 10, 20, 1, 1, false}
	recordLedgerFrame(t, fixture, first, time.Second)
	// Recreate the pre-51 counter shape around an already accepted report.
	// Migration must preserve its watermark, not reclassify it as new traffic.
	for _, statement := range []string{
		"DROP TABLE node_site_counter_epochs",
		"ALTER TABLE node_site_counters DROP COLUMN report_session_id",
		"ALTER TABLE node_site_counters DROP COLUMN report_sequence",
		"PRAGMA user_version=50",
	} {
		if _, err := fixture.app.db.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.app.db.migrate(); err != nil {
		t.Fatal(err)
	}
	recordLedgerFrame(t, fixture, first, 2*time.Second)
	recordLedgerFrame(t, fixture, ledgerFrame{2, 30, 50, 30, 50, 2, 2, false}, 3*time.Second)
	if err := fixture.app.db.migrate(); err != nil {
		t.Fatal(err)
	}
	recordLedgerFrame(t, fixture, first, 4*time.Second)
	recordLedgerFrame(t, fixture, ledgerFrame{3, 10, 20, 40, 70, 3, 2, false}, 5*time.Second)
	assertLedgerTotals(t, fixture, 50, 90, 4)
}

func TestNodeTrafficLedgerFailureAndRetryCommitExactlyOnce(t *testing.T) {
	fixture := newTelemetryFixture(t)
	first := ledgerFrame{1, 10, 20, 10, 20, 1, 1, false}
	recordLedgerFrame(t, fixture, first, time.Second)
	if _, err := fixture.app.db.db.Exec(`CREATE TRIGGER reject_ledger_test_update BEFORE UPDATE OF traffic_used ON sites
		BEGIN SELECT RAISE(ABORT,'synthetic ledger failure'); END`); err != nil {
		t.Fatal(err)
	}
	next := ledgerFrame{2, 30, 50, 30, 50, 2, 2, false}
	if _, err := fixture.app.db.RecordNodeReportResult(fixture.token, ledgerFrameReport(fixture, next), fixture.now.Add(2*time.Second)); err == nil {
		t.Fatal("injected accounting failure must fail the report transaction")
	}
	assertLedgerTotals(t, fixture, 10, 20, 1)
	if _, err := fixture.app.db.db.Exec("DROP TRIGGER reject_ledger_test_update"); err != nil {
		t.Fatal(err)
	}
	recordLedgerFrame(t, fixture, next, 3*time.Second)
	recordLedgerFrame(t, fixture, next, 4*time.Second)
	recordLedgerFrame(t, fixture, first, 5*time.Second)
	assertLedgerTotals(t, fixture, 40, 70, 3)
}

func TestNodeTrafficLedgerBillingCycleAndHistoryPruningKeepWatermarks(t *testing.T) {
	fixture := newTelemetryFixture(t)
	first := ledgerFrame{1, 10, 20, 10, 20, 1, 1, false}
	second := ledgerFrame{2, 30, 50, 30, 50, 2, 2, false}
	recordLedgerFrame(t, fixture, first, time.Second)
	recordLedgerFrame(t, fixture, second, 2*time.Second)
	if _, err := fixture.app.db.db.Exec("UPDATE control_nodes SET cycle_started_at_ms=0,reset_day=1 WHERE id=?", fixture.node.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.db.resetDueNodeCycles(fixture.now.Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// Clearing report history does not erase the anti-replay baseline.
	if _, err := fixture.app.db.db.Exec("DELETE FROM node_site_traffic_logs WHERE node_id=?", fixture.node.ID); err != nil {
		t.Fatal(err)
	}
	recordLedgerFrame(t, fixture, first, 4*time.Second)
	recordLedgerFrame(t, fixture, second, 5*time.Second)
	assertLedgerTotals(t, fixture, 0, 0, 0)
	recordLedgerFrame(t, fixture, ledgerFrame{3, 10, 20, 40, 70, 3, 2, false}, 6*time.Second)
	assertLedgerTotals(t, fixture, 10, 20, 1)
}

func TestNodeTrafficLedgerDeletedOwnersLeaveNoWatermarks(t *testing.T) {
	for _, owner := range []string{"site", "node"} {
		t.Run(owner, func(t *testing.T) {
			fixture := newTelemetryFixture(t)
			recordLedgerFrame(t, fixture, ledgerFrame{1, 10, 20, 10, 20, 1, 1, false}, time.Second)
			recordLedgerFrame(t, fixture, ledgerFrame{2, 30, 50, 30, 50, 2, 2, false}, 2*time.Second)
			var err error
			if owner == "site" {
				err = fixture.app.db.DeleteSite(fixture.site.ID)
			} else {
				err = fixture.app.db.DeleteControlNode(fixture.node.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			var rows int
			if err := fixture.app.db.db.QueryRow("SELECT COUNT(*) FROM node_site_counter_epochs WHERE node_id=? AND site_id=?", fixture.node.ID, fixture.site.ID).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 0 {
				t.Fatalf("deleted %s left %d historical watermarks", owner, rows)
			}
		})
	}
}

func TestNodeTrafficLedgerNewLeasePrunesOnlyUnreachableSessions(t *testing.T) {
	fixture := newTelemetryFixture(t)
	first := ledgerFrame{1, 10, 20, 10, 20, 1, 1, false}
	recordLedgerFrame(t, fixture, first, time.Second)
	tx, err := fixture.app.db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	lease, err := claimAgentLeaseTx(tx, fixture.node.ID, "new-ledger-session", 2, fixture.now.Add(2*time.Second))
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	report := ledgerFrameReport(fixture, ledgerFrame{1, 30, 50, 30, 50, 2, 1, false})
	report.ReportSessionID = "new-ledger-session"
	report.SessionEpoch, report.AgentLeaseID = 2, lease
	if _, err := fixture.app.db.RecordNodeReportResult(fixture.token, report, fixture.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	var staleRows, currentRows int
	if err := fixture.app.db.db.QueryRow("SELECT COUNT(*) FROM node_site_counter_epochs WHERE node_id=? AND report_session_id<>?", fixture.node.ID, report.ReportSessionID).Scan(&staleRows); err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.db.db.QueryRow("SELECT COUNT(*) FROM node_site_counter_epochs WHERE node_id=? AND report_session_id=?", fixture.node.ID, report.ReportSessionID).Scan(&currentRows); err != nil {
		t.Fatal(err)
	}
	if staleRows != 0 || currentRows != 1 {
		t.Fatalf("session pruning removed reachable data or retained stale data: stale=%d current=%d", staleRows, currentRows)
	}
	if _, err := fixture.app.db.RecordNodeReportResult(fixture.token, ledgerFrameReport(fixture, first), fixture.now.Add(4*time.Second)); !errors.Is(err, errStaleAgentSession) {
		t.Fatalf("old session became eligible after watermark pruning: %v", err)
	}
	assertLedgerTotals(t, fixture, 40, 70, 3)
}

func TestNodeTrafficLedgerControllerRestartPreservesRetiredWatermarks(t *testing.T) {
	fixture := newTelemetryFixture(t)
	first := ledgerFrame{1, 10, 20, 10, 20, 1, 1, false}
	second := ledgerFrame{2, 30, 50, 30, 50, 2, 2, false}
	recordLedgerFrame(t, fixture, first, time.Second)
	recordLedgerFrame(t, fixture, second, 2*time.Second)
	path := fixture.app.db.dbPath
	fixture.app.db.Close()
	reopened, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	fixture.app.db = reopened
	fixture.app.pm.database = reopened
	recordLedgerFrame(t, fixture, first, 3*time.Second)
	live := fixture.snapshot(t, 3*time.Second).LiveSites[0]
	if live.CumulativeBytesIn != 30 || live.CumulativeBytesOut != 50 || live.Requests != 2 {
		t.Fatalf("Controller restart allowed a retired report to replace current counters: %#v", live)
	}
	recordLedgerFrame(t, fixture, ledgerFrame{3, 10, 20, 40, 70, 3, 2, false}, 4*time.Second)
	assertLedgerTotals(t, fixture, 50, 90, 4)
}

func TestNodeTrafficLedgerFirstLateGlobalEpochDoesNotBecomeCurrent(t *testing.T) {
	fixture := newTelemetryFixture(t)
	current := ledgerFrameReport(fixture, ledgerFrame{1, 30, 50, 30, 50, 2, 1, false})
	current.SiteCounterEpoch = "2"
	if _, err := fixture.app.db.RecordNodeReportResult(fixture.token, current, fixture.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	fixture.app.db.agentLiveMu.Lock()
	fixture.app.db.agentLive = nil
	fixture.app.db.agentLiveMu.Unlock()
	// The first delivery from a draining legacy global runtime period can
	// have a newer report sequence while naming an older global generation.
	draining := ledgerFrameReport(fixture, ledgerFrame{2, 10, 20, 10, 20, 1, 1, true})
	draining.SiteCounterEpoch = "1"
	if _, err := fixture.app.db.RecordNodeReportResult(fixture.token, draining, fixture.now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	live := fixture.snapshot(t, 2*time.Second).LiveSites[0]
	if live.CumulativeBytesIn != 30 || live.CumulativeBytesOut != 50 || live.Requests != 2 {
		t.Fatalf("first delivery from an older global period replaced current counters: %#v", live)
	}
	next := ledgerFrameReport(fixture, ledgerFrame{3, 10, 20, 40, 70, 3, 1, false})
	next.SiteCounterEpoch = "2"
	if _, err := fixture.app.db.RecordNodeReportResult(fixture.token, next, fixture.now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.app.db.RecordNodeReportResult(fixture.token, draining, fixture.now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertLedgerTotals(t, fixture, 50, 90, 4)
}
