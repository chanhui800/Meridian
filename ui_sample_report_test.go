package main

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestUISampleReportDump renders one complete report from the operator's own
// figures so the layout can be read the way it arrives on a phone. It is opt-in
// via UI_SAMPLE_REPORT=1 so the normal suite stays quiet.
func TestUISampleReportDump(t *testing.T) {
	if os.Getenv("UI_SAMPLE_REPORT") == "" {
		t.Skip("set UI_SAMPLE_REPORT=1 to print a sample report")
	}
	// formatTelegramBytes divides by 1024, so express the figures the way the
	// panel does: 1 GB is 10^9 bytes, which is about 0.93 GiB.
	const gb = int64(1_000_000_000)
	const mb = int64(1_000_000)
	peakStart := time.Date(2026, time.September, 14, 9, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60))
	stats := telegramReportStats{
		GeneratedAt:         time.Date(2026, time.September, 14, 12, 43, 0, 0, time.FixedZone("UTC+8", 8*60*60)),
		UniqueClients:       4,
		ActivePeak:          2,
		PeakStart:           peakStart,
		PeakEnd:             peakStart.Add(time.Hour),
		PeakRequests:        10,
		HasPeakWindow:       true,
		Requests:            115,
		VideoRequests:       2,
		SiteCount:           11,
		RunningSiteCount:    11,
		TodayTraffic:        int64(1.65 * float64(gb)),
		SevenDayTraffic:     int64(114.13 * float64(gb)),
		CycleTraffic:        227_010_587_698,
		CycleStart:          time.Date(2026, time.September, 15, 0, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60)),
		LifetimeTraffic:     171_280_000_000,
		BillingMode:         trafficBillingModeBidirectional,
		ControllerSiteCount: 9,
		TrafficWarnPercent:  80,
		TopTraffic: []telegramReportSiteStat{
			{Name: "1111", Requests: 36, Traffic: 1_640_000_000},
			{Name: "折纸", Requests: 29, Traffic: 3_240_000},
			{Name: "予初", Requests: 16, Traffic: 827_170},
			{Name: "茶百道", Requests: 16, Traffic: 37_440},
			{Name: "桃子", Requests: 16, Traffic: 25_470},
		},
		Nodes: []telegramReportNodeStat{
			// Live figures: rx + tx + the 80 GiB manual offset is 227.01 GB, the
			// same amount the panel renders as 211.42 GiB for this node.
			{Name: "9929", TodayTraffic: 1_640_000_000, CycleTraffic: 227_010_587_698, Remaining: 309_860_324_302, HasQuota: true, SiteCount: 2},
		},
		RetentionSites: []telegramReportRetentionStat{
			{Name: "墨云阁", RemainingDays: 23},
			{Name: "守候公益", RemainingDays: 24},
			{Name: "折纸", RemainingDays: 29},
			{Name: "茶百道", RemainingDays: 30},
		},
	}
	stats.TopUserAgents = append(stats.TopUserAgents,
		struct {
			Name  string
			Count int64
		}{Name: telegramReportClientName("CapyPlayer/1.1.5"), Count: 113},
		struct {
			Name  string
			Count int64
		}{Name: telegramReportClientName("Hills/1.9.0-beta.1 (android; 17)"), Count: 2},
	)
	message := buildTelegramReportMessage(stats)
	fmt.Printf("\n----8<---- report (%d bytes) ----8<----\n%s\n----8<---- end ----8<----\n", len(message), message)
	if len(message) > telegramReportMaxMessageBytes {
		t.Fatalf("sample report exceeds the Telegram limit: %d bytes", len(message))
	}
}
