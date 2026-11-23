package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const pathTargetRoutePrefix = "/_proxy/v1/"

type pathTargetContextKey struct{}

func parsePathTargetRequest(requestURL *url.URL) (*url.URL, string, string, error) {
	if requestURL == nil {
		return nil, "", "", fmt.Errorf("invalid path target")
	}
	escapedPath := requestURL.EscapedPath()
	if target, upstreamPath, rawPath, matched, err := parseCanonicalPathTarget(escapedPath); matched {
		if err != nil {
			return nil, "", "", err
		}
		if nestedTarget, nestedPath, nestedRawPath, nested, nestedErr := parseNestedCanonicalPathTarget(upstreamPath, rawPath); nested {
			return nestedTarget, nestedPath, nestedRawPath, nestedErr
		}
		return target, upstreamPath, rawPath, err
	}

	var parsed *url.URL
	// Media clients commonly join their path field directly after the
	// gateway address, yielding /http://origin:port/base. Treat that
	// absolute URL path as the upstream base.
	if !strings.HasPrefix(requestURL.Path, "/") {
		return nil, "", "", fmt.Errorf("invalid path target")
	}
	var err error
	parsed, err = url.Parse(strings.TrimPrefix(escapedPath, "/"))
	if err != nil || !parsed.IsAbs() {
		parsed, err = url.Parse(strings.TrimPrefix(requestURL.Path, "/"))
	}
	if err != nil {
		return nil, "", "", fmt.Errorf("invalid path target URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if !parsed.IsAbs() || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, "", "", fmt.Errorf("path target must be an absolute HTTP(S) URL")
	}
	// Some media clients append a rewritten root-relative path to their
	// configured Emby base path. When that path contains the canonical route,
	// route it to its declared target instead of nesting it beneath the frontend.
	if target, upstreamPath, rawPath, matched, err := parseNestedCanonicalPathTarget(parsed.Path, parsed.RawPath); matched {
		return target, upstreamPath, rawPath, err
	}
	if parsed.Host == "" || parsed.Hostname() == "" || parsed.Opaque != "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return nil, "", "", fmt.Errorf("invalid path target authority")
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return nil, "", "", fmt.Errorf("path target contains an invalid port")
	}
	if port := parsed.Port(); port != "" {
		value, portErr := strconv.Atoi(port)
		if portErr != nil || value < 1 || value > 65535 {
			return nil, "", "", fmt.Errorf("path target contains an invalid port")
		}
	}
	target := &url.URL{Scheme: parsed.Scheme, Host: parsed.Host}
	return target, parsed.Path, parsed.RawPath, nil
}

func parseNestedCanonicalPathTarget(path, rawPath string) (*url.URL, string, string, bool, error) {
	escapedPath := (&url.URL{Path: path, RawPath: rawPath}).EscapedPath()
	canonicalRoute := strings.Index(escapedPath, pathTargetRoutePrefix)
	if canonicalRoute < 0 {
		return nil, "", "", false, nil
	}
	return parseCanonicalPathTarget(escapedPath[canonicalRoute:])
}

func parseCanonicalPathTarget(escapedPath string) (*url.URL, string, string, bool, error) {
	remaining := strings.TrimPrefix(escapedPath, pathTargetRoutePrefix)
	if remaining == escapedPath {
		return nil, "", "", false, nil
	}
	scheme, remaining, ok := strings.Cut(remaining, "/")
	if !ok {
		return nil, "", "", true, fmt.Errorf("path target must include a scheme and authority")
	}
	scheme = strings.ToLower(scheme)
	if scheme != "http" && scheme != "https" {
		return nil, "", "", true, fmt.Errorf("path target supports only http and https")
	}
	authority, escapedUpstreamPath, hasPath := strings.Cut(remaining, "/")
	if authority == "" {
		return nil, "", "", true, fmt.Errorf("path target authority is required")
	}
	pathValue := ""
	if hasPath {
		pathValue = "/" + escapedUpstreamPath
	}
	parsed, err := url.Parse(scheme + "://" + authority + pathValue)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" || parsed.Opaque != "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return nil, "", "", true, fmt.Errorf("invalid path target authority")
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return nil, "", "", true, fmt.Errorf("path target contains an invalid port")
	}
	if port := parsed.Port(); port != "" {
		value, portErr := strconv.Atoi(port)
		if portErr != nil || value < 1 || value > 65535 {
			return nil, "", "", true, fmt.Errorf("path target contains an invalid port")
		}
	}
	return &url.URL{Scheme: scheme, Host: parsed.Host}, parsed.Path, parsed.RawPath, true, nil
}

func pathTargetRouteURL(target *url.URL) string {
	if target == nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
		return ""
	}
	path := pathTargetRoutePrefix + target.Scheme + "/" + target.Host + target.EscapedPath()
	if target.RawQuery != "" {
		path += "?" + target.RawQuery
	} else if target.ForceQuery {
		path += "?"
	}
	return path
}

func rewritePathTargetResponse(resp *http.Response) {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return
	}
	upstream := resp.Request.URL
	if upstream.Scheme != "http" && upstream.Scheme != "https" || upstream.Host == "" {
		return
	}
	rewritePathTargetSetCookies(resp.Header, upstream)
	location := resp.Header.Get("Location")
	if location == "" {
		return
	}
	reference, err := url.Parse(location)
	if err != nil {
		return
	}
	resolved := upstream.ResolveReference(reference)
	if resolved.Scheme != "http" && resolved.Scheme != "https" || resolved.Host == "" || resolved.User != nil {
		return
	}
	if route := pathTargetRouteURL(resolved); route != "" {
		resp.Header.Set("Location", route)
	}
}

func rewritePathTargetSetCookies(header http.Header, upstream *url.URL) {
	if header == nil || upstream == nil {
		return
	}
	values := header.Values("Set-Cookie")
	if len(values) == 0 {
		return
	}
	base := pathTargetRoutePrefix + upstream.Scheme + "/" + upstream.Host
	header.Del("Set-Cookie")
	for _, value := range values {
		cookie, err := http.ParseSetCookie(value)
		if err != nil {
			continue
		}
		cookie.Domain = ""
		cookiePath := cookie.Path
		if cookiePath == "" {
			cookiePath = "/"
		} else if !strings.HasPrefix(cookiePath, "/") {
			cookiePath = "/" + cookiePath
		}
		cookie.Path = base + cookiePath
		header.Add("Set-Cookie", cookie.String())
	}
}
