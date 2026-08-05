package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type dynamicIPResolverFunc func(context.Context, string) ([]net.IPAddr, error)

func (f dynamicIPResolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

func newTestDynamicSelfTargetPolicy(t *testing.T) *dynamicSelfTargetPolicy {
	t.Helper()
	policy, err := newDynamicSelfTargetPolicy("panel.example.com", 9090, nil, func() ([]net.Addr, error) {
		return nil, nil
	})
	if err != nil {
		t.Fatalf("create dynamic self-target policy: %v", err)
	}
	return policy
}

func TestDynamicFoundationMigrationDefaultsAndIdempotence(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-dynamic.db")
	createLegacySiteDatabase(t, dbPath, false)

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("migrate legacy database: %v", err)
	}
	defer db.Close()
	if err := db.migrate(); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}

	for _, column := range []string{
		"dynamic_discovery_enabled",
		"dynamic_profile",
		"dynamic_discovery_sources",
		"dynamic_domain_rules",
		"dynamic_allow_https_downgrade",
		"dynamic_policy_revision",
	} {
		var count int
		if err := db.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('sites') WHERE name=?", column).Scan(&count); err != nil {
			t.Fatalf("inspect %s: %v", column, err)
		}
		if count != 1 {
			t.Fatalf("column %s count=%d, want 1", column, count)
		}
	}

	site, err := db.GetSite(1)
	if err != nil {
		t.Fatalf("read migrated site: %v", err)
	}
	if site.DynamicDiscoveryEnabled || site.DynamicProfile != dynamicProfileSafe || site.DynamicAllowHTTPSDowngrade {
		t.Fatalf("migrated dynamic policy = %#v", site)
	}
	if site.DynamicPolicyRevision != 1 || len(site.DynamicDomainRules) != 0 || site.StoredDynamicDomainRules != "[]" {
		t.Fatalf("migrated dynamic rules/revision = %#v", site)
	}
	if len(site.DynamicDiscoverySources) != 1 || site.DynamicDiscoverySources[0] != dynamicDiscoverySourceRedirect || site.StoredDynamicDiscoverySources != `["redirect"]` {
		t.Fatalf("migrated dynamic discovery sources = %#v", site)
	}
	site.Name = "manual revision attempt"
	site.DynamicPolicyRevision = 99
	if err := db.UpdateSiteRecord(*site); err != nil {
		t.Fatalf("manual-only update: %v", err)
	}
	site, err = db.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if site.DynamicPolicyRevision != 1 {
		t.Fatalf("client revision was persisted: %d", site.DynamicPolicyRevision)
	}
	site.DynamicAllowHTTPSDowngrade = true
	site.DynamicPolicyRevision = 500
	if err := db.UpdateSiteRecord(*site); err != nil {
		t.Fatalf("dynamic policy update: %v", err)
	}
	site, err = db.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !site.DynamicAllowHTTPSDowngrade || site.DynamicPolicyRevision != 2 {
		t.Fatalf("dynamic policy revision = %#v", site)
	}
}

func TestStoredDynamicPolicyCorruptionFailsStartupAndReopen(t *testing.T) {
	corruptions := []struct {
		name   string
		column string
		value  interface{}
	}{
		{name: "blank profile", column: "dynamic_profile", value: ""},
		{name: "noncanonical profile", column: "dynamic_profile", value: "SAFE"},
		{name: "blank sources", column: "dynamic_discovery_sources", value: " \t"},
		{name: "null sources", column: "dynamic_discovery_sources", value: "null"},
		{name: "non-array sources", column: "dynamic_discovery_sources", value: `{}`},
		{name: "unknown source", column: "dynamic_discovery_sources", value: `["redirect","html"]`},
		{name: "duplicate source", column: "dynamic_discovery_sources", value: `["hls","hls"]`},
		{name: "noncanonical source order", column: "dynamic_discovery_sources", value: `["hls","redirect"]`},
		{name: "blank rules", column: "dynamic_domain_rules", value: " \t"},
		{name: "null rules", column: "dynamic_domain_rules", value: "null"},
		{name: "non-array rules", column: "dynamic_domain_rules", value: `{}`},
		{name: "noncanonical rules", column: "dynamic_domain_rules", value: `[{"type":"EXACT","value":"CDN.Example.COM."}]`},
		{name: "enabled discovery", column: "dynamic_discovery_enabled", value: 1},
		{name: "invalid discovery boolean", column: "dynamic_discovery_enabled", value: 2},
		{name: "invalid downgrade boolean", column: "dynamic_allow_https_downgrade", value: -1},
		{name: "revision below one", column: "dynamic_policy_revision", value: 0},
	}
	for _, tc := range corruptions {
		t.Run(tc.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "corrupt-dynamic.db")
			db, err := openDB(dbPath)
			if err != nil {
				t.Fatalf("create database: %v", err)
			}
			site, err := db.CreateSite("corrupt-dynamic", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
			if err != nil {
				db.Close()
				t.Fatalf("create site: %v", err)
			}
			query := fmt.Sprintf("UPDATE sites SET %s=?, enabled=0 WHERE id=?", tc.column)
			if _, err := db.db.Exec(query, tc.value, site.ID); err != nil {
				db.Close()
				t.Fatalf("inject corruption: %v", err)
			}
			if _, err := db.GetSite(site.ID); err == nil {
				db.Close()
				t.Fatal("GetSite accepted corrupt stored dynamic policy")
			}
			pm := NewProxyManager(db, nil)
			if _, err := pm.StartAllEnabled(); err == nil {
				db.Close()
				t.Fatal("startup accepted corrupt stored dynamic policy")
			}
			db.Close()

			reopened, err := openDB(dbPath)
			if err == nil {
				reopened.Close()
				t.Fatal("database reopen accepted corrupt stored dynamic policy")
			}
		})
	}
}

