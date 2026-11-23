package main

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func requireUniqueXMLAttributeNames(t *testing.T, payload []byte) {
	t.Helper()
	decoder := xml.NewDecoder(bytes.NewReader(payload))
	for {
		token, err := decoder.RawToken()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("rewritten XML is malformed: %v", err)
		}
		if start, ok := token.(xml.StartElement); ok {
			seen := make(map[xml.Name]bool, len(start.Attr))
			for _, attribute := range start.Attr {
				if seen[attribute.Name] {
					t.Fatalf("rewritten XML repeats attribute %v on %s: %s", attribute.Name, start.Name.Local, payload)
				}
				seen[attribute.Name] = true
			}
		}
	}
}

func TestPathTargetDASHProducesValidNamespacesAndSignedTemplates(t *testing.T) {
	for _, test := range []struct {
		name, manifest string
	}{
		{"default namespace", `<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xml:lang="zh"><Period><AdaptationSet><Representation id="v" bandwidth="1000"><SegmentTemplate media="chunk-$Number%05d$.m4s?api_key=backend&amp;sig=%2F%2B" initialization="init.m4s"/></Representation></AdaptationSet></Period></MPD>`},
		{"prefixed namespace", `<d:MPD xmlns:d="urn:mpeg:dash:schema:mpd:2011" xml:lang="zh"><d:Period><d:AdaptationSet><d:Representation id="v" bandwidth="1000"><d:SegmentTemplate media="chunk-$Number%05d$.m4s?api_key=backend&amp;sig=%2F%2B" initialization="init.m4s"/></d:Representation></d:AdaptationSet></d:Period></d:MPD>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/dash+xml")
				_, _ = io.WriteString(w, test.manifest)
			}))
			t.Cleanup(upstream.Close)
			upstreamURL, _ := url.Parse(upstream.URL)
			gateway := newPathTargetRegressionGateway(t)
			response, err := gateway.Client().Get(gateway.URL + "/_proxy/v1/http/" + upstreamURL.Host + "/media/manifest.mpd")
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("DASH response status=%d err=%v", response.StatusCode, err)
			}
			requireUniqueXMLAttributeNames(t, body)
			decoder := xml.NewDecoder(bytes.NewReader(body))
			foundRoot, foundTemplate, foundLanguage := false, false, false
			for {
				token, err := decoder.Token()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				start, ok := token.(xml.StartElement)
				if !ok {
					continue
				}
				if start.Name.Space != "urn:mpeg:dash:schema:mpd:2011" {
					t.Fatalf("DASH element lost its namespace: %v", start.Name)
				}
				foundRoot = foundRoot || start.Name.Local == "MPD"
				for _, attribute := range start.Attr {
					if attribute.Name.Space == "http://www.w3.org/XML/1998/namespace" && attribute.Name.Local == "lang" && attribute.Value == "zh" {
						foundLanguage = true
					}
					if start.Name.Local == "SegmentTemplate" && attribute.Name.Local == "media" {
						want := "/_proxy/v1/http/" + upstreamURL.Host + "/media/chunk-$Number%05d$.m4s?api_key=backend&sig=%2F%2B"
						if attribute.Value != want {
							t.Fatalf("DASH template target=%q, want %q", attribute.Value, want)
						}
						foundTemplate = true
					}
				}
			}
			if !foundRoot || !foundTemplate || !foundLanguage {
				t.Fatalf("DASH root/template/xml:lang=%t/%t/%t", foundRoot, foundTemplate, foundLanguage)
			}
		})
	}
}
