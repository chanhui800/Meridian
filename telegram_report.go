package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/bits"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	telegramReportDefaultTime             = "20:00"
	telegramReportDefaultWeekday          = 1
	telegramReportMaxMessageBytes         = 4096
	telegramReportCipherPrefix            = "v1:"
	telegramReportDefaultTrafficWarnPct   = 80
	telegramReportMinTrafficWarnPct       = 1
	telegramReportMaxTrafficWarnPct       = 100
	telegramReportTrafficWarnDisableValue = 0
)

// telegramReportWarningPercent reports the configured proximity threshold. It is
// read on every build so a threshold change takes effect on the next send, and a
// read failure disables nothing loudly: the report still goes out, without the
// warning section, rather than silently using a stale line.
func (d *DB) telegramReportWarningPercent() int {
	stored, err := d.telegramReportSettings()
	if err != nil {
		log.Printf("[telegram-report] warning threshold read failed: %v", err)
		return telegramReportTrafficWarnDisableValue
	}
	if stored.TrafficWarningPercent < telegramReportMinTrafficWarnPct || stored.TrafficWarningPercent > telegramReportMaxTrafficWarnPct {
		return telegramReportTrafficWarnDisableValue
	}
	return stored.TrafficWarningPercent
}

var telegramReportTimePattern = regexp.MustCompile(`^(?:[01][0-9]|2[0-3]):[0-5][0-9]$`)

type TelegramReportSettings struct {
	Enabled               bool   `json:"enabled"`
	Configured            bool   `json:"configured"`
	ChatID                string `json:"chat_id"`
	ScheduleTime          string `json:"schedule_time"`
	Frequency             string `json:"frequency"`
	Weekday               int    `json:"weekday"`
	TrafficWarningPercent int    `json:"traffic_warning_percent"`
	LastSentKey           string `json:"last_sent_key,omitempty"`
	SecretStable          bool   `json:"secret_stable"`
	BotToken              string `json:"bot_token"`
}

type telegramReportStoredSettings struct {
	TelegramReportSettings
	BotTokenCiphertext string
}

type telegramReportInput struct {
	Enabled               *bool  `json:"enabled"`
	BotToken              string `json:"bot_token"`
	ClearBotToken         bool   `json:"clear_bot_token"`
	ChatID                string `json:"chat_id"`
	ScheduleTime          string `json:"schedule_time"`
	Frequency             string `json:"frequency"`
	Weekday               *int   `json:"weekday"`
	TrafficWarningPercent *int   `json:"traffic_warning_percent"`
	Action                string `json:"action"`
}

type telegramReportSiteStat struct {
	Name     string
	Requests int64
	Traffic  int64
}

type telegramReportRetentionStat struct {
	Name           string
	RemainingDays  int
	CompletedToday bool
}

// telegramReportNodeStat describes one landing node (control_nodes row) that is
// currently serving at least one enabled site. TodayTraffic is charged with the
// same billing mode as the rest of the report; CycleTraffic and Remaining reuse
// the node's own persisted billing cycle so the numbers match the panel.
type telegramReportNodeStat struct {
	ID           int64
	Name         string
	TodayTraffic int64
	CycleTraffic int64
	Remaining    int64
	// CycleStartMS is the node's own billing-cycle boundary, used to scope the
	// alert ledger to one cycle.
	CycleStartMS int64
	// Limit is the node's configured quota. Remaining is clamped at zero once the
	// quota is exhausted, so the ratio must be taken against this figure rather
	// than against used+remaining.
	Limit     int64
	HasQuota  bool
	SiteCount int
}

// telegramReportTrafficWarning is one quota that has reached the configured
// proximity threshold. Used and Limit are stored pre-charged so the renderer
// only formats them.
type telegramReportTrafficWarning struct {
	Kind      string
	Name      string
	Used      int64
	Limit     int64
	Remaining int64
}

type telegramReportStats struct {
	GeneratedAt     time.Time
	UniqueClients   int64
	ActivePeak      int64
	PeakStart       time.Time
	PeakEnd         time.Time
	PeakRequests    int64
	HasPeakWindow   bool
	Requests        int64
	VideoRequests   int64
	TodayTraffic    int64
	SevenDayTraffic int64
	// CycleTraffic is the global billing-cycle total, following the same reset
	// day, timezone and billing mode as the panel. It is named apart from the
	// per-node telegramReportNodeStat.CycleTraffic on purpose: those are two
	// different quantities that must never be assigned to each other.
	CycleTraffic        int64
	CycleStart          time.Time
	LifetimeTraffic     int64
	BillingMode         string
	SiteCount           int
	RunningSiteCount    int
	ControllerSiteCount int
	TrafficWarnPercent  int
	TopRequests         []telegramReportSiteStat
	TopTraffic          []telegramReportSiteStat
	Nodes               []telegramReportNodeStat
	TrafficWarnings     []telegramReportTrafficWarning
	RetentionSites      []telegramReportRetentionStat
	TopUserAgents       []struct {
		Name  string
		Count int64
	}
}

func telegramReportKeyForSecret(secret []byte) []byte {
	h := sha256.New()
	_, _ = h.Write([]byte("meridian telegram report bot token v1\x00"))
	_, _ = h.Write(secret)
	return h.Sum(nil)
}

func encryptTelegramBotToken(token string) (string, error) {
	return encryptTelegramBotTokenWithSecret(token, jwtSecret)
}

func encryptTelegramBotTokenWithSecret(token string, secret []byte) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, "\r\n\t ") || len(token) > 256 {
		return "", fmt.Errorf("invalid Telegram bot token")
	}
	block, err := aes.NewCipher(telegramReportKeyForSecret(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(token), []byte("meridian-telegram-report"))
	payload := append(nonce, sealed...)
	return telegramReportCipherPrefix + base64.RawURLEncoding.EncodeToString(payload), nil
}

func decryptTelegramBotToken(ciphertext string) (string, error) {
	return decryptTelegramBotTokenWithSecret(ciphertext, jwtSecret)
}

