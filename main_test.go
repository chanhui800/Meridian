package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestApp(t *testing.T) *App {
	t.Helper()

	db, err := openDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return &App{
		db: db,
		pm: NewProxyManager(db),
	}
}

func freePort(t *testing.T) int {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port listen: %v", err)
	}
	defer ln.Close()

	return ln.Addr().(*net.TCPAddr).Port
}

func decodeBody(t *testing.T, rr *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()

	var body map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rr.Body.String())
	}
	return body
}

func TestGenerateTokenPreservesSpecialCharacters(t *testing.T) {
	jwtSecret = []byte("test-secret")

	token, err := generateToken(7, `bad"name\user`)
	if err != nil {
		t.Fatalf("generateToken error: %v", err)
	}

	userID, username, err := validateToken(token)
	if err != nil {
		t.Fatalf("validateToken error: %v", err)
	}

	if userID != 7 {
		t.Fatalf("userID = %d, want 7", userID)
	}
	if username != `bad"name\user` {
		t.Fatalf("username = %q", username)
	}
}

func TestResolveJWTSecretGeneratesRandomFallback(t *testing.T) {
	secretA, ephemeralA, err := resolveJWTSecret("")
	if err != nil {
		t.Fatalf("resolveJWTSecret A: %v", err)
	}
	secretB, ephemeralB, err := resolveJWTSecret("")
	if err != nil {
		t.Fatalf("resolveJWTSecret B: %v", err)
	}

	if !ephemeralA || !ephemeralB {
		t.Fatalf("expected ephemeral fallback secrets")
	}
	if len(secretA) == 0 || len(secretB) == 0 {
		t.Fatalf("expected non-empty secrets")
	}
	if bytes.Equal(secretA, secretB) {
		t.Fatalf("expected random fallback secrets to differ")
	}
}

func TestResolveJWTSecretRequiresSufficientEntropy(t *testing.T) {
	if _, _, err := resolveJWTSecret("too-short"); err == nil {
		t.Fatal("short JWT_SECRET unexpectedly accepted")
	}
	configured := strings.Repeat("x", 32)
	secret, ephemeral, err := resolveJWTSecret(configured)
	if err != nil {
		t.Fatalf("resolveJWTSecret configured value: %v", err)
	}
	if ephemeral || string(secret) != configured {
		t.Fatalf("configured JWT secret not preserved")
	}
}

func TestTLSIssuerNameFallsBackSafely(t *testing.T) {
	name := tlsIssuerName(nil)
	if name != "" {
		t.Fatalf("nil issuer name = %q, want empty", name)
	}
}

func TestSecureTLSConfigEnablesVerification(t *testing.T) {
	config := secureTLSConfig("emby.example.com")
	if config.InsecureSkipVerify {
		t.Fatal("TLS certificate verification must remain enabled")
	}
	if config.ServerName != "emby.example.com" {
		t.Fatalf("ServerName = %q, want emby.example.com", config.ServerName)
	}
	if config.MinVersion != tls.VersionTLS12 {
		t.Fatalf("MinVersion = %d, want TLS 1.2", config.MinVersion)
	}
}

func TestNormalizeTargetURLRejectsUnsafeForms(t *testing.T) {
	for _, target := range []string{
		"file://server/path",
		"http://user:password@example.com",
		"https://example.com/path#fragment",
		"http://example.com:70000",
	} {
		if _, err := normalizeTargetURL(target); err == nil {
			t.Errorf("normalizeTargetURL(%q) unexpectedly succeeded", target)
		}
	}

	target, err := normalizeTargetURL("example.com:8096")
	if err != nil {
		t.Fatalf("normalizeTargetURL valid target: %v", err)
	}
	if target.String() != "http://example.com:8096" {
		t.Fatalf("normalized target = %q, want http://example.com:8096", target)
	}
}

func TestSecurityHeaders(t *testing.T) {
	handler := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rr.Header().Get("Content-Security-Policy"); !strings.Contains(got, "script-src 'self'") || !strings.Contains(got, "frame-ancestors 'none'") {
		t.Fatalf("unexpected Content-Security-Policy: %q", got)
	}
	if got := rr.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rr.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("X-Frame-Options = %q, want DENY", got)
	}
}

