package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestNodeReportPersistsAgentWatchHistoryEvent(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "watch-agent", Address: "203.0.113.44", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "watch-agent-site", PublicHost: "watch-agent.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096", WatchHistoryEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET desired_node_id=? WHERE site_id=?", node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	history := &watchHistoryEvent{SiteID: site.ID, SessionHash: strings.Repeat("b", sha256.Size*2), UpstreamItemID: "item-1", EventType: "progress", ObservedAtMS: now.UnixMilli(), PositionTicks: 10, RunTimeTicks: 100, SeasonNumber: -1, EpisodeNumber: -1}
	event := NodeRequestEvent{EventID: 1, EventUID: strings.Repeat("c", 32), SiteID: site.ID, Host: site.PublicHost, Method: http.MethodPost, Path: "/Sessions/Playing/Progress", StatusCode: http.StatusNoContent, RecordedAtMS: now.UnixMilli(), Priority: nodeEventPriorityCritical, SkipRequestLog: true, WatchHistory: history}
	if _, err := app.db.RecordNodeReportResult(token, NodeReport{BootID: "watch-session", ReportSessionID: "watch-session", CounterEpoch: "kernel:eth0", Sequence: 1, InterfaceName: "eth0", Events: []NodeRequestEvent{event}}, now); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM watch_sessions WHERE site_id=? AND session_hash=?", site.ID, history.SessionHash).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("watch session count=%d, want 1", count)
	}
}

func TestDisabledScheduleCleanupClearsStaleAssignments(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "stale-node", Address: "203.0.113.45", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "stale-site", PublicHost: "stale.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_schedules SET enabled=0,desired_node_id=?,applied_node_id=?,cf_zone_id='',cf_record_id='',cf_record_type='A',applied_address='203.0.113.45',dns_status='active' WHERE site_id=?`, node.ID, node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	schedule, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.deleteTrackedSiteDNS(context.Background(), schedule); err != nil {
		t.Fatal(err)
	}
	var enabled int
	var fixed, desired, applied sql.NullInt64
	var zone, record, recordType, address, status string
	if err := app.db.db.QueryRow(`SELECT enabled,fixed_node_id,desired_node_id,applied_node_id,cf_zone_id,cf_record_id,cf_record_type,applied_address,dns_status FROM site_node_schedules WHERE site_id=?`, site.ID).Scan(&enabled, &fixed, &desired, &applied, &zone, &record, &recordType, &address, &status); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 || fixed.Valid || desired.Valid || applied.Valid || zone != "" || record != "" || recordType != "" || address != "" || status != "disabled" {
		t.Fatalf("stale schedule was not cleared: enabled=%d fixed=%v desired=%v applied=%v zone=%q record=%q type=%q address=%q status=%q", enabled, fixed, desired, applied, zone, record, recordType, address, status)
	}
}

