package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	accountRetentionMaxDays               = 3650
	accountRetentionRequiredPlaybackSyncs = 5
	accountRetentionSessionTTL            = 6 * time.Hour
	mediaLibraryCountBodyLimit            = 64 << 10
	mediaLibraryCountMaxValue             = int64(1_000_000_000_000)
)

type accountRetentionCompletionEvent struct {
	SiteID              int64
	ExpectedStartedAtMS int64
	CompletedAtMS       int64
	Done                func(bool)
}

type mediaLibraryCountEvent struct {
	SiteID       int64
	MovieCount   int64
	SeriesCount  int64
	EpisodeCount int64
	ObservedAtMS int64
}

type mediaLibraryCountsBodyObserver struct {
	io.ReadCloser
	database *DB
	siteID   int64
	payload  []byte
	overflow bool
	done     sync.Once
}

func (observer *mediaLibraryCountsBodyObserver) Read(buffer []byte) (int, error) {
	n, err := observer.ReadCloser.Read(buffer)
	if n > 0 && !observer.overflow {
		remaining := mediaLibraryCountBodyLimit - len(observer.payload)
		if remaining > 0 {
			copyBytes := n
			if copyBytes > remaining {
				copyBytes = remaining
			}
			observer.payload = append(observer.payload, buffer[:copyBytes]...)
		}
		if n > remaining {
			observer.overflow = true
		}
	}
	if err == io.EOF {
		observer.done.Do(observer.finish)
	}
	return n, err
}

func (observer *mediaLibraryCountsBodyObserver) finish() {
	if observer.overflow || len(observer.payload) == 0 {
		return
	}
	var counts struct {
		MovieCount   *int64 `json:"MovieCount"`
		SeriesCount  *int64 `json:"SeriesCount"`
		EpisodeCount *int64 `json:"EpisodeCount"`
	}
	if json.Unmarshal(observer.payload, &counts) != nil || counts.MovieCount == nil || counts.SeriesCount == nil || counts.EpisodeCount == nil {
		return
	}
	observer.database.EnqueueMediaLibraryCounts(mediaLibraryCountEvent{
		SiteID:       observer.siteID,
		MovieCount:   *counts.MovieCount,
		SeriesCount:  *counts.SeriesCount,
		EpisodeCount: *counts.EpisodeCount,
		ObservedAtMS: time.Now().UnixMilli(),
	})
}

func normalizeAccountRetentionDays(days int) (int, error) {
	if days < 0 || days > accountRetentionMaxDays {
		return 0, fmt.Errorf("account_retention_days must be between 0 and %d", accountRetentionMaxDays)
	}
	return days, nil
}

type accountRetentionStatus struct {
	Enabled        bool
	RemainingDays  int
	DueAtMS        int64
	CompletedToday bool
}

func accountRetentionStatusAt(site Site, now time.Time, location *time.Location) accountRetentionStatus {
	status := accountRetentionStatus{Enabled: site.AccountRetentionDays > 0 && site.AccountRetentionStartedMS > 0}
	if !status.Enabled {
		return status
	}
	if location == nil {
		location = time.UTC
	}
	started := time.UnixMilli(site.AccountRetentionStartedMS)
	due := started.Add(time.Duration(site.AccountRetentionDays) * 24 * time.Hour)
	status.DueAtMS = due.UnixMilli()
	if remaining := due.Sub(now); remaining > 0 {
		status.RemainingDays = int(math.Ceil(remaining.Hours() / 24))
	}
	if site.AccountRetentionCompletedMS > 0 {
		localNow := now.In(location)
		dayStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location)
		completed := time.UnixMilli(site.AccountRetentionCompletedMS)
		status.CompletedToday = !completed.Before(dayStart) && completed.Before(dayStart.AddDate(0, 0, 1))
	}
	return status
}

type accountRetentionSession struct {
	SiteID            int64
	CycleStartedAtMS  int64
	PlaybackSyncCount int
	LastObservedAt    time.Time
	CompletionPending bool
}

type accountRetentionTracker struct {
	mu             sync.Mutex
	database       *DB
	sessions       map[string]*accountRetentionSession
	cycleOverrides map[int64]int64
}

func newAccountRetentionTracker(database *DB) *accountRetentionTracker {
	return &accountRetentionTracker{
		database:       database,
		sessions:       make(map[string]*accountRetentionSession),
		cycleOverrides: make(map[int64]int64),
	}
}

