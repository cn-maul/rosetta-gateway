package dataplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

func TestR5_AttemptOutcomeProjectsOneLaneDecision(t *testing.T) {
	tests := []struct {
		name          string
		out           Outcome
		wantKind      OutcomeKind
		wantCommitted bool
		wantSuccess   bool
		wantEligible  bool
		wantPenalty   bool
		wantUsage     string
		wantTrace     string
		wantDiag      string
	}{
		{
			name:         "uncommitted upstream failure",
			out:          OutcomeFromErr(&rosetta.APIError{StatusCode: http.StatusBadGateway}),
			wantKind:     OutcomeUpstreamFailed,
			wantEligible: true,
			wantPenalty:  true,
			wantUsage:    store.UsageStatusError,
			wantTrace:    "fail_switch",
			wantDiag:     "failed",
		},
		{
			name:      "uncommitted client cancel",
			out:       Outcome{Kind: OutcomeClientAborted},
			wantKind:  OutcomeClientAborted,
			wantUsage: store.UsageStatusCanceled,
			wantTrace: "fail_terminal",
			wantDiag:  "canceled",
		},
		{
			name:          "committed ok",
			out:           Outcome{Kind: OutcomeCommittedSuccess},
			wantKind:      OutcomeCommittedSuccess,
			wantCommitted: true,
			wantSuccess:   true,
			wantUsage:     store.UsageStatusOK,
			wantTrace:     store.UsageStatusOK,
			wantDiag:      "ok",
		},
		{
			name:          "committed truncation",
			out:           Outcome{Kind: OutcomeCommittedPartial, Code: "stream_truncated"},
			wantKind:      OutcomeCommittedPartial,
			wantCommitted: true,
			wantPenalty:   true,
			wantUsage:     store.UsageStatusTruncated,
			wantTrace:     "committed_fail",
			wantDiag:      "committed_fail",
		},
		{
			name:          "committed client abort",
			out:           Outcome{Kind: OutcomeClientAborted, Committed: true},
			wantKind:      OutcomeClientAborted,
			wantCommitted: true,
			wantUsage:     store.UsageStatusCanceled,
			wantTrace:     "committed_fail",
			wantDiag:      "canceled",
		},
		{
			name:          "committed downstream write failure",
			out:           Outcome{Kind: OutcomeDownstreamWriteFailed, Committed: true, Code: "downstream_write_failed"},
			wantKind:      OutcomeDownstreamWriteFailed,
			wantCommitted: true,
			wantUsage:     store.UsageStatusCanceled,
			wantTrace:     "committed_fail",
			wantDiag:      "canceled",
		},
		{
			name:          "gateway budget timeout",
			out:           Outcome{Kind: OutcomeGatewayTimeout, Committed: true, Code: "gateway_timeout"},
			wantKind:      OutcomeGatewayTimeout,
			wantCommitted: true,
			wantUsage:     store.UsageStatusTimeout,
			wantTrace:     "committed_fail",
			wantDiag:      "timeout",
		},
		{
			name:          "gateway overflow",
			out:           Outcome{Kind: OutcomeGatewayOverflow, Committed: true, Code: "stream_overflow"},
			wantKind:      OutcomeGatewayOverflow,
			wantCommitted: true,
			wantUsage:     store.UsageStatusOverflow,
			wantTrace:     "committed_fail",
			wantDiag:      "committed_fail",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.out.LaneDecision()
			if got.Kind != tc.wantKind {
				t.Errorf("Kind = %v, want %v", got.Kind, tc.wantKind)
			}
			out := tc.out.normalize()
			if out.Committed != tc.wantCommitted {
				t.Errorf("Committed = %v, want %v", out.Committed, tc.wantCommitted)
			}
			if out.Success != tc.wantSuccess {
				t.Errorf("Success = %v, want %v", out.Success, tc.wantSuccess)
			}
			if out.Eligible != tc.wantEligible {
				t.Errorf("Eligible = %v, want %v", out.Eligible, tc.wantEligible)
			}
			if got.PenalizeUpstream != tc.wantPenalty {
				t.Errorf("PenalizeUpstream = %v, want %v", got.PenalizeUpstream, tc.wantPenalty)
			}
			if got.UsageStatus != tc.wantUsage {
				t.Errorf("UsageStatus = %q, want %q", got.UsageStatus, tc.wantUsage)
			}
			if got.TraceOutcome != tc.wantTrace {
				t.Errorf("TraceOutcome = %q, want %q", got.TraceOutcome, tc.wantTrace)
			}
			if got.DiagnosticStatus != tc.wantDiag {
				t.Errorf("DiagnosticStatus = %q, want %q", got.DiagnosticStatus, tc.wantDiag)
			}
		})
	}
}

