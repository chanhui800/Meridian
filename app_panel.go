package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (a *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	snap, err := a.pm.TrafficSnapshot()
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "dashboard unavailable")
		return
	}
	snap.PanelDomain = a.panelHost
	snap.PanelAccessURL = a.panelAccessURL()
	a.jsonOK(w, snap)
}

func (a *App) panelAccessURL() string {
	if a == nil || a.panelHost == "" {
		return ""
	}
	scheme := "http"
	defaultPort := 80
	if a.panelTLSEnabled {
		scheme = "https"
		defaultPort = 443
	}
	host := a.panelHost
	if a.panelListenPort != defaultPort {
		host = net.JoinHostPort(host, strconv.Itoa(a.panelListenPort))
	}
	return scheme + "://" + host
}

// GET /api/ingress-capabilities exposes only coarse deployment state so the
// site form can avoid proposing host-only mode when the backend must reject it.
func (a *App) handleIngressCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	a.jsonOK(w, map[string]interface{}{
		"app_version":                appVersion,
		"host_only_available":        a.pm.HostOnlyIngressSafe(),
		"route_domain":               a.routeDomain,
		"domain_prefix_available":    a.routeDomain != "",
		"panel_tls_enabled":          a.panelTLSEnabled,
		"panel_bind_loopback":        a.panelBindLoopback,
		"trusted_proxy_configured":   len(a.trustedProxies) > 0,
		"upstream_headers_available": a.pm.UpstreamHeadersAvailable(),
		"max_playback_addresses":     maxPlaybackAddresses,
	})
}

