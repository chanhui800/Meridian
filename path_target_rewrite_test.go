package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func pathTargetRewriteTestURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse URL %q: %v", raw, err)
	}
	return parsed
}

func TestPathTargetRewriteSessionMapsPrivateAndRelativeURLsWithoutIssuer(t *testing.T) {
	base := pathTargetRewriteTestURL(t, "https://192.168.1.20:9443/emby/Items/1/PlaybackInfo")
	session := &dynamicRewriteSession{
		ctx:             context.Background(),
		base:            base,
		source:          dynamicDiscoverySourcePlaybackInfo,
		pathTargetMode:  true,
		rewriteRelative: true,
	}
	for _, test := range []struct{ input, want string }{
		{"../Videos/1/original.mkv?api_key=private", "/_proxy/v1/https/192.168.1.20:9443/emby/Items/Videos/1/original.mkv?api_key=private"},
		{"http://127.0.0.1:8096/Videos/2/stream.mkv?api_key=local", "/_proxy/v1/http/127.0.0.1:8096/Videos/2/stream.mkv?api_key=local"},
	} {
		got, err := session.rewriteAgainstSourceKind(test.input, base, dynamicDiscoverySourcePlaybackInfo, dynamicCapabilityKindResource)
		if err != nil {
			t.Fatalf("rewrite %q: %v", test.input, err)
		}
		if got != test.want {
			t.Fatalf("rewrite %q = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestPathTargetPlaybackInfoRewritesRelativeAndPrivateURLs(t *testing.T) {
	base := pathTargetRewriteTestURL(t, "http://127.0.0.1:8096/emby/Items/1/PlaybackInfo")
	session := &dynamicRewriteSession{
		ctx:             context.Background(),
		base:            base,
		source:          dynamicDiscoverySourcePlaybackInfo,
		pathTargetMode:  true,
		rewriteRelative: true,
	}
	payload := []byte(`{"MediaSources":[{"Protocol":"Http","DirectStreamUrl":"/Videos/1/original.mkv?api_key=relative","TranscodingUrl":"master.m3u8?api_key=manifest","TranscodingSubProtocol":"hls","MediaStreams":[{"DeliveryUrl":"http://localhost:8096/emby/Subtitles/1/Stream.vtt?api_key=subtitle","IsExternalUrl":true}]}]}`)
	rewritten, err := rewritePlaybackInfoResponse(payload, session)
	if err != nil {
		t.Fatalf("rewrite PlaybackInfo: %v", err)
	}
	text := string(rewritten)
	for _, expected := range []string{
		"/_proxy/v1/http/127.0.0.1:8096/Videos/1/original.mkv?api_key=relative",
		"/_proxy/v1/http/127.0.0.1:8096/emby/Items/1/master.m3u8?api_key=manifest",
		"/_proxy/v1/http/localhost:8096/emby/Subtitles/1/Stream.vtt?api_key=subtitle",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("PlaybackInfo missing mapped URL %q: %s", expected, text)
		}
	}
	if strings.Contains(text, dynamicRoutePrefix) {
		t.Fatalf("path-target PlaybackInfo unexpectedly minted a capability URL: %s", text)
	}
}

func TestPathTargetPlaybackInfoPreservesCustomRouteUnderEmbyMount(t *testing.T) {
	base := pathTargetRewriteTestURL(t, "https://video.emos.best/emby/Items/1/PlaybackInfo")
	session := &dynamicRewriteSession{
		ctx:             context.Background(),
		base:            base,
		source:          dynamicDiscoverySourcePlaybackInfo,
		pathTargetMode:  true,
		rewriteRelative: true,
	}
	payload := []byte(`{"MediaSources":[{"Protocol":"Http","DirectStreamUrl":"/emya/video?server=emos&media_id=private"},{"Protocol":"Http","DirectStreamUrl":"/Videos/1/original.mkv?api_key=private"},{"Protocol":"Http","Path":"/emya/video?server=emos&media_id=path-media"}]}`)
	rewritten, err := rewritePlaybackInfoResponse(payload, session)
	if err != nil {
		t.Fatalf("rewrite Emos PlaybackInfo: %v", err)
	}
	text := string(rewritten)
	for _, expected := range []string{
		"/_proxy/v1/https/video.emos.best/emby/emya/video?server=emos&media_id=private",
		"/_proxy/v1/https/video.emos.best/Videos/1/original.mkv?api_key=private",
		`"Path":"/_proxy/v1/https/video.emos.best/emby/emya/video?server=emos&media_id=path-media"`,
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("PlaybackInfo missing URL %q: %s", expected, text)
		}
	}
}

func TestPathTargetHLSRewritesRelativeAndPrivateManifestLinks(t *testing.T) {
	base := pathTargetRewriteTestURL(t, "https://10.0.0.8:9443/media/master.m3u8")
	session := &dynamicRewriteSession{ctx: context.Background(), base: base, source: dynamicDiscoverySourceHLS, pathTargetMode: true, rewriteRelative: true}
	payload := []byte("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"a\",URI=\"audio.m3u8?sig=private\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000,AUDIO=\"a\"\nhttp://127.0.0.1:8096/media/variant.m3u8?sig=local\n")
	rewritten, err := rewriteHLSResponse(payload, session)
	if err != nil {
		t.Fatalf("rewrite HLS: %v", err)
	}
	text := string(rewritten)
	for _, expected := range []string{
		`URI="/_proxy/v1/https/10.0.0.8:9443/media/audio.m3u8?sig=private"`,
		"/_proxy/v1/http/127.0.0.1:8096/media/variant.m3u8?sig=local",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("HLS manifest missing mapped link %q: %s", expected, text)
		}
	}
	if strings.Contains(text, dynamicRoutePrefix) {
		t.Fatalf("path-target HLS unexpectedly minted a capability URL: %s", text)
	}
}

