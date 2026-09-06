package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

const testTMDBReadToken = "eyJhbGciOiJIUzI1NiJ9.meridian-backup-test-token-value"

func TestBackupEncryptionRejectsWrongPasswordAndTampering(t *testing.T) {
	plain := []byte("private meridian backup")
	sealed, err := sealBackup(plain, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	opened, err := openBackup(sealed, "correct horse battery staple")
	if err != nil || !bytes.Equal(opened, plain) {
		t.Fatalf("round trip = %q, %v", opened, err)
	}
	if _, err := openBackup(sealed, "wrong password long enough"); err == nil {
		t.Fatal("wrong password was accepted")
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 1
	if _, err := openBackup(tampered, "correct horse battery staple"); err == nil {
		t.Fatal("tampered backup was accepted")
	}
}

func TestBackupV2RoundTripStreamsAndRejectsTampering(t *testing.T) {
	plain := bytes.Repeat([]byte("v2 backup data "), 400000)
	password := "correct horse battery staple"
	sealed, err := sealBackupV2(plain, password)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(sealed, []byte(backupMagicV2)) {
		t.Fatalf("backup magic = %q, want %s", sealed[:len(backupMagicV2)], backupMagicV2)
	}
	opened, err := openBackup(sealed, password)
	if err != nil || !bytes.Equal(opened, plain) {
		t.Fatalf("v2 round trip length=%d err=%v", len(opened), err)
	}
	var streamed bytes.Buffer
	if size, err := openBackupV2Reader(bytes.NewReader(sealed), password, &streamed); err != nil || size != int64(len(plain)) || !bytes.Equal(streamed.Bytes(), plain) {
		t.Fatalf("v2 streaming round trip size=%d err=%v", size, err)
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)/2] ^= 1
	if _, err := openBackup(tampered, password); err == nil {
		t.Fatal("tampered v2 backup was accepted")
	}
}

func TestDatabaseSchemaVersionIsPersisted(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "schema.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != databaseSchemaVersion {
		t.Fatalf("PRAGMA user_version=%d, want %d", version, databaseSchemaVersion)
	}
}

