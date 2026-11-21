package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustFailoverURL(t *testing.T, value string) *url.URL {
	t.Helper()
	target, err := normalizeTargetURL(value)
	if err != nil {
		t.Fatalf("normalize target %q: %v", value, err)
	}
	return target
}

func failoverRequest(t *testing.T, method, path string, primary *url.URL, targets []*url.URL) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, "http://meridian.test"+path, nil)
	original := *req.URL
	applyUpstreamURL(req.URL, primary)
	req.Host = primary.Host
	return withFailoverRequestPlan(req, &original, targets, primary, 4<<20)
}

func TestFailoverTransportRetriesOnlyRetryableReadFailures(t *testing.T) {
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		if r.URL.Path != "/emby/System/Info" {
			t.Fatalf("backup path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("backup"))
	}))
	defer backup.Close()

	primaryTarget := mustFailoverURL(t, primary.URL+"/emby")
	backupTarget := mustFailoverURL(t, backup.URL+"/emby")
	transport := &failoverTransport{base: http.DefaultTransport, state: newUpstreamFailoverState()}
	resp, err := transport.RoundTrip(failoverRequest(t, http.MethodGet, "/System/Info", primaryTarget, []*url.URL{primaryTarget, backupTarget}))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "backup" {
		t.Fatalf("response = %d %q", resp.StatusCode, body)
	}
	if primaryCalls.Load() != 1 || backupCalls.Load() != 1 {
		t.Fatalf("calls primary=%d backup=%d", primaryCalls.Load(), backupCalls.Load())
	}

	// The working backup is preferred while its five-minute preference remains.
	resp, err = transport.RoundTrip(failoverRequest(t, http.MethodGet, "/System/Info", primaryTarget, []*url.URL{primaryTarget, backupTarget}))
	if err != nil {
		t.Fatalf("preferred RoundTrip: %v", err)
	}
	resp.Body.Close()
	if primaryCalls.Load() != 1 || backupCalls.Load() != 2 {
		t.Fatalf("preference calls primary=%d backup=%d", primaryCalls.Load(), backupCalls.Load())
	}
}

func TestFailoverTransportDoesNotRetryNonRetryableOrWrites(t *testing.T) {
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backup.Close()
	primaryTarget := mustFailoverURL(t, primary.URL)
	backupTarget := mustFailoverURL(t, backup.URL)
	transport := &failoverTransport{base: http.DefaultTransport, state: newUpstreamFailoverState()}
	targets := []*url.URL{primaryTarget, backupTarget}

	resp, err := transport.RoundTrip(failoverRequest(t, http.MethodGet, "/missing", primaryTarget, targets))
	if err != nil {
		t.Fatalf("GET RoundTrip: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || primaryCalls.Load() != 1 || backupCalls.Load() != 0 {
		t.Fatalf("GET calls/status primary=%d backup=%d status=%d", primaryCalls.Load(), backupCalls.Load(), resp.StatusCode)
	}

	resp, err = transport.RoundTrip(failoverRequest(t, http.MethodPost, "/write", primaryTarget, targets))
	if err != nil {
		t.Fatalf("POST RoundTrip: %v", err)
	}
	resp.Body.Close()
	if primaryCalls.Load() != 2 || backupCalls.Load() != 0 {
		t.Fatalf("POST unexpectedly retried primary=%d backup=%d", primaryCalls.Load(), backupCalls.Load())
	}
}

func TestFailoverTransportRoutesWritesToEstablishedActiveLineWithoutRetry(t *testing.T) {
	const payload = `{"PositionTicks":12345}`
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			if r.URL.Path != "/emby/Sessions/Playing/Progress" || string(body) != payload {
				t.Fatalf("active write path=%q body=%q", r.URL.Path, body)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backup.Close()

	primaryTarget := mustFailoverURL(t, primary.URL+"/emby")
	backupTarget := mustFailoverURL(t, backup.URL+"/emby")
	targets := []*url.URL{primaryTarget, backupTarget}
	transport := &failoverTransport{base: http.DefaultTransport, state: newUpstreamFailoverState()}
	readResp, err := transport.RoundTrip(failoverRequest(t, http.MethodGet, "/System/Info", primaryTarget, targets))
	if err != nil {
		t.Fatalf("establish active line: %v", err)
	}
	readResp.Body.Close()

	writeReq := httptest.NewRequest(http.MethodPost, "http://meridian.test/Sessions/Playing/Progress", strings.NewReader(payload))
	original := *writeReq.URL
	applyUpstreamURL(writeReq.URL, primaryTarget)
	writeReq.Host = primaryTarget.Host
	writeReq = withFailoverRequestPlan(writeReq, &original, targets, primaryTarget, 4<<20)
	writeResp, err := transport.RoundTrip(writeReq)
	if err != nil {
		t.Fatalf("active write: %v", err)
	}
	writeResp.Body.Close()
	if writeResp.StatusCode != http.StatusNoContent || primaryCalls.Load() != 1 || backupCalls.Load() != 2 {
		t.Fatalf("status=%d primary=%d backup=%d", writeResp.StatusCode, primaryCalls.Load(), backupCalls.Load())
	}
}

func TestFailoverTransportRetriesReplayablePlaybackInfoPost(t *testing.T) {
	const payload = `{"DeviceProfile":{"MaxStreamingBitrate":120000000}}`
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/emby/Items/1/PlaybackInfo" || string(body) != payload {
			t.Fatalf("backup request method=%q path=%q body=%q", r.Method, r.URL.Path, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaSources":[]}`))
	}))
	defer backup.Close()

	primaryTarget := mustFailoverURL(t, primary.URL+"/emby")
	backupTarget := mustFailoverURL(t, backup.URL+"/emby")
	req := httptest.NewRequest(http.MethodPost, "http://meridian.test/Items/1/PlaybackInfo", strings.NewReader(payload))
	if err := prepareFailoverPlaybackInfoBody(req, 4<<20); err != nil {
		t.Fatalf("prepare body: %v", err)
	}
	original := *req.URL
	applyUpstreamURL(req.URL, primaryTarget)
	req.Host = primaryTarget.Host
	req = withFailoverRequestPlan(req, &original, []*url.URL{primaryTarget, backupTarget}, primaryTarget, 4<<20)

	transport := &failoverTransport{base: http.DefaultTransport, state: newUpstreamFailoverState()}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || primaryCalls.Load() != 1 || backupCalls.Load() != 1 {
		t.Fatalf("status=%d primary=%d backup=%d", resp.StatusCode, primaryCalls.Load(), backupCalls.Load())
	}
}

