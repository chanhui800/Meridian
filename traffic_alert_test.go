package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The threshold alert is a separate delivery from the daily report, so these
// tests cover the three things that make it usable rather than spam: it fires
// once per step, it escalates when a resource keeps growing, and the daily
// report's own message is untouched.

func TestTelegramAlertBucketStepsAndEscalation(t *testing.T) {
	const limit = int64(1000)
	for _, testCase := range []struct {
		used     int64
		warn     int
		expected int
		why      string
	}{
		{500, 80, 0, "below the threshold"},
		{799, 80, 0, "one byte below the threshold"},
		{800, 80, 80, "exactly at the threshold"},
		{840, 80, 80, "just under the next step"},
		{850, 80, 85, "the first five-point step"},
		{899, 80, 85, "just under the step after that"},
		{900, 80, 90, "two steps up"},
		{999, 80, 95, "just under a full quota"},
		{1000, 80, 100, "a full quota"},
		{1001, 80, 100, "one byte past a full quota"},
		{1050, 80, 105, "past the quota, onto the next step"},
		{2000, 80, 200, "double the quota"},
		{2400, 80, 240, "past double"},
		{500, 0, 0, "alerts disabled"},
		{0, 80, 0, "no usage"},
	} {
		if got := telegramReportAlertBucket(testCase.used, limit, testCase.warn); got != testCase.expected {
			t.Fatalf("bucket for used=%d warn=%d = %d, want %d (%s)", testCase.used, testCase.warn, got, testCase.expected, testCase.why)
		}
	}
	// The ladder is anchored on the threshold: its first step is the threshold
	// itself, and each further step is one more step size up.
	if got := telegramReportAlertBucket(500, limit, 50); got != 50 {
		t.Fatalf("bucket exactly at a 50%% threshold = %d, want 50", got)
	}
	if got := telegramReportAlertBucket(600, limit, 50); got != 60 {
		t.Fatalf("bucket at 50%% threshold with 60%% used = %d, want 60", got)
	}
	if got := telegramReportAlertBucket(400, limit, 50); got != 0 {
		t.Fatalf("bucket below the 50%% threshold = %d, want 0", got)
	}
	// A threshold that is not a multiple of the step anchors its own first step,
	// which is how the very first alert fires at exactly the configured line
	// rather than at the next round number.
	if got := telegramReportAlertBucket(720, limit, 72); got != 72 {
		t.Fatalf("bucket exactly at a 72%% threshold = %d, want 72", got)
	}
	if got := telegramReportAlertBucket(700, limit, 72); got != 0 {
		t.Fatalf("bucket below a 72%% threshold = %d, want 0", got)
	}
	if got := telegramReportAlertBucket(760, limit, 72); got != 72 {
		t.Fatalf("bucket between the anchoring step and the next = %d, want 72", got)
	}
	if got := telegramReportAlertBucket(770, limit, 72); got != 77 {
		t.Fatalf("bucket one step above a 72%% threshold = %d, want 77", got)
	}
	// A quota large enough to overflow a naive ratio must still classify.
	const huge = int64(1) << 62
	if got := telegramReportAlertBucket(huge, huge, 80); got != 100 {
		t.Fatalf("bucket for a full multi-exabyte quota = %d, want 100", got)
	}
	if got := telegramReportAlertBucket(0, 0, 80); got != 0 {
		t.Fatalf("bucket for an unmetered resource = %d, want 0", got)
	}
	// The ladder is anchored on the threshold, so every step is a whole multiple
	// of the configured step size.
	for percent := 80; percent <= 130; percent += 5 {
		used := int64(percent * 10)
		got := telegramReportAlertBucket(used, limit, 80)
		if got%trafficAlertEscalationStep != 0 || got < 80 || got > percent {
			t.Fatalf("bucket at %d%% = %d, which is not a step at or below the usage", percent, got)
		}
	}
}

