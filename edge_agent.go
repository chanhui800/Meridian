package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/websocket"
)

const (
	edgeEventBodyLimit     = 8 << 10
	edgeEventResponseLimit = maxNodeRequestEventResponseBodyBytes
	edgeReportEventLimit   = maxNodeRequestEventsPerReport
	edgeEventQueueLimit    = 8192
)

var errEdgeAgentUpdated = errors.New("Agent binary updated; restarting")

type edgeAgentState struct {
	NodeGUID string `json:"node_guid"`
	Token    string `json:"agent_token"`
}

type edgeSiteIdentity struct {
	centralID int64
	host      string
}

type edgeEventStore struct {
	mu        sync.Mutex
	next      int64
	items     []NodeRequestEvent
	path      string
	key       []byte
	legacyKey []byte
	dropped   int64
}

func (s *edgeEventStore) init(dir string) error {
	return s.initWithKey(dir, nil)
}

func (s *edgeEventStore) initWithKey(dir string, secret []byte) error {
	var key []byte
	if len(secret) > 0 {
		digest := sha256.Sum256(secret)
		key = append([]byte(nil), digest[:]...)
	}
	return s.initWithRawKey(dir, key, nil)
}

func (s *edgeEventStore) initWithRawKey(dir string, key, legacyKey []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path = filepath.Join(dir, "events.json")
	s.key = append([]byte(nil), key...)
	s.legacyKey = append([]byte(nil), legacyKey...)
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var stored struct {
		Next    int64              `json:"next"`
		Items   []NodeRequestEvent `json:"items"`
		Dropped int64              `json:"dropped"`
	}
	plaintext := true
	plainErr := json.Unmarshal(data, &stored)
	var envelope struct {
		Version    int    `json:"version"`
		Nonce      string `json:"nonce"`
		Ciphertext string `json:"ciphertext"`
	}
	_ = json.Unmarshal(data, &envelope)
	if plainErr != nil || (envelope.Version == 1 && envelope.Ciphertext != "") {
		plaintext = false
		if len(s.key) == 0 {
			return fmt.Errorf("load event queue: %w", plainErr)
		}
		if envelope.Version != 1 {
			return fmt.Errorf("load event queue: invalid encrypted spool")
		}
		opened, decryptErr := decryptEventSpoolEnvelope(envelope.Version, envelope.Nonce, envelope.Ciphertext, s.key)
		if decryptErr != nil && len(s.legacyKey) > 0 {
			opened, decryptErr = decryptEventSpoolEnvelope(envelope.Version, envelope.Nonce, envelope.Ciphertext, s.legacyKey)
			if decryptErr == nil {
				plaintext = true // migration below rewrites using the independent key.
			}
		}
		if decryptErr != nil {
			return fmt.Errorf("load event queue: decrypt: %w", decryptErr)
		}
		if err := json.Unmarshal(opened, &stored); err != nil {
			return fmt.Errorf("load event queue: decrypt: %w", err)
		}
	}
	if len(stored.Items) > edgeEventQueueLimit {
		stored.Dropped += int64(len(stored.Items) - edgeEventQueueLimit)
		stored.Items = stored.Items[len(stored.Items)-edgeEventQueueLimit:]
	}
	s.next, s.items, s.dropped = stored.Next, append([]NodeRequestEvent(nil), stored.Items...), stored.Dropped
	changed := false
	for _, item := range s.items {
		if item.EventID > s.next {
			s.next = item.EventID
		}
		if item.EventUID == "" {
			changed = true
		}
	}
	if changed || (len(s.key) > 0 && plaintext) {
		for i := range s.items {
			if s.items[i].EventUID == "" {
				uid, err := newEdgeEventUID()
				if err != nil {
					return err
				}
				s.items[i].EventUID = uid
			}
		}
		if err := s.persistLocked(); err != nil {
			return fmt.Errorf("migrate event queue identities: %w", err)
		}
	}
	return nil
}

func decryptEventSpoolEnvelope(version int, nonceText, ciphertextText string, key []byte) ([]byte, error) {
	if version != 1 || len(key) == 0 {
		return nil, errors.New("invalid encrypted spool")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce, err := base64.RawStdEncoding.DecodeString(nonceText)
	if err != nil || len(nonce) != gcm.NonceSize() {
		return nil, errors.New("invalid nonce")
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(ciphertextText)
	if err != nil {
		return nil, errors.New("invalid ciphertext")
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func (s *edgeEventStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	data, err := json.Marshal(struct {
		Next    int64              `json:"next"`
		Items   []NodeRequestEvent `json:"items"`
		Dropped int64              `json:"dropped"`
	}{s.next, s.items, s.dropped})
	if err != nil {
		return err
	}
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
		if _, err := rand.Read(nonce); err != nil {
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
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".events-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	_ = tmp.Chmod(0o600)
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(s.path))
}

func newEdgeEventUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func loadOrCreateSpoolKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is the administrator-selected local Agent state directory.
	if err == nil {
		if len(data) != 32 {
			return nil, errors.New("event spool key has an invalid size")
		}
		return append([]byte(nil), data...), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := writePrivateFileAtomic(path, key); err != nil {
		return nil, err
	}
	return key, nil
}

func quarantineEventSpool(path string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	quarantine := fmt.Sprintf("%s.corrupt.%d", path, time.Now().UnixNano())
	if err := os.Rename(path, quarantine); err != nil {
		return err
	}
	return nil
}

func nodeEventPriority(event NodeRequestEvent) string {
	if event.Priority == nodeEventPriorityCritical || event.Priority == nodeEventPriorityBestEffort {
		return event.Priority
	}
	switch event.ResourceCategory {
	case requestLogCategoryPlayback, requestLogCategoryPlaybackSync, requestLogCategoryMetadata:
		return nodeEventPriorityCritical
	default:
		return nodeEventPriorityBestEffort
	}
}

func (s *edgeEventStore) add(event NodeRequestEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	event.EventID = s.next
	if event.EventUID == "" {
		uid, err := newEdgeEventUID()
		if err != nil {
			return err
		}
		event.EventUID = uid
	}
	event.Priority = nodeEventPriority(event)
	if len(s.items) >= edgeEventQueueLimit {
		if event.Priority == nodeEventPriorityCritical {
			removed := -1
			for i, existing := range s.items {
				if nodeEventPriority(existing) == nodeEventPriorityBestEffort {
					removed = i
					break
				}
			}
			if removed >= 0 {
				s.items = append(s.items[:removed], s.items[removed+1:]...)
				s.dropped++
			} else {
				// If the queue is entirely critical, keep the newest critical
				// event and discard the oldest one.
				s.items = append([]NodeRequestEvent(nil), s.items[1:]...)
				s.dropped++
			}
		} else {
			s.dropped++
			return s.persistLocked()
		}
	}
	s.items = append(s.items, event)
	if len(s.items) > edgeEventQueueLimit {
		s.items = append([]NodeRequestEvent(nil), s.items[len(s.items)-edgeEventQueueLimit:]...)
	}
	return s.persistLocked()
}

func (s *edgeEventStore) snapshot() []NodeRequestEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	end := len(s.items)
	if end > edgeReportEventLimit {
		end = edgeReportEventLimit
	}
	return append([]NodeRequestEvent(nil), s.items[:end]...)
}

func (s *edgeEventStore) depth() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

func (s *edgeEventStore) droppedCount() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

func (s *edgeEventStore) ack(events []NodeRequestEvent) error {
	if len(events) == 0 {
		return nil
	}
	accepted := make(map[int64]struct{}, len(events))
	for _, event := range events {
		accepted[event.EventID] = struct{}{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.items[:0]
	for _, item := range s.items {
		if _, ok := accepted[item.EventID]; !ok {
			kept = append(kept, item)
		}
	}
	s.items = kept
	return s.persistLocked()
}

type edgeSiteStats struct {
	mu    sync.Mutex
	items map[string]NodeSiteStat
}

func (s *edgeSiteStats) record(host string, status int, bytesIn, bytesOut, cumulativeIn, cumulativeOut, requests int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[string]NodeSiteStat)
	}
	value := s.items[host]
	value.Host = host
	value.RequestCount++
	value.LastRequestAtMS = time.Now().UnixMilli()
	value.LastStatus = status
	value.BytesIn += bytesIn
	value.BytesOut += bytesOut
	value.CumulativeBytesIn = cumulativeIn
	value.CumulativeBytesOut = cumulativeOut
	if requests > value.RequestCount {
		value.RequestCount = requests
	}
	s.items[host] = value
}

func (s *edgeSiteStats) snapshot() []NodeSiteStat {
	s.mu.Lock()
	defer s.mu.Unlock()
	values := make([]NodeSiteStat, 0, len(s.items))
	for _, value := range s.items {
		values = append(values, value)
	}
	return values
}

type edgeStatusWriter struct {
	http.ResponseWriter
	status       int
	capture      *bytes.Buffer
	captureLimit int
}

func (w *edgeStatusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *edgeStatusWriter) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.capture != nil && w.capture.Len() < w.captureLimit {
		remaining := w.captureLimit - w.capture.Len()
		if remaining > len(payload) {
			remaining = len(payload)
		}
		_, _ = w.capture.Write(payload[:remaining])
	}
	return w.ResponseWriter.Write(payload)
}

func (w *edgeStatusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		flusher.Flush()
	}
}

func (w *edgeStatusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacking is not supported")
	}
	return hijacker.Hijack()
}

func (w *edgeStatusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

type edgeProxyBundle struct {
	database   *DB
	manager    *ProxyManager
	handler    http.Handler
	localSites map[int64]edgeSiteIdentity
	// centralSites is keyed by the stable Controller SiteID. localSites is
	// intentionally keyed by the ephemeral in-memory SQLite ID and must never
	// be used for lifecycle checks against Controller IDs.
	centralSites map[int64]struct{}
}

func (b *edgeProxyBundle) close() {
	if b == nil {
		return
	}
	if b.manager != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		b.manager.GracefulShutdown(ctx)
		cancel()
	}
	if b.database != nil {
		b.database.Close()
	}
}

func (b *edgeProxyBundle) drain(ctx context.Context) {
	if b == nil {
		return
	}
	if b.manager != nil {
		b.manager.DrainShutdown(ctx)
	}
	if b.database != nil {
		b.database.Close()
	}
}

type edgeAgentRuntime struct {
	mu                   sync.RWMutex
	stateDir             string
	nodeGUID             string
	port                 int
	handler              http.Handler
	certificate          *tls.Certificate
	server               *http.Server
	bundle               *edgeProxyBundle
	drainContext         context.Context
	drainCancel          context.CancelFunc
	drainingBundles      map[*edgeProxyBundle]struct{}
	appliedHash          string
	appliedRevision      int64
	telemetrySequence    int64
	siteCounterEpoch     uint64
	siteCounterEpochs    map[int64]uint64
	siteStatsCursor      int64
	mediaCursor          int64
	retentionCursor      int64
	cacheClearGeneration int64
	listenerError        string
	applyError           string
	applyErrorAtMS       int64
	applyFailures        int64
	eventSpoolError      string
	events               edgeEventStore
	stats                edgeSiteStats
	telemetryMu          sync.Mutex
	mediaCounts          map[int64]NodeMediaCount
	retention            map[int64]NodeRetentionStatus
	observations         []NodeDynamicObservation
	siteReported         map[int64]ProxyRuntimeStat
	trafficCounters      map[int64]*edgeSiteTrafficCounter
	// trafficHosts keeps the Controller host alongside the process-stable
	// counter. A route can disappear from the current bundle while an admitted
	// stream is still draining; its counter must remain reportable.
	trafficHosts map[int64]string
	resolver     dynamicIPResolver
	transport    dynamicTransportFactory
	listen       func(string, string) (net.Listener, error)
}

