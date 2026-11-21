package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newJournalTestStore(t testing.TB) *edgeEventStore {
	t.Helper()
	s := &edgeEventStore{}
	if err := s.initWithRawKey(t.TempDir(), bytes.Repeat([]byte{0x52}, 32), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.journal.ioMu.Lock(); s.journal.syncFile = nil; s.journal.ioMu.Unlock(); _ = s.close() })
	return s
}

func journalTestEvent(critical bool, bodyBytes int) NodeRequestEvent {
	priority := nodeEventPriorityBestEffort
	if critical {
		priority = nodeEventPriorityCritical
	}
	return NodeRequestEvent{SiteID: 1, Host: "media.example.test", Method: "GET", Path: "/Items/1", StatusCode: 200, RecordedAtMS: 1, Priority: priority, ResponseBody: strings.Repeat("x", bodyBytes)}
}

func legacyBudgetReference(report NodeReport, events []NodeRequestEvent) []NodeRequestEvent {
	if len(events) > maxNodeRequestEventsPerReport {
		events = events[:maxNodeRequestEventsPerReport]
	}
	var selected []NodeRequestEvent
	for _, event := range events {
		candidate := append(append([]NodeRequestEvent(nil), selected...), event)
		report.Events = candidate
		encoded, err := json.Marshal(report)
		if err != nil || len(encoded) > targetAgentReportBytes {
			break
		}
		selected = candidate
	}
	return selected
}

func TestLinearEventBudgetExactJSONBoundaries(t *testing.T) {
	for _, body := range []string{"", `null`, `{}","a":"<>\n\u2028`, strings.Repeat("界", 2048), strings.Repeat("x", 8<<10), strings.Repeat("x", 64<<10)} {
		for _, count := range []int{0, 1, 2, 128, 129} {
			events := make([]NodeRequestEvent, count)
			for i := range events {
				events[i] = journalTestEvent(false, 0)
				events[i].EventID = int64(i + 1)
				events[i].ResponseBody = body
			}
			for _, report := range []NodeReport{{}, {Events: []NodeRequestEvent{{EventID: 999}}, SiteStats: []NodeSiteStat{}, MediaCounts: []NodeMediaCount{}, Retention: []NodeRetentionStatus{}}, {SiteStats: []NodeSiteStat{{Host: "escaped<>&\u2028"}}}} {
				want := legacyBudgetReference(report, events)
				got := edgeReportEventsByBudget(report, events)
				if len(want) != len(got) {
					t.Fatalf("count=%d body=%d: got %d want %d", count, len(body), len(got), len(want))
				}
				for i := range got {
					if got[i].EventID != want[i].EventID {
						t.Fatal("event order changed")
					}
				}
			}
		}
	}
	// Exercise equality and one byte either side of the exact encoded limit.
	for _, offset := range []int{-1, 0, 1} {
		event := journalTestEvent(false, 0)
		base := NodeReport{}
		base.Events = []NodeRequestEvent{event}
		data, _ := json.Marshal(base)
		event.Body = strings.Repeat("x", targetAgentReportBytes-len(data)-len(`,"body":""`)+offset)
		base.Events = []NodeRequestEvent{event}
		encoded, _ := json.Marshal(base)
		got := edgeReportEventsByBudget(NodeReport{}, []NodeRequestEvent{event})
		if len(got) == 1 != (len(encoded) <= targetAgentReportBytes) {
			t.Fatalf("encoded=%d selected=%d", len(encoded), len(got))
		}
	}
}

