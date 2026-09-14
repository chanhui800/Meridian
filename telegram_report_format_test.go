package main

import (
	"context"
	"database/sql"
	"fmt"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The report format is a user-facing contract: the operator reads it on a phone
// and expects the agreed section order every day. These tests pin the section
// headers, the ranking markers, the deployment arithmetic, and the proximity
// warning so a refactor cannot silently drop or reorder a section.

func TestTelegramReportMessageCarriesEveryAgreedSection(t *testing.T) {
	stats := telegramReportStats{
		GeneratedAt:        time.Date(2026, time.September, 13, 20, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60)),
		UniqueClients:      42,
		ActivePeak:         7,
		Requests:           1234,
		VideoRequests:      567,
		SiteCount:          11,
		RunningSiteCount:   11,
		TodayTraffic:       3 << 30,
		SevenDayTraffic:    21 << 30,
		CycleTraffic:       90 << 30,
		CycleStart:         time.Date(2026, time.September, 15, 0, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60)),
		LifetimeTraffic:    400 << 30,
		BillingMode:        trafficBillingModeBidirectional,
		TrafficWarnPercent: 80,
		TopTraffic: []telegramReportSiteStat{
			{Name: "最热", Traffic: 5 << 30, Requests: 900},
			{Name: "次热", Traffic: 3 << 30, Requests: 800},
			{Name: "第三", Traffic: 2 << 30, Requests: 700},
			{Name: "第四", Traffic: 1 << 30, Requests: 600},
			{Name: "第五", Traffic: 1 << 20, Requests: 500},
			{Name: "第六不该出现", Traffic: 1 << 10, Requests: 400},
		},
		Nodes: []telegramReportNodeStat{{Name: "落地节点", TodayTraffic: 1 << 30, CycleTraffic: 10 << 30, Remaining: 90 << 30, HasQuota: true, SiteCount: 11}},
		RetentionSites: []telegramReportRetentionStat{
			{Name: "保号站", RemainingDays: 5},
		},
	}
	message := buildTelegramReportMessage(stats)
	for _, expected := range []string{
		"📊 Meridian 数据日报",
		"⏱️ 统计时间：2026-09-13 20:00",
		"✨ 今日概览",
		"📈 请求总数：1234 次",
		"🎬 视频请求：567 次",
		"🗂 媒体库站点：11 个（启用 11 个）",
		"🏆 今日最热媒体库：最热",
		"📍 服务器部署信息",
		"🧩 客户端分布",
		"🌐 流量统计",
		"🔥 今日站点热度 TOP 5",
		"🔔 保号提醒",
		"🚀 System Status：Operational",
	} {
		if !strings.Contains(message, expected) {
			t.Fatalf("report is missing %q:\n%s", expected, message)
		}
	}
	// 今日概览 is the headline block and carries no list bullets.
	overview := message[strings.Index(message, "✨ 今日概览"):strings.Index(message, "📍 服务器部署信息")]
	if strings.Contains(overview, "•") {
		t.Fatalf("今日概览 still carries a bullet:\n%s", overview)
	}
	// Lines the operator took out of the agreed layout must stay out.
	for _, removed := range []string{"独立访客", "所有站点统一通过", "由控制端直接承载", "个落地节点 · 服务中", "历史条数", "人/分钟"} {
		if strings.Contains(message, removed) {
			t.Fatalf("report still renders the removed %q line:\n%s", removed, message)
		}
	}
	// Section order is part of the contract, not just presence.
	order := []string{"✨ 今日概览", "📍 服务器部署信息", "🧩 客户端分布", "🌐 流量统计", "🔥 今日站点热度 TOP 5", "🔔 保号提醒", "🚀 System Status"}
	position := -1
	for _, header := range order {
		index := strings.Index(message, header)
		if index <= position {
			t.Fatalf("section %q is out of order:\n%s", header, message)
		}
		position = index
	}
	// The top library is ranked by traffic, so the crown marks the heaviest site
	// and the keycap digits carry the remaining ranks. Each line leads with the
	// traffic figure and follows with the request count.
	if !strings.Contains(message, "👑 最热：5.00 GB 丨 900 次请求") ||
		!strings.Contains(message, "🌟 次热：3.00 GB 丨 800 次请求") ||
		!strings.Contains(message, "3\ufe0f\u20e3 第三：2.00 GB 丨 700 次请求") {
		t.Fatalf("ranking lines are wrong:\n%s", message)
	}
	for index := 3; index <= 5; index++ {
		want := fmt.Sprintf("%d\ufe0f\u20e3", index)
		if !strings.Contains(message, want) {
			t.Fatalf("rank %d is missing its keycap marker %q:\n%s", index, want, message)
		}
	}
	if strings.Contains(message, "No.3") || strings.Contains(message, "\n3. ") {
		t.Fatalf("a bare or spelled-out rank prefix survived:\n%s", message)
	}
	// The list is capped at five even when more sites have traffic.
	if strings.Contains(message, "第六不该出现") {
		t.Fatalf("the heat list rendered more than five rows:\n%s", message)
	}
}