// beginBundleDrain rejects new work on an old generation but gives streams
// admitted before a hot apply time to finish. Runtime shutdown/revoke cancels
// this context and therefore remains an immediate security boundary.
func (runtime *edgeAgentRuntime) beginBundleDrain(bundle *edgeProxyBundle) {
	if runtime == nil || bundle == nil {
		return
	}
	runtime.mu.Lock()
	if runtime.drainContext == nil || runtime.drainCancel == nil {
		runtime.drainContext, runtime.drainCancel = context.WithCancel(context.Background())
	}
	if runtime.drainingBundles == nil {
		runtime.drainingBundles = make(map[*edgeProxyBundle]struct{})
	}
	runtime.drainingBundles[bundle] = struct{}{}
	parent := runtime.drainContext
	runtime.mu.Unlock()
	// Normal drains finish when admitted handlers and final counters are
	// acknowledged. Keep an emergency bound so a wedged handler cannot pin a
	// complete old generation forever or outlive the Controller drain window.
	ctx, cancel := context.WithTimeout(parent, siteNodeDrainWindow)
	go func() {
		defer cancel()
		bundle.drain(ctx)
		runtime.mu.Lock()
		delete(runtime.drainingBundles, bundle)
		runtime.mu.Unlock()
	}()
}

// forceStopCentralSites applies an explicit revocation to the current bundle
// and every generation that is still draining. A route can be absent from the
// current config while an older generation continues to own live streams.
func (runtime *edgeAgentRuntime) forceStopCentralSites(ctx context.Context, siteIDs []int64, extraBundles ...*edgeProxyBundle) {
	if runtime == nil || len(siteIDs) == 0 {
		return
	}
	wanted := make(map[int64]struct{}, len(siteIDs))
	for _, siteID := range siteIDs {
		if siteID > 0 {
			wanted[siteID] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return
	}
	runtime.mu.RLock()
	bundles := make(map[*edgeProxyBundle]struct{}, len(runtime.drainingBundles)+1)
	if runtime.bundle != nil {
		bundles[runtime.bundle] = struct{}{}
	}
	for bundle := range runtime.drainingBundles {
		bundles[bundle] = struct{}{}
	}
	for _, bundle := range extraBundles {
		if bundle != nil {
			bundles[bundle] = struct{}{}
		}
	}
	runtime.mu.RUnlock()
	for bundle := range bundles {
		if bundle == nil || bundle.manager == nil {
			continue
		}
		localIDs := make(map[int64]struct{})
		for localID, identity := range bundle.localSites {
			if _, ok := wanted[identity.centralID]; ok {
				localIDs[localID] = struct{}{}
			}
		}
		if len(localIDs) > 0 {
			bundle.manager.ForceStopSites(ctx, localIDs)
		}
	}
}

type edgeAgentRuntimeState struct {
	port                 int
	server               *http.Server
	bundle               *edgeProxyBundle
	nodeGUID             string
	appliedHash          string
	appliedRevision      int64
	certificate          *tls.Certificate
	handler              http.Handler
	cacheClearGeneration int64
	siteCounterEpoch     uint64
	siteReported         map[int64]ProxyRuntimeStat
	listenerError        string
}

func (runtime *edgeAgentRuntime) trafficCounterFor(siteID int64, hosts ...string) *edgeSiteTrafficCounter {
	if runtime == nil || siteID <= 0 {
		return nil
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.trafficCounters == nil {
		runtime.trafficCounters = make(map[int64]*edgeSiteTrafficCounter)
	}
	if len(hosts) > 0 && strings.TrimSpace(hosts[0]) != "" {
		if runtime.trafficHosts == nil {
			runtime.trafficHosts = make(map[int64]string)
		}
		runtime.trafficHosts[siteID] = requestPublicHost(hosts[0])
	}
	if counter := runtime.trafficCounters[siteID]; counter != nil {
		return counter
	}
	if runtime.siteCounterEpochs == nil {
		runtime.siteCounterEpochs = make(map[int64]uint64)
	}
	epoch := runtime.siteCounterEpochs[siteID] + 1
	if epoch == 0 {
		epoch = 1
	}
	runtime.siteCounterEpochs[siteID] = epoch
	counter := &edgeSiteTrafficCounter{epoch: epoch}
	runtime.trafficCounters[siteID] = counter
	return counter
}

func (runtime *edgeAgentRuntime) queueEvent(event NodeRequestEvent) bool {
	if runtime == nil {
		return false
	}
	if err := runtime.events.add(event); err != nil {
		runtime.mu.Lock()
		runtime.eventSpoolError = err.Error()
		runtime.mu.Unlock()
		return false
	} else {
		runtime.mu.Lock()
		runtime.eventSpoolError = ""
		runtime.mu.Unlock()
		return true
	}
}

func (runtime *edgeAgentRuntime) eventSpoolStatus() (string, int) {
	runtime.mu.RLock()
	errText := runtime.eventSpoolError
	runtime.mu.RUnlock()
	return errText, runtime.events.depth()
}

type edgeTelemetryEvent struct {
	Kind        string
	Media       mediaLibraryCountEvent
	Retention   accountRetentionCompletionEvent
	Observation dynamicObservationEvent
}

// edgeTelemetryEventSiteID converts the ephemeral site ID used by the
// Agent's in-memory proxy database back to the stable Controller site ID.
// Runtime routes are rebuilt from every config apply, so their local
// auto-increment IDs must never cross the Agent report boundary.
func edgeTelemetryEventSiteID(event edgeTelemetryEvent, localSites map[int64]edgeSiteIdentity) (edgeTelemetryEvent, bool) {
	var localID int64
	switch event.Kind {
	case "media_counts":
		localID = event.Media.SiteID
	case "retention":
		localID = event.Retention.SiteID
	case "observation":
		localID = event.Observation.SiteID
	default:
		return event, false
	}
	identity, ok := localSites[localID]
	if !ok || identity.centralID <= 0 {
		return event, false
	}
	switch event.Kind {
	case "media_counts":
		event.Media.SiteID = identity.centralID
	case "retention":
		event.Retention.SiteID = identity.centralID
	case "observation":
		event.Observation.SiteID = identity.centralID
	}
	return event, true
}

func (runtime *edgeAgentRuntime) recordTelemetry(event edgeTelemetryEvent) {
	if runtime == nil {
		return
	}
	runtime.telemetryMu.Lock()
	defer runtime.telemetryMu.Unlock()
	switch event.Kind {
	case "media_counts":
		if runtime.mediaCounts == nil {
			runtime.mediaCounts = make(map[int64]NodeMediaCount)
		}
		current := runtime.mediaCounts[event.Media.SiteID]
		if event.Media.ObservedAtMS >= current.ObservedAtMS {
			runtime.mediaCounts[event.Media.SiteID] = NodeMediaCount{SiteID: event.Media.SiteID, MovieCount: event.Media.MovieCount, SeriesCount: event.Media.SeriesCount, EpisodeCount: event.Media.EpisodeCount, ObservedAtMS: event.Media.ObservedAtMS}
		}
	case "retention":
		if runtime.retention == nil {
			runtime.retention = make(map[int64]NodeRetentionStatus)
		}
		value := NodeRetentionStatus{SiteID: event.Retention.SiteID, ExpectedStartedAtMS: event.Retention.ExpectedStartedAtMS, CompletedAtMS: event.Retention.CompletedAtMS, Done: true}
		if previous, ok := runtime.retention[value.SiteID]; !ok || value.CompletedAtMS >= previous.CompletedAtMS {
			runtime.retention[value.SiteID] = value
		}
	case "observation":
		observation := NodeDynamicObservation{SiteID: event.Observation.SiteID, CanonicalAuthority: event.Observation.CanonicalAuthority, Source: event.Observation.Source, Decision: event.Observation.Decision, ReasonCode: event.Observation.ReasonCode, ObservedAtMS: time.Now().UnixMilli()}
		runtime.observations = append(runtime.observations, observation)
		if len(runtime.observations) > maxNodeTelemetryItemsPerReport*4 {
			runtime.observations = append([]NodeDynamicObservation(nil), runtime.observations[len(runtime.observations)-maxNodeTelemetryItemsPerReport*4:]...)
		}
	}
}

type edgeTelemetryPending struct {
	media          []NodeMediaCount
	retention      []NodeRetentionStatus
	observations   []NodeDynamicObservation
	mediaOrder     []int64
	retentionOrder []int64
}

func (runtime *edgeAgentRuntime) prepareTelemetry() edgeTelemetryPending {
	if runtime == nil {
		return edgeTelemetryPending{}
	}
	runtime.telemetryMu.Lock()
	defer runtime.telemetryMu.Unlock()
	pending := edgeTelemetryPending{}
	mediaIDs := make([]int64, 0, len(runtime.mediaCounts))
	for siteID := range runtime.mediaCounts {
		mediaIDs = append(mediaIDs, siteID)
	}
	sort.Slice(mediaIDs, func(i, j int) bool { return mediaIDs[i] < mediaIDs[j] })
	mediaStart := telemetryCursorStart(mediaIDs, runtime.mediaCursor)
	for offset := 0; offset < len(mediaIDs) && len(pending.media) < maxNodeTelemetryItemsPerReport; offset++ {
		index := (mediaStart + offset) % len(mediaIDs)
		siteID := mediaIDs[index]
		pending.mediaOrder = append(pending.mediaOrder, siteID)
		pending.media = append(pending.media, runtime.mediaCounts[siteID])
	}
	retentionIDs := make([]int64, 0, len(runtime.retention))
	for siteID := range runtime.retention {
		retentionIDs = append(retentionIDs, siteID)
	}
	sort.Slice(retentionIDs, func(i, j int) bool { return retentionIDs[i] < retentionIDs[j] })
	retentionStart := telemetryCursorStart(retentionIDs, runtime.retentionCursor)
	for offset := 0; offset < len(retentionIDs) && len(pending.retention) < maxNodeTelemetryItemsPerReport; offset++ {
		index := (retentionStart + offset) % len(retentionIDs)
		siteID := retentionIDs[index]
		pending.retentionOrder = append(pending.retentionOrder, siteID)
		pending.retention = append(pending.retention, runtime.retention[siteID])
	}
	end := len(runtime.observations)
	if end > maxNodeTelemetryItemsPerReport {
		end = maxNodeTelemetryItemsPerReport
	}
	pending.observations = append([]NodeDynamicObservation(nil), runtime.observations[:end]...)
	return pending
}

func telemetryCursorStart(ids []int64, cursor int64) int {
	if len(ids) == 0 || cursor <= 0 {
		return 0
	}
	for index, siteID := range ids {
		if siteID >= cursor {
			return index
		}
	}
	return 0
}

func acknowledgedSiteIDs(accepted, discarded []int64) map[int64]bool {
	result := make(map[int64]bool, len(accepted)+len(discarded))
	for _, siteID := range accepted {
		if siteID > 0 {
			result[siteID] = true
		}
	}
	for _, siteID := range discarded {
		if siteID > 0 {
			// A discarded item is an explicit, final Controller disposition. It
			// is safe to remove locally; an HTTP success without this field is not.
			result[siteID] = true
		}
	}
	return result
}

func (runtime *edgeAgentRuntime) commitTelemetryWithACK(pending edgeTelemetryPending, mediaACK, retentionACK, observationACK map[int64]bool) {
	if runtime == nil {
		return
	}
	runtime.telemetryMu.Lock()
	defer runtime.telemetryMu.Unlock()
	advanceCursor := func(order []int64, acknowledged map[int64]bool) int64 {
		var next int64
		for _, siteID := range order {
			if acknowledged[siteID] {
				next = siteID + 1
			}
		}
		return next
	}
	if next := advanceCursor(pending.mediaOrder, mediaACK); next > 0 {
		runtime.mediaCursor = next
	}
	if next := advanceCursor(pending.retentionOrder, retentionACK); next > 0 {
		runtime.retentionCursor = next
	}
	for _, sent := range pending.media {
		if mediaACK[sent.SiteID] {
			if current, ok := runtime.mediaCounts[sent.SiteID]; ok && current == sent {
				delete(runtime.mediaCounts, sent.SiteID)
			}
		}
	}
	for _, sent := range pending.retention {
		if retentionACK[sent.SiteID] {
			if current, ok := runtime.retention[sent.SiteID]; ok && current == sent {
				delete(runtime.retention, sent.SiteID)
			}
		}
	}
	if len(pending.observations) > 0 && len(runtime.observations) >= len(pending.observations) {
		remaining := runtime.observations[:0]
		for index, current := range runtime.observations {
			if index < len(pending.observations) && current == pending.observations[index] && observationACK[current.SiteID] {
				continue
			}
			remaining = append(remaining, current)
		}
		runtime.observations = append([]NodeDynamicObservation(nil), remaining...)
	}
}

// commitTelemetry is retained for local diagnostic snapshots and tests. A
// network report must use commitTelemetryWithACK so it cannot silently lose a
// category the Controller did not accept.
func (runtime *edgeAgentRuntime) commitTelemetry(pending edgeTelemetryPending) {
	media := make(map[int64]bool, len(pending.media))
	retention := make(map[int64]bool, len(pending.retention))
	observations := make(map[int64]bool, len(pending.observations))
	for _, value := range pending.media {
		media[value.SiteID] = true
	}
	for _, value := range pending.retention {
		retention[value.SiteID] = true
	}
	for _, value := range pending.observations {
		observations[value.SiteID] = true
	}
	runtime.commitTelemetryWithACK(pending, media, retention, observations)
}

func (runtime *edgeAgentRuntime) telemetrySnapshot() (media []NodeMediaCount, retention []NodeRetentionStatus, observations []NodeDynamicObservation) {
	pending := runtime.prepareTelemetry()
	runtime.commitTelemetry(pending)
	return pending.media, pending.retention, pending.observations
}

type edgeSiteStatsPending struct {
	stats      []NodeSiteStat
	current    map[int64]ProxyRuntimeStat
	final      map[int64]bool
	order      []int64
	nextCursor int64
}

func (runtime *edgeAgentRuntime) prepareSiteStats() edgeSiteStatsPending {
	pending := edgeSiteStatsPending{}
	if runtime == nil {
		return pending
	}
	runtime.mu.RLock()
	bundle := runtime.bundle
	statsCursor := runtime.siteStatsCursor
	previous := make(map[int64]ProxyRuntimeStat, len(runtime.siteReported))
	for siteID, value := range runtime.siteReported {
		previous[siteID] = value
	}
	counters := make(map[int64]*edgeSiteTrafficCounter, len(runtime.trafficCounters))
	hosts := make(map[int64]string, len(runtime.trafficHosts))
	for siteID, counter := range runtime.trafficCounters {
		counters[siteID] = counter
		hosts[siteID] = runtime.trafficHosts[siteID]
	}
	runtime.mu.RUnlock()

	// Request metadata and cache size belong to the current bundle, but byte
	// counters deliberately do not. The latter are process-stable so a route
	// removed during a draining connection still gets a final report.
	requestStats := runtime.stats.snapshot()
	byHost := make(map[string]NodeSiteStat, len(requestStats))
	for _, value := range requestStats {
		byHost[requestPublicHost(value.Host)] = value
	}
	cacheSizes := map[int64]int64(nil)
	cacheErr := error(nil)
	if bundle != nil && bundle.manager != nil {
		cacheSizes, _, cacheErr = bundle.manager.AssetCacheSizes()
		if cacheErr != nil {
			log.Printf("[agent] read asset cache sizes: %v", cacheErr)
			cacheSizes = nil
		}
	}
	pending.current = make(map[int64]ProxyRuntimeStat, len(counters))
	pending.final = make(map[int64]bool)
	pending.stats = make([]NodeSiteStat, 0, len(counters))
	centralIDs := make([]int64, 0, len(counters))
	for centralID := range counters {
		centralIDs = append(centralIDs, centralID)
	}
	sort.Slice(centralIDs, func(i, j int) bool { return centralIDs[i] < centralIDs[j] })
	start := telemetryCursorStart(centralIDs, statsCursor)
	for offset := 0; offset < len(centralIDs); offset++ {
		centralID := centralIDs[(start+offset)%len(centralIDs)]
		counter := counters[centralID]
		if len(pending.stats) >= maxNodeSiteStatsPerReport {
			break
		}
		if centralID <= 0 || counter == nil {
			continue
		}
		host := requestPublicHost(hosts[centralID])
		if host == "" {
			continue
		}
		value := ProxyRuntimeStat{
			SiteID:             centralID,
			Requests:           counter.requests.Load(),
			CumulativeBytesIn:  counter.cumulativeIn.Load(),
			CumulativeBytesOut: counter.cumulativeOut.Load(),
		}
		pending.current[centralID] = value
		prior := previous[centralID]
		inDelta, outDelta := value.CumulativeBytesIn-prior.CumulativeBytesIn, value.CumulativeBytesOut-prior.CumulativeBytesOut
		if inDelta < 0 {
			inDelta = value.CumulativeBytesIn
		}
		if outDelta < 0 {
			outDelta = value.CumulativeBytesOut
		}
		observed := byHost[host]
		observed.SiteID = centralID
		observed.Host = host
		observed.RequestCount = value.Requests
		observed.BytesIn, observed.BytesOut = inDelta, outDelta
		observed.CumulativeBytesIn, observed.CumulativeBytesOut = value.CumulativeBytesIn, value.CumulativeBytesOut
		// A removed route is final only after all requests admitted by the old
		// bundle have finished. The Controller uses this marker to retire the
		// corresponding drain generation and ACK the counter before GC.
		if bundle != nil && bundle.centralSites != nil {
			_, stillRouted := bundle.centralSites[centralID]
			observed.Final = !stillRouted && counter.activeRequests.Load() == 0
		}
		observed.CounterEpoch = counter.epoch
		if observed.Final {
			pending.final[centralID] = true
		}
		if cacheErr == nil {
			observed.CacheSizeBytes = cacheSizes[centralID]
			observed.CacheSizeValid = true
		}
		pending.stats = append(pending.stats, observed)
		pending.order = append(pending.order, centralID)
	}
	if len(pending.order) > 0 {
		pending.nextCursor = pending.order[len(pending.order)-1] + 1
	}
	return pending
}

// liveSiteTrafficSnapshot copies only the counters needed by the dashboard.
// It avoids cache sizing, request metadata, telemetry queues, and event
// spooling so a 2-second sample remains cheap and never consumes full-report
// state.
func (runtime *edgeAgentRuntime) liveSiteTrafficSnapshot() []NodeLiveSiteTraffic {
	if runtime == nil {
		return nil
	}
	runtime.mu.RLock()
	ids := make([]int64, 0, len(runtime.trafficCounters))
	for siteID := range runtime.trafficCounters {
		if runtime.bundle != nil && runtime.bundle.centralSites != nil {
			if _, ok := runtime.bundle.centralSites[siteID]; !ok {
				continue
			}
		}
		ids = append(ids, siteID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > maxNodeLiveSitesPerReport {
		ids = ids[:maxNodeLiveSitesPerReport]
	}
	result := make([]NodeLiveSiteTraffic, 0, len(ids))
	for _, siteID := range ids {
		counter := runtime.trafficCounters[siteID]
		host := requestPublicHost(runtime.trafficHosts[siteID])
		if counter == nil || host == "" {
			continue
		}
		result = append(result, NodeLiveSiteTraffic{
			SiteID: siteID, Host: host,
			CumulativeBytesIn:  counter.cumulativeIn.Load(),
			CumulativeBytesOut: counter.cumulativeOut.Load(),
			Requests:           counter.requests.Load(),
		})
	}
	runtime.mu.RUnlock()
	return result
}

func (runtime *edgeAgentRuntime) commitSiteStatsWithACK(pending edgeSiteStatsPending, acknowledged map[int64]bool) {
	if runtime == nil || len(pending.current) == 0 {
		return
	}
	runtime.mu.Lock()
	if runtime.siteReported == nil {
		runtime.siteReported = make(map[int64]ProxyRuntimeStat)
	}
	deltas := make(map[int64]ProxyRuntimeStat, len(pending.current))
	for index, siteID := range pending.order {
		if acknowledged[siteID] {
			runtime.siteStatsCursor = pending.order[index] + 1
		}
	}
	for siteID, sent := range pending.current {
		if !acknowledged[siteID] {
			continue
		}
		current := runtime.siteReported[siteID]
		delta := ProxyRuntimeStat{SiteID: siteID}
		if sent.CumulativeBytesIn >= current.CumulativeBytesIn {
			delta.CumulativeBytesIn = sent.CumulativeBytesIn - current.CumulativeBytesIn
		} else {
			delta.CumulativeBytesIn = sent.CumulativeBytesIn
		}
		if sent.CumulativeBytesOut >= current.CumulativeBytesOut {
			delta.CumulativeBytesOut = sent.CumulativeBytesOut - current.CumulativeBytesOut
		} else {
			delta.CumulativeBytesOut = sent.CumulativeBytesOut
		}
		if sent.Requests >= current.Requests {
			delta.Requests = sent.Requests - current.Requests
		} else {
			delta.Requests = sent.Requests
		}
		deltas[siteID] = delta
		runtime.siteReported[siteID] = sent
	}
	bundle := runtime.bundle
	reported := make(map[int64]ProxyRuntimeStat, len(deltas))
	for siteID := range deltas {
		reported[siteID] = runtime.siteReported[siteID]
	}
	runtime.mu.Unlock()
	finalAcked := make(map[int64]bool)
	for siteID := range pending.final {
		if acknowledged[siteID] {
			finalAcked[siteID] = true
		}
	}
	runtime.gcRetiredTrafficCounters(finalAcked)
	// Move the ACK watermark into the live instances immediately. Otherwise a
	// successful report would remain part of the local delta until the next
	// config refresh and could be charged twice against a quota.
	if bundle == nil || bundle.manager == nil {
		return
	}
	bundle.manager.mu.RLock()
	defer bundle.manager.mu.RUnlock()
	for localID, identity := range bundle.localSites {
		stat, ok := reported[identity.centralID]
		if !ok {
			continue
		}
		if inst := bundle.manager.proxies[localID]; inst != nil {
			inst.trafficMu.Lock()
			if inst.trafficCycleAuthoritative {
				delta := deltas[identity.centralID]
				// Rebase the Controller baseline and ACK watermark together.
				// The just-committed report must remain billable immediately;
				// it must not disappear until the next config refresh.
				inst.trafficCycleUsage += trafficBillableBytes(inst.trafficCycleMode, delta.CumulativeBytesIn, delta.CumulativeBytesOut)
			}
			inst.trafficAckedCumulativeIn = stat.CumulativeBytesIn
			inst.trafficAckedCumulativeOut = stat.CumulativeBytesOut
			inst.trafficMu.Unlock()
		}
	}
}

// gcRetiredTrafficCounters releases process-stable counters only after the
// Controller ACKed their final snapshot and no request remains admitted.
func (runtime *edgeAgentRuntime) gcRetiredTrafficCounters(finalAcked map[int64]bool) {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	for siteID, counter := range runtime.trafficCounters {
		if !finalAcked[siteID] {
			continue
		}
		if counter == nil || counter.activeRequests.Load() != 0 {
			continue
		}
		if runtime.bundle != nil {
			if _, ok := runtime.bundle.centralSites[siteID]; ok {
				continue
			}
		}
		reported, ok := runtime.siteReported[siteID]
		if !ok || reported.CumulativeBytesIn != counter.cumulativeIn.Load() || reported.CumulativeBytesOut != counter.cumulativeOut.Load() || reported.Requests != counter.requests.Load() {
			continue
		}
		delete(runtime.trafficCounters, siteID)
		delete(runtime.trafficHosts, siteID)
		delete(runtime.siteReported, siteID)
	}
}

func (runtime *edgeAgentRuntime) commitSiteStats(pending edgeSiteStatsPending) {
	acknowledged := make(map[int64]bool, len(pending.current))
	for siteID := range pending.current {
		acknowledged[siteID] = true
	}
	runtime.commitSiteStatsWithACK(pending, acknowledged)
}

// syncSiteTrafficLimits updates the live quota baseline without rebuilding
// the proxy bundle. TrafficCycleUsage is Controller-authoritative usage for
// the current billing cycle; keeping it outside the config hash lets normal
// telemetry refresh the quota state without restarting every site.
func (runtime *edgeAgentRuntime) syncSiteTrafficLimits(config AgentRuntimeConfig) {
	if runtime == nil {
		return
	}
	runtime.mu.RLock()
	bundle := runtime.bundle
	runtime.mu.RUnlock()
	if bundle == nil || bundle.manager == nil || bundle.database == nil {
		return
	}
	runtime.mu.RLock()
	ackedReports := make(map[int64]ProxyRuntimeStat, len(runtime.siteReported))
	for siteID, stat := range runtime.siteReported {
		ackedReports[siteID] = stat
	}
	runtime.mu.RUnlock()
	bundle.manager.mu.RLock()
	defer bundle.manager.mu.RUnlock()
	for _, route := range config.Routes {
		cycleMode := route.TrafficBillingMode
		controllerCycle := cycleMode == trafficBillingModeOutbound || cycleMode == trafficBillingModeBidirectional || route.TrafficCycleStartMS != 0
		if !controllerCycle {
			settings := bundle.database.currentSystemSettings()
			cycleMode = trafficBillingModeLabel(settings.TrafficBillingMode)
		}
		cycleStart := time.Time{}
		if route.TrafficCycleStartMS > 0 {
			cycleStart = time.UnixMilli(route.TrafficCycleStartMS)
		} else if route.TrafficBillingMode == "" {
			// A legacy Controller did not send a cycle marker. Reconstruct the
			// local default only for that wire contract; an empty marker from a
			// current Controller means the configured cycle intentionally has no
			// reset boundary.
			settings := bundle.database.currentSystemSettings()
			cycleStart = trafficCycleStart(time.Now(), settings.TrafficResetDay, timezoneLocation(settings.ScheduleTimezone))
		}
		cycleUsage := route.TrafficCycleUsage
		// Older Controllers did not send a cycle baseline. Preserve their
		// previous behavior as a compatibility fallback; current Controllers
		// always send TrafficCycleStartMS and therefore use the authoritative
		// cycle value above (which may legitimately be zero).
		if route.TrafficCycleStartMS == 0 && route.TrafficBillingMode == "" {
			cycleUsage = route.Site.TrafficUsed
		}
		for localID, identity := range bundle.localSites {
			if identity.centralID != route.SiteID {
				continue
			}
			inst := bundle.manager.proxies[localID]
			if inst == nil {
				continue
			}
			inst.trafficMu.Lock()
			inst.Site.TrafficQuota = route.Site.TrafficQuota
			inst.Site.TrafficUsed = route.Site.TrafficUsed
			inst.Site.TrafficUsedIn = route.Site.TrafficUsedIn
			inst.Site.TrafficUsedOut = route.Site.TrafficUsedOut
			inst.trafficCycleStart = cycleStart
			inst.trafficCycleMode = cycleMode
			inst.trafficCycleUsage = cycleUsage
			inst.trafficCycleAuthoritative = controllerCycle
			acked := ackedReports[route.SiteID]
			inst.trafficAckedCumulativeIn = acked.CumulativeBytesIn
			inst.trafficAckedCumulativeOut = acked.CumulativeBytesOut
			inst.trafficMu.Unlock()
		}
	}
}

func (runtime *edgeAgentRuntime) siteStatsSnapshot() []NodeSiteStat {
	pending := runtime.prepareSiteStats()
	runtime.commitSiteStats(pending)
	return pending.stats
}

func edgeDecodeKey(value string) ([]byte, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(decoded) != sha256.Size {
		return nil, errors.New("runtime key is invalid")
	}
	return decoded, nil
}

func edgeMetadataPath(method, path string) bool {
	if method != http.MethodGet {
		return false
	}
	request, err := http.NewRequest(method, "https://edge.invalid"+path, nil) // #nosec G704 -- fixed non-routable host is used only for local path classification.
	return err == nil && isWatchHistoryMetadataRequest(request)
}

func edgePlaybackSyncPath(path string) bool {
	return strings.Contains(strings.ToLower(path), "/sessions/playing")
}

func edgeClientIP(remote string) string {
	if host, _, err := net.SplitHostPort(remote); err == nil {
		return host
	}
	return remote
}

func (runtime *edgeAgentRuntime) observe(siteIDs map[string]int64, next http.Handler, probeSecret ...[]byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The scheduler probes this endpoint before a site has been assigned to
		// the node. It still requires the per-node runtime key so the endpoint
		// cannot be used as an unauthenticated public liveness oracle.
		if r.Method == http.MethodGet && r.URL.Path == "/.well-known/meridian-agent-health" {
			if len(probeSecret) == 0 || !hmac.Equal([]byte(strings.TrimSpace(r.Header.Get("X-Meridian-Probe"))), []byte(encodeRuntimeKey(probeSecret[0]))) {
				// Return not-found for unauthenticated probes so the endpoint does
				// not disclose that an Agent is present or reveal its identity.
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		host := requestPublicHost(r.Host)
		siteID := siteIDs[host]
		if siteID == 0 {
			http.Error(w, "site not assigned", http.StatusMisdirectedRequest)
			return
		}
		var responseCapture bytes.Buffer
		writer := &edgeStatusWriter{ResponseWriter: w}
		if edgeMetadataPath(r.Method, r.URL.Path) {
			r.Header.Set("Accept-Encoding", "identity")
			writer.capture = &responseCapture
			writer.captureLimit = edgeEventResponseLimit
		}
		var requestBody string
		if edgePlaybackSyncPath(r.URL.Path) && r.Body != nil && (r.ContentLength < 0 || r.ContentLength <= edgeEventBodyLimit) {
			originalBody := r.Body
			body, err := io.ReadAll(io.LimitReader(originalBody, edgeEventBodyLimit+1))
			// Sampling must never consume bytes that the upstream proxy needs. If
			// the body is too large (or a read fails), put the sampled prefix and
			// unread remainder back before the handler. Oversized bodies simply
			// skip telemetry.
			r.Body = &edgeReplayBody{Reader: io.MultiReader(bytes.NewReader(body), originalBody), Closer: originalBody}
			if err == nil && len(body) <= edgeEventBodyLimit {
				requestBody = string(body)
			}
		}
		next.ServeHTTP(writer, r)
		status := writer.status
		if status == 0 {
			status = http.StatusInternalServerError
		}
		runtime.stats.record(host, status, 0, 0, 0, 0, 0)
		responseBody := ""
		if status >= http.StatusOK && status < http.StatusMultipleChoices && strings.Contains(strings.ToLower(w.Header().Get("Content-Type")), "json") {
			responseBody = responseCapture.String()
		}
		authorization := ""
		if edgePlaybackSyncPath(r.URL.Path) {
			authorization = r.Header.Get("Authorization")
		}
		if requestBody == "" && responseBody == "" {
			return
		}
		runtime.queueEvent(NodeRequestEvent{
			SiteID: siteID, Host: host, Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			StatusCode: status, ClientIP: edgeClientIP(r.RemoteAddr), UserAgent: r.UserAgent(),
			Authorization: authorization, Body: requestBody, ContentType: r.Header.Get("Content-Type"),
			ContentEncoding: r.Header.Get("Content-Encoding"), ResponseBody: responseBody,
			ResponseContentType: w.Header().Get("Content-Type"), ResponseContentEncoding: w.Header().Get("Content-Encoding"),
			RecordedAtMS: time.Now().UnixMilli(), Priority: nodeEventPriorityCritical, SkipRequestLog: true,
		})
	})
}

type edgeReplayBody struct {
	io.Reader
	io.Closer
}

// edgeAssetCacheGeneration is deliberately scoped to one route. A change to
// another site (or to the surrounding Agent config envelope) must not cold
// start this site's cache. Only values that affect the upstream response or
// cache policy participate in the generation fingerprint.
func edgeAssetCacheGeneration(site Site, route AgentSiteRoute) string {
	payload := struct {
		TargetURL         string
		PlaybackTargetURL string
		PlaybackMode      string
		StreamHosts       []string
		Headers           map[string][]string
		CacheEnabled      bool
		CacheTTLSec       int
		CacheMaxBytes     int64
		CacheRules        string
		UpstreamHeaders   string
	}{
		TargetURL: site.TargetURL, PlaybackTargetURL: site.PlaybackTargetURL, PlaybackMode: site.PlaybackMode,
		StreamHosts: route.StreamHosts, Headers: route.Headers, CacheEnabled: site.AssetCacheEnabled,
		CacheTTLSec: site.AssetCacheTTLSec, CacheMaxBytes: site.AssetCacheMaxBytes, CacheRules: site.AssetCacheRules,
		UpstreamHeaders: site.StoredUpstreamHeaders,
	}
	data, _ := json.Marshal(payload)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:8])
}

func buildEdgeProxy(config AgentRuntimeConfig, runtime *edgeAgentRuntime) (*edgeProxyBundle, error) {
	dynamicKey, err := edgeDecodeKey(config.DynamicKey)
	if err != nil {
		return nil, err
	}
	probeSecret, err := edgeDecodeKey(config.ProbeSecret)
	if err != nil {
		return nil, err
	}
	database, err := openDB(":memory:")
	if err != nil {
		return nil, err
	}
	database.edgeEphemeral = true
	bundle := &edgeProxyBundle{database: database, localSites: make(map[int64]edgeSiteIdentity), centralSites: make(map[int64]struct{})}
	database.edgeTelemetrySink = func(event edgeTelemetryEvent) {
		mapped, ok := edgeTelemetryEventSiteID(event, bundle.localSites)
		if !ok {
			return
		}
		runtime.recordTelemetry(mapped)
	}
	fail := func(err error) (*edgeProxyBundle, error) {
		bundle.close()
		return nil, err
	}
	siteIDs := make(map[string]int64, len(config.Routes))
	localSites := make(map[int64]edgeSiteIdentity, len(config.Routes))
	runtimeSites := make(map[string]Site, len(config.Routes))
	runtime.mu.RLock()
	ackedReports := make(map[int64]ProxyRuntimeStat, len(runtime.siteReported))
	for siteID, stat := range runtime.siteReported {
		ackedReports[siteID] = stat
	}
	runtime.mu.RUnlock()
	for _, route := range config.Routes {
		site := route.Site
		site.ID = 0
		site.runtimeTrafficCounter = runtime.trafficCounterFor(route.SiteID, route.Host)
		// Site icons are Controller/UI metadata. The Agent's ephemeral site
		// database has no uploaded icon pack, so carrying these fields into
		// CreateSiteRecord would make an otherwise valid runtime config fail
		// with “selected icon does not exist in the current icon pack”.
		site.IconName = ""
		site.IconURL = ""
		site.PublicHost = strings.ToLower(strings.TrimSpace(route.Host))
		site.IngressMode = ingressModeHost
		site.PathPrefix = ""
		site.TargetURL = route.TargetURL
		site.PlaybackTargetURL = route.PlaybackTargetURL
		site.PlaybackMode = route.PlaybackMode
		site.FailoverTargets = route.FailoverTargets
		site.StoredFailoverLines = route.FailoverLines
		site.StreamHosts = route.StreamHostsRaw
		site.StoredUpstreamHeaders = route.UpstreamHeaders
		site.StoredDynamicDiscoverySources = route.DynamicSources
		site.StoredDynamicDomainRules = route.DynamicRules
		site.Enabled = true
		if route.TrafficBillingMode == trafficBillingModeOutbound || route.TrafficBillingMode == trafficBillingModeBidirectional || route.TrafficCycleStartMS != 0 {
			site.runtimeTrafficCycleConfigured = true
			site.runtimeTrafficCycleUsage = route.TrafficCycleUsage
			site.runtimeTrafficCycleStartMS = route.TrafficCycleStartMS
			site.runtimeTrafficBillingMode = route.TrafficBillingMode
			acked := ackedReports[route.SiteID]
			site.runtimeTrafficAckedCumulativeIn = acked.CumulativeBytesIn
			site.runtimeTrafficAckedCumulativeOut = acked.CumulativeBytesOut
		}
		if route.SiteID > 0 {
			site.AssetCacheNamespace = fmt.Sprintf("site-%d-config-%s", route.SiteID, edgeAssetCacheGeneration(site, route))
		}
		created, createErr := database.CreateSiteRecord(site)
		if createErr != nil {
			return fail(fmt.Errorf("site %d runtime config: %w", route.SiteID, createErr))
		}
		if _, updateErr := database.db.Exec("UPDATE sites SET enabled=1 WHERE id=?", created.ID); updateErr != nil {
			return fail(updateErr)
		}
		created.AssetCacheNamespace = site.AssetCacheNamespace
		// CreateSiteRecord hydrates the persisted fields from the ephemeral
		// database, so copy the controller-authoritative quota snapshot back onto
		// the runtime-only fields before StartSite installs the proxy instance.
		created.runtimeTrafficCycleUsage = site.runtimeTrafficCycleUsage
		created.runtimeTrafficCycleStartMS = site.runtimeTrafficCycleStartMS
		created.runtimeTrafficBillingMode = site.runtimeTrafficBillingMode
		created.runtimeTrafficCycleConfigured = site.runtimeTrafficCycleConfigured
		created.runtimeTrafficAckedCumulativeIn = site.runtimeTrafficAckedCumulativeIn
		created.runtimeTrafficAckedCumulativeOut = site.runtimeTrafficAckedCumulativeOut
		created.runtimeTrafficCounter = site.runtimeTrafficCounter
		siteIDs[site.PublicHost] = route.SiteID
		localSites[created.ID] = edgeSiteIdentity{centralID: route.SiteID, host: site.PublicHost}
		bundle.localSites[created.ID] = localSites[created.ID]
		if route.SiteID > 0 {
			bundle.centralSites[route.SiteID] = struct{}{}
		}
		runtimeSites[site.PublicHost] = *created
	}
	database.edgeRequestLogSink = func(event requestLogEvent) {
		identity, ok := localSites[event.SiteID]
		if !ok {
			return
		}
		runtime.queueEvent(NodeRequestEvent{
			SiteID: identity.centralID, Host: identity.host, Method: event.Method, Path: event.Path,
			StatusCode: event.StatusCode, ClientIP: event.ClientIP, UserAgent: event.UserAgent,
			RecordedAtMS: time.Now().UnixMilli(), ResourceCategory: event.ResourceCategory,
			UpstreamUserAgent: event.UpstreamUserAgent, BackendAddress: event.BackendAddress,
			InboundColo: event.InboundColo, OutboundColo: event.OutboundColo,
		})
	}
	database.edgeWatchHistorySink = func(event watchHistoryEvent) bool {
		identity, ok := localSites[event.SiteID]
		if !ok || identity.centralID <= 0 {
			return false
		}
		// The Agent's JWT secret is intentionally process-local and is not the
		// Controller secret. Do not forward an unusable token ciphertext; the
		// parsed playback identity and media fields are sufficient for history.
		mapped := event
		mapped.SiteID = identity.centralID
		mapped.TokenCiphertext = ""
		return runtime.queueEvent(NodeRequestEvent{
			SiteID: identity.centralID, Host: identity.host, Method: http.MethodPost,
			Path: "/Sessions/Playing/Progress", StatusCode: http.StatusNoContent,
			RecordedAtMS: mapped.ObservedAtMS, ResourceCategory: requestLogCategoryPlaybackSync,
			Priority: nodeEventPriorityCritical, SkipRequestLog: true, WatchHistory: &mapped,
		})
	}
	manager := NewProxyManager(database, nil)
	bundle.manager = manager
	if runtime.resolver != nil {
		manager.dynamicRuntime.resolver = runtime.resolver
	}
	manager.dynamicTransportFactory = runtime.transport
	manager.SetHostOnlyIngressSafe(true)
	cache := newAssetCache(filepath.Join(runtime.stateDir, "asset-cache"))
	manager.SetAssetCache(cache)
	for _, site := range runtimeSites {
		if site.AssetCacheNamespace == "" {
			continue
		}
		if err := cache.gcSiteGenerations(assetCacheNamespacePrefix(site.AssetCacheNamespace), site.AssetCacheNamespace); err != nil {
			return fail(fmt.Errorf("gc site %s asset cache generations: %w", site.AssetCacheNamespace, err))
		}
	}
	if err := manager.ConfigureDynamicDiscovery(dynamicKey, "", config.HTTPSPort, nil); err != nil {
		return fail(err)
	}
	for _, route := range config.Routes {
		created := runtimeSites[strings.ToLower(strings.TrimSpace(route.Host))]
		created.RuntimeUpstreamHeaders = route.Headers
		if err := manager.StartSite(created); err != nil {
			return fail(err)
		}
	}
	router := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/meridian-agent-health" {
			w.Header().Set("X-Meridian-Node", config.NodeGUID)
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		host := requestPublicHost(r.Host)
		handler, configured := manager.PublicHostHandler(host)
		if !configured || handler == nil {
			http.Error(w, "site not assigned", http.StatusMisdirectedRequest)
			return
		}
		ctx := context.WithValue(r.Context(), publicHostIngressContextKey{}, true)
		handler.ServeHTTP(w, r.WithContext(ctx))
	})
	bundle.handler = runtime.observe(siteIDs, router, probeSecret)
	return bundle, nil
}

