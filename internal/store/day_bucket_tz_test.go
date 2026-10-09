package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// 按天分桶的日界测试（2026-10-09 修复「过了 0 点按天数据不切日」）。
//
// # 缺陷的形状
//
// dayExpr 用 SQLite 的 'localtime'；容器里没设 TZ 时它是 UTC，北京时间
// 0~8 点的调用被算进「昨天」（实测：用户的 909 条清晨调用全落进前一天的桶）。
//
// # 本文件为什么全部走子进程
//
// 第一版在本进程里改 time.Local，Windows 全绿、CI（ubuntu）变红 ——
// 因为 modernc SQLite 在 Linux 上走 C 的 tzset（读 TZ 并缓存），
// 不看 Go 的 time.Local。那个「本地全绿」是假信号。
//
// 所以现在每条用例都用 `go run` 起一个**带 TZ 的全新进程**去问真实的
// SQLite，父进程只断言结果。这样：
//   - 两个平台测的是同一条路径（TZ 环境变量），与生产一致；
//   - 不存在「本进程缓存状态影响结果」的不可控因素；
//   - 哪天换了 SQLite 绑定、'localtime' 又不跟 TZ 走了，这里立刻红。
//
// TestDayExprFollowsTZEnv 钉住「运行期改 TZ 环境变量 → SQLite 'localtime'
// 的日界跟着变」。
//
// # 为什么测 TZ 而不是 time.Local（第一版修复踩过的坑）
//
// 第一版只改了 time.Local，本测试当时也是那么写的，结果：
//
//	Windows 全绿（'localtime' 跟随 time.Local）
//	Linux 变红（'localtime' 不看 time.Local，走 C 的 tzset 读 TZ 并缓存）
//
// CI 在 ubuntu 上跑，直接把这个错误前提暴露了。所以现在改成测
// **TZ 环境变量**：那是两个平台都真的生效的路径，也是 config.ApplyTimezone
// 实际做的事（它同时设 TZ 与 time.Local，见那里的对照表）。
//
// # 为什么用子进程而不是在本进程里 os.Setenv
//
// 本进程内 Setenv 是否改变已缓存的 tzset 结果，取决于 SQLite 在此之前
// 有没有跑过日期函数 —— 同一个测试包里其它用例早就跑过了，缓存状态不可控。
// 只有**全新的进程**在第一次查询前就带着 TZ 启动，才是生产环境的真实形状
// （main 在最早期 ApplyTimezone，然后才开库）。
//
// 所以用 `go run` 起一个子探针，把结论带回来。
func TestDayExprFollowsTZEnv(t *testing.T) {
	// 东八区 2026-10-09 凌晨 1 点 = UTC 2026-10-08 17:00。
	//   东八区 → 2026-10-09     UTC → 2026-10-08
	cst := time.FixedZone("CST", 8*3600)
	ts := time.Date(2026, 10, 9, 1, 0, 0, 0, cst).UnixMilli()

	for _, tc := range []struct {
		tzEnv string
		want  string
	}{
		{"Asia/Shanghai", "2026-10-09"}, // 修复后应有的口径
		{"UTC", "2026-10-08"},           // 缺陷形状：清晨调用被算进昨天
		{"America/New_York", "2026-10-08"},
	} {
		out, err := runProbeWithTZ(tc.tzEnv, ts)
		if err != nil {
			t.Fatalf("子探针失败(TZ=%s): %v\n%s", tc.tzEnv, err, out)
		}
		got := strings.TrimSpace(out)
		if got != tc.want {
			t.Errorf("TZ=%s 时 dayExpr=%s，期望 %s —— 日界没跟着 TZ 走", tc.tzEnv, got, tc.want)
		}
	}
}

// runProbeWithTZ 起一个带指定 TZ 的子进程跑 dayExpr，返回它算出的日界。
//
// 用子进程而非本进程 Setenv，理由见 TestDayExprFollowsTZEnv 的注释：
// 只有全新进程才是生产的真实形状，也才能绕开 tzset 的缓存。
func runProbeWithTZ(tzEnv string, tsMs int64) (string, error) {
	cmd := exec.Command("go", "run", probePath, "-ts", strconv.FormatInt(tsMs, 10))
	cmd.Env = append(os.Environ(), "TZ="+tzEnv)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// probePath 指向仓库根的 scripts/probe_tz_child.go（本测试在 internal/store 下）。
var probePath = filepath.Join("..", "..", "scripts", "probe_tz_child.go")

// 归档写入与查询读取的 day 必须在**同一个时区口径**下工作 —— 即使
// 两次调用之间 time.Local 被改过（升级配置后重启前的窗口）也不会出现
// 「同一条记录落进两个桶」。
//
// 实现上这由「dayExpr 是常量、读 time.Local」天然保证；这条测试钉的是
// UsageSource 的归一化来源在东八区下把「今晨 1 点」归到「今天」——
// 即用户缺陷的端到端复现与修复验证。
// TestUsageSourceBucketsMorningCallIntoToday 端到端复现用户的缺陷场景：
// 跨过午夜的两条调用必须落在**同一天**，而不是一条算昨天、一条算今天。
//
// 也走子进程（理由同 TestDayExprFollowsTZEnv）：本进程里改 time.Local
// 在 Linux 上对 SQLite 无效，测出来的「通过」是假的 —— 第一版正是这么
// 在 Windows 全绿、CI 变红的。
//
// 子探针建自己的库、插两条记录、按天分组，把「有几组 + 各组的 day 和条数」
// 打回来，父进程断言只有一组。这样端到端覆盖了「真实的 SQLite + 真实的
// TZ 环境变量」，而不是某个中间变量被改了没改。
func TestUsageSourceBucketsMorningCallIntoToday(t *testing.T) {
	cmd := exec.Command("go", "run", probePath, "-e2e")
	cmd.Env = append(os.Environ(), "TZ=Asia/Shanghai")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("子探针失败: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	// 子探针在东八区下：两条（09 凌晨 1 点、09 上午 9 点）应同落一天。
	if got != "2026-10-09:2" {
		t.Fatalf("东八区下两条跨午夜的记录应同落 2026-10-09 一组(count=2)，实际 %q —— 清晨调用被切进了别的天", got)
	}
}

// UTC 对照：同一批记录在 UTC 口径下**必然**裂成两组。
//
// 这条不是「修好了」的证明，而是「测试本身有区分度」的证明 ——
// 没有它的话，上面那条用例即使因为子探针写错而恒返回某个固定值，
// 也可能碰巧与期望值相同。有这条对照，两个口径给出不同答案才成立。
func TestUsageSourceBucketsMorningCallSplitsUnderUTC(t *testing.T) {
	cmd := exec.Command("go", "run", probePath, "-e2e")
	cmd.Env = append(os.Environ(), "TZ=UTC")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("子探针失败: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	// UTC 下：01:00(东八区)=08 日 17:00 UTC → 10-08；09:00(东八区)=09 日 01:00 UTC → 10-09
	if got != "2026-10-08:1,2026-10-09:1" {
		t.Fatalf("UTC 口径下应裂成两组（正是缺陷形状），实际 %q —— 该用例失去区分度", got)
	}
}
