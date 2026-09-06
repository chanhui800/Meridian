package main

import (
	"testing"
	"time"
)

func TestAgentSecurityAuditStateIsBoundedByNodeAndCategory(t *testing.T) {
	db := &DB{}
	for siteID := int64(1); siteID <= 100000; siteID++ {
		db.recordAgentSecurityRejection(7, siteID, "request-event")
	}
	db.agentSecurityMu.Lock()
	entries := len(db.agentSecurityLastLog)
	db.agentSecurityMu.Unlock()
	if entries != 1 {
		t.Fatalf("security audit limiter retained %d site-specific entries, want 1", entries)
	}
	if got := db.agentSecurityRejected.Load(); got != 100000 {
		t.Fatalf("rejection counter=%d, want 100000", got)
	}
}

func TestAgentPreAuthAdmissionRateAndConcurrency(t *testing.T) {
	admission := newAgentPreAuthAdmission()
	now := time.Now()
	releases := make([]func(), 0, agentPreAuthConcurrency)
	for i := 0; i < agentPreAuthConcurrency; i++ {
		release, _, ok := admission.admit("client-"+string(rune('a'+i)), now)
		if !ok {
			t.Fatalf("pre-auth admission %d was rejected before concurrency ceiling", i)
		}
		releases = append(releases, release)
	}
	if _, _, ok := admission.admit("other-client", now); ok {
		t.Fatal("pre-auth concurrency ceiling was bypassed")
	}
	for _, release := range releases {
		release()
	}
	// A separate client has its own token bucket and is admitted immediately;
	// a single noisy client is rate limited without affecting it.
	for i := 0; i < agentPreAuthBurst; i++ {
		release, _, ok := admission.admit("noisy", now)
		if !ok {
			t.Fatalf("burst admission %d was rejected", i)
		}
		release()
	}
	if _, _, ok := admission.admit("noisy", now); ok {
		t.Fatal("pre-auth token bucket did not reject an exhausted client")
	}
	if release, _, ok := admission.admit("independent", now); !ok {
		t.Fatal("one client's pre-auth limiter affected another client")
	} else {
		release()
	}
}

func TestNodeReportAdmissionReclaimsIdleEntries(t *testing.T) {
	admission := newNodeReportAdmission()
	now := time.Now()
	release, _, ok := admission.admit(42, now)
	if !ok {
		t.Fatal("node report was rejected")
	}
	release()
	admission.mu.Lock()
	admission.prune(now.Add(nodeReportEntryTTL + time.Second))
	_, exists := admission.entries[42]
	admission.mu.Unlock()
	if exists {
		t.Fatal("idle node report limiter entry was not reclaimed")
	}
}
