package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPanelACMETokenEncryptionRoundTripAndIsolation(t *testing.T) {
	secret := bytes.Repeat([]byte("a"), 32)
	otherSecret := bytes.Repeat([]byte("b"), 32)
	token := "cloudflare-token-for-automatic-renewal"
	ciphertext, err := encryptPanelACMETokenWithSecret(token, secret)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := decryptPanelACMETokenWithSecret(ciphertext, secret)
	if err != nil || plain != token {
		t.Fatalf("round trip = %q, %v", plain, err)
	}
	if _, err := decryptPanelACMETokenWithSecret(ciphertext, otherSecret); err == nil {
		t.Fatal("ciphertext decrypted with a different JWT secret")
	}
	tamperIndex := len(panelACMETokenCipherPrefix) + 1
	replacement := byte('A')
	if ciphertext[tamperIndex] == replacement {
		replacement = 'B'
	}
	tampered := ciphertext[:tamperIndex] + string(replacement) + ciphertext[tamperIndex+1:]
	if _, err := decryptPanelACMETokenWithSecret(tampered, secret); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}

func TestCertificateRenewalWindow(t *testing.T) {
	base := panelCertificateStatus{Configured: true, CertificateCurrent: true, CertificateValid: true}
	base.DaysRemaining = 31
	if certificateNeedsRenewal(base) || !certificateCanBeReused(base) {
		t.Fatal("certificate with 31 days remaining should be reused")
	}
	base.DaysRemaining = 30
	if !certificateNeedsRenewal(base) || certificateCanBeReused(base) {
		t.Fatal("certificate with 30 days remaining should be renewed")
	}
	base.CertificateValid = false
	base.DaysRemaining = 90
	if !certificateNeedsRenewal(base) {
		t.Fatal("expired certificate should be renewed regardless of days remaining")
	}
}

func TestDisableExpiredPanelTLSIfNeeded(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "meridian.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.BootstrapPanelSettings("panel.example.com", "example.com", true, 9090); err != nil {
		t.Fatal(err)
	}

	tlsDir := t.TempDir()
	certFile := filepath.Join(tlsDir, "fullchain.pem")
	keyFile := filepath.Join(tlsDir, "privkey.pem")
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "*.example.com"},
		DNSNames:     []string{"*.example.com"},
		NotBefore:    now.Add(-48 * time.Hour),
		NotAfter:     now.Add(-time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}, &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "*.example.com"},
		DNSNames:     []string{"*.example.com"},
		NotBefore:    now.Add(-48 * time.Hour),
		NotAfter:     now.Add(-time.Hour),
	}, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(tlsDir, "enabled")
	if err := os.WriteFile(marker, []byte("enabled\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	manager := &panelCertificateManager{certFile: certFile, keyFile: keyFile, accountDir: tlsDir}
	disabled, err := disableExpiredPanelTLSIfNeeded(db, manager)
	if err != nil {
		t.Fatal(err)
	}
	if !disabled {
		t.Fatal("expired certificate was not disabled")
	}
	settings, err := db.PanelSettings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.TLSEnabled {
		t.Fatal("TLS remained enabled after certificate expiry")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("TLS marker still exists: %v", err)
	}
	if _, err := os.Stat(certFile); err != nil {
		t.Fatalf("certificate files should be preserved: %v", err)
	}
}

func TestLoadOrCreateACMEAccountKeyStaysWithinAccountDirectory(t *testing.T) {
	directory := t.TempDir()
	first, err := loadOrCreateACMEAccountKey(directory, "acme-account.pem")
	if err != nil {
		t.Fatalf("create account key: %v", err)
	}
	second, err := loadOrCreateACMEAccountKey(directory, "acme-account.pem")
	if err != nil {
		t.Fatalf("reload account key: %v", err)
	}
	firstPublic, err := x509.MarshalPKIXPublicKey(first.Public())
	if err != nil {
		t.Fatalf("marshal first public key: %v", err)
	}
	secondPublic, err := x509.MarshalPKIXPublicKey(second.Public())
	if err != nil {
		t.Fatalf("marshal second public key: %v", err)
	}
	if string(firstPublic) != string(secondPublic) {
		t.Fatal("reloaded account key does not match the created key")
	}
	if _, err := os.Stat(filepath.Join(directory, "acme-account.pem")); err != nil {
		t.Fatalf("account key file: %v", err)
	}

	if _, err := loadOrCreateACMEAccountKey(directory, "../escape.pem"); err == nil || err.Error() != "ACME account key filename must be a base name" {
		t.Fatalf("directory escape error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(directory), "escape.pem")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escape path was created: %v", err)
	}
}

func TestInstallCertificatePairAtomicKeepsMatchingCurrentPair(t *testing.T) {
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tlsServer.Close()
	certificate := tlsServer.TLS.Certificates[0]
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})
	keyDER, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "edge-nodes", "node", "current")
	certFile, keyFile := filepath.Join(root, "fullchain.pem"), filepath.Join(root, "privkey.pem")
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := installCertificatePairAtomic(certFile, keyFile, certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		t.Fatalf("current pair is not loadable: %v", err)
	}
}

