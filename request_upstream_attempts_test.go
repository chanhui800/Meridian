package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newUpstreamAttemptsProxy(t *testing.T, target, playback string) (*App, Site, *httptest.Server) {
	t.Helper()
	app := newTestApp(t)
	site, err := app.db.CreateSiteRecord(Site{
		Name: "hop diagnostics", PublicHost: "hops.example.test", IngressMode: ingressModeHost,
		TargetURL: target, PlaybackTargetURL: playback, PlaybackMode: "redirect",
		MainVideoStreamMode: mainVideoStreamModeProxy, UAMode: passthroughUAMode, StreamHosts: "[]",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.pm.StartSite(*site); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.pm.StopSite(site.ID) })
	app.pm.mu.RLock()
	handler := app.pm.proxies[site.ID].handler
	app.pm.mu.RUnlock()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return app, *site, server
}

func withUpstreamAttemptsTracker(req *http.Request) (*http.Request, *requestDiagnosticsTracker) {
	tracker := newRequestDiagnosticsTracker(time.Now(), req)
	return req.WithContext(context.WithValue(req.Context(), requestDiagnosticsContextKey{}, tracker)), tracker
}

func TestRequestUpstreamAttemptsConfiguredRedirectFinalConnectionAnd103(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Videos/42/stream":
			w.Header().Set("Location", "/middle?token=never-persist")
			w.WriteHeader(http.StatusMovedPermanently)
		case "/middle":
			w.Header().Set("Location", "/final?signature=never-persist")
			w.WriteHeader(http.StatusFound)
		case "/final":
			w.Header().Set("Link", "</style.css>; rel=preload")
			w.WriteHeader(http.StatusEarlyHints)
			time.Sleep(25 * time.Millisecond)
			w.Header().Del("Link")
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Length", "2")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "ok")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	app, site, proxy := newUpstreamAttemptsProxy(t, upstream.URL, upstream.URL)
	var hints atomic.Int64
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{Got1xxResponse: func(int, textproto.MIMEHeader) error {
		hints.Add(1)
		return nil
	}})
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, proxy.URL+"/Videos/42/stream?api_key=never-persist", nil)
	response, err := proxy.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusPartialContent || string(body) != "ok" || hints.Load() != 1 {
		t.Fatalf("configured response status/body/hints=%d/%q/%d err=%v", response.StatusCode, body, hints.Load(), err)
	}
	entry := waitRequestDiagnosticsLog(t, app, site.ID, "/Videos/42/stream")
	if len(entry.UpstreamAttempts) != 3 || entry.RedirectCount == nil || *entry.RedirectCount != 2 || entry.RetryCount == nil || *entry.RetryCount != 0 || entry.ConnectionState != "mixed" || entry.FinalConnectionState != "reused" {
		t.Fatalf("configured redirect attribution=%+v", entry.RequestDiagnostics)
	}
	for index, attempt := range entry.UpstreamAttempts {
		if attempt.AttemptIndex != index+1 || attempt.SourceRole != "configured" || attempt.Authority != upstream.URL || attempt.StartedMS == nil || attempt.FirstByteMS == nil || attempt.ResponseHeadersMS == nil || attempt.DurationMS == nil || attempt.Outcome != "response" {
			t.Fatalf("configured step %d=%+v", index, attempt)
		}
		wantConnection := "reused"
		if index == 0 {
			wantConnection = "new"
		}
		if attempt.ConnectionState != wantConnection || index > 0 && attempt.TCPDurationMS != nil {
			t.Fatalf("step connection phases %d=%+v", index, attempt)
		}
	}
	final := entry.UpstreamAttempts[2]
	if final.ResponseStatusCode != http.StatusPartialContent || *final.ResponseHeadersMS-*final.FirstByteMS < 15 || entry.UpstreamFirstByteMS == nil || *entry.UpstreamFirstByteMS < *final.StartedMS {
		t.Fatalf("1xx wire byte/final headers attribution=%+v aggregate=%+v", final, entry.RequestDiagnostics)
	}
	if encoded := requestDiagnosticsJSON(entry.RequestDiagnostics); strings.Contains(encoded, "never-persist") || strings.Contains(encoded, "/middle") || strings.Contains(encoded, "/final") {
		t.Fatalf("hop diagnostics retained signed URL data: %s", encoded)
	}
}

