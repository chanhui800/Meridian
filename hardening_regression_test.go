package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	isOwned := func(comment string) bool { return siteDNSMarkerOwned(comment, 7, "test-install-uuid") }
	owned, ok, unowned, ambiguous := classifyOwnedAddressRecords([]cloudflareAddressRecord{{
		ID: "owned", Type: "A", Name: "site.example", Content: "198.51.100.2", Comment: siteDNSOwnershipMarker(7, "test-install-uuid"),
	}}, "site.example", isOwned)
	if !ok || ambiguous || unowned != 0 || owned.ID != "owned" {
		t.Fatalf("owned classification=%#v,%v,%d,%v", owned, ok, unowned, ambiguous)
	}
	// The legacy pre-UUID marker stays owned during the migration window.
	if owned, ok, _, _ := classifyOwnedAddressRecords([]cloudflareAddressRecord{{
		ID: "legacy", Type: "A", Name: "site.example", Content: "198.51.100.2", Comment: legacySiteDNSOwnershipMarker(7),
	}}, "site.example", isOwned); !ok || owned.ID != "legacy" {
		t.Fatalf("legacy marker not classified as owned: ok=%v owned=%#v", ok, owned)
	}
	// A different installation's scoped marker is NOT ours.
	if _, ok, unowned, _ := classifyOwnedAddressRecords([]cloudflareAddressRecord{{
		ID: "other", Type: "A", Name: "site.example", Content: "198.51.100.2", Comment: siteDNSOwnershipMarker(7, "another-install"),
	}}, "site.example", isOwned); ok || unowned != 1 {
		t.Fatalf("foreign controller marker classified as owned: ok=%v unowned=%d", ok, unowned)
	}
	_, ok, unowned, ambiguous = classifyOwnedAddressRecords([]cloudflareAddressRecord{{
		ID: "operator", Type: "A", Name: "site.example", Content: "198.51.100.2",
	}}, "site.example", isOwned)
	if ok || ambiguous || unowned != 1 {
		t.Fatalf("unowned classification ok=%v unowned=%d ambiguous=%v", ok, unowned, ambiguous)
	}
	_, ok, unowned, ambiguous = classifyOwnedAddressRecords([]cloudflareAddressRecord{
		{ID: "one", Type: "A", Name: "site.example", Comment: siteDNSOwnershipMarker(7, "test-install-uuid")},
		{ID: "two", Type: "A", Name: "site.example", Comment: legacySiteDNSOwnershipMarker(7)},
	}, "site.example", isOwned)
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
	marker := siteDNSOwnershipMarker(7, "test-install-uuid")
	cases := []struct {
		name       string
		body       string
		status     int
		wantErr    string
		wantDelete bool
	}{
		{name: "owned record is deleted", body: recordJSON("rec-1", "A", "site.example", "203.0.113.5", marker), status: http.StatusOK, wantDelete: true},
		{name: "legacy marker is deleted", body: recordJSON("rec-1", "A", "site.example", "203.0.113.5", legacySiteDNSOwnershipMarker(7)), status: http.StatusOK, wantDelete: true},
		{name: "foreign controller marker refuses delete", body: recordJSON("rec-1", "A", "site.example", "203.0.113.5", siteDNSOwnershipMarker(7, "another-install")), status: http.StatusOK, wantErr: "refusing to delete"},
		{name: "operator-edited comment refuses delete", body: recordJSON("rec-1", "A", "site.example", "203.0.113.5", ""), status: http.StatusOK, wantErr: "refusing to delete"},
		// A record whose host no longer matches the site still carries the
		// ownership marker and record ID, so cleanup may proceed: refusing it
		// would wedge site deletion forever after a site rename.
		{name: "renamed host with marker is deleted", body: recordJSON("rec-1", "A", "other.example", "203.0.113.5", "Meridian site=7"), status: http.StatusOK, wantDelete: true},
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
			cf := &cloudflareClient{token: "test", httpClient: server.Client(), apiBase: server.URL, installUUID: "test-install-uuid"}
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
	previous := cloudflareAddressRecord{ID: "old", Type: "A", Name: "site.example", Content: "203.0.113.5", Comment: siteDNSOwnershipMarker(7, "test-install-uuid")}
	if err := recreateReplacedDNSRecordBestEffort(context.Background(), cf, "zone-1", previous); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	if payload.Type != "A" || payload.Name != "site.example" || payload.Content != "203.0.113.5" || payload.Comment != siteDNSOwnershipMarker(7, "test-install-uuid") {
		t.Fatalf("restore payload=%+v, want the deleted record preimage", payload)
	}
}

