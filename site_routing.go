package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func normalizeTargetURL(addr string) (*url.URL, error) {
	addr = strings.TrimSpace(addr)
	addr = strings.ReplaceAll(addr, "：", ":")
	if addr == "" {
		return nil, fmt.Errorf("target URL is required")
	}
	if len(addr) > 2048 {
		return nil, fmt.Errorf("target URL is too long")
	}
	explicitScheme := strings.Contains(addr, "://")
	if !explicitScheme {
		addr = "http://" + addr
	}
	parsed, err := url.Parse(addr)
	if err != nil {
		return nil, err
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if !explicitScheme && parsed.Port() == "443" {
		parsed.Scheme = "https"
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Hostname() == "" || parsed.Opaque != "" {
		return nil, fmt.Errorf("invalid target URL")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("target URL must not contain credentials")
	}
	if parsed.Fragment != "" {
		return nil, fmt.Errorf("target URL must not contain a fragment")
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return nil, fmt.Errorf("target URL contains an invalid port")
		}
	}
	return parsed, nil
}

// redactUpstreamURL keeps operator logs useful without retaining credentials,
// paths, or signed query parameters from configured upstreams.
func redactUpstreamURL(target *url.URL) string {
	if target == nil || target.Scheme == "" || target.Host == "" {
		return "configured upstream"
	}
	return strings.ToLower(target.Scheme) + "://" + target.Host
}

func redirectHostKey(target *url.URL) string {
	host := strings.ToLower(strings.TrimSuffix(target.Hostname(), "."))
	if host == "" {
		return ""
	}
	port := target.Port()
	scheme := strings.ToLower(target.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	authority := host
	if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	if port != "" && !((scheme == "http" && port == "80") || (scheme == "https" && port == "443")) {
		authority = net.JoinHostPort(host, port)
	}
	return scheme + "://" + authority
}

// sameRedirectAuthority reports whether two URLs share scheme, host, and
// effective port. Redirects that stay within the same authority may keep the
// client's headers; anything else is a cross-origin hop.
func sameRedirectAuthority(from, to *url.URL) bool {
	return redirectHostKey(from) == redirectHostKey(to)
}

func singleJoiningSlash(a, b string) string {
	aSlash := strings.HasSuffix(a, "/")
	bSlash := strings.HasPrefix(b, "/")
	switch {
	case aSlash && bSlash:
		return a + b[1:]
	case !aSlash && !bSlash:
		return a + "/" + b
	default:
		return a + b
	}
}

func joinURLPath(base, request *url.URL) (joinedPath, joinedRawPath string) {
	if base.RawPath == "" && request.RawPath == "" {
		return singleJoiningSlash(base.Path, request.Path), ""
	}
	basePath := base.EscapedPath()
	requestPath := request.EscapedPath()
	baseSlash := strings.HasSuffix(basePath, "/")
	requestSlash := strings.HasPrefix(requestPath, "/")
	switch {
	case baseSlash && requestSlash:
		return base.Path + request.Path[1:], basePath + requestPath[1:]
	case !baseSlash && !requestSlash:
		return base.Path + "/" + request.Path, basePath + "/" + requestPath
	default:
		return base.Path + request.Path, basePath + requestPath
	}
}

func applyUpstreamURL(requestURL, upstream *url.URL) {
	requestURL.Scheme = upstream.Scheme
	requestURL.Host = upstream.Host
	requestURL.Path, requestURL.RawPath = joinURLPath(upstream, requestURL)
	switch {
	case upstream.RawQuery == "":
	case requestURL.RawQuery == "":
		requestURL.RawQuery = upstream.RawQuery
	default:
		requestURL.RawQuery = upstream.RawQuery + "&" + requestURL.RawQuery
	}
}

func normalizePublicHost(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	value = strings.TrimSuffix(value, ".")
	if value == "" || len(value) > 253 || !strings.Contains(value, ".") {
		return "", fmt.Errorf("public_host must be a fully-qualified DNS name")
	}
	if strings.ContainsAny(value, "/:*[]") || net.ParseIP(value) != nil {
		return "", fmt.Errorf("public_host must not contain a scheme, path, port, wildcard, or IP address")
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("public_host contains an invalid DNS label")
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", fmt.Errorf("public_host must use ASCII DNS labels; encode international names as punycode")
			}
		}
	}
	return value, nil
}

// normalizeRoutePrefix validates the single DNS label used by the optional
// domain-prefix ingress. The full public_host remains the persisted routing
// key so existing databases and Host routing stay compatible.
func normalizeRoutePrefix(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimSuffix(value, ".")
	if value == "" || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return "", fmt.Errorf("route_prefix must be a valid DNS label")
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return "", fmt.Errorf("route_prefix must use only ASCII letters, digits, and hyphens")
		}
	}
	return value, nil
}

