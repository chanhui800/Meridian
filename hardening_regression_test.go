package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNextEdgeSessionEpochRemainsMonotonicAcrossClockRollback(t *testing.T) {
	saved := time.Now().Add(24 * time.Hour).UnixNano()
	next, err := nextEdgeSessionEpoch(saved)
	if err != nil {
		t.Fatalf("nextEdgeSessionEpoch: %v", err)
	}
	if next <= saved {
		t.Fatalf("next epoch=%d, want greater than saved=%d", next, saved)
	}
}

func TestAgentInstanceLockRejectsSecondOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.state.lock")
	release, err := acquireAgentInstanceLock(path)
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	defer release()
	second, err := acquireAgentInstanceLock(path)
	if second != nil {
		second()
	}
	if err != errAgentAlreadyRunning {
		t.Fatalf("second lock error=%v, want %v", err, errAgentAlreadyRunning)
	}
}

func TestClassifyOwnedAddressRecordsDetectsContentChangesAndConflicts(t *testing.T) {
	owned, ok, unowned, ambiguous := classifyOwnedAddressRecords([]cloudflareAddressRecord{{
		ID: "owned", Type: "A", Name: "site.example", Content: "198.51.100.2", Comment: "Meridian site=7",
	}}, "site.example", "Meridian site=7")
	if !ok || ambiguous || unowned != 0 || owned.ID != "owned" {
		t.Fatalf("owned classification=%#v,%v,%d,%v", owned, ok, unowned, ambiguous)
	}
	_, ok, unowned, ambiguous = classifyOwnedAddressRecords([]cloudflareAddressRecord{{
		ID: "operator", Type: "A", Name: "site.example", Content: "198.51.100.2",
	}}, "site.example", "Meridian site=7")
	if ok || ambiguous || unowned != 1 {
		t.Fatalf("unowned classification ok=%v unowned=%d ambiguous=%v", ok, unowned, ambiguous)
	}
	_, ok, unowned, ambiguous = classifyOwnedAddressRecords([]cloudflareAddressRecord{
		{ID: "one", Type: "A", Name: "site.example", Comment: "Meridian site=7"},
		{ID: "two", Type: "A", Name: "site.example", Comment: "Meridian site=7"},
	}, "site.example", "Meridian site=7")
	if ok || !ambiguous || unowned != 0 {
		t.Fatalf("ambiguous classification ok=%v unowned=%d ambiguous=%v", ok, unowned, ambiguous)
	}
}

func TestDashboardTrendGenerationChangesAfterSuccessfulFlushMarker(t *testing.T) {
	inst := &ProxyInstance{trafficCounter: &edgeSiteTrafficCounter{}}
	pm := &ProxyManager{proxies: map[int64]*ProxyInstance{7: inst}}
	if !pm.dashboardTrendGenerationsMatch(nil, map[int64]uint64{7: 0}) {
		t.Fatal("initial generation should match")
	}
	inst.trafficMu.Lock()
	inst.trafficFlushGeneration().Add(1)
	inst.trafficMu.Unlock()
	if pm.dashboardTrendGenerationsMatch(nil, map[int64]uint64{7: 0}) {
		t.Fatal("changed generation must invalidate the snapshot")
	}
}

func TestSiteScheduleRevisionAdvancesOnEveryAdministrativeMutation(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSiteRecord(Site{
		Name: "revision-site", ListenPort: 19091, PublicHost: "revision.example.com",
		TargetURL: "http://127.0.0.1:8096", PlaybackMode: "direct", StreamHosts: "[]", UAMode: "passthrough",
	})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	first, err := app.db.SaveSiteNodeSchedule(site.ID, false, "global", 0, time.Now())
	if err != nil {
		t.Fatalf("save first schedule: %v", err)
	}
	second, err := app.db.SaveSiteNodeSchedule(site.ID, false, "fixed", 0, time.Now().Add(time.Second))
	if err != nil {
		t.Fatalf("save second schedule: %v", err)
	}
	if first.ScheduleRevision <= 0 || second.ScheduleRevision <= first.ScheduleRevision {
		t.Fatalf("revisions did not advance: first=%d second=%d", first.ScheduleRevision, second.ScheduleRevision)
	}
}