func TestPanelCertificateManagerInstallUsesAtomicPair(t *testing.T) {
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tlsServer.Close()
	certificate := tlsServer.TLS.Certificates[0]
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})
	keyDER, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	issuedPair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	manager := &panelCertificateManager{certFile: filepath.Join(root, "fullchain.pem"), keyFile: filepath.Join(root, "privkey.pem"), accountDir: root}
	if err := manager.install(&issuedPanelCertificate{certPEM: certPEM, keyPEM: keyPEM, certificate: issuedPair}, true); err != nil {
		t.Fatal(err)
	}
	if _, enabled, err := manager.tlsConfig(true); err != nil || !enabled {
		t.Fatalf("installed panel pair could not be activated: enabled=%v err=%v", enabled, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".panel-current")); err != nil {
		t.Fatalf("atomic panel pointer missing: %v", err)
	}
}

func TestPanelTLSDisabledReasonDistinguishesManualAndFallback(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "meridian.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.BootstrapPanelSettings("panel.example.com", "example.com", true, 9090); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPanelTLSEnabled(false); err != nil {
		t.Fatal(err)
	}
	settings, err := db.PanelSettings()
	if err != nil || settings.TLSDisabledReason != panelTLSDisabledReasonManual {
		t.Fatalf("manual TLS disable reason=%q err=%v", settings.TLSDisabledReason, err)
	}
	if err := db.setPanelTLSExpiredFallback(); err != nil {
		t.Fatal(err)
	}
	settings, err = db.PanelSettings()
	if err != nil || settings.TLSDisabledReason != panelTLSDisabledReasonExpiredFallback {
		t.Fatalf("fallback TLS disable reason=%q err=%v", settings.TLSDisabledReason, err)
	}
	if err := db.SetPanelTLSEnabled(true); err != nil {
		t.Fatal(err)
	}
	settings, err = db.PanelSettings()
	if err != nil || settings.TLSDisabledReason != "" || !settings.TLSEnabled {
		t.Fatalf("TLS re-enable did not clear reason: %+v err=%v", settings, err)
	}
}

func TestEdgeCertificateIdentifiersAreUniquePerNode(t *testing.T) {
	first := edgeCertificateIdentifiers("example.com", "node-a")
	second := edgeCertificateIdentifiers("example.com", "node-b")
	if len(first) != 2 || len(second) != 2 || first[0] != "*.example.com" || second[0] != "*.example.com" {
		t.Fatalf("unexpected edge identifiers: %#v %#v", first, second)
	}
	if first[1] == second[1] || first[1] == first[0] || second[1] == second[0] {
		t.Fatalf("edge identifiers are not node-unique: %#v %#v", first, second)
	}
	if !strings.HasSuffix(first[1], ".edge.example.com") || !strings.HasSuffix(second[1], ".edge.example.com") {
		t.Fatalf("edge identifiers must live outside the route wildcard: %#v %#v", first, second)
	}
}

func TestCertificateGenerationCleanupKeepsCurrentAndPrevious(t *testing.T) {
	root := t.TempDir()
	generations := filepath.Join(root, "generations")
	current := filepath.Join(root, "current")
	if err := os.MkdirAll(generations, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"generation-old", "generation-previous", "generation-current"} {
		if err := os.Mkdir(filepath.Join(generations, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("generations", "generation-current"), current); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	if err := cleanupOldCertificateGenerations(generations, current, 2); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(generations)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("generation count = %d, want 2", len(entries))
	}
}
