package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func preservedManifestFixture(source, reference string) string {
	switch source {
	case "hls-key":
		return "#EXTM3U\n#EXT-X-VENDOR-TAG:value=1\n#EXT-X-TARGETDURATION:4\n#EXT-X-KEY:METHOD=AES-128,URI=\"" + reference + "\"\n#EXTINF:4,\nsegment.ts\n#EXT-X-ENDLIST\n"
	case dynamicDiscoverySourceDASH:
		return `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:v="urn:vendor:ns"><v:Custom foo="bar"/><BaseURL>` + reference + `</BaseURL><Period><AdaptationSet><Representation id="video" bandwidth="1"><SegmentTemplate media="seg-$Number$.m4s" initialization="init.mp4"/></Representation></AdaptationSet></Period></MPD>`
	default:
		return "#EXTM3U\n#EXT-X-VENDOR-TAG:value=1\n#EXT-X-TARGETDURATION:4\n#EXTINF:4,\n" + reference + "\n#EXT-X-ENDLIST\n"
	}
}

func rewritePreservedManifestFixture(t *testing.T, source, payload string) (*http.Response, error) {
	t.Helper()
	contentType := "application/vnd.apple.mpegurl"
	requestURL := "https://origin.example.com/live/master.m3u8"
	if source == dynamicDiscoverySourceDASH {
		contentType = "application/dash+xml"
		requestURL = "https://origin.example.com/live/manifest.mpd"
	} else {
		source = dynamicDiscoverySourceHLS
	}
	response := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {contentType}},
		Body:          io.NopCloser(strings.NewReader(payload)),
		ContentLength: int64(len(payload)),
		Request:       &http.Request{Method: http.MethodGet, URL: mustStructuredURL(t, requestURL)},
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	err := rewriteDynamicStructuredResponseExpected(response, newStructuredDiscoveryTestIssuer(t), false, source, 0, false)
	return response, err
}

func TestPreservedManifestRejectsSchemeRelativeLocalAuthority(t *testing.T) {
	for _, source := range []string{dynamicDiscoverySourceHLS, "hls-key", dynamicDiscoverySourceDASH} {
		t.Run(source, func(t *testing.T) {
			payload := preservedManifestFixture(source, "//localhost/media/")
			if _, err := rewritePreservedManifestFixture(t, source, payload); err == nil {
				t.Fatal("compatibility preserve exposed a local network authority")
			}
		})
	}
}

func TestPreservedManifestRejectsNumericPrivateAuthority(t *testing.T) {
	// These hosts normalize to RFC1918 or link-local addresses under the
	// WHATWG IPv4 rules: 172.16.0.0, 192.168.0.1, and 169.254.0.0.
	for _, host := range []string{
		"172.1048576", "192.11010049", "169.16646144",
		"0xac.0x100000", "0254.04000000", "0x", "0x.1", "１２７.１",
	} {
		t.Run(host, func(t *testing.T) {
			for _, source := range []string{dynamicDiscoverySourceHLS, "hls-key", dynamicDiscoverySourceDASH} {
				t.Run(source, func(t *testing.T) {
					payload := preservedManifestFixture(source, "http://"+host+"/media/")
					if _, err := rewritePreservedManifestFixture(t, source, payload); err == nil {
						t.Fatal("compatibility preserve exposed a private numeric authority")
					}
				})
			}
		})
	}
}

func TestPreservedURLScanIPv4FormsMatchWHATWG(t *testing.T) {
	// Expected classifications were checked against Node's WHATWG URL parser.
	// Invalid spellings do not name an IPv4 destination and must not be
	// misclassified as a private address by Go-specific numeric extensions.
	cases := []struct {
		host      string
		nonPublic bool
	}{
		{"172.1048576", true},
		{"192.11010049", true},
		{"169.16646144", true},
		{"172.16.257", true},
		{"169.254.256", true},
		{"0xac.0x100000", true},
		{"0254.04000000", true},
		{"0x7f.1", true},
		{"0177.01", true},
		{"0x", true},
		{"0x.1", true},
		{"127.0x", true},
		{"0.0x", true},
		{"１２７.１", true},
		{"0xffffffff", true},
		{"0x1000000", false},
		{"172.16777215", false},
		{"1.2.65535", false},
		{"1.2.3.255", false},
		{"１.１", false},
		{"172.16777216", false},
		{"1.2.65536", false},
		{"1.2.3.256", false},
		{"0x100000000", false},
		{"256.1.1.1", false},
		{"0o177.1", false},
		{"0b1111111.1", false},
		{"0x7_f.1", false},
		{"+127.1", false},
		{"127.08", false},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			if got := dynamicScanHostIsNonPublicShorthand(tc.host); got != tc.nonPublic {
				t.Fatalf("private numeric authority classification = %t, want %t", got, tc.nonPublic)
			}
		})
	}
}

