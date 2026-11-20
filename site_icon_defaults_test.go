package main

import (
	"path/filepath"
	"testing"
)

func TestBuiltinSiteIconPacksAvailableOnFreshInstall(t *testing.T) {
	db, err := openDB(filepath.Join(t.TempDir(), "defaults.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	packs, err := db.SiteIconPacks()
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 3 {
		t.Fatalf("fresh installation has %d packs, want 3", len(packs))
	}
	activeCount := 0
	for index, pack := range packs {
		if pack.SourceURL != builtinSiteIconPackSources[index].SourceURL || !pack.BuiltIn || pack.IconCount == 0 {
			t.Fatalf("invalid builtin summary: %+v", pack)
		}
		loaded, err := db.SiteIconPackByID(pack.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !loaded.BuiltIn || len(loaded.Icons) != pack.IconCount || loaded.Name != pack.Name {
			t.Fatalf("builtin pack does not match its summary: %+v", pack)
		}
		if pack.Active {
			activeCount++
		}
		t.Logf("builtin %q has %d icons", pack.Name, pack.IconCount)
	}
	active, err := db.SiteIconPack()
	if err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 || active.SourceURL != builtinSiteIconPackSources[0].SourceURL {
		t.Fatalf("unexpected default selection: %+v", active)
	}
}

func TestBuiltinSiteIconPacksUpgradePreservesExistingContentAndSelection(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := openDB(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.ClearSiteIconPack(); err != nil {
		db.Close()
		t.Fatal(err)
	}
	existing, err := db.CreateSiteIconPack(SiteIconPack{
		Name: "existing package", Description: "operator metadata", SourceURL: builtinSiteIconPackSources[0].SourceURL,
		Icons: []SiteIcon{{Name: "Emby", URL: "https://cdn.example.test/original.png"}},
	})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	custom, err := db.CreateSiteIconPack(SiteIconPack{
		Name: "custom package", SourceURL: "https://cdn.example.test/custom.json",
		Icons: []SiteIcon{{Name: "Custom", URL: "https://cdn.example.test/custom.png"}},
	})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	site, err := db.CreateSiteRecord(Site{
		Name: "existing icon", IconName: "Emby", IconURL: "https://cdn.example.test/original.png",
		ListenPort: 18101, IngressMode: ingressModePort, TargetURL: "http://127.0.0.1:8096",
		PlaybackMode: "direct", StreamHosts: "[]", UAMode: passthroughUAMode,
	})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.db.Exec("PRAGMA user_version=52"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()

	upgraded, err := openDB(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	packs, err := upgraded.SiteIconPacks()
	if err != nil {
		t.Fatal(err)
	}
	if len(packs) != 4 {
		t.Fatalf("upgrade has %d packs, want existing two plus two missing defaults", len(packs))
	}
	preserved, err := upgraded.SiteIconPackByID(existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	if preserved.Name != existing.Name || preserved.Description != existing.Description || len(preserved.Icons) != 1 || !preserved.Active || preserved.Icons[0] != existing.Icons[0] || !preserved.BuiltIn {
		t.Fatalf("upgrade overwrote existing package: %+v", preserved)
	}
	customLoaded, err := upgraded.SiteIconPackByID(custom.ID)
	if err != nil || customLoaded.BuiltIn || customLoaded.Active {
		t.Fatalf("custom package changed: %+v, %v", customLoaded, err)
	}
	updatedSite, err := upgraded.GetSite(site.ID)
	if err != nil || updatedSite.IconName != site.IconName || updatedSite.IconURL != site.IconURL {
		t.Fatalf("upgrade changed the persisted site icon: %+v, %v", updatedSite, err)
	}
	var removedID int64
	for _, pack := range packs {
		if pack.SourceURL == builtinSiteIconPackSources[1].SourceURL {
			removedID = pack.ID
		}
	}
	if removedID == 0 {
		t.Fatal("missing Sakura builtin")
	}
	if err := upgraded.DeleteSiteIconPack(removedID); err != nil {
		t.Fatal(err)
	}
	upgraded.Close()

	reopened, err := openDB(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	packs, err = reopened.SiteIconPacks()
	if err != nil || len(packs) != 3 {
		t.Fatalf("reopening duplicated or restored deleted packages: count=%d, %v", len(packs), err)
	}
	for _, pack := range packs {
		if pack.SourceURL == builtinSiteIconPackSources[1].SourceURL {
			t.Fatal("deleted builtin package reappeared on restart")
		}
	}
}
