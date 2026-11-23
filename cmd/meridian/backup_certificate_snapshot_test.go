package main

import (
	"archive/zip"
	"bytes"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestBackupExportRejectsMismatchedCertificatePair(t *testing.T) {
	for _, name := range []string{"TLS_STATE_DIR", "PANEL_TLS_CERT_FILE", "PANEL_TLS_KEY_FILE", "EDGE_TLS_CERT_FILE", "EDGE_TLS_KEY_FILE"} {
		t.Setenv(name, "")
	}
	app := newTestApp(t)
	app.dbPath = app.db.dbPath
	certA, _ := selfSignedPanelPair(t)
	_, keyB := selfSignedPanelPair(t)
	certFile, keyFile := panelTLSBackupPaths(app.dbPath)
	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, certA, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyB, 0o600); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/backup/export", strings.NewReader(`{"password":"synthetic-backup-password","include_tls":true}`))
	app.handleBackupExport(response, request)
	if response.Code == http.StatusOK {
		t.Fatal("export accepted a mismatched certificate/key pair")
	}
}

func TestBackupExportRequiresPairWhenSnapshotEnablesTLS(t *testing.T) {
	for _, name := range []string{"TLS_STATE_DIR", "PANEL_TLS_CERT_FILE", "PANEL_TLS_KEY_FILE", "EDGE_TLS_CERT_FILE", "EDGE_TLS_KEY_FILE"} {
		t.Setenv(name, "")
	}
	app := newTestApp(t)
	app.dbPath = app.db.dbPath
	if _, err := app.db.db.Exec("UPDATE panel_settings SET tls_enabled=1 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/backup/export", strings.NewReader(`{"password":"synthetic-backup-password","include_tls":true}`))
	app.handleBackupExport(response, request)
	if response.Code == http.StatusOK {
		t.Fatal("a TLS-enabled snapshot exported without its required certificate pair")
	}
}

