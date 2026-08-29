package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	watchHistoryBodyLimit         = 64 << 10
	watchHistoryMetadataLimit     = 1 << 20
	watchHistoryQueueCapacity     = 1024
	watchHistoryBatchSize         = 64
	watchHistoryGlobalRowLimit    = 50000
	watchHistoryMaxIDBytes        = 256
	watchHistoryMaxTitleBytes     = 512
	watchHistoryMaxTextBytes      = 4096
	watchHistoryMetadataTTL       = 15 * time.Minute
	watchHistoryMetadataMax       = 4096
	watchHistoryActiveWindow      = 2 * time.Minute
	watchHistoryActiveLimit       = 48
	watchHistoryMaxIdentityBytes  = 512
	watchHistoryMaxTokenBytes     = 4096
	watchHistoryTokenCipherPrefix = "wh1:"
)

type watchHistoryMetadataKey struct {
	siteID int64
	itemID string
}

type watchHistoryMetadataEntry struct {
	item      watchHistoryPlaybackItem
	updatedAt time.Time
}

type watchHistoryEvent struct {
	SiteID          int64
	SessionHash     string
	UpstreamItemID  string
	EventType       string
	ObservedAtMS    int64
	PositionTicks   int64
	RunTimeTicks    int64
	PlayMethod      string
	MediaType       string
	Title           string
	OriginalTitle   string
	ProductionYear  int
	SeriesName      string
	SeasonNumber    int
	EpisodeNumber   int
	TMDBType        string
	TMDBID          int64
	IMDBID          string
	TVDBID          string
	UserName        string
	UserID          string
	ClientName      string
	DeviceID        string
	DeviceName      string
	PlaySessionID   string
	TokenCiphertext string
}

type WatchHistoryFilter struct {
	SiteID    int64
	MediaType string
	Query     string
	FromMS    int64
	ToMS      int64
	BeforeMS  int64
	BeforeID  int64
	Limit     int
}

type WatchHistoryEntry struct {
	ID                int64                    `json:"id"`
	SiteID            int64                    `json:"site_id"`
	SiteName          string                   `json:"site_name"`
	MediaItemID       int64                    `json:"media_item_id"`
	UpstreamItemID    string                   `json:"upstream_item_id"`
	MediaType         string                   `json:"media_type"`
	Title             string                   `json:"title"`
	OriginalTitle     string                   `json:"original_title"`
	ProductionYear    int                      `json:"production_year"`
	SeriesName        string                   `json:"series_name"`
	SeasonNumber      int                      `json:"season_number"`
	EpisodeNumber     int                      `json:"episode_number"`
	TMDBType          string                   `json:"tmdb_type"`
	TMDBID            int64                    `json:"tmdb_id"`
	Overview          string                   `json:"overview"`
	PosterPath        string                   `json:"poster_path"`
	BackdropPath      string                   `json:"backdrop_path"`
	ReleaseDate       string                   `json:"release_date"`
	VoteAverage       float64                  `json:"vote_average"`
	Genres            []string                 `json:"genres"`
	Status            string                   `json:"status"`
	LastAirDate       string                   `json:"last_air_date"`
	NextAirDate       string                   `json:"next_air_date"`
	NextSeasonNumber  int                      `json:"next_season_number"`
	NextEpisodeNumber int                      `json:"next_episode_number"`
	NextEpisodeName   string                   `json:"next_episode_name"`
	SeasonCount       int                      `json:"season_count"`
	EpisodeCount      int                      `json:"episode_count"`
	Stills            []string                 `json:"stills"`
	Cast              []WatchHistoryCastMember `json:"cast"`
	MatchStatus       string                   `json:"match_status"`
	StartedAtMS       int64                    `json:"started_at_ms"`
	LastSeenAtMS      int64                    `json:"last_seen_at_ms"`
	StoppedAtMS       int64                    `json:"stopped_at_ms"`
	PositionTicks     int64                    `json:"position_ticks"`
	RunTimeTicks      int64                    `json:"runtime_ticks"`
	PlayMethod        string                   `json:"play_method"`
	Completed         bool                     `json:"completed"`
	UserName          string                   `json:"user_name"`
	UserID            string                   `json:"user_id"`
	ClientName        string                   `json:"client_name"`
	DeviceID          string                   `json:"device_id"`
	DeviceName        string                   `json:"device_name"`
	PlaySessionID     string                   `json:"play_session_id"`
	TokenStored       bool                     `json:"token_stored"`
}

func watchHistoryTokenKeyForSecret(secret []byte) []byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte("meridian-watch-history-token-v1\x00"))
	_, _ = hash.Write(secret)
	return hash.Sum(nil)
}

func encryptWatchHistoryToken(token string) (string, error) {
	if jwtSecretEphemeral {
		return "", errors.New("watch history token storage requires a stable JWT secret")
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > watchHistoryMaxTokenBytes || strings.ContainsAny(token, "\r\n") {
		return "", errors.New("invalid watch history token")
	}
	block, err := aes.NewCipher(watchHistoryTokenKeyForSecret(jwtSecret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, nonce, []byte(token), []byte("meridian-watch-history-token"))
	return watchHistoryTokenCipherPrefix + base64.RawURLEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func decryptWatchHistoryToken(ciphertext string) (string, error) {
	if !strings.HasPrefix(ciphertext, watchHistoryTokenCipherPrefix) {
		return "", errors.New("invalid watch history token ciphertext")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(ciphertext, watchHistoryTokenCipherPrefix))
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(watchHistoryTokenKeyForSecret(jwtSecret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(payload) < gcm.NonceSize()+gcm.Overhead() {
		return "", errors.New("invalid watch history token ciphertext")
	}
	plain, err := gcm.Open(nil, payload[:gcm.NonceSize()], payload[gcm.NonceSize():], []byte("meridian-watch-history-token"))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func decodeWatchHistoryStrings(value string, maxBytes, maxItems int, normalize func(string) string) []string {
	if len(value) == 0 || len(value) > maxBytes || maxItems <= 0 {
		return []string{}
	}
	var raw []string
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return []string{}
	}
	result := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, maxItems)
	for _, item := range raw {
		item = normalize(item)
		if item == "" {
			continue
		}
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		result = append(result, item)
		if len(result) >= maxItems {
			break
		}
	}
	return result
}

func decodeWatchHistoryGenres(value string) []string {
	return decodeWatchHistoryStrings(value, 4<<10, 8, func(item string) string { return requestLogSafeText(item, 128) })
}

func decodeWatchHistoryStills(value string) []string {
	return decodeWatchHistoryStrings(value, 16<<10, 12, func(item string) string {
		item = strings.TrimSpace(item)
		if !validTMDBImagePath(item) {
			return ""
		}
		return item
	})
}

// WatchHistoryCastMember contains only the public, bounded display fields
// needed by the details dialog. ProfilePath is a validated TMDB path; the
// browser still retrieves it through Meridian's same-origin image endpoint.
type WatchHistoryCastMember struct {
	Name        string `json:"name"`
	Character   string `json:"character,omitempty"`
	ProfilePath string `json:"profile_path,omitempty"`
}

func decodeWatchHistoryCast(value string) []WatchHistoryCastMember {
	if len(value) == 0 || len(value) > 16<<10 {
		return []WatchHistoryCastMember{}
	}
	var raw []WatchHistoryCastMember
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return []WatchHistoryCastMember{}
	}
	capacity := len(raw)
	if capacity > 20 {
		capacity = 20
	}
	cast := make([]WatchHistoryCastMember, 0, capacity)
	for _, member := range raw {
		name := requestLogSafeText(member.Name, 256)
		if name == "" {
			continue
		}
		cast = append(cast, WatchHistoryCastMember{
			Name:      name,
			Character: requestLogSafeText(member.Character, 256),
			ProfilePath: func() string {
				profilePath := strings.TrimSpace(member.ProfilePath)
				if !validTMDBImagePath(profilePath) {
					return ""
				}
				return profilePath
			}(),
		})
		if len(cast) >= 20 {
			break
		}
	}
	return cast
}

// watchHistoryBodyCapture observes bytes only while the upstream transport is
// already consuming them. It never pre-reads or delays the proxied request.
type watchHistoryBodyCapture struct {
	io.ReadCloser
	mu              sync.Mutex
	payload         []byte
	totalRead       int64
	contentLength   int64
	contentEncoding string
	overflow        bool
	complete        bool
	metadataDB      *DB
}

func (c *watchHistoryBodyCapture) Read(buffer []byte) (int, error) {
	n, err := c.ReadCloser.Read(buffer)
	c.mu.Lock()
	if n > 0 {
		c.totalRead += int64(n)
		remaining := watchHistoryBodyLimit - len(c.payload)
		if remaining > 0 {
			copyBytes := n
			if copyBytes > remaining {
				copyBytes = remaining
			}
			c.payload = append(c.payload, buffer[:copyBytes]...)
		}
		if c.totalRead > watchHistoryBodyLimit {
			c.overflow = true
		}
		if c.contentLength >= 0 && c.totalRead == c.contentLength {
			c.complete = true
		}
	}
	if err == io.EOF {
		c.complete = true
	}
	c.mu.Unlock()
	return n, err
}

func (c *watchHistoryBodyCapture) snapshot() ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.overflow || !c.complete || len(c.payload) == 0 || c.contentLength >= 0 && c.totalRead != c.contentLength {
		return nil, false
	}
	payload := append([]byte(nil), c.payload...)
	if strings.EqualFold(c.contentEncoding, "gzip") || strings.EqualFold(c.contentEncoding, "deflate") {
		var reader io.ReadCloser
		var err error
		if strings.EqualFold(c.contentEncoding, "gzip") {
			reader, err = gzip.NewReader(bytes.NewReader(payload))
		} else {
			reader = flate.NewReader(bytes.NewReader(payload))
		}
		if err != nil {
			return nil, false
		}
		defer reader.Close()
		decoded, err := io.ReadAll(io.LimitReader(reader, watchHistoryBodyLimit+1))
		if err != nil || len(decoded) > watchHistoryBodyLimit {
			return nil, false
		}
		return decoded, true
	}
	return payload, true
}

func watchHistoryPlaybackSyncMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	default:
		return false
	}
}

