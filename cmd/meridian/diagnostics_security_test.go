package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type diagnosticResolverFunc func(context.Context, string) ([]net.IPAddr, error)

func (resolve diagnosticResolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return resolve(ctx, host)
}

func TestDiagnosticDestinationPolicyPreservesPrivateOrigins(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1", "::1", "10.0.0.8", "172.20.0.2", "192.168.10.2",
		"fc00::1", "fd12:3456::2", "100.64.1.9", "8.8.8.8", "2001:4860:4860::8888",
		"::ffff:192.168.1.8", "64:ff9b::808:808",
	} {
		t.Run(raw, func(t *testing.T) {
			if !diagnosticIPAllowed(netip.MustParseAddr(raw)) {
				t.Fatalf("normal local/private/public origin %s was rejected", raw)
			}
		})
	}
	for _, raw := range []string{
		"0.0.0.0", "0.1.2.3", "::", "169.254.169.254", "169.254.170.2",
		"169.254.1.1", "fe80::1", "fe80::1%eth0", "224.0.0.1", "239.0.0.1", "ff02::1",
		"255.255.255.255", "240.0.0.8", "100.100.100.200", "168.63.129.16",
		"fd00:ec2::254", "::ffff:169.254.169.254", "::ffff:100.100.100.200",
		"64:ff9b::a9fe:a9fe", "64:ff9b::6464:64c8",
	} {
		t.Run(raw, func(t *testing.T) {
			if diagnosticIPAllowed(netip.MustParseAddr(raw)) {
				t.Fatalf("special diagnostic destination %s was accepted", raw)
			}
		})
	}
}

func TestDiagnosticDialerRejectsUnsafeAnswersBeforeAnyConnection(t *testing.T) {
	for _, test := range []struct {
		name    string
		host    string
		answers []net.IPAddr
	}{
		{name: "literal IPv4 metadata", host: "169.254.169.254"},
		{name: "literal IPv6 metadata", host: "fd00:ec2::254"},
		{name: "mapped metadata", host: "::ffff:169.254.169.254"},
		{name: "reserved hostname", host: "MeTaDaTa.GoOgLe.InTeRnAl.", answers: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}},
		{name: "blocked DNS", host: "origin.test", answers: []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}},
		{name: "safe first mixed DNS", host: "origin.test", answers: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}, {IP: net.ParseIP("100.100.100.200")}}},
		{name: "mapped mixed DNS", host: "origin.test", answers: []net.IPAddr{{IP: net.ParseIP("192.168.1.8")}, {IP: net.ParseIP("::ffff:169.254.169.254")}}},
		{name: "invalid DNS answer", host: "origin.test", answers: []net.IPAddr{{IP: nil}}},
		{name: "zone DNS answer", host: "origin.test", answers: []net.IPAddr{{IP: net.ParseIP("fd12::2"), Zone: "eth0"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			dialer := &diagnosticDialer{
				resolver: diagnosticResolverFunc(func(_ context.Context, _ string) ([]net.IPAddr, error) { return test.answers, nil }),
				dialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
					calls++
					return nil, errors.New("unexpected connection")
				},
			}
			_, err := dialer.DialContext(context.Background(), "tcp", net.JoinHostPort(test.host, "80"))
			if !errors.Is(err, errDiagnosticTargetForbidden) || calls != 0 {
				t.Fatalf("dial = %v, connection calls = %d; want policy rejection before any connection", err, calls)
			}
		})
	}
}

func TestDiagnosticHTTPPinsValidatedDNSAndRejectsRebinding(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasPrefix(r.Host, "origin.test:") {
			t.Errorf("HTTP Host = %q, want original authority", r.Host)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	parsed, _ := url.Parse(upstream.URL)
	var lookups atomic.Int64
	var addresses []string
	dialer := &diagnosticDialer{
		resolver: diagnosticResolverFunc(func(_ context.Context, host string) ([]net.IPAddr, error) {
			if host != "origin.test" {
				t.Errorf("lookup host = %q", host)
			}
			if lookups.Add(1) == 1 {
				return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
			}
			return []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}, nil
		}),
		dialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			addresses = append(addresses, address)
			return (&net.Dialer{}).DialContext(ctx, network, address)
		},
		timeout: time.Second,
	}
	client := newProbeClient(time.Second)
	defer client.CloseIdleConnections()
	transport := client.Transport.(*http.Transport)
	transport.DialContext = dialer.DialContext
	transport.DisableKeepAlives = true
	endpoint := "http://" + net.JoinHostPort("origin.test", parsed.Port()) + "/System/Info/Public"
	response, err := client.Get(endpoint)
	if err != nil {
		t.Fatalf("first checked request: %v", err)
	}
	response.Body.Close()
	if len(addresses) != 1 || addresses[0] != parsed.Host || lookups.Load() != 1 {
		t.Fatalf("addresses = %v, lookups = %d; want exactly the validated literal", addresses, lookups.Load())
	}
	_, err = client.Get(endpoint)
	if !errors.Is(err, errDiagnosticTargetForbidden) || hits.Load() != 1 || len(addresses) != 1 {
		t.Fatalf("rebound request = %v, hits = %d, addresses = %v; want no second connection", err, hits.Load(), addresses)
	}
}

