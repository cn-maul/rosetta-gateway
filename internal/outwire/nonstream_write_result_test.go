package outwire

import (
	"errors"
	"net/http"
	"testing"

	"github.com/cn-maul/rosetta"
)

func TestR0AC12_NonStreamWritersReportWriteFailure(t *testing.T) {
	boom := errors.New("downstream connection is gone")
	resp := &rosetta.ChatResponse{
		ID:         "resp-1",
		Content:    []rosetta.Block{{Type: rosetta.BlockText, Text: "hello"}},
		StopReason: rosetta.StopEnd,
		Usage:      rosetta.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8},
	}

	for _, tc := range []struct {
		name string
		call func(http.ResponseWriter) NonStreamWriteResult
	}{
		{"openai_chat", func(w http.ResponseWriter) NonStreamWriteResult {
			return WriteNonStreamResponse(w, resp, "public-model")
		}},
		{"anthropic", func(w http.ResponseWriter) NonStreamWriteResult {
			return WriteAnthropicResponse(w, resp, "public-model")
		}},
		{"openai_responses", func(w http.ResponseWriter) NonStreamWriteResult {
			return WriteResponsesResponseWithTools(w, resp, "public-model", nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &errWriter{remaining: 0, err: boom}
			got := tc.call(w)
			if got.Err == nil {
				t.Fatalf("%s ignored the downstream write error", tc.name)
			}
			if !errors.Is(got.Err, boom) {
				t.Fatalf("%s error = %v, want wrapped %v", tc.name, got.Err, boom)
			}
			if !got.Committed {
				t.Fatalf("%s must report committed after WriteHeader", tc.name)
			}
		})
	}
}

func TestR0AC12_NonStreamWritersDoNotReportSuccessBeforeBodyIsWritten(t *testing.T) {
	resp := &rosetta.ChatResponse{
		ID:         "resp-1",
		Content:    []rosetta.Block{{Type: rosetta.BlockText, Text: "hello"}},
		StopReason: rosetta.StopEnd,
		Usage:      rosetta.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8},
	}
	w := &headerOnlyWriter{status: http.StatusNoContent}

	for _, tc := range []struct {
		name string
		call func(http.ResponseWriter) NonStreamWriteResult
	}{
		{"openai_chat", func(w http.ResponseWriter) NonStreamWriteResult {
			return WriteNonStreamResponse(w, resp, "public-model")
		}},
		{"anthropic", func(w http.ResponseWriter) NonStreamWriteResult {
			return WriteAnthropicResponse(w, resp, "public-model")
		}},
		{"openai_responses", func(w http.ResponseWriter) NonStreamWriteResult {
			return WriteResponsesResponseWithTools(w, resp, "public-model", nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.call(w); got.Err == nil {
				t.Fatalf("%s accepted a short Write without surfacing it", tc.name)
			}
		})
	}
}

func TestR0AC12_EncodeFailureIsNotReportedAsCommitted(t *testing.T) {
	got := encodeNonStreamPayload(&errWriter{}, map[string]any{"unsupported": func() {}})
	if got.Err == nil {
		t.Fatal("unsupported payload encoded successfully")
	}
	if got.Committed {
		t.Fatal("encode failure before WriteHeader must not be committed")
	}
	if !got.EncodeError() {
		t.Fatal("encode failure must be distinguishable from a downstream write failure")
	}
}

type headerOnlyWriter struct {
	status int
	header http.Header
}

func (w *headerOnlyWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *headerOnlyWriter) WriteHeader(status int) { w.status = status }
func (w *headerOnlyWriter) Write([]byte) (int, error) {
	return 0, nil
}
