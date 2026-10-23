package dataplane

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

// requestDiagnostics is the structured, terminal log record for one gateway
// request. Keep these fields stable: operators build latency and failure-rate
// queries directly on the JSON event.
type requestDiagnostics struct {
	// mu guards the mutable request state. It is deliberately not part of
	// diagnosticsState: snapshots must be immutable values and copying a
	// sync.Mutex would make `go vet` fail for a real reason.
	mu sync.Mutex
	diagnosticsState
	attempts []attemptDiagnostics
}

// diagnosticsState contains only copyable, lock-free values. Keep the mutex
// out of this struct so a snapshot can be passed to pure helpers safely.
type diagnosticsState struct {
	startedAt time.Time
	endedAt   time.Time

	requestID   string
	traceparent string
	traceID     string
	method      string
	path        string
	remote      string
	userID      string
	keyID       string

	publicModel      string
	ingressProtocol  string
	upstreamProtocol string
	stream           bool

	bodyBytes    int64
	messages     int
	tools        int
	toolResults  int
	inputChars   int
	outputRunes  int
	outputTokens int64
	toolCalls    int
	firstToolMs  int64

	authMs             int64
	decodeMs           int64
	admissionMs        int64
	quotaMs            int64
	routingMs          int64
	balancePrecheckMs  int64
	credentialSelectMs int64

	responseStatus int
	responseBytes  int64
	writeCount     int64
	writeTotalMs   int64
	writeMaxMs     int64
	flushCount     int64
	flushTotalMs   int64
	flushMaxMs     int64
	responseWrites int64
	streamEvents   int64
	maxEventGapMs  int64

	usageStatus string
	errorCode   string
	errorDetail string
	errorRaw    string
	routeTrace  string

	providerID       string
	providerName     string
	providerSlug     string
	upstreamModel    string
	endpointHost     string
	networkID        string
	proxyConfigured  bool
	credentialID     string
	upstreamResponse string
	upstreamRequest  string

	clientGone     bool
	gatewayTimeout bool
}

type requestDiagnosticsSnapshot struct {
	diagnosticsState
	attempts []attemptDiagnostics
}

type attemptDiagnostics struct {
	Index           int    `json:"index"`
	TargetID        string `json:"target_id,omitempty"`
	ProviderID      string `json:"provider_id,omitempty"`
	ProviderName    string `json:"provider_name,omitempty"`
	ProviderSlug    string `json:"provider_slug,omitempty"`
	UpstreamModel   string `json:"upstream_model,omitempty"`
	Protocol        string `json:"upstream_protocol,omitempty"`
	EndpointHost    string `json:"endpoint_host,omitempty"`
	NetworkID       string `json:"network_id,omitempty"`
	ProxyConfigured bool   `json:"proxy_configured"`
	CredentialID    string `json:"credential_id,omitempty"`

	StartedAtMs         int64 `json:"started_at_ms,omitempty"`
	DurationMs          int64 `json:"duration_ms"`
	CredentialMs        int64 `json:"credential_select_ms"`
	RequestBuildMs      int64 `json:"request_build_ms"`
	UpstreamCallMs      int64 `json:"upstream_call_ms"`
	UpstreamCallStarted bool  `json:"upstream_call_started"`
	TTFBMs              int64 `json:"upstream_ttfb_ms,omitempty"`
	PostTTFBMs          int64 `json:"post_ttfb_ms,omitempty"`
	MaxEventGapMs       int64 `json:"stream_max_event_gap_ms,omitempty"`
	DNSMs               int64 `json:"dns_ms,omitempty"`
	ConnectMs           int64 `json:"connect_ms,omitempty"`
	TLSMs               int64 `json:"tls_ms,omitempty"`
	HeadersMs           int64 `json:"upstream_headers_ms,omitempty"`
	RoundTripMs         int64 `json:"round_trip_ms,omitempty"`
	RoundTrips          int   `json:"http_round_trips,omitempty"`
	FirstRoundTripMs    int64 `json:"first_round_trip_ms,omitempty"`
	LastRoundTripMs     int64 `json:"last_round_trip_ms,omitempty"`
	DNSStarted          bool  `json:"dns_started"`
	DNSDone             bool  `json:"dns_done"`
	ConnectStarted      bool  `json:"connect_started"`
	ConnectDone         bool  `json:"connect_done"`
	TLSStarted          bool  `json:"tls_started"`
	TLSDone             bool  `json:"tls_done"`
	RequestWritten      bool  `json:"request_written"`
	GotConnection       bool  `json:"got_connection"`
	ReusedConnection    bool  `json:"connection_reused"`
	UpstreamHTTPStatus  int   `json:"upstream_http_status,omitempty"`
	StreamEvents        int64 `json:"stream_events,omitempty"`
	FirstToolCallMs     int64 `json:"first_tool_call_ms,omitempty"`
	ToolCalls           int   `json:"tool_calls,omitempty"`
	OutputRunes         int   `json:"output_runes,omitempty"`

	Status                string `json:"status"`
	ErrorCode             string `json:"error_code,omitempty"`
	ErrorType             string `json:"error_type,omitempty"`
	ProviderErrorCode     string `json:"provider_error_code,omitempty"`
	ErrorParam            string `json:"error_param,omitempty"`
	ErrorMessage          string `json:"error_message,omitempty"`
	ErrorDetail           string `json:"error_detail,omitempty"`
	FailureStage          string `json:"failure_stage,omitempty"`
	FailureAttribution    string `json:"failure_attribution,omitempty"`
	AttributionConfidence string `json:"attribution_confidence,omitempty"`
	UpstreamID            string `json:"upstream_response_id,omitempty"`
	Committed             bool   `json:"committed"`
	Eligible              bool   `json:"eligible"`
	Success               bool   `json:"success"`
}

