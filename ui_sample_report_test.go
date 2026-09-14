package main

import (
	"fmt"
	"os"
	"testing"
	"time"
)

// TestUISampleReportDump renders one complete report so the format can be read
// the way the operator reads it. It is opt-in via UI_SAMPLE_REPORT=1 so the
// normal suite stays quiet.
func TestUISampleReportDump(t *testing.T) {
	if os.Getenv("UI_SAMPLE_REPORT") == "" {
		t.Skip("set UI_SAMPLE_REPORT=1 to print a sample report")
	}
	const gib = int64(1) << 30
	stats := telegramReportStats{
		GeneratedAt:        time.Date(2026, time.September, 14, 20, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60)),
		UniqueClients:      186,
		ActivePeak:         23,
		Requests:           48213,
		VideoRequests:      15402,
		SiteCount:          11,
		RunningSiteCount:   11,
		TodayTraffic:       96*gib + 512<<20,
		SevenDayTraffic:    640 * gib,
		ThirtyDayTraffic:   2 * 1024 * gib,
		HistoryTraffic:     3 * 1024 * gib,
		BillingMode:        trafficBillingModeBidirectional,
		ControllerSiteCount: 10,
		TrafficWarnPercent: 65,
		TopTraffic: []telegramReportSiteStat{
			{Name: "样例站点一", Requests: 15402, Traffic: 41*gib + 200<<20},
			{Name: "样例站点二", Requests: 9820, Traffic: 22 * gib},
			{Name: "样例站点三", Requests: 7411, Traffic: 14 * gib},
			{Name: "样例站点四", Requests: 5103, Traffic: 9 * gib},
			{Name: "样例站点五", Requests: 2201, Traffic: 4 * gib},
			{Name: "样例站点六", Requests: 900, Traffic: gib},
		},
		Nodes: []telegramReportNodeStat{
			{Name: "样例落地节点", TodayTraffic: 96*gib + 512<<20, CycleTraffic: 812 * gib, Remaining: 212 * gib, HasQuota: true, SiteCount: 1},
		},
		TrafficWarnings: []telegramReportTrafficWarning{
			{Kind: "node", Name: "样例落地节点", Used: 812 * gib, Limit: 1024 * gib, Remaining: 212 * gib},
			{Kind: "site", Name: "样例站点二", Used: 190 * gib, Limit: 200 * gib, Remaining: 10 * gib},
			{Kind: "site", Name: "样例站点四", Used: 200 * gib, Limit: 200 * gib, Remaining: 0},
		},
		RetentionSites: []telegramReportRetentionStat{
			{Name: "样例站点一", RemainingDays: 26},
			{Name: "样例站点二", RemainingDays: 5},
			{Name: "样例站点三", RemainingDays: 0},
			{Name: "样例站点四", CompletedToday: true, RemainingDays: 30},
		},
	}
	stats.TopUserAgents = append(stats.TopUserAgents,
		struct {
			Name  string
			Count int64
		}{Name: "Infuse/7.8.1", Count: 31200},
		struct {
			Name  string
			Count int64
		}{Name: "Emby for iOS/2.2.5", Count: 9040},
		struct {
			Name  string
			Count int64
		}{Name: "Fileball/1.0.8", Count: 4100},
	)
	message := buildTelegramReportMessage(stats)
	fmt.Printf("\n----8<---- report (%d bytes) ----8<----\n%s\n----8<---- end ----8<----\n", len(message), message)
	if len(message) > telegramReportMaxMessageBytes {
		t.Fatalf("sample report exceeds the Telegram limit: %d bytes", len(message))
	}
}