func dynamicSiteUpdatePayload(t *testing.T, site *Site, overrides map[string]interface{}) []byte {
	t.Helper()
	payload := map[string]interface{}{
		"name":         site.Name,
		"listen_port":  site.ListenPort,
		"public_host":  site.PublicHost,
		"ingress_mode": site.IngressMode,
		"target_url":   site.TargetURL,
	}
	for key, value := range overrides {
		payload[key] = value
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal update payload: %v", err)
	}
	return raw
}

func TestDynamicSiteAPIStrictRoundTripAndRevision(t *testing.T) {
	app := newTestApp(t)
	for name, dynamicFields := range map[string]map[string]interface{}{
		"server revision":      {"dynamic_policy_revision": 1},
		"enabled discovery":    {"dynamic_discovery_enabled": true},
		"safe public exact IP": {"dynamic_domain_rules": []map[string]string{{"type": "exact", "value": "8.8.8.8"}}},
	} {
		t.Run("reject POST "+name, func(t *testing.T) {
			payload := map[string]interface{}{
				"name":        "rejected-" + name,
				"listen_port": freePort(t),
				"target_url":  "http://127.0.0.1:8096",
				"ua_mode":     "infuse",
			}
			for key, value := range dynamicFields {
				payload[key] = value
			}
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			rr := httptest.NewRecorder()
			app.handleSites(rr, httptest.NewRequest(http.MethodPost, "/api/sites", bytes.NewReader(raw)))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s, want 400", rr.Code, rr.Body.String())
			}
		})
	}
	if sites, err := app.db.ListSites(); err != nil || len(sites) != 0 {
		t.Fatalf("rejected POST persisted sites=%#v err=%v", sites, err)
	}
	createPayload, err := json.Marshal(map[string]interface{}{
		"name":                          "dynamic-policy",
		"listen_port":                   freePort(t),
		"public_host":                   "dynamic-policy.example.com",
		"ingress_mode":                  ingressModeHost,
		"target_url":                    "http://127.0.0.1:8096",
		"ua_mode":                       "infuse",
		"dynamic_discovery_enabled":     false,
		"dynamic_profile":               " COMPATIBLE ",
		"dynamic_domain_rules":          []map[string]string{{"type": "suffix", "value": "BÜCHER.DE."}, {"type": "exact", "value": "CDN.Example.COM."}},
		"dynamic_allow_https_downgrade": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	createdResponse := httptest.NewRecorder()
	app.handleSites(createdResponse, httptest.NewRequest(http.MethodPost, "/api/sites", bytes.NewReader(createPayload)))
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	var site Site
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &site); err != nil {
		t.Fatalf("decode created site: %v", err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	if site.DynamicDiscoveryEnabled || site.DynamicProfile != dynamicProfileCompatible || !site.DynamicAllowHTTPSDowngrade || site.DynamicPolicyRevision != 1 {
		t.Fatalf("created dynamic policy = %#v", site)
	}
	if len(site.DynamicDomainRules) != 2 || site.DynamicDomainRules[0] != (DynamicDomainRule{Type: "exact", Value: "cdn.example.com"}) || site.DynamicDomainRules[1] != (DynamicDomainRule{Type: "suffix", Value: "xn--bcher-kva.de"}) {
		t.Fatalf("created normalized rules = %#v", site.DynamicDomainRules)
	}
	if got := strings.Join(site.DynamicDiscoverySources, ","); got != strings.Join(allDynamicDiscoverySources(), ",") {
		t.Fatalf("created discovery sources = %#v, want %#v", site.DynamicDiscoverySources, allDynamicDiscoverySources())
	}

	update := func(overrides map[string]interface{}, wantStatus int) *httptest.ResponseRecorder {
		t.Helper()
		payload := dynamicSiteUpdatePayload(t, &site, overrides)
		rr := httptest.NewRecorder()
		app.handleSiteByID(rr, httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/sites/%d", site.ID), bytes.NewReader(payload)))
		if rr.Code != wantStatus {
			t.Fatalf("update status=%d body=%s, want %d", rr.Code, rr.Body.String(), wantStatus)
		}
		if wantStatus == http.StatusOK {
			if err := json.Unmarshal(rr.Body.Bytes(), &site); err != nil {
				t.Fatalf("decode updated site: %v", err)
			}
		}
		return rr
	}

	update(map[string]interface{}{"name": "manual-only-change"}, http.StatusOK)
	if site.DynamicPolicyRevision != 1 {
		t.Fatalf("manual update revision=%d, want 1", site.DynamicPolicyRevision)
	}
	update(map[string]interface{}{"dynamic_profile": "SAFE", "dynamic_discovery_sources": []string{"redirect", "playback_info"}}, http.StatusOK)
	if site.DynamicProfile != dynamicProfileSafe || site.DynamicPolicyRevision != 2 {
		t.Fatalf("profile update = %q revision=%d", site.DynamicProfile, site.DynamicPolicyRevision)
	}
	update(map[string]interface{}{
		"dynamic_domain_rules": []map[string]string{{"type": "SUFFIX", "value": "bücher.de"}, {"type": "exact", "value": "cdn.example.com"}},
	}, http.StatusOK)
	if site.DynamicPolicyRevision != 2 {
		t.Fatalf("normalized no-op rule update revision=%d, want 2", site.DynamicPolicyRevision)
	}

	update(map[string]interface{}{"dynamic_discovery_enabled": true}, http.StatusBadRequest)
	reloaded, err := app.db.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.DynamicDiscoveryEnabled || reloaded.DynamicPolicyRevision != 2 {
		t.Fatalf("rejected enable changed policy = %#v", reloaded)
	}
	update(map[string]interface{}{"dynamic_policy_revision": 99}, http.StatusBadRequest)
	update(map[string]interface{}{
		"dynamic_domain_rules": []map[string]interface{}{{"type": "exact", "value": "cdn.example.com", "wildcard": true}},
	}, http.StatusBadRequest)
	beforeRejectedIP, err := app.db.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	update(map[string]interface{}{
		"dynamic_domain_rules": []map[string]string{{"type": "exact", "value": "8.8.8.8"}},
	}, http.StatusBadRequest)
	afterRejectedIP, err := app.db.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterRejectedIP.DynamicProfile != beforeRejectedIP.DynamicProfile || afterRejectedIP.StoredDynamicDiscoverySources != beforeRejectedIP.StoredDynamicDiscoverySources || afterRejectedIP.StoredDynamicDomainRules != beforeRejectedIP.StoredDynamicDomainRules || afterRejectedIP.DynamicAllowHTTPSDowngrade != beforeRejectedIP.DynamicAllowHTTPSDowngrade || afterRejectedIP.DynamicPolicyRevision != beforeRejectedIP.DynamicPolicyRevision {
		t.Fatalf("rejected safe IP PUT changed stored policy: before=%#v after=%#v", beforeRejectedIP, afterRejectedIP)
	}

	update(map[string]interface{}{"dynamic_discovery_sources": []string{"DASH", "hls"}}, http.StatusBadRequest)
	update(map[string]interface{}{"dynamic_profile": "compatible", "dynamic_discovery_sources": []string{"DASH", "hls"}}, http.StatusOK)
	if got := strings.Join(site.DynamicDiscoverySources, ","); got != "hls,dash" || site.DynamicPolicyRevision != 3 {
		t.Fatalf("sources update = %#v revision=%d, want [hls dash] revision=3", site.DynamicDiscoverySources, site.DynamicPolicyRevision)
	}
	update(map[string]interface{}{"dynamic_discovery_sources": []string{"redirect", "REDIRECT"}}, http.StatusBadRequest)
	update(map[string]interface{}{"dynamic_discovery_sources": []string{"redirect", "unknown"}}, http.StatusBadRequest)
	update(map[string]interface{}{"dynamic_discovery_sources": nil}, http.StatusBadRequest)
	afterRejectedSources, err := app.db.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(afterRejectedSources.DynamicDiscoverySources, ","); got != "hls,dash" || afterRejectedSources.DynamicPolicyRevision != 3 {
		t.Fatalf("rejected sources PUT changed stored policy: %#v", afterRejectedSources)
	}
}

func TestDynamicProfilesEndpointIsAuthenticatedAndDoesNotDiscloseKey(t *testing.T) {
	app := newTestApp(t)
	secret := strings.Repeat("dynamic-route-secret-", 2)
	key, err := resolveDynamicRouteKey(secret)
	if err != nil {
		t.Fatal(err)
	}
	app.dynamicRouteKey = key
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dynamic-profiles", app.authMiddleware(app.handleDynamicProfiles))

	unauthenticated := httptest.NewRecorder()
	mux.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/api/dynamic-profiles", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d, want 401", unauthenticated.Code)
	}

	token, err := generateToken(1, "admin")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/dynamic-profiles", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("authenticated status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) || strings.Contains(response.Body.String(), fmt.Sprintf("%x", key)) {
		t.Fatalf("dynamic profile response disclosed key material: %s", response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control=%q, want no-store", response.Header().Get("Cache-Control"))
	}

	var body DynamicProfilesResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Stage != "structured-discovery" || !body.Available || !body.KeyConfigured || len(body.Profiles) != 3 {
		t.Fatalf("dynamic capability envelope = %#v", body)
	}
	if body.GlobalLimits != dynamicGlobalLimits() {
		t.Fatalf("global limits = %#v, want %#v", body.GlobalLimits, dynamicGlobalLimits())
	}

	expected := map[string]struct {
		schemes    string
		ports      string
		anyPort    bool
		redirects  int
		authority  int
		capability int
		urls       int
		bodyBytes  int64
		dnsIPs     int
		newPerMin  int
		streams    int
		idle       int64
		lifetime   int64
	}{
		"safe":       {"https", "443", false, 3, 256, 4096, 256, 4 << 20, 16, 60, 32, 30 * 60, 8 * 60 * 60},
		"compatible": {"http,https", "", true, 5, 1024, 16384, 1024, 16 << 20, 32, 300, 128, 2 * 60 * 60, 24 * 60 * 60},
		"extreme":    {"http,https", "", true, 10, 4096, 65536, 4096, 64 << 20, 64, 1200, 512, 24 * 60 * 60, 7 * 24 * 60 * 60},
	}
	for _, profile := range body.Profiles {
		want, ok := expected[profile.ID]
		if !ok {
			t.Fatalf("unexpected profile %#v", profile)
		}
		limits := profile.Limits
		ports := make([]string, len(limits.AllowedPorts))
		for i, port := range limits.AllowedPorts {
			ports[i] = fmt.Sprint(port)
		}
		if strings.Join(limits.AllowedSchemes, ",") != want.schemes || strings.Join(ports, ",") != want.ports || limits.AllowAnyPort != want.anyPort || limits.MaxRedirects != want.redirects || limits.MaxAuthorities != want.authority || limits.MaxActiveCapabilities != want.capability || limits.MaxURLsPerResponse != want.urls || limits.MaxBodyBytes != want.bodyBytes || limits.MaxDNSIPs != want.dnsIPs || limits.MaxNewAuthoritiesPerMinute != want.newPerMin || limits.MaxStreams != want.streams || limits.IdleExpirySeconds != want.idle || limits.AbsoluteLifetimeSeconds != want.lifetime {
			t.Fatalf("profile %s limits = %#v", profile.ID, limits)
		}
		wantFeatures := DynamicFeatureFlags{RedirectDiscovery: true, PlaybackInfo: true, HLS: profile.ID != dynamicProfileSafe, DASH: profile.ID != dynamicProfileSafe}
		if profile.Features != wantFeatures {
			t.Fatalf("profile %s feature flags = %#v, want %#v", profile.ID, profile.Features, wantFeatures)
		}
	}
}

func TestResolveDynamicRouteKeyOptionalAndStrict(t *testing.T) {
	if key, err := resolveDynamicRouteKey(""); err != nil || key != nil {
		t.Fatalf("missing key = %x, %v", key, err)
	}
	valid := strings.Repeat("k", 32)
	if key, err := resolveDynamicRouteKey(valid); err != nil || len(key) != 32 {
		t.Fatalf("valid key len=%d err=%v", len(key), err)
	}
	for _, invalid := range []string{strings.Repeat("k", 31), valid + " ", strings.Repeat("k", 16) + "\u00a0" + strings.Repeat("k", 16)} {
		if _, err := resolveDynamicRouteKey(invalid); err == nil {
			t.Fatalf("invalid key %q was accepted", invalid)
		}
	}
}

func TestDynamicRouteKeyRejectsReuseWithEffectiveSecrets(t *testing.T) {
	shared := strings.Repeat("s", 32)
	distinctJWT, _, err := resolveJWTSecret(strings.Repeat("j", 32))
	if err != nil {
		t.Fatal(err)
	}
	sharedJWT, _, err := resolveJWTSecret(shared)
	if err != nil {
		t.Fatal(err)
	}
	sharedDynamic, err := resolveDynamicRouteKey(shared)
	if err != nil {
		t.Fatal(err)
	}
	distinctDynamic, err := resolveDynamicRouteKey(strings.Repeat("d", 32))
	if err != nil {
		t.Fatal(err)
	}
	sharedUpstream, err := resolveUpstreamHeaderKey(shared)
	if err != nil {
		t.Fatal(err)
	}
	distinctUpstream, err := resolveUpstreamHeaderKey(strings.Repeat("u", 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDynamicRouteKeySeparation(sharedDynamic, sharedJWT, distinctUpstream); err == nil || !strings.Contains(err.Error(), "JWT_SECRET") {
		t.Fatalf("JWT_SECRET reuse error=%v", err)
	}
	if err := validateDynamicRouteKeySeparation(sharedDynamic, distinctJWT, sharedUpstream); err == nil || !strings.Contains(err.Error(), "UPSTREAM_HEADER_KEY") {
		t.Fatalf("UPSTREAM_HEADER_KEY reuse error=%v", err)
	}
	if err := validateDynamicRouteKeySeparation(distinctDynamic, distinctJWT, distinctUpstream); err != nil {
		t.Fatalf("distinct effective keys rejected: %v", err)
	}
}

func TestNormalizeDynamicURLAndDomainRules(t *testing.T) {
	target, err := normalizeDynamicURL("https://BÜCHER.de/media?q=1")
	if err != nil {
		t.Fatalf("normalize IDNA URL: %v", err)
	}
	if target.String() != "https://xn--bcher-kva.de:443/media?q=1" {
		t.Fatalf("normalized URL=%q", target.String())
	}
	httpTarget, err := normalizeDynamicURL("http://Example.COM:080/path")
	if err != nil || httpTarget.Host != "example.com:80" {
		t.Fatalf("canonical HTTP target=%v err=%v", httpTarget, err)
	}
	escapedTarget, err := normalizeDynamicURL("https://cdn.example.com/media/My%20Movie.mkv?label=Director%20Cut&signature=a+b")
	if err != nil || escapedTarget.String() != "https://cdn.example.com:443/media/My%20Movie.mkv?label=Director%20Cut&signature=a+b" {
		t.Fatalf("encoded-space target=%v err=%v", escapedTarget, err)
	}

	invalidURLs := []string{
		"ftp://example.com/file",
		`https://example.com\@evil.example/`,
		"https://user:secret@example.com/",
		"https://example.com/#fragment",
		"https://example.com/\nnext",
		"https://example.com/%0a",
		"https://example.com/%09",
		"https://example.com/%00",
		"https://example.com/%5cpath",
		"https://example.com/?q=%0d%0a",
		"https://[fe80::1%25eth0]/",
		"https://example.com:70000/",
		"https://com/",
		"http://127.0.0.1/",
		"http://[::ffff:127.0.0.1]/",
		"https://[2620:4f:8000::1]/",
		"https://example.com/" + strings.Repeat("a", maxDynamicTargetURLBytes),
	}
	for _, raw := range invalidURLs {
		if _, err := normalizeDynamicURL(raw); err == nil {
			t.Errorf("invalid dynamic URL %q was accepted", raw)
		}
	}

	rules, err := normalizeDynamicDomainRules(dynamicProfileSafe, []DynamicDomainRule{
		{Type: "suffix", Value: "BÜCHER.DE."},
		{Type: "EXACT", Value: "CDN.Example.COM."},
	})
	if err != nil {
		t.Fatalf("normalize rules: %v", err)
	}
	if len(rules) != 2 || rules[0] != (DynamicDomainRule{Type: "exact", Value: "cdn.example.com"}) || rules[1] != (DynamicDomainRule{Type: "suffix", Value: "xn--bcher-kva.de"}) {
		t.Fatalf("normalized rules=%#v", rules)
	}
	compatibleIPRules, err := normalizeDynamicDomainRules(dynamicProfileCompatible, []DynamicDomainRule{{Type: "exact", Value: "8.8.8.8"}})
	if err != nil || len(compatibleIPRules) != 1 || compatibleIPRules[0].Value != "8.8.8.8" {
		t.Fatalf("compatible exact IP rule=%#v err=%v", compatibleIPRules, err)
	}
	if !dynamicDomainRuleMatches("video.bücher.de", rules) || !dynamicDomainRuleMatches("cdn.example.com", rules) {
		t.Fatalf("normalized rules did not match allowed hosts: %#v", rules)
	}
	if dynamicDomainRuleMatches("notbücher.de", rules) || dynamicDomainRuleMatches("cdn.example.com.evil.net", rules) {
		t.Fatalf("dynamic rules crossed a DNS label boundary: %#v", rules)
	}

	invalidRules := [][]DynamicDomainRule{
		{{Type: "wildcard", Value: "example.com"}},
		{{Type: "suffix", Value: ".example.com"}},
		{{Type: "suffix", Value: "*.example.com"}},
		{{Type: "suffix", Value: "com"}},
		{{Type: "suffix", Value: "8.8.8.8"}},
		{{Type: "exact", Value: "127.0.0.1"}},
		{{Type: "exact", Value: "8.8.8.8"}},
		{{Type: "exact", Value: "example.com"}, {Type: "EXACT", Value: "EXAMPLE.COM."}},
	}
	for _, candidate := range invalidRules {
		if _, err := normalizeDynamicDomainRules(dynamicProfileSafe, candidate); err == nil {
			t.Errorf("invalid dynamic rules %#v were accepted", candidate)
		}
	}
}
func TestDynamicDiscoverySourcesFollowProfileContract(t *testing.T) {
	for _, tc := range []struct {
		profile string
		want    string
	}{
		{profile: dynamicProfileSafe, want: "redirect,playback_info"},
		{profile: dynamicProfileCompatible, want: "redirect,playback_info,hls,dash"},
		{profile: dynamicProfileExtreme, want: "redirect,playback_info,hls,dash"},
	} {
		site := Site{DynamicProfile: tc.profile}
		if err := normalizeDynamicSitePolicy(&site); err != nil {
			t.Fatalf("normalize %s defaults: %v", tc.profile, err)
		}
		if got := strings.Join(site.DynamicDiscoverySources, ","); got != tc.want {
			t.Fatalf("%s default sources=%q, want %q", tc.profile, got, tc.want)
		}
	}

	safeWithManifest := Site{DynamicProfile: dynamicProfileSafe, DynamicDiscoverySources: []string{dynamicDiscoverySourceHLS}}
	if err := normalizeDynamicSitePolicy(&safeWithManifest); err == nil {
		t.Fatal("Safe profile accepted an HLS source")
	}
}

func TestValidateDynamicResolvedIPsRejectsSpecialAndMixedAnswers(t *testing.T) {
	blocked := []string{
		"0.0.0.1",
		"10.0.0.1",
		"100.64.0.1",
		"127.0.0.1",
		"169.254.169.254",
		"172.16.0.1",
		"192.0.2.1",
		"192.168.0.1",
		"198.18.0.1",
		"198.51.100.1",
		"203.0.113.1",
		"224.0.0.1",
		"255.255.255.255",
		"::",
		"::1",
		"::ffff:127.0.0.1",
		"2001:2::1",
		"2001:db8::1",
		"2002::1",
		"fc00::1",
		"fe80::1",
		"ff02::1",
		"2620:4f:8000::1",
	}
	for _, raw := range blocked {
		if _, err := validateDynamicResolvedIPs([]net.IP{net.ParseIP(raw)}); err == nil {
			t.Errorf("special address %s was accepted", raw)
		}
	}

	public := []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2606:4700:4700::1111")}
	validated, err := validateDynamicResolvedIPs(public)
	if err != nil || len(validated) != 2 {
		t.Fatalf("public answers=%v err=%v", validated, err)
	}
	if _, err := validateDynamicResolvedIPs([]net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("127.0.0.1")}); err == nil {
		t.Fatal("mixed public/private DNS answers were accepted")
	}
	if _, err := validateDynamicResolvedIPs([]net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2620:4f:8000::1")}); err == nil {
		t.Fatal("mixed DNS answers containing the IPv6 AS112 prefix were accepted")
	}
	target, err := normalizeDynamicURL("https://cdn.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	policy := newTestDynamicSelfTargetPolicy(t)
	resolver := dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}, {IP: net.ParseIP("2620:4f:8000::1")}}, nil
	})
	if _, err := resolveDynamicURLIPs(context.Background(), resolver, target, 16, policy); err == nil {
		t.Fatal("mixed DNS resolution containing the IPv6 AS112 prefix was accepted")
	}
	if _, err := validateDynamicResolvedIPs(nil); err == nil {
		t.Fatal("empty DNS answer was accepted")
	}
}