func newRequestDiagnostics(r *http.Request) *requestDiagnostics {
	traceparent := strings.TrimSpace(r.Header.Get("traceparent"))
	return &requestDiagnostics{
		startedAt:   time.Now(),
		requestID:   "",
		traceparent: traceparent,
		traceID:     traceIDFromTraceparent(traceparent),
		method:      r.Method,
		path:        r.URL.Path,
		bodyBytes:   maxInt64(r.ContentLength, 0),
	}
}

func (d *requestDiagnostics) setRemote(remote string) {
	d.mu.Lock()
	d.remote = remote
	d.mu.Unlock()
}

func (d *requestDiagnostics) begin(_ string) time.Time {
	return time.Now()
}

func (d *requestDiagnostics) add(stage string, ms int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch stage {
	case "auth":
		d.authMs += ms
	case "decode":
		d.decodeMs += ms
	case "admission":
		d.admissionMs += ms
	case "quota":
		d.quotaMs += ms
	case "routing":
		d.routingMs += ms
	case "balance_precheck":
		d.balancePrecheckMs += ms
	case "credential_select":
		d.credentialSelectMs += ms
	}
}

func (d *requestDiagnostics) setResult(status, code, detail string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.usageStatus = status
	d.errorCode = code
	d.errorDetail = detail
}

func (d *requestDiagnostics) result() (string, string, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.usageStatus, d.errorCode, d.errorDetail
}

func outcomeUsageStatus(status int) string {
	if status >= 200 && status < 400 {
		return "ok"
	}
	return "error"
}

func statusForDiagnostics(clientGone, gatewayTimeout bool) string {
	switch {
	case clientGone:
		return "canceled"
	case gatewayTimeout:
		return "timeout"
	default:
		return "error"
	}
}

func errorCodeForDiagnostics(clientGone, gatewayTimeout bool, code string) string {
	switch {
	case clientGone:
		return ""
	case gatewayTimeout:
		return "gateway_timeout"
	default:
		return code
	}
}

func (d *requestDiagnostics) setHTTPStatus(status int) {
	d.mu.Lock()
	d.responseStatus = status
	d.mu.Unlock()
}

func (d *requestDiagnostics) setErrorRaw(raw string) {
	d.mu.Lock()
	d.errorRaw = raw
	d.mu.Unlock()
}

func (d *requestDiagnostics) setRouteTrace(trace string) {
	d.mu.Lock()
	d.routeTrace = trace
	d.mu.Unlock()
}

func (d *requestDiagnostics) setSelected(cand routing.Candidate, credentialID, protocol string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if cand.Provider != nil {
		d.providerID = cand.Provider.ID
		d.providerName = cand.Provider.Name
		d.providerSlug = cand.Provider.Slug
		d.upstreamProtocol = protocol
		if d.upstreamProtocol == "" {
			d.upstreamProtocol = cand.Provider.Protocol
		}
		d.endpointHost = upstream.EndpointHost(cand.Provider.Endpoint)
		d.networkID = cand.Provider.NetworkID
		d.proxyConfigured = cand.Provider.ProxyConfigured
	}
	d.upstreamModel = cand.UpstreamModel.ModelID
	d.credentialID = credentialID
}

func (d *requestDiagnostics) setIngressName(name string) {
	d.mu.Lock()
	d.ingressProtocol = name
	d.mu.Unlock()
}

func (d *requestDiagnostics) observeResponse(o *responseObserver) {
	if o == nil {
		return
	}
	stats := o.snapshot()
	d.mu.Lock()
	if stats.status != 0 {
		d.responseStatus = int(stats.status)
	}
	d.responseBytes = stats.bytes
	d.writeCount = stats.writes
	d.writeTotalMs = stats.writeMs
	d.writeMaxMs = stats.writeMax
	d.flushCount = stats.flushes
	d.flushTotalMs = stats.flushMs
	d.flushMaxMs = stats.flushMax
	d.responseWrites = stats.writes
	d.mu.Unlock()
}

func (d *requestDiagnostics) end(stage string, started time.Time) int64 {
	ms := time.Since(started).Milliseconds()
	d.add(stage, ms)
	return ms
}

func (d *requestDiagnostics) setRequestID(id string) {
	d.mu.Lock()
	d.requestID = id
	d.mu.Unlock()
}

func (d *requestDiagnostics) setAuth(userID, keyID string) {
	d.mu.Lock()
	d.userID, d.keyID = userID, keyID
	d.mu.Unlock()
}

func (d *requestDiagnostics) setPublicModel(model string) {
	if model == "" {
		return
	}
	d.mu.Lock()
	d.publicModel = model
	d.mu.Unlock()
}

func (d *requestDiagnostics) setUpstreamResponse(id string) {
	if id == "" {
		return
	}
	d.mu.Lock()
	d.upstreamResponse = id
	d.mu.Unlock()
}

func (d *requestDiagnostics) setUpstreamRequest(id string) {
	if id == "" {
		return
	}
	d.mu.Lock()
	d.upstreamRequest = id
	d.mu.Unlock()
}

func (d *requestDiagnostics) setStreamOutput(runes, toolCalls int, firstToolMs int64) {
	d.mu.Lock()
	d.outputRunes = runes
	d.toolCalls = toolCalls
	d.firstToolMs = firstToolMs
	d.mu.Unlock()
}

func (d *requestDiagnostics) setStreamMetrics(events int64, maxGapMs int64) {
	d.mu.Lock()
	d.streamEvents = events
	d.maxEventGapMs = maxGapMs
	d.mu.Unlock()
}

func (d *requestDiagnostics) setUsage(outputTokens int64) {
	d.mu.Lock()
	d.outputTokens = outputTokens
	d.mu.Unlock()
}

func (d *requestDiagnostics) setTerminal(clientGone, gatewayTimeout bool) {
	d.mu.Lock()
	d.clientGone = clientGone
	d.gatewayTimeout = gatewayTimeout
	d.mu.Unlock()
}

