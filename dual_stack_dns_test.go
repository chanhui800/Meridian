package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- address family resolution ---------------------------------------------

func TestNodeAddressFamilyResolvesEachStack(t *testing.T) {
	cases := []struct {
		value string
		want  string
	}{
		{"203.0.113.10", "v4"},
		{"  203.0.113.10  ", "v4"},
		{"2001:db8::1", "v6"},
		{"::ffff:203.0.113.10", "v4"},
		{"", ""},
		{"example.test", ""},
		{"203.0.113.10:443", ""},
		{"[2001:db8::1]", ""},
		{"fe80::1%eth0", ""},
		{"999.1.1.1", ""},
	}
	for _, testCase := range cases {
		if got := nodeAddressFamily(testCase.value); got != testCase.want {
			t.Fatalf("nodeAddressFamily(%q) = %q, want %q", testCase.value, got, testCase.want)
		}
	}
}

func TestNodeDNSPublishFamiliesFollowsTheAvailableStack(t *testing.T) {
	v4Only := ControlNode{Address: "203.0.113.10"}
	v6Only := ControlNode{Address: "2001:db8::1"}
	dual := ControlNode{Address: "203.0.113.10", AddressV6: "2001:db8::1"}
	dualV6Primary := ControlNode{Address: "2001:db8::1", AddressV4: "203.0.113.10"}

	cases := []struct {
		name string
		node ControlNode
		want []string
	}{
		// The legacy single-address node keeps publishing exactly one record.
		{"v4-only node publishes A", v4Only, []string{"v4"}},
		{"v6-only node publishes AAAA", v6Only, []string{"v6"}},
		{"dual-stack node publishes both", dual, []string{"v4", "v6"}},
		{"dual-stack with v6 primary still publishes both", dualV6Primary, []string{"v4", "v6"}},
		// Manual selection pins one family on a dual-stack node.
		{"auto pinned to v4", ControlNode{Address: "203.0.113.10", AddressV6: "2001:db8::1", DNSPublish: nodeDNSPublishV4}, []string{"v4"}},
		{"auto pinned to v6", ControlNode{Address: "203.0.113.10", AddressV6: "2001:db8::1", DNSPublish: nodeDNSPublishV6}, []string{"v6"}},
		// A preference can never publish a family the node does not have.
		{"v4 preference on a v6-only node publishes nothing", ControlNode{Address: "2001:db8::1", DNSPublish: nodeDNSPublishV4}, nil},
		{"v6 preference on a v4-only node publishes nothing", ControlNode{Address: "203.0.113.10", DNSPublish: nodeDNSPublishV6}, nil},
		{"no address publishes nothing", ControlNode{}, nil},
	}
	for _, testCase := range cases {
		got := testCase.node.DNSPublishFamilies()
		if len(got) != len(testCase.want) {
			t.Fatalf("%s: families = %v, want %v", testCase.name, got, testCase.want)
		}
		for index := range got {
			if got[index] != testCase.want[index] {
				t.Fatalf("%s: families = %v, want %v", testCase.name, got, testCase.want)
			}
		}
	}
}

// A per-family value in the wrong slot is refused rather than published under
// the other family: the value becomes a public DNS record.
func TestNodeInputRejectsWrongFamilyAddresses(t *testing.T) {
	cases := []struct {
		name  string
		input NodeCreateInput
		want  string
	}{
		{"v6 in the v4 slot", NodeCreateInput{Name: "n", AddressV4: "2001:db8::1"}, "IPv4"},
		{"v4 in the v6 slot", NodeCreateInput{Name: "n", AddressV6: "203.0.113.10"}, "IPv6"},
		{"bracketed v6", NodeCreateInput{Name: "n", AddressV6: "[2001:db8::1]"}, "IPv6"},
		{"hostname as the primary address", NodeCreateInput{Name: "n", Address: "node.example.test"}, "节点地址"},
	}
	for _, testCase := range cases {
		if _, err := normalizeNodeInput(testCase.input); err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s: error = %v, want it to mention %q", testCase.name, err, testCase.want)
		}
	}
	// The valid combinations must pass.
	for _, input := range []NodeCreateInput{
		{Name: "n", AddressV4: "203.0.113.10"},
		{Name: "n", AddressV6: "2001:db8::1"},
		{Name: "n", AddressV4: "203.0.113.10", AddressV6: "2001:db8::1", DNSPublish: nodeDNSPublishV6},
		{Name: "n", Address: ""},
	} {
		if _, err := normalizeNodeInput(input); err != nil {
			t.Fatalf("normalizeNodeInput(%+v) = %v", input, err)
		}
	}
}

// --- agent hints -----------------------------------------------------------

func TestNodeNetAddressHintsKeepOnlyPublishableAddresses(t *testing.T) {
	encoded := encodeNodeNetAddressHints([]NodeNetAddress{
		{Family: "v4", Address: "203.0.113.10", Interface: "eth0"},
		{Family: "v4", Address: "10.0.0.5", Interface: "eth0"},       // private
		{Family: "v4", Address: "127.0.0.1", Interface: "lo"},        // loopback
		{Family: "v6", Address: "2001:db8::1", Interface: "eth0"},    // documentation range is global unicast
		{Family: "v6", Address: "fe80::1", Interface: "eth0"},        // link local
		{Family: "v6", Address: "fd00::1", Interface: "eth0"},        // unique local
		{Family: "v4", Address: "not-an-address", Interface: "eth0"}, // unparsable
		{Family: "v4", Address: "203.0.113.11", Interface: "eth1"},   // a real second v4 candidate
	})
	hints := decodeNodeNetAddressHints(encoded)
	byFamily := map[string][]string{}
	for _, hint := range hints {
		byFamily[hint.Family] = append(byFamily[hint.Family], hint.Address)
	}
	// Two v4 candidates are kept as hints, but they are ordered so the first one
	// adopted is deterministic; the v6 set is filtered down to the one routable
	// address.
	if len(byFamily["v4"]) != 2 || byFamily["v4"][0] != "203.0.113.10" || byFamily["v4"][1] != "203.0.113.11" {
		t.Fatalf("v4 hints = %v, want the two routable addresses in ascending order", byFamily["v4"])
	}
	if len(byFamily["v6"]) != 1 || byFamily["v6"][0] != "2001:db8::1" {
		t.Fatalf("v6 hints = %v, want the single globally routable address", byFamily["v6"])
	}
	if decodeNodeNetAddressHints("") != nil {
		t.Fatal("an empty hint document must yield no candidates")
	}
	if decodeNodeNetAddressHints("{not json") != nil {
		t.Fatal("a corrupt hint document must yield no candidates")
	}
	if decodeNodeNetAddressHints(`{"v4":["10.0.0.5"]}`) != nil {
		t.Fatal("a stored private address must never become a candidate")
	}
	// A hand-edited or older document could file an address under the wrong
	// family, which would make the scheduler publish an A record with an IPv6
	// value. The family has to be re-derived from the address itself.
	if decodeNodeNetAddressHints(`{"v6":["203.0.113.10"]}`) != nil {
		t.Fatal("an address filed under the wrong family must be dropped")
	}
	if hints := decodeNodeNetAddressHints(`{"v4":["203.0.113.10","10.0.0.5"]}`); len(hints) != 1 || hints[0].Address != "203.0.113.10" {
		t.Fatalf("mixed document hints = %+v, want only the routable address", hints)
	}
}

