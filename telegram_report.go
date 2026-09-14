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
	Name         string
	TodayTraffic int64
	CycleTraffic int64
	Remaining    int64
	HasQuota     bool
	SiteCount    int
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
	GeneratedAt          time.Time
	UniqueClients        int64
	ActivePeak           int64
	Requests             int64
	VideoRequests        int64
	TodayTraffic         int64
	SevenDayTraffic      int64
	ThirtyDayTraffic     int64
	HistoryTraffic       int64
	BillingMode          string
	SiteCount            int
	RunningSiteCount     int
	ControllerSiteCount  int
	TrafficWarnPercent   int
	TopRequests          []telegramReportSiteStat
	TopTraffic           []telegramReportSiteStat
	Nodes                []telegramReportNodeStat
	TrafficWarnings      []telegramReportTrafficWarning
	RetentionSites       []telegramReportRetentionStat
	TopUserAgents        []struct {
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
	if stats.ThirtyDayTraffic, err = trafficSum(todayStart.AddDate(0, 0, -29)); err != nil {
		return stats, err
	}
	var historyIn, historyOut int64
	if err := d.db.QueryRow(`SELECT COALESCE(SUM(bytes_in),0), COALESCE(SUM(bytes_out),0) FROM (
			SELECT bytes_in, bytes_out FROM traffic_logs
			UNION ALL
			SELECT bytes_in, bytes_out FROM node_site_traffic_logs
		)`).Scan(&historyIn, &historyOut); err != nil {
		return stats, err
	}
	stats.HistoryTraffic = trafficBillableBytes(billingMode, historyIn, historyOut)

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
	uaRows, err := d.db.Query(`SELECT user_agent, COUNT(*) FROM request_logs WHERE recorded_at_ms>=? AND recorded_at_ms<? AND user_agent<>'' GROUP BY user_agent ORDER BY COUNT(*) DESC LIMIT 5`, todayStart.UnixMilli(), tomorrow.UnixMilli())
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
		stats.TopUserAgents = append(stats.TopUserAgents, struct {
			Name  string
			Count int64
		}{Name: ua, Count: count})
	}
	uaRows.Close()
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

