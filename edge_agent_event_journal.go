package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	edgeEventQueueBytes        = 16 << 20
	edgeJournalFrameBytes      = 1 << 20 // includes worst-case JSON escaping of bounded metadata bodies
	edgeJournalMaxBytes        = 256 << 20
	edgeJournalCompactBytes    = 32 << 20
	edgeLegacySpoolReadBytes   = 128 << 20
	edgeLegacySpoolExportBytes = 32 << 20
	edgeJournalMagic           = "MERIDIAN-EVENTS-2\n"
)

var (
	errEdgeEventStoreClosed = errors.New("event spool is closed")
	errEdgeEventCapacity    = errors.New("event spool byte or item capacity reached")
)

// mu protects the small queue/index and pending operations. Only the writer
// touches the journal. Neither request callers nor ACKs copy the whole payload.
type edgeEventStore struct {
	mu             sync.Mutex
	next           int64
	items          []NodeRequestEvent
	sizes          map[int64]int
	spilled        map[int64]bool
	ready          map[int64]bool
	bytes          int64
	path           string
	key, legacyKey []byte
	dropped        int64
	lastPersist    time.Time
	journal        *edgeEventJournal
	pending        []*edgeJournalOperation
	pendingBytes   int64
	lastErr        error
	retryAt        time.Time
	backoff        time.Duration
	force          bool
	closed         bool
	stopping       bool
	closeErr       error
	closeDone      chan struct{}
	dirty          bool
}

type edgeJournalOperation struct {
	Add     *NodeRequestEvent `json:"add,omitempty"`
	Remove  []int64           `json:"remove,omitempty"`
	Next    int64             `json:"next"`
	Dropped int64             `json:"dropped"`
	done    chan error
	bytes   int
}

type edgeJournalReference struct {
	offset   int64
	length   int
	previous [32]byte
	size     int
	event    NodeRequestEvent
}

type edgeEventJournal struct {
	store   *edgeEventStore
	path    string
	file    *os.File
	ioMu    sync.Mutex
	wake    chan struct{}
	done    chan struct{}
	refs    map[int64]edgeJournalReference
	offset  int64
	chain   [32]byte
	readKey []byte
	// Test injection is set before starting the worker. Production uses Sync.
	syncFile func(*os.File) error
}

func (s *edgeEventStore) init(dir string) error { return s.initWithKey(dir, nil) }

func (s *edgeEventStore) initWithKey(dir string, secret []byte) error {
	var key []byte
	if len(secret) > 0 {
		digest := sha256.Sum256(secret)
		key = digest[:]
	}
	return s.initWithRawKey(dir, key, nil)
}

func readEdgeSpoolBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path) // #nosec G304 -- caller supplies events.json under the administrator-selected private Agent state directory; file size and read bytes are bounded below.
	if err != nil {
		return nil, err
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if stat.Size() > limit {
		return nil, fmt.Errorf("event spool exceeds bounded migration size (%d bytes)", limit)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("event spool grew beyond bounded migration size")
	}
	return data, err
}

func (s *edgeEventStore) initWithRawKey(dir string, key, legacyKey []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.journal != nil {
		return errors.New("event spool already initialized")
	}
	s.path = filepath.Join(dir, "events.json")
	s.key, s.legacyKey = append([]byte(nil), key...), append([]byte(nil), legacyKey...)
	s.sizes, s.spilled, s.ready = make(map[int64]int), make(map[int64]bool), make(map[int64]bool)
	j := &edgeEventJournal{store: s, path: filepath.Join(dir, "events.journal"), wake: make(chan struct{}, 1), done: make(chan struct{}), refs: make(map[int64]edgeJournalReference)}
	j.readKey = append([]byte(nil), key...)
	legacy, legacyErr := readEdgeSpoolBounded(s.path, edgeLegacySpoolReadBytes)
	if legacyErr != nil && !errors.Is(legacyErr, os.ErrNotExist) {
		return legacyErr
	}
	legacyHash := sha256.Sum256(legacy)
	marker, markerErr := os.ReadFile(filepath.Join(dir, "events.legacy.sha256")) // #nosec G304 -- fixed marker name under the administrator-selected private Agent state directory, not a request-controlled path.
	_, journalErr := os.Stat(j.path)
	useLegacy := errors.Is(journalErr, os.ErrNotExist) || legacyErr == nil && (markerErr != nil || strings.TrimSpace(string(marker)) != hex.EncodeToString(legacyHash[:]))
	if journalErr != nil && !errors.Is(journalErr, os.ErrNotExist) {
		return journalErr
	}
	if useLegacy {
		if legacyErr == nil {
			if err := s.loadLegacy(legacy); err != nil {
				return err
			}
		}
		if err := j.createFromLegacy(); err != nil {
			return err
		}
		if err := writePrivateFileAtomic(filepath.Join(dir, "events.legacy.sha256"), []byte(hex.EncodeToString(legacyHash[:]))); err != nil {
			j.file.Close()
			return err
		}
	} else {
		if err := j.replay(); err != nil {
			if j.file != nil {
				j.file.Close()
			}
			return err
		}
		if !bytes.Equal(j.readKey, s.key) {
			if err := j.compactState(s.next, s.dropped); err != nil {
				j.file.Close()
				return err
			}
		}
	}
	s.journal = j
	go j.run()
	return nil
}

