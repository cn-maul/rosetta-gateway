package store

import (
	"context"
	"log/slog"
	"os"
	"testing"
)

// 用**真实单价**复刻现场：price_input=3.0 / price_cache_hit=0.1 / price_output=9.0
// 元每百万 tokens，一次 8070 输入 + 253 输出 = 0.0040294 元 = 0.40 分。
//
// 这条测试的价值在于它用的是真实数据里观察到的形状，而不是凑出来的数：
// 修复前这 6 次调用一共消费 3.33 分、实扣 2 分（只有那次 2.3 分的够一分）。
func TestRealPriceInput_SmallCallsNowGetCharged(t *testing.T) {
	ctx := context.Background()
	st := testStore(t, t.TempDir()+"/gw.db")
	seedPricedModel(t, st, 3.0, 0.1, 9.0)
	mustUser(t, st, "test", 10_000, false) // 100 元

	// 现场观测到的 6 次调用（input, output）：
	type call struct{ in, out int64 }
	calls := []call{
		{8070, 253}, // 0.0040294 元 → 0.40 分
		{7795, 30},  // 0.0013830 元 → 0.14 分
		{7708, 96},  // 0.0019016 元 → 0.19 分
		{7625, 69},  // 0.0015952 元 → 0.16 分
		{7604, 42},  // 0.0231900 元 → 2.32 分
		{523, 129},  // 0.0012452 元 → 0.12 分
	}

	totalCost := 0.0
	for i, c := range calls {
		cost, err := st.CreateUsageRecordWithCost(ctx, &UsageRecord{
			ID: string(rune('a' + i)), UserID: "test", AccessKeyID: "k1",
			PublicModel: "glm", ProviderID: "p1", UpstreamModel: "priced",
			IngressProtocol: "openai-chat", Stream: false,
			InputTokens: c.in, OutputTokens: c.out, TotalTokens: c.in + c.out,
			UsageState: "reported", Status: "ok",
			RequestID: "real-" + string(rune('a'+i)),
		})
		if err != nil {
			t.Fatalf("usage #%d: %v", i, err)
		}
		totalCost += cost
		if err := st.ChargeBalance(ctx, "test", cost, "real-"+string(rune('a'+i))); err != nil {
			t.Fatalf("charge #%d: %v", i, err)
		}
	}

	wantCents := YuanToCents(totalCost)
	gotCents, _, err := st.BalanceOf(ctx, "test")
	if err != nil {
		t.Fatalf("BalanceOf: %v", err)
	}
	charged := int64(10_000) - gotCents
	rem := readRemainder(t, st, "test")

	// 修复前：charged = 2（只有 2.32 分那次够一分），实收远低于消费。
	// 修复后：charged 应该 = 3 分（3.33 分取整 3），余数留下 0.33 分。
	t.Logf("总消费 %.6f 元 = %.2f 分；实扣 %d 分；余数 %d 微元（%.2f 分）",
		totalCost, totalCost*100, charged, rem, float64(rem)/MicrosPerCent)

	if charged != wantCents {
		t.Errorf("实扣 %d 分, want %d（四舍五入后的总消费）", charged, wantCents)
	}
	if charged <= 2 {
		t.Errorf("实扣只有 %d 分 —— 这正是修复前的症状（3.33 分消费只收到 2 分）", charged)
	}
	// 关键不变量：余额减少的分数 + 余数折算出的分数 == 真实消费的分数。
	// 单位必须统一到「分」再比 —— charged 是分、余数是微元，
	// 直接和「元」相加会差 100 倍（第一次写这条断言时正是栽在这里）。
	chargedCents := float64(charged)
	remainderCents := float64(rem) / MicrosPerCent
	accounted := chargedCents + remainderCents
	wantCentsFloat := totalCost * 100
	diff := accounted - wantCentsFloat
	if diff > 0.01 || diff < -0.01 {
		t.Errorf("账目对不上：实扣 %.4f 分 + 余数 %.4f 分 = %.4f 分，真实消费 %.4f 分",
			chargedCents, remainderCents, accounted, wantCentsFloat)
	}
}

// 老库升级：balance_remainder 列必须被补上，且存量用户余数为 0。
func TestRealPriceInput_LegacyDBGetsRemainderColumn(t *testing.T) {
	dir := t.TempDir() + "/gw.db"
	// 先正常建库，模拟「已上线」的库
	st := testStore(t, dir)
	st.Close()

	// 手工删掉余数列，模拟「修复前的 schema」
	st2 := testStore(t, dir)
	if _, err := st2.db.Exec(`ALTER TABLE users DROP COLUMN balance_remainder`); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	if _, err := st2.db.Exec(`INSERT INTO users
		(id, username, password_hash, role, status, auth_version, created_at, updated_at, balance_cents)
		VALUES ('old','old','x','user','active',1,0,0,5000)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	st2.Close()

	// 重新打开：ensureColumns 应补回该列
	st3 := testStore(t, dir)
	var rem int64
	if err := st3.read.QueryRowContext(context.Background(),
		`SELECT balance_remainder FROM users WHERE id='old'`).Scan(&rem); err != nil {
		t.Fatalf("balance_remainder 未被补上: %v", err)
	}
	if rem != 0 {
		t.Errorf("存量用户余数 = %d, want 0 —— 历史上丢掉的费用不该凭空补记", rem)
	}
	// 余额没被动过
	assertBalance(t, st3, "old", 5000, true)
}

var _ = slog.Default
var _ = os.Stderr
