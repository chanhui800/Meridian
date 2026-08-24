package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type tmdbSettingsInput struct {
	Enabled              *bool  `json:"enabled"`
	Token                string `json:"token"`
	ClearToken           bool   `json:"clear_token"`
	Language             string `json:"language"`
	HistoryRetentionDays *int   `json:"history_retention_days"`
}

func (a *App) handleTMDBSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Cache-Control", "no-store")
		settings, err := a.db.tmdbPublicSettings()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "读取 TMDB 设置失败")
			return
		}
		a.jsonOK(w, settings)
	case http.MethodPost:
		var input tmdbSettingsInput
		if err := decodeJSONBody(w, r, &input); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "TMDB 设置格式无效")
			return
		}
		stored, err := a.db.tmdbSettings()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "读取 TMDB 设置失败")
			return
		}
		settings := stored.TMDBSettings
		if input.Enabled != nil {
			settings.Enabled = *input.Enabled
		}
		if input.Language != "" {
			settings.Language = input.Language
		}
		if input.HistoryRetentionDays != nil {
			settings.HistoryRetentionDays = *input.HistoryRetentionDays
		}
		ciphertext := stored.TokenCiphertext
		replaceToken := false
		if input.ClearToken {
			ciphertext = ""
			replaceToken = true
		}
		if strings.TrimSpace(input.Token) != "" {
			if jwtSecretEphemeral {
				a.jsonErr(w, http.StatusBadRequest, "当前 JWT_SECRET 为临时值，不能安全保存 TMDB Token")
				return
			}
			ciphertext, err = encryptTMDBReadToken(input.Token)
			if err != nil {
				a.jsonErr(w, http.StatusBadRequest, err.Error())
				return
			}
			replaceToken = true
		}
		if settings.Enabled && ciphertext == "" {
			a.jsonErr(w, http.StatusBadRequest, "启用 TMDB 前必须配置 Read Access Token")
			return
		}
		if err := a.db.saveTMDBSettings(settings, ciphertext, replaceToken); err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if replaceToken || input.Language != "" {
			_ = a.db.resetTMDBJobs()
		}
		if service := a.tmdbService(); service != nil {
			service.Wake()
		}
		fresh, err := a.db.tmdbPublicSettings()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "读取 TMDB 设置失败")
			return
		}
		a.jsonOK(w, fresh)
	default:
		w.Header().Set("Allow", "GET, POST")
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *App) handleTMDBCacheClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var input struct {
		Scope string `json:"scope"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSONBody(w, r, &input); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "TMDB 缓存清理参数无效")
			return
		}
	}
	scope := strings.ToLower(strings.TrimSpace(input.Scope))
	if scope == "" {
		scope = "stale"
	}
	if scope != "stale" && scope != "all" {
		a.jsonErr(w, http.StatusBadRequest, "TMDB 缓存清理范围无效")
		return
	}
	if err := a.db.clearTMDBCache(scope); err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "清理 TMDB 缓存失败")
		return
	}
	settings, err := a.db.tmdbPublicSettings()
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "读取 TMDB 缓存状态失败")
		return
	}
	a.jsonOK(w, map[string]any{"scope": scope, "settings": settings})
}

func (a *App) handleTMDBTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var input struct {
		Token string `json:"token"`
	}
	if r.ContentLength != 0 {
		if err := decodeJSONBody(w, r, &input); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "TMDB 测试参数无效")
			return
		}
	}
	token := strings.TrimSpace(input.Token)
	usingStored := token == ""
	if usingStored {
		stored, err := a.db.tmdbSettings()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "读取 TMDB 设置失败")
			return
		}
		if stored.TokenCiphertext == "" {
			a.jsonErr(w, http.StatusBadRequest, "请先填写并保存 TMDB Read Access Token")
			return
		}
		token, err = decryptTMDBReadToken(stored.TokenCiphertext)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "TMDB Token 无法解密，请重新保存")
			return
		}
	} else {
		var err error
		token, err = validateTMDBReadToken(token)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	service := a.tmdbService()
	if service == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "TMDB 服务不可用")
		return
	}
	nowMS := time.Now().UnixMilli()
	if err := service.client.testCredentials(r.Context(), token); err != nil {
		if usingStored {
			_ = a.db.markTMDBCredentialResult(tmdbCredentialInvalid, tmdbSafeErrorCode(err), nowMS)
		}
		a.jsonErr(w, http.StatusBadGateway, tmdbChineseError(err))
		return
	}
	if usingStored {
		_ = a.db.markTMDBCredentialResult(tmdbCredentialReady, "", nowMS)
		service.Wake()
	}
	settings, _ := a.db.tmdbPublicSettings()
	a.jsonOK(w, map[string]any{"connected": true, "settings": settings})
}

func tmdbSafeErrorCode(err error) string {
	var apiErr *tmdbAPIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return "network"
}

func tmdbChineseError(err error) string {
	switch tmdbSafeErrorCode(err) {
	case "auth":
		return "TMDB Token 无效或无权访问"
	case "rate_limited":
		return "TMDB 请求过于频繁，请稍后重试"
	case "server":
		return "TMDB 服务暂时不可用"
	case "response_too_large", "invalid_json", "invalid_content_type", "invalid_configuration":
		return "TMDB 返回了无法识别的数据"
	default:
		return "无法连接 TMDB，请检查服务器网络后重试"
	}
}

func (d *DB) resetTMDBJobs() error {
	_, err := d.db.Exec(`UPDATE tmdb_jobs SET state='pending', next_attempt_at_ms=0, lease_until_ms=0, last_error_code='', updated_at_ms=?`, time.Now().UnixMilli())
	return err
}

func (a *App) handleWatchHistoryPoster(w http.ResponseWriter, r *http.Request) {
	a.handleWatchHistoryImage(w, r, "poster", "/api/watch-history/posters/")
}

func (a *App) handleWatchHistoryBackdrop(w http.ResponseWriter, r *http.Request) {
	a.handleWatchHistoryImage(w, r, "backdrop", "/api/watch-history/backdrops/")
}

func (a *App) handleWatchHistoryStill(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimPrefix(r.URL.Path, "/api/watch-history/stills/")
	parts := strings.Split(raw, "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	index, err := strconv.Atoi(parts[1])
	if err != nil || index < 0 || index >= 12 {
		http.NotFound(w, r)
		return
	}
	a.handleWatchHistoryImageWithIndex(w, r, "still", "/api/watch-history/stills/", index)
}

func (a *App) handleWatchHistoryCast(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimPrefix(r.URL.Path, "/api/watch-history/cast/")
	parts := strings.Split(raw, "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	index, err := strconv.Atoi(parts[1])
	if err != nil || index < 0 || index >= 20 {
		http.NotFound(w, r)
		return
	}
	a.handleWatchHistoryImageWithIndex(w, r, "cast", "/api/watch-history/cast/", index)
}

func (a *App) handleWatchHistoryImage(w http.ResponseWriter, r *http.Request, kind, prefix string) {
	a.handleWatchHistoryImageWithIndex(w, r, kind, prefix, 0)
}

func (a *App) handleWatchHistoryImageWithIndex(w http.ResponseWriter, r *http.Request, kind, prefix string, index int) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	rawID := strings.TrimPrefix(r.URL.Path, prefix)
	if kind == "still" || kind == "cast" {
		rawID = strings.Split(rawID, "/")[0]
	}
	if rawID == "" || strings.Contains(rawID, "/") {
		http.NotFound(w, r)
		return
	}
	mediaItemID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || mediaItemID <= 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	service := a.tmdbService()
	if service == nil {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodHead {
		if !a.watchHistoryImageExists(mediaItemID, kind, index) {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	data, contentType, err := service.fetchWatchHistoryImage(r.Context(), mediaItemID, kind, index)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			w.Header().Set("Cache-Control", "private, no-store")
		}
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (a *App) watchHistoryImageExists(mediaItemID int64, kind string, index int) bool {
	if mediaItemID <= 0 {
		return false
	}
	switch kind {
	case "poster":
		var imagePath string
		return a.db.db.QueryRow("SELECT poster_path FROM media_items WHERE id=?", mediaItemID).Scan(&imagePath) == nil && validTMDBPosterPath(imagePath)
	case "backdrop":
		var imagePath string
		return a.db.db.QueryRow("SELECT backdrop_path FROM media_items WHERE id=?", mediaItemID).Scan(&imagePath) == nil && validTMDBImagePath(imagePath)
	case "still":
		var stillsJSON string
		if a.db.db.QueryRow("SELECT stills_json FROM media_items WHERE id=?", mediaItemID).Scan(&stillsJSON) != nil {
			return false
		}
		stills := decodeTMDBStrings(stillsJSON, tmdbStillsJSONMaxBytes, 12, func(value string) string {
			value = strings.TrimSpace(value)
			if !validTMDBImagePath(value) {
				return ""
			}
			return value
		})
		return index >= 0 && index < len(stills)
	case "cast":
		var castJSON string
		if a.db.db.QueryRow("SELECT cast_json FROM media_items WHERE id=?", mediaItemID).Scan(&castJSON) != nil {
			return false
		}
		cast := decodeTMDBCast(castJSON)
		return index >= 0 && index < len(cast) && validTMDBImagePath(cast[index].ProfilePath)
	default:
		return false
	}
}