func (d *requestDiagnostics) setIngressRequest(ing *rosetta.ChatRequest, stream bool) {
	snapshot := inspectRequest(ing, stream)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.messages = snapshot.messages
	d.tools = snapshot.tools
	d.toolResults = snapshot.toolResults
	d.inputChars = snapshot.inputChars
	d.stream = stream
}

func (d *requestDiagnostics) finish(status int, usageStatus, code, detail, raw, routeTrace string) {
	d.mu.Lock()
	d.endedAt = time.Now()
	d.responseStatus = status
	d.usageStatus = usageStatus
	d.errorCode = code
	d.errorDetail = detail
	d.errorRaw = raw
	d.routeTrace = routeTrace
	d.mu.Unlock()
}

func (d *requestDiagnostics) addAttempt(a attemptDiagnostics) {
	d.mu.Lock()
	d.attempts = append(d.attempts, a)
	d.mu.Unlock()
}

func (d *requestDiagnostics) hasAttempts() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.attempts) > 0
}

func (d *requestDiagnostics) snapshot() requestDiagnosticsSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	return requestDiagnosticsSnapshot{
		diagnosticsState: d.diagnosticsState,
		attempts:         append([]attemptDiagnostics(nil), d.attempts...),
	}
}

func (d *requestDiagnostics) attrs() []any {
	s := d.snapshot()
	elapsed := time.Since(s.startedAt).Milliseconds()
	last := lastNetworkAttempt(s.attempts)
	attribution, confidence, failureStage := diagnoseSnapshot(s)
	attrs := []any{
		"event", "gateway_request_diagnostic",
		"request_id", s.requestID,
		"traceparent", s.traceparent,
		"trace_id", s.traceID,
		"method", s.method,
		"path", s.path,
		"remote", s.remote,
		"user_id", s.userID,
		"key_id", s.keyID,
		"public_model", s.publicModel,
		"ingress_protocol", s.ingressProtocol,
		"upstream_protocol", s.upstreamProtocol,
		"stream", s.stream,
		"status", s.responseStatus,
		"usage_status", s.usageStatus,
		"error_code", s.errorCode,
		"body_bytes", s.bodyBytes,
		"messages", s.messages,
		"tools", s.tools,
		"tool_results", s.toolResults,
		"input_chars", s.inputChars,
		"output_runes", s.outputRunes,
		"output_tokens", s.outputTokens,
		"tool_calls", s.toolCalls,
		"first_tool_call_ms", s.firstToolMs,
		"stream_events", s.streamEvents,
		"stream_max_event_gap_ms", s.maxEventGapMs,
		"provider_id", s.providerID,
		"provider_name", s.providerName,
		"provider_slug", s.providerSlug,
		"upstream_model", s.upstreamModel,
		"endpoint_host", s.endpointHost,
		"network_id", s.networkID,
		"proxy_configured", s.proxyConfigured,
		"credential_id", s.credentialID,
		"upstream_request_id", s.upstreamRequest,
		"upstream_response_id", s.upstreamResponse,
		"queue_wait_ms", s.admissionMs,
		"auth_ms", s.authMs,
		"decode_ms", s.decodeMs,
		"admission_ms", s.admissionMs,
		"quota_ms", s.quotaMs,
		"routing_ms", s.routingMs,
		"balance_precheck_ms", s.balancePrecheckMs,
		"credential_select_ms", s.credentialSelectMs,
		"gateway_pre_upstream_ms", gatewayPreUpstreamMs(s),
		"upstream_request_ms", attemptMetric(last, func(a attemptDiagnostics) int64 { return a.UpstreamCallMs }),
		"upstream_ttfb_ms", attemptMetric(last, func(a attemptDiagnostics) int64 { return a.TTFBMs }),
		"stream_post_ttfb_ms", attemptMetric(last, func(a attemptDiagnostics) int64 { return a.PostTTFBMs }),
		"response_bytes", s.responseBytes,
		"response_writes", s.responseWrites,
		"downstream_write_ms", s.writeTotalMs,
		"downstream_write_max_ms", s.writeMaxMs,
		"downstream_flush_ms", s.flushTotalMs,
		"downstream_flush_max_ms", s.flushMaxMs,
		"total_ms", elapsed,
		"output_tokens_per_second_total", perSecond(s.outputTokens, elapsed),
		"output_tokens_per_second_generation", perSecond(s.outputTokens, attemptMetric(last, func(a attemptDiagnostics) int64 { return a.PostTTFBMs })),
		"slowest_observed_phase", slowestObservedPhase(s),
		"performance_flags", performanceFlags(s),
		"failure_stage", failureStage,
		"failure_attribution", attribution,
		"attribution_confidence", confidence,
		"route_trace", s.routeTrace,
		"attempts", s.attempts,
	}
	if s.errorDetail != "" {
		attrs = append(attrs, "error_detail", s.errorDetail)
	}
	if s.errorRaw != "" {
		attrs = append(attrs, "error_raw", s.errorRaw)
	}
	return append(attrs, "observation_scope", "gateway_until_response_written")
}

type ingressSnapshot struct {
	model       string
	stream      bool
	messages    int
	tools       int
	toolResults int
	inputChars  int
}

// gatewayPreUpstreamMs is the uninterrupted request-local work before the
// first upstream attempt starts. It is a real wall-clock interval, unlike a
// "total minus all stages" residual: streaming generation and downstream
// writes overlap, so subtracting both would silently clamp the result to zero.
func gatewayPreUpstreamMs(s requestDiagnosticsSnapshot) int64 {
	if s.startedAt.IsZero() {
		return 0
	}
	for _, a := range s.attempts {
		// Synthetic attempts (probe busy, no healthy credential) never called
		// the SDK. Their zero StartedAtMs must not be mistaken for "the first
		// upstream attempt started at time zero".
		if a.UpstreamCallStarted {
			return maxInt64(a.StartedAtMs, 0)
		}
	}
	ended := s.endedAt
	if ended.IsZero() {
		ended = time.Now()
	}
	return maxInt64(ended.Sub(s.startedAt).Milliseconds(), 0)
}