func normalizeRouteDomain(value string) (string, error) {
	return normalizePublicHost(value)
}

func routeHostForPrefix(prefix, domain string) (string, error) {
	prefix, err := normalizeRoutePrefix(prefix)
	if err != nil {
		return "", err
	}
	domain, err = normalizeRouteDomain(domain)
	if err != nil {
		return "", fmt.Errorf("invalid route domain: %w", err)
	}
	if domain == "" {
		return "", fmt.Errorf("PANEL_ROUTE_DOMAIN is required for domain-prefix ingress")
	}
	host := prefix + "." + domain
	if len(host) > 253 {
		return "", fmt.Errorf("route host is too long")
	}
	return host, nil
}

func routePrefixFromHost(host, domain string) string {
	host, err := normalizePublicHost(host)
	if err != nil {
		return ""
	}
	domain, err = normalizeRouteDomain(domain)
	if err != nil || domain == "" {
		return ""
	}
	suffix := "." + domain
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	prefix := strings.TrimSuffix(host, suffix)
	if strings.Contains(prefix, ".") {
		return ""
	}
	prefix, err = normalizeRoutePrefix(prefix)
	if err != nil {
		return ""
	}
	return prefix
}

func normalizeIngressMode(value, publicHost string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(value))
	if mode == "" {
		if publicHost != "" {
			mode = ingressModeHost
		} else {
			mode = ingressModePort
		}
	}
	switch mode {
	case ingressModePort:
		if publicHost != "" {
			return "", fmt.Errorf("public_host must be empty when ingress_mode is port")
		}
	case ingressModeHost, ingressModeBoth:
		if publicHost == "" {
			return "", fmt.Errorf("public_host is required when ingress_mode is %s", mode)
		}
	default:
		return "", fmt.Errorf("ingress_mode must be port, host, or both")
	}
	return mode, nil
}

func ingressUsesPort(mode string) bool {
	return mode == ingressModePort || mode == ingressModeBoth
}

func ingressUsesHost(mode string) bool {
	return mode == ingressModeHost || mode == ingressModeBoth
}

const internalHostOnlyPortStart = 8001

func nextAvailableInternalSitePort(sites []Site, panelPort int) (int, error) {
	used := make(map[int]struct{}, len(sites)+1)
	if panelPort >= 1 && panelPort <= 65535 {
		used[panelPort] = struct{}{}
	}
	for _, site := range sites {
		if site.ListenPort >= 1 && site.ListenPort <= 65535 {
			used[site.ListenPort] = struct{}{}
		}
	}
	for port := internalHostOnlyPortStart; port <= 65535; port++ {
		if _, exists := used[port]; !exists {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no internal site port is available")
}

func isReservedDynamicRoute(requestPath string) bool {
	return requestPath == strings.TrimSuffix(dynamicRoutePrefix, "/") || strings.HasPrefix(requestPath, dynamicRoutePrefix)
}

func requestPublicHost(hostport string) string {
	hostport = strings.TrimSpace(hostport)
	if hostport == "" || strings.HasPrefix(hostport, "[") {
		return ""
	}
	host := hostport
	if parsedHost, _, err := net.SplitHostPort(hostport); err == nil {
		host = parsedHost
	} else if strings.Contains(hostport, ":") {
		return ""
	}
	normalized, err := normalizePublicHost(host)
	if err != nil {
		return ""
	}
	return normalized
}

func validateSiteSettings(name string, listenPort int, targetURL, playbackTargetURL, playbackMode string, streamHosts []string, uaMode, customUserAgent, customClient, customVersion string, quota int64, speedLimit int) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 || strings.ContainsAny(name, "\r\n") {
		return fmt.Errorf("name must be 1-100 characters without line breaks")
	}
	if listenPort < 1 || listenPort > 65535 {
		return fmt.Errorf("listen_port must be between 1 and 65535")
	}
	if len(targetURL) > maxTargetURLLength {
		return fmt.Errorf("target_url must not exceed %d bytes", maxTargetURLLength)
	}
	if _, err := normalizeTargetURL(targetURL); err != nil {
		return fmt.Errorf("invalid target_url: %w", err)
	}
	if strings.TrimSpace(playbackTargetURL) != "" {
		if len(playbackTargetURL) > maxTargetURLLength {
			return fmt.Errorf("playback_target_url must not exceed %d bytes", maxTargetURLLength)
		}
		if _, err := normalizeTargetURL(playbackTargetURL); err != nil {
			return fmt.Errorf("invalid playback_target_url: %w", err)
		}
	}
	if playbackMode != "direct" && playbackMode != "redirect" {
		return fmt.Errorf("playback_mode must be direct or redirect")
	}
	if _, _, _, _, err := normalizeUAConfig(uaMode, customUserAgent, customClient, customVersion); err != nil {
		return err
	}
	if quota < 0 || speedLimit < 0 {
		return fmt.Errorf("traffic_quota and speed_limit must not be negative")
	}
	if speedLimit > maxSpeedLimitMbps {
		return fmt.Errorf("speed_limit must not exceed %d Mbps", maxSpeedLimitMbps)
	}
	playbackAddressCount := len(streamHosts)
	if strings.TrimSpace(playbackTargetURL) != "" {
		playbackAddressCount++
	}
	if playbackAddressCount > maxPlaybackAddresses {
		return fmt.Errorf("playback addresses must contain at most %d entries", maxPlaybackAddresses)
	}
	for _, host := range streamHosts {
		if len(host) > maxTargetURLLength {
			return fmt.Errorf("stream host must not exceed %d bytes", maxTargetURLLength)
		}
		if _, err := normalizeTargetURL(host); err != nil {
			return fmt.Errorf("invalid stream host %q: %w", host, err)
		}
	}
	return nil
}

func normalizeMainVideoStreamMode(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return mainVideoStreamModeProxy, nil
	}
	if value != mainVideoStreamModeProxy && value != mainVideoStreamModeDirect {
		return "", fmt.Errorf("main_video_stream_mode must be proxy or direct")
	}
	return value, nil
}