func TestTelegramAlertMessageIsStandalone(t *testing.T) {
	alert := telegramTrafficAlert{
		kind: "node", id: 3, name: "9929", bucket: 80,
		used: 481_460_000_000, limit: 536_870_912_000, remaining: 55_410_912_000,
		cycleTraffic: 481_460_000_000, dailyTraffic: 1_640_000_000,
	}
	message := buildTelegramTrafficAlertMessage(alert, 80, time.Date(2026, time.September, 14, 15, 30, 0, 0, time.UTC))
	for _, expected := range []string{"⚠️ 流量预警", "• 节点：9929", "• 已用：", "• 剩余：", "• 今日：", "• 阈值：80%"} {
		if !strings.Contains(message, expected) {
			t.Fatalf("alert is missing %q:\n%s", expected, message)
		}
	}
	// It must not carry the daily report's furniture.
	for _, unwanted := range []string{"Meridian 数据日报", "✨ 今日概览", "保号提醒", "System Status"} {
		if strings.Contains(message, unwanted) {
			t.Fatalf("alert leaked the daily report layout (%q):\n%s", unwanted, message)
		}
	}
	// A spent quota reads as over the limit rather than showing a bogus zero.
	spent := alert
	spent.remaining = 0
	if message := buildTelegramTrafficAlertMessage(spent, 80, time.Now()); !strings.Contains(message, "已超出额度") {
		t.Fatalf("spent quota message is wrong:\n%s", message)
	}
	// A site alert is labelled as a site and carries no node-only daily figure.
	site := alert
	site.kind = "site"
	site.name = "quota-site"
	site.dailyTraffic = 0
	siteMessage := buildTelegramTrafficAlertMessage(site, 80, time.Now())
	if !strings.Contains(siteMessage, "• 站点：quota-site") || strings.Contains(siteMessage, "• 今日：") {
		t.Fatalf("site alert is wrong:\n%s", siteMessage)
	}
}