func TestNewDynamicTransportPinsAddressesAndHasNoProxy(t *testing.T) {
	target, err := normalizeDynamicURL("https://example.com/video")
	if err != nil {
		t.Fatal(err)
	}
	policy := newTestDynamicSelfTargetPolicy(t)
	pins := []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("8.8.8.8")}
	var dialed []string
	dialErr := errors.New("recorded dial")
	transport, err := newDynamicTransportWithDialer(target, pins, func(_ context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" {
			t.Fatalf("network=%q, want tcp", network)
		}
		dialed = append(dialed, address)
		return nil, dialErr
	}, policy)
	if err != nil {
		t.Fatalf("new transport: %v", err)
	}
	if transport.Proxy != nil {
		t.Fatal("dynamic transport inherited an environment proxy")
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.MinVersion < tls.VersionTLS12 || transport.TLSClientConfig.ServerName != "example.com" {
		t.Fatalf("TLS config=%#v", transport.TLSClientConfig)
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "example.com:443"); !errors.Is(err, dialErr) {
		t.Fatalf("pinned dial error=%v, want wrapped recorder error", err)
	}
	if strings.Join(dialed, ",") != "1.1.1.1:443,8.8.8.8:443" {
		t.Fatalf("dialed addresses=%v", dialed)
	}
	before := len(dialed)
	if _, err := transport.DialContext(context.Background(), "tcp", "other.example.com:443"); err == nil {
		t.Fatal("dynamic transport dialed a cross-authority host")
	}
	if len(dialed) != before {
		t.Fatalf("cross-authority request reached dialer: %v", dialed)
	}

	second, err := newDynamicTransport(target, pins, policy)
	if err != nil {
		t.Fatal(err)
	}
	if second == transport {
		t.Fatal("dynamic transport constructor reused a cross-policy pool")
	}

	literal, err := normalizeDynamicURL("https://1.1.1.1/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newDynamicTransport(literal, []net.IP{net.ParseIP("8.8.8.8")}, policy); err == nil {
		t.Fatal("literal target accepted a different pinned IP")
	}
}