func TestPreservedURLScanAuthorityRelativeBoundaries(t *testing.T) {
	cases := []struct {
		reference string
		safe      bool
	}{
		{"//cdn.example.com/media/", true},
		{"//cdn.example.com:443/media/", true},
		{"//cdn.example.com:8443/media/", true},
		{"//cdn.example.com:65535/media/", true},
		{"//cdn.example.com:/media/", true},
		{"//1.1/media/", true},
		{"//localhost/media/", false},
		{"//localhost./media/", false},
		{"//sub.localhost/media/", false},
		{"//127.1/media/", false},
		{"//172.1048576/media/", false},
		{"//[::1]/media/", false},
		{"//user:demo@cdn.example.com/media/", false},
		{"//cdn.example.com:0/media/", false},
		{"//cdn.example.com:65536/media/", false},
		{"//cdn.example.com:invalid/media/", false},
		{"//cdn.example.com/media/#fragment", false},
		{"//cdn.example.com/a/../media/", false},
		{"//cdn.example.com/%00/media/", false},
		{"//[not-an-address]/media/", false},
		{"//", false},
		{"///localhost/media/", false},
		{`/\localhost/media/`, false},
		{`\/localhost/media/`, false},
		{`\\localhost/media/`, false},
	}
	for _, tc := range cases {
		t.Run(tc.reference, func(t *testing.T) {
			candidate, safe := dynamicURLScanCandidate(tc.reference)
			if !candidate || safe != tc.safe {
				t.Fatalf("network reference classification = (%t, %t), want (true, %t)", candidate, safe, tc.safe)
			}
		})
	}
}

func TestPreservedManifestKeepsRoutableCompatibility(t *testing.T) {
	for _, reference := range []string{
		"segment.ts", "../segment.ts", "/media/segment.ts",
		"//cdn.example.com/media/", "//cdn.example.com:8443/media/",
		"//1.1/media/", "http://172.16777215/media/",
		"http://1.2.65535/media/", "http://１.１/media/",
	} {
		t.Run(reference, func(t *testing.T) {
			for _, source := range []string{dynamicDiscoverySourceHLS, "hls-key", dynamicDiscoverySourceDASH} {
				t.Run(source, func(t *testing.T) {
					payload := preservedManifestFixture(source, reference)
					response, err := rewritePreservedManifestFixture(t, source, payload)
					if err != nil {
						candidate, unsafe := scanDASHManifestURLs([]byte(payload))
						if source == dynamicDiscoverySourceHLS {
							candidate, unsafe = scanHLSManifestURLs([]byte(payload))
						}
						t.Fatalf("routable compatibility manifest was rejected: %v (candidate=%t unsafe=%t)", err, candidate, unsafe)
					}
					output, err := io.ReadAll(response.Body)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(output, []byte(payload)) {
						t.Fatal("routable compatibility manifest was not preserved verbatim")
					}
				})
			}
		})
	}
}

func TestPreservedManifestKeepsDRMCompatibility(t *testing.T) {
	cases := []struct {
		source  string
		payload string
	}{
		{
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"skd://asset\",KEYFORMAT=\"com.apple.streamingkeydelivery\"\n#EXTINF:4,\nsegment.ts\n#EXT-X-ENDLIST\n",
		},
		{
			source:  dynamicDiscoverySourceDASH,
			payload: `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:cenc="urn:mpeg:cenc:2013"><Period><AdaptationSet><ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc"><cenc:pssh>c3ludGhldGlj</cenc:pssh></ContentProtection><Representation id="video" bandwidth="1"/></AdaptationSet></Period></MPD>`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			response, err := rewritePreservedManifestFixture(t, tc.source, tc.payload)
			if err != nil {
				t.Fatalf("DRM compatibility manifest was rejected: %v", err)
			}
			output, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(output, []byte(tc.payload)) {
				t.Fatal("DRM compatibility manifest was not preserved verbatim")
			}
		})
	}
}

