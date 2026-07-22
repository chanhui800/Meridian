package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"meridian/web"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

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

func mustUserCount(t *testing.T, db *DB) int {
	t.Helper()
	count, err := db.UserCount()
	if err != nil {
		t.Fatalf("UserCount: %v", err)
	}
	return count
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

func TestNormalizeTargetURLInfersHTTPSForPort443(t *testing.T) {
	for _, input := range []string{"example.com:443", "example.com：443"} {
		target, err := normalizeTargetURL(input)
		if err != nil {
			t.Fatalf("normalizeTargetURL(%q): %v", input, err)
		}
		if target.String() != "https://example.com:443" {
			t.Fatalf("normalizeTargetURL(%q) = %q, want https://example.com:443", input, target)
		}
	}

	explicitHTTP, err := normalizeTargetURL("http://example.com:443")
	if err != nil {
		t.Fatalf("normalize explicit HTTP target: %v", err)
	}
	if explicitHTTP.Scheme != "http" {
		t.Fatalf("explicit HTTP scheme = %q, want http", explicitHTTP.Scheme)
	}
}

func TestRedirectModeTreatsExplicit443AsDefaultHTTPSPort(t *testing.T) {
	configured, err := normalizeTargetURL("media.example.com:443")
	if err != nil {
		t.Fatalf("normalize configured playback target: %v", err)
	}
	if got := redirectHostKey(configured); got != "https://media.example.com" {
		t.Fatalf("redirect host key = %q, want https://media.example.com", got)
	}

	calls := 0
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"https://media.example.com/Videos/1/stream"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("proxied")),
			Request:    req,
		}, nil
	})
	transport := &redirectFollowTransport{
		base:          base,
		playbackHosts: map[string]bool{redirectHostKey(configured): true},
		profile:       getUAProfile("infuse"),
	}
	req := httptest.NewRequest(http.MethodGet, "http://api.example.com/Videos/1/stream", nil)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	if calls != 2 || resp.StatusCode != http.StatusOK {
		t.Fatalf("redirect follow calls=%d status=%d, want calls=2 status=200", calls, resp.StatusCode)
	}
	if got := resp.Request.URL.String(); got != "https://media.example.com/Videos/1/stream" {
		t.Fatalf("followed URL = %q", got)
	}

	t.Run("rejects scheme downgrade", func(t *testing.T) {
		calls := 0
		base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: http.StatusFound,
				Header:     http.Header{"Location": []string{"http://media.example.com/Videos/1/stream"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		})
		transport := &redirectFollowTransport{
			base:          base,
			playbackHosts: map[string]bool{redirectHostKey(configured): true},
			profile:       getUAProfile("infuse"),
		}
		req := httptest.NewRequest(http.MethodGet, "http://api.example.com/Videos/1/stream", nil)
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		defer resp.Body.Close()
		if calls != 1 || resp.StatusCode != http.StatusFound {
			t.Fatalf("downgrade redirect calls=%d status=%d, want calls=1 status=302", calls, resp.StatusCode)
		}
	})

	t.Run("follows custom GET redirect path", func(t *testing.T) {
		calls := 0
		base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{"https://media.example.com/custom/play/path"}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("proxied")),
				Request:    req,
			}, nil
		})
		transport := &redirectFollowTransport{
			base:          base,
			playbackHosts: map[string]bool{redirectHostKey(configured): true},
			profile:       getUAProfile("infuse"),
		}
		req := httptest.NewRequest(http.MethodGet, "http://api.example.com/custom/play/path", nil)
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		defer resp.Body.Close()
		if calls != 2 || resp.StatusCode != http.StatusOK {
			t.Fatalf("custom redirect calls=%d status=%d, want calls=2 status=200", calls, resp.StatusCode)
		}
	})

	t.Run("follows protocol-relative redirect", func(t *testing.T) {
		calls := 0
		base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{"//media.example.com/custom/play/path"}},
					Body:       io.NopCloser(strings.NewReader("")),
					Request:    req,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("proxied")),
				Request:    req,
			}, nil
		})
		transport := &redirectFollowTransport{
			base:          base,
			playbackHosts: map[string]bool{redirectHostKey(configured): true},
			profile:       getUAProfile("infuse"),
		}
		req := httptest.NewRequest(http.MethodGet, "https://api.example.com/custom/play/path", nil)
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		defer resp.Body.Close()
		if calls != 2 || resp.StatusCode != http.StatusOK {
			t.Fatalf("protocol-relative redirect calls=%d status=%d, want calls=2 status=200", calls, resp.StatusCode)
		}
		if got := resp.Request.URL.String(); got != "https://media.example.com/custom/play/path" {
			t.Fatalf("protocol-relative redirect URL = %q", got)
		}
	})

	t.Run("does not follow POST request", func(t *testing.T) {
		calls := 0
		base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: http.StatusTemporaryRedirect,
				Header:     http.Header{"Location": []string{"https://media.example.com/Users/AuthenticateByName"}},
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		})
		transport := &redirectFollowTransport{
			base:          base,
			playbackHosts: map[string]bool{redirectHostKey(configured): true},
			profile:       getUAProfile("infuse"),
		}
		req := httptest.NewRequest(http.MethodPost, "http://api.example.com/Users/AuthenticateByName", strings.NewReader(`{"Username":"test"}`))
		resp, err := transport.RoundTrip(req)
		if err != nil {
			t.Fatalf("RoundTrip: %v", err)
		}
		defer resp.Body.Close()
		if calls != 1 || resp.StatusCode != http.StatusTemporaryRedirect {
			t.Fatalf("API redirect calls=%d status=%d, want calls=1 status=307", calls, resp.StatusCode)
		}
	})
}

