//go:build !windows

package main

import (
	"bufio"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// edgeDefaultInterface discovers the interface carrying the default route via
// the kernel route table and falls back to the first up non-loopback adapter.
func edgeDefaultInterface() string {
	file, err := os.Open("/proc/net/route")
	if err == nil {
		defer file.Close()
		scanner := bufio.NewScanner(file)
		_ = scanner.Scan()
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) >= 4 && fields[1] == "00000000" {
				flags, parseErr := strconv.ParseUint(fields[3], 16, 32)
				if parseErr == nil && flags&2 != 0 {
					return fields[0]
				}
			}
		}
	}
	interfaces, _ := net.Interfaces()
	for _, candidate := range interfaces {
		if candidate.Flags&net.FlagUp != 0 && candidate.Flags&net.FlagLoopback == 0 {
			return candidate.Name
		}
	}
	return ""
}

// edgeCounter reads a sysfs statistics counter for the discovered interface.
func edgeCounter(interfaceName, name string) (int64, error) {
	if name != "rx_bytes" && name != "tx_bytes" {
		return 0, errors.New("invalid counter")
	}
	data, err := os.ReadFile(filepath.Join("/sys/class/net", interfaceName, "statistics", name)) // #nosec G304 -- interface is discovered from the kernel route table.
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}

// edgeCounterEpoch binds the cumulative counters to the kernel boot so the
// Controller can detect a counter reset after a reboot.
func edgeCounterEpoch(interfaceName string) string {
	kernelBootID := "unknown"
	if data, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		kernelBootID = strings.TrimSpace(string(data))
	}
	return kernelBootID + ":" + interfaceName
}
