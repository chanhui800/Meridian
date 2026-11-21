package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPerformanceIndexesUpgrade55AndPreserveRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema55.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	trafficCacheTestSite(t, db, 1)
	if _, err := db.db.Exec("INSERT INTO media_items(site_id,upstream_item_id,tmdb_type,tmdb_id,created_at_ms,updated_at_ms) VALUES(1,'item','movie',42,1,1)"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"DROP INDEX idx_media_items_tmdb", "DROP INDEX idx_node_request_events_pending", "PRAGMA user_version=55"} {
		if _, err := db.db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	db, err = openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, count int
	if err := db.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 56 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	if err := db.db.QueryRow("SELECT COUNT(*) FROM media_items WHERE tmdb_type='movie' AND tmdb_id=42").Scan(&count); err != nil || count != 1 {
		t.Fatalf("media rows=%d err=%v", count, err)
	}
	for _, tc := range []struct {
		index, query string
		args         []any
	}{
		{"idx_media_items_tmdb", `DELETE FROM tmdb_cache WHERE (expires_at_ms>0 AND expires_at_ms<?) OR NOT EXISTS (SELECT 1 FROM media_items mi WHERE mi.tmdb_type=tmdb_cache.tmdb_type AND mi.tmdb_id=tmdb_cache.tmdb_id)`, []any{time.Now().UnixMilli()}},
		{"idx_node_request_events_pending", `SELECT node_id,agent_boot_id,event_id FROM node_request_events WHERE processed_at_ms=0 ORDER BY received_at_ms DESC,node_id,agent_boot_id,event_id LIMIT -1 OFFSET ?`, []any{nodeRequestEventPendingLimit}},
	} {
		rows, err := db.db.Query("EXPLAIN QUERY PLAN "+tc.query, tc.args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var a, b, c int
			var detail string
			if err := rows.Scan(&a, &b, &c, &detail); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(detail)
			plan.WriteByte('\n')
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		if !strings.Contains(plan.String(), tc.index) {
			t.Fatalf("required index %s absent:\n%s", tc.index, plan.String())
		}
	}
	// Reopening/idempotent migration adds only indexes. TEMP traffic tracking
	// never appears in the persisted schema or backup's sqlite_master.
	if err := db.migrate(); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'meridian_traffic_totals%'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("persistent cache objects=%d err=%v", count, err)
	}
}

func BenchmarkPruneWatchHistoryMatchedMetadata(b *testing.B) {
	db, err := openDB(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.Close)
	trafficCacheTestSite(b, db, 1)
	now := time.Now().UnixMilli()
	tx, err := db.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= 5000; i++ {
		if _, err := tx.Exec("INSERT INTO media_items(id,site_id,upstream_item_id,tmdb_type,tmdb_id,created_at_ms,updated_at_ms,metadata_updated_at_ms) VALUES(?,1,?,'movie',?,?,?,?)", i, fmt.Sprintf("item-%d", i), i, now, now, now); err != nil {
			b.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO watch_sessions(site_id,media_item_id,session_hash,started_at_ms,last_seen_at_ms) VALUES(1,?,?,?,?)", i, fmt.Sprintf("session-%d", i), now, now); err != nil {
			b.Fatal(err)
		}
		if _, err := tx.Exec("INSERT INTO tmdb_cache(tmdb_type,tmdb_id,language,updated_at_ms,expires_at_ms) VALUES('movie',?,'zh-CN',?,?)", i, now, now+tmdbCacheLifetime.Milliseconds()); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := db.pruneWatchHistory(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteRequestLogBatchProcessedLedger(b *testing.B) {
	db, err := openDB(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(db.Close)
	trafficCacheTestSite(b, db, 1)
	now := time.Now().UnixMilli()
	tx, err := db.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO control_nodes(id,guid,name,address,created_at_ms,updated_at_ms) VALUES(1,'benchmark-node','node','127.0.0.1',?,?)", now, now); err != nil {
		b.Fatal(err)
	}
	for i := 1; i <= 50000; i++ {
		if _, err := tx.Exec("INSERT INTO node_request_events(node_id,agent_boot_id,event_id,received_at_ms,processed_at_ms) VALUES(1,'boot',?,?,?)", i, now-int64(i), now); err != nil {
			b.Fatal(err)
		}
	}
	for i := 1; i <= 20000; i++ {
		if _, err := tx.Exec("INSERT INTO request_logs(site_id,site_name,resource_category,status_code,client_ip,user_agent,method,path,recorded_at_ms,timeline_at_ms) VALUES(1,'site','api',200,'127.0.0.1','Client','GET','/api',?,?)", now-int64(i), now-int64(i)); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	batch := make([]queuedRequestLog, 50)
	for i := range batch {
		batch[i] = queuedRequestLog{event: requestLogEvent{SiteID: 1, Method: "GET", Path: "/api", ResourceCategory: "api", StatusCode: 200}, recordedAtMS: now}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := db.writeRequestLogBatch(batch); err != nil {
			b.Fatal(err)
		}
	}
}
