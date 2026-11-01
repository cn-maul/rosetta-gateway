// 用量趋势图的序列构建（总览页专用）
//
// 后端 /usage/by-day 只返回**有记录**的那几天（稀疏）。直接拿去画柱状图，
// 横轴会被压缩：连休三天没调用，图上三天就被挤成一根，看上去像「流量集中」；
// 而真正的塌陷（某天 0 调用）根本画不出来 —— 那恰恰是要看的东西。
// 所以这里先按本地日历日补齐成连续序列，再把宽区间按周/月合并。
//
// 口径全部走 range.ts：柱子起点与指标卡的 from 必须是同一个函数给的
// （rangeStart / localMidnight / localDayKey）。两处各写一份就会漂移 ——
// 曾经出现过「数字是今天开始算、图上却带昨天一截」，读图与读数字结论相反。
import * as timeRange from './range'

/** 图上一根柱子。 */
export interface UsageBucket {
  /** v-for key：取桶内首日，天然唯一。 */
  key: string
  /** 桶内请求数合计。 */
  count: number
  /** 桶内 token 合计。 */
  tokens: number
  /** 宽屏标签：按日 `09-30` / 按月 `2026-03`（跨月时 `2026-03~04`）。 */
  labFull: string
  /** 窄标签（桶多时改用）：按日 `30` / 按月 `3月`。 */
  labShort: string
  /** 悬停提示里的日期部分（真实覆盖范围，部分桶会注明只有几天）。 */
  tip: string
}

/** 有记录的日聚合行（/usage/by-day 的 records 元素形状的子集）。 */
export interface UsageDayRow {
  key: string
  count: number
  tokens: number
}

/**
 * 桶宽（天）：≤31 天按日；更长的档按周；「全部」按月。
 *
 * 为什么不按像素自适应：横轴读的是「时间推进」，桶宽必须由区间决定 ——
 * 同一个 30 天区间在 27 寸屏和笔记本上应该显示同样多根柱子，否则两个人的
 * 「近 1 月用量」图形状不同，讨论起来各说各话。
 */
export function bucketDaysFor(days: number): number {
  if (days === 0) return 30
  return days > 31 ? 7 : 1
}

/** 图标题旁的聚合说明。 */
export function granularityLabel(days: number): string {
  const bucket = bucketDaysFor(days)
  if (bucket === 1) return '按日聚合'
  return bucket >= 30 ? '按月聚合' : '按周聚合'
}

export function buildUsageSeries({
  byDay,
  days,
  firstRecordAt,
  now = Date.now(),
}: {
  byDay: UsageDayRow[]
  days: number
  /** 全局最早一条用量的毫秒时间戳（0 = 未知）。只有「全部」档用得到。 */
  firstRecordAt: number
  now?: number
}): UsageBucket[] {
  const map = new Map(byDay.map((e) => [e.key, e]))

  // 「全部」档的起点：明细被剪枝之后 byDay 只剩最近 30 天，用它当年限会把
  // 更早的历史整段丢掉（图凭空变短）。所以优先用终身累计的 first_record_at，
  // 拿不到再退回 byDay 最早一天，最后兜底 30 天前。
  let start: number
  if (days === 0) {
    if (firstRecordAt > 0) {
      start = timeRange.localMidnight(0, firstRecordAt).getTime()
    } else {
      const earliest = byDay.map((e) => e.key).sort()[0]
      start = earliest
        ? new Date(earliest + 'T00:00:00').getTime()
        : timeRange.localMidnight(30, now).getTime()
    }
  } else {
    start = timeRange.rangeStart(days, now)
  }

  // 逐日补齐。日期键必须与后端 dayExpr（localtime）逐字一致：对不上时 map
  // 查不到，每根柱子都被补成 0 —— 一张全零图比报错更难查。
  const daily: { date: string; count: number; tokens: number }[] = []
  const cursor = new Date(timeRange.localMidnight(0, start).getTime())
  const end = timeRange.localMidnight(0, now)
  while (cursor.getTime() <= end.getTime()) {
    const date = timeRange.localDayKey(cursor.getTime())
    const hit = map.get(date)
    daily.push({ date, count: hit?.count ?? 0, tokens: hit?.tokens ?? 0 })
    cursor.setDate(cursor.getDate() + 1)
  }

  const bucketDays = bucketDaysFor(days)
  if (bucketDays === 1) {
    return daily.map((d) => ({
      key: d.date,
      count: d.count,
      tokens: d.tokens,
      labFull: d.date.slice(5), // 09-30
      labShort: d.date.slice(8), // 30
      tip: d.date.slice(5),
    }))
  }

  const monthLabel = (dateStr: string) => `${parseInt(dateStr.slice(5, 7), 10)}月`
  const monthly = bucketDays >= 30
  const out: UsageBucket[] = []
  for (let i = 0; i < daily.length; i += bucketDays) {
    const slice = daily.slice(i, i + bucketDays)
    const first = slice[0]
    const last = slice[slice.length - 1]
    // 尾桶常常不足一整桶宽（「近 1 年」末尾只剩几天）：标签打 `*`、提示写明
    // 实际天数。不标的话，一个只有 2 天的桶会被读成「流量腰斩」。也刻意**不**
    // 按桶宽归一到满桶高度 —— 那会把 2 天的量放大成一整桶的量，语义就变了。
    const partial = slice.length < bucketDays
    const mark = partial ? '*' : ''
    const partialNote = partial ? `（部分桶：仅 ${slice.length} 天）` : ''
    // 月桶跨月时标签要补止月：只写首月「2026-03」会被读成整桶都在 3 月。
    const sameMonth = first.date.slice(0, 7) === last.date.slice(0, 7)
    let labFullMonth = first.date.slice(0, 7)
    if (!sameMonth) {
      labFullMonth +=
        first.date.slice(0, 4) === last.date.slice(0, 4)
          ? `~${last.date.slice(5, 7)}`
          : `~${last.date.slice(0, 7)}`
    }
    const m1 = parseInt(first.date.slice(5, 7), 10)
    const m2 = parseInt(last.date.slice(5, 7), 10)
    out.push({
      key: first.date,
      count: slice.reduce((a, b) => a + b.count, 0),
      tokens: slice.reduce((a, b) => a + b.tokens, 0),
      labFull: (monthly ? labFullMonth : first.date.slice(5)) + mark,
      labShort: (monthly ? (sameMonth ? monthLabel(first.date) : `${m1}~${m2}月`) : monthLabel(first.date)) + mark,
      // tip 给真实日期范围：月桶同样按范围写，悬停即可确认覆盖区间。
      tip: `${first.date.slice(5)} ~ ${last.date.slice(5)}${partialNote}`,
    })
  }
  return out
}

/**
 * 横轴标签抽稀步长：最多显示约 12 个标签。
 *
 * 写死 `i % 3` 在「近 1 年 / 全部」这种几十上百根柱子时会挤成一团竖排碎字。
 */
export function labelStrideFor(count: number): number {
  return Math.max(1, Math.ceil(count / 12))
}
