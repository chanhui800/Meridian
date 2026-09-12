package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTrackedDNSRecordOwnershipDecisionMatrix pins the ownership decision that
// every mutating Cloudflare path must share. DELETE already refused records
// whose marker an operator had cleared; the tracked-record PUT must make the
// same decision, otherwise a scheduled reconcile silently reasserts Meridian's
// address and marker over a record the operator has taken back.
func TestTrackedDNSRecordOwnershipDecisionMatrix(t *testing.T) {
	const installUUID = "install-uuid-1"
	marker := siteDNSOwnershipMarker(7, installUUID)
	cases := []struct {
		name     string
		record   cloudflareAddressRecord
		siteID   int64
		wantErr  bool
		wantText string
	}{
		{
			name:   "current marker is owned",
			record: cloudflareAddressRecord{ID: "rec-1", Type: "A", Name: "site.example", Content: "203.0.113.5", Comment: marker},
			siteID: 7,
		},
		{
			// The pre-installation-UUID marker is still accepted so the next
			// reconcile PUT can migrate the comment to the scoped form.
			name:   "legacy marker is owned",
			record: cloudflareAddressRecord{ID: "rec-1", Type: "A", Name: "site.example", Content: "203.0.113.5", Comment: legacySiteDNSOwnershipMarker(7)},
			siteID: 7,
		},
		{
			// Renaming a site is supported, so ownership must not require the
			// stored hostname to still match.
			name:   "site rename with valid marker is owned",
			record: cloudflareAddressRecord{ID: "rec-1", Type: "A", Name: "renamed.example", Content: "203.0.113.5", Comment: marker},
			siteID: 7,
		},
		{
			name:     "operator-cleared comment is not owned",
			record:   cloudflareAddressRecord{ID: "rec-1", Type: "A", Name: "site.example", Content: "203.0.113.5", Comment: ""},
			siteID:   7,
			wantErr:  true,
			wantText: "ownership marker",
		},
		{
			name:     "operator comment is not owned",
			record:   cloudflareAddressRecord{ID: "rec-1", Type: "A", Name: "site.example", Content: "203.0.113.5", Comment: "managed by ops"},
			siteID:   7,
			wantErr:  true,
			wantText: "ownership marker",
		},
		{
			// Two controllers sharing a zone must never treat each other's
			// records as their own.
			name:     "foreign controller marker is not owned",
			record:   cloudflareAddressRecord{ID: "rec-1", Type: "A", Name: "site.example", Content: "203.0.113.5", Comment: siteDNSOwnershipMarker(7, "other-install")},
			siteID:   7,
			wantErr:  true,
			wantText: "ownership marker",
		},
		{
			name:     "another site's marker is not owned",
			record:   cloudflareAddressRecord{ID: "rec-1", Type: "A", Name: "site.example", Content: "203.0.113.5", Comment: siteDNSOwnershipMarker(8, installUUID)},
			siteID:   7,
			wantErr:  true,
			wantText: "ownership marker",
		},
		{
			name:     "operator-replaced CNAME is not owned",
			record:   cloudflareAddressRecord{ID: "rec-1", Type: "CNAME", Name: "site.example", Content: "target.example", Comment: marker},
			siteID:   7,
			wantErr:  true,
			wantText: "A/AAAA",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := verifySiteDNSRecordOwnership(testCase.record, testCase.siteID, installUUID, "refusing to overwrite it")
			if testCase.wantErr {
				if err == nil || !strings.Contains(err.Error(), testCase.wantText) {
					t.Fatalf("err = %v, want containing %q", err, testCase.wantText)
				}
				return
			}
			if err != nil {
				t.Fatalf("owned record rejected: %v", err)
			}
		})
	}
}

// TestTrackedDNSRecordReadUsesByIDPreimage proves the read helper returns the
// record exactly as Cloudflare holds it. That value is the compensation
// preimage, so a name-scoped listing would be wrong twice over: it cannot find
// a record the operator renamed, and it cannot prove ownership of the ID it
// does return.
func TestTrackedDNSRecordReadUsesByIDPreimage(t *testing.T) {
	var sawLookup string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawLookup = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"result":{"id":"rec-9","type":"A","name":"old.example","content":"203.0.113.9","comment":"Meridian controller=install-uuid-1 site=7"}}`))
	}))
	defer server.Close()
	cf := &cloudflareClient{token: "test", httpClient: server.Client(), apiBase: server.URL, installUUID: "install-uuid-1"}
	schedule := SiteNodeSchedule{SiteID: 7, PublicHost: "renamed.example", cfZoneID: "zone-1", cfRecordID: "rec-9"}
	record, err, owned := readOwnedTrackedSiteDNSRecord(context.Background(), cf, schedule)
	if err != nil || !owned {
		t.Fatalf("owned=%v err=%v, want the tracked record to be owned", owned, err)
	}
	if !strings.HasSuffix(sawLookup, "/dns_records/rec-9") {
		t.Fatalf("lookup path = %q, want a by-ID read", sawLookup)
	}
	if record.Name != "old.example" || record.Content != "203.0.113.9" || record.Comment != siteDNSOwnershipMarker(7, "install-uuid-1") {
		t.Fatalf("preimage = %+v, want the record exactly as stored", record)
	}
}

// TestTrackedDNSRecordReadReportsMissingRecordWithoutOwnershipError keeps the
// existing missing-record recovery reachable: a 404 must not be reported as an
// ownership conflict, or a deleted record could never be recreated.
func TestTrackedDNSRecordReadReportsMissingRecordWithoutOwnershipError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":81044,"message":"record does not exist"}]}`))
	}))
	defer server.Close()
	cf := &cloudflareClient{token: "test", httpClient: server.Client(), apiBase: server.URL, installUUID: "install-uuid-1"}
	schedule := SiteNodeSchedule{SiteID: 7, PublicHost: "site.example", cfZoneID: "zone-1", cfRecordID: "rec-missing"}
	_, err, owned := readOwnedTrackedSiteDNSRecord(context.Background(), cf, schedule)
	if err != nil || owned {
		t.Fatalf("owned=%v err=%v, want a missing record to be neither owned nor a conflict", owned, err)
	}
}

