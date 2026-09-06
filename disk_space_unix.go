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
	blocks := stat.Bavail
	blockSize := uint64(stat.Bsize)
	if blockSize == 0 {
		return 0, nil
	}
	const maxInt64 = uint64(1<<63 - 1)
	if blocks > maxInt64/blockSize {
		return int64(maxInt64), nil // #nosec G115 -- maxInt64 is the largest representable result.
	}
	return int64(blocks * blockSize), nil // #nosec G115 -- the overflow check above bounds the product.
}