func decryptTelegramBotTokenWithSecret(ciphertext string, secret []byte) (string, error) {
	if !strings.HasPrefix(ciphertext, telegramReportCipherPrefix) {
		return "", fmt.Errorf("invalid Telegram bot token ciphertext")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(ciphertext, telegramReportCipherPrefix))
	if err != nil {
		return "", fmt.Errorf("decode Telegram bot token: %w", err)
	}
	block, err := aes.NewCipher(telegramReportKeyForSecret(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(payload) < gcm.NonceSize()+gcm.Overhead() {
		return "", fmt.Errorf("invalid Telegram bot token ciphertext")
	}
	plain, err := gcm.Open(nil, payload[:gcm.NonceSize()], payload[gcm.NonceSize():], []byte("meridian-telegram-report"))
	if err != nil {
		return "", fmt.Errorf("decrypt Telegram bot token: %w", err)
	}
	return string(plain), nil
}

func normalizeTelegramReportSettings(settings TelegramReportSettings) (TelegramReportSettings, error) {
	settings.ScheduleTime = strings.TrimSpace(settings.ScheduleTime)
	if settings.ScheduleTime == "" {
		settings.ScheduleTime = telegramReportDefaultTime
	}
	if !telegramReportTimePattern.MatchString(settings.ScheduleTime) {
		return settings, fmt.Errorf("schedule_time must use HH:MM")
	}
	settings.Frequency = strings.ToLower(strings.TrimSpace(settings.Frequency))
	if settings.Frequency == "" {
		settings.Frequency = "daily"
	}
	if settings.Frequency != "daily" && settings.Frequency != "weekly" {
		return settings, fmt.Errorf("frequency must be daily or weekly")
	}
	if settings.Weekday < 0 || settings.Weekday > 6 {
		return settings, fmt.Errorf("weekday must be between 0 and 6")
	}
	// 0 disables the proximity warning entirely. Rows written before this column
	// existed are backfilled to the default by the migration, so the sentinel is
	// only ever produced by an explicit operator choice.
	if settings.TrafficWarningPercent < telegramReportTrafficWarnDisableValue || settings.TrafficWarningPercent > telegramReportMaxTrafficWarnPct {
		return settings, fmt.Errorf("traffic_warning_percent must be between 0 and 100")
	}
	return settings, nil
}

func (d *DB) telegramReportSettings() (telegramReportStoredSettings, error) {
	var stored telegramReportStoredSettings
	var enabled int
	err := d.db.QueryRow(`SELECT enabled, bot_token_ciphertext, chat_id, schedule_time, frequency, weekday, traffic_warning_percent, last_sent_key FROM telegram_report_settings WHERE id=1`).Scan(
		&enabled, &stored.BotTokenCiphertext, &stored.ChatID, &stored.ScheduleTime, &stored.Frequency, &stored.Weekday, &stored.TrafficWarningPercent, &stored.LastSentKey,
	)
	if errors.Is(err, sql.ErrNoRows) {
		stored.ScheduleTime = telegramReportDefaultTime
		stored.Frequency = "daily"
		stored.Weekday = telegramReportDefaultWeekday
		stored.TrafficWarningPercent = telegramReportDefaultTrafficWarnPct
		return stored, nil
	}
	if err != nil {
		return stored, err
	}
	stored.Enabled = enabled == 1
	stored.Configured = stored.BotTokenCiphertext != "" && stored.ChatID != ""
	stored.SecretStable = !jwtSecretEphemeral
	stored.TelegramReportSettings, err = normalizeTelegramReportSettings(stored.TelegramReportSettings)
	return stored, err
}

func (d *DB) saveTelegramReportSettings(settings TelegramReportSettings, botTokenCiphertext string, replaceToken bool) error {
	settings, err := normalizeTelegramReportSettings(settings)
	if err != nil {
		return err
	}
	if settings.Enabled && (settings.ChatID == "" && !replaceToken) {
		// The caller validates the existing stored token and chat ID. This guard
		// only prevents accidentally enabling an empty destination.
		return fmt.Errorf("chat_id is required when Telegram reports are enabled")
	}
	var query string
	var args []any
	if replaceToken {
		query = `UPDATE telegram_report_settings SET
			last_sent_key=CASE WHEN enabled<>? OR bot_token_ciphertext<>? OR chat_id<>? OR schedule_time<>? OR frequency<>? OR weekday<>? OR traffic_warning_percent<>? THEN '' ELSE last_sent_key END,
			enabled=?, bot_token_ciphertext=?, chat_id=?, schedule_time=?, frequency=?, weekday=?, traffic_warning_percent=?, updated_at=CURRENT_TIMESTAMP WHERE id=1`
		args = []any{
			sqliteBool(settings.Enabled), botTokenCiphertext, settings.ChatID, settings.ScheduleTime, settings.Frequency, settings.Weekday, settings.TrafficWarningPercent,
			sqliteBool(settings.Enabled), botTokenCiphertext, settings.ChatID, settings.ScheduleTime, settings.Frequency, settings.Weekday, settings.TrafficWarningPercent,
		}
	} else {
		query = `UPDATE telegram_report_settings SET
			last_sent_key=CASE WHEN enabled<>? OR chat_id<>? OR schedule_time<>? OR frequency<>? OR weekday<>? OR traffic_warning_percent<>? THEN '' ELSE last_sent_key END,
			enabled=?, chat_id=?, schedule_time=?, frequency=?, weekday=?, traffic_warning_percent=?, updated_at=CURRENT_TIMESTAMP WHERE id=1`
		args = []any{
			sqliteBool(settings.Enabled), settings.ChatID, settings.ScheduleTime, settings.Frequency, settings.Weekday, settings.TrafficWarningPercent,
			sqliteBool(settings.Enabled), settings.ChatID, settings.ScheduleTime, settings.Frequency, settings.Weekday, settings.TrafficWarningPercent,
		}
	}
	_, err = d.db.Exec(query, args...)
	return err
}

func (d *DB) markTelegramReportSent(key string) error {
	_, err := d.db.Exec(`UPDATE telegram_report_settings SET last_sent_key=?, updated_at=CURRENT_TIMESTAMP WHERE id=1`, key)
	return err
}

func telegramReportPublicSettings(stored telegramReportStoredSettings) TelegramReportSettings {
	settings := stored.TelegramReportSettings
	settings.Configured = stored.BotTokenCiphertext != "" && settings.ChatID != ""
	settings.SecretStable = !jwtSecretEphemeral
	if stored.BotTokenCiphertext != "" {
		settings.BotToken, _ = decryptTelegramBotToken(stored.BotTokenCiphertext)
	}
	return settings
}

func parseTelegramReportTime(value string) (hour, minute int, err error) {
	if !telegramReportTimePattern.MatchString(value) {
		return 0, 0, fmt.Errorf("invalid schedule_time")
	}
	hour, _ = strconv.Atoi(value[:2])
	minute, _ = strconv.Atoi(value[3:])
	return hour, minute, nil
}

func telegramReportDue(now time.Time, settings telegramReportSettingsView) (string, bool) {
	now = now.In(timezoneLocation(settings.Timezone))
	hour, minute, err := parseTelegramReportTime(settings.ScheduleTime)
	if err != nil || !settings.Enabled || !settings.Configured {
		return "", false
	}
	if settings.Frequency == "weekly" && int(now.Weekday()) != settings.Weekday {
		return "", false
	}
	cutoff := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if now.Before(cutoff) {
		return "", false
	}
	if settings.Frequency == "weekly" {
		year, week := now.ISOWeek()
		return fmt.Sprintf("weekly:%04d-W%02d", year, week), true
	}
	return "daily:" + now.Format("2006-01-02"), true
}

type telegramReportSettingsView struct {
	Enabled      bool
	Configured   bool
	ScheduleTime string
	Frequency    string
	Weekday      int
	LastSentKey  string
	Timezone     int
}

// telegramReportHTTPClient is a narrow injection point for redirect and
// timeout regression tests. Production callers receive a fresh client with
// redirects disabled because the bot token is embedded in the request URL.
var telegramReportHTTPClient = func() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (d *DB) telegramReportSettingsView() (telegramReportSettingsView, telegramReportStoredSettings, error) {
	stored, err := d.telegramReportSettings()
	if err != nil {
		return telegramReportSettingsView{}, stored, err
	}
	return telegramReportSettingsView{
		Enabled: stored.Enabled, Configured: stored.Configured, ScheduleTime: stored.ScheduleTime,
		Frequency: stored.Frequency, Weekday: stored.Weekday, LastSentKey: stored.LastSentKey,
		Timezone: d.currentSystemSettings().ScheduleTimezone,
	}, stored, nil
}

func (d *DB) buildTelegramReportStats(now time.Time) (telegramReportStats, error) {
	settings := d.currentSystemSettings()
	location := timezoneLocation(settings.ScheduleTimezone)
	billingMode := settings.TrafficBillingMode
	localNow := now.In(location)
	todayStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location)
	tomorrow := todayStart.AddDate(0, 0, 1)
	stats := telegramReportStats{
		GeneratedAt:        localNow,
		BillingMode:        trafficBillingModeLabel(billingMode),
		TrafficWarnPercent: d.telegramReportWarningPercent(),
	}
	if err := d.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(enabled),0) FROM sites`).Scan(&stats.SiteCount, &stats.RunningSiteCount); err != nil {
		return stats, err
	}
	if err := d.db.QueryRow(`SELECT COUNT(DISTINCT client_ip), COALESCE(SUM(CASE WHEN resource_category IN ('video','stream','manifest','segment') THEN 1 ELSE 0 END),0) FROM request_logs WHERE recorded_at_ms>=? AND recorded_at_ms<?`, todayStart.UnixMilli(), tomorrow.UnixMilli()).Scan(&stats.UniqueClients, &stats.VideoRequests); err != nil {
		return stats, err
	}
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM request_logs WHERE recorded_at_ms>=? AND recorded_at_ms<?`, todayStart.UnixMilli(), tomorrow.UnixMilli()).Scan(&stats.Requests); err != nil {
		return stats, err
	}
	if err := d.db.QueryRow(`SELECT COALESCE(MAX(active_clients),0) FROM (SELECT COUNT(DISTINCT client_ip) AS active_clients FROM request_logs WHERE recorded_at_ms>=? AND recorded_at_ms<? GROUP BY CAST(recorded_at_ms/60000 AS INTEGER))`, todayStart.UnixMilli(), tomorrow.UnixMilli()).Scan(&stats.ActivePeak); err != nil {
		return stats, err
	}
	peakStart, peakEnd, peakRequests, hasPeak, peakErr := d.telegramReportPeakWindow(todayStart, tomorrow, location)
	if peakErr != nil {
		return stats, peakErr
	}
	stats.PeakStart, stats.PeakEnd, stats.PeakRequests, stats.HasPeakWindow = peakStart, peakEnd, peakRequests, hasPeak
	trafficSum := func(start time.Time) (int64, error) {
		// Agent-served sites account through node_site_traffic_logs; summing
		// only traffic_logs made the report show ~0 traffic on agent
		// deployments while the dashboard showed the real usage.
		var bytesIn, bytesOut sql.NullInt64
		err := d.db.QueryRow(`SELECT SUM(bytes_in), SUM(bytes_out) FROM (
				SELECT bytes_in, bytes_out FROM traffic_logs WHERE recorded_at>=? AND recorded_at<?
				UNION ALL
				SELECT bytes_in, bytes_out FROM node_site_traffic_logs WHERE recorded_at_ms>=? AND recorded_at_ms<?
			)`, trafficMinuteBucket(start), trafficMinuteBucket(tomorrow), start.UnixMilli(), tomorrow.UnixMilli()).Scan(&bytesIn, &bytesOut)
		if err != nil || !bytesIn.Valid || !bytesOut.Valid {
			return 0, err
		}
		return trafficBillableBytes(billingMode, bytesIn.Int64, bytesOut.Int64), nil
	}
	var err error
	if stats.TodayTraffic, err = trafficSum(todayStart); err != nil {
		return stats, err
	}
	if stats.SevenDayTraffic, err = trafficSum(todayStart.AddDate(0, 0, -6)); err != nil {
		return stats, err
	}
	// The cycle figures follow the panel's own billing cycle: the configured
	// reset day (e.g. the 15th) in the scheduling timezone, charged with the
	// global billing mode. A rolling 30-day window would disagree with the panel.
	cycleStart := trafficCycleStart(localNow, settings.TrafficResetDay, location)
	if !cycleStart.IsZero() {
		if stats.CycleTraffic, err = trafficSum(cycleStart); err != nil {
			return stats, err
		}
		stats.CycleStart = cycleStart
	}
	var lifetimeIn, lifetimeOut int64
	if err := d.db.QueryRow(`SELECT COALESCE(SUM(bytes_in),0), COALESCE(SUM(bytes_out),0) FROM (
			SELECT bytes_in, bytes_out FROM traffic_logs
			UNION ALL
			SELECT bytes_in, bytes_out FROM node_site_traffic_logs
		)`).Scan(&lifetimeIn, &lifetimeOut); err != nil {
		return stats, err
	}
	stats.LifetimeTraffic = trafficBillableBytes(billingMode, lifetimeIn, lifetimeOut)

	requestRows, err := d.db.Query(`SELECT request_logs.site_id,
		COALESCE(NULLIF(sites.name,''), NULLIF(MAX(request_logs.site_name),''), '站点 ' || request_logs.site_id),
		COUNT(*)
		FROM request_logs
		LEFT JOIN sites ON sites.id=request_logs.site_id
		WHERE request_logs.recorded_at_ms>=? AND request_logs.recorded_at_ms<?
		GROUP BY request_logs.site_id, sites.name`, todayStart.UnixMilli(), tomorrow.UnixMilli())
	if err != nil {
		return stats, err
	}
	requestBySite := make(map[int64]*telegramReportSiteStat)
	for requestRows.Next() {
		var id, requests int64
		var name string
		if err := requestRows.Scan(&id, &name, &requests); err != nil {
			requestRows.Close()
			return stats, err
		}
		requestBySite[id] = &telegramReportSiteStat{Name: name, Requests: requests}
	}
	if err := requestRows.Err(); err != nil {
		requestRows.Close()
		return stats, err
	}
	requestRows.Close()
	trafficRows, err := d.db.Query(`SELECT source.site_id, COALESCE(NULLIF(sites.name,''), '站点 ' || source.site_id), COALESCE(SUM(source.bytes_in),0), COALESCE(SUM(source.bytes_out),0) FROM (
			SELECT site_id, bytes_in, bytes_out FROM traffic_logs WHERE recorded_at>=? AND recorded_at<?
			UNION ALL
			SELECT site_id, bytes_in, bytes_out FROM node_site_traffic_logs WHERE recorded_at_ms>=? AND recorded_at_ms<?
		) AS source LEFT JOIN sites ON sites.id=source.site_id GROUP BY source.site_id, sites.name`, trafficMinuteBucket(todayStart), trafficMinuteBucket(tomorrow), todayStart.UnixMilli(), tomorrow.UnixMilli())
	if err != nil {
		return stats, err
	}
	for trafficRows.Next() {
		var id, bytesIn, bytesOut int64
		var name string
		if err := trafficRows.Scan(&id, &name, &bytesIn, &bytesOut); err != nil {
			trafficRows.Close()
			return stats, err
		}
		traffic := trafficBillableBytes(billingMode, bytesIn, bytesOut)
		if stat := requestBySite[id]; stat != nil {
			stat.Traffic = traffic
		} else {
			requestBySite[id] = &telegramReportSiteStat{Name: name, Traffic: traffic}
		}
	}
	if err := trafficRows.Err(); err != nil {
		trafficRows.Close()
		return stats, err
	}
	trafficRows.Close()
	for _, stat := range requestBySite {
		stats.TopRequests = append(stats.TopRequests, *stat)
		stats.TopTraffic = append(stats.TopTraffic, *stat)
	}
	sort.Slice(stats.TopRequests, func(i, j int) bool { return stats.TopRequests[i].Requests > stats.TopRequests[j].Requests })
	sort.Slice(stats.TopTraffic, func(i, j int) bool { return stats.TopTraffic[i].Traffic > stats.TopTraffic[j].Traffic })
	if len(stats.TopRequests) > 5 {
		stats.TopRequests = stats.TopRequests[:5]
	}
	if len(stats.TopTraffic) > 5 {
		stats.TopTraffic = stats.TopTraffic[:5]
	}
	// One client can appear under several versions, so the raw user agents are
	// over-fetched and folded together in Go; a bare LIMIT 5 would drop a client
	// whose versions are individually small but together rank in the top five.
	uaRows, err := d.db.Query(`SELECT user_agent, COUNT(*) FROM request_logs WHERE recorded_at_ms>=? AND recorded_at_ms<? AND user_agent<>'' GROUP BY user_agent ORDER BY COUNT(*) DESC LIMIT 200`, todayStart.UnixMilli(), tomorrow.UnixMilli())
	if err != nil {
		return stats, err
	}
	for uaRows.Next() {
		var ua string
		var count int64
		if err := uaRows.Scan(&ua, &count); err != nil {
			uaRows.Close()
			return stats, err
		}
		// Several versions of the same client must fold into one row, so the
		// version is stripped before the counts are summed rather than after.
		name := telegramReportClientName(ua)
		if name == "" {
			name = "未知客户端"
		}
		stats.TopUserAgents = append(stats.TopUserAgents, struct {
			Name  string
			Count int64
		}{Name: name, Count: count})
	}
	uaRows.Close()
	stats.TopUserAgents = aggregateTelegramReportClients(stats.TopUserAgents, 5)
	retentionRows, err := d.db.Query(`SELECT name, account_retention_days, account_retention_started_at_ms, account_retention_last_completed_at_ms
		FROM sites WHERE account_retention_days>0 ORDER BY sort_order, id`)
	if err != nil {
		return stats, err
	}
	var retentionScanErr error
	for retentionRows.Next() {
		var site Site
		if err := retentionRows.Scan(&site.Name, &site.AccountRetentionDays, &site.AccountRetentionStartedMS, &site.AccountRetentionCompletedMS); err != nil {
			retentionScanErr = err
			break
		}
		status := accountRetentionStatusAt(site, localNow, location)
		if !status.Enabled {
			continue
		}
		stats.RetentionSites = append(stats.RetentionSites, telegramReportRetentionStat{
			Name:           site.Name,
			RemainingDays:  status.RemainingDays,
			CompletedToday: status.CompletedToday,
		})
	}
	retentionRowsErr := retentionRows.Err()
	retentionRowsCloseErr := retentionRows.Close()
	if retentionScanErr != nil {
		return stats, retentionScanErr
	}
	if retentionRowsErr != nil {
		return stats, retentionRowsErr
	}
	if retentionRowsCloseErr != nil {
		return stats, retentionRowsCloseErr
	}
	sort.SliceStable(stats.RetentionSites, func(i, j int) bool {
		if stats.RetentionSites[i].CompletedToday != stats.RetentionSites[j].CompletedToday {
			return !stats.RetentionSites[i].CompletedToday
		}
		if stats.RetentionSites[i].RemainingDays != stats.RetentionSites[j].RemainingDays {
			return stats.RetentionSites[i].RemainingDays < stats.RetentionSites[j].RemainingDays
		}
		return stats.RetentionSites[i].Name < stats.RetentionSites[j].Name
	})
	if err := d.appendTelegramReportDeployment(&stats, settings, localNow, todayStart, tomorrow); err != nil {
		return stats, err
	}
	return stats, nil
}

