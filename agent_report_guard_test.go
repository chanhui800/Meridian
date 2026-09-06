package main

import (
	"sync"
	"testing"
	"time"
)

func TestNodeReportAdmissionLimitsBurstAndRefills(t *testing.T) {
	guard := newNodeReportAdmission()
	now := time.Unix(100, 0)
	for i := 0; i < int(nodeReportBurst); i++ {
		release, _, ok := guard.admit(7, now)
		if !ok {
			t.Fatalf("burst admission %d rejected", i)
		}
		release()
	}
	if release, _, ok := guard.admit(7, now); ok {
		release()
		t.Fatal("report beyond burst was admitted")
	}
	release, _, ok := guard.admit(7, now.Add(time.Second))
	if !ok {
		t.Fatal("one report should be admitted after one second")
	}
	release()
}

func TestNodeReportAdmissionSerializesOneReportPerNode(t *testing.T) {
	guard := newNodeReportAdmission()
	now := time.Unix(200, 0)
	release, _, ok := guard.admit(9, now)
	if !ok {
		t.Fatal("first report rejected")
	}
	if secondRelease, _, secondOK := guard.admit(9, now); secondOK {
		secondRelease()
		t.Fatal("concurrent report for one node was admitted")
	}
	release()
	if nextRelease, _, nextOK := guard.admit(9, now); !nextOK {
		t.Fatal("report should be admitted after release")
	} else {
		nextRelease()
	}
}

func TestNodeReportAdmissionKeepsNodesIndependentAndPrunesIdleEntries(t *testing.T) {
	guard := newNodeReportAdmission()
	now := time.Now()
	releases := make([]func(), 0, 2)
	for _, nodeID := range []int64{1, 2} {
		release, _, ok := guard.admit(nodeID, now)
		if !ok {
			t.Fatalf("node %d rejected", nodeID)
		}
		releases = append(releases, release)
	}
	for _, release := range releases {
		release()
	}
	guard.mu.Lock()
	guard.prune(now.Add(nodeReportEntryTTL + time.Second))
	guard.mu.Unlock()
	if len(guard.entries) != 0 {
		t.Fatalf("idle limiter entries remain: %d", len(guard.entries))
	}
}

func TestNodeReportAdmissionIsSafeForConcurrentNodes(t *testing.T) {
	guard := newNodeReportAdmission()
	now := time.Now()
	var wg sync.WaitGroup
	for i := int64(1); i <= 32; i++ {
		wg.Add(1)
		go func(nodeID int64) {
			defer wg.Done()
			release, _, ok := guard.admit(nodeID, now)
			if ok {
				release()
			}
		}(i)
	}
	wg.Wait()
}
