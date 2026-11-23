package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestProxyResponseRecordsRangeStatusAfterEarlyHints(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-2" {
			http.Error(w, "missing range", http.StatusBadRequest)
			return
		}
		w.Header().Set("Link", "</style.css>; rel=preload; as=style")
		w.WriteHeader(http.StatusEarlyHints)
		w.Header().Del("Link")
		w.Header().Set("Content-Range", "bytes 0-2/12")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "abc")
	}))
	defer upstream.Close()
	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)
	statuses := make(chan [2]int, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		edge := &edgeStatusWriter{ResponseWriter: w}
		logged := &requestLogResponseWriter{ResponseWriter: edge}
		proxy.ServeHTTP(logged, r)
		statuses <- [2]int{logged.StatusCode(), edge.status}
	}))
	defer server.Close()

	var hints []int
	trace := &httptrace.ClientTrace{Got1xxResponse: func(code int, _ textproto.MIMEHeader) error {
		hints = append(hints, code)
		return nil
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", "bytes=0-2")
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Range") != "bytes 0-2/12" || string(body) != "abc" {
		t.Fatalf("range response = %d %q %q, %v", resp.StatusCode, resp.Header.Get("Content-Range"), body, err)
	}
	if len(hints) != 1 || hints[0] != http.StatusEarlyHints {
		t.Fatalf("informational responses = %v, want [103]", hints)
	}
	select {
	case got := <-statuses:
		if got != [2]int{http.StatusPartialContent, http.StatusPartialContent} {
			t.Fatalf("logged and Agent statuses = %v, want [206 206]", got)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestDynamicCapabilityPreservesMediaTransferFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		terminator string
		wantError  error
	}{
		{name: "complete", terminator: "0\r\n\r\n"},
		{name: "interrupted", wantError: io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer := newStructuredDiscoveryTestIssuer(t)
			playlist := mustStructuredURL(t, "https://api.example.com/playlist.m3u8")
			segment := mustStructuredURL(t, "https://cdn.example.com/segment.ts")
			route, discoveryErr := issuer.mint(t.Context(), playlist, segment, dynamicDiscoverySourceHLS)
			if discoveryErr != nil {
				t.Fatalf("mint segment capability: %v", discoveryErr)
			}
			// Exceed the server's buffer so the client's body reader sees the error
			// after the response starts, independently of early header flushing.
			payload := strings.Repeat("test", 4096)
			issuer.transportFactory = redirectRuntimeFactory(make(chan redirectRuntimeDialCapture, 1), func(*http.Request) string {
				return "HTTP/1.1 200 OK\r\nContent-Type: video/mp2t\r\nTransfer-Encoding: chunked\r\n\r\n" +
					fmt.Sprintf("%x\r\n%s\r\n%s", len(payload), payload, tc.terminator)
			})
			server := httptest.NewServer(http.HandlerFunc(issuer.serve))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+route, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK || !errors.Is(err, tc.wantError) || len(body) == 0 || !strings.HasPrefix(payload, string(body)) || (tc.wantError == nil && string(body) != payload) {
				t.Fatalf("segment status = %d, bytes = %d/%d, read error = %v, want %v", resp.StatusCode, len(body), len(payload), err, tc.wantError)
			}
		})
	}
}
