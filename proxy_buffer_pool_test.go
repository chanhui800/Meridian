package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"sync/atomic"
	"testing"
)

type copyBufferTestTransport struct {
	payload []byte
}

func (transport copyBufferTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusPartialContent,
		Header: http.Header{
			"Content-Type":  {"video/mp4"},
			"Content-Range": {"bytes 0-1048575/2097152"},
		},
		ContentLength: int64(len(transport.payload)),
		Body:          io.NopCloser(bytes.NewReader(transport.payload)),
		Request:       request,
	}, nil
}

type copyBufferTestWriter struct {
	header http.Header
	status int
}

func (writer *copyBufferTestWriter) Header() http.Header {
	return writer.header
}

func (writer *copyBufferTestWriter) WriteHeader(status int) {
	writer.status = status
}

func (writer *copyBufferTestWriter) Write(body []byte) (int, error) {
	return len(body), nil
}

func BenchmarkProxyCopyBuffer(b *testing.B) {
	payload := bytes.Repeat([]byte("x"), 1<<20)
	for _, pooled := range []bool{false, true} {
		name := "unpooled"
		if pooled {
			name = "pooled"
		}
		b.Run(name, func(b *testing.B) {
			proxy := &httputil.ReverseProxy{
				Transport: copyBufferTestTransport{payload: payload},
				Rewrite: func(request *httputil.ProxyRequest) {
					request.Out.URL.Scheme = "http"
					request.Out.URL.Host = "origin.example.test"
				},
			}
			if pooled {
				proxy.BufferPool = sharedProxyBufferPool
			}
			request := httptest.NewRequest(http.MethodGet, "http://panel.example.test/Videos/1/stream.mp4", nil)
			request.Header.Set("Range", "bytes=0-1048575")
			var written, cumulative atomic.Int64
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			b.ResetTimer()
			for range b.N {
				writer := &copyBufferTestWriter{header: make(http.Header)}
				metered := &meteredWriter{ResponseWriter: writer, written: &written, cumulative: &cumulative}
				proxy.ServeHTTP(metered, request)
				if writer.status != http.StatusPartialContent || writer.header.Get("Content-Range") != "bytes 0-1048575/2097152" {
					b.Fatalf("Range response changed: status=%d range=%q", writer.status, writer.header.Get("Content-Range"))
				}
			}
			b.StopTimer()
			want := int64(b.N) * int64(len(payload))
			if written.Load() != want || cumulative.Load() != want {
				b.Fatalf("copy accounting = %d/%d, want %d", written.Load(), cumulative.Load(), want)
			}
		})
	}
}