func TestNewDynamicTransportUsesOneTotalPinnedDialDeadline(t *testing.T) {
	target, err := normalizeDynamicURL("https://example.com/video")
	if err != nil {
		t.Fatal(err)
	}
	policy := newTestDynamicSelfTargetPolicy(t)
	pins := []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("8.8.8.8"), net.ParseIP("9.9.9.9")}
	const totalTimeout = 25 * time.Millisecond
	calls := 0
	transport, err := newDynamicTransportWithDialerTimeout(target, pins, func(ctx context.Context, _, _ string) (net.Conn, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > totalTimeout || time.Until(deadline) <= 0 {
			return nil, fmt.Errorf("dial context has invalid total deadline")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}, policy, totalTimeout)
	if err != nil {
		t.Fatalf("new bounded transport: %v", err)
	}
	started := time.Now()
	if _, err := transport.DialContext(context.Background(), "tcp", "example.com:443"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded pinned dial error=%v, want deadline exceeded", err)
	}
	elapsed := time.Since(started)
	if calls != 1 {
		t.Fatalf("bounded pinned dial made %d sequential attempts after total deadline, want 1", calls)
	}
	if elapsed < totalTimeout/2 || elapsed > time.Second {
		t.Fatalf("bounded pinned dial elapsed=%s, want one %s total window", elapsed, totalTimeout)
	}
}