func TestReverseProxyRebuildsForwardingHeadersAfterHopHeaderRemoval(t *testing.T) {
	target, err := normalizeTargetURL("https://upstream.example.com/emby")
	if err != nil {
		t.Fatalf("normalize target: %v", err)
	}
	profile := getUAProfile("infuse")
	var captured *http.Request
	proxy := &httputil.ReverseProxy{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			captured = req.Clone(req.Context())
			captured.Header = req.Header.Clone()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
				Request:    req,
			}, nil
		}),
		Rewrite: func(proxyReq *httputil.ProxyRequest) {
			applyUpstreamURL(proxyReq.Out.URL, target)
			proxyReq.Out.Host = target.Host
			prepareUpstreamHeaders(proxyReq.Out.Header, proxyReq.In, profile)
		},
	}

	req := httptest.NewRequest(http.MethodGet, "http://meridian.example:50001/Videos/1/stream", nil)
	req.RemoteAddr = "198.51.100.24:43210"
	req.Header.Set("Connection", "User-Agent, X-Forwarded-For")
	req.Header.Set("User-Agent", "attacker-controlled")
	req.Header.Set("Forwarded", "for=203.0.113.8;proto=https")
	req.Header.Set("X-Forwarded-For", "203.0.113.8")
	req.Header.Set("X-Forwarded-Host", "attacker.example")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Custom", "must-not-pass")
	req.Header.Set("X-Real-IP", "203.0.113.9")
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if captured == nil {
		t.Fatal("transport did not receive an outbound request")
	}
	if captured.URL.String() != "https://upstream.example.com/emby/Videos/1/stream" {
		t.Fatalf("outbound URL = %q", captured.URL.String())
	}
	if captured.Host != target.Host {
		t.Fatalf("outbound Host = %q, want %q", captured.Host, target.Host)
	}
	if got := captured.Header.Get("User-Agent"); got != profile.UserAgent {
		t.Fatalf("outbound User-Agent = %q, want profile value %q", got, profile.UserAgent)
	}
	for name, want := range map[string]string{
		"X-Forwarded-For":   "198.51.100.24",
		"X-Real-IP":         "198.51.100.24",
		"X-Forwarded-Host":  "meridian.example:50001",
		"X-Forwarded-Proto": "http",
	} {
		if got := captured.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"Forwarded", "X-Forwarded-Custom"} {
		if got := captured.Header.Get(name); got != "" {
			t.Errorf("untrusted %s leaked upstream: %q", name, got)
		}
	}
}