func (runtime *edgeAgentRuntime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	runtime.mu.RLock()
	handler := runtime.handler
	runtime.mu.RUnlock()
	if handler == nil {
		http.Error(w, "configuration unavailable", http.StatusServiceUnavailable)
		return
	}
	handler.ServeHTTP(w, r)
}

func (runtime *edgeAgentRuntime) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	runtime.mu.RLock()
	certificate := runtime.certificate
	runtime.mu.RUnlock()
	if certificate == nil {
		return nil, errors.New("TLS certificate is unavailable")
	}
	return certificate, nil
}

func (runtime *edgeAgentRuntime) stopServer(force ...bool) {
	runtime.mu.Lock()
	server := runtime.server
	runtime.server = nil
	runtime.mu.Unlock()
	if server != nil {
		forceClose := true
		if len(force) > 0 {
			forceClose = force[0]
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		shutdownErr := server.Shutdown(ctx)
		cancel()
		// Shutdown closes the listener promptly, but may leave active handlers
		// alive until the grace period expires. Close is idempotent and ensures a
		// failed config transaction can never leave a listener bound to a port.
		if shutdownErr != nil && forceClose {
			_ = server.Close()
		}
	}
}

func (runtime *edgeAgentRuntime) startServer(port int) error {
	listen := runtime.listen
	if listen == nil {
		listen = net.Listen
	}
	listener, err := listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler: runtime, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 64 << 10,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: runtime.getCertificate},
	}
	runtime.mu.Lock()
	runtime.server = server
	runtime.mu.Unlock()
	go func() {
		if err := server.Serve(tls.NewListener(listener, server.TLSConfig)); err != nil && !errors.Is(err, http.ErrServerClosed) {
			runtime.mu.Lock()
			if runtime.server == server {
				runtime.listenerError = agentStatusError(err)
				runtime.server = nil
			}
			runtime.mu.Unlock()
			fmt.Fprintf(os.Stderr, "Meridian Agent listener failed: %v\n", err)
		}
	}()
	return nil
}