func (s *edgeEventStore) loadLegacy(data []byte) error {
	var envelope struct {
		Version    int    `json:"version"`
		Nonce      string `json:"nonce"`
		Ciphertext string `json:"ciphertext"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("load legacy event queue: %w", err)
	}
	if envelope.Version != 0 {
		opened, err := decryptEventSpoolEnvelope(envelope.Version, envelope.Nonce, envelope.Ciphertext, s.key)
		if err != nil && len(s.legacyKey) > 0 {
			opened, err = decryptEventSpoolEnvelope(envelope.Version, envelope.Nonce, envelope.Ciphertext, s.legacyKey)
		}
		if err != nil {
			return fmt.Errorf("decrypt legacy event queue: %w", err)
		}
		data = opened
	}
	var stored struct {
		Next    int64              `json:"next"`
		Items   []NodeRequestEvent `json:"items"`
		Dropped int64              `json:"dropped"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return errors.New("invalid legacy event queue object")
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token {
		case "next":
			if err := decoder.Decode(&stored.Next); err != nil {
				return err
			}
		case "dropped":
			if err := decoder.Decode(&stored.Dropped); err != nil {
				return err
			}
		case "items":
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			if token == nil {
				continue
			} // legacy empty queues encode a nil Items as null
			if token != json.Delim('[') {
				return errors.New("invalid legacy event array")
			}
			for decoder.More() {
				if len(stored.Items) >= edgeEventQueueLimit {
					return errors.New("legacy event queue exceeds bounded item count; original preserved")
				}
				var event NodeRequestEvent
				if err := decoder.Decode(&event); err != nil {
					return err
				}
				stored.Items = append(stored.Items, event)
			}
			if _, err := decoder.Token(); err != nil {
				return err
			}
		default:
			var ignored json.RawMessage
			if err := decoder.Decode(&ignored); err != nil {
				return err
			}
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("unexpected trailing legacy spool data")
	}
	s.next, s.dropped = stored.Next, stored.Dropped
	// Grandfather durable critical events above the new byte cap. They are
	// streamed to disk below, not evicted; new events wait for ACKs to drain them.
	criticalBytes := int64(0)
	for i := range stored.Items {
		e := &stored.Items[i]
		if e.EventID > s.next {
			s.next = e.EventID
		}
		if e.EventUID == "" {
			uid, err := newEdgeEventUID()
			if err != nil {
				return err
			}
			e.EventUID = uid
		}
		e.Priority = nodeEventPriority(*e)
		encoded, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if len(encoded) > edgeJournalFrameBytes-256 {
			return errors.New("legacy event exceeds journal frame limit; original spool preserved")
		}
		if e.Priority == nodeEventPriorityCritical {
			criticalBytes += int64(len(encoded))
		}
	}
	for _, e := range stored.Items {
		encoded, _ := json.Marshal(e)
		if e.Priority != nodeEventPriorityCritical && (len(s.items) >= edgeEventQueueLimit || criticalBytes+s.bytes+int64(len(encoded)) > edgeEventQueueBytes) {
			s.dropped++
			continue
		}
		if e.Priority == nodeEventPriorityCritical {
			criticalBytes -= int64(len(encoded))
		}
		s.items = append(s.items, e)
		s.sizes[e.EventID] = len(encoded)
		s.bytes += int64(len(encoded))
		s.ready[e.EventID] = true
	}
	return nil
}

func edgeJournalSeal(key, plain []byte, previous [32]byte) ([]byte, error) {
	if len(key) == 0 {
		hash := sha256.New()
		hash.Write(previous[:])
		hash.Write(plain)
		return append(append([]byte{0}, plain...), hash.Sum(nil)...), nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(append([]byte{1}, nonce...), nonce, plain, previous[:]), nil
}

func edgeJournalOpen(key, sealed []byte, previous [32]byte) ([]byte, error) {
	if len(sealed) == 0 {
		return nil, errors.New("empty journal frame")
	}
	if sealed[0] == 0 && len(key) == 0 {
		if len(sealed) < 33 {
			return nil, errors.New("short plain journal frame")
		}
		plain := sealed[1 : len(sealed)-32]
		hash := sha256.New()
		hash.Write(previous[:])
		hash.Write(plain)
		if !bytes.Equal(hash.Sum(nil), sealed[len(sealed)-32:]) {
			return nil, errors.New("plain journal integrity failure")
		}
		return plain, nil
	}
	if sealed[0] != 1 || len(key) == 0 {
		return nil, errors.New("invalid journal encryption")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < 1+gcm.NonceSize()+gcm.Overhead() {
		return nil, errors.New("short encrypted journal frame")
	}
	return gcm.Open(nil, sealed[1:1+gcm.NonceSize()], sealed[1+gcm.NonceSize():], previous[:])
}

func writeEdgeJournalFrame(file *os.File, key []byte, op *edgeJournalOperation, offset int64, chain [32]byte) (edgeJournalReference, [32]byte, error) {
	plain, err := json.Marshal(op)
	if err != nil {
		return edgeJournalReference{}, chain, err
	}
	sealed, err := edgeJournalSeal(key, plain, chain)
	if err != nil {
		return edgeJournalReference{}, chain, err
	}
	sealedLength := len(sealed)
	if sealedLength < 0 || sealedLength > edgeJournalFrameBytes {
		return edgeJournalReference{}, chain, errors.New("event journal frame too large")
	}
	frameLength := uint32(sealedLength)
	frame := make([]byte, 8+len(sealed))
	binary.BigEndian.PutUint32(frame, frameLength)
	binary.BigEndian.PutUint32(frame[4:], ^frameLength)
	copy(frame[8:], sealed)
	if written, writeErr := file.Write(frame); writeErr != nil || written != len(frame) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return edgeJournalReference{}, chain, writeErr
	}
	ref := edgeJournalReference{offset: offset, length: len(frame), previous: chain}
	if op.Add != nil {
		raw, _ := json.Marshal(op.Add)
		ref.size = len(raw)
		ref.event = NodeRequestEvent{EventID: op.Add.EventID, EventUID: op.Add.EventUID, Priority: op.Add.Priority}
	}
	return ref, sha256.Sum256(frame), nil
}

func (j *edgeEventJournal) sync(file *os.File) error {
	if j.syncFile != nil {
		return j.syncFile(file)
	}
	return file.Sync()
}

func (j *edgeEventJournal) readEvent(ref edgeJournalReference) (NodeRequestEvent, error) {
	if j.file == nil || ref.length < 8 || ref.length > edgeJournalFrameBytes+8 || ref.offset < int64(len(edgeJournalMagic)) {
		return NodeRequestEvent{}, errors.New("invalid event journal reference")
	}
	buf := make([]byte, ref.length)
	if _, err := j.file.ReadAt(buf, ref.offset); err != nil {
		return NodeRequestEvent{}, err
	}
	plain, err := edgeJournalOpen(j.readKey, buf[8:], ref.previous)
	if err != nil {
		return NodeRequestEvent{}, err
	}
	var op edgeJournalOperation
	if err = json.Unmarshal(plain, &op); err != nil {
		return NodeRequestEvent{}, err
	}
	if op.Add == nil {
		return NodeRequestEvent{}, errors.New("journal event reference is not an add")
	}
	return *op.Add, nil
}

func (j *edgeEventJournal) createFromLegacy() error {
	file, err := os.CreateTemp(filepath.Dir(j.path), ".events-journal-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if _, err = file.WriteString(edgeJournalMagic); err != nil {
		file.Close()
		return err
	}
	j.offset = int64(len(edgeJournalMagic))
	j.chain = sha256.Sum256([]byte(edgeJournalMagic))
	resident := int64(0)
	for i := range j.store.items {
		e := j.store.items[i]
		op := &edgeJournalOperation{Add: &e, Next: j.store.next, Dropped: j.store.dropped}
		ref, chain, err := writeEdgeJournalFrame(file, j.store.key, op, j.offset, j.chain)
		if err != nil {
			file.Close()
			return err
		}
		j.refs[e.EventID] = ref
		j.offset += int64(ref.length)
		j.chain = chain
		if resident+int64(ref.size) <= edgeEventQueueBytes {
			resident += int64(ref.size)
		} else {
			j.store.items[i] = ref.event
			j.store.spilled[e.EventID] = true
		}
	}
	ref, chain, err := writeEdgeJournalFrame(file, j.store.key, &edgeJournalOperation{Next: j.store.next, Dropped: j.store.dropped}, j.offset, j.chain)
	if err != nil {
		file.Close()
		return err
	}
	j.offset += int64(ref.length)
	j.chain = chain
	if j.offset > edgeJournalMaxBytes {
		file.Close()
		return errors.New("legacy critical backlog exceeds journal capacity; original preserved")
	}
	if err = j.sync(file); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, j.path); err != nil {
		return err
	}
	if err = syncDirectory(filepath.Dir(j.path)); err != nil {
		return err
	}
	j.file, err = os.OpenFile(j.path, os.O_RDWR, 0o600)
	if err == nil {
		_, err = j.file.Seek(j.offset, io.SeekStart)
	}
	return err
}

func (j *edgeEventJournal) replay() error {
	file, err := os.OpenFile(j.path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	j.file = file
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	if stat.Size() > edgeJournalMaxBytes {
		return errors.New("event journal exceeds bounded replay size")
	}
	magic := make([]byte, len(edgeJournalMagic))
	if _, err = io.ReadFull(file, magic); err != nil || string(magic) != edgeJournalMagic {
		return errors.New("invalid event journal header")
	}
	j.offset = int64(len(magic))
	j.chain = sha256.Sum256(magic)
	for j.offset < stat.Size() {
		var header [8]byte
		if _, err = file.ReadAt(header[:], j.offset); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		frameLength := binary.BigEndian.Uint32(header[:])
		if binary.BigEndian.Uint32(header[4:]) != ^frameLength {
			return errors.New("event journal frame header integrity failure")
		}
		if frameLength < 1 || frameLength > edgeJournalFrameBytes {
			return errors.New("invalid event journal frame length")
		}
		length := int(frameLength)
		if j.offset+8+int64(length) > stat.Size() {
			break
		} // only a physically incomplete tail may be truncated
		frame := make([]byte, 8+length)
		copy(frame, header[:])
		if _, err = file.ReadAt(frame[8:], j.offset+8); err != nil {
			return err
		}
		plain, err := edgeJournalOpen(j.readKey, frame[8:], j.chain)
		if err != nil && j.offset == int64(len(edgeJournalMagic)) && len(j.store.legacyKey) > 0 {
			plain, err = edgeJournalOpen(j.store.legacyKey, frame[8:], j.chain)
			if err == nil {
				j.readKey = append([]byte(nil), j.store.legacyKey...)
			}
		}
		if err != nil {
			return fmt.Errorf("event journal integrity failure at %d: %w", j.offset, err)
		}
		var op edgeJournalOperation
		if err = json.Unmarshal(plain, &op); err != nil {
			return err
		}
		j.store.next, j.store.dropped = max(j.store.next, op.Next), max(j.store.dropped, op.Dropped)
		for _, id := range op.Remove {
			delete(j.refs, id)
		}
		if op.Add != nil {
			raw, _ := json.Marshal(op.Add)
			j.refs[op.Add.EventID] = edgeJournalReference{offset: j.offset, length: len(frame), previous: j.chain, size: len(raw), event: NodeRequestEvent{EventID: op.Add.EventID, EventUID: op.Add.EventUID, Priority: op.Add.Priority}}
			if len(j.refs) > edgeEventQueueLimit {
				return errors.New("event journal exceeds bounded live item count")
			}
		}
		j.chain = sha256.Sum256(frame)
		j.offset += int64(len(frame))
	}
	if err = file.Truncate(j.offset); err != nil {
		return err
	}
	if _, err = file.Seek(j.offset, io.SeekStart); err != nil {
		return err
	}
	ids := make([]int64, 0, len(j.refs))
	for id := range j.refs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	resident := int64(0)
	for _, id := range ids {
		ref := j.refs[id]
		j.store.sizes[id] = ref.size
		j.store.bytes += int64(ref.size)
		j.store.ready[id] = true
		if resident+int64(ref.size) <= edgeEventQueueBytes {
			e, err := j.readEvent(ref)
			if err != nil {
				return err
			}
			j.store.items = append(j.store.items, e)
			resident += int64(ref.size)
		} else {
			j.store.items = append(j.store.items, ref.event)
			j.store.spilled[id] = true
		}
	}
	return nil
}

func (s *edgeEventStore) signalLocked(force bool) {
	if s.journal == nil {
		return
	}
	if force {
		s.force = true
	}
	select {
	case s.journal.wake <- struct{}{}:
	default:
	}
}

func (s *edgeEventStore) add(event NodeRequestEvent) error {
	if event.EventUID == "" {
		uid, err := newEdgeEventUID()
		if err != nil {
			return err
		}
		event.EventUID = uid
	}
	event.Priority = nodeEventPriority(event)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errEdgeEventStoreClosed
	}
	if s.lastErr != nil && time.Now().Before(s.retryAt) {
		err := s.lastErr
		s.mu.Unlock()
		return err
	}
	if s.sizes == nil {
		s.sizes = make(map[int64]int)
		s.spilled = make(map[int64]bool)
		s.ready = make(map[int64]bool)
	}
	s.next++
	event.EventID = s.next
	raw, err := json.Marshal(event)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	size := len(raw)
	if size > edgeJournalFrameBytes-256 {
		s.mu.Unlock()
		return errors.New("event exceeds journal frame limit")
	}
	// Reserve pending-operation capacity before evicting anything. A rejected
	// critical add must not leave non-durable removals in the visible queue.
	var removed []int64
	count, queueBytes := len(s.items), s.bytes
	if count >= edgeEventQueueLimit || queueBytes+int64(size) > edgeEventQueueBytes {
		if event.Priority != nodeEventPriorityCritical {
			s.dropped++
			s.dirty = true
			s.mu.Unlock()
			return nil
		}
		for _, e := range s.items {
			if nodeEventPriority(e) == nodeEventPriorityCritical {
				continue
			}
			removed = append(removed, e.EventID)
			count--
			queueBytes -= int64(s.sizes[e.EventID])
			if count < edgeEventQueueLimit && queueBytes+int64(size) <= edgeEventQueueBytes {
				break
			}
		}
		if count >= edgeEventQueueLimit || queueBytes+int64(size) > edgeEventQueueBytes {
			s.mu.Unlock()
			return errEdgeEventCapacity
		}
	}
	op := &edgeJournalOperation{Add: &event, Remove: removed, Next: s.next, Dropped: s.dropped + int64(len(removed))}
	encoded, err := json.Marshal(op)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if len(encoded)+33 > edgeJournalFrameBytes {
		s.mu.Unlock()
		return errors.New("event operation exceeds journal frame limit")
	}
	op.bytes = len(encoded)
	if s.pendingBytes+int64(op.bytes) > edgeEventQueueBytes {
		s.mu.Unlock()
		return errEdgeEventCapacity
	}
	s.removeLocked(removed)
	s.dropped = op.Dropped
	s.items = append(s.items, event)
	s.sizes[event.EventID] = size
	s.bytes += int64(size)
	if s.journal == nil {
		s.ready[event.EventID] = true
		s.mu.Unlock()
		return nil
	}
	critical := event.Priority == nodeEventPriorityCritical
	if critical {
		op.done = make(chan error, 1)
	}
	done := op.done
	s.pending = append(s.pending, op)
	s.pendingBytes += int64(op.bytes)
	if critical || s.pendingBytes >= 256<<10 {
		s.signalLocked(false)
	}
	s.mu.Unlock()
	if critical {
		return <-done
	}
	return nil
}

func (s *edgeEventStore) removeLocked(ids []int64) {
	if len(ids) == 0 {
		return
	}
	remove := make(map[int64]bool, len(ids))
	for _, id := range ids {
		remove[id] = true
	}
	kept := s.items[:0]
	for _, e := range s.items {
		if remove[e.EventID] {
			s.bytes -= int64(s.sizes[e.EventID])
			delete(s.sizes, e.EventID)
			delete(s.spilled, e.EventID)
			delete(s.ready, e.EventID)
		} else {
			kept = append(kept, e)
		}
	}
	clear(s.items[len(kept):])
	s.items = kept
}

func (s *edgeEventStore) barrier(ids []int64, force bool) error {
	s.mu.Lock()
	if s.journal == nil {
		s.removeLocked(ids)
		s.mu.Unlock()
		return nil
	}
	if s.stopping {
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	if s.closed && !force {
		s.mu.Unlock()
		return errEdgeEventStoreClosed
	}
	if !force && s.lastErr != nil && time.Now().Before(s.retryAt) {
		err := s.lastErr
		s.mu.Unlock()
		return err
	}
	op := &edgeJournalOperation{Remove: ids, Next: s.next, Dropped: s.dropped, done: make(chan error, 1)}
	done := op.done
	s.pending = append(s.pending, op)
	s.signalLocked(force)
	s.mu.Unlock()
	return <-done
}

func (s *edgeEventStore) flush() error { return s.barrier(nil, true) }
func (s *edgeEventStore) ack(events []NodeRequestEvent) error {
	if len(events) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(events))
	for _, e := range events {
		ids = append(ids, e.EventID)
	}
	return s.barrier(ids, false)
}
func (s *edgeEventStore) depth() int              { s.mu.Lock(); defer s.mu.Unlock(); return len(s.items) }
func (s *edgeEventStore) droppedCount() int64     { s.mu.Lock(); defer s.mu.Unlock(); return s.dropped }
func (s *edgeEventStore) persistenceError() error { s.mu.Lock(); defer s.mu.Unlock(); return s.lastErr }

func (s *edgeEventStore) snapshot() []NodeRequestEvent {
	s.mu.Lock()
	j := s.journal
	hasSpilled := len(s.spilled) > 0
	s.mu.Unlock()
	if j != nil && hasSpilled {
		j.ioMu.Lock()
		defer j.ioMu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]NodeRequestEvent, 0, min(edgeReportEventLimit, len(s.items)))
	size := 0
	for _, e := range s.items {
		if !s.ready[e.EventID] {
			continue
		}
		if len(result) >= edgeReportEventLimit || len(result) > 0 && size+s.sizes[e.EventID] > targetAgentReportBytes {
			break
		}
		if s.spilled[e.EventID] {
			var err error
			e, err = j.readEvent(j.refs[e.EventID])
			if err != nil {
				s.lastErr = err
				break
			}
		}
		result = append(result, e)
		size += s.sizes[e.EventID]
	}
	return result
}

