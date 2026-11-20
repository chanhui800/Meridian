package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
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

func TestMultipleSiteIconPacksListSwitchAndDelete(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ClearSiteIconPack(); err != nil {
		t.Fatal(err)
	}

	first, err := db.CreateSiteIconPack(SiteIconPack{
		Name:  "first",
		Icons: []SiteIcon{{Name: "Emby", URL: "https://cdn.example.test/emby.png"}},
	})
	if err != nil {
		t.Fatalf("create first pack: %v", err)
	}
	if !first.Active || first.ID <= 0 {
		t.Fatalf("first pack should be active with an id: %+v", first)
	}

	second, err := db.CreateSiteIconPack(SiteIconPack{
		Name: "second",
		Icons: []SiteIcon{
			{Name: "Jellyfin", URL: "https://cdn.example.test/jellyfin.png"},
			{Name: "Plex", URL: "https://cdn.example.test/plex.png"},
		},
	})
	if err != nil {
		t.Fatalf("create second pack: %v", err)
	}
	if second.Active {
		t.Fatalf("second pack should not be active while the first is active: %+v", second)
	}

	packs, err := db.SiteIconPacks()
	if err != nil || len(packs) != 2 {
		t.Fatalf("list packs: %+v, %v", packs, err)
	}
	if packs[0].IconCount != 1 || !packs[0].Active || packs[1].IconCount != 2 || packs[1].Active {
		t.Fatalf("unexpected pack summaries: %+v", packs)
	}

	if err := db.SetActiveSiteIconPack(second.ID); err != nil {
		t.Fatalf("activate second pack: %v", err)
	}
	active, err := db.ActiveSiteIconPack()
	if err != nil || active.ID != second.ID || len(active.Icons) != 2 || !active.Active {
		t.Fatalf("active pack after switch: %+v, %v", active, err)
	}

	if err := db.DeleteSiteIconPack(first.ID); err != nil {
		t.Fatalf("delete first pack: %v", err)
	}
	packs, err = db.SiteIconPacks()
	if err != nil || len(packs) != 1 || packs[0].ID != second.ID {
		t.Fatalf("packs after delete: %+v, %v", packs, err)
	}

	if err := db.ClearSiteIconPack(); err != nil {
		t.Fatalf("clear packs: %v", err)
	}
	active, err = db.ActiveSiteIconPack()
	if err != nil || len(active.Icons) != 0 {
		t.Fatalf("active pack after clear: %+v, %v", active, err)
	}
}

func TestHandleSiteIconPacksRoutes(t *testing.T) {
	app := newTestApp(t)
	if err := app.db.ClearSiteIconPack(); err != nil {
		t.Fatal(err)
	}

	created := httptest.NewRecorder()
	payload := []byte(`{"pack":{"name":"route pack","description":"test","icons":[{"name":"Emby","url":"https://cdn.example.test/emby.png"}]},"source_url":""}`)
	app.handleSiteIconPacks(created, httptest.NewRequest(http.MethodPost, "/api/site-icon-packs", bytes.NewReader(payload)))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var createdPack SiteIconPack
	if err := json.Unmarshal(created.Body.Bytes(), &createdPack); err != nil {
		t.Fatalf("decode created pack: %v", err)
	}
	if createdPack.ID <= 0 || !createdPack.Active {
		t.Fatalf("created pack should be active with id: %+v", createdPack)
	}

	listed := httptest.NewRecorder()
	app.handleSiteIconPacks(listed, httptest.NewRequest(http.MethodGet, "/api/site-icon-packs", nil))
	if listed.Code != http.StatusOK || !bytes.Contains(listed.Body.Bytes(), []byte(`"icon_count"`)) {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}

	activatePath := "/api/site-icon-packs/" + strconv.FormatInt(createdPack.ID, 10) + "/activate"
	activated := httptest.NewRecorder()
	app.handleSiteIconPackItem(activated, httptest.NewRequest(http.MethodPost, activatePath, nil))
	if activated.Code != http.StatusOK {
		t.Fatalf("activate status=%d body=%s", activated.Code, activated.Body.String())
	}

	deletePath := "/api/site-icon-packs/" + strconv.FormatInt(createdPack.ID, 10)
	deleted := httptest.NewRecorder()
	app.handleSiteIconPackItem(deleted, httptest.NewRequest(http.MethodDelete, deletePath, nil))
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
}

