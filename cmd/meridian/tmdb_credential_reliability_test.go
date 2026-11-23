package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func credentialReliabilityFixture(t *testing.T, server *httptest.Server) (*App, *tmdbService) {
	t.Helper()
	database := openWatchHistoryTestDB(t)
	site := createWatchHistoryTestSite(t, database, true)
	event := watchHistoryTestEvent(site.ID, "credential-reliability", time.Now().UnixMilli(), 100)
	event.TMDBID = 42
	writeTMDBRegressionEvent(t, database, event)
	ciphertext, err := encryptTMDBReadToken(strings.Repeat("synthetic-read-token-", 3))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.saveTMDBSettings(TMDBSettings{Enabled: true, Language: "zh-CN"}, ciphertext, true); err != nil {
		t.Fatal(err)
	}
	if err := database.markTMDBCredentialResult(tmdbCredentialReady, "", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	service := newTMDBService(database, newTMDBClientForTest(server.Client(), server.URL, server.URL))
	return &App{db: database, tmdb: service}, service
}

func TestTMDBCredentialTemporaryTestFailureDoesNotPauseWorker(t *testing.T) {
	for _, status := range []int{0, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var recovered atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/configuration" && !recovered.Load() {
					if status == 0 {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err == nil {
							_ = conn.Close()
						}
						return
					}
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"status_message":"temporary unavailable"}`))
					return
				}
				if r.URL.Path == "/configuration" {
					_, _ = w.Write([]byte(`{"images":{"secure_base_url":"https://image.tmdb.org/"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":42,"title":"Example Movie","overview":"worker recovered automatically","credits":{"cast":[]},"images":{"backdrops":[]}}`))
			}))
			defer server.Close()
			app, service := credentialReliabilityFixture(t, server)
			response := httptest.NewRecorder()
			app.handleTMDBTest(response, httptest.NewRequest(http.MethodPost, "/api/tmdb-settings/test", nil))
			if response.Code != http.StatusBadGateway {
				t.Fatalf("temporary credential test status=%d", response.Code)
			}
			settingsResponse := httptest.NewRecorder()
			app.handleTMDBSettings(settingsResponse, httptest.NewRequest(http.MethodGet, "/api/tmdb-settings", nil))
			var settings TMDBSettings
			if err := json.Unmarshal(settingsResponse.Body.Bytes(), &settings); err != nil {
				t.Fatal(err)
			}
			if settings.CredentialState == tmdbCredentialInvalid {
				t.Errorf("temporary HTTP %d invalidated a previously working credential", status)
			}
			recovered.Store(true)
			if processed, err := service.runOne(t.Context(), time.Now().Add(24*time.Hour)); err != nil || !processed {
				t.Fatalf("worker did not recover without a manual retest: processed=%v err=%v", processed, err)
			}
			entries, err := app.db.ListWatchHistory(WatchHistoryFilter{})
			if err != nil || len(entries) != 1 || entries[0].Overview != "worker recovered automatically" {
				t.Fatalf("recovered history metadata=%+v err=%v", entries, err)
			}
		})
	}
}

func TestTMDBCredentialAuthFailureRemainsPausedUntilSuccessfulRetest(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var responseStatus atomic.Int32
			responseStatus.Store(int32(status))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/configuration" {
					if code := responseStatus.Load(); code != 0 {
						w.WriteHeader(int(code))
						_, _ = w.Write([]byte(`{"status_message":"request rejected"}`))
						return
					}
					_, _ = w.Write([]byte(`{"images":{"secure_base_url":"https://image.tmdb.org/"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":42,"title":"Example Movie","credits":{"cast":[]},"images":{"backdrops":[]}}`))
			}))
			defer server.Close()
			app, service := credentialReliabilityFixture(t, server)
			for _, code := range []int{status, http.StatusServiceUnavailable} {
				responseStatus.Store(int32(code))
				response := httptest.NewRecorder()
				app.handleTMDBTest(response, httptest.NewRequest(http.MethodPost, "/api/tmdb-settings/test", nil))
				if response.Code != http.StatusBadGateway {
					t.Fatalf("failed test status=%d", response.Code)
				}
				settings, err := app.db.tmdbPublicSettings()
				if err != nil || settings.CredentialState != tmdbCredentialInvalid || settings.LastErrorCode != "auth" {
					t.Fatalf("HTTP %d erased a conclusive authentication failure: settings=%+v err=%v", code, settings, err)
				}
				if processed, err := service.runOne(t.Context(), time.Now().Add(24*time.Hour)); err != nil || processed {
					t.Fatalf("invalid credential resumed worker: processed=%v err=%v", processed, err)
				}
			}
			responseStatus.Store(0)
			response := httptest.NewRecorder()
			app.handleTMDBTest(response, httptest.NewRequest(http.MethodPost, "/api/tmdb-settings/test", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("successful retest status=%d", response.Code)
			}
			if processed, err := service.runOne(t.Context(), time.Now().Add(24*time.Hour)); err != nil || !processed {
				t.Fatalf("successful retest did not resume worker: processed=%v err=%v", processed, err)
			}
		})
	}
}

