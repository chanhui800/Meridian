package main

import (
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	dynamicObservationSourceRedirect     = dynamicDiscoverySourceRedirect
	dynamicObservationSourcePlaybackInfo = dynamicDiscoverySourcePlaybackInfo
	dynamicObservationSourceHLS          = dynamicDiscoverySourceHLS
	dynamicObservationSourceDASH         = dynamicDiscoverySourceDASH

	dynamicObservationDecisionAllowed = "allowed"
	dynamicObservationDecisionDenied  = "denied"

	dynamicObservationReasonRedirectAllowed      = "redirect_allowed"
	dynamicObservationReasonInvalidLocation      = "invalid_location"
	dynamicObservationReasonUnsupportedStatus    = "unsupported_status"
	dynamicObservationReasonRedirectLoop         = "redirect_loop"
	dynamicObservationReasonHopLimit             = "hop_limit"
	dynamicObservationReasonSchemeDenied         = "scheme_denied"
	dynamicObservationReasonPortDenied           = "port_denied"
	dynamicObservationReasonDomainDenied         = "domain_denied"
	dynamicObservationReasonHTTPSDowngradeDenied = "https_downgrade_denied"
	dynamicObservationReasonSelfTarget           = "self_target"
	dynamicObservationReasonDNSFailure           = "dns_failure"
	dynamicObservationReasonAddressDenied        = "address_denied"
	dynamicObservationReasonDialFailure          = "dial_failure"
	dynamicObservationReasonTLSFailure           = "tls_failure"
	dynamicObservationReasonCapacityLimit        = "capacity_limit"
	dynamicObservationReasonRateLimit            = "rate_limit"
	dynamicObservationReasonResponseFailure      = "response_failure"
	dynamicObservationReasonRuntimeUnavailable   = "runtime_unavailable"

	dynamicObservationQueueCapacity                  = 2048
	dynamicObservationBatchSize                      = 128
	dynamicObservationGlobalRowLimit                 = 10000
	dynamicObservationRetention                      = 30 * 24 * time.Hour
	dynamicObservationMaintenanceInterval            = time.Hour
	dynamicObservationReasonCandidateAllowed         = "candidate_allowed"
	dynamicObservationReasonParseFailure             = "parse_failure"
	dynamicObservationReasonCapabilityInvalid        = "capability_invalid"
	dynamicObservationReasonCapabilityExpired        = "capability_expired"
	dynamicObservationReasonRequestUnclassified      = "request_unclassified"
	dynamicObservationReasonStructuredBodyLimit      = "structured_body_limit"
	dynamicObservationReasonPlaybackInfoDenied       = "playback_info_denied"
	dynamicObservationReasonHLSFeatureDenied         = "hls_feature_denied"
	dynamicObservationReasonDASHFeatureDenied        = "dash_feature_denied"
	dynamicObservationReasonRedirectBodyReplayDenied = "redirect_body_replay_denied"
	dynamicObservationMaxAuthorityBytes              = 512
)

// dynamicObservationEvent is the complete hot-path observation contract. The
// database owns timestamps and aggregation so callers cannot inject chronology
// or counts into the operator-facing record.
type dynamicObservationEvent struct {
	SiteID             int64
	CanonicalAuthority string
	Source             string
	Decision           string
	ReasonCode         string
}

// DynamicObservation is deliberately identical to the frozen database/API
// shape. It never carries a redirect path, query, address, header, or body.
type DynamicObservation struct {
	SiteID             int64  `json:"site_id"`
	CanonicalAuthority string `json:"canonical_authority"`
	Source             string `json:"source"`
	Decision           string `json:"decision"`
	ReasonCode         string `json:"reason_code"`
	FirstSeenMS        int64  `json:"first_seen_ms"`
	LastSeenMS         int64  `json:"last_seen_ms"`
	Count              int64  `json:"count"`
}

type DynamicObservationsResponse struct {
	Observations        []DynamicObservation `json:"observations"`
	DroppedObservations uint64               `json:"dropped_observations"`
}

