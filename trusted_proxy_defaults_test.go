package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrustedProxyDefaultsAcceptOnlyPrivateAndLoopbackPeers(t *testing.T) {
	for _, value := range []string{"", " \n\t"} {
		trusted, err := parseTrustedProxyCIDRs(value)
		if err != nil {
			t.Fatal(err)
		}
		expected := []string{"127.0.0.1/32", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}
		if len(trusted) != len(expected) {
			t.Fatalf("default proxy ranges = %v, want loopback and private ranges", trusted)
		}
		for index, network := range trusted {
			if network.String() != expected[index] {
				t.Fatalf("default proxy range %d = %s, want %s", index, network.String(), expected[index])
			}
		}
		for _, testCase := range []struct {
			address string
			trusted bool
		}{
			{address: "127.0.0.1", trusted: true},
			{address: "::1", trusted: true},
			{address: "127.0.0.2", trusted: false},
			{address: "::2", trusted: false},
			{address: "10.42.0.1", trusted: true},
			{address: "172.15.255.255", trusted: false},
			{address: "172.16.0.1", trusted: true},
			{address: "172.17.0.1", trusted: true},
			{address: "172.31.255.254", trusted: true},
			{address: "172.32.0.1", trusted: false},
			{address: "192.168.1.1", trusted: true},
			{address: "192.169.1.1", trusted: false},
			{address: "fd12:3456::1", trusted: true},
			{address: "fc00::1", trusted: true},
			{address: "fe80::1", trusted: false},
			{address: "2001:db8::1", trusted: false},
			{address: "169.254.169.254", trusted: false},
			{address: "100.64.0.1", trusted: false},
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
		{name: "private 10 proxy", peer: "10.42.0.1:43210", wantClient: "203.0.113.25", wantHTTPS: true},
		{name: "Docker bridge proxy", peer: "172.17.0.1:43210", wantClient: "203.0.113.25", wantHTTPS: true},
		{name: "private 192 proxy", peer: "192.168.1.1:43210", wantClient: "203.0.113.25", wantHTTPS: true},
		{name: "IPv6 private proxy", peer: "[fd12:3456::1]:43210", wantClient: "203.0.113.25", wantHTTPS: true},
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