func TestHandleAuthCheckExposesSingleAdminModeBeforeSetup(t *testing.T) {
	app := newTestApp(t)
	jwtSecretEphemeral = true
	t.Cleanup(func() { jwtSecretEphemeral = false })

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/check", nil)

	app.handleAuthCheck(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	body := decodeBody(t, rr)
	if got := mustBoolValue(t, body, "needs_setup"); !got {
		t.Fatalf("needs_setup = %v, want true", got)
	}
	if got := mustStringValue(t, body, "mode"); got != "single_admin" {
		t.Fatalf("mode = %q, want single_admin", got)
	}
	if got := mustBoolValue(t, body, "jwt_secret_ephemeral"); !got {
		t.Fatalf("jwt_secret_ephemeral = %v, want true", got)
	}
}

func TestSetupRequiresTokenAndCreatesOnlyOneAdmin(t *testing.T) {
	app := newTestApp(t)
	app.setupToken = "one-time-setup-token"

	wrong := httptest.NewRecorder()
	wrongReq := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{
		"username":"admin","password":"correct horse battery staple","setup_token":"wrong"
	}`))
	app.handleSetup(wrong, wrongReq)
	if wrong.Code != http.StatusForbidden {
		t.Fatalf("wrong setup token status = %d, want 403", wrong.Code)
	}
	if got := app.db.UserCount(); got != 0 {
		t.Fatalf("user count after rejected setup = %d, want 0", got)
	}

	ok := httptest.NewRecorder()
	okReq := httptest.NewRequest(http.MethodPost, "/api/auth/setup", strings.NewReader(`{
		"username":"admin","password":"correct horse battery staple","setup_token":"one-time-setup-token"
	}`))
	app.handleSetup(ok, okReq)
	if ok.Code != http.StatusOK {
		t.Fatalf("valid setup status = %d body=%s", ok.Code, ok.Body.String())
	}
	if got := app.db.UserCount(); got != 1 {
		t.Fatalf("user count after setup = %d, want 1", got)
	}
}

func TestCreateInitialUserIsAtomic(t *testing.T) {
	app := newTestApp(t)
	const contenders = 4
	var wg sync.WaitGroup
	results := make(chan error, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := app.db.CreateInitialUser(fmt.Sprintf("admin-%d", i), "correct horse battery staple")
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)

	created := 0
	alreadyExists := 0
	for err := range results {
		switch {
		case err == nil:
			created++
		case errors.Is(err, errAdminAlreadyExists):
			alreadyExists++
		default:
			t.Fatalf("unexpected setup error: %v", err)
		}
	}
	if created != 1 || alreadyExists != contenders-1 {
		t.Fatalf("created=%d alreadyExists=%d, want 1 and %d", created, alreadyExists, contenders-1)
	}
	if got := app.db.UserCount(); got != 1 {
		t.Fatalf("user count = %d, want 1", got)
	}
}

func TestLoginUsesGenericErrorsAndRateLimit(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.db.CreateInitialUser("admin", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateInitialUser: %v", err)
	}

	login := func(username, password string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(fmt.Sprintf(
			`{"username":%q,"password":%q}`, username, password,
		)))
		req.RemoteAddr = "203.0.113.10:12345"
		app.handleLogin(rr, req)
		return rr
	}

	unknown := login("missing", "wrong password")
	badPassword := login("admin", "wrong password")
	if unknown.Code != http.StatusUnauthorized || badPassword.Code != http.StatusUnauthorized {
		t.Fatalf("credential failure statuses = %d, %d; want 401", unknown.Code, badPassword.Code)
	}
	if unknown.Body.String() != badPassword.Body.String() {
		t.Fatalf("credential failure responses differ: %q vs %q", unknown.Body.String(), badPassword.Body.String())
	}

	for i := 0; i < maxLoginFailures-2; i++ {
		login("admin", "wrong password")
	}
	blocked := login("admin", "correct horse battery staple")
	if blocked.Code != http.StatusTooManyRequests {
		t.Fatalf("blocked login status = %d, want 429", blocked.Code)
	}
	if blocked.Header().Get("Retry-After") == "" {
		t.Fatal("blocked login is missing Retry-After")
	}
}

func TestCORSAllowsSameOriginAndRejectsCrossOrigin(t *testing.T) {
	handler := cors(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	same := httptest.NewRecorder()
	sameReq := httptest.NewRequest(http.MethodGet, "http://panel.example/api/auth/check", nil)
	sameReq.Header.Set("Origin", "https://panel.example")
	handler(same, sameReq)
	if same.Code != http.StatusOK || same.Header().Get("Access-Control-Allow-Origin") != "https://panel.example" {
		t.Fatalf("same-origin request status=%d allow-origin=%q", same.Code, same.Header().Get("Access-Control-Allow-Origin"))
	}

	cross := httptest.NewRecorder()
	crossReq := httptest.NewRequest(http.MethodGet, "http://panel.example/api/auth/check", nil)
	crossReq.Header.Set("Origin", "https://evil.example")
	handler(cross, crossReq)
	if cross.Code != http.StatusForbidden {
		t.Fatalf("cross-origin request status = %d, want 403", cross.Code)
	}
}

func TestHandleAuthCheckExposesConfiguredSingleAdminMode(t *testing.T) {
	app := newTestApp(t)
	jwtSecretEphemeral = false

	if _, err := app.db.CreateUser("admin", "admin123"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/check", nil)

	app.handleAuthCheck(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	body := decodeBody(t, rr)
	if got := mustBoolValue(t, body, "needs_setup"); got {
		t.Fatalf("needs_setup = %v, want false", got)
	}
	if got := mustStringValue(t, body, "mode"); got != "single_admin" {
		t.Fatalf("mode = %q, want single_admin", got)
	}
	if got := mustBoolValue(t, body, "jwt_secret_ephemeral"); got {
		t.Fatalf("jwt_secret_ephemeral = %v, want false", got)
	}
}

func TestDiagnoseSiteUsesRootSystemInfoProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Info/Public" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"Version":"4.8.0.80"}`))
	}))
	defer server.Close()

	app := newTestApp(t)
	site, err := app.db.CreateSite("diag", freePort(t), server.URL, "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}

	result := diagnoseSite(site, app.pm)
	if result.Health.Status != "online" {
		t.Fatalf("health.status = %q, want online (error=%q)", result.Health.Status, result.Health.Error)
	}
	if result.Health.EmbyVer != "4.8.0.80" {
		t.Fatalf("emby_version = %q, want 4.8.0.80", result.Health.EmbyVer)
	}
	if result.Health.Probe.Kind != "metadata_api" {
		t.Fatalf("probe.kind = %q, want metadata_api", result.Health.Probe.Kind)
	}
	if result.Health.Probe.Method != http.MethodGet {
		t.Fatalf("probe.method = %q, want GET", result.Health.Probe.Method)
	}
	if !strings.HasSuffix(result.Health.Probe.URL, "/System/Info/Public") {
		t.Fatalf("probe.url = %q, want suffix /System/Info/Public", result.Health.Probe.URL)
	}
	if result.Health.Probe.HTTPStatus != http.StatusOK {
		t.Fatalf("probe.http_status = %d, want 200", result.Health.Probe.HTTPStatus)
	}
}

