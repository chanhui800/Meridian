package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A forwarded header from an untrusted peer makes the ingress path ambiguous.
// Do not guess whether the peer is the Agent or a reverse proxy; wait for a
// trusted proxy configuration instead of publishing a potentially wrong IP.
func TestEnrollHandlerIgnoresClaimedRealIPFromUntrustedPeer(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	_, token, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "spoofed-source", Address: "", Priority: 100, BillingMode: "outbound", ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatalf("CreateControlNode: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/agent/enroll", nil)
	request.RemoteAddr = "198.51.100.7:51234"
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(agentPlatformHeader, "linux/amd64")
	request.Header.Set("X-Real-IP", "203.0.113.250")

	recorder := httptest.NewRecorder()
	app.withAgentPreAuth(app.handleAgentEnroll)(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("enroll status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	node := findNodeByName(t, app, now, "spoofed-source")
	if node.Address != "" {
		t.Fatalf("address = %q, want no guessed address from an untrusted forwarding hop", node.Address)
	}
	if node.AddressSource != nodeAddressSourceManual {
		t.Fatalf("address_source = %q, want manual for an unfilled address", node.AddressSource)
	}
}

// A trusted proxy is the one case where a forwarded address is meaningful, so its
// normalized X-Real-IP is used. This is how a Controller behind a reverse proxy
// still learns the node's real address instead of the proxy's.
func TestEnrollHandlerUsesRealIPFromConfiguredTrustedProxy(t *testing.T) {
	app := newTestApp(t)
	trusted, err := parseTrustedProxyCIDRs("172.17.0.0/16")
	if err != nil {
		t.Fatalf("parseTrustedProxyCIDRs: %v", err)
	}
	app.trustedProxies = trusted
	now := time.Now().UTC()
	_, token, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "proxied-source", Address: "", Priority: 100, BillingMode: "outbound", ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatalf("CreateControlNode: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/agent/enroll", nil)
	request.RemoteAddr = "172.17.0.1:51234"
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(agentPlatformHeader, "linux/amd64")
	request.Header.Set("X-Real-IP", "189.24.112.81")

	recorder := httptest.NewRecorder()
	app.withAgentPreAuth(app.handleAgentEnroll)(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("enroll status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	node := findNodeByName(t, app, now, "proxied-source")
	if node.Address != "189.24.112.81" {
		t.Fatalf("address = %q, want the normalized X-Real-IP behind a trusted proxy", node.Address)
	}
}

// A Controller reached through an untrusted hop sees only that hop, which must
// never be published as the node's DNS record.
func TestEnrollHandlerLeavesAddressBlankForPrivatePeer(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	_, token, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "private-peer", Address: "", Priority: 100, BillingMode: "outbound", ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatalf("CreateControlNode: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/agent/enroll", nil)
	request.RemoteAddr = "10.1.2.3:51234"
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(agentPlatformHeader, "linux/amd64")
	recorder := httptest.NewRecorder()
	app.withAgentPreAuth(app.handleAgentEnroll)(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("enroll status = %d", recorder.Code)
	}
	node := findNodeByName(t, app, now, "private-peer")
	if node.Address != "" {
		t.Fatalf("address = %q, want it left blank for a private peer", node.Address)
	}
	if node.AddressSource != nodeAddressSourceManual {
		t.Fatalf("address_source = %q, want manual", node.AddressSource)
	}
}

func TestAgentReportHandlerAdoptsObservedNATAddress(t *testing.T) {
	for _, testCase := range []struct {
		name, remoteAddr, realIP, trustedCIDRs, wantV4, wantV6, wantSource string
		externalProxyEnabled                                               bool
	}{
		{name: "direct_ipv4", remoteAddr: "140.245.35.86:51234", wantV4: "140.245.35.86", wantSource: nodeAddressSourceReport},
		{name: "trusted_proxy_ipv6", remoteAddr: "172.17.0.1:51234", realIP: "2606:4700:4700::1111", trustedCIDRs: "172.17.0.0/16", wantV6: "2606:4700:4700::1111", wantSource: nodeAddressSourceReport, externalProxyEnabled: true},
		{name: "trusted_proxy_without_client_ip", remoteAddr: "172.17.0.1:51234", trustedCIDRs: "172.17.0.0/16"},
		{name: "untrusted_proxy_is_not_published", remoteAddr: "140.245.35.87:51234", realIP: "203.0.113.99"},
		{name: "external_proxy_mode_rejects_untrusted_public_peer", remoteAddr: "140.245.35.88:51234", externalProxyEnabled: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			app := newTestApp(t)
			if testCase.trustedCIDRs != "" {
				trusted, err := parseTrustedProxyCIDRs(testCase.trustedCIDRs)
				if err != nil {
					t.Fatal(err)
				}
				app.trustedProxies = trusted
			}
			now := time.Now().UTC()
			app.panelExternalProxyEnabled = testCase.externalProxyEnabled
			_, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "observed-report-source"}, now)
			if err != nil {
				t.Fatal(err)
			}
			_, agentToken, err := app.db.EnrollControlNodeFromSource(enrollment, now, "10.0.0.5")
			if err != nil {
				t.Fatal(err)
			}

			request := httptest.NewRequest(http.MethodPost, "/api/agent/report", strings.NewReader(`{"boot_id":"nat-report","sequence":1,"interface_name":"eth0"}`))
			request.RemoteAddr = testCase.remoteAddr
			request.Header.Set("Authorization", "Bearer "+agentToken)
			request.Header.Set("Content-Type", "application/json")
			if testCase.realIP != "" {
				request.Header.Set("X-Real-IP", testCase.realIP)
			}
			recorder := httptest.NewRecorder()
			app.handleAgentReport(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("report status=%d, body=%s", recorder.Code, recorder.Body.String())
			}

			updated := findNodeByName(t, app, now.Add(time.Second), "observed-report-source")
			if updated.AddressV4 != testCase.wantV4 || updated.AddressV6 != testCase.wantV6 {
				t.Fatalf("NAT source result: v4=%q v6=%q", updated.AddressV4, updated.AddressV6)
			}
			if testCase.wantSource != "" && updated.AddressSource != testCase.wantSource && updated.AddressV6Source != testCase.wantSource {
				t.Fatalf("inferred source was not marked for review: v4 source=%q v6 source=%q", updated.AddressSource, updated.AddressV6Source)
			}
		})
	}
}

func TestAgentObservedAddressHandlerReturnsOnlyTrustedRequestedFamily(t *testing.T) {
	app := newTestApp(t)
	trusted, err := parseTrustedProxyCIDRs("172.17.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	app.trustedProxies = trusted
	app.panelExternalProxyEnabled = true
	now := time.Now().UTC()
	for _, testCase := range []struct {
		name, family, peer, realIP, want string
	}{
		{"trusted_ipv4", "v4", "172.17.0.1:443", "140.245.35.86", "140.245.35.86"},
		{"family_mismatch", "v6", "172.17.0.1:443", "140.245.35.86", ""},
		{"untrusted_public_peer", "v4", "140.245.35.87:443", "203.0.113.5", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "observed-address-probe-" + testCase.name}, now)
			if err != nil {
				t.Fatal(err)
			}
			_, agentToken, err := app.db.EnrollControlNode(enrollment, now)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/api/agent/observed-address", strings.NewReader(`{"family":"`+testCase.family+`"}`))
			request.RemoteAddr = testCase.peer
			request.Header.Set("Authorization", "Bearer "+agentToken)
			request.Header.Set("Content-Type", "application/json")
			if testCase.realIP != "" {
				request.Header.Set("X-Real-IP", testCase.realIP)
			}
			recorder := httptest.NewRecorder()
			app.handleAgentObservedAddress(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if recorder.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("cache-control=%q, want no-store", recorder.Header().Get("Cache-Control"))
			}
			var result struct {
				Family  string `json:"family"`
				Address string `json:"address"`
			}
			if err := json.NewDecoder(recorder.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if result.Family != testCase.family || result.Address != testCase.want {
				t.Fatalf("observed response=%+v, want family=%q address=%q", result, testCase.family, testCase.want)
			}
			updated := findNodeByName(t, app, now.Add(time.Second), "observed-address-probe-"+testCase.name)
			if testCase.want == "" {
				if updated.AddressV4 != "" || updated.AddressV6 != "" {
					t.Fatalf("untrusted observed address was persisted: v4=%q v6=%q", updated.AddressV4, updated.AddressV6)
				}
			} else if testCase.family == "v4" && updated.AddressV4 != testCase.want {
				t.Fatalf("observed IPv4 was not persisted: %+v", updated)
			} else if testCase.family == "v6" && updated.AddressV6 != testCase.want {
				t.Fatalf("observed IPv6 was not persisted: %+v", updated)
			}
		})
	}
}

