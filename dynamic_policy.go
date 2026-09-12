package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// The automatic proxy runs one fixed dynamic-discovery policy for every site:
// the historical "compatible" profile with all discovery sources enabled and
// HTTPS downgrades permitted. Per-site policy settings were accepted by older
// releases but never executed by the runtime; they have been removed. The
// remaining machinery below is the policy itself plus the route-key handling
// that gates the feature process-wide.
const (
	maxUpstreamHeaders                 = 16
	maxUpstreamHeaderName              = 64
	maxUpstreamHeaderValue             = 1024
	maxPlaybackAddresses               = 128
	maxTargetURLLength                 = 2048
	ingressModePort                    = "port"
	ingressModeHost                    = "host"
	ingressModeBoth                    = "both"
	ingressModePath                    = "path"
	ingressModeUnset                   = "unset"
	mainVideoStreamModeProxy           = "proxy"
	mainVideoStreamModeDirect          = "direct"
	dynamicRoutePrefix                 = "/_meridian/d/"
	dynamicDiscoverySourceRedirect     = "redirect"
	dynamicDiscoverySourcePlaybackInfo = "playback_info"
	dynamicDiscoverySourceHLS          = "hls"
	dynamicDiscoverySourceDASH         = "dash"
	dynamicCapabilityKindResource      = "resource"
	dynamicCapabilityKindManifest      = "manifest"
	maxDynamicManifestDepth            = 3
)

const (
	// dynamicProfileCompatible is the only profile the runtime assigns. The
	// "safe" and "extreme" profiles are gone: per-site policy was removed in
	// v1.9.86 and the code that branched on them was removed in v1.9.89, so a
	// policy comparison here would always compare equal to this value.
	dynamicProfileCompatible    = "compatible"
	dynamicCapabilityVersion    = 1
	maxDynamicCapabilityBytes   = 16384
	dynamicCapabilityAAD        = "meridian-dynamic-capability-v1"
	maxRequiredHeaderClaims     = 8
	maxRequiredHeaderClaimBytes = 4 << 10

	maxDynamicTargetURLBytes  = 4096
	maxDynamicResolvedIPCount = 64

	globalDynamicMaxAuthorities               = 16384
	globalDynamicMaxActiveCapabilities        = 131072
	globalDynamicMaxStreams                   = 1024
	globalDynamicMaxNewAuthoritiesMinute      = 2400
	globalDynamicMaxDNSWorkers                = 32
	globalDynamicMaxConcurrentParses          = 8
	globalDynamicMaxSiteConcurrentParses      = 2
	globalDynamicMaxParseMemoryBytes          = 256 << 20
	globalDynamicMaxSiteParseMemoryBytes      = 64 << 20
	globalDynamicMaxCapabilityMemoryBytes     = 256 << 20
	globalDynamicMaxSiteCapabilityMemoryBytes = 64 << 20
	dynamicCapabilityPruneInterval            = 30 * time.Second
	globalDynamicMaxParseDepth                = 64
	globalDynamicMaxStringBytes               = 1 << 20
	globalDynamicMaxStructuredInputBytes      = 8 << 20
	globalDynamicMaxStructuredOutputBytes     = 16 << 20
	globalDynamicMaxJSONTokens                = 32768
	maxDynamicCompressionRatio                = 100
	globalDynamicMaxXMLTokens                 = 100000
	globalDynamicMaxXMLNodes                  = 50000
	globalDynamicMaxXMLAttributes             = 50000
	globalDynamicMaxXMLAttributesPerElement   = 50000
	globalDynamicMaxHLSAttributesPerTag       = 256
	minDynamicCompressionRatioBytes           = 1 << 20
)

type DynamicProfileLimits struct {
	AllowedSchemes             []string `json:"allowed_schemes"`
	AllowedPorts               []int    `json:"allowed_ports"`
	AllowAnyPort               bool     `json:"allow_any_port"`
	MaxRedirects               int      `json:"max_redirects"`
	MaxAuthorities             int      `json:"max_authorities"`
	MaxActiveCapabilities      int      `json:"max_active_capabilities"`
	MaxURLsPerResponse         int      `json:"max_urls_per_response"`
	MaxBodyBytes               int64    `json:"max_body_bytes"`
	MaxDNSIPs                  int      `json:"max_dns_ips"`
	MaxNewAuthoritiesPerMinute int      `json:"max_new_authorities_per_minute"`
	MaxStreams                 int      `json:"max_streams"`
	IdleExpirySeconds          int64    `json:"idle_expiry_seconds"`
	AbsoluteLifetimeSeconds    int64    `json:"absolute_lifetime_seconds"`
}

