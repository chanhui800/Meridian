package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const requestDiagnosticsVersion = 1
const requestDiagnosticsMaxJSONBytes = 6144
const requestDiagnosticsMaxDurationMS = int64(7 * 24 * time.Hour / time.Millisecond)
const requestDiagnosticsMaxAttempts = 8
const requestDiagnosticsMaxAttemptIndex = 128
const requestDiagnosticsMaxAuthorityBytes = 320

// RequestUpstreamAttempt records one actual outbound RoundTrip. Dynamic DNS
// validation can precede the RoundTrip; that work belongs to the same attempt.
// FirstByteMS is the first wire byte (possibly 1xx), while ResponseHeadersMS
// records arrival of the final headers. Both are relative to this attempt.
type RequestUpstreamAttempt struct {
	AttemptIndex       int    `json:"attempt_index"`
	SourceRole         string `json:"source_role,omitempty"`
	Authority          string `json:"authority,omitempty"`
	StartedMS          *int64 `json:"started_ms,omitempty"`
	FirstByteMS        *int64 `json:"first_byte_ms,omitempty"`
	ResponseHeadersMS  *int64 `json:"response_headers_ms,omitempty"`
	DurationMS         *int64 `json:"duration_ms,omitempty"`
	ResponseStatusCode int    `json:"response_status_code,omitempty"`
	ConnectionState    string `json:"connection_state,omitempty"`
	DNSDurationMS      *int64 `json:"dns_duration_ms,omitempty"`
	TCPDurationMS      *int64 `json:"tcp_duration_ms,omitempty"`
	TLSDurationMS      *int64 `json:"tls_duration_ms,omitempty"`
	Outcome            string `json:"outcome,omitempty"`
}

// RequestDiagnostics contains bounded numbers, fixed enums and canonical HTTP
// authorities. A nil timing means it was not observed; measured zero stays zero.
// Version zero identifies records produced before diagnostics were supported.
type RequestDiagnostics struct {
	DiagnosticsVersion        int                      `json:"diagnostics_version,omitempty"`
	ResponseStatusCode        int                      `json:"response_status_code,omitempty"`
	TerminationReason         string                   `json:"termination_reason,omitempty"`
	DurationMS                *int64                   `json:"duration_ms,omitempty"`
	UpstreamFirstByteMS       *int64                   `json:"upstream_first_byte_ms,omitempty"`
	BodyFirstByteMS           *int64                   `json:"body_first_byte_ms,omitempty"`
	BodyBytesWritten          *int64                   `json:"body_bytes_written,omitempty"`
	RangeMode                 int                      `json:"range_mode,omitempty"`
	ConnectionState           string                   `json:"connection_state,omitempty"`
	FinalConnectionState      string                   `json:"final_connection_state,omitempty"`
	RedirectCount             *int                     `json:"redirect_count,omitempty"`
	RetryCount                *int                     `json:"retry_count,omitempty"`
	UpstreamAttempts          []RequestUpstreamAttempt `json:"upstream_attempts,omitempty"`
	UpstreamAttemptsTruncated bool                     `json:"upstream_attempts_truncated,omitempty"`
	DNSDurationMS             *int64                   `json:"dns_duration_ms,omitempty"`
	TCPDurationMS             *int64                   `json:"tcp_duration_ms,omitempty"`
	TLSDurationMS             *int64                   `json:"tls_duration_ms,omitempty"`
	CacheLookup               string                   `json:"cache_lookup,omitempty"`
	CacheWrite                string                   `json:"cache_write,omitempty"`
	CacheLookupReason         string                   `json:"cache_lookup_reason,omitempty"`
	CacheWriteReason          string                   `json:"cache_write_reason,omitempty"`
	CacheReason               string                   `json:"cache_reason,omitempty"`
	ResponseType              string                   `json:"response_type,omitempty"`
}

type requestDiagnosticsContextKey struct{}
type requestDiagnosticsTraceContextKey struct{}
type requestDiagnosticsAttemptContextKey struct{}

// An attempt is only retained when its transport is actually called. Prepared
// attempts let DNS pinning be attributed without inventing a network request
// for a rejected candidate or a redirect returned directly to the client.
type requestDiagnosticsAttempt struct {
	tracker                               *requestDiagnosticsTracker
	index                                 int
	startedAt, finishedAt, firstByte      time.Time
	sourceRole, authority, outcome        string
	responseStatus                        int
	headersObserved                       bool
	newConnections, reusedConnections     bool
	dnsDuration, tcpDuration, tlsDuration time.Duration
	dnsObserved, tcpObserved, tlsObserved bool
}