func TestRequestUpstreamAttemptsConfiguredRedirectFinalFailureClearsFirstByte(t *testing.T) {
	failed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	failedURL := failed.URL
	failed.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", failedURL+"/stream?token=never-persist")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer upstream.Close()
	app, site, proxy := newUpstreamAttemptsProxy(t, upstream.URL, failedURL)
	response, err := proxy.Client().Get(proxy.URL + "/Videos/failure/stream")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	entry := waitRequestDiagnosticsLog(t, app, site.ID, "/Videos/failure/stream")
	if response.StatusCode != http.StatusBadGateway || len(entry.UpstreamAttempts) != 2 || entry.UpstreamFirstByteMS != nil || entry.FinalConnectionState != "" || entry.TerminationReason != "upstream_error" {
		t.Fatalf("final failure inherited a responding redirect: %+v", entry.RequestDiagnostics)
	}
	first, final := entry.UpstreamAttempts[0], entry.UpstreamAttempts[1]
	if first.ResponseStatusCode != http.StatusMovedPermanently || first.FirstByteMS == nil || first.ResponseHeadersMS == nil || final.AttemptIndex != 2 || final.Authority != failedURL || final.FirstByteMS != nil || final.ResponseHeadersMS != nil || final.ResponseStatusCode != 0 || final.ConnectionState != "" || final.TCPDurationMS == nil || final.DurationMS == nil || final.Outcome != "error" {
		t.Fatalf("redirect/final failure steps=%+v", entry.UpstreamAttempts)
	}
}

func TestRequestUpstreamAttemptsDynamicTwoHopsDNSReuseAndIsolation(t *testing.T) {
	var forbiddenHeaders, forwardedHints atomic.Int64
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Close || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Private") != "" || r.Header.Get("Range") != "bytes=0-1" {
			forbiddenHeaders.Add(1)
		}
		if r.URL.Path == "/media" {
			w.Header().Set("Location", "http://second.example.net/final?signature=never-persist")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Link", "<https://private.example.net>; rel=preload")
		w.WriteHeader(http.StatusEarlyHints)
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Length", "2")
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "ok")
	}))
	defer cdn.Close()
	var lookups atomic.Int64
	_, state, dials := dynamicPoolTestState(t, cdn, dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		lookups.Add(1)
		time.Sleep(3 * time.Millisecond)
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
	}))
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://first.example.com/media?token=never-persist")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer origin.Close()
	configured := http.DefaultTransport.(*http.Transport).Clone()
	defer configured.CloseIdleConnections()
	follower := &redirectFollowTransport{
		base: configured, dynamicPolicy: redirectRuntimePolicy(true), dynamicState: state,
		followUnknownRedirects: true,
	}
	for index := range 2 {
		request := redirectRuntimeEligibleRequest(http.MethodGet, origin.URL+"/Videos/42/stream")
		request.Header.Set("Range", "bytes=0-1")
		request.Header.Set("Authorization", "never-persist")
		request.Header.Set("Cookie", "never-persist")
		request.Header.Set("X-Private", "never-persist")
		request, tracker := withUpstreamAttemptsTracker(request)
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{Got1xxResponse: func(int, textproto.MIMEHeader) error {
			forwardedHints.Add(1)
			return nil
		}}))
		response, err := follower.RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		if err := dynamicPoolTestConsume(response); err != nil {
			t.Fatal(err)
		}
		result := tracker.snapshot(time.Now(), response.StatusCode, nil, nil)
		if len(result.UpstreamAttempts) != 3 || result.RedirectCount == nil || *result.RedirectCount != 2 || result.RetryCount == nil || *result.RetryCount != 0 {
			t.Fatalf("dynamic route attribution=%+v", result)
		}
		wantConnection := "new"
		if index == 1 {
			wantConnection = "reused"
		}
		for step, attempt := range result.UpstreamAttempts {
			wantRole := "dynamic"
			if step == 0 {
				wantRole = "configured"
			}
			if attempt.AttemptIndex != step+1 || attempt.SourceRole != wantRole || attempt.ConnectionState != wantConnection || attempt.FirstByteMS == nil || attempt.ResponseHeadersMS == nil || attempt.DurationMS == nil {
				t.Fatalf("dynamic step %d=%+v", step, attempt)
			}
			if step > 0 && attempt.DNSDurationMS == nil || step == 0 && attempt.DNSDurationMS != nil || index == 1 && (attempt.TCPDurationMS != nil || attempt.TLSDurationMS != nil) {
				t.Fatalf("dynamic preflight DNS/connection phases step %d=%+v", step, attempt)
			}
		}
		final := result.UpstreamAttempts[2]
		if result.FinalConnectionState != wantConnection || final.Authority != "http://second.example.net:80" || *final.ResponseHeadersMS-*final.FirstByteMS < 10 {
			t.Fatalf("dynamic final headers attribution=%+v", result)
		}
		if encoded := requestDiagnosticsJSON(result); strings.Contains(encoded, "never-persist") || strings.Contains(encoded, "/media") || strings.Contains(encoded, "/final") {
			t.Fatalf("dynamic diagnostics retained URL/identity: %s", encoded)
		}
	}
	if forbiddenHeaders.Load() != 0 || forwardedHints.Load() != 0 || dials.Load() != 2 || lookups.Load() != 4 {
		t.Fatalf("dynamic security/reuse headers/hints/dials/DNS=%d/%d/%d/%d", forbiddenHeaders.Load(), forwardedHints.Load(), dials.Load(), lookups.Load())
	}
	if _, leases := dynamicPoolTestCounts(state.transports); leases != 0 || len(state.streams) != 0 || len(state.runtime.streams) != 0 {
		t.Fatalf("diagnostic tracing leaked leases/site/global streams=%d/%d/%d", leases, len(state.streams), len(state.runtime.streams))
	}
}

