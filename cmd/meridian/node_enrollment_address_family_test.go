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
			wantSource := nodeAddressSourceEnrollment
			if testCase.hint.Family == "v4" {
				wantSource = nodeAddressSourceReport
			}
			if reported.AddressV4 != v4 || reported.AddressV6 != v6 || reported.AddressSource != wantSource {
				t.Fatal("legacy fallback suppressed a real public family hint")
			}
			if testCase.hint.Family == "v6" && reported.AddressV6Source == nodeAddressSourceEnrollment {
				t.Fatal("reported IPv6 was incorrectly classified as enrollment fallback")
			}
		})
	}
}

func TestEnrollmentFamilyRejectsInvalidAndNonPublicSources(t *testing.T) {
	for _, source := range []string{"not-an-ip", "140.245.35.86:443", "10.0.0.5", "100.64.0.1", "100.127.255.254", "127.0.0.1", "169.254.1.1", "0.0.0.0", "::1", "fe80::1", "fd00::1", "ff02::1"} {
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

func TestAgentReportUsesObservedPublicAddressForNATNode(t *testing.T) {
	for _, testCase := range []struct {
		name, source, wantV4, wantV6, wantV4Source, wantV6Source string
	}{
		{"nat_ipv4", "140.245.35.86", "140.245.35.86", "", nodeAddressSourceReport, ""},
		{"nat_ipv6", "2606:4700:4700::1111", "", "2606:4700:4700::1111", nodeAddressSourceManual, nodeAddressSourceReport},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			app := newTestApp(t)
			now := time.Now()
			_, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: testCase.name}, now)
			if err != nil {
				t.Fatal(err)
			}
			_, token, err := app.db.EnrollControlNodeFromSource(enrollment, now, "10.0.0.5")
			if err != nil {
				t.Fatal(err)
			}
			reported, err := app.db.RecordNodeReportResultFromSource(token, NodeReport{
				BootID: "nat-report", Sequence: 1, InterfaceName: "eth0",
			}, now.Add(time.Second), testCase.source)
			if err != nil {
				t.Fatal(err)
			}
			if reported.Node.AddressV4 != testCase.wantV4 || reported.Node.AddressV6 != testCase.wantV6 {
				t.Fatalf("observed NAT address was not adopted: v4=%q v6=%q", reported.Node.AddressV4, reported.Node.AddressV6)
			}
			if reported.Node.AddressSource != testCase.wantV4Source || reported.Node.AddressV6Source != testCase.wantV6Source {
				t.Fatalf("address source markers = %q/%q, want %q/%q", reported.Node.AddressSource, reported.Node.AddressV6Source, testCase.wantV4Source, testCase.wantV6Source)
			}
		})
	}

	app := newTestApp(t)
	now := time.Now()
	manual, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "manual-ip-preserved", AddressV4: "203.0.113.40"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	reported, err := app.db.RecordNodeReportResultFromSource(token, NodeReport{
		BootID: "nat-report", Sequence: 1, InterfaceName: "eth0",
	}, now.Add(time.Second), "140.245.35.86")
	if err != nil {
		t.Fatal(err)
	}
	if reported.Node.AddressV4 != manual.AddressV4 || reported.Node.AddressSource != nodeAddressSourceManual {
		t.Fatalf("manual address was overwritten or relabeled: v4=%q source=%q", reported.Node.AddressV4, reported.Node.AddressSource)
	}

	for _, source := range []string{"10.0.0.5", "100.64.0.1", "127.0.0.1", "169.254.1.1", "fd00::1", "not-an-ip"} {
		t.Run("reject_"+source, func(t *testing.T) {
			app := newTestApp(t)
			now := time.Now()
			_, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "reject-source"}, now)
			if err != nil {
				t.Fatal(err)
			}
			_, token, err := app.db.EnrollControlNode(enrollment, now)
			if err != nil {
				t.Fatal(err)
			}
			reported, err := app.db.RecordNodeReportResultFromSource(token, NodeReport{
				BootID: "nat-report", Sequence: 1, InterfaceName: "eth0",
			}, now.Add(time.Second), source)
			if err != nil {
				t.Fatal(err)
			}
			if reported.Node.AddressV4 != "" || reported.Node.AddressV6 != "" {
				t.Fatalf("non-public observed source was published: v4=%q v6=%q", reported.Node.AddressV4, reported.Node.AddressV6)
			}
		})
	}
}