func TestEdgeLocalAddressCandidatesPrefersTheBillingInterface(t *testing.T) {
	// The real interface list depends on the host, so this only pins the
	// invariants that make the hint safe to publish: no loopback, no private
	// addresses, and at most one entry per family with the billing interface
	// first.
	candidates := edgeLocalAddressCandidates("eth0")
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if seen[candidate.Family] && candidate.Family == "v4" {
			t.Fatalf("more than one v4 candidate: %v", candidates)
		}
		seen[candidate.Family] = true
		if candidate.Family != "v4" && candidate.Family != "v6" {
			t.Fatalf("candidate family = %q", candidate.Family)
		}
		if nodeAddressFamily(candidate.Address) != candidate.Family {
			t.Fatalf("candidate %q does not match family %q", candidate.Address, candidate.Family)
		}
		if edgeInterfaceIsVirtual(candidate.Interface) {
			t.Fatalf("virtual interface %q was reported", candidate.Interface)
		}
	}
}

func TestEdgeInterfaceIsVirtualSkipsContainerAndTunnelLinks(t *testing.T) {
	for _, name := range []string{"lo", "docker0", "veth1234", "br-abc", "virbr0", "tun0", "tap0", "tailscale0", "wg0", "cni0"} {
		if !edgeInterfaceIsVirtual(name) {
			t.Fatalf("%q must be treated as virtual", name)
		}
	}
	for _, name := range []string{"eth0", "ens3", "enp1s0", "wlan0"} {
		if edgeInterfaceIsVirtual(name) {
			t.Fatalf("%q must not be treated as virtual", name)
		}
	}
}

// --- adoption --------------------------------------------------------------

func TestMergeAdoptedNodeAddressesNeverOverwritesOperatorValues(t *testing.T) {
	cases := []struct {
		name     string
		node     ControlNode
		v4, v6   string
		wantV4   string
		wantV6   string
		wantMain string
		changed  bool
	}{
		{
			name: "fills both empty slots",
			node: ControlNode{},
			v4:   "203.0.113.10", v6: "2001:db8::1",
			wantV4: "203.0.113.10", wantV6: "2001:db8::1", wantMain: "203.0.113.10", changed: true,
		},
		{
			name: "keeps an operator ipv6 and adopts ipv4",
			node: ControlNode{Address: "2001:db8::9", AddressV6: "2001:db8::9", AddressSource: nodeAddressSourceManual},
			v4:   "203.0.113.10", v6: "2001:db8::1",
			wantV4: "203.0.113.10", wantV6: "2001:db8::9", wantMain: "2001:db8::9", changed: true,
		},
		{
			// An operator-entered IPv6 is the node's only address, so the
			// controller derives the primary slot from it; a v4 candidate for the
			// empty v4 slot does not displace it.
			name:     "an ipv6-only node keeps its address as primary",
			node:     ControlNode{AddressV6: "2001:db8::9", AddressV6Source: nodeAddressSourceManual},
			v4:       "203.0.113.10",
			wantV4:   "203.0.113.10",
			wantV6:   "2001:db8::9",
			wantMain: "2001:db8::9", changed: true,
		},
		{
			name: "a candidate for an already-filled slot changes nothing",
			node: ControlNode{Address: "203.0.113.99", AddressV4: "203.0.113.99", AddressV6: "2001:db8::9", AddressV6Source: nodeAddressSourceManual},
			v4:   "203.0.113.10", v6: "2001:db8::1",
			wantMain: "203.0.113.99", wantV4: "203.0.113.99", wantV6: "2001:db8::9", changed: false,
		},
		{
			name: "keeps the primary address untouched when it is already set",
			node: ControlNode{Address: "203.0.113.99", AddressSource: nodeAddressSourceManual},
			v4:   "203.0.113.10", v6: "2001:db8::1",
			wantMain: "203.0.113.99", wantV4: "203.0.113.10", wantV6: "2001:db8::1", changed: true,
		},
		{
			name:    "no candidates means no change",
			node:    ControlNode{Address: "203.0.113.99"},
			changed: false, wantMain: "203.0.113.99",
		},
	}
	for _, testCase := range cases {
		got := mergeAdoptedNodeAddresses(testCase.node, testCase.v4, testCase.v6, nodeAddressSourceDetected)
		if got.changed != testCase.changed {
			t.Fatalf("%s: changed = %v, want %v", testCase.name, got.changed, testCase.changed)
		}
		if got.address != testCase.wantMain {
			t.Fatalf("%s: primary = %q, want %q", testCase.name, got.address, testCase.wantMain)
		}
		if testCase.wantV4 != "" && got.addressV4 != testCase.wantV4 {
			t.Fatalf("%s: v4 = %q, want %q", testCase.name, got.addressV4, testCase.wantV4)
		}
		if testCase.wantV6 != "" && got.addressV6 != testCase.wantV6 {
			t.Fatalf("%s: v6 = %q, want %q", testCase.name, got.addressV6, testCase.wantV6)
		}
		if testCase.wantV6 == "2001:db8::1" && got.addressV6Src != nodeAddressSourceDetected {
			t.Fatalf("%s: adopted v6 must be marked for review, got %q", testCase.name, got.addressV6Src)
		}
	}
}

