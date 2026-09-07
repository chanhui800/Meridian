package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDashboardBootstrapUsesOneSnapshotWithoutSiteFlush(t *testing.T) {
	app := newTestApp(t)
	if _, err := app.db.CreateSite("bootstrap", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0); err != nil {
		t.Fatal(err)
	}
	setDBReadonly(t, app, true)
	defer setDBReadonly(t, app, false)

	rr := httptest.NewRecorder()
	app.handleDashboardBootstrap(rr, httptest.NewRequest(http.MethodGet, "/api/dashboard/bootstrap", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var payload dashboardBootstrapResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Snapshot == nil || payload.Snapshot.TotalSites != 1 || len(payload.Sites) != 1 {
		t.Fatalf("bootstrap payload=%+v, want one site snapshot", payload)
	}
}