func TestPathRouteSelectionIsDeterministic(t *testing.T) {
	pm := &ProxyManager{
		pathPrefixes: map[string]int64{"/emby": 2, "/foo": 1},
		proxies: map[int64]*ProxyInstance{
			1: {Site: Site{ID: 1}, handler: http.NotFoundHandler()},
			2: {Site: Site{ID: 2}, handler: http.NotFoundHandler()},
		},
	}
	// /emby/foo/x matches the direct /emby prefix and the embedded /emby/foo
	// form; the longer effective match must win on every iteration instead of
	// following Go's randomized map order.
	for i := 0; i < 50; i++ {
		_, prefix, ok := pm.PathRoute("/emby/foo/x")
		if !ok || prefix != "/foo" {
			t.Fatalf("iteration %d: prefix=%q ok=%v, want the deterministic /foo match", i, prefix, ok)
		}
	}
	// An exact /emby request only matches the /emby site directly.
	for i := 0; i < 50; i++ {
		_, prefix, ok := pm.PathRoute("/emby")
		if !ok || prefix != "/emby" {
			t.Fatalf("iteration %d: prefix=%q ok=%v, want the deterministic /emby match", i, prefix, ok)
		}
	}
	// Nested prefixes: the more specific /foo/bar wins over /foo.
	pm.pathPrefixes["/foo/bar"] = 1
	_, prefix, ok := pm.PathRoute("/foo/bar/baz")
	if !ok || prefix != "/foo/bar" {
		t.Fatalf("nested prefix=%q ok=%v, want /foo/bar", prefix, ok)
	}
}