func TestTelegramReportRankPrefixUsesAskedMarkers(t *testing.T) {
	// The exact markers the operator asked for, byte for byte. The keycap digits
	// are the ones that must not drift back to a bare "3.".
	want := []string{"👑", "🌟", "3\ufe0f\u20e3", "4\ufe0f\u20e3", "5\ufe0f\u20e3"}
	for index, expected := range want {
		if got := telegramReportRankPrefix(index); got != expected {
			t.Fatalf("rank %d prefix = %q, want %q", index+1, got, expected)
		}
	}
	if got := telegramReportRankPrefix(0); got == telegramReportRankPrefix(2) {
		t.Fatalf("the top rank and the third rank share the marker %q", got)
	}
}

func TestTelegramReportDeploymentSectionListsNodes(t *testing.T) {
	message := buildTelegramReportMessage(telegramReportStats{
		GeneratedAt:        time.Now(),
		TrafficWarnPercent: telegramReportTrafficWarnDisableValue,
		Nodes:              []telegramReportNodeStat{{Name: "唯一节点", TodayTraffic: 1 << 30, CycleTraffic: 2 << 30, Remaining: 8 << 30, HasQuota: true, SiteCount: 11}},
	})
	if !strings.Contains(message, "• 唯一节点（11 个站点）：今日 1.00 GB 丨 当月 2.00 GB 丨 剩余 8.00 GB") {
		t.Fatalf("node line is wrong:\n%s", message)
	}
	// The deployment section is the node list and nothing else.
	if strings.Contains(message, "所有站点统一通过") || strings.Contains(message, "个落地节点") {
		t.Fatalf("deployment section carries a summary line the operator removed:\n%s", message)
	}
	// An unmetered node must say so instead of printing a bogus 0 B remaining.
	unmetered := buildTelegramReportMessage(telegramReportStats{
		GeneratedAt: time.Now(),
		Nodes:       []telegramReportNodeStat{{Name: "无限额节点", SiteCount: 1}},
	})
	if !strings.Contains(unmetered, "剩余 未设置额度") {
		t.Fatalf("unmetered node rendered a fake remaining figure:\n%s", unmetered)
	}
	if strings.Contains(unmetered, "⚠️ 流量预警") {
		t.Fatalf("unmetered node produced a proximity warning:\n%s", unmetered)
	}
}

