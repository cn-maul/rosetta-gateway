import test from 'node:test'
import assert from 'node:assert/strict'
import { oneOf, oneOfInt, asBool, readPref, writePref } from '../src/ui/usePersistedState.ts'

// 偏好持久化的契约：读不出来就用默认值，写不进去也不许抛。
// 这些用例不碰 React —— 只有 usePersistedState() 那一层需要组件环境，
// 而它内部就是 readPref/writePref 加一次 useState，逻辑全在这里测。

function fakeStorage(opts = {}) {
  const map = new Map()
  return {
    getItem: (k) => (map.has(k) ? map.get(k) : null),
    setItem: (k, v) => {
      if (opts.throwOnWrite) throw new Error('QuotaExceededError')
      map.set(k, String(v))
    },
    removeItem: (k) => map.delete(k),
    _map: map,
  }
}

function withWindow(storage, fn) {
  const prev = globalThis.window
  globalThis.window = { localStorage: storage }
  try {
    return fn()
  } finally {
    if (prev === undefined) delete globalThis.window
    else globalThis.window = prev
  }
}

test('没有 localStorage 时读偏好回落默认值，不抛', () => {
  assert.equal(readPref('overview.range', '1d', oneOf(['1d', '7d'])), '1d')
  assert.doesNotThrow(() => writePref('overview.range', '7d'))
})

test('写进去的值能原样读回，并带命名空间前缀', () => {
  const storage = fakeStorage()
  withWindow(storage, () => {
    writePref('models.days', 30)
    assert.equal(readPref('models.days', 7, oneOfInt([7, 14, 30])), 30)
    assert.ok(storage._map.has('rosetta.pref.models.days'), '键名必须带 rosetta.pref. 前缀，避免与其它站内数据撞名')
  })
})

test('旧版本写下的失效枚举值回落默认值，而不是塞给组件', () => {
  const storage = fakeStorage()
  withWindow(storage, () => {
    storage.setItem('rosetta.pref.usage.days', JSON.stringify(90))
    assert.equal(readPref('usage.days', 1, oneOfInt([1, 7, 30])), 1)
    storage.setItem('rosetta.pref.models.granularity', JSON.stringify('week'))
    assert.equal(readPref('models.granularity', 'hour', oneOf(['hour', 'day'])), 'hour')
  })
})

test('损坏的 JSON 与错误类型都回落默认值', () => {
  const storage = fakeStorage()
  withWindow(storage, () => {
    storage.setItem('rosetta.pref.history.size', '{not json')
    assert.equal(readPref('history.size', 20, oneOfInt([20, 50, 100])), 20)
    // 'true' 字符串不是布尔值 —— 旧格式脏值不能被当成 true
    storage.setItem('rosetta.pref.some.toggle', '"true"')
    assert.equal(readPref('some.toggle', false, asBool), false)
    // 数字类型的键读到字符串同样不接受
    storage.setItem('rosetta.pref.history.size', '"50"')
    assert.equal(readPref('history.size', 20, oneOfInt([20, 50, 100])), 20)
  })
})

test('写入失败（隐私模式 / 配额满）静默降级，不影响页面', () => {
  const storage = fakeStorage({ throwOnWrite: true })
  withWindow(storage, () => {
    assert.doesNotThrow(() => writePref('overview.range', '7d'))
    assert.equal(readPref('overview.range', '1d', oneOf(['1d', '7d'])), '1d')
  })
})