func TestAgentReportRefreshesOnlyAutomaticallyObservedNATAddresses(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	_, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "dynamic-nat"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNodeFromSource(enrollment, now, "10.0.0.5")
	if err != nil {
		t.Fatal(err)
	}

	for index, source := range []string{"140.245.35.86", "140.245.35.87", "2606:4700:4700::1111", "2606:4700:4700::2222"} {
		reported, err := app.db.RecordNodeReportResultFromSource(token, NodeReport{
			BootID: "dynamic-nat-report", Sequence: int64(index + 1), InterfaceName: "eth0",
		}, now.Add(time.Duration(index+1)*time.Second), source)
		if err != nil {
			t.Fatal(err)
		}
		if index == 1 && reported.Node.AddressV4 != "140.245.35.87" {
			t.Fatalf("updated IPv4 egress was not adopted: %q", reported.Node.AddressV4)
		}
		if index == 2 && (reported.Node.AddressV4 != "140.245.35.87" || reported.Node.AddressV6 != "2606:4700:4700::1111") {
			t.Fatalf("new IPv6 egress was not added without losing IPv4: v4=%q v6=%q", reported.Node.AddressV4, reported.Node.AddressV6)
		}
		if index == 3 && reported.Node.AddressV6 != "2606:4700:4700::2222" {
			t.Fatalf("updated IPv6 egress was not adopted: %q", reported.Node.AddressV6)
		}
	}

	manual, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "manual-nat", AddressV4: "203.0.113.40", AddressV6: "2001:db8::40"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, manualToken, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	for index, source := range []string{"140.245.35.88", "2606:4700:4700::3333"} {
		reported, err := app.db.RecordNodeReportResultFromSource(manualToken, NodeReport{
			BootID: "manual-nat-report", Sequence: int64(index + 1), InterfaceName: "eth0",
		}, now.Add(time.Duration(index+5)*time.Second), source)
		if err != nil {
			t.Fatal(err)
		}
		if reported.Node.AddressV4 != manual.AddressV4 || reported.Node.AddressV6 != manual.AddressV6 || reported.Node.AddressSource != nodeAddressSourceManual || nodeAddressSourceIsAutomatic(reported.Node.AddressV6Source) {
			t.Fatalf("heartbeat replaced a manually saved address: v4=%q v6=%q sources=%q/%q", reported.Node.AddressV4, reported.Node.AddressV6, reported.Node.AddressSource, reported.Node.AddressV6Source)
		}
	}
}

func TestAgentReportCannotForgeControllerObservedAddress(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	_, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "ipv4-nat-over-v6-control"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNodeFromSource(enrollment, now, "10.0.0.5")
	if err != nil {
		t.Fatal(err)
	}
	reported, err := app.db.RecordNodeReportResultFromSource(token, NodeReport{
		BootID: "v6-control-report", Sequence: 1, InterfaceName: "eth0",
		NetAddresses: []NodeNetAddress{{Family: "v4", Address: "140.245.35.86", Interface: "controller"}},
	}, now.Add(time.Second), "2606:4700:4700::1111")
	if err != nil {
		t.Fatal(err)
	}
	if reported.Node.AddressV4 != "" || reported.Node.AddressV6 != "2606:4700:4700::1111" {
		t.Fatalf("Agent-supplied controller marker was trusted: v4=%q v6=%q", reported.Node.AddressV4, reported.Node.AddressV6)
	}
	if reported.Node.AddressSource != nodeAddressSourceManual || reported.Node.AddressV6Source != nodeAddressSourceReport {
		t.Fatalf("untrusted marker changed address provenance = %q/%q", reported.Node.AddressSource, reported.Node.AddressV6Source)
	}

	observed, err := app.db.RecordNodeObservedAddress(token, "v4", "140.245.35.86", now.Add(2*time.Second))
	if err != nil || observed != "140.245.35.86" {
		t.Fatalf("server-observed IPv4 was not persisted: observed=%q err=%v", observed, err)
	}
	updated := findNodeByName(t, app, now.Add(2*time.Second), "ipv4-nat-over-v6-control")
	if updated.AddressV4 != "140.245.35.86" || updated.AddressV6 != "2606:4700:4700::1111" {
		t.Fatalf("authenticated server observation did not update both families: v4=%q v6=%q", updated.AddressV4, updated.AddressV6)
	}
}

func TestObservedAddressPreservesMigratedManualLegacyFamily(t *testing.T) {
	for _, testCase := range []struct {
		name, legacy, family, observed string
		wantV4, wantV6                 string
	}{
		{name: "manual_ipv4", legacy: "203.0.113.40", family: "v4", observed: "140.245.35.86", wantV4: "", wantV6: ""},
		{name: "manual_ipv6_then_ipv4_discovery", legacy: "2001:db8::40", family: "v4", observed: "140.245.35.86", wantV4: "140.245.35.86", wantV6: ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			app := newTestApp(t)
			now := time.Now()
			node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: testCase.name}, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := app.db.db.Exec(`UPDATE control_nodes SET address=?,address_source='manual',address_v4='',address_v6='',address_v6_source='' WHERE id=?`, testCase.legacy, node.ID); err != nil {
				t.Fatal(err)
			}
			_, token, err := app.db.EnrollControlNode(enrollment, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := app.db.RecordNodeObservedAddress(token, testCase.family, testCase.observed, now.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			updated := findNodeByName(t, app, now.Add(time.Second), testCase.name)
			if updated.Address != testCase.legacy || updated.AddressV4 != testCase.wantV4 || updated.AddressV6 != testCase.wantV6 {
				t.Fatalf("legacy manual address changed: address=%q v4=%q v6=%q", updated.Address, updated.AddressV4, updated.AddressV6)
			}
			if testCase.name == "manual_ipv6_then_ipv4_discovery" {
				if updated.AddressV6Source != nodeAddressSourceManual {
					t.Fatalf("manual legacy IPv6 provenance was not preserved: %q", updated.AddressV6Source)
				}
				if _, err := app.db.RecordNodeObservedAddress(token, "v6", "2606:4700:4700::1111", now.Add(2*time.Second)); err != nil {
					t.Fatal(err)
				}
				updated = findNodeByName(t, app, now.Add(2*time.Second), testCase.name)
				if updated.Address != testCase.legacy || updated.AddressV6 != "" {
					t.Fatalf("later probe shadowed legacy manual IPv6: address=%q v6=%q", updated.Address, updated.AddressV6)
				}
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
