package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHandleWebSocketRejectsOversizedUpstreamHeadersOverTCP(t *testing.T) {
	// The manual handshake parser must enforce the same 10 MiB response-header
	// allowance as net/http.Transport. Exercise both ends over real TCP sockets.
	reply := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n" +
		"X-Large: " + strings.Repeat("a", websocketResponseHeaderLimit) + "\r\n\r\n"
	upstream := startFakeWSUpstream(t, reply)
	inst := &ProxyInstance{server: &http.Server{}}
	srv := newWSProxyServer(t, upstream.ln.Addr().String(), inst, 0, UAHeaderPolicy{})

	conn, err := net.DialTimeout("tcp", srv.Listener.Addr().String(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set client deadline: %v", err)
	}
	if _, err := io.WriteString(conn, wsClientHandshake); err != nil {
		t.Fatalf("write WebSocket handshake: %v", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("read proxy response: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("proxy status = %d, want 502", response.StatusCode)
	}
	if body, err := io.ReadAll(response.Body); err != nil || !strings.Contains(string(body), "upstream refused websocket upgrade") {
		t.Fatalf("proxy error body = %q, err = %v", body, err)
	}

	select {
	case <-upstream.done:
	case <-time.After(5 * time.Second):
		t.Fatal("oversized upstream connection did not close")
	}
}
