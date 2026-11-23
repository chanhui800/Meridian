package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"math/big"
	mathrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func trafficAlertReliabilityNodes(t *testing.T, count int, now time.Time) (*DB, []ControlNode) {
	t.Helper()
	database := openWatchHistoryTestDB(t)
	ciphertext, err := encryptTelegramBotToken("123456:synthetic-reliability-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.saveTelegramReportSettings(TelegramReportSettings{
		Enabled: true, ChatID: "synthetic-chat", ScheduleTime: "20:00", Frequency: "daily", Weekday: 1, TrafficWarningPercent: 80,
	}, ciphertext, true); err != nil {
		t.Fatal(err)
	}
	nodes := make([]ControlNode, 0, count)
	for index := 0; index < count; index++ {
		node, _, err := database.CreateControlNode(NodeCreateInput{
			Name: fmt.Sprintf("reliability-quota-%d", index+1), Address: "203.0.113.97", Port: 9443,
			TrafficQuota: 100, BillingMode: trafficBillingModeOutbound, ResetDay: 0,
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
	}
	if _, err := database.db.Exec(`UPDATE control_nodes SET traffic_manual_offset_bytes=80,enabled=1`); err != nil {
		t.Fatal(err)
	}
	return database, nodes
}

func TestTelegramAlertDeliverySurvivesHistoryPruning(t *testing.T) {
	previousLogWriter := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(previousLogWriter) })
	now := time.Now()
	database, _ := trafficAlertReliabilityNodes(t, trafficAlertHistoryKept+1, now)
	sent := 0
	sender := func(context.Context, string, string, string) error { sent++; return nil }
	runTelegramTrafficAlertTick(t.Context(), database, now, sender)
	if sent != trafficAlertHistoryKept+1 {
		t.Fatalf("initial delivery count=%d, want %d", sent, trafficAlertHistoryKept+1)
	}
	runTelegramTrafficAlertTick(t.Context(), database, now.Add(time.Minute), sender)
	if sent != trafficAlertHistoryKept+1 {
		t.Fatalf("unchanged resources redelivered after pruning: sent=%d", sent)
	}
	recent, _, err := database.recentTelegramTrafficAlerts(trafficAlertHistoryKept)
	if err != nil || len(recent) != trafficAlertHistoryKept {
		t.Fatalf("history limit=%d err=%v", len(recent), err)
	}
}

func TestTelegramAlertConcurrentTicksDoNotDuplicateDelivery(t *testing.T) {
	now := time.Now()
	database, _ := trafficAlertReliabilityNodes(t, 1, now)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var sent atomic.Int32
	sender := func(context.Context, string, string, string) error {
		if sent.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	go func() {
		defer close(done)
		runTelegramTrafficAlertTick(t.Context(), database, now, sender)
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("first tick did not reach sender")
	}
	runTelegramTrafficAlertTick(t.Context(), database, now.Add(time.Second), sender)
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("first tick did not finish")
	}
	if sent.Load() != 1 {
		t.Fatalf("concurrent ticks delivered the same current-cycle step %d times", sent.Load())
	}
}

func TestTelegramAlertSendFailureUsesBoundedBackoff(t *testing.T) {
	now := time.Now()
	database, _ := trafficAlertReliabilityNodes(t, 1, now)
	sent := 0
	recovered := false
	sender := func(context.Context, string, string, string) error {
		sent++
		if !recovered {
			return errors.New("temporary provider outage")
		}
		return nil
	}
	for _, offset := range []time.Duration{0, 10 * time.Second, 61 * time.Second, 70 * time.Second, 2 * time.Minute, 182 * time.Second} {
		runTelegramTrafficAlertTick(t.Context(), database, now.Add(offset), sender)
	}
	if sent != 3 {
		t.Fatalf("failed attempts ignored backoff: sent=%d, want attempts at 0, 1 and 3 minutes", sent)
	}
	recovered = true
	runTelegramTrafficAlertTick(t.Context(), database, now.Add(4*time.Minute), sender)
	if sent != 3 {
		t.Fatal("recovered provider bypassed an already scheduled retry")
	}
	runTelegramTrafficAlertTick(t.Context(), database, now.Add(423*time.Second), sender)
	runTelegramTrafficAlertTick(t.Context(), database, now.Add(8*time.Minute), sender)
	if sent != 4 {
		t.Fatalf("recovery should deliver once and then stay deduplicated: sent=%d", sent)
	}
}