func attemptMetric(a *attemptDiagnostics, pick func(attemptDiagnostics) int64) int64 {
	if a == nil {
		return 0
	}
	return maxInt64(pick(*a), 0)
}

func lastNetworkAttempt(attempts []attemptDiagnostics) *attemptDiagnostics {
	for i := len(attempts) - 1; i >= 0; i-- {
		if attempts[i].UpstreamCallStarted || attempts[i].RoundTrips > 0 ||
			attempts[i].DNSMs > 0 || attempts[i].ConnectMs > 0 ||
			attempts[i].TLSMs > 0 || attempts[i].HeadersMs > 0 {
			return &attempts[i]
		}
	}
	return nil
}

func perSecond(tokens, ms int64) float64 {
	if tokens <= 0 || ms <= 0 {
		return 0
	}
	return float64(tokens) * 1000 / float64(ms)
}

// performanceFlags is intentionally a list of observable symptoms, not a
// verdict. A flag can be caused by the provider, the local network, a proxy,
// or downstream backpressure; failure_attribution combines it with status,
// transport signals and the attempt position to state the likely owner.
func performanceFlags(s requestDiagnosticsSnapshot) []string {
	flags := make([]string, 0, 14)
	last := lastNetworkAttempt(s.attempts)
	if s.writeMaxMs >= 1000 || s.writeTotalMs >= 2000 {
		flags = append(flags, "slow_downstream_write")
	}
	if s.flushMaxMs >= 1000 || s.flushTotalMs >= 2000 {
		flags = append(flags, "slow_downstream_flush")
	}
	if last != nil {
		// These are independent symptoms. A proxy that causes both slow DNS
		// and a slow TLS handshake should report both, not just the first one
		// in a mutually exclusive switch.
		if last.DNSMs >= 300 {
			flags = append(flags, "slow_dns")
		}
		if last.ConnectMs >= 1000 {
			flags = append(flags, "slow_connect")
		}
		if last.TLSMs >= 1000 {
			flags = append(flags, "slow_tls")
		}
		if last.HeadersMs >= 2000 {
			flags = append(flags, "slow_upstream_headers")
		}
		if last.TTFBMs >= 5000 {
			flags = append(flags, "slow_ttft")
		}
		if last.MaxEventGapMs >= 5000 {
			flags = append(flags, "slow_generation_gap")
		}
		if last.RoundTrips > 1 {
			flags = append(flags, "extra_http_round_trips")
		}
		if last.ConnectMs > 0 && !last.ReusedConnection {
			flags = append(flags, "new_connection")
		}
	}
	if s.proxyConfigured {
		flags = append(flags, "proxy_configured")
	}
	if s.admissionMs >= 1000 {
		flags = append(flags, "gateway_admission_wait")
	}
	if s.authMs >= 1000 {
		flags = append(flags, "slow_gateway_auth")
	}
	if s.decodeMs >= 1000 {
		flags = append(flags, "slow_gateway_decode")
	}
	if s.routingMs >= 1000 {
		flags = append(flags, "slow_gateway_routing")
	}
	if s.quotaMs >= 1000 {
		flags = append(flags, "slow_gateway_quota")
	}
	if s.balancePrecheckMs >= 1000 {
		flags = append(flags, "slow_gateway_balance_precheck")
	}
	if s.credentialSelectMs >= 1000 {
		flags = append(flags, "slow_gateway_credential_select")
	}
	if gatewayPreUpstreamMs(s) >= 1000 {
		flags = append(flags, "slow_gateway_pre_upstream")
	}
	if s.balancePrecheckMs >= 1000 || s.quotaMs >= 1000 || s.credentialSelectMs >= 1000 {
		flags = append(flags, "slow_gateway_control_plane")
	}
	return flags
}

func slowestObservedPhase(s requestDiagnosticsSnapshot) string {
	phases := []struct {
		name string
		ms   int64
	}{
		{"gateway_pre_upstream", gatewayPreUpstreamMs(s)},
		{"gateway_admission", s.admissionMs},
		{"gateway_quota", s.quotaMs},
		{"gateway_credential_select", s.credentialSelectMs},
		{"downstream_write", s.writeTotalMs},
		{"downstream_flush", s.flushTotalMs},
	}
	if last := lastNetworkAttempt(s.attempts); last != nil {
		phases = append(phases,
			struct {
				name string
				ms   int64
			}{"upstream_dns", last.DNSMs},
			struct {
				name string
				ms   int64
			}{"upstream_connect", last.ConnectMs},
			struct {
				name string
				ms   int64
			}{"upstream_tls", last.TLSMs},
			struct {
				name string
				ms   int64
			}{"upstream_headers", last.HeadersMs},
			struct {
				name string
				ms   int64
			}{"upstream_ttft", last.TTFBMs},
			struct {
				name string
				ms   int64
			}{"upstream_generation", last.PostTTFBMs},
		)
	}
	best := "none"
	var max int64
	for _, p := range phases {
		if p.ms > max {
			best, max = p.name, p.ms
		}
	}
	return best
}