func TestRequestUpstreamAttemptsFailoverKeepsFinalConnectionAndErrorsSeparate(t *testing.T) {
	for _, finalFails := range []bool{false, true} {
		name := "success"
		if finalFails {
			name = "last-failure"
		}
		t.Run(name, func(t *testing.T) {
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer primary.Close()
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "backup")
			}))
			defer backup.Close()
			primaryURL, backupURL := mustFailoverURL(t, primary.URL), mustFailoverURL(t, backup.URL)
			base := http.DefaultTransport.(*http.Transport).Clone()
			defer base.CloseIdleConnections()
			warm, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, primary.URL, nil)
			response, err := base.RoundTrip(warm)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if finalFails {
				backup.Close()
			}
			request, tracker := withUpstreamAttemptsTracker(failoverRequest(t, http.MethodGet, "/System/Info?token=never-persist", primaryURL, []*url.URL{primaryURL, backupURL}))
			transport := &failoverTransport{base: base, state: newUpstreamFailoverState()}
			response, err = transport.RoundTrip(request)
			if response != nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
			result := tracker.snapshot(time.Now(), http.StatusOK, nil, nil)
			if len(result.UpstreamAttempts) != 2 || result.RetryCount == nil || *result.RetryCount != 1 || result.RedirectCount == nil || *result.RedirectCount != 0 || result.UpstreamAttempts[0].ConnectionState != "reused" {
				t.Fatalf("failover route=%+v err=%v", result, err)
			}
			final := result.UpstreamAttempts[1]
			if finalFails {
				if err == nil || result.FinalConnectionState != "" || result.UpstreamFirstByteMS != nil || final.FirstByteMS != nil || final.ResponseHeadersMS != nil || final.ResponseStatusCode != 0 || final.Outcome != "error" {
					t.Fatalf("failed retry inherited a previous response=%+v err=%v", result, err)
				}
			} else if err != nil || result.ConnectionState != "mixed" || result.FinalConnectionState != "new" || final.ResponseStatusCode != http.StatusOK || final.ConnectionState != "new" || final.FirstByteMS == nil || final.ResponseHeadersMS == nil || result.TerminationReason != "completed" {
				t.Fatalf("successful retry attribution=%+v err=%v", result, err)
			}
		})
	}
}