func TestJournalCriticalAcceptanceWaitsForDurableSync(t *testing.T) {
	s := newJournalTestStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	s.journal.ioMu.Lock()
	s.journal.syncFile = func(f *os.File) error { close(entered); <-release; return f.Sync() }
	s.journal.ioMu.Unlock()
	done := make(chan error, 1)
	go func() { done <- s.add(journalTestEvent(true, 1024)) }()
	<-entered
	select {
	case err := <-done:
		t.Fatalf("critical accepted before durable sync: %v", err)
	default:
	}
	if len(s.snapshot()) != 0 {
		t.Fatal("undurable critical event was reportable")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s.journal.ioMu.Lock()
	s.journal.syncFile = nil
	s.journal.ioMu.Unlock()
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	var restored edgeEventStore
	if err := restored.initWithRawKey(filepath.Dir(s.path), s.key, nil); err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	if len(restored.snapshot()) != 1 {
		t.Fatal("accepted critical event did not survive restart")
	}
}

func TestJournalBestEffortCoalescesWithoutRequestSync(t *testing.T) {
	s := newJournalTestStore(t)
	var syncs atomic.Int64
	s.journal.ioMu.Lock()
	s.journal.syncFile = func(f *os.File) error { syncs.Add(1); return f.Sync() }
	s.journal.ioMu.Unlock()
	for i := 0; i < 20; i++ {
		if err := s.add(journalTestEvent(false, 1024)); err != nil {
			t.Fatal(err)
		}
	}
	if syncs.Load() != 0 {
		t.Fatal("small best-effort requests performed synchronous fsync")
	}
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	if syncs.Load() != 1 {
		t.Fatalf("coalesced syncs=%d", syncs.Load())
	}
	if len(s.snapshot()) != 20 {
		t.Fatal("durable best-effort events disappeared")
	}
}

func TestJournalPreservesBoundedMetadataWithJSONEscapes(t *testing.T) {
	s := newJournalTestStore(t)
	event := journalTestEvent(true, 0)
	event.ResponseBody = strings.Repeat("<", maxNodeRequestEventResponseBodyBytes)
	if err := s.add(event); err != nil {
		t.Fatalf("valid bounded metadata rejected after JSON escaping: %v", err)
	}
	page := s.snapshot()
	if len(page) != 1 || page[0].ResponseBody != event.ResponseBody {
		t.Fatal("escaped critical metadata changed")
	}
}

func TestJournalFailedACKRetainsIdentityAndBacksOff(t *testing.T) {
	s := newJournalTestStore(t)
	if err := s.add(journalTestEvent(true, 1024)); err != nil {
		t.Fatal(err)
	}
	old := s.snapshot()
	var attempts atomic.Int64
	s.journal.ioMu.Lock()
	s.journal.syncFile = func(*os.File) error { attempts.Add(1); return errors.New("injected full disk") }
	s.journal.ioMu.Unlock()
	if err := s.ack(old); err == nil {
		t.Fatal("failed ACK falsely succeeded")
	}
	if got := s.snapshot(); len(got) != 1 || got[0].EventUID != old[0].EventUID {
		t.Fatal("failed ACK discarded or changed the retry identity")
	}
	for i := 0; i < 50; i++ {
		if err := s.add(journalTestEvent(true, 1024)); err == nil {
			t.Fatal("backoff falsely confirmed new critical event")
		}
	}
	if attempts.Load() != 1 {
		t.Fatalf("disk failure caused per-request retries: %d", attempts.Load())
	}
	s.journal.ioMu.Lock()
	s.journal.syncFile = nil
	s.journal.ioMu.Unlock()
	if err := s.flush(); err != nil {
		t.Fatal(err)
	}
	if s.depth() != 0 {
		t.Fatal("durable retry did not complete the pending ACK")
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	var restored edgeEventStore
	if err := restored.initWithRawKey(filepath.Dir(s.path), s.key, nil); err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	if restored.depth() != 0 {
		t.Fatal("ACKed event resurrected after restart")
	}
}

func TestJournalConcurrentAddACKCloseAndFlush(t *testing.T) {
	s := newJournalTestStore(t)
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Go(func() {
			for n := 0; n < 20; n++ {
				err := s.add(journalTestEvent(true, 100))
				if err != nil && !errors.Is(err, errEdgeEventStoreClosed) {
					t.Errorf("add: %v", err)
				}
				snapshot := s.snapshot()
				if len(snapshot) > 0 {
					err = s.ack(snapshot)
					if err != nil && !errors.Is(err, errEdgeEventStoreClosed) {
						t.Errorf("ack: %v", err)
					}
				}
			}
		})
	}
	workers.Wait()
	var closes sync.WaitGroup
	for i := 0; i < 8; i++ {
		closes.Go(func() {
			if err := s.close(); err != nil {
				t.Errorf("close: %v", err)
			}
		})
	}
	closes.Wait()
	done := make(chan error, 1)
	go func() { done <- s.flush() }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("flush after close hung")
	}
	if err := s.add(journalTestEvent(true, 0)); !errors.Is(err, errEdgeEventStoreClosed) {
		t.Fatalf("add after close: %v", err)
	}
}