// telegramReportNodeStats measures every landing node. assignedOnly limits the
// result to nodes that are actually carrying an enabled site, which is what the
// daily report lists. The threshold alert passes false: a node with no sites can
// still be over its quota, and silently ignoring it would hide a real cost.
//
// The charge follows the panel exactly: a node's period counters already carry
// the Agent's own billing formula, so they are summed (bidirectional) or taken
// as transmit only (outbound) and the manual offset is added on top. Running
// them through trafficBillableBytes would charge every byte twice.
func (d *DB) telegramReportNodeStats(localNow, todayStart, tomorrow time.Time, billingMode string, assignedOnly bool) ([]telegramReportNodeStat, error) {
	todayStartMS, tomorrowMS := todayStart.UnixMilli(), tomorrow.UnixMilli()
	query := `
		SELECT n.id, n.name, n.billing_mode, n.traffic_quota, n.period_rx_bytes, n.period_tx_bytes,
			n.traffic_manual_offset_bytes, n.cycle_started_at_ms, COUNT(sch.site_id),
			COALESCE((SELECT SUM(t.bytes_in) FROM node_site_traffic_logs t WHERE t.node_id=n.id AND t.recorded_at_ms>=? AND t.recorded_at_ms<?),0),
			COALESCE((SELECT SUM(t.bytes_out) FROM node_site_traffic_logs t WHERE t.node_id=n.id AND t.recorded_at_ms>=? AND t.recorded_at_ms<?),0)
		FROM control_nodes n`
	if assignedOnly {
		query += `
		JOIN site_node_schedules sch ON sch.applied_node_id=n.id AND sch.enabled=1
		JOIN sites s ON s.id=sch.site_id AND s.enabled=1`
	} else {
		query += `
		LEFT JOIN site_node_schedules sch ON sch.applied_node_id=n.id AND sch.enabled=1`
	}
	query += `
		GROUP BY n.id, n.name, n.billing_mode, n.traffic_quota, n.period_rx_bytes, n.period_tx_bytes,
			n.traffic_manual_offset_bytes, n.cycle_started_at_ms
		ORDER BY COUNT(sch.site_id) DESC, n.name`

	rows, err := d.db.Query(query, todayStartMS, tomorrowMS, todayStartMS, tomorrowMS)
	if err != nil {
		return nil, err
	}
	nodes := make([]telegramReportNodeStat, 0)
	for rows.Next() {
		var node telegramReportNodeStat
		var quota, periodIn, periodOut, manualOffset, sites, bytesIn, bytesOut int64
		var nodeBillingMode string
		if err := rows.Scan(&node.ID, &node.Name, &nodeBillingMode, &quota, &periodIn, &periodOut, &manualOffset, &node.CycleStartMS, &sites, &bytesIn, &bytesOut); err != nil {
			rows.Close()
			return nil, err
		}
		node.SiteCount = int(sites)
		node.TodayTraffic = trafficBillableBytes(billingMode, bytesIn, bytesOut)
		node.CycleTraffic = saturatingAddInt64(telegramReportNodeChargedBytes(nodeBillingMode, periodIn, periodOut), manualOffset)
		if node.CycleTraffic < 0 {
			node.CycleTraffic = 0
		}
		if quota > 0 {
			node.HasQuota = true
			node.Limit = quota
			node.Remaining = quota - node.CycleTraffic
			if node.Remaining < 0 {
				node.Remaining = 0
			}
		}
		nodes = append(nodes, node)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return nodes, nil
}

// appendTelegramReportDeployment fills the landing-node section and the quota
// proximity warning. It is a separate pass because both read the node/site
// tables rather than request_logs, and a failure here must be a real error: the
// report would otherwise claim a healthy deployment with no nodes listed.
func (d *DB) appendTelegramReportDeployment(stats *telegramReportStats, settings SystemSettings, localNow, todayStart, tomorrow time.Time) error {
	location := timezoneLocation(settings.ScheduleTimezone)
	billingMode := settings.TrafficBillingMode

	nodes, err := d.telegramReportNodeStats(localNow, todayStart, tomorrow, billingMode, true)
	if err != nil {
		return err
	}
	stats.Nodes = nodes

	if err := d.db.QueryRow(`SELECT COUNT(*) FROM sites s WHERE s.enabled=1 AND NOT EXISTS (
			SELECT 1 FROM site_node_schedules sch WHERE sch.site_id=s.id AND sch.enabled=1 AND sch.applied_node_id IS NOT NULL
		)`).Scan(&stats.ControllerSiteCount); err != nil {
		return err
	}

	warnings := make([]telegramReportTrafficWarning, 0)
	if stats.TrafficWarnPercent > telegramReportTrafficWarnDisableValue {
		for _, node := range nodes {
			if !node.HasQuota {
				continue
			}
			warning, ok := telegramReportProximityWarning("node", node.Name, node.CycleTraffic, node.Limit, stats.TrafficWarnPercent)
			if ok {
				warnings = append(warnings, warning)
			}
		}
		siteRows, err := d.db.Query(`SELECT id, name, traffic_quota FROM sites WHERE enabled=1 AND traffic_quota>0 ORDER BY name`)
		if err != nil {
			return err
		}
		cycleStart := trafficCycleStart(localNow, settings.TrafficResetDay, location)
		type quotaSite struct {
			id    int64
			name  string
			quota int64
		}
		quotaSites := make([]quotaSite, 0)
		for siteRows.Next() {
			var site quotaSite
			if err := siteRows.Scan(&site.id, &site.name, &site.quota); err != nil {
				siteRows.Close()
				return err
			}
			quotaSites = append(quotaSites, site)
		}
		if err := siteRows.Err(); err != nil {
			siteRows.Close()
			return err
		}
		if err := siteRows.Close(); err != nil {
			return err
		}
		for _, site := range quotaSites {
			// Reuse the enforcement helper so the warning line and the request
			// admission path can never disagree about cycle usage.
			used, err := d.SumTrafficSinceForSite(site.id, cycleStart, billingMode)
			if err != nil {
				return err
			}
			warning, ok := telegramReportProximityWarning("site", site.name, used, site.quota, stats.TrafficWarnPercent)
			if ok {
				warnings = append(warnings, warning)
			}
		}
	}
	sort.SliceStable(warnings, func(i, j int) bool {
		left, right := warnings[i], warnings[j]
		if left.Kind != right.Kind {
			return left.Kind == "node"
		}
		leftRatio := float64(left.Used) / float64(left.Limit)
		rightRatio := float64(right.Used) / float64(right.Limit)
		if leftRatio != rightRatio {
			return leftRatio > rightRatio
		}
		return left.Name < right.Name
	})
	stats.TrafficWarnings = warnings
	return nil
}

// telegramReportProximityWarning reports whether used has reached warnPercent of
// the configured limit. The limit is passed in rather than derived from
// used+remaining: Remaining is clamped at zero once the quota is exhausted, so
// deriving it there would make a spent quota look like a quota that is exactly
// spent, and near-spent traffic would read as exactly at the line.
//
// A limit of zero or less means unmetered on both nodes and sites, so it never
// warns.
func telegramReportProximityWarning(kind, name string, used, limit int64, warnPercent int) (telegramReportTrafficWarning, bool) {
	if warnPercent <= telegramReportTrafficWarnDisableValue || limit <= 0 {
		return telegramReportTrafficWarning{}, false
	}
	if !telegramReportRatioAtLeastPercent(used, limit, warnPercent) {
		return telegramReportTrafficWarning{}, false
	}
	remaining := limit - used
	if remaining < 0 {
		remaining = 0
	}
	return telegramReportTrafficWarning{Kind: kind, Name: name, Used: used, Limit: limit, Remaining: remaining}, true
}

// telegramReportRatioAtLeastPercent compares used/limit against a percentage
// without the int64 overflow of the obvious `used*100 < limit*percent`: both
// products are taken as 128-bit values. A quota above ~92 PB is unrealistic, but
// a wrapped product compares negative and silently drops a real warning, which
// is exactly the failure this feature exists to prevent.
//
// Callers must pass a non-negative used and a positive limit.
func telegramReportRatioAtLeastPercent(used, limit int64, percent int) bool {
	// #nosec G115 -- byte counters are never negative and the caller has already
	// rejected a non-positive limit; only the wrap-around of the two 64-bit
	// products is in question, and the wide comparison below removes it.
	return telegramReportUint64RatioAtLeastPercent(uint64(used), uint64(limit), uint64(percent))
}

func telegramReportUint64RatioAtLeastPercent(used, limit, percent uint64) bool {
	usedHigh, usedLow := bits.Mul64(used, 100)
	limitHigh, limitLow := bits.Mul64(limit, percent)
	if usedHigh != limitHigh {
		return usedHigh > limitHigh
	}
	return usedLow >= limitLow
}

// telegramReportClientName reduces a raw User-Agent to the product name the
// operator reads: the token before the version separator, with any trailing
// parenthetical platform detail dropped.
//
//	CapyPlayer/1.1.5                -> CapyPlayer
//	Hills/1.9.0-beta.1 (android;17) -> Hills
//	Emby for iOS/2.2.5              -> Emby for iOS
//	VLC/3.0.20 LibVLC/3.0.20        -> VLC
//
// The short form "Hills/1.9 (android; 17)" intentionally keeps "Hills" only:
// a parenthetical always describes the build, never the product.
func telegramReportClientName(userAgent string) string {
	name := strings.TrimSpace(userAgent)
	if name == "" || strings.HasPrefix(name, "(") {
		// A bare parenthetical carries no product name at all.
		return ""
	}
	// Compare against the lower-cased form so the search index still lines up
	// with the original string.
	lower := strings.ToLower(name)
	for _, separator := range []string{"/", " ("} {
		if index := strings.Index(lower, separator); index > 0 {
			name = name[:index]
			lower = lower[:index]
		}
	}
	if index := strings.IndexAny(name, " \t"); index > 0 {
		name = name[:index]
	}
	return strings.TrimSpace(name)
}

// aggregateTelegramReportClients folds per-version rows into one row per client,
// keeps the heaviest, and orders them the way the report prints them.
func aggregateTelegramReportClients(rows []struct {
	Name  string
	Count int64
}, limit int) []struct {
	Name  string
	Count int64
} {
	totals := make(map[string]int64, len(rows))
	order := make([]string, 0, len(rows))
	for _, row := range rows {
		if _, seen := totals[row.Name]; !seen {
			order = append(order, row.Name)
		}
		totals[row.Name] += row.Count
	}
	merged := make([]struct {
		Name  string
		Count int64
	}, 0, len(order))
	for _, name := range order {
		merged = append(merged, struct {
			Name  string
			Count int64
		}{Name: name, Count: totals[name]})
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].Count != merged[j].Count {
			return merged[i].Count > merged[j].Count
		}
		return merged[i].Name < merged[j].Name
	})
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged
}

