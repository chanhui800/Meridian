package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"sort"
	"strings"
	"time"
)

// The Controller cannot discover a node's own addresses: it only ever sees the
// peer address of one TCP connection, which is a single family and is the
// NAT-visible address rather than the node's. So the Agent reports what it has on
// its interfaces, the Controller stores that as a hint, and an address only
// becomes a published DNS record after the Controller has health-probed it.
//
// That ordering matters: the value ends up in a public A/AAAA record, so a wrong
// guess points the site at a host that does not answer.

// nodeNetAddressHintsPerFamily bounds the hints kept per family. More than one
// usable address per family cannot be published anyway (a hostname carries one A
// and one AAAA), and the bound keeps the stored value small and stable.
const nodeNetAddressHintsPerFamily = 4

// nodeNetAddressHintFreshness bounds how long an Agent-reported hint is worth
// probing. An Agent refreshes its hints with every report, so a stale hint means
// the node stopped reporting and its addresses should not be adopted on the
// strength of an old observation.
const nodeNetAddressHintFreshness = 10 * time.Minute

// nodeNetAddressAdoptionInterval bounds how often a node's candidates are
// probed. A candidate that does not answer keeps not answering, so probing it on
// every fifteen-second scheduler tick would be pure network churn.
const nodeNetAddressAdoptionInterval = 5 * time.Minute

// markNodeAddressAdoptionAttempt records that adoption was attempted, so a
// failing probe is retried on an interval rather than on every tick.
func (d *DB) markNodeAddressAdoptionAttempt(nodeID int64, now time.Time) error {
	if d == nil || nodeID <= 0 {
		return nil
	}
	_, err := d.db.Exec("UPDATE control_nodes SET net_address_adoption_at_ms=? WHERE id=?", now.UnixMilli(), nodeID)
	return err
}

// encodeNodeNetAddressHints normalizes Agent-reported addresses into a bounded,
// deterministic JSON document. Non-literal, private and link-local values are
// dropped here rather than at adoption time so the stored hint can never be
// mistaken for a publishable address.
func encodeNodeNetAddressHints(addresses []NodeNetAddress) string {
	byFamily := map[string][]string{"v4": {}, "v6": {}}
	seen := map[string]bool{}
	for _, candidate := range addresses {
		address := strings.TrimSpace(candidate.Address)
		parsed := net.ParseIP(address)
		if parsed == nil || !nodeAddressIsGlobalUnicast(parsed) {
			continue
		}
		family := nodeAddressFamily(address)
		if family == "" {
			continue
		}
		normalized := parsed.String()
		if seen[normalized] {
			continue
		}
		seen[normalized] = true
		if len(byFamily[family]) >= nodeNetAddressHintsPerFamily {
			continue
		}
		byFamily[family] = append(byFamily[family], normalized)
	}
	for family := range byFamily {
		sort.Strings(byFamily[family])
	}
	if len(byFamily["v4"]) == 0 && len(byFamily["v6"]) == 0 {
		return ""
	}
	document, err := json.Marshal(nodeNetAddressHintDocument{V4: byFamily["v4"], V6: byFamily["v6"]})
	if err != nil {
		return ""
	}
	return string(document)
}

type nodeNetAddressHintDocument struct {
	V4 []string `json:"v4,omitempty"`
	V6 []string `json:"v6,omitempty"`
}

// decodeNodeNetAddressHints parses a stored hint document. A corrupt or partial
// value yields no candidates, so a damaged row can never publish an address.
func decodeNodeNetAddressHints(value string) []NodeNetAddress {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	var document nodeNetAddressHintDocument
	if json.Unmarshal([]byte(trimmed), &document) != nil {
		return nil
	}
	hints := make([]NodeNetAddress, 0, len(document.V4)+len(document.V6))
	for _, entry := range []struct {
		family   string
		contents []string
	}{{"v4", document.V4}, {"v6", document.V6}} {
		for _, address := range entry.contents {
			parsed := net.ParseIP(strings.TrimSpace(address))
			if parsed == nil || !nodeAddressIsGlobalUnicast(parsed) {
				continue
			}
			normalized := parsed.String()
			if nodeAddressFamily(normalized) != entry.family {
				continue
			}
			hints = append(hints, NodeNetAddress{Family: entry.family, Address: normalized})
		}
	}
	if len(hints) == 0 {
		return nil
	}
	return hints
}

