package main

import (
	"testing"
	"time"
)

func TestDesiredNodeDNSAddresses(t *testing.T) {
	tests := []struct {
		name string
		node ControlNode
		want []string
	}{
		{"v4", ControlNode{AddressV4: "203.0.113.10", DNSPublish: "auto"}, []string{"A:203.0.113.10"}},
		{"v6", ControlNode{AddressV6: "2001:db8::10", DNSPublish: "auto"}, []string{"AAAA:2001:db8::10"}},
		{"dual", ControlNode{AddressV4: "203.0.113.10", AddressV6: "2001:db8::10", DNSPublish: "auto"}, []string{"A:203.0.113.10", "AAAA:2001:db8::10"}},
		{"dual-v6-only", ControlNode{AddressV4: "203.0.113.10", AddressV6: "2001:db8::10", DNSPublish: "v6"}, []string{"AAAA:2001:db8::10"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := desiredNodeDNSAddresses(test.node)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(test.want) {
				t.Fatalf("got %v, want %v", got, test.want)
			}
			for i := range got {
				value := got[i].recordType + ":" + got[i].address
				if value != test.want[i] {
					t.Fatalf("got %q, want %q", value, test.want[i])
				}
			}
		})
	}
	if _, err := desiredNodeDNSAddresses(ControlNode{DNSPublish: "v6"}); err == nil {
		t.Fatal("DNS address generation succeeded before the selected IPv6 address was reported")
	}
}

func TestAutomaticListenerFollowsOnlyPublishableAddressFamilies(t *testing.T) {
	tests := []struct {
		name string
		node ControlNode
		want string
	}{
		{
			name: "public IPv4 only",
			node: ControlNode{DNSPublish: "auto", AddressV4: "198.51.100.10"},
			want: "v4",
		},
		{
			name: "public IPv6 only",
			node: ControlNode{DNSPublish: "auto", AddressV6: "2001:db8::10"},
			want: "v6",
		},
		{
			name: "legacy private IPv4 does not block public IPv6",
			node: ControlNode{DNSPublish: "auto", Address: "10.0.0.10", AddressV6: "2001:db8::10"},
			want: "v6",
		},
		{
			name: "legacy carrier NAT IPv4 does not block public IPv6",
			node: ControlNode{DNSPublish: "auto", Address: "100.64.0.10", AddressV6: "2001:db8::10"},
			want: "v6",
		},
		{
			name: "carrier NAT family field is not publishable",
			node: ControlNode{DNSPublish: "auto", AddressV4: "100.64.0.10"},
			want: "none",
		},
		{
			name: "private family field is not publishable",
			node: ControlNode{DNSPublish: "auto", AddressV4: "192.168.1.10"},
			want: "none",
		},
		{
			name: "IPv6 in IPv4 field is rejected",
			node: ControlNode{DNSPublish: "auto", AddressV4: "2001:db8::10"},
			want: "none",
		},
		{
			name: "IPv4 in IPv6 field is rejected",
			node: ControlNode{DNSPublish: "auto", AddressV6: "198.51.100.10"},
			want: "none",
		},
		{
			name: "known dual stack listens on both",
			node: ControlNode{DNSPublish: "auto", AddressV4: "198.51.100.10", AddressV6: "2001:db8::10"},
			want: "auto",
		},
		{
			name: "explicit IPv4 mode is preserved before Agent reports an address",
			node: ControlNode{DNSPublish: "v4"},
			want: "v4",
		},
		{
			name: "explicit IPv6 mode is preserved before Agent reports an address",
			node: ControlNode{DNSPublish: "v6"},
			want: "v6",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := effectiveNodeListenFamily(test.node)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("effective listener family = %q, want %q", got, test.want)
			}
		})
	}
}

