package main

import (
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
	agentPreAuthRatePerSecond = 10.0
	agentPreAuthBurst         = 20.0
	agentPreAuthEntryTTL      = 15 * time.Minute
	agentPreAuthConcurrency   = 32
)

type agentPreAuthEntry struct {
	tokens   float64
	last     time.Time
	lastSeen time.Time
}

// agentPreAuthAdmission is deliberately keyed by the trusted-proxy-aware
// client identity rather than an Agent token. It runs before the SQLite token
// lookup so a stream of invalid credentials cannot monopolize the sole DB
// connection.
type agentPreAuthAdmission struct {
	mu      sync.Mutex
	entries map[string]*agentPreAuthEntry
	active  int
}

func newAgentPreAuthAdmission() *agentPreAuthAdmission {
	return &agentPreAuthAdmission{entries: make(map[string]*agentPreAuthEntry)}
}

func (a *agentPreAuthAdmission) prune(now time.Time) {
	for key, entry := range a.entries {
		if now.Sub(entry.lastSeen) > agentPreAuthEntryTTL {
			delete(a.entries, key)
		}
	}
}

func (a *agentPreAuthAdmission) admit(key string, now time.Time) (func(), time.Duration, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if key == "" {
		key = "unknown"
	}
	a.prune(now)
	if a.active >= agentPreAuthConcurrency {
		return func() {}, time.Second, false
	}
	entry := a.entries[key]
	if entry == nil {
		entry = &agentPreAuthEntry{tokens: agentPreAuthBurst, last: now, lastSeen: now}
		a.entries[key] = entry
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
		wait := time.Duration((1 - entry.tokens) / agentPreAuthRatePerSecond * float64(time.Second))
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		return func() {}, wait, false
	}
	entry.tokens--
	a.active++
	return func() {
		a.mu.Lock()
		if a.active > 0 {
			a.active--
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
