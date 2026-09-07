package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (a *App) handleTraffic(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/traffic/")

	if path == "overview" {
		snap, err := a.pm.TrafficSnapshot()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "traffic overview unavailable")
			return
		}
		a.jsonOK(w, snap)
		return
	}

	envelope := false
	if strings.HasSuffix(path, "/snapshot") {
		envelope = true
		path = strings.TrimSuffix(path, "/snapshot")
	}

	siteID, err := strconv.ParseInt(path, 10, 64)
	if err != nil {
		a.jsonErr(w, 400, "invalid site id")
		return
	}

	hours := 24
	if h := r.URL.Query().Get("hours"); h != "" {
		if v, err := strconv.Atoi(h); err == nil && v >= 1 && v <= 24*366 {
			hours = v
		} else {
			a.jsonErr(w, http.StatusBadRequest, "hours must be between 1 and 8784")
			return
		}
	}

	site, err := a.db.GetSite(siteID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if envelope {
				a.jsonErr(w, http.StatusNotFound, "site not found")
				return
			}
			// The legacy endpoint keeps returning an empty log array for
			// unknown sites.
			a.jsonOK(w, []TrafficLog{})
			return
		}
		a.jsonErr(w, 500, err.Error())
		return
	}

	history, err := a.pm.SiteTrafficHistory(*site, hours)
	if err != nil {
		a.jsonErr(w, 500, err.Error())
		return
	}
	if envelope {
		a.jsonOK(w, history)
		return
	}
	a.jsonOK(w, history.Logs)
}

// GET/DELETE /api/asset-cache
func (a *App) handleAssetCache(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		sites, total, err := a.pm.AssetCacheSizes()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "cache statistics unavailable")
			return
		}
		nodes := make([]map[string]any, 0)
		if controlNodes, nodeErr := a.db.listControlNodes(time.Now()); nodeErr == nil {
			for _, node := range controlNodes {
				state := "applied"
				if node.CacheClearGeneration > node.CacheClearAppliedGeneration {
					state = "pending"
					if node.Status != "online" {
						state = "offline"
					}
				}
				nodes = append(nodes, map[string]any{
					"node_id":            node.ID,
					"status":             state,
					"generation":         node.CacheClearGeneration,
					"applied_generation": node.CacheClearAppliedGeneration,
				})
			}
		}
		a.jsonOK(w, map[string]interface{}{"total_bytes": total, "sites": sites, "nodes": nodes})
	case http.MethodDelete:
		if err := a.pm.ClearAssetCache(); err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "clear asset cache failed")
			return
		}
		nowMS := time.Now().UnixMilli()
		result, err := a.db.db.Exec(`UPDATE control_nodes
			SET cache_clear_generation=cache_clear_generation+1,updated_at_ms=?
			WHERE enabled=1 AND agent_token_hash<>''`, nowMS)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "schedule agent cache clear failed")
			return
		}
		pending, _ := result.RowsAffected()
		status := "cleared"
		if pending > 0 {
			status = "pending"
		}
		a.jsonOK(w, map[string]any{
			"status":        status,
			"local_cleared": true,
			"nodes_pending": pending,
		})
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// GET /api/ua-profiles
func (a *App) handleUAProfiles(w http.ResponseWriter, r *http.Request) {
	profiles := make([]UAProfile, 0, len(uaProfiles))
	for _, p := range uaProfiles {
		profiles = append(profiles, p)
	}
	a.jsonOK(w, profiles)
}

// GET /api/dynamic-profiles reports deploy-time structured discovery availability
// without exposing dynamic route key material.
func (a *App) handleDynamicProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	keyConfigured := len(a.dynamicRouteKey) == sha256.Size
	a.jsonOK(w, DynamicProfilesResponse{
		Stage:         "structured-discovery",
		Available:     keyConfigured,
		KeyConfigured: keyConfigured,
		Profiles:      dynamicProfilesCatalog(),
		GlobalLimits:  dynamicGlobalLimits(),
	})
}

func (a *App) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		a.jsonErr(w, 500, "SSE not supported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	// A comment is a valid SSE frame and makes proxy buffering observable: the
	// client receives bytes as soon as the stream is established.
	_, _ = io.WriteString(w, ": meridian-connected\n\n")
	flusher.Flush()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()

	// Send initial data immediately
	if err := a.sendSSEEvent(w, flusher); err != nil {
		log.Printf("send initial SSE event: %v", err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.sendSSEEvent(w, flusher); err != nil {
				log.Printf("send SSE event: %v", err)
				return
			}
		}
	}
}

func (a *App) sendSSEEvent(w http.ResponseWriter, flusher http.Flusher) error {
	snap, err := a.pm.TrafficSnapshot()
	if err != nil {
		return err
	}
	snap.PanelDomain = a.panelHost
	snap.PanelAccessURL = a.panelAccessURL()

	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil { // #nosec G705 -- json.Marshal escapes control characters before the SSE frame is written.
		return err
	}
	flusher.Flush()
	return nil
}
