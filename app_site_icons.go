package main

import (
	"encoding/json"
	"net/http"
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
