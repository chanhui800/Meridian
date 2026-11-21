package main

import (
	"testing"
	"time"
)

func TestEnrollmentObservedPublicAddressFillsMatchingFamily(t *testing.T) {
	for _, testCase := range []struct{ name, source, v4, v6 string }{
		{"ipv4", "140.245.35.86", "140.245.35.86", ""},
		{"ipv6", "2606:4700:4700::1111", "", "2606:4700:4700::1111"},
		{"mapped_ipv4", "::ffff:140.245.35.86", "140.245.35.86", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			app := newTestApp(t)
			now := time.Now()
			_, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "observed-" + testCase.name}, now)
			if err != nil {
				t.Fatal(err)
			}
			node, token, err := app.db.EnrollControlNodeFromSource(enrollment, now, testCase.source)
			if err != nil {
				t.Fatal(err)
			}
			if node.AddressV4 != testCase.v4 || node.AddressV6 != testCase.v6 || node.AddressSource != nodeAddressSourceEnrollment {
				t.Fatalf("observed family not persisted: address=%q v4=%q v6=%q source=%q", node.Address, node.AddressV4, node.AddressV6, node.AddressSource)
			}
			if testCase.v6 != "" && node.AddressV6Source != nodeAddressSourceEnrollment {
				t.Fatal("inferred IPv6 lost its review warning")
			}
			// A NAT-only interface list is normal. It must not erase enrollment's
			// public family, and it cannot invent an address of the other family.
			reported, err := app.db.RecordNodeReport(token, NodeReport{BootID: "nat", Sequence: 1, InterfaceName: "eth0", NetAddresses: nil}, now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if reported.AddressV4 != testCase.v4 || reported.AddressV6 != testCase.v6 || reported.AddressSource != nodeAddressSourceEnrollment {
				t.Fatal("NAT report erased or fabricated a public family")
			}
		})
	}
}

func TestNodeReportBackfillsLegacyEnrollmentFamilyWithoutPublicHints(t *testing.T) {
	for _, testCase := range []struct{ name, address, v4, v6 string }{
		{"ipv4", "140.245.35.86", "140.245.35.86", ""},
		{"ipv6", "2606:4700:4700::1111", "", "2606:4700:4700::1111"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			app := newTestApp(t)
			now := time.Now()
			node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "legacy-" + testCase.name}, now)
			if err != nil {
				t.Fatal(err)
			}
			_, token, err := app.db.EnrollControlNode(enrollment, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := app.db.db.Exec(`UPDATE control_nodes SET address=?,address_source='enrollment',address_v4='',address_v6='',reported_net_addresses='null' WHERE id=?`, testCase.address, node.ID); err != nil {
				t.Fatal(err)
			}
			for sequence := int64(1); sequence <= 2; sequence++ {
				reported, err := app.db.RecordNodeReport(token, NodeReport{BootID: "nat", Sequence: sequence, InterfaceName: "eth0", NetAddresses: nil}, now.Add(time.Duration(sequence)*time.Second))
				if err != nil {
					t.Fatal(err)
				}
				if reported.AddressV4 != testCase.v4 || reported.AddressV6 != testCase.v6 || reported.AddressSource != nodeAddressSourceEnrollment {
					t.Fatalf("legacy family not repaired idempotently: v4=%q v6=%q source=%q", reported.AddressV4, reported.AddressV6, reported.AddressSource)
				}
				if len(reported.NetAddresses) != 0 {
					t.Fatal("backfill fabricated interface telemetry")
				}
			}
		})
	}
}

func TestEnrollmentAndReportsPreserveManualAddressFamilies(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "manual-families", Address: "203.0.113.20", AddressV6: "2001:db8::20"}, now)
	if err != nil {
		t.Fatal(err)
	}
	enrolled, token, err := app.db.EnrollControlNodeFromSource(enrollment, now, "140.245.35.86")
	if err != nil {
		t.Fatal(err)
	}
	if enrolled.Address != node.Address || enrolled.AddressV4 != node.AddressV4 || enrolled.AddressV6 != node.AddressV6 || enrolled.AddressSource != nodeAddressSourceManual {
		t.Fatal("enrollment overwrote manual values")
	}
	reported, err := app.db.RecordNodeReport(token, NodeReport{BootID: "manual", Sequence: 1, InterfaceName: "eth0", NetAddresses: []NodeNetAddress{{Family: "v4", Address: "198.51.100.10"}, {Family: "v6", Address: "2001:db8::10"}}}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if reported.Address != node.Address || reported.AddressV4 != node.AddressV4 || reported.AddressV6 != node.AddressV6 || reported.AddressSource != nodeAddressSourceManual {
		t.Fatal("report overwrote manual values")
	}
}