func TestTelegramReportProximityWarningHonoursThresholdAndDisableValue(t *testing.T) {
	const gib = int64(1) << 30
	// The renderer consumes the pre-charged warning list; the charging itself is
	// covered by the stats tests below and the pure helper at the end of this
	// test. These cases pin what the reader sees for each usage ratio.
	base := telegramReportStats{
		GeneratedAt:        time.Now(),
		TrafficWarnPercent: 80,
		TrafficWarnings: []telegramReportTrafficWarning{
			{Kind: "node", Name: "接近限额", Used: 85 * gib, Limit: 100 * gib, Remaining: 15 * gib},
		},
	}
	message := buildTelegramReportMessage(base)
	if !strings.Contains(message, "⚠️ 流量预警（已达 80%）") {
		t.Fatalf("warning section missing at the configured threshold:\n%s", message)
	}
	if !strings.Contains(message, "🟠 节点 接近限额：已用 85.0%（剩余 15.00 GB）") {
		t.Fatalf("warning line is wrong:\n%s", message)
	}

	// 0 is the operator's explicit "never warn" choice. The builder is handed an
	// empty list in that case because the stats pass filters on the threshold,
	// so the section must also stay absent when the list is empty.
	disabled := base
	disabled.TrafficWarnPercent = telegramReportTrafficWarnDisableValue
	disabled.TrafficWarnings = nil
	if message := buildTelegramReportMessage(disabled); strings.Contains(message, "⚠️ 流量预警") {
		t.Fatalf("disabled warnings still produced a section:\n%s", message)
	}

	// A fully consumed quota turns red, and a site is labelled as a site.
	over := base
	over.TrafficWarnings = []telegramReportTrafficWarning{{Kind: "site", Name: "已超限", Used: 20 * gib, Limit: 20 * gib, Remaining: 0}}
	if message := buildTelegramReportMessage(over); !strings.Contains(message, "🔴 站点 已超限：已用 100.0%") {
		t.Fatalf("over-limit quota did not turn red:\n%s", message)
	}

	// The threshold comparison itself: exact equality must warn, because the
	// operator picked that line on purpose, and everything below it must not.
	if _, ok := telegramReportProximityWarning("node", "刚好到线", 80, 20, 80); !ok {
		t.Fatal("usage exactly at the threshold did not warn")
	}
	if _, ok := telegramReportProximityWarning("node", "差一点", 79, 21, 80); ok {
		t.Fatal("usage below the threshold warned")
	}
	if _, ok := telegramReportProximityWarning("node", "零额度", 0, 0, 80); ok {
		t.Fatal("an unmetered quota produced a warning")
	}
	if _, ok := telegramReportProximityWarning("node", "关闭预警", 100, 0, telegramReportTrafficWarnDisableValue); ok {
		t.Fatal("a disabled threshold produced a warning")
	}
	// A large quota must not overflow the comparison: 640 PiB at 90%.
	const pib = int64(1) << 50
	if _, ok := telegramReportProximityWarning("node", "超大额度", 640*pib, 64*pib, 80); !ok {
		t.Fatal("a very large quota did not warn at 90%")
	}
}

// TestTelegramReportProximityWarningSurvivesQuotaOverflow checks the comparison
// against an exact big-integer oracle at quotas above the point where the
// obvious `used*100` form wraps int64. Those wrappings produced a negative
// product, so a nearly full quota was reported as nearly empty.
func TestTelegramReportProximityWarningSurvivesQuotaOverflow(t *testing.T) {
	// limit*100 exceeds MaxInt64 once the quota passes ~92 PB (2^56.4).
	// Disagreement with the int64 form is not guaranteed on every input, because
	// two wrapped products can still compare the same way, so it is tracked
	// across the whole matrix and required at least once.
	witnessedDisagreement := false
	for _, limit := range []int64{1 << 57, 1 << 58, 1 << 62, (1 << 63) - 1, int64(1)<<60 + 7919} {
		bigLimit := big.NewInt(limit)
		for _, percent := range []int{1, 20, 50, 80, 99, 100} {
			// The smallest whole number of bytes that reaches the threshold,
			// computed without truncation: ceil(limit * percent / 100).
			exact := new(big.Int).Mul(bigLimit, big.NewInt(int64(percent)))
			exact.Add(exact, big.NewInt(99))
			exact.Div(exact, big.NewInt(100))
			if !exact.IsInt64() {
				t.Fatalf("test setup: %d%% of %d exceeds int64", percent, limit)
			}
			used := exact.Int64()

			warning, ok := telegramReportProximityWarning("node", "超大额度", used, limit-used, percent)
			if !ok {
				t.Fatalf("quota %d with %d%% used did not warn", limit, percent)
			}
			if warning.Limit != limit || warning.Used != used {
				t.Fatalf("warning = %+v, want used %d limit %d", warning, used, limit)
			}
			// One byte below the line must stay quiet, which also proves the
			// comparison does not simply saturate to "always warn".
			if _, ok := telegramReportProximityWarning("node", "超大额度", used-1, limit-used+1, percent); ok {
				t.Fatalf("quota %d one byte below %d%% warned", limit, percent)
			}

			oldForm := used*100 >= limit*int64(percent)
			newForm := telegramReportRatioAtLeastPercent(used, limit, percent)
			if oldForm != newForm {
				if !newForm {
					t.Fatalf("quota %d at %d%%: the two forms disagreed and the new one was wrong", limit, percent)
				}
				witnessedDisagreement = true
			}
		}
	}
	if !witnessedDisagreement {
		t.Fatal("no input in this matrix distinguishes the 128-bit comparison from the int64 one")
	}
	if telegramReportRatioAtLeastPercent(0, 1<<62, 1) {
		t.Fatal("zero usage warned at 1%")
	}
}