func isMainVideoStreamURL(target *url.URL) bool {
	if target == nil {
		return false
	}
	pathValue := strings.ToLower(target.Path)
	for _, suffix := range []string{
		".m3u8", ".m3u", ".mpd", ".ts", ".m4s",
		".srt", ".ass", ".vtt", ".sub",
		".jpg", ".jpeg", ".gif", ".png", ".svg", ".ico", ".webp",
		".js", ".css", ".woff", ".woff2", ".ttf", ".otf", ".map", ".webmanifest",
	} {
		if strings.HasSuffix(pathValue, suffix) {
			return false
		}
	}
	for _, suffix := range []string{".mp4", ".m4v", ".ogv", ".webm", ".mkv", ".mov", ".avi", ".wmv", ".flv"} {
		if strings.HasSuffix(pathValue, suffix) {
			return true
		}
	}
	parts := strings.Split(strings.Trim(pathValue, "/"), "/")
	if len(parts) > 0 && parts[0] == "emby" {
		parts = parts[1:]
	}
	if len(parts) >= 3 && parts[0] == "videos" && parts[1] != "" {
		name := parts[2]
		if dot := strings.IndexByte(name, '.'); dot >= 0 {
			name = name[:dot]
		}
		switch name {
		case "stream", "original", "download", "file":
			return true
		}
	}
	if len(parts) >= 3 && parts[0] == "items" && parts[1] != "" && parts[2] == "download" {
		return true
	}
	for key, values := range target.Query() {
		if !strings.EqualFold(key, "Static") && !strings.EqualFold(key, "Download") {
			continue
		}
		for _, value := range values {
			if strings.EqualFold(value, "true") {
				return true
			}
		}
	}
	return false
}

func isMainVideoStreamRequest(r *http.Request) bool {
	return r != nil && r.URL != nil && (r.Method == http.MethodGet || r.Method == http.MethodHead) && !hasUpgradeIntent(r) && !isReservedDynamicRoute(r.URL.Path) && !isPlaybackInfoRequest(r.URL.Path) && isMainVideoStreamURL(r.URL)
}

func mainVideoDirectTarget(r *http.Request, upstream *url.URL) *url.URL {
	if r == nil || r.URL == nil || upstream == nil {
		return nil
	}
	target := *r.URL
	base := *upstream
	// Configured base queries can contain origin-only credentials. Direct mode
	// exposes only authentication that was already present in the client URL.
	base.RawQuery = ""
	base.ForceQuery = false
	applyUpstreamURL(&target, &base)
	target.User = nil
	target.Fragment = ""
	target.RawFragment = ""
	return &target
}

func writeMainVideoDirectRedirect(w http.ResponseWriter, target *url.URL) {
	if w == nil || target == nil {
		return
	}
	w.Header().Set("Location", target.String())
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusTemporaryRedirect)
}

func isPlaybackRequest(path string) bool {
	path = strings.ToLower(path)
	switch {
	case strings.HasPrefix(path, "/videos/"),
		strings.HasPrefix(path, "/emby/videos/"),
		strings.HasPrefix(path, "/audio/"),
		strings.HasPrefix(path, "/emby/audio/"),
		strings.HasPrefix(path, "/livetv/"),
		strings.HasPrefix(path, "/emby/livetv/"):
		return true
	case strings.HasPrefix(path, "/items/"),
		strings.HasPrefix(path, "/emby/items/"):
		return strings.Contains(path, "/download") || strings.Contains(path, "/file")
	default:
		return false
	}
}

