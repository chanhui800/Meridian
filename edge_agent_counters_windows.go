//go:build windows

package main

import (
	"errors"
	"math"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsIfTypeLoopback is IF_TYPE_SOFTWARE_LOOPBACK from the IANA ifType
// registry; x/sys does not export the named constant.
const windowsIfTypeLoopback = 24

var kernel32 = syscall.NewLazyDLL("kernel32.dll")
var procGetTickCount64 = kernel32.NewProc("GetTickCount64")

// bootTickMilliseconds returns milliseconds since boot via GetTickCount64,
// which x/sys does not wrap. The value resets at boot, giving the Controller
// the same counter reset signal the Linux boot_id provides.
func bootTickMilliseconds() uint64 {
	tick, _, _ := procGetTickCount64.Call()
	return uint64(tick)
}

// edgeWindowsInterfaceName identifies the aggregate of every non-loopback
// interface. Windows exposes per-adapter byte counters only through the IP
// Helper API and adapters are renamed freely, so the Agent reports one summed
// counter instead of chasing the default-route adapter.
const edgeWindowsInterfaceName = "windows-aggregate"

// edgeDefaultInterface returns the synthetic aggregate identity; see
// edgeWindowsInterfaceName.
func edgeDefaultInterface() string {
	return edgeWindowsInterfaceName
}

// edgeCounter returns the summed rx/tx octets of every up non-loopback
// interface via the IP Helper API, replacing the Linux-only sysfs source.
func edgeCounter(interfaceName, name string) (int64, error) {
	if interfaceName != edgeWindowsInterfaceName {
		return 0, errors.New("unknown interface")
	}
	switch name {
	case "rx_bytes", "tx_bytes":
	default:
		return 0, errors.New("invalid counter")
	}
	var table *windows.MibIfTable2
	if err := windows.GetIfTable2Ex(windows.MibIfTableNormal, &table); err != nil {
		return 0, err
	}
	defer windows.FreeMibTable(unsafe.Pointer(table)) // #nosec G103 -- table is allocated by GetIfTable2Ex with this exact element type.
	rows := unsafe.Slice(&table.Table[0], table.NumEntries)
	var rx, tx int64
	for i := range rows {
		row := &rows[i]
		if row.Type == windowsIfTypeLoopback || row.OperStatus != uint32(windows.IfOperStatusUp) {
			continue
		}
		rx += trafficOctetsToBytes(row.InOctets)
		tx += trafficOctetsToBytes(row.OutOctets)
	}
	if name == "rx_bytes" {
		return rx, nil
	}
	return tx, nil
}

// trafficOctetsToBytes converts an unsigned interface octet counter into the
// signed byte accounting used by reports, clamping at the int64 maximum. A
// 64-bit NIC counter only reaches that bound after 8 exabytes, far past any
// reset window, but the conversion must not be allowed to wrap negative.
func trafficOctetsToBytes(value uint64) int64 {
	if value > uint64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(value)
}

// edgeCounterEpoch binds the cumulative counters to the Windows boot session,
// giving the Controller the same counter reset signal the Linux boot_id
// provides.
func edgeCounterEpoch(interfaceName string) string {
	return strconv.FormatUint(bootTickMilliseconds(), 10) + ":" + interfaceName
}