func TestPrepareWebSocketUpstreamHeadersRebuildsForwardingHeaders(t *testing.T) {
	target, err := normalizeTargetURL("https://upstream.example.com/emby")
	if err != nil {
		t.Fatalf("normalize target: %v", err)
	}
	profile := getUAProfile("infuse")
	req := httptest.NewRequest(http.MethodGet, "http://meridian.example:50001/socket", nil)
	req.RemoteAddr = "198.51.100.25:54321"
	req.Header.Set("Connection", "Upgrade, User-Agent")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("User-Agent", "attacker-controlled")
	req.Header.Set("Forwarded", "for=203.0.113.10")
	req.Header.Set("X-Forwarded-For", "203.0.113.10")
	req.Header.Set("X-Forwarded-Custom", "must-not-pass")
	req.Header.Set("X-Real-IP", "203.0.113.11")
	req.Header.Set("Proxy-Connection", "keep-alive")

	header := prepareWebSocketUpstreamHeaders(req, target, profile)
	if got := req.Header.Get("Forwarded"); got == "" {
		t.Fatal("preparing WebSocket headers mutated the inbound request")
	}
	for name, want := range map[string]string{
		"Connection":        "Upgrade",
		"Upgrade":           "websocket",
		"Host":              target.Host,
		"User-Agent":        profile.UserAgent,
		"X-Forwarded-For":   "198.51.100.25",
		"X-Real-IP":         "198.51.100.25",
		"X-Forwarded-Host":  "meridian.example:50001",
		"X-Forwarded-Proto": "http",
	} {
		if got := header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"Forwarded", "X-Forwarded-Custom", "Proxy-Connection"} {
		if got := header.Get(name); got != "" {
			t.Errorf("untrusted WebSocket header %s leaked upstream: %q", name, got)
		}
	}
}

func TestRateLimitedWriterUsesPerRequestProgress(t *testing.T) {
	var siteTraffic atomic.Int64
	siteTraffic.Store(10 << 20)
	recorder := httptest.NewRecorder()
	writer := &rateLimitedWriter{
		ResponseWriter: recorder,
		bytesPerSec:    1024,
		written:        &siteTraffic,
		start:          time.Now().Add(-time.Second),
	}
	payload := bytes.Repeat([]byte("x"), 512)
	n, err := writer.Write(payload)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(payload) || recorder.Body.Len() != len(payload) {
		t.Fatalf("wrote=%d body=%d, want %d", n, recorder.Body.Len(), len(payload))
	}
	if writer.requestWritten != int64(len(payload)) {
		t.Fatalf("requestWritten = %d, want %d", writer.requestWritten, len(payload))
	}
	if got := siteTraffic.Load(); got != (10<<20)+int64(len(payload)) {
		t.Fatalf("site traffic = %d, want %d", got, (10<<20)+len(payload))
	}
}