// TestTelegramAlertFiresOnceAndEscalates is the anti-spam contract: a resource
// sitting above the threshold is announced once, then again only when it climbs
// into the next step.
func TestTelegramAlertFiresOnceAndEscalates(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().In(time.Local)
	const gib = int64(1) << 30
	const quota = int64(500) * gib

	ciphertext, err := encryptTelegramBotToken("123456:example-token")
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}
	settings := TelegramReportSettings{Enabled: true, ChatID: "123456", ScheduleTime: "20:00", Frequency: "daily", Weekday: 1, TrafficWarningPercent: 80}
	if err := app.db.saveTelegramReportSettings(settings, ciphertext, true); err != nil {
		t.Fatalf("save settings: %v", err)
	}

	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "alert-node", Address: "203.0.113.97", Port: 9443,
		TrafficQuota: quota, BillingMode: trafficBillingModeBidirectional, ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatalf("CreateControlNode: %v", err)
	}
	if _, _, err := app.db.EnrollControlNode(enrollment, now); err != nil {
		t.Fatalf("EnrollControlNode: %v", err)
	}

	setUsage := func(fraction float64) {
		t.Helper()
		tx := int64(float64(quota) * fraction)
		if _, err := app.db.db.Exec(`UPDATE control_nodes SET enabled=1, period_rx_bytes=0, period_tx_bytes=?, traffic_manual_offset_bytes=0 WHERE id=?`, tx, node.ID); err != nil {
			t.Fatalf("seed usage: %v", err)
		}
	}
	var sent []string
	sender := func(_ context.Context, _, _, message string) error {
		sent = append(sent, message)
		return nil
	}
	tick := func(fraction float64) {
		t.Helper()
		setUsage(fraction)
		runTelegramTrafficAlertTick(context.Background(), app.db, now, sender)
	}

	tick(0.50)
	if len(sent) != 0 {
		t.Fatalf("a resource below the threshold alerted: %v", sent)
	}
	tick(0.80)
	if len(sent) != 1 {
		t.Fatalf("crossing the threshold sent %d alerts, want 1", len(sent))
	}
	// Sitting above the line but below the next step must not repeat.
	for range 5 {
		tick(0.82)
	}
	if len(sent) != 1 {
		t.Fatalf("a resource parked above the threshold sent %d alerts, want 1", len(sent))
	}
	// Climbing onto the next step is a new announcement.
	tick(0.85)
	if len(sent) != 2 {
		t.Fatalf("crossing onto the first step sent %d alerts, want 2", len(sent))
	}
	for range 5 {
		tick(0.88)
	}
	if len(sent) != 2 {
		t.Fatalf("a resource parked on a step sent %d alerts, want 2", len(sent))
	}
	tick(0.90)
	if len(sent) != 3 {
		t.Fatalf("crossing the following step sent %d alerts, want 3", len(sent))
	}
	// The steps keep escalating past a full quota.
	tick(1.00)
	if len(sent) != 4 {
		t.Fatalf("crossing a full quota sent %d alerts, want 4", len(sent))
	}
	tick(1.10)
	if len(sent) != 5 {
		t.Fatalf("crossing the step past a full quota sent %d alerts, want 5", len(sent))
	}
	tick(1.15)
	if len(sent) != 6 {
		t.Fatalf("crossing the step after that sent %d alerts, want 6", len(sent))
	}
	// Dropping back below the threshold does not re-arm within the same cycle.
	tick(0.10)
	if len(sent) != 6 {
		t.Fatalf("dropping below the threshold changed the alert count to %d", len(sent))
	}
	tick(0.86)
	if len(sent) != 6 {
		t.Fatalf("re-crossing an already announced step re-alerted within the cycle: %d alerts", len(sent))
	}

	// A new billing cycle re-arms every step. The node's own cycle boundary is
	// what the ledger keys on, so the rollover is simulated the way the panel
	// performs it: resetDueNodeCycles advances cycle_started_at_ms and zeroes the
	// period counters.
	nextCycle := now.AddDate(0, 1, 0)
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET cycle_started_at_ms=?, period_rx_bytes=0, period_tx_bytes=0 WHERE id=?`,
		time.Date(nextCycle.Year(), nextCycle.Month(), 1, 0, 0, 0, 0, time.Local).UnixMilli(), node.ID); err != nil {
		t.Fatalf("roll the node cycle: %v", err)
	}
	setUsage(0.85)
	runTelegramTrafficAlertTick(context.Background(), app.db, nextCycle, sender)
	if len(sent) != 7 {
		t.Fatalf("a new cycle sent %d alerts in total, want 7", len(sent))
	}
	// In the current cycle the same state must stay quiet.
	setUsage(0.85)
	runTelegramTrafficAlertTick(context.Background(), app.db, now, sender)
	if len(sent) != 7 {
		t.Fatalf("the original cycle re-alerted after the new cycle ran: %d alerts", len(sent))
	}
	// The ledger must hold one row per (resource, cycle, step): six from the first
	// cycle and one from the second.
	var rows int
	if err := app.db.db.QueryRow(`SELECT COUNT(*) FROM telegram_traffic_alerts WHERE kind='node' AND resource_id=?`, node.ID).Scan(&rows); err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	if rows != 7 {
		t.Fatalf("ledger rows = %d, want 7", rows)
	}
}

func TestTelegramAlertRequiresConfiguration(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().In(time.Local)
	if _, err := app.db.db.Exec(`UPDATE telegram_report_settings SET bot_token_ciphertext='', chat_id='' WHERE id=1`); err != nil {
		t.Fatalf("clear settings: %v", err)
	}
	sent := 0
	sender := func(context.Context, string, string, string) error {
		sent++
		return nil
	}
	runTelegramTrafficAlertTick(context.Background(), app.db, now, sender)
	if sent != 0 {
		t.Fatalf("an unconfigured bot sent %d alerts", sent)
	}
}

func TestTelegramAlertRespectsDisabledThreshold(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().In(time.Local)
	const gib = int64(1) << 30
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "disabled-threshold", Address: "203.0.113.96", Port: 9443,
		TrafficQuota: 100 * gib, BillingMode: trafficBillingModeBidirectional, ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatalf("CreateControlNode: %v", err)
	}
	if _, _, err := app.db.EnrollControlNode(enrollment, now); err != nil {
		t.Fatalf("EnrollControlNode: %v", err)
	}
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET enabled=1, period_tx_bytes=? WHERE id=?`, int64(95)*gib, node.ID); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
	alerts, warnPercent, err := app.db.scanTelegramTrafficAlerts(now)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if warnPercent != 80 || len(alerts) != 1 {
		t.Fatalf("with the default threshold: warnPercent=%d alerts=%d, want 80 and 1", warnPercent, len(alerts))
	}

	// 0 disables the feature outright.
	stored, err := app.db.telegramReportSettings()
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	saved := stored.TelegramReportSettings
	saved.TrafficWarningPercent = telegramReportTrafficWarnDisableValue
	if err := app.db.saveTelegramReportSettings(saved, stored.BotTokenCiphertext, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	alerts, warnPercent, err = app.db.scanTelegramTrafficAlerts(now)
	if err != nil {
		t.Fatalf("scan with alerts disabled: %v", err)
	}
	if warnPercent != 0 || len(alerts) != 0 {
		t.Fatalf("disabled alerts: warnPercent=%d alerts=%d, want 0 and 0", warnPercent, len(alerts))
	}
}

