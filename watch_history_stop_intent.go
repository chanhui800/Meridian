package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	watchHistoryStopStateLimit = 4096
	watchHistoryStopQueueLimit = 1024
)

type watchHistorySessionKey struct {
	siteID      int64
	sessionHash string
}

type watchHistoryTrustedSession struct {
	playback watchHistoryEvent
	stop     watchHistoryEvent
	touched  time.Time
}

// watchHistoryStopState keeps only bounded credential proofs and already
// observed sessions. No request is detached from its client context or replayed.
// Its small queue keeps SQLite and the Agent spool off the body-read hot path.
type watchHistoryStopState struct {
	mu       sync.Mutex
	sessions map[watchHistorySessionKey]watchHistoryTrustedSession
	queue    chan watchHistoryEvent
	done     chan struct{}
	closed   bool
}

func newWatchHistoryStopState(database *DB) *watchHistoryStopState {
	state := &watchHistoryStopState{
		sessions: make(map[watchHistorySessionKey]watchHistoryTrustedSession),
		queue:    make(chan watchHistoryEvent, watchHistoryStopQueueLimit),
		done:     make(chan struct{}),
	}
	go func() {
		defer close(state.done)
		for event := range state.queue {
			if !database.EnqueueWatchHistory(event) {
				state.failed(event)
			}
		}
	}()
	return state
}

func (state *watchHistoryStopState) close() {
	state.mu.Lock()
	if !state.closed {
		state.closed = true
		close(state.queue)
	}
	state.mu.Unlock()
	<-state.done
}

func watchHistoryRequestIdentityHash(siteID int64, token string, clientIdentity ...string) string {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > watchHistoryMaxTokenBytes || strings.ContainsAny(token, "\r\n") {
		return ""
	}
	mac := hmac.New(sha256.New, jwtSecret)
	_, _ = mac.Write([]byte("meridian-watch-history-client-v1\x00"))
	_, _ = mac.Write([]byte(strconv.FormatInt(siteID, 10)))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(token))
	for index := 0; index < 2; index++ {
		_, _ = mac.Write([]byte{0})
		if index < len(clientIdentity) {
			_, _ = mac.Write([]byte(clientIdentity[index]))
		}
	}
	return hex.EncodeToString(mac.Sum(nil))
}

func validWatchHistoryStopEvent(event watchHistoryEvent) bool {
	if event.ClientIdentityHash != "" && (len(event.ClientIdentityHash) != sha256.Size*2 || !isHexString(event.ClientIdentityHash)) {
		return false
	}
	if event.ResponseStatusCode < 0 || event.ResponseStatusCode > 599 || event.ClientStopRequestedAtMS < 0 || event.StopResponseAtMS < 0 {
		return false
	}
	if event.ClientStopOutcome == "" {
		return event.ClientStopRequestedAtMS == 0 && event.StopResponseAtMS == 0
	}
	if event.EventType != "stopped" || event.ClientIdentityHash == "" || event.ClientStopRequestedAtMS <= 0 || event.ClientStopRequestedAtMS != event.ObservedAtMS {
		return false
	}
	switch event.ClientStopOutcome {
	case "pending":
		return event.ResponseStatusCode == 0 && event.StopResponseAtMS == 0
	case "unconfirmed":
		return event.ResponseStatusCode != http.StatusNoContent && !watchHistoryStopDeniedStatus(event.ResponseStatusCode)
	case "confirmed":
		return event.ResponseStatusCode == http.StatusNoContent && event.StopResponseAtMS >= event.ClientStopRequestedAtMS
	case "denied":
		return watchHistoryStopDeniedStatus(event.ResponseStatusCode)
	default:
		return false
	}
}

func watchHistoryStopDeniedStatus(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusUnprocessableEntity
}

func isWatchHistoryStopRequest(request *http.Request) bool {
	if request == nil || request.URL == nil || !watchHistoryPlaybackSyncMethod(request.Method) {
		return false
	}
	path := strings.TrimSuffix(strings.ToLower(request.URL.Path), "/")
	return path == "/sessions/playing/stopped" || path == "/emby/sessions/playing/stopped"
}

