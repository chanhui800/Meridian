package main

import (
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
