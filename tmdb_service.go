package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const (
	tmdbWorkerInterval = 15 * time.Second
	tmdbJobLease       = 2 * time.Minute
	// Bump this whenever the stored detail payload gains fields that must be
	// refreshed for existing history (for example cast profile paths/stills).
	// The migration invalidates the copied media metadata once, while keeping
	// the playback identity and TMDB IDs intact.
	tmdbDetailsVersion     = 2
	tmdbCastJSONMaxBytes   = 16 << 10
	tmdbGenresJSONMaxBytes = 4 << 10
	tmdbStillsJSONMaxBytes = 16 << 10
)

func marshalTMDBCast(value []tmdbCastMember) string {
	data, err := json.Marshal(normalizeTMDBCast(value))
	if err != nil || len(data) > tmdbCastJSONMaxBytes {
		return "[]"
	}
	return string(data)
}

func marshalTMDBStrings(value []string, maxBytes int) string {
	data, err := json.Marshal(value)
	if err != nil || len(data) > maxBytes {
		return "[]"
	}
	return string(data)
}

func decodeTMDBStrings(value string, maxBytes int, maxItems int, normalize func(string) string) []string {
	if len(value) == 0 || len(value) > maxBytes || maxItems <= 0 {
		return []string{}
	}
	var raw []string
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return []string{}
	}
	result := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, maxItems)
	for _, value := range raw {
		value = normalize(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
		if len(result) >= maxItems {
			break
		}
	}
	return result
}

func normalizeTMDBStoredDate(value string) string {
	if len(value) > 32 {
		return ""
	}
	return strings.TrimSpace(value)
}

func normalizeTMDBStoredStatus(value string) string {
	return requestLogSafeText(value, 128)
}

type tmdbService struct {
	db     *DB
	client *tmdbClient
	wake   chan struct{}
}

func newTMDBService(db *DB, client *tmdbClient) *tmdbService {
	if client == nil {
		client = newTMDBClient(nil)
	}
	return &tmdbService{db: db, client: client, wake: make(chan struct{}, 1)}
}