func validDynamicObservationEnums(source, decision, reasonCode string) bool {
	switch source {
	case dynamicObservationSourceRedirect,
		dynamicObservationSourcePlaybackInfo,
		dynamicObservationSourceHLS,
		dynamicObservationSourceDASH:
	default:
		return false
	}
	switch decision {
	case dynamicObservationDecisionAllowed:
		return reasonCode == dynamicObservationReasonRedirectAllowed || reasonCode == dynamicObservationReasonCandidateAllowed
	case dynamicObservationDecisionDenied:
		switch reasonCode {
		case dynamicObservationReasonInvalidLocation,
			dynamicObservationReasonUnsupportedStatus,
			dynamicObservationReasonRedirectLoop,
			dynamicObservationReasonHopLimit,
			dynamicObservationReasonSchemeDenied,
			dynamicObservationReasonPortDenied,
			dynamicObservationReasonDomainDenied,
			dynamicObservationReasonHTTPSDowngradeDenied,
			dynamicObservationReasonSelfTarget,
			dynamicObservationReasonDNSFailure,
			dynamicObservationReasonAddressDenied,
			dynamicObservationReasonDialFailure,
			dynamicObservationReasonTLSFailure,
			dynamicObservationReasonCapacityLimit,
			dynamicObservationReasonRateLimit,
			dynamicObservationReasonParseFailure,
			dynamicObservationReasonRequestUnclassified,
			dynamicObservationReasonStructuredBodyLimit,
			dynamicObservationReasonPlaybackInfoDenied,
			dynamicObservationReasonHLSFeatureDenied,
			dynamicObservationReasonDASHFeatureDenied,
			dynamicObservationReasonRedirectBodyReplayDenied,
			dynamicObservationReasonCapabilityInvalid,
			dynamicObservationReasonCapabilityExpired,
			dynamicObservationReasonResponseFailure,
			dynamicObservationReasonRuntimeUnavailable:
			return true
		}
	}
	return false
}