func TestCleanUpstreamDotSegments(t *testing.T) {
	cases := map[string]string{
		"/emby/../admin":    "/admin",
		"/emby/./Items":     "/emby/Items",
		"/emby/Items":       "/emby/Items",
		"/emby/Items/":      "/emby/Items/",
		"/emby/../..":       "/",
		"/.well-known/acme": "/.well-known/acme",
		"/a/b/../c/./d":     "/a/c/d",
	}
	for input, want := range cases {
		if got := cleanUpstreamDotSegments(input); got != want {
			t.Fatalf("cleanUpstreamDotSegments(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPruneTrafficLogsBoundsHistoryAndDeletesOnSiteDelete(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSiteRecord(Site{Name: "traffic-prune", ListenPort: 19851, TargetURL: "http://127.0.0.1:8096", PlaybackMode: "direct", StreamHosts: "[]", UAMode: "passthrough"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	now := time.Now()
	if err := app.db.addTrafficWithRequests(site.ID, 1, 2, 3); err != nil {
		t.Fatalf("insert current traffic: %v", err)
	}
	old := now.Add(-401 * 24 * time.Hour)
	if _, err := app.db.db.Exec("INSERT INTO traffic_logs (site_id, bytes_in, bytes_out, requests, recorded_at, recorded_at_ms) VALUES (?,?,?,?,?,?)",
		site.ID, 5, 5, 5, old, old.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("INSERT INTO node_site_traffic_logs (node_id, site_id, bytes_in, bytes_out, requests, recorded_at_ms) VALUES (1,?,?,?,?,?)",
		site.ID, 5, 5, 5, old.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := app.db.pruneTrafficLogs(now); err != nil {
		t.Fatalf("prune: %v", err)
	}
	var oldRows, newRows, nodeRows int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM traffic_logs WHERE recorded_at_ms<?", now.Add(-300*24*time.Hour).UnixMilli()).Scan(&oldRows); err != nil {
		t.Fatal(err)
	}
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM traffic_logs WHERE recorded_at_ms>=?", now.Add(-300*24*time.Hour).UnixMilli()).Scan(&newRows); err != nil {
		t.Fatal(err)
	}
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM node_site_traffic_logs").Scan(&nodeRows); err != nil {
		t.Fatal(err)
	}
	if oldRows != 0 || newRows == 0 || nodeRows != 0 {
		t.Fatalf("prune left oldRows=%d newRows=%d nodeRows=%d, want 0/>0/0", oldRows, newRows, nodeRows)
	}
	// The daily guard must skip an immediate second sweep.
	if err := app.db.pruneTrafficLogs(now.Add(time.Minute)); err != nil {
		t.Fatalf("guarded prune: %v", err)
	}
	// Deleting the site must also clear the node traffic ledger.
	if _, err := app.db.db.Exec("INSERT INTO node_site_traffic_logs (node_id, site_id, bytes_in, bytes_out, requests, recorded_at_ms) VALUES (1,?,1,1,1,?)", site.ID, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := app.db.DeleteSite(site.ID); err != nil {
		t.Fatalf("delete site: %v", err)
	}
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM node_site_traffic_logs WHERE site_id=?", site.ID).Scan(&nodeRows); err != nil {
		t.Fatal(err)
	}
	if nodeRows != 0 {
		t.Fatalf("node traffic rows survived site deletion: %d", nodeRows)
	}
}

func TestExactAddressRecordsFollowsPagination(t *testing.T) {
	var pages int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.RawQuery, "page=2") {
			_, _ = w.Write([]byte(`{"success":true,"result_info":{"page":2,"total_pages":2},"result":[{"id":"r2","type":"A","name":"site.example","content":"203.0.113.2","comment":""}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"result_info":{"page":1,"total_pages":2},"result":[{"id":"r1","type":"A","name":"site.example","content":"203.0.113.1","comment":""}]}`))
	}))
	defer server.Close()
	cf := &cloudflareClient{token: "test", httpClient: server.Client(), apiBase: server.URL}
	records, err := cf.exactAddressRecords(context.Background(), "zone-1", "site.example")
	if err != nil {
		t.Fatalf("exactAddressRecords: %v", err)
	}
	if pages != 2 || len(records) != 2 || records[0].ID != "r1" || records[1].ID != "r2" {
		t.Fatalf("pages=%d records=%d, want both pages merged", pages, len(records))
	}
}

func TestSaveManagedPanelSettingsRejectsSitePortConflict(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSiteRecord(Site{Name: "port-site", ListenPort: 19555, IngressMode: ingressModePort, TargetURL: "http://127.0.0.1:8096", PlaybackMode: "direct", StreamHosts: "[]", UAMode: "passthrough"})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	if _, _, err := app.db.SaveManagedPanelSettings("panel.admin.example.test", "example.test", site.ListenPort, false); err == nil {
		t.Fatal("panel port collision with a dedicated-port site must be rejected")
	}
}

