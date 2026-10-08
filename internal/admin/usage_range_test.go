package admin

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestQueryRange_RejectsGarbageInsteadOfScanningAll 坐实另一条：
// `?from=abc` 旧实现把 ParseInt 的错误丢进 `_`，from 落到 0；
// 而 explicit 标记只看「参数非空」，于是 from=0 + explicit=true
// 被 groupRangeClause 解读为「全部历史」—— 不生成 ts >= ? 下界，
// 只剩 ts <= now，直接退化成全表扫描。
//
// usage_records 没有保留策略、行数无上界，这条是本项目最重的查询。
// stats 端点此前已修（parseOptionalUnixMilli），usage 系列三个函数漏了。
func TestQueryRange_RejectsGarbageInsteadOfScanningAll(t *testing.T) {
	cases := []struct {
		name string
		qs   string
	}{
		{"from 非数字", "from=abc"},
		{"to 非数字", "from=1000&to=xyz"},
		{"from 负数", "from=-1"},
		{"to 负数", "from=1000&to=-5"},
		{"from 溢出 int64", "from=99999999999999999999"},
		{"from 是浮点数", "from=1.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(strings.TrimPrefix(tc.qs, "?"))
			if err != nil {
				t.Fatalf("parse query: %v", err)
			}
			if _, _, _, err := queryRangeExplicit(q, 7*24*time.Hour); err == nil {
				t.Fatal("非法时间参数必须报错，实际却静默放行（会退化成全表扫描）")
			}
		})
	}
}

// TestQueryRange_ValidInputsStillWork 保证修复没有误伤正常查询。
func TestQueryRange_ValidInputsStillWork(t *testing.T) {
	q, _ := url.ParseQuery("from=1000&to=2000")
	from, to, _, err := queryRangeExplicit(q, time.Hour)
	if err != nil {
		t.Fatalf("合法参数不应报错: %v", err)
	}
	if from != 1000 || to != 2000 {
		t.Fatalf("from=%d to=%d，期望 1000/2000", from, to)
	}

	// 不传 from → 回落到默认窗口，而不是 0。
	q2, _ := url.ParseQuery("")
	from2, _, explicit2, err := queryRangeExplicit(q2, time.Hour)
	if err != nil {
		t.Fatalf("不传参数不应报错: %v", err)
	}
	if from2 == 0 {
		t.Fatal("不传 from 时应回落到默认窗口，实际为 0")
	}
	if explicit2 {
		t.Error("未传 from 时 explicit 应为 false")
	}

	// 空串等价于「未传」—— parseOptionalUnixMilli 的既有契约，前端
	// 表单清空输入后发出 ?from= 属于正常操作，不能报错。
	q4, _ := url.ParseQuery("from=&to=")
	if _, _, _, err := queryRangeExplicit(q4, time.Hour); err != nil {
		t.Fatalf("空串应被当作未传: %v", err)
	}

	// 显式 from=0 仍然表示「全部历史」—— 总览页的「全部」档依赖这个语义。
	q3, _ := url.ParseQuery("from=0")
	from3, _, explicit, err := queryRangeExplicit(q3, time.Hour)
	if err != nil {
		t.Fatalf("from=0 不应报错: %v", err)
	}
	if !explicit || from3 != 0 {
		t.Fatalf("from=0 应保留 explicit=true 且 from=0，实际 explicit=%v from=%d", explicit, from3)
	}
}