func TestHandleSiteIconPacksImportsLargeLinkedLibrary(t *testing.T) {
	app := newTestApp(t)
	if err := app.db.ClearSiteIconPack(); err != nil {
		t.Fatal(err)
	}
	const iconCount = 8544
	pack := SiteIconPack{Name: "良友媒体图标库", Description: "来自链接的图标包说明", Icons: make([]SiteIcon, iconCount)}
	for index := range pack.Icons {
		identifier := strconv.Itoa(index)
		pack.Icons[index] = SiteIcon{Name: "良友-站点-" + identifier, URL: "https://raw.githubusercontent.com/example/icons/main/icon-" + identifier + ".png"}
	}
	const sourceURL = "https://raw.githubusercontent.com/example/icons/main/library.json"
	payload, err := json.Marshal(map[string]any{"pack": map[string]any{"name": pack.Name, "description": pack.Description, "icons": pack.Icons}, "source_url": sourceURL})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	app.handleSiteIconPacks(response, httptest.NewRequest(http.MethodPost, "/api/site-icon-packs", bytes.NewReader(payload)))
	if response.Code != http.StatusCreated {
		t.Fatalf("large linked library import status=%d body=%s", response.Code, response.Body.String())
	}
	loaded, err := app.db.SiteIconPack()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Name != pack.Name || loaded.Description != pack.Description || loaded.SourceURL != sourceURL || len(loaded.Icons) != iconCount {
		t.Fatalf("large library metadata or count changed: name=%q description=%q source=%q icons=%d", loaded.Name, loaded.Description, loaded.SourceURL, len(loaded.Icons))
	}
	last := loaded.Icons[len(loaded.Icons)-1]
	selected, err := app.db.CreateSiteRecord(Site{
		Name: "large library selection", IconName: last.Name, IconURL: last.URL,
		ListenPort: 18098, IngressMode: ingressModePort, TargetURL: "http://127.0.0.1:8096",
		PlaybackMode: "direct", StreamHosts: "[]", UAMode: passthroughUAMode,
	})
	if err != nil {
		t.Fatalf("select final icon in large library: %v", err)
	}
	if selected.IconName != last.Name || selected.IconURL != last.URL {
		t.Fatal("large library final icon selection changed")
	}
}

func TestParseSiteIconPackRetainsEntryLimit(t *testing.T) {
	icons := make([]SiteIcon, maxSiteIconCount+1)
	for index := range icons {
		icons[index] = SiteIcon{Name: "icon-" + strconv.Itoa(index), URL: "https://cdn.example.test/icon.png"}
	}
	raw, err := json.Marshal(map[string]any{"icons": icons})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseSiteIconPack(raw); err == nil {
		t.Fatal("icon pack exceeding the entry limit was accepted")
	}
}