func TestDynamicAuthorityIdleEvictionReleasesCapacity(t *testing.T) {
	limits := DynamicProfileLimits{MaxAuthorities: 2, MaxNewAuthoritiesPerMinute: 100}
	runtime := newDynamicRuntime()
	state := newDynamicSiteState(runtime, limits)
	commit := func(authority string, at time.Time) {
		reservation, reason := state.reserveAuthority(authority, at)
		if reason != "" {
			t.Fatalf("reserve %s: %s", authority, reason)
		}
		reservation.commit()
	}
	now := time.Now()
	commit("one.example", now)
	commit("two.example", now)
	// At capacity with stale entries, a new authority must be admitted after
	// eviction instead of being permanently rejected.
	late := now.Add(dynamicAuthorityIdleTTL + time.Minute)
	commit("three.example", late)
	// Freshly used committed entries are never evicted: three.example is
	// within the TTL and must still reserve without recreating.
	if entry, reason := state.reserveAuthority("three.example", late.Add(time.Second)); reason != "" || entry == nil {
		t.Fatalf("fresh committed authority rejected after eviction sweep: %s", reason)
	}
}

func TestDynamicAuthorityGlobalSweepDoesNotDeadlockOrLeak(t *testing.T) {
	limits := DynamicProfileLimits{MaxAuthorities: 1, MaxNewAuthoritiesPerMinute: 1000}
	runtime := newDynamicRuntime()
	state := newDynamicSiteState(runtime, limits)
	other := newDynamicSiteState(runtime, limits)
	now := time.Now()
	// Keep the busy site at capacity with a fresh committed entry.
	reservation, reason := state.reserveAuthority("busy.example", now)
	if reason != "" {
		t.Fatalf("reserve busy: %s", reason)
	}
	reservation.commit()
	// Plant a stale committed entry on another site for the global sweep.
	staleAt := now.Add(-dynamicAuthorityIdleTTL - time.Minute)
	staleReservation, reason := other.reserveAuthority("stale.example", staleAt)
	if reason != "" {
		t.Fatalf("reserve stale: %s", reason)
	}
	staleReservation.commit()
	// Reserving a new authority on the busy site runs the global sweep while
	// holding this state's mu: the sweep must skip the caller (no self
	// deadlock), evict the other site's stale entry, and still deny with
	// capacity_limit because the caller's own table stays full.
	done := make(chan string, 1)
	go func() {
		_, sweepReason := state.reserveAuthority("new.example", now.Add(time.Second))
		done <- sweepReason
	}()
	select {
	case sweepReason := <-done:
		if sweepReason != dynamicObservationReasonCapacityLimit {
			t.Fatalf("reason=%q, want capacity_limit", sweepReason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reserveAuthority deadlocked during the global eviction sweep")
	}
	runtime.mu.Lock()
	other.mu.Lock()
	remaining := len(other.authorities)
	other.mu.Unlock()
	runtime.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("global sweep left %d stale entries on the other site", remaining)
	}
}

func TestNodeRequestEventPendingLedgerIsCapped(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	total := nodeRequestEventPendingLimit + 5
	for i := 0; i < total; i++ {
		if _, err := app.db.db.Exec("INSERT INTO node_request_events (node_id, agent_boot_id, event_id, received_at_ms) VALUES (7, 'boot', ?, ?)",
			int64(i), now.Add(-time.Duration(total-i)*time.Minute).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := app.db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := pruneRequestLogsTx(tx, now, 24*time.Hour); err != nil {
		t.Fatalf("prune: %v", err)
	}
	var pending int
	if err := tx.QueryRow("SELECT COUNT(*) FROM node_request_events WHERE processed_at_ms=0").Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending > nodeRequestEventPendingLimit {
		t.Fatalf("pending ledger rows=%d, want <= %d", pending, nodeRequestEventPendingLimit)
	}
}

func TestAgentConfigHashKeepsPreV1986SiteWireCompatibility(t *testing.T) {
	config := AgentRuntimeConfig{
		SchemaVersion: agentConfigSchemaVersion,
		NodeGUID:      "compat-node",
		HTTPSPort:     9090,
		DynamicKey:    testEdgeRuntimeKey(t),
		Routes: []AgentSiteRoute{{
			SiteID: 9, Host: "media.example.test", TargetURL: "https://origin.example.test",
			Site: Site{ID: 9, Name: "compat", PublicHost: "media.example.test", IngressMode: ingressModeHost},
		}},
	}
	if agentSupportsDynamicPolicyRemoval("v1.9.85") {
		t.Fatal("v1.9.85 must be treated as pre-removal")
	}
	if !agentSupportsDynamicPolicyRemoval("v1.9.86") || !agentSupportsDynamicPolicyRemoval("v2.0.0") {
		t.Fatal("removal gate must accept v1.9.86 and later")
	}
	legacyHash, err := agentConfigHashForVersion(config, "v1.9.85")
	if err != nil {
		t.Fatal(err)
	}
	modernHash, err := agentConfigHashForVersion(config, "v1.9.86")
	if err != nil {
		t.Fatal(err)
	}
	if legacyHash == modernHash {
		t.Fatal("legacy and modern site wire hashes must differ after field removal")
	}
	// The legacy payload must serialize the removed fields with zero values in
	// their original positions — that is what a v1.9.85 agent re-marshals.
	payload, err := json.Marshal(agentRuntimeConfigHashLegacy{Routes: []agentSiteRouteHashLegacy{{Site: agentSiteHashLegacy{}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"dynamic_discovery_enabled", "dynamic_profile",
		"dynamic_discovery_sources", "dynamic_domain_rules",
		"dynamic_allow_https_downgrade",
	} {
		if !strings.Contains(string(payload), key) {
			t.Fatalf("legacy hash payload lost key %q", key)
		}
	}
	// Round-trip conversion must not lose current fields.
	legacy, err := legacyAgentSiteHash(Site{ID: 9, Name: "compat", TrafficQuota: 123})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.ID != 9 || legacy.Name != "compat" || legacy.TrafficQuota != 123 || legacy.DynamicDiscoveryEnabled || legacy.DynamicProfile != "" {
		t.Fatalf("legacy site conversion = %#v", legacy)
	}
}

func TestQuotaLimitedWriterAbortsPastQuota(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSiteRecord(Site{Name: "quota-writer", ListenPort: 19861, TargetURL: "http://127.0.0.1:8096", PlaybackMode: "direct", StreamHosts: "[]", UAMode: "passthrough"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE sites SET traffic_quota=? WHERE id=?", 1024, site.ID); err != nil {
		t.Fatal(err)
	}
	inst := &ProxyInstance{Site: *site}
	pm := NewProxyManager(app.db, nil)
	// Seed the cached cycle usage above the quota so the first 16 MiB probe
	// aborts; the cycle boundary matches the current settings so the cache is
	// used instead of a fresh DB sum.
	inst.trafficCycleStart = time.Now().Add(-time.Hour)
	inst.trafficCycleMode = trafficBillingModeBidirectional
	inst.trafficCycleUsage = 4096
	recorder := httptest.NewRecorder()
	writer := &quotaLimitedWriter{
		meteredWriter: meteredWriter{ResponseWriter: recorder, written: inst.trafficBytesOut(), cumulative: inst.trafficCumulativeOut()},
		pm:            pm,
		inst:          inst,
		quota:         1024,
	}
	payload := make([]byte, quotaCheckBytes+1)
	_, err = writer.Write(payload)
	if !errors.Is(err, http.ErrAbortHandler) {
		t.Fatalf("write err=%v, want http.ErrAbortHandler", err)
	}
	if inst.trafficBytesOut().Load() != int64(len(payload)) {
		t.Fatalf("metered=%d, want %d", inst.trafficBytesOut().Load(), len(payload))
	}
}

func TestDynamicPreserveStripsStaleContentEncoding(t *testing.T) {
	resp := &http.Response{
		Header:     http.Header{},
		StatusCode: http.StatusOK,
	}
	resp.Header.Set("Content-Encoding", "gzip")
	resp.Header.Set("ETag", "\"v1\"")
	installDynamicStructuredBody(resp, []byte("#EXTM3U\n"), false)
	if got := resp.Header.Get("Content-Encoding"); got != "" {
		t.Fatalf("preserved response still carries Content-Encoding=%q", got)
	}
	if got := resp.Header.Get("ETag"); got == "" {
		t.Fatal("no-op preserve lost the ETag validator")
	}
	installDynamicStructuredBody(resp, []byte("#EXTM3U\n#rewritten\n"), true)
	if got := resp.Header.Get("ETag"); got != "" {
		t.Fatal("rewritten response kept a stale ETag validator")
	}
}

func TestHLSRewriteDenialsFailClosedUnderPreserveFallback(t *testing.T) {
	// Policy denials (userinfo, scheme, fragment) must remain hard errors even
	// though format-compatibility failures now preserve the upstream manifest.
	if newDynamicPolicyDenialError(fmt.Errorf("x")) == nil {
		t.Fatal("denial constructor returned nil")
	}
	wrapped := newDynamicPolicyDenialError(fmt.Errorf("discovered URL: userinfo"))
	var decoded *dynamicPolicyDenialError
	if !errors.As(error(wrapped), &decoded) {
		t.Fatal("errors.As does not unwrap the denial")
	}
	session := &dynamicRewriteSession{ctx: context.Background(), base: mustStructuredURL(t, "https://api.example.com/live/master.m3u8"), source: dynamicDiscoverySourceHLS}
	issuer := &dynamicCapabilityIssuer{key: make([]byte, 32), siteID: 1, policyRevision: 1, policy: dynamicRedirectPolicy{limits: dynamicDefaultProfileLimits()}, state: newDynamicSiteState(newDynamicRuntime(), dynamicDefaultProfileLimits())}
	session.issuer = issuer
	if _, err := rewriteHLSURIKind("http://user:pass@cdn.example.com/video.ts", session, dynamicCapabilityKindResource); err == nil {
		t.Fatal("userinfo URI was accepted")
	} else if !errors.As(err, &decoded) {
		t.Fatalf("userinfo denial not typed: %v", err)
	}
	if _, err := rewriteHLSURIKind("ftp://cdn.example.com/video.ts", session, dynamicCapabilityKindResource); err == nil || !errors.As(err, &decoded) {
		t.Fatalf("scheme denial not typed: %v", err)
	}
	if _, err := rewriteHLSURIKind("https://cdn.example.com/video.ts#frag", session, dynamicCapabilityKindResource); err == nil || !errors.As(err, &decoded) {
		t.Fatalf("fragment denial not typed: %v", err)
	}
}

func TestSaturatingAddInt64DoesNotWrap(t *testing.T) {
	if got := saturatingAddInt64(math.MaxInt64, 1); got != math.MaxInt64 {
		t.Fatalf("overflow wrapped: %d", got)
	}
	if got := saturatingAddInt64(math.MaxInt64-5, 10); got != math.MaxInt64 {
		t.Fatalf("partial overflow wrapped: %d", got)
	}
	if got := saturatingAddInt64(100, 200); got != 300 {
		t.Fatalf("normal add broken: %d", got)
	}
	if got := saturatingAddInt64(math.MinInt64, -1); got != math.MinInt64 {
		t.Fatalf("underflow wrapped: %d", got)
	}
}

func TestQuotaProbeSharedBoundedToOneCheck(t *testing.T) {
	inst := &ProxyInstance{}
	if quotaProbeShared(nil, 100) {
		t.Fatal("nil instance must not probe")
	}
	first := inst.quotaCheckSince.Add(quotaCheckBytes - 1)
	_ = first
	if !quotaProbeShared(inst, 1) {
		t.Fatal("crossing the threshold must claim the probe")
	}
	if inst.quotaCheckSince.Load() != 0 {
		t.Fatalf("probe did not reset the accumulator: %d", inst.quotaCheckSince.Load())
	}
	if quotaProbeShared(inst, 10) {
		t.Fatal("small follow-up must not probe")
	}
}
