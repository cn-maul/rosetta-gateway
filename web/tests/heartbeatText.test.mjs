import test from 'node:test'
import assert from 'node:assert/strict'
import { heartbeatBucketTexts } from '../src/heartbeatText.ts'

// 原 accessibilityContract.test.mjs 的文案用例：心跳桶摘要必须保留
// 时间、成功率、次数与延迟（原 R11 契约，键盘可达性的文案部分）。
test('heartbeat bucket summaries retain time, count, rate, and latency', () => {
  const text = heartbeatBucketTexts(
    [
      { ts: 1_700_000_000_000, rate: 1, bucket: { ts: 1_700_000_000_000, ok: 3, count: 3, latency: 42 } },
      { ts: 1_700_003_600_000, rate: null, bucket: null },
    ],
    (ts) => (ts === 1_700_000_000_000 ? '01/01 00:00' : '01/01 01:00'),
  )
  assert.deepEqual(text, [
    '01/01 00:00 · 成功率 100% (3/3) · 平均 42ms',
    '01/01 01:00 · 没有探测记录',
  ])
})
