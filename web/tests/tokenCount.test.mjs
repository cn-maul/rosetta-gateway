import test from 'node:test'
import assert from 'node:assert/strict'
import { parseTokenCount, tokenCountHint } from '../src/tokenCount.ts'

// 2026-11-01：上下文窗口与最大输出改用文本输入 + 这里解析。
// 之所以要有测试：这两个数写错不会报错，只会让模型按错的窗口工作，
// 而且错误方向是「静默变成 0 = 回落默认值」，最难被发现。

test('parses plain integers', () => {
  assert.equal(parseTokenCount('131072'), 131072)
  assert.equal(parseTokenCount('0'), 0)
  assert.equal(parseTokenCount('8192'), 8192)
})

test('parses K and M suffixes as binary multiples', () => {
  // 128K 必须等于 131072 而不是 128000 —— 厂商发布的上下文窗口就是前者。
  assert.equal(parseTokenCount('128K'), 131072)
  assert.equal(parseTokenCount('128k'), 131072)
  assert.equal(parseTokenCount('1M'), 1048576)
  assert.equal(parseTokenCount('1m'), 1048576)
  assert.equal(parseTokenCount('512K'), 524288)
})

test('parses decimals in suffixed values', () => {
  assert.equal(parseTokenCount('1.5M'), 1572864)
  assert.equal(parseTokenCount('0.5K'), 512)
})

test('tolerates grouping separators', () => {
  assert.equal(parseTokenCount('131 072'), 131072)
  assert.equal(parseTokenCount('131,072'), 131072)
  assert.equal(parseTokenCount('131_072'), 131072)
})

test('rejects nonsense instead of silently returning 0', () => {
  // 关键：无法解析必须回 null，不能回 0。0 的语义是「未设置，回落默认值」，
  // 把打错的字解释成 0 会让错误彻底沉底。
  for (const bad of ['', '   ', 'abc', 'K', '12X', '-1', '1.2.3', '128KK', '1e6']) {
    assert.equal(parseTokenCount(bad), null, `应当拒绝 ${JSON.stringify(bad)}`)
  }
})

test('hint only appears for suffixed input', () => {
  // 纯数字不需要回显 —— 它本身已经是要存的值，再显示一遍是噪音。
  assert.equal(tokenCountHint('131072'), '')
  assert.equal(tokenCountHint('131 072'), '= 131,072 tokens')
  assert.equal(tokenCountHint('128K'), '= 131,072 tokens')
  assert.equal(tokenCountHint('1M'), '= 1,048,576 tokens')
  assert.equal(tokenCountHint('0'), '')
  assert.equal(tokenCountHint('abc'), '')
})