func TestFailoverTransportDoesNotRetryUnreplayablePlaybackInfoPost(t *testing.T) {
	var calls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer primary.Close()
	primaryTarget := mustFailoverURL(t, primary.URL)
	backupTarget := mustFailoverURL(t, "https://backup.example")
	req := failoverRequest(t, http.MethodPost, "/emby/Items/1/PlaybackInfo", primaryTarget, []*url.URL{primaryTarget, backupTarget})
	req.Body = io.NopCloser(strings.NewReader("{}"))
	req.ContentLength = 2
	req.GetBody = nil
	transport := &failoverTransport{base: http.DefaultTransport, state: newUpstreamFailoverState()}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	resp.Body.Close()
	if calls.Load() != 1 || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("calls=%d status=%d", calls.Load(), resp.StatusCode)
	}
}

func TestFailoverTransportDoesNotRetryReadWithUnreplayableBody(t *testing.T) {
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		if body, err := io.ReadAll(r.Body); err != nil || string(body) != "body" {
			t.Errorf("primary request body = %q, %v", body, err)
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(primary.Close)
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backup.Close)
	primaryTarget := mustFailoverURL(t, primary.URL)
	backupTarget := mustFailoverURL(t, backup.URL)
	req := failoverRequest(t, http.MethodGet, "/System/Info", primaryTarget, []*url.URL{primaryTarget, backupTarget})
	req.Body = io.NopCloser(strings.NewReader("body"))
	req.ContentLength = 4
	transport := &failoverTransport{base: http.DefaultTransport, state: newUpstreamFailoverState()}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if primaryCalls.Load() != 1 || backupCalls.Load() != 0 || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d primary=%d backup=%d", resp.StatusCode, primaryCalls.Load(), backupCalls.Load())
	}
}

