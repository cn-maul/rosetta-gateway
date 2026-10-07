// 时间区间口径（总览 / 调用历史共用一份）
//
// 这里存在的唯一理由：之前「近 N 天」是用 `now - N * 86400_000` 算的，也就是
// **滚动窗口** —— 每刷新一次页面，起点就往后挪几分钟。用户说「最近 1 天」时想的是
// 「今天」，看到的却是「昨天下午三点到现在」，于是「今天早上的调用」明明发生了却
// 不在数字里；而凌晨刚过 0 点时，「近 1 天」里几乎还没有任何数据，看上去像「今天
// 没有流量」。
//
// 口径（与界面上的档位一一对应）：
//   - days = 0（「全部」）：from = 0，沿用「不限起点」的老语义；
//   - days = 1（「今天」）：本地当日 0 点 → now；
//   - days = N（「近 N 天」）：今天往前数 N-1 天的那个 0 点 → now，即**今天 +
//     前 N-1 天，共 N 个自然日**。所以「近 7 天」永远是 7 根日柱，不是「7×24
//     小时」横跨 8 个日期。
//
// 为什么日界必须用**本地时区**构造（new Date(y, m, d)），不能用 toISOString /
// UTC：
//   1. 用户心里的「今天」是本地日历日。UTC 切日在 UTC+8 是早上 8 点 —— 于是
//      「今天」从 8 点才开始算，而早上 0~8 点的调用被算到「昨天」，与按日聚合的
//      归档口径相反。
//   2. 更硬的一条：后端按天分桶用的就是本地时区（store.dayExpr =
//      strftime('%Y-%m-%d', ts/1000, 'unixepoch', 'localtime')，见
//      internal/store/usage_archive.go）。前端一旦按 UTC 切日，柱状图的「一格」
//      和后端统计的「一天」就不是同一天 —— 数字与图对不上，而且越靠近 8 点
//      偏得越离谱。口径必须与后端**逐字一致**。
//
// 另外，跨夏令时的地区「某个自然日的 0 点」在钟表上可能并不存在（会前跳到 1 点）。
// new Date(y, m, d) 会把它归一化到那个自然日里最早的那一刻，仍然是「这一天的开始」；
// 手工加减 24 小时则可能落进前一天的尾巴，那才是真的错。

/** 一天的毫秒数。仅用于「跨月的同一天」这类与日界无关的场合；日界一律走 localMidnight。 */
const DAY_MS = 86400_000

/**
 * 今天往前数 `daysBack` 天的那个自然日的**本地 0 点**。
 *
 * daysBack = 0 → 今天 0 点；= 1 → 昨天 0 点；跨月/跨年由 Date 构造器自行归一化
 * （month 可以是负数或大于 11），不必自己处理进位。
 *
 * 注意不能用 `now - daysBack * DAY_MS` 代替：那是按固定时长回推，跨夏令时会
 * 落到前一天晚上（或跳到后一天早上）—— 而这里要的是「某个自然日的开始」。
 */
export function localMidnight(daysBack = 0, now: number = Date.now()): Date {
  const d = new Date(now)
  return new Date(d.getFullYear(), d.getMonth(), d.getDate() - daysBack)
}

/**
 * 「近 N 天」档的起点（毫秒时间戳）。
 *
 * days <= 0 → 0，即「全部历史」：后端把 from=0 解释为不限起点，这个语义不变。
 */
export function rangeStart(days: number, now: number = Date.now()): number {
  if (days <= 0) return 0
  return localMidnight(days - 1, now).getTime()
}

/**
 * 区间 = [from, to)，毫秒时间戳，直接对应后端的 from/to 查询参数。
 *
 * to 取「现在」而不是「今天 23:59:59」：往未来要数据没有意义，而从后端看
 * to 只是个上界，早一点收口不会漏数据。
 */
export function rangeBounds(days: number, now: number = Date.now()): { from: number; to: number } {
  return { from: rangeStart(days, now), to: now }
}

/**
 * 毫秒时间戳 → 本地日历日键 YYYY-MM-DD。
 *
 * 与后端 dayExpr（localtime）同一口径，专供前端的日粒度分桶/补零使用：
 * 柱状图里每根柱子必须落在同一个「自然日」上，否则一根柱子会横跨后端的两天，
 * 数字与图就对不上。
 */
export function localDayKey(ms: number): string {
  const d = new Date(ms)
  const p = (x: number) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}
