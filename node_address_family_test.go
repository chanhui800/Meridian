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
	if _, err := normalizeNodeInput(NodeCreateInput{Name: "missing", DNSPublish: "v6", Port: 443, BillingMode: "outbound", ResetDay: 1}); err == nil {
		t.Fatal("missing pinned family was accepted")
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