func accountRetentionViewerKey(siteID int64, request *http.Request, trustedProxies []*net.IPNet) string {
	if request == nil {
		return ""
	}
	queryAPIKey := ""
	queryAPIKeyCamel := ""
	if request.URL != nil {
		queryAPIKey = request.URL.Query().Get("api_key")
		queryAPIKeyCamel = request.URL.Query().Get("apiKey")
	}
	identity := ""
	for _, candidate := range []string{
		request.Header.Get("X-Emby-Token"),
		request.Header.Get("X-MediaBrowser-Token"),
		queryAPIKey,
		queryAPIKeyCamel,
	} {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			identity = "token:" + candidate
			break
		}
	}
	if identity == "" {
		for _, headerName := range []string{"X-Emby-Authorization", "Authorization"} {
			if token, deviceID := accountRetentionEmbyIdentity(request.Header.Get(headerName)); token != "" {
				identity = "token:" + token
				break
			} else if identity == "" && deviceID != "" {
				identity = "device:" + deviceID
			}
		}
	}
	if identity == "" {
		if authorization := strings.TrimSpace(request.Header.Get("Authorization")); authorization != "" {
			identity = "authorization:" + authorization
		}
	}
	if identity == "" {
		identity = "anonymous"
	}
	raw := strings.Join([]string{
		strconv.FormatInt(siteID, 10),
		identity,
		requestClientKey(request, trustedProxies),
		request.Header.Get("User-Agent"),
	}, "\x00")
	digest := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", digest[:])
}

func accountRetentionEmbyIdentity(value string) (token, deviceID string) {
	offset := 0
	for offset < len(value) && isEmbyAuthWhitespace(value[offset]) {
		offset++
	}
	schemeStart := offset
	for offset < len(value) && isEmbyAuthToken(value[offset]) {
		offset++
	}
	if schemeStart == offset || (!strings.EqualFold(value[schemeStart:offset], "MediaBrowser") && !strings.EqualFold(value[schemeStart:offset], "Emby")) {
		return "", ""
	}
	for offset < len(value) && isEmbyAuthWhitespace(value[offset]) {
		offset++
	}
	attributes, ok := parseEmbyAuthorizationAttributes(value, offset)
	if !ok {
		return "", ""
	}
	for _, attribute := range attributes {
		switch {
		case strings.EqualFold(attribute.name, "Token") && token == "":
			token = value[attribute.valueStart:attribute.valueEnd]
		case strings.EqualFold(attribute.name, "DeviceId") && deviceID == "":
			deviceID = value[attribute.valueStart:attribute.valueEnd]
		}
	}
	return token, deviceID
}

func accountRetentionSuccessfulStatus(statusCode int) bool {
	return statusCode >= 200 && statusCode < 400
}

func (tracker *accountRetentionTracker) Observe(site Site, request *http.Request, trustedProxies []*net.IPNet, category string, statusCode int, startedAt, endedAt time.Time) {
	if tracker == nil || tracker.database == nil || site.ID <= 0 || site.AccountRetentionDays <= 0 || site.AccountRetentionStartedMS <= 0 ||
		!accountRetentionSuccessfulStatus(statusCode) || category != requestLogCategoryPlaybackSync {
		return
	}
	key := accountRetentionViewerKey(site.ID, request, trustedProxies)
	if key == "" {
		return
	}
	if endedAt.Before(startedAt) {
		endedAt = startedAt
	}

	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	for sessionKey, session := range tracker.sessions {
		if endedAt.Sub(session.LastObservedAt) > accountRetentionSessionTTL {
			delete(tracker.sessions, sessionKey)
		}
	}
	cycleStartedAtMS := site.AccountRetentionStartedMS
	if override := tracker.cycleOverrides[site.ID]; override > cycleStartedAtMS {
		cycleStartedAtMS = override
	} else if site.AccountRetentionStartedMS > override {
		tracker.cycleOverrides[site.ID] = site.AccountRetentionStartedMS
	}
	session := tracker.sessions[key]
	if session == nil || session.SiteID != site.ID || session.CycleStartedAtMS != cycleStartedAtMS {
		session = &accountRetentionSession{SiteID: site.ID, CycleStartedAtMS: cycleStartedAtMS}
		tracker.sessions[key] = session
	}
	session.LastObservedAt = endedAt
	if session.CompletionPending {
		return
	}
	session.PlaybackSyncCount++
	if session.PlaybackSyncCount < accountRetentionRequiredPlaybackSyncs {
		return
	}

	completedAtMS := endedAt.UnixMilli()
	if completedAtMS <= cycleStartedAtMS {
		if cycleStartedAtMS == math.MaxInt64 {
			return
		}
		completedAtMS = cycleStartedAtMS + 1
	}
	session.CompletionPending = true
	enqueued := tracker.database.EnqueueAccountRetentionCompletion(accountRetentionCompletionEvent{
		SiteID:              site.ID,
		ExpectedStartedAtMS: cycleStartedAtMS,
		CompletedAtMS:       completedAtMS,
		Done: func(updated bool) {
			tracker.mu.Lock()
			defer tracker.mu.Unlock()
			if updated {
				tracker.cycleOverrides[site.ID] = completedAtMS
				for sessionKey, current := range tracker.sessions {
					if current.SiteID == site.ID {
						delete(tracker.sessions, sessionKey)
					}
				}
				return
			}
			if current := tracker.sessions[key]; current != nil && current.CycleStartedAtMS == cycleStartedAtMS {
				current.CompletionPending = false
			}
		},
	})
	if !enqueued {
		session.CompletionPending = false
	}
}

