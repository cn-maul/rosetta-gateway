package main

// 访问密钥名落库（usage_records.key_name）的端到端守护。
//
// # 为什么值得一条端到端用例
//
// 名字的来源横跨三层：鉴权快照里的 KeySnapshot.Name → auth.Context.Name →
// 五处 store.UsageRecord 字面量。这五处在 Handle / attemptStream /
// attemptNonStream / finishNonStreamEncodeFailure / finishNonStreamWriteFailure
// 各写一遍，少写一处不会编译失败 —— 那条路径的历史记录就永远没有名字。
//
// 而名字一旦没落库，**删掉密钥之后就永久补不回来**：这正是本次改动要解决的
// 问题（原先靠 LEFT JOIN access_keys 现取，删 key 即丢名字）。
//
// 单测覆盖的是各层的局部行为；这里从 HTTP 打进去、从库里读回来，确认
// 流式与非流式两条收尾路径都把当时的名字写了进去。

import (
	"context"
	"net/http"
	"testing"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// TestE2E_KeyNameRecorded 校验两条收尾路径都把当时的密钥名固化进记录。
func TestE2E_KeyNameRecorded(t *testing.T) {
	for _, stream := range []bool{false, true} {
		up := newFakeUpstream(t, pickReply(stream, "pong"))
		h, db := buildHarness(t, []harnessChain{{slug: "good", url: up.URL()}}, false)

		priceRoute(t, db, "good", "good-model", 1_000_000, 1_000_000)
		e2eSetBalance(t, db, 10_000)

		body := `{"model":"flash","messages":[{"role":"user","content":"hi"}],` +
			`"stream":` + map[bool]string{true: "true", false: "false"}[stream] + `}`
		rec := e2ePost(h, "/v1/chat/completions", testAccessKey, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("stream=%t 期望 200，实际 %d，body=%s", stream, rec.Code, rec.Body.String())
		}

		rows := e2eWaitUsageCount(t, db, "k1", 1)
		if got := keyNameOf(t, db, rows[0].ID); got != "t" {
			t.Fatalf("stream=%t 落库 key_name = %q，期望 %q —— "+
				"这条收尾路径漏了 KeyName 字段，密钥一删这段历史就只剩 id",
				stream, got, "t")
		}

		// 密钥名必须与 key_id 指向同一把钥匙：错填成别的 key 比空着更危险
		// （历史里会出现「某人的 key 名叫 t」这种张冠李戴）。
		var keyID string
		if err := db.DB().QueryRowContext(context.Background(),
			`SELECT COALESCE(access_key_id, '') FROM usage_records WHERE id = ?`, rows[0].ID).
			Scan(&keyID); err != nil {
			t.Fatalf("读 access_key_id: %v", err)
		}
		if keyID != "k1" {
			t.Fatalf("stream=%t 记录的 access_key_id = %q，期望 k1", stream, keyID)
		}
	}
}

// keyNameOf 直接读一行的 key_name 列（不扩 e2eUsageRow 的理由同 effortOf）。
func keyNameOf(t *testing.T, db *store.Store, id string) string {
	t.Helper()
	var v string
	if err := db.DB().QueryRowContext(context.Background(),
		`SELECT COALESCE(key_name, '') FROM usage_records WHERE id = ?`, id).Scan(&v); err != nil {
		t.Fatalf("读 key_name 失败: %v", err)
	}
	return v
}