// nodeEdgeProbeHost returns the per-node edge hostname, whose certificate the
// node serves as soon as it is enrolled. It is the probe identity for a node that
// has no scheduled site yet, and it needs no DNS record: the probe dials the
// literal address and uses this name only for SNI and certificate verification.
func (a *App) nodeEdgeProbeHost(node ControlNode) (string, error) {
	if strings.TrimSpace(node.GUID) == "" {
		return "", nil
	}
	settings, err := a.db.PanelSettings()
	if err != nil {
		return "", err
	}
	routeDomain := strings.TrimSpace(settings.RouteDomain)
	if routeDomain == "" {
		// Without a configured route domain there is no per-node certificate, so
		// adoption has to wait for a scheduled site.
		return "", nil
	}
	return edgeCertificateHost(routeDomain, node.GUID), nil
}

// adoptProbedNodeAddressesForScheduler walks every enrolled node that reported
// address hints and tries to adopt a verified one. A per-node failure is logged
// by the caller and never stops the other nodes.
func (a *App) adoptProbedNodeAddressesForScheduler(ctx context.Context, now time.Time) error {
	nodes, err := a.db.listControlNodes(now)
	if err != nil {
		return err
	}
	var firstErr error
	for _, node := range nodes {
		if node.EnrolledAtMS <= 0 || !node.Enabled {
			continue
		}
		if len(decodeNodeNetAddressHints(node.NetAddressHints)) == 0 {
			continue
		}
		if _, err := a.adoptProbedNodeAddresses(ctx, node, now); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// adoptProbedNodeAddresses fills empty per-family slots from the Agent's own
// reported addresses, but only for addresses this Controller has just reached
// with a real health probe. It never overwrites an operator value, never turns an
// auto-detected slot into a manual one, and reports the addresses it adopted so
// the panel can ask the operator to review them.
//
// It is deliberately additive: publishing an address is what makes a site
// resolvable, so a detected address that fails its probe is simply not adopted
// and the node keeps whatever it had.
func (a *App) adoptProbedNodeAddresses(ctx context.Context, node ControlNode, now time.Time) (ControlNode, error) {
	hints := decodeNodeNetAddressHints(node.NetAddressHints)
	if len(hints) == 0 {
		return node, nil
	}
	// Only probe when the hint is actually fresh. The Agent refreshes its hints
	// with every report, so a stale hint means the node stopped reporting and its
	// addresses should not be adopted on the strength of an old observation.
	if node.NetAddressHintsAtMS <= 0 || now.Sub(time.UnixMilli(node.NetAddressHintsAtMS)) > nodeNetAddressHintFreshness {
		return node, nil
	}
	// Adoption costs a TCP+TLS probe per candidate, and a candidate that fails
	// keeps failing, so the attempt clock is separate from the hint clock and
	// bounded to one attempt per interval.
	if node.NetAddressAdoptionAttemptAtMS > 0 && now.Sub(time.UnixMilli(node.NetAddressAdoptionAttemptAtMS)) < nodeNetAddressAdoptionInterval {
		return node, nil
	}
	// The probe needs a hostname whose certificate this node serves. A scheduled
	// site's public host works, but requiring one created a chicken-and-egg
	// problem: a freshly enrolled dual-stack node has no site yet, so the Agent's
	// reported IPv4 could never be adopted and the node published only the family
	// it happened to enrol over. Every node gets its own edge certificate as soon
	// as it is enrolled, and that certificate carries the per-node edge host, so
	// that host is the fallback and adoption works with nothing scheduled.
	host, ok, err := a.scheduledPublicHostForNode(node.ID)
	if err != nil {
		return node, err
	}
	if !ok {
		host, err = a.nodeEdgeProbeHost(node)
		if err != nil {
			return node, err
		}
	}
	if host == "" {
		// Nothing can vouch for a candidate address yet, so no probe is attempted
		// and the attempt clock is deliberately NOT consumed: once a site is
		// scheduled (or a route domain is configured) adoption should run at once
		// rather than wait out an interval for a probe that never happened.
		return node, nil
	}
	if err := a.db.markNodeAddressAdoptionAttempt(node.ID, now); err != nil {
		return node, err
	}
	probeSecretText, err := dNodeProbeSecret(a.db, node)
	if err != nil {
		return node, err
	}
	probeSecret, err := decodeNodeProbeSecret(probeSecretText)
	if err != nil {
		return node, err
	}
	updateV4, updateV6 := "", ""
	sourceV6 := node.AddressV6Source
	for _, hint := range hints {
		if node.familyAddress(hint.Family) != "" {
			continue
		}
		probeNode, probeErr := a.db.controlNodeByID(node.ID, now)
		if probeErr != nil {
			return node, probeErr
		}
		if probeErr = probeNodeAddress(ctx, probeNode, host, probeSecret, nil, hint.Address); probeErr != nil {
			continue
		}
		switch hint.Family {
		case "v4":
			if updateV4 == "" {
				updateV4 = hint.Address
			}
		case "v6":
			if updateV6 == "" {
				updateV6 = hint.Address
				sourceV6 = nodeAddressSourceDetected
			}
		}
	}
	if updateV4 == "" && updateV6 == "" {
		return node, nil
	}
	if err := a.db.adoptNodeAddresses(node.ID, updateV4, updateV6, sourceV6, now); err != nil {
		return node, err
	}
	return a.db.controlNodeByID(node.ID, now)
}

// nodeAddressAdoption is the pure result of merging verified addresses into a
// node's slots. It is separate from the write so the "never overwrite an
// operator value" rule can be tested without a live probe.
type nodeAddressAdoption struct {
	address      string
	addressV4    string
	addressV6    string
	addressV6Src string
	source       string
	changed      bool
}

// mergeAdoptedNodeAddresses fills only the empty per-family slots. An operator
// value is never replaced, and the legacy primary column keeps its meaning as
// the address the panel shows and the node is dialed on: it is filled when it is
// still empty, and it moves to IPv4 in the one case where the Controller filled
// it itself from the connecting source address.
func mergeAdoptedNodeAddresses(node ControlNode, addressV4, addressV6, sourceV6 string) nodeAddressAdoption {
	result := nodeAddressAdoption{
		address:      node.Address,
		addressV4:    node.AddressV4,
		addressV6:    node.AddressV6,
		addressV6Src: node.AddressV6Source,
	}
	if addressV4 != "" && strings.TrimSpace(node.AddressV4) == "" {
		result.addressV4 = addressV4
	}
	if addressV6 != "" && strings.TrimSpace(node.AddressV6) == "" {
		result.addressV6 = addressV6
		result.addressV6Src = sourceV6
	}
	switch {
	case strings.TrimSpace(node.Address) == "":
		switch {
		case nodeAddressFamily(node.AddressV4) != "":
			// The node already dials on IPv4; keep that family as the primary.
			result.address, result.source = node.AddressV4, nodeAddressSourceDetected
		case nodeAddressFamily(node.AddressV6) != "":
			result.address, result.source = node.AddressV6, nodeAddressSourceDetected
		case nodeAddressFamily(result.addressV4) != "":
			// Nothing was configured at all, so pick IPv4 first to match
			// PrimaryAddress()'s preference for the panel's single-address view.
			result.address, result.source = result.addressV4, nodeAddressSourceDetected
		case nodeAddressFamily(result.addressV6) != "":
			result.address, result.source = result.addressV6, nodeAddressSourceDetected
		}
	default:
		// A dual-stack node is dialed and shown on IPv4, matching
		// PrimaryAddress(). This only ever moves an address the Controller
		// inferred for itself: an operator-entered value keeps its family, and a
		// node pinned to IPv6 keeps its primary until the other family is free.
		if result.addressV4 != "" &&
			nodeAddressFamily(node.Address) == "v6" &&
			nodeAddressSourceInferred(node.AddressSource) &&
			normalizeNodeDNSPublish(node.DNSPublish) != nodeDNSPublishV6 {
			result.address, result.source = result.addressV4, nodeAddressSourceDetected
		}
	}
	result.changed = result.address != node.Address || result.addressV4 != node.AddressV4 || result.addressV6 != node.AddressV6
	return result
}

// scheduledPublicHostForNode returns the public host of a site currently
// scheduled on this node. The probe prefers a hostname whose certificate the node
// serves; a node with no scheduled site falls back to its own edge host.
func (a *App) scheduledPublicHostForNode(nodeID int64) (string, bool, error) {
	var host string
	// public_host lives on sites, not on site_node_schedules: the schedule table
	// only records which node the site is assigned to.
	err := a.db.db.QueryRow(`SELECT s.public_host FROM site_node_schedules n
		JOIN sites s ON s.id = n.site_id
		WHERE n.applied_node_id=? AND n.enabled=1 AND TRIM(COALESCE(s.public_host,''))<>''
		ORDER BY n.site_id LIMIT 1`, nodeID).Scan(&host)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimSpace(host), true, nil
}

// adoptNodeAddresses writes the verified addresses into their per-family slots.
// The legacy primary column is only filled when it is still empty, so adopting a
// detected IPv4 address can never displace an operator-chosen one, and the
// family the node dials keeps its existing meaning.
func (d *DB) adoptNodeAddresses(nodeID int64, addressV4, addressV6, sourceV6 string, now time.Time) error {
	if d == nil || nodeID <= 0 || (addressV4 == "" && addressV6 == "") {
		return nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentAddress, currentV4, currentV6, currentSource, currentV6Source, currentPublish string
	if err := tx.QueryRow("SELECT address,address_source,address_v4,address_v6,address_v6_source,dns_publish FROM control_nodes WHERE id=?", nodeID).
		Scan(&currentAddress, &currentSource, &currentV4, &currentV6, &currentV6Source, &currentPublish); err != nil {
		return err
	}
	current := ControlNode{
		Address: currentAddress, AddressSource: currentSource,
		AddressV4: currentV4, AddressV6: currentV6, AddressV6Source: currentV6Source,
		// The publish pin decides whether an adopted IPv4 may take over the
		// primary address, so it has to be read inside the same transaction
		// rather than assumed.
		DNSPublish: currentPublish,
	}
	result := mergeAdoptedNodeAddresses(current, addressV4, addressV6, sourceV6)
	if !result.changed {
		return nil
	}
	nextSource := result.source
	if nextSource == "" {
		nextSource = currentSource
	}
	// A detected address can change the published record set, so bump every
	// schedule on this node exactly like an operator edit does.
	if _, err := tx.Exec(`UPDATE site_node_schedules SET schedule_revision=schedule_revision+1,updated_at_ms=?
		WHERE desired_node_id=?`, now.UnixMilli(), nodeID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE control_nodes SET address=?,address_source=?,address_v4=?,address_v6=?,address_v6_source=?,updated_at_ms=? WHERE id=?`,
		result.address, nextSource, result.addressV4, result.addressV6, result.addressV6Src, now.UnixMilli(), nodeID); err != nil {
		return err
	}
	return tx.Commit()
}