func TestFailoverProxyKeepsPlaybackRoutingWhileOrdinaryRequestsUseBackup(t *testing.T) {
	for _, mode := range []string{"main-video-direct", "legacy-redirect", "separate-playback"} {
		t.Run(mode, func(t *testing.T) {
			app := newTestApp(t)
			playback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, "configured playback")
			}))
			t.Cleanup(playback.Close)
			const streamPath = "/Videos/1/stream.mkv"
			const manifestPath = "/Videos/1/master.m3u8"
			const apiPath = "/System/Info"
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != apiPath && r.URL.Path != streamPath && r.URL.Path != manifestPath {
					t.Errorf("primary got unexpected path %q", r.URL.Path)
				}
				if (r.URL.Path == streamPath || r.URL.Path == manifestPath) && mode == "legacy-redirect" {
					w.Header().Set("Location", playback.URL+r.URL.Path)
					w.WriteHeader(http.StatusFound)
					return
				}
				w.WriteHeader(http.StatusBadGateway)
			}))
			t.Cleanup(primary.Close)
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != apiPath {
					t.Errorf("playback request reached backup: %q", r.URL.Path)
				}
				_, _ = io.WriteString(w, "backup API")
			}))
			t.Cleanup(backup.Close)
			site := Site{
				ID: 1, Name: mode, PublicHost: "relay.example", IngressMode: ingressModeHost,
				TargetURL: primary.URL, FailoverTargetList: []string{backup.URL}, PlaybackMode: "direct",
				MainVideoStreamMode: mainVideoStreamModeProxy, StreamHosts: "[]", UAMode: passthroughUAMode,
			}
			switch mode {
			case "main-video-direct":
				site.MainVideoStreamMode = mainVideoStreamModeDirect
			case "legacy-redirect":
				site.PlaybackMode = "redirect"
				site.PlaybackTargetURL = playback.URL
			case "separate-playback":
				site.PlaybackTargetURL = playback.URL
			}
			if err := app.pm.StartSite(site); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
			router := app.publicHostRouter(http.NotFoundHandler())
			get := func(path string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://relay.example"+path, nil))
				return rec
			}
			if rec := get(apiPath); rec.Code != http.StatusOK || rec.Body.String() != "backup API" {
				t.Fatalf("API response status=%d body=%q", rec.Code, rec.Body.String())
			}
			rec := get(streamPath)
			if mode == "main-video-direct" {
				if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != primary.URL+streamPath {
					t.Fatalf("direct playback status=%d Location=%q", rec.Code, rec.Header().Get("Location"))
				}
			} else if rec.Code != http.StatusOK || rec.Body.String() != "configured playback" {
				t.Fatalf("separate playback status=%d body=%q", rec.Code, rec.Body.String())
			}
			if mode == "legacy-redirect" {
				if manifest := get(manifestPath); manifest.Code != http.StatusOK || manifest.Body.String() != "configured playback" {
					t.Fatalf("legacy manifest status=%d body=%q", manifest.Code, manifest.Body.String())
				}
			}
		})
	}
}

func TestFailoverProxyPathIngressRewritesUsingBackupBase(t *testing.T) {
	app := newTestApp(t)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(primary.Close)
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backup/System/Info":
			w.Header().Set("Location", "http://"+r.Host+"/backup/web/index.html?x=1")
			w.Header().Add("Set-Cookie", "test_language=zh; Path=/backup/; HttpOnly")
			w.WriteHeader(http.StatusFound)
		case "/backup/web/index.html":
			_, _ = io.WriteString(w, "backup web")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(backup.Close)
	site := Site{
		ID: 1, Name: "path-entry-failover", IngressMode: ingressModePath, PathPrefix: "/relay",
		TargetURL: primary.URL + "/primary", FailoverTargetList: []string{backup.URL + "/backup"},
		PlaybackMode: "direct", MainVideoStreamMode: mainVideoStreamModeProxy, StreamHosts: "[]", UAMode: passthroughUAMode,
	}
	if err := app.pm.StartSite(site); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	router := app.publicHostRouter(http.NotFoundHandler())
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://panel.example"+path, nil))
		return rec
	}
	rec := get("/relay/System/Info")
	location := rec.Header().Get("Location")
	if rec.Code != http.StatusFound || location != "/relay/web/index.html?x=1" {
		t.Fatalf("redirect status=%d location=%q", rec.Code, location)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Path != "/relay/" {
		t.Fatalf("rewritten cookies = %v", cookies)
	}
	if next := get(location); next.Code != http.StatusOK || next.Body.String() != "backup web" {
		t.Fatalf("redirect destination status=%d body=%q", next.Code, next.Body.String())
	}
}

