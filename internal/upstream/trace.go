package upstream

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"
)

// AttemptTrace captures transport-level timings for one upstream attempt.
//
// The SDK may perform more than one HTTP round trip for a logical request
// (detection, redirects, or retries). Each round trip contributes its own
// durations; the first transport start is the anchor for the latency fields,
// which mirrors what the caller actually waited for.
type AttemptTrace struct {
	mu sync.Mutex

	startedAt time.Time

	roundTrips int
	reusedConn bool
	httpStatus int

	dnsStart, dnsDone                 time.Time
	connectStart, connectDone         time.Time
	tlsStart, tlsDone                 time.Time
	wroteRequest                      time.Time
	lastRoundTripStart, lastRoundTrip time.Time

	dnsMs, connectMs, tlsMs    int64
	headersMs, roundTripMs     int64
	dnsTotalMs, connectTotalMs int64
	tlsTotalMs, headersTotalMs int64
	roundTripTotalMs           int64
	firstRoundTripMs           int64
	lastRoundTripMs            int64
}

// NewAttemptTrace returns an empty per-attempt recorder. Callers attach it to
// the request context with WithAttemptTrace before invoking the SDK.
func NewAttemptTrace() *AttemptTrace {
	return &AttemptTrace{}
}

type attemptTraceContextKey struct{}

// WithAttemptTrace attaches a transport recorder to ctx.
func WithAttemptTrace(ctx context.Context, trace *AttemptTrace) context.Context {
	if trace == nil {
		return ctx
	}
	return context.WithValue(ctx, attemptTraceContextKey{}, trace)
}

// AttemptTraceFromContext returns the recorder attached to ctx, if any.
func AttemptTraceFromContext(ctx context.Context) *AttemptTrace {
	trace, _ := ctx.Value(attemptTraceContextKey{}).(*AttemptTrace)
	return trace
}

// TraceTimings is an immutable snapshot used by the data plane after the SDK
// call returns or the stream closes.
type TraceTimings struct {
	RoundTrips       int
	ReusedConn       bool
	HTTPStatus       int
	DNSMs            int64
	ConnectMs        int64
	TLSMs            int64
	HeadersMs        int64
	RoundTripMs      int64
	DNSTotalMs       int64
	ConnectTotalMs   int64
	TLSTotalMs       int64
	HeadersTotalMs   int64
	RoundTripTotalMs int64
	FirstRoundTripMs int64
	LastRoundTripMs  int64
	DNSStarted       bool
	DNSDone          bool
	ConnectStarted   bool
	ConnectDone      bool
	TLSStarted       bool
	TLSDone          bool
	GotConn          bool
	WroteRequest     bool
}

// Snapshot returns a concurrency-safe snapshot of the collected timings.
func (t *AttemptTrace) Snapshot() TraceTimings {
	if t == nil {
		return TraceTimings{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := TraceTimings{
		RoundTrips:       t.roundTrips,
		ReusedConn:       t.reusedConn,
		HTTPStatus:       t.httpStatus,
		DNSMs:            t.dnsMs,
		ConnectMs:        t.connectMs,
		TLSMs:            t.tlsMs,
		HeadersMs:        t.headersMs,
		RoundTripMs:      t.roundTripMs,
		DNSTotalMs:       t.dnsTotalMs,
		ConnectTotalMs:   t.connectTotalMs,
		TLSTotalMs:       t.tlsTotalMs,
		HeadersTotalMs:   t.headersTotalMs,
		RoundTripTotalMs: t.roundTripTotalMs,
		FirstRoundTripMs: t.firstRoundTripMs,
		LastRoundTripMs:  t.lastRoundTripMs,
		DNSStarted:       !t.dnsStart.IsZero(),
		DNSDone:          !t.dnsDone.IsZero(),
		ConnectStarted:   !t.connectStart.IsZero(),
		ConnectDone:      !t.connectDone.IsZero(),
		TLSStarted:       !t.tlsStart.IsZero(),
		TLSDone:          !t.tlsDone.IsZero(),
		GotConn:          t.reusedConn || !t.connectStart.IsZero(),
		WroteRequest:     !t.wroteRequest.IsZero(),
	}
	// FirstRoundTripMs is the full duration of the first HTTP round trip
	// (request write through response headers), not the offset at which that
	// round trip started. HeadersMs is the same span for the last round trip.
	// Keeping both makes retries and redirects visible instead of collapsing
	// them into one aggregate.
	return out
}

func (t *AttemptTrace) beginRoundTrip() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	if t.startedAt.IsZero() {
		t.startedAt = now
	}
	t.roundTrips++
	t.lastRoundTripStart = now
	t.lastRoundTrip = time.Time{}
	t.dnsStart, t.dnsDone = time.Time{}, time.Time{}
	t.connectStart, t.connectDone = time.Time{}, time.Time{}
	t.tlsStart, t.tlsDone = time.Time{}, time.Time{}
	t.wroteRequest = time.Time{}
}

func (t *AttemptTrace) finishRoundTrip() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.lastRoundTrip = now
	var roundTrip, headers int64
	if !t.lastRoundTripStart.IsZero() {
		roundTrip = now.Sub(t.lastRoundTripStart).Milliseconds()
	}
	if !t.wroteRequest.IsZero() {
		headers = now.Sub(t.wroteRequest).Milliseconds()
	}
	t.roundTripMs = roundTrip
	if t.roundTrips == 1 {
		t.firstRoundTripMs = roundTrip
	}
	t.lastRoundTripMs = roundTrip
	t.headersMs = headers
	t.roundTripTotalMs += roundTrip
	t.headersTotalMs += headers
}