func TestDiagnoseSiteTreatsReachable4xxAsOnline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "blocked", http.StatusForbidden)
	}))
	defer server.Close()

	app := newTestApp(t)
	site, err := app.db.CreateSite("diag", freePort(t), server.URL, "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}

	result := diagnoseSite(site, app.pm)
	if result.Health.Status != "online" {
		t.Fatalf("health.status = %q, want online (error=%q)", result.Health.Status, result.Health.Error)
	}
	if result.Health.Error != "" {
		t.Fatalf("health.error = %q, want empty for reachable upstream", result.Health.Error)
	}
	if result.Health.Probe.HTTPStatus != http.StatusForbidden {
		t.Fatalf("probe.http_status = %d, want 403", result.Health.Probe.HTTPStatus)
	}
}

func TestDiagnoseSiteMarksRootReachabilityFallbackProbe(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	app := newTestApp(t)
	site, err := app.db.CreateSite("diag", freePort(t), server.URL, "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}

	result := diagnoseSite(site, app.pm)
	if result.Health.Status != "online" {
		t.Fatalf("health.status = %q, want online (error=%q)", result.Health.Status, result.Health.Error)
	}
	if result.Health.Probe.Kind != "reachability_fallback" {
		t.Fatalf("probe.kind = %q, want reachability_fallback", result.Health.Probe.Kind)
	}
	if result.Health.Probe.Method != http.MethodGet {
		t.Fatalf("probe.method = %q, want GET", result.Health.Probe.Method)
	}
	if result.Health.Probe.URL != server.URL+"/" {
		t.Fatalf("probe.url = %q, want %q", result.Health.Probe.URL, server.URL+"/")
	}
	if result.Health.Probe.HTTPStatus != http.StatusOK {
		t.Fatalf("probe.http_status = %d, want 200", result.Health.Probe.HTTPStatus)
	}
}

func TestHandleSiteDiagReturnsPlaybackFallbackMetadata(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Info/Public" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"Version":"4.8.1.0"}`))
	}))
	defer apiServer.Close()

	app := newTestApp(t)
	site, err := app.db.CreateSite("diag", freePort(t), apiServer.URL, "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sites/"+jsonNumber64(site.ID)+"/diag", nil)

	app.handleSiteByID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	body := decodeBody(t, rr)
	upstreams := mustMapValue(t, body, "upstreams")
	primary := mustMapValue(t, upstreams, "primary")
	playback := mustMapValue(t, upstreams, "playback")

	if got := mustStringValue(t, primary, "effective_url"); got != apiServer.URL {
		t.Fatalf("primary effective_url = %q, want %q", got, apiServer.URL)
	}
	if got := mustBoolValue(t, primary, "show_health"); !got {
		t.Fatalf("primary show_health = %v, want true", got)
	}
	primaryHealth := mustMapValue(t, primary, "health")
	primaryProbe := mustMapValue(t, primaryHealth, "probe")
	if got := mustStringValue(t, primaryProbe, "kind"); got != "metadata_api" {
		t.Fatalf("primary probe.kind = %q, want metadata_api", got)
	}
	if got := mustStringValue(t, primaryProbe, "method"); got != http.MethodGet {
		t.Fatalf("primary probe.method = %q, want GET", got)
	}
	if got := mustStringValue(t, playback, "effective_url"); got != apiServer.URL {
		t.Fatalf("playback effective_url = %q, want %q", got, apiServer.URL)
	}
	if got := mustBoolValue(t, playback, "configured"); got {
		t.Fatalf("playback configured = %v, want false", got)
	}
	if got := mustBoolValue(t, playback, "using_fallback"); !got {
		t.Fatalf("playback using_fallback = %v, want true", got)
	}
	if got := mustBoolValue(t, playback, "same_as_primary"); !got {
		t.Fatalf("playback same_as_primary = %v, want true", got)
	}
	if got := mustBoolValue(t, playback, "show_health"); got {
		t.Fatalf("playback show_health = %v, want false", got)
	}
	if got := mustBoolValue(t, playback, "show_tls"); got {
		t.Fatalf("playback show_tls = %v, want false", got)
	}
	playbackProbe := mustMapValue(t, mustMapValue(t, playback, "health"), "probe")
	if got := mustStringValue(t, playbackProbe, "kind"); got != "metadata_api" {
		t.Fatalf("fallback playback probe.kind = %q, want metadata_api", got)
	}
}