// The tracker is shared by redirects and response writers. Callbacks hold its
// lock only long enough to update scalar values and never retain request data.
type requestDiagnosticsTracker struct {
	mu                                                               sync.Mutex
	startedAt                                                        time.Time
	rangeMode                                                        int
	upstreamFirstByte                                                time.Time
	bodyFirstByte                                                    time.Time
	bodyBytes                                                        int64
	newConnections, reusedConnections                                bool
	dnsDuration, tcpDuration, tlsDuration                            time.Duration
	dnsObserved, tcpObserved, tlsObserved                            bool
	cacheLookup, cacheWrite, lookupReason, writeReason, responseType string
	upstreamError, downstreamError, aborted                          bool
	attemptCount, redirectCount, retryCount                          int
	attempts                                                         []*requestDiagnosticsAttempt
	finalAttempt                                                     *requestDiagnosticsAttempt
}

func newRequestDiagnosticsTracker(startedAt time.Time, req *http.Request) *requestDiagnosticsTracker {
	rangeMode := 1
	if req != nil && strings.TrimSpace(req.Header.Get("Range")) != "" {
		rangeMode = 2
	}
	return &requestDiagnosticsTracker{startedAt: startedAt, rangeMode: rangeMode}
}

func requestDiagnosticsFromContext(ctx context.Context) *requestDiagnosticsTracker {
	if ctx == nil {
		return nil
	}
	tracker, _ := ctx.Value(requestDiagnosticsContextKey{}).(*requestDiagnosticsTracker)
	return tracker
}

func requestDiagnosticsAttemptFromContext(ctx context.Context) *requestDiagnosticsAttempt {
	if ctx == nil {
		return nil
	}
	attempt, _ := ctx.Value(requestDiagnosticsAttemptContextKey{}).(*requestDiagnosticsAttempt)
	return attempt
}

func prepareRequestDiagnosticsAttempt(ctx context.Context, target *url.URL, sourceRole string) context.Context {
	tracker := requestDiagnosticsFromContext(ctx)
	if tracker == nil {
		return ctx
	}
	attempt := &requestDiagnosticsAttempt{
		tracker: tracker, startedAt: time.Now(), sourceRole: sourceRole,
		authority: requestDiagnosticsAuthorityURL(target),
	}
	return context.WithValue(ctx, requestDiagnosticsAttemptContextKey{}, attempt)
}

// Preserve the private prepared attempt explicitly after isolating a dynamic
// context. Caller traces and arbitrary context values stay outside this domain.
func isolateDynamicRequestDiagnosticsContext(ctx context.Context) context.Context {
	isolated := isolateDynamicOutboundContext(ctx)
	if attempt := requestDiagnosticsAttemptFromContext(ctx); attempt != nil && attempt.tracker == requestDiagnosticsFromContext(isolated) {
		return context.WithValue(isolated, requestDiagnosticsAttemptContextKey{}, attempt)
	}
	return isolated
}

func recordRequestDiagnosticsRedirect(ctx context.Context) {
	if tracker := requestDiagnosticsFromContext(ctx); tracker != nil {
		tracker.mu.Lock()
		if tracker.redirectCount < requestDiagnosticsMaxAttemptIndex {
			tracker.redirectCount++
		}
		tracker.mu.Unlock()
	}
}

func recordRequestDiagnosticsRetry(ctx context.Context) {
	if tracker := requestDiagnosticsFromContext(ctx); tracker != nil {
		tracker.mu.Lock()
		if tracker.retryCount < requestDiagnosticsMaxAttemptIndex {
			tracker.retryCount++
		}
		tracker.mu.Unlock()
	}
}

func requestDiagnosticsConnectionState(newConnections, reusedConnections bool) string {
	switch {
	case newConnections && reusedConnections:
		return "mixed"
	case newConnections:
		return "new"
	case reusedConnections:
		return "reused"
	default:
		return ""
	}
}

// Called with the owning tracker's lock held. Snapshot values have no shared
// slices or mutable pointers to the live trace callbacks.
func (a *requestDiagnosticsAttempt) snapshot(requestStartedAt time.Time) RequestUpstreamAttempt {
	result := RequestUpstreamAttempt{
		AttemptIndex: a.index, SourceRole: a.sourceRole, Authority: a.authority,
		StartedMS:          requestDiagnosticsMS(a.startedAt.Sub(requestStartedAt)),
		ResponseStatusCode: a.responseStatus, Outcome: a.outcome,
		ConnectionState: requestDiagnosticsConnectionState(a.newConnections, a.reusedConnections),
	}
	if !a.firstByte.IsZero() {
		result.FirstByteMS = requestDiagnosticsMS(a.firstByte.Sub(a.startedAt))
	}
	if !a.finishedAt.IsZero() {
		result.DurationMS = requestDiagnosticsMS(a.finishedAt.Sub(a.startedAt))
		if a.headersObserved {
			result.ResponseHeadersMS = requestDiagnosticsMS(a.finishedAt.Sub(a.startedAt))
		}
	}
	if a.dnsObserved {
		result.DNSDurationMS = requestDiagnosticsMS(a.dnsDuration)
	}
	if a.tcpObserved {
		result.TCPDurationMS = requestDiagnosticsMS(a.tcpDuration)
	}
	if a.tlsObserved {
		result.TLSDurationMS = requestDiagnosticsMS(a.tlsDuration)
	}
	return result
}