func TestDynamicSelfTargetPolicyRejectsAuthoritiesAndLocalInterfaces(t *testing.T) {
	app := newTestApp(t)
	app.panelHost = "panel.example.com"
	app.panelListenPort = freePort(t)
	hostSitePort := freePort(t)
	if _, err := app.db.CreateSiteRecord(Site{
		Name:         "host-self-target",
		ListenPort:   hostSitePort,
		PublicHost:   "media.example.com",
		IngressMode:  ingressModeHost,
		TargetURL:    "http://127.0.0.1:8096",
		PlaybackMode: "direct",
		UAMode:       "infuse",
	}); err != nil {
		t.Fatalf("create host site: %v", err)
	}

	currentLocalIP := net.ParseIP("8.8.4.4")
	interfaceSnapshots := 0
	policy, err := app.snapshotDynamicSelfTargetPolicy(func() ([]net.Addr, error) {
		interfaceSnapshots++
		return []net.Addr{&net.IPNet{IP: currentLocalIP, Mask: net.CIDRMask(24, 32)}}, nil
	})
	if err != nil {
		t.Fatalf("snapshot self-target policy: %v", err)
	}
	lookups := 0
	resolver := dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		lookups++
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
	})
	preResolutionTargets := []string{
		"https://panel.example.com/",
		"https://media.example.com/",
	}
	for _, raw := range preResolutionTargets {
		target, err := normalizeDynamicURL(raw)
		if err != nil {
			t.Fatalf("normalize %s: %v", raw, err)
		}
		before := lookups
		if _, err := resolveDynamicURLIPs(context.Background(), resolver, target, 16, policy); !errors.Is(err, errDynamicSelfTarget) {
			t.Errorf("self target %s error=%v", raw, err)
		}
		if lookups != before {
			t.Errorf("self target %s reached DNS before rejection", raw)
		}
	}

	localResolver := dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		lookups++
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}, {IP: net.ParseIP("8.8.4.4")}}, nil
	})
	publicAlias, err := normalizeDynamicURL("https://public-alias.example.net/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveDynamicURLIPs(context.Background(), localResolver, publicAlias, 16, policy); !errors.Is(err, errDynamicSelfTarget) {
		t.Fatalf("public local-interface DNS answer error=%v", err)
	}
	literalLocal, err := normalizeDynamicURL("https://8.8.4.4/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolveDynamicURLIPs(context.Background(), resolver, literalLocal, 16, policy); !errors.Is(err, errDynamicSelfTarget) {
		t.Fatalf("public local-interface literal error=%v", err)
	}
	if _, err := newDynamicTransport(publicAlias, []net.IP{net.ParseIP("8.8.4.4")}, policy); !errors.Is(err, errDynamicSelfTarget) {
		t.Fatalf("transport accepted public local-interface pin: %v", err)
	}

	currentLocalIP = net.ParseIP("9.9.9.9")
	dialErr := errors.New("dial must not reach network")
	dials := 0
	beforeConstructionSnapshots := interfaceSnapshots
	transport, err := newDynamicTransportWithDialer(publicAlias, []net.IP{net.ParseIP("1.1.1.1")}, func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, dialErr
	}, policy)
	if err != nil {
		t.Fatalf("construct transport before interface change: %v", err)
	}
	if interfaceSnapshots <= beforeConstructionSnapshots {
		t.Fatal("transport construction did not refresh local interfaces")
	}
	beforeDialSnapshots := interfaceSnapshots
	currentLocalIP = net.ParseIP("1.1.1.1")
	if _, err := transport.DialContext(context.Background(), "tcp", "public-alias.example.net:443"); !errors.Is(err, errDynamicSelfTarget) {
		t.Fatalf("dial accepted newly assigned local-interface pin: %v", err)
	}
	if dials != 0 || interfaceSnapshots <= beforeDialSnapshots {
		t.Fatalf("dials=%d interface snapshots before/after=%d/%d", dials, beforeDialSnapshots, interfaceSnapshots)
	}
}