func isPlaybackRedirectEndpoint(pathValue string) bool {
	pathValue = strings.ToLower(pathValue)
	return pathValue == "/emya/video" || pathValue == "/emby/emya/video"
}

func isPlaybackInfoRequest(path string) bool {
	parts := strings.Split(strings.ToLower(path), "/")
	if len(parts) == 4 {
		return parts[0] == "" && parts[1] == "items" && parts[2] != "" && parts[3] == "playbackinfo"
	}
	return len(parts) == 5 && parts[0] == "" && parts[1] == "emby" && parts[2] == "items" && parts[3] != "" && parts[4] == "playbackinfo"
}

func dynamicStructuredRequestSource(requestPath string) string {
	requestPath = strings.ToLower(requestPath)
	switch {
	case isPlaybackInfoRequest(requestPath):
		return dynamicDiscoverySourcePlaybackInfo
	case strings.HasSuffix(requestPath, ".m3u8"), strings.HasSuffix(requestPath, ".m3u"):
		return dynamicDiscoverySourceHLS
	case strings.HasSuffix(requestPath, ".mpd"):
		return dynamicDiscoverySourceDASH
	default:
		return ""
	}
}

func dynamicStructuredRequestIdentity(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	source := dynamicStructuredRequestSource(r.URL.Path)
	if !dynamicStructuredMethodAllowed(source, r.Method) {
		return ""
	}
	return source
}

func isDynamicRedirectEligibleRequest(r *http.Request) bool {
	if r == nil || r.URL == nil || r.Method != http.MethodGet && r.Method != http.MethodHead || hasUpgradeIntent(r) {
		return false
	}
	return isPlaybackRequest(r.URL.Path) || isPlaybackRedirectEndpoint(r.URL.Path) || isPlaybackInfoRequest(r.URL.Path) || dynamicStructuredRequestSource(r.URL.Path) != ""
}

func isDynamicRedirectEligibleRequestForState(r *http.Request, state *dynamicSiteState) bool {
	if isDynamicRedirectEligibleRequest(r) {
		return true
	}
	return r != nil && r.URL != nil && (r.Method == http.MethodGet || r.Method == http.MethodHead) && !hasUpgradeIntent(r) && state != nil && state.hasLearnedPlaybackPath(r.URL.Path, time.Now())
}

func isExtremeDynamicRedirectEligibleRequest(r *http.Request) bool {
	return r != nil && r.URL != nil && r.Method != http.MethodConnect && !hasUpgradeIntent(r) && !isReservedDynamicRoute(r.URL.Path)
}

func upstreamTargetForRequest(r *http.Request, apiTarget, playbackTarget *url.URL) *url.URL {
	if playbackTarget != nil && isPlaybackRequest(r.URL.Path) {
		return playbackTarget
	}
	return apiTarget
}

// resolvePlaybackConfiguration is the single interpretation of the persisted
// playback fields used by both runtime routing and diagnostics. The first
// stream_hosts entry becomes the effective playback target only when the
// dedicated playback_target_url is empty; every configured authority remains
// an allowed redirect destination.
func resolvePlaybackConfiguration(playbackTargetURL, streamHostsRaw string) (*url.URL, map[string]bool, error) {
	var playbackTarget *url.URL
	var err error
	if strings.TrimSpace(playbackTargetURL) != "" {
		playbackTarget, err = normalizeTargetURL(playbackTargetURL)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid playback target URL: %w", err)
		}
	}
	var extraHosts []string
	if strings.TrimSpace(streamHostsRaw) != "" {
		if err := json.Unmarshal([]byte(streamHostsRaw), &extraHosts); err != nil {
			return nil, nil, fmt.Errorf("invalid stream_hosts: %w", err)
		}
	}
	playbackHosts := make(map[string]bool, len(extraHosts)+1)
	if playbackTarget != nil {
		playbackHosts[redirectHostKey(playbackTarget)] = true
	}
	for _, raw := range extraHosts {
		parsed, err := normalizeTargetURL(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid stream host %q: %w", raw, err)
		}
		playbackHosts[redirectHostKey(parsed)] = true
		if playbackTarget == nil {
			playbackTarget = parsed
		}
	}
	return playbackTarget, playbackHosts, nil
}

// writeWebSocketGatewayError answers a hijacked client directly, since the
// http.ResponseWriter is no longer usable once the connection is taken over.