func startWatchHistoryCapture(site Site, request *http.Request, category string, metadataDB *DB) *watchHistoryBodyCapture {
	if !site.WatchHistoryEnabled || request == nil || !watchHistoryPlaybackSyncMethod(request.Method) || request.URL == nil ||
		category != requestLogCategoryPlaybackSync || request.Body == nil || request.Body == http.NoBody || request.ContentLength == 0 ||
		request.ContentLength > watchHistoryBodyLimit {
		return nil
	}
	// Hills and a few Android clients have sent the same bounded playback
	// payload as form data or with a non-JSON content type. Capture only this
	// allowlisted playback-sync route and let the bounded decoder decide whether
	// the body is JSON/form data; unsupported encodings fail closed at snapshot.
	encoding := strings.ToLower(strings.TrimSpace(request.Header.Get("Content-Encoding")))
	capacity := 0
	if request.ContentLength > 0 {
		capacity = int(request.ContentLength)
	}
	capture := &watchHistoryBodyCapture{
		ReadCloser:      request.Body,
		payload:         make([]byte, 0, capacity),
		contentLength:   request.ContentLength,
		contentEncoding: encoding,
		metadataDB:      metadataDB,
	}
	request.Body = capture
	return capture
}

type watchHistoryPlaybackItem struct {
	ID                string            `json:"Id"`
	Name              string            `json:"Name"`
	OriginalTitle     string            `json:"OriginalTitle"`
	Type              string            `json:"Type"`
	ProductionYear    int               `json:"ProductionYear"`
	SeriesName        string            `json:"SeriesName"`
	ParentIndexNumber int               `json:"ParentIndexNumber"`
	IndexNumber       int               `json:"IndexNumber"`
	RunTimeTicks      int64             `json:"RunTimeTicks"`
	ProviderIDs       map[string]string `json:"ProviderIds"`
}

type watchHistoryPlaybackPayload struct {
	ItemID         string                    `json:"ItemId"`
	PlaySessionID  string                    `json:"PlaySessionId"`
	UserID         string                    `json:"UserId"`
	UserName       string                    `json:"UserName"`
	ClientName     string                    `json:"ClientName"`
	DeviceID       string                    `json:"DeviceId"`
	DeviceName     string                    `json:"DeviceName"`
	MediaSourceID  string                    `json:"MediaSourceId"`
	PositionTicks  int64                     `json:"PositionTicks"`
	RunTimeTicks   int64                     `json:"RunTimeTicks"`
	PlayMethod     string                    `json:"PlayMethod"`
	Item           *watchHistoryPlaybackItem `json:"Item"`
	NowPlayingItem *watchHistoryPlaybackItem `json:"NowPlayingItem"`
}

type watchHistoryClientIdentity struct {
	userName, userID, clientName, deviceID, deviceName, token string
}

func watchHistoryEmbyAuthorizationIdentity(value string) watchHistoryClientIdentity {
	identity := watchHistoryClientIdentity{}
	offset := 0
	for offset < len(value) && isEmbyAuthWhitespace(value[offset]) {
		offset++
	}
	schemeStart := offset
	for offset < len(value) && isEmbyAuthToken(value[offset]) {
		offset++
	}
	if schemeStart == offset || (!strings.EqualFold(value[schemeStart:offset], "MediaBrowser") && !strings.EqualFold(value[schemeStart:offset], "Emby")) {
		return identity
	}
	for offset < len(value) && isEmbyAuthWhitespace(value[offset]) {
		offset++
	}
	attributes, ok := parseEmbyAuthorizationAttributes(value, offset)
	if !ok {
		return identity
	}
	for _, attribute := range attributes {
		candidate := value[attribute.valueStart:attribute.valueEnd]
		switch {
		case strings.EqualFold(attribute.name, "Token") && identity.token == "":
			identity.token = candidate
		case strings.EqualFold(attribute.name, "Client") && identity.clientName == "":
			identity.clientName = candidate
		case strings.EqualFold(attribute.name, "DeviceId") && identity.deviceID == "":
			identity.deviceID = candidate
		case strings.EqualFold(attribute.name, "Device") && identity.deviceName == "":
			identity.deviceName = candidate
		case strings.EqualFold(attribute.name, "UserId") && identity.userID == "":
			identity.userID = candidate
		case strings.EqualFold(attribute.name, "UserName") && identity.userName == "":
			identity.userName = candidate
		}
	}
	return identity
}

func watchHistoryClientIdentityFromRequest(request *http.Request, payload watchHistoryPlaybackPayload) watchHistoryClientIdentity {
	identity := watchHistoryClientIdentity{
		userName:   requestLogSafeText(payload.UserName, watchHistoryMaxIdentityBytes),
		userID:     requestLogSafeText(payload.UserID, watchHistoryMaxIdentityBytes),
		clientName: requestLogSafeText(payload.ClientName, watchHistoryMaxIdentityBytes),
		deviceID:   requestLogSafeText(payload.DeviceID, watchHistoryMaxIdentityBytes),
		deviceName: requestLogSafeText(payload.DeviceName, watchHistoryMaxIdentityBytes),
	}
	if request == nil {
		return identity
	}
	for _, header := range []string{"X-Emby-Authorization", "Authorization"} {
		candidate := watchHistoryEmbyAuthorizationIdentity(request.Header.Get(header))
		if identity.userName == "" {
			identity.userName = requestLogSafeText(candidate.userName, watchHistoryMaxIdentityBytes)
		}
		if identity.userID == "" {
			identity.userID = requestLogSafeText(candidate.userID, watchHistoryMaxIdentityBytes)
		}
		if identity.clientName == "" {
			identity.clientName = requestLogSafeText(candidate.clientName, watchHistoryMaxIdentityBytes)
		}
		if identity.deviceID == "" {
			identity.deviceID = requestLogSafeText(candidate.deviceID, watchHistoryMaxIdentityBytes)
		}
		if identity.deviceName == "" {
			identity.deviceName = requestLogSafeText(candidate.deviceName, watchHistoryMaxIdentityBytes)
		}
		if identity.token == "" {
			identity.token = candidate.token
		}
	}
	if identity.token == "" {
		for _, candidate := range []string{request.Header.Get("X-Emby-Token"), request.Header.Get("X-MediaBrowser-Token"), watchHistoryQueryValue(request, "api_key"), watchHistoryQueryValue(request, "apiKey")} {
			if candidate = strings.TrimSpace(candidate); candidate != "" {
				identity.token = candidate
				break
			}
		}
	}
	return identity
}

// watchHistoryMetadataBodyObserver inspects only small, successful item
// metadata responses. It never buffers media, manifests, images, or arbitrary
// API responses, and stores no credentials or request headers.
type watchHistoryMetadataBodyObserver struct {
	io.ReadCloser
	database        *DB
	siteID          int64
	itemHint        string
	contentEncoding string
	payload         []byte
	overflow        bool
	done            sync.Once
}