func TestPathTargetDASHRewritesPrivateTemplateAndKeepsNumberExpression(t *testing.T) {
	base := pathTargetRewriteTestURL(t, "http://10.1.2.3:8096/media/manifest.mpd")
	session := &dynamicRewriteSession{ctx: context.Background(), base: base, source: dynamicDiscoverySourceDASH, pathTargetMode: true, rewriteRelative: true}
	payload := []byte(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet mimeType="video/mp4"><SegmentTemplate initialization="init-$RepresentationID$.mp4" media="http://127.0.0.1:8096/chunk-$Number%05d$.m4s"/><Representation id="video-main" bandwidth="1000"/></AdaptationSet></Period></MPD>`)
	rewritten, err := rewriteDASHResponse(payload, session)
	if err != nil {
		t.Fatalf("rewrite DASH: %v", err)
	}
	text := string(rewritten)
	for _, expected := range []string{
		"/_proxy/v1/http/10.1.2.3:8096/media/init-video-main.mp4",
		"/_proxy/v1/http/127.0.0.1:8096/chunk-$Number%05d$.m4s",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("DASH manifest missing mapped template %q: %s", expected, text)
		}
	}
	if strings.Contains(text, dynamicRoutePrefix) {
		t.Fatalf("path-target DASH unexpectedly minted a capability URL: %s", text)
	}
}

func TestRewritePathTargetStructuredResponseUsesBoundedSiteParser(t *testing.T) {
	runtime := newDynamicRuntime()
	state := newDynamicSiteState(runtime, dynamicDefaultProfileLimits())
	defer state.close()
	base := pathTargetRewriteTestURL(t, "http://127.0.0.1:8096/emby/Items/1/PlaybackInfo")
	payload := []byte(`{"MediaSources":[{"Protocol":"Http","DirectStreamUrl":"/Videos/1/original.mkv?api_key=private"}]}`)
	response := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(strings.NewReader(string(payload))),
		ContentLength: int64(len(payload)),
		Request:       &http.Request{Method: http.MethodGet, URL: base},
	}
	if err := rewritePathTargetStructuredResponse(response, state); err != nil {
		t.Fatalf("rewrite bounded PlaybackInfo response: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `/_proxy/v1/http/127.0.0.1:8096/Videos/1/original.mkv?api_key=private`) {
		t.Fatalf("bounded response was not rewritten: %s", body)
	}
	if got := response.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length=%q, body length=%d", got, len(body))
	}
}

func TestRewritePathTargetStructuredResponsePreservesUnparseableBody(t *testing.T) {
	runtime := newDynamicRuntime()
	state := newDynamicSiteState(runtime, dynamicDefaultProfileLimits())
	defer state.close()
	base := pathTargetRewriteTestURL(t, "http://127.0.0.1:8096/Items/1/PlaybackInfo")
	payload := []byte(`{"MediaSources":`)
	response := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(strings.NewReader(string(payload))),
		ContentLength: int64(len(payload)),
		Request:       &http.Request{Method: http.MethodGet, URL: base},
	}
	if err := rewritePathTargetStructuredResponse(response, state); err != nil {
		t.Fatalf("optional rewrite should preserve the upstream response: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(payload) {
		t.Fatalf("unparseable response body = %q, want %q", body, payload)
	}
}

func TestPathTargetHLSContentSteeringURLIsRewritten(t *testing.T) {
	base := pathTargetRewriteTestURL(t, "https://10.0.0.8:9443/media/master.m3u8")
	session := &dynamicRewriteSession{ctx: context.Background(), base: base, source: dynamicDiscoverySourceHLS, pathTargetMode: true, rewriteRelative: true}
	payload := []byte("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-CONTENT-STEERING:SERVER-URI=\"http://127.0.0.1:8096/steering.json\",PATHWAY-ID=\"p0\"\n#EXT-X-STREAM-INF:BANDWIDTH=1000\nvariant.m3u8\n")
	rewritten, err := rewriteHLSResponse(payload, session)
	if err != nil {
		t.Fatalf("rewrite HLS content steering: %v", err)
	}
	if !strings.Contains(string(rewritten), `SERVER-URI="/_proxy/v1/http/127.0.0.1:8096/steering.json"`) {
		t.Fatalf("content steering URL was not routed: %s", rewritten)
	}
}