func TestDynamicNetworkPrimitivesRequireSelfTargetPolicy(t *testing.T) {
	var _ func(context.Context, dynamicIPResolver, *url.URL, int, *dynamicSelfTargetPolicy) ([]net.IP, error) = resolveDynamicURLIPs
	var _ func(*url.URL, []net.IP, *dynamicSelfTargetPolicy) (*http.Transport, error) = newDynamicTransport
	var _ func(*url.URL, []net.IP, dynamicDialContextFunc, *dynamicSelfTargetPolicy) (*http.Transport, error) = newDynamicTransportWithDialer

	target, err := normalizeDynamicURL("https://example.net/")
	if err != nil {
		t.Fatal(err)
	}
	resolverCalls := 0
	resolver := dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		resolverCalls++
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
	})
	var missing *dynamicSelfTargetPolicy
	if _, err := resolveDynamicURLIPs(context.Background(), resolver, target, 1, missing); err == nil {
		t.Fatal("resolver accepted a typed-nil self-target policy")
	}
	if resolverCalls != 0 {
		t.Fatalf("typed-nil policy reached resolver %d times", resolverCalls)
	}

	pins := []net.IP{net.ParseIP("1.1.1.1")}
	if _, err := newDynamicTransport(target, pins, missing); err == nil {
		t.Fatal("transport accepted a typed-nil self-target policy")
	}
	dialCalls := 0
	dialErr := errors.New("recorded required-policy dial")
	dial := func(context.Context, string, string) (net.Conn, error) {
		dialCalls++
		return nil, dialErr
	}
	if _, err := newDynamicTransportWithDialer(target, pins, dial, missing); err == nil {
		t.Fatal("testable transport accepted a typed-nil self-target policy")
	}
	if dialCalls != 0 {
		t.Fatalf("typed-nil policy reached dialer %d times", dialCalls)
	}

	policy := newTestDynamicSelfTargetPolicy(t)
	resolved, err := resolveDynamicURLIPs(context.Background(), resolver, target, 1, policy)
	if err != nil || len(resolved) != 1 || !resolved[0].Equal(net.ParseIP("1.1.1.1")) {
		t.Fatalf("valid policy resolution=%v err=%v", resolved, err)
	}
	transport, err := newDynamicTransportWithDialer(target, pins, dial, policy)
	if err != nil {
		t.Fatalf("valid policy transport: %v", err)
	}
	if _, err := transport.DialContext(context.Background(), "tcp", "example.net:443"); !errors.Is(err, dialErr) {
		t.Fatalf("valid policy dial error=%v", err)
	}
	if resolverCalls != 1 || dialCalls != 1 {
		t.Fatalf("valid policy resolver calls=%d dial calls=%d, want 1/1", resolverCalls, dialCalls)
	}
}