func TestJournalCloseWhileCriticalWriteIsPending(t *testing.T) {
	s := newJournalTestStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.journal.ioMu.Lock()
	s.journal.syncFile = func(f *os.File) error { once.Do(func() { close(entered); <-release }); return f.Sync() }
	s.journal.ioMu.Unlock()
	add := make(chan error, 1)
	go func() { add <- s.add(journalTestEvent(true, 1024)) }()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- s.close() }()
	close(release)
	select {
	case err := <-add:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending critical add hung during close")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("close hung behind pending critical add")
	}
	var restored edgeEventStore
	if err := restored.initWithRawKey(filepath.Dir(s.path), s.key, nil); err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	if restored.depth() != 1 {
		t.Fatal("concurrent close lost durable accepted event")
	}
}

func TestJournalPendingCapacityDoesNotEvictBeforeReject(t *testing.T) {
	s := &edgeEventStore{}
	for i := 0; i < edgeEventQueueLimit; i++ {
		if err := s.add(journalTestEvent(false, 0)); err != nil {
			t.Fatal(err)
		}
	}
	firstID, oldDropped := s.items[0].EventID, s.droppedCount()
	s.pendingBytes = edgeEventQueueBytes
	if err := s.add(journalTestEvent(true, 0)); !errors.Is(err, errEdgeEventCapacity) {
		t.Fatalf("critical capacity=%v", err)
	}
	if s.items[0].EventID != firstID || s.droppedCount() != oldDropped || s.depth() != edgeEventQueueLimit {
		t.Fatal("rejected add evicted an event without a journal removal")
	}
}

