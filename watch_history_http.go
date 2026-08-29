package main

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type watchHistoryResponseEntry struct {
	WatchHistoryEntry
	PosterAvailable bool    `json:"poster_available"`
	ProgressPercent float64 `json:"progress_percent"`
}

type watchHistoryResponse struct {
	Items         []watchHistoryResponseEntry `json:"items"`
	NextCursor    string                      `json:"next_cursor"`
	HasMore       bool                        `json:"has_more"`
	DroppedEvents uint64                      `json:"dropped_events"`
}

type watchHistoryActiveResponse struct {
	Items               []watchHistoryResponseEntry `json:"items"`
	ObservedAtMS        int64                       `json:"observed_at_ms"`
	ActiveWindowSeconds int64                       `json:"active_window_seconds"`
}

func encodeWatchHistoryCursor(lastSeenMS, id int64) string {
	if lastSeenMS <= 0 || id <= 0 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d:%d", lastSeenMS, id)))
}

func decodeWatchHistoryCursor(value string) (int64, int64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, 0, nil
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) > 64 {
		return 0, 0, fmt.Errorf("invalid cursor")
	}
	parts := strings.Split(string(decoded), ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid cursor")
	}
	beforeMS, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || beforeMS <= 0 {
		return 0, 0, fmt.Errorf("invalid cursor")
	}
	beforeID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || beforeID <= 0 {
		return 0, 0, fmt.Errorf("invalid cursor")
	}
	return beforeMS, beforeID, nil
}

func parseNonNegativeQueryInt64(value string) (int64, error) {
	if value == "" || strings.EqualFold(value, "all") {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("invalid numeric filter")
	}
	return parsed, nil
}

func (a *App) handleWatchHistory(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		query := r.URL.Query()
		siteID, err := parseNonNegativeQueryInt64(query.Get("site_id"))
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, "站点筛选无效")
			return
		}
		fromMS, err := parseNonNegativeQueryInt64(query.Get("from_ms"))
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, "开始时间无效")
			return
		}
		toMS, err := parseNonNegativeQueryInt64(query.Get("to_ms"))
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, "结束时间无效")
			return
		}
		if fromMS > 0 && toMS > 0 && fromMS > toMS {
			a.jsonErr(w, http.StatusBadRequest, "开始时间不能晚于结束时间")
			return
		}
		beforeMS, beforeID, err := decodeWatchHistoryCursor(query.Get("cursor"))
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, "分页游标无效")
			return
		}
		mediaType := strings.ToLower(strings.TrimSpace(query.Get("media_type")))
		if mediaType != "" && mediaType != "movie" && mediaType != "episode" && mediaType != "series" {
			a.jsonErr(w, http.StatusBadRequest, "媒体类型无效")
			return
		}
		limit := 24
		if rawLimit := query.Get("limit"); rawLimit != "" {
			parsed, parseErr := strconv.Atoi(rawLimit)
			if parseErr != nil || parsed < 1 || parsed > 100 {
				a.jsonErr(w, http.StatusBadRequest, "limit must be between 1 and 100")
				return
			}
			limit = parsed
		}
		readLimit := limit + 1
		if readLimit > 101 {
			readLimit = 101
		}
		entries, err := a.db.ListWatchHistory(WatchHistoryFilter{
			SiteID: siteID, MediaType: mediaType, Query: query.Get("q"), FromMS: fromMS, ToMS: toMS,
			BeforeMS: beforeMS, BeforeID: beforeID, Limit: readLimit,
		})
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "读取观看历史失败")
			return
		}
		hasMore := len(entries) > limit
		if hasMore {
			entries = entries[:limit]
		}
		items := make([]watchHistoryResponseEntry, 0, len(entries))
		for _, entry := range entries {
			progress := 0.0
			if entry.RunTimeTicks > 0 && entry.PositionTicks > 0 {
				progress = float64(entry.PositionTicks) * 100 / float64(entry.RunTimeTicks)
				if progress > 100 {
					progress = 100
				}
			}
			items = append(items, watchHistoryResponseEntry{WatchHistoryEntry: entry, PosterAvailable: validTMDBPosterPath(entry.PosterPath), ProgressPercent: progress})
		}
		nextCursor := ""
		if hasMore && len(entries) > 0 {
			last := entries[len(entries)-1]
			nextCursor = encodeWatchHistoryCursor(last.LastSeenAtMS, last.ID)
		}
		a.jsonOK(w, watchHistoryResponse{Items: items, NextCursor: nextCursor, HasMore: hasMore, DroppedEvents: a.db.DroppedWatchHistory()})
	case http.MethodDelete:
		siteID, err := parseNonNegativeQueryInt64(r.URL.Query().Get("site_id"))
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, "站点筛选无效")
			return
		}
		if err := a.db.ClearWatchHistory(siteID); err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "清理观看历史失败")
			return
		}
		a.jsonOK(w, map[string]any{"cleared": true, "site_id": siteID})
	default:
		w.Header().Set("Allow", "GET, DELETE")
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *App) handleWatchHistoryItem(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	rawID := strings.TrimPrefix(r.URL.Path, "/api/watch-history/")
	if rawID == "active" {
		a.handleActiveWatchHistory(w, r)
		return
	}
	if rawID == "" || strings.Contains(rawID, "/") {
		http.NotFound(w, r)
		return
	}
	historyID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || historyID <= 0 {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if err := a.db.DeleteWatchHistory(historyID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			a.jsonErr(w, http.StatusNotFound, "观看记录不存在")
			return
		}
		a.jsonErr(w, http.StatusInternalServerError, "删除观看记录失败")
		return
	}
	a.jsonOK(w, map[string]any{"deleted": true, "id": historyID})
}

func (a *App) handleActiveWatchHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	query := r.URL.Query()
	siteID, err := parseNonNegativeQueryInt64(query.Get("site_id"))
	if err != nil {
		a.jsonErr(w, http.StatusBadRequest, "站点筛选无效")
		return
	}
	mediaType := strings.ToLower(strings.TrimSpace(query.Get("media_type")))
	if mediaType != "" && mediaType != "movie" && mediaType != "episode" && mediaType != "series" {
		a.jsonErr(w, http.StatusBadRequest, "媒体类型无效")
		return
	}
	entries, err := a.db.ListActiveWatchHistory(WatchHistoryFilter{SiteID: siteID, MediaType: mediaType, Query: query.Get("q")})
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "读取正在观看失败")
		return
	}
	items := make([]watchHistoryResponseEntry, 0, len(entries))
	for _, entry := range entries {
		progress := 0.0
		if entry.RunTimeTicks > 0 && entry.PositionTicks > 0 {
			progress = float64(entry.PositionTicks) * 100 / float64(entry.RunTimeTicks)
			if progress > 100 {
				progress = 100
			}
		}
		items = append(items, watchHistoryResponseEntry{WatchHistoryEntry: entry, PosterAvailable: validTMDBPosterPath(entry.PosterPath), ProgressPercent: progress})
	}
	a.jsonOK(w, watchHistoryActiveResponse{Items: items, ObservedAtMS: time.Now().UnixMilli(), ActiveWindowSeconds: int64(watchHistoryActiveWindow / time.Second)})
}
