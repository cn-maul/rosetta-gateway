import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { heartbeatBucketTexts } from '../src/heartbeatText.ts'

function source(path) {
  return readFileSync(new URL(path, import.meta.url), 'utf8')
}

for (const [name, path] of [
  ['Upstream', '../src/views/Upstream.vue'],
  ['Settings', '../src/views/Settings.vue'],
  ['Wallet', '../src/views/Wallet.vue'],
]) {
  test(`R11 ${name} binds every Tabs instance to its panel`, () => {
    const s = source(path)
    assert.match(s, /<Tabs[^>]*\bref="tabsRef"/)
    assert.match(s, /panelIdFor\(tab\)/)
    assert.match(s, /tabIdFor\(tab\)/)
    assert.match(s, /role="tabpanel"/)
  })
}

test('R11 HeartbeatBar exposes focusable textual bucket details', () => {
  const s = source('../src/components/HeartbeatBar.vue')
  assert.match(s, /:tabindex="i === focused \? 0 : -1"/)
  assert.match(s, /@keydown="onBucketKey\(\$event, i\)"/)
  assert.match(s, /heartbeatBucketTexts\(visible\.value\.map\(\(v\) => v\.beat\), fmtTs\)/)
  assert.match(s, /role="list"/)
  assert.match(s, /role="listitem"/)
})

test('R11 heartbeat bucket summaries retain time, count, rate, and latency', () => {
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
