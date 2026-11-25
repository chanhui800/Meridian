package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSitesExposeOnlyAppliedNodeEntry(t *testing.T) {
	for _, generic := range []bool{false, true} {
		for _, state := range []struct {
			name    string
			enabled bool
			applied bool
		}{
			{"desired-only", true, false},
			{"applied", true, true},
			{"disabled-cleanup-pending", false, true},
			{"cleanup-complete", false, false},
		} {
			t.Run(state.name+map[bool]string{false: "/fixed", true: "/generic"}[generic], func(t *testing.T) {
				app := newTestApp(t)
				now := time.Now()
				node, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "entry-port", Address: "203.0.113.41", Port: 9443}, now)
				if err != nil {
					t.Fatal(err)
				}
				input := Site{Name: "applied-entry", PublicHost: "entry.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096", PlaybackMode: "direct", StreamHosts: "[]", UAMode: passthroughUAMode}
				if generic {
					input.UpstreamMode, input.TargetURL = upstreamModePathTarget, ""
				}
				site, err := app.db.CreateSiteRecord(input)
				if err != nil {
					t.Fatal(err)
				}
				var owner any
				if state.applied {
					owner = node.ID
				}
				appliedPort := 0
				if state.applied {
					appliedPort = node.Port
				}
				if _, err := app.db.db.Exec(`INSERT INTO site_node_schedules(site_id,enabled,mode,desired_node_id,applied_node_id,applied_node_port,created_at_ms,updated_at_ms)
					VALUES(?,?,'global',?,?,?,?,?)`, site.ID, sqliteBool(state.enabled), node.ID, owner, appliedPort, now.UnixMilli(), now.UnixMilli()); err != nil {
					t.Fatal(err)
				}
				response := httptest.NewRecorder()
				app.handleSites(response, httptest.NewRequest(http.MethodGet, "/api/sites", nil))
				if response.Code != http.StatusOK {
					t.Fatalf("sites status=%d body=%s", response.Code, response.Body.String())
				}
				var rows []struct {
					ID               int64 `json:"id"`
					AppliedNodeID    int64 `json:"applied_node_id"`
					AppliedNodePort  int   `json:"applied_node_port"`
					ControllerClosed bool  `json:"controller_closed"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 || rows[0].ID != site.ID {
					t.Fatalf("unexpected site entries: %+v", rows)
				}
				row := rows[0]
				if state.applied {
					if row.AppliedNodeID != node.ID || row.AppliedNodePort != 9443 || !row.ControllerClosed {
						t.Fatalf("applied entry not exposed: %+v", row)
					}
				} else if row.AppliedNodeID != 0 || row.AppliedNodePort != 0 || row.ControllerClosed {
					t.Fatalf("desired or removed entry exposed before applied ownership: %+v", row)
				}
			})
		}
	}
}

// A node's configured port changes before its Agent has applied the new
// listener. The site endpoint must keep exposing the last acknowledged port
// until an Agent report confirms the current config revision and hash.
func TestAppliedNodeEntryPortWaitsForMatchingAgentACK(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "applied-entry-ack", Address: "203.0.113.42", Port: 9443}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	// Keep this test runnable while checking older fixture databases that have
	// not yet run the new production migration.
	var hasAppliedPort int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('site_node_schedules') WHERE name='applied_node_port'").Scan(&hasAppliedPort); err != nil {
		t.Fatal(err)
	}
	if hasAppliedPort == 0 {
		if _, err := app.db.db.Exec("ALTER TABLE site_node_schedules ADD COLUMN applied_node_port INTEGER NOT NULL DEFAULT 0"); err != nil {
			t.Fatal(err)
		}
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "applied-entry-ack", PublicHost: "ack-entry.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096", StreamHosts: "[]", UAMode: passthroughUAMode})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`INSERT INTO site_node_schedules(site_id,enabled,mode,desired_node_id,applied_node_id,applied_node_port,created_at_ms,updated_at_ms)
		VALUES(?,1,'fixed',?,?,?,?,?)`, site.ID, node.ID, node.ID, 9443, now.UnixMilli(), now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	updated, err := app.db.UpdateControlNode(node.ID, NodeCreateInput{
		Name: node.Name, Address: node.Address, AddressV4: node.AddressV4, AddressV6: node.AddressV6,
		DNSPublish: node.DNSPublish, Port: 9555, Priority: node.Priority, TrafficQuota: node.TrafficQuota,
		BillingMode: node.BillingMode, ResetDay: node.ResetDay,
	}, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if updated.Port != 9555 || updated.AppliedConfigRevision == updated.ConfigRevision {
		t.Fatalf("fixture did not leave a new port pending application: desired=%d applied=%d/%d", updated.Port, updated.AppliedConfigRevision, updated.ConfigRevision)
	}
	readSitePort := func() int {
		t.Helper()
		response := httptest.NewRecorder()
		app.handleSites(response, httptest.NewRequest(http.MethodGet, "/api/sites", nil))
		var entries []struct {
			ID     int64 `json:"id"`
			Port   int   `json:"applied_node_port"`
			Closed bool  `json:"controller_closed"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &entries) != nil || len(entries) != 1 || entries[0].ID != site.ID || !entries[0].Closed {
			t.Fatalf("invalid sites response: status=%d body=%s", response.Code, response.Body.String())
		}
		return entries[0].Port
	}
	if got := readSitePort(); got != 9443 {
		t.Fatalf("pending node port leaked into applied entry: got=%d want=9443", got)
	}
	const configHash = "acknowledged-config-v2"
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET desired_config_hash=?,desired_config_revision=config_revision,config_dirty=1 WHERE id=?`, configHash, node.ID); err != nil {
		t.Fatal(err)
	}
	var revision int64
	if err := app.db.db.QueryRow("SELECT config_revision FROM control_nodes WHERE id=?", node.ID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.RecordNodeReport(token, NodeReport{BootID: "applied-entry-ack", Sequence: 1, InterfaceName: "eth0", AgentVersion: appVersion,
		AppliedConfigHash: configHash, AppliedConfigRevision: revision}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := readSitePort(); got != 9555 {
		t.Fatalf("acknowledged node port was not published: got=%d want=9555", got)
	}
}

func TestSchema60AppliedNodePortBackfillRequiresAcknowledgedConfig(t *testing.T) {
	app := newTestApp(t)
	now := time.Now()
	node, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "port-backfill-ready", Address: "203.0.113.43", Port: 9443}, now)
	if err != nil {
		t.Fatal(err)
	}
	uncertainNode, _, err := app.db.CreateControlNode(NodeCreateInput{Name: "port-backfill-pending", Address: "203.0.113.44", Port: 9555}, now)
	if err != nil {
		t.Fatal(err)
	}
	siteReady, err := app.db.CreateSiteRecord(Site{Name: "port-backfill-ready", ListenPort: 18091, PublicHost: "ready.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096", StreamHosts: "[]", UAMode: passthroughUAMode})
	if err != nil {
		t.Fatal(err)
	}
	sitePending, err := app.db.CreateSiteRecord(Site{Name: "port-backfill-pending", ListenPort: 18092, PublicHost: "pending.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8097", StreamHosts: "[]", UAMode: passthroughUAMode})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ siteID, nodeID int64 }{{siteReady.ID, node.ID}, {sitePending.ID, uncertainNode.ID}} {
		if _, err := app.db.db.Exec(`INSERT INTO site_node_schedules(site_id,enabled,mode,desired_node_id,applied_node_id,created_at_ms,updated_at_ms)
			VALUES(?,1,'fixed',?,?,?,?)`, row.siteID, row.nodeID, row.nodeID, now.UnixMilli(), now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET config_dirty=0,desired_config_hash='ack-ready',applied_config_hash='ack-ready',
		config_revision=4,desired_config_revision=4,applied_config_revision=4 WHERE id=?`, node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec(`UPDATE control_nodes SET config_dirty=1,desired_config_hash='pending-new',applied_config_hash='ack-old',
		config_revision=5,desired_config_revision=5,applied_config_revision=4 WHERE id=?`, uncertainNode.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("ALTER TABLE site_node_schedules DROP COLUMN applied_node_port"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("PRAGMA user_version=59"); err != nil {
		t.Fatal(err)
	}
	if err := app.db.migrate(); err != nil {
		t.Fatal(err)
	}
	var readyPort, pendingPort int
	if err := app.db.db.QueryRow("SELECT applied_node_port FROM site_node_schedules WHERE site_id=?", siteReady.ID).Scan(&readyPort); err != nil {
		t.Fatal(err)
	}
	if err := app.db.db.QueryRow("SELECT applied_node_port FROM site_node_schedules WHERE site_id=?", sitePending.ID).Scan(&pendingPort); err != nil {
		t.Fatal(err)
	}
	if readyPort != 9443 || pendingPort != 0 {
		t.Fatalf("schema-59 port backfill = acknowledged:%d pending:%d, want 9443/0", readyPort, pendingPort)
	}
	if err := app.db.migrate(); err != nil {
		t.Fatalf("schema-61 migration is not idempotent: %v", err)
	}
}
