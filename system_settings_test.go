package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestSystemSettingsDefaultsUseBeijingSchedule(t *testing.T) {
	settings := defaultSystemSettings()
	if settings.ScheduleTimezone != 480 {
		t.Fatalf("schedule timezone = %d, want 480", settings.ScheduleTimezone)
	}
	if settings.LogEnabled != true || settings.LogBatchSize != 50 || !settings.LogDisplayUA || !settings.LogDisplayUpstreamUA || settings.LogWriteImage || !settings.LogWritePlayback || settings.LogWriteMetadata || !settings.LogWriteVideo || !settings.LogWriteSubtitle || !settings.LogWriteAsset || !settings.LogWriteWebSocket || !settings.LogWriteAPI || !settings.LogWriteAuth || !settings.LogWriteNode || !settings.LogWriteCategory || !settings.LogWriteStatus || !settings.LogWriteClientIP || !settings.LogWriteUA || !settings.LogWriteUpstreamUA || !settings.LogWriteTimeline {
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

func TestSystemSettingsAPIDoesNotExposeOrAcceptUnimplementedLogOptions(t *testing.T) {
	app := newTestApp(t)
	hidden := []string{
		"log_write_delay_minutes",
		"log_flush_threshold",
		"log_task_lease_ms",
		"log_search_mode",
	}
	requests := []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/system-settings", nil),
		httptest.NewRequest(http.MethodPost, "/api/system-settings", bytes.NewBufferString(`{
			"log_write_delay_minutes": 99,
			"log_flush_threshold": 99,
			"log_task_lease_ms": 99,
			"log_search_mode": "fts"
		}`)),
	}
	for _, request := range requests {
		response := httptest.NewRecorder()
		app.handleSystemSettings(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s system settings status=%d body=%s", request.Method, response.Code, response.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode %s system settings: %v", request.Method, err)
		}
		for _, key := range hidden {
			if _, exists := payload[key]; exists {
				t.Fatalf("%s system settings exposed %q: %s", request.Method, key, response.Body.String())
			}
		}
	}

	var delay, threshold, lease int
	var searchMode string
	if err := app.db.db.QueryRow(`SELECT log_write_delay_minutes, log_flush_threshold, log_task_lease_ms, log_search_mode FROM system_settings WHERE id=1`).Scan(&delay, &threshold, &lease, &searchMode); err != nil {
		t.Fatalf("read legacy log columns: %v", err)
	}
	if delay != 0 || threshold != 1 || lease != 300000 || searchMode != "like" {
		t.Fatalf("hidden log options changed through API: delay=%d threshold=%d lease=%d search=%q", delay, threshold, lease, searchMode)
	}
}

func TestLegacyMetadataWriteSettingMigratesToPlayback(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "legacy-log-taxonomy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.db.Exec(`UPDATE system_settings SET
		log_write_playback=0,
		log_write_metadata=1,
		log_resource_taxonomy_version=0
		WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := db.migrateOnce(); err != nil {
		t.Fatal(err)
	}
	var playback, metadata, version int
	if err := db.db.QueryRow("SELECT log_write_playback, log_write_metadata, log_resource_taxonomy_version FROM system_settings WHERE id=1").Scan(&playback, &metadata, &version); err != nil {
		t.Fatal(err)
	}
	if playback != 1 || metadata != 0 || version != 1 {
		t.Fatalf("migrated taxonomy = playback:%d metadata:%d version:%d", playback, metadata, version)
	}
}
