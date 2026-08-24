package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	tmdbTokenCipherPrefix       = "v1:"
	tmdbDefaultLanguage         = "zh-CN"
	tmdbDefaultHistoryRetention = 90
	tmdbMinHistoryRetention     = 1
	tmdbMaxHistoryRetention     = 3650
	tmdbCredentialUnconfigured  = "unconfigured"
	tmdbCredentialReady         = "ready"
	tmdbCredentialInvalid       = "invalid"
	tmdbCredentialUnknown       = "unknown"
)

type TMDBSettings struct {
	Enabled              bool   `json:"enabled"`
	Configured           bool   `json:"configured"`
	SecretStable         bool   `json:"secret_stable"`
	Language             string `json:"language"`
	HistoryRetentionDays int    `json:"history_retention_days"`
	CredentialState      string `json:"credential_state"`
	LastErrorCode        string `json:"last_error_code,omitempty"`
	LastTestedAtMS       int64  `json:"last_tested_at_ms"`
	CacheEntries         int    `json:"cache_entries"`
	CacheSizeBytes       int64  `json:"cache_size_bytes"`
}

type tmdbStoredSettings struct {
	TMDBSettings
	TokenCiphertext string
}

func tmdbTokenKeyForSecret(secret []byte) []byte {
	h := sha256.New()
	_, _ = h.Write([]byte("meridian tmdb read access token v1\x00"))
	_, _ = h.Write(secret)
	return h.Sum(nil)
}

func validateTMDBReadToken(token string) (string, error) {
	token = strings.TrimSpace(token)
	if len(token) < 32 || len(token) > 2048 {
		return "", fmt.Errorf("TMDB Read Access Token 长度无效")
	}
	for _, r := range token {
		if r <= 0x20 || r == 0x7f {
			return "", fmt.Errorf("TMDB Read Access Token 包含非法字符")
		}
	}
	return token, nil
}

func encryptTMDBReadToken(token string) (string, error) {
	return encryptTMDBReadTokenWithSecret(token, jwtSecret)
}

func encryptTMDBReadTokenWithSecret(token string, secret []byte) (string, error) {
	token, err := validateTMDBReadToken(token)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(tmdbTokenKeyForSecret(secret))
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
	sealed := gcm.Seal(nil, nonce, []byte(token), []byte("meridian-tmdb"))
	return tmdbTokenCipherPrefix + base64.RawURLEncoding.EncodeToString(append(nonce, sealed...)), nil
}

func decryptTMDBReadToken(ciphertext string) (string, error) {
	return decryptTMDBReadTokenWithSecret(ciphertext, jwtSecret)
}

func decryptTMDBReadTokenWithSecret(ciphertext string, secret []byte) (string, error) {
	if !strings.HasPrefix(ciphertext, tmdbTokenCipherPrefix) {
		return "", fmt.Errorf("invalid TMDB token ciphertext")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(ciphertext, tmdbTokenCipherPrefix))
	if err != nil {
		return "", fmt.Errorf("decode TMDB token: %w", err)
	}
	block, err := aes.NewCipher(tmdbTokenKeyForSecret(secret))
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(payload) < gcm.NonceSize()+gcm.Overhead() {
		return "", fmt.Errorf("invalid TMDB token ciphertext")
	}
	plain, err := gcm.Open(nil, payload[:gcm.NonceSize()], payload[gcm.NonceSize():], []byte("meridian-tmdb"))
	if err != nil {
		return "", fmt.Errorf("decrypt TMDB token: %w", err)
	}
	return validateTMDBReadToken(string(plain))
}

func normalizeTMDBLanguage(language string) (string, error) {
	language = strings.TrimSpace(language)
	if language == "" {
		return tmdbDefaultLanguage, nil
	}
	switch language {
	case "zh-CN", "zh-TW", "en-US":
		return language, nil
	default:
		return "", fmt.Errorf("language must be zh-CN, zh-TW or en-US")
	}
}

func normalizeTMDBHistoryRetention(days int) (int, error) {
	if days == 0 {
		return tmdbDefaultHistoryRetention, nil
	}
	if days < tmdbMinHistoryRetention || days > tmdbMaxHistoryRetention {
		return 0, fmt.Errorf("history_retention_days must be between %d and %d", tmdbMinHistoryRetention, tmdbMaxHistoryRetention)
	}
	return days, nil
}

func normalizeTMDBCredentialState(state string, configured bool) string {
	state = strings.TrimSpace(strings.ToLower(state))
	if !configured {
		return tmdbCredentialUnconfigured
	}
	switch state {
	case tmdbCredentialReady, tmdbCredentialInvalid, tmdbCredentialUnknown:
		return state
	default:
		return tmdbCredentialUnknown
	}
}

