package dataplane

import (
	"bytes"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta"
)

func TestSlowestUpstreamPhasePrefersConcreteTransportStage(t *testing.T) {
	attempt := &attemptDiagnostics{
		DNSMs:      18,
		ConnectMs:  5000,
		TLSMs:      20,
		HeadersMs:  180,
		TTFBMs:     5250,
		PostTTFBMs: 400,
	}
	if got := slowestUpstreamPhase(attempt); got != "upstream_connect" {
		t.Fatalf("phase = %q, want upstream_connect", got)
	}

	attempt = &attemptDiagnostics{DNSMs: 10, ConnectMs: 20, TLSMs: 30, HeadersMs: 40, TTFBMs: 6000}
	if got := slowestUpstreamPhase(attempt); got != "upstream_ttft" {
		t.Fatalf("phase = %q, want upstream_ttft", got)
	}

	attempt = &attemptDiagnostics{ConnectMs: 20, HeadersMs: 100, TTFBMs: 150, PostTTFBMs: 7000}
	if got := slowestUpstreamPhase(attempt); got != "upstream_generation" {
		t.Fatalf("phase = %q, want upstream_generation", got)
	}
}

func TestUpstreamProtocolNameUsesDetectedProtocol(t *testing.T) {
	cases := map[rosetta.Protocol]string{
		rosetta.ProtoOpenAIChat:      "chat",
		rosetta.ProtoOpenAIResponses: "response",
		rosetta.ProtoAnthropic:       "anthropic",
	}
	for protocol, want := range cases {
		if got := upstreamProtocolName(protocol); got != want {
			t.Errorf("upstreamProtocolName(%q) = %q, want %q", protocol, got, want)
		}
	}
}

func TestSetIngressRequestCapturesFullRequestShape(t *testing.T) {
	diag := newRequestDiagnostics(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	diag.setPublicModel("public-model")
	diag.setIngressRequest(&rosetta.ChatRequest{
		Model: "upstream-model",
		Messages: []rosetta.Message{
			{Role: rosetta.RoleUser, Blocks: []rosetta.Block{
				{Type: rosetta.BlockText, Text: "hello"},
				{Type: rosetta.BlockToolResult, Content: "result"},
				{Type: rosetta.BlockToolCall, Arguments: `{"x":1}`},
			}},
		},
		Tools: []rosetta.ToolDefinition{{Name: "lookup"}},
	}, true)

	got := diag.snapshot()
	if got.publicModel != "public-model" {
		t.Fatalf("public model = %q, want public-model", got.publicModel)
	}
	if got.messages != 1 || got.tools != 1 || got.toolResults != 1 {
		t.Fatalf("shape = messages:%d tools:%d tool_results:%d, want 1/1/1",
			got.messages, got.tools, got.toolResults)
	}
	if want := len([]rune("hello" + "result" + `{"x":1}`)); got.inputChars != want {
		t.Fatalf("input chars = %d, want %d", got.inputChars, want)
	}
	if !got.stream {
		t.Fatal("stream = false, want true")
	}
}

func TestSetAttemptDiagnosisSeparatesDirectAndProxyNetworkFailures(t *testing.T) {
	err := &rosetta.TransportError{Err: errors.New("dial tcp: connection refused")}
	direct := attemptDiagnostics{DNSStarted: true, DNSDone: true, ConnectStarted: true}
	setAttemptDiagnosis(&direct, Outcome{Code: "upstream_error", Eligible: true}, err)
	if direct.FailureAttribution != "upstream_network" || direct.FailureStage != "upstream_connect" {
		t.Fatalf("direct diagnosis = %+v", direct)
	}

	proxied := attemptDiagnostics{
		DNSStarted: true, DNSDone: true, ConnectStarted: true,
		ProxyConfigured: true,
	}
	setAttemptDiagnosis(&proxied, Outcome{Code: "upstream_error", Eligible: true}, err)
	if proxied.FailureAttribution != "network_or_proxy" || proxied.FailureStage != "upstream_connect" {
		t.Fatalf("proxy diagnosis = %+v", proxied)
	}
}

func TestTransportFailureStageUsesWrappedNetworkError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"dns", &net.DNSError{Err: "no such host", Name: "api.example"}, "upstream_dns"},
		{"dial", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, "upstream_connect"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attempt := attemptDiagnostics{}
			if got := transportFailureStage(&attempt, tc.err); got != tc.want {
				t.Fatalf("stage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSetAttemptDiagnosisUsesUpstreamHTTPStatus(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "upstream_auth"},
		{http.StatusPaymentRequired, "upstream_quota"},
		{http.StatusTooManyRequests, "upstream_rate_limit"},
		{http.StatusBadGateway, "upstream_provider_error"},
		{http.StatusBadRequest, "caller_configuration"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			attempt := attemptDiagnostics{}
			err := &rosetta.APIError{StatusCode: tc.status, Message: "provider rejected request"}
			setAttemptDiagnosis(&attempt, Outcome{Code: "upstream_error"}, err)
			if attempt.FailureAttribution != tc.want {
				t.Fatalf("status %d attribution = %q, want %q", tc.status, attempt.FailureAttribution, tc.want)
			}
		})
	}
}

func BenchmarkResponseObserverWrite(b *testing.B) {
	payload := bytes.Repeat([]byte("x"), 256)
	b.SetBytes(int64(len(payload)))

	b.Run("raw", func(b *testing.B) {
		w := httptest.NewRecorder()
		for range b.N {
			_, _ = w.Write(payload)
		}
	})
	b.Run("observed", func(b *testing.B) {
		w := observeResponse(httptest.NewRecorder())
		for range b.N {
			_, _ = w.Write(payload)
		}
	})
}