func TestDeleteControlNodeRemovesOnlyManagedEdgeTLS(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meridian.db")
	t.Setenv("TLS_STATE_DIR", filepath.Join(dir, "tls-state"))
	t.Setenv("EDGE_TLS_CERT_FILE", filepath.Join(dir, "external", "edge.pem"))
	t.Setenv("EDGE_TLS_KEY_FILE", filepath.Join(dir, "external", "edge.key"))
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	node, _, err := db.CreateControlNode(NodeCreateInput{Name: "tls-delete", Address: "203.0.113.10", Port: 443}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	managedRoot := filepath.Join(tlsStateDir(dbPath), "edge-nodes", node.GUID)
	if err := os.MkdirAll(managedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedRoot, "sentinel"), []byte("managed"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyRoot := filepath.Join(filepath.Dir(filepath.Join(dir, "external", "edge.pem")), "edge-nodes")
	if err := os.MkdirAll(filepath.Join(legacyRoot, "unrelated-node"), 0o700); err != nil {
		t.Fatal(err)
	}
	legacySentinel := filepath.Join(legacyRoot, "unrelated-node", "sentinel.txt")
	if err := os.WriteFile(legacySentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteControlNode(node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(managedRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed node TLS root still exists, err=%v", err)
	}
	if data, err := os.ReadFile(legacySentinel); err != nil || string(data) != "keep" {
		t.Fatalf("legacy external TLS state changed: data=%q err=%v", data, err)
	}
}

func TestUpdateControlNodePortInvalidatesRuntimeConfig(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 8, 30, 12, 30, 0, 0, time.UTC)
	node, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "runtime-node", Address: "203.0.113.152", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "runtime-site", PublicHost: "runtime.example.com", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	const appliedHash = "runtime-config-v1"
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET desired_config_hash=?,applied_config_hash=? WHERE id=?`, appliedHash, appliedHash, node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=?,config_hash=?,config_pending_since_ms=0 WHERE site_id=?`, node.ID, node.ID, appliedHash, site.ID); err != nil {
		t.Fatal(err)
	}

	updated, err := app.db.UpdateControlNode(node.ID, NodeCreateInput{
		Name: node.Name, Address: node.Address, Port: 9091, Priority: node.Priority,
		TrafficQuota: node.TrafficQuota, BillingMode: node.BillingMode, ResetDay: node.ResetDay,
		TrafficManualOffsetBytes: node.TrafficManualOffset,
	}, true, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if updated.DesiredConfigHash != "" {
		t.Fatalf("port change left desired config hash=%q", updated.DesiredConfigHash)
	}
	schedule, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.ConfigHash != "" || schedule.ConfigPendingSinceMS != now.Add(time.Second).UnixMilli() {
		t.Fatalf("port change did not invalidate schedule: %#v", schedule)
	}
}

func TestUpdateControlNodeAddressKeepsRuntimeConfig(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 8, 30, 12, 45, 0, 0, time.UTC)
	node, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "address-node", Address: "203.0.113.152", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	const appliedHash = "address-config-v1"
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET desired_config_hash=?,applied_config_hash=? WHERE id=?`, appliedHash, appliedHash, node.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := app.db.UpdateControlNode(node.ID, NodeCreateInput{
		Name: node.Name, Address: "203.0.113.153", Port: node.Port, Priority: node.Priority,
		TrafficQuota: node.TrafficQuota, BillingMode: node.BillingMode, ResetDay: node.ResetDay,
		TrafficManualOffsetBytes: node.TrafficManualOffset,
	}, true, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if updated.DesiredConfigHash != appliedHash || updated.ConfigDirty {
		t.Fatalf("address change invalidated runtime config: %#v", updated)
	}
}

func TestRecordNodeReportPersistsAndClearsApplyError(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "apply-error", Address: "203.0.113.90"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	base := NodeReport{BootID: "apply-boot", ReportSessionID: "apply-session", CounterEpoch: "kernel:eth0", Sequence: 1, InterfaceName: "eth0", AgentVersion: "test"}
	if _, err := app.db.RecordNodeReport(token, base, now); err != nil {
		t.Fatal(err)
	}
	failure := base
	failure.Sequence = 2
	failure.ApplyError = "listen tcp :9090: bind: address already in use"
	reported, err := app.db.RecordNodeReport(token, failure, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if reported.AgentApplyError != failure.ApplyError || reported.AgentApplyFailures != 1 || reported.AgentApplyErrorAtMS != now.Add(time.Second).UnixMilli() {
		t.Fatalf("apply failure was not persisted: %#v", reported)
	}
	failure.Sequence = 3
	reported, err = app.db.RecordNodeReport(token, failure, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if reported.AgentApplyFailures != 2 {
		t.Fatalf("repeated apply failure count=%d, want 2", reported.AgentApplyFailures)
	}
	if _, err := app.db.db.Exec("UPDATE control_nodes SET desired_config_hash=? WHERE id=?", "config-ok", node.ID); err != nil {
		t.Fatal(err)
	}
	success := base
	success.Sequence = 4
	success.AppliedConfigHash = "config-ok"
	reported, err = app.db.RecordNodeReport(token, success, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if reported.AgentApplyError != "" || reported.AgentApplyFailures != 0 || reported.AgentApplyErrorAtMS != 0 {
		t.Fatalf("successful apply did not clear diagnostics: %#v", reported)
	}
}

func TestRefreshNodeEnrollmentKeepsOldAgentUntilReplacement(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "reenroll", Address: "203.0.113.60"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNode(enrollment, now); err != nil {
		t.Fatal(err)
	}
	_, oldToken, err := app.db.EnrollControlNode(enrollment, now.Add(time.Second))
	if !errors.Is(err, errInvalidNodeToken) || oldToken != "" {
		t.Fatalf("reused initial enrollment token: token=%q err=%v", oldToken, err)
	}
	// Enroll once through a fresh node so we have a valid long-lived token.
	_, enrollment, err = app.db.RefreshNodeEnrollment(node.ID, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	_, oldToken, err = app.db.EnrollControlNode(enrollment, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	_, pending, err := app.db.RefreshNodeEnrollment(node.ID, now.Add(4*time.Second))
	if err != nil || pending == "" {
		t.Fatalf("refresh enrollment: token=%q err=%v", pending, err)
	}
	if _, err := app.db.RecordNodeReport(oldToken, NodeReport{BootID: "old", Sequence: 1, InterfaceName: "eth0", AgentVersion: "test"}, now.Add(5*time.Second)); err != nil {
		t.Fatalf("old Agent was revoked before replacement: %v", err)
	}
	if _, err := app.db.RecordNodeReport(oldToken, NodeReport{BootID: "old-24h", Sequence: 2, InterfaceName: "eth0", AgentVersion: "test"}, now.Add(25*time.Hour)); err != nil {
		t.Fatalf("old Agent was revoked after pending enrollment expiry: %v", err)
	}
	if _, _, err := app.db.EnrollControlNode(pending, now.Add(25*time.Hour+time.Second)); !errors.Is(err, errInvalidNodeToken) {
		t.Fatalf("expired pending enrollment token remained valid: %v", err)
	}
	_, newToken, err := app.db.EnrollControlNode(pending, now.Add(6*time.Second))
	if err != nil || newToken == "" {
		t.Fatalf("replacement enrollment: token=%q err=%v", newToken, err)
	}
	if _, err := app.db.RecordNodeReport(oldToken, NodeReport{BootID: "old-2", Sequence: 2, InterfaceName: "eth0", AgentVersion: "test"}, now.Add(7*time.Second)); !errors.Is(err, errInvalidAgentToken) {
		t.Fatalf("old Agent token remained valid after replacement: %v", err)
	}
}

func TestNodeCredentialsRejectEphemeralJWTSecret(t *testing.T) {
	app := newTestApp(t)
	previousSecret, previousEphemeral := jwtSecret, jwtSecretEphemeral
	t.Cleanup(func() { jwtSecret, jwtSecretEphemeral = previousSecret, previousEphemeral })
	jwtSecret = nil
	jwtSecretEphemeral = true
	if _, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "ephemeral", Address: "203.0.113.70"}, time.Now()); err == nil {
		t.Fatal("node creation succeeded with an ephemeral JWT secret")
	}
	// Enrollment is checked before token lookup so a pending token can never
	// create a ciphertext that will be undecryptable after restart.
	if _, _, err := app.db.EnrollControlNode("invalid", time.Now()); err == nil || !strings.Contains(err.Error(), "persistent JWT_SECRET") {
		t.Fatalf("ephemeral enrollment error = %v", err)
	}
}

func TestLegacyNodeProbeSecretDoesNotPersistWithEphemeralJWT(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "legacy-probe", Address: "203.0.113.72"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNode(enrollment, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE control_nodes SET probe_secret_ciphertext='' WHERE id=?", node.ID); err != nil {
		t.Fatal(err)
	}
	previousSecret, previousEphemeral := jwtSecret, jwtSecretEphemeral
	t.Cleanup(func() { jwtSecret, jwtSecretEphemeral = previousSecret, previousEphemeral })
	jwtSecret = nil
	jwtSecretEphemeral = true
	loaded, err := app.db.controlNodeByID(node.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dNodeProbeSecret(app.db, loaded); !errors.Is(err, errPersistentJWTRequired) {
		t.Fatalf("legacy probe secret with ephemeral JWT error = %v", err)
	}
	var ciphertext string
	if err := app.db.db.QueryRow("SELECT probe_secret_ciphertext FROM control_nodes WHERE id=?", node.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if ciphertext != "" {
		t.Fatalf("ephemeral JWT persisted probe secret ciphertext %q", ciphertext)
	}
}

func TestBrokenProbeSecretCanBeRotatedWithStableJWT(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "probe-rotate", Address: "203.0.113.71"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, agentToken, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	originalGUID := node.GUID
	if _, err := app.db.db.Exec("UPDATE control_nodes SET probe_secret_ciphertext=? WHERE id=?", "v1:broken", node.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := app.db.controlNodeByID(node.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := dNodeProbeSecret(app.db, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeNodeProbeSecret(rotated); err != nil {
		t.Fatalf("rotated probe secret is invalid: %v", err)
	}
	if loaded.GUID != originalGUID {
		t.Fatalf("node GUID changed during probe rotation: %q", loaded.GUID)
	}
	if _, err := app.db.nodeByAgentToken(agentToken, now); err != nil {
		t.Fatalf("agent token changed during probe rotation: %v", err)
	}
}

func TestAuthorizedNodeSiteAcceptsBufferedHostAfterRename(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "alias-node", Address: "203.0.113.90"}, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "alias-site", PublicHost: "old.alias.example", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET desired_node_id=?, applied_node_id=? WHERE site_id=?", node.ID, node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	updated := *site
	updated.PublicHost = "new.alias.example"
	if err := app.db.UpdateSiteRecord(updated); err != nil {
		t.Fatal(err)
	}
	tx, err := app.db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := authorizedNodeSitesTx(tx, node.ID, time.Now().UnixMilli())
	_ = tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	if !authorizedNodeSiteHost(allowed, site.ID, "new.alias.example") || !authorizedNodeSiteHost(allowed, site.ID, "old.alias.example") {
		t.Fatalf("authorized hosts after rename = %#v", allowed[site.ID])
	}
	if authorizedNodeSiteHost(allowed, site.ID, "other.alias.example") {
		t.Fatal("unrelated host was authorized")
	}
}

func TestRefreshNodeEnrollmentPreservesRuntimeStateAndTrafficBaseline(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "preserve-runtime", Address: "203.0.113.61"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, agentToken, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	first := NodeReport{
		BootID: "legacy-boot", ReportSessionID: "session-a", CounterEpoch: "kernel-boot:eth0", Sequence: 7,
		InterfaceName: "eth0", RXBytes: 10_000, TXBytes: 20_000, AgentVersion: "test",
	}
	if _, err := app.db.RecordNodeReport(agentToken, first, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	before, err := app.db.controlNodeByID(node.ID, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if before.LastSeenAtMS == 0 || before.lastBootID == "" || before.lastReportSessionID == "" || before.lastSequence != first.Sequence || before.lastRawRXBytes != first.RXBytes || before.lastRawTXBytes != first.TXBytes {
		t.Fatalf("initial runtime state was not recorded: %#v", before)
	}
	if _, _, err := app.db.RefreshNodeEnrollment(node.ID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	after, err := app.db.controlNodeByID(node.ID, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if after.LastSeenAtMS != before.LastSeenAtMS || after.lastBootID != before.lastBootID || after.lastReportSessionID != before.lastReportSessionID || after.lastSequence != before.lastSequence || after.lastRawRXBytes != before.lastRawRXBytes || after.lastRawTXBytes != before.lastRawTXBytes {
		t.Fatalf("refresh reset runtime state: before=%#v after=%#v", before, after)
	}
	second := first
	second.ReportSessionID = "session-b"
	second.Sequence = 1
	second.RXBytes = 10_500
	second.TXBytes = 20_750
	reported, err := app.db.RecordNodeReport(agentToken, second, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if reported.PeriodRXBytes != 500 || reported.PeriodTXBytes != 750 {
		t.Fatalf("traffic baseline was lost across enrollment refresh: %#v", reported)
	}
}

func TestRecordNodeReportResultRetiresInvalidEvents(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "ack", Address: "203.0.113.50"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "ack-site", PublicHost: "x.example", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET desired_node_id=? WHERE site_id=?", node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	validUID := strings.Repeat("a", 32)
	result, err := app.db.RecordNodeReportResult(token, NodeReport{
		BootID: "session", ReportSessionID: "session", CounterEpoch: "epoch", Sequence: 1,
		InterfaceName: "eth0", Events: []NodeRequestEvent{
			{EventID: 1, EventUID: validUID, SiteID: site.ID, Host: "x.example", Method: "GET", Path: "/", StatusCode: 200, RecordedAtMS: now.UnixMilli()},
			{EventID: 2, EventUID: strings.Repeat("b", 32), SiteID: site.ID, Host: "x.example", Method: "GET", Path: strings.Repeat("x", 2049), StatusCode: 200, RecordedAtMS: now.UnixMilli()},
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Node.ID != node.ID || len(result.AcceptedEventUIDs) != 1 || len(result.DiscardedEventIDs) != 1 || len(result.DiscardedEventUIDs) != 1 {
		t.Fatalf("unexpected report result: %#v", result)
	}
}

func TestRecordNodeReportRejectsUnauthorizedSiteData(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	nodeA, enrollmentA, err := app.db.CreateControlNode(NodeCreateInput{Name: "report-a", Address: "203.0.113.70"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, tokenA, err := app.db.EnrollControlNode(enrollmentA, now)
	if err != nil {
		t.Fatal(err)
	}
	nodeB, enrollmentB, err := app.db.CreateControlNode(NodeCreateInput{Name: "report-b", Address: "203.0.113.71"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNode(enrollmentB, now); err != nil {
		t.Fatal(err)
	}
	siteA, err := app.db.CreateSiteRecord(Site{Name: "report-site-a", ListenPort: 18083, PublicHost: "report-a.example.com", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	siteB, err := app.db.CreateSiteRecord(Site{Name: "report-site-b", ListenPort: 18084, PublicHost: "report-b.example.com", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18081"})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []struct {
		siteID, nodeID int64
	}{
		{siteA.ID, nodeA.ID},
		{siteB.ID, nodeB.ID},
	} {
		if _, err := app.db.SaveSiteNodeSchedule(value.siteID, true, "fixed", value.nodeID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := app.db.db.Exec("UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=? WHERE site_id=?", value.nodeID, value.nodeID, value.siteID); err != nil {
			t.Fatal(err)
		}
	}
	result, err := app.db.RecordNodeReportResult(tokenA, NodeReport{
		BootID: "report", ReportSessionID: "report", CounterEpoch: "report-epoch", Sequence: 1, InterfaceName: "eth0",
		SiteStats: []NodeSiteStat{
			{SiteID: siteB.ID, Host: siteB.PublicHost, RequestCount: 2, BytesIn: 11, CumulativeBytesIn: 11, LastRequestAtMS: now.UnixMilli(), LastStatus: 200},
			{SiteID: siteA.ID, Host: siteB.PublicHost, RequestCount: 3, LastRequestAtMS: now.UnixMilli(), LastStatus: 200},
		},
		MediaCounts:  []NodeMediaCount{{SiteID: siteB.ID, MovieCount: 99, SeriesCount: 99, EpisodeCount: 99, ObservedAtMS: now.UnixMilli()}},
		Retention:    []NodeRetentionStatus{{SiteID: siteB.ID, ExpectedStartedAtMS: now.UnixMilli() - 1000, CompletedAtMS: now.UnixMilli()}},
		Observations: []NodeDynamicObservation{{SiteID: siteB.ID, CanonicalAuthority: "https://cdn.example.com:443", Source: "hls", Decision: "denied", ReasonCode: "parse_failure", ObservedAtMS: now.UnixMilli()}},
		Events: []NodeRequestEvent{
			{EventID: 1, EventUID: strings.Repeat("a", 32), SiteID: siteB.ID, Host: siteB.PublicHost, Method: "GET", Path: "/Items", StatusCode: 200, RecordedAtMS: now.UnixMilli()},
			{EventID: 2, EventUID: strings.Repeat("b", 32), SiteID: siteA.ID, Host: siteB.PublicHost, Method: "GET", Path: "/Items", StatusCode: 200, RecordedAtMS: now.UnixMilli()},
		},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.AcceptedSiteIDs) != 0 || len(result.DiscardedSiteIDs) != 2 || len(result.DiscardedMediaSiteIDs) != 1 || len(result.DiscardedRetentionSiteIDs) != 1 || len(result.DiscardedObservationSiteIDs) != 1 || len(result.DiscardedEventIDs) != 2 {
		t.Fatalf("unexpected authorization result: %#v", result)
	}
	var count int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM node_site_counters WHERE node_id=? AND site_id=?", nodeA.ID, siteB.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("unauthorized site stats created a counter")
	}
	if err := app.db.db.QueryRow("SELECT media_movie_count FROM sites WHERE id=?", siteB.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 99 {
		t.Fatalf("unauthorized media count changed site B: %d", count)
	}
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM dynamic_observations WHERE site_id=?", siteB.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("unauthorized observation was persisted")
	}
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM node_request_events WHERE node_id=?", nodeA.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("unauthorized request event was persisted")
	}
}

func TestRecordNodeReportAcceptsDisabledSiteFinalization(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "disabled-tail", Address: "203.0.113.72"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "disabled-tail-site", PublicHost: "disabled-tail.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18082"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=? WHERE site_id=?", node.ID, node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.db.SetSiteEnabled(site.ID, false); err != nil {
		t.Fatal(err)
	}
	result, err := app.db.RecordNodeReportResult(token, NodeReport{
		BootID: "disabled", ReportSessionID: "disabled", CounterEpoch: "disabled-epoch", Sequence: 1, InterfaceName: "eth0",
		SiteStats: []NodeSiteStat{{SiteID: site.ID, Host: site.PublicHost, RequestCount: 1, BytesIn: 17, CumulativeBytesIn: 17, LastRequestAtMS: now.UnixMilli(), LastStatus: 200}},
	}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.AcceptedSiteIDs) != 1 || result.AcceptedSiteIDs[0] != site.ID {
		t.Fatalf("disabled final stat was not accepted: %#v", result)
	}
	var used int64
	if err := app.db.db.QueryRow("SELECT traffic_used FROM sites WHERE id=?", site.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 17 {
		t.Fatalf("disabled site final traffic=%d, want 17", used)
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

func TestNodeSiteTrafficExplicitEpochKeepsFirstRequestBatch(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "epoch-node", Address: "203.0.113.220"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "epoch-site", PublicHost: "epoch.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=? WHERE site_id=?", node.ID, node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	first := NodeReport{BootID: "boot", ReportSessionID: "session-a", CounterEpoch: "kernel-a", SiteCounterEpoch: "1", Sequence: 1, InterfaceName: "eth0", SiteStats: []NodeSiteStat{{SiteID: site.ID, Host: site.PublicHost, RequestCount: 3, CounterEpoch: 1, LastRequestAtMS: now.UnixMilli(), LastStatus: 200}}}
	if _, err := app.db.RecordNodeReport(token, first, now); err != nil {
		t.Fatal(err)
	}
	second := NodeReport{BootID: "boot", ReportSessionID: "session-b", CounterEpoch: "kernel-a", SiteCounterEpoch: "2", Sequence: 1, InterfaceName: "eth0", SiteStats: []NodeSiteStat{{SiteID: site.ID, Host: site.PublicHost, RequestCount: 10, CounterEpoch: 1, LastRequestAtMS: now.Add(time.Second).UnixMilli(), LastStatus: 200}}}
	if _, err := app.db.RecordNodeReport(token, second, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var requests int64
	if err := app.db.db.QueryRow("SELECT COALESCE(SUM(requests),0) FROM node_site_traffic_logs WHERE node_id=? AND site_id=?", node.ID, site.ID).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if requests != 13 {
		t.Fatalf("explicit epoch request total=%d, want 13 (3 + 10)", requests)
	}
}

func TestDisabledSiteRevocationSurvivesAppliedConfigAndFinalStat(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "final-node", Address: "203.0.113.221"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "final-site", PublicHost: "final.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=?,config_hash='applied' WHERE site_id=?", node.ID, node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.db.SetSiteEnabled(site.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE control_nodes SET desired_config_hash='applied',desired_config_revision=4,config_revision=4,applied_config_revision=0 WHERE id=?", node.ID); err != nil {
		t.Fatal(err)
	}
	report := NodeReport{BootID: "final", ReportSessionID: "final", CounterEpoch: "final", AppliedConfigHash: "applied", AppliedConfigRevision: 4, Sequence: 1, InterfaceName: "eth0", SiteStats: []NodeSiteStat{{SiteID: site.ID, Host: site.PublicHost, RequestCount: 1, CounterEpoch: 1, Final: true, LastRequestAtMS: now.UnixMilli(), LastStatus: 200}}}
	if _, err := app.db.RecordNodeReportResult(token, report, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var revocations, drains int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM agent_route_revocations WHERE node_id=? AND site_id=?", node.ID, site.ID).Scan(&revocations); err != nil {
		t.Fatal(err)
	}
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM site_node_drains WHERE node_id=? AND site_id=?", node.ID, site.ID).Scan(&drains); err != nil {
		t.Fatal(err)
	}
	if revocations != 1 {
		t.Fatalf("revocation tombstone was removed after config ACK: %d", revocations)
	}
	if drains != 0 {
		t.Fatalf("unexpected drain row for disabled site: %d", drains)
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

func TestNodeReportConfigChangedIncludesDirtyAndBlankDesiredHash(t *testing.T) {
	if !nodeReportConfigChanged(ControlNode{DesiredConfigHash: "same", ConfigDirty: true}, "same", 0) {
		t.Fatal("dirty node did not request an immediate config refresh")
	}
	if !nodeReportConfigChanged(ControlNode{}, "", 0) {
		t.Fatal("blank desired hash did not request an initial config refresh")
	}
	if nodeReportConfigChanged(ControlNode{DesiredConfigHash: "same"}, "same", 0) {
		t.Fatal("matching clean hashes incorrectly requested a refresh")
	}
}

func TestLegacyHeartbeatCannotClearNewConfigRevision(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "revision-race", Address: "203.0.113.201"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, agentToken, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET config_revision=5,desired_config_revision=5,applied_config_revision=5,
		desired_config_hash='old-hash',applied_config_hash='old-hash',config_dirty=0 WHERE id=?`, node.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := app.db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := markAgentConfigsDirtyTx(tx); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	reported, err := app.db.RecordNodeReport(agentToken, NodeReport{
		BootID: "revision-boot", ReportSessionID: "revision-session", CounterEpoch: "revision-epoch",
		Sequence: 1, InterfaceName: "eth0", AgentVersion: "test", AppliedConfigHash: "old-hash", AppliedConfigRevision: 5,
	}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !reported.ConfigDirty || reported.DesiredConfigHash != "" || reported.ConfigRevision != 6 {
		t.Fatalf("old heartbeat cleared a newer config revision: %#v", reported)
	}
}

func TestManualSchedulerRejectsUnavailableNode(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "offline-manual", Address: "203.0.113.190", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.UpdateNodeScheduler("manual", node.ID, now); !errors.Is(err, errManualNodeUnavailable) {
		t.Fatalf("manual scheduler error=%v, want unavailable-node error", err)
	}
}

func TestAgentConfigPollingDoesNotResetPendingSince(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 8, 30, 14, 0, 0, 0, time.UTC)
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "config-node", Address: "203.0.113.15", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, agentToken, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.RecordNodeReport(agentToken, NodeReport{BootID: "config-boot", Sequence: 1, InterfaceName: "eth0", AgentVersion: "test"}, now); err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "config-site", PublicHost: "config.example.com", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	// A config request with no enabled site routes still exercises the exact
	// schedule hash update used by real Agents, without requiring a certificate.
	if _, err := app.db.db.Exec("UPDATE sites SET enabled=0 WHERE id=?", site.ID); err != nil {
		t.Fatal(err)
	}
	first, err := app.buildAgentConfigForPlatform(agentToken, now.Add(time.Second), "")
	if err != nil {
		t.Fatalf("first config: %v", err)
	}
	firstSchedule, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstSchedule.ConfigPendingSinceMS == 0 || first.ConfigHash == "" {
		t.Fatalf("initial config did not start pending timer: schedule=%#v config=%#v", firstSchedule, first)
	}
	if _, err := app.db.RecordNodeReport(agentToken, NodeReport{BootID: "config-boot", Sequence: 2, InterfaceName: "eth0", AgentVersion: "test"}, now.Add(60*time.Second)); err != nil {
		t.Fatalf("keep node online for polling: %v", err)
	}
	second, err := app.buildAgentConfigForPlatform(agentToken, now.Add(60*time.Second), "")
	if err != nil {
		t.Fatalf("polled config: %v", err)
	}
	secondSchedule, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if secondSchedule.ConfigPendingSinceMS != firstSchedule.ConfigPendingSinceMS {
		t.Fatalf("config polling reset pending timer: first=%d second=%d hashes=%q/%q", firstSchedule.ConfigPendingSinceMS, secondSchedule.ConfigPendingSinceMS, first.ConfigHash, second.ConfigHash)
	}
	if _, err := app.db.RecordNodeReport(agentToken, NodeReport{BootID: "config-boot", Sequence: 3, InterfaceName: "eth0", AgentVersion: "test", AppliedConfigHash: second.ConfigHash}, now.Add(70*time.Second)); err != nil {
		t.Fatalf("applied config report: %v", err)
	}
	clearedSchedule, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if clearedSchedule.ConfigPendingSinceMS != 0 {
		t.Fatalf("applied config did not clear pending timer: %#v", clearedSchedule)
	}
}

func TestAgentColdStartMismatchStartsPendingConfigTimer(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 8, 30, 14, 15, 0, 0, time.UTC)
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "cold-start-node", Address: "203.0.113.151", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "cold-start-site", PublicHost: "cold-start.example.com", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	const desiredHash = "cold-start-config-v1"
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET desired_config_hash=?,applied_config_hash='' WHERE id=?`, desiredHash, node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_schedules SET desired_node_id=?,config_hash=?,config_pending_since_ms=0 WHERE site_id=?`, node.ID, desiredHash, site.ID); err != nil {
		t.Fatal(err)
	}

	// The Agent has restarted and has not applied the desired hash yet. The
	// first heartbeat must start the pending clock even though the hash itself
	// did not change during this process restart.
	if _, err := app.db.RecordNodeReport(token, NodeReport{BootID: "cold-boot", Sequence: 1, InterfaceName: "eth0"}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	schedule, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.ConfigPendingSinceMS != now.Add(time.Second).UnixMilli() {
		t.Fatalf("cold-start mismatch pending_since=%d, want %d", schedule.ConfigPendingSinceMS, now.Add(time.Second).UnixMilli())
	}

	if _, err := app.db.RecordNodeReport(token, NodeReport{BootID: "cold-boot", Sequence: 2, InterfaceName: "eth0", AppliedConfigHash: desiredHash}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	schedule, err = app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.ConfigPendingSinceMS != 0 {
		t.Fatalf("matching applied hash did not clear pending timer: %#v", schedule)
	}
}

func TestSiteScheduleReadinessBlocksDNSUntilSiteConfigIsApplied(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 8, 30, 14, 30, 0, 0, time.UTC)
	site, err := app.db.CreateSiteRecord(Site{
		Name:        "site-readiness",
		PublicHost:  "site-readiness.example.com",
		IngressMode: ingressModeHost,
		TargetURL:   "http://127.0.0.1:8096",
	})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := app.db.CreateControlNode(NodeCreateInput{
		Name:    "site-readiness-node",
		Address: "203.0.113.45",
		Port:    9090,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`
		UPDATE control_nodes
		SET desired_config_hash=?, applied_config_hash=?
		WHERE id=?`, "site-config-v1", "site-config-v1", node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`
		UPDATE site_node_schedules
		SET desired_node_id=?, config_hash=?, config_pending_since_ms=?
		WHERE site_id=?`, node.ID, "site-config-v1", now.UnixMilli(), site.ID); err != nil {
		t.Fatal(err)
	}
	schedule, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = app.reconcileOneSiteSchedule(context.Background(), schedule, now)
	var readiness *nodeReadinessError
	if !errors.As(err, &readiness) || readiness.Kind != readinessConfig {
		t.Fatalf("reconcile error = %v, want config readiness error", err)
	}
	if !strings.Contains(err.Error(), "site configuration") {
		t.Fatalf("reconcile error = %q, want site configuration detail", err)
	}
}

func TestSchema32BackfillsPendingSiteConfigurationTimers(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 8, 30, 15, 0, 0, 0, time.UTC)
	stableNode, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "stable-schema-node", Address: "203.0.113.46"}, now)
	if err != nil {
		t.Fatal(err)
	}
	pendingNode, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "pending-schema-node", Address: "203.0.113.47"}, now)
	if err != nil {
		t.Fatal(err)
	}
	stableSite, err := app.db.CreateSiteRecord(Site{Name: "stable-schema-site", ListenPort: freePort(t), PublicHost: "stable-schema.example.com", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatal(err)
	}
	pendingSite, err := app.db.CreateSiteRecord(Site{Name: "pending-schema-site", ListenPort: freePort(t), PublicHost: "pending-schema.example.com", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		nodeID int64
		siteID int64
		hash   string
	}{
		{stableNode.ID, stableSite.ID, "stable-config"},
		{pendingNode.ID, pendingSite.ID, "pending-config"},
	} {
		if _, err := app.db.SaveSiteNodeSchedule(item.siteID, true, "fixed", item.nodeID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := app.db.db.Exec(`
			UPDATE site_node_schedules
			SET desired_node_id=?, config_hash=?, config_pending_since_ms=0
			WHERE site_id=?`, item.nodeID, item.hash, item.siteID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.db.db.Exec(`
		UPDATE control_nodes
		SET desired_config_hash=?, applied_config_hash=?
		WHERE id=?`, "stable-config", "stable-config", stableNode.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`
		UPDATE control_nodes
		SET desired_config_hash=?, applied_config_hash=?
		WHERE id=?`, "pending-config", "old-config", pendingNode.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("PRAGMA user_version = 31"); err != nil {
		t.Fatal(err)
	}
	if err := app.db.migrate(); err != nil {
		t.Fatalf("schema 32 migration: %v", err)
	}
	var stablePending, pendingSince int64
	if err := app.db.db.QueryRow("SELECT config_pending_since_ms FROM site_node_schedules WHERE site_id=?", stableSite.ID).Scan(&stablePending); err != nil {
		t.Fatal(err)
	}
	if err := app.db.db.QueryRow("SELECT config_pending_since_ms FROM site_node_schedules WHERE site_id=?", pendingSite.ID).Scan(&pendingSince); err != nil {
		t.Fatal(err)
	}
	if stablePending != 0 {
		t.Fatalf("stable schedule pending_since=%d, want 0", stablePending)
	}
	if pendingSince <= 0 {
		t.Fatalf("inconsistent schedule pending_since=%d, want migration timestamp", pendingSince)
	}
	var schemaVersion int
	if err := app.db.db.QueryRow("PRAGMA user_version").Scan(&schemaVersion); err != nil {
		t.Fatal(err)
	}
	if schemaVersion != databaseSchemaVersion {
		t.Fatalf("schema version=%d, want %d", schemaVersion, databaseSchemaVersion)
	}
}

func TestSiteNodeAutoSchedulingFallsBackAfterProbeCooldown(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 8, 30, 15, 0, 0, 0, time.UTC)
	site, err := app.db.CreateSiteRecord(Site{Name: "fallback", ListenPort: 18092, PublicHost: "fallback.example.com", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096", PlaybackMode: "direct", StreamHosts: "[]", UAMode: "passthrough"})
	if err != nil {
		t.Fatal(err)
	}
	first, tokenA, err := app.db.CreateControlNode(NodeCreateInput{Name: "first", Address: "203.0.113.20", Port: 9090, Priority: 200}, now)
	if err != nil {
		t.Fatal(err)
	}
	second, tokenB, err := app.db.CreateControlNode(NodeCreateInput{Name: "second", Address: "203.0.113.21", Port: 9090, Priority: 100}, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{tokenA, tokenB} {
		_, agentToken, enrollErr := app.db.EnrollControlNode(token, now)
		if enrollErr != nil {
			t.Fatal(enrollErr)
		}
		if _, reportErr := app.db.RecordNodeReport(agentToken, NodeReport{BootID: "boot-" + token[:4], Sequence: 1, InterfaceName: "eth0"}, now); reportErr != nil {
			t.Fatal(reportErr)
		}
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "global", 0, now); err != nil {
		t.Fatal(err)
	}
	if err := app.refreshSiteAssignments(now); err != nil {
		t.Fatal(err)
	}
	schedule, err := app.db.siteNodeSchedule(site.ID)
	if err != nil || schedule.DesiredNodeID != first.ID {
		t.Fatalf("initial desired node = %#v, %v", schedule, err)
	}
	if err := app.db.recordSiteNodeProbeFailure(site.ID, first.ID, errors.New("probe failed"), now); err != nil {
		t.Fatal(err)
	}
	if err := app.refreshSiteAssignments(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	schedule, err = app.db.siteNodeSchedule(site.ID)
	if err != nil || schedule.DesiredNodeID != second.ID {
		t.Fatalf("fallback desired node = %#v, %v", schedule, err)
	}
}

func TestSiteNodeAutoSchedulingKeepsSingleOnlineNodeDuringProbeCooldown(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 8, 30, 16, 0, 0, 0, time.UTC)
	site, err := app.db.CreateSiteRecord(Site{Name: "single", PublicHost: "single.example.com", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatal(err)
	}
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "only", Address: "203.0.113.30", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, agentToken, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.RecordNodeReport(agentToken, NodeReport{BootID: "only", Sequence: 1, InterfaceName: "eth0"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "global", 0, now); err != nil {
		t.Fatal(err)
	}
	if err := app.refreshSiteAssignments(now); err != nil {
		t.Fatal(err)
	}
	if err := app.db.recordSiteNodeProbeFailure(site.ID, node.ID, errors.New("probe failed"), now); err != nil {
		t.Fatal(err)
	}
	if err := app.refreshSiteAssignments(now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	schedule, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.DesiredNodeID != node.ID {
		t.Fatalf("single online node disappeared during probe cooldown: %#v", schedule)
	}
}

func TestAgentSiteCacheTelemetryUsesCentralSiteIDAndPreservesUnknownSize(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 8, 30, 16, 30, 0, 0, time.UTC)
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "cache-node", Address: "203.0.113.31", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, agentToken, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.RecordNodeReport(agentToken, NodeReport{BootID: "cache", Sequence: 1, InterfaceName: "eth0"}, now); err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "cache-site", PublicHost: "cache.example.com", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET desired_node_id=? WHERE site_id=?", node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	stat := NodeSiteStat{Host: site.PublicHost, RequestCount: 1, BytesIn: 5, CumulativeBytesIn: 5, CacheSizeBytes: 438 << 20, CacheSizeValid: true}
	if _, err := app.db.RecordNodeReport(agentToken, NodeReport{BootID: "cache", Sequence: 2, InterfaceName: "eth0", SiteStats: []NodeSiteStat{stat}}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var cacheSize int64
	if err := app.db.db.QueryRow("SELECT cache_size_bytes FROM node_site_counters WHERE node_id=? AND site_id=?", node.ID, site.ID).Scan(&cacheSize); err != nil {
		t.Fatal(err)
	}
	if cacheSize != stat.CacheSizeBytes {
		t.Fatalf("central site cache size=%d, want=%d", cacheSize, stat.CacheSizeBytes)
	}
	stat.CacheSizeBytes = 0
	stat.CacheSizeValid = false
	if _, err := app.db.RecordNodeReport(agentToken, NodeReport{BootID: "cache", Sequence: 3, InterfaceName: "eth0", SiteStats: []NodeSiteStat{stat}}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := app.db.db.QueryRow("SELECT cache_size_bytes FROM node_site_counters WHERE node_id=? AND site_id=?", node.ID, site.ID).Scan(&cacheSize); err != nil {
		t.Fatal(err)
	}
	if cacheSize != 438<<20 {
		t.Fatalf("unknown cache size erased value: got=%d", cacheSize)
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

func TestDisabledSiteKeepsNodeSchedulingPreference(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 8, 30, 14, 30, 0, 0, time.UTC)
	node, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "preferred", Address: "203.0.113.80", Port: 19080}, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "disabled-site", ListenPort: freePort(t), PublicHost: "disabled.example.com", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096", PlaybackMode: "direct", StreamHosts: "[]", UAMode: passthroughUAMode})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=?,dns_status='active',config_hash='old-hash' WHERE site_id=?`, node.ID, node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE sites SET enabled=0 WHERE id=?", site.ID); err != nil {
		t.Fatal(err)
	}
	schedule, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.reconcileOneSiteSchedule(context.Background(), schedule, now); err != nil {
		t.Fatalf("reconcile disabled site: %v", err)
	}
	updated, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Enabled || updated.Mode != "fixed" || updated.FixedNodeID != node.ID {
		t.Fatalf("scheduling preference was cleared: %#v", updated)
	}
	if updated.DesiredNodeID != 0 || updated.AppliedNodeID != 0 || updated.DNSStatus != "disabled" {
		t.Fatalf("disabled site runtime was not cleared: %#v", updated)
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
	if !strings.Contains(script, "Linux amd64 and arm64 only") {
		t.Fatal("install script does not declare the supported architectures")
	}
	if strings.Contains(script, "IGNORECASE") || !strings.Contains(script, "tolower($1)") {
		t.Fatal("install script must parse response headers with portable case folding")
	}
}

func TestAgentInstallScriptIsTransactional(t *testing.T) {
	script := buildNodeInstallScript("https://panel.example.com", "enrollment-secret")
	downloadIndex := strings.Index(script, "curl --proto")
	stopIndex := strings.Index(script, "systemctl stop meridian-agent.service")
	if downloadIndex < 0 || stopIndex < 0 || stopIndex < downloadIndex {
		t.Fatalf("Agent installer stops the service before downloading and validating: download=%d stop=%d", downloadIndex, stopIndex)
	}
	if !strings.Contains(script, "--reenroll") || !strings.Contains(script, "wait_for_registration") {
		t.Fatal("Agent installer does not expose explicit re-enrollment and registration verification")
	}
	if !strings.Contains(script, "previous Agent, state, token, and service were restored") {
		t.Fatal("Agent installer does not expose rollback behavior")
	}
}

func TestBuildNodeEnrollmentScriptForcesReenrollment(t *testing.T) {
	script := buildNodeInstallScriptWithOptions("https://panel.example.com", "enrollment-secret", true)
	if !strings.Contains(script, "-t 'enrollment-secret' --reenroll") {
		t.Fatalf("enrollment script does not request re-enrollment: %s", script)
	}
	command := buildNodeInstallCommandWithOptions("https://panel.example.com", "enrollment-secret", true)
	if !strings.HasSuffix(command, "--reenroll") {
		t.Fatalf("enrollment command does not request re-enrollment: %s", command)
	}
}

func TestAgentPlatformHeaderNormalization(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "linux/amd64", want: "linux/amd64"},
		{input: "Linux-amd64", want: "linux/amd64"},
		{input: "x86_64", want: "linux/amd64"},
		{input: "linux/arm64", want: "linux/arm64"},
		{input: "aarch64", want: "linux/arm64"},
	} {
		if got := normalizeAgentPlatform(test.input); got != test.want {
			t.Fatalf("normalizeAgentPlatform(%q) = %q, want %q", test.input, got, test.want)
		}
	}
	if got := normalizeAgentPlatform("windows/amd64"); got != "" {
		t.Fatalf("unsupported platform normalized to %q", got)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/agent/binary", nil)
	request.Header.Set(agentPlatformHeader, "ARM64")
	platform, err := requestedAgentPlatform(request)
	if err != nil || platform != "linux/arm64" {
		t.Fatalf("requested platform = %q, err=%v", platform, err)
	}
}

func TestLegacyAgentConfigDoesNotAdvertiseControllerBinary(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	_, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "legacy-agent", Address: "203.0.113.40", Port: 9090}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	config, err := app.buildAgentConfigForPlatform(token, now, "")
	if err != nil {
		t.Fatalf("build legacy Agent config: %v", err)
	}
	if config.AgentVersion != "" || config.AgentSHA256 != "" {
		t.Fatalf("legacy Agent config advertised a local binary: version=%q sha=%q", config.AgentVersion, config.AgentSHA256)
	}
}

func TestAgentConfigHashSeparatesReleaseMetadata(t *testing.T) {
	config := AgentRuntimeConfig{
		SchemaVersion:    agentConfigSchemaVersion,
		NodeGUID:         "hash-node",
		HTTPSPort:        9090,
		DynamicKey:       testEdgeRuntimeKey(t),
		AgentVersion:     "v1.9.30",
		AgentSHA256:      strings.Repeat("a", 64),
		AgentDownloadURL: "https://github.com/chanhui800/Meridian/releases/download/v1.9.30/meridian-agent-linux-amd64",
		ProbeSecret:      encodeRuntimeKey(bytes.Repeat([]byte{0x42}, 32)),
		Routes:           []AgentSiteRoute{},
	}
	runtimeHash, err := agentConfigHash(config)
	if err != nil {
		t.Fatal(err)
	}
	config.AgentVersion = "v1.9.31"
	config.AgentSHA256 = strings.Repeat("b", 64)
	config.AgentDownloadURL = "https://github.com/chanhui800/Meridian/releases/download/v1.9.31/meridian-agent-linux-amd64"
	changedHash, err := agentConfigHash(config)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeHash != changedHash {
		t.Fatalf("release metadata changed runtime config hash: %s != %s", runtimeHash, changedHash)
	}
	legacyHash, err := agentConfigLegacyHash(config)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeHash == legacyHash {
		t.Fatal("legacy metadata-inclusive hash unexpectedly matched runtime hash")
	}
	if !agentUsesRuntimeConfigHash("v1.9.30") || agentUsesRuntimeConfigHash("v1.9.29") {
		t.Fatal("runtime config hash compatibility gate is incorrect")
	}
	if !agentUsesRuntimeConfigHash("v1.9.74-pre.6") || !agentSupportsProbeSecret("v1.9.74-pre.6") || !agentSupportsTrafficBaseline("v1.9.74-pre.6") {
		t.Fatal("prerelease build must use the current Agent compatibility contract")
	}
	if preHash, err := agentConfigHashForVersion(config, "v1.9.74-pre.6"); err != nil || preHash != runtimeHash {
		t.Fatalf("prerelease hash differs from runtime hash: got=%q want=%q err=%v", preHash, runtimeHash, err)
	}
	if agentSupportsProbeSecret("v1.9.42") || !agentSupportsProbeSecret("v1.9.43") {
		t.Fatal("probe secret compatibility gate is incorrect")
	}
	if agentSupportsCacheClear("v1.9.49") || !agentSupportsCacheClear("v1.9.50") {
		t.Fatal("cache clear compatibility gate is incorrect")
	}
	if agentSupportsForceStop("v1.9.64") || !agentSupportsForceStop("v1.9.65") {
		t.Fatal("force-stop compatibility gate is incorrect")
	}
	forceStopConfig := config
	forceStopConfig.ForceStopSiteIDs = []int64{42, 7}
	withoutForceStop := forceStopConfig
	withoutForceStop.ForceStopSiteIDs = nil
	wantPreForceStopHash, err := agentConfigHash(withoutForceStop)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := agentConfigHashForVersion(forceStopConfig, "v1.9.64"); err != nil || got != wantPreForceStopHash {
		t.Fatalf("v1.9.64 hash included unknown force-stop field: got=%q want=%q err=%v", got, wantPreForceStopHash, err)
	}
	if got, err := agentConfigHashForVersion(forceStopConfig, "v1.9.65"); err != nil || got != wantPreForceStopHash {
		t.Fatalf("v1.9.65 force-stop command changed runtime hash: got=%q want=%q err=%v", got, wantPreForceStopHash, err)
	}
	cacheGenerationConfig := config
	cacheGenerationConfig.CacheClearGeneration = 7
	cacheGenerationHash, err := agentConfigHash(cacheGenerationConfig)
	if err != nil {
		t.Fatal(err)
	}
	legacyCacheGenerationHash, err := agentConfigHashForVersion(cacheGenerationConfig, "v1.9.49")
	if err != nil {
		t.Fatal(err)
	}
	withoutCacheGeneration := cacheGenerationConfig
	withoutCacheGeneration.CacheClearGeneration = 0
	wantLegacyCacheGenerationHash, err := agentConfigHash(withoutCacheGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if legacyCacheGenerationHash != wantLegacyCacheGenerationHash || cacheGenerationHash == legacyCacheGenerationHash {
		t.Fatalf("cache clear field was not gated for v1.9.49: legacy=%q want=%q runtime=%q", legacyCacheGenerationHash, wantLegacyCacheGenerationHash, cacheGenerationHash)
	}
	if got, err := agentConfigHashForVersion(cacheGenerationConfig, "v1.9.50"); err != nil || got != cacheGenerationHash {
		t.Fatalf("v1.9.50 config hash did not include cache clear generation: got=%q want=%q err=%v", got, cacheGenerationHash, err)
	}
	preProbeSecret := config
	preProbeSecret.ProbeSecret = ""
	preProbeHash, err := agentConfigHash(preProbeSecret)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := agentConfigHashForVersion(config, "v1.9.42"); err != nil || got != preProbeHash {
		t.Fatalf("v1.9.42 config hash did not omit probe secret: got=%q want=%q err=%v", got, preProbeHash, err)
	}
	if got, err := agentConfigHashForVersion(config, "v1.9.43"); err != nil || got != runtimeHash {
		t.Fatalf("v1.9.43 config hash changed unexpectedly: got=%q want=%q err=%v", got, runtimeHash, err)
	}
	if runtimeHash == preProbeHash {
		t.Fatal("probe secret did not change the current runtime hash")
	}
	// Freeze the v1.9.42 wire shape rather than comparing two current structs.
	// This catches future fields that JSON-unmarshal would silently discard on
	// an old Agent and ensures the compatibility hash is calculated over what
	// that Agent actually understood.
	type agentRuntimeConfigV1942 struct {
		SchemaVersion  int              `json:"schema_version"`
		ConfigHash     string           `json:"config_hash"`
		NodeGUID       string           `json:"node_guid"`
		EntryMode      string           `json:"entry_mode"`
		HTTPPort       int              `json:"http_port"`
		HTTPSPort      int              `json:"https_port"`
		CertificatePEM string           `json:"certificate_pem,omitempty"`
		PrivateKeyPEM  string           `json:"private_key_pem,omitempty"`
		DynamicKey     string           `json:"dynamic_key,omitempty"`
		AgentVersion   string           `json:"agent_version,omitempty"`
		AgentSHA256    string           `json:"agent_sha256,omitempty"`
		Routes         []AgentSiteRoute `json:"routes"`
	}
	wire, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var frozen agentRuntimeConfigV1942
	if err := json.Unmarshal(wire, &frozen); err != nil {
		t.Fatal(err)
	}
	frozenHash, err := agentConfigHash(AgentRuntimeConfig{
		SchemaVersion: frozen.SchemaVersion, ConfigHash: frozen.ConfigHash,
		NodeGUID: frozen.NodeGUID, EntryMode: frozen.EntryMode,
		HTTPPort: frozen.HTTPPort, HTTPSPort: frozen.HTTPSPort,
		CertificatePEM: frozen.CertificatePEM, PrivateKeyPEM: frozen.PrivateKeyPEM,
		DynamicKey: frozen.DynamicKey, AgentVersion: frozen.AgentVersion,
		AgentSHA256: frozen.AgentSHA256, Routes: frozen.Routes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := agentConfigHashForVersion(config, "v1.9.42"); err != nil || got != frozenHash {
		t.Fatalf("v1.9.42 frozen wire hash mismatch: got=%q want=%q err=%v", got, frozenHash, err)
	}
}

func TestAgentConfigHashPreservesPreTrafficBaselineCompatibility(t *testing.T) {
	config := AgentRuntimeConfig{
		SchemaVersion: agentConfigSchemaVersion,
		NodeGUID:      "traffic-hash-node",
		HTTPSPort:     9090,
		DynamicKey:    testEdgeRuntimeKey(t),
		Routes: []AgentSiteRoute{{
			SiteID: 7,
			Host:   "media.example.test",
			Site: Site{
				TrafficQuota: 10_000,
				TrafficUsed:  3_000,
			},
		}},
	}

	legacyHash, err := agentConfigHashForVersion(config, "v1.9.60")
	if err != nil {
		t.Fatal(err)
	}
	wantLegacyHash, err := hashAgentConfigPayloadForVersion(config, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if legacyHash != wantLegacyHash {
		t.Fatalf("pre-v1.9.61 hash did not preserve live traffic fields: got=%q want=%q", legacyHash, wantLegacyHash)
	}

	modernHash, err := agentConfigHashForVersion(config, "v1.9.61")
	if err != nil {
		t.Fatal(err)
	}
	wantModernHash, err := agentConfigHash(config)
	if err != nil {
		t.Fatal(err)
	}
	if modernHash != wantModernHash {
		t.Fatalf("v1.9.61 hash changed unexpectedly: got=%q want=%q", modernHash, wantModernHash)
	}
	if legacyHash == modernHash {
		t.Fatal("traffic baseline compatibility hashes unexpectedly matched")
	}
	if agentSupportsTrafficBaseline("v1.9.60") || !agentSupportsTrafficBaseline("v1.9.61") {
		t.Fatal("traffic baseline compatibility gate is incorrect")
	}
}

func TestAgentConfigHashIgnoresSiteIconMetadata(t *testing.T) {
	config := AgentRuntimeConfig{
		SchemaVersion: agentConfigSchemaVersion,
		NodeGUID:      "icon-hash-node",
		HTTPSPort:     9090,
		DynamicKey:    testEdgeRuntimeKey(t),
		ProbeSecret:   encodeRuntimeKey(bytes.Repeat([]byte{0x37}, 32)),
		Routes: []AgentSiteRoute{{
			SiteID:    1,
			Host:      "media.example.test",
			TargetURL: "https://origin.example.test",
			Site: Site{
				ID:       1,
				Name:     "Icon site",
				IconName: "Emby",
				IconURL:  "https://icons.example.test/emby.png",
			},
		}},
	}
	withIcons, err := agentConfigHash(config)
	if err != nil {
		t.Fatal(err)
	}
	withoutIcons := config
	withoutIcons.Routes = append([]AgentSiteRoute(nil), config.Routes...)
	withoutIcons.Routes[0].Site.IconName = ""
	withoutIcons.Routes[0].Site.IconURL = ""
	withoutIconsHash, err := agentConfigHash(withoutIcons)
	if err != nil {
		t.Fatal(err)
	}
	if withIcons != withoutIconsHash {
		t.Fatalf("site icon metadata changed Agent config hash: %q != %q", withIcons, withoutIconsHash)
	}
	payload, err := json.Marshal(agentConfigHashPayload(config, false))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte(`"icon_name"`)) || bytes.Contains(payload, []byte(`"icon_url"`)) {
		t.Fatalf("Agent hash payload includes UI-only icon metadata: %s", payload)
	}
}

func TestNormalizeControllerURLRequiresHTTPSForRemoteHosts(t *testing.T) {
	if _, err := normalizeControllerURL("http://panel.example.com"); err == nil {
		t.Fatal("HTTP controller URL accepted")
	}
	for _, input := range []string{"https://panel.example.com/", "https://localhost:9090", "https://127.0.0.1:9090"} {
		if _, err := normalizeControllerURL(input); err != nil {
			t.Fatalf("normalizeControllerURL(%q): %v", input, err)
		}
	}
	for _, input := range []string{"http://localhost:9090", "http://127.0.0.1:9090", "http://[::1]:9090"} {
		if _, err := normalizeControllerURL(input); err == nil {
			t.Fatalf("HTTP controller URL %q accepted", input)
		}
	}
}

func TestNormalizeControllerURLRejectsBasePaths(t *testing.T) {
	for _, input := range []string{
		"https://panel.example.com/meridian",
		"https://panel.example.com/meridian/",
		"https://panel.example.com/%2Fmeridian",
	} {
		if _, err := normalizeControllerURL(input); err == nil || !strings.Contains(err.Error(), "must not contain a path") {
			t.Fatalf("normalizeControllerURL(%q) accepted path: %v", input, err)
		}
	}
}

func TestAgentSessionLeaseRejectsStaleReports(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "lease-node", Address: "203.0.113.210"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	first, err := app.buildAgentConfigForRequest(context.Background(), token, now, "", "", agentSessionRequest{ID: "session-a", Epoch: 100})
	if err != nil {
		t.Fatalf("claim first session: %v", err)
	}
	if first.AgentLeaseID == "" {
		t.Fatal("first config did not issue an Agent lease")
	}
	if _, err := app.db.RecordNodeReport(token, NodeReport{BootID: "session-a", ReportSessionID: "session-a", SessionEpoch: 100, AgentLeaseID: first.AgentLeaseID, CounterEpoch: "kernel-a", Sequence: 1, InterfaceName: "eth0", RXBytes: 100, TXBytes: 200, AgentVersion: "test"}, now.Add(time.Second)); err != nil {
		t.Fatalf("first session report: %v", err)
	}
	second, err := app.buildAgentConfigForRequest(context.Background(), token, now.Add(2*time.Second), "", "", agentSessionRequest{ID: "session-b", Epoch: 200})
	if err != nil {
		t.Fatalf("claim second session: %v", err)
	}
	if second.AgentLeaseID == first.AgentLeaseID {
		t.Fatal("new session reused the previous Agent lease")
	}
	before, err := app.db.controlNodeByID(node.ID, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.RecordNodeReport(token, NodeReport{BootID: "session-a", ReportSessionID: "session-a", SessionEpoch: 100, AgentLeaseID: first.AgentLeaseID, CounterEpoch: "kernel-a", Sequence: 2, InterfaceName: "eth0", RXBytes: 999999, TXBytes: 999999, AgentVersion: "stale"}, now.Add(3*time.Second)); !errors.Is(err, errStaleAgentSession) {
		t.Fatalf("stale report error=%v, want errStaleAgentSession", err)
	}
	after, err := app.db.controlNodeByID(node.ID, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if after.LastSeenAtMS != before.LastSeenAtMS || after.lastRawRXBytes != before.lastRawRXBytes || after.lastRawTXBytes != before.lastRawTXBytes {
		t.Fatalf("stale report changed node state: before=%#v after=%#v", before, after)
	}
	if _, err := app.db.RecordNodeReport(token, NodeReport{BootID: "session-b", ReportSessionID: "session-b", SessionEpoch: 200, AgentLeaseID: second.AgentLeaseID, CounterEpoch: "kernel-b", Sequence: 1, InterfaceName: "eth0", RXBytes: 300, TXBytes: 400, AgentVersion: "current"}, now.Add(4*time.Second)); err != nil {
		t.Fatalf("current session report: %v", err)
	}
}