// TestTrackedDNSRecordReadRefusesForeignOwnership is the same guard seen
// through the HTTP layer: the refusal must come from the record's own comment,
// not from the hostname the schedule happens to store.
func TestTrackedDNSRecordReadRefusesForeignOwnership(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"result":{"id":"rec-1","type":"A","name":"site.example","content":"198.51.100.7","comment":""}}`))
	}))
	defer server.Close()
	cf := &cloudflareClient{token: "test", httpClient: server.Client(), apiBase: server.URL, installUUID: "install-uuid-1"}
	schedule := SiteNodeSchedule{SiteID: 7, PublicHost: "site.example", cfZoneID: "zone-1", cfRecordID: "rec-1"}
	_, err, owned := readOwnedTrackedSiteDNSRecord(context.Background(), cf, schedule)
	if err == nil || owned {
		t.Fatalf("owned=%v err=%v, want an operator-cleared record to be refused", owned, err)
	}
	if !strings.Contains(err.Error(), "ownership marker") {
		t.Fatalf("err = %v, want an ownership refusal", err)
	}
}

// TestPreservedStructuredBodyRejectsUnroutableURLs covers the gap between "the
// strict parser refused this manifest" and "the original bytes are safe to
// forward". A parser stops at the first unsupported feature, so a URL that
// appears later never reaches per-URL validation and would otherwise be handed
// to the player verbatim.
func TestPreservedStructuredBodyRejectsUnroutableURLs(t *testing.T) {
	cases := []struct {
		name    string
		source  string
		payload string
		safe    bool
	}{
		{
			// The DRM branch fails before the key URI is ever validated, so the
			// loopback key URL must be caught by the preserve scan instead.
			name:    "hls unsupported drm key with public uri",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"https://cdn.example/key\"\n#EXTINF:4.0,\nseg1.ts\n",
			safe:    true,
		},
		{
			name:    "hls key uri with userinfo",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"https://user:pass@cdn.example/key\"\n#EXTINF:4.0,\nseg1.ts\n",
			safe:    false,
		},
		{
			name:    "hls key uri with fragment",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"https://cdn.example/key#frag\"\n#EXTINF:4.0,\nseg1.ts\n",
			safe:    false,
		},
		{
			name:    "hls uri line with dot segments",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXTINF:4.0,\nhttp://cdn.example/a/../b/seg.ts\n",
			safe:    false,
		},
		{
			name:    "hls media uri line with userinfo",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXTINF:4.0,\nhttps://user@cdn.example/seg.ts\n",
			safe:    false,
		},
		{
			// Ordinary vendor tags and relative URIs must still preserve.
			name:    "hls vendor tag with relative uris",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXT-X-VENDOR-TAG:value=1\n#EXTINF:4.0,\nseg1.ts\n",
			safe:    true,
		},
		{
			name:    "hls absolute https uri is safe",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"https://cdn.example/key\"\n#EXTINF:4.0,\nseg1.ts\n",
			safe:    true,
		},
		{
			name:    "hls keyformats identifier is not a candidate",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,KEYFORMAT=\"com.apple.streamingkeydelivery\",URI=\"https://cdn.example/key\"\n#EXTINF:4.0,\nseg1.ts\n",
			safe:    true,
		},
		{
			name:    "dash attribute with loopback base url",
			source:  dynamicDiscoverySourceDASH,
			payload: `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><BaseURL>http://127.0.0.1:8080/</BaseURL></Period></MPD>`,
			safe:    false,
		},
		{
			name:    "dash text node with userinfo",
			source:  dynamicDiscoverySourceDASH,
			payload: `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><SegmentTemplate media="https://user@cdn.example/$Number$.m4s"/></Period></MPD>`,
			safe:    false,
		},
		{
			name:    "dash unsupported feature with public url preserves",
			source:  dynamicDiscoverySourceDASH,
			payload: `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:x="urn:vendor"><x:Vendor><BaseURL>https://cdn.example/</BaseURL></x:Vendor></MPD>`,
			safe:    true,
		},
		{
			name:    "dash scheme id uri is not a candidate",
			source:  dynamicDiscoverySourceDASH,
			payload: `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc"/></Period></MPD>`,
			safe:    true,
		},
		// Malformed use of the IPv6-literal brackets must still be refused; only
		// the well-formed `[address]` / `[address]:port` shapes are URLs.
		{
			name:    "hls unbalanced ipv6 bracket",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXTINF:4.0,\nhttps://[2606:4700::1111/seg.ts\n",
			safe:    false,
		},
		{
			name:    "hls bracketed non-literal host",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXTINF:4.0,\nhttps://[not-an-address]/seg.ts\n",
			safe:    false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := preservedStructuredBodyIsSafe(testCase.source, []byte(testCase.payload)); got != testCase.safe {
				t.Fatalf("preservedStructuredBodyIsSafe = %v, want %v", got, testCase.safe)
			}
		})
	}
}