// rollbackApply restores the in-memory candidate state and, when the old
// listener was stopped for the attempted transition, puts that listener back.
// A failed listener restore is part of the returned error; silently reporting
// the old state as healthy would leave the Agent unreachable while its status
// claims that the rollback succeeded.
func (runtime *edgeAgentRuntime) rollbackApply(old edgeAgentRuntimeState, listenerWasStopped, newServerStarted bool, cause error) error {
	if newServerStarted {
		runtime.stopServer()
	}
	runtime.mu.Lock()
	runtime.nodeGUID = old.nodeGUID
	runtime.appliedHash = old.appliedHash
	runtime.appliedRevision = old.appliedRevision
	runtime.port = old.port
	runtime.certificate = old.certificate
	runtime.handler = old.handler
	runtime.bundle = old.bundle
	runtime.cacheClearGeneration = old.cacheClearGeneration
	runtime.siteCounterEpoch = old.siteCounterEpoch
	runtime.siteReported = old.siteReported
	runtime.listenerError = old.listenerError
	if listenerWasStopped {
		runtime.server = nil
	} else {
		runtime.server = old.server
	}
	runtime.mu.Unlock()
	if listenerWasStopped && old.server != nil {
		if err := runtime.startServer(old.port); err != nil {
			restoreErr := fmt.Errorf("restore old listener on port %d: %w", old.port, err)
			runtime.mu.Lock()
			runtime.listenerError = restoreErr.Error()
			runtime.server = nil
			runtime.mu.Unlock()
			return errors.Join(cause, restoreErr)
		}
	}
	return cause
}

