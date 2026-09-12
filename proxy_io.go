package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

type originalRequestContextKey struct{}

// isClientRequestCancellation distinguishes a browser/player abandoning an
// in-flight request from Meridian stopping the site. The proxy receives a
// derived context which is canceled for both cases, so the original HTTP
// request context is carried separately and must be canceled as well.
func isClientRequestCancellation(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) && ctx != nil && errors.Is(ctx.Err(), context.Canceled)
}

func originalRequestContext(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	if original, ok := ctx.Value(originalRequestContextKey{}).(context.Context); ok {
		return original
	}
	return ctx
}

// metered response writer
type meteredWriter struct {
	http.ResponseWriter
	written    *atomic.Int64
	cumulative *atomic.Int64
}

func addMeteredBytes(primary, cumulative *atomic.Int64, n int) {
	if n <= 0 {
		return
	}
	if primary != nil {
		primary.Add(int64(n))
	}
	if cumulative != nil {
		cumulative.Add(int64(n))
	}
}

func (m *meteredWriter) Write(b []byte) (int, error) {
	n, err := m.ResponseWriter.Write(b)
	addMeteredBytes(m.written, m.cumulative, n)
	return n, err
}

// quotaCheckBytes bounds how often the site-wide mid-stream quota probe runs.
const quotaCheckBytes = 16 << 20

// errTrafficQuotaExceeded aborts request bodies and tunnel transfers whose
// site has crossed its billing quota; it is not exposed to clients.
var errTrafficQuotaExceeded = errors.New("traffic quota exceeded")

// errTrafficQuotaUnavailable aborts a transfer whose billing-cycle usage could
// not be read. A configured quota is a hard limit, so "usage unknown" must not
// degrade into "usage is fine": an unreadable usage baseline would otherwise
// disable enforcement for every stream on the site while the database is
// failing.
var errTrafficQuotaUnavailable = errors.New("traffic quota enforcement is unavailable")

// quotaUsageDecision evaluates one quota probe at a checkpoint. It returns
// errTrafficQuotaExceeded when the site is over its limit and
// errTrafficQuotaUnavailable when the usage baseline cannot be read; both are
// fail-closed conditions for the caller.
func quotaUsageDecision(pm *ProxyManager, inst *ProxyInstance, limit int64, now time.Time) error {
	if pm == nil || inst == nil || limit <= 0 {
		return nil
	}
	usage, err := pm.currentTrafficCycleUsage(inst, now)
	if err != nil {
		return errTrafficQuotaUnavailable
	}
	if usage >= limit {
		return errTrafficQuotaExceeded
	}
	return nil
}

// quotaProbeShared records bytes against the instance-wide probe accumulator
// and reports whether the caller must run the quota check now. The first
// stream to cross the threshold claims the check.
func quotaProbeShared(inst *ProxyInstance, n int64) bool {
	if inst == nil || n <= 0 {
		return false
	}
	return inst.quotaCheckSince.Add(n) >= quotaCheckBytes && inst.quotaCheckSince.Swap(0) >= 0
}

// quotaLimitedWriter aborts a streaming response once the site's billing-cycle
// usage crosses its quota. The probe uses the instance-wide byte accumulator
// shared by every concurrent stream, request body and tunnel, so the
// undetected overshoot is bounded by ~quotaCheckBytes of aggregate traffic
// rather than per-stream windows.
type quotaLimitedWriter struct {
	meteredWriter
	pm    *ProxyManager
	inst  *ProxyInstance
	quota int64
}

func (q *quotaLimitedWriter) Write(b []byte) (int, error) {
	n, err := q.meteredWriter.Write(b)
	if err != nil {
		return n, err
	}
	if quotaProbeShared(q.inst, int64(n)) {
		if decision := quotaUsageDecision(q.pm, q.inst, q.quota, time.Now()); decision != nil {
			return n, http.ErrAbortHandler
		}
	}
	return n, err
}

// quotaLimitedReader aborts an upload body once the site crosses its quota;
// bidirectional billing must not admit an unbounded POST after admission.
type quotaLimitedReader struct {
	meteredReader
	pm    *ProxyManager
	inst  *ProxyInstance
	quota int64
}

func (q *quotaLimitedReader) Read(p []byte) (int, error) {
	n, err := q.meteredReader.Read(p)
	// Readers may legally return data together with io.EOF. Account and probe
	// that final chunk before propagating the underlying error so a checkpoint
	// cannot be skipped at end of stream.
	if n > 0 && quotaProbeShared(q.inst, int64(n)) {
		if decision := quotaUsageDecision(q.pm, q.inst, q.quota, time.Now()); decision != nil {
			return n, decision
		}
	}
	return n, err
}

