package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// diskSpaceProvider is replaceable by tests so low-space behavior can be
// verified without filling the host filesystem.
var diskSpaceProvider = diskAvailableBytes

var errDiskSpaceUnsupported = errors.New("disk space check unsupported")

// ensureDiskSpace fails before a backup/restore transaction starts when the
// filesystem cannot hold its temporary copies. Platforms without a portable
// free-space syscall return nil from diskAvailableBytes and retain the
// existing bounded-size protections.
func ensureDiskSpace(path string, required int64) error {
	if required <= 0 {
		return nil
	}
	available, err := diskSpaceProvider(path)
	if errors.Is(err, errDiskSpaceUnsupported) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查磁盘剩余空间失败: %w", err)
	}
	if available <= 0 {
		return errors.New("无法确定磁盘剩余空间")
	}
	if available < required {
		return fmt.Errorf("磁盘剩余空间不足：需要至少 %d MiB，可用 %d MiB", required>>20, available>>20)
	}
	return nil
}

func diskSpacePath(path string) string {
	path = filepath.Clean(path)
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return filepath.Dir(path)
	}
	return path
}
