package dataplane

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/cn-maul/rosetta"
	"github.com/cn-maul/rosetta-gateway/internal/routing"
	"github.com/cn-maul/rosetta-gateway/internal/store"
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
	got := committedKindForStatus(store.UsageStatusCanceled)
	if got != OutcomeClientAborted {
		t.Fatalf("committedKindForStatus(canceled) = %v, want client_aborted without a more specific code", got)
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
