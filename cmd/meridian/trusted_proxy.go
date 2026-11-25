package main

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

const defaultTrustedProxyCIDRs = "127.0.0.1/32,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7"

func parseTrustedProxyCIDRs(value string) ([]*net.IPNet, error) {
	if strings.TrimSpace(value) == "" {
		value = defaultTrustedProxyCIDRs
	}
	var networks []*net.IPNet
	for _, raw := range strings.Split(value, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		_, network, err := net.ParseCIDR(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid TRUSTED_PROXY_CIDRS entry %q: %w", raw, err)
		}
		networks = append(networks, network)
	}
	return networks, nil
}

// Forwarded-header trust describes which transport peers may provide client
// identity; it does not establish that a reverse proxy is deployed. Older
// installations used exact /32 and /128 peers as that deployment signal, so
// retain only that narrow compatibility case when the explicit flag is absent.
func parsePanelExternalProxyEnabled(value string, networks []*net.IPNet) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true":
		if len(networks) == 0 {
			return false, fmt.Errorf("PANEL_EXTERNAL_PROXY_ENABLED=true requires a non-empty TRUSTED_PROXY_CIDRS source allowlist")
		}
		return true, nil
	case "false":
		return false, nil
	case "":
		if len(networks) == 0 {
			return false, nil
		}
		for _, network := range networks {
			if network == nil {
				return false, nil
			}
			prefix, bits := network.Mask.Size()
			if (bits != 32 && bits != 128) || prefix != bits {
				return false, nil
			}
		}
		return true, nil
	default:
		return false, fmt.Errorf("PANEL_EXTERNAL_PROXY_ENABLED must be true or false")
	}
}

func hostOnlyIngressAvailable(panelBindLoopback, panelTLSEnabled, externalProxyEnabled bool, trustedProxies []*net.IPNet) bool {
	return panelBindLoopback || panelTLSEnabled || (externalProxyEnabled && len(trustedProxies) > 0)
}

func remoteAddressIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return net.ParseIP(host)
	}
	return net.ParseIP(remoteAddr)
}

func isTrustedProxy(ip net.IP, networks []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func requestClientKey(r *http.Request, trustedProxies []*net.IPNet) string {
	peerIP := remoteAddressIP(r.RemoteAddr)
	if isTrustedProxy(peerIP, trustedProxies) {
		// The trusted edge must normalize the client address into a single
		// X-Real-IP value. Never select from X-Forwarded-For: common
		// $proxy_add_x_forwarded_for configurations retain attacker-supplied
		// left-most values and would let clients rotate the login limiter key.
		if value, ok := singleForwardedHeaderValue(r.Header, "X-Real-IP"); ok {
			if forwarded := net.ParseIP(value); forwarded != nil {
				return forwarded.String()
			}
		}
	}
	if peerIP != nil {
		return peerIP.String()
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "unknown"
}

// agentReportSourceIP returns a trustworthy observed source address for node
// address discovery. A trusted reverse proxy must provide one normalized
// X-Real-IP value; if it does not, do not mistake the proxy address for the
// Agent. Forwarding headers from an untrusted peer make the hop ambiguous, so
// they also suppress discovery instead of publishing either the claimed value
// or a possible proxy address.
func agentReportSourceIP(r *http.Request, trustedProxies []*net.IPNet, externalProxyEnabled bool) string {
	if r == nil {
		return ""
	}
	peerIP := remoteAddressIP(r.RemoteAddr)
	if peerIP == nil {
		return ""
	}
	if externalProxyEnabled && !isTrustedProxy(peerIP, trustedProxies) {
		return ""
	}
	if isTrustedProxy(peerIP, trustedProxies) {
		value, ok := singleForwardedHeaderValue(r.Header, "X-Real-IP")
		if !ok {
			return ""
		}
		forwarded := net.ParseIP(value)
		if forwarded == nil {
			return ""
		}
		return forwarded.String()
	}
	for name, values := range r.Header {
		if !isManagedForwardingHeaderName(name) {
			continue
		}
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				return ""
			}
		}
	}
	return peerIP.String()
}

func isManagedForwardingHeaderName(name string) bool {
	lowerName := strings.ToLower(name)
	if lowerName == "forwarded" || lowerName == "x-real-ip" || strings.HasPrefix(lowerName, "x-forwarded-") {
		return true
	}
	switch lowerName {
	case "cf-connecting-ip", "cf-connecting-ipv6", "fastly-client-ip", "fly-client-ip",
		"true-client-ip", "x-appengine-user-ip", "x-azure-clientip", "x-client-ip",
		"x-cluster-client-ip", "x-envoy-external-address", "x-original-forwarded-for":
		return true
	default:
		return false
	}
}

func removeClientForwardingHeaders(header http.Header) {
	for name := range header {
		if isManagedForwardingHeaderName(name) {
			delete(header, name)
		}
	}
}

func singleForwardedHeaderValue(header http.Header, name string) (string, bool) {
	values := header.Values(name)
	if len(values) != 1 {
		return "", false
	}
	value := strings.TrimSpace(values[0])
	if value == "" || strings.Contains(value, ",") {
		return "", false
	}
	return value, true
}

func setTrustedForwardingHeaders(header http.Header, inbound *http.Request, trustedProxies ...[]*net.IPNet) {
	removeClientForwardingHeaders(header)
	if inbound == nil {
		return
	}
	var configured []*net.IPNet
	if len(trustedProxies) > 0 {
		configured = trustedProxies[0]
	}
	peerIP := remoteAddressIP(inbound.RemoteAddr)
	clientIP := peerIP
	forwardedProto := "http"
	if inbound.TLS != nil {
		forwardedProto = "https"
	}
	if isTrustedProxy(peerIP, configured) {
		// Trust only the single value a configured edge proxy normalized. Never
		// relay an arbitrary inbound X-Forwarded-For chain to the upstream.
		if value, ok := singleForwardedHeaderValue(inbound.Header, "X-Real-IP"); ok {
			if forwardedIP := net.ParseIP(value); forwardedIP != nil {
				clientIP = forwardedIP
			}
		}
		if candidateProto, ok := singleForwardedHeaderValue(inbound.Header, "X-Forwarded-Proto"); ok {
			if strings.EqualFold(candidateProto, "http") || strings.EqualFold(candidateProto, "https") {
				forwardedProto = strings.ToLower(candidateProto)
			}
		}
	}
	if clientIP != nil {
		header.Set("X-Forwarded-For", clientIP.String())
		header.Set("X-Real-IP", clientIP.String())
	}
	header.Set("X-Forwarded-Proto", forwardedProto)
}

func requestIsHTTPS(r *http.Request, trustedProxies []*net.IPNet) bool {
	if r.TLS != nil {
		return true
	}
	if !isTrustedProxy(remoteAddressIP(r.RemoteAddr), trustedProxies) {
		return false
	}
	forwardedProto, ok := singleForwardedHeaderValue(r.Header, "X-Forwarded-Proto")
	return ok && strings.EqualFold(forwardedProto, "https")
}
