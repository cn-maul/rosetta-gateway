package config

import (
	"fmt"
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
// 改写 time.Local 一处生效：
//   - modernc.org/sqlite 的 'localtime' 实测跟随 Go 的 time.Local
//     （scripts 下的探针验证：TZ env 与运行期改写 time.Local 都能改变
//     'localtime' 的输出，后者正是本函数做的事）；
//   - Go 标准库里所有 time.Now().Format / time.Date 默认走 time.Local，
//     日志时间戳、有效期天数计算（Keys 的 daysLeft）随之统一；
//   - 不需要动任何 SQL。
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
	time.Local = loc
	return loc.String(), nil
}