func (j *edgeEventJournal) run() {
	defer close(j.done)
	defer func() {
		j.ioMu.Lock()
		if j.file != nil {
			j.file.Close()
		}
		j.ioMu.Unlock()
	}()
	ticker := time.NewTicker(edgeEventPersistInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-j.wake:
		}
		s := j.store
		s.mu.Lock()
		if s.stopping && len(s.pending) == 0 {
			s.mu.Unlock()
			return
		}
		if s.stopping {
			s.force = true
		}
		if len(s.pending) == 0 && s.dirty {
			s.pending = append(s.pending, &edgeJournalOperation{Next: s.next, Dropped: s.dropped})
		}
		if len(s.pending) == 0 || !s.force && time.Now().Before(s.retryAt) {
			s.mu.Unlock()
			continue
		}
		batch := s.pending
		s.pending = nil
		s.pendingBytes = 0
		s.force = false
		s.dirty = false
		s.mu.Unlock()
		j.ioMu.Lock()
		err := j.writeBatch(batch)
		s.mu.Lock()
		if err != nil {
			s.lastErr = err
			if s.backoff == 0 {
				s.backoff = time.Second
			} else {
				s.backoff = min(30*time.Second, s.backoff*2)
			}
			s.retryAt = time.Now().Add(s.backoff)
			for _, op := range batch {
				if op.done != nil {
					op.done <- err
					op.done = nil
				}
				s.pendingBytes += int64(op.bytes)
			}
			s.pending = append(batch, s.pending...)
		} else {
			s.lastErr = nil
			s.backoff = 0
			s.retryAt = time.Time{}
			s.lastPersist = time.Now()
			for _, op := range batch {
				if op.Add != nil {
					if _, ok := s.sizes[op.Add.EventID]; ok {
						s.ready[op.Add.EventID] = true
					}
				}
				s.removeLocked(op.Remove)
				if op.done != nil {
					op.done <- nil
					op.done = nil
				}
			}
		}
		if s.stopping {
			// Every outstanding barrier has now received its durable result.
			// Failed operations remain on disk/in memory for recovery, with no
			// live caller waiting on a worker that has exited.
			for _, op := range s.pending {
				if op.done != nil {
					if err == nil {
						op.done <- errEdgeEventStoreClosed
					} else {
						op.done <- err
					}
					op.done = nil
				}
			}
			s.mu.Unlock()
			j.ioMu.Unlock()
			return
		}
		s.mu.Unlock()
		j.ioMu.Unlock()
	}
}

