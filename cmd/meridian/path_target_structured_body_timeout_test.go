package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type pathTargetHeaderBlockingBody struct {
	prefix      *bytes.Reader
	blocked     chan struct{}
	closed      chan struct{}
	blockedOnce sync.Once
	closeOnce   sync.Once
}

func (body *pathTargetHeaderBlockingBody) Read(payload []byte) (int, error) {
	if body.prefix.Len() > 0 {
		return body.prefix.Read(payload)
	}
	body.blockedOnce.Do(func() { close(body.blocked) })
	<-body.closed
	return 0, io.ErrClosedPipe
}

func (body *pathTargetHeaderBlockingBody) Close() error {
	body.closeOnce.Do(func() { close(body.closed) })
	return nil
}

func TestPathTargetStructuredBodyTimeoutIncludesGzipHeader(t *testing.T) {
	prefix := []byte{0x1f, 0x8b}
	body := &pathTargetHeaderBlockingBody{
		prefix: bytes.NewReader(prefix), blocked: make(chan struct{}), closed: make(chan struct{}),
	}
	t.Cleanup(func() { _ = body.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	response := &http.Response{
		Header:        http.Header{"Content-Encoding": {"gzip"}},
		Body:          body,
		ContentLength: -1,
		Request:       httptest.NewRequest(http.MethodGet, "https://origin.example/master.m3u8", nil).WithContext(ctx),
	}
	finished := make(chan error, 1)
	go func() {
		_, _, _, err := readPathTargetStructuredBody(response, 1024)
		finished <- err
	}()
	select {
	case <-body.blocked:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("gzip initialization did not reach the blocking header read")
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("incomplete gzip header unexpectedly parsed")
		}
	case <-time.After(500 * time.Millisecond):
		_ = body.Close()
		<-finished
		t.Fatal("gzip header read remained blocked after the request deadline")
	}
	select {
	case <-body.closed:
	default:
		t.Fatal("gzip header read was not interrupted by closing the upstream body")
	}
	replayed := make([]byte, len(prefix))
	if _, err := io.ReadFull(response.Body, replayed); err != nil || !bytes.Equal(replayed, prefix) {
		t.Fatalf("timed-out gzip header replay=%x err=%v, want %x", replayed, err, prefix)
	}
}

func TestPathTargetStructuredBodyEarlyReturnStopsDeadlineTimer(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		encoding      string
		contentLength int64
	}{
		{"unsupported-encoding", "br", -1},
		{"oversized-body", "gzip", 2048},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			prefix := []byte("original upstream body")
			body := &pathTargetHeaderBlockingBody{
				prefix: bytes.NewReader(prefix), blocked: make(chan struct{}), closed: make(chan struct{}),
			}
			t.Cleanup(func() { _ = body.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
			defer cancel()
			response := &http.Response{
				Header:        http.Header{"Content-Encoding": {scenario.encoding}},
				Body:          body,
				ContentLength: scenario.contentLength,
				Request:       httptest.NewRequest(http.MethodGet, "https://origin.example/master.m3u8", nil).WithContext(ctx),
			}
			_, restore, _, err := readPathTargetStructuredBody(response, 1024)
			if err == nil || restore == nil {
				t.Fatal("unreadable structured body did not preserve its replay path")
			}
			restore()
			select {
			case <-body.closed:
				t.Fatal("early return left the deadline timer closing the original upstream body")
			case <-time.After(100 * time.Millisecond):
			}
			replayed := make([]byte, len(prefix))
			if _, err := io.ReadFull(response.Body, replayed); err != nil || !bytes.Equal(replayed, prefix) {
				t.Fatalf("early-return body replay=%q err=%v, want %q", replayed, err, prefix)
			}
		})
	}
}