func (d *DB) tmdbSettings() (tmdbStoredSettings, error) {
	var stored tmdbStoredSettings
	var enabled int
	err := d.db.QueryRow(`SELECT enabled, token_ciphertext, language, history_retention_days,
		credential_state, last_error_code, last_tested_at_ms FROM tmdb_settings WHERE id=1`).Scan(
		&enabled, &stored.TokenCiphertext, &stored.Language, &stored.HistoryRetentionDays,
		&stored.CredentialState, &stored.LastErrorCode, &stored.LastTestedAtMS,
	)
	if errors.Is(err, sql.ErrNoRows) {
		stored.Language = tmdbDefaultLanguage
		stored.HistoryRetentionDays = tmdbDefaultHistoryRetention
		stored.CredentialState = tmdbCredentialUnconfigured
		stored.SecretStable = !jwtSecretEphemeral
		return stored, nil
	}
	if err != nil {
		return stored, err
	}
	stored.Enabled = enabled == 1
	stored.Configured = stored.TokenCiphertext != ""
	stored.SecretStable = !jwtSecretEphemeral
	stored.Language, err = normalizeTMDBLanguage(stored.Language)
	if err != nil {
		return stored, err
	}
	stored.HistoryRetentionDays, err = normalizeTMDBHistoryRetention(stored.HistoryRetentionDays)
	if err != nil {
		return stored, err
	}
	stored.CredentialState = normalizeTMDBCredentialState(stored.CredentialState, stored.Configured)
	return stored, nil
}

func tmdbPublicSettings(stored tmdbStoredSettings) TMDBSettings {
	settings := stored.TMDBSettings
	settings.Configured = stored.TokenCiphertext != ""
	settings.SecretStable = !jwtSecretEphemeral
	settings.CredentialState = normalizeTMDBCredentialState(settings.CredentialState, settings.Configured)
	return settings
}

func (d *DB) tmdbPublicSettings() (TMDBSettings, error) {
	stored, err := d.tmdbSettings()
	if err != nil {
		return TMDBSettings{}, err
	}
	settings := tmdbPublicSettings(stored)
	var entries int
	var sizeBytes int64
	err = d.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(
		COALESCE(length(title),0)+COALESCE(length(original_title),0)+COALESCE(length(overview),0)+
		COALESCE(length(poster_path),0)+COALESCE(length(backdrop_path),0)+COALESCE(length(genres_json),0)+
		COALESCE(length(status),0)+COALESCE(length(last_air_date),0)+COALESCE(length(next_air_date),0)+
		COALESCE(length(next_episode_name),0)+COALESCE(length(stills_json),0)+COALESCE(length(cast_json),0)), 0)
		FROM tmdb_cache`).Scan(&entries, &sizeBytes)
	if err != nil {
		return TMDBSettings{}, err
	}
	// Older databases may contain the copied metadata on media_items while the
	// normalized tmdb_cache row was never written (or was removed by an older
	// cleanup routine). Treat those copied fields as cache entries so the panel
	// does not report 0 even when an interrupted migration left the timestamp at
	// its zero value. Manual cache clearing also resets the copied fields, so it
	// still correctly reports an empty cache after a clear operation.
	if entries == 0 {
		err = d.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(
			COALESCE(length(title),0)+COALESCE(length(original_title),0)+COALESCE(length(overview),0)+
			COALESCE(length(poster_path),0)+COALESCE(length(backdrop_path),0)+COALESCE(length(genres_json),0)+
			COALESCE(length(status),0)+COALESCE(length(last_air_date),0)+COALESCE(length(next_air_date),0)+
			COALESCE(length(next_episode_name),0)+COALESCE(length(stills_json),0)+COALESCE(length(cast_json),0)), 0)
			FROM media_items
			WHERE (
				overview<>'' OR poster_path<>'' OR backdrop_path<>'' OR genres_json<>'[]' OR
				status<>'' OR last_air_date<>'' OR next_air_date<>'' OR next_episode_name<>'' OR
				stills_json<> '[]' OR cast_json<> '[]')`).Scan(&entries, &sizeBytes)
		if err != nil {
			return TMDBSettings{}, err
		}
	}
	settings.CacheEntries = entries
	settings.CacheSizeBytes = sizeBytes
	return settings, nil
}