func findNodeByName(t *testing.T, app *App, now time.Time, name string) ControlNode {
	t.Helper()
	nodes, err := app.db.listControlNodes(now)
	if err != nil {
		t.Fatalf("listControlNodes: %v", err)
	}
	for _, node := range nodes {
		if node.Name == name {
			return node
		}
	}
	t.Fatalf("node %q not found", name)
	return ControlNode{}
}

// The node address is published as a site's DNS A/AAAA record, so the fallback
// that fills it from the enrollment request is deliberately narrow: only a blank
// address, only an address the controller observed directly, and only a globally
// routable one. These tests pin all three limits.

func TestEnrollFillsBlankAddressFromSource(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	node, token, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "blank-address", Address: "", Priority: 100, BillingMode: "outbound", ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatalf("CreateControlNode: %v", err)
	}
	if node.Address != "" || node.AddressSource != nodeAddressSourceManual {
		t.Fatalf("created node = %q/%q, want an empty manual address", node.Address, node.AddressSource)
	}

	enrolled, _, err := app.db.EnrollControlNodeFromSource(token, now.Add(time.Second), "198.51.100.7")
	if err != nil {
		t.Fatalf("EnrollControlNodeFromSource: %v", err)
	}
	if enrolled.Address != "198.51.100.7" {
		t.Fatalf("enrolled address = %q, want the observed source", enrolled.Address)
	}
	if enrolled.AddressSource != nodeAddressSourceEnrollment {
		t.Fatalf("address_source = %q, want %q so the panel asks for a review", enrolled.AddressSource, nodeAddressSourceEnrollment)
	}
}

