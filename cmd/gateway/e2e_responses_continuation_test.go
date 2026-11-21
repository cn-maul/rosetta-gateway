package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/ingress"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

func responsesBody(previous string, stream bool) string {
	previousField := ""
	if previous != "" {
		previousField = fmt.Sprintf(`"previous_response_id":%q,`, previous)
	}
	return fmt.Sprintf(`{
		"model":"flash","stream":%t,%s
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]
	}`, stream, previousField)
}

type terminalResponseWriter struct {
	*httptest.ResponseRecorder
	failed bool
}

func (writer *terminalResponseWriter) Write(body []byte) (int, error) {
	if strings.Contains(string(body), "event: response.completed\n") {
		writer.failed = true
		return 0, errors.New("write: broken pipe")
	}
	return writer.ResponseRecorder.Write(body)
}

func TestE2E_ResponsesContinuationTerminalWriteFailure(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, `data: {"type":"response.created","response":{"id":"resp_failed","status":"in_progress","output":[]}}`+"\n\n")
		_, _ = fmt.Fprint(w, `data: {"type":"response.completed","response":{"id":"resp_failed","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}}`+"\n\n")
	})
	h, db, _ := buildHarnessFull(t, []harnessChain{{slug: "up", url: up.URL(), protocol: "response"}}, false, ingress.NewResponses())
	writer := &terminalResponseWriter{ResponseRecorder: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(responsesBody("", true)))
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	h(writer, req)
	if !writer.failed {
		t.Fatal("terminal write failure was not exercised")
	}
	rows := e2eWaitUsageCount(t, db, "k1", 1)
	if rows[0].Status != store.UsageStatusCanceled || rows[0].ErrorCode != "downstream_write_failed" {
		t.Fatalf("terminal write usage = %q/%q", rows[0].Status, rows[0].ErrorCode)
	}
	rec := e2ePost(h, "/v1/responses", testAccessKey, responsesBody("resp_failed", false))
	if rec.Code != http.StatusBadRequest || up.Count() != 1 {
		t.Fatalf("failed terminal left a binding: status=%d calls=%d body=%s", rec.Code, up.Count(), rec.Body.String())
	}
}

func responsesJSON(id, content string) string {
	idField := ""
	if id != "" {
		idField = fmt.Sprintf(`"id":%q,`, id)
	}
	return fmt.Sprintf(`{%s"object":"response","created_at":1,"model":"first-model",
		"status":"completed",
		"output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant",
			"content":[{"type":"output_text","text":%q}]}],
		"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`, idField, content)
}