func (observer *watchHistoryMetadataBodyObserver) Read(buffer []byte) (int, error) {
	n, err := observer.ReadCloser.Read(buffer)
	if n > 0 && !observer.overflow {
		remaining := watchHistoryMetadataLimit - len(observer.payload)
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

func (observer *watchHistoryMetadataBodyObserver) finish() {
	if observer == nil || observer.database == nil || observer.overflow || len(observer.payload) == 0 {
		return
	}
	payload := observer.payload
	if strings.EqualFold(observer.contentEncoding, "gzip") {
		reader, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return
		}
		decompressed, err := io.ReadAll(io.LimitReader(reader, watchHistoryMetadataLimit+1))
		_ = reader.Close()
		if err != nil || len(decompressed) > watchHistoryMetadataLimit {
			return
		}
		payload = decompressed
	}
	var envelope struct {
		Items []watchHistoryPlaybackItem `json:"Items"`
	}
	var item watchHistoryPlaybackItem
	if err := json.Unmarshal(payload, &item); err == nil && strings.TrimSpace(item.ID) != "" {
		observer.database.rememberWatchHistoryMetadata(observer.siteID, item)
		return
	}
	items := make([]watchHistoryPlaybackItem, 0)
	var rawItems []watchHistoryPlaybackItem
	if err := json.Unmarshal(payload, &rawItems); err == nil {
		items = append(items, rawItems...)
	} else if err := json.Unmarshal(payload, &envelope); err == nil {
		items = append(items, envelope.Items...)
	}
	for _, candidate := range items {
		if strings.TrimSpace(candidate.ID) == "" {
			continue
		}
		if observer.itemHint != "" && candidate.ID != observer.itemHint {
			continue
		}
		observer.database.rememberWatchHistoryMetadata(observer.siteID, candidate)
		if observer.itemHint != "" {
			break
		}
	}
}

func (d *DB) rememberWatchHistoryMetadata(siteID int64, item watchHistoryPlaybackItem) {
	if d == nil || siteID <= 0 || strings.TrimSpace(item.ID) == "" {
		return
	}
	item.ID = requestLogSafeText(item.ID, watchHistoryMaxIDBytes)
	item.Name = requestLogSafeText(item.Name, watchHistoryMaxTitleBytes)
	item.OriginalTitle = requestLogSafeText(item.OriginalTitle, watchHistoryMaxTitleBytes)
	item.SeriesName = requestLogSafeText(item.SeriesName, watchHistoryMaxTitleBytes)
	if item.ID == "" {
		return
	}
	now := time.Now()
	d.watchHistoryMetadataMu.Lock()
	defer d.watchHistoryMetadataMu.Unlock()
	if d.watchHistoryMetadata == nil {
		d.watchHistoryMetadata = make(map[watchHistoryMetadataKey]watchHistoryMetadataEntry)
	}
	for key, entry := range d.watchHistoryMetadata {
		if now.Sub(entry.updatedAt) > watchHistoryMetadataTTL {
			delete(d.watchHistoryMetadata, key)
		}
	}
	key := watchHistoryMetadataKey{siteID: siteID, itemID: item.ID}
	if _, exists := d.watchHistoryMetadata[key]; !exists && len(d.watchHistoryMetadata) >= watchHistoryMetadataMax {
		var oldestKey watchHistoryMetadataKey
		var oldest time.Time
		for candidateKey, entry := range d.watchHistoryMetadata {
			if oldest.IsZero() || entry.updatedAt.Before(oldest) {
				oldestKey, oldest = candidateKey, entry.updatedAt
			}
		}
		if !oldest.IsZero() {
			delete(d.watchHistoryMetadata, oldestKey)
		}
	}
	d.watchHistoryMetadata[key] = watchHistoryMetadataEntry{item: item, updatedAt: now}
}

func (d *DB) watchHistoryMetadataFor(siteID int64, itemID string) (watchHistoryPlaybackItem, bool) {
	if d == nil || siteID <= 0 || strings.TrimSpace(itemID) == "" {
		return watchHistoryPlaybackItem{}, false
	}
	now := time.Now()
	d.watchHistoryMetadataMu.Lock()
	defer d.watchHistoryMetadataMu.Unlock()
	key := watchHistoryMetadataKey{siteID: siteID, itemID: itemID}
	entry, ok := d.watchHistoryMetadata[key]
	if !ok || now.Sub(entry.updatedAt) > watchHistoryMetadataTTL {
		if ok {
			delete(d.watchHistoryMetadata, key)
		}
		return watchHistoryPlaybackItem{}, false
	}
	return entry.item, true
}

func isWatchHistoryMetadataPath(path string) bool {
	parts := strings.Split(strings.Trim(strings.ToLower(path), "/"), "/")
	for index, part := range parts {
		if part != "items" || index+1 >= len(parts) || parts[index+1] == "" {
			continue
		}
		itemID := parts[index+1]
		if index+2 != len(parts) || itemID == "counts" || itemID == "latest" || itemID == "filters" || itemID == "images" || itemID == "resume" {
			continue
		}
		return true
	}
	return false
}

func isWatchHistoryMetadataCollectionPath(path string) bool {
	parts := strings.Split(strings.Trim(strings.ToLower(path), "/"), "/")
	if len(parts) == 0 {
		return false
	}
	last := parts[len(parts)-1]
	if last == "items" {
		return true
	}
	if last == "resume" || last == "latest" {
		for _, part := range parts[:len(parts)-1] {
			if part == "items" {
				return true
			}
		}
	}
	if last == "episodes" || last == "seasons" || last == "nextup" {
		for _, part := range parts[:len(parts)-1] {
			if part == "shows" {
				return true
			}
		}
	}
	return false
}

func watchHistoryMetadataItemHint(request *http.Request) string {
	if request == nil || request.URL == nil {
		return ""
	}
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	for index, part := range parts {
		if strings.EqualFold(strings.TrimSpace(part), "Items") && index+2 == len(parts) {
			itemID := strings.ToLower(strings.TrimSpace(parts[index+1]))
			if itemID == "counts" || itemID == "latest" || itemID == "filters" || itemID == "images" || itemID == "resume" {
				return ""
			}
			return requestLogSafeText(parts[index+1], watchHistoryMaxIDBytes)
		}
	}
	return ""
}

func isWatchHistoryMetadataRequest(request *http.Request) bool {
	if request == nil || request.URL == nil || request.Method != http.MethodGet {
		return false
	}
	return isWatchHistoryMetadataPath(request.URL.Path) || isWatchHistoryMetadataCollectionPath(request.URL.Path)
}

func captureWatchHistoryMetadata(response *http.Response, database *DB, siteID int64) error {
	if response == nil || database == nil || siteID <= 0 || response.Request == nil || response.Request.URL == nil ||
		response.Request.Method != http.MethodGet ||
		(!isWatchHistoryMetadataPath(response.Request.URL.Path) && !isWatchHistoryMetadataCollectionPath(response.Request.URL.Path)) ||
		response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices || response.Body == nil ||
		response.ContentLength > watchHistoryMetadataLimit || !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "json") {
		return nil
	}
	encoding := strings.ToLower(strings.TrimSpace(response.Header.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" && encoding != "gzip" {
		return nil
	}
	capacity := 0
	if response.ContentLength > 0 {
		capacity = int(response.ContentLength)
	}
	response.Body = &watchHistoryMetadataBodyObserver{
		ReadCloser:      response.Body,
		database:        database,
		siteID:          siteID,
		itemHint:        watchHistoryMetadataItemHint(response.Request),
		contentEncoding: encoding,
		payload:         make([]byte, 0, capacity),
	}
	return nil
}

func watchHistoryEventType(path string) string {
	path = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(path)), "/")
	switch {
	case strings.HasSuffix(path, "/progress"):
		return "progress"
	case strings.HasSuffix(path, "/stopped"):
		return "stopped"
	default:
		return "started"
	}
}

func watchHistoryProviderID(providerIDs map[string]string, name string) string {
	for key, value := range providerIDs {
		if strings.EqualFold(strings.TrimSpace(key), name) {
			return requestLogSafeText(value, watchHistoryMaxIDBytes)
		}
	}
	return ""
}

func normalizeWatchHistoryMediaType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "movie":
		return "movie"
	case "episode":
		return "episode"
	case "series":
		return "series"
	default:
		return ""
	}
}

func watchHistorySessionDigest(siteID int64, playSessionID, fallbackIdentity, itemID string, observedAt time.Time) string {
	identity := strings.TrimSpace(playSessionID)
	if identity == "" {
		bucket := observedAt.Unix() / int64(accountRetentionSessionTTL/time.Second)
		identity = strings.Join([]string{"fallback", fallbackIdentity, itemID, strconv.FormatInt(bucket, 10)}, "\x00")
	}
	mac := hmac.New(sha256.New, jwtSecret)
	_, _ = mac.Write([]byte("meridian-watch-history-session-v1\x00"))
	_, _ = mac.Write([]byte(strconv.FormatInt(siteID, 10)))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(identity))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(itemID))
	return fmt.Sprintf("%x", mac.Sum(nil))
}

func watchHistoryEventFromCapture(capture *watchHistoryBodyCapture, metadataDB *DB, site Site, request *http.Request, trustedProxies []*net.IPNet, statusCode int, observedAt time.Time) (watchHistoryEvent, bool) {
	if !site.WatchHistoryEnabled || request == nil || request.URL == nil || statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return watchHistoryEvent{}, false
	}
	legacyEvent, legacyOK := watchHistoryLegacyProgressEvent(site, request, trustedProxies, observedAt)
	payloadEvent, payloadOK := watchHistoryPlaybackEventFromCapture(capture, site, request, trustedProxies, observedAt)
	if legacyOK {
		// Some clients send the legacy query-string progress endpoint while also
		// including Item/NowPlayingItem in the JSON body. Keep the authoritative
		// progress/session fields from the legacy endpoint, but merge its richer
		// metadata instead of discarding it.
		if payloadOK {
			legacyEvent = mergeWatchHistoryEvents(legacyEvent, payloadEvent)
		}
		if metadataDB == nil && capture != nil {
			metadataDB = capture.metadataDB
		}
		legacyEvent = enrichWatchHistoryEvent(metadataDB, legacyEvent)
		return legacyEvent, validWatchHistoryEvent(legacyEvent)
	}
	if payloadOK {
		if metadataDB == nil && capture != nil {
			metadataDB = capture.metadataDB
		}
		payloadEvent = enrichWatchHistoryEvent(metadataDB, payloadEvent)
		return payloadEvent, validWatchHistoryEvent(payloadEvent)
	}
	return watchHistoryEvent{}, false
}

