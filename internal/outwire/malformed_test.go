package outwire

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta"
)

// 2026-10-10 回归：上游返回「HTTP 成功但响应体解不开」时的分类与故障转移。
//
// # 修复前
//
// SDK 用裸 fmt.Errorf 包装解码失败，三段判定（ErrInvalidRequest /
// *APIError / *TransportError）全部不命中 → 兜底 500 internal_error；
// FailoverEligible 同样不命中 → false → 故障转移链**不换目标**。
// 实测：链首返回 `200 + 半截 JSON`，健康上游被调用 0 次。
//
// # 修复后（rosetta v1.0.1 新增 ErrUpstreamMalformed）
//
// 归 502 upstream_error，且可转移 → 链换到健康上游并成功。

// TestMalformed_MapsTo502NotInternalError 是核心断言：不能是 500。
func TestMalformed_MapsTo502NotInternalError(t *testing.T) {
	err := wrapMalformedLikeSDK()

	status, code, msg := MapUpstreamError(err)
	if status == http.StatusInternalServerError || code == "internal_error" {
		t.Fatalf("畸形上游响应仍被归为网关内部错误：%d %s %q —— "+
			"这会把排障方向指向网关自己，而坏的是上游", status, code, msg)
	}
	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502（目标级故障）", status)
	}
	if code != "upstream_error" {
		t.Errorf("code = %q, want upstream_error", code)
	}
	// 响应文案必须是固定串，不含上游原文（不外泄）。
	if strings.Contains(msg, "{") || strings.Contains(msg, "invalid character") {
		t.Errorf("响应文案泄露了上游原文细节：%q", msg)
	}
}

// TestMalformed_IsFailoverEligible 是另一个核心断言：分类对了还不够，
// 不转移的话链依然一个目标都不试。
func TestMalformed_IsFailoverEligible(t *testing.T) {
	if !FailoverEligible(wrapMalformedLikeSDK()) {
		t.Error("畸形上游响应不可转移 —— 链不会换到健康上游，这正是本次要修的核心症状")
	}
}

// 反向断言：哨兵**过宽**会把整个错误分类体系压平，比不加更糟；
// 同时本哨兵**不得改变**任何既有错误的可转移性。
func TestMalformed_SentinelIsNotTooWide(t *testing.T) {
	// cases 里显式标注哪些**本就应该**可转移 —— 断言必须区分「因本哨兵而被判可转移」
	// 与「本来就该可转移」，否则这个测试会要求把既有正确行为改错。
	//
	//	APIError(401/429/5xx) 与 TransportError 可转移：既有正确行为（上游问题）。
	//	两个流哨兵不可转移：已写出字节，换上游会重放已交付的输出。
	//	其余（本地参数类、畸形哨兵、普通错误）不可转移。
	cases := []struct {
		name           string
		err            error
		wantFailoverOK bool // 该错误本来是否就该可转移
	}{
		{"APIError 401", &rosetta.APIError{StatusCode: 401, Type: "auth", Message: "no"}, true},
		{"APIError 429", &rosetta.APIError{StatusCode: 429, Type: "rate", Message: "slow"}, true},
		{"APIError 500", &rosetta.APIError{StatusCode: 500, Type: "server", Message: "oops"}, true},
		{"TransportError", &rosetta.TransportError{Method: "GET", URL: "http://x", Err: errors.New("dial failed")}, true},
		{"ErrInvalidRequest", rosetta.ErrInvalidRequest, false},
		{"ErrContextTooLong", rosetta.ErrContextTooLong, false},
		{"流截断哨兵", rosetta.ErrStreamTruncated, false},
		{"流溢出哨兵", rosetta.ErrStreamOverflow, false},
		{"普通错误", errors.New("something else entirely"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 断言一：不得命中畸形哨兵（哨兵过宽会压平整个分类体系）。
			if errors.Is(c.err, rosetta.ErrUpstreamMalformed) {
				t.Errorf("%s 错误命中了畸形哨兵：分类会被误判成「上游返回垃圾」", c.name)
			}
			// 断言二：可转移性必须与既有正确行为一致 —— 本哨兵**不得改变**它。
			if got := FailoverEligible(c.err); got != c.wantFailoverOK {
				t.Errorf("FailoverEligible = %v，want %v —— 本次改动不应改变既有错误的可转移性",
					got, c.wantFailoverOK)
			}
		})
	}
}

// wrapMalformedLikeSDK 构造与 SDK 实际形状一致的错误：
//
//	fmt.Errorf("%w: decoding openai-chat response: %w", ErrUpstreamMalformed, jsonErr)
//
// 刻意用**双层** %w：单层测不出 errors.Is 能否穿透，而这正是网关侧的用法。
//
// 原始 json 错误用**真实解析**产生（json.SyntaxError 的 msg 字段未导出，
// 构造不出来 —— 这本身也印证了 SDK 侧「错误里从不带响应体片段」）。
func wrapMalformedLikeSDK() error {
	var probe struct {
		Choices []struct {
			Delta struct{ Content string } `json:"content"`
		} `json:"choices"`
	}
	// 半截 JSON：正是「上游接受请求却返回垃圾」的典型形态。
	jsonErr := json.Unmarshal([]byte(`{"choices": [ this is not json`), &probe)
	if jsonErr == nil {
		panic("测试素材失效：这段 JSON 本该解析失败")
	}
	return fmt.Errorf("%w: decoding openai-chat response: %w",
		rosetta.ErrUpstreamMalformed, jsonErr)
}

// 断言 errors.Is 确实穿透双层包装 —— 若 SDK 某天改成单层或加了一层别的，
// 这里会立刻发现（网关侧的错误分类整体依赖它）。
func TestMalformed_ErrorsIsReachesSentinel(t *testing.T) {
	if !errors.Is(wrapMalformedLikeSDK(), rosetta.ErrUpstreamMalformed) {
		t.Fatal("errors.Is 未能穿透到哨兵 —— 网关侧分类会整体失效")
	}
	// 原始 json 错误也应保留（errors.As 能取到 Offset，排障要用）
	var se *json.SyntaxError
	if !errors.As(wrapMalformedLikeSDK(), &se) || se.Offset <= 0 {
		t.Errorf("原始解码错误未被保留：%+v", se)
	}
}
