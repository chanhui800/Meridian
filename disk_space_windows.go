//go:build windows

package main

func diskAvailableBytes(string) (int64, error) {
	return 0, errDiskSpaceUnsupported
}
