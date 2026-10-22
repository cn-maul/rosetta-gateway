package ingress

import (
	crand "crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/inwire"
	"github.com/cn-maul/rosetta-gateway/internal/outwire"
)

// Codec abstracts the downstream protocol's request decoding, error shape,
// non-stream response encoding, and stream sink. The forwarding skeleton
// (auth, quotas, routing, failover, watchdogs, accounting) stays protocol
// independent.
type Codec interface {
	// Name is stored in usage_records.ingress_protocol.
	Name() string
	// Decode reads and parses the request body; the caller maps errors to
	// 400/413.
	Decode(r *http.Request, maxBytes int64) (*Request, error)
	// WriteError renders a gateway error code in the protocol's envelope.
	WriteError(w http.ResponseWriter, status int, code, message string)
	// WriteNonStream encodes a successful non-stream response.
	WriteNonStream(w http.ResponseWriter, resp *rosetta.ChatResponse, model string, req *Request)
	// NewSink constructs the stream encoder after SSE headers are written.
	NewSink(w http.ResponseWriter, model string, req *Request) outwire.StreamSink
}

// NewOpenAIChat returns the codec for POST /v1/chat/completions.
func NewOpenAIChat() Codec {
	return openaiChatCodec{}
}

// NewAnthropicMessages returns the codec for POST /v1/messages.
func NewAnthropicMessages() Codec {
	return anthropicMessagesCodec{}
}

// NewResponses returns the codec for POST /v1/responses.
func NewResponses() Codec {
	return openaiResponsesCodec{}
}

type openaiChatCodec struct{}

func (openaiChatCodec) Name() string { return "chat" }

func (openaiChatCodec) Decode(r *http.Request, maxBytes int64) (*Request, error) {
	req, err := inwire.DecodeOpenAIChatRequest(r, maxBytes)
	if err != nil {
		return nil, err
	}
	return &Request{
		BuildRosetta:             req.ToRosetta,
		Stream:                   req.Stream,
		Model:                    req.Model,
		WantsStreamUsage:         wantsStreamUsage(req),
		RequiresStructuredOutput: req.RequiresStructuredOutput(),
		ApplyUpstreamExtras:      req.ApplyProtocolPrivateExtra,
		RawEffort:                req.RawEffort,
	}, nil
}

func (openaiChatCodec) WriteError(w http.ResponseWriter, status int, code, message string) {
	outwire.WriteOpenAIError(w, status, code, message)
}

func (openaiChatCodec) WriteNonStream(w http.ResponseWriter, resp *rosetta.ChatResponse, model string, _ *Request) {
	outwire.WriteNonStreamResponse(w, resp, model)
}

func (openaiChatCodec) NewSink(w http.ResponseWriter, model string, _ *Request) outwire.StreamSink {
	flusher, _ := w.(http.Flusher)
	return outwire.NewOpenAISink(outwire.NewSSEWriter(w, flusher, "chatcmpl-"+newID(), model, time.Now().Unix()))
}

type anthropicMessagesCodec struct{}

func (anthropicMessagesCodec) Name() string { return "anthropic" }

func (anthropicMessagesCodec) Decode(r *http.Request, maxBytes int64) (*Request, error) {
	req, err := inwire.DecodeAnthropicMessagesRequest(r, maxBytes)
	if err != nil {
		return nil, err
	}
	return &Request{
		BuildRosetta:             req.ToRosetta,
		Stream:                   req.Stream,
		Model:                    req.Model,
		WantsStreamUsage:         false, // Anthropic message_delta always carries usage
		RequiresStructuredOutput: false, // Anthropic ingress has no structured-output concept
		ApplyUpstreamExtras:      req.ApplyUpstreamExtras,
		RawEffort:                req.RawEffort,
		AnthropicBudget:          req.ThinkingBudget(),
	}, nil
}

func (anthropicMessagesCodec) WriteError(w http.ResponseWriter, status int, code, message string) {
	outwire.WriteAnthropicError(w, status, code, message)
}

func (anthropicMessagesCodec) WriteNonStream(w http.ResponseWriter, resp *rosetta.ChatResponse, model string, _ *Request) {
	outwire.WriteAnthropicResponse(w, resp, model)
}

func (anthropicMessagesCodec) NewSink(w http.ResponseWriter, model string, _ *Request) outwire.StreamSink {
	flusher, _ := w.(http.Flusher)
	return outwire.NewAnthropicSSE(w, flusher, "msg_"+newID(), model)
}

type openaiResponsesCodec struct{}

func (openaiResponsesCodec) Name() string { return "response" }

func (openaiResponsesCodec) Decode(r *http.Request, maxBytes int64) (*Request, error) {
	req, err := inwire.DecodeResponsesRequest(r, maxBytes)
	if err != nil {
		return nil, err
	}
	return &Request{
		BuildRosetta:             req.ToRosetta,
		Stream:                   req.Stream,
		Model:                    req.Model,
		WantsStreamUsage:         false, // response.completed always carries usage
		RequiresStructuredOutput: req.RequiresStructuredOutput(),
		ApplyUpstreamExtras:      req.ApplyUpstreamExtras,
		RawEffort:                req.RawEffort,
		DowngradedTools:          req.DowngradedTools(),
	}, nil
}

func (openaiResponsesCodec) WriteError(w http.ResponseWriter, status int, code, message string) {
	outwire.WriteOpenAIError(w, status, code, message)
}

func (openaiResponsesCodec) WriteNonStream(w http.ResponseWriter, resp *rosetta.ChatResponse, model string, req *Request) {
	outwire.WriteResponsesResponseWithTools(w, resp, model, req.DowngradedTools)
}

func (openaiResponsesCodec) NewSink(w http.ResponseWriter, model string, req *Request) outwire.StreamSink {
	flusher, _ := w.(http.Flusher)
	return outwire.NewResponsesSSE(w, flusher, "resp_"+newID(), model, req.DowngradedTools)
}

// wantsStreamUsage reports whether the caller asked for OpenAI's trailing
// {"stream_options":{"include_usage":true}} chunk.
func wantsStreamUsage(req *inwire.OpenAIChatRequest) bool {
	return req.StreamOptions != nil &&
		req.StreamOptions.IncludeUsage != nil &&
		*req.StreamOptions.IncludeUsage
}

func newID() string {
	b := make([]byte, 16)
	crand.Read(b)
	return hex.EncodeToString(b)
}
