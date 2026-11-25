package main

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func trafficCacheTestSite(t testing.TB, db *DB, id int64) {
	t.Helper()
	if _, err := db.db.Exec("INSERT INTO sites(id,name,listen_port,target_url) VALUES(?,?,?,?)", id, fmt.Sprintf("cache-site-%d", id), 19000+id, "http://127.0.0.1:8096"); err != nil {
		t.Fatal(err)
	}
}

func trafficCacheTestRead(t *testing.T, db *DB, start time.Time, mode string, want map[int64]int64) {
	t.Helper()
	got, err := db.SumTrafficSinceBySite(start, mode)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("persisted totals=%v err=%v, want %v", got, err, want)
	}
	var wantSum int64
	for _, value := range want {
		wantSum = saturatingAddInt64(wantSum, value)
	}
	if got, err := db.SumTrafficSince(start, mode); err != nil || got != wantSum {
		t.Fatalf("total=%d err=%v, want %d", got, err, wantSum)
	}
}

func TestTrafficTotalsCacheTracksCommittedRawWritesAndPreservesUnrelatedHits(t *testing.T) {
	db := openWatchHistoryTestDB(t)
	trafficCacheTestSite(t, db, 1)
	now := time.Now().Truncate(time.Minute)
	start := now.Add(-time.Hour)
	if err := db.addTrafficWithRequestsAt(1, 10, 20, 1, now); err != nil {
		t.Fatal(err)
	}
	trafficCacheTestRead(t, db, start, "outbound", map[int64]int64{1: 20})
	initial := db.trafficTotals
	if initial == nil {
		t.Fatal("authoritative fill did not become reusable")
	}
	// Returned maps are caller-owned, including callers that clear or mutate
	// the entire map before the next sampler tick.
	got, err := db.SumTrafficSinceBySite(start, "outbound")
	if err != nil {
		t.Fatal(err)
	}
	got[1], got[999] = 999, 1000
	delete(got, 1)
	trafficCacheTestRead(t, db, start, "outbound", map[int64]int64{1: 20})
	if db.trafficTotals != initial {
		t.Fatal("unchanged read rebuilt the month aggregate")
	}
	if _, err := db.writeRequestLogBatch([]queuedRequestLog{{event: requestLogEvent{SiteID: 1, Method: "GET", Path: "/api", StatusCode: 200, ResourceCategory: "api"}, recordedAtMS: now.UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("UPDATE sites SET name='renamed',media_movie_count=5 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err := db.addTrafficWithRequestsAt(1, 0, 0, 1, now); err != nil {
		t.Fatal(err)
	}
	trafficCacheTestRead(t, db, start, "outbound", map[int64]int64{1: 20})
	if db.trafficTotals != initial {
		t.Fatal("log, request-only checkpoint or unrelated site update rebuilt byte totals")
	}
	tx, err := db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE traffic_logs SET bytes_out=600 WHERE site_id=1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	trafficCacheTestRead(t, db, start, "outbound", map[int64]int64{1: 20})
	if db.trafficTotals != initial {
		t.Fatal("rollback invalidated committed totals")
	}
	if _, err := db.db.Exec("UPDATE traffic_logs SET bytes_out=30 WHERE site_id=1"); err != nil {
		t.Fatal(err)
	}
	trafficCacheTestRead(t, db, start, "outbound", map[int64]int64{1: 30})
	if db.trafficTotals == initial {
		t.Fatal("raw traffic update reused obsolete aggregate")
	}
	if _, err := db.db.Exec("INSERT INTO traffic_logs(site_id,bytes_in,bytes_out,recorded_at,recorded_at_ms) VALUES(1,3,7,?,?)", trafficMinuteBucket(now.Add(-10*time.Minute)), now.Add(-10*time.Minute).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	trafficCacheTestRead(t, db, start, "outbound", map[int64]int64{1: 37})
	if _, err := db.db.Exec("DELETE FROM traffic_logs WHERE site_id=1"); err != nil {
		t.Fatal(err)
	}
	trafficCacheTestRead(t, db, start, "outbound", map[int64]int64{})
}

func TestTrafficRequestTotalsCacheInvalidatesWhenTimestampsCrossRangeBoundary(t *testing.T) {
	db := openWatchHistoryTestDB(t)
	trafficCacheTestSite(t, db, 1)
	start := time.Now().Truncate(time.Minute)
	inside := start.Add(time.Minute).UnixMilli()
	outside := start.Add(-time.Minute).UnixMilli()
	if _, err := db.db.Exec(`INSERT INTO traffic_logs(site_id,bytes_in,bytes_out,requests,recorded_at,recorded_at_ms) VALUES(1,0,0,2,?,?)`, trafficMinuteBucket(time.UnixMilli(inside)), inside); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO node_site_traffic_logs(node_id,site_id,bytes_in,bytes_out,requests,recorded_at_ms) VALUES(7,1,0,0,3,?)`, inside); err != nil {
		t.Fatal(err)
	}
	read := func(want int64) {
		t.Helper()
		got, err := db.SumRequestsSince(start)
		if err != nil || got != want {
			t.Fatalf("request total=%d err=%v, want %d", got, err, want)
		}
	}
	read(5)
	if _, err := db.db.Exec(`UPDATE traffic_logs SET recorded_at_ms=?,recorded_at=? WHERE site_id=1`, outside, trafficMinuteBucket(time.UnixMilli(outside))); err != nil {
		t.Fatal(err)
	}
	read(3)
	if _, err := db.db.Exec(`UPDATE node_site_traffic_logs SET recorded_at_ms=? WHERE node_id=7 AND site_id=1`, outside); err != nil {
		t.Fatal(err)
	}
	read(0)
	if _, err := db.db.Exec(`UPDATE traffic_logs SET recorded_at_ms=?,recorded_at=? WHERE site_id=1`, inside, trafficMinuteBucket(time.UnixMilli(inside))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE node_site_traffic_logs SET recorded_at_ms=? WHERE node_id=7 AND site_id=1`, inside); err != nil {
		t.Fatal(err)
	}
	read(5)
}

func TestTrafficTotalsCacheTracksExternalCommitConnectionReplacementAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "traffic-cache.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { db.Close() }()
	trafficCacheTestSite(t, db, 1)
	now := time.Now().Truncate(time.Minute)
	start := now.Add(-time.Hour)
	if err := db.addTrafficWithRequestsAt(1, 5, 10, 1, now); err != nil {
		t.Fatal(err)
	}
	trafficCacheTestRead(t, db, start, "outbound", map[int64]int64{1: 10})
	epoch := db.trafficTotalsEpoch
	raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("UPDATE traffic_logs SET bytes_out=50"); err != nil {
		t.Fatal(err)
	}
	trafficCacheTestRead(t, db, start, "outbound", map[int64]int64{1: 50})
	if db.trafficTotalsEpoch != epoch {
		t.Fatal("external commit replaced a healthy local connection identity")
	}
	// Expire the only physical connection. TEMP state vanishes, but the Go
	// cache still exists and generation/data_version may repeat on the next one.
	db.db.SetMaxIdleConns(0)
	if _, err := raw.Exec("UPDATE traffic_logs SET bytes_out=70"); err != nil {
		t.Fatal(err)
	}
	db.db.SetMaxIdleConns(1)
	trafficCacheTestRead(t, db, start, "outbound", map[int64]int64{1: 70})
	if db.trafficTotalsEpoch == epoch {
		t.Fatal("replacement connection reused the previous random cache epoch")
	}
	// Main schema changes may remove TEMP triggers bound to a dropped table.
	// Detect them independently of the accounting generation and reinstall the
	// tracking set before caching again.
	epoch = db.trafficTotalsEpoch
	if _, err := db.db.Exec("CREATE INDEX traffic_cache_test_schema_change ON traffic_logs(bytes_out)"); err != nil {
		t.Fatal(err)
	}
	trafficCacheTestRead(t, db, start, "outbound", map[int64]int64{1: 70})
	if db.trafficTotalsEpoch == epoch {
		t.Fatal("schema change reused the previous tracking epoch")
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	trafficCacheTestRead(t, db, start, "outbound", map[int64]int64{1: 70})
}

func TestTrafficTotalsCacheBoundaryPolicyLegacyAndLifetime(t *testing.T) {
	originalLocal := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = originalLocal })
	db := openWatchHistoryTestDB(t)
	trafficCacheTestSite(t, db, 1)
	trafficCacheTestSite(t, db, 2)
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		at          string
		ms, in, out int64
	}{
		{"2026-10-31 23:59:00", start.Add(-time.Minute).UnixMilli(), 1, 100},
		{"2026-11-01 00:01:00", start.Add(time.Minute).UnixMilli(), 2, 20},
		{"2026-11-01 00:02:00", 0, 3, 30},
		{"2026-11-01T00:03:00Z", 0, 4, 40},
	} {
		if _, err := db.db.Exec("INSERT INTO traffic_logs(site_id,recorded_at,recorded_at_ms,bytes_in,bytes_out) VALUES(1,?,?,?,?)", row.at, row.ms, row.in, row.out); err != nil {
			t.Fatal(err)
		}
	}
	trafficCacheTestRead(t, db, start, "outbound", map[int64]int64{1: 90})
	trafficCacheTestRead(t, db, start, "bidirectional", map[int64]int64{1: 198})
	// Changing only the key's boundary cannot reuse a previous policy/cycle.
	trafficCacheTestRead(t, db, start.Add(-time.Hour), "outbound", map[int64]int64{1: 190})
	trafficCacheTestRead(t, db, start, "outbound", map[int64]int64{1: 90})
	// Preserve historical SQL's local-time text semantics without reinterpreting
	// ISO legacy records. Compare the authoritative scalar path in both zones.
	time.Local = time.FixedZone("UTC+08", 8*60*60)
	want, err := db.SumTrafficSinceForSite(1, start, "outbound")
	if err != nil {
		t.Fatal(err)
	}
	trafficCacheTestRead(t, db, start, "outbound", map[int64]int64{1: want})
	if _, err := db.db.Exec("UPDATE sites SET traffic_used=81 WHERE id=2"); err != nil {
		t.Fatal(err)
	}
	trafficCacheTestRead(t, db, time.Time{}, "outbound", map[int64]int64{1: 0, 2: 41})
	if _, err := db.db.Exec("UPDATE sites SET traffic_used=30,traffic_used_in=10,traffic_used_out=20 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	trafficCacheTestRead(t, db, time.Time{}, "bidirectional", map[int64]int64{1: 60, 2: 162})
	if err := db.DeleteSite(2); err != nil {
		t.Fatal(err)
	}
	trafficCacheTestRead(t, db, time.Time{}, "bidirectional", map[int64]int64{1: 60})
}

func TestTrafficTotalsCacheAgentBackfillAndRetiredReports(t *testing.T) {
	fixture := newTelemetryFixture(t)
	start := fixture.now.Add(-time.Minute)
	frames := []ledgerFrame{
		{1, 10, 20, 10, 20, 1, 1, false},
		{2, 4, 6, 14, 26, 2, 1, false},
		{3, 30, 50, 30, 50, 2, 2, false},
		{4, 4, 7, 18, 33, 3, 1, true},
		{4, 4, 7, 18, 33, 3, 1, true},
	}
	for i, frame := range frames {
		recordLedgerFrame(t, fixture, frame, time.Duration(i+1)*time.Second)
		want, err := fixture.app.db.SumTrafficSinceForSite(fixture.site.ID, start, "bidirectional")
		if err != nil {
			t.Fatal(err)
		}
		trafficCacheTestRead(t, fixture.app.db, start, "bidirectional", map[int64]int64{fixture.site.ID: want})
	}
	if _, err := fixture.app.db.db.Exec("DELETE FROM node_site_traffic_logs WHERE site_id=?", fixture.site.ID); err != nil {
		t.Fatal(err)
	}
	trafficCacheTestRead(t, fixture.app.db, start, "bidirectional", map[int64]int64{})
}

func TestTrafficTotalsCacheConcurrentReadersAndCommittedWriters(t *testing.T) {
	db := openWatchHistoryTestDB(t)
	trafficCacheTestSite(t, db, 1)
	now := time.Now().Truncate(time.Minute)
	start := now.Add(-time.Hour)
	var group sync.WaitGroup
	failures := make(chan error, 8)
	for reader := 0; reader < 4; reader++ {
		group.Go(func() {
			for i := 0; i < 40; i++ {
				got, err := db.SumTrafficSinceBySite(start, "outbound")
				if err != nil {
					failures <- err
					return
				}
				got[1] = -1 // caller mutations must be isolated across goroutines
			}
		})
	}
	group.Go(func() {
		for i := 0; i < 40; i++ {
			if err := db.addTrafficWithRequestsAt(1, 1, 2, 1, now); err != nil {
				failures <- err
				return
			}
		}
	})
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	trafficCacheTestRead(t, db, start, "outbound", map[int64]int64{1: 80})
}

func TestTrafficTotalsCacheReadOnlyFallback(t *testing.T) {
	db := openWatchHistoryTestDB(t)
	trafficCacheTestSite(t, db, 1)
	now := time.Now().Truncate(time.Minute)
	if err := db.addTrafficWithRequestsAt(1, 1, 2, 1, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.db.Exec("PRAGMA query_only=OFF") })
	trafficCacheTestRead(t, db, now.Add(-time.Hour), "outbound", map[int64]int64{1: 2})
	if db.trafficTotals != nil {
		t.Fatal("read-only fallback installed unverifiable cache state")
	}
}