func TestRequestUpstreamAttemptsDynamicFinalFailureDoesNotRetainRedirectTiming(t *testing.T) {
	failed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	failed.Close()
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://second.example.net/final?signature=never-persist")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer cdn.Close()
	_, state, dials := dynamicPoolTestState(t, cdn, nil)
	state.transports.build = func(target *url.URL, pins []net.IP, policy *dynamicSelfTargetPolicy) (*http.Transport, error) {
		server := cdn
		if target.Hostname() == "second.example.net" {
			server = failed
		}
		return dynamicPoolTestBuilder(server, dials)(target, pins, policy)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://first.example.com/media")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer origin.Close()
	configured := http.DefaultTransport.(*http.Transport).Clone()
	defer configured.CloseIdleConnections()
	follower := &redirectFollowTransport{
		base: configured, dynamicPolicy: redirectRuntimePolicy(true), dynamicState: state, followUnknownRedirects: true,
	}
	request, tracker := withUpstreamAttemptsTracker(redirectRuntimeEligibleRequest(http.MethodGet, origin.URL+"/Videos/42/stream"))
	response, err := follower.RoundTrip(request)
	if response != nil || err == nil {
		t.Fatalf("closed final target response=%v err=%v", response, err)
	}
	result := tracker.snapshot(time.Now(), http.StatusBadGateway, nil, nil)
	if len(result.UpstreamAttempts) != 3 || result.UpstreamFirstByteMS != nil || result.FinalConnectionState != "" || result.RedirectCount == nil || *result.RedirectCount != 2 {
		t.Fatalf("dynamic final failure inherited earlier responses: %+v", result)
	}
	final := result.UpstreamAttempts[2]
	if final.AttemptIndex != 3 || final.SourceRole != "dynamic" || final.Authority != "http://second.example.net:80" || final.DNSDurationMS == nil || final.TCPDurationMS == nil || final.FirstByteMS != nil || final.ResponseHeadersMS != nil || final.ResponseStatusCode != 0 || final.Outcome != "error" {
		t.Fatalf("dynamic failed attempt=%+v", final)
	}
	if _, leases := dynamicPoolTestCounts(state.transports); leases != 0 || len(state.streams) != 0 || len(state.runtime.streams) != 0 {
		t.Fatalf("dynamic failure leaked leases/site/global streams=%d/%d/%d", leases, len(state.streams), len(state.runtime.streams))
	}
}

func TestRequestUpstreamAttemptsCapabilityIncludesInitialPinnedDNS(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/media" {
			w.Header().Set("Location", "/final?signature=never-persist")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	_, state, _ := dynamicPoolTestState(t, server, nil)
	issuer := &dynamicCapabilityIssuer{
		key: []byte(strings.Repeat("k", 32)), siteID: 7, policyRevision: 1,
		policy: redirectRuntimePolicy(true), state: state, site: Site{MainVideoStreamMode: mainVideoStreamModeProxy},
	}
	target := dynamicPoolTestTarget(t, "http://cdn.example.com/media?token=never-persist")
	route, discoveryErr := issuer.mint(t.Context(), nil, target, dynamicDiscoverySourceRedirect)
	if discoveryErr != nil {
		t.Fatal(discoveryErr)
	}
	request, tracker := withUpstreamAttemptsTracker(httptest.NewRequest(http.MethodGet, "http://public.example.net"+route, nil))
	writer := httptest.NewRecorder()
	issuer.serve(writer, request)
	result := tracker.snapshot(time.Now(), writer.Code, nil, nil)
	if writer.Code != http.StatusOK || writer.Body.String() != "ok" || len(result.UpstreamAttempts) != 2 || result.RedirectCount == nil || *result.RedirectCount != 1 || result.ConnectionState != "mixed" || result.FinalConnectionState != "reused" {
		t.Fatalf("capability route diagnostics=%+v response=%d/%q", result, writer.Code, writer.Body.String())
	}
	for _, attempt := range result.UpstreamAttempts {
		if attempt.SourceRole != "dynamic" || attempt.DNSDurationMS == nil || attempt.Authority != "http://cdn.example.com:80" || attempt.ResponseHeadersMS == nil {
			t.Fatalf("capability lost its initial/prepared DNS attribution=%+v", result)
		}
	}
}

func TestRequestUpstreamAttemptsPoolTracePreservesEOFAndCanceledLeaseLifecycle(t *testing.T) {
	for _, mode := range []string{"eof", "partial", "canceled", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			var served atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "65536")
				if served.Add(1) == 1 && mode == "truncated" {
					_, _ = io.WriteString(w, "partial")
					return
				}
				_, _ = io.WriteString(w, strings.Repeat("x", 65536))
			}))
			defer server.Close()
			_, state, dials := dynamicPoolTestState(t, server, nil)
			target := dynamicPoolTestTarget(t, "http://cdn.example.com/media")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			firstTracker := newRequestDiagnosticsTracker(time.Now(), nil)
			ctx = context.WithValue(ctx, requestDiagnosticsContextKey{}, firstTracker)
			first, err := dynamicPoolTestFetch(ctx, state, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			observeRequestDiagnosticsResponse(first)
			defer first.Body.Close()
			switch mode {
			case "eof":
				if _, err := io.Copy(io.Discard, first.Body); err != nil {
					t.Fatal(err)
				}
			case "truncated":
				if _, err := io.Copy(io.Discard, first.Body); !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("truncated transfer error=%v", err)
				}
				_ = first.Body.Close()
			default:
				if _, err := io.ReadFull(first.Body, make([]byte, 1)); err != nil {
					t.Fatal(err)
				}
				if mode == "canceled" {
					cancel()
					if _, err := io.Copy(io.Discard, first.Body); !errors.Is(err, context.Canceled) {
						t.Fatalf("canceled transfer error=%v", err)
					}
				}
				_ = first.Body.Close()
			}
			firstResult := firstTracker.snapshot(time.Now(), first.StatusCode, ctx.Err(), nil)
			if len(firstResult.UpstreamAttempts) != 1 || firstResult.FinalConnectionState != "new" || firstResult.UpstreamAttempts[0].ResponseHeadersMS == nil {
				t.Fatalf("tracked first transfer=%+v", firstResult)
			}
			if mode == "canceled" && firstResult.TerminationReason != "client_canceled" || mode == "truncated" && firstResult.TerminationReason != "upstream_error" {
				t.Fatalf("tracked transfer termination=%+v", firstResult)
			}
			for repeat := range 2 {
				tracker := newRequestDiagnosticsTracker(time.Now(), nil)
				response, err := dynamicPoolTestFetch(context.WithValue(t.Context(), requestDiagnosticsContextKey{}, tracker), state, target, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := dynamicPoolTestConsume(response); err != nil {
					t.Fatal(err)
				}
				wantState := "reused"
				if repeat == 0 && mode != "eof" {
					wantState = "new"
				}
				if result := tracker.snapshot(time.Now(), 200, nil, nil); result.FinalConnectionState != wantState {
					t.Fatalf("tracked lease after %s state=%+v want=%s", mode, result, wantState)
				}
				cancel()
				_ = first.Body.Close()
			}
			wantDials := int64(2)
			if mode == "eof" {
				wantDials = 1
			}
			if _, leases := dynamicPoolTestCounts(state.transports); leases != 0 || dials.Load() != wantDials {
				t.Fatalf("tracked %s lifecycle leases/dials=%d/%d want=%d", mode, leases, dials.Load(), wantDials)
			}
		})
	}
}

