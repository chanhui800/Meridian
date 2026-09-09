package main

import (
	"container/list"
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// agentCredentialIdentity is attached to a request after the short pre-auth
// admission window has authenticated its bearer credential. Keeping the
// result in context avoids repeating the SQLite lookup in each handler while
// ensuring the global admission slot is released before any slow work such as
// a GitHub binary proxy begins.
type agentCredentialIdentity struct {
	Token      string
	Node       ControlNode
	HasNode    bool
	Enrollment bool
}

type agentCredentialContextKey struct{}

func withAgentCredential(r *http.Request, identity agentCredentialIdentity) *http.Request {
	if r == nil {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), agentCredentialContextKey{}, identity))
}

func agentCredentialFromContext(ctx context.Context) (agentCredentialIdentity, bool) {
	if ctx == nil {
		return agentCredentialIdentity{}, false
	}
	identity, ok := ctx.Value(agentCredentialContextKey{}).(agentCredentialIdentity)
	return identity, ok && identity.Token != ""
}

const (
	nodeReportRatePerSecond = 1.0
	nodeReportBurst         = 4.0
	nodeReportEntryTTL      = 15 * time.Minute
)

type nodeReportAdmissionEntry struct {
	tokens   float64
	last     time.Time
	lastSeen time.Time
	inFlight bool
}

// nodeReportAdmission is shared by the HTTP and WebSocket report handlers.
// It authenticates a node before body decoding, limits bursts independently
// per node, and admits at most one database transaction at a time per node.
type nodeReportAdmission struct {
	mu      sync.Mutex
	entries map[int64]*nodeReportAdmissionEntry
}

const (
	agentPreAuthRatePerSecond     = 10.0
	agentPreAuthBurst             = 20.0
	agentPreAuthEntryTTL          = 15 * time.Minute
	agentPreAuthConcurrency       = 32
	maxTrackedAgentPreAuthClients = 4096
	agentPreAuthPruneInterval     = time.Minute
)

type agentPreAuthEntry struct {
	tokens         float64
	last           time.Time
	lastSeen       time.Time
	active         int
	inactiveHandle *list.Element
}

// agentPreAuthAdmission is deliberately keyed by the trusted-proxy-aware
// client identity rather than an Agent token. It runs before the SQLite token
// lookup so a stream of invalid credentials cannot monopolize the sole DB
// connection.
type agentPreAuthAdmission struct {
	mu         sync.Mutex
	entries    map[string]*agentPreAuthEntry
	inactive   *list.List
	active     int
	maxEntries int
	lastPrune  time.Time
}

func newAgentPreAuthAdmission() *agentPreAuthAdmission {
	return &agentPreAuthAdmission{entries: make(map[string]*agentPreAuthEntry), inactive: list.New(), maxEntries: maxTrackedAgentPreAuthClients}
}

func (a *agentPreAuthAdmission) addInactive(key string, entry *agentPreAuthEntry) {
	if entry == nil || entry.active > 0 {
		return
	}
	if a.inactive == nil {
		a.inactive = list.New()
	}
	if entry.inactiveHandle != nil {
		a.inactive.MoveToBack(entry.inactiveHandle)
		return
	}
	entry.inactiveHandle = a.inactive.PushBack(key)
}

func (a *agentPreAuthAdmission) removeInactive(entry *agentPreAuthEntry) {
	if entry == nil || entry.inactiveHandle == nil || a.inactive == nil {
		return
	}
	a.inactive.Remove(entry.inactiveHandle)
	entry.inactiveHandle = nil
}

func (a *agentPreAuthAdmission) prune(now time.Time) {
	for key, entry := range a.entries {
		if entry.active == 0 && now.Sub(entry.lastSeen) > agentPreAuthEntryTTL {
			a.removeInactive(entry)
			delete(a.entries, key)
		}
	}
}

func (a *agentPreAuthAdmission) maybePrune(now time.Time) {
	if !a.lastPrune.IsZero() && now.Sub(a.lastPrune) < agentPreAuthPruneInterval {
		return
	}
	a.prune(now)
	a.lastPrune = now
}

func (a *agentPreAuthAdmission) evictOne() {
	if a.inactive == nil {
		return
	}
	for element := a.inactive.Front(); element != nil; element = a.inactive.Front() {
		key, ok := element.Value.(string)
		a.inactive.Remove(element)
		if !ok {
			continue
		}
		entry := a.entries[key]
		if entry == nil || entry.active > 0 {
			if entry != nil {
				entry.inactiveHandle = nil
			}
			continue
		}
		entry.inactiveHandle = nil
		delete(a.entries, key)
		return
	}
}

