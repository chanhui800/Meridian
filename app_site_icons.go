package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

func (a *App) handleSiteIconPack(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		pack, err := a.db.SiteIconPack()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "site icon pack unavailable")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		a.jsonOK(w, pack)
	case http.MethodPost:
		var req struct {
			Pack      json.RawMessage `json:"pack"`
			SourceURL string          `json:"source_url"`
		}
		if err := decodeJSONBodyWithLimit(w, r, &req, maxSiteIconPackBytes+64<<10); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid icon pack request")
			return
		}
		pack, err := parseSiteIconPack(req.Pack)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if source := strings.TrimSpace(req.SourceURL); source != "" {
			// The browser fetches the URL; this field is metadata only. Do not
			// turn the controller into a server-side URL fetcher/SSRF primitive.
			if _, err := normalizeSiteIconURL(source); err != nil {
				a.jsonErr(w, http.StatusBadRequest, "source_url must be an HTTPS URL")
				return
			}
			pack.SourceURL = source
		}
		if err := a.db.SaveSiteIconPack(pack); err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "save icon pack failed")
			return
		}
		pack.UpdatedAt = iconPackUpdatedAt()
		pack.BuiltIn = isBuiltinSiteIconPack(pack.SourceURL)
		a.jsonResponse(w, http.StatusCreated, pack)
	case http.MethodDelete:
		if err := a.db.ClearSiteIconPack(); err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "clear icon pack failed")
			return
		}
		a.jsonOK(w, SiteIconPack{Icons: []SiteIcon{}})
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *App) handleSiteIconPacks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		packs, err := a.db.SiteIconPacks()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "site icon packs unavailable")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		a.jsonOK(w, map[string]any{"packs": packs})
	case http.MethodPost:
		var req struct {
			Pack      json.RawMessage `json:"pack"`
			SourceURL string          `json:"source_url"`
		}
		if err := decodeJSONBodyWithLimit(w, r, &req, maxSiteIconPackBytes+64<<10); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid icon pack request")
			return
		}
		pack, err := parseSiteIconPack(req.Pack)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if source := strings.TrimSpace(req.SourceURL); source != "" {
			if _, err := normalizeSiteIconURL(source); err != nil {
				a.jsonErr(w, http.StatusBadRequest, "source_url must be an HTTPS URL")
				return
			}
			pack.SourceURL = source
		}
		created, err := a.db.CreateSiteIconPack(pack)
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "create icon pack failed")
			return
		}
		a.jsonResponse(w, http.StatusCreated, created)
	default:
		w.Header().Set("Allow", "GET, POST")
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (a *App) handleSiteIconPackItem(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/site-icon-packs/")
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		w.Header().Set("Allow", "DELETE")
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		a.jsonErr(w, http.StatusBadRequest, "invalid icon pack id")
		return
	}
	if len(parts) == 2 && parts[1] == "activate" && r.Method == http.MethodPost {
		if err := a.db.SetActiveSiteIconPack(id); err != nil {
			if err == errSiteIconPackNotFound {
				a.jsonErr(w, http.StatusNotFound, "site icon pack not found")
				return
			}
			a.jsonErr(w, http.StatusInternalServerError, "activate icon pack failed")
			return
		}
		a.jsonOK(w, map[string]any{"id": id, "active": true})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if err := a.db.DeleteSiteIconPack(id); err != nil {
			if err == errSiteIconPackNotFound {
				a.jsonErr(w, http.StatusNotFound, "site icon pack not found")
				return
			}
			a.jsonErr(w, http.StatusInternalServerError, "delete icon pack failed")
			return
		}
		a.jsonOK(w, map[string]any{"id": id, "deleted": true})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodPut {
		stored, err := a.db.SiteIconPackByID(id)
		if err != nil {
			if errors.Is(err, errSiteIconPackNotFound) {
				a.jsonErr(w, http.StatusNotFound, "site icon pack not found")
				return
			}
			a.jsonErr(w, http.StatusInternalServerError, "site icon pack unavailable")
			return
		}
		if stored.SourceURL == "" {
			a.jsonErr(w, http.StatusBadRequest, "图标包没有来源链接，无法刷新")
			return
		}
		var req struct {
			Pack json.RawMessage `json:"pack"`
		}
		if err := decodeJSONBodyWithLimit(w, r, &req, maxSiteIconPackBytes+64<<10); err != nil {
			a.jsonErr(w, http.StatusBadRequest, "invalid icon pack request")
			return
		}
		pack, err := parseSiteIconPack(req.Pack)
		if err != nil {
			a.jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		updated, err := a.db.UpdateSiteIconPack(id, pack)
		if err != nil {
			switch {
			case errors.Is(err, errSiteIconPackNotFound):
				a.jsonErr(w, http.StatusNotFound, "site icon pack not found")
			case errors.Is(err, errInvalidSiteIconPack), errors.Is(err, errSiteIconPackSourceMissing):
				a.jsonErr(w, http.StatusBadRequest, err.Error())
			default:
				a.jsonErr(w, http.StatusInternalServerError, "refresh icon pack failed")
			}
			return
		}
		a.jsonOK(w, updated)
		return
	}
	w.Header().Set("Allow", "DELETE, PUT, POST")
	a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
}