func TestNormalizeNodeInputAddressFamilies(t *testing.T) {
	input, err := normalizeNodeInput(NodeCreateInput{Name: "dual", Address: "203.0.113.20", AddressV6: "2001:db8::20", DNSPublish: "auto", Port: 443, BillingMode: "outbound", ResetDay: 1})
	if err != nil {
		t.Fatal(err)
	}
	if input.AddressV4 != "203.0.113.20" || input.AddressV6 != "2001:db8::20" {
		t.Fatalf("unexpected normalized addresses: %#v", input)
	}
	if _, err := normalizeNodeInput(NodeCreateInput{Name: "bad", AddressV4: "2001:db8::1", DNSPublish: "v4", Port: 443, BillingMode: "outbound", ResetDay: 1}); err == nil {
		t.Fatal("wrong-family address was accepted")
	}
	if _, err := normalizeNodeInput(NodeCreateInput{Name: "carrier-nat-v4", AddressV4: "100.64.0.1", DNSPublish: "v4", Port: 443, BillingMode: "outbound", ResetDay: 1}); err == nil {
		t.Fatal("carrier-grade NAT address was accepted as a public IPv4 value")
	}
	legacy, err := normalizeNodeInput(NodeCreateInput{Name: "carrier-nat-legacy", Address: "100.127.255.254", DNSPublish: "auto", Port: 443, BillingMode: "outbound", ResetDay: 1})
	if err != nil {
		t.Fatalf("legacy private node dial address should remain compatible: %v", err)
	}
	if legacy.AddressV4 != "" {
		t.Fatalf("carrier-grade NAT legacy address was promoted to a public DNS family: %q", legacy.AddressV4)
	}
	for _, mode := range []string{"v4", "v6"} {
		pending, err := normalizeNodeInput(NodeCreateInput{Name: "pending-" + mode, DNSPublish: mode, Port: 443, BillingMode: "outbound", ResetDay: 1})
		if err != nil {
			t.Fatalf("DNS mode %s should be saved while its Agent address is pending: %v", mode, err)
		}
		if pending.AddressV4 != "" || pending.AddressV6 != "" {
			t.Fatalf("pending DNS mode %s unexpectedly has an address: %#v", mode, pending)
		}
	}
}

