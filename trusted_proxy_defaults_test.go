package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrustedProxyDefaultsAcceptOnlyExactLoopbackPeers(t *testing.T) {
	for _, value := range []string{"", " \n\t"} {
		trusted, err := parseTrustedProxyCIDRs(value)
		if err != nil {
			t.Fatal(err)
		}
		if len(trusted) != 2 || trusted[0].String() != "127.0.0.1/32" || trusted[1].String() != "::1/128" {
			t.Fatalf("default proxy ranges = %v, want exact IPv4 and IPv6 loopback", trusted)
		}
		for _, testCase := range []struct {
			address string
			trusted bool
		}{
			{address: "127.0.0.1", trusted: true},
			{address: "::1", trusted: true},
			{address: "127.0.0.2", trusted: false},
			{address: "::2", trusted: false},
			{address: "172.17.0.1", trusted: false},
			{address: "198.51.100.7", trusted: false},
		} {
			if got := isTrustedProxy(net.ParseIP(testCase.address), trusted); got != testCase.trusted {
				t.Fatalf("default trust for %s = %t, want %t", testCase.address, got, testCase.trusted)
			}
		}
	}
}

func TestTrustedProxyDefaultsForwardClientIdentityAndHTTPS(t *testing.T) {
	trusted, err := parseTrustedProxyCIDRs("")
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name       string
		peer       string
		wantClient string
		wantHTTPS  bool
	}{
		{name: "local IPv4 proxy", peer: "127.0.0.1:43210", wantClient: "203.0.113.25", wantHTTPS: true},
		{name: "local IPv6 proxy", peer: "[::1]:43210", wantClient: "203.0.113.25", wantHTTPS: true},
		{name: "other loopback address", peer: "127.0.0.2:43210", wantClient: "127.0.0.2", wantHTTPS: false},
		{name: "remote client", peer: "198.51.100.7:43210", wantClient: "198.51.100.7", wantHTTPS: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://panel.example.com/api/auth/login", nil)
			request.RemoteAddr = testCase.peer
			request.Header.Set("X-Real-IP", "203.0.113.25")
			request.Header.Set("X-Forwarded-For", "192.0.2.99, 203.0.113.25")
			request.Header.Set("X-Forwarded-Proto", "https")
			request.Header.Set("Origin", "https://panel.example.com")
			if got := requestClientKey(request, trusted); got != testCase.wantClient {
				t.Fatalf("client key = %q, want %q", got, testCase.wantClient)
			}
			if got := requestIsHTTPS(request, trusted); got != testCase.wantHTTPS {
				t.Fatalf("HTTPS = %t, want %t", got, testCase.wantHTTPS)
			}
			if got := requestHasSameOriginWithProxies(request, trusted); got != testCase.wantHTTPS {
				t.Fatalf("same origin = %t, want %t", got, testCase.wantHTTPS)
			}
			app := &App{trustedProxies: trusted}
			response := httptest.NewRecorder()
			app.setSessionCookie(response, request, "test-session")
			cookies := response.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Secure != testCase.wantHTTPS {
				t.Fatalf("unexpected session cookie security: %+v", cookies)
			}
		})
	}
}

func TestTrustedProxyExplicitConfigurationReplacesDefaults(t *testing.T) {
	trusted, err := parseTrustedProxyCIDRs("172.18.0.5/32")
	if err != nil {
		t.Fatal(err)
	}
	if len(trusted) != 1 || !isTrustedProxy(net.ParseIP("172.18.0.5"), trusted) || isTrustedProxy(net.ParseIP("127.0.0.1"), trusted) || isTrustedProxy(net.ParseIP("::1"), trusted) {
		t.Fatalf("explicit proxy configuration unexpectedly includes default ranges: %v", trusted)
	}
	if _, err := parseTrustedProxyCIDRs("127.0.0.1/32,invalid-network"); err == nil {
		t.Fatal("invalid custom proxy range was accepted")
	}
}