// TestPreservedStructuredBodyScanAcceptsRealisticManifests guards the other
// direction of the preserve scan: it must not reject ordinary manifests. Each
// sample below is a construct real HLS/DASH output emits (vendor DRM key
// formats, stringified segment templates, public IPv6 literals, base64 DRM
// metadata, XML comments), and a false positive here would turn a working
// stream into a hard failure.
func TestPreservedStructuredBodyScanAcceptsRealisticManifests(t *testing.T) {
	hlsSamples := map[string]string{
		"widevine key with public uri": "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"https://cdn.example/key\",KEYFORMAT=\"urn:uuid:edef8ba9\"\n#EXTINF:4,\nseg.ts\n",
		"fairplay skd uri":             "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"skd://asset\",KEYFORMAT=\"com.apple.streamingkeydelivery\"\n#EXTINF:4,\nseg.ts\n",
		"iframe stream inf":            "#EXTM3U\n#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=1000,URI=\"https://cdn.example/iframe.m3u8\"\n",
		"interstitial asset uri":       "#EXTM3U\n#EXT-X-DATERANGE:ID=\"1\",X-ASSET-URI=\"https://ads.example/ad.m3u8\"\n#EXTINF:4,\nseg.ts\n",
		"relative query string":        "#EXTM3U\n#EXTINF:4,\nseg.ts?token=abc&x=1\n",
		"absolute with port":           "#EXTM3U\n#EXTINF:4,\nhttps://cdn.example:8443/a/b/seg.ts\n",
		"encoded path component":       "#EXTM3U\n#EXTINF:4,\nhttps://cdn.example/a%20b/seg.ts\n",
		"public ipv6 literal":          "#EXTM3U\n#EXTINF:4,\nhttps://[2606:4700::1111]/seg.ts\n",
		"byterange and date time":      "#EXTM3U\n#EXT-X-PROGRAM-DATE-TIME:2026-01-01T00:00:00Z\n#EXTINF:4,\n#EXT-X-BYTERANGE:1000@0\nseg.ts\n",
	}
	for name, payload := range hlsSamples {
		if !preservedStructuredBodyIsSafe(dynamicDiscoverySourceHLS, []byte(payload)) {
			t.Fatalf("realistic HLS manifest %q must still preserve: %s", name, payload)
		}
	}
	dashSamples := map[string]string{
		"cenc pssh base64":     `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:cenc="urn:mpeg:cenc:2013"><Period><ContentProtection schemeIdUri="urn:mpeg:dash:mp4protection:2011" value="cenc"><cenc:pssh>AAAAVnBzc2gAAAAA7e+LqXnWSs6jyCfc1R0h7QAAADYSEAqAgLQ0FJGTEJOLd4tQKSsaDXdpZGV2aW5lX3Rlc3QiDHRlc3RfY29udGVudA==</cenc:pssh></ContentProtection></Period></MPD>`,
		"utc timing http":      `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><UTCTiming schemeIdUri="urn:mpeg:dash:utc:http-xsdate:2014" value="https://time.example/utc"/></MPD>`,
		"stringified template": `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet><SegmentTemplate media="$RepresentationID$/seg-$Number$.m4s" initialization="$RepresentationID$/init.mp4" timescale="1000"/></AdaptationSet></Period></MPD>`,
		"vendor extension":     `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:v="urn:vendor:ns"><v:Custom foo="bar"/></MPD>`,
		"comment with url":     `<?xml version="1.0"?><!-- generated from https://cdn.example/source --><MPD xmlns="urn:mpeg:dash:schema:mpd:2011"/>`,
		"public base url":      `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><BaseURL>https://cdn.example/dash/</BaseURL></MPD>`,
	}
	for name, payload := range dashSamples {
		if !preservedStructuredBodyIsSafe(dynamicDiscoverySourceDASH, []byte(payload)) {
			t.Fatalf("realistic DASH manifest %q must still preserve: %s", name, payload)
		}
	}
}