func TestHandleSiteDiagMarksSharedPlaybackTarget(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Info/Public" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"Version":"4.8.1.0"}`))
	}))
	defer apiServer.Close()

	app := newTestApp(t)
	site, err := app.db.CreateSite("diag", freePort(t), apiServer.URL, apiServer.URL, "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sites/"+jsonNumber64(site.ID)+"/diag", nil)

	app.handleSiteByID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	body := decodeBody(t, rr)
	playback := mustMapValue(t, mustMapValue(t, body, "upstreams"), "playback")

	if got := mustBoolValue(t, playback, "configured"); !got {
		t.Fatalf("playback configured = %v, want true", got)
	}
	if got := mustBoolValue(t, playback, "using_fallback"); got {
		t.Fatalf("playback using_fallback = %v, want false", got)
	}
	if got := mustBoolValue(t, playback, "same_as_primary"); !got {
		t.Fatalf("playback same_as_primary = %v, want true", got)
	}
	if got := mustBoolValue(t, playback, "show_health"); got {
		t.Fatalf("playback show_health = %v, want false", got)
	}
	playbackProbe := mustMapValue(t, mustMapValue(t, playback, "health"), "probe")
	if got := mustStringValue(t, playbackProbe, "kind"); got != "metadata_api" {
		t.Fatalf("shared playback probe.kind = %q, want metadata_api", got)
	}
}