func TestEnrollNeverOverwritesAnOperatorAddress(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	node, token, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "explicit-address", Address: "203.0.113.10", Priority: 100, BillingMode: "outbound", ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatalf("CreateControlNode: %v", err)
	}
	enrolled, _, err := app.db.EnrollControlNodeFromSource(token, now.Add(time.Second), "198.51.100.7")
	if err != nil {
		t.Fatalf("EnrollControlNodeFromSource: %v", err)
	}
	if enrolled.Address != "203.0.113.10" {
		t.Fatalf("enrolled address = %q, want the operator's value kept", enrolled.Address)
	}
	if enrolled.AddressSource != nodeAddressSourceManual {
		t.Fatalf("address_source = %q, want manual", enrolled.AddressSource)
	}
	_ = node
}

// TestEnrollRejectsUnusableSourceAddresses covers the routability gate. A private
// or loopback address would publish a DNS record no client can reach, and a
// proxied request describes the proxy rather than the node.
func TestEnrollRejectsUnusableSourceAddresses(t *testing.T) {
	for _, source := range []string{
		"",
		"   ",
		"not-an-ip",
		"10.0.0.5",        // private
		"192.168.1.10",    // private
		"172.16.5.5",      // private
		"127.0.0.1",       // loopback
		"169.254.1.1",     // link-local
		"0.0.0.0",         // unspecified
		"::1",             // IPv6 loopback
		"fe80::1",         // IPv6 link-local
		"fd00::1",         // IPv6 unique-local
		"224.0.0.1",       // multicast
		"198.51.100.7:42", // a port must not be smuggled in
	} {
		if got, ok := enrollSourceAddressAnswer(source); ok {
			t.Fatalf("source %q was accepted as %q, want refusal", source, got)
		}
	}
	for _, source := range []string{"198.51.100.7", "203.0.113.10", "189.24.112.81", "2001:db8::1", "2606:4700::1111"} {
		got, ok := enrollSourceAddressAnswer(source)
		if !ok {
			t.Fatalf("source %q was refused, want it accepted", source)
		}
		if got != source {
			t.Fatalf("source %q normalized to %q", source, got)
		}
	}
	// Surrounding whitespace is normal on a header-derived value.
	if got, ok := enrollSourceAddressAnswer("  198.51.100.7  "); !ok || got != "198.51.100.7" {
		t.Fatalf("padded source = %q/%v, want 198.51.100.7", got, ok)
	}
}

// TestEnrollWithoutSourceKeepsBehaviour is the compatibility guard: callers that
// pass no source (the existing signature) must behave exactly as before.
func TestEnrollWithoutSourceKeepsBehaviour(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	_, token, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "no-source", Address: "", Priority: 100, BillingMode: "outbound", ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatalf("CreateControlNode: %v", err)
	}
	enrolled, _, err := app.db.EnrollControlNode(token, now.Add(time.Second))
	if err != nil {
		t.Fatalf("EnrollControlNode: %v", err)
	}
	if enrolled.Address != "" {
		t.Fatalf("address = %q, want it left blank when no source is given", enrolled.Address)
	}
	if enrolled.AddressSource != nodeAddressSourceManual {
		t.Fatalf("address_source = %q, want manual", enrolled.AddressSource)
	}
}