// TestStructuredPreserveRefusesUnroutableURLThroughProxy drives the same rule
// through the real response path: a manifest the strict rewriter rejects must
// not be preserved verbatim when it carries a URL the client would follow.
func TestStructuredPreserveRefusesUnroutableURLThroughProxy(t *testing.T) {
	issuer := newStructuredDiscoveryTestIssuer(t)
	manifest := "#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"http://127.0.0.1:9999/key\"\n#EXTINF:4.0,\nseg1.ts\n"
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/vnd.apple.mpegurl"}},
		Body:       io.NopCloser(strings.NewReader(manifest)),
		Request:    &http.Request{Method: http.MethodGet, URL: mustStructuredURL(t, "https://origin.example.com/live/master.m3u8")},
	}
	if err := rewriteDynamicStructuredResponseExpected(response, issuer, false, dynamicDiscoverySourceHLS, 0, false); err == nil {
		t.Fatal("a manifest carrying an unroutable URL must not be preserved")
	}

	// A manifest with only routable URLs still preserves, so ordinary vendor
	// tags keep working.
	benign := "#EXTM3U\n#EXT-X-VENDOR-TAG:value=1\n#EXTINF:4.0,\nseg1.ts\n"
	benignResponse := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/vnd.apple.mpegurl"}},
		Body:       io.NopCloser(strings.NewReader(benign)),
		Request:    &http.Request{Method: http.MethodGet, URL: mustStructuredURL(t, "https://origin.example.com/live/master.m3u8")},
	}
	if err := rewriteDynamicStructuredResponseExpected(benignResponse, issuer, false, dynamicDiscoverySourceHLS, 0, false); err != nil {
		t.Fatalf("benign vendor manifest must still preserve: %v", err)
	}
}

// TestPreservedStructuredBodyRejectsURLsHiddenFromTheStrictParser covers the
// extraction gaps the first version of the preserve scan left open: a URL in a
// tag or attribute the strict rewriter never reached, a URL hidden behind a
// syntax error, a CDATA text node, and an entity-encoded scheme. Each of these
// would otherwise reach the player verbatim.
func TestPreservedStructuredBodyRejectsURLsHiddenFromTheStrictParser(t *testing.T) {
	cases := []struct {
		name    string
		source  string
		payload string
	}{
		{
			// #EXT-X-CONTENT-STEERING is refused outright by the strict parser,
			// so its SERVER-URI never goes through per-URL validation.
			name:    "hls content steering server uri",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXT-X-CONTENT-STEERING:SERVER-URI=\"http://127.0.0.1:9999/v1\",PATHWAY-ID=\"CDN-A\"\n#EXTINF:4.0,\nseg1.ts\n",
		},
		{
			// VALUE feeds {$k} substitution, so a loopback URL placed there
			// reaches the key request after the player expands it.
			name:    "hls define value holding a loopback url",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXT-X-DEFINE:NAME=\"k\",VALUE=\"http://127.0.0.1:9999/key\"\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"{$k}\"\n#EXTINF:4.0,\nseg.ts\n",
		},
		{
			// A malformed attribute list must not hide a URL earlier on the line.
			name:    "hls malformed attribute list hides a url",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXT-X-DATERANGE:ID=\"1\",X-ASSET-URI=\"https://user@cdn.example/ad.m3u8\",bad\n#EXTINF:4.0,\nseg.ts\n",
		},
		{
			name:    "dash cdata base url",
			source:  dynamicDiscoverySourceDASH,
			payload: "<?xml version=\"1.0\"?><MPD xmlns=\"urn:mpeg:dash:schema:mpd:2011\"><Period><BaseURL><![CDATA[http://127.0.0.1:8080/]]></BaseURL></Period></MPD>",
		},
		{
			name:    "dash entity encoded scheme",
			source:  dynamicDiscoverySourceDASH,
			payload: `<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><BaseURL>&#104;ttp://127.0.0.1:8080/</BaseURL></Period></MPD>`,
		},
		{
			name:    "hls localhost shorthand",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXTINF:4.0,\nhttp://localhost/seg.ts\n",
		},
		{
			name:    "hls short form ipv4",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXTINF:4.0,\nhttp://127.1/seg.ts\n",
		},
		{
			name:    "hls decimal ipv4",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXTINF:4.0,\nhttp://2130706433/seg.ts\n",
		},
		{
			name:    "hls hex ipv4",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXTINF:4.0,\nhttp://0x7f000001/seg.ts\n",
		},
		{
			name:    "hls ipv4 mapped ipv6 literal",
			source:  dynamicDiscoverySourceHLS,
			payload: "#EXTM3U\n#EXTINF:4.0,\nhttps://[::ffff:127.0.0.1]/seg.ts\n",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if preservedStructuredBodyIsSafe(testCase.source, []byte(testCase.payload)) {
				t.Fatalf("unroutable URL was not detected in: %s", testCase.payload)
			}
		})
	}
}

// TestScanHostNonPublicShorthandRecognizesResolverSpellings pins the numeric
// host forms a resolver accepts but net.ParseIP does not. Getting one wrong is
// a real regression in either direction: a missed loopback spelling leaks the
// address to the client, and a false positive turns a working manifest into a
// hard failure.
func TestScanHostNonPublicShorthandRecognizesResolverSpellings(t *testing.T) {
	nonPublic := []string{
		"localhost", "LOCALHOST", "localhost.", "sub.localhost",
		"127.1", "127.0.1", "127.0.0.1", "2130706433", "0x7f000001", "0177.0.0.1",
		"10.1", "192.168", "169.254.1.1", "0.0.0.0", "0",
	}
	for _, host := range nonPublic {
		if !dynamicScanHostIsNonPublicShorthand(host) {
			t.Fatalf("host %q must be recognized as non-public", host)
		}
	}
	public := []string{
		"cdn.example.com", "example.com", "", "not-an-address",
		"8.8.8.8", "1.1", "203.0.114.1", "2606:4700::1111",
		"1.2.3.4.5", "256.1.1.1",
	}
	for _, host := range public {
		if dynamicScanHostIsNonPublicShorthand(host) {
			t.Fatalf("host %q must not be reported as non-public", host)
		}
	}
}