// telegramReportNodeChargedBytes applies a node's own billing mode to its
// period counters. This mirrors scanControlNode exactly: a bidirectional node
// sums both directions, an outbound node takes the transmit direction only.
//
// It deliberately differs from trafficBillableBytes, which converts the raw
// relayed directions recorded in traffic_logs into the VPS billing convention
// by doubling them. A node's period counters have already been through that
// conversion, so doubling them again is the double-charge this function avoids.
func telegramReportNodeChargedBytes(billingMode string, rx, tx int64) int64 {
	if billingMode == trafficBillingModeOutbound {
		return tx
	}
	return saturatingAddInt64(rx, tx)
}

// telegramReportPeakWindow finds the busiest wall-clock hour of the day and how
// many requests fell inside it. Shifting the timestamps by the day's own offset
// keeps the buckets aligned with local hour boundaries even for a timezone whose
// offset is not a whole hour.
func (d *DB) telegramReportPeakWindow(todayStart, tomorrow time.Time, location *time.Location) (start, end time.Time, requests int64, ok bool, err error) {
	base := todayStart.In(location)
	midnight := time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, location)
	offsetMS := midnight.UnixMilli()
	dayEnd := midnight.AddDate(0, 0, 1)
	var bucket, count int64
	err = d.db.QueryRow(`SELECT (recorded_at_ms - ?) / 3600000 AS hour_bucket, COUNT(*)
		FROM request_logs
		WHERE recorded_at_ms>=? AND recorded_at_ms<?
		GROUP BY hour_bucket
		ORDER BY COUNT(*) DESC, hour_bucket ASC
		LIMIT 1`, offsetMS, offsetMS, dayEnd.UnixMilli()).Scan(&bucket, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, time.Time{}, 0, false, nil
	}
	if err != nil {
		return time.Time{}, time.Time{}, 0, false, err
	}
	windowStart := midnight.Add(time.Duration(bucket) * time.Hour)
	return windowStart, windowStart.Add(time.Hour), count, true, nil
}