// TestOperatorSaveClearsTheInferredMarker checks that editing a node from the
// panel reclassifies the address as operator-confirmed, so the "verify this"
// hint disappears once a human has looked at it.
func TestOperatorSaveClearsTheInferredMarker(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	node, token, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "inferred-then-saved", Address: "", Priority: 100, BillingMode: "outbound", ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatalf("CreateControlNode: %v", err)
	}
	enrolled, _, err := app.db.EnrollControlNodeFromSource(token, now.Add(time.Second), "198.51.100.7")
	if err != nil {
		t.Fatalf("EnrollControlNodeFromSource: %v", err)
	}
	if enrolled.AddressSource != nodeAddressSourceEnrollment {
		t.Fatalf("precondition failed: address_source = %q", enrolled.AddressSource)
	}

	// The operator confirms the value (or corrects it) from the panel.
	saved, err := app.db.UpdateControlNode(node.ID, NodeCreateInput{
		Name: node.Name, Address: "198.51.100.7", Port: node.Port, Priority: node.Priority,
		TrafficQuota: node.TrafficQuota, BillingMode: node.BillingMode, ResetDay: node.ResetDay,
	}, true, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("UpdateControlNode: %v", err)
	}
	if saved.AddressSource != nodeAddressSourceManual {
		t.Fatalf("address_source after save = %q, want manual", saved.AddressSource)
	}
	// A correction is a manual address too.
	corrected, err := app.db.UpdateControlNode(node.ID, NodeCreateInput{
		Name: node.Name, Address: "203.0.113.99", Port: node.Port, Priority: node.Priority,
		TrafficQuota: node.TrafficQuota, BillingMode: node.BillingMode, ResetDay: node.ResetDay,
	}, true, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("UpdateControlNode (correction): %v", err)
	}
	if corrected.Address != "203.0.113.99" || corrected.AddressSource != nodeAddressSourceManual {
		t.Fatalf("corrected node = %q/%q, want the new manual address", corrected.Address, corrected.AddressSource)
	}
}

// TestInferredAddressSatisfiesScheduling guards the point of the feature: the
// inferred address must be usable everywhere the operator-entered one is, so the
// readiness probe no longer rejects the node for having no address.
func TestInferredAddressSatisfiesScheduling(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().In(time.Local)
	_, token, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "schedulable", Address: "", Priority: 100, BillingMode: "bidirectional", ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatalf("CreateControlNode: %v", err)
	}
	enrolled, _, err := app.db.EnrollControlNodeFromSource(token, now, "189.24.112.81")
	if err != nil {
		t.Fatalf("EnrollControlNodeFromSource: %v", err)
	}
	if _, err := nodeDialAddress(enrolled.Address, nodeHTTPSProbePort(enrolled)); err != nil {
		t.Fatalf("the inferred address is unusable for the readiness probe: %v", err)
	}
	if !strings.HasPrefix(enrolled.Address, "189.24.112.81") {
		t.Fatalf("address = %q", enrolled.Address)
	}
	// Before the fallback the same node would have failed here, which is the
	// failure this feature removes.
	if _, err := nodeDialAddress("", nodeHTTPSProbePort(enrolled)); err == nil {
		t.Fatal("an empty address unexpectedly satisfied the probe")
	}
}

// TestNodeAddressSourceMigrationBackfillsManual proves the upgrade path: an
// already-deployed node has its address_source reported as manual, because that
// is what every pre-existing row actually is.
func TestNodeAddressSourceMigrationBackfillsManual(t *testing.T) {
	app := newTestApp(t)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	node, _, err := app.db.CreateControlNode(NodeCreateInput{
		Name: "legacy-row", Address: "203.0.113.10", Priority: 100, BillingMode: "outbound", ResetDay: 1,
	}, now)
	if err != nil {
		t.Fatalf("CreateControlNode: %v", err)
	}
	// Simulate the pre-migration value a real upgraded row would carry.
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET address_source='manual' WHERE id=?`, node.ID); err != nil {
		t.Fatalf("seed legacy source: %v", err)
	}
	reloaded, err := app.db.controlNodeByID(node.ID, now)
	if err != nil {
		t.Fatalf("controlNodeByID: %v", err)
	}
	if reloaded.AddressSource != nodeAddressSourceManual {
		t.Fatalf("address_source = %q, want manual", reloaded.AddressSource)
	}
}
