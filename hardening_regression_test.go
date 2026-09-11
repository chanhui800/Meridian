package main

import (
	"path/filepath"
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