func TestTMDBCredentialUpgradeRepairsOnlyKnownTemporaryInvalidation(t *testing.T) {
	for _, testCase := range []struct {
		code      string
		wantState string
	}{
		{"network", tmdbCredentialUnknown}, {"rate_limited", tmdbCredentialUnknown}, {"server", tmdbCredentialUnknown},
		{"read", tmdbCredentialUnknown}, {"invalid_json", tmdbCredentialUnknown}, {"invalid_configuration", tmdbCredentialUnknown},
		{"auth", tmdbCredentialInvalid}, {"decrypt", tmdbCredentialInvalid}, {"unrecognized", tmdbCredentialInvalid}, {"", tmdbCredentialInvalid},
	} {
		t.Run(testCase.code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":42,"title":"Example Movie","credits":{"cast":[]},"images":{"backdrops":[]}}`))
			}))
			defer server.Close()
			app, oldService := credentialReliabilityFixture(t, server)
			if _, err := app.db.db.Exec(`UPDATE tmdb_settings SET credential_state='invalid',last_error_code=? WHERE id=1`, testCase.code); err != nil {
				t.Fatal(err)
			}
			if _, err := app.db.db.Exec("PRAGMA user_version=58"); err != nil {
				t.Fatal(err)
			}
			path := app.db.dbPath
			app.db.Close()
			upgraded, err := openDB(path)
			if err != nil {
				t.Fatal(err)
			}
			defer upgraded.Close()
			settings, err := upgraded.tmdbPublicSettings()
			if err != nil || settings.CredentialState != testCase.wantState || settings.LastErrorCode != testCase.code {
				t.Fatalf("upgrade changed credential classification incorrectly: settings=%+v err=%v", settings, err)
			}
			if err := upgraded.migrate(); err != nil {
				t.Fatal(err)
			}
			service := newTMDBService(upgraded, oldService.client)
			processed, err := service.runOne(t.Context(), time.Now().Add(24*time.Hour))
			if err != nil || processed != (testCase.wantState == tmdbCredentialUnknown) {
				t.Fatalf("upgraded worker processed=%v err=%v for prior error %q", processed, err, testCase.code)
			}
		})
	}
}

func TestTMDBCredentialStaleResultCannotInvalidateReplacementToken(t *testing.T) {
	for _, useWorker := range []bool{false, true} {
		t.Run(fmt.Sprintf("worker=%t", useWorker), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				close(entered)
				<-release
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"status_message":"old token was revoked"}`))
			}))
			defer server.Close()
			app, service := credentialReliabilityFixture(t, server)
			done := make(chan error, 1)
			go func() {
				if useWorker {
					_, err := service.runOne(t.Context(), time.Now())
					done <- err
					return
				}
				response := httptest.NewRecorder()
				app.handleTMDBTest(response, httptest.NewRequest(http.MethodPost, "/api/tmdb-settings/test", nil))
				done <- nil
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				close(release)
				t.Fatal("old credential request did not reach upstream")
			}
			response := httptest.NewRecorder()
			app.handleTMDBSettings(response, httptest.NewRequest(http.MethodPost, "/api/tmdb-settings",
				strings.NewReader(`{"token":"`+strings.Repeat("replacement-read-token-", 3)+`"}`)))
			close(release)
			if response.Code != http.StatusOK {
				t.Fatalf("replacement token save status=%d", response.Code)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("old credential result did not finish")
			}
			settings, err := app.db.tmdbPublicSettings()
			if err != nil || settings.CredentialState != tmdbCredentialUnknown || settings.LastErrorCode != "" {
				t.Fatalf("late old credential result invalidated replacement: settings=%+v err=%v", settings, err)
			}
		})
	}
}

func TestTMDBCredentialOlderWorkerResultCannotOverwriteNewerTest(t *testing.T) {
	for _, workerSucceeded := range []bool{true, false} {
		for _, earlierClockOffset := range []time.Duration{0, time.Second} {
			t.Run(fmt.Sprintf("worker_success=%t_clock_offset=%s", workerSucceeded, earlierClockOffset), func(t *testing.T) {
				entered, release := make(chan struct{}), make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/configuration" {
						if workerSucceeded {
							w.WriteHeader(http.StatusUnauthorized)
							_, _ = w.Write([]byte(`{"status_message":"token revoked"}`))
						} else {
							_, _ = w.Write([]byte(`{"images":{"secure_base_url":"https://image.tmdb.org/"}}`))
						}
						return
					}
					close(entered)
					<-release
					if !workerSucceeded {
						w.WriteHeader(http.StatusUnauthorized)
						_, _ = w.Write([]byte(`{"status_message":"old request was rejected"}`))
						return
					}
					_, _ = w.Write([]byte(`{"id":42,"title":"Before Revocation","credits":{"cast":[]},"images":{"backdrops":[]}}`))
				}))
				defer server.Close()
				app, service := credentialReliabilityFixture(t, server)
				done := make(chan error, 1)
				go func() {
					_, err := service.runOne(t.Context(), time.Now().Add(earlierClockOffset))
					done <- err
				}()
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					close(release)
					t.Fatal("old metadata request did not reach upstream")
				}
				// Distinguish starts on the ordinary clock, and also exercise a
				// one-second backwards clock correction between the requests.
				time.Sleep(10 * time.Millisecond)
				response := httptest.NewRecorder()
				app.handleTMDBTest(response, httptest.NewRequest(http.MethodPost, "/api/tmdb-settings/test", nil))
				wantState, wantCode, wantStatus := tmdbCredentialReady, "", http.StatusOK
				if workerSucceeded {
					wantState, wantCode, wantStatus = tmdbCredentialInvalid, "auth", http.StatusBadGateway
				}
				newer, err := app.db.tmdbPublicSettings()
				if err != nil || response.Code != wantStatus || newer.CredentialState != wantState || newer.LastErrorCode != wantCode {
					close(release)
					t.Fatalf("newer credential result not persisted: settings=%+v status=%d err=%v", newer, response.Code, err)
				}
				close(release)
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("old credential request did not finish")
				}
				final, err := app.db.tmdbPublicSettings()
				if err != nil || final.CredentialState != wantState || final.LastErrorCode != wantCode || final.LastTestedAtMS < newer.LastTestedAtMS {
					t.Fatalf("old worker overwrote newer credential proof: final=%+v newer=%+v err=%v", final, newer, err)
				}
			})
		}
	}
}
