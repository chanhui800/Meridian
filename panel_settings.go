package main

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/net/publicsuffix"
)

const panelSettingsRowID = 1

type PanelSettings struct {
	PanelDomain         string `json:"panel_domain"`
	RouteDomain         string `json:"route_domain"`
	ListenPort          int    `json:"listen_port"`
	TLSEnabled          bool   `json:"tls_enabled"`
	Configured          bool   `json:"-"`
	ACMEEmail           string `json:"-"`
	ACMEDNSProvider     string `json:"-"`
	ACMETokenCiphertext string `json:"-"`
	ACMEStaging         bool   `json:"-"`
	TLSDisabledReason   string `json:"-"`
}

const (
	panelTLSDisabledReasonManual          = "manual"
	panelTLSDisabledReasonExpiredFallback = "expired_fallback"
)

// normalizeWildcardDomain accepts the UI form (*.example.com) and stores the
// DNS suffix without the wildcard marker so routing code can continue to build
// concrete hosts such as movie.example.com.
func normalizeWildcardDomain(value string) (string, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "*.")
	return normalizeRouteDomain(value)
}

func normalizeManagedPanelPrefix(panelPrefix, wildcardDomain string) (PanelSettings, error) {
	if !strings.HasPrefix(strings.TrimSpace(wildcardDomain), "*.") {
		return PanelSettings{}, errors.New("节点泛域名必须以 *. 开头，例如 *.example.com")
	}
	routeDomain, err := normalizeWildcardDomain(wildcardDomain)
	if err != nil {
		return PanelSettings{}, fmt.Errorf("节点泛域名无效: %w", err)
	}
	prefix, err := normalizeRoutePrefix(panelPrefix)
	if err != nil {
		return PanelSettings{}, fmt.Errorf("面板访问域名前缀无效: %w", err)
	}
	return normalizeManagedPanelSettings(prefix+"."+routeDomain, routeDomain)
}

func panelPrefixForSettings(settings PanelSettings) string {
	if prefix, ok := routePrefixFromConfiguredHost(settings.PanelDomain, settings.RouteDomain); ok {
		return prefix
	}
	return ""
}

func wildcardDomainForSettings(settings PanelSettings) string {
	if settings.RouteDomain == "" {
		return ""
	}
	return "*." + settings.RouteDomain
}

func scanPanelSettings(scanner interface{ Scan(...any) error }) (PanelSettings, error) {
	var settings PanelSettings
	var tlsEnabled, configured, acmeStaging int
	err := scanner.Scan(
		&settings.PanelDomain, &settings.RouteDomain, &settings.ListenPort, &tlsEnabled, &configured,
		&settings.ACMEEmail, &settings.ACMEDNSProvider, &settings.ACMETokenCiphertext, &acmeStaging, &settings.TLSDisabledReason,
	)
	settings.TLSEnabled = tlsEnabled != 0
	settings.Configured = configured != 0
	settings.ACMEStaging = acmeStaging != 0
	return settings, err
}

func (d *DB) PanelSettings() (PanelSettings, error) {
	if d == nil || d.db == nil {
		return PanelSettings{}, errors.New("panel settings database is unavailable")
	}
	return scanPanelSettings(d.db.QueryRow(`
		SELECT panel_domain, route_domain, listen_port, tls_enabled, configured,
			acme_email, acme_dns_provider, acme_token_ciphertext, acme_staging, tls_disabled_reason
		FROM panel_settings WHERE id=?`, panelSettingsRowID))
}

func normalizeStoredPanelSettings(panelDomain, routeDomain string, tlsEnabled bool) (PanelSettings, error) {
	panelDomain, err := normalizePublicHost(panelDomain)
	if err != nil {
		return PanelSettings{}, fmt.Errorf("invalid panel domain: %w", err)
	}
	routeDomain, err = normalizeRouteDomain(routeDomain)
	if err != nil {
		return PanelSettings{}, fmt.Errorf("invalid route domain: %w", err)
	}
	return PanelSettings{
		PanelDomain: panelDomain,
		RouteDomain: routeDomain,
		TLSEnabled:  tlsEnabled,
		Configured:  panelDomain != "" || routeDomain != "" || tlsEnabled,
	}, nil
}