func TestTelegramAlertLeaseSurvivesRestartAndExpiresForRedelivery(t *testing.T) {
	now := time.Now()
	database, _ := trafficAlertReliabilityNodes(t, 1, now)
	alerts, _, err := database.scanTelegramTrafficAlerts(now)
	if err != nil || len(alerts) != 1 {
		t.Fatalf("initial scan=%v err=%v", alerts, err)
	}
	oldClaim, claimed, err := database.claimTelegramTrafficAlert(alerts[0], now)
	if err != nil || !claimed {
		t.Fatalf("initial reservation claimed=%v err=%v", claimed, err)
	}
	path := database.dbPath
	database.Close()
	reopened, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	sent := 0
	sender := func(context.Context, string, string, string) error { sent++; return nil }
	runTelegramTrafficAlertTick(t.Context(), reopened, now.Add(10*time.Second), sender)
	if sent != 0 {
		t.Fatal("restart discarded an unexpired reservation")
	}
	runTelegramTrafficAlertTick(t.Context(), reopened, now.Add(61*time.Second), sender)
	if sent != 1 {
		t.Fatalf("expired reservation did not permit recovery: sent=%d", sent)
	}
	if err := reopened.retryTelegramTrafficAlert(alerts[0], oldClaim, now.Add(62*time.Second)); err != nil {
		t.Fatal(err)
	}
	runTelegramTrafficAlertTick(t.Context(), reopened, now.Add(2*time.Minute), sender)
	if sent != 1 {
		t.Fatal("stale failure rearmed a successfully delivered reservation")
	}
}

