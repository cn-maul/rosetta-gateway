package outwire

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cn-maul/rosetta"
)

func TestFailoverEligible(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"500", &rosetta.APIError{StatusCode: 500}, true},
		{"503", &rosetta.APIError{StatusCode: 503}, true},
		{"401", &rosetta.APIError{StatusCode: 401}, true},
		{"403", &rosetta.APIError{StatusCode: 403}, true},
		{"402", &rosetta.APIError{StatusCode: 402}, true},
		{"429", &rosetta.APIError{StatusCode: 429}, true},
		{"408", &rosetta.APIError{StatusCode: 408}, true},
		// 404/410 是目标级配置问题（上游没有这个模型），链正是为吸收它而存在。
		// 2026-09-24 之前这里期望 false，导致链首模型被上游退役后整条链硬失败。
		{"404", &rosetta.APIError{StatusCode: 404}, true},
		{"410", &rosetta.APIError{StatusCode: 410}, true},
		{"400", &rosetta.APIError{StatusCode: 400}, false},
		{"422", &rosetta.APIError{StatusCode: 422}, false},
		{"transport", &rosetta.TransportError{Err: errors.New("dial")}, true},
		{"wrapped 500", fmt.Errorf("ctx: %w", &rosetta.APIError{StatusCode: 500}), true},
		{"truncated", fmt.Errorf("%w", rosetta.ErrStreamTruncated), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FailoverEligible(c.err); got != c.want {
				t.Fatalf("FailoverEligible(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestCredentialCooldown(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want time.Duration
	}{
		{"401", &rosetta.APIError{StatusCode: 401}, 30 * time.Minute},
		{"403", &rosetta.APIError{StatusCode: 403}, 30 * time.Minute},
		{"402", &rosetta.APIError{StatusCode: 402}, time.Hour},
		{"429", &rosetta.APIError{StatusCode: 429}, 60 * time.Second},
		{"500", &rosetta.APIError{StatusCode: 500}, 60 * time.Second},
		{"408", &rosetta.APIError{StatusCode: 408}, 60 * time.Second},
		{"transport", &rosetta.TransportError{Err: errors.New("reset")}, 60 * time.Second},
		{"400 no cooldown", &rosetta.APIError{StatusCode: 400}, 0},
		// 404 可转移但**不冷却凭据**：key 是好的，错的是目标的模型配置。
		// 若给 404 也罚 30 分钟，一个健康凭据会陪着一个配错的模型一起下线，
		// 把「目标级故障」放大成「provider 级故障」。
		{"404 no cooldown", &rosetta.APIError{StatusCode: 404}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CredentialCooldown(c.err); got != c.want {
				t.Fatalf("CredentialCooldown = %v, want %v", got, c.want)
			}
		})
	}
}

// MapUpstreamError 对可转移状态应落到 5xx 网关语义，供最终写回使用。
func TestMapUpstreamError_StillMaps(t *testing.T) {
	got, _, _ := MapUpstreamError(&rosetta.APIError{StatusCode: 500})
	if got != http.StatusBadGateway {
		t.Fatalf("500 should map to 502, got %d", got)
	}
}

// 上游 404 要落成对外的 404 model_not_found，而不是 502 + 上游原话
// （旧实现返回 502 upstream_error 并把上游那句裸 "not found" 当 message 泄给客户端）。
func TestMapUpstreamError_NotFound(t *testing.T) {
	status, code, msg := MapUpstreamError(&rosetta.APIError{StatusCode: 404, Message: "not found"})
	if status != http.StatusNotFound || code != "model_not_found" {
		t.Fatalf("404 should map to 404/model_not_found, got %d/%s", status, code)
	}
	if msg == "not found" {
		t.Fatalf("不应把上游原始 message 直接透给客户端")
	}
}
