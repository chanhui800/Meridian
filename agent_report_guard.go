package main

import (
	"container/list"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

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
// runs before endpoint-specific parsing or SQLite authentication so invalid
// credentials cannot rotate between URLs to evade the admission budget.
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
		defer release()
		next(w, r)
	}
}