// An upstream acknowledgement is independent of whether a canceled client
// received those response headers. Synthesized gateway responses are excluded.
func (t *requestDiagnosticsTracker) upstreamResponseStatusCode() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finalAttempt == nil || !t.finalAttempt.headersObserved {
		return 0
	}
	return t.finalAttempt.responseStatus
}

func (t *requestDiagnosticsTracker) SetCacheLookup(lookup, reason string) {
	if t == nil {
		return
	}
	lookup = requestDiagnosticsEnum(lookup, "disabled", "bypass", "hit", "miss", "read_error")
	reason = requestDiagnosticsCacheReason(reason)
	t.mu.Lock()
	t.cacheLookup, t.lookupReason = lookup, reason
	t.mu.Unlock()
}

func (t *requestDiagnosticsTracker) SetCacheWrite(write, reason string) {
	if t == nil {
		return
	}
	write = requestDiagnosticsEnum(write, "not_attempted", "stored", "skipped", "write_error")
	reason = requestDiagnosticsCacheReason(reason)
	t.mu.Lock()
	t.cacheWrite, t.writeReason = write, reason
	t.mu.Unlock()
}

func (t *requestDiagnosticsTracker) SetResponseType(contentType string) {
	if t == nil {
		return
	}
	responseType := requestDiagnosticsResponseType(contentType)
	t.mu.Lock()
	t.responseType = responseType
	t.mu.Unlock()
}

func (t *requestDiagnosticsTracker) observeBodyWrite(n int, now time.Time, failed bool) {
	if t == nil {
		return
	}
	t.mu.Lock()
	if n > 0 {
		if t.bodyFirstByte.IsZero() {
			t.bodyFirstByte = now
		}
		t.bodyBytes += int64(n)
	}
	t.downstreamError = t.downstreamError || failed
	t.mu.Unlock()
}

func (t *requestDiagnosticsTracker) markUpstreamError() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.upstreamError = true
	t.mu.Unlock()
}

func (t *requestDiagnosticsTracker) markAborted() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.aborted = true
	t.mu.Unlock()
}

func requestDiagnosticsMS(duration time.Duration) *int64 {
	value := duration.Milliseconds()
	if value < 0 {
		value = 0
	}
	if value > requestDiagnosticsMaxDurationMS {
		value = requestDiagnosticsMaxDurationMS
	}
	return &value
}