func TestStaleDisabledCleanupCannotPublishRevocation(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "cleanup-node", Address: "203.0.113.90", Port: 9090}, now)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "cleanup-site", PublicHost: "cleanup.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, false, "fixed", 0, now); err != nil {
		t.Fatalf("create disabled schedule: %v", err)
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=?,dns_status='waiting' WHERE site_id=?`, node.ID, node.ID, site.ID); err != nil {
		t.Fatalf("seed disabled assignment: %v", err)
	}
	stale, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatalf("load stale schedule: %v", err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now.Add(time.Second)); err != nil {
		t.Fatalf("re-enable schedule: %v", err)
	}
	if err := app.finalizeDisabledSiteNodeSchedule(stale); err != nil {
		t.Fatalf("stale cleanup: %v", err)
	}
	var revocations, enabled int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM agent_route_revocations WHERE site_id=?", site.ID).Scan(&revocations); err != nil {
		t.Fatal(err)
	}
	if err := app.db.db.QueryRow("SELECT enabled FROM site_node_schedules WHERE site_id=?", site.ID).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if revocations != 0 || enabled != 1 {
		t.Fatalf("stale cleanup changed replacement generation: revocations=%d enabled=%d", revocations, enabled)
	}
}

func TestClaimAgentLeaseTTLTakeover(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "lease-node", Address: "203.0.113.91", Port: 9090}, now)
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	claim := func(sessionID string, epoch int64, at time.Time) (string, error) {
		tx, err := app.db.db.Begin()
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback()
		lease, err := claimAgentLeaseTx(tx, node.ID, sessionID, epoch, at)
		if err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return lease, nil
	}
	leaseA, err := claim("session-a", 100, now)
	if err != nil || leaseA == "" {
		t.Fatalf("first claim: lease=%q err=%v", leaseA, err)
	}
	leaseB, err := claim("session-b", 101, now.Add(5*time.Second))
	if err != nil || leaseB == "" || leaseB == leaseA {
		t.Fatalf("newer epoch must rotate the lease: lease=%q err=%v", leaseB, err)
	}
	if _, err := claim("session-a", 100, now.Add(6*time.Second)); !errors.Is(err, errStaleAgentSession) {
		t.Fatalf("older epoch against a fresh lease must stay stale, err=%v", err)
	}
	renewed, err := claim("session-b", 101, now.Add(7*time.Second))
	if err != nil || renewed != leaseB {
		t.Fatalf("same session must renew without rotating: lease=%q err=%v", renewed, err)
	}
	// Expire the active lease: the previously superseded session must now be
	// able to take the node over instead of waiting for a process restart.
	if _, err := app.db.db.Exec("UPDATE control_nodes SET agent_lease_expires_at_ms=? WHERE id=?", now.Add(10*time.Second).UnixMilli(), node.ID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}
	takeoverAt := now.Add(20 * time.Second)
	leaseA2, err := claim("session-a", 100, takeoverAt)
	if err != nil || leaseA2 == "" || leaseA2 == leaseA {
		t.Fatalf("expired lease must allow takeover: lease=%q err=%v", leaseA2, err)
	}
	var activeSession string
	var activeEpoch, expiryMS int64
	if err := app.db.db.QueryRow("SELECT active_agent_session_id,agent_session_epoch,agent_lease_expires_at_ms FROM control_nodes WHERE id=?", node.ID).Scan(&activeSession, &activeEpoch, &expiryMS); err != nil {
		t.Fatal(err)
	}
	if activeSession != "session-a" || activeEpoch != 100 {
		t.Fatalf("takeover identity session=%q epoch=%d, want session-a/100", activeSession, activeEpoch)
	}
	if expiryMS != takeoverAt.Add(agentLeaseTTL).UnixMilli() {
		t.Fatalf("takeover expiry=%d, want %d", expiryMS, takeoverAt.Add(agentLeaseTTL).UnixMilli())
	}
	// A lower-epoch session (another stale clone) stays fenced by the fresh
	// lease; a higher epoch may still take over by the newer-session rule.
	if _, err := claim("session-c", 99, takeoverAt.Add(time.Second)); !errors.Is(err, errStaleAgentSession) {
		t.Fatalf("lower-epoch clone must stay fenced after takeover, err=%v", err)
	}
}

func TestNodeSchedulerWorkerSkipsSupersededJobs(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	site, err := app.db.CreateSiteRecord(Site{Name: "stale-job-site", PublicHost: "stale-job.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, false, "global", 0, now); err != nil {
		t.Fatalf("create disabled schedule: %v", err)
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_schedules SET cf_zone_id='zone-1',cf_record_id='rec-1',cf_record_type='A',applied_address='203.0.113.9' WHERE site_id=?`, site.ID); err != nil {
		t.Fatalf("track DNS handle: %v", err)
	}
	current, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatalf("load schedule: %v", err)
	}
	queue := &nodeSchedulerQueue{jobs: make(chan nodeSchedulerJob, 4), inFlight: map[int64]struct{}{}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A job captured before the latest generation must not touch the site at
	// all: no Cloudflare client attempt, no outcome write, no revision bump.
	stale := current
	stale.ScheduleRevision--
	app.runNodeSchedulerJob(ctx, queue, nodeSchedulerJob{value: stale, cleanup: true, now: now})
	afterStale, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterStale.ScheduleRevision != current.ScheduleRevision || afterStale.LastError != "" || afterStale.cfRecordID != "rec-1" {
		t.Fatalf("stale job mutated the schedule: revision=%d error=%q record=%q", afterStale.ScheduleRevision, afterStale.LastError, afterStale.cfRecordID)
	}

	// A current job must proceed past re-validation into the Cloudflare path,
	// which fails in a bare test app before any remote side effect.
	app.runNodeSchedulerJob(ctx, queue, nodeSchedulerJob{value: current, cleanup: true, now: now})
	afterFresh, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterFresh.LastError == "" {
		t.Fatal("current cleanup job did not reach the Cloudflare path")
	}
}