func TestRequestUpstreamAttemptsConfigured101RetainsDuplexBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		_ = buffered.Flush()
		payload := make([]byte, 4)
		if _, err := io.ReadFull(buffered, payload); err == nil {
			_, _ = conn.Write(payload)
		}
	}))
	defer server.Close()
	request, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/upgrade", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "echo")
	request, tracker := withUpstreamAttemptsTracker(request)
	base := http.DefaultTransport.(*http.Transport).Clone()
	defer base.CloseIdleConnections()
	response, err := roundTripWithRequestDiagnostics(base, request, "configured")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body := response.Body
	observeRequestDiagnosticsResponse(response)
	duplex, ok := response.Body.(io.ReadWriteCloser)
	if !ok || response.Body != body {
		t.Fatal("diagnostic response observer replaced the upgraded duplex body")
	}
	if _, err := duplex.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 4)
	if _, err := io.ReadFull(duplex, payload); err != nil || string(payload) != "ping" {
		t.Fatalf("duplex response=%q err=%v", payload, err)
	}
	result := tracker.snapshot(time.Now(), http.StatusSwitchingProtocols, nil, nil)
	if len(result.UpstreamAttempts) != 1 || result.FinalConnectionState != "new" || result.UpstreamAttempts[0].ResponseStatusCode != 101 || result.UpstreamAttempts[0].FirstByteMS == nil || result.UpstreamAttempts[0].ResponseHeadersMS == nil {
		t.Fatalf("upgraded attempt diagnostics=%+v", result)
	}
}

func TestRequestUpstreamAttemptsUpstreamAckSurvivesCanceledClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/Sessions/Playing/Stopped", strings.NewReader(`{"PositionTicks":42}`))
	request, tracker := withUpstreamAttemptsTracker(request)
	response, err := roundTripWithRequestDiagnostics(http.DefaultTransport, request, "configured")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	cancel()
	// The client has received no recorded response headers, yet the upstream
	// acknowledgement was observed before cancellation and remains authoritative.
	result := tracker.snapshot(time.Now(), 0, ctx.Err(), nil)
	if result.ResponseStatusCode != 0 || result.TerminationReason != "client_canceled" || tracker.upstreamResponseStatusCode() != http.StatusNoContent {
		t.Fatalf("upstream acknowledgement was confused with client delivery: %+v", result)
	}
	if newRequestDiagnosticsTracker(time.Now(), nil).upstreamResponseStatusCode() != 0 {
		t.Fatal("a local response invented an upstream acknowledgement")
	}
}