func (d *DB) saveTMDBSettings(settings TMDBSettings, tokenCiphertext string, replaceToken bool) error {
	language, err := normalizeTMDBLanguage(settings.Language)
	if err != nil {
		return err
	}
	retention, err := normalizeTMDBHistoryRetention(settings.HistoryRetentionDays)
	if err != nil {
		return err
	}
	if settings.Enabled && tokenCiphertext == "" && replaceToken {
		return fmt.Errorf("启用 TMDB 前必须配置 Read Access Token")
	}
	if replaceToken {
		state := tmdbCredentialUnknown
		if tokenCiphertext == "" {
			settings.Enabled = false
			state = tmdbCredentialUnconfigured
		}
		_, err = d.db.Exec(`UPDATE tmdb_settings SET enabled=?, token_ciphertext=?, language=?, history_retention_days=?,
			credential_state=?, last_error_code='', last_tested_at_ms=0, updated_at=CURRENT_TIMESTAMP WHERE id=1`,
			sqliteBool(settings.Enabled), tokenCiphertext, language, retention, state)
		return err
	}
	_, err = d.db.Exec(`UPDATE tmdb_settings SET enabled=?, language=?, history_retention_days=?, updated_at=CURRENT_TIMESTAMP WHERE id=1`,
		sqliteBool(settings.Enabled), language, retention)
	return err
}

func (d *DB) clearTMDBCache(scope string) error {
	if d == nil || d.db == nil {
		return errDynamicObservationWriterClosed
	}
	if err := d.flushDynamicObservations(); err != nil {
		return err
	}
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		scope = "stale"
	}
	if scope != "stale" && scope != "all" {
		return fmt.Errorf("invalid TMDB cache scope")
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	nowMS := time.Now().UnixMilli()
	mediaWhere := "1=1"
	mediaArgs := []any{nowMS}
	if scope == "stale" {
		mediaWhere = `EXISTS (
			SELECT 1 FROM tmdb_cache tc
			WHERE tc.tmdb_type=media_items.tmdb_type AND tc.tmdb_id=media_items.tmdb_id
			AND tc.expires_at_ms>0 AND tc.expires_at_ms<=?
		)`
		mediaArgs = append(mediaArgs, nowMS)
	}
	// tmdb_cache is only one layer of the cache. History cards also read the
	// copied fields in media_items, so clear those fields and requeue the job
	// or a manual cache clear would appear to do nothing in the UI.
	if _, err := tx.Exec(`UPDATE media_items SET overview='', poster_path='', backdrop_path='', release_date='',
		vote_average=0, genres_json='[]', status='', last_air_date='', next_air_date='', next_season_number=-1,
		next_episode_number=-1, next_episode_name='', season_count=0, episode_count=0, stills_json='[]', cast_json='[]',
		details_version=0, match_status='pending', metadata_updated_at_ms=0, updated_at_ms=MAX(updated_at_ms,?)
		WHERE `+mediaWhere, mediaArgs...); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO tmdb_jobs
		(media_item_id, state, attempts, next_attempt_at_ms, lease_until_ms, last_error_code, revision, updated_at_ms)
		SELECT id, 'pending', 0, 0, 0, '', 0, ? FROM media_items WHERE `+mediaWhere, append([]any{nowMS}, mediaArgs[1:]...)...); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE tmdb_jobs SET state='pending', attempts=0,
		next_attempt_at_ms=0, lease_until_ms=0, last_error_code='', revision=revision+1, updated_at_ms=?
		WHERE media_item_id IN (SELECT id FROM media_items WHERE `+mediaWhere+`)`, append([]any{nowMS}, mediaArgs[1:]...)...); err != nil {
		return err
	}
	if scope == "all" {
		if _, err := tx.Exec("DELETE FROM tmdb_cache"); err != nil {
			return err
		}
	} else if _, err := tx.Exec(`DELETE FROM tmdb_cache WHERE
		(expires_at_ms>0 AND expires_at_ms<=?) OR NOT EXISTS (
			SELECT 1 FROM media_items mi WHERE mi.tmdb_type=tmdb_cache.tmdb_type AND mi.tmdb_id=tmdb_cache.tmdb_id
		)`, nowMS); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) markTMDBCredentialResult(state, errorCode string, testedAtMS int64) error {
	state = normalizeTMDBCredentialState(state, true)
	if len(errorCode) > 64 {
		errorCode = errorCode[:64]
	}
	_, err := d.db.Exec(`UPDATE tmdb_settings SET credential_state=?, last_error_code=?, last_tested_at_ms=?, updated_at=CURRENT_TIMESTAMP WHERE id=1`, state, errorCode, testedAtMS)
	return err
}
