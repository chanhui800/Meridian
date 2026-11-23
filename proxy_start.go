package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

func (pm *ProxyManager) StartSite(site Site) error {
	pm.lifecycleMu.Lock()
	defer pm.lifecycleMu.Unlock()
	if pm.shutdownStarted.Load() {
		return errProxyManagerShuttingDown
	}
	publicHost, err := normalizePublicHost(site.PublicHost)
	if err != nil {
		return fmt.Errorf("invalid public host: %w", err)
	}
	site.PublicHost = publicHost
	site.PathPrefix, err = normalizePathPrefix(site.PathPrefix)
	if err != nil {
		return fmt.Errorf("invalid path prefix: %w", err)
	}
	site.IngressMode, err = normalizeIngressMode(site.IngressMode, site.PublicHost)
	if err != nil {
		return fmt.Errorf("invalid ingress configuration: %w", err)
	}
	if site.IngressMode == ingressModeUnset {
		return errUnsetIngress
	}
	if site.IngressMode == ingressModePath && site.PathPrefix == "" {
		return fmt.Errorf("invalid ingress configuration: path_prefix is required when ingress_mode is path")
	}
	upstreamMode, err := normalizeUpstreamMode(site.UpstreamMode)
	if err != nil {
		return fmt.Errorf("invalid upstream mode: %w", err)
	}
	pathTargetMode := upstreamMode == upstreamModePathTarget
	if pathTargetMode && site.IngressMode != ingressModeHost && site.IngressMode != ingressModePort {
		return fmt.Errorf("path-target upstream mode requires host or port ingress")
	}
	if pathTargetMode && (strings.TrimSpace(site.TargetURL) != "" || strings.TrimSpace(site.PlaybackTargetURL) != "" || site.AssetCacheEnabled) {
		return fmt.Errorf("path-target upstream mode cannot use fixed upstream settings")
	}
	if pathTargetMode {
		site.UpstreamMode = upstreamModePathTarget
		site.AssetCacheEnabled = false
		site.AccountRetentionDays = 0
		site.MainVideoStreamMode = mainVideoStreamModeProxy
	} else {
		site.UpstreamMode = ""
	}
	if err := pm.validateIngressSafety(site.IngressMode); err != nil {
		return err
	}
	var target *url.URL
	var failoverTargets []*url.URL
	var playbackTarget *url.URL
	var playbackHostsSet map[string]bool
	if !pathTargetMode {
		target, err = normalizeTargetURL(site.TargetURL)
		if err != nil {
			return fmt.Errorf("invalid target URL: %w", err)
		}
		failoverTargets, err = parseFailoverTargets(site.TargetURL, site.FailoverTargetList)
		if err != nil {
			return fmt.Errorf("invalid failover targets: %w", err)
		}
		playbackTarget, playbackHostsSet, err = resolvePlaybackConfiguration(site.PlaybackTargetURL, site.StreamHosts)
		if err != nil {
			return err
		}
	} else {
		if len(site.FailoverTargetList) != 0 || strings.TrimSpace(site.StoredFailoverLines) != "" && strings.TrimSpace(site.StoredFailoverLines) != "[]" ||
			strings.TrimSpace(site.StreamHosts) != "" && strings.TrimSpace(site.StreamHosts) != "[]" || site.StoredUpstreamHeaders != "" && site.StoredUpstreamHeaders != "[]" {
			return fmt.Errorf("path-target upstream mode cannot use fixed upstream settings")
		}
		playbackHostsSet = make(map[string]bool)
	}

	policy, err := resolveUAHeaderPolicy(site)
	if err != nil {
		return fmt.Errorf("invalid UA profile: %w", err)
	}
	configuredHeaders := upstreamHeaderPolicy{}
	if pathTargetMode {
		configuredHeaders = upstreamHeaderPolicy{}
	} else if site.RuntimeUpstreamHeaders != nil {
		configuredHeaders = upstreamHeaderPolicy{authority: redirectHostKey(target), values: make(http.Header, len(site.RuntimeUpstreamHeaders))}
		for name, values := range site.RuntimeUpstreamHeaders {
			configuredHeaders.values[name] = append([]string(nil), values...)
		}
	} else {
		configuredHeaders, err = resolveUpstreamHeaderPolicy(site.StoredUpstreamHeaders, pm.upstreamHeaderKey, target)
		if err != nil {
			return fmt.Errorf("invalid upstream headers: %w", err)
		}
	}
	if site.AssetCacheEnabled {
		// Each running site has an immutable cache generation until its configuration is reloaded.
		site.AssetCacheNamespace = assetCacheNamespace(site)
	}
	redirectPolicy := dynamicRedirectPolicy{}
	if !pathTargetMode {
		redirectPolicy, err = newDynamicRedirectPolicy(pm.DynamicDiscoveryAvailable())
		if err != nil {
			return err
		}
	}
	configuredAuthorities := make(map[string]bool, len(playbackHostsSet)+1)
	if target != nil {
		configuredAuthorities[redirectHostKey(target)] = true
	}
	if len(failoverTargets) > 1 {
		for _, failoverTarget := range failoverTargets[1:] {
			configuredAuthorities[redirectHostKey(failoverTarget)] = true
		}
	}
	for authority := range playbackHostsSet {
		configuredAuthorities[authority] = true
	}
	var dynamicState *dynamicSiteState
	if pathTargetMode {
		// Structured media rewriting still needs bounded per-site parse memory,
		// but this mode must never mint dynamic capabilities or validate targets
		// against the fixed-upstream discovery policy.
		dynamicState = newDynamicSiteState(pm.dynamicRuntime, dynamicDefaultProfileLimits())
	} else if redirectPolicy.configured {
		dynamicState = newDynamicSiteState(pm.dynamicRuntime, redirectPolicy.limits)
	}
	var dynamicIssuer *dynamicCapabilityIssuer
	if redirectPolicy.configured {
		dynamicIssuer = &dynamicCapabilityIssuer{
			key:                   append([]byte(nil), pm.dynamicRouteKey...),
			siteID:                site.ID,
			policyRevision:        site.DynamicPolicyRevision,
			policy:                redirectPolicy,
			state:                 dynamicState,
			database:              pm.database,
			transportFactory:      pm.dynamicTransportFactory,
			configuredAuthorities: configuredAuthorities,
			primaryAuthority:      redirectHostKey(target),
			site:                  site,
			trustedProxies:        append([]*net.IPNet(nil), pm.trustedProxies...),
			uaPolicy:              policy,
			upstreamHeaderPolicy:  configuredHeaders,
			pathPrefix:            site.PathPrefix,
		}
	}
	instanceCtx, instanceCancel := context.WithCancel(context.Background())
	inst := &ProxyInstance{
		Site:           site,
		startedAt:      time.Now(),
		ctx:            instanceCtx,
		cancel:         instanceCancel,
		hijackedConns:  make(map[net.Conn]struct{}),
		trustedProxies: append([]*net.IPNet(nil), pm.trustedProxies...),
		dynamicState:   dynamicState,
		failoverState:  newUpstreamFailoverState(),
		trafficCounter: site.runtimeTrafficCounter,
	}
	installed := false
	defer func() {
		if !installed {
			instanceCancel()
			if dynamicState != nil {
				dynamicState.close()
			}
		}
	}()
	inst.persistedTraffic.Store(site.TrafficUsed)
	inst.persistedBytesIn.Store(site.TrafficUsedIn)
	inst.persistedBytesOut.Store(site.TrafficUsedOut)
	if site.runtimeTrafficCycleConfigured {
		inst.trafficCycleUsage = site.runtimeTrafficCycleUsage
		inst.trafficCycleMode = site.runtimeTrafficBillingMode
		if site.runtimeTrafficCycleStartMS > 0 {
			inst.trafficCycleStart = time.UnixMilli(site.runtimeTrafficCycleStartMS)
		}
		inst.trafficAckedCumulativeIn = site.runtimeTrafficAckedCumulativeIn
		inst.trafficAckedCumulativeOut = site.runtimeTrafficAckedCumulativeOut
		inst.trafficCycleAuthoritative = true
	}
	if inst.persistedBytesIn.Load() == 0 && inst.persistedBytesOut.Load() == 0 && site.TrafficUsed > 0 {
		legacyIn, legacyOut := legacyTrafficDirections(site.TrafficUsed)
		inst.persistedBytesIn.Store(legacyIn)
		inst.persistedBytesOut.Store(legacyOut)
	}

	isRedirectMode := playbackTarget != nil && site.PlaybackMode == "redirect"
	routedUpstream := func(r *http.Request) *url.URL {
		if pathTargetMode {
			if r == nil {
				return nil
			}
			upstream, _ := r.Context().Value(pathTargetContextKey{}).(*url.URL)
			return upstream
		}
		if isRedirectMode {
			return target
		}
		return upstreamTargetForRequest(r, target, playbackTarget)
	}
	allowsFailover := func(r *http.Request, upstream *url.URL) bool {
		if len(failoverTargets) < 2 || !sameConfiguredUpstreamURL(upstream, target) || len(configuredHeaders.values) != 0 ||
			mainVideoDirectFallbackTarget(r.Context()) != nil {
			return false
		}
		if !isRedirectMode {
			return true
		}
		// Legacy media still uses the configured main line to discover its
		// playback destination. Ordinary APIs, including replayable PlaybackInfo,
		// can use the backup lines.
		path := r.URL.Path
		source := dynamicStructuredRequestSource(path)
		if isPlaybackRequest(path) || isPlaybackRedirectEndpoint(path) ||
			source == dynamicDiscoverySourceHLS || source == dynamicDiscoverySourceDASH || isMainVideoStreamRequest(r) {
			return false
		}
		return dynamicState == nil || !dynamicState.hasLearnedPlaybackPath(path, time.Now())
	}
	proxyTransport := http.DefaultTransport.(*http.Transport).Clone()
	proxyTransport.TLSClientConfig = secureTLSConfig("")
	proxyTransport.ResponseHeaderTimeout = 30 * time.Second
	proxyTransport.MaxIdleConnsPerHost = 32
	inst.transport = proxyTransport
	configuredTransport := &requestDiagnosticsTransport{base: proxyTransport, sourceRole: "configured"}
	if dynamicIssuer != nil {
		dynamicIssuer.configuredTransport = configuredTransport
	}

	proxy := &httputil.ReverseProxy{
		Transport:  configuredTransport,
		BufferPool: sharedProxyBufferPool,
		Rewrite: func(proxyReq *httputil.ProxyRequest) {
			ingressPrefix, _ := proxyReq.In.Context().Value(pathIngressContextKey{}).(string)
			if redirectPolicy.configured {
				if eligible := isDynamicRedirectEligibleRequestForState(proxyReq.In, dynamicState); eligible {
					ctx := context.WithValue(proxyReq.Out.Context(), dynamicRequestEligibleContextKey{}, true)
					proxyReq.Out = proxyReq.Out.WithContext(ctx)
				}
			}
			upstream := routedUpstream(proxyReq.In)
			proxyReq.Out = proxyReq.Out.WithContext(context.WithValue(proxyReq.Out.Context(), configuredUpstreamContextKey{}, upstream))
			if tracker := backendAddressTrackerFromContext(proxyReq.Out.Context()); tracker != nil {
				tracker.SetURL(upstream)
			}
			originalURL := *proxyReq.Out.URL
			applyUpstreamURL(proxyReq.Out.URL, upstream)
			proxyReq.Out.Host = upstream.Host
			prepareUpstreamHeadersWithClientIPMode(proxyReq.Out.Header, proxyReq.In, policy, site.ClientIPMode, inst.trustedProxies)
			if !pathTargetMode && !sameRedirectAuthority(target, upstream) {
				// Direct playback can target a separate CDN. Treat that authority like
				// a cross-origin redirect and rebuild from a narrow allowlist so client
				// cookies, bearer tokens, and arbitrary secret headers cannot follow it.
				proxyReq.Out.Header = crossAuthorityRedirectHeadersWithClientIPMode(proxyReq.Out.Header, site.ClientIPMode)
			}
			applySiteForwardedHost(proxyReq.Out.Header, proxyReq.In, site)
			configuredHeaders.apply(proxyReq.Out.Header, upstream)
			if proxyReq.In != nil && proxyReq.In.URL != nil && isMediaLibraryCountsPath(proxyReq.In.URL.Path) {
				// Keep the small count response directly inspectable without ever
				// storing or replaying the client's authentication material.
				proxyReq.Out.Header.Set("Accept-Encoding", "identity")
			}
			if proxyReq.In != nil && isWatchHistoryMetadataRequest(proxyReq.In) {
				// Metadata observation only accepts identity-encoded JSON; preserve
				// the original response while making the bounded observer reliable.
				proxyReq.Out.Header.Set("Accept-Encoding", "identity")
			}
			if allowsFailover(proxyReq.In, upstream) {
				proxyReq.Out = withFailoverRequestPlan(proxyReq.Out, &originalURL, failoverTargets, target, redirectPolicy.limits.MaxBodyBytes)
			}
			if source := dynamicStructuredRequestIdentity(proxyReq.In); source != "" {
				ctx := context.WithValue(proxyReq.Out.Context(), dynamicExpectedStructuredSourceContextKey{}, source)
				if source == dynamicDiscoverySourcePlaybackInfo && proxyReq.In != nil && proxyReq.In.URL != nil {
					clientBase := *proxyReq.In.URL
					if ingressPrefix != "" {
						clientBase.Path = addIngressPathPrefix(clientBase.Path, ingressPrefix)
						clientBase.RawPath = ""
					}
					clientBase.Scheme = ""
					clientBase.Host = ""
					clientBase.User = nil
					clientBase.RawQuery = ""
					clientBase.ForceQuery = false
					clientBase.Fragment = ""
					clientBase.RawFragment = ""
					ctx = context.WithValue(ctx, dynamicPlaybackInfoBaseContextKey{}, &clientBase)
				}
				proxyReq.Out = proxyReq.Out.WithContext(ctx)
				if redirectPolicy.sourceEnabled(source) {
					proxyReq.Out.Header.Set("Accept-Encoding", "identity")
					for _, name := range []string{"Range", "If-Range", "If-Modified-Since", "If-None-Match"} {
						proxyReq.Out.Header.Del(name)
					}
				}
			}
			proxyReq.Out = ownConfiguredRequestDiagnosticsContext(proxyReq.Out)
		},
		ModifyResponse: func(resp *http.Response) error {
			observeRequestDiagnosticsResponse(resp)
			if err := captureMediaLibraryCounts(resp, pm.database, site.ID); err != nil {
				return err
			}
			if site.WatchHistoryEnabled {
				if err := captureWatchHistoryMetadata(resp, pm.database, site.ID); err != nil {
					return err
				}
			}
			dynamicResponse := responseIsDynamic(resp)
			expectedSource := dynamicResponseExpectedStructuredSource(resp)
			rewriteRelative := ingressUsesPath(site.IngressMode) && dynamicIssuer != nil
			if err := rewriteDynamicStructuredResponseExpected(resp, dynamicIssuer, dynamicResponse || rewriteRelative, expectedSource, 0, false); err != nil {
				rollbackDynamicResponseAuthorities(resp)
				return err
			}
			if dynamicResponse && resp.StatusCode >= http.StatusBadRequest {
				sanitizeDynamicResourceErrorResponse(resp)
			}
			if resp.StatusCode < http.StatusBadRequest {
				commitDynamicResponseAuthorities(resp)
			} else {
				rollbackDynamicResponseAuthorities(resp)
			}
			if dynamicResponse {
				rebuildDynamicResponseHeaders(resp)
			}
			stripPanelSessionSetCookies(resp.Header)
			if pathTargetMode {
				if err := rewritePathTargetStructuredResponse(resp, dynamicState); err != nil {
					log.Printf("[%s] path-target media rewrite unavailable", site.Name)
				}
				rewritePathTargetResponse(resp)
				return nil
			}
			upstreamBasePath := target.Path
			if resp.Request != nil {
				if upstream, _ := resp.Request.Context().Value(configuredUpstreamContextKey{}).(*url.URL); upstream != nil {
					upstreamBasePath = upstream.Path
				}
			}
			prefixPathIngressResponse(resp, site.PathPrefix, upstreamBasePath)
			if dynamicResponse {
				return nil
			}
			if err := prepareAssetCacheResponse(resp, pm.assetCache, site); err != nil {
				return err
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if !isClientRequestCancellation(originalRequestContext(r.Context()), err) {
				requestDiagnosticsFromContext(r.Context()).markUpstreamError()
			}
			var discoveryErr *dynamicProxyError
			if errors.As(err, &discoveryErr) {
				log.Printf("[%s] dynamic discovery denied: %s", site.Name, discoveryErr.reasonCode)
				discoveryErr.writeResponse(w)
				return
			}
			if isClientRequestCancellation(originalRequestContext(r.Context()), err) {
				return
			}
			if pathTargetMode {
				// Transport errors can embed the full target URL, including the
				// query string. Keep dynamic-target failures free of request data.
				log.Printf("[%s] request-path upstream unavailable", site.Name)
			} else {
				log.Printf("[%s] proxy error: %v", site.Name, err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"upstream unavailable"}`))
		},
	}
	if isRedirectMode || redirectPolicy.configured {
		proxy.Transport = &redirectFollowTransport{
			base:                     proxy.Transport,
			playbackHosts:            playbackHostsSet,
			configuredPlaybackTarget: playbackTarget,
			configuredUpstreams:      failoverTargets,
			configuredAuthorities:    configuredAuthorities,
			disableLegacyRedirects:   !isRedirectMode,
			followUnknownRedirects:   redirectPolicy.configured,
			policy:                   policy,
			clientIPMode:             site.ClientIPMode,
			upstreamHeaderPolicy:     configuredHeaders,
			dynamicPolicy:            redirectPolicy,
			dynamicTransportFactory:  pm.dynamicTransportFactory,
			dynamicState:             dynamicState,
			capabilityIssuer:         dynamicIssuer,
			mainVideoDirect:          site.MainVideoStreamMode == mainVideoStreamModeDirect,
			database:                 pm.database,
			siteID:                   site.ID,
		}
	}
	if len(failoverTargets) > 1 {
		proxy.Transport = &failoverTransport{base: proxy.Transport, state: inst.failoverState}
	}

	// Speed limit in bytes/sec (field is in Mbps, 0 = unlimited)
	speedLimitBytes := int64(site.SpeedLimit) * 125000 // Mbps -> bytes/sec

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every ingress, including dedicated ports and WebSocket upgrades, shares
		// this admission boundary. Reject before parsing bodies, recording watch
		// activity or contacting upstream; admitted streams retain their context.
		if pm.database.rejectControllerNodeAdmission(w, site.ID) {
			return
		}
		requestController, accepted := inst.beginHTTPRequest(w)
		if !accepted {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"site is stopping"}`))
			return
		}
		defer inst.endHTTPRequest(requestController)
		normalizeEmbeddedDynamicCapabilityRequestPath(r.URL)
		if site.PathPrefix != "" && !isReservedDynamicRoute(r.URL.Path) {
			stripIngressPathPrefix(r.URL, site.PathPrefix)
		}
		var requestPathTarget *url.URL
		var requestPathTargetPath, requestPathTargetRawPath string
		var requestPathTargetErr error
		if pathTargetMode {
			requestPathTarget, requestPathTargetPath, requestPathTargetRawPath, requestPathTargetErr = parsePathTargetRequest(r.URL)
		}
		requestStartedAt := time.Now()
		clientRequestContext := r.Context()
		requestCtx, requestCancel := context.WithCancel(r.Context())
		requestCtx = context.WithValue(requestCtx, originalRequestContextKey{}, clientRequestContext)
		if requestPathTarget != nil {
			requestCtx = context.WithValue(requestCtx, pathTargetContextKey{}, requestPathTarget)
		}
		backendTracker := &backendAddressTracker{}
		backendTracker.SetURL(routedUpstream(r))
		requestCtx = context.WithValue(requestCtx, backendAddressContextKey{}, backendTracker)
		diagnostics := newRequestDiagnosticsTracker(requestStartedAt, r)
		requestCtx = context.WithValue(requestCtx, requestDiagnosticsContextKey{}, diagnostics)
		stopInstanceCancel := context.AfterFunc(inst.ctx, requestCancel)
		defer func() {
			stopInstanceCancel()
			requestCancel()
		}()
		r = r.WithContext(requestCtx)
		requestLogWriter := &requestLogResponseWriter{ResponseWriter: w, diagnostics: diagnostics}
		requestLogRequest := r
		if pathTargetMode {
			requestLogRequest = r.Clone(r.Context())
			logURL := *r.URL
			if requestPathTarget != nil {
				logURL.Path = requestPathTargetPath
				logURL.RawPath = requestPathTargetRawPath
			} else {
				// Malformed embedded URLs may contain userinfo. Never persist the
				// original target path when parsing failed.
				logURL.Path = pathTargetRoutePrefix + "invalid"
				logURL.RawPath = ""
			}
			requestLogRequest.URL = &logURL
		}
		requestLogEntry := newRequestLogEvent(site, requestLogRequest, inst.trustedProxies, policy)
		if pathTargetMode && requestPathTargetErr == nil {
			// Playback and stop observers use the resolved source API path rather
			// than the gateway path that embeds a target authority.
			r.URL.Path = requestPathTargetPath
			r.URL.RawPath = requestPathTargetRawPath
		}
		watchHistoryCapture := startWatchHistoryCapture(site, r, requestLogEntry.ResourceCategory, pm.database)
		attachWatchHistoryStopIntentCapture(watchHistoryCapture, pm.database, site, r, inst.trustedProxies, requestStartedAt)
		defer func() {
			panicValue := recover()
			if panicValue != nil {
				diagnostics.markAborted()
			}
			requestEndedAt := time.Now()
			requestLogEntry.StatusCode = requestLogWriter.StatusCode()
			if errors.Is(clientRequestContext.Err(), context.Canceled) {
				requestLogEntry.StatusCode = clientClosedRequestStatus
			}
			requestLogEntry.OutboundColo = requestLogColo(requestLogWriter.Header().Get("CF-Ray"))
			if tracker := backendAddressTrackerFromContext(r.Context()); tracker != nil {
				requestLogEntry.BackendAddress = tracker.Get()
			}
			requestLogEntry.RequestDiagnostics = diagnostics.snapshot(requestEndedAt, requestLogWriter.ResponseStatusCode(), clientRequestContext.Err(), inst.ctx.Err())
			finishWatchHistoryCapture(watchHistoryCapture, pm.database, site, r, inst.trustedProxies, diagnostics.upstreamResponseStatusCode(), requestStartedAt)
			pm.accountRetention.Observe(site, r, inst.trustedProxies, requestLogEntry.ResourceCategory, requestLogEntry.StatusCode, requestStartedAt, requestEndedAt)
			pm.database.EnqueueRequestLog(requestLogEntry)
			if panicValue != nil {
				panic(panicValue)
			}
		}()
		w = requestLogWriter
		inst.trafficRequests().Add(1)
		inst.trafficPendingRequests().Add(1)

		// The Controller can refresh quota usage on an already running Agent
		// without rebuilding the proxy bundle. Read the live quota baseline from
		// the instance so a scheduled route cannot retain a stale zero quota.
		inst.trafficMu.Lock()
		trafficQuota := inst.Site.TrafficQuota
		inst.trafficMu.Unlock()
		if trafficQuota > 0 {
			// A configured quota is a hard limit, so an unreadable usage
			// baseline must fail closed with "enforcement unavailable" rather
			// than admitting traffic that the site may no longer be allowed to
			// carry. This is deliberately not 403: the quota is not known to be
			// exhausted, the check itself could not run.
			switch err := quotaUsageDecision(pm, inst, trafficQuota, time.Now()); err {
			case nil:
			case errTrafficQuotaExceeded:
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				w.Write([]byte(`{"error":"traffic quota exceeded"}`))
				return
			default:
				log.Printf("[%s] refusing request: %v", site.Name, errTrafficQuotaUnavailable)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte(`{"error":"traffic quota enforcement unavailable"}`))
				return
			}
		}
		if pathTargetMode {
			if requestPathTargetErr != nil {
				http.Error(w, "invalid path target", http.StatusBadRequest)
				return
			}
			if r.Method == http.MethodConnect {
				http.Error(w, "CONNECT tunneling is not supported", http.StatusMethodNotAllowed)
				return
			}
		}
		if !pathTargetMode && isReservedDynamicRoute(r.URL.Path) {
			var rw http.ResponseWriter
			if speedLimitBytes > 0 {
				limited := &rateLimitedWriter{
					ResponseWriter: w,
					bytesPerSec:    speedLimitBytes,
					written:        inst.trafficBytesOut(),
					cumulative:     inst.trafficCumulativeOut(),
					start:          time.Now(),
					ctx:            r.Context(),
				}
				if trafficQuota > 0 {
					rw = &quotaLimitedWriter{meteredWriter: meteredWriter{ResponseWriter: limited}, pm: pm, inst: inst, quota: trafficQuota}
				} else {
					rw = limited
				}
			} else {
				rw = pm.meteredOrQuotaWriter(w, inst, trafficQuota)
			}
			if dynamicIssuer == nil {
				writeDynamicCapabilityUnavailable(rw)
			} else {
				dynamicIssuer.serve(rw, r)
			}
			return
		}

		if site.MainVideoStreamMode == mainVideoStreamModeDirect && isMainVideoStreamRequest(r) {
			directTarget := mainVideoDirectTarget(r, routedUpstream(r))
			if tracker := backendAddressTrackerFromContext(r.Context()); tracker != nil {
				tracker.SetURL(directTarget)
			}
			r = r.WithContext(context.WithValue(r.Context(), mainVideoDirectFallbackContextKey{}, directTarget))
		}

		if hasUpgradeIntent(r) {
			if !isWebSocketUpgrade(r) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid websocket upgrade"}`))
				return
			}
			handleWebSocket(w, r, routedUpstream(r), target, policy, inst, speedLimitBytes, pm, trafficQuota, configuredHeaders)
			return
		}

		cacheTarget := routedUpstream(r)
		var cacheHit *assetCacheHit
		r, cacheHit = prepareAssetCacheRequest(pm.assetCache, site, r, assetCacheTargetURL(r, cacheTarget))
		if cacheHit != nil {
			var cacheWriter http.ResponseWriter
			if speedLimitBytes > 0 {
				limited := &rateLimitedWriter{ResponseWriter: w, bytesPerSec: speedLimitBytes, written: inst.trafficBytesOut(), cumulative: inst.trafficCumulativeOut(), start: time.Now(), ctx: r.Context()}
				if trafficQuota > 0 {
					cacheWriter = &quotaLimitedWriter{meteredWriter: meteredWriter{ResponseWriter: limited}, pm: pm, inst: inst, quota: trafficQuota}
				} else {
					cacheWriter = limited
				}
			} else {
				cacheWriter = pm.meteredOrQuotaWriter(w, inst, trafficQuota)
			}
			serveAssetCacheHit(cacheWriter, r, cacheHit)
			return
		}

		if r.Body != nil {
			if trafficQuota > 0 {
				r.Body = &quotaLimitedReader{meteredReader: meteredReader{ReadCloser: r.Body, read: inst.trafficBytesIn(), cumulative: inst.trafficCumulativeIn()}, pm: pm, inst: inst, quota: trafficQuota}
			} else {
				r.Body = &meteredReader{ReadCloser: r.Body, read: inst.trafficBytesIn(), cumulative: inst.trafficCumulativeIn()}
			}
		}
		if r.Method == http.MethodPost && allowsFailover(r, cacheTarget) {
			if err := prepareFailoverPlaybackInfoBody(r, redirectPolicy.limits.MaxBodyBytes); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid request body"}`))
				return
			}
		}

		var rw http.ResponseWriter
		if speedLimitBytes > 0 {
			limited := &rateLimitedWriter{
				ResponseWriter: w,
				bytesPerSec:    speedLimitBytes,
				written:        inst.trafficBytesOut(),
				cumulative:     inst.trafficCumulativeOut(),
				start:          time.Now(),
				ctx:            r.Context(),
			}
			// The inner writer already meters; pass nil counters so wrapping
			// does not double-count the same bytes.
			if trafficQuota > 0 {
				rw = &quotaLimitedWriter{meteredWriter: meteredWriter{ResponseWriter: limited}, pm: pm, inst: inst, quota: trafficQuota}
			} else {
				rw = limited
			}
		} else {
			rw = pm.meteredOrQuotaWriter(w, inst, trafficQuota)
		}
		proxy.ServeHTTP(rw, r) // #nosec G704 -- path-target sites intentionally proxy HTTP(S) destinations named in each request path, including private destinations.
	})

	inst.handler = handler
	var listener net.Listener
	var server *http.Server
	if ingressUsesPort(site.IngressMode) {
		listenAddr := fmt.Sprintf(":%d", site.ListenPort)
		listener, err = net.Listen("tcp", listenAddr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", listenAddr, err)
		}
		if siteTLSConfig := pm.siteTLSConfigSnapshot(); siteTLSConfig != nil {
			listener = tls.NewListener(listener, siteTLSConfig)
		}
		listener = limitListener(listener, 2048)
		server = &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       0,
			WriteTimeout:      0,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    64 << 10,
		}
		inst.server = server
		inst.listener = listener
	}
	closeNewListener := func() {
		if listener != nil {
			_ = listener.Close()
		}
	}

	pm.mu.Lock()
	if ingressUsesHost(site.IngressMode) {
		if assignedID, ok := pm.publicHosts[site.PublicHost]; ok && assignedID != site.ID {
			pm.mu.Unlock()
			closeNewListener()
			return fmt.Errorf("public_host %s is already assigned to another site", site.PublicHost)
		}
	}
	if ingressUsesPath(site.IngressMode) {
		if assignedID, ok := pm.pathPrefixes[site.PathPrefix]; ok && assignedID != site.ID {
			pm.mu.Unlock()
			closeNewListener()
			return fmt.Errorf("path_prefix %s is already assigned to another site", site.PathPrefix)
		}
	}
	existing := pm.proxies[site.ID]
	pm.mu.Unlock()
	if existing != nil {
		// Verify persistence before stopping the old instance. lifecycleMu pins the
		// selected instance while trafficMu serializes this flush, so the global
		// routing lock need not be held across a potentially slow SQLite write.
		if err := pm.flushProxyTraffic(existing); err != nil {
			closeNewListener()
			return fmt.Errorf("flush traffic of the instance being replaced: %w", err)
		}
	}

	if existing != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
		shutdownErr := existing.shutdown(shutdownCtx)
		shutdownCancel()
		if shutdownErr != nil {
			closeNewListener()
			return fmt.Errorf("drain the instance being replaced: %w", shutdownErr)
		}
		// Account for bytes produced after the pre-stop flush. If this fails, keep
		// the closed instance in the map with its counters so a retry can persist
		// them rather than silently orphaning traffic.
		if err := pm.flushProxyTraffic(existing); err != nil {
			closeNewListener()
			return fmt.Errorf("final traffic flush of the instance being replaced: %w", err)
		}
		// The 60s flusher mutates these Site fields under trafficMu; take the
		// same lock so a concurrent flush cannot hand us a torn baseline.
		existing.trafficMu.Lock()
		flushedTraffic := existing.Site.TrafficUsed
		flushedIn := existing.Site.TrafficUsedIn
		flushedOut := existing.Site.TrafficUsedOut
		existing.trafficMu.Unlock()
		if flushedTraffic > inst.persistedTraffic.Load() {
			inst.persistedTraffic.Store(flushedTraffic)
			inst.persistedBytesIn.Store(flushedIn)
			inst.persistedBytesOut.Store(flushedOut)
			inst.Site.TrafficUsed = flushedTraffic
		}
	}

	nextSelfTargets, snapshotErr := pm.buildDynamicSelfTargetPolicy()
	pm.mu.Lock()
	if err := pm.registerSiteHostLocked(site); err != nil {
		pm.mu.Unlock()
		closeNewListener()
		return err
	}
	if snapshotErr != nil {
		pm.dynamicRuntime.selfTargets.Store(nil)
	} else {
		pm.dynamicRuntime.selfTargets.Store(nextSelfTargets)
	}
	pm.proxies[site.ID] = inst
	pm.mu.Unlock()
	if snapshotErr != nil {
		log.Printf("dynamic discovery disabled until the next lifecycle refresh: %v", snapshotErr)
	}
	installed = true

	upstreamLogTarget := redactUpstreamURL(target)
	if pathTargetMode {
		upstreamLogTarget = "request-path target"
	}
	if server == nil {
		if ingressUsesPath(site.IngressMode) {
			log.Printf("[%s] shared-path proxy %s -> %s (UA: %s)", site.Name, site.PathPrefix, upstreamLogTarget, site.UAMode)
		} else {
			log.Printf("[%s] shared-host proxy %s -> %s (UA: %s)", site.Name, site.PublicHost, upstreamLogTarget, site.UAMode)
		}
		return nil
	}
	inst.portServing.Store(true)
	go func() {
		defer inst.portServing.Store(false)
		if len(playbackHostsSet) > 0 {
			hosts := make([]string, 0, len(playbackHostsSet))
			for h := range playbackHostsSet {
				hosts = append(hosts, h)
			}
			log.Printf("[%s] proxy :%d -> %s (playback hosts: %s, mode: %s, UA: %s)", site.Name, site.ListenPort, upstreamLogTarget, strings.Join(hosts, ", "), site.PlaybackMode, site.UAMode)
		} else {
			protocol := "http"
			if pm.siteTLSConfigSnapshot() != nil {
				protocol = "https"
			}
			log.Printf("[%s] %s :%d -> %s (UA: %s)", site.Name, protocol, site.ListenPort, upstreamLogTarget, site.UAMode)
		}
		err := server.Serve(listener)
		if inst.isAccepting() {
			// A Serve loop that disappears while the lifecycle gate is still open
			// makes a port-only site unavailable even though its instance remains in
			// the map. Record that state for API/runtime diagnostics.
			inst.portServeFailed.Store(true)
			if err != nil && err != http.ErrServerClosed {
				log.Printf("[%s] server error: %v", site.Name, err)
			} else {
				log.Printf("[%s] server stopped unexpectedly", site.Name)
			}
		}
	}()

	return nil
}

// meteredOrQuotaWriter returns the response writer that meters site traffic,
// wrapped with mid-stream quota enforcement when the site has a quota.
func (pm *ProxyManager) meteredOrQuotaWriter(w http.ResponseWriter, inst *ProxyInstance, trafficQuota int64) http.ResponseWriter {
	metered := meteredWriter{ResponseWriter: w, written: inst.trafficBytesOut(), cumulative: inst.trafficCumulativeOut()}
	if trafficQuota > 0 {
		return &quotaLimitedWriter{meteredWriter: metered, pm: pm, inst: inst, quota: trafficQuota}
	}
	return &metered
}