func TestParseFailoverTargetsRejectsDuplicatesAndLimits(t *testing.T) {
	if _, err := parseFailoverTargets("https://primary.example", []string{"https://primary.example"}); err == nil {
		t.Fatal("duplicate primary accepted")
	}
	tooMany := make([]string, maxFailoverTargets)
	for i := range tooMany {
		tooMany[i] = "https://backup" + string(rune('a'+i)) + ".example"
	}
	if _, err := parseFailoverTargets("https://primary.example", tooMany); err == nil {
		t.Fatal("too many backups accepted")
	}

	state := newUpstreamFailoverState()
	primary := mustFailoverURL(t, "https://primary.example")
	backup := mustFailoverURL(t, "https://backup.example")
	targets := []*url.URL{primary, backup}
	now := time.Now()
	initial := state.ordered(targets, now)
	state.markFailure(primary, now, &initial)
	ordered := state.ordered(targets, now)
	if ordered.targets[0] != backup {
		t.Fatalf("cooled primary remained first: %s", ordered.targets[0])
	}
}

func TestNormalizeFailoverLinesKeepsDisabledMetadataOutOfRuntimeTargets(t *testing.T) {
	lines, targets, err := normalizeFailoverLines("https://primary.example", []FailoverLine{
		{Name: "备用一", URL: "https://backup-one.example", Enabled: true},
		{Name: "停用线路", URL: "https://backup-two.example", Enabled: false},
	}, nil)
	if err != nil {
		t.Fatalf("normalizeFailoverLines: %v", err)
	}
	if len(lines) != 2 || len(targets) != 1 || targets[0] != "https://backup-one.example" {
		t.Fatalf("lines=%+v targets=%+v", lines, targets)
	}
	if lines[1].Name != "停用线路" || lines[1].Enabled {
		t.Fatalf("disabled metadata changed: %+v", lines[1])
	}
}

func TestNormalizeFailoverLinesConvertsLegacyTargets(t *testing.T) {
	lines, targets, err := normalizeFailoverLines("https://primary.example", nil, []string{"https://backup.example"})
	if err != nil {
		t.Fatalf("normalizeFailoverLines legacy: %v", err)
	}
	if len(lines) != 1 || lines[0].Name != "线路2" || !lines[0].Enabled || len(targets) != 1 {
		t.Fatalf("legacy conversion lines=%+v targets=%+v", lines, targets)
	}
}

func TestFailoverStateProbesRecoveredPrimaryAndRestoresPriority(t *testing.T) {
	state := newUpstreamFailoverState()
	primary := mustFailoverURL(t, "https://primary.example")
	backup := mustFailoverURL(t, "https://backup.example")
	targets := []*url.URL{primary, backup}
	now := time.Now()
	initial := state.ordered(targets, now)
	state.markFailure(primary, now, &initial)
	state.markSuccess(backup, now, initial)

	duringCooldown := state.ordered(targets, now.Add(failoverCooldown-time.Second))
	if duringCooldown.targets[0] != backup {
		t.Fatalf("backup not preferred during primary cooldown: %s", duringCooldown.targets[0])
	}
	probe := state.ordered(targets, now.Add(failoverCooldown+time.Second))
	if !probe.probe || probe.targets[0] != primary {
		t.Fatalf("primary was not selected for recovery probe: %s", probe.targets[0])
	}
	concurrent := state.ordered(targets, now.Add(failoverCooldown+2*time.Second))
	if len(concurrent.targets) != 1 || concurrent.targets[0] != backup {
		t.Fatalf("concurrent request did not stay on backup: %s", concurrent.targets)
	}
	state.markSuccess(backup, now.Add(failoverCooldown+3*time.Second), concurrent)
	pending := state.ordered(targets, now.Add(2*failoverCooldown+2*time.Second))
	if len(pending.targets) != 1 || pending.targets[0] != backup {
		t.Fatalf("backup response freed the outstanding primary probe: %s", pending.targets)
	}
	state.markSuccess(primary, now.Add(2*failoverCooldown+3*time.Second), probe)
	state.markSuccess(backup, now.Add(2*failoverCooldown+4*time.Second), pending)
	restored := state.ordered(targets, now.Add(2*failoverCooldown+5*time.Second))
	state.releaseProbe(probe)
	if restored.targets[0] != primary {
		t.Fatalf("late backup response superseded the recovered primary: %s", restored.targets[0])
	}
}