// diagnoseSnapshot returns (attribution, confidence, failure stage). The
// attribution names the most likely owner of a failure or observed slowdown;
// confidence is high only when the evidence is structural (an HTTP status, a
// timeout reached by a known stage, or a write that failed). A configured
// proxy plus DNS/connect/TLS failure is reported as network_or_proxy with
// medium confidence because a direct route can fail the same way.
func diagnoseSnapshot(s requestDiagnosticsSnapshot) (string, string, string) {
	last := lastNetworkAttempt(s.attempts)
	lastAttempt := lastAttemptDiagnostics(s.attempts)
	stage := "pre_upstream"
	if last != nil {
		stage = slowestUpstreamPhase(last)
	}
	if s.clientGone {
		if s.writeCount > 0 {
			return "downstream_client", "high", "downstream_write"
		}
		return "downstream_client", "high", "downstream_request"
	}
	if s.gatewayTimeout {
		return "gateway_timeout", "high", "gateway_budget"
	}
	// Failures before the first upstream call have no attempt to inspect.
	// Map their stable gateway error codes explicitly; otherwise every local
	// rejection (bad key, unknown model, oversized body) would be mislabeled
	// as a gateway-internal failure.
	switch s.errorCode {
	case "server_overloaded":
		return "gateway_overload", "high", "gateway_admission"
	case "invalid_api_key":
		return "caller_configuration", "high", "caller_auth"
	case "model_not_found":
		return "caller_configuration", "high", "gateway_routing"
	case "model_not_allowed":
		return "caller_configuration", "high", "gateway_authorization"
	case "rate_limit_exceeded":
		return "caller_configuration", "high", "gateway_rate_limit"
	case "insufficient_quota":
		return "caller_configuration", "high", "gateway_quota"
	case "insufficient_balance":
		return "caller_configuration", "high", "gateway_balance"
	case "request_too_large":
		return "caller_configuration", "high", "inbound_body"
	case "invalid_request_error":
		return "caller_configuration", "high", "inbound_request"
	case "balance_precheck_failed":
		return "gateway_internal", "high", "gateway_balance_precheck"
	case "internal_error":
		return "gateway_internal", "high", "gateway_internal"
	}
	if last != nil {
		if last.Status == "failed" || last.Status == "committed_fail" || last.Status == "canceled" {
			switch last.FailureAttribution {
			case "upstream_auth", "upstream_rate_limit", "upstream_quota",
				"upstream_provider_error", "caller_configuration",
				"target_configuration", "gateway_internal",
				"gateway_overload", "gateway_timeout", "downstream_write":
				return last.FailureAttribution, diagnosticConfidence(last, "high"), last.FailureStage
			case "upstream_timeout", "upstream_network", "network_or_proxy",
				"downstream_client":
				return last.FailureAttribution, diagnosticConfidence(last, "medium"), last.FailureStage
			}
		}
		if last.MaxEventGapMs >= 5000 || last.TTFBMs >= 5000 || last.HeadersMs >= 2000 {
			return "upstream_latency", "medium", stage
		}
	}
	if lastAttempt != nil &&
		(lastAttempt.Status == "failed" || lastAttempt.Status == "skipped" || lastAttempt.Status == "committed_fail") &&
		lastAttempt.FailureAttribution != "" {
		return lastAttempt.FailureAttribution,
			diagnosticConfidence(lastAttempt, "medium"), lastAttempt.FailureStage
	}
	if s.writeMaxMs >= 1000 || s.writeTotalMs >= 2000 {
		return "downstream_write", "medium", "downstream_write"
	}
	if gatewayPreUpstreamMs(s) >= 1000 || s.admissionMs >= 1000 ||
		s.quotaMs >= 1000 || s.credentialSelectMs >= 1000 {
		return "gateway_internal", "medium", "gateway_pre_upstream"
	}
	if s.responseStatus >= 500 {
		return "gateway_internal", "medium", "gateway_response"
	}
	return "", "", ""
}

func lastAttemptDiagnostics(attempts []attemptDiagnostics) *attemptDiagnostics {
	if len(attempts) == 0 {
		return nil
	}
	return &attempts[len(attempts)-1]
}

// slowestUpstreamPhase chooses the most specific observed upstream bottleneck.
// TTFB is deliberately a fallback, not a peer of DNS/connect/TLS/headers:
// it contains those spans, so comparing raw values directly would always pick
// TTFB and hide the lower-level cause. Post-TTFB generation is a separate span
// and is compared normally.
func slowestUpstreamPhase(a *attemptDiagnostics) string {
	if a == nil {
		return "upstream_call"
	}
	phases := []struct {
		name string
		ms   int64
	}{
		{"upstream_dns", a.DNSMs},
		{"upstream_connect", a.ConnectMs},
		{"upstream_tls", a.TLSMs},
		{"upstream_headers", a.HeadersMs},
		{"upstream_generation", a.PostTTFBMs},
	}
	best := "upstream_call"
	var max int64
	for _, p := range phases {
		if p.ms > max {
			best, max = p.name, p.ms
		}
	}
	// No lower-level milestone but a measurable TTFB means the time was spent
	// waiting for the upstream's first event after the transport phases.
	if max == 0 && a.TTFBMs > 0 {
		return "upstream_ttft"
	}
	if a.TTFBMs >= 5000 && max < 5000 {
		return "upstream_ttft"
	}
	return best
}

func diagnosticConfidence(a *attemptDiagnostics, fallback string) string {
	if a != nil && a.AttributionConfidence != "" {
		return a.AttributionConfidence
	}
	return fallback
}

func inspectRequest(req *rosetta.ChatRequest, stream bool) ingressSnapshot {
	s := ingressSnapshot{stream: stream}
	if req == nil {
		return s
	}
	s.model = req.Model
	s.messages = len(req.Messages)
	s.tools = len(req.Tools)
	for _, m := range req.Messages {
		for _, b := range m.Blocks {
			switch b.Type {
			case rosetta.BlockText:
				s.inputChars += len([]rune(b.Text))
			case rosetta.BlockToolResult:
				s.toolResults++
				s.inputChars += len([]rune(b.Content))
			case rosetta.BlockToolCall:
				s.inputChars += len([]rune(b.Arguments))
			}
		}
	}
	return s
}

