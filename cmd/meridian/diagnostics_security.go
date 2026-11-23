package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errDiagnosticTargetForbidden = errors.New("diagnostic target is not allowed")

// Administrators may diagnose local/private Emby servers. Cloud control-plane
// endpoints and non-unicast destinations are not legitimate upstream tests.
var diagnosticForbiddenPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func diagnosticIPAllowed(address netip.Addr) bool {
	if !address.IsValid() || address.Zone() != "" {
		return false
	}
	address = address.Unmap()
	if address.IsUnspecified() || address.IsMulticast() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() {
		return false
	}
	for _, prefix := range diagnosticForbiddenPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	switch address {
	case netip.MustParseAddr("100.100.100.200"), // Alibaba metadata.
		netip.MustParseAddr("168.63.129.16"), // Azure host/WireServer endpoint.
		netip.MustParseAddr("fd00:ec2::254"): // AWS IPv6 metadata.
		return false
	}
	// The well-known NAT64 prefix can otherwise encode a forbidden IPv4
	// endpoint while passing an IPv6-only address check.
	if netip.MustParsePrefix("64:ff9b::/96").Contains(address) {
		encoded := address.As16()
		return diagnosticIPAllowed(netip.AddrFrom4([4]byte{encoded[12], encoded[13], encoded[14], encoded[15]}))
	}
	return true
}

func diagnosticHostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimRight(host, "."))
	switch host {
	case "", "instance-data", "instance-data.ec2.internal", "metadata.ec2.internal",
		"metadata.google.internal", "metadata.google", "metadata.goog",
		"metadata.azure.internal", "metadata.tencentyun.com", "metadata.aliyun.com":
		return false
	}
	return true
}

type diagnosticIPResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

// diagnosticDialer checks the complete DNS answer before connecting and passes
// only checked IP literals to the system dialer. A second DNS lookup cannot
// swap in a metadata address between validation and connection establishment.
type diagnosticDialer struct {
	resolver    diagnosticIPResolver
	dialContext func(context.Context, string, string) (net.Conn, error)
	timeout     time.Duration
}

func newDiagnosticDialer(timeout time.Duration) *diagnosticDialer {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	return &diagnosticDialer{
		resolver:    net.DefaultResolver,
		dialContext: dialer.DialContext,
		timeout:     timeout,
	}
}

func (dialer *diagnosticDialer) DialContext(ctx context.Context, network, target string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, errors.New("diagnostic connection requires TCP")
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return nil, errors.New("invalid diagnostic target address")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 || !diagnosticHostAllowed(host) {
		return nil, errDiagnosticTargetForbidden
	}
	if dialer.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, dialer.timeout)
		defer cancel()
	}
	var addresses []netip.Addr
	if literal, parseErr := netip.ParseAddr(host); parseErr == nil {
		addresses = append(addresses, literal)
	} else {
		answers, lookupErr := dialer.resolver.LookupIPAddr(ctx, host)
		if lookupErr != nil {
			return nil, fmt.Errorf("resolve diagnostic target: %w", lookupErr)
		}
		for _, answer := range answers {
			address, valid := netip.AddrFromSlice(answer.IP)
			if !valid || answer.Zone != "" {
				return nil, errDiagnosticTargetForbidden
			}
			addresses = append(addresses, address.Unmap())
		}
	}
	if len(addresses) == 0 {
		return nil, errors.New("diagnostic target has no IP addresses")
	}
	for _, address := range addresses {
		if !diagnosticIPAllowed(address) {
			return nil, errDiagnosticTargetForbidden
		}
	}
	var candidates []netip.Addr
	for _, address := range addresses {
		address = address.Unmap()
		if network == "tcp4" && !address.Is4() || network == "tcp6" && !address.Is6() {
			continue
		}
		candidates = append(candidates, address)
	}
	var lastErr error
	for index, address := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		candidateCtx := ctx
		cancel := func() {}
		if deadline, bounded := ctx.Deadline(); bounded && index+1 < len(candidates) {
			// Leave later checked addresses a share of the remaining budget. A
			// black-holed IPv6 answer must not consume the IPv4 fallback's entire
			// timeout; a candidate's deadline is not the overall request deadline.
			budget := time.Until(deadline) / time.Duration(len(candidates)-index)
			candidateCtx, cancel = context.WithTimeout(ctx, budget)
		}
		connection, dialErr := dialer.dialContext(candidateCtx, network, net.JoinHostPort(address.String(), port))
		cancel()
		if dialErr == nil {
			return connection, nil
		}
		lastErr = dialErr
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	if lastErr == nil {
		lastErr = errors.New("diagnostic target has no matching IP family")
	}
	return nil, lastErr
}

const (
	upstreamTestMaxConcurrent = 4
	upstreamTestBurst         = 24
	upstreamTestRefill        = time.Second
)

// The arbitrary-URL test has a per-controller budget, independent of login
// admission. Saved-site diagnostics keep their existing batch behavior.
type upstreamTestAdmission struct {
	mu     sync.Mutex
	active int
	tokens float64
	last   time.Time
}

func (admission *upstreamTestAdmission) acquire(now time.Time) (func(), bool) {
	admission.mu.Lock()
	defer admission.mu.Unlock()
	if admission.last.IsZero() {
		admission.tokens = upstreamTestBurst
		admission.last = now
	} else if now.After(admission.last) {
		admission.tokens += float64(now.Sub(admission.last)) / float64(upstreamTestRefill)
		if admission.tokens > upstreamTestBurst {
			admission.tokens = upstreamTestBurst
		}
		admission.last = now
	}
	if admission.active >= upstreamTestMaxConcurrent || admission.tokens < 1 {
		return nil, false
	}
	admission.active++
	admission.tokens--
	var once sync.Once
	return func() {
		once.Do(func() {
			admission.mu.Lock()
			admission.active--
			admission.mu.Unlock()
		})
	}, true
}