func (j *edgeEventJournal) writeBatch(batch []*edgeJournalOperation) error {
	if j.file == nil {
		var err error
		j.file, err = os.OpenFile(j.path, os.O_RDWR, 0o600)
		if err != nil {
			return err
		}
		if _, err = j.file.Seek(j.offset, io.SeekStart); err != nil {
			return err
		}
	}
	pathStat, err := os.Stat(j.path)
	if err != nil {
		return err
	}
	fileStat, err := j.file.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(pathStat, fileStat) {
		return errors.New("event journal was replaced outside the writer")
	}
	// Compact before capacity is reached. ACK barriers retain their own frame
	// headroom even when producers are rejected, so a full journal can drain.
	estimate := int64(0)
	for _, op := range batch {
		estimate += int64(op.bytes + len(op.Remove)*22 + 256)
	}
	if j.offset > edgeJournalCompactBytes && j.offset+estimate > max(int64(edgeJournalCompactBytes), j.liveBytes()*2+4<<20) || j.offset+estimate > edgeJournalMaxBytes-(1<<20) {
		if err := j.compact(); err != nil {
			return err
		}
	}
	if j.offset+estimate > edgeJournalMaxBytes {
		return errEdgeEventCapacity
	}
	start, chain := j.offset, j.chain
	refs := make([]edgeJournalReference, len(batch))
	for i, op := range batch {
		ref, next, err := writeEdgeJournalFrame(j.file, j.store.key, op, j.offset, j.chain)
		if err != nil {
			_ = j.file.Truncate(start)
			_, _ = j.file.Seek(start, io.SeekStart)
			j.offset, j.chain = start, chain
			return err
		}
		refs[i] = ref
		j.offset += int64(ref.length)
		j.chain = next
	}
	if err := j.sync(j.file); err != nil {
		_ = j.file.Truncate(start)
		_, _ = j.file.Seek(start, io.SeekStart)
		j.offset, j.chain = start, chain
		return err
	}
	for i, op := range batch {
		for _, id := range op.Remove {
			delete(j.refs, id)
		}
		if op.Add != nil {
			j.refs[op.Add.EventID] = refs[i]
		}
	}
	return nil
}