func TestDiagnosticTLSUsesDestinationPolicyAndCertificateVerification(t *testing.T) {
	for _, answers := range [][]net.IPAddr{
		{{IP: net.ParseIP("fd00:ec2::254")}},
		{{IP: net.ParseIP("127.0.0.1")}, {IP: net.ParseIP("169.254.169.254")}},
	} {
		calls := 0
		dialer := &diagnosticDialer{
			resolver: diagnosticResolverFunc(func(_ context.Context, _ string) ([]net.IPAddr, error) { return answers, nil }),
			dialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				calls++
				return nil, errors.New("unexpected connection")
			},
		}
		target, _ := url.Parse("https://origin.test:443")
		result := probeSiteTLSWithDialer(target, dialer)
		if !result.Enabled || result.Valid || result.Error != errDiagnosticTargetForbidden.Error() || calls != 0 {
			t.Fatalf("TLS result = %+v, dial calls = %d", result, calls)
		}
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("untrusted certificate must stop the TLS diagnostic before HTTP")
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	result := probeSiteTLS(target)
	if !result.Enabled || result.Valid || result.Error == "" {
		t.Fatalf("self-signed certificate accepted: %+v", result)
	}
}

func TestDiagnosticDialerBoundsDNSLookup(t *testing.T) {
	dialer := &diagnosticDialer{
		resolver: diagnosticResolverFunc(func(ctx context.Context, _ string) ([]net.IPAddr, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}),
		dialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
			t.Error("timed-out DNS must not connect")
			return nil, errors.New("unexpected connection")
		},
		timeout: 20 * time.Millisecond,
	}
	_, err := dialer.DialContext(context.Background(), "tcp", "origin.test:80")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DNS timeout = %v, want context deadline", err)
	}
}

func TestDiagnosticDialerKeepsBudgetForReachableFallback(t *testing.T) {
	for _, test := range []struct {
		name           string
		network        string
		answers        []net.IPAddr
		dialerTimeout  time.Duration
		requestTimeout time.Duration
	}{
		{
			name:          "IPv4 after unreachable IPv6",
			network:       "tcp",
			answers:       []net.IPAddr{{IP: net.ParseIP("fd12::1")}, {IP: net.ParseIP("127.0.0.1")}},
			dialerTimeout: 100 * time.Millisecond,
		},
		{
			name:           "request deadline bounds candidate budgets",
			network:        "tcp",
			answers:        []net.IPAddr{{IP: net.ParseIP("fd12::1")}, {IP: net.ParseIP("127.0.0.1")}},
			dialerTimeout:  time.Second,
			requestTimeout: 100 * time.Millisecond,
		},
		{
			name:          "only matching family shares the budget",
			network:       "tcp4",
			answers:       []net.IPAddr{{IP: net.ParseIP("fd12::1")}, {IP: net.ParseIP("127.0.0.2")}, {IP: net.ParseIP("127.0.0.1")}},
			dialerTimeout: 100 * time.Millisecond,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var attempted []string
			dialer := &diagnosticDialer{
				resolver: diagnosticResolverFunc(func(context.Context, string) ([]net.IPAddr, error) { return test.answers, nil }),
				dialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
					attempted = append(attempted, address)
					if !strings.HasPrefix(address, "127.0.0.1:") {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					if err := ctx.Err(); err != nil {
						return nil, err
					}
					connection, peer := net.Pipe()
					peer.Close()
					return connection, nil
				},
				timeout: test.dialerTimeout,
			}
			ctx := context.Background()
			if test.requestTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, test.requestTimeout)
				defer cancel()
			}
			connection, err := dialer.DialContext(ctx, test.network, "dualstack.test:443")
			if connection != nil {
				connection.Close()
			}
			if err != nil || len(attempted) != 2 || attempted[1] != "127.0.0.1:443" {
				t.Fatalf("fallback = %v, attempted = %v; want the reachable checked IPv4", err, attempted)
			}
		})
	}
}

func TestDiagnosticDialerStopsFallbackOnRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	dialer := &diagnosticDialer{
		resolver: diagnosticResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("fd12::1")}, {IP: net.ParseIP("127.0.0.1")}}, nil
		}),
		dialContext: func(context.Context, string, string) (net.Conn, error) {
			attempts++
			cancel()
			return nil, context.Canceled
		},
		timeout: time.Second,
	}
	_, err := dialer.DialContext(ctx, "tcp", "dualstack.test:443")
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("cancelled fallback = %v, attempts = %d; want one attempt", err, attempts)
	}
}