func normalizeManagedPanelSettings(panelDomain, routeDomain string) (PanelSettings, error) {
	settings, err := normalizeStoredPanelSettings(panelDomain, routeDomain, true)
	if err != nil {
		return PanelSettings{}, err
	}
	if settings.PanelDomain == "" || settings.RouteDomain == "" {
		return PanelSettings{}, errors.New("面板访问域名和节点基础域名不能为空")
	}
	if strings.EqualFold(settings.PanelDomain, settings.RouteDomain) {
		return PanelSettings{}, errors.New("面板访问域名和节点基础域名必须不同")
	}
	panelRoot, err := publicsuffix.EffectiveTLDPlusOne(settings.PanelDomain)
	if err != nil {
		return PanelSettings{}, fmt.Errorf("面板访问域名注册域无效: %w", err)
	}
	routeRoot, err := publicsuffix.EffectiveTLDPlusOne(settings.RouteDomain)
	if err != nil {
		return PanelSettings{}, fmt.Errorf("节点基础域名注册域无效: %w", err)
	}
	if !strings.EqualFold(panelRoot, routeRoot) {
		return PanelSettings{}, errors.New("面板访问域名和节点基础域名必须属于同一注册域")
	}
	settings.Configured = true
	settings.TLSEnabled = true
	return settings, nil
}

func validatePanelListenPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("监听端口必须在 1 到 65535 之间")
	}
	return nil
}

func (d *DB) BootstrapPanelSettings(panelDomain, routeDomain string, tlsEnabled bool, listenPort int) (PanelSettings, error) {
	if err := validatePanelListenPort(listenPort); err != nil {
		return PanelSettings{}, err
	}
	current, err := d.PanelSettings()
	if err != nil {
		return PanelSettings{}, err
	}
	if current.Configured {
		if current.ListenPort == 0 {
			if _, err := d.db.Exec("UPDATE panel_settings SET listen_port=?, updated_at=CURRENT_TIMESTAMP WHERE id=?", listenPort, panelSettingsRowID); err != nil {
				return PanelSettings{}, err
			}
			current.ListenPort = listenPort
		}
		return current, nil
	}
	candidate, err := normalizeStoredPanelSettings(panelDomain, routeDomain, tlsEnabled)
	if err != nil {
		return PanelSettings{}, err
	}
	if candidate.Configured {
		candidate, err = normalizeManagedPanelSettings(panelDomain, routeDomain)
		if err != nil {
			return PanelSettings{}, err
		}
		candidate.TLSEnabled = tlsEnabled
	}
	candidate.ListenPort = listenPort
	if !candidate.Configured {
		if current.ListenPort == 0 {
			if _, err := d.db.Exec("UPDATE panel_settings SET listen_port=?, updated_at=CURRENT_TIMESTAMP WHERE id=? AND configured=0", listenPort, panelSettingsRowID); err != nil {
				return PanelSettings{}, err
			}
			current.ListenPort = listenPort
		}
		return current, nil
	}
	_, err = d.db.Exec(`
		UPDATE panel_settings
		SET panel_domain=?, route_domain=?, listen_port=?, tls_enabled=?, configured=1, updated_at=CURRENT_TIMESTAMP
		WHERE id=? AND configured=0`,
		candidate.PanelDomain, candidate.RouteDomain, candidate.ListenPort, sqliteBool(candidate.TLSEnabled), panelSettingsRowID)
	if err != nil {
		return PanelSettings{}, err
	}
	return d.PanelSettings()
}

func routePrefixFromConfiguredHost(host, routeDomain string) (string, bool) {
	if host == "" || routeDomain == "" {
		return "", false
	}
	suffix := "." + routeDomain
	if !strings.HasSuffix(host, suffix) {
		return "", false
	}
	prefix := strings.TrimSuffix(host, suffix)
	if prefix == "" || strings.Contains(prefix, ".") {
		return "", false
	}
	normalized, err := normalizeRoutePrefix(prefix)
	return normalized, err == nil && normalized == prefix
}

type panelHostMigration struct {
	SiteID int64
	Host   string
}

