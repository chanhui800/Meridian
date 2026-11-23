package main

import (
	"crypto/sha256"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestControllerAndAgentRequestLogSwitchParity(t *testing.T) {
	for _, flag := range []string{"image", "ua", "timeline", "status", "node", "category", "upstream_ua", "backend", "colo", "enabled", "error", "asset"} {
		t.Run(flag, func(t *testing.T) {
			app := newTestApp(t)
			now := time.Now().UTC()
			node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "log-parity", Address: "203.0.113.85"}, now)
			if err != nil {
				t.Fatal(err)
			}
			_, token, err := app.db.EnrollControlNode(enrollment, now)
			if err != nil {
				t.Fatal(err)
			}
			site, err := app.db.CreateSiteRecord(Site{Name: "log-parity-site", PublicHost: "log-parity.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
				t.Fatal(err)
			}
			if _, err := app.db.db.Exec("UPDATE site_node_schedules SET desired_node_id=? WHERE site_id=?", node.ID, site.ID); err != nil {
				t.Fatal(err)
			}
			settings := app.db.currentSystemSettings()
			settings.LogWriteImage, settings.LogWriteMetadata, settings.LogWriteColo = true, true, true
			category, path := requestLogCategoryAPI, "/System/Info"
			suppressed := false
			switch flag {
			case "image":
				settings.LogWriteImage = false
				category, path = requestLogCategoryImage, "/Items/diag/Images/Primary"
				suppressed = true
			case "asset":
				settings.LogWriteAsset = false
				category, path = requestLogCategoryAsset, "/web/app.js"
				suppressed = true
			case "enabled":
				settings.LogEnabled = false
				suppressed = true
			case "error":
				settings.LogLevel = "error"
				suppressed = true
			case "ua":
				settings.LogWriteUA = false
			case "timeline":
				settings.LogWriteTimeline = false
			case "status":
				settings.LogWriteStatus = false
			case "node":
				settings.LogWriteNode = false
			case "category":
				settings.LogWriteCategory = false
			case "upstream_ua":
				settings.LogWriteUpstreamUA = false
			case "backend":
				settings.LogWriteBackendAddress = false
			case "colo":
				settings.LogWriteColo = false
			}
			if err := app.db.saveSystemSettings(settings); err != nil {
				t.Fatal(err)
			}
			local := requestLogEvent{SiteID: site.ID, SiteName: site.Name, FinalNode: "controller", ResourceCategory: category, StatusCode: http.StatusOK, UserAgent: "ParityClient/1", UpstreamUserAgent: "ParityUpstream/1", BackendAddress: "https://origin.example:443", InboundColo: "HKG", OutboundColo: "SIN", Method: http.MethodGet, Path: path}
			app.db.EnqueueRequestLog(local)
			if _, err := app.db.ListRequestLogs(RequestLogFilter{Limit: 10}); err != nil {
				t.Fatal(err)
			}
			var localCount int
			if err := app.db.db.QueryRow("SELECT COUNT(*) FROM request_logs").Scan(&localCount); err != nil {
				t.Fatal(err)
			}
			want := 1
			if suppressed {
				want = 0
			}
			if localCount != want {
				t.Fatalf("local rows=%d want=%d", localCount, want)
			}
			if err := app.db.ClearRequestLogs(); err != nil {
				t.Fatal(err)
			}
			// Legacy Agents omit the policy/diagnostics version. The Controller
			// still filters their valid events inside the dedup transaction.
			wire := NodeRequestEvent{EventID: 1, SiteID: site.ID, Host: site.PublicHost, Method: local.Method, Path: local.Path, StatusCode: local.StatusCode, UserAgent: local.UserAgent, UpstreamUserAgent: local.UpstreamUserAgent, BackendAddress: local.BackendAddress, InboundColo: local.InboundColo, OutboundColo: local.OutboundColo, ResourceCategory: category, RecordedAtMS: now.UnixMilli()}
			result, err := app.db.RecordNodeReportResult(token, NodeReport{BootID: "log-parity-session", ReportSessionID: "log-parity-session", CounterEpoch: "kernel:eth0", Sequence: 1, InterfaceName: "eth0", Events: []NodeRequestEvent{wire}}, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.AcceptedEventIDs) != 1 || result.AcceptedEventIDs[0] != 1 {
				t.Fatalf("suppressed event lost ACK: %+v", result)
			}
			var count, status int
			var ua, upstreamUA, backend, finalNode, recordedCategory, inbound, outbound string
			var timeline int64
			if err := app.db.db.QueryRow("SELECT COUNT(*),COALESCE(MAX(status_code),0),COALESCE(MAX(user_agent),''),COALESCE(MAX(upstream_user_agent),''),COALESCE(MAX(backend_address),''),COALESCE(MAX(final_node),''),COALESCE(MAX(resource_category),''),COALESCE(MAX(inbound_colo),''),COALESCE(MAX(outbound_colo),''),COALESCE(MAX(timeline_at_ms),0) FROM request_logs").Scan(&count, &status, &ua, &upstreamUA, &backend, &finalNode, &recordedCategory, &inbound, &outbound, &timeline); err != nil {
				t.Fatal(err)
			}
			if count != want || flag == "ua" && ua != "" || flag == "upstream_ua" && upstreamUA != "" || flag == "timeline" && timeline != 0 || flag == "status" && status != 0 || flag == "node" && finalNode != "" || flag == "category" && recordedCategory != "" || flag == "backend" && backend != "" || flag == "colo" && (inbound != "" || outbound != "") {
				t.Fatalf("Agent failed %s switch: count=%d status=%d ua=%q upstream=%q backend=%q node=%q category=%q colo=%q/%q timeline=%d", flag, count, status, ua, upstreamUA, backend, finalNode, recordedCategory, inbound, outbound, timeline)
			}
			// Exercise the actual ephemeral Enqueue branch, which must apply the
			// same field and category policy before sending a best-effort log.
			edgeDB := &DB{edgeEphemeral: true}
			edgeDB.systemSettings.Store(&settings)
			var captured *requestLogEvent
			edgeDB.edgeRequestLogSink = func(event requestLogEvent) { captured = &event }
			edgeDB.EnqueueRequestLog(local)
			if suppressed && captured != nil || !suppressed && captured == nil {
				t.Fatalf("Agent Enqueue bypassed %s switch", flag)
			}
			if captured != nil && (flag == "ua" && captured.UserAgent != "" || flag == "upstream_ua" && captured.UpstreamUserAgent != "" || flag == "status" && captured.StatusCode != 0 || flag == "node" && captured.FinalNode != "" || flag == "category" && captured.ResourceCategory != "" || flag == "backend" && captured.BackendAddress != "" || flag == "colo" && (captured.InboundColo != "" || captured.OutboundColo != "")) {
				t.Fatalf("Agent queued disabled field for %s: %+v", flag, *captured)
			}
		})
	}
}

