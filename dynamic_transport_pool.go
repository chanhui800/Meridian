package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	dynamicTransportPoolCapacity       = 16
	globalDynamicTransportPoolCapacity = 64
	dynamicTransportIdleConnsPerHost   = 4
	dynamicTransportIdleTimeout        = time.Minute
)

var errDynamicTransportPoolClosed = errors.New("dynamic transport runtime is closed")
var errDynamicTransportPolicyChanged = errors.New("dynamic transport validation changed")

// A pool belongs to one running site instance. Its key includes the complete
// validated address set and the live self-target policy generation, so an idle
// connection never replaces the next request's authority and DNS validation.
type dynamicTransportPoolKey struct {
	authority  string
	pins       string
	interfaces string
	policy     *dynamicSelfTargetPolicy
	generation uint64
}

type dynamicTransportPoolEntry struct {
	key       dynamicTransportPoolKey
	transport *http.Transport
	done      chan struct{}
	err       error
	leases    int
	lastUsed  time.Time
	retired   bool
	building  bool
}

type dynamicTransportPool struct {
	runtime *dynamicRuntime
	limits  DynamicProfileLimits
	build   dynamicTransportFactory

	mu       sync.Mutex
	byKey    map[dynamicTransportPoolKey]*dynamicTransportPoolEntry
	entries  map[*dynamicTransportPoolEntry]struct{}
	closed   bool
	timer    *time.Timer
	idleTTL  time.Duration
	capacity int
}

func newDynamicTransportPool(runtime *dynamicRuntime, limits DynamicProfileLimits) *dynamicTransportPool {
	return &dynamicTransportPool{
		runtime: runtime, limits: limits, build: newDynamicTransport,
		byKey:   make(map[dynamicTransportPoolKey]*dynamicTransportPoolEntry),
		entries: make(map[*dynamicTransportPoolEntry]struct{}),
		idleTTL: dynamicTransportIdleTimeout, capacity: dynamicTransportPoolCapacity,
	}
}

func dynamicIPSetKey(ips []net.IP) string {
	values := make([]string, 0, len(ips))
	for _, ip := range ips {
		values = append(values, ip.String())
	}
	sort.Strings(values)
	return strings.Join(values, ",")
}

func (p *dynamicTransportPool) validatedKey(target *url.URL, pins []net.IP, policy *dynamicSelfTargetPolicy) (dynamicTransportPoolKey, []net.IP, error) {
	var key dynamicTransportPoolKey
	if p == nil || p.runtime == nil || target == nil || policy == nil {
		return key, nil, errDynamicTransportPoolClosed
	}
	current, generation := p.runtime.selfTargets.snapshot()
	if current != policy {
		return key, nil, errDynamicTransportPolicyChanged
	}
	normalized, err := normalizeDynamicURL(target.String())
	if err != nil {
		return key, nil, err
	}
	key.authority = dynamicCanonicalAuthority(normalized)
	if err := policy.validateNormalizedTarget(normalized); err != nil {
		p.invalidateAuthority(key.authority)
		return key, nil, err
	}
	validated, err := validateDynamicResolvedIPs(pins)
	if err != nil {
		p.invalidateAuthority(key.authority)
		return key, nil, err
	}
	interfaces, err := snapshotDynamicInterfaceIPs(policy.interfaceAddrs)
	if err != nil {
		p.invalidateAuthority(key.authority)
		return key, nil, err
	}
	interfaceValues := make([]string, 0, len(interfaces))
	for ip := range interfaces {
		interfaceValues = append(interfaceValues, ip.String())
	}
	sort.Strings(interfaceValues)
	for _, ip := range validated {
		if err := policy.validateIPAgainst(ip, interfaces); err != nil {
			p.invalidateAuthority(key.authority)
			return key, nil, err
		}
		if direct := net.ParseIP(normalized.Hostname()); direct != nil && !direct.Equal(ip) {
			return key, nil, fmt.Errorf("dynamic transport pins do not match the literal target IP")
		}
	}
	if len(validated) == 0 || len(validated) > maxDynamicResolvedIPCount {
		return key, nil, fmt.Errorf("dynamic transport has an invalid pinned IP count")
	}
	key.pins = dynamicIPSetKey(validated)
	key.interfaces = strings.Join(interfaceValues, ",")
	key.policy, key.generation = policy, generation
	return key, validated, nil
}