func (a *App) handlePanelCertificate(w http.ResponseWriter, r *http.Request) {
	if a.panelCertificates == nil {
		a.jsonErr(w, http.StatusServiceUnavailable, "certificate management is unavailable")
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Cache-Control", "no-store")
		settings, err := a.db.PanelSettings()
		if err != nil {
			a.jsonErr(w, http.StatusInternalServerError, "failed to read panel settings")
			return
		}
		a.jsonOK(w, a.panelCertificates.status(settings, a.panelHost, a.routeDomain, a.panelListenPort, a.panelTLSEnabled))
	case http.MethodPost:
		a.handlePanelCertificateIssue(w, r)
	default:
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

type panelSettingsRequest struct {
	PanelPrefix    string `json:"panel_prefix"`
	WildcardDomain string `json:"wildcard_domain"`
	ListenPort     int    `json:"listen_port"`
}

func (a *App) handlePanelSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req panelSettingsRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	managed, err := normalizeManagedPanelPrefix(req.PanelPrefix, req.WildcardDomain)
	if err != nil {
		a.jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validatePanelListenPort(req.ListenPort); err != nil {
		a.jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	current, err := a.db.PanelSettings()
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "failed to read panel settings")
		return
	}
	settings, migrated, err := a.db.SaveManagedPanelSettings(managed.PanelDomain, managed.RouteDomain, req.ListenPort, current.TLSEnabled)
	if err != nil {
		a.jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	status := a.panelCertificates.status(settings, a.panelHost, a.routeDomain, a.panelListenPort, a.panelTLSEnabled)
	log.Printf("panel settings saved for %s and *.%s (migrated sites: %d, restart required: %t)", settings.PanelDomain, settings.RouteDomain, migrated, status.RestartRequired)
	a.jsonOK(w, status)
}

func (a *App) handlePanelCertificateIssue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req struct {
		Provider   string `json:"dns_provider"`
		Email      string `json:"email"`
		APIToken   string `json:"dns_api_token"`
		UseStaging bool   `json:"staging"`
	}
	if err := decodeJSONBody(w, r, &req); err != nil {
		a.jsonErr(w, http.StatusBadRequest, "invalid request")
		return
	}
	if strings.ToLower(strings.TrimSpace(req.Provider)) != "cloudflare" {
		a.jsonErr(w, http.StatusBadRequest, "only Cloudflare DNS is currently supported")
		return
	}
	settings, err := a.db.PanelSettings()
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "failed to read panel settings")
		return
	}
	if !settings.Configured || settings.PanelDomain == "" || settings.RouteDomain == "" || settings.ListenPort == 0 {
		a.jsonErr(w, http.StatusConflict, "请先保存面板前缀、泛域名和监听端口")
		return
	}
	currentStatus := a.panelCertificates.status(settings, a.panelHost, a.routeDomain, a.panelListenPort, a.panelTLSEnabled)
	if currentStatus.CertificateCurrent {
		if !settings.TLSEnabled {
			var err error
			settings, _, err = a.db.SaveManagedPanelSettings(settings.PanelDomain, settings.RouteDomain, settings.ListenPort, true)
			if err != nil {
				a.jsonErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			currentStatus = a.panelCertificates.status(settings, a.panelHost, a.routeDomain, a.panelListenPort, a.panelTLSEnabled)
		}
		currentStatus.CertificateReused = true
		a.jsonOK(w, currentStatus)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	issued, err := a.panelCertificates.issueCloudflare(ctx, req.Email, req.APIToken, settings.PanelDomain, settings.RouteDomain, req.UseStaging)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errCertificateIssuanceBusy) {
			status = http.StatusConflict
		} else if validateErr := validatePanelCertificateRequest(req.Email, req.APIToken); validateErr != nil {
			status = http.StatusBadRequest
		}
		log.Printf("panel certificate request failed for %s: %v", settings.RouteDomain, err)
		a.jsonErr(w, status, err.Error())
		return
	}
	backup, err := a.panelCertificates.backupInstalledFiles()
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := a.panelCertificates.install(issued, false); err != nil {
		_ = a.panelCertificates.restoreInstalledFiles(backup)
		a.jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	settings, migrated, err := a.db.SaveManagedPanelSettings(settings.PanelDomain, settings.RouteDomain, settings.ListenPort, true)
	if err != nil {
		_ = a.panelCertificates.restoreInstalledFiles(backup)
		a.jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	status := a.panelCertificates.status(settings, a.panelHost, a.routeDomain, a.panelListenPort, a.panelTLSEnabled)
	if !status.RestartRequired {
		a.panelCertificates.activate(issued)
	}
	log.Printf("panel wildcard certificate installed for *.%s (migrated sites: %d, restart required: %t)", settings.RouteDomain, migrated, status.RestartRequired)
	a.jsonOK(w, status)
}

func normalizePanelCertificateDomains(panelPrefix, wildcardDomain, legacyPanelDomain, legacyRouteDomain string) (PanelSettings, error) {
	if strings.TrimSpace(panelPrefix) != "" || strings.TrimSpace(wildcardDomain) != "" {
		return normalizeManagedPanelPrefix(panelPrefix, wildcardDomain)
	}
	return normalizeManagedPanelSettings(legacyPanelDomain, legacyRouteDomain)
}

func (a *App) handleSystemRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	settings, err := a.db.PanelSettings()
	if err != nil {
		a.jsonErr(w, http.StatusInternalServerError, "failed to read panel settings")
		return
	}
	status := a.panelCertificates.status(settings, a.panelHost, a.routeDomain, a.panelListenPort, a.panelTLSEnabled)
	if settings.PanelDomain == "" || settings.ListenPort == 0 {
		a.jsonErr(w, http.StatusConflict, "请先保存面板域名和监听端口")
		return
	}
	if settings.TLSEnabled && (!status.Configured || !status.CertificateCurrent) {
		a.jsonErr(w, http.StatusConflict, "泛域名已改变，请先申请匹配的 TLS 证书")
		return
	}
	host := settings.PanelDomain
	if settings.ListenPort != 443 {
		host = net.JoinHostPort(host, strconv.Itoa(settings.ListenPort))
	}
	scheme := "http"
	if settings.TLSEnabled {
		scheme = "https"
	}
	a.jsonOK(w, map[string]interface{}{
		"restarting":   true,
		"redirect_url": scheme + "://" + host,
	})
	if a.restartCh == nil {
		return
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	a.restartOnce.Do(func() {
		time.AfterFunc(500*time.Millisecond, func() { close(a.restartCh) })
	})
}