func formatTelegramBytes(value int64) string {
	if value < 1024 {
		return fmt.Sprintf("%d B", value)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	f := float64(value)
	for _, unit := range units {
		f /= 1024
		if f < 1024 || unit == "TB" {
			return fmt.Sprintf("%.2f %s", f, unit)
		}
	}
	return fmt.Sprintf("%d B", value)
}

func buildTelegramReportMessage(stats telegramReportStats) string {
	var b strings.Builder
	b.WriteString("📊 Meridian 数据日报\n")
	b.WriteString("━━━━━━━━━━━━━━━━━━━━\n\n")
	// The reader wants the moment first: a report is only actionable if the
	// window it covers is unambiguous.
	fmt.Fprintf(&b, "⏱️ 统计时间：%s\n\n", stats.GeneratedAt.Format("2006-01-02 15:04"))

	b.WriteString("✨ 今日概览\n")
	// No list bullets here: these are the headline figures, and the operator
	// asked for this block to read without them.
	fmt.Fprintf(&b, "📈 请求总数：%d 次\n", stats.Requests)
	fmt.Fprintf(&b, "🎬 视频请求：%d 次\n", stats.VideoRequests)
	fmt.Fprintf(&b, "🗂 媒体库站点：%d 个（启用 %d 个）\n", stats.SiteCount, stats.RunningSiteCount)
	// The peak line reports the busiest hour of the day and how many requests
	// landed in it, which is a time window rather than a head count.
	if stats.HasPeakWindow {
		fmt.Fprintf(&b, "⏰ 活跃高峰：%s - %s（%d 次）\n", stats.PeakStart.Format("15:04"), stats.PeakEnd.Format("15:04"), stats.PeakRequests)
	} else {
		b.WriteString("⏰ 活跃高峰：暂无数据\n")
	}
	if len(stats.TopTraffic) > 0 {
		hottest := stats.TopTraffic[0]
		fmt.Fprintf(&b, "🏆 今日最热媒体库：%s（%s）\n", truncateTelegramText(hottest.Name, 48), formatTelegramBytes(hottest.Traffic))
	} else {
		b.WriteString("🏆 今日最热媒体库：暂无数据\n")
	}
	b.WriteString("\n")

	b.WriteString("📍 服务器部署信息\n")
	if len(stats.Nodes) == 0 {
		b.WriteString("• 暂无落地节点\n")
	}
	for _, node := range stats.Nodes {
		remaining := "未设置额度"
		if node.HasQuota {
			remaining = formatTelegramBytes(node.Remaining)
		}
		fmt.Fprintf(&b, "• %s（%d 个站点）：今日 %s 丨 当月 %s 丨 剩余 %s\n",
			truncateTelegramText(node.Name, 48), node.SiteCount, formatTelegramBytes(node.TodayTraffic), formatTelegramBytes(node.CycleTraffic), remaining)
	}
	b.WriteString("\n")

	b.WriteString("🧩 客户端分布\n")
	if len(stats.TopUserAgents) == 0 {
		b.WriteString("• 暂无客户端数据\n")
	} else {
		var total int64
		for _, item := range stats.TopUserAgents {
			total += item.Count
		}
		for _, item := range stats.TopUserAgents {
			share := float64(0)
			if total > 0 {
				share = float64(item.Count) / float64(total) * 100
			}
			fmt.Fprintf(&b, "• %s：%d 次（%.1f%%）\n", truncateTelegramText(item.Name, 48), item.Count, share)
		}
	}
	b.WriteString("\n")

	b.WriteString("🌐 流量统计\n")
	fmt.Fprintf(&b, "• 当天：%s\n", formatTelegramBytes(stats.TodayTraffic))
	fmt.Fprintf(&b, "• 七天内：%s\n", formatTelegramBytes(stats.SevenDayTraffic))
	if stats.CycleStart.IsZero() {
		fmt.Fprintf(&b, "• 当月流量：%s\n", formatTelegramBytes(stats.CycleTraffic))
	} else {
		fmt.Fprintf(&b, "• 当月流量（%s 起）：%s\n", stats.CycleStart.Format("01-02"), formatTelegramBytes(stats.CycleTraffic))
	}
	fmt.Fprintf(&b, "• 计费口径：%s\n", telegramReportBillingSummary(stats.BillingMode))
	b.WriteString("\n")

	b.WriteString("🔥 今日站点热度 TOP 5\n")
	if len(stats.TopTraffic) == 0 {
		b.WriteString("• 暂无流量数据\n")
	} else {
		limit := len(stats.TopTraffic)
		if limit > 5 {
			limit = 5
		}
		for index := 0; index < limit; index++ {
			item := stats.TopTraffic[index]
			// The rank is driven by traffic, so the traffic figure leads and the
			// request count follows as the secondary number on the same line.
			fmt.Fprintf(&b, "%s %s：%s 丨 %d 次请求\n", telegramReportRankPrefix(index), truncateTelegramText(item.Name, 48), formatTelegramBytes(item.Traffic), item.Requests)
		}
	}
	b.WriteString("\n")

	if len(stats.TrafficWarnings) > 0 {
		fmt.Fprintf(&b, "⚠️ 流量预警（已达 %d%%）\n", stats.TrafficWarnPercent)
		for _, warning := range stats.TrafficWarnings {
			label := "节点"
			if warning.Kind == "site" {
				label = "站点"
			}
			ratio := float64(0)
			if warning.Limit > 0 {
				ratio = float64(warning.Used) / float64(warning.Limit) * 100
			}
			marker := "🟠"
			if ratio >= 100 {
				marker = "🔴"
			}
			fmt.Fprintf(&b, "• %s %s %s：已用 %.1f%%（剩余 %s）\n",
				marker, label, truncateTelegramText(warning.Name, 40), ratio, formatTelegramBytes(warning.Remaining))
		}
		b.WriteString("\n")
	}

	if len(stats.RetentionSites) > 0 {
		b.WriteString("🔔 保号提醒\n")
		for _, item := range stats.RetentionSites {
			name := truncateTelegramText(item.Name, 72)
			switch {
			case item.CompletedToday:
				fmt.Fprintf(&b, "• %s：✅ 完成保号\n", name)
			case item.RemainingDays <= 0:
				fmt.Fprintf(&b, "• %s：🔴 已到期\n", name)
			case item.RemainingDays <= 7:
				fmt.Fprintf(&b, "• %s：🔴 剩余 %d 天\n", name, item.RemainingDays)
			default:
				fmt.Fprintf(&b, "• %s：剩余 %d 天\n", name, item.RemainingDays)
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("🚀 System Status：Operational")
	message := b.String()
	if len(message) > telegramReportMaxMessageBytes {
		message = truncateTelegramText(message, telegramReportMaxMessageBytes)
	}
	return message
}

// telegramReportRankPrefix marks a rank in the daily heat list: the crown and
// the star carry the top two, then the keycap digits. The keycap form is what
// the operator asked for, and it is used consistently for ranks three onward so
// the list never mixes an emoji marker with a bare digit.
func telegramReportRankPrefix(index int) string {
	switch index {
	case 0:
		return "👑"
	case 1:
		return "🌟"
	default:
		return fmt.Sprintf("%d\ufe0f\u20e3", index+1)
	}
}

func telegramReportBillingSummary(mode string) string {
	if mode == trafficBillingModeOutbound {
		return "仅下行计费"
	}
	return "上下行双向计费"
}

func truncateTelegramText(value string, maxBytes int) string {
	value = strings.TrimSpace(value)
	if maxBytes < 4 || len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes-3]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "..."
}

func sendTelegramReport(ctx context.Context, botToken, chatID, message string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	botToken = strings.TrimSpace(botToken)
	chatID = strings.TrimSpace(chatID)
	if botToken == "" || chatID == "" {
		return fmt.Errorf("telegram bot token and chat ID are required")
	}
	payload, err := json.Marshal(map[string]any{"chat_id": chatID, "text": message, "disable_web_page_preview": true})
	if err != nil {
		return err
	}
	endpoint := "https://api.telegram.org/bot" + url.PathEscape(botToken) + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := telegramReportHTTPClient()
	if client == nil {
		return errors.New("telegram HTTP client is unavailable")
	}
	resp, err := client.Do(req)
	if err != nil {
		// url.Error prints the full request URL, which embeds the bot token
		// in the path; strip it so logs and API error responses never leak the
		// credential on DNS/timeout/TLS failures.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("telegram request to api.telegram.org failed: %w", urlErr.Err)
		}
		return fmt.Errorf("telegram request failed: %w", err)
	}
	defer resp.Body.Close()
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !result.OK {
		return fmt.Errorf("telegram API rejected message: %s", result.Description)
	}
	return nil
}

func (a *App) handleTelegramReport(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		stored, err := a.db.telegramReportSettings()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "failed to read Telegram report settings")
			return
		}
		a.jsonOK(w, telegramReportPublicSettings(stored))
	case http.MethodPost:
		var input telegramReportInput
		if err := json.NewDecoder(io.LimitReader(r.Body, 32<<10)).Decode(&input); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid Telegram report settings")
			return
		}
		stored, err := a.db.telegramReportSettings()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "failed to read Telegram report settings")
			return
		}
		settings := stored.TelegramReportSettings
		if input.Enabled != nil {
			settings.Enabled = *input.Enabled
		}
		if input.ChatID != "" {
			settings.ChatID = strings.TrimSpace(input.ChatID)
		}
		if input.ScheduleTime != "" {
			settings.ScheduleTime = strings.TrimSpace(input.ScheduleTime)
		}
		if input.Frequency != "" {
			settings.Frequency = strings.TrimSpace(input.Frequency)
		}
		if input.Weekday != nil {
			if *input.Weekday < 0 || *input.Weekday > 6 {
				a.jsonErr(w, http.StatusBadRequest, "weekday must be between 0 and 6")
				return
			}
			settings.Weekday = *input.Weekday
		}
		// A pointer keeps the stored threshold when the field is absent, which is
		// what an older client sends, while still letting 0 mean "no warnings".
		if input.TrafficWarningPercent != nil {
			settings.TrafficWarningPercent = *input.TrafficWarningPercent
		}
		settings, err = normalizeTelegramReportSettings(settings)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if len(settings.ChatID) > 128 {
			a.jsonErr(w, http.StatusBadRequest, "chat_id is too long")
			return
		}
		ciphertext := stored.BotTokenCiphertext
		replaceToken := false
		if input.ClearBotToken {
			ciphertext, replaceToken = "", true
		}
		if strings.TrimSpace(input.BotToken) != "" {
			ciphertext, err = encryptTelegramBotToken(input.BotToken)
			if err != nil {
				a.jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
			replaceToken = true
		}
		if settings.Enabled && (ciphertext == "" || settings.ChatID == "") {
			a.jsonErr(w, http.StatusBadRequest, "启用日报前必须配置 Bot Token 和 Chat ID")
			return
		}
		if input.Action == "test" {
			if ciphertext == "" || settings.ChatID == "" {
				a.jsonErr(w, http.StatusBadRequest, "请先配置 Bot Token 和 Chat ID")
				return
			}
			token, decryptErr := decryptTelegramBotToken(ciphertext)
			if decryptErr != nil {
				a.jsonErr(w, http.StatusInternalServerError, "Telegram Bot Token 无法解密，请重新保存")
				return
			}
			stats, statsErr := a.db.buildTelegramReportStats(time.Now())
			if statsErr != nil {
				a.jsonErr(w, http.StatusInternalServerError, "failed to build daily report")
				return
			}
			if sendErr := sendTelegramReport(r.Context(), token, settings.ChatID, buildTelegramReportMessage(stats)); sendErr != nil {
				a.jsonErr(w, http.StatusBadGateway, sendErr.Error())
				return
			}
			a.jsonOK(w, map[string]any{"sent": true})
			return
		}
		if replaceToken && ciphertext == "" {
			settings.Enabled = false
		}
		if err := a.db.saveTelegramReportSettings(settings, ciphertext, replaceToken); err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		fresh, err := a.db.telegramReportSettings()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "failed to read Telegram report settings")
			return
		}
		a.jsonOK(w, telegramReportPublicSettings(fresh))
	default:
		w.Header().Set("Allow", "GET, POST")
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func runTelegramReportScheduler(ctx context.Context, db *DB) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	state := &telegramReportSchedulerState{}
	for {
		select {
		case <-ticker.C:
			runTelegramReportSchedulerTick(ctx, db, state, time.Now(), sendTelegramReport)
		case <-ctx.Done():
			return
		}
	}
}