func writeLegacyJournalFixture(t testing.TB, dir string, count, body int, critical bool) {
	t.Helper()
	file, err := os.Create(filepath.Join(dir, "events.json"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(file, `{"next":%d,"dropped":7,"items":[`, count)
	event := journalTestEvent(critical, body)
	for i := 0; i < count; i++ {
		event.EventID = int64(i + 1)
		event.EventUID = fmt.Sprintf("%032x", i+1)
		if i > 0 {
			file.WriteString(",")
		}
		encoded, _ := json.Marshal(event)
		if _, err := file.Write(encoded); err != nil {
			t.Fatal(err)
		}
	}
	file.WriteString("]}")
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestJournalLegacyCriticalBacklogSpillsWithoutDropping(t *testing.T) {
	dir := t.TempDir()
	writeLegacyJournalFixture(t, dir, 300, 60<<10, true)
	var s edgeEventStore
	if err := s.initWithRawKey(dir, bytes.Repeat([]byte{0x32}, 32), nil); err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if s.depth() != 300 || s.droppedCount() != 7 || len(s.spilled) == 0 {
		t.Fatalf("critical migration: depth=%d dropped=%d spilled=%d", s.depth(), s.droppedCount(), len(s.spilled))
	}
	if err := s.add(journalTestEvent(true, 0)); !errors.Is(err, errEdgeEventCapacity) {
		t.Fatal("over-cap critical backlog falsely accepted new events")
	}
	seen := map[string]bool{}
	for s.depth() > 0 {
		page := s.snapshot()
		if len(page) == 0 {
			t.Fatal("disk backlog cannot drain")
		}
		for _, e := range page {
			if len(e.ResponseBody) != 60<<10 || seen[e.EventUID] {
				t.Fatal("spilled event changed or replayed after ACK")
			}
			seen[e.EventUID] = true
		}
		if err := s.ack(page); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 300 {
		t.Fatal("critical legacy events lost")
	}
}

func TestJournalSpilledSnapshotsStayAtomicWithACK(t *testing.T) {
	dir := t.TempDir()
	writeLegacyJournalFixture(t, dir, 300, 60<<10, true)
	var s edgeEventStore
	if err := s.initWithRawKey(dir, bytes.Repeat([]byte{0x32}, 32), nil); err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if len(s.spilled) == 0 {
		t.Fatal("fixture did not create disk-backed events")
	}
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 4; i++ {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, event := range s.snapshot() {
					if len(event.ResponseBody) != 60<<10 {
						t.Error("concurrent snapshot observed an invalid or partially removed reference")
						return
					}
				}
				runtime.Gosched()
			}
		})
	}
	for s.depth() > 0 {
		page := s.snapshot()
		if len(page) == 0 {
			close(stop)
			readers.Wait()
			t.Fatal("spilled queue stopped draining")
		}
		if err := s.ack(page); err != nil {
			close(stop)
			readers.Wait()
			t.Fatal(err)
		}
	}
	close(stop)
	readers.Wait()
	if err := s.persistenceError(); err != nil {
		t.Fatalf("spilled snapshot raced ACK publication: %v", err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	var restored edgeEventStore
	if err := restored.initWithRawKey(dir, s.key, nil); err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	if restored.depth() != 0 {
		t.Fatal("concurrently ACKed disk-backed events resurrected")
	}
}

func TestJournalInvalidReferenceReturnsError(t *testing.T) {
	s := newJournalTestStore(t)
	s.journal.ioMu.Lock()
	defer s.journal.ioMu.Unlock()
	if _, err := s.journal.readEvent(edgeJournalReference{}); err == nil {
		t.Fatal("invalid zero reference accepted")
	}
}

func TestJournalLegacyBestEffortByteCapAndDroppedCounter(t *testing.T) {
	dir := t.TempDir()
	writeLegacyJournalFixture(t, dir, 400, 60<<10, false)
	var s edgeEventStore
	if err := s.initWithRawKey(dir, bytes.Repeat([]byte{0x32}, 32), nil); err != nil {
		t.Fatal(err)
	}
	defer s.close()
	if s.bytes > edgeEventQueueBytes || s.depth() >= 400 || s.droppedCount() != 7+int64(400-s.depth()) {
		t.Fatalf("migration bytes=%d depth=%d dropped=%d", s.bytes, s.depth(), s.droppedCount())
	}
}

func TestJournalIntegrityAndIncompleteTailRecovery(t *testing.T) {
	s := newJournalTestStore(t)
	for i := 0; i < 3; i++ {
		if err := s.add(journalTestEvent(true, 1024)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	path := s.journal.path
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"tail", "header", "ciphertext", "reorder"} {
		t.Run(mode, func(t *testing.T) {
			data := append([]byte(nil), original...)
			switch mode {
			case "tail":
				var header [8]byte
				binary.BigEndian.PutUint32(header[:], 100)
				binary.BigEndian.PutUint32(header[4:], ^uint32(100))
				data = append(data, header[:]...)
				data = append(data, 1, 2, 3)
			case "header":
				data[len(edgeJournalMagic)] ^= 1
			case "ciphertext":
				data[len(edgeJournalMagic)+20] ^= 1
			case "reorder":
				n := 8 + int(binary.BigEndian.Uint32(data[len(edgeJournalMagic):]))
				copy(data[len(edgeJournalMagic):len(edgeJournalMagic)+n], original[len(edgeJournalMagic)+n:len(edgeJournalMagic)+2*n])
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			var restored edgeEventStore
			err := restored.initWithRawKey(filepath.Dir(s.path), s.key, nil)
			if mode == "tail" {
				if err != nil {
					t.Fatal(err)
				}
				if restored.depth() != 3 {
					t.Fatal("tail recovery lost committed events")
				}
				if err := restored.close(); err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					restored.close()
					t.Fatal("interior journal corruption silently accepted")
				}
				after, _ := os.ReadFile(path)
				if !bytes.Equal(data, after) {
					t.Fatal("corrupt interior was truncated/rewritten")
				}
			}
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestJournalCompactionKeepsACKHeadroomAndRekeys(t *testing.T) {
	s := newJournalTestStore(t)
	if err := s.add(journalTestEvent(true, 1024)); err != nil {
		t.Fatal(err)
	}
	page := s.snapshot()
	s.journal.ioMu.Lock()
	if err := s.journal.file.Truncate(edgeJournalMaxBytes - (512 << 10)); err != nil {
		t.Fatal(err)
	}
	s.journal.offset = edgeJournalMaxBytes - (512 << 10)
	s.journal.ioMu.Unlock()
	if err := s.ack(page); err != nil {
		t.Fatalf("full journal prevented ACK/drain: %v", err)
	}
	if err := s.add(journalTestEvent(true, 100)); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	newKey := bytes.Repeat([]byte{0x73}, 32)
	var rekeyed edgeEventStore
	if err := rekeyed.initWithRawKey(filepath.Dir(s.path), newKey, s.key); err != nil {
		t.Fatal(err)
	}
	if len(rekeyed.snapshot()) != 1 {
		t.Fatal("rekey lost durable event")
	}
	if err := rekeyed.close(); err != nil {
		t.Fatal(err)
	}
	var restored edgeEventStore
	if err := restored.initWithRawKey(filepath.Dir(s.path), newKey, nil); err != nil {
		t.Fatal(err)
	}
	defer restored.close()
	if restored.depth() != 1 {
		t.Fatal("rekeyed journal did not use the independent key")
	}
}

func TestJournalControlledRollbackAndForwardImport(t *testing.T) {
	s := newJournalTestStore(t)
	if err := s.add(journalTestEvent(true, 100)); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	var old edgeEventStore
	old.key = s.key
	old.sizes = map[int64]int{}
	old.ready = map[int64]bool{}
	old.spilled = map[int64]bool{}
	if err := old.loadLegacy(data); err != nil {
		t.Fatal(err)
	}
	if len(old.items) != 1 {
		t.Fatal("pre6 rollback could not read durable queue")
	}
	// Simulate pre6 acknowledging that event and persisting a new critical one.
	event := journalTestEvent(true, 100)
	event.EventID = 2
	event.EventUID = strings.Repeat("a", 32)
	payload, _ := json.Marshal(struct {
		Next    int64              `json:"next"`
		Items   []NodeRequestEvent `json:"items"`
		Dropped int64              `json:"dropped"`
	}{2, []NodeRequestEvent{event}, 0})
	if err := os.WriteFile(s.path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	var forward edgeEventStore
	if err := forward.initWithRawKey(filepath.Dir(s.path), s.key, nil); err != nil {
		t.Fatal(err)
	}
	defer forward.close()
	page := forward.snapshot()
	if len(page) != 1 || page[0].EventUID != event.EventUID {
		t.Fatal("forward upgrade replayed stale journal instead of importing pre6 queue")
	}
}

func TestJournalLegacyReadLimitPreservesOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(edgeLegacySpoolReadBytes + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	var s edgeEventStore
	if err := s.initWithRawKey(dir, bytes.Repeat([]byte{1}, 32), nil); err == nil {
		t.Fatal("oversize legacy spool accepted")
	}
	stat, err := os.Stat(path)
	if err != nil || stat.Size() != edgeLegacySpoolReadBytes+1 {
		t.Fatal("oversize legacy spool changed")
	}
	if _, err := os.Stat(filepath.Join(dir, "events.journal")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("oversize legacy spool was silently replaced")
	}
}

func TestJournalLegacyEmptyNullAndArray(t *testing.T) {
	for _, items := range []string{"null", "[]"} {
		t.Run(items, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "events.json"), []byte(`{"next":55,"items":`+items+`,"dropped":9}`), 0o600); err != nil {
				t.Fatal(err)
			}
			var s edgeEventStore
			if err := s.initWithRawKey(dir, bytes.Repeat([]byte{0x12}, 32), nil); err != nil {
				t.Fatal(err)
			}
			defer s.close()
			if s.depth() != 0 || s.next != 55 || s.droppedCount() != 9 {
				t.Fatal("empty legacy queue metadata changed")
			}
		})
	}
}

func TestJournalRuntimeUpdateExportFailureKeepsPreviousBinary(t *testing.T) {
	rt := &edgeAgentRuntime{stateDir: t.TempDir()}
	if err := rt.events.initWithRawKey(rt.stateDir, bytes.Repeat([]byte{3}, 32), nil); err != nil {
		t.Fatal(err)
	}
	defer rt.close()
	if err := rt.events.add(journalTestEvent(true, 100)); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(rt.stateDir, "not-a-directory")
	if err := os.WriteFile(blocked, []byte("block export"), 0o600); err != nil {
		t.Fatal(err)
	}
	rt.events.path = filepath.Join(blocked, "events.json")
	executable := filepath.Join(t.TempDir(), "agent")
	temporary := executable + ".download"
	os.WriteFile(executable, []byte("previous"), 0o600)
	os.WriteFile(temporary, []byte("replacement"), 0o600)
	update := &edgeAgentUpdate{executable: executable, temporary: temporary}
	if err := commitEdgeAgentRuntimeUpdate(context.Background(), rt, update); err == nil || errors.Is(err, errEdgeAgentUpdated) || !strings.Contains(err.Error(), "backward-compatible event spool") {
		t.Fatal("replacement committed without a rollback-readable queue")
	}
	if data, _ := os.ReadFile(executable); string(data) != "previous" {
		t.Fatal("export failure replaced previous binary")
	}
	if _, err := os.Stat(rt.events.journal.path); err != nil {
		t.Fatal("export failure lost journal")
	}
	if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed update kept staged executable")
	}
}

func TestJournalRuntimeUpdateExportsBeforeReplacingBinary(t *testing.T) {
	rt := &edgeAgentRuntime{stateDir: t.TempDir()}
	if err := rt.events.initWithRawKey(rt.stateDir, bytes.Repeat([]byte{3}, 32), nil); err != nil {
		t.Fatal(err)
	}
	defer rt.close()
	if err := rt.events.add(journalTestEvent(true, 100)); err != nil {
		t.Fatal(err)
	}
	want := rt.events.snapshot()[0].EventUID
	executable := filepath.Join(t.TempDir(), "agent")
	temporary := executable + ".download"
	os.WriteFile(executable, []byte("previous"), 0o600)
	os.WriteFile(temporary, []byte("replacement"), 0o600)
	checked := false
	update := &edgeAgentUpdate{executable: executable, temporary: temporary, syncParent: func(string) error {
		data, err := os.ReadFile(rt.events.path)
		if err != nil {
			t.Error(err)
			return err
		}
		var old edgeEventStore
		old.key = rt.events.key
		old.sizes = map[int64]int{}
		old.ready = map[int64]bool{}
		old.spilled = map[int64]bool{}
		if err := old.loadLegacy(data); err != nil {
			t.Error(err)
			return err
		}
		if len(old.items) != 1 || old.items[0].EventUID != want {
			t.Error("legacy queue was stale at executable commit")
		}
		checked = true
		return nil
	}}
	if err := commitEdgeAgentRuntimeUpdate(context.Background(), rt, update); !errors.Is(err, errEdgeAgentUpdated) {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("commit bypassed export boundary")
	}
	select {
	case <-rt.events.journal.done:
	default:
		t.Fatal("new executable committed while old writer still alive")
	}
}

func TestJournalStaleLeaseQuiesceKeepsOneWriterForRecovery(t *testing.T) {
	rt := &edgeAgentRuntime{stateDir: t.TempDir()}
	if err := rt.events.initWithRawKey(rt.stateDir, bytes.Repeat([]byte{3}, 32), nil); err != nil {
		t.Fatal(err)
	}
	defer rt.close()
	writer := rt.events.journal
	if err := rt.quiesce(false); err != nil {
		t.Fatal(err)
	}
	if err := rt.events.add(journalTestEvent(true, 100)); err != nil {
		t.Fatalf("recoverable stale lease permanently closed spool: %v", err)
	}
	if rt.events.journal != writer {
		t.Fatal("stale recovery started another writer")
	}
}

func BenchmarkLinearEventBudget(b *testing.B) {
	events := make([]NodeRequestEvent, 128)
	for i := range events {
		events[i] = journalTestEvent(false, 8<<10)
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		edgeReportEventsByBudget(NodeReport{}, events)
	}
}

func BenchmarkJournalLargeLegacyQueueRequest(b *testing.B) {
	dir := b.TempDir()
	writeLegacyJournalFixture(b, dir, 8192, 8<<10, false)
	var s edgeEventStore
	if err := s.initWithRawKey(dir, bytes.Repeat([]byte{0x32}, 32), nil); err != nil {
		b.Fatal(err)
	}
	defer s.close()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.add(journalTestEvent(true, 100)); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	runtime.ReadMemStats(&after)
	b.Logf("resident_heap_before=%d after=%d migrated_depth=%d dropped=%d bytes=%d", before.HeapAlloc, after.HeapAlloc, s.depth(), s.droppedCount(), s.bytes)
}
