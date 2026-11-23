package main

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/websocket"
)

func TestPathTargetRedirectedManifestsKeepCredentiallessChildren(t *testing.T) {
	for _, test := range []struct {
		name, filename, contentType, manifest, child string
	}{
		{"HLS", "master.m3u8", "application/vnd.apple.mpegurl", "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXTINF:1,\nsegment.ts?api_key=backend-token\n#EXT-X-ENDLIST\n", "/media/segment.ts"},
		{"DASH", "manifest.mpd", "application/dash+xml", `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet><SegmentTemplate media="chunk-$Number$.m4s?api_key=backend-token" initialization="init.mp4"/><Representation id="main" bandwidth="1000"/></AdaptationSet></Period></MPD>`, "/media/chunk-1.m4s"},
	} {
		t.Run(test.name, func(t *testing.T) {
			observed := make(chan http.Header, 2)
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				observed <- r.Header.Clone()
				if r.URL.Path == "/media/"+test.filename {
					w.Header().Set("Content-Type", test.contentType)
					_, _ = io.WriteString(w, test.manifest)
					return
				}
				if r.URL.Path != test.child || r.URL.RawQuery != "api_key=backend-token" {
					t.Errorf("child target=%s?%s", r.URL.Path, r.URL.RawQuery)
				}
				w.Header().Set("Content-Type", "video/mp4")
				w.Header().Set("Content-Range", "bytes 2-5/8")
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, "cdef")
			}))
			t.Cleanup(backend.Close)
			frontend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", backend.URL+"/media/"+test.filename)
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			t.Cleanup(frontend.Close)
			upstream, _ := url.Parse(frontend.URL)
			gateway := newPathTargetRegressionGateway(t)
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
				gateway.URL+"/_proxy/v1/http/"+upstream.Host+"/emya/video", nil)
			if err != nil {
				t.Fatal(err)
			}
			setPathTargetRegressionCredentials(request)
			response, err := gateway.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("manifest status=%d err=%v", response.StatusCode, err)
			}
			assertPathTargetRegressionCredentials(t, <-observed, false)
			child := ""
			if test.name == "HLS" {
				for _, line := range strings.Split(string(body), "\n") {
					if line != "" && !strings.HasPrefix(line, "#") {
						child = line
						break
					}
				}
			} else {
				decoder := xml.NewDecoder(bytes.NewReader(body))
				for {
					token, err := decoder.Token()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if element, ok := token.(xml.StartElement); ok && element.Name.Local == "SegmentTemplate" {
						for _, attribute := range element.Attr {
							if attribute.Name.Local == "media" {
								child = strings.ReplaceAll(attribute.Value, "$Number$", "1")
							}
						}
					}
				}
			}
			if !strings.HasPrefix(child, "/_proxy/v1/_redirect/http/") {
				t.Fatalf("manifest lost the credential restriction: child=%q body=%s", child, body)
			}
			request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, gateway.URL+child, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Media players can repeat their API headers for every playlist child.
			setPathTargetRegressionCredentials(request)
			response, err = gateway.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			body, err = io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != http.StatusPartialContent || string(body) != "cdef" {
				t.Fatalf("child media status=%d body=%q err=%v", response.StatusCode, body, err)
			}
			assertPathTargetRegressionCredentials(t, <-observed, false)
		})
	}
}

func TestPathTargetCredentiallessWebSocketKeepsNegotiation(t *testing.T) {
	observed := make(chan http.Header, 1)
	origin := httptest.NewServer(websocket.Server{
		Handshake: func(_ *websocket.Config, r *http.Request) error {
			observed <- r.Header.Clone()
			return nil
		},
		Handler: websocket.Handler(func(connection *websocket.Conn) {
			var value string
			if err := websocket.Message.Receive(connection, &value); err == nil {
				_ = websocket.Message.Send(connection, value)
			}
		}),
	})
	t.Cleanup(origin.Close)
	upstream, _ := url.Parse(origin.URL)
	gateway := newPathTargetRegressionGateway(t)
	config, err := websocket.NewConfig("ws"+strings.TrimPrefix(gateway.URL, "http")+"/_proxy/v1/_redirect/http/"+upstream.Host+"/socket?api_key=backend-token", gateway.URL)
	if err != nil {
		t.Fatal(err)
	}
	request := &http.Request{Header: make(http.Header)}
	setPathTargetRegressionCredentials(request)
	config.Header = request.Header
	connection, err := websocket.DialConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := websocket.Message.Send(connection, "ping"); err != nil {
		t.Fatal(err)
	}
	var echoed string
	if err := websocket.Message.Receive(connection, &echoed); err != nil || echoed != "ping" {
		t.Fatalf("websocket echo=%q err=%v", echoed, err)
	}
	assertPathTargetRegressionCredentials(t, <-observed, false)
}
