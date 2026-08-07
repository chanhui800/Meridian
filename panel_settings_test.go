package main

import (
	"testing"
)

func TestNormalizeManagedPanelSettings(t *testing.T) {
	settings, err := normalizeManagedPanelSettings("Panel.Example.com.", "Example.com.")
	if err != nil {
		t.Fatalf("normalize managed settings: %v", err)
	}
	if settings.PanelDomain != "panel.example.com" || settings.RouteDomain != "example.com" || !settings.TLSEnabled {
		t.Fatalf("unexpected settings: %+v", settings)
	}
	for _, tc := range [][2]string{
		{"example.com", "example.com"},
		{"a.b.example.com", "example.com"},
		{"panel.example.com", ""},
	} {
		if _, err := normalizeManagedPanelSettings(tc[0], tc[1]); err == nil {
			t.Fatalf("normalizeManagedPanelSettings(%q, %q) unexpectedly succeeded", tc[0], tc[1])
		}
	}
}

func TestNormalizeManagedPanelPrefixAcceptsWildcardForm(t *testing.T) {
	settings, err := normalizeManagedPanelPrefix("panel", "*.example.com")
	if err != nil {
		t.Fatalf("normalize panel prefix: %v", err)
	}
	if settings.PanelDomain != "panel.example.com" || settings.RouteDomain != "example.com" {
		t.Fatalf("unexpected normalized wildcard settings: %+v", settings)
	}
	if got := wildcardDomainForSettings(settings); got != "*.example.com" {
		t.Fatalf("wildcard domain = %q", got)
	}
}

func TestBootstrapPanelSettingsImportsEnvironmentOnlyOnce(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()
	first, err := db.BootstrapPanelSettings("panel.example.com", "example.com", false)
	if err != nil {
		t.Fatalf("bootstrap settings: %v", err)
	}
	second, err := db.BootstrapPanelSettings("other.example.net", "example.net", true)
	if err != nil {
		t.Fatalf("second bootstrap settings: %v", err)
	}
	if first != second || second.PanelDomain != "panel.example.com" || second.RouteDomain != "example.com" || second.TLSEnabled {
		t.Fatalf("environment settings were not imported once: first=%+v second=%+v", first, second)
	}
}

func TestSaveManagedPanelSettingsMigratesPrefixHosts(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()
	if _, err := db.db.Exec(`INSERT INTO sites (name, listen_port, public_host, target_url) VALUES (?, ?, ?, ?)`, "One", 19001, "one.old.example.com", "http://127.0.0.1:8096"); err != nil {
		t.Fatalf("insert site: %v", err)
	}
	if _, err := db.BootstrapPanelSettings("panel.old.example.com", "old.example.com", false); err != nil {
		t.Fatalf("bootstrap old settings: %v", err)
	}
	settings, migrated, err := db.SaveManagedPanelSettings("panel.example.com", "example.com")
	if err != nil {
		t.Fatalf("save managed settings: %v", err)
	}
	if migrated != 1 || settings.PanelDomain != "panel.example.com" {
		t.Fatalf("unexpected migration result: settings=%+v migrated=%d", settings, migrated)
	}
	var host string
	if err := db.db.QueryRow("SELECT public_host FROM sites WHERE name=?", "One").Scan(&host); err != nil {
		t.Fatalf("read migrated site: %v", err)
	}
	if host != "one.example.com" {
		t.Fatalf("migrated site host = %q, want one.example.com", host)
	}
}