// TestTelegramAlertStatusReportsArmedState covers the read-only view the panel
// uses, including that a not-yet-delivered alert is marked pending.
func TestTelegramAlertStatusReportsArmedState(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().In(time.Local)
	const gib = int64(1) << 30
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "status-node", Address: "203.0.113.95", Port: 9443,
		TrafficQuota: 100 * gib, BillingMode: trafficBillingModeBidirectional, ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatalf("CreateControlNode: %v", err)
	}
	if _, _, err := app.db.EnrollControlNode(enrollment, now); err != nil {
		t.Fatalf("EnrollControlNode: %v", err)
	}
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET enabled=1, period_tx_bytes=? WHERE id=?`, int64(90)*gib, node.ID); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	status, err := app.db.telegramTrafficAlertStatus(now)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	over, _ := status["over_threshold"].([]map[string]any)
	if len(over) != 1 {
		t.Fatalf("over_threshold = %+v, want one entry", status["over_threshold"])
	}
	if over[0]["name"] != "status-node" || over[0]["pending_delivery"] != true {
		t.Fatalf("over_threshold entry = %+v, want status-node pending", over[0])
	}
	if status["threshold_percent"] != 80 {
		t.Fatalf("threshold_percent = %v, want 80", status["threshold_percent"])
	}

	// Once delivered, the same state is no longer pending but still reported.
	scanned, _, err := app.db.scanTelegramTrafficAlerts(now)
	if err != nil || len(scanned) != 1 {
		t.Fatalf("scan = %+v (%v), want one alert", scanned, err)
	}
	if err := app.db.recordTelegramTrafficAlert(scanned[0], now); err != nil {
		t.Fatalf("record: %v", err)
	}
	status, err = app.db.telegramTrafficAlertStatus(now)
	if err != nil {
		t.Fatalf("status after delivery: %v", err)
	}
	over, _ = status["over_threshold"].([]map[string]any)
	if len(over) != 1 || over[0]["pending_delivery"] != false {
		t.Fatalf("after delivery over_threshold = %+v, want one non-pending entry", over)
	}
	history, _ := status["recent_alerts"].([]map[string]any)
	if len(history) != 1 || history[0]["name"] != "status-node" {
		t.Fatalf("recent_alerts = %+v, want the delivered alert", history)
	}
}

// TestDailyReportWarningSectionStillWorks guards the requirement that the daily
// report keeps its own format while alerts are delivered separately.
func TestDailyReportWarningSectionStillWorks(t *testing.T) {
	stats := telegramReportStats{
		GeneratedAt:        time.Now(),
		TrafficWarnPercent: 80,
		TrafficWarnings: []telegramReportTrafficWarning{
			{Kind: "node", Name: "9929", Used: 85, Limit: 100, Remaining: 15},
		},
	}
	message := buildTelegramReportMessage(stats)
	if !strings.Contains(message, "⚠️ 流量预警（已达 80%）") || !strings.Contains(message, "🟠 节点 9929：已用 85.0%（剩余 15 B）") {
		t.Fatalf("the daily report warning section changed:\n%s", message)
	}
	if !strings.Contains(message, "📊 Meridian 数据日报") || !strings.Contains(message, "System Status") {
		t.Fatalf("the daily report layout changed:\n%s", message)
	}
}
