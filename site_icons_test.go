package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseSiteIconPackAcceptsReferenceShapeAndDeduplicatesNames(t *testing.T) {
	pack, err := parseSiteIconPack([]byte(`{
		"name":"Example icons",
		"description":"test",
		"icons":[
			{"name":"Emby","url":"https://cdn.example.test/emby.png"},
			{"name":"emby","url":"https://cdn.example.test/other.png"},
			{"name":" Jellyfin ","url":"https://cdn.example.test/jellyfin.png"}
		]
	}`))
	if err != nil {
		t.Fatalf("parse icon pack: %v", err)
	}
	if len(pack.Icons) != 2 || pack.Icons[0].Name != "Emby" || pack.Icons[1].Name != "Jellyfin" {
		t.Fatalf("unexpected icons: %+v", pack.Icons)
	}
}

func TestParseSiteIconPackRejectsUnsafeURLsAndUnknownFields(t *testing.T) {
	for _, raw := range []string{
		`{"icons":[{"name":"bad","url":"javascript:alert(1)"}]}`,
		`{"icons":[{"name":"bad","url":"http://cdn.example.test/a.png"}]}`,
		`{"icons":[{"name":"bad","url":"https://cdn.example.test/a.png","html":"<svg>"}]}`,
	} {
		if _, err := parseSiteIconPack([]byte(raw)); err == nil {
			t.Fatalf("expected invalid icon pack: %s", raw)
		}
	}
}

func TestSiteIconPackAndSiteSelectionPersist(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pack := SiteIconPack{Name: "test", Icons: []SiteIcon{{Name: "Emby", URL: "https://cdn.example.test/emby.png"}}}
	if err := db.SaveSiteIconPack(pack); err != nil {
		t.Fatalf("save icon pack: %v", err)
	}
	loaded, err := db.SiteIconPack()
	if err != nil || len(loaded.Icons) != 1 {
		t.Fatalf("load icon pack: %+v, %v", loaded, err)
	}
	site, err := db.CreateSiteRecord(Site{Name: "icon site", IconName: "Emby", IconURL: pack.Icons[0].URL, ListenPort: 18096, IngressMode: ingressModePort, TargetURL: "http://127.0.0.1:8096", PlaybackMode: "direct", StreamHosts: "[]", UAMode: passthroughUAMode})
	if err != nil {
		t.Fatalf("create site: %v", err)
	}
	if site.IconName != "Emby" || site.IconURL != pack.Icons[0].URL {
		t.Fatalf("site icon not returned: %+v", site)
	}
	encoded, _ := json.Marshal(site)
	if string(encoded) == "" {
		t.Fatal("site JSON was empty")
	}
}

func TestHandleSiteIconPackAcceptsUserUploadAndClearsIt(t *testing.T) {
	app := newTestApp(t)
	payload := []byte(`{"pack":{"name":"Uploaded","description":"user supplied","icons":[{"name":"Emby","url":"https://cdn.example.test/emby.png"}]},"source_url":""}`)
	created := httptest.NewRecorder()
	app.handleSiteIconPack(created, httptest.NewRequest(http.MethodPost, "/api/site-icon-pack", bytes.NewReader(payload)))
	if created.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", created.Code, created.Body.String())
	}
	loaded := httptest.NewRecorder()
	app.handleSiteIconPack(loaded, httptest.NewRequest(http.MethodGet, "/api/site-icon-pack", nil))
	if loaded.Code != http.StatusOK || !bytes.Contains(loaded.Body.Bytes(), []byte(`"Emby"`)) {
		t.Fatalf("loaded pack status=%d body=%s", loaded.Code, loaded.Body.String())
	}
	cleared := httptest.NewRecorder()
	app.handleSiteIconPack(cleared, httptest.NewRequest(http.MethodDelete, "/api/site-icon-pack", nil))
	if cleared.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	pack, err := app.db.SiteIconPack()
	if err != nil {
		t.Fatal(err)
	}
	if len(pack.Icons) != 0 {
		t.Fatalf("cleared pack still has icons: %+v", pack.Icons)
	}
}

func TestUpdateSiteIconOnlyChangesIconMetadata(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SaveSiteIconPack(SiteIconPack{Icons: []SiteIcon{{Name: "Emby", URL: "https://cdn.example.test/emby.png"}}}); err != nil {
		t.Fatal(err)
	}
	site, err := db.CreateSiteRecord(Site{
		Name: "stable site", ListenPort: 18096, IngressMode: ingressModePort,
		TargetURL: "http://127.0.0.1:8096", PlaybackMode: "direct", StreamHosts: "[]", UAMode: passthroughUAMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := db.UpdateSiteIcon(site.ID, "Emby", "https://cdn.example.test/emby.png")
	if err != nil {
		t.Fatal(err)
	}
	if updated.IconName != "Emby" || updated.IconURL != "https://cdn.example.test/emby.png" || updated.TargetURL != site.TargetURL || updated.Name != site.Name {
		t.Fatalf("icon update changed unrelated site fields: before=%+v after=%+v", site, updated)
	}
	if _, err := db.UpdateSiteIcon(site.ID, "", "https://cdn.example.test/emby.png"); err == nil {
		t.Fatal("expected incomplete icon selection to be rejected")
	}
	if _, err := db.UpdateSiteIcon(site.ID, "Custom", "https://cdn.example.test/custom.png"); err == nil {
		t.Fatal("unknown icon selection unexpectedly accepted")
	}
}

func TestSiteEditPreservesIconAfterPackReplacement(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	oldURL := "https://cdn.example.test/v1/emby.png"
	newURL := "https://cdn.example.test/v2/emby.png"
	if err := db.SaveSiteIconPack(SiteIconPack{Icons: []SiteIcon{{Name: "Emby", URL: oldURL}}}); err != nil {
		t.Fatal(err)
	}
	site, err := db.CreateSiteRecord(Site{Name: "before", IconName: "Emby", IconURL: oldURL, ListenPort: 18097, IngressMode: ingressModePort, TargetURL: "http://127.0.0.1:8096", PlaybackMode: "direct", StreamHosts: "[]", UAMode: passthroughUAMode})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SaveSiteIconPack(SiteIconPack{Icons: []SiteIcon{{Name: "Emby", URL: newURL}}}); err != nil {
		t.Fatal(err)
	}
	site.Name = "after"
	if err := db.UpdateSiteRecord(*site); err != nil {
		t.Fatalf("editing site after icon pack replacement: %v", err)
	}
	updated, err := db.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Name != "after" || updated.IconName != "Emby" || updated.IconURL != oldURL {
		t.Fatalf("site edit changed persisted icon: %#v", updated)
	}
	if _, err := db.UpdateSiteIcon(site.ID, "Emby", newURL); err != nil {
		t.Fatalf("selecting replacement icon: %v", err)
	}
	updated, err = db.GetSite(site.ID)
	if err != nil || updated.IconURL != newURL {
		t.Fatalf("replacement icon not applied: %#v err=%v", updated, err)
	}
}
