package main

import "testing"

func TestDashboardRealtimePointFromSnapshotUsesCounterDeltas(t *testing.T) {
	first := &TrafficSnapshot{
		BillingMode: "bidirectional",
		LiveSites: []SiteTraffic{{
			ID:                 7,
			CumulativeBytesIn:  100,
			CumulativeBytesOut: 200,
			Requests:           4,
		}},
	}
	point, previous := dashboardRealtimePointFromSnapshot(first, nil, 1_000)
	if point.BytesIn != 0 || point.BytesOut != 0 || point.Requests != 0 {
		t.Fatalf("first sample must establish a baseline, got %+v", point)
	}
	if got := point.SiteContributions[7].Traffic; got != 0 {
		t.Fatalf("first sample traffic=%d, want 0", got)
	}

	second := &TrafficSnapshot{
		BillingMode: "bidirectional",
		LiveSites: []SiteTraffic{{
			ID:                 7,
			CumulativeBytesIn:  140,
			CumulativeBytesOut: 260,
			Requests:           5,
		}},
	}
	point, next := dashboardRealtimePointFromSnapshot(second, previous, 3_000)
	if point.BytesIn != 40 || point.BytesOut != 60 || point.Requests != 1 {
		t.Fatalf("aggregate deltas=%d/%d/%d, want 40/60/1", point.BytesIn, point.BytesOut, point.Requests)
	}
	if point.DownloadBPS != 30 || point.UploadBPS != 20 {
		t.Fatalf("aggregate rates=%v/%v, want 30/20", point.DownloadBPS, point.UploadBPS)
	}
	if point.Traffic != 200 {
		t.Fatalf("aggregate traffic=%d, want 200", point.Traffic)
	}
	contribution, ok := point.SiteContributions[7]
	if !ok || contribution.BytesIn != 40 || contribution.BytesOut != 60 || contribution.Requests != 1 {
		t.Fatalf("site contribution=%+v, present=%v", contribution, ok)
	}
	if next[7].BytesIn != 140 || next[7].BytesOut != 260 || next[7].Requests != 5 {
		t.Fatalf("next baseline=%+v", next[7])
	}
}

func TestAppendDashboardRealtimePointBoundsAndReplacement(t *testing.T) {
	points := make([]dashboardTrendPoint, 0, dashboardRealtimeServerMaxPoints+10)
	for i := int64(0); i < dashboardRealtimeServerMaxPoints+10; i++ {
		points = appendDashboardRealtimePoint(points, dashboardTrendPoint{
			TimestampMS: i * dashboardRealtimeServerInterval.Milliseconds(),
			DownloadBPS: float64(i),
		})
	}
	if len(points) != dashboardRealtimeServerMaxPoints {
		t.Fatalf("point count=%d, want %d", len(points), dashboardRealtimeServerMaxPoints)
	}
	latest := points[len(points)-1].TimestampMS
	points = appendDashboardRealtimePoint(points, dashboardTrendPoint{TimestampMS: latest, DownloadBPS: 999})
	if len(points) != dashboardRealtimeServerMaxPoints || points[len(points)-1].DownloadBPS != 999 {
		t.Fatalf("duplicate timestamp was not replaced: len=%d latest=%+v", len(points), points[len(points)-1])
	}
}
