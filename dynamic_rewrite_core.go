package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type dynamicRewriteSession struct {
	ctx                  context.Context
	issuer               *dynamicCapabilityIssuer
	base                 *url.URL
	learningBase         *url.URL
	source               string
	depth                int
	outputLimit          int64
	rewriteRelative      bool
	inheritedHeaders     []dynamicCapabilityHeaderClaim
	seen                 map[string]string
	minted               []string
	urlCount             int
	learnedPlaybackPaths []string
}

func (s *dynamicRewriteSession) rememberRelativePlaybackPath(value string) {
	if s == nil || s.learningBase == nil || !playbackInfoSafeRelativeURL(value) {
		return
	}
	reference, err := url.Parse(value)
	if err != nil || reference.Path == "" {
		return
	}
	resolved := s.learningBase.ResolveReference(reference)
	pathValue := canonicalDynamicPlaybackPath(resolved.Path)
	if pathValue == "" {
		return
	}
	for _, existing := range s.learnedPlaybackPaths {
		if existing == pathValue {
			return
		}
	}
	s.learnedPlaybackPaths = append(s.learnedPlaybackPaths, pathValue)
}

func (s *dynamicRewriteSession) publishLearnedPlaybackPaths() {
	if s == nil || s.issuer == nil || s.issuer.state == nil || len(s.learnedPlaybackPaths) == 0 {
		return
	}
	paths := s.learnedPlaybackPaths
	s.learnedPlaybackPaths = nil
	now := time.Now()
	for _, pathValue := range paths {
		s.issuer.state.learnPlaybackPath(pathValue, now)
	}
}

func (s *dynamicRewriteSession) rememberCapability(seenKey, token string) string {
	route := s.issuer.clientRoute(dynamicRoutePrefix + token)
	if s.seen == nil {
		s.seen = make(map[string]string)
	}
	s.seen[seenKey] = route
	s.minted = append(s.minted, token)
	return route
}

func (s *dynamicRewriteSession) reuseShallowTrustedManifest(target *url.URL, source, kind string, depth int) (string, bool) {
	if s == nil || s.issuer == nil || s.issuer.state == nil || s.issuer.configuredTransport == nil || target == nil || kind != dynamicCapabilityKindManifest || depth <= 1 {
		return "", false
	}
	for candidateDepth := 1; candidateDepth < depth; candidateDepth++ {
		cacheKey := trustedCapabilityCacheKey(source, kind, candidateDepth, target.String(), nil, nil)
		if token, exists := s.issuer.state.reuseCapability(cacheKey, time.Now()); exists {
			seenKey := "trusted\x00" + source + "\x00" + kind + "\x00" + strconv.Itoa(depth) + "\x00" + target.String()
			return s.rememberCapability(seenKey, token), true
		}
	}
	return "", false
}

func (s *dynamicRewriteSession) reuseShallowDynamicManifest(base, target *url.URL, source, kind string, depth int) (string, bool, *dynamicProxyError) {
	if s == nil || s.issuer == nil || s.issuer.state == nil || base == nil || target == nil || kind != dynamicCapabilityKindManifest || depth <= 1 {
		return "", false, nil
	}
	previousScheme := ""
	if strings.EqualFold(base.Scheme, "http") || strings.EqualFold(base.Scheme, "https") {
		previousScheme = strings.ToLower(base.Scheme)
	}
	for candidateDepth := 1; candidateDepth < depth; candidateDepth++ {
		cacheKey := dynamicCapabilityCacheKey(source, kind, candidateDepth, previousScheme, target.String(), nil, nil)
		token, exists := s.issuer.state.reuseCapability(cacheKey, time.Now())
		if !exists {
			continue
		}
		undo := func() {
			_ = s.issuer.state.settleCapabilities([]string{token}, false, time.Now())
		}
		selfTargets := s.issuer.state.runtime.selfTargets.Load()
		authority := dynamicCanonicalAuthority(target)
		if reasonCode := s.issuer.policy.validateTarget(base, target, selfTargets); reasonCode != "" {
			undo()
			s.issuer.observe(source, dynamicObservationDecisionDenied, reasonCode, authority)
			return "", false, newDynamicProxyError(reasonCode)
		}
		reservation, reasonCode := s.issuer.state.reserveAuthority(authority, time.Now())
		if reasonCode != "" {
			undo()
			s.issuer.observe(source, dynamicObservationDecisionDenied, reasonCode, authority)
			return "", false, newDynamicProxyError(reasonCode)
		}
		if _, reasonCode = reservation.resolve(s.ctx, target, selfTargets); reasonCode != "" {
			reservation.rollback()
			undo()
			s.issuer.observe(source, dynamicObservationDecisionDenied, reasonCode, authority)
			return "", false, newDynamicProxyError(reasonCode)
		}
		reservation.rollback()
		s.issuer.observe(source, dynamicObservationDecisionAllowed, dynamicObservationReasonCandidateAllowed, authority)
		seenKey := "dynamic\x00" + source + "\x00" + kind + "\x00" + strconv.Itoa(depth) + "\x00" + target.String()
		return s.rememberCapability(seenKey, token), true, nil
	}
	return "", false, nil
}

func (s *dynamicRewriteSession) structuredOutputLimit() int64 {
	if s == nil || s.issuer == nil {
		return 0
	}
	limit := s.outputLimit
	if limit <= 0 || limit > globalDynamicMaxStructuredOutputBytes {
		limit = globalDynamicMaxStructuredOutputBytes
	}
	if limit > s.issuer.policy.limits.MaxBodyBytes {
		limit = s.issuer.policy.limits.MaxBodyBytes
	}
	return limit
}

func (s *dynamicRewriteSession) rewrite(raw string) (string, error) {
	return s.rewriteAgainstKind(raw, s.base, dynamicCapabilityKindResource)
}

func (s *dynamicRewriteSession) rewriteManifest(raw string) (string, error) {
	return s.rewriteAgainstKind(raw, s.base, dynamicCapabilityKindManifest)
}