// TestPlaybackInfoFallbackFailureIsHard drives the whole response path. It has
// to enter the automatic fallback for real, which requires the strict walker to
// fail with one of the compatibility-only diagnostics: a backslash inside a
// subtitle URL is the reachable case (delivery percent-escapes and Protocol
// mismatches produce other codes). The payload then also carries a stream URL
// the walker cannot route, so the fallback's own failure is what the response
// disposition must reflect. Preserving the upstream body there would hand the
// client every URL in it unproxied, so one unroutable field would cost the
// capability, the accounting and the quota for all of them.
func TestPlaybackInfoFallbackFailureIsHard(t *testing.T) {
	issuer := newStructuredDiscoveryTestIssuer(t)
	base := mustStructuredURL(t, "http://line.example.com/Items/1/PlaybackInfo")

	// The strict walker must really hand over to the fallback for this payload;
	// otherwise the assertion below would be satisfied by an earlier failure and
	// would keep passing with the fallback's fix reverted.
	strict := &dynamicRewriteSession{ctx: context.Background(), issuer: issuer, base: base, source: dynamicDiscoverySourcePlaybackInfo}
	_, strictErr := rewritePlaybackInfoResponse([]byte(fallbackFailureProbePayload), strict)
	strict.rollback()
	if strictErr == nil || !playbackInfoAutomaticFallbackAllowed(strictErr) {
		t.Fatalf("payload does not enter the automatic fallback: err=%v code=%s", strictErr, playbackInfoRewriteDiagnosticCode(strictErr))
	}
	// And the walker must really be the thing that refuses it.
	fallback := &dynamicRewriteSession{ctx: context.Background(), issuer: issuer, base: base, source: dynamicDiscoverySourcePlaybackInfo}
	if _, fallbackErr := rewriteAutomaticPlaybackInfoResponse([]byte(fallbackFailureProbePayload), fallback); fallbackErr == nil {
		t.Fatal("the automatic walker unexpectedly proxied every URL in the payload")
	}
	fallback.rollback()

	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(fallbackFailureProbePayload)),
		Request:    &http.Request{Method: http.MethodGet, URL: base},
	}
	err := rewriteDynamicStructuredResponseExpected(response, issuer, false, dynamicDiscoverySourcePlaybackInfo, 0, false)
	if err == nil {
		t.Fatal("a payload the fallback cannot proxy must fail the response")
	}
	// The failure must be a dynamic proxy error (which the proxy turns into a
	// 502), not a silently installed body.
	var discoveryErr *dynamicProxyError
	if !errors.As(err, &discoveryErr) {
		t.Fatalf("failure = %v, want a dynamic proxy error", err)
	}
}

// fallbackFailureProbePayload fails the strict walker with the reachable
// compatibility-only url_backslash diagnostic, and fails the automatic walker
// on the unroutable subtitle URL.
//
// Field order matters: the strict walker validates DirectStreamUrl before the
// MediaStreams entries, so the backslash URL has to be the DirectStreamUrl for
// the compatibility-only diagnostic to be the one that surfaces.
const fallbackFailureProbePayload = `{"MediaSources":[{"DirectStreamUrl":"http://line.example.com/a\\b","MediaStreams":[{"DeliveryUrl":"http://backend.invalidtld/subtitle.vtt","IsExternalUrl":true}]}]}`

// TestPlaybackInfoAutomaticFallbackIsNotEnteredForSecurityFailures tightens the
// gate that decides whether the schema-free walker may take over. That walker
// is the path that keeps individual URLs on the proxy, so entering it after a
// security decision would let a URL the strict walker refused be re-decided
// without the same rules.
func TestPlaybackInfoAutomaticFallbackIsNotEnteredForSecurityFailures(t *testing.T) {
	allowed := []error{
		errorsNew("invalid discovered URL: surrounding whitespace"),
		errorsNew("invalid discovered URL: parse"),
		errorsNew("invalid discovered URL: backslash"),
		errorsNew("invalid discovered URL: unsafe character"),
	}
	for _, err := range allowed {
		if !playbackInfoAutomaticFallbackAllowed(err) {
			t.Fatalf("compatibility-only failure must allow fallback: %v", err)
		}
	}
	// No error at all is not a fallback decision.
	if playbackInfoAutomaticFallbackAllowed(nil) {
		t.Fatal("a nil error must not request a fallback")
	}
	blocked := []error{
		errorsNew("invalid discovered URL: userinfo"),
		errorsNew("invalid discovered URL: fragment"),
		errorsNew("invalid discovered URL: target normalization host"),
		errorsNew("invalid discovered URL: target normalization dot_segments"),
		errorsNew("invalid discovered URL: target normalization scheme"),
		errorsNew("invalid discovered URL: target normalization port"),
		errorsNew("invalid discovered URL: target normalization escaped_component"),
		errorsNew("invalid trusted capability URL"),
		errorsNew("discovered URL count exceeds its limit"),
		errorsNew("structured response output exceeds its limit"),
		newDynamicPolicyDenialError(errorsNew("capability denied")),
	}
	for _, err := range blocked {
		if playbackInfoAutomaticFallbackAllowed(err) {
			t.Fatalf("security decision must not allow fallback: %v", err)
		}
	}
}

