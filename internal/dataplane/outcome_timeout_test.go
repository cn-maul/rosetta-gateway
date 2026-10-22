package dataplane

import (
	"context"
	"net/http"
	"testing"

	"github.com/cn-maul/rosetta"
)

// TestOutcomeFromErrCtx_GatewayBudgetIsNotUpstreamFailure 钉住 *X11 的归因：
// 请求总预算到期时，同一条 TransportError 不能再被当成「上游坏了」。
func TestOutcomeFromErrCtx_GatewayBudgetIsNotUpstreamFailure(t *testing.T) {
	err := &rosetta.TransportError{Method: "POST", URL: "https://upstream.example", Err: context.DeadlineExceeded}

	plain := OutcomeFromErr(err)
	if !plain.Eligible || plain.CredentialCooldown == 0 {
		t.Fatalf("前置假设不成立：普通 TransportError 应是可转移且罚凭据，实际 %+v", plain)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-ctx.Done()

	got := OutcomeFromErrCtx(ctx, err)
	if !got.GatewayTimeout {
		t.Fatalf("预算到期必须标记 GatewayTimeout，实际 %+v", got)
	}
	if got.Eligible {
		t.Error("预算到期后不应继续转移：共享 deadline 已经到期")
	}
	if got.CredentialCooldown != 0 {
		t.Errorf("预算到期不应冷却凭据，实际 %s", got.CredentialCooldown)
	}
	if got.StatusCode != http.StatusGatewayTimeout || got.Code != "gateway_timeout" {
		t.Errorf("映射 = (%d, %q)，期望 (504, gateway_timeout)", got.StatusCode, got.Code)
	}
}