func enrichWatchHistoryEvent(database *DB, event watchHistoryEvent) watchHistoryEvent {
	if database == nil || event.UpstreamItemID == "" {
		return event
	}
	item, ok := database.watchHistoryMetadataFor(event.SiteID, event.UpstreamItemID)
	if !ok {
		return event
	}
	metadata := watchHistoryEvent{
		SiteID:         event.SiteID,
		UpstreamItemID: event.UpstreamItemID,
		ObservedAtMS:   event.ObservedAtMS,
		MediaType:      normalizeWatchHistoryMediaType(item.Type),
		Title:          item.Name,
		OriginalTitle:  item.OriginalTitle,
		ProductionYear: item.ProductionYear,
		SeriesName:     item.SeriesName,
		SeasonNumber:   item.ParentIndexNumber,
		EpisodeNumber:  item.IndexNumber,
		RunTimeTicks:   item.RunTimeTicks,
	}
	if metadata.MediaType == "movie" {
		metadata.TMDBType = "movie"
	} else if metadata.MediaType == "series" {
		metadata.TMDBType = "tv"
	}
	if metadata.MediaType != "episode" {
		tmdbID := watchHistoryProviderID(item.ProviderIDs, "Tmdb")
		if parsed, err := strconv.ParseInt(tmdbID, 10, 64); err == nil && parsed > 0 {
			metadata.TMDBID = parsed
		}
		metadata.IMDBID = watchHistoryProviderID(item.ProviderIDs, "Imdb")
		metadata.TVDBID = watchHistoryProviderID(item.ProviderIDs, "Tvdb")
	}
	return fillWatchHistoryEventMetadata(event, metadata)
}

func fillWatchHistoryEventMetadata(current, candidate watchHistoryEvent) watchHistoryEvent {
	if current.MediaType == "" {
		current.MediaType = candidate.MediaType
	}
	if current.Title == "" {
		current.Title = candidate.Title
	}
	if current.OriginalTitle == "" {
		current.OriginalTitle = candidate.OriginalTitle
	}
	if current.ProductionYear <= 0 {
		current.ProductionYear = candidate.ProductionYear
	}
	if current.SeriesName == "" {
		current.SeriesName = candidate.SeriesName
	}
	if current.SeasonNumber < 0 {
		current.SeasonNumber = candidate.SeasonNumber
	}
	if current.EpisodeNumber < 0 {
		current.EpisodeNumber = candidate.EpisodeNumber
	}
	if current.RunTimeTicks <= 0 {
		current.RunTimeTicks = candidate.RunTimeTicks
	}
	if current.TMDBType == "" {
		current.TMDBType = candidate.TMDBType
	}
	if current.TMDBID <= 0 {
		current.TMDBID = candidate.TMDBID
	}
	if current.IMDBID == "" {
		current.IMDBID = candidate.IMDBID
	}
	if current.TVDBID == "" {
		current.TVDBID = candidate.TVDBID
	}
	return current
}