func (j *edgeEventJournal) liveBytes() int64 {
	var total int64
	for _, ref := range j.refs {
		total += int64(ref.size + 128)
	}
	return total
}

func (j *edgeEventJournal) compact() error {
	j.store.mu.Lock()
	next, dropped := j.store.next, j.store.dropped
	j.store.mu.Unlock()
	return j.compactState(next, dropped)
}

// Initialization already owns store.mu; the active writer obtains an explicit
// metadata snapshot through compact above, without making the lock recursive.
func (j *edgeEventJournal) compactState(next, dropped int64) error {
	file, err := os.CreateTemp(filepath.Dir(j.path), ".events-compact-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	_ = file.Chmod(0o600)
	if _, err = file.WriteString(edgeJournalMagic); err != nil {
		file.Close()
		return err
	}
	ids := make([]int64, 0, len(j.refs))
	for id := range j.refs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	offset := int64(len(edgeJournalMagic))
	chain := sha256.Sum256([]byte(edgeJournalMagic))
	refs := make(map[int64]edgeJournalReference, len(ids))
	for _, id := range ids {
		e, readErr := j.readEvent(j.refs[id])
		if readErr != nil {
			file.Close()
			return readErr
		}
		ref, newChain, writeErr := writeEdgeJournalFrame(file, j.store.key, &edgeJournalOperation{Add: &e, Next: next, Dropped: dropped}, offset, chain)
		if writeErr != nil {
			file.Close()
			return writeErr
		}
		refs[id] = ref
		offset += int64(ref.length)
		chain = newChain
	}
	ref, newChain, err := writeEdgeJournalFrame(file, j.store.key, &edgeJournalOperation{Next: next, Dropped: dropped}, offset, chain)
	if err != nil {
		file.Close()
		return err
	}
	offset += int64(ref.length)
	chain = newChain
	if offset > edgeJournalMaxBytes-(1<<20) {
		file.Close()
		return errEdgeEventCapacity
	}
	if err = j.sync(file); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	// Close before replacing on Windows; the old file remains untouched on any
	// failed rename and is reopened for retries.
	if err = j.file.Close(); err != nil {
		return err
	}
	if err = os.Rename(name, j.path); err != nil {
		j.file, _ = os.OpenFile(j.path, os.O_RDWR, 0o600)
		if j.file != nil {
			_, _ = j.file.Seek(j.offset, io.SeekStart)
		}
		return err
	}
	j.offset, j.chain, j.refs = offset, chain, refs
	j.readKey = append(j.readKey[:0], j.store.key...)
	j.file, err = os.OpenFile(j.path, os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	_, err = j.file.Seek(offset, io.SeekStart)
	if err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(j.path))
}

// exportLegacy is deliberately a cold shutdown/update boundary. It enables a
// controlled rollback to an older Agent; regular requests never rewrite it.
func (s *edgeEventStore) exportLegacy() error {
	if err := s.flush(); err != nil {
		return err
	}
	s.mu.Lock()
	j := s.journal
	s.mu.Unlock()
	if j == nil {
		return nil
	}
	j.ioMu.Lock()
	defer j.ioMu.Unlock()
	s.mu.Lock()
	next, dropped := s.next, s.dropped
	s.mu.Unlock()
	var plain bytes.Buffer
	fmt.Fprintf(&plain, `{"next":%d,"items":[`, next)
	ids := make([]int64, 0, len(j.refs))
	for id := range j.refs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	for i, id := range ids {
		e, err := j.readEvent(j.refs[id])
		if err != nil {
			return err
		}
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if plain.Len()+len(data)+128 > edgeLegacySpoolExportBytes {
			return errors.New("legacy rollback export exceeds bounded size; drain Agent events before rollback")
		}
		if i > 0 {
			plain.WriteByte(',')
		}
		plain.Write(data)
	}
	fmt.Fprintf(&plain, `],"dropped":%d}`, dropped)
	data := plain.Bytes()
	if len(s.key) > 0 {
		block, err := aes.NewCipher(s.key)
		if err != nil {
			return err
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return err
		}
		nonce := make([]byte, gcm.NonceSize())
		if _, err = rand.Read(nonce); err != nil {
			return err
		}
		sealed := gcm.Seal(nil, nonce, data, nil)
		data, err = json.Marshal(struct {
			Version    int    `json:"version"`
			Nonce      string `json:"nonce"`
			Ciphertext string `json:"ciphertext"`
		}{1, base64.RawStdEncoding.EncodeToString(nonce), base64.RawStdEncoding.EncodeToString(sealed)})
		if err != nil {
			return err
		}
	}
	if err := writePrivateFileAtomic(s.path, data); err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	return writePrivateFileAtomic(filepath.Join(filepath.Dir(s.path), "events.legacy.sha256"), []byte(hex.EncodeToString(digest[:])))
}

func (s *edgeEventStore) close() error {
	s.mu.Lock()
	if s.closeDone != nil {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	s.closeDone = make(chan struct{})
	s.closed = true
	s.mu.Unlock()
	err := s.exportLegacy()
	s.mu.Lock()
	s.closeErr = err
	s.stopping = true
	j := s.journal
	s.signalLocked(true)
	s.mu.Unlock()
	if j != nil {
		<-j.done
	}
	s.mu.Lock()
	close(s.closeDone)
	s.mu.Unlock()
	return err
}