func (d *DB) SaveManagedPanelSettings(panelDomain, routeDomain string, listenPort int, tlsEnabled bool) (PanelSettings, int, error) {
	if err := validatePanelListenPort(listenPort); err != nil {
		return PanelSettings{}, 0, err
	}
	candidate, err := normalizeManagedPanelSettings(panelDomain, routeDomain)
	if err != nil {
		return PanelSettings{}, 0, err
	}
	candidate.ListenPort = listenPort
	candidate.TLSEnabled = tlsEnabled
	tx, err := d.db.Begin()
	if err != nil {
		return PanelSettings{}, 0, err
	}
	defer tx.Rollback()

	current, err := scanPanelSettings(tx.QueryRow(`
		SELECT panel_domain, route_domain, listen_port, tls_enabled, configured,
			acme_email, acme_dns_provider, acme_token_ciphertext, acme_staging, tls_disabled_reason
		FROM panel_settings WHERE id=?`, panelSettingsRowID))
	if err != nil {
		return PanelSettings{}, 0, err
	}
	if current.RouteDomain != "" && current.RouteDomain != candidate.RouteDomain {
		var managedRecords int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM site_node_schedules WHERE TRIM(cf_record_id) <> ''`).Scan(&managedRecords); err != nil {
			return PanelSettings{}, 0, err
		}
		if managedRecords > 0 {
			return PanelSettings{}, 0, errors.New("请先停用节点 DNS 调度并清理已托管 DNS 记录，再修改节点基础域名")
		}
	}
	rows, err := tx.Query("SELECT id, public_host FROM sites WHERE public_host <> '' ORDER BY id")
	if err != nil {
		return PanelSettings{}, 0, err
	}
	var migrations []panelHostMigration
	assigned := make(map[string]int64)
	for rows.Next() {
		var siteID int64
		var host string
		if err := rows.Scan(&siteID, &host); err != nil {
			rows.Close()
			return PanelSettings{}, 0, err
		}
		host, err = normalizePublicHost(host)
		if err != nil {
			rows.Close()
			return PanelSettings{}, 0, fmt.Errorf("站点 %d 的访问域名无效: %w", siteID, err)
		}
		newHost := host
		if current.RouteDomain != candidate.RouteDomain {
			if prefix, ok := routePrefixFromConfiguredHost(host, current.RouteDomain); ok {
				newHost, err = routeHostForPrefix(prefix, candidate.RouteDomain)
				if err != nil {
					rows.Close()
					return PanelSettings{}, 0, err
				}
			}
		}
		if newHost == candidate.PanelDomain {
			rows.Close()
			return PanelSettings{}, 0, fmt.Errorf("面板访问域名与站点 %d 冲突", siteID)
		}
		key := strings.ToLower(newHost)
		if otherID, exists := assigned[key]; exists && otherID != siteID {
			rows.Close()
			return PanelSettings{}, 0, fmt.Errorf("修改节点基础域名后站点 %d 与站点 %d 的访问域名冲突", otherID, siteID)
		}
		assigned[key] = siteID
		if newHost != host {
			migrations = append(migrations, panelHostMigration{SiteID: siteID, Host: newHost})
		}
	}
	if err := rows.Close(); err != nil {
		return PanelSettings{}, 0, err
	}
	if err := rows.Err(); err != nil {
		return PanelSettings{}, 0, err
	}
	for _, migration := range migrations {
		if _, err := tx.Exec("UPDATE sites SET public_host=?, updated_at=CURRENT_TIMESTAMP WHERE id=?", migration.Host, migration.SiteID); err != nil {
			return PanelSettings{}, 0, err
		}
	}
	reason := ""
	if !candidate.TLSEnabled {
		reason = panelTLSDisabledReasonManual
	}
	if _, err := tx.Exec(`
		UPDATE panel_settings
		SET panel_domain=?, route_domain=?, listen_port=?, tls_enabled=?, tls_disabled_reason=?, configured=1, updated_at=CURRENT_TIMESTAMP
		WHERE id=?`, candidate.PanelDomain, candidate.RouteDomain, candidate.ListenPort, sqliteBool(candidate.TLSEnabled), reason, panelSettingsRowID); err != nil {
		return PanelSettings{}, 0, err
	}
	if err := tx.Commit(); err != nil {
		return PanelSettings{}, 0, err
	}
	persisted, err := d.PanelSettings()
	if err != nil {
		return PanelSettings{}, 0, err
	}
	return persisted, len(migrations), nil
}

func (d *DB) SavePanelACMECredentials(email, provider, tokenCiphertext string, staging bool) error {
	if d == nil || d.db == nil {
		return errors.New("panel settings database is unavailable")
	}
	_, err := d.db.Exec(`
		UPDATE panel_settings
		SET acme_email=?, acme_dns_provider=?, acme_token_ciphertext=?, acme_staging=?, updated_at=CURRENT_TIMESTAMP
		WHERE id=?`, email, provider, tokenCiphertext, sqliteBool(staging), panelSettingsRowID)
	return err
}

// SetPanelTLSEnabled changes only the active panel transport. Certificate and
// ACME files remain intact so HTTPS can be re-enabled or renewed later.
func (d *DB) SetPanelTLSEnabled(enabled bool) error {
	if d == nil || d.db == nil {
		return errors.New("panel settings database is unavailable")
	}
	reason := ""
	if !enabled {
		reason = panelTLSDisabledReasonManual
	}
	_, err := d.db.Exec(`
		UPDATE panel_settings
		SET tls_enabled=?, tls_disabled_reason=?, updated_at=CURRENT_TIMESTAMP
		WHERE id=?`, sqliteBool(enabled), reason, panelSettingsRowID)
	return err
}

func (d *DB) setPanelTLSExpiredFallback() error {
	if d == nil || d.db == nil {
		return errors.New("panel settings database is unavailable")
	}
	_, err := d.db.Exec(`UPDATE panel_settings SET tls_enabled=0, tls_disabled_reason=?, updated_at=CURRENT_TIMESTAMP WHERE id=?`, panelTLSDisabledReasonExpiredFallback, panelSettingsRowID)
	return err
}