func TestRequestUpstreamAttemptsFreshTraceBoundAndConcurrentPhases(t *testing.T) {
	request, tracker := withUpstreamAttemptsTracker(httptest.NewRequest(http.MethodGet, "http://origin.example.test/video?token=never-persist", nil))
	base := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(req.Context())
		trace.GotConn(httptrace.GotConnInfo{Reused: true})
		trace.GotConn(httptrace.GotConnInfo{Reused: false})
		var workers sync.WaitGroup
		for worker := range 8 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				address := string(rune('a'+worker)) + ":80"
				trace.ConnectStart("tcp", address)
				trace.ConnectDone("tcp", address, nil)
				_ = tracker.snapshot(time.Now(), 200, nil, nil)
			}()
		}
		workers.Wait()
		trace.GotFirstResponseByte()
		trace.GotFirstResponseByte()
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}, nil
	})
	for range 12 {
		response, err := roundTripWithRequestDiagnostics(base, request, "configured")
		if err != nil {
			t.Fatal(err)
		}
		// Reuse the previous traced context, as a redirect/failover clone can.
		request = response.Request.Clone(response.Request.Context())
	}
	result := tracker.snapshot(time.Now(), 200, nil, nil)
	if len(result.UpstreamAttempts) != requestDiagnosticsMaxAttempts || !result.UpstreamAttemptsTruncated || result.UpstreamAttempts[7].AttemptIndex != 12 || result.FinalConnectionState != "mixed" {
		t.Fatalf("bounded physical attempts=%+v", result)
	}
	for index, attempt := range result.UpstreamAttempts[:7] {
		if attempt.AttemptIndex != index+1 || attempt.ConnectionState != "mixed" || attempt.TCPDurationMS == nil {
			t.Fatalf("trace callbacks were composed across attempts: %+v", result.UpstreamAttempts)
		}
	}
	if !validRequestDiagnosticsWire(result) {
		t.Fatalf("valid bounded snapshot failed wire validation: %s", requestDiagnosticsJSON(result))
	}
	if _, err := roundTripWithRequestDiagnostics(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, context.Canceled
	}), request, "configured"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	result = tracker.snapshot(time.Now(), 0, context.Canceled, nil)
	final := result.UpstreamAttempts[7]
	if final.AttemptIndex != 13 || final.Outcome != "canceled" || final.FirstByteMS != nil || final.ResponseHeadersMS != nil || final.ConnectionState != "" || result.UpstreamFirstByteMS != nil || result.FinalConnectionState != "" || tracker.upstreamResponseStatusCode() != 0 {
		t.Fatalf("canceled final attempt retained earlier trace observations=%+v", result)
	}
}

