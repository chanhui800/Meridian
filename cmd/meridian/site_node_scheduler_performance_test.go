package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type schedulerDNSRoundTripper func(*http.Request) (*http.Response, error)

func (f schedulerDNSRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type schedulerDNSFixture struct {
	app                   *App
	site                  Site
	node                  ControlNode
	now                   time.Time
	host                  string
	probeSecret           []byte
	gets                  atomic.Int64
	puts                  atomic.Int64
	posts                 atomic.Int64
	deletes               atomic.Int64
	probes                atomic.Int64
	probeFails            atomic.Bool
	rejectDuplicates      bool
	injectDuplicateOnPost bool
	injectedContent       string
	mu                    sync.Mutex
	records               map[string]cloudflareAddressRecord
	probeIPs              map[string]int
	token                 string
	onGet                 func()
}

func newSchedulerDNSFixture(t *testing.T, dual bool) *schedulerDNSFixture {
	t.Helper()
	app := newTestApp(t)
	now := time.Now()
	input := NodeCreateInput{Name: "maintenance-fixture", AddressV4: "203.0.113.5", Port: 443, DNSPublish: "auto"}
	if dual {
		input.AddressV6 = "2001:db8::5"
	}
	node, enrollment, err := app.db.CreateControlNode(input, now)
	if err != nil {
		t.Fatal(err)
	}
	node, _, err = app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	probeSecretText, err := dNodeProbeSecret(app.db, node)
	if err != nil {
		t.Fatal(err)
	}
	probeSecret, err := decodeNodeProbeSecret(probeSecretText)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the real certificate hostname check and encrypted node-probe secret;
	// only the network probe and Cloudflare API are replaced at their boundaries.
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the DNS fixture unexpectedly made a real health-probe request")
	}))
	leaf, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	server.Close()
	if err != nil || len(leaf.DNSNames) == 0 {
		t.Fatal("fixture certificate is unavailable", err)
	}
	host := leaf.DNSNames[0]
	app.panelCertificates = newPanelCertificateManager(filepath.Join(t.TempDir(), "fixture.db"), nil)
	certPath, _, err := app.panelCertificates.nodeEdgeTLSPaths(node.GUID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "maintenance-fixture", PublicHost: host, IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{"UPDATE sites SET enabled=1 WHERE id=?", []any{site.ID}},
		{"UPDATE control_nodes SET desired_config_hash='fixture-hash',applied_config_hash='fixture-hash' WHERE id=?", []any{node.ID}},
		{`UPDATE site_node_schedules SET desired_node_id=?,applied_node_id=?,dns_status='active',config_hash='fixture-hash',config_pending_since_ms=0,cf_zone_id='zone-1',cf_record_id='record-a',cf_record_type='A',applied_address='203.0.113.5' WHERE site_id=?`, []any{node.ID, node.ID, site.ID}},
	} {
		if _, err := app.db.db.Exec(statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	f := &schedulerDNSFixture{app: app, site: *site, node: node, now: now, host: host, probeSecret: probeSecret, records: make(map[string]cloudflareAddressRecord), probeIPs: make(map[string]int)}
	marker := siteDNSOwnershipMarker(site.ID, app.db.installUUID)
	f.records["record-a"] = cloudflareAddressRecord{ID: "record-a", Type: "A", Name: host, Content: "203.0.113.5", Comment: marker}
	if dual {
		f.records["record-aaaa"] = cloudflareAddressRecord{ID: "record-aaaa", Type: "AAAA", Name: host, Content: "2001:db8::5", Comment: marker}
		if _, err := app.db.db.Exec(`INSERT INTO site_node_dns_records(site_id,family,zone_id,record_id,record_type,address,updated_at_ms) VALUES(?,'v6','zone-1','record-aaaa','AAAA','2001:db8::5',?)`, site.ID, now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	f.credentials(t, 'a')
	previousTransport := http.DefaultTransport
	http.DefaultTransport = schedulerDNSRoundTripper(f.roundTrip)
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	return f
}

func (f *schedulerDNSFixture) credentials(t *testing.T, value byte) {
	t.Helper()
	token := strings.Repeat(string(value), 40)
	ciphertext, err := encryptPanelACMETokenWithSecret(token, jwtSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.app.db.SavePanelACMECredentials("fixture@example.test", "cloudflare", ciphertext, false); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.token = "Bearer " + token
	f.mu.Unlock()
}

func schedulerDNSRecordJSON(record cloudflareAddressRecord) map[string]string {
	return map[string]string{"id": record.ID, "type": record.Type, "name": record.Name, "content": record.Content, "comment": record.Comment}
}

func (f *schedulerDNSFixture) roundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Host != "api.cloudflare.com" {
		return nil, errors.New("unexpected network target in scheduler DNS fixture")
	}
	f.mu.Lock()
	token, hook := f.token, f.onGet
	f.mu.Unlock()
	if request.Header.Get("Authorization") != token {
		return nil, errors.New("scheduler used an outdated Cloudflare credential")
	}
	if request.Method == http.MethodGet && hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := path.Base(request.URL.Path)
	var result any
	switch request.Method {
	case http.MethodGet:
		f.gets.Add(1)
		if id == "zones" {
			result = []map[string]string{{"id": "zone-1"}}
		} else if id == "dns_records" {
			values := make([]map[string]string, 0, len(f.records))
			for _, record := range f.records {
				if name := request.URL.Query().Get("name"); name == "" || strings.EqualFold(name, record.Name) {
					values = append(values, schedulerDNSRecordJSON(record))
				}
			}
			result = values
		} else if record, ok := f.records[id]; ok {
			result = schedulerDNSRecordJSON(record)
		} else {
			return schedulerDNSMissingRecordResponse(request), nil
		}
	case http.MethodPut, http.MethodPost:
		var payload struct{ Type, Name, Content, Comment string }
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			return nil, err
		}
		if f.rejectDuplicates {
			for existingID, existing := range f.records {
				if existingID != id && existing.Type == payload.Type && strings.EqualFold(existing.Name, payload.Name) && existing.Content == payload.Content {
					body := `{"success":false,"errors":[{"code":81058,"message":"An identical record already exists."}],"result":null}`
					return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
				}
			}
		}
		if request.Method == http.MethodPut {
			if _, exists := f.records[id]; !exists {
				return schedulerDNSMissingRecordResponse(request), nil
			}
			f.puts.Add(1)
		} else {
			id = fmt.Sprintf("record-created-%d", f.posts.Add(1))
			if f.injectDuplicateOnPost {
				f.injectDuplicateOnPost = false
				duplicateID := "record-race-" + strings.ToLower(payload.Type)
				content := payload.Content
				if f.injectedContent != "" {
					content = f.injectedContent
				}
				f.records[duplicateID] = cloudflareAddressRecord{ID: duplicateID, Type: payload.Type, Name: payload.Name, Content: content, Comment: payload.Comment}
				body := `{"success":false,"errors":[{"code":81058,"message":"An identical record already exists."}],"result":null}`
				return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
			}
		}
		record := cloudflareAddressRecord{ID: id, Type: payload.Type, Name: payload.Name, Content: payload.Content, Comment: payload.Comment}
		f.records[id] = record
		result = schedulerDNSRecordJSON(record)
	case http.MethodDelete:
		f.deletes.Add(1)
		delete(f.records, id)
		result = map[string]string{"id": id}
	default:
		return nil, fmt.Errorf("unexpected Cloudflare fixture method %s", request.Method)
	}
	payload, err := json.Marshal(map[string]any{"success": true, "result": result, "result_info": map[string]int{"page": 1, "total_pages": 1}})
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(payload))), Request: request}, nil
}

