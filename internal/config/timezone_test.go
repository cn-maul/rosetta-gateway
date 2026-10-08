package config

import (
	"testing"
	"time"
)

// ApplyTimezone 的行为测试（2026-10-09 修复「过了 0 点按天数据不切日」）。
//
// # 缺陷的形状
//
// SQLite 的 'localtime'（store.dayExpr 按天分桶的日界）跟随 Go 的
// time.Local。容器里没设 TZ、镜像里没有 /etc/localtime 时它是 UTC，
// 于是北京时间 0~8 点的调用被算进「昨天」。
//
// # 测什么
//
// 两个断言，各防一种失效：
//
//  1. 指定时区生效：改成东八区后，东八区「凌晨 1 点」的 UTC 时刻
//     （= 前一天 17:00 UTC）在本地日历里必须是**当天** —— 这正是
//     用户报的那条记录的形状（清晨调用被算进昨天）。
//
//  2. 空串不覆盖：留空时进程时区保持原样。裸机部署在东八区机器上的
//     用户什么都不用改，这个语义必须钉住 —— 否则「升级后时区被
//     悄悄重置」会造出新一轮的按天错位。
//
// ⚠️ time.Local 是包级全局：测试改写它会污染同包其它测试。所以每个
// 用例**先保存再还原**，并用 t.Cleanup 兜底 —— 中途 Fatal 也能还原。
func TestApplyTimezone_SetsLocal(t *testing.T) {
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })

	// 2006-01-02 20:04:05 UTC：
	//   - 东八区 → 2006-01-03 04:04:05（跨过午夜，日期进一天）
	//   - UTC    → 2006-01-02（缺陷口径下「清晨调用」被算进的那天）
	//   - 纽约   → 2006-01-02 15:04:05（同 UTC 日期）
	// 三个口径两个答案，能区分出「东八区生效」与「仍是 UTC」。
	// （Go 的 time.Date 不是常量表达式，只能运行期算。）
	tsUTC := time.Date(2006, 1, 2, 20, 4, 5, 0, time.UTC).Unix()

	got, err := ApplyTimezone("Asia/Shanghai")
	if err != nil {
		t.Fatalf("ApplyTimezone: %v", err)
	}
	if got != "Asia/Shanghai" {
		t.Fatalf("应用后应报告 Asia/Shanghai，实际 %q", got)
	}
	// 东八区的本地日历日必须是 01-03（tsUTC + 8h 已过午夜）。
	if d := time.Unix(tsUTC, 0).Format("2006-01-02"); d != "2006-01-03" {
		t.Fatalf("东八区下该时刻应为 2006-01-03，实际 %s —— 清晨调用仍被算进昨天", d)
	}

	// 换一个时区验证「不是硬编码 +8」：纽约下同一时刻是 01-02。
	if _, err := ApplyTimezone("America/New_York"); err != nil {
		t.Fatalf("ApplyTimezone(New_York): %v", err)
	}
	if d := time.Unix(tsUTC, 0).Format("2006-01-02"); d != "2006-01-02" {
		t.Fatalf("纽约下该时刻应为 2006-01-02，实际 %s", d)
	}
}

func TestApplyTimezone_EmptyKeepsExisting(t *testing.T) {
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })

	// 先设一个已知的非默认时区，再对空串调用 —— 不许动它。
	time.Local = time.FixedZone("probe-existing", 8*3600)
	got, err := ApplyTimezone("")
	if err != nil {
		t.Fatalf("ApplyTimezone(\"\"): %v", err)
	}
	if time.Local.String() != "probe-existing" {
		t.Fatalf("空串不得覆盖 time.Local：改成了 %q", time.Local.String())
	}
	if got != "probe-existing" {
		t.Fatalf("空串应报告现有时区名，实际 %q", got)
	}
}

func TestApplyTimezone_RejectsBadName(t *testing.T) {
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })

	if _, err := ApplyTimezone("Mars/Olympus_Mons"); err == nil {
		t.Fatal("拼错的时区名必须报错，静默落到 UTC 就是本次缺陷的形状")
	}
	// 失败时不得污染现有值。
	if time.Local != saved {
		t.Fatalf("失败路径改写了 time.Local（%v）", time.Local)
	}
}