func TestLegacyEnrollmentBackfillOnlyFillsEmptyFamily(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "existing-families"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET address='140.245.35.86',address_source='enrollment',address_v4='203.0.113.55' WHERE id=?`, node.ID); err != nil {
		t.Fatal(err)
	}
	reported, err := app.db.RecordNodeReport(token, NodeReport{BootID: "legacy", Sequence: 1, InterfaceName: "eth0", NetAddresses: []NodeNetAddress{{Family: "v4", Address: "198.51.100.10"}, {Family: "v6", Address: "2001:db8::10"}}}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if reported.AddressV4 != "203.0.113.55" || reported.AddressV6 != "2001:db8::10" {
		t.Fatal("legacy repair overwrote existing family or suppressed a real public IPv6 hint")
	}
}

func TestLegacyEnrollmentBackfillPrefersRealPublicHints(t *testing.T) {
	for _, testCase := range []struct {
		name, legacy string
		hint         NodeNetAddress
	}{
		{"ipv4", "140.245.35.86", NodeNetAddress{Family: "v4", Address: "198.51.100.88"}},
		{"ipv6", "2606:4700:4700::1111", NodeNetAddress{Family: "v6", Address: "2001:db8::88"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			app := newTestApp(t)
			now := time.Now()
			node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "real-hint-" + testCase.name}, now)
			if err != nil {
				t.Fatal(err)
			}
			_, token, err := app.db.EnrollControlNode(enrollment, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := app.db.db.Exec(`UPDATE control_nodes SET address=?,address_source='enrollment',address_v4='',address_v6='',address_v6_source='' WHERE id=?`, testCase.legacy, node.ID); err != nil {
				t.Fatal(err)
			}
			reported, err := app.db.RecordNodeReport(token, NodeReport{BootID: "public-hint", Sequence: 1, InterfaceName: "eth0", NetAddresses: []NodeNetAddress{testCase.hint}}, now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			v4, v6 := publicNodeAddressFamilies(testCase.hint.Address)
			if reported.AddressV4 != v4 || reported.AddressV6 != v6 || reported.AddressSource != nodeAddressSourceEnrollment {
				t.Fatal("legacy fallback suppressed a real public family hint")
			}
			if testCase.hint.Family == "v6" && reported.AddressV6Source == nodeAddressSourceEnrollment {
				t.Fatal("reported IPv6 was incorrectly classified as enrollment fallback")
			}
		})
	}
}

func TestEnrollmentFamilyRejectsInvalidAndNonPublicSources(t *testing.T) {
	for _, source := range []string{"not-an-ip", "140.245.35.86:443", "10.0.0.5", "127.0.0.1", "169.254.1.1", "0.0.0.0", "::1", "fe80::1", "fd00::1", "ff02::1"} {
		t.Run(source, func(t *testing.T) {
			app := newTestApp(t)
			now := time.Now()
			_, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "invalid"}, now)
			if err != nil {
				t.Fatal(err)
			}
			node, _, err := app.db.EnrollControlNodeFromSource(enrollment, now, source)
			if err != nil {
				t.Fatal(err)
			}
			if node.Address != "" || node.AddressV4 != "" || node.AddressV6 != "" || node.AddressSource != nodeAddressSourceManual {
				t.Fatal("invalid enrollment source was published")
			}
		})
	}
}

func TestNodeReportDoesNotBackfillInvalidLegacyEnrollmentAddress(t *testing.T) {
	for _, source := range []string{"not-an-ip", "10.0.0.5", "::1", "fd00::1"} {
		t.Run(source, func(t *testing.T) {
			app := newTestApp(t)
			now := time.Now()
			node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "invalid-legacy"}, now)
			if err != nil {
				t.Fatal(err)
			}
			_, token, err := app.db.EnrollControlNode(enrollment, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := app.db.db.Exec(`UPDATE control_nodes SET address=?,address_source='enrollment' WHERE id=?`, source, node.ID); err != nil {
				t.Fatal(err)
			}
			reported, err := app.db.RecordNodeReport(token, NodeReport{BootID: "invalid", Sequence: 1, InterfaceName: "eth0"}, now.Add(time.Second))
			if err != nil {
				t.Fatal(err)
			}
			if reported.AddressV4 != "" || reported.AddressV6 != "" {
				t.Fatal("invalid legacy value became a published family")
			}
		})
	}
}