func testBackupArchive(t *testing.T, manifest backupManifest, files map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	w := zip.NewWriter(&buffer)
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	all := map[string][]byte{backupManifestEntry: manifestData}
	for name, data := range files {
		all[name] = data
	}
	for name, data := range all {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestParseBackupArchiveRejectsUnknownAndTraversalEntries(t *testing.T) {
	base := backupManifest{Format: "meridian-backup", FormatVersion: 1, Files: []string{backupDatabaseEntry}}
	for _, name := range []string{"../meridian.db", "secrets.env"} {
		manifest := base
		manifest.Files = []string{backupDatabaseEntry, name}
		archive := testBackupArchive(t, manifest, map[string][]byte{backupDatabaseEntry: []byte("db"), name: []byte("bad")})
		if _, _, err := parseBackupArchive(archive); err == nil {
			t.Fatalf("entry %q was accepted", name)
		}
	}
}

func TestParseBackupArchiveEnforcesTLSManifestScope(t *testing.T) {
	withoutTLS := false
	manifest := backupManifest{
		Format:        "meridian-backup",
		FormatVersion: backupFormatVersion,
		Files:         []string{backupDatabaseEntry, backupTLSCertificate},
		IncludeTLS:    &withoutTLS,
	}
	archive := testBackupArchive(t, manifest, map[string][]byte{
		backupDatabaseEntry:  []byte("db"),
		backupTLSCertificate: []byte("certificate"),
	})
	if _, _, err := parseBackupArchive(archive); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("TLS data outside the declared scope was accepted: %v", err)
	}

	// Backups from before include_tls was introduced always included TLS.
	manifest.IncludeTLS = nil
	archive = testBackupArchive(t, manifest, map[string][]byte{
		backupDatabaseEntry:  []byte("db"),
		backupTLSCertificate: []byte("certificate"),
	})
	parsed, _, err := parseBackupArchive(archive)
	if err != nil || !manifestIncludesTLS(parsed) {
		t.Fatalf("legacy TLS backup rejected: include=%v err=%v", manifestIncludesTLS(parsed), err)
	}
}

func TestBuildBackupIncludesTLSOnlyWhenSelected(t *testing.T) {
	t.Setenv("PANEL_TLS_CERT_FILE", "")
	t.Setenv("PANEL_TLS_KEY_FILE", "")
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meridian.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.db.Exec(`INSERT INTO users (username, password_hash) VALUES ('admin', 'hash')`); err != nil {
		t.Fatal(err)
	}
	tlsDir := filepath.Join(dir, "tls")
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{
		"fullchain.pem":    "certificate",
		"privkey.pem":      "private key",
		"enabled":          "true\n",
		"acme-account.pem": "account",
	} {
		if err := os.WriteFile(filepath.Join(tlsDir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	edgeDir := filepath.Join(tlsDir, "edge-nodes", "node-a", "current")
	if err := os.MkdirAll(edgeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"fullchain.pem": "edge certificate", "privkey.pem": "edge private key"} {
		if err := os.WriteFile(filepath.Join(edgeDir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	app := &App{db: db, dbPath: dbPath, pm: NewProxyManager(db, bytes.Repeat([]byte("h"), 32))}
	const password = "correct horse battery staple"

	withoutTLS, err := app.buildBackup(password, false)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := openBackup(withoutTLS, password)
	if err != nil {
		t.Fatal(err)
	}
	manifest, entries, err := parseBackupArchive(plain)
	if err != nil {
		t.Fatal(err)
	}
	if manifestIncludesTLS(manifest) {
		t.Fatal("backup unexpectedly declares TLS data")
	}
	for name := range entries {
		if strings.HasPrefix(name, "tls/") {
			t.Fatalf("backup unexpectedly contains %s", name)
		}
	}

	withTLS, err := app.buildBackup(password, true)
	if err != nil {
		t.Fatal(err)
	}
	plain, err = openBackup(withTLS, password)
	if err != nil {
		t.Fatal(err)
	}
	manifest, entries, err = parseBackupArchive(plain)
	if err != nil {
		t.Fatal(err)
	}
	if !manifestIncludesTLS(manifest) {
		t.Fatal("selected TLS backup does not declare TLS data")
	}
	if manifest.DatabaseSchemaVersion != databaseSchemaVersion {
		t.Fatalf("backup schema version=%d, want %d", manifest.DatabaseSchemaVersion, databaseSchemaVersion)
	}
	for _, name := range []string{backupTLSCertificate, backupTLSPrivateKey, backupTLSEnabled, backupACMEAccount} {
		if _, ok := entries[name]; !ok {
			t.Fatalf("selected TLS backup is missing %s", name)
		}
	}
	for _, name := range []string{backupTLSEdgeNodesPrefix + "node-a/fullchain.pem", backupTLSEdgeNodesPrefix + "node-a/privkey.pem"} {
		if _, ok := entries[name]; !ok {
			t.Fatalf("selected TLS backup is missing %s", name)
		}
	}
}

func TestParseBackupArchiveRejectsNewerDatabaseSchema(t *testing.T) {
	includeTLS := false
	manifest := backupManifest{
		Format:                "meridian-backup",
		FormatVersion:         backupFormatVersion,
		DatabaseSchemaVersion: databaseSchemaVersion + 1,
		Files:                 []string{backupDatabaseEntry},
		IncludeTLS:            &includeTLS,
	}
	archive := testBackupArchive(t, manifest, map[string][]byte{backupDatabaseEntry: []byte("db")})
	if _, _, err := parseBackupArchive(archive); err == nil || !strings.Contains(err.Error(), "更高数据库版本") {
		t.Fatalf("newer schema backup was accepted: %v", err)
	}
}

func TestBackupEntryLimitAllowsOnlySafeEdgeNodePairs(t *testing.T) {
	if limit, ok := backupEntryLimit(backupTLSEdgeNodesPrefix + "node-a/fullchain.pem"); !ok || limit <= 0 {
		t.Fatal("valid edge node certificate entry was rejected")
	}
	for _, name := range []string{
		backupTLSEdgeNodesPrefix + "../fullchain.pem",
		backupTLSEdgeNodesPrefix + "node-a/other.pem",
	} {
		if _, ok := backupEntryLimit(name); ok {
			t.Fatalf("unsafe edge backup entry accepted: %s", name)
		}
	}
}

func makeBackupDatabase(t *testing.T, path string, jwt, upstreamKey []byte) {
	t.Helper()
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	targetURL := "https://emby.example.com"
	authority := "https://emby.example.com"
	ciphertext, err := encryptUpstreamHeaderValue("X-Test-Key", "secret-value", authority, upstreamKey)
	if err != nil {
		t.Fatal(err)
	}
	if value, decryptErr := decryptUpstreamHeaderValue("X-Test-Key", ciphertext, authority, upstreamKey); decryptErr != nil || value != "secret-value" {
		t.Fatalf("fixture header encryption failed: %q, %v", value, decryptErr)
	}
	raw, _ := json.Marshal([]storedUpstreamHeader{{Name: "X-Test-Key", Ciphertext: ciphertext}})
	if _, err := db.db.Exec(`INSERT INTO sites (name, listen_port, target_url, upstream_headers) VALUES ('test', 19001, ?, ?)`, targetURL, string(raw)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	token, err := encryptTelegramBotTokenWithSecret("123456:test-token", jwt)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE telegram_report_settings SET bot_token_ciphertext=?, chat_id='123' WHERE id=1`, token); err != nil {
		db.Close()
		t.Fatal(err)
	}
	acmeToken, err := encryptPanelACMETokenWithSecret("cloudflare-backup-token-value", jwt)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE panel_settings SET acme_email='admin@example.com', acme_dns_provider='cloudflare', acme_token_ciphertext=?, acme_staging=1 WHERE id=1`, acmeToken); err != nil {
		db.Close()
		t.Fatal(err)
	}
	tmdbToken, err := encryptTMDBReadTokenWithSecret(testTMDBReadToken, jwt)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE tmdb_settings SET enabled=1, token_ciphertext=?, credential_state='ready' WHERE id=1`, tmdbToken); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO users (username, password_hash) VALUES ('admin', 'hash')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
}

func TestReencryptRestoredSecrets(t *testing.T) {
	oldJWT := bytes.Repeat([]byte("j"), 32)
	newJWT := bytes.Repeat([]byte("n"), 32)
	oldHeader := sha256.Sum256([]byte("old-upstream-header-key-material"))
	newHeader := sha256.Sum256([]byte("new-upstream-header-key-material"))
	path := filepath.Join(t.TempDir(), "backup.db")
	makeBackupDatabase(t, path, oldJWT, oldHeader[:])
	before, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var beforeRaw string
	if err := before.QueryRow("SELECT upstream_headers FROM sites WHERE name='test'").Scan(&beforeRaw); err != nil {
		before.Close()
		t.Fatal(err)
	}
	before.Close()
	beforeStored, err := parseStoredUpstreamHeaders(beforeRaw)
	if err != nil {
		t.Fatal(err)
	}
	if value, decryptErr := decryptUpstreamHeaderValue(beforeStored[0].Name, beforeStored[0].Ciphertext, "https://emby.example.com", oldHeader[:]); decryptErr != nil || value != "secret-value" {
		t.Fatalf("stored fixture header failed: %q, %v (%s)", value, decryptErr, beforeRaw)
	}
	if err := reencryptRestoredSecrets(path, oldJWT, oldHeader[:], newJWT, newHeader[:]); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var raw string
	if err := db.QueryRow("SELECT upstream_headers FROM sites WHERE name='test'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	stored, err := parseStoredUpstreamHeaders(raw)
	if err != nil {
		t.Fatal(err)
	}
	value, err := decryptUpstreamHeaderValue(stored[0].Name, stored[0].Ciphertext, "https://emby.example.com", newHeader[:])
	if err != nil || value != "secret-value" {
		t.Fatalf("header = %q, %v", value, err)
	}
	if _, err := decryptUpstreamHeaderValue(stored[0].Name, stored[0].Ciphertext, "https://emby.example.com", oldHeader[:]); err == nil {
		t.Fatal("old upstream key still decrypts migrated header")
	}
	var telegramCiphertext string
	if err := db.QueryRow("SELECT bot_token_ciphertext FROM telegram_report_settings WHERE id=1").Scan(&telegramCiphertext); err != nil {
		t.Fatal(err)
	}
	value, err = decryptTelegramBotTokenWithSecret(telegramCiphertext, newJWT)
	if err != nil || value != "123456:test-token" {
		t.Fatalf("telegram token = %q, %v", value, err)
	}
	var acmeCiphertext string
	if err := db.QueryRow("SELECT acme_token_ciphertext FROM panel_settings WHERE id=1").Scan(&acmeCiphertext); err != nil {
		t.Fatal(err)
	}
	value, err = decryptPanelACMETokenWithSecret(acmeCiphertext, newJWT)
	if err != nil || value != "cloudflare-backup-token-value" {
		t.Fatalf("ACME token = %q, %v", value, err)
	}
	if _, err := decryptPanelACMETokenWithSecret(acmeCiphertext, oldJWT); err == nil {
		t.Fatal("old JWT secret still decrypts migrated ACME token")
	}
	var tmdbCiphertext string
	if err := db.QueryRow("SELECT token_ciphertext FROM tmdb_settings WHERE id=1").Scan(&tmdbCiphertext); err != nil {
		t.Fatal(err)
	}
	value, err = decryptTMDBReadTokenWithSecret(tmdbCiphertext, newJWT)
	if err != nil || value != testTMDBReadToken {
		t.Fatalf("TMDB token = %q, %v", value, err)
	}
	if _, err := decryptTMDBReadTokenWithSecret(tmdbCiphertext, oldJWT); err == nil {
		t.Fatal("old JWT secret still decrypts migrated TMDB token")
	}
}

func TestReencryptRestoredSecretsSupportsBackupWithoutTMDBSettings(t *testing.T) {
	oldJWT := bytes.Repeat([]byte("j"), 32)
	newJWT := bytes.Repeat([]byte("n"), 32)
	oldHeader := sha256.Sum256([]byte("old-upstream-header-key-material"))
	newHeader := sha256.Sum256([]byte("new-upstream-header-key-material"))
	path := filepath.Join(t.TempDir(), "legacy-backup.db")
	makeBackupDatabase(t, path, oldJWT, oldHeader[:])
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DROP TABLE tmdb_settings"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reencryptRestoredSecrets(path, oldJWT, oldHeader[:], newJWT, newHeader[:]); err != nil {
		t.Fatalf("legacy backup migration failed: %v", err)
	}
}

func TestReencryptRestoredSecretsContinuesAfterCurrentACMEToken(t *testing.T) {
	oldJWT := bytes.Repeat([]byte("j"), 32)
	newJWT := bytes.Repeat([]byte("n"), 32)
	oldHeader := sha256.Sum256([]byte("old-upstream-header-key-material"))
	newHeader := sha256.Sum256([]byte("new-upstream-header-key-material"))
	path := filepath.Join(t.TempDir(), "mixed-token-backup.db")
	makeBackupDatabase(t, path, oldJWT, oldHeader[:])
	currentACME, err := encryptPanelACMETokenWithSecret("target-cloudflare-token-value", newJWT)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE panel_settings SET acme_token_ciphertext=? WHERE id=1", currentACME); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reencryptRestoredSecrets(path, oldJWT, oldHeader[:], newJWT, newHeader[:]); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var acmeCiphertext, tmdbCiphertext string
	if err := db.QueryRow("SELECT acme_token_ciphertext FROM panel_settings WHERE id=1").Scan(&acmeCiphertext); err != nil {
		t.Fatal(err)
	}
	if acmeCiphertext != currentACME {
		t.Fatal("ACME token already encrypted with the target key was rewritten")
	}
	if err := db.QueryRow("SELECT token_ciphertext FROM tmdb_settings WHERE id=1").Scan(&tmdbCiphertext); err != nil {
		t.Fatal(err)
	}
	if value, err := decryptTMDBReadTokenWithSecret(tmdbCiphertext, newJWT); err != nil || value != testTMDBReadToken {
		t.Fatalf("TMDB migration after current ACME token = %q, %v", value, err)
	}
}

func TestReencryptRestoredSecretsKeepsCurrentTMDBToken(t *testing.T) {
	oldJWT := bytes.Repeat([]byte("j"), 32)
	newJWT := bytes.Repeat([]byte("n"), 32)
	oldHeader := sha256.Sum256([]byte("old-upstream-header-key-material"))
	newHeader := sha256.Sum256([]byte("new-upstream-header-key-material"))
	path := filepath.Join(t.TempDir(), "current-tmdb-token.db")
	makeBackupDatabase(t, path, oldJWT, oldHeader[:])
	currentTMDB, err := encryptTMDBReadTokenWithSecret(testTMDBReadToken, newJWT)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE tmdb_settings SET token_ciphertext=? WHERE id=1", currentTMDB); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reencryptRestoredSecrets(path, oldJWT, oldHeader[:], newJWT, newHeader[:]); err != nil {
		t.Fatal(err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ciphertext string
	if err := db.QueryRow("SELECT token_ciphertext FROM tmdb_settings WHERE id=1").Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if ciphertext != currentTMDB {
		t.Fatal("TMDB token already encrypted with the target key was rewritten")
	}
}

func makeBackupTokenDatabase(t *testing.T, telegramToken, tmdbToken string, dropTMDB bool) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.db")
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("UPDATE telegram_report_settings SET bot_token_ciphertext=? WHERE id=1", telegramToken); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if dropTMDB {
		if _, err := db.db.Exec("DROP TABLE tmdb_settings"); err != nil {
			db.Close()
			t.Fatal(err)
		}
	} else if _, err := db.db.Exec("UPDATE tmdb_settings SET token_ciphertext=? WHERE id=1", tmdbToken); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestEphemeralJWTBackupPreflightDetectsTelegramOrTMDBToken(t *testing.T) {
	for _, test := range []struct {
		name      string
		telegram  string
		tmdb      string
		dropTMDB  bool
		wantToken bool
	}{
		{name: "no token"},
		{name: "legacy backup without TMDB settings", dropTMDB: true},
		{name: "Telegram token", telegram: "encrypted-telegram-token", wantToken: true},
		{name: "TMDB token", tmdb: "encrypted-tmdb-token", wantToken: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			hasToken, err := backupHasJWTProtectedToken(makeBackupTokenDatabase(t, test.telegram, test.tmdb, test.dropTMDB))
			if err != nil || hasToken != test.wantToken {
				t.Fatalf("preflight token=%v, err=%v; want %v", hasToken, err, test.wantToken)
			}
		})
	}
}

func TestHandleBackupRestoreRejectsTMDBTokenWithEphemeralJWT(t *testing.T) {
	originalEphemeral := jwtSecretEphemeral
	jwtSecretEphemeral = true
	t.Cleanup(func() { jwtSecretEphemeral = originalEphemeral })

	oldJWT := bytes.Repeat([]byte("j"), 32)
	oldHeader := bytes.Repeat([]byte("h"), 32)
	tmdbCiphertext, err := encryptTMDBReadTokenWithSecret(testTMDBReadToken, oldJWT)
	if err != nil {
		t.Fatal(err)
	}
	includeTLS := false
	manifest := backupManifest{
		Format:            "meridian-backup",
		FormatVersion:     backupFormatVersion,
		Files:             []string{backupDatabaseEntry},
		IncludeTLS:        &includeTLS,
		JWTSecret:         base64.RawStdEncoding.EncodeToString(oldJWT),
		UpstreamHeaderKey: base64.RawStdEncoding.EncodeToString(oldHeader),
	}
	archive := testBackupArchive(t, manifest, map[string][]byte{
		backupDatabaseEntry: makeBackupTokenDatabase(t, "", tmdbCiphertext, false),
	})
	const password = "correct horse battery staple"
	payload, err := sealBackup(archive, password)
	if err != nil {
		t.Fatal(err)
	}
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	if err := writer.WriteField("password", password); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("confirm", "恢复"); err != nil {
		t.Fatal(err)
	}
	file, err := writer.CreateFormFile("backup", "backup.mrbak")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	app := &App{dbPath: filepath.Join(t.TempDir(), "target.db")}
	request := httptest.NewRequest(http.MethodPost, "/api/backup/restore", &form)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	app.handleBackupRestore(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "TMDB") {
		t.Fatalf("ephemeral JWT restore status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestApplyPendingRestoreAndRollback(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meridian.db")
	if err := os.WriteFile(dbPath, []byte("old database"), 0o600); err != nil {
		t.Fatal(err)
	}
	tlsDir := filepath.Join(dir, "tls")
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tlsDir, "fullchain.pem"), []byte("old cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	pending := dbPath + backupPendingSuffix
	if err := os.MkdirAll(filepath.Join(pending, "tls"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pending, backupDatabaseEntry), []byte("new database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pending, filepath.FromSlash(backupTLSCertificate)), []byte("new cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker, _ := json.Marshal(restoreMarker{Files: []string{backupDatabaseEntry, backupTLSCertificate}})
	if err := os.WriteFile(filepath.Join(pending, "restore.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := applyPendingRestore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(dbPath)
	if string(data) != "new database" {
		t.Fatalf("applied database = %q", data)
	}
	if err := rollbackAppliedRestore(dbPath, state); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(dbPath)
	if string(data) != "old database" {
		t.Fatalf("rolled back database = %q", data)
	}
	data, _ = os.ReadFile(filepath.Join(tlsDir, "fullchain.pem"))
	if string(data) != "old cert" {
		t.Fatalf("rolled back cert = %q", data)
	}
}

func TestBackupExporterRejectsUnsafeEntriesAndFileCountOverflow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entry")
	if err := os.WriteFile(path, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	files := make([]string, backupMaxFiles-1)
	expanded := int64(0)
	if err := addBackupEntry(writer, &files, &expanded, backupTLSEdgeNodesPrefix+"node!/fullchain.pem", path); err == nil {
		t.Fatal("unsafe edge node entry was accepted")
	}
	files = make([]string, backupMaxFiles-1)
	if err := addBackupEntry(writer, &files, &expanded, backupTLSEdgeNodesPrefix+"node-a/fullchain.pem", path); err == nil {
		t.Fatal("backup file count overflow was accepted")
	}
	_ = writer.Close()
}

func TestTLSRestoreScopeProtectsCustomParents(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meridian.db")
	panelDir := filepath.Join(dir, "shared-panel")
	edgeDir := filepath.Join(dir, "shared-edge")
	if err := os.MkdirAll(panelDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(edgeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(panelDir, "sentinel.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(edgeDir, "sentinel.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PANEL_TLS_CERT_FILE", filepath.Join(panelDir, "panel.pem"))
	t.Setenv("PANEL_TLS_KEY_FILE", filepath.Join(panelDir, "panel.key"))
	t.Setenv("EDGE_TLS_CERT_FILE", filepath.Join(edgeDir, "edge.pem"))
	t.Setenv("EDGE_TLS_KEY_FILE", filepath.Join(edgeDir, "edge.key"))
	scope := managedTLSRestoreScope(dbPath)
	if len(scope.OwnedRoots) != 1 || filepath.Clean(scope.OwnedRoots[0]) != filepath.Clean(filepath.Join(dir, "tls")) {
		t.Fatalf("owned TLS roots = %#v", scope.OwnedRoots)
	}
	if len(scope.ExactPaths) != 0 {
		t.Fatalf("operator TLS files unexpectedly destructive: %#v", scope.ExactPaths)
	}
	for _, parent := range []string{panelDir, edgeDir} {
		for _, root := range scope.OwnedRoots {
			if filepath.Clean(root) == filepath.Clean(parent) {
				t.Fatalf("custom parent was treated as owned root: %s", parent)
			}
		}
	}
	rollback := filepath.Join(dir, "rollback")
	if err := snapshotTLSNamespace(dbPath, rollback); err != nil {
		t.Fatal(err)
	}
	if err := removeManagedTLSNamespace(dbPath); err != nil {
		t.Fatal(err)
	}
	for _, parent := range []string{panelDir, edgeDir} {
		if _, err := os.Stat(filepath.Join(parent, "sentinel.txt")); err != nil {
			t.Fatalf("custom parent sentinel removed: %s: %v", parent, err)
		}
	}
	if err := restoreTLSNamespaceSnapshot(dbPath, rollback); err != nil {
		t.Fatal(err)
	}
	for _, parent := range []string{panelDir, edgeDir} {
		if _, err := os.Stat(filepath.Join(parent, "sentinel.txt")); err != nil {
			t.Fatalf("custom parent sentinel missing after rollback: %s: %v", parent, err)
		}
	}
}

func TestTLSRestorePreservesOperatorCertificateFiles(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meridian.db")
	if err := os.WriteFile(dbPath, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(dir, "shared")
	if err := os.MkdirAll(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"panel.pem":    "panel-cert",
		"panel.key":    "panel-key",
		"edge.pem":     "edge-cert",
		"edge.key":     "edge-key",
		"sentinel.txt": "keep",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(shared, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	managed := filepath.Join(dir, "tls")
	if err := os.MkdirAll(managed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managed, "enabled"), []byte("true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PANEL_TLS_CERT_FILE", filepath.Join(shared, "panel.pem"))
	t.Setenv("PANEL_TLS_KEY_FILE", filepath.Join(shared, "panel.key"))
	t.Setenv("EDGE_TLS_CERT_FILE", filepath.Join(shared, "edge.pem"))
	t.Setenv("EDGE_TLS_KEY_FILE", filepath.Join(shared, "edge.key"))
	if got, want := targetTLSPath(dbPath, backupTLSCertificate), filepath.Join(dir, "tls", ".panel-current", "fullchain.pem"); got != want {
		t.Fatalf("panel restore target = %q, want managed state path %q", got, want)
	}
	scope := managedTLSRestoreScope(dbPath)
	for _, path := range []string{filepath.Join(shared, "panel.pem"), filepath.Join(shared, "panel.key"), filepath.Join(shared, "edge.pem"), filepath.Join(shared, "edge.key")} {
		for _, owned := range scope.OwnedRoots {
			if tlsPathsOverlap(owned, path) {
				t.Fatalf("operator file is inside destructive scope: %s", path)
			}
		}
	}
	rollback := filepath.Join(dir, "rollback")
	if err := snapshotTLSNamespace(dbPath, rollback); err != nil {
		t.Fatal(err)
	}
	if err := removeManagedTLSNamespace(dbPath); err != nil {
		t.Fatal(err)
	}
	if err := restoreTLSNamespaceSnapshot(dbPath, rollback); err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		data, err := os.ReadFile(filepath.Join(shared, name))
		if err != nil || string(data) != want {
			t.Fatalf("shared TLS file %s changed after restore: %q err=%v", name, data, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(managed, "enabled")); err != nil || string(data) != "true\n" {
		t.Fatalf("managed TLS state was not restored: %q err=%v", data, err)
	}
}

func TestTLSPathConfigurationRejectsRelativeAndOverlappingPaths(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meridian.db")
	t.Setenv("PANEL_TLS_CERT_FILE", "relative/panel.pem")
	t.Setenv("PANEL_TLS_KEY_FILE", "relative/panel.key")
	if err := validateTLSPathConfiguration(dbPath); err == nil {
		t.Fatal("relative TLS paths were accepted")
	}
	t.Setenv("PANEL_TLS_CERT_FILE", filepath.Join(dir, "tls", "panel.pem"))
	t.Setenv("PANEL_TLS_KEY_FILE", filepath.Join(dir, "tls", "panel.key"))
	t.Setenv("TLS_STATE_DIR", filepath.Join(dir, "tls"))
	if err := validateTLSPathConfiguration(dbPath); err == nil {
		t.Fatal("TLS paths inside TLS_STATE_DIR were accepted")
	}
	t.Setenv("PANEL_TLS_CERT_FILE", "")
	t.Setenv("PANEL_TLS_KEY_FILE", "")
	t.Setenv("TLS_STATE_DIR", dbPath+backupPendingSuffix)
	if err := validateTLSPathConfiguration(dbPath); err == nil {
		t.Fatal("TLS_STATE_DIR inside restore staging was accepted")
	}
}

func TestTLSPathConfigurationResolvesSymlinkedAncestors(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meridian.db")
	stateDir := filepath.Join(dir, "tls")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "tls-link")
	if err := os.Symlink(stateDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("TLS_STATE_DIR", stateDir)
	t.Setenv("PANEL_TLS_CERT_FILE", filepath.Join(link, "panel.pem"))
	t.Setenv("PANEL_TLS_KEY_FILE", filepath.Join(link, "panel.key"))
	if err := validateTLSPathConfiguration(dbPath); err == nil {
		t.Fatal("symlinked TLS path inside TLS_STATE_DIR was accepted")
	}
	linkedState := filepath.Join(dir, "linked-state")
	if err := os.Symlink(stateDir, linkedState); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("TLS_STATE_DIR", linkedState)
	t.Setenv("PANEL_TLS_CERT_FILE", filepath.Join(dir, "panel.pem"))
	t.Setenv("PANEL_TLS_KEY_FILE", filepath.Join(dir, "panel.key"))
	if err := validateTLSPathConfiguration(dbPath); err == nil {
		t.Fatal("symlinked TLS_STATE_DIR was accepted")
	}
	parentLink := filepath.Join(dir, "parent-link")
	if err := os.Symlink(dir, parentLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("TLS_STATE_DIR", filepath.Join(parentLink, "tls-state"))
	t.Setenv("PANEL_TLS_CERT_FILE", "")
	t.Setenv("PANEL_TLS_KEY_FILE", "")
	if err := validateTLSPathConfiguration(dbPath); err == nil {
		t.Fatal("TLS_STATE_DIR with a symlinked ancestor was accepted")
	}
}

func TestCopyTLSNamespaceTreeRejectsOverlappingPaths(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "tls")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "cert.pem"), []byte("cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyTLSNamespaceTree(source, filepath.Join(source, "rollback")); err == nil {
		t.Fatal("snapshot destination inside source was accepted")
	}
	if err := copyTLSNamespaceTree(filepath.Join(source, "nested"), source); err == nil {
		t.Fatal("snapshot source inside destination was accepted")
	}
}

func TestTLSRestoreReplacesManagedNamespaceAndRollbackRestoresIt(t *testing.T) {
	t.Setenv("PANEL_TLS_CERT_FILE", "")
	t.Setenv("PANEL_TLS_KEY_FILE", "")
	t.Setenv("EDGE_TLS_CERT_FILE", "")
	t.Setenv("EDGE_TLS_KEY_FILE", "")
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meridian.db")
	if err := os.WriteFile(dbPath, []byte("old database"), 0o600); err != nil {
		t.Fatal(err)
	}
	tlsDir := filepath.Join(dir, "tls")
	for _, guid := range []string{"node-a", "node-b", "node-c"} {
		current := filepath.Join(tlsDir, "edge-nodes", guid, "current")
		if err := os.MkdirAll(current, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(current, "fullchain.pem"), []byte("old-"+guid+"-cert"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(current, "privkey.pem"), []byte("old-"+guid+"-key"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pending := dbPath + backupPendingSuffix
	for _, guid := range []string{"node-a", "node-b"} {
		current := filepath.Join(pending, "tls", "edge-nodes", guid)
		if err := os.MkdirAll(current, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(current, "fullchain.pem"), []byte("new-"+guid+"-cert"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(current, "privkey.pem"), []byte("new-"+guid+"-key"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(pending, backupDatabaseEntry), []byte("new database"), 0o600); err != nil {
		t.Fatal(err)
	}
	includeTLS := true
	manifest := restoreMarker{Files: []string{
		backupDatabaseEntry,
		backupTLSEdgeNodesPrefix + "node-a/fullchain.pem",
		backupTLSEdgeNodesPrefix + "node-a/privkey.pem",
		backupTLSEdgeNodesPrefix + "node-b/fullchain.pem",
		backupTLSEdgeNodesPrefix + "node-b/privkey.pem",
	}, IncludeTLS: &includeTLS}
	markerData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pending, "restore.json"), markerData, 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := applyPendingRestore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tlsDir, "edge-nodes", "node-c")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale node-c TLS namespace remains after restore: %v", err)
	}
	for _, guid := range []string{"node-a", "node-b"} {
		data, err := os.ReadFile(filepath.Join(tlsDir, "edge-nodes", guid, "current", "fullchain.pem"))
		if err != nil || string(data) != "new-"+guid+"-cert" {
			t.Fatalf("restored %s certificate=%q err=%v", guid, data, err)
		}
	}
	if err := rollbackAppliedRestore(dbPath, state); err != nil {
		t.Fatal(err)
	}
	for _, guid := range []string{"node-a", "node-b", "node-c"} {
		data, err := os.ReadFile(filepath.Join(tlsDir, "edge-nodes", guid, "current", "fullchain.pem"))
		if err != nil || string(data) != "old-"+guid+"-cert" {
			t.Fatalf("rolled back %s certificate=%q err=%v", guid, data, err)
		}
	}
}

func TestRestoreWithoutTLSPreservesTargetSettingsAndFiles(t *testing.T) {
	t.Setenv("PANEL_TLS_CERT_FILE", "")
	t.Setenv("PANEL_TLS_KEY_FILE", "")
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target.db")
	targetDB, err := openDB(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := targetDB.db.Exec(`UPDATE panel_settings SET panel_domain='target.example.com', route_domain='route.target.example.com', listen_port=9443, tls_enabled=1, configured=1 WHERE id=1`); err != nil {
		targetDB.Close()
		t.Fatal(err)
	}
	targetJWT := bytes.Repeat([]byte("n"), 32)
	targetACME, err := encryptPanelACMETokenWithSecret("target-cloudflare-token-value", targetJWT)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := targetDB.db.Exec(`UPDATE panel_settings SET acme_email='target@example.com', acme_dns_provider='cloudflare', acme_token_ciphertext=?, acme_staging=1 WHERE id=1`, targetACME); err != nil {
		t.Fatal(err)
	}
	preserved, err := readBackupPanelSettings(targetDB.db)
	if err != nil {
		targetDB.Close()
		t.Fatal(err)
	}
	targetDB.Close()

	tlsDir := filepath.Join(dir, "tls")
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(tlsDir, "fullchain.pem")
	if err := os.WriteFile(certPath, []byte("target certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldJWT := bytes.Repeat([]byte("j"), 32)
	newJWT := targetJWT
	oldHeader := bytes.Repeat([]byte("h"), 32)
	newHeader := bytes.Repeat([]byte("k"), 32)
	backupPath := filepath.Join(dir, "backup.db")
	makeBackupDatabase(t, backupPath, oldJWT, oldHeader)
	backupDB, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backupDB.Exec(`UPDATE panel_settings SET panel_domain='source.example.com', route_domain='route.source.example.com', listen_port=8443, tls_enabled=0, configured=1 WHERE id=1`); err != nil {
		backupDB.Close()
		t.Fatal(err)
	}
	backupDB.Close()
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	includeTLS := false
	manifest := backupManifest{
		Format:            "meridian-backup",
		FormatVersion:     backupFormatVersion,
		Files:             []string{backupDatabaseEntry},
		IncludeTLS:        &includeTLS,
		JWTSecret:         base64.RawStdEncoding.EncodeToString(oldJWT),
		UpstreamHeaderKey: base64.RawStdEncoding.EncodeToString(oldHeader),
	}
	if _, err := writeRestorePending(targetPath, manifest, map[string][]byte{backupDatabaseEntry: backupData}, newJWT, newHeader, preserved, false); err != nil {
		t.Fatal(err)
	}
	state, err := applyPendingRestore(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if state == nil {
		t.Fatal("restore was not applied")
	}
	restoredDB, err := sql.Open("sqlite", targetPath)
	if err != nil {
		t.Fatal(err)
	}
	var panelDomain, routeDomain, acmeEmail, acmeCiphertext string
	var listenPort, tlsEnabled, configured, acmeStaging int
	if err := restoredDB.QueryRow(`SELECT panel_domain, route_domain, listen_port, tls_enabled, configured, acme_email, acme_token_ciphertext, acme_staging FROM panel_settings WHERE id=1`).Scan(&panelDomain, &routeDomain, &listenPort, &tlsEnabled, &configured, &acmeEmail, &acmeCiphertext, &acmeStaging); err != nil {
		restoredDB.Close()
		t.Fatal(err)
	}
	restoredDB.Close()
	if panelDomain != preserved.PanelDomain || routeDomain != preserved.RouteDomain || listenPort != preserved.ListenPort || tlsEnabled != preserved.TLSEnabled || configured != preserved.Configured {
		t.Fatalf("target panel settings were not preserved: %q %q %d %d %d", panelDomain, routeDomain, listenPort, tlsEnabled, configured)
	}
	if acmeEmail != "target@example.com" || acmeStaging != 1 {
		t.Fatalf("target ACME settings were not preserved: email=%q staging=%d", acmeEmail, acmeStaging)
	}
	if value, err := decryptPanelACMETokenWithSecret(acmeCiphertext, newJWT); err != nil || value != "target-cloudflare-token-value" {
		t.Fatalf("target ACME token was not preserved: %q %v", value, err)
	}
	cert, err := os.ReadFile(certPath)
	if err != nil || string(cert) != "target certificate" {
		t.Fatalf("target certificate changed after restore: %q %v", cert, err)
	}
	if err := rollbackAppliedRestore(targetPath, state); err != nil {
		t.Fatal(err)
	}
	cert, err = os.ReadFile(certPath)
	if err != nil || string(cert) != "target certificate" {
		t.Fatalf("target certificate changed after rollback: %q %v", cert, err)
	}
}

func addBackupIngressSites(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows := []struct {
		name       string
		port       int
		publicHost string
		pathPrefix string
		mode       string
	}{
		{"shared-host", 18001, "host.source.example.com", "", ingressModeHost},
		{"shared-both", 18002, "both.source.example.com", "", ingressModeBoth},
		{"dedicated-port", 18003, "", "", ingressModePort},
		{"shared-path", 18004, "", "/emby", ingressModePath},
	}
	for _, row := range rows {
		if _, err := db.Exec(`INSERT INTO sites (name, listen_port, public_host, path_prefix, ingress_mode, target_url, stream_hosts, ua_mode, enabled) VALUES (?, ?, ?, ?, ?, 'https://origin.example.com', '[]', 'passthrough', 1)`, row.name, row.port, row.publicHost, row.pathPrefix, row.mode); err != nil {
			t.Fatal(err)
		}
	}
}

func restoreIngressFixture(t *testing.T, targetHostIngressWithoutTLS bool, targetSettings backupPanelSettings) (string, int64) {
	t.Helper()
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target.db")
	targetDB, err := openDB(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := targetDB.db.Exec(`UPDATE panel_settings SET panel_domain=?, route_domain=?, listen_port=?, tls_enabled=?, configured=? WHERE id=1`, targetSettings.PanelDomain, targetSettings.RouteDomain, targetSettings.ListenPort, targetSettings.TLSEnabled, targetSettings.Configured); err != nil {
		targetDB.Close()
		t.Fatal(err)
	}
	preserved, err := readBackupPanelSettings(targetDB.db)
	if err != nil {
		targetDB.Close()
		t.Fatal(err)
	}
	targetDB.Close()

	oldJWT := bytes.Repeat([]byte("j"), 32)
	newJWT := bytes.Repeat([]byte("n"), 32)
	oldHeader := bytes.Repeat([]byte("h"), 32)
	newHeader := bytes.Repeat([]byte("k"), 32)
	backupPath := filepath.Join(dir, "backup.db")
	makeBackupDatabase(t, backupPath, oldJWT, oldHeader)
	addBackupIngressSites(t, backupPath)
	backupData, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	includeTLS := false
	manifest := backupManifest{
		Format:            "meridian-backup",
		FormatVersion:     backupFormatVersion,
		Files:             []string{backupDatabaseEntry},
		IncludeTLS:        &includeTLS,
		JWTSecret:         base64.RawStdEncoding.EncodeToString(oldJWT),
		UpstreamHeaderKey: base64.RawStdEncoding.EncodeToString(oldHeader),
	}
	resetCount, err := writeRestorePending(targetPath, manifest, map[string][]byte{backupDatabaseEntry: backupData}, newJWT, newHeader, preserved, targetHostIngressWithoutTLS)
	if err != nil {
		t.Fatal(err)
	}
	state, err := applyPendingRestore(targetPath)
	if err != nil || state == nil {
		t.Fatalf("apply restore state=%#v err=%v", state, err)
	}
	return targetPath, resetCount
}

func TestRestoreClearsUnsupportedIngressAndKeepsSites(t *testing.T) {
	path, resetCount := restoreIngressFixture(t, false, backupPanelSettings{ListenPort: 9090})
	if resetCount != 2 {
		t.Fatalf("reset ingress count=%d, want 2", resetCount)
	}
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sites, err := db.ListSites()
	if err != nil {
		t.Fatal(err)
	}
	if len(sites) != 5 { // makeBackupDatabase adds one dedicated-port fixture.
		t.Fatalf("restored sites=%d, want 5", len(sites))
	}
	for _, site := range sites {
		switch site.Name {
		case "shared-host", "shared-both":
			if site.IngressMode != ingressModeUnset || site.PublicHost != "" || site.Enabled {
				t.Fatalf("unsupported site %q ingress=%q host=%q enabled=%v", site.Name, site.IngressMode, site.PublicHost, site.Enabled)
			}
		case "dedicated-port":
			if site.IngressMode != ingressModePort || !site.Enabled {
				t.Fatalf("supported port site ingress=%q enabled=%v", site.IngressMode, site.Enabled)
			}
		case "shared-path":
			if site.IngressMode != ingressModePath || site.PathPrefix != "/emby" || !site.Enabled {
				t.Fatalf("supported path site ingress=%q path=%q enabled=%v", site.IngressMode, site.PathPrefix, site.Enabled)
			}
		}
	}
}

func TestRestorePreservesSupportedHostIngress(t *testing.T) {
	path, resetCount := restoreIngressFixture(t, true, backupPanelSettings{RouteDomain: "target.example.com", ListenPort: 9090, Configured: 1})
	if resetCount != 0 {
		t.Fatalf("reset ingress count=%d, want 0", resetCount)
	}
	db, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sites, err := db.ListSites()
	if err != nil {
		t.Fatal(err)
	}
	for _, site := range sites {
		if site.Name == "shared-host" && (site.IngressMode != ingressModeHost || site.PublicHost == "" || !site.Enabled) {
			t.Fatalf("supported host site changed: %#v", site)
		}
		if site.Name == "shared-both" && (site.IngressMode != ingressModeBoth || site.PublicHost == "" || !site.Enabled) {
			t.Fatalf("supported both site changed: %#v", site)
		}
	}
}

func TestInterruptedRestoreRollsBackAndDiscardsIncompleteStage(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meridian.db")
	rollback := dbPath + backupRollbackSuffix
	pending := dbPath + backupPendingSuffix
	if err := os.MkdirAll(rollback, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pending, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath, []byte("partial restored database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rollback, backupDatabaseEntry), []byte("original database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath+backupAppliedSuffix, []byte("pending validation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := applyPendingRestore(dbPath)
	if err != nil || state != nil {
		t.Fatalf("apply interrupted restore = %#v, %v", state, err)
	}
	data, err := os.ReadFile(dbPath)
	if err != nil || string(data) != "original database" {
		t.Fatalf("database after automatic rollback = %q, %v", data, err)
	}
	if _, err := os.Stat(pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete stage still exists: %v", err)
	}
}

func TestWriteRestorePendingRejectsCorruptSQLite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "target.db")
	manifest := backupManifest{
		Format:            "meridian-backup",
		FormatVersion:     1,
		Files:             []string{backupDatabaseEntry},
		JWTSecret:         base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte("j"), 32)),
		UpstreamHeaderKey: base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte("h"), 32)),
	}
	_, err := writeRestorePending(dbPath, manifest, map[string][]byte{backupDatabaseEntry: []byte("not sqlite")}, bytes.Repeat([]byte("n"), 32), bytes.Repeat([]byte("k"), 32), nil, false)
	if err == nil || !strings.Contains(err.Error(), "SQLite") && !strings.Contains(strings.ToLower(err.Error()), "database") {
		t.Fatalf("corrupt database error = %v", err)
	}
}