func TestHandleSiteDiagExposesSeparatePlaybackTLS(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Info/Public" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"Version":"4.8.1.0"}`))
	}))
	defer apiServer.Close()

	playbackServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/System/Info/Public" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"Version":"4.8.2.0"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer playbackServer.Close()

	app := newTestApp(t)
	site, err := app.db.CreateSite("diag", freePort(t), apiServer.URL, playbackServer.URL, "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sites/"+jsonNumber64(site.ID)+"/diag", nil)

	app.handleSiteByID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	body := decodeBody(t, rr)
	upstreams := mustMapValue(t, body, "upstreams")
	primary := mustMapValue(t, upstreams, "primary")
	playback := mustMapValue(t, upstreams, "playback")
	playbackHealth := mustMapValue(t, playback, "health")
	playbackProbe := mustMapValue(t, playbackHealth, "probe")
	playbackTLS := mustMapValue(t, playback, "tls")

	if got := mustBoolValue(t, primary, "show_tls"); got {
		t.Fatalf("primary show_tls = %v, want false", got)
	}
	if got := mustBoolValue(t, playback, "configured"); !got {
		t.Fatalf("playback configured = %v, want true", got)
	}
	if got := mustBoolValue(t, playback, "same_as_primary"); got {
		t.Fatalf("playback same_as_primary = %v, want false", got)
	}
	if got := mustBoolValue(t, playback, "show_health"); !got {
		t.Fatalf("playback show_health = %v, want true", got)
	}
	if got := mustBoolValue(t, playback, "show_tls"); !got {
		t.Fatalf("playback show_tls = %v, want true", got)
	}
	if got := mustStringValue(t, playbackProbe, "kind"); got != "metadata_api" {
		t.Fatalf("playback probe.kind = %q, want metadata_api", got)
	}
	if got := mustStringValue(t, playbackProbe, "method"); got != http.MethodGet {
		t.Fatalf("playback probe.method = %q, want GET", got)
	}
	if got := mustStringValue(t, playbackProbe, "url"); got != playbackServer.URL+"/System/Info/Public" {
		t.Fatalf("playback probe.url = %q, want metadata URL", got)
	}
	if got := mustStringValue(t, playbackHealth, "status"); got != "offline" {
		t.Fatalf("playback health.status = %q, want offline for an untrusted test certificate", got)
	}
	if got := mustStringValue(t, playbackHealth, "error"); got == "" {
		t.Fatal("playback health.error should report TLS verification failure")
	}
	if got := mustBoolValue(t, playbackTLS, "enabled"); !got {
		t.Fatalf("playback tls.enabled = %v, want true", got)
	}
	if got := mustBoolValue(t, playbackTLS, "valid"); got {
		t.Fatalf("playback tls.valid = %v, want false for an untrusted test certificate", got)
	}
	if got := mustStringValue(t, playback, "effective_url"); got != playbackServer.URL {
		t.Fatalf("playback effective_url = %q, want %q", got, playbackServer.URL)
	}
}