func (t *AttemptTrace) getConn(reused bool) {
	t.mu.Lock()
	t.reusedConn = t.reusedConn || reused
	t.mu.Unlock()
}

func (t *AttemptTrace) dnsStarted() {
	t.mu.Lock()
	t.dnsStart = time.Now()
	t.mu.Unlock()
}

func (t *AttemptTrace) dnsFinished() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dnsDone = time.Now()
	if !t.dnsStart.IsZero() {
		ms := t.dnsDone.Sub(t.dnsStart).Milliseconds()
		t.dnsMs = ms
		t.dnsTotalMs += ms
	}
}

func (t *AttemptTrace) connectStarted() {
	t.mu.Lock()
	t.connectStart = time.Now()
	t.mu.Unlock()
}

func (t *AttemptTrace) connectFinished() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.connectDone = time.Now()
	if !t.connectStart.IsZero() {
		ms := t.connectDone.Sub(t.connectStart).Milliseconds()
		t.connectMs = ms
		t.connectTotalMs += ms
	}
}

func (t *AttemptTrace) tlsStarted() {
	t.mu.Lock()
	t.tlsStart = time.Now()
	t.mu.Unlock()
}

func (t *AttemptTrace) tlsFinished() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tlsDone = time.Now()
	if !t.tlsStart.IsZero() {
		ms := t.tlsDone.Sub(t.tlsStart).Milliseconds()
		t.tlsMs = ms
		t.tlsTotalMs += ms
	}
}

func (t *AttemptTrace) wrote() {
	t.mu.Lock()
	t.wroteRequest = time.Now()
	t.mu.Unlock()
}

type traceTransport struct {
	base http.RoundTripper
}

// withAttemptTraceTransport composes transport observation without changing
// the connection pool, proxy, or redirect policy of the wrapped client.
func withAttemptTraceTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &traceTransport{base: base}
}

func (t *traceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req == nil {
		return t.base.RoundTrip(req)
	}
	trace := AttemptTraceFromContext(req.Context())
	if trace == nil {
		return t.base.RoundTrip(req)
	}

	trace.beginRoundTrip()
	clientTrace := &httptrace.ClientTrace{
		DNSStart:          func(httptrace.DNSStartInfo) { trace.dnsStarted() },
		DNSDone:           func(httptrace.DNSDoneInfo) { trace.dnsFinished() },
		ConnectStart:      func(string, string) { trace.connectStarted() },
		ConnectDone:       func(string, string, error) { trace.connectFinished() },
		TLSHandshakeStart: func() { trace.tlsStarted() },
		TLSHandshakeDone:  func(tls.ConnectionState, error) { trace.tlsFinished() },
		GotConn:           func(info httptrace.GotConnInfo) { trace.getConn(info.Reused) },
		WroteRequest:      func(httptrace.WroteRequestInfo) { trace.wrote() },
	}
	cloned := req.Clone(httptrace.WithClientTrace(req.Context(), clientTrace))
	resp, err := t.base.RoundTrip(cloned)
	trace.finishRoundTrip()
	if resp != nil {
		trace.setHTTPStatus(resp.StatusCode)
	}
	return resp, err
}

func (t *AttemptTrace) setHTTPStatus(status int) {
	t.mu.Lock()
	t.httpStatus = status
	t.mu.Unlock()
}

// EndpointHost returns a log-safe host for an endpoint URL.
func EndpointHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}