// Flush support for streaming
func (m *meteredWriter) Flush() {
	if f, ok := m.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack support for WebSocket upgrade
func (m *meteredWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := m.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("hijack not supported")
}

// metered request body reader
type meteredReader struct {
	io.ReadCloser
	read       *atomic.Int64
	cumulative *atomic.Int64
}

func (m *meteredReader) Read(p []byte) (int, error) {
	n, err := m.ReadCloser.Read(p)
	addMeteredBytes(m.read, m.cumulative, n)
	return n, err
}

type rateLimitedWriter struct {
	http.ResponseWriter
	bytesPerSec    int64
	written        *atomic.Int64
	cumulative     *atomic.Int64
	requestWritten int64
	start          time.Time
	ctx            context.Context
}

func (w *rateLimitedWriter) Write(b []byte) (int, error) {
	if w.bytesPerSec <= 0 {
		n, err := w.ResponseWriter.Write(b)
		addMeteredBytes(w.written, w.cumulative, n)
		return n, err
	}
	totalWritten := 0
	for len(b) > 0 {
		elapsed := time.Since(w.start).Seconds()
		if elapsed < 0.001 {
			elapsed = 0.001
		}
		allowed := int64(elapsed*float64(w.bytesPerSec)) - w.requestWritten
		if allowed <= 0 {
			if w.ctx == nil {
				time.Sleep(10 * time.Millisecond)
			} else {
				select {
				case <-w.ctx.Done():
					return totalWritten, w.ctx.Err()
				case <-time.After(10 * time.Millisecond):
				}
			}
			continue
		}
		chunk := b
		if int64(len(chunk)) > allowed {
			chunk = b[:allowed]
		}
		n, err := w.ResponseWriter.Write(chunk)
		addMeteredBytes(w.written, w.cumulative, n)
		w.requestWritten += int64(n)
		totalWritten += n
		b = b[n:]
		if err != nil {
			return totalWritten, err
		}
		if n == 0 {
			return totalWritten, io.ErrNoProgress
		}
	}
	return totalWritten, nil
}

func (w *rateLimitedWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *rateLimitedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, fmt.Errorf("hijack not supported")
}

// tunnelWriter meters, and optionally paces, bytes copied through a hijacked
// WebSocket tunnel. Accounting has to happen per chunk rather than once the copy
// returns, otherwise a long-lived tunnel stays invisible to the quota gate and
// exempt from the site's speed limit for as long as it is open.
type tunnelWriter struct {
	dst         io.Writer
	counter     *atomic.Int64
	cumulative  *atomic.Int64
	bytesPerSec int64
	written     int64
	start       time.Time
	quota       *quotaTunnelProbe
}

// quotaTunnelProbe lets a WebSocket tunnel share the site-wide quota
// checkpoint: a long-lived connection must stop transferring once its site
// is over quota, exactly like the HTTP response path.
type quotaTunnelProbe struct {
	pm    *ProxyManager
	inst  *ProxyInstance
	limit int64
}

func (t *tunnelWriter) Write(b []byte) (int, error) {
	if t.bytesPerSec <= 0 {
		n, err := t.dst.Write(b)
		addMeteredBytes(t.counter, t.cumulative, n)
		if err == nil && n > 0 && t.quota != nil && quotaProbeShared(t.quota.inst, int64(n)) {
			if decision := quotaUsageDecision(t.quota.pm, t.quota.inst, t.quota.limit, time.Now()); decision != nil {
				return n, decision
			}
		}
		return n, err
	}
	total := 0
	for len(b) > 0 {
		elapsed := time.Since(t.start).Seconds()
		if elapsed < 0.001 {
			elapsed = 0.001
		}
		allowed := int64(elapsed*float64(t.bytesPerSec)) - t.written
		if allowed <= 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		chunk := b
		if int64(len(chunk)) > allowed {
			chunk = b[:allowed]
		}
		n, err := t.dst.Write(chunk)
		addMeteredBytes(t.counter, t.cumulative, n)
		t.written += int64(n)
		total += n
		b = b[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
		if t.quota != nil && quotaProbeShared(t.quota.inst, int64(n)) {
			if decision := quotaUsageDecision(t.quota.pm, t.quota.inst, t.quota.limit, time.Now()); decision != nil {
				return total, decision
			}
		}
	}
	return total, nil
}

// headerHasToken reports whether a comma-separated header such as Connection
// carries the given token, which is how RFC 9110 requires these be compared.
func headerHasToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// isWebSocketUpgrade reports whether the request is a real RFC 6455 handshake.
// A bare "Upgrade: websocket" header is not enough to qualify: routing an
// ordinary request into the hijacked tunnel would skip the metering and
// speed-limit wrappers that the normal proxy path installs, and would relay raw
// bytes to an upstream that never agreed to switch protocols. Any request with
// upgrade intent that fails this check is rejected before ReverseProxy, because
// its generic 101 tunnel would bypass Meridian's traffic accounting and limits.
func isWebSocketUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		headerHasToken(r.Header, "Connection", "upgrade") &&
		r.Header.Get("Sec-WebSocket-Key") != ""
}

func hasUpgradeIntent(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get("Upgrade")) != "" ||
		headerHasToken(r.Header, "Connection", "upgrade")
}
