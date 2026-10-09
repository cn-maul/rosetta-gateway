package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// ApplyTimezone 把进程本地时区设置成配置里指定的那个。
//
// # 为什么是「改写 time.Local」而不是在 SQL 里显式拼偏移
//
// 按天分桶的日界表达式是 store.dayExpr：
//
//	strftime('%Y-%m-%d', ts/1000, 'unixepoch', 'localtime')
//
// 它出现在两类地方（归档写入、查询读取），必须逐字一致，否则边界那天的
// 记录会落进两个桶。把 'localtime' 换成显式偏移（如 '+8 hours'）需要
// 在每一处拼进同一个偏移量 —— 而偏移量对夏令时地区不是常数（一年变两次），
// 拼常数会把夏令时期间的所有天界切错一小时。
//
// # 必须同时设 TZ 环境变量与 time.Local —— 只设一个在另一个平台上失效
//
// 这是第一版修复踩过的坑（CI 在 ubuntu 上直接红，而本地 Windows 全绿）：
//
//	┌──────────┬────────────────────┬────────────────────────┐
//	│          │ 只改 time.Local    │ 运行期 os.Setenv("TZ") │
//	├──────────┼────────────────────┼────────────────────────┤
//	│ Windows  │ 'localtime' 跟随 ✓ │ 'localtime' 跟随 ✓     │
//	│ Linux    │ 'localtime' 不跟随 ✗ │ 'localtime' 跟随 ✓   │
//	└──────────┴────────────────────┴────────────────────────┘
//
// 原因：modernc.org/sqlite 在 Linux 上走 C 的 tzset —— 它读 TZ 环境变量
// 与 /etc/localtime 并缓存结果，**不看 Go 的 time.Local**。Windows 上
// 没有那套 C 运行时语义，才表现为跟随 time.Local。
//
// 所以本函数两个都设：
//   - os.Setenv("TZ", ...)  → 决定 SQLite 'localtime'（两个平台都有效）
//   - time.Local = loc      → 决定日志时间戳等 Go 侧输出
//     （os.Setenv 不会改变已初始化的 time.Local，它只在包初始化时读一次）
//
// 顺序上 Setenv 必须**早于**任何 SQLite 查询：tzset 的结果会被缓存，
// 晚设的生效时机不可控。
//
// # 时区在什么时候被「读走」
//
// time.Local 是包级变量，读取它的代码拿到的是**当时的值**。所以本函数
// 必须在 main 的最早期调用 —— 早于 store 打开（归档水位用本地日）、
// 早于任何 goroutine 启动。晚于此的话，已经算出去的「今天」就是旧口径。
//
// # 空串的语义
//
// 沿用进程现有时区，不覆盖。裸机部署在东八区机器上的用户、以及已经用
// TZ 环境变量配好的容器，都不需要改配置。Docker 镜像的 entrypoint
// 默认设 TZ=Asia/Shanghai，也可被 -e TZ=... 覆盖。
//
// 返回应用的时区名，供启动日志打印「按天统计的日界时区」—— 这个值
// 影响所有按天数字，必须让运维一眼能查到当前生效的是什么。
func ApplyTimezone(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		// 不覆盖，但要报告实际生效的时区：TZ 未设且容器无 /etc/localtime
		// 时这里是 UTC —— 正是本次缺陷的形状。打印出来，运维看到
		// 「timezone=UTC」而用户都在东八区，就知道该配了。
		return time.Now().Location().String(), nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return "", fmt.Errorf("load timezone %q: %w", name, err)
	}

	// 两处都要设，且顺序不能反。
	//
	// 1) os.Setenv("TZ", ...)：SQLite 侧。
	//    modernc.org/sqlite 在 Linux 上走 C 的 tzset —— 它读 TZ 环境变量
	//    与 /etc/localtime，**不看 Go 的 time.Local**。
	//    实测（本修复的第一版只改了 time.Local，CI 在 ubuntu 上直接红）：
	//      - Windows：改 time.Local → 'localtime' 跟随
	//      - Linux  ：改 time.Local → 'localtime' 不跟随（仍是 TZ 决定）
	//    所以这一行才是 Linux 上真正生效的那个动作，缺了它 dayExpr 的日界
	//    不会变。必须在打开数据库（store.Open）之前调用：tzset 的结果会被
	//    缓存，晚设的生效时机不可控。
	//
	// 2) time.Local = loc：Go 侧。
	//    日志时间戳、有效期天数等一切走 Go time 包默认位置的输出。
	//    os.Setenv 不会改变已经初始化的 time.Local（它只在包初始化时读
	//    一次），所以这行也必须显式写。
	if err := os.Setenv("TZ", name); err != nil {
		return "", fmt.Errorf("set TZ env %q: %w", name, err)
	}
	time.Local = loc
	return loc.String(), nil
}
