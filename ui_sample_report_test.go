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
	const mb = int64(1) << 20
	stats := telegramReportStats{
		GeneratedAt:         time.Date(2026, time.September, 14, 11, 39, 0, 0, time.FixedZone("UTC+8", 8*60*60)),
		UniqueClients:       4,
		ActivePeak:          2,
		Requests:            115,
		VideoRequests:       2,
		SiteCount:           11,
		RunningSiteCount:    11,
		TodayTraffic:        1650 * mb,
		SevenDayTraffic:     114130 * mb,
		ThirtyDayTraffic:    171280 * mb,
		HistoryTraffic:      171280 * mb,
		BillingMode:         trafficBillingModeBidirectional,
		ControllerSiteCount: 9,
		TrafficWarnPercent:  80,
		TopTraffic: []telegramReportSiteStat{
			{Name: "1111", Requests: 20, Traffic: 1640 * mb},
			{Name: "折纸", Requests: 5, Traffic: 3240000},
			{Name: "予初", Requests: 3, Traffic: 827170},
			{Name: "茶百道", Requests: 2, Traffic: 37440},
			{Name: "桃子", Requests: 1, Traffic: 25470},
		},
		Nodes: []telegramReportNodeStat{
			{Name: "9929", TodayTraffic: 1640 * mb, CycleTraffic: 342750 * mb, Remaining: 157250 * mb, HasQuota: true, SiteCount: 2},
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
		}{Name: "CapyPlayer/1.1.5", Count: 113},
		struct {
			Name  string
			Count int64
		}{Name: "Hills/1.9.0-beta.1 (android; 17)", Count: 2},
	)
	message := buildTelegramReportMessage(stats)
	fmt.Printf("\n----8<---- report (%d bytes) ----8<----\n%s\n----8<---- end ----8<----\n", len(message), message)
	if len(message) > telegramReportMaxMessageBytes {
		t.Fatalf("sample report exceeds the Telegram limit: %d bytes", len(message))
	}
}
