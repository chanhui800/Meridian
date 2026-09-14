package main

import (
	"context"
	"database/sql"
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
		ThirtyDayTraffic:   90 << 30,
		HistoryTraffic:     400 << 30,
		BillingMode:        trafficBillingModeBidirectional,
		TrafficWarnPercent: 80,
		TopTraffic: []telegramReportSiteStat{
			{Name: "最热", Traffic: 5 << 30},
			{Name: "次热", Traffic: 3 << 30},
			{Name: "第三", Traffic: 2 << 30},
		},
		Nodes: []telegramReportNodeStat{{Name: "落地节点", TodayTraffic: 1 << 30, CycleTraffic: 10 << 30, Remaining: 90 << 30, HasQuota: true, SiteCount: 11}},
		RetentionSites: []telegramReportRetentionStat{
			{Name: "保号站", RemainingDays: 5},
		},
	}
	message := buildTelegramReportMessage(stats)
	for _, expected := range []string{
		"📊 Meridian 数据日报",
		"⏱ 统计时间：2026-09-13 20:00",
		"✨ 今日概览",
		"👥 独立访客：42 人",
		"🏆 今日最热媒体库：最热",
		"📍 服务器部署信息",
		"🧩 客户端分布",
		"🌐 流量统计",
		"🔥 今日节点热度 TOP 5",
		"🔔 保号提醒",
		"🚀 System Status：Operational",
	} {
		if !strings.Contains(message, expected) {
			t.Fatalf("report is missing %q:\n%s", expected, message)
		}
	}
	// Section order is part of the contract, not just presence.
	order := []string{"✨ 今日概览", "📍 服务器部署信息", "🧩 客户端分布", "🌐 流量统计", "🔥 今日节点热度 TOP 5", "🔔 保号提醒", "🚀 System Status"}
	position := -1
	for _, header := range order {
		index := strings.Index(message, header)
		if index <= position {
			t.Fatalf("section %q is out of order:\n%s", header, message)
		}
		position = index
	}
	// The top library is ranked by traffic, so the crown marks the heaviest site.
	if !strings.Contains(message, "👑 最热：") || !strings.Contains(message, "🌟 次热：") || !strings.Contains(message, "3. 第三：") {
		t.Fatalf("ranking markers are wrong:\n%s", message)
	}
	if strings.Contains(message, "历史条数") {
		t.Fatalf("report still renders the removed history-count line:\n%s", message)
	}
}

func TestTelegramReportDeploymentSectionSummarisesSingleNode(t *testing.T) {
	message := buildTelegramReportMessage(telegramReportStats{
		GeneratedAt:        time.Now(),
		TrafficWarnPercent: telegramReportTrafficWarnDisableValue,
		Nodes:              []telegramReportNodeStat{{Name: "唯一节点", TodayTraffic: 1 << 30, CycleTraffic: 2 << 30, Remaining: 8 << 30, HasQuota: true, SiteCount: 11}},
	})
	if !strings.Contains(message, "所有站点统一通过 唯一节点 中转") {
		t.Fatalf("single-node deployment summary missing:\n%s", message)
	}
	if !strings.Contains(message, "共 1 个落地节点 · 服务中 1 个 ✅") {
		t.Fatalf("node tally missing:\n%s", message)
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
	// A large quota must not overflow the integer comparison: 1 PiB used at 80%.
	const pib = int64(1) << 50
	if _, ok := telegramReportProximityWarning("node", "超大额度", 90*(pib/100), 10*(pib/100), 80); !ok {
		t.Fatal("a very large quota did not warn at 90%")
	}
}

func TestTelegramReportStatsCountLandingNodeUsage(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().In(time.Local)
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "report-node", Address: "203.0.113.77", Port: 9443,
		TrafficQuota: 1000, BillingMode: trafficBillingModeOutbound, ResetDay: 1,
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
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET enabled=1, period_rx_bytes=?, period_tx_bytes=? WHERE id=?`, int64(500), int64(400), node.ID); err != nil {
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
	// The node cycle uses its OWN outbound mode, so 400 out of a 1000 quota
	// leaves 600. Reading the panel-wide bidirectional mode here would report
	// 900 used and 100 remaining, and would trip an 80% warning wrongly.
	if got.CycleTraffic != 400 || got.Remaining != 600 || !got.HasQuota {
		t.Fatalf("node cycle = %+v, want used 400 remaining 600", got)
	}
	if len(stats.TrafficWarnings) != 0 {
		t.Fatalf("warnings = %+v, want none at 40%% of quota", stats.TrafficWarnings)
	}

	// Push the node over the 80% line and confirm the warning follows the
	// node's own mode.
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET period_tx_bytes=? WHERE id=?`, int64(850), node.ID); err != nil {
		t.Fatalf("raise node usage: %v", err)
	}
	stats, err = app.db.buildTelegramReportStats(now)
	if err != nil {
		t.Fatalf("rebuild stats: %v", err)
	}
	if len(stats.TrafficWarnings) != 1 || stats.TrafficWarnings[0].Kind != "node" || stats.TrafficWarnings[0].Name != "report-node" {
		t.Fatalf("warnings = %+v, want one node warning", stats.TrafficWarnings)
	}
	if stats.TrafficWarnings[0].Remaining != 150 {
		t.Fatalf("warning remaining = %d, want 150", stats.TrafficWarnings[0].Remaining)
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
	if strings.Contains(empty, "共 0 个落地节点") {
		t.Fatalf("empty report printed a node tally without nodes:\n%s", empty)
	}
	if strings.Contains(empty, "⚠️ 流量预警") {
		t.Fatalf("empty report printed a warning section:\n%s", empty)
	}
}