func TestRequestUpstreamAttemptsSanitationWireBoundsAndPrivacyMasks(t *testing.T) {
	maximum := requestDiagnosticsMaxDurationMS
	count := requestDiagnosticsMaxAttemptIndex
	bytesWritten := int64(1<<63 - 1)
	longHost := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	value := RequestDiagnostics{
		DiagnosticsVersion: 1, ResponseStatusCode: 206, TerminationReason: "client_canceled",
		DurationMS: &maximum, UpstreamFirstByteMS: &maximum, BodyFirstByteMS: &maximum, BodyBytesWritten: &bytesWritten,
		RangeMode: 2, ConnectionState: "mixed", FinalConnectionState: "reused", RedirectCount: &count, RetryCount: &count,
		DNSDurationMS: &maximum, TCPDurationMS: &maximum, TLSDurationMS: &maximum,
		CacheLookup: "read_error", CacheWrite: "not_attempted", CacheLookupReason: "invalid_entry", CacheWriteReason: "response_status", CacheReason: "response_status", ResponseType: "manifest",
	}
	for index := range requestDiagnosticsMaxAttempts {
		value.UpstreamAttempts = append(value.UpstreamAttempts, RequestUpstreamAttempt{
			AttemptIndex: index + 1, SourceRole: "configured", Authority: "https://" + longHost + ":65535",
			StartedMS: &maximum, FirstByteMS: &maximum, ResponseHeadersMS: &maximum, DurationMS: &maximum,
			ResponseStatusCode: 206, ConnectionState: "reused", Outcome: "response",
			DNSDurationMS: &maximum, TCPDurationMS: &maximum, TLSDurationMS: &maximum,
		})
	}
	encoded := requestDiagnosticsJSON(value)
	if encoded == "" || len(encoded) > requestDiagnosticsMaxJSONBytes {
		t.Fatalf("bounded diagnostics were dropped or exceeded JSON cap: %d bytes", len(encoded))
	}
	parsed := parseRequestDiagnosticsJSON(encoded)
	if len(parsed.UpstreamAttempts) == 0 || parsed.UpstreamAttempts[len(parsed.UpstreamAttempts)-1].AttemptIndex != 8 || parsed.RedirectCount == nil || *parsed.RedirectCount != count || !validRequestDiagnosticsWire(parsed) {
		t.Fatalf("payload cap lost the final result/counts=%+v", parsed)
	}
	if len(parsed.UpstreamAttempts) < 8 && !parsed.UpstreamAttemptsTruncated {
		t.Fatal("payload cap did not disclose omitted steps")
	}
	wireValue := value
	wireValue.UpstreamAttempts = append([]RequestUpstreamAttempt(nil), value.UpstreamAttempts...)
	for index := range wireValue.UpstreamAttempts {
		wireValue.UpstreamAttempts[index].Authority = "https://cdn.example.net:443"
	}
	if !validRequestDiagnosticsWire(wireValue) {
		t.Fatal("valid eight-step payload failed wire validation")
	}
	for _, test := range []struct {
		name   string
		mutate func(*RequestDiagnostics)
	}{
		{"too-many", func(v *RequestDiagnostics) { v.UpstreamAttempts = append(v.UpstreamAttempts, v.UpstreamAttempts[0]) }},
		{"bad-index", func(v *RequestDiagnostics) { v.UpstreamAttempts[0].AttemptIndex = 0 }},
		{"unsorted", func(v *RequestDiagnostics) { v.UpstreamAttempts[1].AttemptIndex = 1 }},
		{"authority-path", func(v *RequestDiagnostics) {
			v.UpstreamAttempts[0].Authority = "https://cdn.example.net/private?token=secret"
		}},
		{"authority-user", func(v *RequestDiagnostics) {
			v.UpstreamAttempts[0].Authority = "https://user:secret@cdn.example.net:443"
		}},
		{"authority-large", func(v *RequestDiagnostics) {
			v.UpstreamAttempts[0].Authority = strings.Repeat("a", requestDiagnosticsMaxAuthorityBytes+1)
		}},
		{"role", func(v *RequestDiagnostics) { v.UpstreamAttempts[0].SourceRole = "token=secret" }},
		{"outcome", func(v *RequestDiagnostics) { v.UpstreamAttempts[0].Outcome = "token=secret" }},
		{"connection", func(v *RequestDiagnostics) { v.FinalConnectionState = "token=secret" }},
		{"negative-time", func(v *RequestDiagnostics) { bad := int64(-1); v.UpstreamAttempts[0].FirstByteMS = &bad }},
		{"large-time", func(v *RequestDiagnostics) { bad := maximum + 1; v.UpstreamAttempts[0].DNSDurationMS = &bad }},
		{"late-first-byte", func(v *RequestDiagnostics) { short := int64(1); v.UpstreamAttempts[0].DurationMS = &short }},
		{"negative-count", func(v *RequestDiagnostics) { bad := -1; v.RetryCount = &bad }},
		{"status", func(v *RequestDiagnostics) { v.UpstreamAttempts[0].ResponseStatusCode = 999 }},
		{"version", func(v *RequestDiagnostics) { v.DiagnosticsVersion = 2 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			bad := wireValue
			bad.UpstreamAttempts = append([]RequestUpstreamAttempt(nil), wireValue.UpstreamAttempts...)
			test.mutate(&bad)
			if validRequestDiagnosticsWire(bad) {
				t.Fatalf("invalid diagnostic wire accepted: %+v", bad)
			}
			if sanitized := requestDiagnosticsJSON(bad); strings.Contains(sanitized, "secret") || len(sanitized) > requestDiagnosticsMaxJSONBytes {
				t.Fatalf("sanitation retained a sensitive/oversized payload: %s", sanitized)
			}
		})
	}
	legacy := RequestDiagnostics{DurationMS: &maximum, ResponseStatusCode: 206}
	if !validRequestDiagnosticsWire(legacy) || requestDiagnosticsJSON(legacy) != "" {
		t.Fatal("legacy optional diagnostics rejected a report or invented telemetry")
	}
	for _, mask := range []struct {
		name                                string
		status, timeline, category, backend bool
	}{
		{"backend", true, true, true, false},
		{"timing", true, false, true, true},
		{"status", false, true, true, true},
		{"category", true, true, false, true},
		{"all", false, false, false, false},
	} {
		t.Run("mask-"+mask.name, func(t *testing.T) {
			masked := filterRequestDiagnosticsFields(value, mask.status, mask.timeline, mask.category, mask.backend)
			if !mask.backend && (masked.FinalConnectionState != "" || masked.RedirectCount != nil || masked.RetryCount != nil || masked.UpstreamAttemptsTruncated) {
				t.Fatalf("backend topology was retained: %+v", masked)
			}
			if mask.name == "all" {
				if encoded := requestDiagnosticsJSON(masked); encoded != `{"diagnostics_version":1}` {
					t.Fatalf("disabled fields retained diagnostics: %s", encoded)
				}
				return
			}
			for _, attempt := range masked.UpstreamAttempts {
				if !mask.backend && (attempt.Authority != "" || attempt.ConnectionState != "" || attempt.DNSDurationMS != nil || attempt.TCPDurationMS != nil || attempt.TLSDurationMS != nil) ||
					!mask.timeline && (attempt.StartedMS != nil || attempt.FirstByteMS != nil || attempt.ResponseHeadersMS != nil || attempt.DurationMS != nil || attempt.DNSDurationMS != nil || attempt.TCPDurationMS != nil || attempt.TLSDurationMS != nil) ||
					!mask.status && (attempt.ResponseStatusCode != 0 || attempt.Outcome != "") || !mask.category && attempt.SourceRole != "" {
					t.Fatalf("hop field mask leaked related observations: %+v", attempt)
				}
			}
		})
	}
	if encoded, _ := json.Marshal(value); strings.Contains(string(encoded), "secret") {
		t.Fatal("masking mutated the source snapshot")
	}
}