func (runtime *edgeAgentRuntime) apply(config AgentRuntimeConfig) (retErr error) {
	defer func() {
		runtime.mu.Lock()
		if retErr != nil {
			runtime.applyError = agentStatusError(retErr)
			runtime.applyErrorAtMS = time.Now().UnixMilli()
			runtime.applyFailures++
		} else {
			runtime.applyError = ""
			runtime.applyErrorAtMS = 0
			runtime.applyFailures = 0
		}
		runtime.mu.Unlock()
	}()
	if err := validateAgentConfigEnvelope(config); err != nil {
		return err
	}
	var err error
	if len(config.Routes) > 0 && (config.HTTPSPort < 1 || config.HTTPSPort > 65535) {
		return errors.New("Agent port is invalid")
	}
	var certificate *tls.Certificate
	if len(config.Routes) > 0 {
		parsed, err := tls.X509KeyPair([]byte(config.CertificatePEM), []byte(config.PrivateKeyPEM))
		if err != nil {
			return err
		}
		certificate = &parsed
	}
	bundle, err := buildEdgeProxy(config, runtime)
	if err != nil {
		return err
	}
	runtime.mu.RLock()
	oldState := edgeAgentRuntimeState{
		port: runtime.port, server: runtime.server, bundle: runtime.bundle,
		nodeGUID: runtime.nodeGUID, appliedHash: runtime.appliedHash, appliedRevision: runtime.appliedRevision, certificate: runtime.certificate, handler: runtime.handler,
		cacheClearGeneration: runtime.cacheClearGeneration, siteCounterEpoch: runtime.siteCounterEpoch,
		listenerError: runtime.listenerError,
	}
	oldSiteReported := make(map[int64]ProxyRuntimeStat, len(runtime.siteReported))
	for siteID, value := range runtime.siteReported {
		oldSiteReported[siteID] = value
	}
	runtime.mu.RUnlock()
	oldState.siteReported = oldSiteReported
	needsListener := len(config.Routes) > 0
	listenerChanged := oldState.port != config.HTTPSPort || (oldState.server == nil) != !needsListener
	if listenerChanged {
		// Closing the listener stops new connections, while a non-forcing
		// shutdown leaves already admitted playback/WebSocket handlers alive.
		runtime.stopServer(false)
	}
	runtime.mu.Lock()
	runtime.nodeGUID = config.NodeGUID
	runtime.port = config.HTTPSPort
	runtime.certificate = certificate
	runtime.handler = bundle.handler
	runtime.bundle = bundle
	// siteReported is a Controller ACK watermark. Stable per-site counters are
	// shared by old and new bundles, so this watermark remains valid across a
	// routing-only hot apply.
	runtime.siteReported = oldState.siteReported
	runtime.mu.Unlock()
	newServerStarted := false
	if listenerChanged && needsListener {
		if err := runtime.startServer(config.HTTPSPort); err != nil {
			bundle.close()
			return runtime.rollbackApply(oldState, listenerChanged, false, err)
		}
		newServerStarted = true
	}
	if config.CacheClearGeneration > oldState.cacheClearGeneration {
		if err := bundle.manager.ClearAssetCache(); err != nil {
			// Stop accepting new candidate requests, restore the old handler, and
			// only then drain the failed bundle. Shutdown may time out while an
			// admitted request is still using the candidate, so closing it before
			// the runtime points back at the old bundle can race that request.
			if newServerStarted {
				runtime.stopServer()
			}
			rollbackErr := runtime.rollbackApply(oldState, listenerChanged, false, err)
			runtime.beginBundleDrain(bundle)
			return rollbackErr
		}
	}
	runtime.mu.Lock()
	runtime.appliedHash = config.ConfigHash
	runtime.appliedRevision = config.ConfigRevision
	// A counter epoch identifies the Agent process, not a routing bundle. Keep
	// it stable across hot applies so draining requests remain in one stream.
	if runtime.siteCounterEpoch == 0 {
		runtime.siteCounterEpoch = 1
	}
	if config.CacheClearGeneration > runtime.cacheClearGeneration {
		runtime.cacheClearGeneration = config.CacheClearGeneration
	}
	runtime.listenerError = ""
	runtime.mu.Unlock()
	if len(config.ForceStopSiteIDs) > 0 {
		forceCtx, forceCancel := context.WithTimeout(context.Background(), 5*time.Second)
		runtime.forceStopCentralSites(forceCtx, config.ForceStopSiteIDs, oldState.bundle)
		forceCancel()
	}
	if oldState.bundle != nil {
		// Hot config changes are not a security revocation. Existing playback and
		// WebSocket streams finish naturally; close() cancels them on shutdown.
		runtime.beginBundleDrain(oldState.bundle)
	}
	return nil
}