func TestR5_DownstreamWriteFailureKeepsItsCommittedKind(t *testing.T) {
	got := committedKindFor(store.UsageStatusCanceled, "")
	if got != OutcomeClientAborted {
		t.Fatalf("committedKindFor(canceled) = %v, want client_aborted without a more specific code", got)
	}

	out := Outcome{Kind: OutcomeClientAborted, Committed: true, Code: "downstream_write_failed"}.normalize()
	if out.Kind != OutcomeDownstreamWriteFailed {
		t.Fatalf("downstream write failure normalized to %v, want downstream_write_failed", out.Kind)
	}
	if !out.Committed || out.LaneDecision().PenalizeUpstream || !out.ClientGone {
		t.Fatalf("downstream write failure flags = %+v, want committed, no upstream penalty, client gone", out)
	}
}

func TestR5_FirstSinkWriteFailureTerminatesTheAttempt(t *testing.T) {
	out := Outcome{Kind: OutcomeDownstreamWriteFailed, Committed: true, Code: "downstream_write_failed"}.normalize()
	decision := out.LaneDecision()
	if !decision.Committed {
		t.Fatal("first sink write failure must be committed after the status line")
	}
	if decision.Eligible || decision.TraceOutcome != "committed_fail" {
		t.Fatalf("first sink write failure decision = %+v, want terminal committed failure", decision)
	}
	if decision.UsageStatus != store.UsageStatusCanceled ||
		decision.DiagnosticError != "downstream_write_failed" {
		t.Fatalf("first sink write failure accounting = status %q code %q", decision.UsageStatus, decision.DiagnosticError)
	}
}

func TestR5_OutcomeConstructorsCannotExpressConflictingBooleans(t *testing.T) {
	upstream := OutcomeFromErrCtx(context.Background(), errors.New("upstream failed"))
	if upstream.Kind != OutcomeUpstreamFailed || upstream.Committed || upstream.Success {
		t.Fatalf("uncommitted upstream result = %+v", upstream)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	client := OutcomeFromErrCtx(canceled, context.Canceled)
	if client.Kind != OutcomeClientAborted || client.Committed {
		t.Fatalf("client abort result = %+v", client)
	}

	deadline, cancelDeadline := context.WithTimeout(context.Background(), 0)
	defer cancelDeadline()
	<-deadline.Done()
	timeout := OutcomeFromErrCtx(deadline, context.DeadlineExceeded)
	if timeout.Kind != OutcomeGatewayTimeout || timeout.Eligible {
		t.Fatalf("gateway timeout result = %+v", timeout)
	}
}

func TestR5_ClientCancellationWinsOverWrappedUpstreamError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := OutcomeFromErrCtx(ctx, &rosetta.APIError{
		StatusCode: http.StatusBadGateway,
		Message:    "upstream failed after cancellation",
	})
	if out.Kind != OutcomeClientAborted {
		t.Fatalf("Kind = %v, want client_aborted", out.Kind)
	}
	if got := out.LaneDecision().UsageStatus; got != store.UsageStatusCanceled {
		t.Fatalf("usage status = %q, want canceled", got)
	}
}

func TestR5_BalancePreflightUsesAttemptSet(t *testing.T) {
	cheap := routing.Candidate{
		TargetID:      "cheap#t0",
		Provider:      &routing.ProviderRef{ID: "cheap", Slug: "cheap"},
		UpstreamModel: &routing.UpstreamModel{ID: "cheap/m", PriceOutput: 1},
	}
	expensive := routing.Candidate{
		TargetID:      "expensive#t1",
		Provider:      &routing.ProviderRef{ID: "expensive", Slug: "expensive"},
		UpstreamModel: &routing.UpstreamModel{ID: "expensive/m", PriceOutput: 1_000_000},
	}

	if got := preflightCandidates(false, 1, []routing.Candidate{cheap, expensive}); len(got) != 1 || got[0].Provider.ID != "cheap" {
		t.Fatalf("disabled failover candidates = %+v, want only the head target", got)
	}
	if got := preflightCandidates(true, 1, []routing.Candidate{cheap, expensive}); len(got) != 1 || got[0].Provider.ID != "cheap" {
		t.Fatalf("attempt budget candidates = %+v, want only the attempted head target", got)
	}
	if got := preflightCandidates(true, 2, []routing.Candidate{cheap, expensive}); len(got) != 2 {
		t.Fatalf("enabled failover candidates = %+v, want the full attempted set", got)
	}
}

