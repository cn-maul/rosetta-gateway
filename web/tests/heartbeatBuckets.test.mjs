import test from 'node:test'
import assert from 'node:assert/strict'
import { bucketStart, buildBeats, shiftBucket } from '../src/heartbeatBuckets.ts'

// 2026-10-31 修（后端接口审计 #4）：探测时间轴的桶必须按**本地时区**对齐，
// 与后端 store.probeBucket、localDayKey 同一口径。下面几条都不假设具体时区：
// 它们只用 Date 的本地字段构造基准点，所以在 UTC 与东八区都成立；但都能
// 挑出「按 UTC 整倍数切」的旧实现（天桶旧版对齐到本地 08:00）。

function localMidnight(d) {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()
}

test('day buckets align to local midnight, not UTC midnight', () => {
  const base = new Date(2026, 0, 15) // 本地 2026-01-15 00:00
  // 同一天里的两个时刻：本地 00:30 与 23:30，落入同一个天桶。
  const early = new Date(base.getFullYear(), base.getMonth(), base.getDate(), 0, 30).getTime()
  const late = new Date(base.getFullYear(), base.getMonth(), base.getDate(), 23, 30).getTime()
  assert.equal(bucketStart(early, 1440), base.getTime())
  assert.equal(bucketStart(late, 1440), base.getTime())
})

test('hour buckets align to local hour', () => {
  const base = new Date(2026, 0, 15, 9, 0, 0, 0) // 本地 09:00
  const inside = base.getTime() + 30 * 60000
  assert.equal(bucketStart(inside, 60), base.getTime())
  // 相邻本地整点各自成桶
  assert.notEqual(bucketStart(base.getTime() - 1, 60), base.getTime())
})

test('shiftBucket walks local calendar days across month boundary', () => {
  const feb1 = new Date(2026, 1, 1)
  assert.equal(shiftBucket(feb1, 1, 1440), new Date(2026, 0, 31).getTime())
  assert.equal(shiftBucket(feb1, 2, 1440), new Date(2026, 0, 30).getTime())
})

test('buildBeats lays out day beats ending at the current local day', () => {
  const beats = buildBeats([], 3 * 24, 1440) // 3 天，天粒度
  assert.equal(beats.length, 3)
  const today = new Date()
  assert.equal(beats[beats.length - 1].ts, localMidnight(today))
  for (const b of beats) assert.equal(b.rate, null) // 没有探测记录 ≠ 成功率 0
  // 单调递增且间隔正好一天（本地日历日）
  for (let i = 1; i < beats.length; i++) {
    assert.equal(shiftBucket(new Date(beats[i].ts), 1, 1440), beats[i - 1].ts)
  }
})

test('buildBeats puts a bucket onto the beat whose local start matches', () => {
  const today = new Date()
  const todayStart = localMidnight(today)
  const bucket = { model_id: 'm', ts: todayStart + 5 * 3600000, count: 4, ok: 3, latency: 120 }
  const beats = buildBeats([bucket], 48, 1440)
  const hit = beats.filter((b) => b.bucket === bucket)
  assert.equal(hit.length, 1)
  assert.equal(hit[0].ts, todayStart)
  assert.equal(hit[0].rate, 0.75)
})

test('buildBeats with hour granularity keeps one beat per local hour', () => {
  const beats = buildBeats([], 24, 60)
  assert.equal(beats.length, 24)
  for (const b of beats) {
    const d = new Date(b.ts)
    assert.equal(d.getMinutes(), 0)
    assert.equal(d.getSeconds(), 0)
  }
  for (let i = 1; i < beats.length; i++) assert.equal(beats[i].ts - beats[i - 1].ts, 3600000)
})