func watchHistorySameObservedClient(expected, incoming watchHistoryEvent) bool {
	if expected.ClientIdentityHash == "" || !hmac.Equal([]byte(expected.ClientIdentityHash), []byte(incoming.ClientIdentityHash)) || expected.sourceNodeID != incoming.sourceNodeID || expected.SiteID != incoming.SiteID || expected.UpstreamItemID != incoming.UpstreamItemID || expected.PlaySessionID != incoming.PlaySessionID {
		return false
	}
	if expected.UserID != "" && incoming.UserID != "" && expected.UserID != incoming.UserID {
		return false
	}
	for _, pair := range [][2]string{
		{expected.DeviceID, incoming.DeviceID}, {expected.ClientName, incoming.ClientName},
	} {
		if pair[0] != "" && pair[0] != pair[1] {
			return false
		}
	}
	return true
}

func (state *watchHistoryStopState) rememberPlayback(event watchHistoryEvent) {
	if state == nil || event.ClientStopOutcome != "" || (event.EventType != "started" && event.EventType != "progress") {
		return
	}
	if event.ClientIdentityHash == "" && event.TokenCiphertext != "" && event.sourceNodeID == 0 {
		if token, err := decryptWatchHistoryToken(event.TokenCiphertext); err == nil {
			event.ClientIdentityHash = watchHistoryRequestIdentityHash(event.SiteID, token, event.DeviceID, event.ClientName)
		}
	}
	if event.ClientIdentityHash == "" {
		state.mu.Lock()
		key := watchHistorySessionKey{event.SiteID, event.SessionHash}
		if current, exists := state.sessions[key]; exists && event.ObservedAtMS >= current.playback.ObservedAtMS {
			delete(state.sessions, key)
		}
		state.mu.Unlock()
		return
	}
	event.TokenCiphertext = ""
	key := watchHistorySessionKey{event.SiteID, event.SessionHash}
	now := time.Now()
	state.mu.Lock()
	defer state.mu.Unlock()
	current, exists := state.sessions[key]
	if exists && event.ObservedAtMS < current.playback.ObservedAtMS {
		return
	}
	if exists && event.ObservedAtMS == current.playback.ObservedAtMS && !watchHistorySameObservedClient(current.playback, event) {
		// Two successful identities at one millisecond cannot be ordered. Leave
		// playback visible until a newer success establishes a matching proof.
		delete(state.sessions, key)
		return
	}
	if !exists {
		state.evictLocked(now)
	}
	if exists && !watchHistorySameObservedClient(current.playback, event) {
		current.stop = watchHistoryEvent{}
	}
	current.playback = event
	current.touched = now
	if current.stop.ClientStopRequestedAtMS < event.ObservedAtMS {
		current.stop = watchHistoryEvent{}
	}
	state.sessions[key] = current
}