func TestDynamicSelfTargetPolicyAllowsRemoteHostOnListenerPort(t *testing.T) {
	panelPort := freePort(t)
	sitePort := freePort(t)
	localIP := net.ParseIP("8.8.4.4")
	policy, err := newDynamicSelfTargetPolicy("panel.example.com", panelPort, []Site{{
		ID:          1,
		PublicHost:  "media.example.com",
		IngressMode: ingressModeBoth,
		ListenPort:  sitePort,
	}}, func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: localIP, Mask: net.CIDRMask(24, 32)}}, nil
	})
	if err != nil {
		t.Fatalf("create self-target policy: %v", err)
	}

	resolverCalls := 0
	resolver := dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		resolverCalls++
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
	})
	var remoteTarget *url.URL
	for _, port := range []int{panelPort, sitePort} {
		remoteTarget, err = normalizeDynamicURL(fmt.Sprintf("https://remote.example.net:%d/", port))
		if err != nil {
			t.Fatalf("normalize remote target on port %d: %v", port, err)
		}
		resolved, err := resolveDynamicURLIPs(context.Background(), resolver, remoteTarget, 1, policy)
		if err != nil || len(resolved) != 1 || !resolved[0].Equal(net.ParseIP("1.1.1.1")) {
			t.Fatalf("remote target on listener port %d resolution=%v err=%v", port, resolved, err)
		}
	}

	for _, raw := range []string{
		fmt.Sprintf("https://panel.example.com:%d/", panelPort),
		fmt.Sprintf("https://media.example.com:%d/", sitePort),
		fmt.Sprintf("https://8.8.4.4:%d/", panelPort),
	} {
		target, err := normalizeDynamicURL(raw)
		if err != nil {
			t.Fatalf("normalize denied target %s: %v", raw, err)
		}
		before := resolverCalls
		if _, err := resolveDynamicURLIPs(context.Background(), resolver, target, 1, policy); !errors.Is(err, errDynamicSelfTarget) {
			t.Errorf("denied target %s error=%v", raw, err)
		}
		if resolverCalls != before {
			t.Errorf("denied target %s reached resolver", raw)
		}
	}

	remoteLiteral, err := normalizeDynamicURL(fmt.Sprintf("https://1.1.1.1:%d/", panelPort))
	if err != nil {
		t.Fatal(err)
	}
	before := resolverCalls
	resolved, err := resolveDynamicURLIPs(context.Background(), resolver, remoteLiteral, 1, policy)
	if err != nil || len(resolved) != 1 || !resolved[0].Equal(net.ParseIP("1.1.1.1")) {
		t.Fatalf("remote literal on listener port resolution=%v err=%v", resolved, err)
	}
	if resolverCalls != before {
		t.Fatal("remote IP literal unexpectedly reached resolver")
	}

	dialErr := errors.New("recorded listener-port dial")
	dials := 0
	transport, err := newDynamicTransportWithDialer(remoteTarget, []net.IP{net.ParseIP("1.1.1.1")}, func(_ context.Context, network, address string) (net.Conn, error) {
		dials++
		if network != "tcp" || address != fmt.Sprintf("1.1.1.1:%d", sitePort) {
			t.Fatalf("dial network=%q address=%q", network, address)
		}
		return nil, dialErr
	}, policy)
	if err != nil {
		t.Fatalf("construct remote listener-port transport: %v", err)
	}
	if _, err := transport.DialContext(context.Background(), "tcp", remoteTarget.Host); !errors.Is(err, dialErr) {
		t.Fatalf("remote listener-port dial error=%v", err)
	}
	if dials != 1 {
		t.Fatalf("remote listener-port dials=%d, want 1", dials)
	}
}