func (t *requestDiagnosticsTracker) snapshot(now time.Time, responseStatus int, clientErr, requestErr error) RequestDiagnostics {
	if t == nil {
		return RequestDiagnostics{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	bytesWritten := t.bodyBytes
	redirectCount, retryCount := t.redirectCount, t.retryCount
	result := RequestDiagnostics{
		DiagnosticsVersion: requestDiagnosticsVersion,
		ResponseStatusCode: responseStatus,
		TerminationReason:  "completed",
		DurationMS:         requestDiagnosticsMS(now.Sub(t.startedAt)),
		BodyBytesWritten:   &bytesWritten,
		RangeMode:          t.rangeMode,
		RedirectCount:      &redirectCount, RetryCount: &retryCount,
		CacheLookup: t.cacheLookup, CacheWrite: t.cacheWrite,
		CacheLookupReason: t.lookupReason, CacheWriteReason: t.writeReason,
		CacheReason: t.lookupReason, ResponseType: t.responseType,
	}
	// A successful fill must not erase a failed lookup that forced a refetch.
	if t.cacheLookup != "read_error" && t.writeReason != "" {
		result.CacheReason = t.writeReason
	}
	if !t.upstreamFirstByte.IsZero() {
		result.UpstreamFirstByteMS = requestDiagnosticsMS(t.upstreamFirstByte.Sub(t.startedAt))
	}
	if !t.bodyFirstByte.IsZero() {
		result.BodyFirstByteMS = requestDiagnosticsMS(t.bodyFirstByte.Sub(t.startedAt))
	}
	result.ConnectionState = requestDiagnosticsConnectionState(t.newConnections, t.reusedConnections)
	if t.finalAttempt != nil {
		result.FinalConnectionState = requestDiagnosticsConnectionState(t.finalAttempt.newConnections, t.finalAttempt.reusedConnections)
	}
	if len(t.attempts) > 0 {
		result.UpstreamAttemptsTruncated = t.attemptCount > requestDiagnosticsMaxAttempts
		result.UpstreamAttempts = make([]RequestUpstreamAttempt, 0, len(t.attempts))
		for _, attempt := range t.attempts {
			result.UpstreamAttempts = append(result.UpstreamAttempts, attempt.snapshot(t.startedAt))
		}
	}
	if t.dnsObserved {
		result.DNSDurationMS = requestDiagnosticsMS(t.dnsDuration)
	}
	if t.tcpObserved {
		result.TCPDurationMS = requestDiagnosticsMS(t.tcpDuration)
	}
	if t.tlsObserved {
		result.TLSDurationMS = requestDiagnosticsMS(t.tlsDuration)
	}
	switch {
	case clientErr == context.Canceled:
		result.TerminationReason = "client_canceled"
	case clientErr == context.DeadlineExceeded:
		result.TerminationReason = "client_deadline"
	case requestErr != nil:
		result.TerminationReason = "site_stopping"
	case t.downstreamError:
		result.TerminationReason = "downstream_error"
	case t.upstreamError:
		result.TerminationReason = "upstream_error"
	case t.aborted:
		result.TerminationReason = "proxy_aborted"
	}
	if result.TerminationReason == "completed" && result.ResponseStatusCode == 0 {
		// net/http sends its implicit 200 when a normal handler returns without
		// explicitly writing headers or a body.
		result.ResponseStatusCode = http.StatusOK
	}
	return result
}

type requestDiagnosticsReadCloser struct {
	io.ReadCloser
	tracker *requestDiagnosticsTracker
}

func (body *requestDiagnosticsReadCloser) Read(payload []byte) (int, error) {
	n, err := body.ReadCloser.Read(payload)
	if err != nil && err != io.EOF {
		body.tracker.markUpstreamError()
	}
	return n, err
}

func observeRequestDiagnosticsResponse(resp *http.Response) {
	if resp == nil || resp.Request == nil {
		return
	}
	tracker := requestDiagnosticsFromContext(resp.Request.Context())
	if tracker == nil {
		return
	}
	tracker.SetResponseType(resp.Header.Get("Content-Type"))
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return
	}
	if resp.Body != nil {
		if _, alreadyWrapped := resp.Body.(*requestDiagnosticsReadCloser); !alreadyWrapped {
			resp.Body = &requestDiagnosticsReadCloser{ReadCloser: resp.Body, tracker: tracker}
		}
	}
}

// Discard inherited traces before installing our callbacks. In particular,
// clients cannot attach Got1xxResponse callbacks to an isolated CDN request.
type requestDiagnosticsOwnedTraceContext struct{ context.Context }

func (c requestDiagnosticsOwnedTraceContext) Value(key any) any {
	value := c.Context.Value(key)
	if _, inheritedTrace := value.(*httptrace.ClientTrace); inheritedTrace {
		return nil
	}
	return value
}

// Clean the configured request before ReverseProxy adds its own 1xx forwarding
// callback. The actual attempt trace is installed immediately at RoundTrip.
func ownConfiguredRequestDiagnosticsContext(req *http.Request) *http.Request {
	if req == nil {
		return req
	}
	tracker := requestDiagnosticsFromContext(req.Context())
	if tracker == nil {
		return req
	}
	ctx := context.WithValue(requestDiagnosticsOwnedTraceContext{req.Context()}, requestDiagnosticsTraceContextKey{}, tracker)
	return req.WithContext(ctx)
}

type requestDiagnosticsRoundTripOwner interface {
	requestDiagnosticsOwnsRoundTrip()
}

type requestDiagnosticsTransport struct {
	base       http.RoundTripper
	sourceRole string
}

func (*requestDiagnosticsTransport) requestDiagnosticsOwnsRoundTrip() {}

func (t *requestDiagnosticsTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return roundTripWithRequestDiagnostics(t.base, req, t.sourceRole)
}

// Redirect and failover wrappers can also be exercised with an unwrapped test
// transport. Already owned configured/dynamic transports instrument themselves
// at their physical RoundTrip boundary, so no nested trace is accumulated.
func roundTripWithRequestDiagnostics(base http.RoundTripper, req *http.Request, sourceRole string) (*http.Response, error) {
	if _, owned := base.(requestDiagnosticsRoundTripOwner); owned {
		return base.RoundTrip(req)
	}
	req, attempt := beginRequestDiagnosticsRoundTrip(req, sourceRole)
	resp, err := base.RoundTrip(req)
	finishRequestDiagnosticsRoundTrip(attempt, resp, err, req.Context().Err())
	if resp != nil {
		resp.Request = req
	}
	return resp, err
}

func beginRequestDiagnosticsRoundTrip(req *http.Request, sourceRole string) (*http.Request, *requestDiagnosticsAttempt) {
	if req == nil {
		return req, nil
	}
	tracker := requestDiagnosticsFromContext(req.Context())
	if tracker == nil {
		return req, nil
	}
	authority := requestDiagnosticsAuthorityURL(req.URL)
	attempt := requestDiagnosticsAttemptFromContext(req.Context())
	tracker.mu.Lock()
	if attempt == nil || attempt.tracker != tracker || attempt.index != 0 || attempt.authority != authority || attempt.sourceRole != sourceRole {
		attempt = &requestDiagnosticsAttempt{tracker: tracker, startedAt: time.Now(), sourceRole: sourceRole, authority: authority}
	}
	if tracker.attemptCount < requestDiagnosticsMaxAttemptIndex {
		tracker.attemptCount++
	}
	attempt.index = tracker.attemptCount
	if len(tracker.attempts) < requestDiagnosticsMaxAttempts {
		tracker.attempts = append(tracker.attempts, attempt)
	} else {
		// Preserve the first seven steps and the final one, including a final
		// failure. The real index makes omitted middle steps visible to readers.
		tracker.attempts[requestDiagnosticsMaxAttempts-1] = attempt
	}
	tracker.finalAttempt = attempt
	tracker.upstreamFirstByte = time.Time{}
	tracker.mu.Unlock()
	return attachRequestDiagnosticsAttemptTrace(req, attempt), attempt
}

func finishRequestDiagnosticsRoundTrip(attempt *requestDiagnosticsAttempt, resp *http.Response, err, contextErr error) {
	if attempt == nil {
		return
	}
	now := time.Now()
	attempt.tracker.mu.Lock()
	defer attempt.tracker.mu.Unlock()
	attempt.finishedAt = now
	if resp != nil && resp.StatusCode >= 100 && resp.StatusCode <= 599 {
		attempt.responseStatus = resp.StatusCode
		attempt.headersObserved = err == nil
	}
	switch {
	case err == nil && resp != nil:
		attempt.outcome = "response"
	case errors.Is(err, context.Canceled) || errors.Is(contextErr, context.Canceled):
		attempt.outcome = "canceled"
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(contextErr, context.DeadlineExceeded):
		attempt.outcome = "deadline"
	default:
		attempt.outcome = "error"
	}
}

func attachRequestDiagnosticsAttemptTrace(req *http.Request, attempt *requestDiagnosticsAttempt) *http.Request {
	tracker := attempt.tracker
	// Starts live only for this outbound request and are bounded independently
	// of the aggregate tracker. TCP dials can overlap under Happy Eyeballs.
	var phaseMu sync.Mutex
	var dnsStarts, tlsStarts []time.Time
	connectStarts := make(map[string][]time.Time)
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			phaseMu.Lock()
			if len(dnsStarts) < 8 {
				dnsStarts = append(dnsStarts, time.Now())
			}
			phaseMu.Unlock()
		},
		DNSDone: func(httptrace.DNSDoneInfo) {
			phaseMu.Lock()
			if len(dnsStarts) == 0 {
				phaseMu.Unlock()
				return
			}
			start := dnsStarts[0]
			dnsStarts = dnsStarts[1:]
			phaseMu.Unlock()
			duration := time.Since(start)
			tracker.mu.Lock()
			tracker.dnsDuration += duration
			tracker.dnsObserved = true
			attempt.dnsDuration += duration
			attempt.dnsObserved = true
			tracker.mu.Unlock()
		},
		ConnectStart: func(network, address string) {
			phaseMu.Lock()
			key := network + "\x00" + address
			if len(connectStarts) < 8 || len(connectStarts[key]) != 0 {
				if len(connectStarts[key]) < 8 {
					connectStarts[key] = append(connectStarts[key], time.Now())
				}
			}
			phaseMu.Unlock()
		},
		ConnectDone: func(network, address string, _ error) {
			phaseMu.Lock()
			key := network + "\x00" + address
			starts := connectStarts[key]
			if len(starts) == 0 {
				phaseMu.Unlock()
				return
			}
			start := starts[0]
			if len(starts) == 1 {
				delete(connectStarts, key)
			} else {
				connectStarts[key] = starts[1:]
			}
			phaseMu.Unlock()
			tracker.mu.Lock()
			duration := time.Since(start)
			tracker.tcpDuration += duration
			tracker.tcpObserved = true
			attempt.tcpDuration += duration
			attempt.tcpObserved = true
			tracker.mu.Unlock()
		},
		TLSHandshakeStart: func() {
			phaseMu.Lock()
			if len(tlsStarts) < 8 {
				tlsStarts = append(tlsStarts, time.Now())
			}
			phaseMu.Unlock()
		},
		TLSHandshakeDone: func(tls.ConnectionState, error) {
			phaseMu.Lock()
			if len(tlsStarts) == 0 {
				phaseMu.Unlock()
				return
			}
			start := tlsStarts[0]
			tlsStarts = tlsStarts[1:]
			phaseMu.Unlock()
			tracker.mu.Lock()
			duration := time.Since(start)
			tracker.tlsDuration += duration
			tracker.tlsObserved = true
			attempt.tlsDuration += duration
			attempt.tlsObserved = true
			tracker.mu.Unlock()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			tracker.mu.Lock()
			if info.Reused {
				tracker.reusedConnections = true
				attempt.reusedConnections = true
			} else {
				tracker.newConnections = true
				attempt.newConnections = true
			}
			tracker.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			tracker.mu.Lock()
			if attempt.firstByte.IsZero() {
				attempt.firstByte = time.Now()
				if tracker.finalAttempt == attempt {
					tracker.upstreamFirstByte = attempt.firstByte
				}
			}
			tracker.mu.Unlock()
		},
	}
	if attempt.sourceRole == "configured" {
		// Only the callback added after our configured context was cleaned is
		// retained. Dynamic requests never inherit even this callback.
		if owned, _ := req.Context().Value(requestDiagnosticsTraceContextKey{}).(*requestDiagnosticsTracker); owned == tracker {
			if inherited := httptrace.ContextClientTrace(req.Context()); inherited != nil {
				trace.Got1xxResponse = inherited.Got1xxResponse
			}
		}
	}
	ctx := context.WithValue(requestDiagnosticsOwnedTraceContext{req.Context()}, requestDiagnosticsTraceContextKey{}, tracker)
	ctx = context.WithValue(ctx, requestDiagnosticsAttemptContextKey{}, attempt)
	return req.WithContext(httptrace.WithClientTrace(ctx, trace))
}

