package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
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

func TestDevelopmentAgentManifestUsesBundledBinary(t *testing.T) {
	path := t.TempDir() + string(os.PathSeparator) + "meridian-agent-linux-amd64"
	contents := []byte("development Agent binary")
	if err := os.WriteFile(path, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	previousVersion := appVersion
	previousPath := os.Getenv("MERIDIAN_AGENT_BINARY_LINUX_AMD64")
	defer func() {
		appVersion = previousVersion
		_ = os.Setenv("MERIDIAN_AGENT_BINARY_LINUX_AMD64", previousPath)
	}()
	appVersion = "v1.11.0-pre.2"
	if err := os.Setenv("MERIDIAN_AGENT_BINARY_LINUX_AMD64", path); err != nil {
		t.Fatal(err)
	}
	manifest, err := agentReleaseManifestForPlatform(context.Background(), "linux/amd64")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != appVersion || manifest.Platform != "linux/amd64" || manifest.DownloadURL != "/api/agent/binary" || manifest.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("development manifest = %#v", manifest)
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

func TestLocalAgentBinaryMustMatchReleaseManifest(t *testing.T) {
	path := t.TempDir() + string(os.PathSeparator) + "meridian-agent"
	contents := []byte("stale Agent binary")
	if err := os.WriteFile(path, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	manifest := AgentBinaryManifest{Version: "v1.9.32", Platform: "linux/amd64", SHA256: hex.EncodeToString(digest[:])}
	if !localAgentBinaryMatchesManifest(path, manifest) {
		t.Fatal("matching local Agent binary was rejected")
	}
	manifest.SHA256 = strings.Repeat("0", 64)
	if localAgentBinaryMatchesManifest(path, manifest) {
		t.Fatal("stale local Agent binary was accepted for a different release")
	}
}
