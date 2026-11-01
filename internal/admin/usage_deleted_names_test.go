package admin

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// 2026-11-01：调用历史里，被删掉的密钥与上游要**仍然显示当时的名字**，
// 而不是退化成一串 sha256 id。
//
// # 为什么要新增落库列
//
// 改动前密钥名是 `COALESCE(a.name,”)` 从 LEFT JOIN access_keys 取来的：
// 密钥一删，JOIN 落空，几百行历史从此只剩 id。而「这个 key 是谁的、叫什么」
// 恰恰是事后追账时最想知道的。上游名（provider_name）本来就是落库固化的，
// 密钥名照同一个口径补上。
//
// # 为什么还要两个 deleted 标志
//
// 名字固化之后，「空名字」不再等于「对象被删」—— 它也可能是升级前的旧记录
// （那时还没有这一列）。所以「还在不在库里」必须由后端单独回答，前端才好
// 决定是显示「已删除 · 原名」还是「名称未固化」。
func seedDeletedNameHistory(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := t.Context()
	if err := st.CreateProvider(ctx, &store.Provider{
		ID: "pDel", Slug: "deleted-one", Name: "腾讯WorkBuddy", Protocol: "chat",
		Endpoint: "https://x.example.com/v1", Enabled: true, TimeoutMs: 60000,
	}); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	if err := st.CreateAccessKey(ctx, &store.AccessKey{
		ID: "kDel", KeyHash: "hashDel", KeyPrefix: "sk-d", Name: "生产密钥",
		Enabled: true,
	}); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	ts := time.Now().UnixMilli()
	rows := []struct {
		id, keyName, providerID, providerName string
	}{
		// 名字已固化：删掉对象之后这一行的名字必须还在。
		{"d-frozen", "生产密钥", "pDel", "腾讯WorkBuddy"},
		// 升级前的旧记录：没有固化名字，名字要靠当前表补；对象删掉后就补不到了。
		{"d-legacy", "", "pDel", ""},
	}
	for _, r := range rows {
		if err := st.CreateUsageRecord(ctx, &store.UsageRecord{
			ID: r.id, Ts: ts, AccessKeyID: "kDel", KeyName: r.keyName,
			PublicModel: "code", ProviderID: r.providerID, ProviderName: r.providerName,
			UpstreamModel: "cn:glm", IngressProtocol: "chat",
			TotalTokens: 10, Status: "ok", HTTPStatus: http.StatusOK,
			RequestID: r.id, UsageState: "reported",
		}); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
}

// 对象被删掉后，名字仍要读得到，且 deleted 标志要为真。
func TestUsage_History_KeepsDeletedNames(t *testing.T) {
	st := newScopeStore(t)
	seedDeletedNameHistory(t, st)
	ts := time.Now().UnixMilli()

	before := byRequestID(fetchHistory(t, st, fmt.Sprintf("?from=0&to=%d&limit=100", ts+1)))
	if before["d-frozen"].KeyName == "" {
		t.Fatalf("删之前 d-frozen 的 key_name 就是空的，用例前提已不成立")
	}
	if before["d-frozen"].KeyDeleted || before["d-frozen"].ProviderDeleted {
		t.Fatalf("删之前不该报 deleted：%+v", before["d-frozen"])
	}

	if err := st.DeleteAccessKey(t.Context(), "kDel"); err != nil {
		t.Fatalf("delete key: %v", err)
	}
	if err := st.DeleteProvider(t.Context(), "pDel"); err != nil {
		t.Fatalf("delete provider: %v", err)
	}

	after := byRequestID(fetchHistory(t, st, fmt.Sprintf("?from=0&to=%d&limit=100", ts+1)))

	frozen, ok := after["d-frozen"]
	if !ok {
		t.Fatalf("记录 d-frozen 不在响应里（实际 %v）", keysOfIDs(after))
	}
	if frozen.KeyName != "生产密钥" {
		t.Errorf("密钥被删之后 key_name = %q，期望仍是固化的原名「生产密钥」——"+
			"这正是新增 usage_records.key_name 列要解决的问题", frozen.KeyName)
	}
	if !frozen.KeyDeleted {
		t.Error("密钥已删除，key_deleted 却是 false —— 前端无法区分「旧记录没名字」与「密钥被删了」")
	}
	if frozen.ProviderName != "腾讯WorkBuddy" {
		t.Errorf("上游被删之后 provider_name = %q，期望仍是固化的原名", frozen.ProviderName)
	}
	if !frozen.ProviderDeleted {
		t.Error("上游已删除，provider_deleted 却是 false")
	}

	// 旧记录：没固化名字，且对象已删 → 名字就是空的，但 deleted 必须为真，
	// 这样前端才知道「这不是一条没名字的新记录」。
	legacy, ok := after["d-legacy"]
	if !ok {
		t.Fatalf("记录 d-legacy 不在响应里（实际 %v）", keysOfIDs(after))
	}
	if legacy.KeyName != "" {
		t.Errorf("旧记录本就没有 key_name，实际 %q", legacy.KeyName)
	}
	if !legacy.KeyDeleted || !legacy.ProviderDeleted {
		t.Errorf("旧记录的 deleted 标志未置位：key=%v provider=%v",
			legacy.KeyDeleted, legacy.ProviderDeleted)
	}
}

// 对象还在时，旧记录的空名字要能靠当前表补出来（升级平滑）。
func TestUsage_History_FillsLegacyNameFromLiveRows(t *testing.T) {
	st := newScopeStore(t)
	seedDeletedNameHistory(t, st)
	ts := time.Now().UnixMilli()

	rows := byRequestID(fetchHistory(t, st, fmt.Sprintf("?from=0&to=%d&limit=100", ts+1)))
	legacy, ok := rows["d-legacy"]
	if !ok {
		t.Fatalf("记录 d-legacy 不在响应里（实际 %v）", keysOfIDs(rows))
	}
	if legacy.KeyName != "生产密钥" {
		t.Errorf("旧记录没有固化名字，但密钥还在库里，应回填成当前名字；实际 %q", legacy.KeyName)
	}
	if legacy.KeyDeleted {
		t.Error("密钥还在库里，key_deleted 不该为 true")
	}
}

func byRequestID(resp usageHistoryResponse) map[string]usageHistoryEntry {
	out := make(map[string]usageHistoryEntry, len(resp.Records))
	for _, e := range resp.Records {
		out[e.RequestID] = e
	}
	return out
}

func keysOfIDs(m map[string]usageHistoryEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
