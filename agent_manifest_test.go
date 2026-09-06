package main

import (
	"strings"
	"testing"
)

func TestParseAgentReleaseChecksums(t *testing.T) {
	checksums, err := parseAgentReleaseChecksums(strings.NewReader(
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef  meridian-agent-linux-amd64\n" +
			"ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789 *meridian-agent-linux-arm64\n" +
			"not-a-checksum  ignored\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	if checksums["meridian-agent-linux-amd64"] == "" || checksums["meridian-agent-linux-arm64"] != "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789" {
		t.Fatalf("parsed checksums = %#v", checksums)
	}
}

func TestValidateAgentReleaseDownloadURL(t *testing.T) {
	url := agentReleaseDownloadURL("v1.9.30", "meridian-agent-linux-arm64")
	if err := validateAgentReleaseDownloadURL(url, "v1.9.30", "meridian-agent-linux-arm64"); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		"https://github.com/chanhui800/Meridian/releases/latest/meridian-agent-linux-arm64",
		"https://github.com/other/repo/releases/download/v1.9.30/meridian-agent-linux-arm64",
		"http://github.com/chanhui800/Meridian/releases/download/v1.9.30/meridian-agent-linux-arm64",
	} {
		if err := validateAgentReleaseDownloadURL(invalid, "v1.9.30", "meridian-agent-linux-arm64"); err == nil {
			t.Fatalf("invalid Agent release URL accepted: %s", invalid)
		}
	}
}