func TestEdgeRequestLogPolicyRefreshWithSameConfigHash(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		if r.URL.Path == "/Items/diag" {
			w.Header().Set("Content-Type", "application/json")
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	runtime := &edgeAgentRuntime{stateDir: t.TempDir()}
	settings := defaultSystemSettings()
	settings.LogWriteImage = true
	config := AgentRuntimeConfig{
		SchemaVersion: agentConfigSchemaVersion, ConfigHash: "stable-log-policy", NodeGUID: "log-policy-node", HTTPSPort: 19090,
		DynamicKey: testEdgeRuntimeKey(t), RequestLogPolicy: newAgentRequestLogPolicy(settings),
		Routes: []AgentSiteRoute{{SiteID: 81, Host: "hot-policy.example.test", TargetURL: upstream.URL,
			Site:            Site{Name: "hot-policy", PublicHost: "hot-policy.example.test", IngressMode: ingressModeHost, TargetURL: upstream.URL, PlaybackMode: "direct", MainVideoStreamMode: "proxy", StreamHosts: "[]", UAMode: passthroughUAMode, ClientIPMode: clientIPModeBoth},
			FailoverTargets: "[]", StreamHostsRaw: "[]"}},
	}
	bundle, err := buildEdgeProxy(config, runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer bundle.close()
	runtime.mu.Lock()
	runtime.bundle = bundle
	runtime.mu.Unlock()
	request := func(path string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "https://hot-policy.example.test"+path, nil)
		req.Header.Set("User-Agent", "policy-UA-must-disappear")
		response := httptest.NewRecorder()
		bundle.handler.ServeHTTP(response, req)
		if response.Code != 200 || response.Body.String() != "ok" {
			t.Fatalf("policy request failed=%d %q", response.Code, response.Body.String())
		}
	}
	request("/Items/diag/Images/Primary")
	events := runtime.events.snapshot()
	if len(events) != 1 || events[0].ResourceCategory != requestLogCategoryImage || events[0].DiagnosticsVersion != 1 {
		t.Fatalf("enabled image logging was lost: %+v", events)
	}
	if err := runtime.events.ack(events); err != nil {
		t.Fatal(err)
	}
	settings.LogWriteImage, settings.LogWriteMetadata = false, true
	settings.LogWriteUA, settings.LogWriteTimeline = false, false
	refreshed := config
	refreshed.RequestLogPolicy = newAgentRequestLogPolicy(settings)
	runtime.syncSiteTrafficLimits(refreshed)
	if runtime.bundle != bundle || refreshed.ConfigHash != config.ConfigHash {
		t.Fatal("policy refresh rebuilt the route bundle")
	}
	request("/Items/diag/Images/Primary")
	if runtime.events.depth() != 0 {
		t.Fatal("disabled image log was still queued")
	}
	request("/Items/diag")
	events = runtime.events.snapshot()
	var logEvents []NodeRequestEvent
	for _, event := range events {
		if !event.SkipRequestLog {
			logEvents = append(logEvents, event)
		}
	}
	if len(logEvents) != 1 || logEvents[0].UserAgent != "" || logEvents[0].TimelineAtMS == nil || *logEvents[0].TimelineAtMS != 0 || logEvents[0].DurationMS != nil || logEvents[0].ResourceCategory != requestLogCategoryMetadata {
		t.Fatalf("hash-stable policy was not applied: %+v", events)
	}
	if len(events) != 2 || !events[1].SkipRequestLog || events[1].ResponseBody == "" {
		t.Fatal("log policy suppressed the independent metadata effect")
	}
	if err := runtime.events.ack(events); err != nil {
		t.Fatal(err)
	}
	stats := runtime.siteStatsSnapshot()
	if len(stats) != 1 || stats[0].SiteID != 81 || stats[0].RequestCount != 3 || stats[0].CumulativeBytesOut != 6 {
		t.Fatalf("logging suppression changed traffic counters: %+v", stats)
	}
	// A legacy Controller omits the policy. Its own transaction remains the
	// authoritative filter, so the Agent must forward enabled categories.
	refreshed.RequestLogPolicy = nil
	runtime.syncSiteTrafficLimits(refreshed)
	request("/Items/diag/Images/Primary")
	if events = runtime.events.snapshot(); len(events) != 1 || events[0].ResourceCategory != requestLogCategoryImage {
		t.Fatalf("legacy policy fallback swallowed image log: %+v", events)
	}
}

func TestNodeRequestLogSanitizedReplayPreservesWatchEffectsAndTraffic(t *testing.T) {
	app := newTestApp(t)
	now := time.Now().UTC()
	node, enrollment, err := app.db.CreateControlNode(NodeCreateInput{Name: "masked-event-node", Address: "203.0.113.85"}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.db.EnrollControlNode(enrollment, now)
	if err != nil {
		t.Fatal(err)
	}
	site, err := app.db.CreateSiteRecord(Site{Name: "masked-events", PublicHost: "masked-events.example.test", IngressMode: ingressModeHost, TargetURL: "http://127.0.0.1:8096", WatchHistoryEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.SaveSiteNodeSchedule(site.ID, true, "fixed", node.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := app.db.db.Exec("UPDATE site_node_schedules SET desired_node_id=? WHERE site_id=?", node.ID, site.ID); err != nil {
		t.Fatal(err)
	}
	settings := app.db.currentSystemSettings()
	settings.LogLevel, settings.LogWriteStatus, settings.LogWriteCategory = "error", false, false
	settings.LogWriteUA, settings.LogWriteUpstreamUA, settings.LogWriteTimeline, settings.LogWriteBackendAddress = false, false, false, false
	settings.LogWriteAPI = false // /socket cannot be reclassified as API after the category was masked.
	if err := app.db.saveSystemSettings(settings); err != nil {
		t.Fatal(err)
	}
	edgeDB := &DB{edgeEphemeral: true}
	edgeDB.systemSettings.Store(&settings)
	var captured *requestLogEvent
	edgeDB.edgeRequestLogSink = func(value requestLogEvent) { captured = &value }
	duration := int64(0)
	edgeDB.EnqueueRequestLog(requestLogEvent{
		RequestDiagnostics: RequestDiagnostics{DiagnosticsVersion: 1, ResponseStatusCode: 502, TerminationReason: "upstream_error", DurationMS: &duration},
		SiteID:             site.ID, Method: http.MethodGet, Path: "/socket?token=never-persist", StatusCode: 502, ResourceCategory: requestLogCategoryWebSocket,
		UserAgent: "never-persist-UA", UpstreamUserAgent: "never-persist-upstream", BackendAddress: "https://origin.example.test/private?token=never-persist",
	})
	if captured == nil || captured.StatusCode != 0 || captured.ResourceCategory != "" || captured.UserAgent != "" || captured.DurationMS != nil {
		t.Fatalf("Agent mask failed: %+v", captured)
	}
	logEvent := NodeRequestEvent{RequestDiagnostics: captured.RequestDiagnostics, EventID: 1, EventUID: strings.Repeat("d", 32), SiteID: site.ID, Host: site.PublicHost, Method: captured.Method, Path: captured.Path, StatusCode: captured.StatusCode, ResourceCategory: captured.ResourceCategory, RecordedAtMS: now.UnixMilli(), TimelineAtMS: captured.timelineAtMS}
	history := &watchHistoryEvent{SiteID: site.ID, SessionHash: strings.Repeat("b", sha256.Size*2), UpstreamItemID: "item-1", EventType: "progress", ObservedAtMS: now.UnixMilli(), PositionTicks: 10, RunTimeTicks: 100, SeasonNumber: -1, EpisodeNumber: -1}
	critical := NodeRequestEvent{EventID: 2, EventUID: strings.Repeat("c", 32), SiteID: site.ID, Host: site.PublicHost, Method: http.MethodPost, Path: "/Sessions/Playing/Progress", StatusCode: 204, RecordedAtMS: now.UnixMilli(), ResourceCategory: requestLogCategoryPlaybackSync, Priority: nodeEventPriorityCritical, WatchHistory: history}
	baseline := NodeReport{BootID: "masked-session", ReportSessionID: "masked-session", CounterEpoch: "kernel:eth0", Sequence: 1, InterfaceName: "eth0"}
	if _, err := app.db.RecordNodeReportResult(token, baseline, now); err != nil {
		t.Fatal(err)
	}
	report := baseline
	report.Sequence, report.TXBytes, report.Events = 2, 23, []NodeRequestEvent{logEvent, critical}
	report.SiteStats = []NodeSiteStat{{SiteID: site.ID, Host: site.PublicHost, RequestCount: 2, LastRequestAtMS: now.UnixMilli(), LastStatus: 204, CumulativeBytesIn: 11, CumulativeBytesOut: 19}}
	result, err := app.db.RecordNodeReportResult(token, report, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.AcceptedEventIDs) != 2 || len(result.AcceptedSiteIDs) != 1 || result.Node.PeriodTXBytes != 23 {
		t.Fatalf("masked event lost ACK/effects/traffic: %+v", result)
	}
	logs, err := app.db.ListRequestLogs(RequestLogFilter{Limit: 10})
	if err != nil || len(logs) != 1 || logs[0].StatusCode != 0 || logs[0].ResourceCategory != "" || logs[0].UserAgent != "" || logs[0].RecordedAtMS != 0 || logs[0].DurationMS != nil || logs[0].ResponseStatusCode != 0 || logs[0].DiagnosticsVersion != 1 {
		t.Fatalf("double filter dropped/restored masked log: %+v err=%v", logs, err)
	}
	report.Sequence = 3
	result, err = app.db.RecordNodeReportResult(token, report, now.Add(2*time.Second))
	if err != nil || len(result.AcceptedEventIDs) != 2 || result.Node.PeriodTXBytes != 23 {
		t.Fatalf("duplicate ACK or traffic changed: %+v err=%v", result, err)
	}
	var watchCount, logCount int
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM watch_sessions WHERE site_id=?", site.ID).Scan(&watchCount); err != nil {
		t.Fatal(err)
	}
	if err := app.db.db.QueryRow("SELECT COUNT(*) FROM request_logs WHERE site_id=?", site.ID).Scan(&logCount); err != nil {
		t.Fatal(err)
	}
	if watchCount != 1 || logCount != 1 {
		t.Fatalf("suppression/replay effects: watch=%d logs=%d", watchCount, logCount)
	}
}