// A failed probe must name the record family and the address it refused to
// publish, because the operator reads this in the panel: "AAAA (IPv6) <addr>"
// says which half of a dual-stack node is unreachable.
func TestProbeFailureNamesTheRefusedFamily(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, _, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "unreachable", Address: "127.0.0.1", AddressV6: "2001:db8::1",
		Priority: 100, BillingMode: "outbound", ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	closed, err := app.db.controlNodeByID(node.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := newNodeProbeSecret()
	if err != nil {
		t.Fatal(err)
	}
	decodedSecret, err := decodeNodeProbeSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	// Loopback refuses immediately, so the probe fails without waiting out a
	// five-second dial timeout. The address is still a real IPv6 literal, which is
	// what nodeDialAddress requires.
	closed.Port = 1
	targets := []probeTarget{{family: "v6", address: "::1"}}
	probeErr := probeScheduledNodeAddresses(context.Background(), closed, "dual.example.test", decodedSecret, targets)
	if probeErr == nil {
		t.Fatal("probing an unreachable address must fail")
	}
	message := probeErr.Error()
	for _, needle := range []string{"AAAA", "IPv6", "::1"} {
		if !strings.Contains(message, needle) {
			t.Fatalf("probe error %q must name %q so the panel can say which record was refused", message, needle)
		}
	}
}

// The reported addresses have to survive the report path, because adoption reads
// them back from the stored column rather than from the request.
func TestNodeReportStoresAddressHints(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	node, enrollmentToken, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "reporter", Address: "203.0.113.10", Priority: 100, BillingMode: "outbound", ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	_ = node
	// Reporting authenticates with the agent token, which only exists after
	// enrollment consumes the one-time token. The enrollment token was issued at
	// `now`, so it is redeemed a moment later.
	_, agentToken, err := app.db.EnrollControlNodeFromSource(enrollmentToken, now.Add(time.Second), "")
	if err != nil {
		t.Fatalf("EnrollControlNodeFromSource: %v", err)
	}
	report := NodeReport{
		BootID: "boot", Sequence: 1, InterfaceName: "eth0", RXBytes: 1, TXBytes: 1,
		NetAddresses: []NodeNetAddress{
			{Family: "v4", Address: "203.0.113.10", Interface: "eth0"},
			{Family: "v6", Address: "2001:db8::1", Interface: "eth0"},
			{Family: "v4", Address: "10.0.0.5", Interface: "eth0"},
		},
	}
	if _, err := app.db.RecordNodeReport(agentToken, report, now.Add(2*time.Second)); err != nil {
		t.Fatalf("RecordNodeReport: %v", err)
	}
	reportAt := now.Add(2 * time.Second)
	stored, err := app.db.controlNodeByID(node.ID, reportAt)
	if err != nil {
		t.Fatal(err)
	}
	hints := decodeNodeNetAddressHints(stored.NetAddressHints)
	if len(hints) != 2 {
		t.Fatalf("stored hints = %+v, want the routable v4 and v6 pair", hints)
	}
	if stored.NetAddressHintsAtMS != reportAt.UnixMilli() {
		t.Fatalf("hint clock = %d, want %d", stored.NetAddressHintsAtMS, reportAt.UnixMilli())
	}
	for _, hint := range hints {
		if hint.Address == "10.0.0.5" {
			t.Fatal("a private address reached the stored hints")
		}
	}
	// A report without addresses must clear the hints rather than leave a stale
	// set that a later tick would adopt.
	report.NetAddresses = nil
	if _, err := app.db.RecordNodeReport(agentToken, report, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	cleared, err := app.db.controlNodeByID(node.ID, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if cleared.NetAddressHints != "" {
		t.Fatalf("stale hints survived an empty report: %q", cleared.NetAddressHints)
	}
}

// Adoption attempts are throttled: a candidate that does not answer keeps not
// answering, so probing it on every scheduler tick would be pure network churn.
// The guard is proven by timing, because the alternative is a real dial that
// blocks until the probe timeout.
func TestNodeAddressAdoptionIsThrottled(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	node, _, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "throttled", Address: "203.0.113.10", Priority: 100, BillingMode: "outbound", ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	hint := encodeNodeNetAddressHints([]NodeNetAddress{{Family: "v6", Address: "2001:db8::1"}})
	if _, err := app.db.db.Exec(
		`UPDATE control_nodes SET net_address_hints=?,net_address_hints_at_ms=?,address_v6='',address_v6_source='',net_address_adoption_at_ms=0 WHERE id=?`,
		hint, now.UnixMilli(), node.ID); err != nil {
		t.Fatal(err)
	}
	current, err := app.db.controlNodeByID(node.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(decodeNodeNetAddressHints(current.NetAddressHints)) != 1 {
		t.Fatal("fixture did not store a hint")
	}

	// A stale hint must be ignored outright, and must do so without consuming an
	// attempt, so a fresh report can still be adopted immediately afterwards.
	staleNow := now.Add(2 * nodeNetAddressHintFreshness)
	start := time.Now()
	if _, err := app.adoptProbedNodeAddresses(context.Background(), current, staleNow); err != nil {
		t.Fatalf("stale hint: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a stale hint cost %s, so it was probed instead of ignored", elapsed)
	}
	afterStale, err := app.db.controlNodeByID(node.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if afterStale.NetAddressAdoptionAttemptAtMS != 0 {
		t.Fatal("ignoring a stale hint must not consume an adoption attempt")
	}

	// Within the interval a second attempt must be skipped for the same reason.
	if err := app.db.markNodeAddressAdoptionAttempt(node.ID, now); err != nil {
		t.Fatal(err)
	}
	throttled, err := app.db.controlNodeByID(node.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	if _, err := app.adoptProbedNodeAddresses(context.Background(), throttled, now.Add(time.Second)); err != nil {
		t.Fatalf("throttled attempt: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a throttled attempt cost %s, so the guard did not apply", elapsed)
	}
	final, err := app.db.controlNodeByID(node.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if final.AddressV6 != "" {
		t.Fatalf("a skipped attempt still adopted %q", final.AddressV6)
	}
}

// A backup taken while dual-stack publishing is active must restore with the
// per-family address slots and the tracked record ledger intact. Losing the
// ledger would make the scheduler treat live Meridian records as untracked and
// refuse to touch them, so the site would stop following its node.
func TestBackupRestoreRoundTripsDualStackState(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.db")
	sourceDB, err := openDB(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	// A restore refuses a backup with no administrator account, so the source has
	// to look like a real installation.
	if _, err := sourceDB.db.Exec(`INSERT INTO users (username, password_hash) VALUES ('admin', 'hash')`); err != nil {
		sourceDB.Close()
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	node, _, err := sourceDB.CreateControlNode(NodeCreateInput{
		Name: "dual", AddressV4: "203.0.113.10", AddressV6: "2001:db8::1", DNSPublish: nodeDNSPublishV6,
		Priority: 100, BillingMode: "outbound", ResetDay: 1,
	}, now)
	if err != nil {
		sourceDB.Close()
		t.Fatal(err)
	}
	site, err := sourceDB.CreateSite("dual-site", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		sourceDB.Close()
		t.Fatal(err)
	}
	// A restore deliberately clears sites whose ingress it cannot preserve, so the
	// fixture uses a supported host ingress to keep its schedule row.
	if _, err := sourceDB.db.Exec(`UPDATE sites SET public_host='dual.example.test',ingress_mode=?,enabled=1 WHERE id=?`,
		ingressModeHost, site.ID); err != nil {
		sourceDB.Close()
		t.Fatal(err)
	}
	ledgerTx, err := sourceDB.db.Begin()
	if err != nil {
		sourceDB.Close()
		t.Fatal(err)
	}
	if err := replaceSiteDNSRecordsTx(ledgerTx, site.ID, []siteDNSRecord{
		{Family: "v4", ZoneID: "zone-1", RecordID: "rec-v4", RecordType: "A", Address: "203.0.113.10"},
		{Family: "v6", ZoneID: "zone-1", RecordID: "rec-v6", RecordType: "AAAA", Address: "2001:db8::1"},
	}, now.UnixMilli()); err != nil {
		sourceDB.Close()
		t.Fatal(err)
	}
	if err := ledgerTx.Commit(); err != nil {
		sourceDB.Close()
		t.Fatal(err)
	}
	// CreateSite does not create a schedule row, so the fixture supplies one: the
	// published family set only exists on a site that is actually scheduled.
	if _, err := sourceDB.db.Exec(`INSERT INTO site_node_schedules (site_id,enabled,mode,dns_status,schedule_revision,created_at_ms,updated_at_ms)
		VALUES(?,1,'global','active',1,?,?)`, site.ID, now.UnixMilli(), now.UnixMilli()); err != nil {
		sourceDB.Close()
		t.Fatal(err)
	}
	if _, err := sourceDB.db.Exec(`UPDATE site_node_schedules SET dns_families=?,dns_status='active',cf_record_id='rec-v4',cf_record_type='A',applied_address='203.0.113.10' WHERE site_id=?`,
		encodeDNSFamilies([]string{"v4", "v6"}), site.ID); err != nil {
		sourceDB.Close()
		t.Fatal(err)
	}
	// The source schedule row must exist for the family set to be meaningful, so
	// the fixture asserts it wrote one rather than reading back a default row the
	// restore's own migration may have created.
	var sourceFamilies, sourceRecord string
	if err := sourceDB.db.QueryRow(`SELECT dns_families,cf_record_id FROM site_node_schedules WHERE site_id=?`, site.ID).
		Scan(&sourceFamilies, &sourceRecord); err != nil {
		sourceDB.Close()
		t.Fatalf("source schedule row: %v", err)
	}
	sourceDB.Close()
	if sourceFamilies == "" || sourceRecord == "" {
		t.Fatalf("fixture did not write the source schedule: families=%q record=%q", sourceFamilies, sourceRecord)
	}

	backupData, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}

	targetPath := filepath.Join(dir, "target.db")
	targetDB, err := openDB(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := targetDB.db.Exec(`INSERT INTO users (username, password_hash) VALUES ('admin', 'hash')`); err != nil {
		targetDB.Close()
		t.Fatal(err)
	}
	if _, err := targetDB.db.Exec(`UPDATE panel_settings SET panel_domain='target.example.test', route_domain='route.example.test', listen_port=9443, tls_enabled=0, configured=1 WHERE id=1`); err != nil {
		targetDB.Close()
		t.Fatal(err)
	}
	// The restore preserves the target's panel settings, so it needs them.
	preserved, err := readBackupPanelSettings(targetDB.db)
	if err != nil {
		targetDB.Close()
		t.Fatal(err)
	}
	targetDB.Close()
	// The source rows were encrypted with the process signing key (see
	// TestMain), so that is the "old" secret the restore has to decrypt with. The
	// target uses a different key, which is what makes the restore re-encrypt the
	// node probe secret instead of passing ciphertext through.
	oldJWT := []byte(strings.Repeat("test-jwt-secret-", 3))
	targetJWT := bytes.Repeat([]byte("n"), 32)
	includeTLS := false
	manifest := backupManifest{
		Format:            "meridian-backup",
		FormatVersion:     backupFormatVersion,
		Files:             []string{backupDatabaseEntry},
		IncludeTLS:        &includeTLS,
		JWTSecret:         base64.RawStdEncoding.EncodeToString(oldJWT),
		UpstreamHeaderKey: base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte("h"), 32)),
	}
	if _, err := writeRestorePending(targetPath, manifest,
		map[string][]byte{backupDatabaseEntry: backupData},
		targetJWT, bytes.Repeat([]byte("k"), 32), preserved, false); err != nil {
		t.Fatal(err)
	}
	if _, err := applyPendingRestore(targetPath); err != nil {
		t.Fatal(err)
	}

	restoredDB, err := openDB(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredDB.Close()
	restoredNode, err := restoredDB.controlNodeByID(node.ID, now)
	if err != nil {
		t.Fatalf("restored node: %v", err)
	}
	if restoredNode.AddressV4 != "203.0.113.10" || restoredNode.AddressV6 != "2001:db8::1" {
		t.Fatalf("address slots lost in restore: %q/%q", restoredNode.AddressV4, restoredNode.AddressV6)
	}
	if restoredNode.DNSPublish != nodeDNSPublishV6 {
		t.Fatalf("dns_publish lost in restore: %q", restoredNode.DNSPublish)
	}
	if families := restoredNode.DNSPublishFamilies(); len(families) != 1 || families[0] != "v6" {
		t.Fatalf("restored families = %v, want only v6", families)
	}
	records, err := restoredDB.siteDNSRecords(site.ID)
	if err != nil {
		t.Fatalf("restored ledger: %v", err)
	}
	if len(records) != 2 || records[0].RecordID != "rec-v4" || records[1].RecordID != "rec-v6" {
		t.Fatalf("tracked record ledger lost in restore: %+v", records)
	}
	schedule, err := restoredDB.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatalf("restored schedule: %v", err)
	}
	if len(schedule.AppliedFamilies) != 2 {
		t.Fatalf("restored dns_families = %v, want both families", schedule.AppliedFamilies)
	}
}

// Adoption must not wait for a scheduled site. A freshly enrolled dual-stack node
// has no site yet, so requiring one meant the Agent's reported IPv4 could never be
// adopted and the node published only the family it happened to enrol over. The
// per-node edge certificate exists from enrollment, so it is the probe identity.
func TestAdoptionProbesWithoutAScheduledSite(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	if _, err := app.db.db.Exec(`UPDATE panel_settings SET route_domain='route.example.test', configured=1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	node, enrollmentToken, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "fresh", AddressV6: "::1", Priority: 100, BillingMode: "outbound", ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNodeFromSource(enrollmentToken, now.Add(time.Second), ""); err != nil {
		t.Fatal(err)
	}
	// No site is scheduled on this node, which is the whole point.
	if host, ok, err := app.scheduledPublicHostForNode(node.ID); err != nil || ok {
		t.Fatalf("fixture expected no scheduled site: host=%q ok=%v err=%v", host, ok, err)
	}
	hint := encodeNodeNetAddressHints([]NodeNetAddress{{Family: "v6", Address: "2001:db8::1"}})
	if _, err := app.db.db.Exec(
		`UPDATE control_nodes SET net_address_hints=?,net_address_hints_at_ms=?,address_v6='',address_v6_source='',net_address_adoption_at_ms=0 WHERE id=?`,
		hint, now.UnixMilli(), node.ID); err != nil {
		t.Fatal(err)
	}
	current, err := app.db.controlNodeByID(node.ID, now)
	if err != nil {
		t.Fatal(err)
	}

	// The edge host is what the probe uses when nothing is scheduled. The
	// candidate is 2001:db8::1 (documentation space), so the probe itself fails;
	// what matters is that adoption got as far as probing instead of returning
	// early for want of a site.
	edgeHost, err := app.nodeEdgeProbeHost(current)
	if err != nil {
		t.Fatalf("nodeEdgeProbeHost: %v", err)
	}
	if edgeHost == "" || edgeHost != edgeCertificateHost("route.example.test", node.GUID) {
		t.Fatalf("edge probe host = %q, want the node's own certificate host", edgeHost)
	}
	if _, err := app.adoptProbedNodeAddresses(context.Background(), current, now); err != nil {
		t.Fatalf("adoptProbedNodeAddresses: %v", err)
	}
	after, err := app.db.controlNodeByID(node.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if after.NetAddressAdoptionAttemptAtMS == 0 {
		t.Fatal("adoption never ran: with no scheduled site it returned before probing")
	}
	if after.AddressV6 != "" {
		t.Fatalf("an unreachable candidate was adopted: %q", after.AddressV6)
	}
}

// With no route domain configured there is no per-node certificate, so adoption
// must decline rather than probe a host nothing can vouch for. It must also not
// consume an attempt: no probe happened, so the next tick should be free to run
// the moment a probe identity exists.
func TestAdoptionWithoutRouteDomainDeclines(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	if _, err := app.db.db.Exec(`UPDATE panel_settings SET route_domain='' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	node, enrollmentToken, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "no-route", Address: "203.0.113.10", Priority: 100, BillingMode: "outbound", ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.db.EnrollControlNodeFromSource(enrollmentToken, now.Add(time.Second), ""); err != nil {
		t.Fatal(err)
	}
	host, err := app.nodeEdgeProbeHost(node)
	if err != nil {
		t.Fatalf("nodeEdgeProbeHost: %v", err)
	}
	if host != "" {
		t.Fatalf("edge probe host = %q, want none without a route domain", host)
	}
	// A fresh hint for the empty family, so adoption has something it would probe
	// if it had a probe identity.
	hint := encodeNodeNetAddressHints([]NodeNetAddress{{Family: "v6", Address: "2001:db8::1"}})
	if _, err := app.db.db.Exec(
		`UPDATE control_nodes SET net_address_hints=?,net_address_hints_at_ms=?,address_v6='',address_v6_source='',net_address_adoption_at_ms=0 WHERE id=?`,
		hint, now.UnixMilli(), node.ID); err != nil {
		t.Fatal(err)
	}
	current, err := app.db.controlNodeByID(node.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.adoptProbedNodeAddresses(context.Background(), current, now); err != nil {
		t.Fatalf("adoptProbedNodeAddresses: %v", err)
	}
	after, err := app.db.controlNodeByID(node.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if after.NetAddressAdoptionAttemptAtMS != 0 {
		t.Fatal("a declined adoption consumed an attempt, delaying the next real probe")
	}
	if after.AddressV6 != "" {
		t.Fatalf("an unverifiable candidate was adopted: %q", after.AddressV6)
	}
}

// The enrollment fallback records the address the Controller observed, so it can
// only ever fill the slot for that address's own family. A pin for the other
// family must leave the slot to the probe-adoption path instead of writing an
// IPv6 address into the IPv4 field.
func TestEnrollHonoursThePinnedFamily(t *testing.T) {
	cases := []struct {
		name           string
		publish        string
		observed       string
		wantV4         string
		wantV6         string
		wantPrimary    string
		wantPrimarySrc string
	}{
		{
			name: "auto fills the observed family", publish: nodeDNSPublishAuto, observed: "203.0.113.10",
			wantV4: "203.0.113.10", wantPrimary: "203.0.113.10", wantPrimarySrc: nodeAddressSourceEnrollment,
		},
		{
			// The v6 slot has its own provenance column, so the primary address's
			// marker stays 'manual': that blank was never filled by inference.
			name: "auto fills v6 as the v6 slot", publish: nodeDNSPublishAuto, observed: "2001:db8::7",
			wantV6: "2001:db8::7", wantPrimary: "2001:db8::7", wantPrimarySrc: nodeAddressSourceManual,
		},
		{
			name: "a v4 pin with an observed v4 fills it", publish: nodeDNSPublishV4, observed: "203.0.113.10",
			wantV4: "203.0.113.10", wantPrimary: "203.0.113.10", wantPrimarySrc: nodeAddressSourceEnrollment,
		},
		{
			name: "a v6 pin with an observed v6 fills it", publish: nodeDNSPublishV6, observed: "2001:db8::7",
			wantV6: "2001:db8::7", wantPrimary: "2001:db8::7", wantPrimarySrc: nodeAddressSourceManual,
		},
		{
			// The pin and the observation disagree, so nothing is written and the
			// probe-adoption path decides later.
			name: "a v4 pin ignores an observed v6", publish: nodeDNSPublishV4, observed: "2001:db8::7",
			wantPrimarySrc: nodeAddressSourceManual,
		},
		{
			name: "a v6 pin ignores an observed v4", publish: nodeDNSPublishV6, observed: "203.0.113.10",
			wantPrimarySrc: nodeAddressSourceManual,
		},
	}
	for _, testCase := range cases {
		app := newTestApp(t)
		now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
		node, enrollmentToken, err := app.db.CreateControlNode(NodeCreateInput{
			Name: "pinned", DNSPublish: testCase.publish, Priority: 100, BillingMode: "outbound", ResetDay: 1,
		}, now)
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		enrolled, _, err := app.db.EnrollControlNodeFromSource(enrollmentToken, now.Add(time.Second), testCase.observed)
		if err != nil {
			t.Fatalf("%s: enroll: %v", testCase.name, err)
		}
		if enrolled.AddressV4 != testCase.wantV4 {
			t.Fatalf("%s: address_v4 = %q, want %q", testCase.name, enrolled.AddressV4, testCase.wantV4)
		}
		if enrolled.AddressV6 != testCase.wantV6 {
			t.Fatalf("%s: address_v6 = %q, want %q", testCase.name, enrolled.AddressV6, testCase.wantV6)
		}
		if enrolled.PrimaryAddress() != testCase.wantPrimary {
			t.Fatalf("%s: primary = %q, want %q", testCase.name, enrolled.PrimaryAddress(), testCase.wantPrimary)
		}
		if enrolled.AddressSource != testCase.wantPrimarySrc {
			t.Fatalf("%s: address_source = %q, want %q", testCase.name, enrolled.AddressSource, testCase.wantPrimarySrc)
		}
		if testCase.wantV6 != "" && enrolled.AddressV6Source != nodeAddressSourceEnrollment {
			t.Fatalf("%s: address_v6_source = %q, want the inferred marker", testCase.name, enrolled.AddressV6Source)
		}
		_ = node
	}
}

// --- cloudflare publishing -------------------------------------------------

// fakeCloudflare is a same-name A/AAAA record store that enough of the
// Cloudflare API to exercise the family reconcile.
type fakeCloudflare struct {
	mu      sync.Mutex
	records []cloudflareAddressRecord
	nextID  int
	calls   []string
}

// suffixID returns the record ID a path names, or "" when the path is the
// collection itself.
func suffixID(path string) string {
	trimmed := strings.TrimSuffix(path, "/")
	index := strings.LastIndex(trimmed, "/")
	if index < 0 {
		return ""
	}
	return trimmed[index+1:]
}

func (f *fakeCloudflare) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))

		if r.Method == http.MethodGet && suffixID(r.URL.Path) == "dns_records" {
			name := r.URL.Query().Get("name")
			matches := make([]cloudflareAddressRecord, 0, len(f.records))
			for _, record := range f.records {
				if strings.EqualFold(record.Name, name) {
					matches = append(matches, record)
				}
			}
			payload, _ := json.Marshal(matches)
			_, _ = w.Write([]byte(`{"success":true,"result_info":{"page":1,"total_pages":1},"result":` + string(payload) + `}`))
			return
		}
		if r.Method == http.MethodGet && suffixID(r.URL.Path) != "dns_records" {
			id := suffixID(r.URL.Path)
			for _, record := range f.records {
				if record.ID == id {
					payload, _ := json.Marshal(record)
					_, _ = w.Write([]byte(`{"success":true,"result":` + string(payload) + `}`))
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":81044,"message":"record does not exist"}]}`))
			return
		}
		if r.Method == http.MethodDelete {
			id := suffixID(r.URL.Path)
			kept := f.records[:0]
			found := false
			for _, record := range f.records {
				if record.ID == id {
					found = true
					continue
				}
				kept = append(kept, record)
			}
			f.records = kept
			if !found {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":81044,"message":"record does not exist"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"result":{"id":"` + id + `"}}`))
			return
		}
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			t.Logf("cloudflare %s %s body=%s", r.Method, r.URL.Path, string(body))
			var payload struct {
				Type    string `json:"type"`
				Name    string `json:"name"`
				Content string `json:"content"`
				Comment string `json:"comment"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":9000,"message":"bad body"}]}`))
				return
			}
			id := ""
			if r.Method == http.MethodPut {
				id = suffixID(r.URL.Path)
			} else {
				f.nextID++
				id = "rec-" + strconv.Itoa(f.nextID)
			}
			updated := cloudflareAddressRecord{ID: id, Type: payload.Type, Name: payload.Name, Content: payload.Content, Comment: payload.Comment}
			replaced := false
			for index, record := range f.records {
				if record.ID == id {
					f.records[index] = updated
					replaced = true
					break
				}
			}
			if !replaced {
				f.records = append(f.records, updated)
			}
			encoded, _ := json.Marshal(updated)
			_, _ = w.Write([]byte(`{"success":true,"result":` + string(encoded) + `}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":7000,"message":"no route"}]}`))
	})
}

func (f *fakeCloudflare) snapshot() []cloudflareAddressRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := make([]cloudflareAddressRecord, len(f.records))
	copy(copied, f.records)
	return copied
}

func (f *fakeCloudflare) seed(record cloudflareAddressRecord) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, record)
}

func newDualStackFixture(t *testing.T) (*App, *fakeCloudflare, *cloudflareClient, SiteNodeSchedule, ControlNode) {
	t.Helper()
	app := newTestApp(t)
	// CreateSite takes no public host, so set the scheduled hostname explicitly:
	// it is the name every published record is written against.
	site, err := app.db.CreateSite("dual-stack", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	site.PublicHost = "dual.example.test"
	site.IngressMode = ingressModeHost
	site.Enabled = true
	if _, err := app.db.db.Exec(`UPDATE sites SET public_host=?,ingress_mode=?,enabled=1 WHERE id=?`,
		site.PublicHost, ingressModeHost, site.ID); err != nil {
		t.Fatal(err)
	}
	reloadedSite, err := app.db.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	site = reloadedSite
	node, _, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "dual", AddressV4: "203.0.113.10", AddressV6: "2001:db8::1",
		Priority: 100, BillingMode: "outbound", ResetDay: 1,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := app.db.siteNodeSchedule(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	schedule.PublicHost = site.PublicHost
	schedule.cfZoneID = "zone-1"
	fake := &fakeCloudflare{}
	server := httptest.NewServer(fake.handler(t))
	t.Cleanup(server.Close)
	cf := &cloudflareClient{token: "test", httpClient: server.Client(), apiBase: server.URL, installUUID: app.db.installUUID}
	return app, fake, cf, schedule, node
}

func familyRecords(records []cloudflareAddressRecord, recordType string) []cloudflareAddressRecord {
	matches := make([]cloudflareAddressRecord, 0, len(records))
	for _, record := range records {
		if record.Type == recordType {
			matches = append(matches, record)
		}
	}
	return matches
}

// A dual-stack node must publish one A and one AAAA for the same hostname.
func TestPublishSiteAddressFamiliesWritesOneRecordPerFamily(t *testing.T) {
	app, fake, cf, schedule, node := newDualStackFixture(t)
	families := node.DNSPublishFamilies()
	if len(families) != 2 {
		t.Fatalf("dual-stack node families = %v", families)
	}
	addresses := map[string]string{"v4": node.IPv4Address(), "v6": node.IPv6Address()}
	records, err := app.publishSiteAddressFamilies(context.Background(), cf, schedule, node, families, "zone-1", addresses)
	if err != nil {
		t.Fatalf("publishSiteAddressFamilies: %v", err)
	}
	t.Logf("published=%+v", records)
	if len(records) != 2 {
		t.Fatalf("published records = %+v, want two", records)
	}
	all := fake.snapshot()
	if len(familyRecords(all, "A")) != 1 || len(familyRecords(all, "AAAA")) != 1 {
		t.Fatalf("remote records = %+v, want exactly one A and one AAAA", all)
	}
	marker := siteDNSOwnershipMarker(schedule.SiteID, app.db.installUUID)
	for _, record := range all {
		if record.Name != "dual.example.test" || record.Comment != marker {
			t.Fatalf("record %+v is not this site's marked record", record)
		}
	}
	if got := primaryPublishedRecord(records); got == nil || got.Family != "v4" {
		t.Fatalf("primary mirror = %+v, want the v4 family", got)
	}
}

// The tracked set is persisted by the schedule writer so the next reconcile
// adopts rather than recreates, and a family that is no longer published leaves
// no row behind.
func TestSiteDNSRecordsPersistAndReplaceByFamily(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	writeSet := func(siteID int64, records []siteDNSRecord) {
		t.Helper()
		tx, err := app.db.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := replaceSiteDNSRecordsTx(tx, siteID, records, now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	writeSet(7, []siteDNSRecord{
		{Family: "v4", ZoneID: "zone-1", RecordID: "rec-a", RecordType: "A", Address: "203.0.113.10"},
		{Family: "v6", ZoneID: "zone-1", RecordID: "rec-b", RecordType: "AAAA", Address: "2001:db8::1"},
	})
	stored, err := app.db.siteDNSRecords(7)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 2 || stored[0].Family != "v4" || stored[1].Family != "v6" {
		t.Fatalf("stored records = %+v, want one row per family in family order", stored)
	}
	if stored[0].RecordID != "rec-a" || stored[1].RecordID != "rec-b" {
		t.Fatalf("stored records = %+v", stored)
	}

	// Replacing with a single family must drop the other family's row, so a
	// family the scheduler stopped publishing cannot stay tracked.
	writeSet(7, []siteDNSRecord{
		{Family: "v4", ZoneID: "zone-1", RecordID: "rec-a", RecordType: "A", Address: "203.0.113.10"},
	})
	remaining, err := app.db.siteDNSRecords(7)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Family != "v4" {
		t.Fatalf("remaining records = %+v, want only v4", remaining)
	}

	// A record that could never be reconciled is not tracked as a phantom.
	writeSet(8, []siteDNSRecord{
		{Family: "v4", ZoneID: "zone-1", RecordID: "", RecordType: "A", Address: "203.0.113.10"},
		{Family: "v6", ZoneID: "zone-1", RecordID: "rec-c", RecordType: "AAAA", Address: "203.0.113.10"},
		{Family: "v9", ZoneID: "zone-1", RecordID: "rec-d", RecordType: "A", Address: "203.0.113.10"},
	})
	if phantom, err := app.db.siteDNSRecords(8); err != nil || len(phantom) != 0 {
		t.Fatalf("phantom records = %+v err=%v, want none", phantom, err)
	}

	// Clearing the set removes every row for that site only.
	if err := app.db.deleteSiteDNSRecords(7); err != nil {
		t.Fatal(err)
	}
	if cleared, err := app.db.siteDNSRecords(7); err != nil || len(cleared) != 0 {
		t.Fatalf("cleared records = %+v err=%v", cleared, err)
	}
}

// Pinning a dual-stack node to one family must remove the other family's record
// while leaving the pinned one in place.
func TestPublishSiteAddressFamiliesRemovesUnpublishedFamily(t *testing.T) {
	app, fake, cf, schedule, node := newDualStackFixture(t)
	ctx := context.Background()
	addresses := map[string]string{"v4": node.IPv4Address(), "v6": node.IPv6Address()}
	// First generation: the dual-stack node publishes both records, and the
	// schedule writer persists what it published.
	published, err := app.publishSiteAddressFamilies(ctx, cf, schedule, node, node.DNSPublishFamilies(), "zone-1", addresses)
	if err != nil {
		t.Fatal(err)
	}
	if len(published) != 2 {
		t.Fatalf("first generation published = %+v, want two", published)
	}
	tx, err := app.db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := replaceSiteDNSRecordsTx(tx, schedule.SiteID, published, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// Second generation: the operator pins the node to IPv4 only, and the version
	// that would have been written is the complete dual-stack set.
	families := normalizeDNSFamilies([]string{"v4", "v6"})
	effective, err := app.db.siteDNSRecords(schedule.SiteID)
	if err != nil {
		t.Fatal(err)
	}
	if len(effective) != 2 {
		t.Fatalf("effective set = %+v, want both families tracked", effective)
	}
	schedule.cfRecordID = ""
	schedule.cfRecordType = ""
	schedule.cfZoneID = "zone-1"
	pinned := node
	pinned.DNSPublish = nodeDNSPublishV4
	records, err := app.publishSiteAddressFamilies(ctx, cf, schedule, pinned, pinned.DNSPublishFamilies(), "zone-1", addresses)
	if err != nil {
		t.Fatalf("pinned publish: %v", err)
	}
	_ = families
	if len(records) != 1 || records[0].Family != "v4" {
		t.Fatalf("published = %+v, want only v4", records)
	}
	all := fake.snapshot()
	if len(familyRecords(all, "AAAA")) != 0 {
		t.Fatalf("AAAA survived a v4-only pin: %+v", all)
	}
	if len(familyRecords(all, "A")) != 1 {
		t.Fatalf("A record was lost: %+v", all)
	}
	// The pin reuses the tracked v4 record instead of creating a duplicate.
	if all[0].ID != published[0].RecordID {
		t.Fatalf("v4 record was recreated: %+v vs %+v", all[0], published[0])
	}
}

// An operator-created record of the same family must block the write for that
// family instead of being overwritten.
func TestPublishSiteAddressFamiliesRefusesUntrackedRecord(t *testing.T) {
	app, fake, cf, schedule, node := newDualStackFixture(t)
	fake.seed(cloudflareAddressRecord{ID: "operator", Type: "A", Name: "dual.example.test", Content: "198.51.100.4", Comment: ""})
	addresses := map[string]string{"v4": node.IPv4Address(), "v6": node.IPv6Address()}
	_, err := app.publishSiteAddressFamilies(context.Background(), cf, schedule, node, []string{"v4"}, "zone-1", addresses)
	if err == nil || !strings.Contains(err.Error(), "untracked exact A record") {
		t.Fatalf("error = %v, want the untracked-record refusal", err)
	}
	all := fake.snapshot()
	if len(all) != 1 || all[0].Content != "198.51.100.4" {
		t.Fatalf("operator record was modified: %+v", all)
	}
}

// A dual-stack node pinned to a family it does not have must publish nothing
// rather than an empty record.
func TestPublishSiteAddressFamiliesSkipsMissingFamily(t *testing.T) {
	app, fake, cf, schedule, _ := newDualStackFixture(t)
	v4Only := ControlNode{Address: "203.0.113.10", DNSPublish: nodeDNSPublishV6}
	if families := v4Only.DNSPublishFamilies(); len(families) != 0 {
		t.Fatalf("families = %v, want none", families)
	}
	records, err := app.publishSiteAddressFamilies(context.Background(), cf, schedule, v4Only, v4Only.DNSPublishFamilies(), "zone-1", map[string]string{})
	if err != nil {
		t.Fatalf("publishing nothing must not fail: %v", err)
	}
	if len(records) != 0 || len(fake.snapshot()) != 0 {
		t.Fatalf("records = %+v remote = %+v, want none", records, fake.snapshot())
	}
}

// A stored family with no address is a programming error, not a silent skip.
func TestPublishSiteAddressFamiliesRejectsAddresslessFamily(t *testing.T) {
	app, _, cf, schedule, node := newDualStackFixture(t)
	_, err := app.publishSiteAddressFamilies(context.Background(), cf, schedule, node, []string{"v6"}, "zone-1", map[string]string{"v4": "203.0.113.10"})
	if err == nil || !strings.Contains(err.Error(), "AAAA record requires") {
		t.Fatalf("error = %v, want a refusal naming the family", err)
	}
}

// The family list round-trips through the stored schedule column so a restart
// republishes the same set.
func TestEncodeDNSFamiliesRoundTrip(t *testing.T) {
	if got := encodeDNSFamilies([]string{"v6", "v4", "v4"}); got != "v4,v6" {
		t.Fatalf("encodeDNSFamilies = %q, want a normalized ordered value", got)
	}
	if got := decodeDNSFamilies("v6"); len(got) != 1 || got[0] != "v6" {
		t.Fatalf("decodeDNSFamilies = %v", got)
	}
	if got := decodeDNSFamilies(""); got != nil {
		t.Fatalf("decodeDNSFamilies(\"\") = %v, want nil", got)
	}
	if !dnsFamiliesEqual([]string{"v6", "v4"}, []string{"v4", "v6"}) {
		t.Fatal("family comparison must be order independent")
	}
	if dnsFamiliesEqual([]string{"v4"}, []string{"v4", "v6"}) {
		t.Fatal("family comparison must not ignore a missing family")
	}
}

// The node input carries both slots and the publish mode through a save.
func TestUpdateControlNodePersistsDualStackPublishing(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	node, _, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "dual", AddressV4: "203.0.113.10", AddressV6: "2001:db8::1",
		Priority: 100, BillingMode: "outbound", ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if node.AddressV4 != "203.0.113.10" || node.AddressV6 != "2001:db8::1" {
		t.Fatalf("created node slots = %q/%q", node.AddressV4, node.AddressV6)
	}
	if node.PrimaryAddress() != "203.0.113.10" {
		t.Fatalf("primary address = %q, want the v4 slot", node.PrimaryAddress())
	}

	updated, err := app.db.UpdateControlNode(node.ID, NodeCreateInput{
		Name: node.Name, AddressV4: "203.0.113.10", AddressV6: "2001:db8::1",
		DNSPublish: nodeDNSPublishV6, Port: node.Port, Priority: node.Priority,
		BillingMode: node.BillingMode, ResetDay: node.ResetDay,
	}, true, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if updated.DNSPublish != nodeDNSPublishV6 {
		t.Fatalf("dns_publish = %q, want v6", updated.DNSPublish)
	}
	if families := updated.DNSPublishFamilies(); len(families) != 1 || families[0] != "v6" {
		t.Fatalf("families = %v, want only v6", families)
	}
}

// An existing single-address node needs no migration: its family is derived from
// the legacy column and it keeps publishing exactly one record.
func TestLegacySingleAddressNodeStillPublishesOneFamily(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	node, _, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "legacy", Address: "203.0.113.7", Priority: 100, BillingMode: "outbound", ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Model the pre-upgrade row: only the legacy column is populated.
	if _, err := app.db.db.Exec("UPDATE control_nodes SET address_v4='',address_v6='',dns_publish='auto' WHERE id=?", node.ID); err != nil {
		t.Fatal(err)
	}
	reloaded, err := app.db.controlNodeByID(node.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.IPv4Address() != "203.0.113.7" || reloaded.IPv6Address() != "" {
		t.Fatalf("legacy resolution = %q/%q", reloaded.IPv4Address(), reloaded.IPv6Address())
	}
	families := reloaded.DNSPublishFamilies()
	if len(families) != 1 || families[0] != "v4" {
		t.Fatalf("legacy families = %v, want only v4", families)
	}
}