func isCanonicalDynamicObservationAuthority(value string) bool {
	if value == "" || len(value) > dynamicObservationMaxAuthorityBytes || strings.TrimSpace(value) != value {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Host == "" || parsed.Opaque != "" {
		return false
	}
	if parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return false
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil || host == "" || portText == "" {
		return false
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 || strconv.Itoa(port) != portText {
		return false
	}
	normalizedHost, _, err := normalizeDynamicHost(host)
	if err != nil {
		return false
	}
	return value == parsed.Scheme+"://"+net.JoinHostPort(normalizedHost, portText)
}

// EnqueueDynamicObservation performs validation and a single bounded channel
// send. It never waits for SQLite or reports an optional telemetry failure to
// the media request path.
func (d *DB) EnqueueDynamicObservation(event dynamicObservationEvent) {
	if d == nil {
		return
	}
	if event.SiteID <= 0 || !validDynamicObservationEnums(event.Source, event.Decision, event.ReasonCode) || !isCanonicalDynamicObservationAuthority(event.CanonicalAuthority) {
		d.droppedDynamicObservations.Add(1)
		return
	}
	if d.edgeEphemeral {
		if d.edgeTelemetrySink != nil {
			d.edgeTelemetrySink(edgeTelemetryEvent{Kind: "observation", Observation: event})
		}
		return
	}
	command := dynamicObservationCommand{
		kind: dynamicObservationCommandWrite,
		event: queuedDynamicObservation{
			event:        event,
			observedAtMS: time.Now().UnixMilli(),
		},
	}
	if !d.dynamicObservationGate.TryRLock() {
		d.droppedDynamicObservations.Add(1)
		return
	}
	defer d.dynamicObservationGate.RUnlock()
	if d.dynamicObservationClosed.Load() || d.dynamicObservationQueue == nil {
		d.droppedDynamicObservations.Add(1)
		return
	}
	select {
	case d.dynamicObservationQueue <- command:
	default:
		d.droppedDynamicObservations.Add(1)
	}
}

func (d *DB) DroppedDynamicObservations() uint64 {
	if d == nil {
		return 0
	}
	return d.droppedDynamicObservations.Load()
}

func (d *DB) ClearDynamicObservations(siteID int64) error {
	if siteID <= 0 {
		return fmt.Errorf("invalid dynamic observation site id")
	}
	return d.sendDynamicObservationControl(dynamicObservationCommandClear, siteID)
}

func (d *DB) ListDynamicObservations(siteID int64) ([]DynamicObservation, error) {
	if siteID <= 0 {
		return nil, fmt.Errorf("invalid dynamic observation site id")
	}
	// The ordered barrier makes observations already accepted by the nonblocking
	// queue visible before the read begins.
	if err := d.flushDynamicObservations(); err != nil {
		return nil, err
	}
	rows, err := d.db.Query(`
		SELECT site_id, canonical_authority, source, decision, reason_code, first_seen_ms, last_seen_ms, count
		FROM dynamic_observations
		WHERE site_id=?
		ORDER BY last_seen_ms DESC, first_seen_ms DESC, canonical_authority, source, decision, reason_code`, siteID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	observations := make([]DynamicObservation, 0)
	for rows.Next() {
		var observation DynamicObservation
		if err := rows.Scan(
			&observation.SiteID,
			&observation.CanonicalAuthority,
			&observation.Source,
			&observation.Decision,
			&observation.ReasonCode,
			&observation.FirstSeenMS,
			&observation.LastSeenMS,
			&observation.Count,
		); err != nil {
			return nil, err
		}
		if observation.SiteID != siteID || !isCanonicalDynamicObservationAuthority(observation.CanonicalAuthority) || !validDynamicObservationEnums(observation.Source, observation.Decision, observation.ReasonCode) || observation.FirstSeenMS < 0 || observation.LastSeenMS < observation.FirstSeenMS || observation.Count <= 0 {
			return nil, fmt.Errorf("stored dynamic observation failed validation")
		}
		observations = append(observations, observation)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return observations, nil
}

func (d *DB) writeDynamicObservationBatch(batch []queuedDynamicObservation) (int, error) {
	type observationKey struct {
		siteID             int64
		canonicalAuthority string
		source             string
		decision           string
		reasonCode         string
	}
	type aggregate struct {
		event       dynamicObservationEvent
		firstSeenMS int64
		lastSeenMS  int64
		count       int64
	}
	aggregated := make([]aggregate, 0, len(batch))
	indexes := make(map[observationKey]int, len(batch))
	for _, queued := range batch {
		event := queued.event
		key := observationKey{
			siteID:             event.SiteID,
			canonicalAuthority: event.CanonicalAuthority,
			source:             event.Source,
			decision:           event.Decision,
			reasonCode:         event.ReasonCode,
		}
		if index, ok := indexes[key]; ok {
			current := &aggregated[index]
			current.firstSeenMS = min(current.firstSeenMS, queued.observedAtMS)
			current.lastSeenMS = max(current.lastSeenMS, queued.observedAtMS)
			current.count++
			continue
		}
		indexes[key] = len(aggregated)
		aggregated = append(aggregated, aggregate{
			event:       event,
			firstSeenMS: queued.observedAtMS,
			lastSeenMS:  queued.observedAtMS,
			count:       1,
		})
	}

	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	statement, err := tx.Prepare(`
		INSERT INTO dynamic_observations
			(site_id, canonical_authority, source, decision, reason_code, first_seen_ms, last_seen_ms, count)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM sites WHERE id=?)
		ON CONFLICT(site_id, canonical_authority, source, decision, reason_code) DO UPDATE SET
			first_seen_ms=MIN(dynamic_observations.first_seen_ms, excluded.first_seen_ms),
			last_seen_ms=MAX(dynamic_observations.last_seen_ms, excluded.last_seen_ms),
			count=CASE
				WHEN excluded.count >= 9223372036854775807-dynamic_observations.count THEN 9223372036854775807
				ELSE dynamic_observations.count+excluded.count
			END`)
	if err != nil {
		return 0, err
	}
	defer statement.Close()
	skipped := 0
	for _, current := range aggregated {
		event := current.event
		result, err := statement.Exec(
			event.SiteID,
			event.CanonicalAuthority,
			event.Source,
			event.Decision,
			event.ReasonCode,
			current.firstSeenMS,
			current.lastSeenMS,
			current.count,
			event.SiteID,
		)
		if err != nil {
			return 0, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if rows == 0 {
			skipped += int(current.count)
		}
	}
	if err := pruneDynamicObservationsTx(tx, time.Now()); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return skipped, nil
}

func pruneDynamicObservationsTx(tx *sql.Tx, now time.Time) error {
	cutoffMS := now.Add(-dynamicObservationRetention).UnixMilli()
	if _, err := tx.Exec("DELETE FROM dynamic_observations WHERE last_seen_ms<?", cutoffMS); err != nil {
		return err
	}
	_, err := tx.Exec(`
		DELETE FROM dynamic_observations
		WHERE (site_id, canonical_authority, source, decision, reason_code) IN (
			SELECT site_id, canonical_authority, source, decision, reason_code
			FROM dynamic_observations
			ORDER BY last_seen_ms DESC, first_seen_ms DESC, site_id, canonical_authority, source, decision, reason_code
			LIMIT -1 OFFSET ?
		)`, dynamicObservationGlobalRowLimit)
	return err
}

func (d *DB) pruneDynamicObservations() error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := pruneDynamicObservationsTx(tx, time.Now()); err != nil {
		return err
	}
	return tx.Commit()
}