func (d *DB) EnqueueAccountRetentionCompletion(event accountRetentionCompletionEvent) bool {
	if d == nil || event.SiteID <= 0 || event.ExpectedStartedAtMS <= 0 || event.CompletedAtMS < event.ExpectedStartedAtMS {
		return false
	}
	if d.edgeEphemeral {
		if d.edgeTelemetrySink != nil {
			d.edgeTelemetrySink(edgeTelemetryEvent{Kind: "retention", Retention: event})
		}
		// Observe holds its tracker mutex while enqueueing; complete the local
		// session asynchronously so the callback cannot self-deadlock.
		if event.Done != nil {
			go event.Done(true)
		}
		return true
	}
	command := dynamicObservationCommand{kind: dynamicObservationCommandAccountRetentionComplete, retention: event}
	if !d.dynamicObservationGate.TryRLock() {
		return false
	}
	defer d.dynamicObservationGate.RUnlock()
	if d.dynamicObservationClosed.Load() || d.dynamicObservationQueue == nil {
		return false
	}
	select {
	case d.dynamicObservationQueue <- command:
		return true
	default:
		return false
	}
}

func (d *DB) EnqueueMediaLibraryCounts(event mediaLibraryCountEvent) bool {
	if d == nil || event.SiteID <= 0 || event.ObservedAtMS <= 0 || event.MovieCount < 0 || event.SeriesCount < 0 || event.EpisodeCount < 0 ||
		event.MovieCount > mediaLibraryCountMaxValue || event.SeriesCount > mediaLibraryCountMaxValue || event.EpisodeCount > mediaLibraryCountMaxValue {
		return false
	}
	if d.edgeEphemeral {
		if d.edgeTelemetrySink != nil {
			d.edgeTelemetrySink(edgeTelemetryEvent{Kind: "media_counts", Media: event})
		}
		return true
	}
	command := dynamicObservationCommand{kind: dynamicObservationCommandMediaCountsUpdate, mediaCounts: event}
	if !d.dynamicObservationGate.TryRLock() {
		return false
	}
	defer d.dynamicObservationGate.RUnlock()
	if d.dynamicObservationClosed.Load() || d.dynamicObservationQueue == nil {
		return false
	}
	select {
	case d.dynamicObservationQueue <- command:
		return true
	default:
		return false
	}
}

func isMediaLibraryCountsPath(path string) bool {
	path = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(path)), "/")
	return path == "/items/counts" || strings.HasSuffix(path, "/items/counts")
}

func captureMediaLibraryCounts(response *http.Response, database *DB, siteID int64) error {
	if response == nil || database == nil || siteID <= 0 || response.Request == nil || response.Request.URL == nil ||
		response.Request.Method != http.MethodGet || !isMediaLibraryCountsPath(response.Request.URL.Path) ||
		response.StatusCode < 200 || response.StatusCode >= 300 || response.Body == nil ||
		response.ContentLength > mediaLibraryCountBodyLimit ||
		!strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "json") {
		return nil
	}
	encoding := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" {
		return nil
	}
	capacity := 0
	if response.ContentLength > 0 {
		capacity = int(response.ContentLength)
	}
	response.Body = &mediaLibraryCountsBodyObserver{
		ReadCloser: response.Body,
		database:   database,
		siteID:     siteID,
		payload:    make([]byte, 0, capacity),
	}
	return nil
}
