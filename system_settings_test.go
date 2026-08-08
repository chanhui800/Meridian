package main

import (
	"testing"
	"time"
)

func TestSystemSettingsDefaultsUseBeijingSchedule(t *testing.T) {
	settings := defaultSystemSettings()
	if settings.ScheduleTimezone != 480 {
		t.Fatalf("schedule timezone = %d, want 480", settings.ScheduleTimezone)
	}
	if settings.LogEnabled != true || settings.LogBatchSize != 50 || settings.LogDisplayUA != true || settings.LogWriteImage || settings.LogWriteMetadata || !settings.LogWriteVideo || !settings.LogWriteAPI || !settings.LogWriteAuth || !settings.LogWriteNode || !settings.LogWriteCategory || !settings.LogWriteStatus || !settings.LogWriteClientIP || !settings.LogWriteUA || !settings.LogWriteTimeline {
		t.Fatalf("unexpected defaults: %+v", settings)
	}
}

func TestTelegramReportDueNormalizesToBeijing(t *testing.T) {
	settings := telegramReportSettingsView{Enabled: true, Configured: true, ScheduleTime: "20:00", Frequency: "daily", Timezone: 480}
	utc := time.Date(2026, time.August, 8, 12, 1, 0, 0, time.UTC)
	key, due := telegramReportDue(utc, settings)
	if !due || key != "daily:2026-08-08" {
		t.Fatalf("due = (%q, %v)", key, due)
	}
}

func TestSystemSettingsAllowManualScheduleTimezone(t *testing.T) {
	settings := defaultSystemSettings()
	settings.ScheduleTimezone = -300
	normalized, err := normalizeSystemSettings(settings)
	if err != nil || normalized.ScheduleTimezone != -300 {
		t.Fatalf("manual timezone = %d, err = %v", normalized.ScheduleTimezone, err)
	}
}
