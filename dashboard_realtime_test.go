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

func TestDashboardRealtimeRemoteRepeatedSampleKeepsLastRate(t *testing.T) {
	first := &TrafficSnapshot{BillingMode: "bidirectional", LiveSites: []SiteTraffic{{
		ID: 7, CumulativeBytesIn: 100, CumulativeBytesOut: 200, Requests: 4,
		AgentRuntime: true, AgentSampledAtMS: 1_000, AgentReceivedAtMS: 1_000,
	}}}
	_, previous := dashboardRealtimePointFromSnapshot(first, nil, 1_000)
	second := &TrafficSnapshot{BillingMode: "bidirectional", LiveSites: []SiteTraffic{{
		ID: 7, CumulativeBytesIn: 140, CumulativeBytesOut: 260, Requests: 5,
		AgentRuntime: true, AgentSampledAtMS: 3_000, AgentReceivedAtMS: 3_000,
		AgentDownloadBPS: 33, AgentUploadBPS: 22, AgentRateValid: true,
	}}}
	point, previous := dashboardRealtimePointFromSnapshot(second, previous, 3_000)
	if point.DownloadBPS != 33 || point.UploadBPS != 22 {
		t.Fatalf("remote initial rate=%v/%v, want Agent rate 33/22", point.DownloadBPS, point.UploadBPS)
	}
	repeated := &TrafficSnapshot{BillingMode: "bidirectional", LiveSites: []SiteTraffic{{
		ID: 7, CumulativeBytesIn: 140, CumulativeBytesOut: 260, Requests: 5,
		AgentRuntime: true, AgentSampledAtMS: 3_000, AgentReceivedAtMS: 3_000,
		AgentDownloadBPS: 33, AgentUploadBPS: 22, AgentRateValid: true,
	}}}
	point, _ = dashboardRealtimePointFromSnapshot(repeated, previous, 5_000)
	if point.BytesIn != 0 || point.BytesOut != 0 || point.DownloadBPS != 33 || point.UploadBPS != 22 || point.SpeedUnavailable {
		t.Fatalf("repeated remote sample=%+v, want zero deltas with held 33/22 rate", point)
	}
}

func TestDashboardRealtimeRemoteStaleSampleClearsRate(t *testing.T) {
	first := &TrafficSnapshot{BillingMode: "bidirectional", LiveSites: []SiteTraffic{{
		ID: 9, CumulativeBytesIn: 100, CumulativeBytesOut: 200, Requests: 4,
		AgentRuntime: true, AgentSampledAtMS: 1_000, AgentReceivedAtMS: 1_000,
	}}}
	_, previous := dashboardRealtimePointFromSnapshot(first, nil, 1_000)
	second := &TrafficSnapshot{BillingMode: "bidirectional", LiveSites: []SiteTraffic{{
		ID: 9, CumulativeBytesIn: 140, CumulativeBytesOut: 260, Requests: 5,
		AgentRuntime: true, AgentSampledAtMS: 3_000, AgentReceivedAtMS: 3_000,
		AgentDownloadBPS: 30, AgentUploadBPS: 20, AgentRateValid: true,
	}}}
	_, previous = dashboardRealtimePointFromSnapshot(second, previous, 3_000)
	stale := &TrafficSnapshot{BillingMode: "bidirectional", LiveSites: []SiteTraffic{{
		ID: 9, CumulativeBytesIn: 140, CumulativeBytesOut: 260, Requests: 5,
		AgentRuntime: true, AgentSampledAtMS: 3_000, AgentReceivedAtMS: 3_000,
		AgentDownloadBPS: 30, AgentUploadBPS: 20, AgentRateValid: true,
	}}}
	point, _ := dashboardRealtimePointFromSnapshot(stale, previous, 3_000+agentRealtimeRateHoldWindow.Milliseconds()+1)
	if point.DownloadBPS != 0 || point.UploadBPS != 0 || !point.SpeedUnavailable {
		t.Fatalf("stale remote sample=%+v, want unavailable zero rate", point)
	}
}