func structuredURLSharesAuthority(raw string, base *url.URL) bool {
	if base == nil || raw == "" || strings.Contains(raw, `\`) || containsDynamicUnsafeRune(raw) {
		return false
	}
	reference, err := url.Parse(raw)
	if err != nil || reference.User != nil || reference.Fragment != "" || reference.RawFragment != "" {
		return false
	}
	return sameRedirectAuthority(base, base.ResolveReference(reference))
}

func validateSameAuthorityStructuredURL(target *url.URL) error {
	if target == nil || len(target.String()) > maxDynamicTargetURLBytes || target.User != nil || target.Fragment != "" || target.RawFragment != "" || target.Host == "" {
		return fmt.Errorf("invalid same-authority structured URL")
	}
	scheme := strings.ToLower(target.Scheme)
	if scheme != "http" && scheme != "https" || !dynamicURLDecodedComponentIsSafe(target.EscapedPath(), false) || !dynamicURLDecodedComponentIsSafe(target.RawQuery, true) {
		return fmt.Errorf("invalid same-authority structured URL")
	}
	if dynamicURLPathHasDotSegments(target.EscapedPath()) {
		return fmt.Errorf("invalid same-authority structured URL")
	}
	if target.Port() != "" {
		port, err := strconv.Atoi(target.Port())
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("invalid same-authority structured URL")
		}
	}
	return nil
}

func normalizeTrustedCapabilityURL(value string) (*url.URL, error) {
	if value == "" || len(value) > maxDynamicTargetURLBytes || value != strings.TrimSpace(value) || containsDynamicUnsafeRune(value) || strings.Contains(value, `\`) || strings.Contains(value, "#") {
		return nil, fmt.Errorf("invalid trusted capability URL")
	}
	target, err := url.Parse(value)
	if err != nil || !target.IsAbs() || target.Opaque != "" {
		return nil, fmt.Errorf("invalid trusted capability URL")
	}
	target.Scheme = strings.ToLower(target.Scheme)
	host, _, err := normalizeDynamicHostSyntax(target.Hostname())
	if err != nil {
		return nil, fmt.Errorf("invalid trusted capability URL")
	}
	port := target.Port()
	if port == "" {
		if target.Scheme == "https" {
			port = "443"
		} else if target.Scheme == "http" {
			port = "80"
		}
	}
	target.Host = net.JoinHostPort(host, port)
	if err := validateSameAuthorityStructuredURL(target); err != nil {
		return nil, fmt.Errorf("invalid trusted capability URL")
	}
	return target, nil
}

// dynamicPolicyDenialError marks a rewrite failure as a security or policy
// refusal (unsafe URL, userinfo, scheme, fragment, count/depth limits, mint
// denial) rather than a format-compatibility problem. The upstream-preserve
// fallback must fail closed on these: relaying the original payload would
// hand the client raw absolute URLs the dynamic boundary refused to proxy.
type dynamicPolicyDenialError struct{ reason string }

func newDynamicPolicyDenialError(err error) *dynamicPolicyDenialError {
	if err == nil {
		return nil
	}
	return &dynamicPolicyDenialError{reason: err.Error()}
}

// dynamicURLScanCandidate reports whether a downstream-visible string is
// shaped like an absolute URL a client would follow, and whether it is safe to
// hand over unproxied. Only the destination selector is considered: a fragment
// or userinfo cannot be carried by a Meridian capability, so a URL using either
// would be followed by the player directly.
//
// A string that is not URL-shaped (a relative path, a bare filename, a DRM
// key-format identifier) is deliberately not a candidate: it cannot reach a
// third-party host on its own, and an unrelated string that merely parses as a
// URL must not block a compatibility preserve.
func dynamicURLScanCandidate(value string) (candidate, safe bool) {
	trimmed := strings.TrimSpace(value)
	lowered := strings.ToLower(trimmed)
	scheme := ""
	switch {
	case strings.HasPrefix(lowered, "http://"):
		scheme = "http://"
	case strings.HasPrefix(lowered, "https://"):
		scheme = "https://"
	default:
		// Non-http schemes are deliberately not candidates here. The strict
		// parsers already refuse them as a policy denial, so those bodies are
		// never preserved, and several of them — skd://, for instance — are DRM
		// identifiers that a player resolves through its own licence stack
		// rather than a URL it fetches. Treating them as unroutable would
		// reject ordinary FairPlay manifests for no security gain.
		//
		// A scheme followed by an authority is the one shape that could still
		// reach a third-party host, so it is refused unless it is known not to
		// name one. This keeps the scan conservative if a future strict parser
		// stops rejecting an unfamiliar scheme.
		if marker := strings.Index(lowered, "://"); marker > 0 {
			switch lowered[:marker] {
			case "skd", "blob", "data":
				return false, false
			default:
				return true, false
			}
		}
		return false, false
	}
	if len(trimmed) < len("http://x") {
		return true, false
	}
	// A URL-shaped string must be safe. Anything about it that cannot be
	// proven safe stays unsafe, including a host-less or opaque remainder.
	if trimmed != value || containsDynamicUnsafeRune(trimmed) || strings.Contains(trimmed, `\`) || strings.Contains(trimmed, "{$") {
		return true, false
	}
	hostPort := trimmed[len(scheme):]
	if cut := strings.IndexAny(hostPort, "/?#"); cut >= 0 {
		hostPort = hostPort[:cut]
	}
	if strings.HasPrefix(hostPort, "[") {
		// A bracketed host is the RFC 3986 IPv6-literal form. Accept exactly
		// the well-formed `[address]` or `[address]:port` shapes; every other
		// use of a bracket stays unsafe.
		closing := strings.IndexByte(hostPort, ']')
		if closing < 0 {
			return true, false
		}
		literal := net.ParseIP(hostPort[1:closing])
		remainder := hostPort[closing+1:]
		if literal == nil {
			return true, false
		}
		// An IPv4-mapped literal such as [::ffff:198.51.100.7] is an IPv4
		// destination in disguise, so it is judged by the address it maps to
		// rather than rejected for having a mapped form.
		addr, ok := netip.AddrFromSlice(literal)
		if !ok {
			return true, false
		}
		addr = addr.Unmap()
		if addr.Is4() {
			return true, false
		}
		if !dynamicIPIsPublic(addr) {
			return true, false
		}
		if remainder != "" && (remainder[0] != ':' || len(remainder) == 1) {
			return true, false
		}
		hostPort = hostPort[closing+1:]
	} else if strings.ContainsAny(hostPort, "[]") {
		return true, false
	}
	if strings.ContainsAny(hostPort, " \t\"'<>(){}|^`") {
		return true, false
	}
	if at := strings.LastIndexByte(hostPort, '@'); at >= 0 {
		return true, false
	}
	if strings.Contains(hostPort, "#") {
		return true, false
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || !parsed.IsAbs() || parsed.Opaque != "" || parsed.Host == "" || parsed.Hostname() == "" {
		return true, false
	}
	if len(parsed.String()) > maxDynamicTargetURLBytes {
		return true, false
	}
	// A fragment is carried by the client, never by the capability, so a
	// fragment-bearing URL would be followed outside Meridian.
	if parsed.Fragment != "" || parsed.RawFragment != "" {
		return true, false
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return true, false
	}
	if !dynamicURLDecodedComponentIsSafe(parsed.EscapedPath(), false) || !dynamicURLDecodedComponentIsSafe(parsed.RawQuery, true) {
		return true, false
	}
	if dynamicURLPathHasDotSegments(parsed.EscapedPath()) {
		return true, false
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return true, false
		}
	}
	// The strict rewriter refuses non-public resolved addresses, so an IP
	// literal pointing at a private, loopback, or otherwise special range is a
	// destination Meridian would never have proxied. Preserving the body would
	// hand that address to the client instead. The alternative numeric IPv4
	// spellings (127.1, 0x7f000001, 2130706433) are not parsed by net.ParseIP,
	// so they are matched by name below.
	host := parsed.Hostname()
	if literal := net.ParseIP(host); literal != nil {
		addr, ok := netip.AddrFromSlice(literal)
		if !ok || !dynamicIPIsPublic(addr.Unmap()) {
			return true, false
		}
	} else if dynamicScanHostIsNonPublicShorthand(host) {
		return true, false
	}
	return true, true
}