// responseObserver wraps a ResponseWriter to measure downstream write and
// flush latency. It deliberately delegates all optional interfaces through
// Unwrap so ResponseController can still reach the real net.Conn.
type responseObserver struct {
	http.ResponseWriter
	mu          sync.Mutex
	status      int
	wroteHeader bool
	bytes       int64
	writes      int64
	writeMs     int64
	writeMax    int64
	flushes     int64
	flushMs     int64
	flushMax    int64
}

func observeResponse(w http.ResponseWriter) *responseObserver {
	return &responseObserver{ResponseWriter: w, status: http.StatusOK}
}

func (w *responseObserver) WriteHeader(code int) {
	w.mu.Lock()
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.mu.Unlock()
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseObserver) statusCode() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func (w *responseObserver) Write(b []byte) (int, error) {
	start := time.Now()
	n, err := w.ResponseWriter.Write(b)
	ms := time.Since(start).Milliseconds()
	w.mu.Lock()
	w.wroteHeader = true
	w.bytes += int64(n)
	w.writes++
	w.writeMs += ms
	if ms > w.writeMax {
		w.writeMax = ms
	}
	w.mu.Unlock()
	return n, err
}

func (w *responseObserver) Flush() {
	start := time.Now()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
	ms := time.Since(start).Milliseconds()
	w.mu.Lock()
	w.flushes++
	w.flushMs += ms
	if ms > w.flushMax {
		w.flushMax = ms
	}
	w.mu.Unlock()
}

func (w *responseObserver) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

type responseStats struct {
	status   int
	bytes    int64
	writes   int64
	writeMs  int64
	writeMax int64
	flushes  int64
	flushMs  int64
	flushMax int64
}

func (w *responseObserver) snapshot() responseStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return responseStats{
		status: w.status, bytes: w.bytes,
		writes: w.writes, writeMs: w.writeMs, writeMax: w.writeMax,
		flushes: w.flushes, flushMs: w.flushMs, flushMax: w.flushMax,
	}
}