func TestMobileModalKeepsBodyScrollableAndActionsVisible(t *testing.T) {
	css, err := web.StaticFiles.ReadFile("static/css/style.css")
	if err != nil {
		t.Fatalf("read embedded CSS: %v", err)
	}
	for _, rule := range []string{
		"max-height: calc(100dvh - 48px)",
		"overflow-y: auto",
		"-webkit-overflow-scrolling: touch",
		".btn-modal { flex: 1; min-height: 44px",
	} {
		if !strings.Contains(string(css), rule) {
			t.Errorf("mobile modal CSS missing %q", rule)
		}
	}

	appJS, err := web.StaticFiles.ReadFile("static/js/app.js")
	if err != nil {
		t.Fatalf("read embedded app JavaScript: %v", err)
	}
	if !strings.Contains(string(appJS), "document.getElementById('modal-body').scrollTop = 0") {
		t.Error("opening a modal must reset the form scroll position")
	}

	sitesJS, err := web.StaticFiles.ReadFile("static/js/pages/sites.js")
	if err != nil {
		t.Fatalf("read embedded sites JavaScript: %v", err)
	}
	if !strings.Contains(string(sitesJS), "openModal({ closeOnBackdrop: false })") {
		t.Error("site add/edit form must not close when its backdrop is clicked")
	}
	for _, snippet := range []string{`id="m-speed"`, "speed_limit: parseInt(document.getElementById('m-speed').value || 0)"} {
		if !strings.Contains(string(sitesJS), snippet) {
			t.Errorf("site form must expose and submit speed limit; missing %q", snippet)
		}
	}

	indexHTML, err := web.StaticFiles.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read embedded index HTML: %v", err)
	}
	for _, asset := range []string{"/css/style.css?v=1.5.0", "/js/pages/sites.js?v=1.5.0", "/js/app.js?v=1.5.0"} {
		if !strings.Contains(string(indexHTML), asset) {
			t.Errorf("index must cache-bust updated asset %q", asset)
		}
	}
	if strings.Contains(string(indexHTML), "fonts.googleapis.com") || strings.Contains(string(indexHTML), "fonts.gstatic.com") {
		t.Error("index must not request fonts blocked by the Content-Security-Policy")
	}
}

func TestStaticHandlerDisablesCaching(t *testing.T) {
	staticFS, err := fs.Sub(web.StaticFiles, "static")
	if err != nil {
		t.Fatalf("static fs: %v", err)
	}
	rr := httptest.NewRecorder()
	staticHandler(staticFS).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/js/pages/sites.js", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store, no-cache, must-revalidate" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := rr.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q, want no-cache", got)
	}
	if got := rr.Header().Get("Expires"); got != "0" {
		t.Fatalf("Expires = %q, want 0", got)
	}
}

func TestAPIClientClearsRejectedStoredToken(t *testing.T) {
	apiJS, err := web.StaticFiles.ReadFile("static/js/api.js")
	if err != nil {
		t.Fatalf("read embedded API JavaScript: %v", err)
	}
	source := string(apiJS)
	for _, expected := range []string{"res.status === 401", "this.logout()", "window.location.reload()"} {
		if !strings.Contains(source, expected) {
			t.Errorf("API client missing %q", expected)
		}
	}
}