func (s *tmdbService) Wake() {
	if s == nil {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *tmdbService) Run(ctx context.Context) {
	if s == nil || s.db == nil || s.client == nil {
		return
	}
	ticker := time.NewTicker(tmdbWorkerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
		if ctx.Err() != nil {
			return
		}
		s.runOne(ctx, time.Now())
	}
}

func tmdbRetryDelay(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 8 {
		attempts = 8
	}
	delay := time.Minute * time.Duration(1<<uint(attempts-1))
	if delay > 6*time.Hour {
		return 6 * time.Hour
	}
	return delay
}

func (s *tmdbService) runOne(ctx context.Context, now time.Time) {
	stored, err := s.db.tmdbSettings()
	if err != nil || !stored.Enabled || stored.TokenCiphertext == "" || stored.CredentialState == tmdbCredentialInvalid {
		return
	}
	token, err := decryptTMDBReadToken(stored.TokenCiphertext)
	if err != nil {
		_ = s.db.markTMDBCredentialResult(tmdbCredentialInvalid, "decrypt", now.UnixMilli())
		return
	}
	job, found, err := s.db.claimTMDBJob(now.UnixMilli())
	if err != nil || !found {
		return
	}
	if cached, ok, cacheErr := s.db.cachedTMDBMetadata(job, stored.Language, now.UnixMilli()); cacheErr == nil && ok {
		_ = s.db.completeTMDBJob(job.ID, job.JobRevision, cached, stored.Language, now.UnixMilli())
		return
	}
	metadata, lookupErr := s.client.lookupMedia(ctx, token, stored.Language, job)
	if lookupErr == nil {
		if err := s.db.completeTMDBJob(job.ID, job.JobRevision, metadata, stored.Language, now.UnixMilli()); err == nil {
			_ = s.db.markTMDBCredentialResult(tmdbCredentialReady, "", now.UnixMilli())
		}
		return
	}
	var apiErr *tmdbAPIError
	if !errors.As(lookupErr, &apiErr) {
		_ = s.db.retryTMDBJob(job.ID, job.JobRevision, jobAttempts(job.ID, s.db), "network", now.Add(tmdbRetryDelay(1)).UnixMilli())
		return
	}
	switch apiErr.Code {
	case "auth":
		_ = s.db.markTMDBCredentialResult(tmdbCredentialInvalid, "auth", now.UnixMilli())
		_ = s.db.retryTMDBJob(job.ID, job.JobRevision, 1, "auth", now.Add(6*time.Hour).UnixMilli())
	case "no_confident_match", "insufficient_metadata", "unsupported_media_type", "client_error", "redirect":
		_ = s.db.finishTMDBJobWithoutMatch(job.ID, job.JobRevision, apiErr.Code, now.UnixMilli())
	default:
		wait := apiErr.RetryAfter
		attempts := jobAttempts(job.ID, s.db)
		if wait <= 0 {
			wait = tmdbRetryDelay(attempts)
		}
		_ = s.db.retryTMDBJob(job.ID, job.JobRevision, attempts, apiErr.Code, now.Add(wait).UnixMilli())
	}
}

func (d *DB) cachedTMDBMetadata(item tmdbLookupMedia, language string, nowMS int64) (tmdbMetadata, bool, error) {
	tmdbType := tmdbTypeForMedia(item.MediaType)
	if item.TMDBType == "movie" || item.TMDBType == "tv" {
		tmdbType = item.TMDBType
	}
	if item.TMDBID <= 0 || tmdbType == "" {
		return tmdbMetadata{}, false, nil
	}
	var metadata tmdbMetadata
	metadata.TMDBType = tmdbType
	metadata.TMDBID = item.TMDBID
	var castJSON, genresJSON, stillsJSON string
	var detailsVersion int
	err := d.db.QueryRow(`SELECT title, original_title, overview, release_year, release_date, poster_path, backdrop_path,
		vote_average, genres_json, status, last_air_date, next_air_date, next_season_number, next_episode_number,
		next_episode_name, season_count, episode_count, stills_json, cast_json, details_version
		FROM tmdb_cache WHERE tmdb_type=? AND tmdb_id=? AND language=? AND expires_at_ms>?`, tmdbType, item.TMDBID, language, nowMS).Scan(
		&metadata.Title, &metadata.OriginalTitle, &metadata.Overview, &metadata.ReleaseYear, &metadata.ReleaseDate,
		&metadata.PosterPath, &metadata.BackdropPath, &metadata.VoteAverage, &genresJSON, &metadata.Status,
		&metadata.LastAirDate, &metadata.NextAirDate, &metadata.NextSeasonNumber, &metadata.NextEpisodeNumber,
		&metadata.NextEpisodeName, &metadata.SeasonCount, &metadata.EpisodeCount, &stillsJSON, &castJSON, &detailsVersion,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return tmdbMetadata{}, false, nil
	}
	if err != nil {
		return tmdbMetadata{}, false, err
	}
	if detailsVersion < tmdbDetailsVersion {
		return tmdbMetadata{}, false, nil
	}
	metadata.MatchMethod = "cache"
	metadata.Genres = decodeTMDBStrings(genresJSON, tmdbGenresJSONMaxBytes, 8, func(value string) string { return requestLogSafeText(value, 128) })
	metadata.Stills = decodeTMDBStrings(stillsJSON, tmdbStillsJSONMaxBytes, 12, func(value string) string {
		value = strings.TrimSpace(value)
		if !validTMDBImagePath(value) {
			return ""
		}
		return value
	})
	metadata.Cast = decodeTMDBCast(castJSON)
	return metadata, true, nil
}

func decodeTMDBCast(value string) []tmdbCastMember {
	if len(value) == 0 || len(value) > tmdbCastJSONMaxBytes {
		return []tmdbCastMember{}
	}
	var cast []tmdbCastMember
	if err := json.Unmarshal([]byte(value), &cast); err != nil {
		return []tmdbCastMember{}
	}
	return normalizeTMDBCast(cast)
}

func jobAttempts(mediaItemID int64, db *DB) int {
	if db == nil {
		return 1
	}
	var attempts int
	if err := db.db.QueryRow("SELECT attempts FROM tmdb_jobs WHERE media_item_id=?", mediaItemID).Scan(&attempts); err != nil || attempts < 1 {
		return 1
	}
	return attempts
}

func (d *DB) claimTMDBJob(nowMS int64) (tmdbLookupMedia, bool, error) {
	tx, err := d.db.Begin()
	if err != nil {
		return tmdbLookupMedia{}, false, err
	}
	defer tx.Rollback()
	var item tmdbLookupMedia
	err = tx.QueryRow(`SELECT m.id, j.revision, m.media_type, m.title, m.original_title, m.production_year,
		m.series_name, m.season_number, m.episode_number, m.tmdb_type, m.tmdb_id, m.imdb_id, m.tvdb_id
		FROM tmdb_jobs j JOIN media_items m ON m.id=j.media_item_id
		WHERE j.state IN ('pending','retry','running') AND j.next_attempt_at_ms<=? AND j.lease_until_ms<=?
		ORDER BY j.next_attempt_at_ms, j.media_item_id LIMIT 1`, nowMS, nowMS).Scan(
		&item.ID, &item.JobRevision, &item.MediaType, &item.Title, &item.OriginalTitle, &item.ProductionYear,
		&item.SeriesName, &item.SeasonNumber, &item.EpisodeNumber, &item.TMDBType, &item.TMDBID, &item.IMDBID, &item.TVDBID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return tmdbLookupMedia{}, false, nil
	}
	if err != nil {
		return tmdbLookupMedia{}, false, err
	}
	result, err := tx.Exec(`UPDATE tmdb_jobs SET state='running', attempts=attempts+1, lease_until_ms=?, updated_at_ms=?
		WHERE media_item_id=? AND next_attempt_at_ms<=? AND lease_until_ms<=?`, nowMS+tmdbJobLease.Milliseconds(), nowMS, item.ID, nowMS, nowMS)
	if err != nil {
		return tmdbLookupMedia{}, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return tmdbLookupMedia{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return tmdbLookupMedia{}, false, err
	}
	return item, true, nil
}

func prepareTMDBJobResult(tx *sql.Tx, mediaItemID, expectedRevision, nowMS int64) (bool, error) {
	var state string
	var currentRevision int64
	if err := tx.QueryRow("SELECT state, revision FROM tmdb_jobs WHERE media_item_id=?", mediaItemID).Scan(&state, &currentRevision); err != nil {
		return false, err
	}
	if state == "running" && currentRevision == expectedRevision {
		return true, nil
	}
	if currentRevision != expectedRevision {
		if _, err := tx.Exec(`UPDATE tmdb_jobs SET state='pending', attempts=0, next_attempt_at_ms=0,
			lease_until_ms=0, last_error_code='', updated_at_ms=? WHERE media_item_id=?`, nowMS, mediaItemID); err != nil {
			return false, err
		}
	}
	return false, nil
}

func (d *DB) completeTMDBJob(mediaItemID, expectedRevision int64, metadata tmdbMetadata, language string, nowMS int64) error {
	if mediaItemID <= 0 || metadata.TMDBID <= 0 || (metadata.TMDBType != "movie" && metadata.TMDBType != "tv") {
		return fmt.Errorf("invalid TMDB metadata")
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := prepareTMDBJobResult(tx, mediaItemID, expectedRevision, nowMS)
	if err != nil {
		return err
	}
	if !current {
		return tx.Commit()
	}
	castJSON := marshalTMDBCast(metadata.Cast)
	genresJSON := marshalTMDBStrings(metadata.Genres, tmdbGenresJSONMaxBytes)
	stillsJSON := marshalTMDBStrings(metadata.Stills, tmdbStillsJSONMaxBytes)
	if _, err := tx.Exec(`INSERT INTO tmdb_cache
		(tmdb_type, tmdb_id, language, title, original_title, overview, release_year, release_date, poster_path, backdrop_path,
		 vote_average, genres_json, status, last_air_date, next_air_date, next_season_number, next_episode_number,
		 next_episode_name, season_count, episode_count, stills_json, cast_json, details_version, updated_at_ms, expires_at_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(tmdb_type,tmdb_id,language) DO UPDATE SET title=excluded.title, original_title=excluded.original_title,
		overview=excluded.overview, release_year=excluded.release_year, release_date=excluded.release_date,
		poster_path=excluded.poster_path, backdrop_path=excluded.backdrop_path, vote_average=excluded.vote_average,
		genres_json=excluded.genres_json, status=excluded.status, last_air_date=excluded.last_air_date, next_air_date=excluded.next_air_date,
		next_season_number=excluded.next_season_number, next_episode_number=excluded.next_episode_number, next_episode_name=excluded.next_episode_name,
		season_count=excluded.season_count, episode_count=excluded.episode_count, stills_json=excluded.stills_json, cast_json=excluded.cast_json,
		details_version=excluded.details_version,
		updated_at_ms=excluded.updated_at_ms, expires_at_ms=excluded.expires_at_ms`,
		metadata.TMDBType, metadata.TMDBID, language, metadata.Title, metadata.OriginalTitle, metadata.Overview,
		metadata.ReleaseYear, normalizeTMDBStoredDate(metadata.ReleaseDate), metadata.PosterPath, metadata.BackdropPath,
		metadata.VoteAverage, genresJSON, normalizeTMDBStoredStatus(metadata.Status), normalizeTMDBStoredDate(metadata.LastAirDate),
		normalizeTMDBStoredDate(metadata.NextAirDate), metadata.NextSeasonNumber, metadata.NextEpisodeNumber,
		normalizeTMDBStoredStatus(metadata.NextEpisodeName), metadata.SeasonCount, metadata.EpisodeCount, stillsJSON, castJSON, tmdbDetailsVersion,
		nowMS, nowMS+tmdbCacheLifetime.Milliseconds()); err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE media_items SET tmdb_type=?, tmdb_id=?,
		title=CASE WHEN media_type<>'episode' AND title='' AND ?<>'' THEN ? ELSE title END,
		original_title=CASE WHEN media_type<>'episode' AND original_title='' AND ?<>'' THEN ? ELSE original_title END,
		series_name=CASE WHEN media_type='episode' AND series_name='' AND ?<>'' THEN ? ELSE series_name END,
		overview=?, poster_path=?, cast_json=?, details_version=?, backdrop_path=?, release_date=?, vote_average=?, genres_json=?, status=?,
		last_air_date=?, next_air_date=?, next_season_number=?, next_episode_number=?, next_episode_name=?, season_count=?, episode_count=?, stills_json=?,
		match_status=?, metadata_updated_at_ms=?, updated_at_ms=MAX(updated_at_ms,?) WHERE id=?`,
		metadata.TMDBType, metadata.TMDBID,
		metadata.Title, metadata.Title, metadata.OriginalTitle, metadata.OriginalTitle,
		metadata.Title, metadata.Title,
		metadata.Overview, metadata.PosterPath, castJSON, tmdbDetailsVersion, metadata.BackdropPath, normalizeTMDBStoredDate(metadata.ReleaseDate), metadata.VoteAverage,
		genresJSON, normalizeTMDBStoredStatus(metadata.Status), normalizeTMDBStoredDate(metadata.LastAirDate), normalizeTMDBStoredDate(metadata.NextAirDate),
		metadata.NextSeasonNumber, metadata.NextEpisodeNumber, normalizeTMDBStoredStatus(metadata.NextEpisodeName), metadata.SeasonCount, metadata.EpisodeCount,
		stillsJSON, metadata.MatchMethod, nowMS, nowMS, mediaItemID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		if err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if _, err := tx.Exec(`UPDATE tmdb_jobs SET state='done', lease_until_ms=0, last_error_code='', updated_at_ms=?
		WHERE media_item_id=? AND state='running' AND revision=?`, nowMS, mediaItemID, expectedRevision); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) retryTMDBJob(mediaItemID, expectedRevision int64, attempts int, errorCode string, nextAttemptMS int64) error {
	if attempts < 1 {
		attempts = 1
	}
	if len(errorCode) > 64 {
		errorCode = errorCode[:64]
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	nowMS := time.Now().UnixMilli()
	current, err := prepareTMDBJobResult(tx, mediaItemID, expectedRevision, nowMS)
	if err != nil {
		return err
	}
	if !current {
		return tx.Commit()
	}
	if _, err := tx.Exec(`UPDATE tmdb_jobs SET state='retry', lease_until_ms=0, next_attempt_at_ms=?, last_error_code=?, updated_at_ms=?
		WHERE media_item_id=? AND state='running' AND revision=?`, nextAttemptMS, errorCode, nowMS, mediaItemID, expectedRevision); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) finishTMDBJobWithoutMatch(mediaItemID, expectedRevision int64, reason string, nowMS int64) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := prepareTMDBJobResult(tx, mediaItemID, expectedRevision, nowMS)
	if err != nil {
		return err
	}
	if !current {
		return tx.Commit()
	}
	if _, err := tx.Exec(`UPDATE media_items SET match_status=?, metadata_updated_at_ms=? WHERE id=?`, reason, nowMS, mediaItemID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE tmdb_jobs SET state='done', lease_until_ms=0, last_error_code=?, updated_at_ms=?
		WHERE media_item_id=? AND state='running' AND revision=?`, reason, nowMS, mediaItemID, expectedRevision); err != nil {
		return err
	}
	return tx.Commit()
}

func validTMDBPosterPath(value string) bool {
	if len(value) < 2 || len(value) > 256 || value[0] != '/' || strings.Contains(value, "\\") || strings.Contains(value, "..") || strings.ContainsAny(value, "?#\x00") {
		return false
	}
	switch strings.ToLower(path.Ext(value)) {
	case ".jpg", ".jpeg", ".png", ".webp":
		return true
	default:
		return false
	}
}

func (s *tmdbService) fetchPoster(ctx context.Context, mediaItemID int64) ([]byte, string, error) {
	return s.fetchWatchHistoryImage(ctx, mediaItemID, "poster", 0)
}

func (s *tmdbService) fetchWatchHistoryImage(ctx context.Context, mediaItemID int64, kind string, index int) ([]byte, string, error) {
	if s == nil || s.db == nil || s.client == nil || mediaItemID <= 0 {
		return nil, "", sql.ErrNoRows
	}
	var imagePath string
	switch kind {
	case "poster":
		if err := s.db.db.QueryRow("SELECT poster_path FROM media_items WHERE id=?", mediaItemID).Scan(&imagePath); err != nil {
			return nil, "", err
		}
		if !validTMDBPosterPath(imagePath) {
			return nil, "", sql.ErrNoRows
		}
	case "backdrop":
		if err := s.db.db.QueryRow("SELECT backdrop_path FROM media_items WHERE id=?", mediaItemID).Scan(&imagePath); err != nil {
			return nil, "", err
		}
		if !validTMDBImagePath(imagePath) {
			return nil, "", sql.ErrNoRows
		}
	case "still":
		var stillsJSON string
		if err := s.db.db.QueryRow("SELECT stills_json FROM media_items WHERE id=?", mediaItemID).Scan(&stillsJSON); err != nil {
			return nil, "", err
		}
		stills := decodeTMDBStrings(stillsJSON, tmdbStillsJSONMaxBytes, 12, func(value string) string {
			value = strings.TrimSpace(value)
			if !validTMDBImagePath(value) {
				return ""
			}
			return value
		})
		if index < 0 || index >= len(stills) {
			return nil, "", sql.ErrNoRows
		}
		imagePath = stills[index]
	case "cast":
		var castJSON string
		if err := s.db.db.QueryRow("SELECT cast_json FROM media_items WHERE id=?", mediaItemID).Scan(&castJSON); err != nil {
			return nil, "", err
		}
		cast := decodeTMDBCast(castJSON)
		if index < 0 || index >= len(cast) || !validTMDBImagePath(cast[index].ProfilePath) {
			return nil, "", sql.ErrNoRows
		}
		imagePath = cast[index].ProfilePath
	default:
		return nil, "", sql.ErrNoRows
	}
	if imagePath == "" {
		return nil, "", sql.ErrNoRows
	}
	base, err := url.Parse(strings.TrimRight(s.client.imageBase, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, "", fmt.Errorf("image service unavailable")
	}
	imageBase := base
	if kind == "backdrop" || kind == "still" {
		imageBase = tmdbImageBaseWithSize(base, "w1280")
	} else if kind == "cast" {
		imageBase = tmdbImageBaseWithSize(base, "w185")
	}
	imageURL := strings.TrimRight(imageBase.String(), "/") + imagePath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imageURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/jpeg")
	req.Header.Set("User-Agent", "Meridian/"+strings.TrimPrefix(appVersion, "v"))
	resp, err := s.client.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 32<<10))
		return nil, "", fmt.Errorf("image upstream status %d", resp.StatusCode)
	}
	if resp.ContentLength > tmdbPosterMaxBytes {
		return nil, "", fmt.Errorf("image too large")
	}
	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, "", fmt.Errorf("invalid image content type")
	}
	switch contentType {
	case "image/jpeg", "image/png", "image/webp":
	default:
		return nil, "", fmt.Errorf("unsupported image content type")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, tmdbPosterMaxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > tmdbPosterMaxBytes {
		return nil, "", fmt.Errorf("image too large")
	}
	return data, contentType, nil
}

func tmdbImageBaseWithSize(base *url.URL, size string) *url.URL {
	if base == nil || strings.TrimSpace(size) == "" {
		return base
	}
	copyURL := *base
	parts := strings.Split(strings.Trim(copyURL.Path, "/"), "/")
	for index, part := range parts {
		if strings.HasPrefix(strings.ToLower(part), "w") && len(part) > 1 {
			parts[index] = size
			copyURL.Path = "/" + strings.Join(parts, "/")
			return &copyURL
		}
	}
	return &copyURL
}
