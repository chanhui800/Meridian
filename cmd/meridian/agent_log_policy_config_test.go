package main

import "testing"

func TestAgentConfigLogPolicyRefreshKeepsRouteIdentity(t *testing.T) {
	settings := defaultSystemSettings()
	config := AgentRuntimeConfig{
		SchemaVersion:    agentConfigSchemaVersion,
		NodeGUID:         "log-policy-refresh-test",
		HTTPSPort:        9090,
		RequestLogPolicy: newAgentRequestLogPolicy(settings),
		Routes: []AgentSiteRoute{{
			Host: "media.example.test", SiteID: 7, TargetURL: "https://origin.example.test",
			Site: Site{ID: 7, TargetURL: "https://origin.example.test", UAMode: passthroughUAMode},
		}},
	}
	for _, version := range []string{"v1.11.6", "v1.11.7-pre.20261121.5", "dev"} {
		before, err := agentConfigHashForVersion(config, version)
		if err != nil {
			t.Fatal(err)
		}
		changed := config
		changedSettings := settings
		changedSettings.LogWriteImage = !changedSettings.LogWriteImage
		changedSettings.LogWriteUA = !changedSettings.LogWriteUA
		changed.RequestLogPolicy = newAgentRequestLogPolicy(changedSettings)
		after, err := agentConfigHashForVersion(changed, version)
		if err != nil || before != after {
			t.Fatalf("%s log-policy-only refresh changed route identity: before=%s after=%s err=%v", version, before, after, err)
		}
	}
}
