//go:build !windows

package main

import (
	"golang.org/x/sys/unix"
)

func diskAvailableBytes(path string) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(diskSpacePath(path), &stat); err != nil {
		return 0, err
	}
	return int64(stat.Bavail) * int64(stat.Bsize), nil
}