type DynamicGlobalLimits struct {
	MaxAuthorities               int   `json:"max_authorities"`
	MaxActiveCapabilities        int   `json:"max_active_capabilities"`
	MaxStreams                   int   `json:"max_streams"`
	MaxNewAuthoritiesPerMinute   int   `json:"max_new_authorities_per_minute"`
	MaxDNSWorkers                int   `json:"max_dns_workers"`
	MaxConcurrentParses          int   `json:"max_concurrent_parses"`
	MaxSiteConcurrentParses      int   `json:"max_site_concurrent_parses"`
	MaxParseMemoryBytes          int64 `json:"max_parse_memory_bytes"`
	MaxSiteParseMemoryBytes      int64 `json:"max_site_parse_memory_bytes"`
	MaxCapabilityMemoryBytes     int64 `json:"max_capability_memory_bytes"`
	MaxSiteCapabilityMemoryBytes int64 `json:"max_site_capability_memory_bytes"`
	MaxParseDepth                int   `json:"max_parse_depth"`
	MaxStringBytes               int64 `json:"max_string_bytes"`
	MaxTargetURLBytes            int   `json:"max_target_url_bytes"`
}

// dynamicDefaultProfileLimits is the single runtime policy shared by every
// site: http/https, any port, all discovery sources, HTTPS downgrade allowed.
func dynamicDefaultProfileLimits() DynamicProfileLimits {
	return DynamicProfileLimits{
		AllowedSchemes:             []string{"http", "https"},
		AllowedPorts:               []int{},
		AllowAnyPort:               true,
		MaxRedirects:               5,
		MaxAuthorities:             1024,
		MaxActiveCapabilities:      16384,
		MaxURLsPerResponse:         1024,
		MaxBodyBytes:               16 << 20,
		MaxDNSIPs:                  32,
		MaxNewAuthoritiesPerMinute: 300,
		MaxStreams:                 128,
		IdleExpirySeconds:          int64((2 * time.Hour) / time.Second),
		AbsoluteLifetimeSeconds:    int64((24 * time.Hour) / time.Second),
	}
}

func dynamicGlobalLimits() DynamicGlobalLimits {
	return DynamicGlobalLimits{
		MaxAuthorities:               globalDynamicMaxAuthorities,
		MaxActiveCapabilities:        globalDynamicMaxActiveCapabilities,
		MaxStreams:                   globalDynamicMaxStreams,
		MaxNewAuthoritiesPerMinute:   globalDynamicMaxNewAuthoritiesMinute,
		MaxDNSWorkers:                globalDynamicMaxDNSWorkers,
		MaxConcurrentParses:          globalDynamicMaxConcurrentParses,
		MaxSiteConcurrentParses:      globalDynamicMaxSiteConcurrentParses,
		MaxParseMemoryBytes:          globalDynamicMaxParseMemoryBytes,
		MaxSiteParseMemoryBytes:      globalDynamicMaxSiteParseMemoryBytes,
		MaxCapabilityMemoryBytes:     globalDynamicMaxCapabilityMemoryBytes,
		MaxSiteCapabilityMemoryBytes: globalDynamicMaxSiteCapabilityMemoryBytes,
		MaxParseDepth:                globalDynamicMaxParseDepth,
		MaxStringBytes:               globalDynamicMaxStringBytes,
		MaxTargetURLBytes:            maxDynamicTargetURLBytes,
	}
}

func resolveDynamicRouteKey(value string) ([]byte, error) {
	if value == "" {
		return nil, nil
	}
	if strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return nil, fmt.Errorf("DYNAMIC_ROUTE_KEY must not contain whitespace")
	}
	if len(value) < 32 {
		return nil, fmt.Errorf("DYNAMIC_ROUTE_KEY must be at least 32 bytes")
	}
	sum := sha256.Sum256([]byte(value))
	key := make([]byte, len(sum))
	copy(key, sum[:])
	return key, nil
}

func validateDynamicRouteKeySeparation(dynamicKey, effectiveJWTSecret, effectiveUpstreamHeaderKey []byte) error {
	if len(dynamicKey) == 0 {
		return nil
	}
	if len(dynamicKey) != sha256.Size || len(effectiveJWTSecret) == 0 {
		return fmt.Errorf("resolved DYNAMIC_ROUTE_KEY and JWT_SECRET are required for key separation")
	}
	jwtDigest := sha256.Sum256(effectiveJWTSecret)
	if subtle.ConstantTimeCompare(dynamicKey, jwtDigest[:]) == 1 {
		return fmt.Errorf("DYNAMIC_ROUTE_KEY must differ from JWT_SECRET")
	}
	if len(effectiveUpstreamHeaderKey) > 0 {
		if len(effectiveUpstreamHeaderKey) != sha256.Size {
			return fmt.Errorf("resolved UPSTREAM_HEADER_KEY has an invalid length")
		}
		if subtle.ConstantTimeCompare(dynamicKey, effectiveUpstreamHeaderKey) == 1 {
			return fmt.Errorf("DYNAMIC_ROUTE_KEY must differ from UPSTREAM_HEADER_KEY")
		}
	}
	return nil
}

func allDynamicDiscoverySources() []string {
	return []string{
		dynamicDiscoverySourceRedirect,
		dynamicDiscoverySourcePlaybackInfo,
		dynamicDiscoverySourceHLS,
		dynamicDiscoverySourceDASH,
	}
}

func dynamicDiscoverySourceEnabled(sources []string, source string) bool {
	for _, candidate := range sources {
		if candidate == source {
			return true
		}
	}
	return false
}