func schedulerDNSMissingRecordResponse(request *http.Request) *http.Response {
	body := `{"success":false,"errors":[{"code":81044,"message":"The record does not exist."}],"result":null}`
	return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}
}

func (f *schedulerDNSFixture) probe(ctx context.Context, node ControlNode, host string, secret []byte) error {
	f.probes.Add(1)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if node.GUID != f.node.GUID || host != f.host || len(secret) != 32 || !bytes.Equal(secret, f.probeSecret) {
		return errors.New("scheduler health probe lost node/host/authentication binding")
	}
	f.mu.Lock()
	f.probeIPs[node.Address]++
	f.mu.Unlock()
	if f.probeFails.Load() {
		return errors.New("fixture entry is unhealthy")
	}
	return nil
}

func (f *schedulerDNSFixture) reconcile(at time.Time) error {
	unlock := f.app.lockSiteSchedule(f.site.ID)
	defer unlock()
	schedule, err := f.app.db.siteNodeSchedule(f.site.ID)
	if err != nil {
		return err
	}
	return f.app.reconcileOneSiteScheduleWithProbeLocked(context.Background(), schedule, at, f.probe)
}

func TestSiteDNSPrimaryRecordFamilySwitchPromotesTrackedTargetFamily(t *testing.T) {
	f := newSchedulerDNSFixture(t, true)
	f.rejectDuplicates = true
	if _, err := f.app.db.db.Exec(`UPDATE control_nodes SET dns_publish='v6' WHERE id=?`, f.node.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(f.now.Add(15 * time.Second)); err != nil {
		t.Fatalf("switch the schedule primary record from A to its existing owned AAAA: %v", err)
	}
	schedule, err := f.app.db.siteNodeSchedule(f.site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.DNSStatus != "active" || schedule.cfRecordID != "record-aaaa" || schedule.cfRecordType != "AAAA" || schedule.AppliedAddress != "2001:db8::5" {
		t.Fatalf("promoted primary DNS record = id %q type %q address %q status %q", schedule.cfRecordID, schedule.cfRecordType, schedule.AppliedAddress, schedule.DNSStatus)
	}
	f.mu.Lock()
	_, oldIPv4Remains := f.records["record-a"]
	_, targetIPv6Remains := f.records["record-aaaa"]
	f.mu.Unlock()
	if oldIPv4Remains || !targetIPv6Remains {
		t.Fatalf("family switch left old A=%t, target AAAA=%t", oldIPv4Remains, targetIPv6Remains)
	}
	if f.puts.Load() != 0 || f.posts.Load() != 0 || f.deletes.Load() != 1 {
		t.Fatalf("family switch performed PUT/POST/DELETE=%d/%d/%d; expected adopt AAAA then delete old A", f.puts.Load(), f.posts.Load(), f.deletes.Load())
	}
	var remaining int
	if err := f.app.db.db.QueryRow(`SELECT COUNT(*) FROM site_node_dns_records WHERE site_id=?`, f.site.ID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("family-switch transition left %d secondary DNS ledger rows", remaining)
	}
}

func TestSiteDNSPrimaryRecordFamilySwitchCreatesTargetFamilyWhenMissing(t *testing.T) {
	f := newSchedulerDNSFixture(t, false)
	if _, err := f.app.db.db.Exec(`UPDATE control_nodes SET address_v6='2001:db8::5',dns_publish='v6' WHERE id=?`, f.node.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(f.now.Add(15 * time.Second)); err != nil {
		t.Fatalf("create the AAAA primary and retire the old A record: %v", err)
	}
	schedule, err := f.app.db.siteNodeSchedule(f.site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.DNSStatus != "active" || schedule.cfRecordType != "AAAA" || schedule.AppliedAddress != "2001:db8::5" {
		t.Fatalf("new-family primary record = type %q address %q status %q", schedule.cfRecordType, schedule.AppliedAddress, schedule.DNSStatus)
	}
	f.mu.Lock()
	_, oldIPv4Remains := f.records["record-a"]
	var ipv6Records int
	for _, record := range f.records {
		if record.Type == "AAAA" && strings.EqualFold(record.Name, f.host) && record.Content == "2001:db8::5" {
			ipv6Records++
		}
	}
	f.mu.Unlock()
	if oldIPv4Remains || ipv6Records != 1 {
		t.Fatalf("family switch left old A=%t, matching AAAA records=%d", oldIPv4Remains, ipv6Records)
	}
}

func TestSiteDNSSecondaryPostAdoptsConcurrentOwnedDuplicate(t *testing.T) {
	f := newSchedulerDNSFixture(t, false)
	f.injectDuplicateOnPost = true
	if _, err := f.app.db.db.Exec(`UPDATE control_nodes SET address_v6='2001:db8::5' WHERE id=?`, f.node.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(f.now.Add(15 * time.Second)); err != nil {
		t.Fatalf("adopt a concurrent Meridian-owned secondary AAAA record: %v", err)
	}
	schedule, err := f.app.db.siteNodeSchedule(f.site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.DNSStatus != "active" || schedule.cfRecordID != "record-a" || schedule.cfRecordType != "A" {
		t.Fatalf("primary record changed while adopting the secondary: %+v", schedule)
	}
	var id, recordType, address string
	if err := f.app.db.db.QueryRow(`SELECT record_id,record_type,address FROM site_node_dns_records WHERE site_id=? AND family='v6'`, f.site.ID).Scan(&id, &recordType, &address); err != nil {
		t.Fatal(err)
	}
	if id != "record-race-aaaa" || recordType != "AAAA" || address != "2001:db8::5" {
		t.Fatalf("secondary record ledger = %q/%q/%q", id, recordType, address)
	}
	if f.posts.Load() != 1 || f.puts.Load() != 0 || f.deletes.Load() != 0 {
		t.Fatalf("duplicate recovery performed POST/PUT/DELETE=%d/%d/%d", f.posts.Load(), f.puts.Load(), f.deletes.Load())
	}
}

func TestSiteDNSFamilySwitchDuplicateAdoptionKeepsRecordOnCASFailure(t *testing.T) {
	f := newSchedulerDNSFixture(t, false)
	f.injectDuplicateOnPost = true
	if _, err := f.app.db.db.Exec(`UPDATE control_nodes SET address_v6='2001:db8::5',dns_publish='v6' WHERE id=?`, f.node.ID); err != nil {
		t.Fatal(err)
	}
	var revisionBumped sync.Once
	f.onGet = func() {
		revisionBumped.Do(func() {
			if _, err := f.app.db.db.Exec(`UPDATE site_node_schedules SET schedule_revision=schedule_revision+1 WHERE site_id=?`, f.site.ID); err != nil {
				t.Errorf("advance schedule revision during Cloudflare reconciliation: %v", err)
			}
		})
	}
	err := f.reconcile(f.now.Add(15 * time.Second))
	if err == nil || !strings.Contains(err.Error(), "site schedule changed during DNS reconciliation") {
		t.Fatalf("family switch CAS failure = %v, want schedule-changed error", err)
	}
	f.mu.Lock()
	_, oldIPv4Remains := f.records["record-a"]
	_, adoptedIPv6Remains := f.records["record-race-aaaa"]
	f.mu.Unlock()
	if !oldIPv4Remains || !adoptedIPv6Remains {
		t.Fatalf("failed family-switch CAS removed an existing DNS record: old A=%t adopted AAAA=%t", oldIPv4Remains, adoptedIPv6Remains)
	}
}

func TestSiteDNSFamilySwitchRecoversStalePrimaryAndCleansOwnedOldFamily(t *testing.T) {
	for _, blank := range []bool{false, true} {
		t.Run(fmt.Sprintf("blank-primary=%t", blank), func(t *testing.T) {
			f := newSchedulerDNSFixture(t, true)
			f.mu.Lock()
			old := f.records["record-a"]
			delete(f.records, old.ID)
			old.ID = "record-a-replaced"
			f.records[old.ID] = old
			f.mu.Unlock()
			if _, err := f.app.db.db.Exec(`UPDATE control_nodes SET dns_publish='v6' WHERE id=?`, f.node.ID); err != nil {
				t.Fatal(err)
			}
			if blank {
				if _, err := f.app.db.db.Exec(`UPDATE site_node_schedules SET cf_record_id='' WHERE site_id=?`, f.site.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.reconcile(f.now.Add(15 * time.Second)); err != nil {
				t.Fatalf("recover target AAAA when old A tracking is stale: %v", err)
			}
			schedule, err := f.app.db.siteNodeSchedule(f.site.ID)
			if err != nil {
				t.Fatal(err)
			}
			if schedule.DNSStatus != "active" || schedule.cfRecordID != "record-aaaa" || schedule.cfRecordType != "AAAA" {
				t.Fatalf("recovered schedule primary=%q/%q status=%q", schedule.cfRecordID, schedule.cfRecordType, schedule.DNSStatus)
			}
			f.mu.Lock()
			_, oldFamilyRemains := f.records["record-a-replaced"]
			_, desiredFamilyRemains := f.records["record-aaaa"]
			f.mu.Unlock()
			if oldFamilyRemains || !desiredFamilyRemains {
				t.Fatalf("stale primary recovery failed to converge DNS families: old A=%t desired AAAA=%t", oldFamilyRemains, desiredFamilyRemains)
			}
			var remaining int
			if err := f.app.db.db.QueryRow(`SELECT COUNT(*) FROM site_node_dns_records WHERE site_id=?`, f.site.ID).Scan(&remaining); err != nil {
				t.Fatal(err)
			}
			if remaining != 0 {
				t.Fatalf("stale primary recovery left %d secondary ledger rows", remaining)
			}
		})
	}
}

func TestSiteDNSFamilySwitchFromIPv6ToIPv4OrAuto(t *testing.T) {
	for _, mode := range []string{"v4", "auto"} {
		for _, targetExists := range []bool{false, true} {
			t.Run(fmt.Sprintf("mode=%s/existing-target=%t", mode, targetExists), func(t *testing.T) {
				f := newSchedulerDNSFixture(t, true)
				f.rejectDuplicates = true
				if _, err := f.app.db.db.Exec(`UPDATE control_nodes SET dns_publish='v6' WHERE id=?`, f.node.ID); err != nil {
					t.Fatal(err)
				}
				if err := f.reconcile(f.now.Add(15 * time.Second)); err != nil {
					t.Fatalf("establish IPv6-only primary: %v", err)
				}
				if targetExists {
					f.mu.Lock()
					f.records["record-a-ready"] = cloudflareAddressRecord{ID: "record-a-ready", Type: "A", Name: f.host, Content: "203.0.113.5", Comment: siteDNSOwnershipMarker(f.site.ID, f.app.db.installUUID)}
					f.mu.Unlock()
				}
				if _, err := f.app.db.db.Exec(`UPDATE control_nodes SET dns_publish=? WHERE id=?`, mode, f.node.ID); err != nil {
					t.Fatal(err)
				}
				if err := f.reconcile(f.now.Add(30 * time.Second)); err != nil {
					t.Fatalf("switch IPv6 primary to %s: %v", mode, err)
				}
				schedule, err := f.app.db.siteNodeSchedule(f.site.ID)
				if err != nil {
					t.Fatal(err)
				}
				if schedule.DNSStatus != "active" || schedule.cfRecordType != "A" || schedule.AppliedAddress != "203.0.113.5" {
					t.Fatalf("reverse family switch primary=%q/%q status=%q", schedule.cfRecordType, schedule.AppliedAddress, schedule.DNSStatus)
				}
				f.mu.Lock()
				familyCounts := make(map[string]int)
				for _, record := range f.records {
					if strings.EqualFold(record.Name, f.host) {
						familyCounts[record.Type]++
					}
				}
				f.mu.Unlock()
				wantIPv6 := 0
				if mode == "auto" {
					wantIPv6 = 1
				}
				if familyCounts["A"] != 1 || familyCounts["AAAA"] != wantIPv6 {
					t.Fatalf("reverse family switch records=%v, want A=1 AAAA=%d", familyCounts, wantIPv6)
				}
				var secondaryCount int
				if err := f.app.db.db.QueryRow(`SELECT COUNT(*) FROM site_node_dns_records WHERE site_id=?`, f.site.ID).Scan(&secondaryCount); err != nil {
					t.Fatal(err)
				}
				if secondaryCount != wantIPv6 {
					t.Fatalf("reverse family switch secondary ledger count=%d, want %d", secondaryCount, wantIPv6)
				}
				if err := f.reconcile(f.now.Add(45 * time.Second)); err != nil {
					t.Fatalf("repeat stable reverse family switch: %v", err)
				}
			})
		}
	}
}

func TestSiteDNSDuplicateAdoptionCorrectsDesiredContent(t *testing.T) {
	for _, mode := range []string{"v6", "auto"} {
		t.Run(mode, func(t *testing.T) {
			f := newSchedulerDNSFixture(t, false)
			f.injectDuplicateOnPost = true
			f.injectedContent = "2001:db8::99"
			if _, err := f.app.db.db.Exec(`UPDATE control_nodes SET address_v6='2001:db8::5',dns_publish=? WHERE id=?`, mode, f.node.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.reconcile(f.now.Add(15 * time.Second)); err != nil {
				t.Fatalf("recover owned duplicate and reconcile desired content: %v", err)
			}
			f.mu.Lock()
			record, exists := f.records["record-race-aaaa"]
			f.mu.Unlock()
			if !exists || record.Content != "2001:db8::5" || record.Type != "AAAA" {
				t.Fatalf("adopted record was published without correcting desired content: exists=%t type=%q content=%q", exists, record.Type, record.Content)
			}
		})
	}
}

func TestSiteDNSPrimarySameFamilyDuplicateAdoptsOwnedTargetAndDeletesTrackedOldRecord(t *testing.T) {
	f := newSchedulerDNSFixture(t, true)
	f.rejectDuplicates = true
	marker := siteDNSOwnershipMarker(f.site.ID, f.app.db.installUUID)
	f.mu.Lock()
	f.records["record-a-target"] = cloudflareAddressRecord{ID: "record-a-target", Type: "A", Name: f.host, Content: "203.0.113.6", Comment: marker}
	f.mu.Unlock()
	if _, err := f.app.db.db.Exec(`UPDATE control_nodes SET address_v4='203.0.113.6' WHERE id=?`, f.node.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(f.now.Add(15 * time.Second)); err != nil {
		t.Fatalf("adopt an existing uniquely-owned same-family primary record: %v", err)
	}
	schedule, err := f.app.db.siteNodeSchedule(f.site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.DNSStatus != "active" || schedule.cfRecordID != "record-a-target" || schedule.cfRecordType != "A" || schedule.AppliedAddress != "203.0.113.6" {
		t.Fatalf("same-family duplicate adoption schedule=%q/%q/%q status=%q", schedule.cfRecordID, schedule.cfRecordType, schedule.AppliedAddress, schedule.DNSStatus)
	}
	f.mu.Lock()
	_, oldPrimaryRemains := f.records["record-a"]
	target := f.records["record-a-target"]
	_, secondaryRemains := f.records["record-aaaa"]
	f.mu.Unlock()
	if oldPrimaryRemains || target.Content != "203.0.113.6" || !secondaryRemains || f.deletes.Load() != 1 {
		t.Fatalf("same-family duplicate state old=%t target=%#v secondary=%t deletes=%d", oldPrimaryRemains, target, secondaryRemains, f.deletes.Load())
	}
}

func TestSiteDNSSecondarySameFamilyDuplicateAdoptsOwnedTarget(t *testing.T) {
	f := newSchedulerDNSFixture(t, true)
	f.rejectDuplicates = true
	marker := siteDNSOwnershipMarker(f.site.ID, f.app.db.installUUID)
	f.mu.Lock()
	f.records["record-aaaa-target"] = cloudflareAddressRecord{ID: "record-aaaa-target", Type: "AAAA", Name: f.host, Content: "2001:db8::6", Comment: marker}
	f.mu.Unlock()
	if _, err := f.app.db.db.Exec(`UPDATE control_nodes SET address_v6='2001:db8::6' WHERE id=?`, f.node.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(f.now.Add(15 * time.Second)); err != nil {
		t.Fatalf("adopt an existing uniquely-owned same-family secondary record: %v", err)
	}
	var id, address string
	if err := f.app.db.db.QueryRow(`SELECT record_id,address FROM site_node_dns_records WHERE site_id=? AND family='v6'`, f.site.ID).Scan(&id, &address); err != nil {
		t.Fatal(err)
	}
	if id != "record-aaaa-target" || address != "2001:db8::6" {
		t.Fatalf("secondary ledger = %q/%q, want target record and requested address", id, address)
	}
	f.mu.Lock()
	_, oldSecondaryRemains := f.records["record-aaaa"]
	target := f.records["record-aaaa-target"]
	f.mu.Unlock()
	if oldSecondaryRemains || target.Content != "2001:db8::6" || f.deletes.Load() != 1 {
		t.Fatalf("same-family secondary duplicate old=%t target=%#v deletes=%d", oldSecondaryRemains, target, f.deletes.Load())
	}
}

func TestSiteDNSSameFamilyDuplicateRefusesUnownedTarget(t *testing.T) {
	f := newSchedulerDNSFixture(t, true)
	f.rejectDuplicates = true
	f.mu.Lock()
	f.records["record-a-foreign-target"] = cloudflareAddressRecord{ID: "record-a-foreign-target", Type: "A", Name: f.host, Content: "203.0.113.6", Comment: "managed outside Meridian"}
	f.mu.Unlock()
	if _, err := f.app.db.db.Exec(`UPDATE control_nodes SET address_v4='203.0.113.6' WHERE id=?`, f.node.ID); err != nil {
		t.Fatal(err)
	}
	err := f.reconcile(f.now.Add(15 * time.Second))
	if err == nil || !strings.Contains(err.Error(), "untracked exact A record already contains the requested address") {
		t.Fatalf("unowned duplicate result=%v, want ownership refusal", err)
	}
	f.mu.Lock()
	_, oldPrimaryRemains := f.records["record-a"]
	_, foreignTargetRemains := f.records["record-a-foreign-target"]
	f.mu.Unlock()
	if !oldPrimaryRemains || !foreignTargetRemains || f.deletes.Load() != 0 {
		t.Fatalf("unowned duplicate changed DNS: tracked old=%t foreign target=%t deletes=%d", oldPrimaryRemains, foreignTargetRemains, f.deletes.Load())
	}
}

func TestSiteDNSMissingPrimaryAdoptionKeepsExistingRecordOnCASFailure(t *testing.T) {
	f := newSchedulerDNSFixture(t, true)
	f.mu.Lock()
	old := f.records["record-a"]
	delete(f.records, old.ID)
	old.ID = "record-a-replaced"
	f.records[old.ID] = old
	f.mu.Unlock()
	if _, err := f.app.db.db.Exec(`UPDATE control_nodes SET dns_publish='v6' WHERE id=?`, f.node.ID); err != nil {
		t.Fatal(err)
	}
	var revisionBumped sync.Once
	f.onGet = func() {
		revisionBumped.Do(func() {
			if _, err := f.app.db.db.Exec(`UPDATE site_node_schedules SET schedule_revision=schedule_revision+1 WHERE site_id=?`, f.site.ID); err != nil {
				t.Errorf("advance schedule while resolving stale primary: %v", err)
			}
		})
	}
	err := f.reconcile(f.now.Add(15 * time.Second))
	if err == nil || !strings.Contains(err.Error(), "site schedule changed during DNS reconciliation") {
		t.Fatalf("stale primary recovery CAS failure=%v, want schedule-changed error", err)
	}
	f.mu.Lock()
	_, oldFamilyRemains := f.records["record-a-replaced"]
	_, adoptedRecordRemains := f.records["record-aaaa"]
	f.mu.Unlock()
	if !oldFamilyRemains || !adoptedRecordRemains {
		t.Fatalf("stale primary recovery rollback removed existing records: old A=%t adopted AAAA=%t", oldFamilyRemains, adoptedRecordRemains)
	}
}

func TestSiteDNSMaintenanceSkipsStableWritesButProbesEveryRound(t *testing.T) {
	for _, dual := range []bool{false, true} {
		t.Run(fmt.Sprintf("dual=%t", dual), func(t *testing.T) {
			f := newSchedulerDNSFixture(t, dual)
			for round := range 3 {
				if err := f.reconcile(f.now.Add(time.Duration(round) * 15 * time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			families := int64(1)
			if dual {
				families = 2
			}
			if f.gets.Load() != families || f.puts.Load() != 0 || f.posts.Load() != 0 || f.probes.Load() != families*3 {
				t.Fatalf("steady maintenance GET/PUT/POST/probes=%d/%d/%d/%d", f.gets.Load(), f.puts.Load(), f.posts.Load(), f.probes.Load())
			}
			if err := f.reconcile(f.now.Add(5 * time.Minute)); err != nil {
				t.Fatal(err)
			}
			if f.gets.Load() != families*2 || f.puts.Load() != 0 || f.probes.Load() != families*4 {
				t.Fatalf("five-minute maintenance did not revalidate ownership: GET/PUT/probes=%d/%d/%d", f.gets.Load(), f.puts.Load(), f.probes.Load())
			}
		})
	}
}

func TestSiteDNSMaintenanceInvalidatesOnAddressConfigCredentialsAndRevision(t *testing.T) {
	for _, change := range []string{"address", "hash", "credentials", "schedule_revision", "node_revision", "target"} {
		t.Run(change, func(t *testing.T) {
			f := newSchedulerDNSFixture(t, false)
			if err := f.reconcile(f.now); err != nil {
				t.Fatal(err)
			}
			var err error
			switch change {
			case "address":
				_, err = f.app.db.db.Exec("UPDATE control_nodes SET address='203.0.113.6',address_v4='203.0.113.6' WHERE id=?", f.node.ID)
			case "hash":
				_, err = f.app.db.db.Exec("UPDATE control_nodes SET desired_config_hash='next-hash',applied_config_hash='next-hash' WHERE id=?", f.node.ID)
				if err == nil {
					_, err = f.app.db.db.Exec("UPDATE site_node_schedules SET config_hash='next-hash' WHERE site_id=?", f.site.ID)
				}
			case "credentials":
				f.credentials(t, 'b')
			case "schedule_revision":
				_, err = f.app.db.db.Exec("UPDATE site_node_schedules SET schedule_revision=schedule_revision+1 WHERE site_id=?", f.site.ID)
			case "node_revision":
				_, err = f.app.db.db.Exec("UPDATE control_nodes SET config_revision=config_revision+1 WHERE id=?", f.node.ID)
			case "target":
				_, err = f.app.db.db.Exec("UPDATE sites SET target_url='http://127.0.0.1:9096' WHERE id=?", f.site.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := f.reconcile(f.now.Add(15 * time.Second)); err != nil {
				t.Fatal(err)
			}
			wantWrites := int64(0)
			if change == "address" {
				wantWrites = 1
			}
			if f.gets.Load() != 2 || f.puts.Load() != wantWrites || f.probes.Load() != 2 {
				t.Fatalf("configuration change reused old maintenance: GET/PUT/probes=%d/%d/%d", f.gets.Load(), f.puts.Load(), f.probes.Load())
			}
			if err := f.reconcile(f.now.Add(30 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if f.gets.Load() != 2 || f.puts.Load() != wantWrites || f.probes.Load() != 3 {
				t.Fatal("new generation did not become stable after validation")
			}
		})
	}
}

func TestSiteDNSMaintenanceOwnershipAndUnhealthyProbeRemainFailClosed(t *testing.T) {
	for _, unhealthy := range []bool{false, true} {
		t.Run(fmt.Sprintf("probe_failure=%t", unhealthy), func(t *testing.T) {
			f := newSchedulerDNSFixture(t, false)
			if err := f.reconcile(f.now); err != nil {
				t.Fatal(err)
			}
			if unhealthy {
				f.probeFails.Store(true)
			} else {
				f.mu.Lock()
				record := f.records["record-a"]
				record.Comment, record.Content = "operator-owned", "203.0.113.99"
				f.records[record.ID] = record
				f.mu.Unlock()
			}
			checkAt := f.now.Add(5 * time.Minute)
			if unhealthy {
				checkAt = f.now.Add(15 * time.Second)
			}
			if err := f.reconcile(checkAt); err == nil {
				t.Fatal("readiness/ownership failure was hidden by maintenance cache")
			}
			if f.puts.Load() != 0 || f.posts.Load() != 0 {
				t.Fatal("readiness/ownership failure performed a DNS write")
			}
			if unhealthy {
				f.probeFails.Store(false)
				if err := f.reconcile(checkAt.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				if f.gets.Load() != 2 {
					t.Fatal("failed probe left a cached successful DNS maintenance state")
				}
			}
		})
	}
}

func TestSiteDNSMaintenanceRechecksSecondaryTrackingAndOwnership(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(fmt.Sprintf("owned=%t", owned), func(t *testing.T) {
			f := newSchedulerDNSFixture(t, true)
			if err := f.reconcile(f.now); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			record := f.records["record-aaaa"]
			delete(f.records, record.ID)
			record.ID = "record-aaaa-next"
			if !owned {
				record.Comment = "operator-owned"
			}
			f.records[record.ID] = record
			f.mu.Unlock()
			if _, err := f.app.db.db.Exec("UPDATE site_node_dns_records SET record_id=? WHERE site_id=? AND family='v6'", record.ID, f.site.ID); err != nil {
				t.Fatal(err)
			}
			err := f.reconcile(f.now.Add(15 * time.Second))
			if owned && err != nil || !owned && err == nil {
				t.Fatalf("secondary ownership/tracking decision=%v owned=%t", err, owned)
			}
			if f.gets.Load() != 4 || f.puts.Load() != 0 || f.posts.Load() != 0 || f.probes.Load() != 4 {
				t.Fatalf("secondary tracking reused stale validation or wrote equal/foreign DNS: GET/PUT/POST/probe=%d/%d/%d/%d", f.gets.Load(), f.puts.Load(), f.posts.Load(), f.probes.Load())
			}
		})
	}
}

func TestSiteDNSMaintenanceCASFailureRestoresPreimageAndKeepsNewRevision(t *testing.T) {
	f := newSchedulerDNSFixture(t, false)
	f.mu.Lock()
	record := f.records["record-a"]
	record.Content = "203.0.113.4"
	f.records[record.ID] = record
	var once sync.Once
	f.onGet = func() {
		once.Do(func() {
			if _, err := f.app.db.db.Exec("UPDATE site_node_schedules SET schedule_revision=schedule_revision+1 WHERE site_id=?", f.site.ID); err != nil {
				t.Error(err)
			}
		})
	}
	f.mu.Unlock()
	before, err := f.app.db.siteNodeSchedule(f.site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reconcile(f.now); err == nil || !strings.Contains(err.Error(), "schedule changed") {
		t.Fatal("revision race did not fail its schedule CAS", err)
	}
	after, err := f.app.db.siteNodeSchedule(f.site.ID)
	if err != nil || after.ScheduleRevision != before.ScheduleRevision+1 {
		t.Fatal("stale reconciliation overwrote the new revision", err)
	}
	f.mu.Lock()
	restored := f.records["record-a"]
	f.onGet = nil
	f.mu.Unlock()
	if restored.Content != "203.0.113.4" || restored.Comment != record.Comment || f.puts.Load() != 2 {
		t.Fatalf("failed CAS did not restore owned DNS preimage: content=%s PUT=%d", restored.Content, f.puts.Load())
	}
	if err := f.reconcile(f.now.Add(time.Second)); err != nil {
		t.Fatal("new revision could not reconcile after compensation", err)
	}
}

func TestSiteDNSMaintenanceConcurrentRoundsRetainPerSiteSerialization(t *testing.T) {
	f := newSchedulerDNSFixture(t, false)
	if err := f.reconcile(f.now); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			if err := f.reconcile(f.now.Add(15 * time.Second)); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	if f.gets.Load() != 1 || f.puts.Load() != 0 || f.probes.Load() != 9 {
		t.Fatalf("concurrent steady rounds amplified DNS writes: GET/PUT/probes=%d/%d/%d", f.gets.Load(), f.puts.Load(), f.probes.Load())
	}
}
