package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Traffic threshold alerts are deliberately separate from the daily report: the
// report keeps its own schedule and layout, while an alert goes out as soon as a
// resource is found above the configured line. The daily report's own warning
// section is unchanged, so neither depends on the other.

const (
	// The measurement is cheap but not free: it re-reads every node's counters
	// and every metered site's cycle usage. A minute is far tighter than the
	// reporting cadence that feeds it, and keeps the alert timely.
	trafficAlertCheckInterval = time.Minute
	// How far past the threshold a resource must climb before it is announced
	// again. Five points means a resource at 80% is announced again at 85%, 90%,
	// 95%, 100% and so on: fine enough to track the approach to a limit step by
	// step, coarse enough that a resource parked at one level stays quiet rather
	// than alerting on every tick.
	trafficAlertEscalationStep = 5
	trafficAlertHistoryKept    = 200
)

// telegramTrafficAlert is one resource measured against its quota.
type telegramTrafficAlert struct {
	kind string
	id   int64
	name string
	// bucket is the threshold step this state belongs to and is part of the
	// delivery key, so crossing into a higher step is what re-arms an alert.
	bucket int
	// cycleStartMS identifies the billing cycle for de-duplication.
	cycleStartMS int64

	used      int64
	limit     int64
	remaining int64
	// cycleTraffic and dailyTraffic are message context only.
	cycleTraffic int64
	dailyTraffic int64
}

// telegramReportAlertBucket maps a used/limit state onto its escalation step.
// The steps are multiples of trafficAlertEscalationStep starting at the first
// multiple that is at least the configured threshold, and the result is the
// highest step the usage has reached. With the default 80% threshold and a
// five-point step those are 80, 85, 90, 95, 100 and so on, so a resource is
// announced when it crosses the line and then once per further step.
//
// The comparisons go through telegramReportRatioAtLeastPercent rather than
// scaling the numbers, because a quota large enough to overflow int64 would
// otherwise wrap and silently drop an alert.
func telegramReportAlertBucket(used, limit int64, warnPercent int) int {
	if limit <= 0 || used <= 0 || warnPercent <= telegramReportTrafficWarnDisableValue {
		return 0
	}
	if !telegramReportRatioAtLeastPercent(used, limit, warnPercent) {
		return 0
	}
	step := trafficAlertEscalationStep
	bucket := warnPercent
	for {
		next := bucket + step
		if next <= bucket {
			// The step must advance the bucket. A zero or negative step would
			// otherwise spin here forever, which would hang the scheduler
			// goroutine rather than merely mis-classify one alert.
			return bucket
		}
		if !telegramReportRatioAtLeastPercent(used, limit, next) {
			return bucket
		}
		bucket = next
	}
} // scanTelegramTrafficAlerts measures every metered node and site. It performs no
// de-duplication: it reports the current state, and the caller decides what has
// already been announced.
func (d *DB) scanTelegramTrafficAlerts(now time.Time) ([]telegramTrafficAlert, int, error) {
	if d == nil {
		return nil, 0, fmt.Errorf("traffic alert scan requires a database")
	}
	settings := d.currentSystemSettings()
	warnPercent := d.telegramReportWarningPercent()
	if warnPercent <= telegramReportTrafficWarnDisableValue {
		return nil, warnPercent, nil
	}
	location := timezoneLocation(settings.ScheduleTimezone)
	localNow := now.In(location)
	cycleStart := trafficCycleStart(localNow, settings.TrafficResetDay, location)
	cycleStartMS := int64(0)
	if !cycleStart.IsZero() {
		cycleStartMS = cycleStart.UnixMilli()
	}
	billingMode := settings.TrafficBillingMode
	todayStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location)
	tomorrow := todayStart.AddDate(0, 0, 1)

	nodes, err := d.telegramReportNodeStats(localNow, todayStart, tomorrow, billingMode, false)
	if err != nil {
		return nil, warnPercent, err
	}
	alerts := make([]telegramTrafficAlert, 0, len(nodes))
	for _, node := range nodes {
		if !node.HasQuota {
			continue
		}
		bucket := telegramReportAlertBucket(node.CycleTraffic, node.Limit, warnPercent)
		if bucket == 0 {
			continue
		}
		alerts = append(alerts, telegramTrafficAlert{
			kind: "node", id: node.ID, name: node.Name,
			bucket: bucket, cycleStartMS: node.CycleStartMS,
			used: node.CycleTraffic, limit: node.Limit, remaining: node.Remaining,
			cycleTraffic: node.CycleTraffic, dailyTraffic: node.TodayTraffic,
		})
	}

	metered, err := d.meteredSites()
	if err != nil {
		return nil, warnPercent, err
	}
	for _, site := range metered {
		// The same helper request admission uses, so an alert can never disagree
		// with the limiter about how much of the quota is gone.
		used, err := d.SumTrafficSinceForSite(site.id, cycleStart, billingMode)
		if err != nil {
			return nil, warnPercent, err
		}
		bucket := telegramReportAlertBucket(used, site.quota, warnPercent)
		if bucket == 0 {
			continue
		}
		remaining := site.quota - used
		if remaining < 0 {
			remaining = 0
		}
		alerts = append(alerts, telegramTrafficAlert{
			kind: "site", id: site.id, name: site.name,
			bucket: bucket, cycleStartMS: cycleStartMS,
			used: used, limit: site.quota, remaining: remaining,
			cycleTraffic: used,
		})
	}
	return alerts, warnPercent, nil
}

