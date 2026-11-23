package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestACMEIssueLockIsSharedAcrossDatabaseConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acme-lock.db")
	first, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := openDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := first.acquireACMELock(context.Background(), "first", time.Minute); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	if err := second.acquireACMELock(ctx, "second", time.Minute); err == nil {
		t.Fatal("second connection acquired an active ACME lease")
	}
	if err := first.releaseACMELock("first"); err != nil {
		t.Fatal(err)
	}
	if err := second.acquireACMELock(context.Background(), "second", time.Minute); err != nil {
		t.Fatalf("second connection could not acquire released lease: %v", err)
	}
	if err := second.releaseACMELock("second"); err != nil {
		t.Fatal(err)
	}
}
