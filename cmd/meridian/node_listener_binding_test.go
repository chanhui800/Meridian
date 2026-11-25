package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNodePublishedFamiliesAndListenerFamilyFollowDNSAddresses(t *testing.T) {
	tests := []struct {
		name       string
		node       ControlNode
		wantV4     bool
		wantV6     bool
		wantListen string
	}{
		{
			name: "auto waits without reported addresses",
			node: ControlNode{DNSPublish: "auto"}, wantV4: true, wantV6: true, wantListen: "none",
		},
		{
			name: "auto ignores a private legacy NAT address",
			node: ControlNode{Address: "10.0.0.2", DNSPublish: "auto"}, wantV4: true, wantV6: true, wantListen: "none",
		},
		{
			name:       "auto follows only reported IPv4",
			node:       ControlNode{DNSPublish: "auto", AddressV4: "198.51.100.10"},
			wantV4:     true,
			wantV6:     true,
			wantListen: "v4",
		},
		{
			name:       "auto follows only reported IPv6",
			node:       ControlNode{DNSPublish: "auto", AddressV6: "2001:db8::10"},
			wantV4:     true,
			wantV6:     true,
			wantListen: "v6",
		},
		{
			name:       "auto dual stack requires both addresses to be known",
			node:       ControlNode{DNSPublish: "auto", AddressV4: "198.51.100.11", AddressV6: "2001:db8::11"},
			wantV4:     true,
			wantV6:     true,
			wantListen: "auto",
		},
		{
			name:       "explicit IPv4 mode is single family before address report",
			node:       ControlNode{DNSPublish: "v4"},
			wantV4:     true,
			wantV6:     false,
			wantListen: "v4",
		},
		{
			name:       "explicit IPv6 mode is single family before address report",
			node:       ControlNode{DNSPublish: "v6"},
			wantV4:     false,
			wantV6:     true,
			wantListen: "v6",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ipv4, ipv6, err := nodePublishedAddressFamilies(test.node)
			listenFamily, listenErr := effectiveNodeListenFamily(test.node)
			if err == nil {
				err = listenErr
			}
			if err != nil {
				t.Fatal(err)
			}
			if ipv4 != test.wantV4 || ipv6 != test.wantV6 || listenFamily != test.wantListen {
				t.Fatalf("published families/listener = %t/%t/%q, want %t/%t/%q", ipv4, ipv6, listenFamily, test.wantV4, test.wantV6, test.wantListen)
			}
		})
	}
}

func TestScheduledNodeHealthAddressFollowsPublishedFamily(t *testing.T) {
	for _, testCase := range []struct {
		name, family, want string
	}{
		{"v4 uses discovered public IPv4 over private legacy dial address", "v4", "198.51.100.21"},
		{"v6 uses discovered public IPv6 over private legacy dial address", "v6", "2001:db8::21"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			node := ControlNode{Address: "10.0.0.2", DNSPublish: testCase.family, AddressV4: "198.51.100.21", AddressV6: "2001:db8::21"}
			got, err := scheduledNodeHealthAddress(node)
			if err != nil || got != testCase.want {
				t.Fatalf("scheduled health address=%q err=%v, want %q", got, err, testCase.want)
			}
		})
	}
	if _, err := scheduledNodeHealthAddress(ControlNode{Address: "10.0.0.2", DNSPublish: "auto"}); err == nil {
		t.Fatal("private legacy address should not be used for health probing")
	}
	dual, err := scheduledNodeHealthAddresses(ControlNode{DNSPublish: "auto", AddressV4: "198.51.100.31", AddressV6: "2001:db8::31"})
	if err != nil || len(dual) != 2 || dual[0] != "198.51.100.31" || dual[1] != "2001:db8::31" {
		t.Fatalf("dual-stack health candidates=%v err=%v, want IPv4 then IPv6", dual, err)
	}
}