// A continuation must stay on the target that produced the response, even when
// another target in the chain is healthy. Silent failover here would send the
// upstream's response id to a server that has never seen it.
func TestE2E_ResponsesContinuationPinsTarget(t *testing.T) {
	firstCalls := 0
	first := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		firstCalls++
		if firstCalls > 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responsesJSON("resp_up_1", "pong")))
	})
	second := newFakeUpstream(t, replyOK("wrong-target"))

	h := buildHarnessProtocols(t, []harnessChain{
		{slug: "first", url: first.URL(), protocol: "response"},
		{slug: "second", url: second.URL(), protocol: "response"},
	}, true, ingress.NewResponses())

	rec := e2ePost(h, "/v1/responses", testAccessKey, responsesBody("", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("first turn: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var firstResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &firstResp); err != nil {
		t.Fatalf("first turn decode: %v", err)
	}
	if firstResp.ID != "resp_up_1" {
		t.Fatalf("client response id = %q, want resp_up_1", firstResp.ID)
	}

	// The pinned target now fails. The request must fail rather than silently
	// continue on "second", which never issued resp_up_1.
	rec2 := e2ePost(h, "/v1/responses", testAccessKey, responsesBody(firstResp.ID, false))
	if rec2.Code < 500 {
		t.Fatalf("pinned target failure must not be replaced by another target: got %d body=%s",
			rec2.Code, rec2.Body.String())
	}
	if second.Count() != 0 {
		t.Fatalf("continuation silently switched targets: second upstream called %d times", second.Count())
	}

	// An unknown continuation id is a client error, and must not touch any
	// upstream.
	before := first.Count() + second.Count()
	rec3 := e2ePost(h, "/v1/responses", testAccessKey, responsesBody("resp_unknown", false))
	if rec3.Code != http.StatusBadRequest || !strings.Contains(rec3.Body.String(), "invalid_request_error") {
		t.Fatalf("unknown continuation: want 400 invalid_request_error, got %d body=%s",
			rec3.Code, rec3.Body.String())
	}
	if after := first.Count() + second.Count(); after != before {
		t.Fatalf("unknown continuation reached an upstream: calls %d -> %d", before, after)
	}
}

// Upstreams are not required to echo an id. The gateway generates one for the
// client, so that generated id is the only continuation key the caller can
// quote back.
func TestE2E_ResponsesContinuationUsesGeneratedID(t *testing.T) {
	var bodies []string
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		bodies = append(bodies, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responsesJSON("", "pong")))
	})

	h := buildHarnessProtocols(t, []harnessChain{
		{slug: "up", url: up.URL(), protocol: "response"},
	}, false, ingress.NewResponses())

	rec := e2ePost(h, "/v1/responses", testAccessKey, responsesBody("", false))
	if rec.Code != http.StatusOK {
		t.Fatalf("first turn: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(out.ID, "resp_") {
		t.Fatalf("gateway must generate a response id when upstream omits one, got %q", out.ID)
	}

	rec2 := e2ePost(h, "/v1/responses", testAccessKey, responsesBody(out.ID, false))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("second turn without upstream id: want 400, got %d body=%s",
			rec2.Code, rec2.Body.String())
	}
	if len(bodies) != 1 {
		t.Fatalf("generated id reached upstream: %d requests", len(bodies))
	}
}

// Streaming responses surface response.created before any content. The id the
// client reads there — including a gateway-generated one — must be bindable.
func TestE2E_ResponsesContinuationStreamUsesDownstreamID(t *testing.T) {
	var up *fakeRecorder
	up = newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(up.LastBody(), `"stream":true`) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(responsesJSON("resp_up_s", "pong")))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, event := range []string{
			`{"type":"response.created","response":{"object":"response","created_at":1,"model":"up-model","status":"in_progress","output":[]}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_s","status":"in_progress","role":"assistant","content":[]}}`,
			`{"type":"response.output_text.delta","item_id":"msg_s","output_index":0,"content_index":0,"delta":"pong"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_s","status":"completed","role":"assistant","content":[{"type":"output_text","text":"pong"}]}}`,
			`{"type":"response.completed","response":{"object":"response","created_at":1,"model":"up-model","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`,
		} {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", event)
		}
	})

	h := buildHarnessProtocols(t, []harnessChain{
		{slug: "up", url: up.URL(), protocol: "response"},
	}, false, ingress.NewResponses())

	rec := e2ePost(h, "/v1/responses", testAccessKey, responsesBody("", true))
	if rec.Code != http.StatusOK {
		t.Fatalf("stream turn: want 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var responseID string
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, `"response.created"`) {
			continue
		}
		var event struct {
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("decode response.created: %v", err)
		}
		responseID = event.Response.ID
	}
	if responseID == "" {
		t.Fatalf("stream did not expose response.created id:\n%s", rec.Body.String())
	}

	rec2 := e2ePost(h, "/v1/responses", testAccessKey, responsesBody(responseID, false))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("continuation without upstream id %q: want 400, got %d body=%s",
			responseID, rec2.Code, rec2.Body.String())
	}
	if up.Count() != 1 {
		t.Fatalf("generated streamed id reached upstream: %d requests", up.Count())
	}
}