func (state *watchHistoryStopState) playbackChangesClient(event watchHistoryEvent) bool {
	if event.ClientStopOutcome != "" || (event.EventType != "started" && event.EventType != "progress") {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	current, ok := state.sessions[watchHistorySessionKey{event.SiteID, event.SessionHash}]
	return ok && event.ObservedAtMS >= current.playback.ObservedAtMS && !watchHistorySameObservedClient(current.playback, event)
}

func (state *watchHistoryStopState) evictLocked(now time.Time) {
	var oldestKey watchHistorySessionKey
	var oldest time.Time
	for key, session := range state.sessions {
		if now.Sub(session.touched) > accountRetentionSessionTTL {
			delete(state.sessions, key)
			continue
		}
		if oldest.IsZero() || session.touched.Before(oldest) {
			oldestKey, oldest = key, session.touched
		}
	}
	if len(state.sessions) >= watchHistoryStopStateLimit && !oldest.IsZero() {
		delete(state.sessions, oldestKey)
	}
}

func (state *watchHistoryStopState) resolveLocked(event watchHistoryEvent) (watchHistoryEvent, watchHistoryTrustedSession, bool) {
	key := watchHistorySessionKey{event.SiteID, event.SessionHash}
	current, exists := state.sessions[key]
	if !exists && event.PlaySessionID == "" {
		// A token-bound fallback may cross the six-hour digest bucket while a
		// playback is active. Only one recently observed matching client/item
		// may bridge that boundary; never guess between concurrent sessions.
		matches := 0
		for candidateKey, candidate := range state.sessions {
			if candidateKey.siteID == event.SiteID && watchHistorySameObservedClient(candidate.playback, event) && event.ObservedAtMS-candidate.playback.ObservedAtMS >= 0 && event.ObservedAtMS-candidate.playback.ObservedAtMS <= watchHistoryActiveWindow.Milliseconds() {
				key, current = candidateKey, candidate
				matches++
			}
		}
		exists = matches == 1
	}
	if !exists || time.Since(current.touched) > accountRetentionSessionTTL || !watchHistorySameObservedClient(current.playback, event) || event.ObservedAtMS < current.playback.ObservedAtMS || event.ClientStopRequestedAtMS < current.stop.ClientStopRequestedAtMS {
		return event, current, false
	}
	event.SessionHash = key.sessionHash
	return event, current, true
}

func (state *watchHistoryStopState) resolve(event watchHistoryEvent) (watchHistoryEvent, bool) {
	if state == nil {
		return event, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	resolved, _, ok := state.resolveLocked(event)
	// close stops admission, while events already accepted by the ordinary
	// writer still need their trusted proof to finish the shutdown drain.
	return resolved, ok
}

func (state *watchHistoryStopState) submit(event watchHistoryEvent) bool {
	if state == nil || !validWatchHistoryEvent(event) {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.closed {
		return false
	}
	resolved, current, ok := state.resolveLocked(event)
	if !ok {
		return false
	}
	if current.stop.ClientStopOutcome == "confirmed" && current.stop.ClientStopRequestedAtMS == resolved.ClientStopRequestedAtMS {
		return true
	}
	// Confirmation is published only after the ordinary durable writer commits.
	// A rejected stop can restore activity immediately while it is being saved.
	if resolved.ClientStopOutcome != "confirmed" {
		current.stop = resolved
	} else if current.stop.ClientStopRequestedAtMS != resolved.ClientStopRequestedAtMS {
		current.stop = resolved
		current.stop.ClientStopOutcome = "pending"
		current.stop.ResponseStatusCode = 0
		current.stop.StopResponseAtMS = 0
	}
	current.touched = time.Now()
	key := watchHistorySessionKey{resolved.SiteID, resolved.SessionHash}
	state.sessions[key] = current
	select {
	case state.queue <- resolved:
		return true
	default:
		if current.stop.ClientStopOutcome != "denied" {
			current.stop.ClientStopOutcome = "unconfirmed"
			current.stop.ResponseStatusCode = 0
			current.stop.StopResponseAtMS = 0
			state.sessions[key] = current
		}
		return false
	}
}

func (state *watchHistoryStopState) failed(event watchHistoryEvent) {
	state.mu.Lock()
	defer state.mu.Unlock()
	key := watchHistorySessionKey{event.SiteID, event.SessionHash}
	current, ok := state.sessions[key]
	if ok && current.stop.ClientStopRequestedAtMS == event.ClientStopRequestedAtMS && current.stop.ClientStopOutcome != "denied" && current.stop.ClientStopOutcome != "confirmed" {
		current.stop.ClientStopOutcome = "unconfirmed"
		current.stop.ResponseStatusCode = 0
		current.stop.StopResponseAtMS = 0
		state.sessions[key] = current
	}
}

func (state *watchHistoryStopState) committed(event watchHistoryEvent) {
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	resolved, current, ok := state.resolveLocked(event)
	if !ok || (current.stop.ClientStopRequestedAtMS == resolved.ClientStopRequestedAtMS && current.stop.ClientStopOutcome != "pending" && resolved.ClientStopOutcome == "pending") {
		return
	}
	current.stop = resolved
	if resolved.ClientStopOutcome == "confirmed" && current.playback.ObservedAtMS < resolved.ObservedAtMS {
		current.playback.ObservedAtMS = resolved.ObservedAtMS
	}
	current.touched = time.Now()
	state.sessions[watchHistorySessionKey{resolved.SiteID, resolved.SessionHash}] = current
}

func (state *watchHistoryStopState) overlay(entry *WatchHistoryEntry) {
	if state == nil || entry == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	current, ok := state.sessions[watchHistorySessionKey{entry.SiteID, entry.sessionHash}]
	if !ok || current.stop.ClientStopRequestedAtMS == 0 || !watchHistorySameObservedClient(current.playback, current.stop) || current.stop.ClientStopRequestedAtMS < entry.LastSeenAtMS || current.stop.ClientStopRequestedAtMS < entry.ClientStopRequestedAtMS || time.Since(current.touched) > accountRetentionSessionTTL {
		return
	}
	entry.ClientStopRequestedAtMS = current.stop.ClientStopRequestedAtMS
	entry.ClientStopOutcome = current.stop.ClientStopOutcome
}

func watchHistoryEntryHasStopIntent(entry WatchHistoryEntry) bool {
	return entry.ClientStopRequestedAtMS > 0 && entry.ClientStopOutcome != "" && entry.ClientStopOutcome != "denied"
}

func attachWatchHistoryStopIntentCapture(capture *watchHistoryBodyCapture, database *DB, site Site, request *http.Request, trustedProxies []*net.IPNet, observedAt time.Time) {
	if capture == nil || database == nil || database.watchHistoryStops == nil || !site.Enabled || !site.WatchHistoryEnabled || !isWatchHistoryStopRequest(request) {
		return
	}
	capture.mu.Lock()
	capture.onComplete = func() {
		if event, ok := watchHistoryStopEventFromCapture(capture, site, request, trustedProxies, observedAt); ok {
			_ = database.watchHistoryStops.submit(event)
		}
	}
	capture.mu.Unlock()
}

func watchHistoryStopEventFromCapture(capture *watchHistoryBodyCapture, site Site, request *http.Request, trustedProxies []*net.IPNet, observedAt time.Time) (watchHistoryEvent, bool) {
	if capture == nil || !isWatchHistoryStopRequest(request) || !site.Enabled || !site.WatchHistoryEnabled {
		return watchHistoryEvent{}, false
	}
	data, complete := capture.snapshot()
	if !complete {
		return watchHistoryEvent{}, false
	}
	payload, valid := decodeWatchHistoryPlaybackPayload(data)
	if !valid || payload.PositionTicks < 0 || payload.RunTimeTicks < 0 {
		return watchHistoryEvent{}, false
	}
	itemID := strings.TrimSpace(payload.ItemID)
	if len(itemID) > watchHistoryMaxIDBytes || len(strings.TrimSpace(payload.PlaySessionID)) > watchHistoryMaxIdentityBytes {
		return watchHistoryEvent{}, false
	}
	for _, item := range []*watchHistoryPlaybackItem{payload.Item, payload.NowPlayingItem} {
		if item != nil && strings.TrimSpace(item.ID) != "" {
			if len(strings.TrimSpace(item.ID)) > watchHistoryMaxIDBytes {
				return watchHistoryEvent{}, false
			}
			if itemID != "" && itemID != strings.TrimSpace(item.ID) {
				return watchHistoryEvent{}, false
			}
			itemID = strings.TrimSpace(item.ID)
		}
	}
	event, ok := watchHistoryPlaybackEventFromCapture(capture, site, request, trustedProxies, observedAt)
	if !ok || event.ClientIdentityHash == "" {
		return watchHistoryEvent{}, false
	}
	event.TokenCiphertext = ""
	event.ClientStopRequestedAtMS = event.ObservedAtMS
	event.ClientStopOutcome = "pending"
	return event, validWatchHistoryEvent(event)
}

func finishWatchHistoryCapture(capture *watchHistoryBodyCapture, database *DB, site Site, request *http.Request, trustedProxies []*net.IPNet, upstreamStatusCode int, observedAt time.Time) {
	if database == nil || !site.Enabled || !site.WatchHistoryEnabled || request == nil || request.URL == nil {
		return
	}
	if watchHistoryEventType(request.URL.Path) != "stopped" {
		if event, ok := watchHistoryEventFromCapture(capture, database, site, request, trustedProxies, upstreamStatusCode, observedAt); ok {
			event.ResponseStatusCode = upstreamStatusCode
			database.EnqueueWatchHistory(event)
		}
		return
	}
	event, ok := watchHistoryStopEventFromCapture(capture, site, request, trustedProxies, observedAt)
	if !ok || database.watchHistoryStops == nil {
		return
	}
	event.ResponseStatusCode = upstreamStatusCode
	switch upstreamStatusCode {
	case http.StatusNoContent:
		event.ClientStopOutcome = "confirmed"
		event.StopResponseAtMS = time.Now().UnixMilli()
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusUnprocessableEntity:
		event.ClientStopOutcome = "denied"
	default:
		event.ClientStopOutcome = "unconfirmed"
	}
	_ = database.watchHistoryStops.submit(event)
}

// writeWatchHistoryStopTx updates existing sessions only. It cannot create a
// media item or let a known PlaySessionId stand in for a previously observed
// client credential, identity, and authenticated Agent source.
func (d *DB) writeWatchHistoryStopTx(tx *sql.Tx, event watchHistoryEvent, successfulBatch map[string]watchHistoryEvent) (watchHistoryEvent, bool, error) {
	resolved, proofOK := d.watchHistoryStops.resolve(event)
	key := strconv.FormatInt(event.SiteID, 10) + "\x00" + event.SessionHash
	if expected, ok := successfulBatch[key]; ok {
		var previousAtMS int64
		if d.watchHistoryStops != nil {
			d.watchHistoryStops.mu.Lock()
			previousAtMS = d.watchHistoryStops.sessions[watchHistorySessionKey{event.SiteID, event.SessionHash}].playback.ObservedAtMS
			d.watchHistoryStops.mu.Unlock()
		}
		if expected.ObservedAtMS >= previousAtMS {
			// A newer successful client/source in this transaction supersedes the
			// old snapshot. Never fall back to that snapshot on an identity mismatch.
			resolved = event
			proofOK = (expected.EventType == "progress" || expected.EventType == "started") && watchHistorySameObservedClient(expected, event) && expected.ObservedAtMS <= event.ObservedAtMS
		}
	}
	if !proofOK {
		return event, false, nil
	}
	var stored watchHistoryEvent
	var id, lastSeenMS, requestedMS, stoppedMS int64
	var positionTicks, runtimeTicks int64
	var outcome, storedCiphertext string
	var playMethod string
	err := tx.QueryRow(`SELECT ws.id,ws.last_seen_at_ms,ws.client_stop_requested_at_ms,ws.client_stop_outcome,ws.stopped_at_ms,
		ws.user_id,ws.device_id,ws.client_name,ws.play_session_id,mi.upstream_item_id,ws.token_ciphertext,ws.position_ticks,ws.runtime_ticks,ws.play_method
		FROM watch_sessions ws JOIN media_items mi ON mi.id=ws.media_item_id JOIN sites s ON s.id=ws.site_id
		WHERE ws.site_id=? AND ws.session_hash=? AND s.enabled=1 AND s.watch_history_enabled=1`, resolved.SiteID, resolved.SessionHash).
		Scan(&id, &lastSeenMS, &requestedMS, &outcome, &stoppedMS, &stored.UserID, &stored.DeviceID, &stored.ClientName, &stored.PlaySessionID, &stored.UpstreamItemID, &storedCiphertext, &positionTicks, &runtimeTicks, &playMethod)
	if errors.Is(err, sql.ErrNoRows) {
		return event, false, nil
	}
	if err != nil {
		return event, false, err
	}
	stored.SiteID, stored.ClientIdentityHash, stored.sourceNodeID = resolved.SiteID, resolved.ClientIdentityHash, resolved.sourceNodeID
	if storedCiphertext != "" && resolved.sourceNodeID == 0 {
		token, err := decryptWatchHistoryToken(storedCiphertext)
		if err != nil || !hmac.Equal([]byte(watchHistoryRequestIdentityHash(resolved.SiteID, token, stored.DeviceID, stored.ClientName)), []byte(resolved.ClientIdentityHash)) {
			return event, false, nil
		}
	}
	if !watchHistorySameObservedClient(stored, resolved) || resolved.ClientStopRequestedAtMS < lastSeenMS || resolved.ClientStopRequestedAtMS < requestedMS || (outcome == "confirmed" && requestedMS == resolved.ClientStopRequestedAtMS) {
		return event, false, nil
	}
	if resolved.ClientStopOutcome == "pending" && requestedMS == resolved.ClientStopRequestedAtMS && outcome != "" && outcome != "pending" {
		return event, false, nil
	}
	if resolved.ClientStopOutcome == "confirmed" {
		stoppedMS = resolved.StopResponseAtMS
		if resolved.ObservedAtMS > lastSeenMS || resolved.PositionTicks >= positionTicks {
			positionTicks = resolved.PositionTicks
			if resolved.RunTimeTicks > 0 {
				runtimeTicks = resolved.RunTimeTicks
			}
			if resolved.PlayMethod != "" {
				playMethod = resolved.PlayMethod
			}
		}
		_, err = tx.Exec(`UPDATE watch_sessions SET client_stop_requested_at_ms=?,client_stop_outcome=?,stopped_at_ms=?,
			last_seen_at_ms=MAX(last_seen_at_ms,?),position_ticks=?,runtime_ticks=?,play_method=?,completed=MAX(completed,?) WHERE id=?`,
			resolved.ClientStopRequestedAtMS, resolved.ClientStopOutcome, stoppedMS, resolved.ObservedAtMS,
			positionTicks, runtimeTicks, playMethod, sqliteBool(watchHistoryCompleted(positionTicks, runtimeTicks)), id)
		return resolved, err == nil, err
	}
	_, err = tx.Exec(`UPDATE watch_sessions SET client_stop_requested_at_ms=?,client_stop_outcome=?,stopped_at_ms=? WHERE id=?`,
		resolved.ClientStopRequestedAtMS, resolved.ClientStopOutcome, stoppedMS, id)
	return resolved, err == nil, err
}
