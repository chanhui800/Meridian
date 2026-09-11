//go:build windows

package main

import (
	"testing"
)

// TestEdgeCounterWindowsAggregate exercises the real IP Helper API path so a
// struct layout or API regression fails on a Windows host instead of silently
// disabling Agent traffic reporting.
func TestEdgeCounterWindowsAggregate(t *testing.T) {
	if edgeDefaultInterface() != edgeWindowsInterfaceName {
		t.Fatalf("edgeDefaultInterface() = %q, want %q", edgeDefaultInterface(), edgeWindowsInterfaceName)
	}
	rx, err := edgeCounter(edgeWindowsInterfaceName, "rx_bytes")
	if err != nil {
		t.Fatalf("edgeCounter(rx_bytes) failed: %v", err)
	}
	tx, err := edgeCounter(edgeWindowsInterfaceName, "tx_bytes")
	if err != nil {
		t.Fatalf("edgeCounter(tx_bytes) failed: %v", err)
	}
	if rx < 0 || tx < 0 {
		t.Fatalf("negative counters: rx=%d tx=%d", rx, tx)
	}
	if _, err := edgeCounter("eth0", "rx_bytes"); err == nil {
		t.Fatalf("edgeCounter with a non-aggregate interface must fail")
	}
	if _, err := edgeCounter(edgeWindowsInterfaceName, "packets"); err == nil {
		t.Fatalf("edgeCounter with an invalid counter name must fail")
	}
	epoch := edgeCounterEpoch(edgeWindowsInterfaceName)
	if epoch == "" || epoch == "unknown:"+edgeWindowsInterfaceName {
		t.Fatalf("edgeCounterEpoch = %q, want a boot-tick based epoch", epoch)
	}
}