// dynamicScanHostIsNonPublicShorthand recognizes host names that resolve
// without DNS to a local or otherwise special address: the literal "localhost"
// and the alternative numeric IPv4 spellings that the resolver accepts but
// net.ParseIP does not. A strict rewrite never proxies these, so preserving a
// body that names one would expose it to the client instead.
func dynamicScanHostIsNonPublicShorthand(host string) bool {
	trimmed := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if trimmed == "" {
		return false
	}
	if trimmed == "localhost" || strings.HasSuffix(trimmed, ".localhost") {
		return true
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) > 4 {
		return false
	}
	octets := make([]int64, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return false
		}
		// Base 0 accepts the decimal, octal and hexadecimal spellings a
		// resolver accepts; anything outside a 32-bit range is not an address.
		value, err := strconv.ParseInt(part, 0, 64)
		if err != nil || value < 0 || value > 0xffffffff {
			return false
		}
		octets = append(octets, value)
	}
	// Every part before the last is a single octet; the last absorbs all
	// remaining bytes, which is why "127.1" means 127.0.0.1 and "2130706433"
	// means 127.0.0.1 as well. Every shift below operates on a value already
	// proven to be in [0, 0xffffffff], so the additions cannot overflow.
	address := int64(0)
	if len(octets) == 1 {
		address = octets[0]
	} else {
		for index := 0; index < len(octets)-1; index++ {
			if octets[index] > 0xff {
				return false
			}
			address |= octets[index] << (8 * (3 - index))
		}
		trailing := octets[len(octets)-1]
		switch len(octets) {
		case 2:
			trailing <<= 16
		case 3:
			trailing <<= 8
		}
		address |= trailing
	}
	bytes4 := [4]byte{
		byte((address >> 24) & 0xff),
		byte((address >> 16) & 0xff),
		byte((address >> 8) & 0xff),
		byte(address & 0xff),
	}
	return !dynamicIPIsPublic(netip.AddrFrom4(bytes4))
}

// scanHLSManifestURLs extracts every string a strict HLS rewrite would treat
// as a network location: media URI lines and the URL-bearing attributes of
// known tags. It is deliberately tolerant of the syntax errors that made the
// strict parser give up, because its only job is to prove that no URL in the
// payload would be followed outside Meridian.
func scanHLSManifestURLs(payload []byte) (candidate, unsafe bool) {
	sawCandidate := false
	for _, rawLine := range strings.Split(string(payload), "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			found, bad := dynamicURLScanValue(line)
			sawCandidate = sawCandidate || found
			if bad {
				return true, true
			}
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 || colon+1 >= len(line) {
			continue
		}
		tag := line[:colon]
		attributes, err := parseHLSAttributeList(line[colon+1:])
		if err != nil {
			// The strict parser stops at the first syntax error, which is often
			// the only reason this body is being preserved. A malformed list
			// must not hide a URL that appears earlier on the same line, so
			// fall back to scanning every quoted or bare value on it.
			for _, value := range hlsScanRawAttributeValues(line[colon+1:]) {
				found, bad := dynamicURLScanValue(value)
				sawCandidate = sawCandidate || found
				if bad {
					return true, true
				}
			}
			continue
		}
		for _, attribute := range attributes {
			value := line[colon+1:][attribute.valueStart:attribute.valueEnd]
			if !hlsAttributeNameCarriesURL(tag, attribute.name) {
				// Even an attribute that is not supposed to hold a URL must not
				// smuggle one. #EXT-X-DEFINE VALUE is the documented case: a
				// later {$name} reference expands it into a URI, so the URL
				// reaches the player through a tag the strict rewriter rejected
				// before it ever looked at the reference. Scanning every
				// attribute value covers that case without having to enumerate
				// which extension tags can feed a URI.
				if found, bad := dynamicURLScanValue(value); bad {
					return true, true
				} else {
					sawCandidate = sawCandidate || found
				}
				continue
			}
			found, bad := dynamicURLScanValue(value)
			sawCandidate = sawCandidate || found
			if bad {
				return true, true
			}
		}
	}
	return sawCandidate, false
}

// hlsScanRawAttributeValues extracts value-shaped tokens from an attribute
// list that failed to parse, so a syntax error cannot hide a URL on that line.
func hlsScanRawAttributeValues(body string) []string {
	values := make([]string, 0, 8)
	for index := 0; index < len(body); index++ {
		switch body[index] {
		case '"':
			close := strings.IndexByte(body[index+1:], '"')
			if close < 0 {
				values = append(values, body[index+1:])
				return values
			}
			values = append(values, body[index+1:index+1+close])
			index += close + 1
		case '=':
			start := index + 1
			if start < len(body) && body[start] == '"' {
				continue
			}
			end := start
			for end < len(body) && body[end] != ',' {
				end++
			}
			values = append(values, body[start:end])
			index = end
		}
	}
	return values
}

// hlsAttributeNameCarriesURL reports whether an attribute value is expected to
// name a network location, so the scan can distinguish a missing rewrite from a
// smuggled URL. It is intentionally broader than the strict rewriter's rewrite
// list: a tag the strict parser refused (content steering, for example) still
// carries an origin the player would fetch. Values of every other attribute are
// still scanned by the caller, so an unlisted URL-bearing name is not a hole.
func hlsAttributeNameCarriesURL(tag, name string) bool {
	switch name {
	case "URI", "URL", "X-URI", "X-URL", "X-ASSET-URI", "X-ASSET-LIST", "SERVER-URI", "BASE-URI":
		return true
	}
	return strings.HasSuffix(name, "-URI") || strings.HasSuffix(name, "-URL")
}

// scanDASHManifestURLs extracts XML attribute values and text nodes that are
// shaped like absolute URLs. Text nodes are included because the strict
// rewriter rewrites recognized text content and, more importantly, because an
// unrecognized feature inside a text node must not be handed to the player
// with a live third-party destination.
func scanDASHManifestURLs(payload []byte) (candidate, unsafe bool) {
	sawCandidate := false
	scan := func(value string) bool {
		for _, decoded := range dashScanDecodedValues(value) {
			found, bad := dynamicURLScanValue(decoded)
			sawCandidate = sawCandidate || found
			if bad {
				return true
			}
		}
		return false
	}
	rest := string(payload)
	for {
		open := strings.IndexByte(rest, '<')
		if open < 0 {
			break
		}
		rest = rest[open+1:]
		end := strings.IndexByte(rest, '>')
		if end < 0 {
			break
		}
		tag := rest[:end]
		rest = rest[end+1:]
		if strings.HasPrefix(tag, "!") {
			// CDATA carries text, not markup, so its content must be scanned as
			// a value rather than skipped with the other declarations.
			if body, ok := dashCDataBody(tag); ok {
				if scan(body) {
					return true, true
				}
			}
			continue
		}
		if strings.HasPrefix(tag, "/") || strings.HasPrefix(tag, "?") {
			continue
		}
		nameEnd := strings.IndexAny(tag, " \t\r\n/")
		if nameEnd < 0 {
			continue
		}
		for _, attribute := range dashScanAttributes(tag[nameEnd:]) {
			if scan(attribute) {
				return true, true
			}
		}
	}
	for _, text := range dashTextNodes(string(payload)) {
		if scan(text) {
			return true, true
		}
	}
	return sawCandidate, false
}