// stream_malformed 必须与普通已提交失败分开：它的内容其实已经交付，
// 只是上游在终止事件上给了自相矛盾的元数据。按普通失败记账会让一家
// 中转在三次请求内被整家熔断（2026-11-11 tiantoken 的真实现场）。
func TestCommittedKindSeparatesMalformedFromOrdinaryStreamFailure(t *testing.T) {
	if got := committedKindFor(store.UsageStatusError, errorCodeStreamMalformed); got != OutcomeCommittedMalformed {
		t.Fatalf("committedKindFor(error, stream_malformed) = %v，期望 committed_malformed", got)
	}
	// 其余错误码仍是普通已提交失败：这条防止有人把判据写成「status=error 就中性」。
	if got := committedKindFor(store.UsageStatusError, "upstream_error"); got != OutcomeCommittedPartial {
		t.Fatalf("committedKindFor(error, upstream_error) = %v，期望 committed_partial", got)
	}
	if got := committedKindFor(store.UsageStatusError, "stream_idle_timeout"); got != OutcomeCommittedPartial {
		t.Fatalf("committedKindFor(error, stream_idle_timeout) = %v，期望 committed_partial", got)
	}
	// 成功/取消/超时不受 code 影响。
	if got := committedKindFor(store.UsageStatusOK, errorCodeStreamMalformed); got != OutcomeCommittedSuccess {
		t.Fatalf("status=ok 被 code 改写成了 %v", got)
	}
	if got := committedKindFor(store.UsageStatusCanceled, errorCodeStreamMalformed); got != OutcomeClientAborted {
		t.Fatalf("status=canceled 被 code 改写成了 %v", got)
	}
}

func TestCommittedMalformedProjectsNeutralAccounting(t *testing.T) {
	out := Outcome{
		Kind:      OutcomeCommittedMalformed,
		Committed: true,
		Code:      errorCodeStreamMalformed,
	}.normalize()
	d := out.LaneDecision()

	if !d.Committed {
		t.Error("已提交的流必须标记 committed，否则外层会当成未提交失败再写一条 502")
	}
	if d.PenalizeUpstream {
		t.Error("stream_malformed 不该计入目标熔断")
	}
	if !d.NeutralUpstream {
		t.Error("stream_malformed 必须是中性记账：既不记失败也不记成功")
	}
	if d.Success {
		t.Error("上游给出自相矛盾的数据不能算成功")
	}
	// 轨迹要如实说「响应开始后失败」：客户端确实拿到了一条没有正常收尾的流。
	if d.TraceOutcome != "committed_fail" || d.TraceReason != errorCodeStreamMalformed {
		t.Errorf("轨迹投影 = %q:%q，期望 committed_fail:%s", d.TraceOutcome, d.TraceReason, errorCodeStreamMalformed)
	}
	if d.UsageStatus != store.UsageStatusError {
		t.Errorf("落库状态 = %q，期望 error", d.UsageStatus)
	}
}

// 中性记账的关键是「不清零既有失败计数」。若误走 RecordTargetSuccess，
// 一个「坏一次好一次」的上游会永远熔断不了 —— 熔断器被成功路径绕过了。
func TestCommittedMalformedKeepsTheTargetFailureCount(t *testing.T) {
	pool := upstream.NewPool(slog.New(slog.NewTextHandler(io.Discard, nil)))
	const (
		target    = "prov#t0"
		threshold = 2
	)

	// 一次真实失败：还不到阈值，目标仍可用（consecutiveFails=1）。
	pool.RecordTargetFailure(target, 0, threshold)
	if !pool.TargetAvailable(target) {
		t.Fatalf("阈值 %d 下第一次失败不该熔断", threshold)
	}

	recordCommittedOutcome(pool, Outcome{
		Kind:      OutcomeCommittedMalformed,
		Committed: true,
		Code:      errorCodeStreamMalformed,
	}.normalize().LaneDecision(), target, "", 0, threshold)

	// 再来一次真实失败。若上一步把计数清零，现在只是 1 → 仍可用 → 测试红。
	pool.RecordTargetFailure(target, 0, threshold)
	if pool.TargetAvailable(target) {
		t.Fatal("stream_malformed 不该清零目标的连续失败计数：两次真实失败后必须熔断")
	}
}