func watchHistoryPlaybackEventFromCapture(capture *watchHistoryBodyCapture, site Site, request *http.Request, trustedProxies []*net.IPNet, observedAt time.Time) (watchHistoryEvent, bool) {
	if capture == nil {
		return watchHistoryEvent{}, false
	}
	payloadBytes, ok := capture.snapshot()
	if !ok {
		return watchHistoryEvent{}, false
	}
	payload, ok := decodeWatchHistoryPlaybackPayload(payloadBytes)
	if !ok {
		return watchHistoryEvent{}, false
	}
	item := mergeWatchHistoryPlaybackItems(payload.Item, payload.NowPlayingItem)
	itemID := requestLogSafeText(payload.ItemID, watchHistoryMaxIDBytes)
	if item != nil && itemID == "" {
		itemID = requestLogSafeText(item.ID, watchHistoryMaxIDBytes)
	}
	if itemID == "" {
		return watchHistoryEvent{}, false
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	event := watchHistoryEvent{
		SiteID:         site.ID,
		UpstreamItemID: itemID,
		EventType:      watchHistoryEventType(request.URL.Path),
		ObservedAtMS:   observedAt.UnixMilli(),
		PositionTicks:  payload.PositionTicks,
		RunTimeTicks:   payload.RunTimeTicks,
		PlayMethod:     requestLogSafeText(payload.PlayMethod, 64),
		SeasonNumber:   -1,
		EpisodeNumber:  -1,
	}
	identity := watchHistoryClientIdentityFromRequest(request, payload)
	event.UserName = identity.userName
	event.UserID = identity.userID
	event.ClientName = identity.clientName
	event.DeviceID = identity.deviceID
	event.DeviceName = identity.deviceName
	event.PlaySessionID = requestLogSafeText(payload.PlaySessionID, watchHistoryMaxIdentityBytes)
	if identity.token != "" {
		event.TokenCiphertext, _ = encryptWatchHistoryToken(identity.token)
	}
	if event.PositionTicks < 0 {
		event.PositionTicks = 0
	}
	if event.RunTimeTicks < 0 {
		event.RunTimeTicks = 0
	}
	if item != nil {
		event.MediaType = normalizeWatchHistoryMediaType(item.Type)
		event.Title = requestLogSafeText(item.Name, watchHistoryMaxTitleBytes)
		event.OriginalTitle = requestLogSafeText(item.OriginalTitle, watchHistoryMaxTitleBytes)
		event.ProductionYear = item.ProductionYear
		event.SeriesName = requestLogSafeText(item.SeriesName, watchHistoryMaxTitleBytes)
		event.SeasonNumber = item.ParentIndexNumber
		event.EpisodeNumber = item.IndexNumber
		if event.RunTimeTicks == 0 && item.RunTimeTicks > 0 {
			event.RunTimeTicks = item.RunTimeTicks
		}
		// Episode ProviderIds identify the episode itself. TMDB enrichment for an
		// episode is series-based, so persisting those IDs as series IDs can collide
		// with an unrelated show. Episodes are matched by SeriesName instead.
		if event.MediaType != "episode" {
			tmdbID := watchHistoryProviderID(item.ProviderIDs, "Tmdb")
			if parsed, err := strconv.ParseInt(tmdbID, 10, 64); err == nil && parsed > 0 {
				event.TMDBID = parsed
			}
			event.IMDBID = watchHistoryProviderID(item.ProviderIDs, "Imdb")
			event.TVDBID = watchHistoryProviderID(item.ProviderIDs, "Tvdb")
		}
	}
	switch event.MediaType {
	case "movie":
		event.TMDBType = "movie"
	case "series":
		event.TMDBType = "tv"
	}
	fallbackIdentity := accountRetentionViewerKey(site.ID, request, trustedProxies)
	event.SessionHash = watchHistorySessionDigest(site.ID, payload.PlaySessionID, fallbackIdentity, itemID, observedAt)
	return event, validWatchHistoryEvent(event)
}

func decodeWatchHistoryPlaybackPayload(payloadBytes []byte) (watchHistoryPlaybackPayload, bool) {
	var payload watchHistoryPlaybackPayload
	if err := json.Unmarshal(payloadBytes, &payload); err == nil {
		if strings.TrimSpace(payload.ItemID) != "" || payload.Item != nil || payload.NowPlayingItem != nil {
			return payload, true
		}
	}
	values, err := url.ParseQuery(string(payloadBytes))
	if err != nil {
		return watchHistoryPlaybackPayload{}, false
	}
	value := func(names ...string) string {
		for key, entries := range values {
			for _, name := range names {
				if strings.EqualFold(key, name) && len(entries) > 0 {
					return strings.TrimSpace(entries[0])
				}
			}
		}
		return ""
	}
	parseTicks := func(names ...string) int64 {
		parsed, _ := strconv.ParseInt(value(names...), 10, 64)
		return parsed
	}
	payload.ItemID = value("ItemId", "ItemID")
	payload.PlaySessionID = value("PlaySessionId", "PlaySessionID")
	payload.UserID = value("UserId", "UserID")
	payload.UserName = value("UserName")
	payload.DeviceID = value("DeviceId", "DeviceID")
	payload.DeviceName = value("DeviceName")
	payload.MediaSourceID = value("MediaSourceId", "MediaSourceID")
	payload.PositionTicks = parseTicks("PositionTicks")
	payload.RunTimeTicks = parseTicks("RunTimeTicks")
	payload.PlayMethod = value("PlayMethod")
	return payload, payload.ItemID != ""
}

func mergeWatchHistoryPlaybackItems(primary, secondary *watchHistoryPlaybackItem) *watchHistoryPlaybackItem {
	if primary == nil && secondary == nil {
		return nil
	}
	merged := watchHistoryPlaybackItem{}
	if primary != nil {
		merged = *primary
	}
	if secondary == nil {
		return &merged
	}
	if merged.ID == "" {
		merged.ID = secondary.ID
	}
	if merged.Name == "" {
		merged.Name = secondary.Name
	}
	if merged.OriginalTitle == "" {
		merged.OriginalTitle = secondary.OriginalTitle
	}
	if merged.Type == "" {
		merged.Type = secondary.Type
	}
	if merged.ProductionYear <= 0 {
		merged.ProductionYear = secondary.ProductionYear
	}
	if merged.SeriesName == "" {
		merged.SeriesName = secondary.SeriesName
	}
	if merged.ParentIndexNumber == 0 {
		merged.ParentIndexNumber = secondary.ParentIndexNumber
	}
	if merged.IndexNumber == 0 {
		merged.IndexNumber = secondary.IndexNumber
	}
	if merged.RunTimeTicks <= 0 {
		merged.RunTimeTicks = secondary.RunTimeTicks
	}
	if len(merged.ProviderIDs) == 0 {
		merged.ProviderIDs = secondary.ProviderIDs
	}
	return &merged
}

func watchHistoryQueryValue(request *http.Request, name string) string {
	if request == nil || request.URL == nil {
		return ""
	}
	for key, values := range request.URL.Query() {
		if strings.EqualFold(strings.TrimSpace(key), name) && len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
	}
	return ""
}

func watchHistoryQueryTicks(request *http.Request, name string) (int64, bool) {
	value := watchHistoryQueryValue(request, name)
	if value == "" {
		return 0, true
	}
	ticks, err := strconv.ParseInt(value, 10, 64)
	return ticks, err == nil && ticks >= 0
}

func watchHistoryLegacyProgressEvent(site Site, request *http.Request, trustedProxies []*net.IPNet, observedAt time.Time) (watchHistoryEvent, bool) {
	if request == nil || request.URL == nil || !watchHistoryPlaybackSyncMethod(request.Method) {
		return watchHistoryEvent{}, false
	}
	rawItemID, ok := legacyPlaybackProgressItemID(request.URL.Path)
	if !ok {
		return watchHistoryEvent{}, false
	}
	itemID := requestLogSafeText(rawItemID, watchHistoryMaxIDBytes)
	positionTicks, positionOK := watchHistoryQueryTicks(request, "PositionTicks")
	runtimeTicks, runtimeOK := watchHistoryQueryTicks(request, "RunTimeTicks")
	if itemID == "" || !positionOK || !runtimeOK {
		return watchHistoryEvent{}, false
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	playSessionID := requestLogSafeText(watchHistoryQueryValue(request, "PlaySessionId"), watchHistoryMaxIDBytes)
	fallbackIdentity := accountRetentionViewerKey(site.ID, request, trustedProxies)
	event := watchHistoryEvent{
		SiteID:         site.ID,
		SessionHash:    watchHistorySessionDigest(site.ID, playSessionID, fallbackIdentity, itemID, observedAt),
		UpstreamItemID: itemID,
		EventType:      "progress",
		ObservedAtMS:   observedAt.UnixMilli(),
		PositionTicks:  positionTicks,
		RunTimeTicks:   runtimeTicks,
		PlayMethod:     requestLogSafeText(watchHistoryQueryValue(request, "PlayMethod"), 64),
		SeasonNumber:   -1,
		EpisodeNumber:  -1,
	}
	payload := watchHistoryPlaybackPayload{PlaySessionID: playSessionID}
	identity := watchHistoryClientIdentityFromRequest(request, payload)
	event.UserName = identity.userName
	event.UserID = identity.userID
	event.ClientName = identity.clientName
	event.DeviceID = identity.deviceID
	event.DeviceName = identity.deviceName
	event.PlaySessionID = requestLogSafeText(playSessionID, watchHistoryMaxIdentityBytes)
	if identity.token != "" {
		event.TokenCiphertext, _ = encryptWatchHistoryToken(identity.token)
	}
	return event, validWatchHistoryEvent(event)
}

func validWatchHistoryEvent(event watchHistoryEvent) bool {
	return event.SiteID > 0 && len(event.SessionHash) == sha256.Size*2 && event.UpstreamItemID != "" &&
		len(event.UpstreamItemID) <= watchHistoryMaxIDBytes && event.ObservedAtMS > 0 && event.PositionTicks >= 0 && event.RunTimeTicks >= 0 &&
		len(event.Title) <= watchHistoryMaxTitleBytes && len(event.OriginalTitle) <= watchHistoryMaxTitleBytes &&
		len(event.SeriesName) <= watchHistoryMaxTitleBytes && len(event.IMDBID) <= watchHistoryMaxIDBytes && len(event.TVDBID) <= watchHistoryMaxIDBytes &&
		len(event.UserName) <= watchHistoryMaxIdentityBytes && len(event.UserID) <= watchHistoryMaxIdentityBytes && len(event.DeviceID) <= watchHistoryMaxIdentityBytes && len(event.DeviceName) <= watchHistoryMaxIdentityBytes && len(event.PlaySessionID) <= watchHistoryMaxIdentityBytes
}

func (d *DB) EnqueueWatchHistory(event watchHistoryEvent) bool {
	if d == nil || !validWatchHistoryEvent(event) {
		return false
	}
	command := dynamicObservationCommand{kind: dynamicObservationCommandWatchHistoryWrite, watchHistory: event}
	if !d.dynamicObservationGate.TryRLock() {
		d.droppedWatchHistory.Add(1)
		return false
	}
	defer d.dynamicObservationGate.RUnlock()
	if d.dynamicObservationClosed.Load() || d.dynamicObservationQueue == nil {
		d.droppedWatchHistory.Add(1)
		return false
	}
	select {
	case d.dynamicObservationQueue <- command:
		return true
	default:
		d.droppedWatchHistory.Add(1)
		return false
	}
}

func (d *DB) DroppedWatchHistory() uint64 {
	if d == nil {
		return 0
	}
	return d.droppedWatchHistory.Load()
}

func mergeWatchHistoryEvents(current, candidate watchHistoryEvent) watchHistoryEvent {
	newer := candidate.ObservedAtMS > current.ObservedAtMS
	if newer {
		current.EventType = candidate.EventType
		current.ObservedAtMS = candidate.ObservedAtMS
		current.PositionTicks = candidate.PositionTicks
		current.RunTimeTicks = candidate.RunTimeTicks
		if candidate.PlayMethod != "" {
			current.PlayMethod = candidate.PlayMethod
		}
	} else if candidate.ObservedAtMS == current.ObservedAtMS {
		if candidate.EventType == "stopped" {
			current.EventType = "stopped"
		}
		if candidate.PositionTicks >= current.PositionTicks {
			current.PositionTicks = candidate.PositionTicks
			if candidate.RunTimeTicks > 0 {
				current.RunTimeTicks = candidate.RunTimeTicks
			}
			if candidate.PlayMethod != "" {
				current.PlayMethod = candidate.PlayMethod
			}
		} else if current.RunTimeTicks == 0 && candidate.RunTimeTicks > 0 {
			current.RunTimeTicks = candidate.RunTimeTicks
		}
	}
	if candidate.MediaType != "" {
		current.MediaType = candidate.MediaType
	}
	if candidate.Title != "" {
		current.Title = candidate.Title
	}
	if candidate.OriginalTitle != "" {
		current.OriginalTitle = candidate.OriginalTitle
	}
	if candidate.ProductionYear > 0 {
		current.ProductionYear = candidate.ProductionYear
	}
	if candidate.SeriesName != "" {
		current.SeriesName = candidate.SeriesName
	}
	if candidate.SeasonNumber >= 0 {
		current.SeasonNumber = candidate.SeasonNumber
	}
	if candidate.EpisodeNumber >= 0 {
		current.EpisodeNumber = candidate.EpisodeNumber
	}
	if candidate.TMDBType != "" {
		current.TMDBType = candidate.TMDBType
	}
	if candidate.TMDBID > 0 {
		current.TMDBID = candidate.TMDBID
	}
	if candidate.IMDBID != "" {
		current.IMDBID = candidate.IMDBID
	}
	if candidate.TVDBID != "" {
		current.TVDBID = candidate.TVDBID
	}
	if candidate.UserName != "" {
		current.UserName = candidate.UserName
	}
	if candidate.UserID != "" {
		current.UserID = candidate.UserID
	}
	if candidate.DeviceID != "" {
		current.DeviceID = candidate.DeviceID
	}
	if candidate.DeviceName != "" {
		current.DeviceName = candidate.DeviceName
	}
	if candidate.PlaySessionID != "" {
		current.PlaySessionID = candidate.PlaySessionID
	}
	if candidate.TokenCiphertext != "" {
		current.TokenCiphertext = candidate.TokenCiphertext
	}
	return current
}

func watchHistoryCompleted(positionTicks, runtimeTicks int64) bool {
	if positionTicks <= 0 || runtimeTicks <= 0 {
		return false
	}
	return positionTicks >= runtimeTicks-runtimeTicks/10
}

func (d *DB) writeWatchHistoryBatch(batch []watchHistoryEvent) (int, error) {
	merged := make(map[string]watchHistoryEvent, len(batch))
	skipped := 0
	for _, event := range batch {
		if !validWatchHistoryEvent(event) {
			skipped++
			continue
		}
		key := strconv.FormatInt(event.SiteID, 10) + "\x00" + event.SessionHash
		if current, ok := merged[key]; ok {
			merged[key] = mergeWatchHistoryEvents(current, event)
		} else {
			merged[key] = event
		}
	}
	if len(merged) == 0 {
		return skipped, nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	siteExists := make(map[int64]bool)
	for _, event := range merged {
		exists, checked := siteExists[event.SiteID]
		if !checked {
			var count int
			if err := tx.QueryRow("SELECT COUNT(*) FROM sites WHERE id=?", event.SiteID).Scan(&count); err != nil {
				return 0, err
			}
			exists = count == 1
			siteExists[event.SiteID] = exists
		}
		if !exists {
			skipped++
			continue
		}
		var previous struct {
			mediaType, title, originalTitle, seriesName, imdbID, tvdbID string
			productionYear, seasonNumber, episodeNumber                 int
			tmdbID                                                      int64
		}
		previousFound := true
		previousErr := tx.QueryRow(`SELECT media_type, title, original_title, production_year, series_name,
			season_number, episode_number, tmdb_id, imdb_id, tvdb_id FROM media_items WHERE site_id=? AND upstream_item_id=?`,
			event.SiteID, event.UpstreamItemID).Scan(&previous.mediaType, &previous.title, &previous.originalTitle,
			&previous.productionYear, &previous.seriesName, &previous.seasonNumber, &previous.episodeNumber,
			&previous.tmdbID, &previous.imdbID, &previous.tvdbID)
		if errors.Is(previousErr, sql.ErrNoRows) {
			previousFound = false
		} else if previousErr != nil {
			return 0, previousErr
		}
		metadataImproved := previousFound && (event.MediaType != "" && event.MediaType != previous.mediaType ||
			event.Title != "" && event.Title != previous.title ||
			event.OriginalTitle != "" && event.OriginalTitle != previous.originalTitle ||
			event.ProductionYear > 0 && event.ProductionYear != previous.productionYear ||
			event.SeriesName != "" && event.SeriesName != previous.seriesName ||
			event.SeasonNumber >= 0 && event.SeasonNumber != previous.seasonNumber ||
			event.EpisodeNumber >= 0 && event.EpisodeNumber != previous.episodeNumber ||
			event.TMDBID > 0 && event.TMDBID != previous.tmdbID ||
			event.IMDBID != "" && event.IMDBID != previous.imdbID ||
			event.TVDBID != "" && event.TVDBID != previous.tvdbID)

		_, err := tx.Exec(`INSERT INTO media_items (
			site_id, upstream_item_id, media_type, title, original_title, production_year, series_name,
			season_number, episode_number, tmdb_type, tmdb_id, imdb_id, tvdb_id, created_at_ms, updated_at_ms
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(site_id, upstream_item_id) DO UPDATE SET
			media_type=CASE WHEN excluded.media_type<>'' THEN excluded.media_type ELSE media_items.media_type END,
			title=CASE WHEN excluded.title<>'' THEN excluded.title ELSE media_items.title END,
			original_title=CASE WHEN excluded.original_title<>'' THEN excluded.original_title ELSE media_items.original_title END,
			production_year=CASE WHEN excluded.production_year>0 THEN excluded.production_year ELSE media_items.production_year END,
			series_name=CASE WHEN excluded.series_name<>'' THEN excluded.series_name ELSE media_items.series_name END,
			season_number=CASE WHEN excluded.season_number>=0 THEN excluded.season_number ELSE media_items.season_number END,
			episode_number=CASE WHEN excluded.episode_number>=0 THEN excluded.episode_number ELSE media_items.episode_number END,
			tmdb_type=CASE WHEN excluded.tmdb_type<>'' THEN excluded.tmdb_type ELSE media_items.tmdb_type END,
			tmdb_id=CASE WHEN excluded.tmdb_id>0 THEN excluded.tmdb_id ELSE media_items.tmdb_id END,
			imdb_id=CASE WHEN excluded.imdb_id<>'' THEN excluded.imdb_id ELSE media_items.imdb_id END,
			tvdb_id=CASE WHEN excluded.tvdb_id<>'' THEN excluded.tvdb_id ELSE media_items.tvdb_id END,
			updated_at_ms=MAX(media_items.updated_at_ms, excluded.updated_at_ms)`,
			event.SiteID, event.UpstreamItemID, event.MediaType, event.Title, event.OriginalTitle, event.ProductionYear, event.SeriesName,
			event.SeasonNumber, event.EpisodeNumber, event.TMDBType, event.TMDBID, event.IMDBID, event.TVDBID, event.ObservedAtMS, event.ObservedAtMS)
		if err != nil {
			return 0, err
		}
		var mediaItemID int64
		if err := tx.QueryRow("SELECT id FROM media_items WHERE site_id=? AND upstream_item_id=?", event.SiteID, event.UpstreamItemID).Scan(&mediaItemID); err != nil {
			return 0, err
		}
		stoppedAtMS := int64(0)
		if event.EventType == "stopped" {
			stoppedAtMS = event.ObservedAtMS
		}
		completed := sqliteBool(watchHistoryCompleted(event.PositionTicks, event.RunTimeTicks))
		_, err = tx.Exec(`INSERT INTO watch_sessions (
			site_id, media_item_id, session_hash, started_at_ms, last_seen_at_ms, stopped_at_ms,
			position_ticks, runtime_ticks, play_method, completed, user_name, user_id, client_name, device_id, device_name, play_session_id, token_ciphertext
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(site_id, session_hash) DO UPDATE SET
			media_item_id=excluded.media_item_id,
			started_at_ms=MIN(watch_sessions.started_at_ms, excluded.started_at_ms),
			last_seen_at_ms=MAX(watch_sessions.last_seen_at_ms, excluded.last_seen_at_ms),
			stopped_at_ms=MAX(watch_sessions.stopped_at_ms, excluded.stopped_at_ms),
			position_ticks=CASE WHEN excluded.last_seen_at_ms>watch_sessions.last_seen_at_ms OR (excluded.last_seen_at_ms=watch_sessions.last_seen_at_ms AND excluded.position_ticks>=watch_sessions.position_ticks) THEN excluded.position_ticks ELSE watch_sessions.position_ticks END,
			runtime_ticks=CASE WHEN excluded.runtime_ticks>0 AND (excluded.last_seen_at_ms>watch_sessions.last_seen_at_ms OR watch_sessions.runtime_ticks=0 OR (excluded.last_seen_at_ms=watch_sessions.last_seen_at_ms AND excluded.position_ticks>=watch_sessions.position_ticks)) THEN excluded.runtime_ticks ELSE watch_sessions.runtime_ticks END,
			play_method=CASE WHEN excluded.play_method<>'' AND (excluded.last_seen_at_ms>watch_sessions.last_seen_at_ms OR watch_sessions.play_method='' OR (excluded.last_seen_at_ms=watch_sessions.last_seen_at_ms AND excluded.position_ticks>=watch_sessions.position_ticks)) THEN excluded.play_method ELSE watch_sessions.play_method END,
			completed=MAX(watch_sessions.completed, excluded.completed),
			user_name=CASE WHEN excluded.user_name<>'' THEN excluded.user_name ELSE watch_sessions.user_name END,
			user_id=CASE WHEN excluded.user_id<>'' THEN excluded.user_id ELSE watch_sessions.user_id END,
			client_name=CASE WHEN excluded.client_name<>'' THEN excluded.client_name ELSE watch_sessions.client_name END,
			device_id=CASE WHEN excluded.device_id<>'' THEN excluded.device_id ELSE watch_sessions.device_id END,
			device_name=CASE WHEN excluded.device_name<>'' THEN excluded.device_name ELSE watch_sessions.device_name END,
			play_session_id=CASE WHEN excluded.play_session_id<>'' THEN excluded.play_session_id ELSE watch_sessions.play_session_id END,
			token_ciphertext=CASE WHEN excluded.token_ciphertext<>'' THEN excluded.token_ciphertext ELSE watch_sessions.token_ciphertext END`,
			event.SiteID, mediaItemID, event.SessionHash, event.ObservedAtMS, event.ObservedAtMS, stoppedAtMS,
			event.PositionTicks, event.RunTimeTicks, event.PlayMethod, completed, event.UserName, event.UserID, event.ClientName, event.DeviceID, event.DeviceName, event.PlaySessionID, event.TokenCiphertext)
		if err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO tmdb_jobs (
			media_item_id, state, attempts, next_attempt_at_ms, lease_until_ms, last_error_code, updated_at_ms
		) VALUES (?, 'pending', 0, 0, 0, '', ?)`, mediaItemID, event.ObservedAtMS); err != nil {
			return 0, err
		}
		if metadataImproved {
			if _, err := tx.Exec(`UPDATE tmdb_jobs SET
				revision=revision+1,
				state=CASE WHEN state='running' THEN state ELSE 'pending' END,
				attempts=CASE WHEN state='running' THEN attempts ELSE 0 END,
				next_attempt_at_ms=CASE WHEN state='running' THEN next_attempt_at_ms ELSE 0 END,
				lease_until_ms=CASE WHEN state='running' THEN lease_until_ms ELSE 0 END,
				last_error_code=CASE WHEN state='running' THEN last_error_code ELSE '' END,
				updated_at_ms=?
				WHERE media_item_id=?`, event.ObservedAtMS, mediaItemID); err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return skipped, nil
}

func (d *DB) ListWatchHistory(filter WatchHistoryFilter) ([]WatchHistoryEntry, error) {
	if filter.SiteID < 0 || filter.FromMS < 0 || filter.ToMS < 0 || filter.BeforeMS < 0 || filter.BeforeID < 0 ||
		filter.FromMS > 0 && filter.ToMS > 0 && filter.FromMS > filter.ToMS {
		return nil, fmt.Errorf("invalid watch history filter")
	}
	rawMediaType := strings.TrimSpace(filter.MediaType)
	filter.MediaType = normalizeWatchHistoryMediaType(rawMediaType)
	if rawMediaType != "" && filter.MediaType == "" {
		return nil, fmt.Errorf("invalid watch history media type")
	}
	filter.Query = requestLogSafeText(filter.Query, watchHistoryMaxTitleBytes)
	if filter.Limit <= 0 {
		filter.Limit = 48
	}
	if filter.Limit > 101 {
		filter.Limit = 101
	}
	if err := d.flushDynamicObservations(); err != nil {
		return nil, err
	}
	conditions := []string{"1=1"}
	outerConditions := []string{"series_rank=1"}
	args := make([]any, 0, 12)
	if filter.SiteID > 0 {
		conditions = append(conditions, "ws.site_id=?")
		args = append(args, filter.SiteID)
	}
	if filter.MediaType != "" {
		conditions = append(conditions, "mi.media_type=?")
		args = append(args, filter.MediaType)
	}
	if filter.Query != "" {
		conditions = append(conditions, "(mi.title LIKE '%'||?||'%' OR mi.original_title LIKE '%'||?||'%' OR mi.series_name LIKE '%'||?||'%')")
		args = append(args, filter.Query, filter.Query, filter.Query)
	}
	if filter.FromMS > 0 {
		conditions = append(conditions, "ws.last_seen_at_ms>=?")
		args = append(args, filter.FromMS)
	}
	if filter.ToMS > 0 {
		conditions = append(conditions, "ws.last_seen_at_ms<=?")
		args = append(args, filter.ToMS)
	}
	if filter.BeforeMS > 0 {
		// Apply the cursor after choosing one representative per series. Applying
		// it inside the ranked source could make a completed episode reappear on a
		// later page after its preferred in-progress episode was filtered out.
		outerConditions = append(outerConditions, "(last_seen_at_ms<? OR (last_seen_at_ms=? AND history_id<?))")
		args = append(args, filter.BeforeMS, filter.BeforeMS, filter.BeforeID)
	}
	args = append(args, filter.Limit)
	// #nosec G202 -- conditions contains only fixed SQL fragments selected by
	// validated filters; every caller-supplied value remains a bound parameter.
	rows, err := d.db.Query(`WITH ranked_history AS (
		SELECT ws.id AS history_id, ws.site_id AS history_site_id, s.name AS site_name,
		mi.id AS media_item_id, mi.upstream_item_id, mi.media_type,
		mi.title, mi.original_title, mi.production_year, mi.series_name, mi.season_number, mi.episode_number,
		mi.tmdb_type, mi.tmdb_id, mi.overview, mi.poster_path, mi.backdrop_path, mi.release_date, mi.vote_average,
		mi.genres_json, mi.status, mi.last_air_date, mi.next_air_date, mi.next_season_number, mi.next_episode_number,
		mi.next_episode_name, mi.season_count, mi.episode_count, mi.stills_json, mi.cast_json, mi.match_status,
		ws.started_at_ms, ws.last_seen_at_ms, ws.stopped_at_ms, ws.position_ticks, ws.runtime_ticks, ws.play_method, ws.completed,
		ws.user_name, ws.user_id, ws.client_name, ws.device_id, ws.device_name, ws.play_session_id, CASE WHEN ws.token_ciphertext<>'' THEN 1 ELSE 0 END AS token_stored,
		ROW_NUMBER() OVER (
			PARTITION BY ws.site_id, CASE
				WHEN mi.media_type='episode' AND trim(mi.series_name)<>'' THEN 'series-name:'||lower(trim(mi.series_name))
				WHEN mi.media_type='episode' AND mi.tmdb_type='tv' AND mi.tmdb_id>0 THEN 'series-tmdb:'||CAST(mi.tmdb_id AS TEXT)
				ELSE 'media:'||CAST(mi.id AS TEXT)
			END
			ORDER BY CASE
				WHEN mi.media_type='episode' AND ws.completed=0 AND ws.runtime_ticks>0 THEN 0
				WHEN mi.media_type='episode' THEN 1
				ELSE 0
			END ASC,
				ws.last_seen_at_ms DESC, ws.id DESC
		) AS series_rank
		FROM watch_sessions ws
		JOIN media_items mi ON mi.id=ws.media_item_id
		JOIN sites s ON s.id=ws.site_id
		WHERE `+strings.Join(conditions, " AND ")+`
	)
	SELECT history_id, history_site_id, site_name, media_item_id, upstream_item_id, media_type,
		title, original_title, production_year, series_name, season_number, episode_number,
		tmdb_type, tmdb_id, overview, poster_path, backdrop_path, release_date, vote_average,
		genres_json, status, last_air_date, next_air_date, next_season_number, next_episode_number,
		next_episode_name, season_count, episode_count, stills_json, cast_json, match_status,
		started_at_ms, last_seen_at_ms, stopped_at_ms, position_ticks, runtime_ticks, play_method, completed,
		user_name, user_id, client_name, device_id, device_name, play_session_id, token_stored
	FROM ranked_history
	WHERE `+strings.Join(outerConditions, " AND ")+`
	ORDER BY last_seen_at_ms DESC, history_id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]WatchHistoryEntry, 0)
	for rows.Next() {
		var entry WatchHistoryEntry
		var castJSON, genresJSON, stillsJSON string
		var completed, tokenStored int
		if err := rows.Scan(&entry.ID, &entry.SiteID, &entry.SiteName, &entry.MediaItemID, &entry.UpstreamItemID, &entry.MediaType,
			&entry.Title, &entry.OriginalTitle, &entry.ProductionYear, &entry.SeriesName, &entry.SeasonNumber, &entry.EpisodeNumber,
			&entry.TMDBType, &entry.TMDBID, &entry.Overview, &entry.PosterPath, &entry.BackdropPath, &entry.ReleaseDate, &entry.VoteAverage,
			&genresJSON, &entry.Status, &entry.LastAirDate, &entry.NextAirDate, &entry.NextSeasonNumber, &entry.NextEpisodeNumber,
			&entry.NextEpisodeName, &entry.SeasonCount, &entry.EpisodeCount, &stillsJSON, &castJSON, &entry.MatchStatus,
			&entry.StartedAtMS, &entry.LastSeenAtMS, &entry.StoppedAtMS, &entry.PositionTicks, &entry.RunTimeTicks, &entry.PlayMethod, &completed,
			&entry.UserName, &entry.UserID, &entry.ClientName, &entry.DeviceID, &entry.DeviceName, &entry.PlaySessionID, &tokenStored); err != nil {
			return nil, err
		}
		entry.Genres = decodeWatchHistoryGenres(genresJSON)
		entry.Stills = decodeWatchHistoryStills(stillsJSON)
		entry.Cast = decodeWatchHistoryCast(castJSON)
		entry.Completed = completed == 1
		entry.TokenStored = tokenStored == 1
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (d *DB) ListActiveWatchHistory(filter WatchHistoryFilter) ([]WatchHistoryEntry, error) {
	return d.listActiveWatchHistoryAt(filter, time.Now())
}

func (d *DB) listActiveWatchHistoryAt(filter WatchHistoryFilter, now time.Time) ([]WatchHistoryEntry, error) {
	if filter.SiteID < 0 {
		return nil, fmt.Errorf("invalid watch history filter")
	}
	rawMediaType := strings.TrimSpace(filter.MediaType)
	filter.MediaType = normalizeWatchHistoryMediaType(rawMediaType)
	if rawMediaType != "" && filter.MediaType == "" {
		return nil, fmt.Errorf("invalid watch history media type")
	}
	filter.Query = requestLogSafeText(filter.Query, watchHistoryMaxTitleBytes)
	if err := d.flushDynamicObservations(); err != nil {
		return nil, err
	}
	conditions := []string{"ws.stopped_at_ms=0", "ws.completed=0", "ws.last_seen_at_ms>=?"}
	args := []any{now.Add(-watchHistoryActiveWindow).UnixMilli()}
	if filter.SiteID > 0 {
		conditions = append(conditions, "ws.site_id=?")
		args = append(args, filter.SiteID)
	}
	if filter.MediaType != "" {
		conditions = append(conditions, "mi.media_type=?")
		args = append(args, filter.MediaType)
	}
	if filter.Query != "" {
		conditions = append(conditions, "(mi.title LIKE '%'||?||'%' OR mi.original_title LIKE '%'||?||'%' OR mi.series_name LIKE '%'||?||'%')")
		args = append(args, filter.Query, filter.Query, filter.Query)
	}
	args = append(args, watchHistoryActiveLimit)
	// #nosec G202 -- conditions contains only fixed SQL fragments selected by
	// validated filters; every caller-supplied value remains a bound parameter.
	rows, err := d.db.Query(`SELECT ws.id, ws.site_id, s.name, mi.id, mi.upstream_item_id, mi.media_type,
		mi.title, mi.original_title, mi.production_year, mi.series_name, mi.season_number, mi.episode_number,
		mi.tmdb_type, mi.tmdb_id, mi.overview, mi.poster_path, mi.backdrop_path, mi.release_date, mi.vote_average,
		mi.genres_json, mi.status, mi.last_air_date, mi.next_air_date, mi.next_season_number, mi.next_episode_number,
		mi.next_episode_name, mi.season_count, mi.episode_count, mi.stills_json, mi.cast_json, mi.match_status,
		ws.started_at_ms, ws.last_seen_at_ms, ws.stopped_at_ms, ws.position_ticks, ws.runtime_ticks, ws.play_method, ws.completed,
		ws.user_name, ws.user_id, ws.client_name, ws.device_id, ws.device_name, ws.play_session_id, CASE WHEN ws.token_ciphertext<>'' THEN 1 ELSE 0 END
		FROM watch_sessions ws
		JOIN media_items mi ON mi.id=ws.media_item_id
		JOIN sites s ON s.id=ws.site_id
		WHERE `+strings.Join(conditions, " AND ")+`
		ORDER BY ws.last_seen_at_ms DESC, ws.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]WatchHistoryEntry, 0)
	for rows.Next() {
		var entry WatchHistoryEntry
		var castJSON, genresJSON, stillsJSON string
		var completed, tokenStored int
		if err := rows.Scan(&entry.ID, &entry.SiteID, &entry.SiteName, &entry.MediaItemID, &entry.UpstreamItemID, &entry.MediaType,
			&entry.Title, &entry.OriginalTitle, &entry.ProductionYear, &entry.SeriesName, &entry.SeasonNumber, &entry.EpisodeNumber,
			&entry.TMDBType, &entry.TMDBID, &entry.Overview, &entry.PosterPath, &entry.BackdropPath, &entry.ReleaseDate, &entry.VoteAverage,
			&genresJSON, &entry.Status, &entry.LastAirDate, &entry.NextAirDate, &entry.NextSeasonNumber, &entry.NextEpisodeNumber,
			&entry.NextEpisodeName, &entry.SeasonCount, &entry.EpisodeCount, &stillsJSON, &castJSON, &entry.MatchStatus,
			&entry.StartedAtMS, &entry.LastSeenAtMS, &entry.StoppedAtMS, &entry.PositionTicks, &entry.RunTimeTicks, &entry.PlayMethod, &completed,
			&entry.UserName, &entry.UserID, &entry.ClientName, &entry.DeviceID, &entry.DeviceName, &entry.PlaySessionID, &tokenStored); err != nil {
			return nil, err
		}
		entry.Genres = decodeWatchHistoryGenres(genresJSON)
		entry.Stills = decodeWatchHistoryStills(stillsJSON)
		entry.Cast = decodeWatchHistoryCast(castJSON)
		entry.Completed = completed == 1
		entry.TokenStored = tokenStored == 1
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (d *DB) ClearWatchHistory(siteID int64) error {
	if siteID < 0 {
		return fmt.Errorf("invalid site id")
	}
	return d.sendDynamicObservationControl(dynamicObservationCommandWatchHistoryClear, siteID)
}

func (d *DB) DeleteWatchHistory(historyID int64) error {
	if historyID <= 0 {
		return fmt.Errorf("invalid watch history id")
	}
	return d.sendDynamicObservationHistoryDelete(historyID)
}

func (d *DB) clearWatchHistoryRows(siteID int64) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if siteID > 0 {
		if _, err := tx.Exec("DELETE FROM tmdb_jobs WHERE media_item_id IN (SELECT id FROM media_items WHERE site_id=?)", siteID); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM watch_sessions WHERE site_id=?", siteID); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM media_items WHERE site_id=?", siteID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM tmdb_cache WHERE NOT EXISTS (
			SELECT 1 FROM media_items mi WHERE mi.tmdb_type=tmdb_cache.tmdb_type AND mi.tmdb_id=tmdb_cache.tmdb_id
		)`); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec("DELETE FROM tmdb_jobs"); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM watch_sessions"); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM media_items"); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM tmdb_cache"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) deleteWatchHistoryRow(historyID int64) error {
	if historyID <= 0 {
		return fmt.Errorf("invalid watch history id")
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var mediaItemID int64
	if err := tx.QueryRow("SELECT media_item_id FROM watch_sessions WHERE id=?", historyID).Scan(&mediaItemID); errors.Is(err, sql.ErrNoRows) {
		return sql.ErrNoRows
	} else if err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM watch_sessions WHERE id=?", historyID); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM tmdb_jobs WHERE media_item_id=? AND NOT EXISTS (SELECT 1 FROM watch_sessions WHERE media_item_id=?)", mediaItemID, mediaItemID); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM media_items WHERE id=? AND NOT EXISTS (SELECT 1 FROM watch_sessions WHERE media_item_id=?)", mediaItemID, mediaItemID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM tmdb_cache WHERE NOT EXISTS (
		SELECT 1 FROM media_items mi WHERE mi.tmdb_type=tmdb_cache.tmdb_type AND mi.tmdb_id=tmdb_cache.tmdb_id
	)`); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) pruneWatchHistory() error {
	var retentionDays int
	if err := d.db.QueryRow("SELECT history_retention_days FROM tmdb_settings WHERE id=1").Scan(&retentionDays); err != nil {
		return err
	}
	now := time.Now()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if retentionDays > 0 {
		cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour).UnixMilli()
		if _, err := tx.Exec("DELETE FROM watch_sessions WHERE last_seen_at_ms<?", cutoff); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM watch_sessions WHERE id IN (
		SELECT id FROM watch_sessions ORDER BY last_seen_at_ms DESC, id DESC LIMIT -1 OFFSET ?
	)`, watchHistoryGlobalRowLimit); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM tmdb_jobs WHERE media_item_id IN (SELECT mi.id FROM media_items mi LEFT JOIN watch_sessions ws ON ws.media_item_id=mi.id WHERE ws.id IS NULL)"); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM media_items WHERE NOT EXISTS (SELECT 1 FROM watch_sessions ws WHERE ws.media_item_id=media_items.id)"); err != nil {
		return err
	}
	metadataCutoffMS := now.Add(-tmdbCacheLifetime).UnixMilli()
	if _, err := tx.Exec(`UPDATE tmdb_jobs SET state='pending', attempts=0, next_attempt_at_ms=0,
		lease_until_ms=0, last_error_code='', revision=revision+1, updated_at_ms=?
		WHERE media_item_id IN (
			SELECT id FROM media_items WHERE metadata_updated_at_ms>0 AND metadata_updated_at_ms<?
		)`, now.UnixMilli(), metadataCutoffMS); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE media_items SET overview='', poster_path='', details_version=0, backdrop_path='', release_date='', vote_average=0,
		genres_json='[]', status='', last_air_date='', next_air_date='', next_season_number=-1, next_episode_number=-1,
		next_episode_name='', season_count=0, episode_count=0, stills_json='[]', cast_json='[]', match_status='pending',
		metadata_updated_at_ms=0 WHERE metadata_updated_at_ms>0 AND metadata_updated_at_ms<?`, metadataCutoffMS); err != nil {
		return err
	}
	// TMDB cache follows the history lifecycle: expired entries and entries
	// no longer referenced by retained history are removed during the same
	// retention pass. Active history keeps its metadata available even when
	// the cache entry itself is older than the retention window.
	if _, err := tx.Exec(`DELETE FROM tmdb_cache WHERE
		(expires_at_ms>0 AND expires_at_ms<?) OR NOT EXISTS (
		SELECT 1 FROM media_items mi WHERE mi.tmdb_type=tmdb_cache.tmdb_type AND mi.tmdb_id=tmdb_cache.tmdb_id
		)`, now.UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}