func TestApplyUAProfileHeadersRewritesClientAndVersionIdentity(t *testing.T) {
	header := http.Header{}
	header.Set("User-Agent", "OldUA/1.0")
	header.Set("X-Emby-Authorization", `MediaBrowser Client="Old Client", Device="TV", Version="9.9.9"`)
	header.Set("Authorization", `MediaBrowser Client="Old Client", Device="TV", Version="9.9.9"`)

	applyUAProfileHeaders(header, uaProfiles["client"])

	if got := header.Get("User-Agent"); got != uaProfiles["client"].UserAgent {
		t.Fatalf("User-Agent = %q, want %q", got, uaProfiles["client"].UserAgent)
	}
	if got := header.Get("X-Emby-Authorization"); !strings.Contains(got, `Client="Emby Theater"`) {
		t.Fatalf("X-Emby-Authorization = %q", got)
	}
	if got := header.Get("X-Emby-Authorization"); !strings.Contains(got, `Version="4.7.0"`) {
		t.Fatalf("X-Emby-Authorization version = %q", got)
	}
	if got := header.Get("Authorization"); !strings.Contains(got, `Client="Emby Theater"`) {
		t.Fatalf("Authorization = %q", got)
	}
	if got := header.Get("Authorization"); !strings.Contains(got, `Version="4.7.0"`) {
		t.Fatalf("Authorization version = %q", got)
	}
}

