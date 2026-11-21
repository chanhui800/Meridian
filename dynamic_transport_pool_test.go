package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func dynamicPoolTestState(t testing.TB, server *httptest.Server, resolver dynamicIPResolver) (*dynamicRuntime, *dynamicSiteState, *atomic.Int64) {
	t.Helper()
	runtime := newDynamicRuntime()
	if resolver == nil {
		resolver = dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
		})
	}
	runtime.resolver = resolver
	policy, err := newDynamicSelfTargetPolicy("panel.example.net", 8080, nil, func() ([]net.Addr, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	runtime.selfTargets.Store(policy)
	state := newDynamicSiteState(runtime, dynamicDefaultProfileLimits())
	t.Cleanup(state.close)
	dials := &atomic.Int64{}
	state.transports.build = dynamicPoolTestBuilder(server, dials)
	return runtime, state, dials
}

func dynamicPoolTestBuilder(server *httptest.Server, dials *atomic.Int64) dynamicTransportFactory {
	return func(target *url.URL, pins []net.IP, policy *dynamicSelfTargetPolicy) (*http.Transport, error) {
		transport, err := newDynamicTransportWithDialer(target, pins, func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil || port != target.Port() {
				return nil, fmt.Errorf("unexpected pinned address %q", address)
			}
			matched := false
			for _, ip := range pins {
				matched = matched || host == ip.String()
			}
			if !matched {
				return nil, fmt.Errorf("unpinned dial %q", address)
			}
			dials.Add(1)
			dialer := &net.Dialer{Timeout: time.Second}
			return dialer.DialContext(ctx, network, server.Listener.Addr().String())
		}, policy)
		if err == nil && server.TLS != nil {
			roots := x509.NewCertPool()
			roots.AddCert(server.Certificate())
			transport.TLSClientConfig.RootCAs = roots
		}
		return transport, err
	}
}

func dynamicPoolTestTarget(t testing.TB, value string) *url.URL {
	t.Helper()
	target, err := normalizeDynamicURL(value)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

// Exercise the production reserve/DNS/pin/permit lifecycle, while allowing a
// benchmark to select the original dedicated transport factory for comparison.
func dynamicPoolTestFetch(ctx context.Context, state *dynamicSiteState, target *url.URL, factory dynamicTransportFactory) (*http.Response, error) {
	policy := state.runtime.selfTargets.Load()
	reservation, reason := state.reserveAuthority(dynamicCanonicalAuthority(target), time.Now())
	if reason != "" {
		return nil, newDynamicProxyError(reason)
	}
	defer reservation.rollback()
	pins, reason := reservation.resolve(ctx, target, policy)
	if reason != "" {
		return nil, newDynamicProxyError(reason)
	}
	release, acquired := state.acquireStream()
	if !acquired {
		return nil, newDynamicProxyError(dynamicObservationReasonCapacityLimit)
	}
	lease, err := borrowDynamicTransport(ctx, state, target, pins, policy, factory)
	if err != nil {
		release()
		return nil, err
	}
	request, err := http.NewRequestWithContext(isolateDynamicOutboundContext(ctx), http.MethodGet, target.String(), nil)
	if err != nil {
		lease.Close()
		release()
		return nil, err
	}
	request.Close = !lease.pooled()
	request.Header.Set("Range", "bytes=0-2047")
	response, err := lease.RoundTrip(request)
	if err != nil || response == nil {
		lease.Close()
		release()
		return response, err
	}
	reservation.commit()
	if response.Body == nil {
		response.Body = http.NoBody
	}
	response.Body = &dynamicResponseBody{ReadCloser: response.Body, release: release}
	return response, nil
}

func dynamicPoolTestConsume(response *http.Response) error {
	if response == nil {
		return fmt.Errorf("response is nil")
	}
	_, err := io.Copy(io.Discard, response.Body)
	closeErr := response.Body.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func dynamicPoolTestCounts(pool *dynamicTransportPool) (entries, leases int) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	entries = len(pool.entries)
	for entry := range pool.entries {
		leases += entry.leases
	}
	return entries, leases
}

func TestDynamicTransportPoolReusesValidatedRangeRequests(t *testing.T) {
	var lookups, closeRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Close {
			closeRequests.Add(1)
		}
		if r.Header.Get("Range") != "bytes=0-2047" || r.Host != "cdn.example.com:80" {
			t.Errorf("range/host=%q/%q", r.Header.Get("Range"), r.Host)
		}
		w.Header().Set("Content-Length", "2048")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, strings.Repeat("x", 2048))
	}))
	defer server.Close()
	runtime, state, dials := dynamicPoolTestState(t, server, dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		if lookups.Add(1)%2 == 0 {
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("1.1.1.1")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}, {IP: net.ParseIP("8.8.8.8")}}, nil
	}))
	target := dynamicPoolTestTarget(t, "http://cdn.example.com/media")
	for range 8 {
		response, err := dynamicPoolTestFetch(context.Background(), state, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := dynamicPoolTestConsume(response); err != nil {
			t.Fatal(err)
		}
	}
	if dials.Load() != 1 || lookups.Load() != 8 || closeRequests.Load() != 0 {
		t.Fatalf("dials/DNS/Close=%d/%d/%d, want 1/8/0", dials.Load(), lookups.Load(), closeRequests.Load())
	}
	if entries, leases := dynamicPoolTestCounts(state.transports); entries != 1 || leases != 0 || runtime.transportPoolEntries.Load() != 1 {
		t.Fatalf("pool entries/leases/global=%d/%d/%d", entries, leases, runtime.transportPoolEntries.Load())
	}
	state.transports.mu.Lock()
	for entry := range state.transports.entries {
		transport := entry.transport
		if transport.Proxy != nil || transport.DisableKeepAlives || transport.ForceAttemptHTTP2 || transport.MaxConnsPerHost <= 1 || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.ClientSessionCache != nil {
			t.Errorf("unsafe pooled transport: %#v", transport)
		}
	}
	state.transports.mu.Unlock()
}