func TestFailoverStateProbeSurvivesPreferenceChanges(t *testing.T) {
	for _, change := range []string{"backup-failure", "preference-expiry"} {
		t.Run(change, func(t *testing.T) {
			state := newUpstreamFailoverState()
			primary := mustFailoverURL(t, "https://primary.example")
			backup := mustFailoverURL(t, "https://backup.example")
			targets := []*url.URL{primary, backup}
			now := time.Unix(1700000000, 0)
			initial := state.ordered(targets, now)
			state.markFailure(primary, now, &initial)
			state.markSuccess(backup, now, initial)
			probe := state.ordered(targets, now.Add(failoverCooldown+time.Second))
			if !probe.probe {
				t.Fatal("primary recovery probe was not reserved")
			}
			changedAt := now.Add(2 * failoverCooldown)
			if change == "preference-expiry" {
				changedAt = now.Add(failoverPreferredLifetime + time.Second)
			}
			concurrent := state.ordered(targets, changedAt)
			if len(concurrent.targets) != 1 || concurrent.targets[0] != backup {
				t.Fatalf("pending probe admitted another primary attempt: %v", concurrent.targets)
			}
			if change == "backup-failure" {
				state.markFailure(backup, changedAt, &concurrent)
			}
			state.markSuccess(backup, changedAt.Add(time.Second), concurrent)
			pending := state.ordered(targets, changedAt.Add(failoverCooldown+time.Second))
			if len(pending.targets) != 1 || pending.targets[0] != backup {
				t.Fatalf("backup result released the primary reservation: %v", pending.targets)
			}
			state.markSuccess(primary, changedAt.Add(failoverCooldown+2*time.Second), probe)
			state.releaseProbe(probe)
			if preferred, _ := state.preferredTarget(targets, changedAt.Add(failoverCooldown+3*time.Second)); preferred != primary {
				t.Fatalf("successful outstanding probe did not restore primary: %v", preferred)
			}
		})
	}
}

func TestFailoverStateFailedProbeKeepsCooldownUntilReleased(t *testing.T) {
	state := newUpstreamFailoverState()
	primary := mustFailoverURL(t, "https://primary.example")
	backup := mustFailoverURL(t, "https://backup.example")
	targets := []*url.URL{primary, backup}
	now := time.Unix(1700000000, 0)
	initial := state.ordered(targets, now)
	state.markFailure(primary, now, &initial)
	state.markSuccess(backup, now, initial)
	probe := state.ordered(targets, now.Add(failoverCooldown+time.Second))
	failedAt := now.Add(failoverCooldown + 2*time.Second)
	state.markFailure(primary, failedAt, &probe)
	state.markSuccess(backup, failedAt.Add(time.Second), probe)
	pending := state.ordered(targets, failedAt.Add(2*time.Second))
	if len(pending.targets) != 1 || pending.targets[0] != backup {
		t.Fatalf("fallback success released an outstanding failed probe: %v", pending.targets)
	}
	state.releaseProbe(probe)
	cooling := state.ordered(targets, failedAt.Add(failoverCooldown-time.Second))
	if cooling.probe || cooling.targets[0] != backup {
		t.Fatalf("failed probe lost its cooldown: %v", cooling.targets)
	}
	retry := state.ordered(targets, failedAt.Add(failoverCooldown+time.Second))
	defer state.releaseProbe(retry)
	if !retry.probe || retry.targets[0] != primary {
		t.Fatalf("failed probe was not released for recovery: %v", retry.targets)
	}
}

func TestFailoverTransportReleasesCancelledPrimaryProbe(t *testing.T) {
	state := newUpstreamFailoverState()
	primary := mustFailoverURL(t, "https://primary.example")
	backup := mustFailoverURL(t, "https://backup.example")
	targets := []*url.URL{primary, backup}
	now := time.Now()
	initial := state.ordered(targets, now.Add(-2*failoverCooldown))
	state.markFailure(primary, now.Add(-2*failoverCooldown), &initial)
	state.markSuccess(backup, now, initial)
	started := make(chan struct{})
	var primaryCalls atomic.Int32
	transport := &failoverTransport{state: state, base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == primary.Host {
			primaryCalls.Add(1)
			close(started)
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: req}, nil
	})}
	request := failoverRequest(t, http.MethodGet, "/System/Info", primary, targets)
	ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
	defer cancel()
	request = request.WithContext(ctx)
	finished := make(chan error, 1)
	go func() {
		_, err := transport.RoundTrip(request)
		finished <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("primary probe did not start")
	}
	response, err := transport.RoundTrip(failoverRequest(t, http.MethodGet, "/System/Info", primary, targets))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if primaryCalls.Load() != 1 {
		t.Fatalf("concurrent request started %d primary probes", primaryCalls.Load())
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled probe error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled probe did not finish")
	}
	state.mu.Lock()
	stillReserved, failedUntil := state.primaryProbe, state.failedUntil[upstreamTargetKey(primary)]
	state.mu.Unlock()
	if stillReserved || !failedUntil.IsZero() {
		t.Fatalf("cancelled probe reservation/failure = %t/%v", stillReserved, failedUntil)
	}
	next := state.ordered(targets, now.Add(2*failoverCooldown))
	defer state.releaseProbe(next)
	if !next.probe || next.targets[0] != primary {
		t.Fatalf("cancelled probe was not released for a later request: %v", next.targets)
	}
}

