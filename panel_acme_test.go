package main

import (
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

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