func TestDynamicTransportPoolRefreshesDNSWhileOldStreamIsLeased(t *testing.T) {
	var lookups atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "2048")
		_, _ = io.WriteString(w, strings.Repeat("x", 2048))
	}))
	defer server.Close()
	_, state, dials := dynamicPoolTestState(t, server, dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		switch lookups.Add(1) {
		case 1:
			return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
		case 2:
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
		default:
			return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")}}, nil
		}
	}))
	target := dynamicPoolTestTarget(t, "http://cdn.example.com/media")
	policy := state.runtime.selfTargets.Load()
	old, reason := state.reserveAuthority(dynamicCanonicalAuthority(target), time.Now())
	if reason != "" {
		t.Fatal(reason)
	}
	defer old.rollback()
	if _, reason := old.resolve(context.Background(), target, policy); reason != "" {
		t.Fatal(reason)
	}
	// Keep the original authority reservation in flight while the next
	// request resolves a changed address and then a forbidden mixed answer.
	response, err := dynamicPoolTestFetch(context.Background(), state, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := dynamicPoolTestConsume(response); err != nil {
		t.Fatal(err)
	}
	if _, err := dynamicPoolTestFetch(context.Background(), state, target, nil); err == nil {
		t.Fatal("mixed public/private rebinding response reached the pool")
	}
	if lookups.Load() != 3 || dials.Load() != 1 {
		t.Fatalf("lookup/dial=%d/%d, want 3/1", lookups.Load(), dials.Load())
	}
	if entries, _ := dynamicPoolTestCounts(state.transports); entries != 0 {
		t.Fatalf("denied DNS left %d old idle transports", entries)
	}
}