func TestNormalizeReportedNetworkInterfacesIsBoundedAndDeterministic(t *testing.T) {
	got := normalizeReportedNetworkInterfaces([]NodeNetworkInterface{
		{Name: "eth1", IPv6: true},
		{Name: "eth0", IPv4: true},
		{Name: "eth1", IPv4: true},
		{Name: "metadata", IPv4: true},
		{Name: "bad/name", IPv6: true},
		{Name: "no-address"},
	})
	want := []NodeNetworkInterface{{Name: "eth0", IPv4: true}, {Name: "eth1", IPv4: true, IPv6: true}}
	if len(got) != len(want) {
		t.Fatalf("interfaces = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("interface[%d] = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestNodeDNSPublicationUpdateInvalidatesAgentAndScheduleConfig(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "listener-node", AddressV4: "198.51.100.91", AddressV6: "2001:db8::91", DNSPublish: "auto", Port: 443,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, agentToken, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	interfaces := []NodeNetworkInterface{{Name: "eth6", IPv6: true}, {Name: "eth0", IPv4: true}}
	if _, err := app.db.RecordNodeReport(agentToken, NodeReport{
		BootID: "listener-boot", Sequence: 1, InterfaceName: "eth0", AgentVersion: "v1.11.12", NetworkInterfaces: interfaces,
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "listener-site", PublicHost: "listener.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	const previousHash = "listener-config-before"
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET desired_config_hash=?,applied_config_hash=?,config_dirty=0 WHERE id=?`, previousHash, previousHash, node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=?,config_hash=?,config_pending_since_ms=0 WHERE site_id=?`, node.ID, node.ID, previousHash, site.ID); err != nil {
		t.Fatal(err)
	}
	before, err := app.db.controlNodeByID(node.ID, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	updated, err := app.db.UpdateControlNode(node.ID, NodeCreateInput{
		Name: node.Name, Address: node.Address, AddressV4: node.AddressV4, AddressV6: node.AddressV6,
		DNSPublish: "v6", Port: node.Port, Priority: node.Priority,
		TrafficQuota: node.TrafficQuota, BillingMode: node.BillingMode, ResetDay: node.ResetDay,
	}, true, now.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if updated.DNSPublish != "v6" || updated.DesiredConfigHash != "" || !updated.ConfigDirty || updated.ConfigRevision <= before.ConfigRevision {
		t.Fatalf("DNS publication update was not persisted/applied as new config: before=%#v after=%#v", before, updated)
	}
	schedule, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.ConfigHash != "" || schedule.ConfigPendingSinceMS != now.Add(3*time.Second).UnixMilli() {
		t.Fatalf("listener change did not suspend DNS cutover: %#v", schedule)
	}
}

func TestBuildAgentConfigUsesListenerFamilyAndPreservesOldHashContract(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "listener-hash-node", AddressV4: "198.51.100.92", AddressV6: "2001:db8::92", DNSPublish: "auto", Port: 443,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.RecordNodeReport(token, NodeReport{
		BootID: "listener-hash-boot", Sequence: 1, InterfaceName: "eth6", AgentVersion: "v1.11.12",
		NetworkInterfaces: []NodeNetworkInterface{{Name: "eth6", IPv6: true}},
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	updated, err := app.db.UpdateControlNode(node.ID, NodeCreateInput{
		Name: node.Name, Address: node.Address, AddressV4: node.AddressV4, AddressV6: node.AddressV6, DNSPublish: "v6",
		Port: node.Port, Priority: node.Priority, TrafficQuota: node.TrafficQuota, BillingMode: node.BillingMode, ResetDay: node.ResetDay,
	}, true, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !updated.ConfigDirty {
		t.Fatalf("DNS publication setting was not persisted as a pending config: %#v", updated)
	}
	config, err := app.buildAgentConfigForRequest(context.Background(), token, now.Add(3*time.Second), "linux/amd64", "v1.11.12")
	if err != nil {
		t.Fatal(err)
	}
	if config.ListenFamily != "v6" || config.ListenInterface != "" {
		t.Fatalf("Agent listener config = family %q interface %q, want v6 with retired interface unset", config.ListenFamily, config.ListenInterface)
	}
	newHash, err := agentConfigHashForVersion(config, "v1.11.12-pre.20261124.1")
	if err != nil {
		t.Fatal(err)
	}
	if newHash != config.ConfigHash {
		t.Fatalf("new Agent config hash mismatch: built=%q recalculated=%q", config.ConfigHash, newHash)
	}
	oldHash, err := agentConfigHashForVersion(config, "v1.11.11")
	if err != nil {
		t.Fatal(err)
	}
	withoutListener := config
	withoutListener.ListenFamily = ""
	withoutListener.ListenInterface = ""
	wantOldHash, err := agentConfigHashForVersion(withoutListener, "v1.11.11")
	if err != nil {
		t.Fatal(err)
	}
	if oldHash != wantOldHash || oldHash == newHash {
		t.Fatalf("listener hash compatibility mismatch: old=%q want-old=%q new=%q", oldHash, wantOldHash, newHash)
	}
}

func TestAgentRuntimeHashIgnoresRetiredInterfaceAndZerosLegacyListenerFields(t *testing.T) {
	config := AgentRuntimeConfig{
		SchemaVersion: agentConfigSchemaVersion, NodeGUID: "legacy-listener-hash", ListenFamily: "v6",
		ListenInterface: "eth9", Routes: []AgentSiteRoute{},
	}
	modernHash, err := agentConfigHash(config)
	if err != nil {
		t.Fatal(err)
	}
	withoutInterface := config
	withoutInterface.ListenInterface = ""
	withoutInterfaceHash, err := agentConfigHash(withoutInterface)
	if err != nil {
		t.Fatal(err)
	}
	if modernHash != withoutInterfaceHash {
		t.Fatal("retired listen_interface still affects the current Agent runtime hash")
	}
	modernPayload, err := json.Marshal(agentConfigHashPayloadForVersion(config, false, true))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(modernPayload), `"listen_interface"`) {
		t.Fatalf("current Agent hash payload still includes retired interface binding: %s", modernPayload)
	}

	legacyPayload := agentConfigHashPayloadForVersion(config, true, true)
	if legacyPayload.ListenFamily != "" {
		t.Fatalf("legacy listener family = %q, want empty", legacyPayload.ListenFamily)
	}
	legacyHash, err := hashAgentConfigPayload(config, true)
	if err != nil {
		t.Fatal(err)
	}
	legacyExpected := config
	legacyExpected.ListenFamily = ""
	legacyExpected.ListenInterface = ""
	expectedHash, err := hashAgentConfigPayload(legacyExpected, true)
	if err != nil {
		t.Fatal(err)
	}
	if legacyHash != expectedHash {
		t.Fatalf("legacy hash=%q, want listener fields cleared hash=%q", legacyHash, expectedHash)
	}
}

func TestAutoDNSWithNoReportedAddressWithholdsRoutesForAllAgentVersions(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 11, 24, 13, 30, 0, 0, time.UTC)
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "pending-auto-node", DNSPublish: "auto", Port: 19443,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "pending-auto-site", PublicHost: "pending-auto.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"v1.11.12-pre.20261124.2", "v1.11.11"} {
		t.Run(version, func(t *testing.T) {
			config, err := app.buildAgentConfigForRequest(context.Background(), token, now.Add(time.Second), "linux/amd64", version)
			if err != nil {
				t.Fatal(err)
			}
			if len(config.Routes) != 0 {
				t.Fatalf("automatic DNS without an address must not start an unknown-family listener; routes=%#v", config.Routes)
			}
			if version == "v1.11.11" {
				if config.ListenFamily != "" {
					t.Fatalf("legacy Agent received a new listener field: %q", config.ListenFamily)
				}
			} else if config.ListenFamily != "none" {
				t.Fatalf("no-address listener family = %q, want none", config.ListenFamily)
			}
		})
	}
}

func TestOlderAgentReportPreservesReportedNetworkInterfaces(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 11, 24, 14, 0, 0, 0, time.UTC)
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "inventory-node"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	const appliedHash = "inventory-listener-before"
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET config_dirty=0,config_revision=5,desired_config_revision=5,applied_config_revision=5,
		desired_config_hash=?,applied_config_hash=? WHERE id=?`, appliedHash, appliedHash, node.ID); err != nil {
		t.Fatal(err)
	}
	interfaces := []NodeNetworkInterface{{Name: "eth0", IPv4: true, IPv6: true}, {Name: "eth6", IPv6: true}}
	if _, err := app.db.RecordNodeReport(token, NodeReport{
		BootID: "inventory", Sequence: 1, InterfaceName: "eth6", AgentVersion: "v1.11.12",
		NetAddresses: []NodeNetAddress{{Family: "v6", Address: "2001:db8::95", Interface: "eth6"}}, NetworkInterfaces: interfaces,
		AppliedConfigHash: appliedHash, AppliedConfigRevision: 5,
	}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.RecordNodeReport(token, NodeReport{BootID: "inventory", Sequence: 2, InterfaceName: "eth0", AgentVersion: "v1.11.11"}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := app.db.controlNodeByID(node.ID, now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.NetworkInterfaces) != 2 || got.NetworkInterfaces[0] != interfaces[0] || got.NetworkInterfaces[1] != interfaces[1] {
		t.Fatalf("older Agent erased or reordered network inventory: %#v", got.NetworkInterfaces)
	}
	if got.AddressV6 != "2001:db8::95" || !got.ConfigDirty || got.DesiredConfigHash != "" || got.ConfigRevision != 6 {
		t.Fatalf("IPv6-only address adoption did not select a v6 listener config: %#v", got)
	}
}

func TestAgentListenerOnlyBindsIPv6ForIPv6Publishing(t *testing.T) {
	node := ControlNode{DNSPublish: "v6", AddressV4: "198.51.100.93", AddressV6: "2001:db8::93"}
	desired, err := desiredNodeDNSAddresses(node)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired) != 1 || desired[0].recordType != "AAAA" {
		t.Fatalf("IPv6-only node DNS addresses = %#v", desired)
	}
	family, err := effectiveNodeListenFamily(node)
	if err != nil {
		t.Fatal(err)
	}
	if family != "v6" {
		t.Fatalf("IPv6-only DNS selected listener family %q, want v6-only", family)
	}
	autoNode := ControlNode{DNSPublish: "auto", AddressV6: "2001:db8::94"}
	family, err = effectiveNodeListenFamily(autoNode)
	if err != nil {
		t.Fatal(err)
	}
	if family != "v6" {
		t.Fatalf("automatic DNS with only an IPv6 address selected listener family %q, want v6-only", family)
	}
}