func TestDynamicDNSAndPinBudgetsFailClosed(t *testing.T) {
	policy := newTestDynamicSelfTargetPolicy(t)
	literal, err := normalizeDynamicURL("https://1.1.1.1/")
	if err != nil {
		t.Fatal(err)
	}
	resolverCalls := 0
	resolver := dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		resolverCalls++
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
	})
	for _, limit := range []int{-1, 0, 65} {
		if _, err := resolveDynamicURLIPs(context.Background(), resolver, literal, limit, policy); err == nil {
			t.Errorf("invalid maxIPs %d was accepted for an IP literal", limit)
		}
	}
	if resolverCalls != 0 {
		t.Fatalf("literal budget validation reached resolver %d times", resolverCalls)
	}

	answers := make([]net.IPAddr, 64)
	pins := make([]net.IP, 65)
	for i := range pins {
		pins[i] = net.ParseIP(fmt.Sprintf("1.1.1.%d", i+1))
		if i < len(answers) {
			answers[i] = net.IPAddr{IP: pins[i]}
		}
	}
	hostname, err := normalizeDynamicURL("https://budget.example.net/")
	if err != nil {
		t.Fatal(err)
	}
	resolver = dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		resolverCalls++
		return answers, nil
	})
	resolved, err := resolveDynamicURLIPs(context.Background(), resolver, hostname, 64, policy)
	if err != nil || len(resolved) != 64 {
		t.Fatalf("64-address resolution count=%d err=%v", len(resolved), err)
	}

	if _, err := newDynamicTransport(hostname, pins, policy); err == nil {
		t.Fatal("direct transport accepted 65 pinned IP addresses")
	}
	dialCalls := 0
	dial := func(context.Context, string, string) (net.Conn, error) {
		dialCalls++
		return nil, errors.New("unexpected budget dial")
	}
	if _, err := newDynamicTransportWithDialer(hostname, nil, dial, policy); err == nil {
		t.Fatal("testable transport accepted no pinned IP addresses")
	}
	if _, err := newDynamicTransportWithDialer(hostname, pins, dial, policy); err == nil {
		t.Fatal("testable transport accepted 65 pinned IP addresses")
	}
	if _, err := newDynamicTransportWithDialer(hostname, pins[:64], dial, policy); err != nil {
		t.Fatalf("testable transport rejected 64 pinned IP addresses: %v", err)
	}
	if dialCalls != 0 {
		t.Fatalf("pin budget validation reached dialer %d times", dialCalls)
	}
}