func validateAgentConfigEnvelope(config AgentRuntimeConfig) error {
	if config.SchemaVersion != agentConfigSchemaVersion || config.ConfigHash == "" || config.NodeGUID == "" {
		return errors.New("Agent configuration is invalid")
	}
	expectedHash, err := agentConfigHash(config)
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(expectedHash), []byte(config.ConfigHash)) {
		// During rolling upgrades a controller may deliberately use the
		// v1.9.29 hash for an Agent that has not advertised its version yet.
		legacyHash, legacyErr := agentConfigLegacyHash(config)
		if legacyErr != nil || !hmac.Equal([]byte(legacyHash), []byte(config.ConfigHash)) {
			return errors.New("Agent configuration checksum mismatch")
		}
	}
	if len(config.Routes) > 0 && (config.HTTPSPort < 1 || config.HTTPSPort > 65535) {
		return errors.New("Agent port is invalid")
	}
	return nil
}

const maxAgentStatusErrorLength = 1024

func agentStatusError(err error) string {
	if err == nil {
		return ""
	}
	value := strings.TrimSpace(err.Error())
	if len(value) <= maxAgentStatusErrorLength {
		return value
	}
	return value[:maxAgentStatusErrorLength]
}

func (runtime *edgeAgentRuntime) status() (string, int64, string, string, int64, int64) {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	return runtime.appliedHash, runtime.appliedRevision, runtime.listenerError, runtime.applyError, runtime.applyErrorAtMS, runtime.applyFailures
}