// dashCDataBody returns the section content when a `<!...>` token is a CDATA
// section.
func dashCDataBody(tag string) (string, bool) {
	const prefix = "![CDATA["
	if !strings.HasPrefix(tag, prefix) {
		return "", false
	}
	body := tag[len(prefix):]
	return strings.TrimSuffix(body, "]]"), true
}

// dashScanDecodedValues returns the value together with its numeric-character-
// reference decoding. An XML producer may legally write a scheme as entities
// (`&#104;ttp://…`), and the parsed document would then carry a real URL even
// though the raw bytes never contain the literal scheme.
func dashScanDecodedValues(value string) []string {
	values := []string{value}
	if !strings.Contains(value, "&#") {
		return values
	}
	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); {
		if value[index] == '&' && index+1 < len(value) && value[index+1] == '#' {
			end := strings.IndexByte(value[index:], ';')
			if end > 2 {
				digits := value[index+2 : index+end]
				base := 10
				if digits[0] == 'x' || digits[0] == 'X' {
					digits, base = digits[1:], 16
				}
				if code, err := strconv.ParseInt(digits, base, 32); err == nil && code > 0 && code <= 0x10ffff {
					builder.WriteRune(rune(code))
					index += end + 1
					continue
				}
			}
		}
		builder.WriteByte(value[index])
		index++
	}
	if decoded := builder.String(); decoded != value {
		values = append(values, decoded)
	}
	return values
}

// dashScanAttributes returns the quoted attribute values of one XML start tag.
func dashScanAttributes(body string) []string {
	values := make([]string, 0, 8)
	for index := 0; index < len(body); index++ {
		quote := body[index]
		if quote != '"' && quote != '\'' {
			continue
		}
		close := strings.IndexByte(body[index+1:], quote)
		if close < 0 {
			break
		}
		values = append(values, body[index+1:index+1+close])
		index += close + 1
	}
	return values
}

// dashTextNodes returns the character data between XML tags.
func dashTextNodes(document string) []string {
	nodes := make([]string, 0, 16)
	rest := document
	for {
		open := strings.IndexByte(rest, '>')
		if open < 0 {
			break
		}
		rest = rest[open+1:]
		next := strings.IndexByte(rest, '<')
		if next < 0 {
			if trimmed := strings.TrimSpace(rest); trimmed != "" {
				nodes = append(nodes, trimmed)
			}
			break
		}
		if trimmed := strings.TrimSpace(rest[:next]); trimmed != "" {
			nodes = append(nodes, trimmed)
		}
		rest = rest[next:]
	}
	return nodes
}

// dynamicURLScanValue classifies one extracted string.
func dynamicURLScanValue(value string) (candidate, unsafe bool) {
	candidate, safe := dynamicURLScanCandidate(value)
	return candidate, candidate && !safe
}

// preservedStructuredBodyIsSafe proves that preserving an upstream structured
// body verbatim will not hand the client a network destination Meridian cannot
// represent. Every URL-shaped string the strict rewriter would have rewritten
// must be one a capability could have carried.
//
// This closes the gap between "the strict parser rejected this manifest" and
// "therefore the original bytes are safe to forward". A parser that stops
// early on a vendor tag, an unsupported DRM descriptor, or an attribute error
// never reaches its per-URL validation, so its failure alone proves nothing
// about the URLs still in the payload. A relative path or a non-URL token
// proves nothing either way and is ignored.
func preservedStructuredBodyIsSafe(source string, payload []byte) bool {
	switch source {
	case dynamicDiscoverySourceHLS:
		_, unsafe := scanHLSManifestURLs(payload)
		return !unsafe
	case dynamicDiscoverySourceDASH:
		_, unsafe := scanDASHManifestURLs(payload)
		return !unsafe
	default:
		return true
	}
}

func (e *dynamicPolicyDenialError) Error() string { return e.reason }

func (s *dynamicRewriteSession) rewriteAgainstKind(raw string, base *url.URL, kind string) (string, error) {
	route, err := s.rewriteAgainstSourceKind(raw, base, s.source, kind)
	if err != nil {
		return route, newDynamicPolicyDenialError(err)
	}
	return route, nil
}

func (s *dynamicRewriteSession) rewriteAgainstSourceKind(raw string, base *url.URL, source, kind string) (string, error) {
	return s.rewriteAgainstSourceKindWithRequiredHeaders(raw, base, source, kind, nil)
}

func (s *dynamicRewriteSession) rewriteAgainstSourceKindWithRequiredHeaders(raw string, base *url.URL, source, kind string, requiredHeaders []dynamicCapabilityHeaderClaim) (string, error) {
	depth := 0
	if kind == dynamicCapabilityKindManifest {
		depth = s.depth + 1
	}
	return s.rewriteAgainstSourceKindDepthWithRequiredHeaders(raw, base, source, kind, depth, requiredHeaders)
}

func (s *dynamicRewriteSession) rewriteAgainstSourceKindDepth(raw string, base *url.URL, source, kind string, depth int) (string, error) {
	return s.rewriteAgainstSourceKindDepthWithRequiredHeaders(raw, base, source, kind, depth, nil)
}