func TestRequestClientKeyUsesOnlyConfiguredTrustedProxy(t *testing.T) {
	trusted, err := parseTrustedProxyCIDRs("172.17.0.0/16")
	if err != nil {
		t.Fatalf("parse trusted proxies: %v", err)
	}

	trustedRequest := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	trustedRequest.RemoteAddr = "172.17.0.1:45678"
	trustedRequest.Header.Set("X-Real-IP", "203.0.113.25")
	if got := requestClientKey(trustedRequest, trusted); got != "203.0.113.25" {
		t.Fatalf("trusted proxy client key = %q", got)
	}

	untrustedRequest := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	untrustedRequest.RemoteAddr = "198.51.100.7:45678"
	untrustedRequest.Header.Set("X-Real-IP", "203.0.113.25")
	if got := requestClientKey(untrustedRequest, trusted); got != "198.51.100.7" {
		t.Fatalf("untrusted proxy client key = %q", got)
	}

	if _, err := parseTrustedProxyCIDRs("not-a-network"); err == nil {
		t.Fatal("invalid trusted proxy CIDR unexpectedly accepted")
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
	if got := mustUserCount(t, app.db); got != 0 {
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
	if got := mustUserCount(t, app.db); got != 1 {
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
	if got := mustUserCount(t, app.db); got != 1 {
		t.Fatalf("user count = %d, want 1", got)
	}
}

func TestVerifyUserAcceptsExistingXCryptoBcryptHash(t *testing.T) {
	app := newTestApp(t)
	// Compatibility vector generated by golang.org/x/crypto/bcrypt. Existing
	// installations must continue to authenticate after switching providers.
	const legacyHash = "$2a$10$XajjQvNhvvRt5GSeFk1xFeyqRrsxkhBkUiQeg0dt.wU1qD4aFDcga"
	result, err := app.db.db.Exec(
		"INSERT INTO users (username, password_hash) VALUES (?, ?)",
		"legacy-admin",
		legacyHash,
	)
	if err != nil {
		t.Fatalf("insert legacy user: %v", err)
	}
	wantID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("legacy user id: %v", err)
	}

	gotID, err := app.db.VerifyUser("legacy-admin", "allmine")
	if err != nil {
		t.Fatalf("VerifyUser rejected a legacy bcrypt hash: %v", err)
	}
	if gotID != wantID {
		t.Fatalf("VerifyUser id = %d, want %d", gotID, wantID)
	}
	if _, err := app.db.VerifyUser("legacy-admin", "not-the-password"); !errors.Is(err, errInvalidCredentials) {
		t.Fatalf("wrong password error = %v, want invalid credentials", err)
	}
}

func TestResetAdminPasswordUpdatesOnlyConfiguredAdministrator(t *testing.T) {
	app := newTestApp(t)
	const oldPassword = "correct horse battery staple"
	const newPassword = "new correct horse battery staple"
	if _, err := app.db.CreateInitialUser("admin", oldPassword); err != nil {
		t.Fatalf("CreateInitialUser: %v", err)
	}
	if err := app.db.ResetAdminPassword(newPassword); err != nil {
		t.Fatalf("ResetAdminPassword: %v", err)
	}
	if _, err := app.db.VerifyUser("admin", oldPassword); !errors.Is(err, errInvalidCredentials) {
		t.Fatalf("old password error = %v, want invalid credentials", err)
	}
	if _, err := app.db.VerifyUser("admin", newPassword); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
}

func TestResetAdminPasswordRejectsInvalidDatabaseStateAndLength(t *testing.T) {
	app := newTestApp(t)
	if err := app.db.ResetAdminPassword("long enough password"); !errors.Is(err, errAdminNotConfigured) {
		t.Fatalf("empty database error = %v, want administrator not configured", err)
	}
	if _, err := app.db.CreateUser("admin-one", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateUser one: %v", err)
	}
	if _, err := app.db.CreateUser("admin-two", "correct horse battery staple"); err != nil {
		t.Fatalf("CreateUser two: %v", err)
	}
	if err := app.db.ResetAdminPassword("another valid password"); !errors.Is(err, errMultipleAdmins) {
		t.Fatalf("multiple users error = %v, want multiple administrators", err)
	}
	for _, password := range []string{"too-short", strings.Repeat("x", 73)} {
		if err := app.db.ResetAdminPassword(password); !errors.Is(err, errInvalidAdminPassword) {
			t.Fatalf("password length %d error = %v, want invalid password", len(password), err)
		}
	}
}

func TestResetAdminPasswordAcceptsLengthBoundaries(t *testing.T) {
	for _, length := range []int{12, 72} {
		app := newTestApp(t)
		if _, err := app.db.CreateInitialUser("admin", "correct horse battery staple"); err != nil {
			t.Fatalf("CreateInitialUser: %v", err)
		}
		password := strings.Repeat("x", length)
		if err := app.db.ResetAdminPassword(password); err != nil {
			t.Fatalf("length %d rejected: %v", length, err)
		}
		if _, err := app.db.VerifyUser("admin", password); err != nil {
			t.Fatalf("length %d password did not verify: %v", length, err)
		}
	}
}

func TestAdminResetPasswordCommandReadsPasswordOnlyFromStdin(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "command.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if _, err := db.CreateInitialUser("admin", "correct horse battery staple"); err != nil {
		db.Close()
		t.Fatalf("CreateInitialUser: %v", err)
	}
	db.Close()

	const newPassword = "stdin-only replacement password"
	var output bytes.Buffer
	handled, err := runCommandLine(
		[]string{"admin", "reset-password", "--db", dbPath, "--password-stdin"},
		strings.NewReader(newPassword+"\n"),
		&output,
	)
	if err != nil {
		t.Fatalf("runCommandLine: %v", err)
	}
	if !handled {
		t.Fatal("admin command was not handled")
	}
	if strings.Contains(output.String(), newPassword) {
		t.Fatal("command output exposed the password")
	}

	verifyDB, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	defer verifyDB.Close()
	if _, err := verifyDB.VerifyUser("admin", newPassword); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
}

func TestAdminResetPasswordCommandRejectsUnsafeInputShapes(t *testing.T) {
	const misplacedPassword = "must-not-appear-in-errors"
	for _, tc := range []struct {
		name  string
		args  []string
		input string
	}{
		{name: "missing stdin flag", args: []string{"admin", "reset-password", "--db", "test.db"}, input: "valid replacement password\n"},
		{name: "password argument", args: []string{"admin", "reset-password", "--db", "test.db", "--password", misplacedPassword}},
		{name: "multiple lines", args: []string{"admin", "reset-password", "--db", "test.db", "--password-stdin"}, input: "valid replacement password\nsecond line\n"},
		{name: "too long", args: []string{"admin", "reset-password", "--db", "test.db", "--password-stdin"}, input: strings.Repeat("x", 73) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handled, err := runCommandLine(tc.args, strings.NewReader(tc.input), io.Discard)
			if !handled || err == nil {
				t.Fatalf("handled=%v err=%v, want handled error", handled, err)
			}
			if strings.Contains(err.Error(), misplacedPassword) {
				t.Fatal("command error exposed a password-shaped argument")
			}
		})
	}
}