type preservedLongIPv4Fixture struct {
	name      string
	host      string
	canonical string
	nonPublic bool
}

func preservedLongIPv4Fixtures() []preservedLongIPv4Fixture {
	asciiZeros := strings.Repeat("0", 70)
	fullwidthZeros := strings.Repeat("０", 70)
	mixedZeros := strings.Repeat("0０", 35)
	longFullwidthZeros := strings.Repeat("０", 260)
	return []preservedLongIPv4Fixture{
		{"ascii loopback", asciiZeros + "177.1", "127.0.0.1", true},
		{"fullwidth loopback", fullwidthZeros + "１７７.１", "127.0.0.1", true},
		{"mixed loopback", mixedZeros + "1７7.１", "127.0.0.1", true},
		{"IDNA dot loopback", fullwidthZeros + "１７７。１", "127.0.0.1", true},
		{"long fullwidth loopback", longFullwidthZeros + "１７７.１", "127.0.0.1", true},
		{"fullwidth hex loopback", "0x" + fullwidthZeros + "７ｆ.１", "127.0.0.1", true},
		{"ascii public", asciiZeros + "1.1", "1.0.0.1", false},
		{"fullwidth public", fullwidthZeros + "１.１", "1.0.0.1", false},
		{"mixed public", mixedZeros + "１.1", "1.0.0.1", false},
		{"long fullwidth public", longFullwidthZeros + "１.１", "1.0.0.1", false},
		{"fullwidth hex public", "0x" + fullwidthZeros + "１.１", "1.0.0.1", false},
		{"invalid fullwidth octal", fullwidthZeros + "８.１", "", false},
		{"invalid fullwidth first part", fullwidthZeros + "４００.１", "", false},
		{"invalid fullwidth last part", "1." + fullwidthZeros + "１００００００００", "", false},
		{"invalid mixed octal", mixedZeros + "８.1", "", false},
		{"non-numeric trailing dots", fullwidthZeros + "１７７.１..", asciiZeros + "177.1..", false},
	}
}

func TestPreservedURLScanLongIPv4FormsMatchWHATWG(t *testing.T) {
	// WHATWG consumes IDNA-mapped numbers before any DNS label-length
	// checks. The canonical hosts and invalid forms are fixed results
	// independently verified with Node's URL parser.
	for _, tc := range preservedLongIPv4Fixtures() {
		t.Run(tc.name, func(t *testing.T) {
			if got := dynamicScanHostIsNonPublicShorthand(tc.host); got != tc.nonPublic {
				t.Fatalf("long numeric authority classification = %t, want %t (WHATWG host %q)", got, tc.nonPublic, tc.canonical)
			}
		})
	}
}

func TestPreservedManifestLongIDNANumericAuthority(t *testing.T) {
	for _, tc := range preservedLongIPv4Fixtures() {
		if tc.canonical == "" {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			for _, prefix := range []string{"http://", "//"} {
				t.Run(prefix, func(t *testing.T) {
					for _, source := range []string{dynamicDiscoverySourceHLS, "hls-key", dynamicDiscoverySourceDASH} {
						t.Run(source, func(t *testing.T) {
							payload := preservedManifestFixture(source, prefix+tc.host+"/media/")
							response, err := rewritePreservedManifestFixture(t, source, payload)
							if tc.nonPublic {
								if err == nil {
									t.Fatal("compatibility preserve exposed a long IDNA numeric private authority")
								}
								return
							}
							if err != nil {
								t.Fatalf("compatible long IDNA host manifest was rejected: %v", err)
							}
							output, err := io.ReadAll(response.Body)
							if err != nil {
								t.Fatal(err)
							}
							if !bytes.Equal(output, []byte(payload)) {
								t.Fatal("compatible long IDNA host manifest was not preserved verbatim")
							}
						})
					}
				})
			}
		})
	}
}