// appendTelegramReportDeployment fills the landing-node section and the quota
// proximity warning. It is a separate pass because both read the node/site
// tables rather than request_logs, and a failure here must be a real error: the
// report would otherwise claim a healthy deployment with no nodes listed.
func (d *DB) appendTelegramReportDeployment(stats *telegramReportStats, settings SystemSettings, localNow, todayStart, tomorrow time.Time) error {
	location := timezoneLocation(settings.ScheduleTimezone)
	billingMode := settings.TrafficBillingMode
	todayStartMS, tomorrowMS := todayStart.UnixMilli(), tomorrow.UnixMilli()

	nodeRows, err := d.db.Query(`
		SELECT n.name, n.billing_mode, n.traffic_quota, n.period_rx_bytes, n.period_tx_bytes, n.traffic_manual_offset_bytes,
			COUNT(sch.site_id),
			COALESCE((SELECT SUM(t.bytes_in) FROM node_site_traffic_logs t WHERE t.node_id=n.id AND t.recorded_at_ms>=? AND t.recorded_at_ms<?),0),
			COALESCE((SELECT SUM(t.bytes_out) FROM node_site_traffic_logs t WHERE t.node_id=n.id AND t.recorded_at_ms>=? AND t.recorded_at_ms<?),0)
		FROM control_nodes n
		JOIN site_node_schedules sch ON sch.applied_node_id=n.id AND sch.enabled=1
		JOIN sites s ON s.id=sch.site_id AND s.enabled=1
		GROUP BY n.id, n.name, n.billing_mode, n.traffic_quota, n.period_rx_bytes, n.period_tx_bytes, n.traffic_manual_offset_bytes
		ORDER BY COUNT(sch.site_id) DESC, n.name`, todayStartMS, tomorrowMS, todayStartMS, tomorrowMS)
	if err != nil {
		return err
	}
	nodes := make([]telegramReportNodeStat, 0)
	for nodeRows.Next() {
		var node telegramReportNodeStat
		var quota, periodIn, periodOut, manualOffset, sites, bytesIn, bytesOut int64
		var nodeBillingMode string
		if err := nodeRows.Scan(&node.Name, &nodeBillingMode, &quota, &periodIn, &periodOut, &manualOffset, &sites, &bytesIn, &bytesOut); err != nil {
			nodeRows.Close()
			return err
		}
		node.SiteCount = int(sites)
		node.TodayTraffic = trafficBillableBytes(billingMode, bytesIn, bytesOut)
		// The node charges its own billing cycle with its own stored mode, so the
		// cycle columns must not inherit the panel-wide mode: an outbound node
		// under a bidirectional panel would otherwise report double usage and
		// trip the proximity warning at half its real quota.
		node.CycleTraffic = saturatingAddInt64(trafficBillableBytes(trafficBillingModeLabel(nodeBillingMode), periodIn, periodOut), manualOffset)
		if node.CycleTraffic < 0 {
			node.CycleTraffic = 0
		}
		if quota > 0 {
			node.HasQuota = true
			node.Remaining = quota - node.CycleTraffic
			if node.Remaining < 0 {
				node.Remaining = 0
			}
		}
		nodes = append(nodes, node)
	}
	if err := nodeRows.Err(); err != nil {
		nodeRows.Close()
		return err
	}
	if err := nodeRows.Close(); err != nil {
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
			warning, ok := telegramReportProximityWarning("node", node.Name, node.CycleTraffic, node.Remaining, stats.TrafficWarnPercent)
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
			remaining := site.quota - used
			if remaining < 0 {
				remaining = 0
			}
			warning, ok := telegramReportProximityWarning("site", site.name, used, remaining, stats.TrafficWarnPercent)
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

// telegramReportProximityWarning reports whether used/limit has reached the
// warning line, and returns the pre-charged numbers for rendering. A quota of
// zero means unmetered on both nodes and sites, so it never warns.
func telegramReportProximityWarning(kind, name string, used, remaining int64, warnPercent int) (telegramReportTrafficWarning, bool) {
	limit := used + remaining
	if warnPercent <= telegramReportTrafficWarnDisableValue || limit <= 0 {
		return telegramReportTrafficWarning{}, false
	}
	if !telegramReportRatioAtLeastPercent(used, limit, warnPercent) {
		return telegramReportTrafficWarning{}, false
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
	fmt.Fprintf(&b, "⏱ 统计时间：%s\n\n", stats.GeneratedAt.Format("2006-01-02 15:04"))

	b.WriteString("✨ 今日概览\n")
	fmt.Fprintf(&b, "• 👥 独立访客：%d 人\n", stats.UniqueClients)
	fmt.Fprintf(&b, "• 📈 请求总数：%d 次\n", stats.Requests)
	fmt.Fprintf(&b, "• 🎬 视频请求：%d 次\n", stats.VideoRequests)
	fmt.Fprintf(&b, "• 🗂 媒体库站点：%d 个（启用 %d 个）\n", stats.SiteCount, stats.RunningSiteCount)
	fmt.Fprintf(&b, "• ⚡ 活跃高峰：%d 人/分钟\n", stats.ActivePeak)
	if len(stats.TopTraffic) > 0 {
		hottest := stats.TopTraffic[0]
		fmt.Fprintf(&b, "• 🏆 今日最热媒体库：%s（%s）\n", truncateTelegramText(hottest.Name, 48), formatTelegramBytes(hottest.Traffic))
	} else {
		b.WriteString("• 🏆 今日最热媒体库：暂无数据\n")
	}
	b.WriteString("\n")

	b.WriteString("📍 服务器部署信息\n")
	if len(stats.Nodes) == 0 {
		b.WriteString("• 暂无落地节点\n")
	}
	if len(stats.Nodes) == 1 {
		fmt.Fprintf(&b, "• 所有站点统一通过 %s 中转\n", truncateTelegramText(stats.Nodes[0].Name, 48))
	}
	for _, node := range stats.Nodes {
		remaining := "未设置额度"
		if node.HasQuota {
			remaining = formatTelegramBytes(node.Remaining)
		}
		fmt.Fprintf(&b, "• %s（%d 个站点）：今日 %s 丨 当月 %s 丨 剩余 %s\n",
			truncateTelegramText(node.Name, 48), node.SiteCount, formatTelegramBytes(node.TodayTraffic), formatTelegramBytes(node.CycleTraffic), remaining)
	}
	if stats.ControllerSiteCount > 0 {
		fmt.Fprintf(&b, "• 另有 %d 个站点由控制端直接承载（未走落地节点）\n", stats.ControllerSiteCount)
	}
	if len(stats.Nodes) > 0 {
		online := 0
		for _, node := range stats.Nodes {
			if node.SiteCount > 0 {
				online++
			}
		}
		fmt.Fprintf(&b, "• 共 %d 个落地节点 · 服务中 %d 个 ✅\n", len(stats.Nodes), online)
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
	fmt.Fprintf(&b, "• 30 天内：%s\n", formatTelegramBytes(stats.ThirtyDayTraffic))
	fmt.Fprintf(&b, "• 历史累计：%s\n", formatTelegramBytes(stats.HistoryTraffic))
	fmt.Fprintf(&b, "• 计费口径：%s\n", telegramReportBillingSummary(stats.BillingMode))
	b.WriteString("\n")

	b.WriteString("🔥 今日节点热度 TOP 5\n")
	if len(stats.TopTraffic) == 0 {
		b.WriteString("• 暂无流量数据\n")
	} else {
		limit := len(stats.TopTraffic)
		if limit > 5 {
			limit = 5
		}
		for index := 0; index < limit; index++ {
			item := stats.TopTraffic[index]
			fmt.Fprintf(&b, "%s %s：%s\n", telegramReportRankPrefix(index), truncateTelegramText(item.Name, 48), formatTelegramBytes(item.Traffic))
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

func telegramReportRankPrefix(index int) string {
	switch index {
	case 0:
		return "👑"
	case 1:
		return "🌟"
	default:
		return fmt.Sprintf("%d.", index+1)
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