func (p *dynamicTransportPool) reserveGlobalSlot() bool {
	for {
		count := p.runtime.transportPoolEntries.Load()
		if count >= globalDynamicTransportPoolCapacity {
			return false
		}
		if p.runtime.transportPoolEntries.CompareAndSwap(count, count+1) {
			return true
		}
	}
}

func (p *dynamicTransportPool) removeLocked(entry *dynamicTransportPoolEntry) {
	if _, exists := p.entries[entry]; !exists {
		return
	}
	if p.byKey[entry.key] == entry {
		delete(p.byKey, entry.key)
	}
	delete(p.entries, entry)
	p.runtime.transportPoolEntries.Add(-1)
}

func (p *dynamicTransportPool) retireLocked(entry *dynamicTransportPoolEntry) {
	entry.retired = true
	if p.byKey[entry.key] == entry {
		delete(p.byKey, entry.key)
	}
	if entry.transport != nil {
		// CloseIdleConnections also prevents a currently leased connection from
		// returning to this retired transport after its response reaches EOF.
		entry.transport.CloseIdleConnections()
	}
	if entry.leases == 0 && !entry.building {
		p.removeLocked(entry)
	}
}

func (p *dynamicTransportPool) pruneLocked(now time.Time) {
	for entry := range p.entries {
		// Pending construction and active responses are never idle victims.
		if entry.leases == 0 && !entry.building && (entry.retired || now.Sub(entry.lastUsed) >= p.idleTTL) {
			p.retireLocked(entry)
		}
	}
}

func (p *dynamicTransportPool) scheduleLocked(now time.Time) {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	if p.closed {
		return
	}
	var next time.Time
	for entry := range p.entries {
		if entry.leases != 0 || entry.building {
			continue
		}
		expires := entry.lastUsed.Add(p.idleTTL)
		if next.IsZero() || expires.Before(next) {
			next = expires
		}
	}
	if !next.IsZero() {
		p.timer = time.AfterFunc(max(time.Millisecond, next.Sub(now)), p.prune)
	}
}

func (p *dynamicTransportPool) prune() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.pruneLocked(now)
	p.scheduleLocked(now)
}

func (p *dynamicTransportPool) invalidateAuthority(authority string) {
	if p == nil || authority == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for entry := range p.entries {
		if entry.key.authority == authority {
			p.retireLocked(entry)
		}
	}
	p.scheduleLocked(time.Now())
}

func (p *dynamicTransportPool) invalidatePolicy(generation uint64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for entry := range p.entries {
		if entry.key.generation < generation {
			p.retireLocked(entry)
		}
	}
	p.scheduleLocked(time.Now())
}

func (p *dynamicTransportPool) close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for entry := range p.entries {
		p.retireLocked(entry)
	}
	p.scheduleLocked(time.Now())
}

