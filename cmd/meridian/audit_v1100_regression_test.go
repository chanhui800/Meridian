package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression tests for the v1.10.0 full-project audit. Each test is written so
// that reverting its fix makes it fail.

// seedPanelTLSFixture writes the minimum live TLS namespace the panel uses: the
// atomic .panel-current pointer, the enabled marker and an ACME account key.
func seedPanelTLSFixture(t *testing.T, dbPath string) []string {
	t.Helper()
	tlsDir := tlsStateDir(dbPath)
	if tlsDir == "" {
		t.Fatal("fixture requires a managed TLS state directory")
	}
	current := filepath.Join(tlsDir, ".panel-current")
	if err := os.MkdirAll(current, 0o700); err != nil {
		t.Fatal(err)
	}
	live := []string{
		filepath.Join(current, "fullchain.pem"),
		filepath.Join(current, "privkey.pem"),
		filepath.Join(tlsDir, "enabled"),
		filepath.Join(tlsDir, "acme-account.pem"),
	}
	for _, path := range live {
		if err := os.WriteFile(path, []byte("live "+filepath.Base(path)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return live
}

// selfSignedPanelPair builds a usable certificate/key pair for the panel manager.
func selfSignedPanelPair(t *testing.T) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "panel.example.test"},
		DNSNames:              []string{"panel.example.test", "*.route.example.test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// An interrupted apply whose TLS namespace snapshot never completed must not
// delete the live TLS files. The returned error is the only signal the boot path
// gives the operator, so a swallowed error here is a silently broken panel.
func TestAuditRollbackRefusesToDeleteLiveTLSWithoutSnapshot(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meridian.db")
	rollback := dbPath + backupRollbackSuffix
	pending := dbPath + backupPendingSuffix

	if err := os.WriteFile(dbPath, []byte("partial restored database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rollback, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pending, 0o700); err != nil {
		t.Fatal(err)
	}
	// The rollback holds the pre-restore database and the incoming marker, but
	// no tls-namespace.json and no per-entry TLS copy: exactly the state left
	// behind when snapshotTLSNamespace fails after rollbackReady is set.
	if err := os.WriteFile(filepath.Join(rollback, backupDatabaseEntry), []byte("original database"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker, err := json.Marshal(restoreMarker{
		Files:      []string{backupDatabaseEntry, backupTLSCertificate, backupTLSPrivateKey, backupTLSEnabled, backupACMEAccount},
		IncludeTLS: boolPointer(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rollback, "restore.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pending, "restore.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath+backupAppliedSuffix, []byte("pending validation\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	live := seedPanelTLSFixture(t, dbPath)

	if _, err := applyPendingRestore(dbPath); err == nil {
		t.Fatal("applyPendingRestore reported success with no TLS snapshot to restore from")
	}
	for _, path := range live {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("live TLS file %s was removed by the rollback: %v", filepath.Base(path), err)
		}
	}
	// The rollback directory and the applied marker are the only remaining
	// recovery path, so the failure must preserve both for the next boot.
	if _, err := os.Stat(rollback); err != nil {
		t.Fatalf("rollback directory was discarded: %v", err)
	}
	if _, err := os.Stat(dbPath + backupAppliedSuffix); err != nil {
		t.Fatalf("applied marker was removed, so the next boot cannot retry: %v", err)
	}
	if data, err := os.ReadFile(dbPath); err != nil || !bytes.Equal(data, []byte("original database")) {
		t.Fatalf("database was not rolled back to the pre-restore copy: %q, %v", data, err)
	}
}

// The companion case: a rollback that carries no TLS at all (include_tls=false)
// still restores the database, discards the stage and leaves TLS untouched.
func TestAuditRollbackWithoutTLSStillSucceeds(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meridian.db")
	rollback := dbPath + backupRollbackSuffix
	pending := dbPath + backupPendingSuffix

	if err := os.WriteFile(dbPath, []byte("partial restored database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rollback, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pending, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rollback, backupDatabaseEntry), []byte("original database"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker, err := json.Marshal(restoreMarker{Files: []string{backupDatabaseEntry}, IncludeTLS: boolPointer(false)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rollback, "restore.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pending, "restore.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath+backupAppliedSuffix, []byte("pending validation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	live := seedPanelTLSFixture(t, dbPath)

	state, err := applyPendingRestore(dbPath)
	if err != nil || state != nil {
		t.Fatalf("apply interrupted restore = %#v, %v", state, err)
	}
	if data, err := os.ReadFile(dbPath); err != nil || !bytes.Equal(data, []byte("original database")) {
		t.Fatalf("database after automatic rollback = %q, %v", data, err)
	}
	for _, path := range live {
		if data, err := os.ReadFile(path); err != nil || !bytes.HasPrefix(data, []byte("live ")) {
			t.Fatalf("live TLS file %s changed: %q, %v", filepath.Base(path), data, err)
		}
	}
}

// A rollback that carries only part of the TLS entries must restore what it has
// and leave the rest alone. Deleting a live file whose source is missing destroys
// the only copy, because the rollback is the last recovery path.
func TestAuditRollbackKeepsLiveTLSFileWithNoRollbackSource(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meridian.db")
	rollback := dbPath + backupRollbackSuffix
	pending := dbPath + backupPendingSuffix

	if err := os.WriteFile(dbPath, []byte("partial restored database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rollback, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pending, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rollback, backupDatabaseEntry), []byte("original database"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Only the panel pair is present in the rollback; tls/enabled is declared by
	// the incoming restore but has no rollback copy.
	certPEM, keyPEM := selfSignedPanelPair(t)
	if err := os.MkdirAll(filepath.Join(rollback, "tls"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rollback, filepath.FromSlash(backupTLSCertificate)), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rollback, filepath.FromSlash(backupTLSPrivateKey)), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	marker, err := json.Marshal(restoreMarker{
		Files:      []string{backupDatabaseEntry, backupTLSCertificate, backupTLSPrivateKey, backupTLSEnabled},
		IncludeTLS: boolPointer(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rollback, "restore.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pending, "restore.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath+backupAppliedSuffix, []byte("pending validation\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	live := seedPanelTLSFixture(t, dbPath)
	markerPath := filepath.Join(tlsStateDir(dbPath), "enabled")

	if _, err := applyPendingRestore(dbPath); err != nil {
		t.Fatalf("applyPendingRestore = %v", err)
	}
	// The entry with no rollback source keeps its live file.
	if data, err := os.ReadFile(markerPath); err != nil || !bytes.Equal(data, []byte("live enabled\n")) {
		t.Fatalf("live TLS marker was removed or replaced although the rollback had no copy: %q, %v", data, err)
	}
	// The entries that do have a copy are restored.
	certFile, _ := newPanelCertificateManager(dbPath, nil).panelPairPaths()
	if data, err := os.ReadFile(certFile); err != nil || !bytes.Equal(data, certPEM) {
		t.Fatalf("rollback did not restore the panel certificate from its copy: %v", err)
	}
	_ = live
}

// --- dashboard trend newest-point rate --------------------------------------

// The newest chart point belongs to the bucket that is still accumulating, so its
// rate must divide by the elapsed part of that bucket. Dividing by the full
// bucket understates the live speed, which is the number the operator watches.
func TestAuditTrendNewestPointUsesElapsedBucketTime(t *testing.T) {
	location := time.UTC
	now := time.Date(2026, 9, 14, 14, 20, 0, 0, location)
	name, start, end, bucket, err := dashboardTrendWindowWithLocation("day", now, time.Time{}, time.Time{}, location)
	if err != nil {
		t.Fatal(err)
	}
	if name != "day" || bucket != time.Hour {
		t.Fatalf("window = %q bucket=%s", name, bucket)
	}
	if !end.After(now) {
		t.Fatalf("fixture expects the window to end inside the current bucket, end=%s", end)
	}

	const bytesOut = int64(3600 * 1024 * 1024)
	logs := []TrafficLog{{BytesOut: bytesOut, RecordedAtMS: end.Add(-time.Minute).UnixMilli()}}
	points := dashboardTrendPoints(start, end, bucket, name, trafficBillingModeOutbound, logs, dashboardPendingTraffic{}, now)
	newest := points[len(points)-1]
	// 20 of the 60 bucket minutes have elapsed, so the rate is bytes/1200s.
	want := float64(bytesOut) / (20 * 60)
	if got := newest.DownloadBPS; got != want {
		t.Fatalf("newest point rate = %v B/s, want %v B/s (full-bucket divisor would give %v)", got, want, float64(bytesOut)/3600)
	}
}

// A completed historical window must keep dividing by the full bucket, so the fix
// cannot simply shorten every point.
func TestAuditTrendCompletedBucketsKeepFullDivisor(t *testing.T) {
	location := time.UTC
	now := time.Date(2026, 9, 14, 14, 20, 0, 0, location)
	name, start, end, bucket, err := dashboardTrendWindowWithLocation("6h", now, time.Time{}, time.Time{}, location)
	if err != nil {
		t.Fatal(err)
	}
	const bytesOut = int64(300 * 1024 * 1024)
	logs := []TrafficLog{{BytesOut: bytesOut, RecordedAtMS: start.Add(time.Minute).UnixMilli()}}
	points := dashboardTrendPoints(start, end, bucket, name, trafficBillingModeOutbound, logs, dashboardPendingTraffic{}, now)
	want := float64(bytesOut) / bucket.Seconds()
	if got := points[0].DownloadBPS; got != want {
		t.Fatalf("historical point rate = %v B/s, want %v B/s", got, want)
	}
}

// The custom range already subdivided its final bucket; the fix must not change it.
func TestAuditTrendCustomRangeDivisorUnchanged(t *testing.T) {
	location := time.UTC
	now := time.Date(2026, 9, 14, 14, 20, 0, 0, location)
	customStart := time.Date(2026, 9, 14, 12, 0, 0, 0, location)
	customEnd := time.Date(2026, 9, 14, 12, 37, 0, 0, location)
	name, start, end, bucket, err := dashboardTrendWindowWithLocation("custom", now, customStart, customEnd, location)
	if err != nil {
		t.Fatal(err)
	}
	const bytesOut = int64(60 * 1024 * 1024)
	logs := []TrafficLog{{BytesOut: bytesOut, RecordedAtMS: customEnd.Add(-time.Minute).UnixMilli()}}
	points := dashboardTrendPoints(start, end, bucket, name, trafficBillingModeOutbound, logs, dashboardPendingTraffic{}, now)
	newest := points[len(points)-1]
	bucketStart := start.Add(time.Duration(len(points)-1) * bucket)
	want := float64(bytesOut) / end.Sub(bucketStart).Seconds()
	if got := newest.DownloadBPS; got != want {
		t.Fatalf("custom newest rate = %v B/s, want %v B/s", got, want)
	}
}

// --- traffic alert escalation ladder ----------------------------------------

// The escalation step is part of the operator's alerting contract, so the panel
// must announce the threshold and then each further step, and nothing between.
func TestAuditAlertLadderFollowsFivePointSteps(t *testing.T) {
	const limit = int64(100 << 30)
	cases := []struct {
		percent int64
		want    int
	}{
		{79, 0},
		{80, 80},
		{84, 80},
		{85, 85},
		{99, 95},
		{100, 100},
		{106, 105},
	}
	for _, testCase := range cases {
		used := limit * testCase.percent / 100
		if got := telegramReportAlertBucket(used, limit, 80); got != testCase.want {
			t.Fatalf("used=%d%% bucket = %d, want %d", testCase.percent, got, testCase.want)
		}
	}
}

// --- node quota labelling with the reset day disabled -----------------------

// With a node reset day of zero the period counters never reset, so the report
// must not label the lifetime figure as a monthly one.
func TestAuditReportLabelsLifetimeNodeTrafficWhenResetDisabled(t *testing.T) {
	message := buildTelegramReportMessage(telegramReportStats{
		GeneratedAt:        time.Now(),
		TrafficWarnPercent: telegramReportTrafficWarnDisableValue,
		TrafficResetDay:    0,
		Nodes: []telegramReportNodeStat{{
			Name: "无限期节点", ResetDay: 0, TodayTraffic: 1 << 30, CycleTraffic: 9 << 30,
			Remaining: 1 << 30, HasQuota: true, SiteCount: 2,
		}},
	})
	if !strings.Contains(message, "今日 1.00 GB 丨 累计 9.00 GB 丨 剩余 1.00 GB") {
		t.Fatalf("node line mislabels a lifetime total:\n%s", message)
	}
	if strings.Contains(message, "当月 9.00 GB") {
		t.Fatalf("lifetime node total is still labelled 当月:\n%s", message)
	}
	if !strings.Contains(message, "累计流量（未设置重置日）") {
		t.Fatalf("traffic section mislabels a disabled cycle:\n%s", message)
	}
}

// The monthly wording must be unchanged when a cycle is configured.
func TestAuditReportKeepsMonthlyWordingWhenCycleConfigured(t *testing.T) {
	message := buildTelegramReportMessage(telegramReportStats{
		GeneratedAt:        time.Now(),
		TrafficWarnPercent: telegramReportTrafficWarnDisableValue,
		TrafficResetDay:    1,
		CycleStart:         time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		Nodes: []telegramReportNodeStat{{
			Name: "月度节点", ResetDay: 1, TodayTraffic: 1 << 30, CycleTraffic: 2 << 30,
			Remaining: 8 << 30, HasQuota: true, SiteCount: 3,
		}},
	})
	if !strings.Contains(message, "当月 2.00 GB") {
		t.Fatalf("monthly node label changed:\n%s", message)
	}
	if !strings.Contains(message, "当月流量（09-01 起）") {
		t.Fatalf("monthly traffic label changed:\n%s", message)
	}
	if strings.Contains(message, "累计流量（未设置重置日）") {
		t.Fatalf("a configured cycle was labelled as disabled:\n%s", message)
	}
}

// --- panel certificate reload after a rollback ------------------------------

// A certificate pair that exists on disk after the rollback must be loaded back
// into the manager. Leaving the in-memory pointer nil fails every later TLS
// handshake while the certificate on disk looks healthy, so nothing repairs it
// without a manual restart.
func TestAuditRestoreInstalledFilesReloadsExistingPair(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meridian.db")
	certPEM, keyPEM := selfSignedPanelPair(t)
	tlsDir := tlsStateDir(dbPath)
	current := filepath.Join(tlsDir, ".panel-current")
	if err := os.MkdirAll(current, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, "fullchain.pem"), certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, "privkey.pem"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tlsDir, "enabled"), []byte("enabled\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager := newPanelCertificateManager(dbPath, nil)
	if manager == nil {
		t.Fatal("certificate manager is unavailable")
	}
	backup, err := manager.backupInstalledFiles()
	if err != nil {
		t.Fatalf("backupInstalledFiles = %v", err)
	}
	// Model a failed install: the atomic pointer is gone and the in-memory
	// certificate was dropped, so only the rollback can bring HTTPS back.
	if _, _, err := manager.tlsConfig(true); err != nil {
		t.Fatalf("tlsConfig = %v", err)
	}
	if err := os.RemoveAll(current); err != nil {
		t.Fatal(err)
	}
	manager.currentCertificate = nil

	if err := manager.restoreInstalledFiles(backup); err != nil {
		t.Fatalf("restoreInstalledFiles = %v", err)
	}
	if manager.currentCertificate == nil {
		t.Fatal("the restored-on-disk certificate pair was not reloaded, so every later handshake fails")
	}
	certFile, keyFile := manager.panelPairPaths()
	if _, err := os.Stat(certFile); err != nil {
		t.Fatalf("restored certificate file missing: %v", err)
	}
	if _, err := os.Stat(keyFile); err != nil {
		t.Fatalf("restored key file missing: %v", err)
	}
	// The reloaded pair has to be the one a TLS handshake would serve.
	cfg, enabled, err := manager.tlsConfig(true)
	if err != nil || !enabled || cfg == nil || cfg.GetCertificate == nil {
		t.Fatalf("tlsConfig after rollback = %v enabled=%v err=%v", cfg != nil, enabled, err)
	}
	if _, err := cfg.GetCertificate(nil); err != nil {
		t.Fatalf("GetCertificate after rollback = %v", err)
	}
}