func (a *agentPreAuthAdmission) admit(key string, now time.Time) (func(), time.Duration, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if key == "" {
		key = "unknown"
	}
	a.maybePrune(now)
	if a.active >= agentPreAuthConcurrency {
		return func() {}, time.Second, false
	}
	entry := a.entries[key]
	if entry == nil {
		if a.maxEntries <= 0 {
			a.maxEntries = maxTrackedAgentPreAuthClients
		}
		if len(a.entries) >= a.maxEntries {
			a.evictOne()
			if len(a.entries) >= a.maxEntries {
				return func() {}, time.Second, false
			}
		}
		entry = &agentPreAuthEntry{tokens: agentPreAuthBurst, last: now, lastSeen: now}
		a.entries[key] = entry
		a.addInactive(key, entry)
	} else if entry.active == 0 {
		a.removeInactive(entry)
	}
	if entry.last.IsZero() {
		entry.last = now
	}
	if elapsed := now.Sub(entry.last).Seconds(); elapsed > 0 {
		entry.tokens += elapsed * agentPreAuthRatePerSecond
		if entry.tokens > agentPreAuthBurst {
			entry.tokens = agentPreAuthBurst
		}
		entry.last = now
	}
	entry.lastSeen = now
	if entry.tokens < 1 {
		a.addInactive(key, entry)
		wait := time.Duration((1 - entry.tokens) / agentPreAuthRatePerSecond * float64(time.Second))
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		return func() {}, wait, false
	}
	entry.tokens--
	entry.active++
	a.active++
	return func() {
		a.mu.Lock()
		if a.active > 0 {
			a.active--
		}
		if entry.active > 0 {
			entry.active--
		}
		if entry.active == 0 {
			a.addInactive(key, entry)
		}
		if current := a.entries[key]; current != nil {
			current.lastSeen = time.Now()
		}
		a.mu.Unlock()
	}, 0, true
}

func newNodeReportAdmission() *nodeReportAdmission {
	return &nodeReportAdmission{entries: make(map[int64]*nodeReportAdmissionEntry)}
}

func (a *nodeReportAdmission) prune(now time.Time) {
	for id, entry := range a.entries {
		if !entry.inFlight && now.Sub(entry.lastSeen) > nodeReportEntryTTL {
			delete(a.entries, id)
		}
	}
}

// admit returns release, retryAfter and admitted. A rejected admission never
// consumes a token, so a concurrent request does not reduce the node's rate
// budget while it is already being serviced.
func (a *nodeReportAdmission) admit(nodeID int64, now time.Time) (func(), time.Duration, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if nodeID <= 0 {
		return func() {}, time.Second, false
	}
	a.prune(now)
	entry := a.entries[nodeID]
	if entry == nil {
		entry = &nodeReportAdmissionEntry{tokens: nodeReportBurst, last: now, lastSeen: now}
		a.entries[nodeID] = entry
	}
	if entry.inFlight {
		entry.lastSeen = now
		return func() {}, time.Second, false
	}
	if entry.last.IsZero() {
		entry.last = now
	}
	elapsed := now.Sub(entry.last).Seconds()
	if elapsed > 0 {
		entry.tokens += elapsed * nodeReportRatePerSecond
		if entry.tokens > nodeReportBurst {
			entry.tokens = nodeReportBurst
		}
		entry.last = now
	}
	entry.lastSeen = now
	if entry.tokens < 1 {
		wait := time.Duration((1 - entry.tokens) / nodeReportRatePerSecond * float64(time.Second))
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		return func() {}, wait, false
	}
	entry.tokens--
	entry.inFlight = true
	return func() {
		a.mu.Lock()
		if current := a.entries[nodeID]; current != nil {
			current.inFlight = false
			current.lastSeen = time.Now()
		}
		a.mu.Unlock()
	}, 0, true
}

func (a *App) agentReports() *nodeReportAdmission {
	if a == nil {
		return newNodeReportAdmission()
	}
	a.agentReportMu.Lock()
	defer a.agentReportMu.Unlock()
	if a.agentReportLimiter == nil {
		a.agentReportLimiter = newNodeReportAdmission()
	}
	return a.agentReportLimiter
}

