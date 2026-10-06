package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
)

// RPM 限速端到端：key 配 rpm_limit=2 后，第 3 个请求 429 并带 Retry-After。
func TestRateLimit_RPMRejectsThirdRequest(t *testing.T) {
	up := fakeGood()
	defer up.Close()
	h, db, _ := buildHarnessFull(t, []struct {
		slug, url string
		fail      bool
	}{{slug: "good", url: up.URL}}, false, openaiChatCodec{})

	// 给 k1 配 RPM=2，并从 DB 重建快照（快照是限速额度的下发通道）。
	k, err := db.GetAccessKey(context.Background(), "k1")
	if err != nil || k == nil {
		t.Fatalf("get key: %v", err)
	}
	k.RPMLimit = 2
	if err := db.UpdateAccessKey(context.Background(), "k1", k); err != nil {
		t.Fatalf("update key: %v", err)
	}
	// 限速额度经快照下发。harness 的路由只存在于手工快照里（DB 无 routes 行），
	// 不能用 RebuildFromDB 整体重建 —— 按快照的不可变契约复制一份、只换 keys。
	old := snapshot.Get()
	newKeys := make(map[string]*snapshot.KeySnapshot, len(old.KeysByHash))
	for hash, ks := range old.KeysByHash {
		cp := *ks
		if cp.ID == "k1" {
			cp.RPMLimit = 2
		}
		newKeys[hash] = &cp
	}
	snapshot.Init(&snapshot.Snapshot{
		Routes:     old.Routes,
		Providers:  old.Providers,
		KeysByHash: newKeys,
		// UsersByID 必须一起带上：漏了它，鉴权会因「查不到归属用户」
		// 直接 401，而本测试想验证的是限速 —— 失败信息会指向限速而非缺用户。
		UsersByID: old.UsersByID,
		Runtime:   old.Runtime,
	})

	for i := 0; i < 2; i++ {
		rec := postChat(h, "flash")
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: code = %d", i+1, rec.Code)
		}
	}
	rec := postChat(h, "flash")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third request: code = %d, want 429", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rate_limit_exceeded") {
		t.Fatalf("error code missing: %s", rec.Body.String())
	}
	retryAfter := rec.Header().Get("Retry-After")
	if n, err := strconv.Atoi(retryAfter); err != nil || n < 1 || n > 61 {
		t.Fatalf("Retry-After = %q, want 1..61", retryAfter)
	}

	// TPM 额度为 0（不限）：TPM 不该拦正常请求。
	rec2 := postMessages(h, `{"model":"flash","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`)
	// 注意：RPM 已耗尽，这条走的是 /v1/messages 入口 —— 同一把 key 同一个
	// 限速器，应同样 429（限速在 ingress 骨架层，两个入口共享）。
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("cross-ingress rpm: code = %d, want 429", rec2.Code)
	}
}