func TestUpstreamTestAdmissionPreservesLineBatchAndRefills(t *testing.T) {
	var admission upstreamTestAdmission
	now := time.Unix(1_000, 0)
	for index := range upstreamTestBurst {
		release, ok := admission.acquire(now)
		if !ok {
			t.Fatalf("serial line test %d was rejected within the burst", index+1)
		}
		release()
		release() // Defensive release must not corrupt the concurrent count.
	}
	if _, ok := admission.acquire(now); ok {
		t.Fatal("request beyond the burst was admitted")
	}
	release, ok := admission.acquire(now.Add(upstreamTestRefill))
	if !ok {
		t.Fatal("budget did not refill after one second")
	}
	release()
	if _, ok := admission.acquire(now.Add(upstreamTestRefill)); ok {
		t.Fatal("one elapsed interval refilled more than one request")
	}
	var independent upstreamTestAdmission
	if release, ok := independent.acquire(now); !ok {
		t.Fatal("separate controller inherited another controller's exhausted budget")
	} else {
		release()
	}
}

type diagnosticLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (buffer *diagnosticLogBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.Buffer.Write(data)
}

func (buffer *diagnosticLogBuffer) contents() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.Buffer.String()
}

func TestUpstreamTestKeepsAuthenticationAndRedactsAudit(t *testing.T) {
	app := newTestApp(t)
	id, err := app.db.CreateInitialUser("admin", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	token, err := generateToken(id, "admin")
	if err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.URL.RawQuery != "" {
			t.Error("incoming credentials or target query were forwarded")
		}
		_, _ = io.WriteString(w, `{"Version":"1.0","secret":"response-body-secret"}`)
	}))
	defer upstream.Close()
	var captured diagnosticLogBuffer
	previousLog := log.Writer()
	log.SetOutput(&captured)
	defer log.SetOutput(previousLog)
	target := upstream.URL + "/sensitive-path?api_key=target-query-secret"
	body, _ := json.Marshal(map[string]string{"target_url": target})
	protected := app.authMiddleware(app.handleUpstreamTest)
	for _, test := range []struct {
		name   string
		cookie bool
		origin string
		want   int
	}{
		{name: "anonymous", origin: "https://panel.example", want: http.StatusUnauthorized},
		{name: "cross origin", cookie: true, origin: "https://other.example", want: http.StatusForbidden},
		{name: "missing origin", cookie: true, want: http.StatusForbidden},
		{name: "authenticated", cookie: true, origin: "https://panel.example", want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "https://panel.example/api/upstream-test", bytes.NewReader(body))
			request.Header.Set("Authorization", "Bearer incoming-bearer-secret")
			if test.cookie {
				request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token})
			}
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			recorder := httptest.NewRecorder()
			before := hits.Load()
			protected(recorder, request)
			if recorder.Code != test.want {
				t.Fatalf("status = %d body=%s, want %d", recorder.Code, recorder.Body.String(), test.want)
			}
			if test.want != http.StatusOK && hits.Load() != before {
				t.Fatal("rejected request reached an upstream")
			}
			if strings.Contains(recorder.Body.String(), "response-body-secret") {
				t.Fatal("upstream body exposed in test response")
			}
		})
	}
	output := captured.contents()
	if !strings.Contains(output, `upstream-test host="127.0.0.1" result=online`) {
		t.Fatalf("missing safe audit event: %s", output)
	}
	for _, secret := range []string{"sensitive-path", "target-query-secret", "incoming-bearer-secret", "response-body-secret", token} {
		if strings.Contains(output, secret) {
			t.Fatalf("audit log exposed sensitive test value %q", secret)
		}
	}
}

func TestDiagnosticClientDisablesEnvironmentProxy(t *testing.T) {
	client := newProbeClient(time.Second)
	defer client.CloseIdleConnections()
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("diagnostic transport must dial validated destinations directly, without environment proxies")
	}
}

func TestUpstreamTestLimitsConcurrentRequests(t *testing.T) {
	app := newTestApp(t)
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var attempts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	body, _ := json.Marshal(map[string]string{"target_url": upstream.URL})
	done := make(chan *httptest.ResponseRecorder, 5)
	request := func() {
		recorder := httptest.NewRecorder()
		app.handleUpstreamTest(recorder, httptest.NewRequest(http.MethodPost, "/api/upstream-test", bytes.NewReader(body)))
		done <- recorder
	}
	for range 4 {
		go request()
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("allowed test did not reach the upstream")
		}
	}
	go request()
	var limited *httptest.ResponseRecorder
	select {
	case limited = <-done:
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if limited == nil {
		for range 5 {
			<-done
		}
		t.Fatal("fifth concurrent test was allowed to dial instead of being rejected")
	}
	for range 4 {
		<-done
	}
	if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("limited response = %d %s, want 429 with Retry-After", limited.Code, limited.Body.String())
	}
	if attempts.Load() != 4 {
		t.Fatalf("upstream attempts = %d, want only the four admitted tests", attempts.Load())
	}
}