func (p *dynamicTransportPool) borrow(ctx context.Context, target *url.URL, pins []net.IP, policy *dynamicSelfTargetPolicy) (*dynamicTransportLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key, validated, err := p.validatedKey(target, pins, policy)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errDynamicTransportPoolClosed
	}
	now := time.Now()
	p.pruneLocked(now)
	for entry := range p.entries {
		if entry.key.authority == key.authority && entry.key != key {
			p.retireLocked(entry)
		}
	}
	entry := p.byKey[key]
	if entry != nil {
		entry.leases++
		p.scheduleLocked(now)
		p.mu.Unlock()
		return p.await(ctx, entry)
	}
	if len(p.entries) >= p.capacity {
		var oldest *dynamicTransportPoolEntry
		for candidate := range p.entries {
			if candidate.leases == 0 && (oldest == nil || candidate.lastUsed.Before(oldest.lastUsed)) {
				oldest = candidate
			}
		}
		if oldest != nil {
			p.retireLocked(oldest)
		}
	}
	if len(p.entries) >= p.capacity || !p.reserveGlobalSlot() {
		p.scheduleLocked(now)
		p.mu.Unlock()
		// A full pool with leased entries does not evict active requests or
		// deny an otherwise valid stream. The existing strict builder supplies
		// a dedicated connection; stream permits still bound active traffic.
		transport, buildErr := p.build(target, validated, policy)
		if buildErr != nil {
			return nil, buildErr
		}
		if transport == nil {
			return nil, fmt.Errorf("dynamic transport builder returned no transport")
		}
		return &dynamicTransportLease{transport: transport, validationPool: p, validationKey: key}, nil
	}
	entry = &dynamicTransportPoolEntry{key: key, done: make(chan struct{}), leases: 1, lastUsed: now, building: true}
	p.entries[entry] = struct{}{}
	p.byKey[key] = entry
	p.scheduleLocked(now)
	p.mu.Unlock()

	strict, buildErr := p.build(target, validated, policy)
	var transport *http.Transport
	if buildErr == nil && strict == nil {
		buildErr = fmt.Errorf("dynamic transport builder returned no transport")
	}
	if buildErr == nil {
		// Clone only this validated, proxy-free builder. No default transport,
		// global TLS session cache, or HTTP/2 connection pool is inherited.
		transport = strict.Clone()
		transport.Proxy = nil
		transport.DisableKeepAlives = false
		transport.DisableCompression = true
		transport.ForceAttemptHTTP2 = false
		transport.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
		transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
		transport.MaxIdleConns = dynamicTransportIdleConnsPerHost
		transport.MaxIdleConnsPerHost = dynamicTransportIdleConnsPerHost
		transport.MaxConnsPerHost = max(2, p.limits.MaxStreams)
		transport.IdleConnTimeout = p.idleTTL
		strict.CloseIdleConnections()
	}
	p.mu.Lock()
	entry.transport, entry.err, entry.building = transport, buildErr, false
	current, generation := p.runtime.selfTargets.snapshot()
	if buildErr != nil || p.closed || current != key.policy || generation != key.generation {
		p.retireLocked(entry)
	}
	close(entry.done)
	p.mu.Unlock()
	return p.await(ctx, entry)
}

func (p *dynamicTransportPool) await(ctx context.Context, entry *dynamicTransportPoolEntry) (*dynamicTransportLease, error) {
	lease := &dynamicTransportLease{pool: p, entry: entry}
	select {
	case <-ctx.Done():
		lease.Close()
		return nil, ctx.Err()
	case <-entry.done:
	}
	p.mu.Lock()
	lease.transport = entry.transport
	err := entry.err
	if err == nil && (p.closed || entry.retired) {
		err = errDynamicTransportPolicyChanged
	}
	p.mu.Unlock()
	if err != nil {
		lease.Close()
		return nil, err
	}
	return lease, nil
}

type dynamicTransportLease struct {
	transport      *http.Transport
	pool           *dynamicTransportPool
	entry          *dynamicTransportPoolEntry
	validationPool *dynamicTransportPool
	validationKey  dynamicTransportPoolKey
	once           sync.Once
	used           atomic.Bool
	closed         atomic.Bool
}

func borrowDynamicTransport(ctx context.Context, state *dynamicSiteState, target *url.URL, pins []net.IP, policy *dynamicSelfTargetPolicy, factory dynamicTransportFactory) (*dynamicTransportLease, error) {
	if state == nil || state.closed.Load() || state.transports == nil {
		return nil, errDynamicTransportPoolClosed
	}
	if factory != nil {
		// Custom factories keep their original per-request lifecycle and strict
		// request Close behavior, including the test TLS/dial abstractions.
		transport, err := factory(target, pins, policy)
		if err != nil {
			return nil, err
		}
		if transport == nil {
			return nil, fmt.Errorf("dynamic transport factory returned no transport")
		}
		return &dynamicTransportLease{transport: transport}, nil
	}
	return state.transports.borrow(ctx, target, pins, policy)
}

func (l *dynamicTransportLease) pooled() bool { return l != nil && l.pool != nil }