func BenchmarkTrafficSnapshotRetainedMinuteHistory(b *testing.B) {
	db, err := openDB(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.Close)
	for site := int64(1); site <= 11; site++ {
		trafficCacheTestSite(b, db, site)
	}
	now := time.Now().Truncate(time.Minute)
	tx, err := db.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := tx.Prepare("INSERT INTO traffic_logs(site_id,bytes_in,bytes_out,requests,recorded_at,recorded_at_ms) VALUES(?,1024,1048576,2,?,?)")
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 475200; i++ {
		at := now.Add(-time.Duration(i/11) * time.Minute)
		if _, err := stmt.Exec(i%11+1, trafficMinuteBucket(at), at.UnixMilli()); err != nil {
			b.Fatal(err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	pm := NewProxyManager(db, nil)
	if _, err := pm.TrafficSnapshot(); err != nil {
		b.Fatal(err)
	}
	b.Run("unchanged", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := pm.TrafficSnapshot(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("request_log_write", func(b *testing.B) {
		batch := []queuedRequestLog{{event: requestLogEvent{SiteID: 1, Method: "GET", Path: "/api", ResourceCategory: "api", StatusCode: 200}, recordedAtMS: now.UnixMilli()}}
		b.ReportAllocs()
		for b.Loop() {
			if _, err := db.writeRequestLogBatch(batch); err != nil {
				b.Fatal(err)
			}
			if _, err := pm.TrafficSnapshot(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("committed_traffic_write", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := db.addTrafficWithRequestsAt(1, 1, 2, 1, now); err != nil {
				b.Fatal(err)
			}
			if _, err := pm.TrafficSnapshot(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestTrafficTotalsCacheSnapshotRefreshesAfterWriteAndPendingCounters(t *testing.T) {
	app := newTestApp(t)
	trafficCacheTestSite(t, app.db, 1)
	if err := app.db.addTrafficWithRequestsAt(1, 10, 20, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int64{60, 120} {
		snapshot, err := app.pm.TrafficSnapshot()
		if err != nil || snapshot.MonthlyTraffic != want {
			t.Fatalf("snapshot monthly=%v err=%v, want %d", snapshot, err, want)
		}
		if want == 60 {
			if err := app.db.addTrafficWithRequestsAt(1, 10, 20, 1, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The cache stores persisted usage only. A live local instance's pending
	// counters must remain visible on every snapshot without any SQL mutation.
	inst := &ProxyInstance{Site: Site{ID: 1}}
	app.pm.proxies[1] = inst
	inst.trafficBytesOut().Store(5)
	first, err := app.pm.TrafficSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	inst.trafficBytesOut().Store(9)
	second, err := app.pm.TrafficSnapshot()
	if err != nil || first.MonthlyTraffic != 130 || second.MonthlyTraffic != 138 {
		t.Fatalf("pending snapshots first=%v second=%v err=%v", first, second, err)
	}
	delete(app.pm.proxies, 1)
}