// TestPlaybackInfoAutomaticFallbackRefusesUnproxyableURL proves the walker
// itself no longer preserves a URL it cannot route. Preserving it would hand
// the player the upstream destination, bypassing the capability, its traffic
// accounting and its quota.
func TestPlaybackInfoAutomaticFallbackRefusesUnproxyableURL(t *testing.T) {
	issuer := newStructuredDiscoveryTestIssuer(t)
	base := mustStructuredURL(t, "http://line.example.com/Items/1/PlaybackInfo")
	session := &dynamicRewriteSession{ctx: context.Background(), issuer: issuer, base: base, source: dynamicDiscoverySourcePlaybackInfo}
	root := map[string]any{"DirectStreamUrl": "http://backend.invalidtld/stream.mkv"}
	if _, err := rewriteAutomaticPlaybackInfoValue(root, session, 0, ""); err == nil {
		t.Fatal("an unproxyable URL must fail the response instead of being preserved")
	}

	// A relative playback path is not a network destination, so it is kept and
	// learned exactly as before.
	relative := map[string]any{"DirectStreamUrl": "/Videos/1/original.mkv"}
	rewritten, err := rewriteAutomaticPlaybackInfoValue(relative, session, 0, "")
	if err != nil {
		t.Fatalf("relative playback path must be preserved: %v", err)
	}
	object, ok := rewritten.(map[string]any)
	if !ok || object["DirectStreamUrl"] != "/Videos/1/original.mkv" {
		t.Fatalf("relative playback path changed: %#v", rewritten)
	}
}

// TestInstallationIdentityRotationIsIndependent pins the clone-vs-takeover
// distinction: a controller cloned from another controller's backup must be
// able to adopt its own identity, or both installations would present the same
// Cloudflare ownership marker.
func TestInstallationIdentityRotationIsIndependent(t *testing.T) {
	app := newTestApp(t)
	original := app.db.installUUID
	if len(original) != 32 {
		t.Fatalf("installation UUID = %q, want 32 hex characters", original)
	}
	rotated, err := app.db.rotateInstallationUUID()
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated == original || len(rotated) != 32 {
		t.Fatalf("rotated identity = %q, want a new 32-character value", rotated)
	}
	if app.db.installUUID != rotated {
		t.Fatalf("cached identity = %q, want %q", app.db.installUUID, rotated)
	}
	var stored string
	if err := app.db.db.QueryRow("SELECT install_uuid FROM installation_meta WHERE id=1").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != rotated {
		t.Fatalf("stored identity = %q, want %q", stored, rotated)
	}
	// Records carrying the previous identity must stop being treated as owned.
	if siteDNSMarkerOwned(siteDNSOwnershipMarker(7, original), 7, rotated) {
		t.Fatal("a record scoped to the previous identity must not be owned")
	}
	if !siteDNSMarkerOwned(siteDNSOwnershipMarker(7, rotated), 7, rotated) {
		t.Fatal("a record scoped to the new identity must be owned")
	}
}