func traceIDFromTraceparent(value string) string {
	parts := strings.Split(strings.TrimSpace(value), "-")
	if len(parts) != 4 || len(parts[1]) != 32 {
		return ""
	}
	return parts[1]
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func finishAttempt(attempt *attemptDiagnostics, trace *upstream.AttemptTrace, started time.Time, out Outcome, err error, diag *requestDiagnostics) {
	if attempt == nil {
		return
	}
	applyTraceTimings(attempt, trace)
	attempt.DurationMs = time.Since(started).Milliseconds()
	// Non-stream callers have already measured the SDK call wall time. Only
	// fill the fallback when no call span was recorded; overwriting it with the
	// whole attempt would fold request construction into the upstream latency.
	if !out.Committed && attempt.UpstreamCallMs == 0 {
		attempt.UpstreamCallMs = attempt.DurationMs
	}
	attempt.Status = outcomeDiagnosticStatus(out)
	if out.Success {
		attempt.ErrorCode = ""
	} else {
		attempt.ErrorCode = out.Code
	}
	attempt.ErrorMessage = out.Message
	attempt.ErrorDetail = out.Detail
	attempt.Committed = out.Committed
	attempt.Eligible = out.Eligible
	attempt.Success = out.Success
	appendAttemptError(attempt, err)
	setAttemptDiagnosis(attempt, out, err)
	if diag != nil {
		if id := upstreamRequestID(err); id != "" {
			diag.setUpstreamRequest(id)
		}
		diag.addAttempt(*attempt)
	}
}

// applyTraceTimings copies transport milestones into an attempt. Successful
// streamed attempts only reach their terminal state after the SDK call returns,
// so they call this again immediately before recording the attempt.
func applyTraceTimings(attempt *attemptDiagnostics, trace *upstream.AttemptTrace) {
	if attempt == nil || trace == nil {
		return
	}
	t := trace.Snapshot()
	attempt.DNSMs = t.DNSMs
	attempt.ConnectMs = t.ConnectMs
	attempt.TLSMs = t.TLSMs
	attempt.HeadersMs = t.HeadersMs
	attempt.RoundTripMs = t.RoundTripMs
	attempt.RoundTrips = t.RoundTrips
	attempt.FirstRoundTripMs = t.FirstRoundTripMs
	attempt.LastRoundTripMs = t.LastRoundTripMs
	attempt.ReusedConnection = t.ReusedConn
	attempt.UpstreamHTTPStatus = t.HTTPStatus
	attempt.DNSStarted = t.DNSStarted
	attempt.DNSDone = t.DNSDone
	attempt.ConnectStarted = t.ConnectStarted
	attempt.ConnectDone = t.ConnectDone
	attempt.TLSStarted = t.TLSStarted
	attempt.TLSDone = t.TLSDone
	attempt.RequestWritten = t.WroteRequest
	attempt.GotConnection = t.GotConn
}

func outcomeDiagnosticStatus(out Outcome) string {
	switch {
	case out.Success:
		return "ok"
	case out.ClientGone:
		return "canceled"
	case out.GatewayTimeout:
		return "timeout"
	case out.Committed:
		return "committed_fail"
	default:
		return "failed"
	}
}

func appendAttemptError(attempt *attemptDiagnostics, err error) {
	if err == nil {
		return
	}
	attempt.ErrorMessage = err.Error()
	var apiErr *rosetta.APIError
	if errors.As(err, &apiErr) {
		attempt.UpstreamID = apiErr.RequestID
		attempt.ErrorType = apiErr.Type
		attempt.ProviderErrorCode = apiErr.Code
		attempt.ErrorParam = apiErr.Param
		if attempt.UpstreamHTTPStatus == 0 {
			attempt.UpstreamHTTPStatus = apiErr.StatusCode
		}
	}
}

func setAttemptDiagnosis(attempt *attemptDiagnostics, out Outcome, err error) {
	if attempt == nil || (err == nil && out.Success && attempt.ErrorCode == "") {
		return
	}
	// Terminal semantics take precedence over the wrapped transport error.
	// A client cancel is also surfaced by the SDK as a TransportError; checking
	// the concrete error first would blame the upstream's network for a user
	// pressing Stop.
	switch attempt.ErrorCode {
	case "downstream_write_failed":
		attempt.FailureAttribution = "downstream_write"
		attempt.AttributionConfidence = "high"
		attempt.FailureStage = "downstream_response_write"
		return
	case "stream_idle_timeout":
		attempt.FailureAttribution = "upstream_timeout"
		attempt.AttributionConfidence = "high"
		attempt.FailureStage = "upstream_stream_idle"
		return
	case "stream_truncated":
		attempt.FailureAttribution = "upstream_network"
		attempt.AttributionConfidence = "medium"
		attempt.FailureStage = "upstream_stream"
		return
	case "stream_overflow":
		attempt.FailureAttribution = "gateway_internal"
		attempt.AttributionConfidence = "high"
		attempt.FailureStage = "gateway_stream_accumulator"
		return
	case "gateway_timeout":
		attempt.FailureAttribution = "gateway_timeout"
		attempt.AttributionConfidence = "high"
		attempt.FailureStage = "gateway_budget"
		return
	case "upstream_timeout":
		attempt.FailureAttribution = "upstream_timeout"
		attempt.AttributionConfidence = "high"
		attempt.FailureStage = "upstream_ttft"
		if err != nil && !attempt.RequestWritten {
			attempt.FailureStage = transportFailureStage(attempt, err)
		}
		return
	case "upstream_error":
		if err == nil && !out.Committed {
			attempt.FailureAttribution = "upstream_provider_error"
			attempt.AttributionConfidence = "high"
			attempt.FailureStage = "upstream_stream"
			return
		}
	case "server_overloaded":
		attempt.FailureAttribution = "gateway_overload"
		attempt.AttributionConfidence = "high"
		attempt.FailureStage = "gateway_admission"
		return
	case "no_healthy_credential", "probe_busy_or_cooling":
		attempt.FailureAttribution = "target_configuration"
		attempt.AttributionConfidence = "high"
		attempt.FailureStage = "target_credential_pool"
		return
	}
	if out.ClientGone {
		attempt.FailureAttribution = "downstream_client"
		attempt.AttributionConfidence = "high"
		attempt.FailureStage = "downstream_request"
		if out.Committed {
			attempt.FailureStage = "downstream_stream"
		}
		return
	}
	if out.GatewayTimeout {
		attempt.FailureAttribution = "gateway_timeout"
		attempt.AttributionConfidence = "high"
		attempt.FailureStage = "gateway_budget"
		return
	}
	var apiErr *rosetta.APIError
	var transportErr *rosetta.TransportError
	switch {
	case errors.As(err, &apiErr):
		switch {
		case apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden:
			attempt.FailureAttribution = "upstream_auth"
			attempt.AttributionConfidence = "high"
			attempt.FailureStage = "upstream_http_response"
		case apiErr.StatusCode == http.StatusPaymentRequired:
			attempt.FailureAttribution = "upstream_quota"
			attempt.AttributionConfidence = "high"
			attempt.FailureStage = "upstream_http_response"
		case apiErr.StatusCode == http.StatusTooManyRequests:
			attempt.FailureAttribution = "upstream_rate_limit"
			attempt.AttributionConfidence = "high"
			attempt.FailureStage = "upstream_http_response"
		case apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusGone:
			attempt.FailureAttribution = "target_configuration"
			attempt.AttributionConfidence = "high"
			attempt.FailureStage = "upstream_http_response"
		case apiErr.StatusCode == http.StatusBadRequest || apiErr.StatusCode == http.StatusUnprocessableEntity:
			attempt.FailureAttribution = "caller_configuration"
			attempt.AttributionConfidence = "high"
			attempt.FailureStage = "upstream_http_response"
		case apiErr.StatusCode >= 500:
			attempt.FailureAttribution = "upstream_provider_error"
			attempt.AttributionConfidence = "high"
			attempt.FailureStage = "upstream_http_response"
		default:
			attempt.FailureAttribution = "upstream_provider_error"
			attempt.AttributionConfidence = "medium"
			attempt.FailureStage = "upstream_http_response"
		}
		if apiErr.InBand {
			attempt.FailureStage = "upstream_response_body"
		}
	case errors.As(err, &transportErr):
		attempt.FailureAttribution = "upstream_network"
		attempt.AttributionConfidence = "medium"
		attempt.FailureStage = transportFailureStage(attempt, transportErr.Err)
		if attempt.ProxyConfigured && (attempt.FailureStage == "upstream_dns" ||
			attempt.FailureStage == "upstream_connect" || attempt.FailureStage == "upstream_tls") {
			attempt.FailureAttribution = "network_or_proxy"
		}
		if errors.Is(transportErr.Err, context.DeadlineExceeded) || isTimeout(transportErr.Err) {
			attempt.FailureAttribution = "upstream_timeout"
		}
	case errors.Is(err, rosetta.ErrUpstreamMalformed):
		attempt.FailureAttribution = "upstream_provider_error"
		attempt.AttributionConfidence = "high"
		attempt.FailureStage = "upstream_decode"
	case errors.Is(err, rosetta.ErrStreamTruncated):
		attempt.FailureAttribution = "upstream_network"
		attempt.AttributionConfidence = "medium"
		attempt.FailureStage = "upstream_stream"
	case errors.Is(err, rosetta.ErrStreamOverflow):
		attempt.FailureAttribution = "gateway_internal"
		attempt.AttributionConfidence = "high"
		attempt.FailureStage = "gateway_stream_accumulator"
	default:
		switch {
		case errors.Is(err, rosetta.ErrInvalidRequest), errors.Is(err, rosetta.ErrContextTooLong):
			attempt.FailureAttribution = "caller_configuration"
			attempt.AttributionConfidence = "high"
			attempt.FailureStage = "request_validation"
		case errors.Is(err, rosetta.ErrNoEndpoint), errors.Is(err, rosetta.ErrNoAPIKey),
			errors.Is(err, rosetta.ErrUnknownModel), errors.Is(err, rosetta.ErrThinkingUnsupported),
			errors.Is(err, rosetta.ErrNotSupported):
			attempt.FailureAttribution = "target_configuration"
			attempt.AttributionConfidence = "high"
			attempt.FailureStage = "target_configuration"
		case out.Code == "invalid_request_error", out.Code == "request_too_large":
			attempt.FailureAttribution = "caller_configuration"
			attempt.AttributionConfidence = "high"
			attempt.FailureStage = "inbound_request"
		case out.Code == "internal_error":
			attempt.FailureAttribution = "gateway_internal"
			attempt.AttributionConfidence = "high"
			attempt.FailureStage = "gateway_internal"
		}
	}
}

// transportFailureStage prefers the observed httptrace milestone over string
// matching. If the failure happened before any connection milestone was
// recorded, it remains "transport"; the network/proxy attribution stays
// medium-confidence because DNS and connect can fail on a direct route too.
func transportFailureStage(attempt *attemptDiagnostics, err error) string {
	if attempt != nil {
		switch {
		case attempt.DNSStarted && !attempt.DNSDone:
			return "upstream_dns"
		case attempt.DNSDone && attempt.ConnectStarted && !attempt.ConnectDone:
			return "upstream_connect"
		case attempt.ConnectDone && attempt.TLSStarted && !attempt.TLSDone:
			return "upstream_tls"
		case attempt.GotConnection && !attempt.RequestWritten:
			return "upstream_request_write"
		case attempt.RequestWritten:
			return "upstream_headers"
		}
	}
	// Completion flags cannot tell a failed DNS lookup from a failed dial when
	// both callbacks fire. The wrapped concrete error still can.
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "upstream_dns"
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return "upstream_tls"
	}
	var recordErr tls.RecordHeaderError
	if errors.As(err, &recordErr) {
		return "upstream_tls"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return "upstream_connect"
	}
	if isTimeout(err) {
		return "upstream_timeout"
	}
	return "upstream_transport"
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func upstreamRequestID(err error) string {
	var apiErr *rosetta.APIError
	if errors.As(err, &apiErr) {
		return apiErr.RequestID
	}
	return ""
}

func attemptFromCandidate(index int, cand routing.Candidate, trace *upstream.AttemptTrace) attemptDiagnostics {
	a := attemptDiagnostics{Index: index}
	if cand.Provider != nil {
		a.TargetID = cand.TargetID
		a.ProviderID = cand.Provider.ID
		a.ProviderName = cand.Provider.Name
		a.ProviderSlug = cand.Provider.Slug
		a.Protocol = cand.Provider.Protocol
		a.EndpointHost = upstream.EndpointHost(cand.Provider.Endpoint)
		a.NetworkID = cand.Provider.NetworkID
		a.ProxyConfigured = cand.Provider.ProxyConfigured
	}
	a.UpstreamModel = cand.UpstreamModel.ModelID
	if trace != nil {
		t := trace.Snapshot()
		a.DNSMs = t.DNSMs
		a.ConnectMs = t.ConnectMs
		a.TLSMs = t.TLSMs
		a.HeadersMs = t.HeadersMs
		a.RoundTripMs = t.RoundTripMs
		a.RoundTrips = t.RoundTrips
		a.FirstRoundTripMs = t.FirstRoundTripMs
		a.LastRoundTripMs = t.LastRoundTripMs
		a.ReusedConnection = t.ReusedConn
		a.UpstreamHTTPStatus = t.HTTPStatus
		a.DNSStarted = t.DNSStarted
		a.DNSDone = t.DNSDone
		a.ConnectStarted = t.ConnectStarted
		a.ConnectDone = t.ConnectDone
		a.TLSStarted = t.TLSStarted
		a.TLSDone = t.TLSDone
		a.RequestWritten = t.WroteRequest
		a.GotConnection = t.GotConn
	}
	return a
}

func providerID(cand routing.Candidate) string {
	if cand.Provider == nil {
		return ""
	}
	return cand.Provider.ID
}

func providerName(cand routing.Candidate) string {
	if cand.Provider == nil {
		return ""
	}
	return cand.Provider.Name
}

func providerSlug(cand routing.Candidate) string {
	if cand.Provider == nil {
		return ""
	}
	return cand.Provider.Slug
}

// upstreamProtocolName maps the SDK's detected protocol to the stable values
// stored in usage history and shown by the admin UI. Reading it from the
// selected client (rather than the provider config) makes an "auto" provider
// report the protocol it actually negotiated.
func upstreamProtocolName(p rosetta.Protocol) string {
	switch p {
	case rosetta.ProtoOpenAIChat:
		return "chat"
	case rosetta.ProtoOpenAIResponses:
		return "response"
	case rosetta.ProtoAnthropic:
		return "anthropic"
	default:
		return string(p)
	}
}

func providerEndpointHost(cand routing.Candidate) string {
	if cand.Provider == nil {
		return ""
	}
	return upstream.EndpointHost(cand.Provider.Endpoint)
}

func providerNetworkID(cand routing.Candidate) string {
	if cand.Provider == nil {
		return ""
	}
	return cand.Provider.NetworkID
}

func providerProxyConfigured(cand routing.Candidate) bool {
	return cand.Provider != nil && cand.Provider.ProxyConfigured
}