func TestDeleteTrackedSiteDNSRemoteVerifiesOwnership(t *testing.T) {
	recordJSON := func(id, recordType, name, content, comment string) string {
		return `{"success":true,"result":{"id":"` + id + `","type":"` + recordType + `","name":"` + name + `","content":"` + content + `","comment":"` + comment + `"}}`
	}
	schedule := SiteNodeSchedule{SiteID: 7, PublicHost: "site.example", cfZoneID: "zone-1", cfRecordID: "rec-1"}
	cases := []struct {
		name       string
		body       string
		status     int
		wantErr    string
		wantDelete bool
	}{
		{name: "owned record is deleted", body: recordJSON("rec-1", "A", "site.example", "203.0.113.5", "Meridian site=7"), status: http.StatusOK, wantDelete: true},
		{name: "operator-edited comment refuses delete", body: recordJSON("rec-1", "A", "site.example", "203.0.113.5", ""), status: http.StatusOK, wantErr: "refusing to delete"},
		{name: "renamed record refuses delete", body: recordJSON("rec-1", "A", "other.example", "203.0.113.5", "Meridian site=7"), status: http.StatusOK, wantErr: "refusing to delete"},
		{name: "wrong family refuses delete", body: recordJSON("rec-1", "CNAME", "site.example", "target.example", "Meridian site=7"), status: http.StatusOK, wantErr: "refusing to delete"},
		{name: "missing record stays idempotent", body: `{"success":false,"errors":[{"code":81044,"message":"record does not exist"}]}`, status: http.StatusNotFound, wantDelete: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var sawDelete bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					sawDelete = true
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(testCase.body))
			}))
			defer server.Close()
			cf := &cloudflareClient{token: "test", httpClient: server.Client(), apiBase: server.URL}
			err := deleteTrackedSiteDNSRemote(context.Background(), cf, schedule)
			if testCase.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
					t.Fatalf("err=%v, want containing %q", err, testCase.wantErr)
				}
				if sawDelete {
					t.Fatal("delete must not run for a record that lost ownership")
				}
				return
			}
			if err != nil {
				t.Fatalf("deleteTrackedSiteDNSRemote: %v", err)
			}
			if sawDelete != testCase.wantDelete {
				t.Fatalf("delete ran=%v, want %v", sawDelete, testCase.wantDelete)
			}
		})
	}
}

func TestRecreateReplacedDNSRecordRestoresPreimage(t *testing.T) {
	var payload struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Content string `json:"content"`
		Comment string `json:"comment"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/dns_records") {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode create payload: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"result":{"id":"restored"}}`))
	}))
	defer server.Close()
	cf := &cloudflareClient{token: "test", httpClient: server.Client(), apiBase: server.URL}
	previous := cloudflareAddressRecord{ID: "old", Type: "A", Name: "site.example", Content: "203.0.113.5", Comment: "Meridian site=7"}
	if err := recreateReplacedDNSRecordBestEffort(context.Background(), cf, "zone-1", previous); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if payload.Type != "A" || payload.Name != "site.example" || payload.Content != "203.0.113.5" || payload.Comment != "Meridian site=7" {
		t.Fatalf("restore payload=%+v, want the deleted record preimage", payload)
	}
}