func TestBackupCertificateSnapshotHandlesMissingAndOversizedFiles(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "fullchain.pem"), filepath.Join(dir, "privkey.pem")
	if _, exists, err := snapshotBackupCertificatePair(certFile, keyFile); err != nil || exists {
		t.Fatalf("absent optional pair: exists=%v err=%v", exists, err)
	}
	cert, key := selfSignedPanelPair(t)
	if err := os.WriteFile(certFile, cert, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := snapshotBackupCertificatePair(certFile, keyFile); err == nil {
		t.Fatal("one missing pair member was accepted")
	}
	if err := os.WriteFile(keyFile, key, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, bytes.Repeat([]byte("x"), int(backupAllowedEntries[backupTLSCertificate])+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := snapshotBackupCertificatePair(certFile, keyFile); err == nil {
		t.Fatal("oversized certificate was accepted")
	}
}

func TestBackupCertificateEntriesAreValidatedBeforeArchiveWrite(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "current", "fullchain.pem"), filepath.Join(dir, "current", "privkey.pem")
	certA, keyA := selfSignedPanelPair(t)
	certB, keyB := selfSignedPanelPair(t)
	if err := installCertificatePairAtomic(certFile, keyFile, certA, keyA); err != nil {
		t.Fatal(err)
	}
	snapshot, exists, err := snapshotBackupCertificatePair(certFile, keyFile)
	if err != nil || !exists {
		t.Fatalf("capture pair: exists=%v err=%v", exists, err)
	}
	if err := installCertificatePairAtomic(certFile, keyFile, certB, keyB); err != nil {
		t.Fatal(err)
	}
	// The source generation has changed. Archive the already validated pair,
	// ensuring the second entry cannot be reread from the new current pointer.
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	var files []string
	var size int64
	if err := addBackupBytes(writer, &files, &size, backupTLSCertificate, snapshot.certificate); err != nil {
		t.Fatal(err)
	}
	if err := addBackupBytes(writer, &files, &size, backupTLSPrivateKey, snapshot.privateKey); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	archive, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	entries := make(map[string][]byte)
	for _, file := range archive.File {
		opened, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		var content bytes.Buffer
		if _, err := content.ReadFrom(opened); err != nil {
			_ = opened.Close()
			t.Fatal(err)
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		entries[file.Name] = content.Bytes()
	}
	if !bytes.Equal(entries[backupTLSCertificate], certA) || !bytes.Equal(entries[backupTLSPrivateKey], keyA) {
		t.Fatal("archive contents changed after capturing the certificate pair")
	}
	if _, err := tls.X509KeyPair(entries[backupTLSCertificate], entries[backupTLSPrivateKey]); err != nil {
		t.Fatal(err)
	}
}

func TestBackupCertificateSnapshotsRemainPairedDuringRenewal(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "current", "fullchain.pem"), filepath.Join(dir, "current", "privkey.pem")
	certA, keyA := selfSignedPanelPair(t)
	certB, keyB := selfSignedPanelPair(t)
	if err := installCertificatePairAtomic(certFile, keyFile, certA, keyA); err != nil {
		t.Fatal(err)
	}
	var writers sync.WaitGroup
	writers.Add(1)
	errors := make(chan error, 1)
	go func() {
		defer writers.Done()
		for round := 0; round < 10; round++ {
			cert, key := certA, keyA
			if round%2 == 0 {
				cert, key = certB, keyB
			}
			if err := installCertificatePairAtomic(certFile, keyFile, cert, key); err != nil {
				errors <- err
				return
			}
		}
	}()
	for round := 0; round < 30; round++ {
		snapshot, exists, err := snapshotBackupCertificatePair(certFile, keyFile)
		if err != nil || !exists {
			writers.Wait()
			t.Fatalf("capture during renewal: exists=%v err=%v", exists, err)
		}
		if _, err := tls.X509KeyPair(snapshot.certificate, snapshot.privateKey); err != nil {
			writers.Wait()
			t.Fatal(err)
		}
	}
	writers.Wait()
	select {
	case err := <-errors:
		t.Fatal(err)
	default:
	}
}

func TestBackupExportDuringRenewalContainsRecoverableCertificatePair(t *testing.T) {
	for _, name := range []string{"TLS_STATE_DIR", "PANEL_TLS_CERT_FILE", "PANEL_TLS_KEY_FILE", "EDGE_TLS_CERT_FILE", "EDGE_TLS_KEY_FILE"} {
		t.Setenv(name, "")
	}
	app := newTestApp(t)
	app.dbPath = app.db.dbPath
	manager := newPanelCertificateManager(app.dbPath, nil)
	app.panelCertificates = manager
	certA, keyA := selfSignedPanelPair(t)
	certB, keyB := selfSignedPanelPair(t)
	pairA, err := tls.X509KeyPair(certA, keyA)
	if err != nil {
		t.Fatal(err)
	}
	pairB, err := tls.X509KeyPair(certB, keyB)
	if err != nil {
		t.Fatal(err)
	}
	issued := []*issuedPanelCertificate{{certPEM: certA, keyPEM: keyA, certificate: pairA}, {certPEM: certB, keyPEM: keyB, certificate: pairB}}
	if err := manager.install(issued[0], true); err != nil {
		t.Fatal(err)
	}
	var renewal sync.WaitGroup
	renewal.Add(1)
	renewalErrors := make(chan error, 1)
	go func() {
		defer renewal.Done()
		for iteration := 0; iteration < 10; iteration++ {
			if err := manager.install(issued[iteration%2], true); err != nil {
				renewalErrors <- err
				return
			}
		}
	}()
	defer renewal.Wait()
	for iteration := 0; iteration < 3; iteration++ {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/backup/export", strings.NewReader(`{"password":"synthetic-backup-password","include_tls":true}`))
		app.handleBackupExport(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("export during renewal returned %d", response.Code)
		}
		plain, err := openBackup(response.Body.Bytes(), "synthetic-backup-password")
		if err != nil {
			t.Fatal(err)
		}
		_, entries, err := parseBackupArchive(plain)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tls.X509KeyPair(entries[backupTLSCertificate], entries[backupTLSPrivateKey]); err != nil {
			t.Fatalf("successful export contained an unrecoverable TLS pair: %v", err)
		}
	}
	renewal.Wait()
	select {
	case err := <-renewalErrors:
		t.Fatal(err)
	default:
	}
}