func TestDynamicTransportPoolIsolatesPolicySiteAndAddressChanges(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer server.Close()
	var answer atomic.Int32
	answer.Store(1)
	runtime, first, dials := dynamicPoolTestState(t, server, dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		ip := "1.1.1.1"
		if answer.Load() == 2 {
			ip = "8.8.8.8"
		}
		return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
	}))
	second := newDynamicSiteState(runtime, dynamicDefaultProfileLimits())
	t.Cleanup(second.close)
	second.transports.build = dynamicPoolTestBuilder(server, dials)
	target := dynamicPoolTestTarget(t, "http://cdn.example.com/media")
	fetch := func(state *dynamicSiteState) {
		t.Helper()
		response, err := dynamicPoolTestFetch(context.Background(), state, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := dynamicPoolTestConsume(response); err != nil {
			t.Fatal(err)
		}
	}
	fetch(first)
	fetch(second)
	if dials.Load() != 2 {
		t.Fatalf("separate site instances shared a connection: dials=%d", dials.Load())
	}
	answer.Store(2)
	fetch(first)
	if dials.Load() != 3 {
		t.Fatalf("changed DNS set reused old connection: dials=%d", dials.Load())
	}
	policy, err := newDynamicSelfTargetPolicy("panel.example.net", 8080, nil, func() ([]net.Addr, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	runtime.selfTargets.Store(policy)
	if entries, _ := dynamicPoolTestCounts(first.transports); entries != 0 {
		t.Fatalf("policy replacement retained %d old idle entries", entries)
	}
	fetch(first)
	if dials.Load() != 4 {
		t.Fatalf("new policy generation reused old connection: dials=%d", dials.Load())
	}
	denied, err := newDynamicSelfTargetPolicy("cdn.example.com", 8080, nil, func() ([]net.Addr, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	runtime.selfTargets.Store(denied)
	if _, err := dynamicPoolTestFetch(context.Background(), first, target, nil); err == nil || dials.Load() != 4 {
		t.Fatalf("new self-target policy was bypassed: err=%v dials=%d", err, dials.Load())
	}
}

func TestDynamicTransportPoolEOFReuseAndIncompleteBodies(t *testing.T) {
	for _, mode := range []string{"eof_before_close", "partial_close", "canceled_read", "upstream_read_error"} {
		t.Run(mode, func(t *testing.T) {
			var served atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "65536")
				if served.Add(1) == 1 && mode == "upstream_read_error" {
					_, _ = io.WriteString(w, "partial")
					return
				}
				_, _ = io.WriteString(w, strings.Repeat("x", 65536))
			}))
			defer server.Close()
			_, state, dials := dynamicPoolTestState(t, server, nil)
			target := dynamicPoolTestTarget(t, "http://cdn.example.com/media")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first, err := dynamicPoolTestFetch(ctx, state, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Body.Close()
			if mode == "upstream_read_error" {
				if _, err := io.Copy(io.Discard, first.Body); err == nil {
					t.Fatal("truncated upstream response did not report a read error")
				}
				_ = first.Body.Close()
			} else if mode == "eof_before_close" {
				if _, err := io.Copy(io.Discard, first.Body); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := io.ReadFull(first.Body, make([]byte, 1)); err != nil {
					t.Fatal(err)
				}
				if mode == "canceled_read" {
					cancel()
					if _, err := io.Copy(io.Discard, first.Body); err == nil {
						t.Fatal("canceled partial response read succeeded")
					}
				}
				_ = first.Body.Close()
			}
			second, err := dynamicPoolTestFetch(context.Background(), state, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := dynamicPoolTestConsume(second); err != nil {
				t.Fatal(err)
			}
			cancel()
			_ = first.Body.Close()
			third, err := dynamicPoolTestFetch(context.Background(), state, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := dynamicPoolTestConsume(third); err != nil {
				t.Fatal(err)
			}
			want := int64(2)
			if mode == "eof_before_close" {
				want = 1
			}
			if dials.Load() != want {
				t.Fatalf("dials=%d, want %d", dials.Load(), want)
			}
			if _, leases := dynamicPoolTestCounts(state.transports); leases != 0 {
				t.Fatalf("response completion leaked %d leases", leases)
			}
		})
	}
}

func TestDynamicTransportPoolConcurrentResourcesDoNotSerialize(t *testing.T) {
	const concurrent = 4
	allowBodies := make(chan struct{})
	var unblock sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "2")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-allowBodies
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	defer unblock.Do(func() { close(allowBodies) })
	_, state, dials := dynamicPoolTestState(t, server, nil)
	target := dynamicPoolTestTarget(t, "http://cdn.example.com/media")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	responses := make(chan *http.Response, concurrent)
	errors := make(chan error, concurrent)
	for range concurrent {
		go func() {
			response, err := dynamicPoolTestFetch(ctx, state, target, nil)
			if err != nil {
				errors <- err
				return
			}
			responses <- response
		}()
	}
	var held []*http.Response
	for range concurrent {
		select {
		case response := <-responses:
			held = append(held, response)
		case err := <-errors:
			t.Fatal(err)
		case <-ctx.Done():
			t.Fatal("concurrent resources serialized behind one HTTP/1 connection")
		}
	}
	unblock.Do(func() { close(allowBodies) })
	for _, response := range held {
		if err := dynamicPoolTestConsume(response); err != nil {
			t.Fatal(err)
		}
	}
	if dials.Load() != concurrent {
		t.Fatalf("concurrent dials=%d, want %d", dials.Load(), concurrent)
	}
}

func TestDynamicTransportPoolIdleExpiryLeasedEntriesAndStop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "65536")
		_, _ = io.WriteString(w, strings.Repeat("x", 65536))
	}))
	defer server.Close()
	runtime, state, _ := dynamicPoolTestState(t, server, nil)
	state.transports.idleTTL = 20 * time.Millisecond
	target := dynamicPoolTestTarget(t, "http://cdn.example.com/media")
	response, err := dynamicPoolTestFetch(context.Background(), state, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	state.transports.mu.Lock()
	state.transports.pruneLocked(time.Now().Add(time.Hour))
	state.transports.mu.Unlock()
	if entries, leases := dynamicPoolTestCounts(state.transports); entries != 1 || leases != 1 {
		t.Fatalf("expiry evicted a leased response: entries/leases=%d/%d", entries, leases)
	}
	if err := dynamicPoolTestConsume(response); err != nil {
		t.Fatalf("expiry closed an active connection: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		entries, _ := dynamicPoolTestCounts(state.transports)
		if entries == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle pool entry did not expire without another request")
		}
		time.Sleep(time.Millisecond)
	}
	response, err = dynamicPoolTestFetch(context.Background(), state, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	state.close()
	if _, reason := state.reserveAuthority(dynamicCanonicalAuthority(target), time.Now()); reason != dynamicObservationReasonRuntimeUnavailable {
		t.Fatalf("closed site accepted reservation: %q", reason)
	}
	if _, err := borrowDynamicTransport(context.Background(), state, target, []net.IP{net.ParseIP("1.1.1.1")}, runtime.selfTargets.Load(), nil); !errors.Is(err, errDynamicTransportPoolClosed) {
		t.Fatalf("closed site reopened pool: %v", err)
	}
	if err := dynamicPoolTestConsume(response); err != nil {
		t.Fatal(err)
	}
	if runtime.transportPoolEntries.Load() != 0 {
		t.Fatalf("stop leaked global pool slots: %d", runtime.transportPoolEntries.Load())
	}
}

func TestDynamicTransportPoolPendingConstructionIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer server.Close()
	runtime, state, dials := dynamicPoolTestState(t, server, nil)
	target := dynamicPoolTestTarget(t, "http://cdn.example.com/media")
	policy := runtime.selfTargets.Load()
	started := make(chan struct{})
	finish := make(chan struct{})
	var allow sync.Once
	defer allow.Do(func() { close(finish) })
	state.transports.build = func(target *url.URL, pins []net.IP, policy *dynamicSelfTargetPolicy) (*http.Transport, error) {
		close(started)
		<-finish
		return dynamicPoolTestBuilder(server, dials)(target, pins, policy)
	}
	result := make(chan error, 1)
	go func() {
		lease, err := state.transports.borrow(context.Background(), target, []net.IP{net.ParseIP("1.1.1.1")}, policy)
		if lease != nil {
			lease.Close()
		}
		result <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := state.transports.borrow(ctx, target, []net.IP{net.ParseIP("1.1.1.1")}, policy); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled construction waiter=%v", err)
	}
	state.transports.mu.Lock()
	state.transports.pruneLocked(time.Now().Add(time.Hour))
	state.transports.mu.Unlock()
	if entries, leases := dynamicPoolTestCounts(state.transports); entries != 1 || leases != 1 {
		t.Fatalf("pending construction entries/leases=%d/%d", entries, leases)
	}
	state.close()
	if runtime.transportPoolEntries.Load() != 1 {
		t.Fatal("stop lost track of an unfinished construction")
	}
	allow.Do(func() { close(finish) })
	select {
	case err := <-result:
		if !errors.Is(err, errDynamicTransportPolicyChanged) {
			t.Fatalf("pending borrow after stop=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pending construction did not finish after stop")
	}
	if runtime.transportPoolEntries.Load() != 0 || dials.Load() != 0 {
		t.Fatalf("pending stop slots/dials=%d/%d", runtime.transportPoolEntries.Load(), dials.Load())
	}
}

func TestDynamicTransportPoolCapacityFallbackKeepsPolicyBinding(t *testing.T) {
	for _, change := range []string{"policy", "close", "interfaces"} {
		t.Run(change, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
			defer server.Close()
			runtime, state, dials := dynamicPoolTestState(t, server, nil)
			state.transports.capacity = 1
			var currentLocal atomic.Bool
			policy, err := newDynamicSelfTargetPolicy("panel.example.net", 8080, nil, func() ([]net.Addr, error) {
				if currentLocal.Load() {
					return []net.Addr{&net.IPAddr{IP: net.ParseIP("1.1.1.1")}}, nil
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			runtime.selfTargets.Store(policy)
			firstTarget := dynamicPoolTestTarget(t, "http://cdn.example.com/media")
			secondTarget := dynamicPoolTestTarget(t, "http://another.example.com/media")
			pins := []net.IP{net.ParseIP("1.1.1.1")}
			first, err := borrowDynamicTransport(context.Background(), state, firstTarget, pins, policy, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer first.Close()
			fallback, err := borrowDynamicTransport(context.Background(), state, secondTarget, pins, policy, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer fallback.Close()
			if fallback.pooled() || !fallback.transport.DisableKeepAlives || runtime.transportPoolEntries.Load() != 1 {
				t.Fatal("full pool evicted its active entry or retained an unbounded fallback")
			}
			switch change {
			case "policy":
				runtime.selfTargets.Store(policy)
			case "close":
				state.close()
			case "interfaces":
				currentLocal.Store(true)
			}
			request, err := http.NewRequestWithContext(isolateDynamicOutboundContext(context.Background()), http.MethodGet, secondTarget.String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Close = true
			if response, err := fallback.RoundTrip(request); err == nil || response != nil || dials.Load() != 0 {
				t.Fatalf("fallback bypassed %s change: resp=%v err=%v dials=%d", change, response, err, dials.Load())
			}
		})
	}
}

func TestDynamicTransportPoolDelayedPolicyInvalidationPreservesNewGeneration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer server.Close()
	runtime, state, _ := dynamicPoolTestState(t, server, nil)
	target := dynamicPoolTestTarget(t, "http://cdn.example.com/media")
	policy := runtime.selfTargets.Load()
	_, previousGeneration := runtime.selfTargets.snapshot()
	// Store publishes the new snapshot before taking runtime.mu to enumerate
	// pools. Hold that lock to admit a request in the real publication window.
	runtime.mu.Lock()
	locked := true
	defer func() {
		if locked {
			runtime.mu.Unlock()
		}
	}()
	published := make(chan struct{})
	go func() {
		runtime.selfTargets.Store(policy)
		close(published)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		_, currentGeneration := runtime.selfTargets.snapshot()
		if currentGeneration > previousGeneration {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("policy Store did not publish before its pool sweep")
		}
		time.Sleep(time.Millisecond)
	}
	lease, err := state.transports.borrow(context.Background(), target, []net.IP{net.ParseIP("1.1.1.1")}, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	runtime.mu.Unlock()
	locked = false
	select {
	case <-published:
	case <-time.After(time.Second):
		t.Fatal("policy pool sweep did not finish")
	}
	// An even older, delayed callback also cannot retire this newer entry.
	state.transports.invalidatePolicy(previousGeneration)
	state.transports.mu.Lock()
	retired := lease.entry.retired
	state.transports.mu.Unlock()
	if retired {
		t.Fatal("delayed policy invalidation killed a current-generation entry")
	}
	request, _ := http.NewRequestWithContext(isolateDynamicOutboundContext(context.Background()), http.MethodGet, target.String(), nil)
	response, err := lease.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := dynamicPoolTestConsume(response); err != nil {
		t.Fatal(err)
	}
}

func TestDynamicTransportPoolRetiredLastLeaseClosesIdleConnection(t *testing.T) {
	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	server.Start()
	defer server.Close()
	runtime, state, _ := dynamicPoolTestState(t, server, nil)
	target := dynamicPoolTestTarget(t, "http://cdn.example.com/media")
	policy := runtime.selfTargets.Load()
	lease, err := state.transports.borrow(context.Background(), target, []net.IP{net.ParseIP("1.1.1.1")}, policy)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	// This private test hook retires the entry after lease validation but
	// before Transport.queueForIdleConn clears CloseIdleConnections' flag.
	trace := &httptrace.ClientTrace{GetConn: func(string) { runtime.selfTargets.Store(policy) }}
	ctx := httptrace.WithClientTrace(isolateDynamicOutboundContext(context.Background()), trace)
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	response, err := lease.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := dynamicPoolTestConsume(response); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("retired final lease left a newly idle connection alive")
	}
	if runtime.transportPoolEntries.Load() != 0 {
		t.Fatalf("retired final lease retained %d global pool slots", runtime.transportPoolEntries.Load())
	}
}

func TestDynamicTransportPoolOwnsTraceAndPreservesTypedContexts(t *testing.T) {
	var informational, lookups atomic.Int64
	requests := make(chan *http.Request, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Clone(context.Background())
		w.Header().Set("Link", "<https://private.example.com>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Set("Content-Length", "2")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	_, state, dials := dynamicPoolTestState(t, server, dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		lookups.Add(1)
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
	}))
	target := dynamicPoolTestTarget(t, "http://cdn.example.com/media")
	follower := &redirectFollowTransport{
		base: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return redirectRuntimeResponse(request, http.StatusTemporaryRedirect, []string{target.String()}, http.NoBody), nil
		}),
		dynamicPolicy: redirectRuntimePolicy(true), dynamicState: state, followUnknownRedirects: true,
	}
	for index := range 2 {
		request := redirectRuntimeEligibleRequest(http.MethodGet, "http://origin.example.net/Videos/42/stream")
		tracker := newRequestDiagnosticsTracker(time.Now(), request)
		backend := &backendAddressTracker{}
		base := dynamicPoolTestTarget(t, "http://origin.example.net/PlaybackInfo")
		ctx := context.WithValue(request.Context(), requestDiagnosticsContextKey{}, tracker)
		ctx = context.WithValue(ctx, backendAddressContextKey{}, backend)
		ctx = context.WithValue(ctx, dynamicPlaybackInfoBaseContextKey{}, base)
		ctx = context.WithValue(ctx, dynamicExpectedStructuredSourceContextKey{}, dynamicDiscoverySourceHLS)
		ctx = context.WithValue(ctx, "caller-value", "secret")
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{Got1xxResponse: func(int, textproto.MIMEHeader) error {
			informational.Add(1)
			return nil
		}})
		request = request.WithContext(ctx)
		request.Header.Set("Range", "bytes=0-1")
		request.Header.Set("Authorization", "private")
		request.Header.Set("Cookie", "private")
		response, err := follower.RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		if !responseIsDynamic(response) || dynamicResponseExpectedStructuredSource(response) != dynamicDiscoverySourceHLS || requestDiagnosticsFromContext(response.Request.Context()) != tracker {
			t.Fatal("dynamic response lost its typed context or private diagnostics tracker")
		}
		if response.Request.Context().Value(dynamicPlaybackInfoBaseContextKey{}) != base || response.Request.Context().Value("caller-value") != nil {
			t.Fatal("dynamic context isolation dropped its typed base or retained caller data")
		}
		if err := dynamicPoolTestConsume(response); err != nil {
			t.Fatal(err)
		}
		result := tracker.snapshot(time.Now(), response.StatusCode, nil, nil)
		wantState := "new"
		if index == 1 {
			wantState = "reused"
		}
		if result.ConnectionState != wantState || result.DNSDurationMS == nil || result.UpstreamFirstByteMS == nil || result.TLSDurationMS != nil {
			t.Fatalf("owned diagnostics=%+v, want connection=%s and actual DNS/first-byte only", result, wantState)
		}
		captured := <-requests
		if captured.Close || captured.Header.Get("Authorization") != "" || captured.Header.Get("Cookie") != "" || captured.Header.Get("Range") != "bytes=0-1" {
			t.Fatalf("dynamic outbound Close/headers=%t/%v", captured.Close, captured.Header)
		}
	}
	if informational.Load() != 0 || dials.Load() != 1 || lookups.Load() != 2 {
		t.Fatalf("caller 1xx/dials/DNS=%d/%d/%d", informational.Load(), dials.Load(), lookups.Load())
	}
}

func TestDynamicTransportPoolTLSVerificationAndReuse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Errorf("dynamic transport negotiated HTTP/%d", r.ProtoMajor)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	_, state, dials := dynamicPoolTestState(t, server, nil)
	target := dynamicPoolTestTarget(t, "https://example.com/media")
	for index := range 2 {
		request, _ := http.NewRequest(http.MethodGet, target.String(), nil)
		tracker := newRequestDiagnosticsTracker(time.Now(), request)
		ctx := context.WithValue(context.Background(), requestDiagnosticsContextKey{}, tracker)
		response, err := dynamicPoolTestFetch(ctx, state, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := dynamicPoolTestConsume(response); err != nil {
			t.Fatal(err)
		}
		result := tracker.snapshot(time.Now(), response.StatusCode, nil, nil)
		if index == 0 && (result.ConnectionState != "new" || result.TLSDurationMS == nil) {
			t.Fatalf("new verified TLS connection diagnostics=%+v", result)
		}
		if index == 1 && (result.ConnectionState != "reused" || result.TLSDurationMS != nil || result.TCPDurationMS != nil) {
			t.Fatalf("reused TLS connection invented TCP/TLS measurements: %+v", result)
		}
	}
	if dials.Load() != 1 {
		t.Fatalf("TLS range requests used %d connections, want 1", dials.Load())
	}
	badHost := dynamicPoolTestTarget(t, "https://cdn.example.net/media")
	if response, err := dynamicPoolTestFetch(context.Background(), state, badHost, nil); err == nil || response != nil || dynamicTransportFailureReason(err) != dynamicObservationReasonTLSFailure {
		t.Fatalf("pooled transport skipped certificate hostname verification: response=%v err=%v", response, err)
	}
}

func TestDynamicTransportPoolGlobalCapacityIncludesActiveEntries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer server.Close()
	runtime, state, dials := dynamicPoolTestState(t, server, nil)
	target := dynamicPoolTestTarget(t, "http://cdn.example.com/media")
	policy := runtime.selfTargets.Load()
	states := []*dynamicSiteState{state}
	var held []*dynamicTransportLease
	for index := range globalDynamicTransportPoolCapacity + 1 {
		candidate := state
		if index != 0 {
			candidate = newDynamicSiteState(runtime, dynamicDefaultProfileLimits())
			candidate.transports.build = dynamicPoolTestBuilder(server, dials)
			states = append(states, candidate)
			t.Cleanup(candidate.close)
		}
		lease, err := borrowDynamicTransport(context.Background(), candidate, target, []net.IP{net.ParseIP("1.1.1.1")}, policy, nil)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, lease)
		if index < globalDynamicTransportPoolCapacity && !lease.pooled() {
			t.Fatalf("pool fell back before its global capacity: index=%d", index)
		}
		if index == globalDynamicTransportPoolCapacity && lease.pooled() {
			t.Fatal("pool admitted more than its global active-entry capacity")
		}
	}
	if runtime.transportPoolEntries.Load() != globalDynamicTransportPoolCapacity {
		t.Fatalf("global retained transport count=%d", runtime.transportPoolEntries.Load())
	}
	for _, lease := range held {
		lease.Close()
	}
	for _, candidate := range states {
		candidate.close()
	}
	if runtime.transportPoolEntries.Load() != 0 || dials.Load() != 0 {
		t.Fatalf("capacity cleanup slots/dials=%d/%d", runtime.transportPoolEntries.Load(), dials.Load())
	}
}

