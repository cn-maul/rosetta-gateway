package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/ingress"
	"github.com/cn-maul/rosetta-gateway/internal/outwire"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

type nonStreamEncodeFailCodec struct {
	ingress.Codec
	err error
}

func (c nonStreamEncodeFailCodec) WriteNonStream(http.ResponseWriter, *rosetta.ChatResponse, string, *ingress.Request) outwire.NonStreamWriteResult {
	return outwire.NonStreamWriteResult{Err: c.err}
}

func TestR0AC13_NonStreamEncodeFailureIsFinalizedOnce(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fakeUpstreamChat(w, r)
	}))
	defer up.Close()

	codec := nonStreamEncodeFailCodec{
		Codec: ingress.NewOpenAIChat(),
		err:   errors.New("encode: unsupported response payload"),
	}
	h, db, pool := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{"up", up.URL, false}}, true, codec)
	snap := *snapshot.Get()
	snap.Runtime.FailoverFailureThreshold = 1
	snapshot.Init(&snap)
	priceRoute(t, db, "up", "up-model", 1_000_000, 1_000_000)
	e2eSetBalance(t, db, 10_000)
	startCents, startRem := e2eBalance(t, db)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"flash","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+testAccessKey)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if got := strings.Count(rec.Body.String(), "response_encode_failed"); got != 1 {
		t.Fatalf("response_encode_failed occurrences = %d, want 1; body=%s", got, rec.Body.String())
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}

	rows := e2eWaitUsageCount(t, db, "k1", 1)
	// The recorder is asynchronous. Give a duplicate finalizer time to surface.
	time.Sleep(100 * time.Millisecond)
	rows = e2eUsageRows(t, db, "k1")
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want exactly 1: %+v", len(rows), rows)
	}
	if rows[0].Status != store.UsageStatusError || rows[0].ErrorCode != "response_encode_failed" {
		t.Fatalf("usage = status %q code %q, want error/response_encode_failed",
			rows[0].Status, rows[0].ErrorCode)
	}
	endCents, endRem := e2eBalance(t, db)
	if charged := float64(startCents-endCents)*0.01 + float64(endRem-startRem)/1e6; charged != 0 {
		t.Errorf("charged = %v, want 0", charged)
	}
	if !pool.TargetAvailable("up#t0") {
		t.Error("gateway encode failure opened the target circuit")
	}
}

func TestR0AC13_NonStreamDownstreamWriteFailureIsTerminalAndNotCharged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		path  string
		body  string
		codec ingress.Codec
	}{
		{
			name:  "chat",
			path:  "/v1/chat/completions",
			body:  `{"model":"flash","stream":false,"messages":[{"role":"user","content":"hi"}]}`,
			codec: ingress.NewOpenAIChat(),
		},
		{
			name:  "responses",
			path:  "/v1/responses",
			body:  `{"model":"flash","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			codec: ingress.NewResponses(),
		},
		{
			name:  "anthropic",
			path:  "/v1/messages",
			body:  `{"model":"flash","stream":false,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
			codec: ingress.NewAnthropicMessages(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int64
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				switch tc.name {
				case "chat":
					fakeUpstreamChat(w, r)
				case "responses":
					fakeUpstreamResponses(w, r)
				default:
					fakeUpstreamAnthropic(w, r)
				}
			}))
			defer up.Close()

			h, db, _ := buildHarnessFull(t, []struct {
				slug, url string
				fail      bool
			}{{"up", up.URL, false}}, false, tc.codec)
			priceRoute(t, db, "up", "up-model", 1_000_000, 1_000_000)
			e2eSetBalance(t, db, 10_000)
			startCents, startRem := e2eBalance(t, db)

			w := newFailingWriterAfter(0, errors.New("write: broken pipe"))
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+testAccessKey)

			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				middlewareForTest(h).ServeHTTP(w, req)
			}()
			wg.Wait()

			if got := hits.Load(); got != 1 {
				t.Fatalf("upstream hits = %d, want 1: write failure must not fail over", got)
			}
			rows := e2eWaitUsageCount(t, db, "k1", 1)
			row := rows[0]
			if row.Status != "canceled" {
				t.Errorf("usage.status = %q, want canceled", row.Status)
			}
			if row.ErrorCode != "downstream_write_failed" {
				t.Errorf("usage.error_code = %q, want downstream_write_failed", row.ErrorCode)
			}
			if row.Stream {
				t.Errorf("usage.stream = %v, want false", row.Stream)
			}
			endCents, endRem := e2eBalance(t, db)
			if charged := float64(startCents-endCents)*0.01 + float64(endRem-startRem)/1e6; charged != 0 {
				t.Errorf("charged = %v, want 0", charged)
			}
			var charges int
			if err := db.DB().QueryRowContext(context.Background(),
				`SELECT COUNT(*) FROM balance_charges WHERE user_id = ? AND request_id = ?`,
				testUserID, row.RequestID).Scan(&charges); err != nil {
				t.Fatalf("count balance_charges: %v", err)
			}
			if charges != 0 {
				t.Errorf("balance_charges rows = %d, want 0", charges)
			}
		})
	}
}

func TestR0AC14_NonStreamWriteFailureDoesNotPenalizeTargetOrCredential(t *testing.T) {
	up := newFakeUpstream(t, replyOK("pong"))
	h, db, pool := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{"up", up.URL(), false}}, true, ingress.NewOpenAIChat())
	snap := *snapshot.Get()
	snap.Runtime.FailoverFailureThreshold = 1
	snapshot.Init(&snap)

	for i := 0; i < 3; i++ {
		w := newFailingWriterAfter(0, errors.New("write: broken pipe"))
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"flash","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+testAccessKey)
		h(w, req)
	}

	rows := e2eWaitUsageCount(t, db, "k1", 3)
	for _, row := range rows {
		if row.Status != store.UsageStatusCanceled {
			t.Fatalf("usage.status = %q, want canceled", row.Status)
		}
		if row.ErrorCode != "downstream_write_failed" {
			t.Fatalf("usage.error_code = %q, want downstream_write_failed", row.ErrorCode)
		}
	}
	if !pool.TargetAvailable("up#t0") {
		t.Error("downstream write failure opened the target circuit")
	}
	if _, _, err := pool.GetAnyClient("up"); err != nil {
		t.Errorf("downstream write failure cooled the credential: %v", err)
	}
}