type meteredSite struct {
	id    int64
	name  string
	quota int64
}

func (d *DB) meteredSites() ([]meteredSite, error) {
	rows, err := d.db.Query(`SELECT id, name, traffic_quota FROM sites WHERE enabled=1 AND traffic_quota>0 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sites := make([]meteredSite, 0)
	for rows.Next() {
		var site meteredSite
		if err := rows.Scan(&site.id, &site.name, &site.quota); err != nil {
			return nil, err
		}
		sites = append(sites, site)
	}
	return sites, rows.Err()
}

// pendingTelegramTrafficAlerts filters out alerts already delivered for the same
// resource, cycle and step. The resource is identified by kind plus id, because
// node and site ids live in separate tables and can collide.
func (d *DB) pendingTelegramTrafficAlerts(alerts []telegramTrafficAlert) ([]telegramTrafficAlert, error) {
	if len(alerts) == 0 {
		return nil, nil
	}
	pending := make([]telegramTrafficAlert, 0, len(alerts))
	for _, alert := range alerts {
		var exists int
		err := d.db.QueryRow(`SELECT COUNT(*) FROM telegram_traffic_alerts
			WHERE kind=? AND resource_id=? AND cycle_start_ms=? AND alert_bucket=?`,
			alert.kind, alert.id, alert.cycleStartMS, alert.bucket).Scan(&exists)
		if err != nil {
			return nil, err
		}
		if exists == 0 {
			pending = append(pending, alert)
		}
	}
	return pending, nil
}

// recordTelegramTrafficAlert persists a delivered alert. A conflict means another
// tick or process already announced this exact state, which is not an error.
func (d *DB) recordTelegramTrafficAlert(alert telegramTrafficAlert, now time.Time) error {
	_, err := d.db.Exec(`INSERT OR IGNORE INTO telegram_traffic_alerts
		(kind, resource_id, cycle_start_ms, alert_bucket, name, used_bytes, limit_bytes, remaining_bytes, cycle_traffic, daily_traffic, sent_at_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		alert.kind, alert.id, alert.cycleStartMS, alert.bucket, alert.name,
		alert.used, alert.limit, alert.remaining, alert.cycleTraffic, alert.dailyTraffic, now.UnixMilli())
	return err
}

// recentTelegramTrafficAlerts returns the most recent deliveries, newest first.
func (d *DB) recentTelegramTrafficAlerts(limit int) ([]telegramTrafficAlert, []time.Time, error) {
	if limit <= 0 || limit > trafficAlertHistoryKept {
		limit = trafficAlertHistoryKept
	}
	rows, err := d.db.Query(`SELECT kind, name, used_bytes, limit_bytes, remaining_bytes, alert_bucket, cycle_start_ms, cycle_traffic, daily_traffic, sent_at_ms
		FROM telegram_traffic_alerts ORDER BY sent_at_ms DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	alerts := make([]telegramTrafficAlert, 0)
	sent := make([]time.Time, 0)
	for rows.Next() {
		var alert telegramTrafficAlert
		var sentAtMS int64
		if err := rows.Scan(&alert.kind, &alert.name, &alert.used, &alert.limit, &alert.remaining, &alert.bucket, &alert.cycleStartMS, &alert.cycleTraffic, &alert.dailyTraffic, &sentAtMS); err != nil {
			return nil, nil, err
		}
		alerts = append(alerts, alert)
		sent = append(sent, time.UnixMilli(sentAtMS))
	}
	return alerts, sent, rows.Err()
}

// pruneTelegramTrafficAlerts keeps the ledger small. Rows beyond the newest
// trafficAlertHistoryKept are only history: de-duplication keys on the current
// cycle and step rather than on age, so pruning never re-arms an old alert.
func (d *DB) pruneTelegramTrafficAlerts() error {
	_, err := d.db.Exec(`DELETE FROM telegram_traffic_alerts WHERE id NOT IN (
			SELECT id FROM telegram_traffic_alerts ORDER BY sent_at_ms DESC, id DESC LIMIT ?
		)`, trafficAlertHistoryKept)
	return err
}

// buildTelegramTrafficAlertMessage renders a standalone alert. It deliberately
// does not reuse the daily report layout: this is a single-subject message and
// the report keeps its own format.
func buildTelegramTrafficAlertMessage(alert telegramTrafficAlert, warnPercent int, now time.Time) string {
	var b strings.Builder
	b.WriteString("⚠️ 流量预警\n\n")
	label := "节点"
	if alert.kind == "site" {
		label = "站点"
	}
	fmt.Fprintf(&b, "• %s：%s\n", label, truncateTelegramText(alert.name, 48))
	ratio := float64(0)
	if alert.limit > 0 {
		ratio = float64(alert.used) / float64(alert.limit) * 100
	}
	fmt.Fprintf(&b, "• 已用：%s / %s（%.1f%%）\n", formatTelegramBytes(alert.used), formatTelegramBytes(alert.limit), ratio)
	if alert.remaining > 0 {
		fmt.Fprintf(&b, "• 剩余：%s\n", formatTelegramBytes(alert.remaining))
	} else {
		b.WriteString("• 剩余：已超出额度\n")
	}
	if alert.kind == "node" && alert.dailyTraffic > 0 {
		fmt.Fprintf(&b, "• 今日：%s\n", formatTelegramBytes(alert.dailyTraffic))
	}
	fmt.Fprintf(&b, "• 阈值：%d%%\n", warnPercent)
	fmt.Fprintf(&b, "\n⏱️ %s", now.Format("2006-01-02 15:04"))
	return b.String()
}

func runTelegramTrafficAlertScheduler(ctx context.Context, db *DB) {
	ticker := time.NewTicker(trafficAlertCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			runTelegramTrafficAlertTick(ctx, db, time.Now(), sendTelegramReport)
		case <-ctx.Done():
			return
		}
	}
}

// runTelegramTrafficAlertTick scans for over-threshold resources and sends one
// message per newly crossed step. A send failure leaves the ledger row unwritten
// so the next tick retries; a success is recorded immediately after delivery.
func runTelegramTrafficAlertTick(ctx context.Context, db *DB, now time.Time, send telegramReportSender) {
	if db == nil || send == nil {
		return
	}
	_, stored, err := db.telegramReportSettingsView()
	if err != nil || !stored.Configured {
		return
	}
	alerts, warnPercent, err := db.scanTelegramTrafficAlerts(now)
	if err != nil {
		log.Printf("[telegram-alert] scan failed: %v", err)
		return
	}
	pending, err := db.pendingTelegramTrafficAlerts(alerts)
	if err != nil {
		log.Printf("[telegram-alert] ledger read failed: %v", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	token, err := decryptTelegramBotToken(stored.BotTokenCiphertext)
	if err != nil {
		log.Printf("[telegram-alert] bot token decrypt failed: %v", err)
		return
	}
	delivered := 0
	for _, alert := range pending {
		if err := send(ctx, token, stored.ChatID, buildTelegramTrafficAlertMessage(alert, warnPercent, now)); err != nil {
			log.Printf("[telegram-alert] send failed for %s %q: %v", alert.kind, alert.name, err)
			continue
		}
		if err := db.recordTelegramTrafficAlert(alert, now); err != nil {
			log.Printf("[telegram-alert] delivery record failed for %s %q: %v", alert.kind, alert.name, err)
			continue
		}
		delivered++
		log.Printf("[telegram-alert] sent %s %q at step %d", alert.kind, alert.name, alert.bucket)
	}
	if delivered > 0 {
		if err := db.pruneTelegramTrafficAlerts(); err != nil {
			log.Printf("[telegram-alert] prune failed: %v", err)
		}
	}
}

// telegramTrafficAlertStatus reports the current over-threshold state without
// sending anything, so the panel can show what is armed and what has gone out.
func (d *DB) telegramTrafficAlertStatus(now time.Time) (map[string]any, error) {
	alerts, warnPercent, err := d.scanTelegramTrafficAlerts(now)
	if err != nil {
		return nil, err
	}
	pending, err := d.pendingTelegramTrafficAlerts(alerts)
	if err != nil {
		return nil, err
	}
	over := make([]map[string]any, 0, len(alerts))
	for _, alert := range alerts {
		ratio := float64(0)
		if alert.limit > 0 {
			ratio = float64(alert.used) / float64(alert.limit) * 100
		}
		over = append(over, map[string]any{
			"kind":             alert.kind,
			"name":             alert.name,
			"used_bytes":       alert.used,
			"limit_bytes":      alert.limit,
			"remaining_bytes":  alert.remaining,
			"percent":          ratio,
			"alert_bucket":     alert.bucket,
			"cycle_start_ms":   alert.cycleStartMS,
			"pending_delivery": containsTelegramTrafficAlert(pending, alert),
		})
	}
	recent, sent, err := d.recentTelegramTrafficAlerts(20)
	if err != nil {
		return nil, err
	}
	history := make([]map[string]any, 0, len(recent))
	for index, alert := range recent {
		history = append(history, map[string]any{
			"kind":           alert.kind,
			"name":           alert.name,
			"used_bytes":     alert.used,
			"limit_bytes":    alert.limit,
			"alert_bucket":   alert.bucket,
			"cycle_start_ms": alert.cycleStartMS,
			"sent_at":        sent[index].Format(time.RFC3339),
		})
	}
	return map[string]any{
		"threshold_percent": warnPercent,
		"over_threshold":    over,
		"recent_alerts":     history,
		"checked_at":        now.Format(time.RFC3339),
	}, nil
}

func containsTelegramTrafficAlert(list []telegramTrafficAlert, target telegramTrafficAlert) bool {
	for _, item := range list {
		if item.kind == target.kind && item.id == target.id && item.bucket == target.bucket && item.cycleStartMS == target.cycleStartMS {
			return true
		}
	}
	return false
}

// handleTelegramTrafficAlerts exposes the current over-threshold state and the
// recent deliveries, so an operator can confirm what is armed without waiting
// for the next message.
func (a *App) handleTelegramTrafficAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	status, err := a.db.telegramTrafficAlertStatus(time.Now())
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "failed to read traffic alerts")
		return
	}
	a.jsonOK(w, status)
}
