package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestTelegramReportTokenRoundTrip(t *testing.T) {
	ciphertext, err := encryptTelegramBotToken("123456:example-token")
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}
	plaintext, err := decryptTelegramBotToken(ciphertext)
	if err != nil {
		t.Fatalf("decrypt token: %v", err)
	}
	if plaintext != "123456:example-token" {
		t.Fatalf("round trip token = %q", plaintext)
	}

	public := telegramReportPublicSettings(telegramReportStoredSettings{
		TelegramReportSettings: TelegramReportSettings{ChatID: "-100123"},
		BotTokenCiphertext:     ciphertext,
	})
	if public.BotToken != "123456:example-token" {
		t.Fatalf("public settings token = %q", public.BotToken)
	}
}

func TestTelegramReportStatsCountCurrentVideoCategoriesAndMergeRenamedSite(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSite("current-name", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for index, category := range []string{requestLogCategoryStream, requestLogCategoryManifest, requestLogCategorySegment, requestLogCategoryVideo, requestLogCategoryAPI} {
		name := "old-name"
		if index > 1 {
			name = ""
		}
		_, err := app.db.db.Exec(`INSERT INTO request_logs
			(site_id, site_name, resource_category, status_code, client_ip, user_agent, method, path, recorded_at_ms, timeline_at_ms)
			VALUES (?, ?, ?, 200, '127.0.0.1', 'test', 'GET', ?, ?, ?)`, site.ID, name, category, fmt.Sprintf("/request/%d", index), now.UnixMilli(), now.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
	}
	stats, err := app.db.buildTelegramReportStats(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.VideoRequests != 4 || stats.Requests != 5 {
		t.Fatalf("requests total/video = %d/%d, want 5/4", stats.Requests, stats.VideoRequests)
	}
	if len(stats.TopRequests) != 1 || stats.TopRequests[0].Name != "current-name" || stats.TopRequests[0].Requests != 5 {
		t.Fatalf("top requests = %+v, want one merged current-name row with 5", stats.TopRequests)
	}
}

func TestTelegramSchedulerSuppressesDuplicateAfterSentMarkerFailure(t *testing.T) {
	app := newTestApp(t)
	settings := TelegramReportSettings{Enabled: true, ChatID: "123456", ScheduleTime: "00:00", Frequency: "daily", Weekday: 1}
	ciphertext, err := encryptTelegramBotToken("123456:example-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.db.saveTelegramReportSettings(settings, ciphertext, true); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`CREATE TRIGGER fail_telegram_sent_marker
		BEFORE UPDATE OF last_sent_key ON telegram_report_settings
		WHEN NEW.last_sent_key <> OLD.last_sent_key
		BEGIN SELECT RAISE(ABORT, 'forced sent marker failure'); END`); err != nil {
		t.Fatal(err)
	}
	state := &telegramReportSchedulerState{}
	sends := 0
	sender := func(context.Context, string, string, string) error {
		sends++
		return nil
	}
	now := time.Now()
	runTelegramReportSchedulerTick(context.Background(), app.db, state, now, sender)
	runTelegramReportSchedulerTick(context.Background(), app.db, state, now.Add(15*time.Second), sender)
	if sends != 1 {
		t.Fatalf("successful sends = %d, want 1 despite marker failure", sends)
	}
}

func TestTelegramReportDueUsesConfiguredSchedule(t *testing.T) {
	settings := telegramReportSettingsView{Enabled: true, Configured: true, ScheduleTime: "20:00", Frequency: "daily", Timezone: 480}
	now := time.Date(2026, time.August, 8, 20, 1, 0, 0, time.FixedZone("HKT", 8*60*60))
	key, due := telegramReportDue(now, settings)
	if !due || key != "daily:2026-08-08" {
		t.Fatalf("due = (%q, %v)", key, due)
	}
	before := now.Add(-2 * time.Hour)
	if _, due := telegramReportDue(before, settings); due {
		t.Fatal("report should not be due before configured time")
	}
}

func TestTelegramReportMessageIsBoundedAtUTF8Boundary(t *testing.T) {
	message := truncateTelegramText(strings.Repeat("客户端", 2000), telegramReportMaxMessageBytes)
	if len(message) > telegramReportMaxMessageBytes {
		t.Fatalf("message length = %d", len(message))
	}
	if !strings.HasSuffix(message, "...") {
		t.Fatal("long report should end with an ellipsis")
	}
}

func TestTelegramReportIncludesAccountRetentionCompletionAndCountdown(t *testing.T) {
	message := buildTelegramReportMessage(telegramReportStats{
		GeneratedAt: time.Date(2026, time.August, 24, 20, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60)),
		RetentionSites: []telegramReportRetentionStat{
			{Name: "已观看", RemainingDays: 30, CompletedToday: true},
			{Name: "即将到期", RemainingDays: 7},
			{Name: "已超期", RemainingDays: 0},
		},
	})
	for _, expected := range []string{
		"🔔 保号提醒",
		"已观看：✅ 完成保号",
		"即将到期：🔴 剩余 7 天",
		"已超期：🔴 已到期",
	} {
		if !strings.Contains(message, expected) {
			t.Fatalf("report missing %q:\n%s", expected, message)
		}
	}
}

func TestTelegramReportScheduleChangeRearmsCurrentPeriod(t *testing.T) {
	app := newTestApp(t)
	settings := TelegramReportSettings{
		Enabled: true, ChatID: "123456", ScheduleTime: "20:00", Frequency: "daily", Weekday: 1,
	}
	ciphertext, err := encryptTelegramBotToken("123456:example-token")
	if err != nil {
		t.Fatalf("encrypt token: %v", err)
	}
	if err := app.db.saveTelegramReportSettings(settings, ciphertext, true); err != nil {
		t.Fatalf("save initial settings: %v", err)
	}
	if err := app.db.markTelegramReportSent("daily:2026-08-10"); err != nil {
		t.Fatalf("mark sent: %v", err)
	}

	if err := app.db.saveTelegramReportSettings(settings, ciphertext, false); err != nil {
		t.Fatalf("save unchanged settings: %v", err)
	}
	stored, err := app.db.telegramReportSettings()
	if err != nil {
		t.Fatalf("read unchanged settings: %v", err)
	}
	if stored.LastSentKey != "daily:2026-08-10" {
		t.Fatalf("unchanged settings cleared last_sent_key: %q", stored.LastSentKey)
	}

	settings.ScheduleTime = "20:05"
	if err := app.db.saveTelegramReportSettings(settings, ciphertext, false); err != nil {
		t.Fatalf("save changed schedule: %v", err)
	}
	stored, err = app.db.telegramReportSettings()
	if err != nil {
		t.Fatalf("read changed settings: %v", err)
	}
	if stored.LastSentKey != "" {
		t.Fatalf("changed schedule kept last_sent_key: %q", stored.LastSentKey)
	}
}

func TestTelegramReportTrafficUsesGlobalBillingMode(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSite("telegram-billing", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	now := time.Now()
	if err := app.db.addTrafficWithRequestsAt(site.ID, 10, 90, 1, now); err != nil {
		t.Fatalf("add traffic: %v", err)
	}

	dual, err := app.db.buildTelegramReportStats(now)
	if err != nil {
		t.Fatalf("build bidirectional stats: %v", err)
	}
	if dual.TodayTraffic != 200 || dual.HistoryTraffic != 200 || len(dual.TopTraffic) != 1 || dual.TopTraffic[0].Traffic != 200 {
		t.Fatalf("bidirectional stats = %+v, want traffic 200", dual)
	}

	settings := app.db.currentSystemSettings()
	settings.TrafficBillingMode = trafficBillingModeOutbound
	if err := app.db.saveSystemSettings(settings); err != nil {
		t.Fatalf("save outbound mode: %v", err)
	}
	outbound, err := app.db.buildTelegramReportStats(now)
	if err != nil {
		t.Fatalf("build outbound stats: %v", err)
	}
	if outbound.TodayTraffic != 90 || outbound.HistoryTraffic != 90 || len(outbound.TopTraffic) != 1 || outbound.TopTraffic[0].Traffic != 90 {
		t.Fatalf("outbound stats = %+v, want traffic 90", outbound)
	}
}