func TestRefreshSiteIconPackPreservesIdentityAndSiteSelection(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ClearSiteIconPack(); err != nil {
		t.Fatal(err)
	}
	const sourceURL = "https://cdn.example.test/package.json"
	const oldURL = "https://cdn.example.test/old-emby.png"
	const newURL = "https://cdn.example.test/new-emby.png"
	pack, err := db.CreateSiteIconPack(SiteIconPack{
		Name: "Original", SourceURL: sourceURL,
		Icons: []SiteIcon{{Name: "Emby", URL: oldURL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("UPDATE site_icon_packs SET created_at='2000-01-01 00:00:00', updated_at='2000-01-01 00:00:00' WHERE id=?", pack.ID); err != nil {
		t.Fatal(err)
	}
	var createdAt string
	if err := db.db.QueryRow("SELECT created_at FROM site_icon_packs WHERE id=?", pack.ID).Scan(&createdAt); err != nil {
		t.Fatal(err)
	}
	previous, err := db.SiteIconPackByID(pack.ID)
	if err != nil {
		t.Fatal(err)
	}
	site, err := db.CreateSiteRecord(Site{
		Name: "existing selection", IconName: "Emby", IconURL: oldURL,
		ListenPort: 18099, IngressMode: ingressModePort, TargetURL: "http://127.0.0.1:8096",
		PlaybackMode: "direct", StreamHosts: "[]", UAMode: passthroughUAMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := db.UpdateSiteIconPack(pack.ID, SiteIconPack{
		Name: "Refreshed", Description: "Updated metadata", SourceURL: "https://other.example.test/ignored.json",
		Icons: []SiteIcon{{Name: "Emby", URL: newURL}, {Name: "Jellyfin", URL: "https://cdn.example.test/jellyfin.png"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != pack.ID || !updated.Active || updated.SourceURL != sourceURL || updated.Name != "Refreshed" || updated.Description != "Updated metadata" || len(updated.Icons) != 2 || updated.UpdatedAt == previous.UpdatedAt {
		t.Fatalf("unexpected refreshed package: %+v", updated)
	}
	var refreshedCreatedAt string
	if err := db.db.QueryRow("SELECT created_at FROM site_icon_packs WHERE id=?", pack.ID).Scan(&refreshedCreatedAt); err != nil {
		t.Fatal(err)
	}
	if createdAt != refreshedCreatedAt {
		t.Fatal("refresh changed the package creation time")
	}
	site.Name = "still using the selected icon"
	if err := db.UpdateSiteRecord(*site); err != nil {
		t.Fatalf("edit site after refreshing icon package: %v", err)
	}
	persisted, err := db.GetSite(site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.IconName != "Emby" || persisted.IconURL != oldURL {
		t.Fatal("refresh replaced an existing site icon")
	}
	if _, err := db.UpdateSiteIcon(site.ID, "Jellyfin", "https://cdn.example.test/jellyfin.png"); err != nil {
		t.Fatalf("newly refreshed icon is not selectable: %v", err)
	}
}

func TestRefreshInactiveSiteIconPackDoesNotSwitchActivePack(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ClearSiteIconPack(); err != nil {
		t.Fatal(err)
	}
	first, err := db.CreateSiteIconPack(SiteIconPack{Name: "Active", SourceURL: "https://cdn.example.test/active.json", Icons: []SiteIcon{{Name: "First", URL: "https://cdn.example.test/first.png"}}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.CreateSiteIconPack(SiteIconPack{Name: "Inactive", SourceURL: "https://cdn.example.test/inactive.json", Icons: []SiteIcon{{Name: "Second", URL: "https://cdn.example.test/second.png"}}})
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := db.UpdateSiteIconPack(second.ID, SiteIconPack{Name: "Inactive updated", Icons: []SiteIcon{{Name: "Third", URL: "https://cdn.example.test/third.png"}}})
	if err != nil {
		t.Fatal(err)
	}
	active, err := db.ActiveSiteIconPack()
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Active || active.ID != first.ID || refreshed.SourceURL != second.SourceURL {
		t.Fatalf("inactive refresh switched the active package: active=%+v refreshed=%+v", active, refreshed)
	}
}

func TestSiteIconPackActivationPreservesContentUpdatedAt(t *testing.T) {
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pack, err := db.CreateSiteIconPack(SiteIconPack{Name: "Unchanged", SourceURL: "https://cdn.example.test/package.json", Icons: []SiteIcon{{Name: "Emby", URL: "https://cdn.example.test/emby.png"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("UPDATE site_icon_packs SET updated_at='2000-01-01 00:00:00' WHERE id=?", pack.ID); err != nil {
		t.Fatal(err)
	}
	previous, err := db.SiteIconPackByID(pack.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetActiveSiteIconPack(pack.ID); err != nil {
		t.Fatal(err)
	}
	active, err := db.SiteIconPack()
	if err != nil {
		t.Fatal(err)
	}
	if active.UpdatedAt != previous.UpdatedAt {
		t.Fatal("activating a package changed its content update time")
	}
}

func TestHandleSiteIconPackRefreshRejectsInvalidUpdatesWithoutReplacement(t *testing.T) {
	app := newTestApp(t)
	pack, err := app.db.CreateSiteIconPack(SiteIconPack{Name: "Original", SourceURL: "https://cdn.example.test/package.json", Icons: []SiteIcon{{Name: "Emby", URL: "https://cdn.example.test/emby.png"}}})
	if err != nil {
		t.Fatal(err)
	}
	before, err := app.db.SiteIconPackByID(pack.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/site-icon-packs/" + strconv.FormatInt(pack.ID, 10)
	for _, payload := range []string{
		`{"pack":{"name":"Bad","icons":[{"name":"Emby","url":"http://cdn.example.test/a.png"}]}}`,
		`{"pack":{"icons":[{"name":"Emby","url":"https://cdn.example.test/a.png","html":"<svg>"}]}}`,
		`{"pack":{"icons":[]}}`,
		`{"pack":{"icons":[{"name":"Emby","url":"https://cdn.example.test/a.png"}]},"source_url":"https://other.example.test/package.json"}`,
	} {
		response := httptest.NewRecorder()
		app.handleSiteIconPackItem(response, httptest.NewRequest(http.MethodPut, path, bytes.NewBufferString(payload)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid refresh status=%d body=%s", response.Code, response.Body.String())
		}
		after, err := app.db.SiteIconPackByID(pack.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatal("invalid refresh changed the saved package")
		}
	}
	missing := httptest.NewRecorder()
	app.handleSiteIconPackItem(missing, httptest.NewRequest(http.MethodPut, "/api/site-icon-packs/999999", bytes.NewBufferString(`{"pack":{}}`)))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing package status=%d body=%s", missing.Code, missing.Body.String())
	}
	unlinked, err := app.db.CreateSiteIconPack(SiteIconPack{Name: "Uploaded", Icons: []SiteIcon{{Name: "Emby", URL: "https://cdn.example.test/emby.png"}}})
	if err != nil {
		t.Fatal(err)
	}
	noSource := httptest.NewRecorder()
	app.handleSiteIconPackItem(noSource, httptest.NewRequest(http.MethodPut, "/api/site-icon-packs/"+strconv.FormatInt(unlinked.ID, 10), bytes.NewBufferString(`{"pack":{}}`)))
	if noSource.Code != http.StatusBadRequest {
		t.Fatalf("unlinked package refresh status=%d body=%s", noSource.Code, noSource.Body.String())
	}
}

func TestHandleSiteIconPackRefreshReturnsUpdatedPackage(t *testing.T) {
	app := newTestApp(t)
	pack, err := app.db.CreateSiteIconPack(SiteIconPack{Name: "Original", SourceURL: "https://cdn.example.test/package.json", Icons: []SiteIcon{{Name: "Emby", URL: "https://cdn.example.test/emby.png"}}})
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"pack":{"name":"Refreshed","description":"New source metadata","icons":[{"name":"Emby","url":"https://cdn.example.test/emby.png"},{"name":"Jellyfin","url":"https://cdn.example.test/jellyfin.png"}]}}`
	response := httptest.NewRecorder()
	app.handleSiteIconPackItem(response, httptest.NewRequest(http.MethodPut, "/api/site-icon-packs/"+strconv.FormatInt(pack.ID, 10), bytes.NewBufferString(payload)))
	if response.Code != http.StatusOK {
		t.Fatalf("refresh status=%d body=%s", response.Code, response.Body.String())
	}
	var refreshed SiteIconPack
	if err := json.Unmarshal(response.Body.Bytes(), &refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed.ID != pack.ID || refreshed.SourceURL != pack.SourceURL || refreshed.Active != pack.Active || refreshed.Name != "Refreshed" || len(refreshed.Icons) != 2 {
		t.Fatalf("unexpected refresh response: %+v", refreshed)
	}
}