func TestRequestUpstreamAttemptsPersistenceKeepsWriteAndReadMasksIndependent(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSiteRecord(Site{Name: "hop field privacy", PublicHost: "hop-privacy.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
	if err != nil {
		t.Fatal(err)
	}
	zero, one := int64(0), 1
	value := RequestDiagnostics{
		DiagnosticsVersion: 1, ResponseStatusCode: 206, TerminationReason: "client_canceled", DurationMS: &zero,
		ConnectionState: "mixed", FinalConnectionState: "reused", RedirectCount: &one, RetryCount: &one,
		UpstreamAttempts: []RequestUpstreamAttempt{{AttemptIndex: 2, SourceRole: "dynamic", Authority: "https://cdn.example.net:443", StartedMS: &zero, FirstByteMS: &zero, ResponseHeadersMS: &zero, DurationMS: &zero, DNSDurationMS: &zero, TCPDurationMS: &zero, TLSDurationMS: &zero, ConnectionState: "reused", ResponseStatusCode: 206, Outcome: "response"}},
	}
	event := requestLogEvent{RequestDiagnostics: value, SiteID: site.ID, SiteName: site.Name, ResourceCategory: requestLogCategoryStream, StatusCode: 499, Method: http.MethodGet, Path: "/stored"}
	app.db.EnqueueRequestLog(event)
	if logs, err := app.db.ListRequestLogs(RequestLogFilter{}); err != nil || len(logs) != 1 || len(logs[0].UpstreamAttempts) != 1 || logs[0].FinalConnectionState != "reused" {
		t.Fatalf("persisted hop diagnostics=%+v err=%v", logs, err)
	}
	settings := app.db.currentSystemSettings()
	settings.LogDisplayBackendAddress = false
	if err := app.db.saveSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	logs, err := app.db.ListRequestLogs(RequestLogFilter{})
	if err != nil || len(logs) != 1 || logs[0].FinalConnectionState != "" || logs[0].RedirectCount != nil || logs[0].RetryCount != nil || logs[0].UpstreamAttempts[0].Authority != "" || logs[0].UpstreamAttempts[0].ConnectionState != "" || logs[0].UpstreamAttempts[0].DNSDurationMS != nil || logs[0].UpstreamAttempts[0].FirstByteMS == nil {
		t.Fatalf("read mask=%+v err=%v", logs, err)
	}
	var stored string
	if err := app.db.db.QueryRow("SELECT diagnostics_json FROM request_logs WHERE path='/stored'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if persisted := parseRequestDiagnosticsJSON(stored); len(persisted.UpstreamAttempts) != 1 || persisted.UpstreamAttempts[0].Authority == "" || persisted.FinalConnectionState != "reused" {
		t.Fatal("read mask changed the persisted hop observations")
	}
	settings.LogDisplayBackendAddress = true
	settings.LogWriteBackendAddress = false
	if err := app.db.saveSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	event.Path = "/write-masked"
	app.db.EnqueueRequestLog(event)
	if _, err := app.db.ListRequestLogs(RequestLogFilter{}); err != nil {
		t.Fatal(err)
	}
	if err := app.db.db.QueryRow("SELECT diagnostics_json FROM request_logs WHERE path='/write-masked'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"authority", "connection_state", "redirect_count", "retry_count", "dns_duration_ms", "tcp_duration_ms", "tls_duration_ms"} {
		if strings.Contains(stored, field) {
			t.Fatalf("write mask persisted %s: %s", field, stored)
		}
	}
	if persisted := parseRequestDiagnosticsJSON(stored); len(persisted.UpstreamAttempts) != 1 || persisted.UpstreamAttempts[0].ResponseStatusCode != 206 || persisted.UpstreamAttempts[0].FirstByteMS == nil {
		t.Fatalf("write mask lost unrelated observations: %+v", persisted)
	}
	settings.LogWriteStatus, settings.LogWriteTimeline, settings.LogWriteCategory = false, false, false
	if err := app.db.saveSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	event.Path = "/all-masked"
	app.db.EnqueueRequestLog(event)
	if _, err := app.db.ListRequestLogs(RequestLogFilter{}); err != nil {
		t.Fatal(err)
	}
	if err := app.db.db.QueryRow("SELECT diagnostics_json FROM request_logs WHERE path='/all-masked'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != `{"diagnostics_version":1}` {
		t.Fatalf("all disabled fields persisted a hop record: %s", stored)
	}
}