func (l *dynamicTransportLease) Close() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.closed.Store(true)
		if l.pool == nil {
			if l.transport != nil {
				l.transport.CloseIdleConnections()
			}
			return
		}
		p := l.pool
		p.mu.Lock()
		defer p.mu.Unlock()
		if l.entry.leases > 0 {
			l.entry.leases--
		}
		l.entry.lastUsed = time.Now()
		if l.entry.retired && l.entry.leases == 0 && !l.entry.building {
			if l.entry.transport != nil {
				l.entry.transport.CloseIdleConnections()
			}
			p.removeLocked(l.entry)
		}
		p.scheduleLocked(l.entry.lastUsed)
	})
}

func (*dynamicTransportLease) requestDiagnosticsOwnsRoundTrip() {}

func (l *dynamicTransportLease) RoundTrip(req *http.Request) (*http.Response, error) {
	if l == nil || l.transport == nil || req == nil || l.closed.Load() || !l.used.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("dynamic transport lease is unavailable")
	}
	validationPool, validationKey := l.validationPool, l.validationKey
	if l.pool != nil {
		validationPool, validationKey = l.pool, l.entry.key
	}
	if validationPool != nil {
		key, _, err := validationPool.validatedKey(req.URL, dynamicIPsFromKey(validationKey.pins), validationKey.policy)
		if err != nil || key != validationKey {
			validationPool.invalidateAuthority(validationKey.authority)
			l.Close()
			if err != nil {
				return nil, err
			}
			return nil, errDynamicTransportPolicyChanged
		}
		validationPool.mu.Lock()
		retired := validationPool.closed || l.entry != nil && l.entry.retired
		validationPool.mu.Unlock()
		if retired {
			l.Close()
			return nil, errDynamicTransportPolicyChanged
		}
	}
	req, attempt := beginRequestDiagnosticsRoundTrip(req, "dynamic")
	var conn net.Conn
	var connMu sync.Mutex
	if l.pool != nil {
		// Only this private lifecycle callback is composed with Meridian's
		// owned trace, after the caller's context has been isolated.
		trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
			connMu.Lock()
			conn = info.Conn
			connMu.Unlock()
		}}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	}
	resp, err := l.transport.RoundTrip(req)
	finishRequestDiagnosticsRoundTrip(attempt, resp, err, req.Context().Err())
	connMu.Lock()
	responseConn := conn
	connMu.Unlock()
	if resp != nil {
		resp.Request = req
	}
	if err != nil || resp == nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if responseConn != nil {
			_ = responseConn.Close()
		}
		l.Close()
		return resp, err
	}
	if resp.Body == nil || resp.Body == http.NoBody || resp.ContentLength == 0 {
		l.Close()
		return resp, nil
	}
	resp.Body = &dynamicTransportResponseBody{ReadCloser: resp.Body, ctx: req.Context(), conn: responseConn, lease: l}
	return resp, nil
}

func dynamicIPsFromKey(value string) []net.IP {
	values := strings.Split(value, ",")
	ips := make([]net.IP, 0, len(values))
	for _, value := range values {
		ips = append(ips, net.ParseIP(value))
	}
	return ips
}

type dynamicTransportResponseBody struct {
	io.ReadCloser
	ctx      context.Context
	conn     net.Conn
	lease    *dynamicTransportLease
	complete atomic.Bool
	once     sync.Once
}

func (b *dynamicTransportResponseBody) finish(complete bool) {
	b.once.Do(func() {
		if !complete && b.conn != nil {
			// Explicitly close the leased HTTP/1 connection before abandoning a
			// partial or canceled body. Other active pool connections are safe.
			_ = b.conn.Close()
		}
		b.lease.Close()
	})
}

func (b *dynamicTransportResponseBody) Read(payload []byte) (int, error) {
	if b.complete.Load() {
		return b.ReadCloser.Read(payload)
	}
	if err := b.ctx.Err(); err != nil {
		b.finish(false)
		_ = b.ReadCloser.Close()
		return 0, err
	}
	n, err := b.ReadCloser.Read(payload)
	if err == io.EOF && b.ctx.Err() == nil {
		b.complete.Store(true)
		b.finish(true)
	} else if err != nil {
		b.finish(false)
		if err == io.EOF && b.ctx.Err() != nil {
			err = b.ctx.Err()
		}
	}
	return n, err
}

func (b *dynamicTransportResponseBody) Close() error {
	b.finish(b.complete.Load())
	return b.ReadCloser.Close()
}