func TestDynamicTransportPoolCapabilityRedirectReuseAndIsolation(t *testing.T) {
	for _, redirectHost := range []string{"", "http://another.example.net"} {
		t.Run(redirectHost, func(t *testing.T) {
			var lookups atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.Close || request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
					t.Errorf("capability exposed credentials or forced Close: close=%t headers=%v", request.Close, request.Header)
				}
				if request.URL.Path == "/media" {
					w.Header().Set("Location", redirectHost+"/final")
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				}
				if request.Header.Get("Range") != "bytes=0-2047" {
					t.Errorf("capability redirect lost Range: %q", request.Header.Get("Range"))
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", "2048")
				w.Header().Set("Set-Cookie", "private")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, strings.Repeat("x", 2048))
			}))
			defer server.Close()
			_, state, dials := dynamicPoolTestState(t, server, dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
				lookups.Add(1)
				return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
			}))
			target := dynamicPoolTestTarget(t, "http://cdn.example.com/media")
			issuer := &dynamicCapabilityIssuer{
				key: []byte(strings.Repeat("k", 32)), siteID: 7, policyRevision: 1,
				policy: redirectRuntimePolicy(true), state: state,
				site: Site{MainVideoStreamMode: mainVideoStreamModeProxy},
			}
			route, discoveryErr := issuer.mint(context.Background(), nil, target, dynamicDiscoverySourceRedirect)
			if discoveryErr != nil {
				t.Fatal(discoveryErr)
			}
			// Neither a tampered token nor an authenticated expired claim may reach
			// classification, redirect following, DNS, or the transport pool.
			tampered := route[:len(route)-1] + "A"
			if tampered == route {
				tampered = route[:len(route)-1] + "B"
			}
			expiredClaims, err := openDynamicCapability(issuer.key, issuer.capabilityToken(route))
			if err != nil {
				t.Fatal(err)
			}
			expiredClaims.IssuedAt = time.Now().Add(-2 * time.Minute).Unix()
			expiredClaims.ExpiresAt = time.Now().Add(-time.Minute).Unix()
			expiredToken, err := sealDynamicCapability(issuer.key, expiredClaims)
			if err != nil {
				t.Fatal(err)
			}
			for _, invalidRoute := range []string{tampered, dynamicRoutePrefix + expiredToken} {
				writer := httptest.NewRecorder()
				issuer.serve(writer, httptest.NewRequest(http.MethodGet, "http://public.example.net"+invalidRoute, nil))
				if writer.Code != http.StatusNotFound || dials.Load() != 0 || lookups.Load() != 1 {
					t.Fatalf("invalid capability reached dynamic following: status/dials/DNS=%d/%d/%d", writer.Code, dials.Load(), lookups.Load())
				}
			}
			for range 2 {
				request := httptest.NewRequest(http.MethodGet, "http://public.example.net"+route, nil)
				request.Header.Set("Range", "bytes=0-2047")
				request.Header.Set("Authorization", "private")
				request.Header.Set("Cookie", "private")
				tracker := newRequestDiagnosticsTracker(time.Now(), request)
				request = request.WithContext(context.WithValue(request.Context(), requestDiagnosticsContextKey{}, tracker))
				writer := httptest.NewRecorder()
				issuer.serve(writer, request)
				if writer.Code != http.StatusPartialContent || writer.Body.Len() != 2048 || writer.Header().Get("Set-Cookie") != "" {
					t.Fatalf("capability status/body/headers=%d/%d/%v", writer.Code, writer.Body.Len(), writer.Header())
				}
				result := tracker.snapshot(time.Now(), writer.Code, nil, nil)
				if result.DNSDurationMS == nil || result.UpstreamFirstByteMS == nil {
					t.Fatalf("capability request lost owned diagnostics: %+v", result)
				}
			}
			wantDials := int64(1)
			if redirectHost != "" {
				wantDials = 2
			}
			if dials.Load() != wantDials || lookups.Load() != 5 {
				t.Fatalf("mint plus two capability redirect chains dials/DNS=%d/%d, want %d/5", dials.Load(), lookups.Load(), wantDials)
			}
			if _, leases := dynamicPoolTestCounts(state.transports); leases != 0 || len(state.streams) != 0 || len(state.runtime.streams) != 0 {
				t.Fatalf("capability redirect leaked leases/site streams/global streams=%d/%d/%d", leases, len(state.streams), len(state.runtime.streams))
			}
		})
	}
}