// nextTelemetrySequence is shared by the full and lightweight report loops.
// Their channel-local sequences intentionally remain independent, while this
// monotonic value provides the Controller with one ordering boundary.
func (runtime *edgeAgentRuntime) nextTelemetrySequence() int64 {
	if runtime == nil {
		return 0
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.telemetrySequence++
	if runtime.telemetrySequence <= 0 {
		runtime.telemetrySequence = 1
	}
	return runtime.telemetrySequence
}

func (runtime *edgeAgentRuntime) close() {
	runtime.stopServer()
	runtime.mu.Lock()
	bundle := runtime.bundle
	runtime.bundle = nil
	cancel := runtime.drainCancel
	runtime.drainCancel = nil
	runtime.drainContext = nil
	runtime.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	bundle.close()
}

func edgeNormalizeController(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("controller must be an absolute HTTPS URL")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return strings.TrimRight(parsed.String(), "/"), nil
}

func edgeLoadState(path string) (edgeAgentState, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- administrator-selected local Agent state path.
	if errors.Is(err, os.ErrNotExist) {
		return edgeAgentState{}, nil
	}
	if err != nil {
		return edgeAgentState{}, err
	}
	var state edgeAgentState
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	return state, nil
}

func edgeSaveState(path string, state edgeAgentState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	_ = temporary.Chmod(0o600)
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

type edgeAPIError struct {
	StatusCode int
	AgentState string
	Message    string
}

func (e *edgeAPIError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func isEdgeAgentRevoked(err error) bool {
	var apiErr *edgeAPIError
	return errors.As(err, &apiErr) && strings.EqualFold(strings.TrimSpace(apiErr.AgentState), "revoked")
}

// edgeQuiesceRevoked closes the data plane before recording the durable
// marker. If the disk is unavailable, returning would let systemd restart the
// same revoked credential forever; stay inert until an operator stops or
// repairs the service instead.
func edgeQuiesceRevoked(ctx context.Context, runtime *edgeAgentRuntime, marker string) error {
	if runtime != nil {
		runtime.close()
	}
	if err := os.WriteFile(marker, []byte("revoked\n"), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "Meridian Agent is revoked but cannot persist revoked state: %v; remaining offline until stopped\n", err)
		<-ctx.Done()
		return nil
	}
	return nil
}

func edgeEnrollmentTokenAvailable(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	data, err := os.ReadFile(path) // #nosec G304 -- the enrollment path is supplied by the service configuration.
	return err == nil && strings.TrimSpace(string(data)) != ""
}

func edgeAPIRequest(ctx context.Context, client *http.Client, method, endpoint, token string, body, output any) error {
	return edgeAPIRequestWithHeaders(ctx, client, method, endpoint, token, body, output, nil)
}

func edgeAPIRequestWithHeaders(ctx context.Context, client *http.Client, method, endpoint, token string, body, output any, headers http.Header) error {
	var reader io.Reader
	var payload []byte
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = data
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	for key, values := range headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
		if len(payload) >= 1024 {
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			if _, err := writer.Write(payload); err != nil {
				_ = writer.Close()
				return err
			}
			if err := writer.Close(); err != nil {
				return err
			}
			request.Body = io.NopCloser(bytes.NewReader(compressed.Bytes()))
			request.ContentLength = int64(compressed.Len())
			request.Header.Set("Content-Encoding", "gzip")
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Error      string `json:"error"`
			AgentState string `json:"agent_state"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&failure)
		if failure.Error == "" {
			failure.Error = response.Status
		}
		state := strings.TrimSpace(response.Header.Get("X-Meridian-Agent-State"))
		if state == "" {
			state = failure.AgentState
		}
		return &edgeAPIError{StatusCode: response.StatusCode, AgentState: state, Message: failure.Error}
	}
	if output == nil {
		_, err = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return err
	}
	return json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(output)
}

type edgeReportAck struct {
	AcceptedSiteIDs             []int64  `json:"accepted_site_ids"`
	DiscardedSiteIDs            []int64  `json:"discarded_site_ids"`
	AcceptedMediaSiteIDs        []int64  `json:"accepted_media_site_ids"`
	DiscardedMediaSiteIDs       []int64  `json:"discarded_media_site_ids"`
	AcceptedRetentionSiteIDs    []int64  `json:"accepted_retention_site_ids"`
	DiscardedRetentionSiteIDs   []int64  `json:"discarded_retention_site_ids"`
	AcceptedObservationSiteIDs  []int64  `json:"accepted_observation_site_ids"`
	DiscardedObservationSiteIDs []int64  `json:"discarded_observation_site_ids"`
	AcceptedEventIDs            []int64  `json:"accepted_event_ids"`
	AcceptedEventUIDs           []string `json:"accepted_event_uids"`
	DiscardedEventIDs           []int64  `json:"discarded_event_ids"`
	DiscardedEventUIDs          []string `json:"discarded_event_uids"`
	ConfigHash                  string   `json:"config_hash"`
	ConfigChanged               bool     `json:"config_changed"`
}

type edgeWSReportClient struct {
	mu         sync.Mutex
	controller string
	token      string
	conn       *websocket.Conn
	nextTry    time.Time
}

func newEdgeWSReportClient(controller, token string) *edgeWSReportClient {
	return &edgeWSReportClient{controller: controller, token: token}
}

func (c *edgeWSReportClient) closeLocked() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

func (c *edgeWSReportClient) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked()
}

func (c *edgeWSReportClient) connectLocked(ctx context.Context) error {
	if c.conn != nil {
		return nil
	}
	if !c.nextTry.IsZero() && time.Now().Before(c.nextTry) {
		return errors.New("websocket retry backoff")
	}
	parsed, err := url.Parse(c.controller + "/api/agent/ws")
	if err != nil {
		return err
	}
	if parsed.Scheme == "https" {
		parsed.Scheme = "wss"
	} else {
		parsed.Scheme = "ws"
	}
	origin := *parsed
	origin.Path = "/"
	if origin.Scheme == "wss" {
		origin.Scheme = "https"
	} else {
		origin.Scheme = "http"
	}
	config, err := websocket.NewConfig(parsed.String(), origin.String())
	if err != nil {
		return err
	}
	config.Header.Set("Authorization", "Bearer "+c.token)
	config.TlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	conn, err := config.DialContext(ctx)
	if err != nil {
		c.nextTry = time.Now().Add(30 * time.Second)
		return err
	}
	conn.MaxPayloadBytes = maxAgentReportBodyBytes
	c.conn = conn
	c.nextTry = time.Time{}
	return nil
}

func (c *edgeWSReportClient) report(ctx context.Context, payload NodeReport) (edgeReportAck, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.connectLocked(dialCtx); err != nil {
		return edgeReportAck{}, err
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		c.closeLocked()
		return edgeReportAck{}, err
	}
	if err := websocket.JSON.Send(c.conn, payload); err != nil {
		c.closeLocked()
		c.nextTry = time.Now().Add(30 * time.Second)
		return edgeReportAck{}, err
	}
	if err := c.conn.SetReadDeadline(time.Now().Add(45 * time.Second)); err != nil {
		c.closeLocked()
		return edgeReportAck{}, err
	}
	var ack edgeReportAck
	if err := websocket.JSON.Receive(c.conn, &ack); err != nil {
		c.closeLocked()
		c.nextTry = time.Now().Add(30 * time.Second)
		return edgeReportAck{}, err
	}
	return ack, nil
}

func edgeHTTPTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 32
	transport.MaxIdleConnsPerHost = 8
	transport.IdleConnTimeout = 90 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	return transport
}

func edgeEnroll(ctx context.Context, client *http.Client, controller, tokenFile, statePath string) (edgeAgentState, error) {
	data, err := os.ReadFile(tokenFile) // #nosec G304 -- administrator-selected enrollment file.
	if err != nil {
		return edgeAgentState{}, err
	}
	var response struct {
		NodeGUID string `json:"node_guid"`
		Token    string `json:"agent_token"`
	}
	if err := edgeAPIRequestWithHeaders(ctx, client, http.MethodPost, controller+"/api/agent/enroll", strings.TrimSpace(string(data)), nil, &response, http.Header{
		agentPlatformHeader: []string{goruntime.GOOS + "/" + goruntime.GOARCH},
	}); err != nil {
		return edgeAgentState{}, err
	}
	state := edgeAgentState{NodeGUID: response.NodeGUID, Token: response.Token}
	if err := edgeSaveState(statePath, state); err != nil {
		return edgeAgentState{}, err
	}
	_ = os.Remove(tokenFile)
	return state, nil
}

func edgeDefaultInterface() string {
	file, err := os.Open("/proc/net/route")
	if err == nil {
		defer file.Close()
		scanner := bufio.NewScanner(file)
		_ = scanner.Scan()
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) >= 4 && fields[1] == "00000000" {
				flags, parseErr := strconv.ParseUint(fields[3], 16, 32)
				if parseErr == nil && flags&2 != 0 {
					return fields[0]
				}
			}
		}
	}
	interfaces, _ := net.Interfaces()
	for _, candidate := range interfaces {
		if candidate.Flags&net.FlagUp != 0 && candidate.Flags&net.FlagLoopback == 0 {
			return candidate.Name
		}
	}
	return ""
}

func edgeCounter(interfaceName, name string) (int64, error) {
	if name != "rx_bytes" && name != "tx_bytes" {
		return 0, errors.New("invalid counter")
	}
	data, err := os.ReadFile(filepath.Join("/sys/class/net", interfaceName, "statistics", name)) // #nosec G304 -- interface is discovered from the kernel route table.
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
}

func edgeBootID() string {
	random := make([]byte, 8)
	_, _ = rand.Read(random)
	prefix := ""
	if data, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		prefix = strings.TrimSpace(string(data)) + ":"
	}
	return prefix + hex.EncodeToString(random)
}

func edgeCounterEpoch(interfaceName string) string {
	kernelBootID := "unknown"
	if data, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		kernelBootID = strings.TrimSpace(string(data))
	}
	return kernelBootID + ":" + interfaceName
}

func edgeCollect(sessionID string, sequence int64) (NodeReport, error) {
	interfaceName := edgeDefaultInterface()
	if interfaceName == "" {
		return NodeReport{}, errors.New("no active network interface")
	}
	rx, err := edgeCounter(interfaceName, "rx_bytes")
	if err != nil {
		return NodeReport{}, err
	}
	tx, err := edgeCounter(interfaceName, "tx_bytes")
	if err != nil {
		return NodeReport{}, err
	}
	return NodeReport{BootID: sessionID, ReportSessionID: sessionID, CounterEpoch: edgeCounterEpoch(interfaceName), Sequence: sequence, InterfaceName: interfaceName, RXBytes: rx, TXBytes: tx, AgentVersion: appVersion}, nil
}

func edgeLiveReportLoop(ctx context.Context, client *http.Client, controller, token, sessionID string, runtime *edgeAgentRuntime) error {
	if runtime == nil {
		return nil
	}
	sequence := int64(0)
	send := func() error {
		sequence++
		report := NodeLiveReport{
			ReportSessionID:   sessionID,
			CounterEpoch:      edgeCounterEpoch(edgeDefaultInterface()),
			Sequence:          sequence,
			TelemetrySequence: runtime.nextTelemetrySequence(),
			SampledAtMS:       time.Now().UnixMilli(),
			SiteStats:         runtime.liveSiteTrafficSnapshot(),
		}
		var ack struct{}
		return edgeAPIRequest(ctx, client, http.MethodPost, controller+"/api/agent/live", token, report, &ack)
	}
	// Send one sample immediately so a freshly applied route does not wait for
	// the first ticker boundary.
	if err := send(); err != nil && isEdgeAgentRevoked(err) {
		return err
	}
	ticker := time.NewTicker(agentLiveReportInterval)
	defer ticker.Stop()
	var lastLog time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := send(); err != nil {
				if isEdgeAgentRevoked(err) {
					return err
				}
				if lastLog.IsZero() || time.Since(lastLog) >= 30*time.Second {
					fmt.Fprintf(os.Stderr, "Meridian Agent live report failed: %v\n", err)
					lastLog = time.Now()
				}
			}
		}
	}
}

// edgeReportEventsByBudget takes both the protocol item limit and the encoded
// JSON budget into account. Without the byte budget a queue containing large
// metadata responses can exceed the Controller limit forever: the same head
// events are retried on every heartbeat and no ACK is ever possible.
func edgeReportEventsByBudget(report NodeReport, events []NodeRequestEvent) []NodeRequestEvent {
	if len(events) == 0 {
		return nil
	}
	if len(events) > maxNodeRequestEventsPerReport {
		events = events[:maxNodeRequestEventsPerReport]
	}
	selected := make([]NodeRequestEvent, 0, len(events))
	for _, event := range events {
		candidate := append(append([]NodeRequestEvent(nil), selected...), event)
		report.Events = candidate
		encoded, err := json.Marshal(report)
		if err != nil {
			break
		}
		if len(encoded) > targetAgentReportBytes {
			// A single event is bounded well below the report limit. If the
			// non-event payload itself ever grows beyond the target, retain no
			// event in this report so the queue can still make progress after
			// the base payload is reduced by a later protocol change.
			if len(selected) == 0 {
				return nil
			}
			break
		}
		selected = candidate
	}
	return selected
}

func edgeExecutableDigest() (string, string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", "", err
	}
	file, err := os.Open(executable) // #nosec G304 -- current executable path supplied by the kernel.
	if err != nil {
		return "", "", err
	}
	defer file.Close()
	digest := sha256.New()
	_, err = io.Copy(digest, file)
	return executable, hex.EncodeToString(digest.Sum(nil)), err
}

func edgeMaybeUpdate(ctx context.Context, client *http.Client, controller, token string, config AgentRuntimeConfig) error {
	if len(config.AgentSHA256) != sha256.Size*2 {
		return nil
	}
	executable, current, err := edgeExecutableDigest()
	if err != nil || strings.EqualFold(current, config.AgentSHA256) {
		return err
	}
	downloadURL := strings.TrimSpace(config.AgentDownloadURL)
	if downloadURL == "" {
		manifest, manifestErr := edgeFetchAgentManifest(ctx, client, controller, token)
		if manifestErr != nil {
			return manifestErr
		}
		downloadURL = manifest.DownloadURL
		if !strings.EqualFold(manifest.SHA256, config.AgentSHA256) {
			return errors.New("Agent release checksum differs from controller configuration")
		}
	}
	platform := goruntime.GOOS + "/" + goruntime.GOARCH
	asset, assetErr := agentReleaseAssetName(platform)
	if assetErr != nil {
		return assetErr
	}
	if err := validateAgentReleaseDownloadURL(downloadURL, strings.TrimSpace(config.AgentVersion), asset); err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return err
	}
	releaseClient := *client
	releaseClient.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("refusing non-HTTPS Agent release redirect")
		}
		return nil
	}
	response, err := releaseClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Agent update returned %s", response.Status)
	}
	temporary, err := os.CreateTemp(filepath.Dir(executable), ".meridian-agent-update-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	digest := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, digest), io.LimitReader(response.Body, 128<<20))
	if copyErr != nil || written <= 0 || !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), config.AgentSHA256) {
		_ = temporary.Close()
		return errors.New("Agent update checksum mismatch")
	}
	if err := temporary.Chmod(0o755); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, executable); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(executable)); err != nil {
		return err
	}
	return errEdgeAgentUpdated
}

func syncDirectory(path string) error {
	if goruntime.GOOS == "windows" {
		// Windows does not expose a directory fsync operation. The file itself
		// is flushed before rename; directory metadata durability is best effort.
		return nil
	}
	directory, err := os.Open(path) // #nosec G304 G703 -- path is the parent directory of the Agent executable selected by Meridian.
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func edgeFetchAgentManifest(ctx context.Context, client *http.Client, controller, token string) (AgentBinaryManifest, error) {
	var manifest AgentBinaryManifest
	if err := edgeAPIRequestWithHeaders(ctx, client, http.MethodGet, controller+"/api/agent/manifest", token, nil, &manifest, http.Header{
		agentPlatformHeader: []string{goruntime.GOOS + "/" + goruntime.GOARCH},
	}); err != nil {
		return AgentBinaryManifest{}, err
	}
	asset, err := agentReleaseAssetName(manifest.Platform)
	if err != nil {
		return AgentBinaryManifest{}, err
	}
	if !strings.EqualFold(manifest.Platform, goruntime.GOOS+"/"+goruntime.GOARCH) || !validAgentReleaseVersion(manifest.Version) || len(manifest.SHA256) != sha256.Size*2 {
		return AgentBinaryManifest{}, errors.New("controller returned an invalid Agent release manifest")
	}
	if err := validateAgentReleaseDownloadURL(manifest.DownloadURL, manifest.Version, asset); err != nil {
		return AgentBinaryManifest{}, err
	}
	return manifest, nil
}

func runEdgeAgent() error {
	flags := flag.NewFlagSet("meridian-agent", flag.ContinueOnError)
	controllerValue := flags.String("controller", "", "controller URL")
	statePath := flags.String("state", "/var/lib/meridian-agent/state.json", "state path")
	tokenFile := flags.String("enroll-token-file", "", "one-time enrollment token file")
	once := flags.Bool("once", false, "report once and exit")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	controller, err := edgeNormalizeController(*controllerValue)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Minute, Transport: edgeHTTPTransport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	revokedMarker := *statePath + ".revoked"
	if _, statErr := os.Stat(revokedMarker); statErr == nil {
		if !edgeEnrollmentTokenAvailable(*tokenFile) {
			fmt.Fprintln(os.Stderr, "Meridian Agent token was revoked; run the generated re-enrollment script to start it again.")
			// Keep the service quiescent instead of returning into a
			// Restart=always loop. The process has no listener or runtime at this
			// point; an operator-provided enrollment token causes the service to be
			// stopped and started again, at which point enrollment can proceed.
			<-ctx.Done()
			return nil
		}
		// The current state contains the credential that the Controller revoked.
		// Remove it before loading state so a manually supplied enrollment token
		// (or an interrupted --reenroll install) cannot accidentally keep using
		// the stale token instead of completing enrollment.
		if err := os.Remove(*statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove revoked Agent state: %w", err)
		}
		_ = os.Remove(revokedMarker)
	}
	state, err := edgeLoadState(*statePath)
	if err != nil {
		return err
	}
	if state.Token == "" {
		if strings.TrimSpace(*tokenFile) == "" {
			return errors.New("Agent is not enrolled")
		}
		state, err = edgeEnroll(ctx, client, controller, *tokenFile, *statePath)
		if err != nil {
			return err
		}
	}
	runtime := &edgeAgentRuntime{stateDir: filepath.Dir(*statePath), nodeGUID: state.NodeGUID}
	spoolDir := filepath.Join(runtime.stateDir, "events")
	spoolKey, spoolKeyErr := loadOrCreateSpoolKey(filepath.Join(runtime.stateDir, "spool.key"))
	if spoolKeyErr != nil {
		_ = quarantineEventSpool(filepath.Join(runtime.stateDir, "spool.key"))
		spoolKey, spoolKeyErr = loadOrCreateSpoolKey(filepath.Join(runtime.stateDir, "spool.key"))
	}
	if spoolKeyErr != nil {
		return fmt.Errorf("initialize event spool key: %w", spoolKeyErr)
	}
	legacyDigest := sha256.Sum256([]byte(state.Token))
	if err := runtime.events.initWithRawKey(spoolDir, spoolKey, legacyDigest[:]); err != nil {
		// Re-enrollment, interrupted upgrades, or manual edits must never trap
		// systemd in a restart loop. Preserve the corrupt spool for inspection
		// and continue with an empty queue encrypted by the independent key.
		if quarantineErr := quarantineEventSpool(filepath.Join(spoolDir, "events.json")); quarantineErr == nil {
			runtime.eventSpoolError = "event spool quarantined after load failure: " + err.Error()
			if resetErr := runtime.events.initWithRawKey(spoolDir, spoolKey, nil); resetErr != nil {
				return fmt.Errorf("reset event spool: %w", resetErr)
			}
		} else {
			return fmt.Errorf("load event spool: %w (quarantine failed: %v)", err, quarantineErr)
		}
	}
	defer runtime.close()
	wsReporter := newEdgeWSReportClient(controller, state.Token)
	defer wsReporter.close()
	bootID := edgeBootID()
	liveErrCh := make(chan error, 1)
	liveCancel := func() {}
	liveDone := make(chan struct{})
	close(liveDone)
	if !*once {
		liveCtx, cancelLive := context.WithCancel(ctx)
		liveCancel = cancelLive
		liveDone = make(chan struct{})
		go func() {
			defer close(liveDone)
			if err := edgeLiveReportLoop(liveCtx, client, controller, state.Token, bootID, runtime); err != nil {
				select {
				case liveErrCh <- err:
				default:
				}
			}
		}()
	}
	defer func() {
		liveCancel()
		<-liveDone
	}()
	sequence := int64(0)
	const configRefreshInterval = 60 * time.Second
	const agentUpdateRetryInterval = 5 * time.Minute
	const agentApplyRetryBase = 5 * time.Second
	const agentApplyRetryMax = 60 * time.Second
	lastConfigAt := time.Time{}
	lastUpdateAttempt := time.Time{}
	nextApplyAttempt := time.Time{}
	applyFailures := 0
	var pendingConfig *AgentRuntimeConfig
	for {
		select {
		case liveErr := <-liveErrCh:
			if isEdgeAgentRevoked(liveErr) {
				return edgeQuiesceRevoked(ctx, runtime, revokedMarker)
			}
		default:
		}
		now := time.Now()
		if lastConfigAt.IsZero() || now.Sub(lastConfigAt) >= configRefreshInterval {
			// Record the attempt even when the Controller is temporarily
			// unavailable, otherwise a network outage would create a tight fetch
			// loop while the normal report loop is still running.
			lastConfigAt = now
			var config AgentRuntimeConfig
			if err := edgeAPIRequestWithHeaders(ctx, client, http.MethodGet, controller+"/api/agent/config", state.Token, nil, &config, http.Header{
				agentPlatformHeader: []string{goruntime.GOOS + "/" + goruntime.GOARCH},
				agentVersionHeader:  []string{appVersion},
			}); err != nil {
				if isEdgeAgentRevoked(err) {
					return edgeQuiesceRevoked(ctx, runtime, revokedMarker)
				}
				fmt.Fprintf(os.Stderr, "Meridian Agent config fetch failed: %v\n", err)
			} else if configErr := validateAgentConfigEnvelope(config); configErr != nil {
				fmt.Fprintf(os.Stderr, "Meridian Agent rejected config: %v\n", configErr)
			} else {
				// A new identity must be applied atomically. Updating the old
				// bundle before apply would leak a failed candidate's quota into
				// the still-serving configuration. Hash-stable refreshes are the
				// only path allowed to mutate the live baseline in place.
				appliedHash, appliedRevision, _, _, _, _ := runtime.status()
				if agentConfigIdentityEqual(config, AgentRuntimeConfig{ConfigHash: appliedHash, ConfigRevision: appliedRevision}) {
					runtime.syncSiteTrafficLimits(config)
				}
				// Refresh the pending payload when the Controller sends a newer
				// hash, but do not reset an in-progress retry backoff when the
				// same failing configuration is fetched again after a report ACK.
				if pendingConfig == nil || !agentConfigIdentityEqual(*pendingConfig, config) {
					pendingConfig = &config
					nextApplyAttempt = now
					applyFailures = 0
				} else {
					pendingConfig = &config
				}
				// Applying a valid runtime configuration must not depend on the
				// availability of the optional Agent release service. A GitHub
				// outage should leave the current proxy converged while update
				// retries happen independently in the background.
				if lastUpdateAttempt.IsZero() || now.Sub(lastUpdateAttempt) >= agentUpdateRetryInterval {
					lastUpdateAttempt = now
					if updateErr := edgeMaybeUpdate(ctx, client, controller, state.Token, config); updateErr != nil {
						if errors.Is(updateErr, errEdgeAgentUpdated) {
							return updateErr
						}
						fmt.Fprintf(os.Stderr, "Meridian Agent update failed: %v\n", updateErr)
					}
				}
			}
		}
		if pendingConfig != nil && (nextApplyAttempt.IsZero() || !now.Before(nextApplyAttempt)) {
			applied, appliedRevision, _, _, _, _ := runtime.status()
			if agentConfigIdentityEqual(*pendingConfig, AgentRuntimeConfig{ConfigHash: applied, ConfigRevision: appliedRevision}) {
				pendingConfig = nil
				applyFailures = 0
				nextApplyAttempt = time.Time{}
			} else if err := runtime.apply(*pendingConfig); err != nil {
				fmt.Fprintf(os.Stderr, "Meridian Agent config apply failed: %v\n", err)
				applyFailures++
				delay := agentApplyRetryBase
				for attempt := 1; attempt < applyFailures && delay < agentApplyRetryMax; attempt++ {
					delay *= 2
					if delay >= agentApplyRetryMax {
						delay = agentApplyRetryMax
						break
					}
				}
				nextApplyAttempt = now.Add(delay)
			} else {
				pendingConfig = nil
				applyFailures = 0
				nextApplyAttempt = time.Time{}
			}
		}
		refreshImmediately := false
		sequence++
		report, collectErr := edgeCollect(bootID, sequence)
		if collectErr != nil {
			fmt.Fprintf(os.Stderr, "Meridian Agent traffic collection failed: %v\n", collectErr)
		} else {
			report.AppliedConfigHash, report.AppliedConfigRevision, report.ListenerError, report.ApplyError, report.ApplyErrorAtMS, report.ApplyFailures = runtime.status()
			runtime.mu.RLock()
			report.CacheClearGeneration = runtime.cacheClearGeneration
			report.SiteCounterEpoch = strconv.FormatUint(runtime.siteCounterEpoch, 10)
			runtime.mu.RUnlock()
			report.TelemetrySequence = runtime.nextTelemetrySequence()
			report.EventSpoolError, report.EventQueueDepth = runtime.eventSpoolStatus()
			report.EventDropped = runtime.events.droppedCount()
			pendingStats := runtime.prepareSiteStats()
			pendingTelemetry := runtime.prepareTelemetry()
			report.SiteStats = pendingStats.stats
			report.MediaCounts, report.Retention, report.Observations = pendingTelemetry.media, pendingTelemetry.retention, pendingTelemetry.observations
			report.Events = edgeReportEventsByBudget(report, runtime.events.snapshot())
			ack, wsErr := wsReporter.report(ctx, report)
			if wsErr != nil {
				wsErr = edgeAPIRequest(ctx, client, http.MethodPost, controller+"/api/agent/report", state.Token, report, &ack)
			}
			if wsErr != nil {
				if isEdgeAgentRevoked(wsErr) {
					return edgeQuiesceRevoked(ctx, runtime, revokedMarker)
				}
				fmt.Fprintf(os.Stderr, "Meridian Agent report failed: %v\n", wsErr)
			} else {
				runtime.commitSiteStatsWithACK(pendingStats, acknowledgedSiteIDs(ack.AcceptedSiteIDs, ack.DiscardedSiteIDs))
				runtime.commitTelemetryWithACK(
					pendingTelemetry,
					acknowledgedSiteIDs(ack.AcceptedMediaSiteIDs, ack.DiscardedMediaSiteIDs),
					acknowledgedSiteIDs(ack.AcceptedRetentionSiteIDs, ack.DiscardedRetentionSiteIDs),
					acknowledgedSiteIDs(ack.AcceptedObservationSiteIDs, ack.DiscardedObservationSiteIDs),
				)
				accepted := make(map[int64]bool, len(ack.AcceptedEventIDs))
				for _, id := range ack.AcceptedEventIDs {
					accepted[id] = true
				}
				acceptedUIDs := make(map[string]bool, len(ack.AcceptedEventUIDs))
				for _, uid := range ack.AcceptedEventUIDs {
					acceptedUIDs[uid] = true
				}
				discarded := make(map[int64]bool, len(ack.DiscardedEventIDs))
				for _, id := range ack.DiscardedEventIDs {
					discarded[id] = true
				}
				discardedUIDs := make(map[string]bool, len(ack.DiscardedEventUIDs))
				for _, uid := range ack.DiscardedEventUIDs {
					discardedUIDs[uid] = true
				}
				ackEvents := make([]NodeRequestEvent, 0, len(report.Events))
				for _, event := range report.Events {
					if accepted[event.EventID] || discarded[event.EventID] || (event.EventUID != "" && (acceptedUIDs[event.EventUID] || discardedUIDs[event.EventUID])) {
						ackEvents = append(ackEvents, event)
					}
				}
				if err := runtime.events.ack(ackEvents); err != nil {
					runtime.mu.Lock()
					runtime.eventSpoolError = err.Error()
					runtime.mu.Unlock()
				} else if len(ackEvents) > 0 {
					runtime.mu.Lock()
					runtime.eventSpoolError = ""
					runtime.mu.Unlock()
				}
				if ack.ConfigChanged {
					lastConfigAt = time.Time{}
					refreshImmediately = true
				}
			}
		}
		if *once {
			return nil
		}
		if refreshImmediately {
			// The controller has explicitly invalidated this Agent's runtime
			// snapshot. Skip the normal full-report sleep and fetch the
			// replacement configuration immediately.
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(func() time.Duration {
			wait := agentFullReportInterval
			if pendingConfig != nil && !nextApplyAttempt.IsZero() {
				if until := time.Until(nextApplyAttempt); until < wait {
					wait = until
				}
			}
			if runtime.events.depth() > 0 {
				if wait > time.Second {
					return time.Second
				}
			}
			if wait < 0 {
				return 0
			}
			return wait
		}()):
		}
	}
}

// agentConfigIdentityEqual includes the monotonic revision alongside the
// content hash. A revision can change while the serialized runtime content
// returns to an earlier value (for example an edit is reverted); treating the
// hash alone as an acknowledgement would leave the Controller's CAS revision
// permanently pending.
func agentConfigIdentityEqual(left, right AgentRuntimeConfig) bool {
	return strings.TrimSpace(left.ConfigHash) == strings.TrimSpace(right.ConfigHash) &&
		left.ConfigRevision == right.ConfigRevision
}
