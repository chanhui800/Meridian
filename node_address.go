package main

import (
	"net"
	"strings"
)

// A node's published address is a per-family pair rather than one string. The
// legacy Address column stays authoritative for the Agent's TLS dial and for the
// panel's single-address display, while AddressV4/AddressV6 let a dual-stack node
// publish one A record and one AAAA record for the same site.
//
// Resolution rules (stable and deliberately narrow):
//   - A primary address always comes from the legacy Address column when it
//     holds a literal IP of that family, so an existing database needs no
//     migration and an enrolment-inferred address keeps working untouched.
//   - A family column is used when it holds a literal IP of its own family.
//     A wrong-family or non-literal value is ignored rather than published,
//     because the value becomes a public DNS record.
const (
	nodeDNSPublishAuto = "auto"
	nodeDNSPublishV4   = "v4"
	nodeDNSPublishV6   = "v6"
)

// nodeAddressSourceDetected marks an address the Controller verified with its own
// health probe, after the Agent reported it from one of its interfaces. Like an
// enrolment-inferred address it is flagged for review in the panel, because it
// becomes a public DNS record.
const nodeAddressSourceDetected = "detected"

// nodeDNSPublishModes lists the accepted dns_publish values in display order.
func nodeDNSPublishModes() []string {
	return []string{nodeDNSPublishAuto, nodeDNSPublishV4, nodeDNSPublishV6}
}

func normalizeNodeDNSPublish(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case nodeDNSPublishV4, "ipv4", "a":
		return nodeDNSPublishV4
	case nodeDNSPublishV6, "ipv6", "aaaa":
		return nodeDNSPublishV6
	default:
		return nodeDNSPublishAuto
	}
}

// nodeAddressFamily returns "v4" or "v6" for a literal address, or "" when the
// value is not a literal IP. Only literals are accepted: the value is published
// as a DNS record, and a hostname would require resolution the Controller cannot
// verify.
func nodeAddressFamily(value string) string {
	ip := net.ParseIP(strings.TrimSpace(value))
	if ip == nil {
		return ""
	}
	if ip.To4() != nil {
		return "v4"
	}
	return "v6"
}

// familyAddress returns the literal address for one family, preferring the
// family column and falling back to the legacy primary address.
func (n ControlNode) familyAddress(family string) string {
	switch family {
	case "v4":
		if nodeAddressFamily(n.AddressV4) == "v4" {
			return strings.TrimSpace(n.AddressV4)
		}
		if nodeAddressFamily(n.Address) == "v4" {
			return strings.TrimSpace(n.Address)
		}
	case "v6":
		if nodeAddressFamily(n.AddressV6) == "v6" {
			return strings.TrimSpace(n.AddressV6)
		}
		if nodeAddressFamily(n.Address) == "v6" {
			return strings.TrimSpace(n.Address)
		}
	}
	return ""
}

func (n ControlNode) IPv4Address() string { return n.familyAddress("v4") }
func (n ControlNode) IPv6Address() string { return n.familyAddress("v6") }

// HasAddressFamily reports whether the node can be reached on that family, which
// is what decides whether a record of that family may be published at all.
func (n ControlNode) HasAddressFamily(family string) bool {
	return n.familyAddress(family) != ""
}

// PrimaryAddress is the address the Agent listener is dialed on and the value the
// panel shows as THE address. IPv4 wins when both exist so the legacy single
// column keeps a stable meaning for every existing caller.
func (n ControlNode) PrimaryAddress() string {
	if v4 := n.IPv4Address(); v4 != "" {
		return v4
	}
	return n.IPv6Address()
}

// PrimaryFamily names the family PrimaryAddress belongs to, or "" when the node
// has no usable address.
func (n ControlNode) PrimaryFamily() string {
	if n.IPv4Address() != "" {
		return "v4"
	}
	if n.IPv6Address() != "" {
		return "v6"
	}
	return ""
}

// DNSPublishFamilies returns the record families this node should publish, in a
// stable order. The node's own reachable families are the ceiling: an operator
// preference can never publish an address the node does not have.
//
//   - auto: every family the node has an address for, so a dual-stack node
//     publishes one A and one AAAA.
//   - v4 / v6: exactly that family, so a dual-stack node can be pinned to one.
func (n ControlNode) DNSPublishFamilies() []string {
	mode := normalizeNodeDNSPublish(n.DNSPublish)
	hasV4, hasV6 := n.HasAddressFamily("v4"), n.HasAddressFamily("v6")
	switch mode {
	case nodeDNSPublishV4:
		if hasV4 {
			return []string{"v4"}
		}
		return nil
	case nodeDNSPublishV6:
		if hasV6 {
			return []string{"v6"}
		}
		return nil
	default:
		families := make([]string, 0, 2)
		if hasV4 {
			families = append(families, "v4")
		}
		if hasV6 {
			families = append(families, "v6")
		}
		return families
	}
}

// DNSPublishModeLabel renders the configured preference for the panel and for
// diagnostics. It never prints an address.
func (n ControlNode) DNSPublishModeLabel() string {
	switch normalizeNodeDNSPublish(n.DNSPublish) {
	case nodeDNSPublishV4:
		return "仅 IPv4"
	case nodeDNSPublishV6:
		return "仅 IPv6"
	default:
		return "自动"
	}
}

// dnsRecordType maps a family to the Cloudflare record type it publishes.
func dnsRecordType(family string) string {
	if family == "v6" {
		return "AAAA"
	}
	return "A"
}

// dnsFamilyLabel names a family for operator-facing messages, so a failed probe
// says which record it refused to publish.
func dnsFamilyLabel(family string) string {
	if family == "v6" {
		return "IPv6"
	}
	return "IPv4"
}

// dnsFamilyForRecordType is the inverse of dnsRecordType, used when adopting or
// matching a record that already exists remotely.
func dnsFamilyForRecordType(recordType string) string {
	switch strings.ToUpper(strings.TrimSpace(recordType)) {
	case "AAAA":
		return "v6"
	case "A":
		return "v4"
	default:
		return ""
	}
}
