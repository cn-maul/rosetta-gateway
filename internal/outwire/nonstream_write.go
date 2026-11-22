package outwire

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// NonStreamWriteResult reports whether a non-stream response reached the
// downstream connection completely. Committed means at least the status line
// and headers may have been sent; it does not mean the body was written.
type NonStreamWriteResult struct {
	Committed bool
	Err       error
	// ResponseID is the identifier actually written to the client. Protocol
	// encoders that generate an ID when the upstream omitted one must report
	// the generated value so continuation state keys on what the caller saw,
	// not on the empty upstream value.
	ResponseID string
	// UpstreamResponseID is non-empty only when ResponseID came from the
	// upstream. A generated downstream id is not a valid upstream continuation
	// key, so the caller must not bind it.
	UpstreamResponseID string
}

// EncodeError reports a failure that happened before any downstream bytes
// were committed. The caller may still render a protocol-native 500 response.
func (r NonStreamWriteResult) EncodeError() bool {
	return r.Err != nil && !r.Committed
}

// encodeNonStreamPayload marshals the entire payload before touching the
// ResponseWriter. Encoding failures therefore remain recoverable with an error
// response; write failures are not.
func encodeNonStreamPayload(w http.ResponseWriter, payload any) NonStreamWriteResult {
	body, err := json.Marshal(payload)
	if err != nil {
		return NonStreamWriteResult{Err: fmt.Errorf("encode non-stream response: %w", err)}
	}
	return writeNonStreamPayload(w, body)
}

func writeNonStreamPayload(w http.ResponseWriter, body []byte) NonStreamWriteResult {
	w.Header().Set("Content-Type", "application/json")
	// WriteHeader commits the response. Every error from here on is terminal:
	// a retry cannot replace bytes the client may already have received.
	w.WriteHeader(http.StatusOK)
	n, err := w.Write(body)
	if err == nil && n != len(body) {
		err = fmt.Errorf("short downstream write: wrote %d of %d bytes", n, len(body))
	}
	if err != nil {
		return NonStreamWriteResult{Committed: true, Err: err}
	}
	return NonStreamWriteResult{Committed: true}
}