func (s *dynamicRewriteSession) rewriteAgainstSourceKindDepthWithRequiredHeaders(raw string, base *url.URL, source, kind string, depth int, requiredHeaders []dynamicCapabilityHeaderClaim) (string, error) {
	if s == nil || s.issuer == nil || base == nil {
		return "", fmt.Errorf("invalid discovered URL: context")
	}
	switch {
	case raw == "":
		return "", fmt.Errorf("invalid discovered URL: empty")
	case raw != strings.TrimSpace(raw):
		return "", fmt.Errorf("invalid discovered URL: surrounding whitespace")
	case containsDynamicUnsafeRune(raw):
		return "", fmt.Errorf("invalid discovered URL: unsafe character")
	case strings.Contains(raw, `\`):
		return "", fmt.Errorf("invalid discovered URL: backslash")
	}
	if source == dynamicDiscoverySourcePlaybackInfo {
		if normalized, ok := normalizePlaybackInfoSchemelessURL(raw, base); ok {
			raw = normalized
		}
	}
	if err := validateDynamicCapabilityRequiredHeaderClaims(requiredHeaders); err != nil || len(requiredHeaders) > 0 {
		return "", fmt.Errorf("invalid discovered URL required headers")
	}
	if source == dynamicDiscoverySourceDASH && strings.Contains(raw, dashLiteralDollarClaimMarker) {
		return "", fmt.Errorf("DASH URL contains a reserved marker")
	}
	if err := s.ctx.Err(); err != nil {
		return "", fmt.Errorf("structured response deadline exceeded")
	}
	resourceDepthValid := validDynamicCapabilityResource(source, kind, depth)
	mayReuseOverDepth := kind == dynamicCapabilityKindManifest && (source == dynamicDiscoverySourceHLS || source == dynamicDiscoverySourceDASH) && depth > maxDynamicManifestDepth
	if !resourceDepthValid && !mayReuseOverDepth {
		return "", fmt.Errorf("invalid structured resource kind or depth")
	}
	s.urlCount++
	if s.urlCount > s.issuer.policy.limits.MaxURLsPerResponse {
		return "", fmt.Errorf("discovered URL count exceeds its limit")
	}
	reference, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid discovered URL: parse")
	}
	if reference.User != nil {
		return "", fmt.Errorf("invalid discovered URL: userinfo")
	}
	if reference.Fragment != "" || reference.RawFragment != "" {
		return "", fmt.Errorf("invalid discovered URL: fragment")
	}
	resolved := base.ResolveReference(reference)
	if len(requiredHeaders) == 0 && len(s.inheritedHeaders) > 0 && sameRedirectAuthority(s.base, resolved) {
		requiredHeaders = s.inheritedHeaders
	}
	headerKey := dynamicCapabilityRequiredHeadersCacheKey(requiredHeaders)
	configuredStructuredTarget := s.issuer.configuredAuthorities[redirectHostKey(resolved)] && (source == dynamicDiscoverySourceHLS || source == dynamicDiscoverySourceDASH || source == dynamicDiscoverySourcePlaybackInfo && (reference.IsAbs() || reference.Host != "") || len(requiredHeaders) > 0)
	if configuredStructuredTarget {
		target, err := normalizeTrustedCapabilityURL(resolved.String())
		if err != nil {
			return "", fmt.Errorf("invalid configured structured URL")
		}
		seenKey := "trusted\x00" + source + "\x00" + kind + "\x00" + strconv.Itoa(depth) + "\x00" + target.String() + headerKey
		if route, exists := s.seen[seenKey]; exists {
			return route, nil
		}
		if len(requiredHeaders) == 0 {
			if route, reused := s.reuseShallowTrustedManifest(target, source, kind, depth); reused {
				return route, nil
			}
		}
		if !resourceDepthValid {
			return "", fmt.Errorf("manifest nesting exceeds its depth limit")
		}
		route, acquired, discoveryErr := s.issuer.mintTrustedValidatedWithRequiredHeadersTracked(target, target.String(), nil, nil, requiredHeaders, source, kind, depth)
		if discoveryErr != nil {
			return "", discoveryErr
		}
		if s.seen == nil {
			s.seen = make(map[string]string)
		}
		s.seen[seenKey] = route
		if acquired {
			s.minted = append(s.minted, s.issuer.capabilityToken(route))
		}
		return route, nil
	}
	if resourceDepthValid && len(requiredHeaders) == 0 && !s.rewriteRelative && sameRedirectAuthority(s.base, resolved) {
		if err := validateSameAuthorityStructuredURL(resolved); err != nil {
			return "", err
		}
		return resolved.RequestURI(), nil
	}
	target, err := normalizeDynamicURL(resolved.String())
	if err != nil {
		return "", fmt.Errorf("invalid discovered URL: target normalization %s", dynamicURLNormalizationDiagnosticCode(err))
	}
	seenKey := "dynamic\x00" + source + "\x00" + kind + "\x00" + strconv.Itoa(depth) + "\x00" + target.String() + headerKey
	if route, exists := s.seen[seenKey]; exists {
		return route, nil
	}
	if len(requiredHeaders) == 0 {
		if route, reused, reuseErr := s.reuseShallowDynamicManifest(base, target, source, kind, depth); reuseErr != nil {
			return "", reuseErr
		} else if reused {
			return route, nil
		}
	}
	if !resourceDepthValid {
		return "", fmt.Errorf("manifest nesting exceeds its depth limit")
	}
	route, acquired, discoveryErr := s.issuer.mintValidatedResourceWithRequiredHeadersTracked(s.ctx, base, target, source, target.String(), nil, nil, requiredHeaders, kind, depth)
	if discoveryErr != nil {
		return "", discoveryErr
	}
	if s.seen == nil {
		s.seen = make(map[string]string)
	}
	s.seen[seenKey] = route
	if acquired {
		s.minted = append(s.minted, s.issuer.capabilityToken(route))
	}
	return route, nil
}

func (s *dynamicRewriteSession) commit() bool {
	if s == nil || s.issuer == nil || s.issuer.state == nil {
		return false
	}
	tokens := s.minted
	s.minted = nil
	return s.issuer.state.settleCapabilities(tokens, true, time.Now())
}

func (s *dynamicRewriteSession) rollback() {
	if s == nil || s.issuer == nil || s.issuer.state == nil {
		return
	}
	tokens := s.minted
	s.minted = nil
	s.learnedPlaybackPaths = nil
	_ = s.issuer.state.settleCapabilities(tokens, false, time.Now())
}

func dynamicResponseMediaType(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return ""
	}
	return strings.ToLower(mediaType)
}

func dynamicResponseIsActiveContent(resp *http.Response) bool {
	switch dynamicResponseMediaType(resp) {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "text/javascript", "application/javascript", "application/ecmascript", "text/ecmascript":
		return true
	default:
		return false
	}
}

func dynamicStructuredMethodAllowed(source, method string) bool {
	switch source {
	case dynamicDiscoverySourcePlaybackInfo:
		return method == http.MethodGet || method == http.MethodPost || method == http.MethodHead
	case dynamicDiscoverySourceHLS, dynamicDiscoverySourceDASH:
		return method == http.MethodGet || method == http.MethodHead
	default:
		return false
	}
}

func dynamicStructuredContentTypeAllowed(source string, resp *http.Response) bool {
	mediaType := dynamicResponseMediaType(resp)
	switch source {
	case dynamicDiscoverySourcePlaybackInfo:
		return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
	case dynamicDiscoverySourceHLS:
		return mediaType == "application/vnd.apple.mpegurl" || mediaType == "application/x-mpegurl" || mediaType == "audio/mpegurl" || mediaType == "audio/x-mpegurl" || mediaType == "text/plain" || mediaType == "application/octet-stream"
	case dynamicDiscoverySourceDASH:
		return mediaType == "application/dash+xml" || mediaType == "application/xml" || mediaType == "text/xml" || mediaType == "application/octet-stream"
	default:
		return false
	}
}

func dynamicStructuredResponseSource(resp *http.Response) (string, bool) {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return "", false
	}
	requestPath := strings.ToLower(resp.Request.URL.Path)
	mediaType := dynamicResponseMediaType(resp)
	if isPlaybackInfoRequest(requestPath) && dynamicStructuredMethodAllowed(dynamicDiscoverySourcePlaybackInfo, resp.Request.Method) {
		return dynamicDiscoverySourcePlaybackInfo, dynamicStructuredContentTypeAllowed(dynamicDiscoverySourcePlaybackInfo, resp)
	}
	hlsType := mediaType == "application/vnd.apple.mpegurl" || mediaType == "application/x-mpegurl" || mediaType == "audio/mpegurl" || mediaType == "audio/x-mpegurl"
	if (hlsType || strings.HasSuffix(requestPath, ".m3u8") || strings.HasSuffix(requestPath, ".m3u")) && dynamicStructuredMethodAllowed(dynamicDiscoverySourceHLS, resp.Request.Method) {
		return dynamicDiscoverySourceHLS, dynamicStructuredContentTypeAllowed(dynamicDiscoverySourceHLS, resp)
	}
	dashType := mediaType == "application/dash+xml"
	if (dashType || strings.HasSuffix(requestPath, ".mpd")) && dynamicStructuredMethodAllowed(dynamicDiscoverySourceDASH, resp.Request.Method) {
		return dynamicDiscoverySourceDASH, dynamicStructuredContentTypeAllowed(dynamicDiscoverySourceDASH, resp)
	}
	return "", false
}

func dynamicResponseHasPositiveStructuredContentType(resp *http.Response) bool {
	mediaType := dynamicResponseMediaType(resp)
	switch mediaType {
	case "application/vnd.apple.mpegurl", "application/x-mpegurl", "audio/mpegurl", "audio/x-mpegurl", "application/dash+xml":
		return true
	case "application/json":
		return resp != nil && resp.Request != nil && resp.Request.URL != nil && isPlaybackInfoRequest(resp.Request.URL.Path)
	default:
		return strings.HasSuffix(mediaType, "+json") && resp != nil && resp.Request != nil && resp.Request.URL != nil && isPlaybackInfoRequest(resp.Request.URL.Path)
	}
}

func dynamicStructuredWorkingSet(resp *http.Response, profileLimit int64) (memory, inputLimit, outputLimit int64, err error) {
	return dynamicStructuredWorkingSetWithin(resp, profileLimit, globalDynamicMaxSiteParseMemoryBytes)
}

func dynamicStructuredWorkingSetWithin(resp *http.Response, profileLimit, memoryLimit int64) (memory, inputLimit, outputLimit int64, err error) {
	if resp == nil || profileLimit <= 0 || memoryLimit < 8<<20 {
		return 0, 0, 0, fmt.Errorf("structured response budget is unavailable")
	}
	memoryLimit = min(memoryLimit, int64(globalDynamicMaxSiteParseMemoryBytes))
	inputLimit = min(profileLimit, int64(globalDynamicMaxStructuredInputBytes))
	outputLimit = min(profileLimit, int64(globalDynamicMaxStructuredOutputBytes))
	memory = memoryLimit
	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if (encoding == "" || encoding == "identity") && resp.ContentLength >= 0 {
		if resp.ContentLength > inputLimit {
			return 0, 0, 0, fmt.Errorf("structured response working set exceeds its limit")
		}
		memory = resp.ContentLength*8 + (4 << 20)
		if memory < 8<<20 {
			memory = 8 << 20
		}
		if memory > memoryLimit {
			return 0, 0, 0, fmt.Errorf("structured response working set exceeds its limit")
		}
	}
	if (encoding != "" && encoding != "identity" || resp.ContentLength < 0) && inputLimit > memory/8 {
		inputLimit = memory / 8
	}
	if quarter := memory / 4; outputLimit > quarter {
		outputLimit = quarter
	}
	if inputLimit <= 0 || outputLimit <= 0 {
		return 0, 0, 0, fmt.Errorf("structured response working set exceeds its limit")
	}
	return memory, inputLimit, outputLimit, nil
}

type dynamicBoundedBuffer struct {
	buffer bytes.Buffer
	limit  int64
}

func (b *dynamicBoundedBuffer) Write(payload []byte) (int, error) {
	if b == nil || b.limit < 0 || int64(len(payload)) > b.limit-int64(b.buffer.Len()) {
		return 0, fmt.Errorf("structured response output exceeds its limit")
	}
	return b.buffer.Write(payload)
}

func (b *dynamicBoundedBuffer) WriteString(value string) (int, error) {
	if b == nil || b.limit < 0 || int64(len(value)) > b.limit-int64(b.buffer.Len()) {
		return 0, fmt.Errorf("structured response output exceeds its limit")
	}
	return b.buffer.WriteString(value)
}

func (b *dynamicBoundedBuffer) Bytes() []byte {
	if b == nil {
		return nil
	}
	return b.buffer.Bytes()
}

func readDynamicStructuredBody(resp *http.Response, limit int64) ([]byte, error) {
	if resp == nil || resp.Body == nil || limit <= 0 {
		return nil, fmt.Errorf("structured response body is unavailable")
	}
	defer resp.Body.Close()
	timer := time.AfterFunc(dynamicStructuredBodyTimeout, func() {
		_ = resp.Body.Close()
	})
	defer timer.Stop()
	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	var reader io.Reader = resp.Body
	var gzipReader *gzip.Reader
	var compressed *io.LimitedReader
	switch encoding {
	case "", "identity":
		if resp.ContentLength > limit {
			return nil, fmt.Errorf("structured response body exceeds its limit")
		}
	case "gzip":
		if resp.ContentLength > limit {
			return nil, fmt.Errorf("compressed structured response body exceeds its limit")
		}
		compressed = &io.LimitedReader{R: resp.Body, N: limit + 1}

		var err error
		gzipReader, err = gzip.NewReader(compressed)
		if err != nil {
			return nil, fmt.Errorf("invalid gzip response body")
		}
		defer gzipReader.Close()
		reader = gzipReader
	default:
		return nil, fmt.Errorf("unsupported structured response encoding")
	}
	payload, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read structured response body: %w", err)
	}
	if int64(len(payload)) > limit {
		return nil, fmt.Errorf("structured response body exceeds its limit")
	}
	if compressed != nil {
		compressedBytes := limit + 1 - compressed.N
		if compressedBytes > limit {
			return nil, fmt.Errorf("compressed structured response body exceeds its limit")
		}
		if int64(len(payload)) > minDynamicCompressionRatioBytes && (compressedBytes <= 0 || int64(len(payload)) > compressedBytes*maxDynamicCompressionRatio) {
			return nil, fmt.Errorf("structured response compression ratio exceeds its limit")
		}
	}
	return payload, nil
}

// installDynamicStructuredBody installs a (re-)encoded structured body. When
// the rewrite changed nothing, the body's bytes still match the stored payload
// but the stored validators describe them exactly, so the cache headers are
// preserved for conditional revalidation; they are stripped only when the
// content was actually rewritten, where they would be lies.
func installDynamicStructuredBody(resp *http.Response, payload []byte, rewritten bool) {
	resp.Body = io.NopCloser(bytes.NewReader(payload))
	resp.ContentLength = int64(len(payload))
	resp.Uncompressed = true
	resp.Trailer = nil
	// The installed body is always the decoded payload, so wire-representation
	// headers (Content-Encoding from a gunzipped original, and checksums
	// computed over those wire bytes) are stale in every mode and must go.
	for _, name := range []string{
		"Content-Encoding", "Content-MD5", "Digest",
	} {
		resp.Header.Del(name)
	}
	if rewritten {
		for _, name := range []string{
			"Accept-Ranges", "Content-Range", "ETag", "Last-Modified", "Vary",
		} {
			resp.Header.Del(name)
		}
	}
	resp.Header.Set("Content-Length", strconv.Itoa(len(payload)))
	if rewritten {
		resp.Header.Set("Cache-Control", "private, no-store")
	}
	resp.Header.Set("Referrer-Policy", "no-referrer")
	resp.Header.Set("X-Content-Type-Options", "nosniff")
}

func sanitizeDynamicUpstreamErrorResponse(resp *http.Response, payload []byte) {
	if resp == nil {
		return
	}
	retryAfter := append([]string(nil), resp.Header.Values("Retry-After")...)
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	header := make(http.Header)
	if len(retryAfter) > 0 {
		header["Retry-After"] = retryAfter
	}
	resp.Header = header
	resp.Trailer = nil
	if resp.Request != nil && resp.Request.Method == http.MethodHead {
		resp.Body = http.NoBody
		resp.ContentLength = -1
		return
	}
	resp.Body = io.NopCloser(bytes.NewReader(payload))
	resp.ContentLength = int64(len(payload))
	resp.Header.Set("Content-Length", strconv.Itoa(len(payload)))
	resp.Header.Set("Content-Type", "application/json")
}

func sanitizeDynamicManifestErrorResponse(resp *http.Response) {
	sanitizeDynamicUpstreamErrorResponse(resp, []byte(`{"error":"upstream manifest request failed"}`))
}

func sanitizeDynamicResourceErrorResponse(resp *http.Response) {
	sanitizeDynamicUpstreamErrorResponse(resp, []byte(`{"error":"upstream dynamic request failed"}`))
}

var errDynamicCapabilityExpiredDuringUse = errors.New("dynamic capability expired during use")

func rewriteDynamicStructuredResponse(resp *http.Response, issuer *dynamicCapabilityIssuer, rewriteRelative bool) error {
	return rewriteDynamicStructuredResponseExpected(resp, issuer, rewriteRelative, "", 0, false)
}

func rewriteDynamicStructuredResponseExpected(resp *http.Response, issuer *dynamicCapabilityIssuer, rewriteRelative bool, expectedSource string, depth int, required bool) error {
	return rewriteDynamicStructuredResponseAccepted(resp, issuer, rewriteRelative, expectedSource, depth, required, nil, nil)
}

func dynamicStructuredRewriteDeniedReason(source string) string {
	switch source {
	case dynamicDiscoverySourcePlaybackInfo:
		return dynamicObservationReasonPlaybackInfoDenied
	case dynamicDiscoverySourceHLS:
		return dynamicObservationReasonHLSFeatureDenied
	case dynamicDiscoverySourceDASH:
		return dynamicObservationReasonDASHFeatureDenied
	default:
		return dynamicObservationReasonParseFailure
	}
}

func rewriteDynamicStructuredResponseAccepted(resp *http.Response, issuer *dynamicCapabilityIssuer, rewriteRelative bool, expectedSource string, depth int, required bool, inheritedHeaders []dynamicCapabilityHeaderClaim, accept func() bool) error {
	dynamicResponseAuthorityLease(resp).retainThroughRewrite()
	source, contentTypeAllowed := dynamicStructuredResponseSource(resp)
	if expectedSource != "" && dynamicStructuredContentTypeAllowed(expectedSource, resp) {
		source = expectedSource
		contentTypeAllowed = true
	}
	if issuer == nil {
		return nil
	}
	authority := ""
	if resp != nil && resp.Request != nil {
		authority = dynamicCanonicalAuthority(resp.Request.URL)
	}
	recordFailure := func(reasonCode string) error {
		observationSource := source
		if expectedSource != "" {
			observationSource = expectedSource
		}
		issuer.observe(observationSource, dynamicObservationDecisionDenied, reasonCode, authority)
		return newDynamicProxyError(reasonCode)
	}
	if expectedSource != "" && rewriteRelative && resp != nil && resp.StatusCode < http.StatusBadRequest && (source != expectedSource || !contentTypeAllowed) {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return recordFailure(dynamicObservationReasonRequestUnclassified)
	}
	if required && (!issuer.policy.sourceEnabled(expectedSource) || !validDynamicCapabilityResource(expectedSource, dynamicCapabilityKindManifest, depth)) {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return recordFailure(dynamicObservationReasonRequestUnclassified)
	}
	if required && resp != nil && resp.StatusCode >= http.StatusBadRequest {
		sanitizeDynamicManifestErrorResponse(resp)
		return nil
	}
	if required && (source == "" || source != expectedSource || !contentTypeAllowed) {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return recordFailure(dynamicObservationReasonRequestUnclassified)
	}
	if rewriteRelative && resp != nil && resp.StatusCode < http.StatusBadRequest && source != "" && contentTypeAllowed && !issuer.policy.sourceEnabled(source) {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return recordFailure(dynamicObservationReasonRequestUnclassified)
	}
	if source == "" || !issuer.policy.sourceEnabled(source) {
		return nil
	}
	if resp.Request.Method == http.MethodHead {
		if resp.StatusCode >= http.StatusBadRequest {
			return nil
		}
		if !contentTypeAllowed || resp.StatusCode != http.StatusOK {
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			return recordFailure(dynamicObservationReasonRequestUnclassified)
		}
		resp.ContentLength = -1
		for _, name := range []string{"Accept-Ranges", "Content-Length", "Content-MD5", "Content-Range", "Digest", "ETag", "Last-Modified", "Vary"} {
			resp.Header.Del(name)
		}
		resp.Header.Set("Cache-Control", "private, no-store")
		resp.Header.Set("Referrer-Policy", "no-referrer")
		resp.Header.Set("X-Content-Type-Options", "nosniff")
		if accept != nil && !accept() {
			return errDynamicCapabilityExpiredDuringUse
		}
		return nil
	}
	if resp.StatusCode >= http.StatusBadRequest {
		return nil
	}
	if !contentTypeAllowed || resp.StatusCode != http.StatusOK {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return recordFailure(dynamicObservationReasonRequestUnclassified)
	}
	parseContext, cancelParse := context.WithTimeout(resp.Request.Context(), dynamicStructuredBodyTimeout)
	defer cancelParse()
	workingMemory, inputLimit, outputLimit, budgetErr := dynamicStructuredWorkingSetWithin(resp, issuer.policy.limits.MaxBodyBytes, issuer.state.availableParseMemory())
	if budgetErr != nil {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		issuer.observe(source, dynamicObservationDecisionDenied, dynamicObservationReasonCapacityLimit, authority)
		return newDynamicProxyError(dynamicObservationReasonCapacityLimit)
	}
	release, acquired := issuer.state.acquireParse(workingMemory)
	if !acquired {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		issuer.observe(source, dynamicObservationDecisionDenied, dynamicObservationReasonCapacityLimit, authority)
		return newDynamicProxyError(dynamicObservationReasonCapacityLimit)
	}
	defer release()
	payload, err := readDynamicStructuredBody(resp, inputLimit)
	if err != nil {
		return recordFailure(dynamicObservationReasonStructuredBodyLimit)
	}
	var learningBase *url.URL
	if resp.Request != nil {
		if stored, ok := resp.Request.Context().Value(dynamicPlaybackInfoBaseContextKey{}).(*url.URL); ok && stored != nil {
			clone := *stored
			learningBase = &clone
		}
	}
	session := &dynamicRewriteSession{ctx: parseContext, issuer: issuer, base: resp.Request.URL, learningBase: learningBase, source: source, depth: depth, outputLimit: outputLimit, rewriteRelative: rewriteRelative, inheritedHeaders: inheritedHeaders}
	var rewritten []byte
	switch source {
	case dynamicDiscoverySourcePlaybackInfo:
		rewritten, err = rewritePlaybackInfoResponse(payload, session)
	case dynamicDiscoverySourceHLS:
		rewritten, err = rewriteHLSResponse(payload, session)
	case dynamicDiscoverySourceDASH:
		rewritten, err = rewriteDASHResponse(payload, session)
	}
	if err != nil && source == dynamicDiscoverySourcePlaybackInfo && playbackInfoAutomaticFallbackAllowed(err) {
		session.rollback()
		fallbackSession := &dynamicRewriteSession{
			ctx:              parseContext,
			issuer:           issuer,
			base:             resp.Request.URL,
			learningBase:     learningBase,
			source:           source,
			depth:            depth,
			outputLimit:      outputLimit,
			rewriteRelative:  false,
			inheritedHeaders: inheritedHeaders,
		}
		fallback, fallbackErr := rewriteAutomaticPlaybackInfoResponse(payload, fallbackSession)
		if fallbackErr == nil {
			log.Printf("[%s] PlaybackInfo switched to automatic URL proxy fallback: diagnostic=%s", issuer.site.Name, playbackInfoRewriteDiagnosticCode(err))
			session = fallbackSession
			rewritten = fallback
			err = nil
		} else {
			fallbackSession.rollback()
			if accept != nil && !accept() {
				return errDynamicCapabilityExpiredDuringUse
			}
			// The schema-free walker refused a URL it recognized but could not
			// route. Falling back to the upstream bytes here would be worse
			// than the failure it avoids: the client would receive the whole
			// body with every URL unproxied, so one unroutable field would cost
			// the capability, the traffic accounting and the quota for all of
			// them. Fail the response instead.
			log.Printf("[%s] PlaybackInfo rewrite and automatic fallback both rejected: diagnostic=%s", issuer.site.Name, playbackInfoRewriteDiagnosticCode(fallbackErr))
			return recordFailure(dynamicObservationReasonPlaybackInfoDenied)
		}
	}
	if err != nil {
		session.rollback()
		if source == dynamicDiscoverySourcePlaybackInfo {
			log.Printf("[%s] PlaybackInfo rewrite rejected: diagnostic=%s fingerprint=%s profile=%s", issuer.site.Name, playbackInfoRewriteDiagnosticCode(err), playbackInfoRewriteDiagnosticFingerprint(err), issuer.policy.profile)
		}
		// A manifest the strict rewriter cannot parse is not a client error:
		// real-world playlists routinely carry vendor tags, BOMs, DRM
		// descriptors, or foreign XML namespaces that the strict parser
		// deliberately rejects. Relay the upstream response verbatim instead
		// of replacing a working 200 with a hard 502 that would break playback
		// for the whole site. PlaybackInfo keeps its own stricter chain: its
		// denials (for example required headers on a relative URL that no
		// capability could carry) must stay hard errors.
		//
		// Preserving is only safe once the payload has been proven to contain
		// no network destination Meridian could not have represented: the
		// strict parser stops at the first unsupported feature, so its failure
		// says nothing about the URLs that follow it.
		var denial *dynamicPolicyDenialError
		if resp != nil && resp.Body != nil && resp.StatusCode < http.StatusBadRequest &&
			(source == dynamicDiscoverySourceHLS || source == dynamicDiscoverySourceDASH) &&
			!errors.As(err, &denial) {
			if !preservedStructuredBodyIsSafe(source, payload) {
				log.Printf("[%s] %s rewrite rejected and the body contains an unroutable URL; refusing to preserve it", issuer.site.Name, source)
				return recordFailure(dynamicStructuredRewriteDeniedReason(source))
			}
			log.Printf("[%s] %s rewrite rejected; preserving upstream response: %v", issuer.site.Name, source, err)
			issuer.observe(source, dynamicObservationDecisionDenied, dynamicStructuredRewriteDeniedReason(source), authority)
			installDynamicStructuredBody(resp, payload, false)
			return nil
		}
		return recordFailure(dynamicStructuredRewriteDeniedReason(source))
	}
	if int64(len(rewritten)) > outputLimit {
		session.rollback()
		return recordFailure(dynamicObservationReasonStructuredBodyLimit)
	}
	if accept != nil && !accept() {
		session.rollback()
		return errDynamicCapabilityExpiredDuringUse
	}
	if !session.commit() {
		issuer.observe(source, dynamicObservationDecisionDenied, dynamicObservationReasonCapacityLimit, authority)
		return newDynamicProxyError(dynamicObservationReasonCapacityLimit)
	}
	session.publishLearnedPlaybackPaths()
	installDynamicStructuredBody(resp, rewritten, !bytes.Equal(payload, rewritten))
	return nil
}

func validateDynamicJSONStructureWithin(ctx context.Context, payload []byte, maxTokens, maxDepth int) (int, error) {
	if ctx == nil || ctx.Err() != nil {
		return 0, fmt.Errorf("JSON parsing deadline exceeded")
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	depth := 0
	seenValue := false
	tokens := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
		seenValue = true
		tokens++
		if tokens&255 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, fmt.Errorf("JSON parsing deadline exceeded")
			}
		}
		if tokens > maxTokens {
			return 0, fmt.Errorf("JSON token count exceeds its limit")
		}
		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{', '[':
				depth++
				if depth > maxDepth {
					return 0, fmt.Errorf("JSON nesting exceeds its limit")
				}
			case '}', ']':
				depth--
				if depth < 0 {
					return 0, fmt.Errorf("invalid JSON nesting")
				}
			}
		case string:
			if int64(len(value)) > globalDynamicMaxStringBytes {
				return 0, fmt.Errorf("JSON string exceeds its limit")
			}
		}
	}
	if !seenValue || depth != 0 {
		return 0, fmt.Errorf("invalid JSON body")
	}
	return tokens, nil
}