func (a *App) agentPreAuthAdmission() *agentPreAuthAdmission {
	if a == nil {
		return newAgentPreAuthAdmission()
	}
	a.agentPreAuthMu.Lock()
	defer a.agentPreAuthMu.Unlock()
	if a.agentPreAuth == nil {
		a.agentPreAuth = newAgentPreAuthAdmission()
	}
	return a.agentPreAuth
}

// withAgentPreAuth is shared by every credential-bearing Agent endpoint. It
// runs before endpoint-specific parsing and covers the credential lookup so
// invalid credentials cannot rotate between URLs to evade the admission
// budget. The slot is released before endpoint work begins.
func (a *App) withAgentPreAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		release, retryAfter, admitted := a.agentPreAuthAdmission().admit(requestClientKey(r, a.trustedProxies), time.Now())
		if !admitted {
			seconds := int(math.Ceil(retryAfter.Seconds()))
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(seconds))
			a.jsonErr(w, http.StatusTooManyRequests, "agent authentication rate limit exceeded")
			return
		}
		// A few unit-test embedders use this admission helper without a DB. Keep
		// that lightweight behavior, while the production router always has a DB
		// and authenticates before handing control to the endpoint.
		if a == nil || a.db == nil || a.db.db == nil {
			release()
			next(w, r)
			return
		}
		identity, err := a.authenticateAgentRequest(r)
		release()
		if err != nil {
			writeAgentAuthenticationError(a, w, r, err)
			return
		}
		next(w, withAgentCredential(r, identity))
	}
}

// writeAgentAuthFailure keeps ordinary HTTP authentication semantics while
// giving an already-enrolled Agent an explicit revocation signal. A stale or
// replaced Agent token must stop its data-plane listener; a missing bearer
// token remains a normal 401 for callers that have not authenticated.
func writeAgentAuthFailure(a *App, w http.ResponseWriter, r *http.Request) {
	if requestBearerToken(r) != "" {
		w.Header().Set("X-Meridian-Agent-State", "revoked")
	}
	a.jsonErr(w, http.StatusUnauthorized, "invalid agent token")
}

// writeAgentAuthenticationError deliberately reserves the Agent revocation
// signal for a credential that was conclusively rejected. A transient SQLite
// or filesystem failure must remain retryable: an Agent persists `revoked`
// and would otherwise take itself permanently offline after one failed lookup.
func writeAgentAuthenticationError(a *App, w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, errInvalidAgentToken) || errors.Is(err, errInvalidNodeToken) {
		writeAgentAuthFailure(a, w, r)
		return
	}
	a.jsonErr(w, http.StatusServiceUnavailable, "agent authentication temporarily unavailable")
}

// authenticateAgentRequest performs the only credential lookups protected by
// the global pre-auth concurrency budget. Enrollment credentials are retained
// because the binary, manifest, and enroll endpoints intentionally accept the
// one-time enrollment token.
func (a *App) authenticateAgentRequest(r *http.Request) (agentCredentialIdentity, error) {
	if a == nil || a.db == nil || a.db.db == nil {
		return agentCredentialIdentity{}, errInvalidAgentToken
	}
	token := requestBearerToken(r)
	if token == "" {
		return agentCredentialIdentity{}, errInvalidAgentToken
	}
	now := time.Now()
	if node, err := a.db.nodeByAgentToken(token, now); err == nil {
		return agentCredentialIdentity{Token: token, Node: node, HasNode: true}, nil
	} else if !errors.Is(err, errInvalidAgentToken) {
		return agentCredentialIdentity{}, err
	}
	if err := a.db.AuthorizeEnrollmentToken(token, now); err == nil {
		return agentCredentialIdentity{Token: token, Enrollment: true}, nil
	} else if !errors.Is(err, errInvalidNodeToken) {
		return agentCredentialIdentity{}, err
	}
	return agentCredentialIdentity{}, errInvalidAgentToken
}

func agentIdentityForRequest(a *App, r *http.Request) (agentCredentialIdentity, error) {
	if identity, ok := agentCredentialFromContext(r.Context()); ok {
		return identity, nil
	}
	return a.authenticateAgentRequest(r)
}

func agentNodeIdentityForRequest(a *App, r *http.Request) (agentCredentialIdentity, error) {
	identity, err := agentIdentityForRequest(a, r)
	if err != nil {
		return agentCredentialIdentity{}, err
	}
	if !identity.HasNode || identity.Enrollment {
		return agentCredentialIdentity{}, errInvalidAgentToken
	}
	return identity, nil
}