type telegramReportSchedulerState struct {
	lastDeliveredFingerprint string
}

type telegramReportSender func(context.Context, string, string, string) error

func runTelegramReportSchedulerTick(ctx context.Context, db *DB, state *telegramReportSchedulerState, now time.Time, send telegramReportSender) {
	if db == nil || state == nil || send == nil {
		return
	}
	view, stored, err := db.telegramReportSettingsView()
	if err != nil || !view.Enabled || !view.Configured {
		return
	}
	key, due := telegramReportDue(now, view)
	fingerprint := strings.Join([]string{key, view.ScheduleTime, view.Frequency, strconv.Itoa(view.Weekday), stored.ChatID, stored.BotTokenCiphertext}, "\x00")
	if !due || key == view.LastSentKey || fingerprint == state.lastDeliveredFingerprint {
		return
	}
	token, err := decryptTelegramBotToken(stored.BotTokenCiphertext)
	if err != nil {
		log.Printf("[telegram-report] bot token decrypt failed: %v", err)
		return
	}
	stats, err := db.buildTelegramReportStats(now)
	if err != nil {
		log.Printf("[telegram-report] build report failed: %v", err)
		return
	}
	if err := send(ctx, token, stored.ChatID, buildTelegramReportMessage(stats)); err != nil {
		log.Printf("[telegram-report] send failed: %v", err)
		return
	}
	// Remember successful external delivery before persisting the marker. This
	// prevents a read-only/full SQLite database from causing a notification
	// storm every 15 seconds. A configuration change produces a new fingerprint
	// and intentionally rearms the current schedule period.
	state.lastDeliveredFingerprint = fingerprint
	if err := db.markTelegramReportSent(key); err != nil {
		log.Printf("[telegram-report] mark sent failed after delivery; suppressing duplicate in this process: %v", err)
		return
	}
	log.Printf("[telegram-report] sent %s", key)
}
