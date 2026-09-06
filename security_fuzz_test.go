package main

import (
	"context"
	"crypto/sha256"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// These targets are deliberately small and deterministic.  They exercise the
// parsers that sit on the URL, media-manifest, authorization, and backup trust
// boundaries without making network calls or writing to the database.
func FuzzNormalizeDynamicURL(f *testing.F) {
	for _, seed := range []string{
		"https://media.example.test/video/master.m3u8",
		"http://127.0.0.1:8096/Items/1",
		"https://[::1]/stream",
		"https://media.example.test/a%2Fb?token=x",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		parsed, err := normalizeDynamicURL(value)
		if err != nil {
			return
		}
		if parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || strings.ContainsAny(parsed.Host, "\\\r\n\t") || filepath.Clean(parsed.Path) != parsed.Path {
			t.Fatalf("unsafe dynamic URL accepted: %q", parsed.String())
		}
	})
}

func FuzzHLSRewrite(f *testing.F) {
	for _, seed := range []string{
		"URI=\"segment.ts\",BANDWIDTH=1000",
		"URI=\"https://media.example.test/key\"",
		"URI=\"bad\\\"value\"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(_ *testing.T, value string) {
		_, _ = parseHLSAttributeList(value)
	})
}

func FuzzDASHRewrite(f *testing.F) {
	for _, seed := range []string{
		"<MPD xmlns=\"urn:mpeg:dash:schema:mpd:2011\"><Period/></MPD>",
		"<MPD><Period><AdaptationSet><Representation/></AdaptationSet></Period></MPD>",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(_ *testing.T, value string) {
		_, _ = parseDASHXML(context.Background(), []byte(value))
	})
}

func FuzzPlaybackInfoRewrite(f *testing.F) {
	for _, seed := range []string{
		`{"MediaSources":[]}`,
		`{"MediaSources":[{"Path":"/Items/1"}]}`,
		`{"MediaSources":[{"Path":"https://media.example.test/stream"}]}`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(_ *testing.T, value string) {
		// Structure validation is the first, allocation-bounded stage of the
		// PlaybackInfo rewriter and is safe to run without an issuer/session.
		_, _ = validateDynamicJSONStructureWithin(context.Background(), []byte(value), 4096, globalDynamicMaxParseDepth)
	})
}

func FuzzBackupArchiveParser(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("not a zip"),
		[]byte("PK\x03\x04"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value []byte) {
		_, entries, err := parseBackupArchive(value)
		if err != nil {
			return
		}
		if len(entries) > backupMaxFiles {
			t.Fatalf("backup parser accepted too many entries: %d", len(entries))
		}
		for name := range entries {
			if _, ok := backupEntryLimit(name); !ok || filepath.ToSlash(filepath.Clean(name)) != name || strings.HasPrefix(name, "/") {
				t.Fatalf("backup parser accepted unsafe entry %q", name)
			}
		}
	})
}

func FuzzCrossAuthorityHeaders(f *testing.F) {
	for _, seed := range []string{"secret", "", "MediaBrowser Client=\"test\", Token=\"secret\""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		header := make(http.Header)
		header.Set("Authorization", value)
		header.Set("Proxy-Authorization", value)
		header.Set("Cookie", value)
		header.Set("X-Emby-Token", value)
		header.Set("X-MediaBrowser-Token", value)
		header.Set("X-Emby-Authorization", value)
		stripSensitiveRedirectHeaders(header)
		for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Emby-Token", "X-MediaBrowser-Token"} {
			if header.Get(name) != "" {
				t.Fatalf("sensitive header survived cross-authority stripping: %s", name)
			}
		}
	})
}

func FuzzEmbyAuthorizationParser(f *testing.F) {
	for _, seed := range []string{
		`MediaBrowser Client="Meridian", DeviceId="test"`,
		`Emby Token="abc"`,
		`Bearer token`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(_ *testing.T, value string) {
		_, _ = parseEmbyAuthorizationAttributes(value, 0)
	})
}

func FuzzDynamicCapabilityClaims(f *testing.F) {
	key := sha256.Sum256([]byte("meridian-fuzz-key"))
	for _, seed := range []string{"", "!", "AAECAwQFBgcICQ"} {
		f.Add(seed)
	}
	f.Fuzz(func(_ *testing.T, token string) {
		_, _ = openDynamicCapability(key[:], token)
	})
}