func TestTelegramAlertStaleFailureCannotInvalidateNewReservation(t *testing.T) {
	now := time.Now()
	database, _ := trafficAlertReliabilityNodes(t, 1, now)
	alerts, _, err := database.scanTelegramTrafficAlerts(now)
	if err != nil || len(alerts) != 1 {
		t.Fatalf("initial scan=%v err=%v", alerts, err)
	}
	oldClaim, claimed, err := database.claimTelegramTrafficAlert(alerts[0], now)
	if err != nil || !claimed {
		t.Fatalf("old reservation claimed=%v err=%v", claimed, err)
	}
	newAlert := alerts[0]
	newAlert.bucket = 95
	newClaim, claimed, err := database.claimTelegramTrafficAlert(newAlert, now.Add(61*time.Second))
	if err != nil || !claimed {
		t.Fatalf("replacement reservation claimed=%v err=%v", claimed, err)
	}
	if err := database.retryTelegramTrafficAlert(alerts[0], oldClaim, now.Add(62*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := database.recordTelegramTrafficAlertClaim(newAlert, newClaim, now.Add(63*time.Second)); err != nil {
		t.Fatal(err)
	}
	pending, err := database.pendingTelegramTrafficAlerts([]telegramTrafficAlert{newAlert})
	if err != nil || len(pending) != 0 {
		t.Fatalf("stale result discarded replacement delivery: pending=%v err=%v", pending, err)
	}
}

func TestTelegramAlertArithmeticRemainsBoundedForTinyQuota(t *testing.T) {
	if os.Getenv("MERIDIAN_ALERT_BUCKET_TEST_CHILD") == "1" {
		database, err := openDB(os.Getenv("MERIDIAN_ALERT_BUCKET_TEST_DB"))
		if err != nil {
			t.Fatal(err)
		}
		defer database.Close()
		now := time.Now()
		input := NodeCreateInput{
			Name: "tiny-quota", Address: "203.0.113.97", Port: 9443,
			TrafficQuota: 1, BillingMode: trafficBillingModeOutbound, ResetDay: 0,
		}
		node, _, err := database.CreateControlNode(input, now)
		if err != nil {
			t.Fatal(err)
		}
		input.TrafficManualOffsetBytes = 1 << 40
		if _, err := database.UpdateControlNode(node.ID, input, true, now); err != nil {
			t.Fatal(err)
		}
		fmt.Println("entered alert handler with an accepted tiny quota")
		response := httptest.NewRecorder()
		(&App{db: database}).handleTelegramTrafficAlerts(response, httptest.NewRequest(http.MethodGet, "/api/telegram-alerts", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("alert status=%d body=%s", response.Code, response.Body.String())
		}
		return
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, binary, "-test.run=^TestTelegramAlertArithmeticRemainsBoundedForTinyQuota$", "-test.v")
	child.Env = append(os.Environ(), "MERIDIAN_ALERT_BUCKET_TEST_CHILD=1",
		"MERIDIAN_ALERT_BUCKET_TEST_DB="+filepath.Join(t.TempDir(), "fixture.db"),
		"GORACE="+os.Getenv("GORACE")+" atexit_sleep_ms=0")
	output, runErr := child.CombinedOutput()
	if !strings.Contains(string(output), "entered alert handler with an accepted tiny quota") {
		t.Fatalf("fixture did not reach handler: %v\n%s", runErr, output)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("alert calculation exhausted the isolated process deadline\n%s", output)
	}
	if runErr != nil {
		t.Fatalf("alert fixture failed: %v\n%s", runErr, output)
	}
}

func TestTelegramAlertArithmeticMatchesExactIntegerReference(t *testing.T) {
	reference := func(used, limit int64, warn int) int {
		if used <= 0 || limit <= 0 || warn <= 0 {
			return 0
		}
		percent := new(big.Int).Quo(new(big.Int).Mul(big.NewInt(used), big.NewInt(100)), big.NewInt(limit))
		if percent.Cmp(big.NewInt(int64(warn))) < 0 {
			return 0
		}
		maxInt := big.NewInt(int64(int(^uint(0) >> 1)))
		if percent.Cmp(maxInt) > 0 {
			percent.Set(maxInt)
		}
		steps := new(big.Int).Quo(new(big.Int).Sub(percent, big.NewInt(int64(warn))), big.NewInt(5))
		return int(new(big.Int).Add(new(big.Int).Mul(steps, big.NewInt(5)), big.NewInt(int64(warn))).Int64())
	}
	for _, testCase := range []struct {
		used, limit int64
		warn, want  int
	}{
		{720, 1000, 72, 72}, {769, 1000, 72, 72}, {770, 1000, 72, 77}, {1000, 1000, 72, 97},
		{math.MaxInt64, math.MaxInt64, 80, 100}, {math.MaxInt64, math.MaxInt64 - 1, 72, 97},
		{0, 1, 80, 0}, {-1, 1, 80, 0}, {1, 0, 80, 0}, {1, 1, 0, 0},
	} {
		if got := telegramReportAlertBucket(testCase.used, testCase.limit, testCase.warn); got != testCase.want {
			t.Fatalf("used=%d limit=%d warn=%d got=%d want=%d", testCase.used, testCase.limit, testCase.warn, got, testCase.want)
		}
	}
	random := mathrand.New(mathrand.NewPCG(7, 19))
	for index := 0; index < 600; index++ {
		used, limit := int64(random.Uint64()>>1), int64(random.Uint64()>>1)|1
		warn := 1 + int(random.Uint64()%100)
		if index%3 == 0 {
			used, limit = math.MaxInt64-int64(index), int64(index%17+1)
		}
		want := reference(used, limit, warn)
		if got := telegramReportAlertBucket(used, limit, warn); got != want {
			t.Fatalf("case=%d used=%d limit=%d warn=%d got=%d want=%d", index, used, limit, warn, got, want)
		}
	}
}

func TestTelegramAlertStateUsesHighestDeliveredStep(t *testing.T) {
	now := time.Now()
	database, _ := trafficAlertReliabilityNodes(t, 1, now)
	alerts, _, err := database.scanTelegramTrafficAlerts(now)
	if err != nil || len(alerts) != 1 {
		t.Fatalf("initial alert scan=%v err=%v", alerts, err)
	}
	alert := alerts[0]
	for _, bucket := range []int{80, 95, 105, 105, 95} {
		alert.bucket = bucket
		if err := database.recordTelegramTrafficAlert(alert, now); err != nil {
			t.Fatal(err)
		}
	}
	candidates := make([]telegramTrafficAlert, 0, 5)
	for _, bucket := range []int{80, 95, 100, 105, 110} {
		candidate := alert
		candidate.bucket = bucket
		candidates = append(candidates, candidate)
	}
	pending, err := database.pendingTelegramTrafficAlerts(candidates)
	if err != nil || len(pending) != 1 || pending[0].bucket != 110 {
		t.Fatalf("highest-step deduplication pending=%v err=%v", pending, err)
	}
	recent, _, err := database.recentTelegramTrafficAlerts(200)
	if err != nil || len(recent) != 3 {
		t.Fatalf("duplicate/lower deliveries rewrote history: recent=%d err=%v", len(recent), err)
	}
}

func TestTelegramAlertStateRetainsCurrentAndLatestPriorCycle(t *testing.T) {
	now := time.Now()
	database, nodes := trafficAlertReliabilityNodes(t, 1, now)
	base := now.UnixMilli()
	for cycle := int64(1); cycle <= 12; cycle++ {
		if _, err := database.db.Exec(`UPDATE control_nodes SET cycle_started_at_ms=? WHERE id=?`, base+cycle, nodes[0].ID); err != nil {
			t.Fatal(err)
		}
		alert := telegramTrafficAlert{kind: "node", id: nodes[0].ID, cycleStartMS: base + cycle, bucket: 80}
		if err := database.recordTelegramTrafficAlert(alert, now.Add(time.Duration(cycle)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	late := telegramTrafficAlert{kind: "node", id: nodes[0].ID, cycleStartMS: base + 11, bucket: 85}
	if err := database.recordTelegramTrafficAlert(late, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	current := telegramTrafficAlert{kind: "node", id: nodes[0].ID, cycleStartMS: base + 13, bucket: 80}
	if _, err := database.db.Exec(`UPDATE control_nodes SET cycle_started_at_ms=? WHERE id=?`, current.cycleStartMS, nodes[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := database.recordTelegramTrafficAlert(current, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err := database.db.Query(`SELECT cycle_start_ms FROM telegram_traffic_alert_state WHERE kind='node' AND resource_id=? ORDER BY cycle_start_ms`, nodes[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var cycles []int64
	for rows.Next() {
		var cycle int64
		if err := rows.Scan(&cycle); err != nil {
			t.Fatal(err)
		}
		cycles = append(cycles, cycle)
	}
	if err := rows.Err(); err != nil || len(cycles) != 2 || cycles[0] != base+12 || cycles[1] != base+13 {
		t.Fatalf("late delivery displaced the latest prior cycle: cycles=%v err=%v", cycles, err)
	}
}

func TestTelegramAlertStateKeepsNoResetCycleAndCleansDeletedResources(t *testing.T) {
	now := time.Now()
	database, nodes := trafficAlertReliabilityNodes(t, 1, now.AddDate(-2, 0, 0))
	site := createWatchHistoryTestSite(t, database, true)
	settings := database.currentSystemSettings()
	settings.TrafficResetDay = 0
	if err := database.saveSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	nodeAlert := telegramTrafficAlert{kind: "node", id: nodes[0].ID, cycleStartMS: 0, bucket: 80}
	siteAlert := telegramTrafficAlert{kind: "site", id: site.ID, cycleStartMS: 0, bucket: 80}
	if _, err := database.db.Exec(`UPDATE control_nodes SET cycle_started_at_ms=0,reset_day=0 WHERE id=?`, nodes[0].ID); err != nil {
		t.Fatal(err)
	}
	for _, alert := range []telegramTrafficAlert{nodeAlert, siteAlert} {
		if err := database.recordTelegramTrafficAlert(alert, now.AddDate(-2, 0, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.pruneTelegramTrafficAlerts(); err != nil {
		t.Fatal(err)
	}
	sent := 0
	runTelegramTrafficAlertTick(t.Context(), database, now.AddDate(2, 0, 0), func(context.Context, string, string, string) error { sent++; return nil })
	if sent != 0 {
		t.Fatal("no-reset cycle was rearmed by age")
	}
	pending, err := database.pendingTelegramTrafficAlerts([]telegramTrafficAlert{siteAlert})
	if err != nil || len(pending) != 0 {
		t.Fatalf("site cycle zero lost its watermark: pending=%v err=%v", pending, err)
	}
	if _, err := database.db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec("DELETE FROM control_nodes WHERE id=?", nodes[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec("DELETE FROM sites WHERE id=?", site.ID); err != nil {
		t.Fatal(err)
	}
	var stateRows int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM telegram_traffic_alert_state").Scan(&stateRows); err != nil || stateRows != 0 {
		t.Fatalf("deleted resources retained state: rows=%d err=%v", stateRows, err)
	}
}

func TestTelegramAlertStateLateCycleCannotEvictActualNoResetCycle(t *testing.T) {
	now := time.Now()
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	settings := database.currentSystemSettings()
	settings.TrafficResetDay = 0
	if err := database.saveSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	base := now.Add(-time.Hour).UnixMilli()
	for _, cycle := range []int64{base, base + 1, 0} {
		if err := database.recordTelegramTrafficAlert(telegramTrafficAlert{kind: "site", id: site.ID, cycleStartMS: cycle, bucket: 80}, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.recordTelegramTrafficAlert(telegramTrafficAlert{kind: "site", id: site.ID, cycleStartMS: base, bucket: 85}, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	pending, err := database.pendingTelegramTrafficAlerts([]telegramTrafficAlert{{kind: "site", id: site.ID, cycleStartMS: 0, bucket: 80}})
	if err != nil || len(pending) != 0 {
		t.Fatalf("late old-cycle result evicted actual cycle zero: pending=%v err=%v", pending, err)
	}
	var rows int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM telegram_traffic_alert_state WHERE kind='site' AND resource_id=?", site.ID).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("cycle zero preservation lost the two-cycle bound: rows=%d err=%v", rows, err)
	}
}

func TestTelegramAlertStateUpgradePreservesLegacyWatermarksAndIsBounded(t *testing.T) {
	now := time.Now()
	database, nodes := trafficAlertReliabilityNodes(t, 1, now)
	var currentCycle int64
	if err := database.db.QueryRow("SELECT cycle_started_at_ms FROM control_nodes WHERE id=?", nodes[0].ID).Scan(&currentCycle); err != nil {
		t.Fatal(err)
	}
	tx, err := database.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 200; index++ {
		cycle := currentCycle - int64(3-index/50)
		bucket := 80 + 5*(index%50)
		if _, err := tx.Exec(`INSERT INTO telegram_traffic_alerts
			(kind,resource_id,cycle_start_ms,alert_bucket,sent_at_ms) VALUES ('node',?,?,?,?)`, nodes[0].ID, cycle, bucket, now.Add(time.Duration(index)*time.Second).UnixMilli()); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec("PRAGMA user_version=58"); err != nil {
		t.Fatal(err)
	}
	path := database.dbPath
	database.Close()
	upgraded, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if err := upgraded.migrate(); err != nil {
		t.Fatal(err)
	}
	var stateRows, schema int
	if err := upgraded.db.QueryRow("SELECT COUNT(*) FROM telegram_traffic_alert_state").Scan(&stateRows); err != nil || stateRows != 2 {
		t.Fatalf("legacy state was not bounded by two cycles: rows=%d err=%v", stateRows, err)
	}
	if err := upgraded.db.QueryRow("PRAGMA user_version").Scan(&schema); err != nil || schema != databaseSchemaVersion {
		t.Fatalf("schema=%d err=%v", schema, err)
	}
	for _, testCase := range []struct {
		cycle   int64
		bucket  int
		pending bool
	}{{currentCycle, 80, false}, {currentCycle, 325, false}, {currentCycle, 330, true}, {currentCycle + 1, 80, true}} {
		pending, err := upgraded.pendingTelegramTrafficAlerts([]telegramTrafficAlert{{kind: "node", id: nodes[0].ID, cycleStartMS: testCase.cycle, bucket: testCase.bucket}})
		if err != nil || (len(pending) == 1) != testCase.pending {
			t.Fatalf("legacy/new-cycle pending=%v err=%v for %+v", pending, err, testCase)
		}
	}
}
