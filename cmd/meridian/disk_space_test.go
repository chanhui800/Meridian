package main

import (
	"errors"
	"strings"
	"testing"
)

func TestEnsureDiskSpaceRejectsInsufficientCapacity(t *testing.T) {
	previous := diskSpaceProvider
	diskSpaceProvider = func(string) (int64, error) { return 10 << 20, nil }
	t.Cleanup(func() { diskSpaceProvider = previous })
	if err := ensureDiskSpace(t.TempDir(), 11<<20); err == nil || !strings.Contains(err.Error(), "磁盘剩余空间不足") {
		t.Fatalf("low disk space was accepted: %v", err)
	}
}

func TestEnsureDiskSpaceAllowsUnknownPlatformCapacity(t *testing.T) {
	previous := diskSpaceProvider
	diskSpaceProvider = func(string) (int64, error) { return 0, errDiskSpaceUnsupported }
	t.Cleanup(func() { diskSpaceProvider = previous })
	if err := ensureDiskSpace(t.TempDir(), 1<<30); err != nil {
		t.Fatalf("unknown disk capacity should retain bounded-size fallback: %v", err)
	}
}

func TestEnsureDiskSpaceFailsClosedOnProviderError(t *testing.T) {
	previous := diskSpaceProvider
	diskSpaceProvider = func(string) (int64, error) { return 0, errors.New("statfs failed") }
	t.Cleanup(func() { diskSpaceProvider = previous })
	if err := ensureDiskSpace(t.TempDir(), 1<<20); err == nil || !strings.Contains(err.Error(), "检查磁盘剩余空间失败") {
		t.Fatalf("disk space provider error was accepted: %v", err)
	}
}