func TestJWTSecretRotationInvalidatesExistingToken(t *testing.T) {
	originalSecret := jwtSecret
	originalEphemeral := jwtSecretEphemeral
	t.Cleanup(func() {
		jwtSecret = originalSecret
		jwtSecretEphemeral = originalEphemeral
	})

	jwtSecret = []byte("old-test-signing-secret-000000000000")
	token, err := generateToken(1, "admin")
	if err != nil {
		t.Fatalf("generateToken: %v", err)
	}
	jwtSecret = []byte("new-test-signing-secret-000000000000")
	if _, _, err := validateToken(token); err == nil {
		t.Fatal("token signed before JWT secret rotation remained valid")
	}
}

func TestPanelListenAddressSeparatesPanelFromSiteListeners(t *testing.T) {
	for _, tc := range []struct {
		name string
		bind string
		port int
		want string
	}{
		{name: "default", port: 9090, want: "0.0.0.0:9090"},
		{name: "loopback", bind: "127.0.0.1", port: 9090, want: "127.0.0.1:9090"},
		{name: "ipv6", bind: "::1", port: 9090, want: "[::1]:9090"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := panelListenAddress(tc.bind, tc.port)
			if err != nil || got != tc.want {
				t.Fatalf("panelListenAddress() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		bind string
		port int
	}{
		{bind: "panel.example.com", port: 9090},
		{bind: "127.0.0.1", port: 0},
		{bind: "127.0.0.1", port: 65536},
	} {
		if _, err := panelListenAddress(tc.bind, tc.port); err == nil {
			t.Fatalf("panelListenAddress(%q, %d) unexpectedly succeeded", tc.bind, tc.port)
		}
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

func TestDatabaseReadFailuresAreReported(t *testing.T) {
	app := newTestApp(t)
	app.db.Close()
	if _, err := app.db.UserCount(); err == nil {
		t.Fatal("UserCount unexpectedly ignored a closed database")
	}
	if _, err := app.db.DashboardStats(); err == nil {
		t.Fatal("DashboardStats unexpectedly ignored a closed database")
	}
	if _, err := app.pm.StartAllEnabled(); err == nil {
		t.Fatal("StartAllEnabled unexpectedly ignored a closed database")
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/check", nil)
	app.handleAuthCheck(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("auth check status = %d, want 500; body=%s", rr.Code, rr.Body.String())
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
	inst := &ProxyInstance{Site: *site, startedAt: time.Now().Add(-3 * time.Second)}
	inst.reqCount.Store(7)
	app.pm.proxies[site.ID] = inst

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
	if !result.Proxy.Running {
		t.Fatal("proxy.running = false, want true")
	}
	if result.Proxy.TotalReqs != 7 {
		t.Fatalf("proxy.total_requests = %d, want 7", result.Proxy.TotalReqs)
	}
	if result.Proxy.Uptime == "" {
		t.Fatal("proxy.uptime is empty for a running site")
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

func TestHandleSiteUpdatePreservesOmittedSpeedLimit(t *testing.T) {
	app := newTestApp(t)
	port := freePort(t)
	site, err := app.db.CreateSite("limited", port, "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 25)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if enabled, err := app.db.ToggleSite(site.ID); err != nil || enabled {
		t.Fatalf("disable site: enabled=%v err=%v", enabled, err)
	}

	body := strings.NewReader(`{"name":"limited","listen_port":` + jsonNumber(port) + `,"target_url":"http://127.0.0.1:8096","ua_mode":"infuse"}`)
	req := httptest.NewRequest(http.MethodPut, "/api/sites/"+jsonNumber64(site.ID), body)
	rr := httptest.NewRecorder()
	app.handleSiteByID(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}

	reloaded, err := app.db.GetSite(site.ID)
	if err != nil {
		t.Fatalf("GetSite: %v", err)
	}
	if reloaded.SpeedLimit != 25 {
		t.Fatalf("speed_limit = %d, want preserved value 25", reloaded.SpeedLimit)
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
	if got := rr.Result().Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
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

func TestStartSiteRejectsCorruptStreamHosts(t *testing.T) {
	app := newTestApp(t)
	base := Site{
		ID:           999,
		Name:         "corrupt-stream-hosts",
		ListenPort:   freePort(t),
		TargetURL:    "http://127.0.0.1:8096",
		PlaybackMode: "direct",
		UAMode:       "infuse",
		Enabled:      true,
	}

	invalidJSON := base
	invalidJSON.StreamHosts = "{"
	if err := app.pm.StartSite(invalidJSON); err == nil || !strings.Contains(err.Error(), "invalid stream_hosts") {
		t.Fatalf("invalid JSON error = %v", err)
	}

	invalidURL := base
	invalidURL.StreamHosts = `["file://media.example.com/path"]`
	if err := app.pm.StartSite(invalidURL); err == nil || !strings.Contains(err.Error(), "invalid stream host") {
		t.Fatalf("invalid stream host error = %v", err)
	}
	if app.pm.IsRunning(base.ID) {
		t.Fatal("corrupt site unexpectedly started")
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

func TestProxyPreservesConfiguredUpstreamBasePath(t *testing.T) {
	app := newTestApp(t)
	received := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.URL.RequestURI()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	site, err := app.db.CreateSite("base-path", freePort(t), upstream.URL+"/emby?from=base", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatalf("StartSite: %v", err)
	}
	t.Cleanup(func() { app.pm.StopSite(site.ID) })

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/System/Info/Public?client=1", site.ListenPort))
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}

	select {
	case got := <-received:
		if got != "/emby/System/Info/Public?from=base&client=1" {
			t.Fatalf("upstream request URI = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not receive request")
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