func TestFailoverTransportLateBackupCannotUndoPrimaryRecovery(t *testing.T) {
	for _, lateStatus := range []int{http.StatusOK, http.StatusBadGateway} {
		t.Run(http.StatusText(lateStatus), func(t *testing.T) {
			state := newUpstreamFailoverState()
			primary := mustFailoverURL(t, "https://primary.example")
			backup := mustFailoverURL(t, "https://backup.example")
			targets := []*url.URL{primary, backup}
			now := time.Now()
			initial := state.ordered(targets, now)
			state.markFailure(primary, now, &initial)
			state.markSuccess(backup, now, initial)
			lateStarted, probeStarted := make(chan struct{}), make(chan struct{})
			allowLate, allowProbe := make(chan struct{}), make(chan struct{})
			releaseLate := sync.OnceFunc(func() { close(allowLate) })
			releaseProbe := sync.OnceFunc(func() { close(allowProbe) })
			defer releaseLate()
			defer releaseProbe()
			var primaryCalls atomic.Int32
			transport := &failoverTransport{state: state, base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				status := http.StatusOK
				if req.URL.Host == backup.Host && req.URL.Path == "/late" {
					close(lateStarted)
					select {
					case <-allowLate:
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
					status = lateStatus
				}
				if req.URL.Host == primary.Host {
					primaryCalls.Add(1)
					if req.URL.Path == "/probe" {
						close(probeStarted)
						select {
						case <-allowProbe:
						case <-req.Context().Done():
							return nil, req.Context().Err()
						}
					} else if req.URL.Path == "/late" {
						status = http.StatusBadGateway
					}
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: http.NoBody, Request: req}, nil
			})}
			start := func(path string) <-chan error {
				request := failoverRequest(t, http.MethodGet, path, primary, targets)
				ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
				t.Cleanup(cancel)
				request = request.WithContext(ctx)
				finished := make(chan error, 1)
				go func() {
					response, err := transport.RoundTrip(request)
					if response != nil {
						response.Body.Close()
					}
					finished <- err
				}()
				return finished
			}
			wait := func(started <-chan struct{}) {
				t.Helper()
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("request did not reach its selected upstream")
				}
			}
			finish := func(finished <-chan error) {
				t.Helper()
				select {
				case err := <-finished:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("request did not finish")
				}
			}
			lateFinished := start("/late")
			wait(lateStarted)
			// Advance only the primary cooldown while the old backup request is
			// in flight, so the recovery order has no dependency on sleeping.
			state.mu.Lock()
			delete(state.failedUntil, upstreamTargetKey(primary))
			state.primaryProbeAt = now.Add(-time.Second)
			state.mu.Unlock()
			probeFinished := start("/probe")
			wait(probeStarted)
			response, err := transport.RoundTrip(failoverRequest(t, http.MethodGet, "/concurrent", primary, targets))
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			pending := state.ordered(targets, now.Add(2*failoverCooldown))
			if primaryCalls.Load() != 1 || len(pending.targets) != 1 || pending.targets[0] != backup {
				t.Fatalf("concurrent backup success admitted another probe: primary=%d targets=%v", primaryCalls.Load(), pending.targets)
			}
			releaseProbe()
			finish(probeFinished)
			releaseLate()
			finish(lateFinished)
			preferred, _ := state.preferredTarget(targets, time.Now())
			state.mu.Lock()
			primaryFailedUntil := state.failedUntil[upstreamTargetKey(primary)]
			state.mu.Unlock()
			if preferred != primary || !primaryFailedUntil.IsZero() {
				t.Fatalf("late result changed recovered primary: preferred=%v failedUntil=%v", preferred, primaryFailedUntil)
			}
		})
	}
}