func TestDynamicTransportPoolCapabilityClassificationDoesNotAuthorizeOrdinaryMedia(t *testing.T) {
	var lookups atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer server.Close()
	_, state, dials := dynamicPoolTestState(t, server, dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		lookups.Add(1)
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
	}))
	target := dynamicPoolTestTarget(t, "http://cdn.example.com/media")
	follower := &redirectFollowTransport{
		base: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return redirectRuntimeResponse(request, http.StatusTemporaryRedirect, []string{target.String()}, http.NoBody), nil
		}),
		dynamicPolicy: redirectRuntimePolicy(true), dynamicState: state,
		followUnknownRedirects: true,
		capabilityIssuer:       &dynamicCapabilityIssuer{state: state},
	}
	request := httptest.NewRequest(http.MethodGet, "http://origin.example.net/media", nil)
	response, err := follower.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect || response.Header.Get("Location") != target.String() || responseIsDynamic(response) || lookups.Load() != 0 || dials.Load() != 0 {
		t.Fatalf("ordinary /media acquired capability classification: status/dynamic/DNS/dials=%d/%t/%d/%d", response.StatusCode, responseIsDynamic(response), lookups.Load(), dials.Load())
	}
}

func BenchmarkDynamicTransportRangeRequests(b *testing.B) {
	for _, scheme := range []string{"http", "https"} {
		b.Run(scheme, func(b *testing.B) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Length", "2048")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, strings.Repeat("x", 2048))
			})
			server := httptest.NewServer(handler)
			if scheme == "https" {
				server.Close()
				server = httptest.NewTLSServer(handler)
			}
			defer server.Close()
			for _, strategy := range []string{"strict", "pooled"} {
				b.Run(strategy, func(b *testing.B) {
					var lookups atomic.Int64
					_, state, dials := dynamicPoolTestState(b, server, dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
						lookups.Add(1)
						return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
					}))
					target := dynamicPoolTestTarget(b, scheme+"://example.com/media")
					var factory dynamicTransportFactory
					if strategy == "strict" {
						factory = state.transports.build
					}
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						response, err := dynamicPoolTestFetch(context.Background(), state, target, factory)
						if err != nil {
							b.Fatal(err)
						}
						if err := dynamicPoolTestConsume(response); err != nil {
							b.Fatal(err)
						}
					}
					b.StopTimer()
					b.ReportMetric(float64(dials.Load())/float64(b.N), "connections/op")
					b.ReportMetric(float64(lookups.Load())/float64(b.N), "DNS/op")
				})
			}
		})
	}
}