// TestTLSRollbackPreflightRejectsIncompleteSnapshot proves the rollback refuses
// to start when its TLS tree cannot restore everything the manifest claims.
// The rollback deletes the live database first and the live TLS namespace
// after, so an incomplete snapshot discovered mid-flight would destroy both
// copies of the state.
func TestTLSRollbackPreflightRejectsIncompleteSnapshot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "meridian.db")
	rollback := dbPath + backupRollbackSuffix
	ownedRoot := filepath.Join(filepath.Dir(dbPath), "tls")

	writeSnapshot := func(t *testing.T, snapshot tlsNamespaceSnapshot) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(rollback, "tls-tree"), 0o700); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rollback, "tls-namespace.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("present owned root without a copy is refused", func(t *testing.T) {
		snapshot := tlsNamespaceSnapshot{
			Scope: tlsRestoreScope{OwnedRoots: []string{ownedRoot}},
			Owned: []tlsSnapshotRoot{{Path: ownedRoot, Present: true}},
		}
		writeSnapshot(t, snapshot)
		scope, legacy, err := tlsRollbackLayout(dbPath, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateTLSRollbackSnapshot(rollback, snapshot, scope, legacy); err == nil {
			t.Fatal("a missing rollback copy of a present root must be refused")
		}
	})

	t.Run("present root with an empty directory is accepted", func(t *testing.T) {
		snapshot := tlsNamespaceSnapshot{
			Scope: tlsRestoreScope{OwnedRoots: []string{ownedRoot}},
			Owned: []tlsSnapshotRoot{{Path: ownedRoot, Present: true}},
		}
		writeSnapshot(t, snapshot)
		if err := os.MkdirAll(filepath.Join(rollback, "tls-tree", "owned", "0"), 0o700); err != nil {
			t.Fatal(err)
		}
		scope, legacy, err := tlsRollbackLayout(dbPath, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateTLSRollbackSnapshot(rollback, snapshot, scope, legacy); err != nil {
			t.Fatalf("a copied empty root must be accepted: %v", err)
		}
	})

	t.Run("absent root needs no copy", func(t *testing.T) {
		snapshot := tlsNamespaceSnapshot{
			Scope: tlsRestoreScope{OwnedRoots: []string{ownedRoot}},
			Owned: []tlsSnapshotRoot{{Path: ownedRoot, Present: false}},
		}
		writeSnapshot(t, snapshot)
		scope, legacy, err := tlsRollbackLayout(dbPath, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateTLSRollbackSnapshot(rollback, snapshot, scope, legacy); err != nil {
			t.Fatalf("a root that never existed must not require a copy: %v", err)
		}
	})

	t.Run("present exact file without a copy is refused", func(t *testing.T) {
		exactPath := filepath.Join(filepath.Dir(dbPath), "panel.pem")
		snapshot := tlsNamespaceSnapshot{
			Scope: tlsRestoreScope{ExactPaths: []string{exactPath}},
			Exact: []tlsSnapshotPath{{Path: exactPath, Present: true}},
		}
		writeSnapshot(t, snapshot)
		scope, legacy, err := tlsRollbackLayout(dbPath, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateTLSRollbackSnapshot(rollback, snapshot, scope, legacy); err == nil {
			t.Fatal("a missing rollback copy of a present exact path must be refused")
		}
	})

	t.Run("manifest entry count mismatch is refused", func(t *testing.T) {
		other := filepath.Join(filepath.Dir(dbPath), "tls-other")
		snapshot := tlsNamespaceSnapshot{
			Scope: tlsRestoreScope{OwnedRoots: []string{ownedRoot, other}},
			Owned: []tlsSnapshotRoot{{Path: ownedRoot, Present: true}},
		}
		writeSnapshot(t, snapshot)
		scope, legacy, err := tlsRollbackLayout(dbPath, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateTLSRollbackSnapshot(rollback, snapshot, scope, legacy); err == nil {
			t.Fatal("an ownership list that disagrees with the scope must be refused")
		}
	})
}

// TestLegacyTLSRollbackUsesTheV1934TreeLayout pins the v1.9.34 compatibility
// path. Those snapshots carry only Roots and copied each owned root straight to
// tls-tree/<index>; the owned/ subdirectory only exists from v1.9.35. Reading
// the new layout for a legacy manifest would reject a snapshot that is in fact
// complete, and because the boot-time rollback runs before the panel starts,
// that rejection is an unbootable installation rather than a recoverable error.
// TestTLSRollbackLayoutMatchesTheManifestShape is the load-bearing test for the
// rollback tree layout. Two on-disk shapes exist and they must be told apart
// from the manifest alone:
//
//   - v1.9.34 wrote `roots` and copied each owned root to tls-tree/<index>;
//   - every release from v1.9.35 onwards writes a `scope` and copies to
//     tls-tree/owned/<index>.
//
// The `owned` presence metadata used by the preflight was itself added later
// than the whole v1.9.35..v1.9.88 range, so "the manifest has no owned field"
// is NOT a valid legacy test; using it (as an earlier revision of this fix did)
// misclassifies every snapshot those versions wrote and refuses a complete
// rollback tree, which on the boot path is an unbootable installation.
func TestTLSRollbackLayoutMatchesTheManifestShape(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meridian.db")
	rollback := dbPath + backupRollbackSuffix
	ownedRoot := filepath.Join(dir, "tls")
	if err := os.MkdirAll(rollback, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("scope carrying manifest uses the owned subdirectory", func(t *testing.T) {
		// Exactly what v1.9.35..v1.9.88 wrote: a scope, and no owned metadata
		// because that field did not exist yet.
		snapshot := tlsNamespaceSnapshot{Scope: tlsRestoreScope{OwnedRoots: []string{ownedRoot}}}
		scope, legacy, err := tlsRollbackLayout(dbPath, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if legacy {
			t.Fatal("a scope-carrying manifest must not be treated as v1.9.34")
		}
		if len(scope.OwnedRoots) != 1 || filepath.Clean(scope.OwnedRoots[0]) != filepath.Clean(ownedRoot) {
			t.Fatalf("scope = %#v, want the manifest's own owned roots", scope)
		}
		if err := os.MkdirAll(filepath.Join(rollback, "tls-tree", "owned", "0"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := validateTLSRollbackSnapshot(rollback, snapshot, scope, legacy); err != nil {
			t.Fatalf("a complete v1.9.35-style snapshot must pass the preflight: %v", err)
		}
	})

	t.Run("roots only manifest uses the legacy layout", func(t *testing.T) {
		snapshot := tlsNamespaceSnapshot{Roots: []string{ownedRoot}}
		scope, legacy, err := tlsRollbackLayout(dbPath, snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if !legacy {
			t.Fatal("a bare roots manifest must be treated as v1.9.34")
		}
		if len(scope.OwnedRoots) != 1 {
			t.Fatalf("scope = %#v, want the legacy roots", scope)
		}
		// The validator and the restore must agree on where the tree lives.
		if got, want := tlsRollbackOwnedPath(filepath.Join(rollback, "tls-tree"), 0, legacy), filepath.Join(rollback, "tls-tree", "0"); filepath.Clean(got) != filepath.Clean(want) {
			t.Fatalf("legacy owned path = %q, want %q", got, want)
		}
		if err := os.MkdirAll(filepath.Join(rollback, "tls-tree", "0"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := validateTLSRollbackSnapshot(rollback, snapshot, scope, legacy); err != nil {
			t.Fatalf("a complete legacy tree must pass the preflight: %v", err)
		}
		// A legacy manifest whose tree is genuinely absent is still refused, so
		// the compatibility path cannot silently restore nothing.
		if err := os.RemoveAll(filepath.Join(rollback, "tls-tree", "0")); err != nil {
			t.Fatal(err)
		}
		if err := validateTLSRollbackSnapshot(rollback, snapshot, scope, legacy); err == nil {
			t.Fatal("a legacy manifest without its tree must be refused")
		}
	})

	t.Run("legacy roots outside the managed namespace are refused", func(t *testing.T) {
		// A v1.9.34 manifest that names an arbitrary parent directory must never
		// be replayed as an owned root.
		outside := filepath.Join(dir, "elsewhere")
		snapshot := tlsNamespaceSnapshot{Roots: []string{outside}}
		if _, _, err := tlsRollbackLayout(dbPath, snapshot); err == nil {
			t.Fatal("a legacy manifest naming a custom directory must be refused")
		}
	})
}

// TestSnapshotRecordsOwnedRootPresence pins the metadata that makes the
// preflight possible: copyTLSNamespaceTree treats a missing source as success,
// so without the presence bit a rollback cannot tell "no TLS state yet" from
// "the rollback copy was lost".
func TestSnapshotRecordsOwnedRootPresence(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "meridian.db")
	rollback := dbPath + backupRollbackSuffix
	if err := os.MkdirAll(rollback, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := snapshotTLSNamespace(dbPath, rollback); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(rollback, "tls-namespace.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot tlsNamespaceSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Scope.OwnedRoots) == 0 {
		t.Fatal("snapshot recorded no owned roots")
	}
	if len(snapshot.Owned) != len(snapshot.Scope.OwnedRoots) {
		t.Fatalf("owned metadata %d entries, want %d", len(snapshot.Owned), len(snapshot.Scope.OwnedRoots))
	}
	// The TLS state directory does not exist yet in this fixture, so presence
	// must be false rather than silently assumed.
	for index, entry := range snapshot.Owned {
		if entry.Path != snapshot.Scope.OwnedRoots[index] {
			t.Fatalf("owned[%d].Path = %q, want %q", index, entry.Path, snapshot.Scope.OwnedRoots[index])
		}
		if entry.Present {
			t.Fatalf("owned[%d] reported present for a directory that does not exist: %q", index, entry.Path)
		}
	}
}

func errorsNew(message string) error {
	return &staticError{message: message}
}

type staticError struct{ message string }

func (e *staticError) Error() string { return e.message }

// TestQuotaUsageDecisionFailsClosed pins the enforcement rule for a hard quota:
// an unreadable usage baseline is not permission to transfer. The admission
// path answers 503 and the streaming paths abort, so a failing database cannot
// silently disable a site's limit.
func TestQuotaUsageDecisionFailsClosed(t *testing.T) {
	app := newTestApp(t)
	site, err := app.db.CreateSite("quota-unavailable", freePort(t), "http://127.0.0.1:8096", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	inst := &ProxyInstance{Site: *site}
	// A healthy database with no usage is under the quota.
	if decision := quotaUsageDecision(app.pm, inst, 1024, time.Now()); decision != nil {
		t.Fatalf("usage under quota must be allowed, got %v", decision)
	}
	// Usage above the quota is a quota decision, not an availability one.
	inst.trafficCycleMode = ""
	inst.trafficCycleStart = time.Time{}
	inst.trafficCycleUsage = 0
	if err := app.db.addTrafficWithRequests(site.ID, 4096, 4096, 1); err != nil {
		t.Fatal(err)
	}
	if decision := quotaUsageDecision(app.pm, inst, 1024, time.Now()); decision != errTrafficQuotaExceeded {
		t.Fatalf("usage over quota decision = %v, want errTrafficQuotaExceeded", decision)
	}

	// A database that cannot answer must fail closed rather than admit the
	// transfer.
	broken := newTestApp(t)
	brokenSite, err := broken.db.CreateSite("quota-broken", freePort(t), "http://127.0.0.1:8097", "", "direct", "[]", "infuse", 0, 0)
	if err != nil {
		t.Fatalf("CreateSite: %v", err)
	}
	brokenInst := &ProxyInstance{Site: *brokenSite}
	if err := broken.db.db.Close(); err != nil {
		t.Fatal(err)
	}
	if decision := quotaUsageDecision(broken.pm, brokenInst, 1024, time.Now()); decision != errTrafficQuotaUnavailable {
		t.Fatalf("unreadable usage decision = %v, want errTrafficQuotaUnavailable", decision)
	}
}