func TestSelectedDNSFamilyCanBeSavedBeforeAgentAddressReport(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "pending-v6-node", DNSPublish: "v6", Port: 443}, now)
	if err != nil {
		t.Fatal(err)
	}
	if node.DNSPublish != "v6" || node.AddressV6 != "" {
		t.Fatalf("selected DNS family was not saved pending address telemetry: %#v", node)
	}
	if _, err := desiredNodeDNSAddresses(node); err == nil {
		t.Fatal("scheduler produced an AAAA record before the IPv6 address was reported")
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.RecordNodeReport(token, NodeReport{
		BootID: "pending-v6-boot", Sequence: 1, InterfaceName: "eth6", AgentVersion: "v1.11.12",
		NetAddresses: []NodeNetAddress{{Family: "v6", Address: "2001:db8::81", Interface: "eth6"}},
	}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	updated, err := app.db.controlNodeByID(node.ID, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if updated.AddressV6 != "2001:db8::81" || updated.AddressV4 != "" {
		t.Fatalf("Agent report did not fill the selected DNS family automatically: %#v", updated)
	}
	desired, err := desiredNodeDNSAddresses(updated)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired) != 1 || desired[0].recordType != "AAAA" {
		t.Fatalf("reported IPv6 address produced unexpected DNS records: %#v", desired)
	}
	if family, err := effectiveNodeListenFamily(updated); err != nil || family != "v6" {
		t.Fatalf("reported IPv6 address selected listener family %q, err=%v; want v6", family, err)
	}
}

func TestAddressRecordClassificationIsPerFamily(t *testing.T) {
	records := []cloudflareAddressRecord{{ID: "a", Type: "A", Name: "site.example", Comment: "owned"}, {ID: "aaaa", Type: "AAAA", Name: "site.example", Comment: "owned"}}
	for _, typ := range []string{"A", "AAAA"} {
		_, ok, unowned, ambiguous := classifyOwnedAddressRecords(addressRecordsOfType(records, typ), "site.example", func(v string) bool { return v == "owned" })
		if !ok || unowned != 0 || ambiguous {
			t.Fatalf("family %s was not classified independently", typ)
		}
	}
}

func TestReportedNodeAddressesExcludePrivateAndCarrierNATCandidates(t *testing.T) {
	got := normalizeReportedNodeNetAddresses([]NodeNetAddress{
		{Family: "v4", Address: "10.0.0.5", Interface: "eth0"},
		{Family: "v4", Address: "100.64.0.5", Interface: "eth0"},
		{Family: "v4", Address: "140.245.35.86", Interface: "eth0"},
		{Family: "v6", Address: "fd00::5", Interface: "eth0"},
		{Family: "v6", Address: "2606:4700:4700::1111", Interface: "eth0"},
		{Family: "v4", Address: "198.51.100.99", Interface: "controller"},
	})
	if len(got) != 2 || got[0].Address != "140.245.35.86" || got[1].Address != "2606:4700:4700::1111" {
		t.Fatalf("reported addresses = %#v; want only public IPv4 and IPv6 candidates", got)
	}
}

func TestAgentReportAdoptsPublicInterfaceAddressesAndServerObservationRefreshesThem(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "public-interface-refresh", DNSPublish: "auto"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for index, pair := range [][2]string{{"198.51.100.41", "2001:db8::41"}, {"198.51.100.42", "2001:db8::42"}} {
		report := NodeReport{
			BootID: "public-interface-report", Sequence: int64(index + 1), InterfaceName: "eth0",
			NetAddresses: []NodeNetAddress{
				{Family: "v4", Address: pair[0], Interface: "eth0"},
				{Family: "v6", Address: pair[1], Interface: "eth0"},
			},
		}
		var reported NodeReportResult
		if index == 0 {
			reported, err = app.db.RecordNodeReportResult(token, report, now.Add(time.Duration(index+2)*time.Second))
		} else {
			reported, err = app.db.RecordNodeReportResultFromSource(token, report, now.Add(time.Duration(index+2)*time.Second), pair[0])
		}
		if err != nil {
			t.Fatal(err)
		}
		wantV6 := pair[1]
		if index > 0 {
			wantV6 = "2001:db8::41"
		}
		if reported.Node.AddressV4 != pair[0] || reported.Node.AddressV6 != wantV6 || reported.Node.AddressSource != nodeAddressSourceReport || reported.Node.AddressV6Source != nodeAddressSourceReport {
			t.Fatalf("address hint/server observation result: v4=%q v6=%q sources=%q/%q", reported.Node.AddressV4, reported.Node.AddressV6, reported.Node.AddressSource, reported.Node.AddressV6Source)
		}
		if index == 1 {
			if _, err := app.db.RecordNodeObservedAddress(token, "v6", pair[1], now.Add(4*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	updated, err := app.db.controlNodeByID(node.ID, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if updated.AddressV4 != "198.51.100.42" || updated.AddressV6 != "2001:db8::42" || updated.Address != "198.51.100.42" {
		t.Fatalf("latest server-observed public families were not retained: address=%q v4=%q v6=%q", updated.Address, updated.AddressV4, updated.AddressV6)
	}
}

func TestPublicInterfaceAddressTakesPrecedenceOverDifferentObservedEgress(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "multi-egress-precedence", DNSPublish: "auto"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	first, err := app.db.RecordNodeReportResultFromSource(token, NodeReport{
		BootID: "multi-egress", Sequence: 1, InterfaceName: "eth0",
		NetAddresses: []NodeNetAddress{{Family: "v4", Address: "203.0.113.41", Interface: "eth0"}},
	}, now.Add(2*time.Second), "198.51.100.41")
	if err != nil {
		t.Fatal(err)
	}
	if first.Node.AddressV4 != "203.0.113.41" {
		t.Fatalf("initial public interface address=%q, want ingress 203.0.113.41", first.Node.AddressV4)
	}
	second, err := app.db.RecordNodeReportResultFromSource(token, NodeReport{
		BootID: "multi-egress", Sequence: 2, InterfaceName: "eth0",
		NetAddresses: []NodeNetAddress{{Family: "v4", Address: "203.0.113.41", Interface: "eth0"}},
	}, now.Add(3*time.Second), "198.51.100.42")
	if err != nil {
		t.Fatal(err)
	}
	if second.Node.AddressV4 != "203.0.113.41" {
		t.Fatalf("public interface address was replaced with a different egress source: %q", second.Node.AddressV4)
	}
	if second.Node.ID != node.ID {
		t.Fatalf("report updated node %d, want %d", second.Node.ID, node.ID)
	}
}

func TestControlNodePersistsFamiliesAndAdoptsEmptyAgentHints(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "families", AddressV4: "203.0.113.31", AddressV6: "2001:db8::31", DNSPublish: "v6", Port: 443}, now)
	if err != nil {
		t.Fatal(err)
	}
	if node.AddressV4 != "203.0.113.31" || node.AddressV6 != "2001:db8::31" || node.DNSPublish != "v6" {
		t.Fatalf("families not persisted: %#v", node)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.db.RecordNodeReport(token, NodeReport{BootID: "boot", Sequence: 1, InterfaceName: "eth0", NetAddresses: []NodeNetAddress{{Family: "v4", Address: "198.51.100.40", Interface: "eth0"}}}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	after, err := app.db.controlNodeByID(node.ID, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if after.AddressV4 != "203.0.113.31" {
		t.Fatalf("manual IPv4 was overwritten: %q", after.AddressV4)
	}
	if len(after.NetAddresses) != 1 || after.NetAddresses[0].Address != "198.51.100.40" {
		t.Fatalf("reported candidates missing: %#v", after.NetAddresses)
	}

	empty, enrollment2, err := app.db.CreateControlNode(NodeCreateInput{Name: "adopt", DNSPublish: "auto", Port: 443}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token2, err := app.db.EnrollControlNode(enrollment2, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	_, err = app.db.RecordNodeReport(token2, NodeReport{BootID: "boot2", Sequence: 1, InterfaceName: "eth0", NetAddresses: []NodeNetAddress{{Family: "v4", Address: "198.51.100.41"}, {Family: "v6", Address: "2001:db8::41"}}}, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	after, err = app.db.controlNodeByID(empty.ID, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if after.AddressV4 != "198.51.100.41" || after.AddressV6 != "2001:db8::41" {
		t.Fatalf("empty slots were not adopted: %#v", after)
	}
}