func TestHandleSiteDiagReturnsSpoofedVersionField(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/System/Info/Public" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"Version":"4.8.1.0"}`))
	}))
	defer apiServer.Close()

	app := newTestApp(t)
	site, err := app.db.CreateSite("diag", freePort(t), apiServer.URL, "", "direct", "[]", "client", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/sites/"+jsonNumber64(site.ID)+"/diag", nil)

	app.handleSiteByID(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	headers := mustMapValue(t, decodeBody(t, rr), "headers")
	if got := mustBoolValue(t, headers, "ua_applied"); !got {
		t.Fatalf("ua_applied = %v, want true", got)
	}
	if got := mustStringValue(t, headers, "current_ua"); got != uaProfiles["client"].UserAgent {
		t.Fatalf("current_ua = %q, want %q", got, uaProfiles["client"].UserAgent)
	}
	if got := mustStringValue(t, headers, "client_field"); got != uaProfiles["client"].Client {
		t.Fatalf("client_field = %q, want %q", got, uaProfiles["client"].Client)
	}
	if got := mustStringValue(t, headers, "version_field"); got != uaProfiles["client"].Version {
		t.Fatalf("version_field = %q, want %q", got, uaProfiles["client"].Version)
	}
}

func TestHandleSitesCreateRollsBackOnStartFailure(t *testing.T) {
	app := newTestApp(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupied listen: %v", err)
	}
	port := occupied.Addr().(*net.TCPAddr).Port
	occupied.Close()
	occupied, err = net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Fatalf("occupied wildcard listen: %v", err)
	}
	defer occupied.Close()

	body := strings.NewReader(`{"name":"conflict","listen_port":` + jsonNumber(port) + `,"target_url":"http://127.0.0.1:8096","ua_mode":"infuse"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/sites", body)
	rr := httptest.NewRecorder()

	app.handleSites(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if count := lenMust(app.db.ListSites()); count != 0 {
		t.Fatalf("site count = %d, want 0", count)
	}
}

func TestHandleSiteToggleRevertsWhenStartFails(t *testing.T) {
	app := newTestApp(t)
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupied listen: %v", err)
	}
	port := occupied.Addr().(*net.TCPAddr).Port
	occupied.Close()
	occupied, err = net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		t.Fatalf("occupied wildcard listen: %v", err)
	}
	defer occupied.Close()

	site, err := app.db.CreateSite("disabled", port, "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if _, err := app.db.db.Exec("UPDATE sites SET enabled=0 WHERE id=?", site.ID); err != nil {
		t.Fatalf("disable site: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/sites/"+jsonNumber64(site.ID)+"/toggle", nil)
	rr := httptest.NewRecorder()

	app.handleSiteByID(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	reloaded, err := app.db.GetSite(site.ID)
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}
	if reloaded.Enabled {
		t.Fatalf("site enabled = true, want false")
	}
}

func TestHandleSiteUpdateRollsBackOnStartFailure(t *testing.T) {
	app := newTestApp(t)
	initialPort := freePort(t)
	site, err := app.db.CreateSite("stable", initialPort, "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatalf("StartSite: %v", err)
	}
	t.Cleanup(func() { app.pm.StopSite(site.ID) })

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupied listen: %v", err)
	}
	conflictPort := occupied.Addr().(*net.TCPAddr).Port
	occupied.Close()
	occupied, err = net.Listen("tcp", fmt.Sprintf(":%d", conflictPort))
	if err != nil {
		t.Fatalf("occupied wildcard listen: %v", err)
	}
	defer occupied.Close()

	body := strings.NewReader(`{"name":"stable","listen_port":` + jsonNumber(conflictPort) + `,"target_url":"http://127.0.0.1:8096","ua_mode":"infuse"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/sites/"+jsonNumber64(site.ID), body)
	rr := httptest.NewRecorder()

	app.handleSiteByID(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	reloaded, err := app.db.GetSite(site.ID)
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}
	if reloaded.ListenPort != initialPort {
		t.Fatalf("listen_port = %d, want %d", reloaded.ListenPort, initialPort)
	}
	if !app.pm.IsRunning(site.ID) {
		t.Fatalf("expected original site to keep running")
	}
}

func TestFlushTrafficUpdatesBaselineAndStopPersistsPendingUsage(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSite("traffic", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 1024, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}

	inst := &ProxyInstance{Site: *site, server: &http.Server{}}
	inst.bytesIn.Store(120)
	inst.bytesOut.Store(80)
	app.pm.proxies[site.ID] = inst

	app.pm.FlushTraffic()

	if got := inst.persistedTraffic.Load(); got != 200 {
		t.Fatalf("persistedTraffic after flush = %d, want 200", got)
	}
	inst.bytesIn.Store(10)
	inst.bytesOut.Store(5)
	app.pm.StopSite(site.ID)

	reloaded, err := app.db.GetSite(site.ID)
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}
	if reloaded.TrafficUsed != 215 {
		t.Fatalf("traffic_used = %d, want 215", reloaded.TrafficUsed)
	}
}

func TestAddTrafficAggregatesSameHour(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSite("aggregate", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}

	app.db.AddTraffic(site.ID, 10, 20)
	app.db.AddTraffic(site.ID, 5, 7)

	logs, err := app.db.GetTrafficLogs(site.ID, 1)
	if err != nil {
		t.Fatalf("GetTrafficLogs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs) = %d, want 1", len(logs))
	}
	if logs[0].BytesIn != 15 || logs[0].BytesOut != 27 {
		t.Fatalf("aggregated log = in:%d out:%d", logs[0].BytesIn, logs[0].BytesOut)
	}
}

func TestHandleSitesCreatePersistsPlaybackTargetURL(t *testing.T) {
	app := newTestApp(t)

	body := strings.NewReader(`{"name":"split","listen_port":` + jsonNumber(freePort(t)) + `,"target_url":"http://127.0.0.1:8096","playback_target_url":"https://media.example.com","ua_mode":"infuse"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/sites", body)
	rr := httptest.NewRecorder()

	app.handleSites(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	var site Site
	if err := json.Unmarshal(rr.Body.Bytes(), &site); err != nil {
		t.Fatalf("decode site: %v body=%s", err, rr.Body.String())
	}
	if site.PlaybackTargetURL != "https://media.example.com" {
		t.Fatalf("playback_target_url = %q, want %q", site.PlaybackTargetURL, "https://media.example.com")
	}

	reloaded, err := app.db.GetSite(site.ID)
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}
	if reloaded.PlaybackTargetURL != "https://media.example.com" {
		t.Fatalf("persisted playback_target_url = %q, want %q", reloaded.PlaybackTargetURL, "https://media.example.com")
	}
}

func TestProxyRoutesPlaybackRequestsToPlaybackTarget(t *testing.T) {
	app := newTestApp(t)

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Content-Security-Policy", "default-src 'none'")
		w.Write([]byte("api:" + r.URL.Path))
	}))
	defer apiServer.Close()

	playbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("playback:" + r.URL.Path))
	}))
	defer playbackServer.Close()

	site, err := app.db.CreateSite("split", freePort(t), apiServer.URL, playbackServer.URL, "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatalf("StartSite: %v", err)
	}
	t.Cleanup(func() { app.pm.StopSite(site.ID) })
	app.pm.mu.RLock()
	proxyServer := app.pm.proxies[site.ID].server
	app.pm.mu.RUnlock()
	if proxyServer.ReadHeaderTimeout != 10*time.Second || proxyServer.IdleTimeout != 120*time.Second || proxyServer.MaxHeaderBytes != 64<<10 {
		t.Fatalf("proxy server limits not configured: %+v", proxyServer)
	}

	mainResp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/System/Info", site.ListenPort))
	if err != nil {
		t.Fatalf("GET main route: %v", err)
	}
	defer mainResp.Body.Close()
	if got := mainResp.Header.Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Fatalf("upstream X-Frame-Options = %q, want SAMEORIGIN", got)
	}
	if got := mainResp.Header.Get("Content-Security-Policy"); got != "default-src 'none'" {
		t.Fatalf("upstream Content-Security-Policy = %q, want preserved value", got)
	}

	playbackResp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/emby/Videos/123/stream", site.ListenPort))
	if err != nil {
		t.Fatalf("GET playback route: %v", err)
	}
	defer playbackResp.Body.Close()

	if body := mustReadBody(t, mainResp); !strings.Contains(body, "api:/System/Info") {
		t.Fatalf("main route body = %q", body)
	}
	if body := mustReadBody(t, playbackResp); !strings.Contains(body, "playback:/emby/Videos/123/stream") {
		t.Fatalf("playback route body = %q", body)
	}
}

func TestProxyPlaybackRequestsFallBackToMainTarget(t *testing.T) {
	app := newTestApp(t)

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("api:" + r.URL.Path))
	}))
	defer apiServer.Close()

	site, err := app.db.CreateSite("single", freePort(t), apiServer.URL, "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatalf("StartSite: %v", err)
	}
	t.Cleanup(func() { app.pm.StopSite(site.ID) })

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/Videos/42/stream", site.ListenPort))
	if err != nil {
		t.Fatalf("GET fallback playback route: %v", err)
	}
	defer resp.Body.Close()

	if body := mustReadBody(t, resp); !strings.Contains(body, "api:/Videos/42/stream") {
		t.Fatalf("fallback playback body = %q", body)
	}
}

func lenMust(sites []Site, err error) int {
	if err != nil {
		panic(err)
	}
	return len(sites)
}

func jsonNumber(v int) string {
	return strconv.Itoa(v)
}

func jsonNumber64(v int64) string {
	return strconv.FormatInt(v, 10)
}

func mustReadBody(t *testing.T, resp *http.Response) string {
	t.Helper()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

func mustMapValue(t *testing.T, body map[string]interface{}, key string) map[string]interface{} {
	t.Helper()

	value, ok := body[key]
	if !ok {
		t.Fatalf("missing key %q in %#v", key, body)
	}
	result, ok := value.(map[string]interface{})
	if !ok {
		t.Fatalf("key %q = %#v, want object", key, value)
	}
	return result
}

func mustStringValue(t *testing.T, body map[string]interface{}, key string) string {
	t.Helper()

	value, ok := body[key]
	if !ok {
		t.Fatalf("missing key %q in %#v", key, body)
	}
	result, ok := value.(string)
	if !ok {
		t.Fatalf("key %q = %#v, want string", key, value)
	}
	return result
}

func mustBoolValue(t *testing.T, body map[string]interface{}, key string) bool {
	t.Helper()

	value, ok := body[key]
	if !ok {
		t.Fatalf("missing key %q in %#v", key, body)
	}
	result, ok := value.(bool)
	if !ok {
		t.Fatalf("key %q = %#v, want bool", key, value)
	}
	return result
}

func mustNumberValue(t *testing.T, body map[string]interface{}, key string) int {
	t.Helper()

	value, ok := body[key]
	if !ok {
		t.Fatalf("missing key %q in %#v", key, body)
	}
	result, ok := value.(float64)
	if !ok {
		t.Fatalf("key %q = %#v, want number", key, value)
	}
	return int(result)
}
