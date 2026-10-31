import type { ProbeTimelineBucket } from './types'
import type { HeartbeatBucket } from './heartbeatText'

/**
 * 探测桶的**本地时区**对齐（2026-10-31 修，后端接口审计 #4）。
 *
 * 与后端 `store.probeBucket` 必须同一套口径：那边用
 * `strftime(..., 'unixepoch', 'localtime')` 分组、再把桶键 ParseInLocation
 * 还原成本地起点毫秒；这边就用 Date 的本地字段做同一件事。
 *
 * 为什么必须对齐：早先两端都是 `Math.floor(now / step) * step`，即 UTC 整倍数
 * 对齐。小时桶在东八区恰好等于本地整点（偏移是整小时），**天桶却差 8 小时**
 * —— 桶起点落在本地 08:00，与「按天用量」的本地日界（localDayKey）差一天，
 * 同一天在两张图上落到不同的柱子。
 */

/** 把 ts 归到本地桶起点：小时桶＝本地整点，天桶＝本地午夜。 */
export function bucketStart(ms: number, bucketMinutes: number): number {
  const d = new Date(ms)
  const minuteOfDay = d.getHours() * 60 + d.getMinutes()
  const aligned = Math.floor(minuteOfDay / bucketMinutes) * bucketMinutes
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime() + aligned * 60000
}

/**
 * 从本地桶起点往前推 k 个桶宽。走**本地日历**字段（new Date(y,m,d,...)），
 * 而不是直接减毫秒：夏令时那天减毫秒会得到 23:00 或 01:00 这种不在桶边界
 * 上的值，本地日历运算由 Date 自己归一化。
 */
export function shiftBucket(base: Date, back: number, bucketMinutes: number): number {
  if (bucketMinutes >= 1440) {
    return new Date(base.getFullYear(), base.getMonth(), base.getDate() - back * (bucketMinutes / 1440)).getTime()
  }
  return new Date(
    base.getFullYear(),
    base.getMonth(),
    base.getDate(),
    base.getHours(),
    base.getMinutes() - back * bucketMinutes,
  ).getTime()
}

/**
 * 把稀疏桶铺成等距 beat 序列：从最后一个桶（含此刻的那个本地桶）向前铺
 * total=hours/bucketMinutes 根；桶按本地桶起点对齐，缺失补
 * { rate:null, bucket:null }。「没有探测记录」与「成功率 0」必须分开。
 */
export function buildBeats(
  buckets: readonly ProbeTimelineBucket[],
  hours: number,
  bucketMinutes: number,
): HeartbeatBucket[] {
  const step = bucketMinutes * 60000
  const total = Math.max(1, Math.round((hours * 3600000) / step))
  const end = new Date(bucketStart(Date.now(), bucketMinutes))
  const byTs = new Map<number, ProbeTimelineBucket>()
  for (const b of buckets) byTs.set(bucketStart(b.ts, bucketMinutes), b)
  const beats: HeartbeatBucket[] = []
  for (let i = total - 1; i >= 0; i--) {
    const ts = shiftBucket(end, i, bucketMinutes)
    const bucket = byTs.get(ts) ?? null
    beats.push({ ts, rate: bucket && bucket.count > 0 ? bucket.ok / bucket.count : null, bucket })
  }
  return beats
}