func TestDashboardRealtimeAgentTrafficBackfillPreservesCounters(t *testing.T) {
	previous := map[int64]dashboardRealtimeCounter{
		7: {BytesIn: 100, BytesOut: 200, Requests: 4, AgentRuntime: true, AgentSampledAtMS: 3_000, LastAgentAdvanceMS: 3_000},
	}
	points := []dashboardTrendPoint{
		{TimestampMS: 3_000, SiteContributions: map[int64]dashboardTrendPoint{7: {TimestampMS: 3_000}}},
		{TimestampMS: 5_000, SiteContributions: map[int64]dashboardTrendPoint{7: {TimestampMS: 5_000}}},
	}
	current := dashboardTrendPoint{
		TimestampMS: 7_000, BytesIn: 41, BytesOut: 61, Requests: 2, Traffic: 204,
		SiteContributions: map[int64]dashboardTrendPoint{7: {
			TimestampMS: 7_000, BytesIn: 41, BytesOut: 61, Requests: 2, Traffic: 204,
		}},
	}
	next := map[int64]dashboardRealtimeCounter{
		7: {BytesIn: 141, BytesOut: 261, Requests: 6, AgentRuntime: true, AgentSampledAtMS: 7_000, LastAgentAdvanceMS: 7_000},
	}

	backfillDashboardRealtimeAgentTraffic(points, &current, "bidirectional", previous, next)
	older := points[0].SiteContributions[7]
	filled := points[1].SiteContributions[7]
	latest := current.SiteContributions[7]
	if older.BytesIn != 0 || older.BytesOut != 0 {
		t.Fatalf("sample at the previous Agent watermark changed: %+v", older)
	}
	if filled.BytesIn != 21 || filled.BytesOut != 31 || latest.BytesIn != 20 || latest.BytesOut != 30 {
		t.Fatalf("backfill split=%+v/%+v, want 21/31 and 20/30", filled, latest)
	}
	if filled.BytesIn+latest.BytesIn != 41 || filled.BytesOut+latest.BytesOut != 61 {
		t.Fatalf("backfill lost counters: filled=%+v latest=%+v", filled, latest)
	}
	if filled.Requests != 0 || latest.Requests != 2 {
		t.Fatalf("request deltas were backfilled: filled=%d latest=%d", filled.Requests, latest.Requests)
	}
	if points[1].Traffic != 104 || current.Traffic != 100 {
		t.Fatalf("aggregate billing traffic=%d/%d, want 104/100", points[1].Traffic, current.Traffic)
	}
}

func TestDashboardRealtimeAgentTrafficDoesNotInventLongBackfill(t *testing.T) {
	previous := map[int64]dashboardRealtimeCounter{
		7: {AgentRuntime: true, AgentSampledAtMS: 1_000, LastAgentAdvanceMS: 1_000},
	}
	points := make([]dashboardTrendPoint, agentRealtimeBackfillMaxPoints)
	for index := range points {
		points[index] = dashboardTrendPoint{
			TimestampMS:       int64(index+2) * 2_000,
			SiteContributions: map[int64]dashboardTrendPoint{7: {TimestampMS: int64(index+2) * 2_000}},
		}
	}
	current := dashboardTrendPoint{
		TimestampMS: 20_000, BytesOut: 90, Traffic: 90,
		SiteContributions: map[int64]dashboardTrendPoint{7: {TimestampMS: 20_000, BytesOut: 90, Traffic: 90}},
	}
	next := map[int64]dashboardRealtimeCounter{
		7: {AgentRuntime: true, AgentSampledAtMS: 20_000, LastAgentAdvanceMS: 20_000},
	}

	backfillDashboardRealtimeAgentTraffic(points, &current, "outbound", previous, next)
	for index, point := range points {
		if got := point.SiteContributions[7].BytesOut; got != 0 {
			t.Fatalf("long gap point %d received invented backfill=%d", index, got)
		}
	}
	if current.BytesOut != 90 || current.SiteContributions[7].BytesOut != 90 {
		t.Fatalf("long gap current counters changed: %+v", current)
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