func TestTelegramReportStatsCountLandingNodeUsage(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().In(time.Local)
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "report-node", Address: "203.0.113.77", Port: 9443,
		TrafficQuota: 1000, BillingMode: trafficBillingModeBidirectional, ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatalf("CreateControlNode: %v", err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatalf("EnrollControlNode: %v", err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "report-site", PublicHost: "report-site.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:18080"})
	if err != nil {
		t.Fatalf("CreateSiteRecord: %v", err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatalf("SaveSiteNodeSchedule: %v", err)
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=?,dns_status='active' WHERE site_id=?`, node.ID, node.ID, site.ID); err != nil {
		t.Fatalf("apply schedule: %v", err)
	}
	// A bidirectional node's period counters already carry the Agent's charge,
	// so the report sums them exactly as the panel does: rx + tx + offset. The
	// panel would render 1000 here; the doubling convention would print 1900.
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET enabled=1, period_rx_bytes=?, period_tx_bytes=?, traffic_manual_offset_bytes=? WHERE id=?`,
		int64(500), int64(400), int64(100), node.ID); err != nil {
		t.Fatalf("seed node counters: %v", err)
	}
	if _, err := app.db.db.Exec(`INSERT INTO node_site_traffic_logs(node_id,site_id,bytes_in,bytes_out,requests,recorded_at_ms) VALUES(?,?,?,?,?,?)`,
		node.ID, site.ID, 60, 40, 3, now.UnixMilli()); err != nil {
		t.Fatalf("seed node traffic: %v", err)
	}
	_ = token

	stats, err := app.db.buildTelegramReportStats(now)
	if err != nil {
		t.Fatalf("buildTelegramReportStats: %v", err)
	}
	if len(stats.Nodes) != 1 {
		t.Fatalf("landing nodes = %+v, want exactly one", stats.Nodes)
	}
	got := stats.Nodes[0]
	if got.Name != "report-node" || got.SiteCount != 1 {
		t.Fatalf("node identity/site count = %+v", got)
	}
	// Today's traffic uses the panel-wide billing mode (bidirectional by default).
	if got.TodayTraffic != 200 {
		t.Fatalf("node today traffic = %d, want 200", got.TodayTraffic)
	}
	if got.CycleTraffic != 1000 || got.Remaining != 0 || !got.HasQuota {
		t.Fatalf("node cycle = %+v, want used 1000 remaining 0", got)
	}
	// A fully consumed quota must warn, so the double-charge cannot hide here.
	if len(stats.TrafficWarnings) != 1 || stats.TrafficWarnings[0].Kind != "node" || stats.TrafficWarnings[0].Used != 1000 {
		t.Fatalf("warnings = %+v, want one node warning with used 1000", stats.TrafficWarnings)
	}
}

// TestTelegramReportNodeCycleMatchesPanelFormula pins the node arithmetic the
// operator compared against the panel. A node's period counters already carry
// the Agent's own billing charge, so the report sums them the way the panel
// does: bidirectional adds both directions, outbound takes transmit only, and
// neither doubles them.
//
// This is the regression that made the report disagree with the panel: routing
// these counters through trafficBillableBytes applied the panel-wide doubling
// convention a second time, so a node the panel showed at 211 GiB was reported
// at 343 GB.
func TestTelegramReportNodeCycleMatchesPanelFormula(t *testing.T) {
	for _, testCase := range []struct {
		mode     string
		rx, tx   int64
		expected int64
	}{
		{trafficBillingModeBidirectional, 500, 400, 900},
		{trafficBillingModeOutbound, 500, 400, 400},
		{"", 500, 400, 900},
		{trafficBillingModeBidirectional, 0, 0, 0},
	} {
		got := telegramReportNodeChargedBytes(testCase.mode, testCase.rx, testCase.tx)
		if got != testCase.expected {
			t.Fatalf("node charge for mode %q rx=%d tx=%d = %d, want %d", testCase.mode, testCase.rx, testCase.tx, got, testCase.expected)
		}
		// The panel-wide conversion doubles these same numbers in bidirectional
		// mode, where the double-charge actually occurred. Outbound mode charges
		// transmit only under both formulas, so only the summing mode can
		// distinguish them.
		if testCase.mode != trafficBillingModeOutbound && testCase.rx+testCase.tx > 0 {
			doubled := trafficBillableBytes(trafficBillingModeBidirectional, testCase.rx, testCase.tx)
			if doubled == got {
				t.Fatalf("node charge coincides with the doubling convention for mode %q; the double-charge is back", testCase.mode)
			}
		}
	}
}

// TestTelegramReportNodeCycleMatchesALiveNode reproduces the live figures: 67.38
// GiB in, 64.04 GiB out and an 80 GiB manual offset on a bidirectional node. The
// panel charges rx+tx+offset = 211.42 GiB; the report must print that rather
// than the doubled 342.84 GiB.
func TestTelegramReportNodeCycleMatchesALiveNode(t *testing.T) {
	const (
		rx     = int64(72_344_430_181)
		tx     = int64(68_766_811_597)
		offset = int64(85_899_345_920)
	)
	total := saturatingAddInt64(telegramReportNodeChargedBytes(trafficBillingModeBidirectional, rx, tx), offset)
	if total != 227_010_587_698 {
		t.Fatalf("node cycle = %d, want 227010587698 (211.42 GiB)", total)
	}
	doubled := saturatingAddInt64(trafficBillableBytes(trafficBillingModeBidirectional, rx, tx), offset)
	if doubled == total {
		t.Fatal("the doubling convention produced the same figure; this case no longer distinguishes them")
	}
	if doubled != 368_121_829_476 {
		t.Fatalf("doubling convention = %d, want 368121829476; the fixture drifted", doubled)
	}
}

func TestTelegramReportStatsWarnOnSiteQuotaProximity(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().In(time.Local)
	site, err := app.db.CreateSite("quota-site", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if _, err := app.db.db.Exec(`UPDATE sites SET traffic_quota=? WHERE id=?`, int64(1000), site.ID); err != nil {
		t.Fatalf("set site quota: %v", err)
	}
	if err := app.db.addTrafficWithRequestsAt(site.ID, 100, 350, 1, now); err != nil {
		t.Fatalf("seed site traffic: %v", err)
	}

	// Bidirectional billing charges (100+350)*2 = 900 of a 1000 quota: 90%.
	stats, err := app.db.buildTelegramReportStats(now)
	if err != nil {
		t.Fatalf("buildTelegramReportStats: %v", err)
	}
	if len(stats.TrafficWarnings) != 1 {
		t.Fatalf("warnings = %+v, want one site warning", stats.TrafficWarnings)
	}
	warning := stats.TrafficWarnings[0]
	if warning.Kind != "site" || warning.Name != "quota-site" || warning.Used != 900 || warning.Limit != 1000 || warning.Remaining != 100 {
		t.Fatalf("site warning = %+v, want site quota-site used 900 limit 1000 remaining 100", warning)
	}

	// Disabling the threshold must suppress it even though usage is unchanged.
	stored, err := app.db.telegramReportSettings()
	if err != nil {
		t.Fatalf("read telegram settings: %v", err)
	}
	settings := stored.TelegramReportSettings
	settings.TrafficWarningPercent = telegramReportTrafficWarnDisableValue
	if err := app.db.saveTelegramReportSettings(settings, stored.BotTokenCiphertext, false); err != nil {
		t.Fatalf("disable warnings: %v", err)
	}
	stats, err = app.db.buildTelegramReportStats(now)
	if err != nil {
		t.Fatalf("rebuild stats: %v", err)
	}
	if len(stats.TrafficWarnings) != 0 {
		t.Fatalf("warnings = %+v, want none when disabled", stats.TrafficWarnings)
	}

	// A higher threshold must also suppress it, which proves the stored value
	// drives the comparison instead of a hard-coded 80%.
	settings = stored.TelegramReportSettings
	settings.TrafficWarningPercent = 95
	if err := app.db.saveTelegramReportSettings(settings, stored.BotTokenCiphertext, false); err != nil {
		t.Fatalf("raise threshold: %v", err)
	}
	stats, err = app.db.buildTelegramReportStats(now)
	if err != nil {
		t.Fatalf("rebuild stats at 95%%: %v", err)
	}
	if len(stats.TrafficWarnings) != 0 {
		t.Fatalf("warnings = %+v, want none at a 95%% threshold", stats.TrafficWarnings)
	}
}

func TestTelegramReportTrafficWarningPercentRoundTrip(t *testing.T) {
	app := newTestApp(t)
	stored, err := app.db.telegramReportSettings()
	if err != nil {
		t.Fatalf("read defaults: %v", err)
	}
	if stored.TrafficWarningPercent != telegramReportDefaultTrafficWarnPct {
		t.Fatalf("default threshold = %d, want %d", stored.TrafficWarningPercent, telegramReportDefaultTrafficWarnPct)
	}

	ciphertext, err := encryptTelegramBotToken("123456:example-token")
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}
	settings := stored.TelegramReportSettings
	settings.Enabled = true
	settings.ChatID = "123"
	settings.TrafficWarningPercent = 55
	if err := app.db.saveTelegramReportSettings(settings, ciphertext, true); err != nil {
		t.Fatalf("save threshold: %v", err)
	}
	reloaded, err := app.db.telegramReportSettings()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.TrafficWarningPercent != 55 {
		t.Fatalf("threshold round trip = %d, want 55", reloaded.TrafficWarningPercent)
	}
	if got := app.db.telegramReportWarningPercent(); got != 55 {
		t.Fatalf("warning percent = %d, want 55", got)
	}

	// 0 must survive as the explicit opt-out instead of snapping back to 80.
	settings = reloaded.TelegramReportSettings
	settings.TrafficWarningPercent = telegramReportTrafficWarnDisableValue
	if err := app.db.saveTelegramReportSettings(settings, ciphertext, false); err != nil {
		t.Fatalf("save disabled threshold: %v", err)
	}
	reloaded, err = app.db.telegramReportSettings()
	if err != nil {
		t.Fatalf("reload disabled: %v", err)
	}
	if reloaded.TrafficWarningPercent != telegramReportTrafficWarnDisableValue {
		t.Fatalf("disabled threshold = %d, want 0", reloaded.TrafficWarningPercent)
	}
	if got := app.db.telegramReportWarningPercent(); got != telegramReportTrafficWarnDisableValue {
		t.Fatalf("warning percent after disable = %d, want 0", got)
	}
}

func TestTelegramReportTrafficWarningPercentValidation(t *testing.T) {
	for _, value := range []int{-1, 101, 1000} {
		settings := TelegramReportSettings{ScheduleTime: "20:00", Frequency: "daily", Weekday: 1, TrafficWarningPercent: value}
		if _, err := normalizeTelegramReportSettings(settings); err == nil {
			t.Fatalf("threshold %d was accepted", value)
		}
	}
	for _, value := range []int{0, 1, 55, 100} {
		settings := TelegramReportSettings{ScheduleTime: "20:00", Frequency: "daily", Weekday: 1, TrafficWarningPercent: value}
		normalized, err := normalizeTelegramReportSettings(settings)
		if err != nil {
			t.Fatalf("threshold %d was rejected: %v", value, err)
		}
		if normalized.TrafficWarningPercent != value {
			t.Fatalf("threshold %d normalized to %d", value, normalized.TrafficWarningPercent)
		}
	}
}

// TestTelegramReportThresholdMigrationBackfillsDefault proves the upgrade path
// for an already-deployed panel: the column is added with the product default,
// not the 0 sentinel, so an existing installation keeps warning without the
// operator touching the new field.
//
// It opens a bare connection rather than going through openDB: openDB starts the
// telemetry writers, and on Windows those hold the pool in a way that makes any
// later DDL against the same file wait forever.
func TestTelegramReportThresholdMigrationBackfillsDefault(t *testing.T) {
	raw, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy-telegram.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	raw.SetMaxOpenConns(1)
	defer raw.Close()

	// The pre-migration table shape: same columns, no threshold.
	if _, err := raw.Exec(`CREATE TABLE telegram_report_settings (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		enabled INTEGER NOT NULL DEFAULT 0,
		bot_token_ciphertext TEXT NOT NULL DEFAULT '',
		chat_id TEXT NOT NULL DEFAULT '',
		schedule_time TEXT NOT NULL DEFAULT '20:00',
		frequency TEXT NOT NULL DEFAULT 'daily',
		weekday INTEGER NOT NULL DEFAULT 1,
		last_sent_key TEXT NOT NULL DEFAULT '',
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO telegram_report_settings (id, chat_id) VALUES (1, '123')`); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	conn, err := raw.Conn(context.Background())
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer conn.Close()
	if err := ensureTelegramReportSettingsSchema(context.Background(), conn); err != nil {
		t.Fatalf("ensureTelegramReportSettingsSchema: %v", err)
	}
	var backfilled int
	// Read through the checked-out connection: with MaxOpenConns(1) a pool-level
	// query would wait for the connection this test is already holding.
	if err := conn.QueryRowContext(context.Background(), `SELECT traffic_warning_percent FROM telegram_report_settings WHERE id=1`).Scan(&backfilled); err != nil {
		t.Fatalf("read backfilled threshold: %v", err)
	}
	if backfilled != telegramReportDefaultTrafficWarnPct {
		t.Fatalf("backfilled threshold = %d, want %d", backfilled, telegramReportDefaultTrafficWarnPct)
	}
	// Re-running it must be a no-op rather than an error.
	if err := ensureTelegramReportSettingsSchema(context.Background(), conn); err != nil {
		t.Fatalf("second ensure failed: %v", err)
	}
}

func TestTelegramReportClientNameDropsVersionAndPlatform(t *testing.T) {
	for _, testCase := range []struct{ raw, want string }{
		{"CapyPlayer/1.1.5", "CapyPlayer"},
		{"Hills/1.9.0-beta.1 (android; 17)", "Hills"},
		{"Hills/1.9", "Hills"},
		{"Infuse/7.8.1", "Infuse"},
		{"Emby for iOS/2.2.5", "Emby"},
		{"VLC/3.0.20 LibVLC/3.0.20", "VLC"},
		{"  Padded/1.0  ", "Padded"},
		{"Bare", "Bare"},
		{"", ""},
		{"(anonymous)", ""},
	} {
		if got := telegramReportClientName(testCase.raw); got != testCase.want {
			t.Fatalf("client name for %q = %q, want %q", testCase.raw, got, testCase.want)
		}
	}
}

func TestTelegramReportClientAggregationFoldsVersions(t *testing.T) {
	rows := []struct {
		Name  string
		Count int64
	}{
		{Name: "CapyPlayer", Count: 60},
		{Name: "CapyPlayer", Count: 40},
		{Name: "Infuse", Count: 70},
		{Name: "Emby", Count: 5},
	}
	merged := aggregateTelegramReportClients(rows, 2)
	if len(merged) != 2 {
		t.Fatalf("merged rows = %+v, want 2", merged)
	}
	// The two CapyPlayer versions must outrank the single Infuse row.
	if merged[0].Name != "CapyPlayer" || merged[0].Count != 100 {
		t.Fatalf("top client = %+v, want CapyPlayer with 100", merged[0])
	}
	if merged[1].Name != "Infuse" || merged[1].Count != 70 {
		t.Fatalf("second client = %+v, want Infuse with 70", merged[1])
	}
	// Ties are ordered by name so the report is stable between runs.
	tied := aggregateTelegramReportClients([]struct {
		Name  string
		Count int64
	}{{Name: "zeta", Count: 5}, {Name: "alpha", Count: 5}}, 2)
	if tied[0].Name != "alpha" {
		t.Fatalf("tie order = %+v, want alpha first", tied)
	}
}

func TestTelegramReportPeakWindowIsTheBusiestHour(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().In(time.Local)
	settings := app.db.currentSystemSettings()
	location := timezoneLocation(settings.ScheduleTimezone)
	local := now.In(location)
	todayStart := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)

	site, err := app.db.CreateSite("peak-window", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	insert := func(at time.Time) {
		t.Helper()
		if _, err := app.db.db.Exec(`INSERT INTO request_logs
			(site_id, site_name, resource_category, status_code, client_ip, user_agent, method, path, recorded_at_ms, timeline_at_ms)
			VALUES (?, 'peak-window', 'video', 200, '127.0.0.1', 'test', 'GET', '/x', ?, ?)`,
			site.ID, at.UnixMilli(), at.UnixMilli()); err != nil {
			t.Fatalf("insert request log: %v", err)
		}
	}
	// 09:00 gets three requests, 14:00 gets one: the window must be 09:00-10:00.
	for range 3 {
		insert(todayStart.Add(9*time.Hour + 5*time.Minute))
	}
	insert(todayStart.Add(14*time.Hour + 30*time.Minute))

	start, end, requests, ok, err := app.db.telegramReportPeakWindow(todayStart, todayStart.AddDate(0, 0, 1), location)
	if err != nil {
		t.Fatalf("peak window: %v", err)
	}
	if !ok {
		t.Fatal("peak window reported no data despite requests today")
	}
	if start.Hour() != 9 || end.Hour() != 10 || requests != 3 {
		t.Fatalf("peak window = %s-%s with %d, want 09:00-10:00 with 3", start.Format("15:04"), end.Format("15:04"), requests)
	}
	if start.Format("15:04") != "09:00" {
		t.Fatalf("peak window start rendered as %s", start.Format("15:04"))
	}

	// The report line prints the window and the count, never a head count.
	stats := telegramReportStats{GeneratedAt: local, PeakStart: start, PeakEnd: end, PeakRequests: requests, HasPeakWindow: true}
	if message := buildTelegramReportMessage(stats); !strings.Contains(message, "⏰ 活跃高峰：09:00 - 10:00（3 次）") {
		t.Fatalf("peak line is wrong:\n%s", message)
	}

	// A day with no requests must not invent a window.
	emptyStart, _, _, emptyOK, err := app.db.telegramReportPeakWindow(todayStart.AddDate(0, 0, -3), todayStart.AddDate(0, 0, -2), location)
	if err != nil {
		t.Fatalf("empty peak window: %v", err)
	}
	if emptyOK || !emptyStart.IsZero() {
		t.Fatalf("empty day reported a window: ok=%v start=%s", emptyOK, emptyStart)
	}
	if message := buildTelegramReportMessage(telegramReportStats{GeneratedAt: local}); !strings.Contains(message, "⏰ 活跃高峰：暂无数据") {
		t.Fatalf("empty peak line is wrong:\n%s", message)
	}
}

func TestTelegramReportClientDistributionListsShares(t *testing.T) {
	stats := telegramReportStats{GeneratedAt: time.Now(), TrafficWarnPercent: 0}
	stats.TopUserAgents = append(stats.TopUserAgents, struct {
		Name  string
		Count int64
	}{Name: "Infuse", Count: 75}, struct {
		Name  string
		Count int64
	}{Name: "Emby", Count: 25})
	message := buildTelegramReportMessage(stats)
	if !strings.Contains(message, "• Infuse：75 次（75.0%）") || !strings.Contains(message, "• Emby：25 次（25.0%）") {
		t.Fatalf("client distribution shares are wrong:\n%s", message)
	}
	empty := buildTelegramReportMessage(telegramReportStats{GeneratedAt: time.Now()})
	if !strings.Contains(empty, "• 暂无客户端数据") || !strings.Contains(empty, "• 暂无落地节点") {
		t.Fatalf("empty report is missing its empty states:\n%s", empty)
	}
	if strings.Contains(empty, "⚠️ 流量预警") {
		t.Fatalf("empty report printed a warning section:\n%s", empty)
	}
}