func observeDynamicDNSDuration(ctx context.Context, duration time.Duration) {
	tracker := requestDiagnosticsFromContext(ctx)
	if tracker == nil || duration < 0 {
		return
	}
	attempt := requestDiagnosticsAttemptFromContext(ctx)
	tracker.mu.Lock()
	tracker.dnsDuration += duration
	tracker.dnsObserved = true
	if attempt != nil && attempt.tracker == tracker && attempt.finishedAt.IsZero() {
		attempt.dnsDuration += duration
		attempt.dnsObserved = true
	}
	tracker.mu.Unlock()
}

// Use syntax normalization here, rather than public-target authorization: a
// configured upstream can legitimately be a private address or local hostname.
// Only the HTTP authority is retained, never credentials or URL components.
func requestDiagnosticsAuthorityURL(target *url.URL) string {
	if target == nil || target.Opaque != "" || target.Host == "" || strings.Contains(target.Host, "%") || strings.HasSuffix(target.Host, ":") {
		return ""
	}
	scheme := strings.ToLower(target.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	host, _, err := normalizeDynamicHostSyntax(target.Hostname())
	if err != nil {
		return ""
	}
	port := target.Port()
	if port == "" {
		port = "80"
		if scheme == "https" {
			port = "443"
		}
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return ""
	}
	authority := scheme + "://" + net.JoinHostPort(host, strconv.Itoa(portNumber))
	if len(authority) > requestDiagnosticsMaxAuthorityBytes {
		return ""
	}
	return authority
}

func requestDiagnosticsAuthority(value string) string {
	if value == "" || len(value) > requestDiagnosticsMaxAuthorityBytes {
		return ""
	}
	target, err := url.Parse(value)
	if err != nil {
		return ""
	}
	return requestDiagnosticsAuthorityURL(target)
}

func requestDiagnosticsEnum(value string, allowed ...string) string {
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return ""
}

func requestDiagnosticsCacheReason(value string) string {
	return requestDiagnosticsEnum(value, "disabled", "method", "rule_no_match", "range", "dynamic", "head_no_fill", "response_status", "set_cookie", "no_store", "no_cache", "private", "vary", "mime", "too_large", "empty_body", "permission", "no_space", "readonly", "io", "expired", "invalid_entry", "evicted", "canceled", "incomplete", "length_mismatch", "generation_changed")
}

func requestDiagnosticsResponseType(contentType string) string {
	if len(contentType) > 512 {
		return "other"
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		if contentType == "" {
			return ""
		}
		return "other"
	}
	mediaType = strings.ToLower(mediaType)
	switch {
	case mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
		return "json"
	case mediaType == "application/vnd.apple.mpegurl" || mediaType == "application/x-mpegurl" || mediaType == "audio/mpegurl" || mediaType == "audio/x-mpegurl" || mediaType == "application/dash+xml":
		return "manifest"
	case mediaType == "text/vtt" || mediaType == "application/x-subrip" || mediaType == "text/x-ssa" || mediaType == "text/x-ass":
		return "subtitle"
	case strings.HasPrefix(mediaType, "image/"):
		return "image"
	case strings.HasPrefix(mediaType, "video/"):
		return "video"
	case strings.HasPrefix(mediaType, "audio/"):
		return "audio"
	case strings.HasPrefix(mediaType, "font/") || mediaType == "application/font-woff" || mediaType == "application/vnd.ms-fontobject":
		return "font"
	case mediaType == "application/javascript" || mediaType == "text/javascript":
		return "script"
	case mediaType == "text/css":
		return "style"
	case mediaType == "text/html":
		return "html"
	case strings.HasPrefix(mediaType, "text/"):
		return "text"
	case mediaType == "application/octet-stream":
		return "binary"
	default:
		return "other"
	}
}

func sanitizeRequestDiagnostics(value RequestDiagnostics) RequestDiagnostics {
	if value.DiagnosticsVersion != requestDiagnosticsVersion {
		return RequestDiagnostics{}
	}
	if value.ResponseStatusCode < 100 || value.ResponseStatusCode > 599 {
		value.ResponseStatusCode = 0
	}
	value.TerminationReason = requestDiagnosticsEnum(value.TerminationReason, "completed", "client_canceled", "client_deadline", "upstream_error", "downstream_error", "proxy_aborted", "site_stopping")
	for _, field := range []**int64{&value.DurationMS, &value.UpstreamFirstByteMS, &value.BodyFirstByteMS, &value.DNSDurationMS, &value.TCPDurationMS, &value.TLSDurationMS} {
		if *field != nil && (**field < 0 || **field > requestDiagnosticsMaxDurationMS) {
			*field = nil
		}
	}
	if value.BodyBytesWritten != nil && *value.BodyBytesWritten < 0 {
		value.BodyBytesWritten = nil
	}
	if value.RangeMode != 1 && value.RangeMode != 2 {
		value.RangeMode = 0
	}
	value.ConnectionState = requestDiagnosticsEnum(value.ConnectionState, "new", "reused", "mixed")
	value.FinalConnectionState = requestDiagnosticsEnum(value.FinalConnectionState, "new", "reused", "mixed")
	for _, field := range []**int{&value.RedirectCount, &value.RetryCount} {
		if *field != nil && (**field < 0 || **field > requestDiagnosticsMaxAttemptIndex) {
			*field = nil
		}
	}
	inputAttempts := value.UpstreamAttempts
	if len(inputAttempts) > requestDiagnosticsMaxAttempts {
		bounded := make([]RequestUpstreamAttempt, requestDiagnosticsMaxAttempts)
		copy(bounded, inputAttempts[:requestDiagnosticsMaxAttempts-1])
		bounded[requestDiagnosticsMaxAttempts-1] = inputAttempts[len(inputAttempts)-1]
		inputAttempts = bounded
		value.UpstreamAttemptsTruncated = true
	}
	value.UpstreamAttempts = nil
	previousIndex := 0
	for _, attempt := range inputAttempts {
		if attempt.AttemptIndex <= previousIndex || attempt.AttemptIndex > requestDiagnosticsMaxAttemptIndex {
			continue
		}
		previousIndex = attempt.AttemptIndex
		attempt.SourceRole = requestDiagnosticsEnum(attempt.SourceRole, "configured", "dynamic")
		attempt.Authority = requestDiagnosticsAuthority(attempt.Authority)
		if attempt.ResponseStatusCode < 100 || attempt.ResponseStatusCode > 599 {
			attempt.ResponseStatusCode = 0
		}
		attempt.ConnectionState = requestDiagnosticsEnum(attempt.ConnectionState, "new", "reused", "mixed")
		attempt.Outcome = requestDiagnosticsEnum(attempt.Outcome, "response", "error", "canceled", "deadline")
		for _, field := range []**int64{&attempt.StartedMS, &attempt.FirstByteMS, &attempt.ResponseHeadersMS, &attempt.DurationMS, &attempt.DNSDurationMS, &attempt.TCPDurationMS, &attempt.TLSDurationMS} {
			if *field != nil && (**field < 0 || **field > requestDiagnosticsMaxDurationMS) {
				*field = nil
			}
		}
		if attempt.DurationMS != nil {
			if attempt.FirstByteMS != nil && *attempt.FirstByteMS > *attempt.DurationMS {
				attempt.FirstByteMS = nil
			}
			if attempt.ResponseHeadersMS != nil && *attempt.ResponseHeadersMS > *attempt.DurationMS {
				attempt.ResponseHeadersMS = nil
			}
		}
		value.UpstreamAttempts = append(value.UpstreamAttempts, attempt)
	}
	if len(value.UpstreamAttempts) == 0 {
		value.UpstreamAttemptsTruncated = false
	}
	value.CacheLookup = requestDiagnosticsEnum(value.CacheLookup, "disabled", "bypass", "hit", "miss", "read_error")
	value.CacheWrite = requestDiagnosticsEnum(value.CacheWrite, "not_attempted", "stored", "skipped", "write_error")
	value.CacheLookupReason = requestDiagnosticsCacheReason(value.CacheLookupReason)
	value.CacheWriteReason = requestDiagnosticsCacheReason(value.CacheWriteReason)
	value.CacheReason = requestDiagnosticsCacheReason(value.CacheReason)
	value.ResponseType = requestDiagnosticsEnum(value.ResponseType, "json", "manifest", "subtitle", "image", "video", "audio", "font", "script", "style", "html", "text", "binary", "other")
	return boundRequestDiagnosticsPayload(value)
}

func boundRequestDiagnosticsPayload(value RequestDiagnostics) RequestDiagnostics {
	if len(value.UpstreamAttempts) == 0 {
		return value
	}
	encoded, err := json.Marshal(value)
	for err == nil && len(encoded) > requestDiagnosticsMaxJSONBytes && len(value.UpstreamAttempts) > 1 {
		// Both the Agent's struct payload and persisted JSON use this bound.
		// Keep the final result, real indices and counters when omitting steps.
		last := value.UpstreamAttempts[len(value.UpstreamAttempts)-1]
		value.UpstreamAttempts = append(value.UpstreamAttempts[:len(value.UpstreamAttempts)-2], last)
		value.UpstreamAttemptsTruncated = true
		encoded, err = json.Marshal(value)
	}
	return value
}

func requestDiagnosticsJSON(value RequestDiagnostics) string {
	value = sanitizeRequestDiagnostics(value)
	if value.DiagnosticsVersion == 0 {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > requestDiagnosticsMaxJSONBytes {
		return ""
	}
	return string(encoded)
}

func parseRequestDiagnosticsJSON(encoded string) RequestDiagnostics {
	var value RequestDiagnostics
	if encoded == "" || len(encoded) > requestDiagnosticsMaxJSONBytes || json.Unmarshal([]byte(encoded), &value) != nil {
		return RequestDiagnostics{}
	}
	return sanitizeRequestDiagnostics(value)
}

// Validate additive Agent telemetry before it reaches a report transaction or
// spool. Comparing the fixed struct after sanitation rejects noncanonical
// authorities, unknown enums, invalid timing ranges and excess/unsorted steps.
func validRequestDiagnosticsWire(value RequestDiagnostics) bool {
	if len(value.UpstreamAttempts) > requestDiagnosticsMaxAttempts || value.DiagnosticsVersion != 0 && value.DiagnosticsVersion != requestDiagnosticsVersion {
		return false
	}
	remainingTextBytes := requestDiagnosticsMaxJSONBytes
	for _, text := range []string{value.TerminationReason, value.ConnectionState, value.FinalConnectionState, value.CacheLookup, value.CacheWrite, value.CacheLookupReason, value.CacheWriteReason, value.CacheReason, value.ResponseType} {
		if len(text) > remainingTextBytes {
			return false
		}
		remainingTextBytes -= len(text)
	}
	for _, attempt := range value.UpstreamAttempts {
		if len(attempt.Authority) > requestDiagnosticsMaxAuthorityBytes {
			return false
		}
		for _, text := range []string{attempt.SourceRole, attempt.Authority, attempt.ConnectionState, attempt.Outcome} {
			if len(text) > remainingTextBytes {
				return false
			}
			remainingTextBytes -= len(text)
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > requestDiagnosticsMaxJSONBytes {
		return false
	}
	if value.DiagnosticsVersion == 0 {
		// Legacy optional scalar diagnostics are ignored by sanitation. Do not
		// reject the report and its acknowledgement for those absent-version
		// fields, but do not accept an unversioned attempt array.
		return len(value.UpstreamAttempts) == 0
	}
	cleaned, err := json.Marshal(sanitizeRequestDiagnostics(value))
	return err == nil && string(encoded) == string(cleaned)
}
