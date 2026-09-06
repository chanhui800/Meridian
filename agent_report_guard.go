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
