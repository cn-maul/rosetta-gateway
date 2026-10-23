import test from 'node:test'
import assert from 'node:assert/strict'
import { parseRouteTrace, parseTraceStep } from '../src/routeTrace.ts'
import { unitPrice, cacheUnitPrice, modelPriceLine } from '../src/modelPrice.ts'

test('保留模型中的路径、冒号与箭头，保留未知原因原文', () => {
  const step = parseTraceStep('2:backup/org/model:latest -> alias -> fail_switch:vendor:extra')
  assert.equal(step.attempt, 2)
  assert.equal(step.provider, 'backup')
  assert.equal(step.model, 'org/model:latest -> alias')
  assert.equal(step.reasonLabel, 'vendor:extra')
})

test('转义后的上游名、模型名与分号都能原样还原', () => {
  const raw = String.raw`1:org\/team\;\name\/prod/org/model\;v2 -> fail_switch:upstream_error; 2:肥鱼中转\/华东/gpt-4o -> ok`
  const steps = parseRouteTrace(raw)
  assert.equal(steps.length, 2)
  assert.equal(steps[0].provider, 'org/team;\\name/prod')
  assert.equal(steps[0].model, 'org/model;v2')
  assert.equal(steps[1].provider, '肥鱼中转/华东')
  assert.equal(steps[1].model, 'gpt-4o')
  assert.equal(steps[1].outcomeLabel, '完成响应')
})

test('允许转移不表示已执行后备；跳过与调用失败分别解释', () => {
  const steps = parseRouteTrace('1:primary/model -> skip:probe_busy_or_cooling; 2:backup/model -> fail_switch:upstream_timeout')
  assert.equal(steps.length, 2)
  assert.match(steps[0].explanation, /未进入上游调用/)
  assert.match(steps[1].outcomeLabel, /允许转移/)
  assert.match(steps[1].explanation, /后备目标和预算/)
  assert.match(parseTraceStep('1:a/b -> fail:no_healthy_credential').explanation, /未向此目标发起调用/)
})

test('成功和流开始后失败不会被解释为允许切换', () => {
  assert.equal(parseTraceStep('2:a/b -> ok').outcomeLabel, '完成响应')
  assert.match(parseTraceStep('1:a/b -> committed_fail:upstream_error').explanation, /无法再切换/)
  assert.match(parseTraceStep('1:a/b -> fail_terminal:invalid_request_error').explanation, /不再选择后备/)
})

test('历史、损坏与未知记录保留原文，不猜测', () => {
  for (const raw of ['legacy', '0:a/b -> ok', '-1:a/b -> ok', '9007199254740992:a/b -> ok', '1:a/ -> ok', '1:a/b -> ']) {
    const step = parseTraceStep(raw)
    assert.equal(step.parsed, false)
    assert.equal(step.raw, raw.trim())
  }
  assert.match(parseTraceStep('1:a/b -> future_result:future_reason').outcomeLabel, /未知结果/)
  assert.equal(parseTraceStep('1:a/b -> constructor:toString').reasonLabel, 'toString')
  for (const empty of [null, undefined, '', ' ; ; ']) assert.deepEqual(parseRouteTrace(empty), [])
})

test('参考单价保留有效数字，并标明缓存沿用与未配置', () => {
  assert.equal(unitPrice(0.000001), '¥0.000001')
  assert.equal(unitPrice(1.23456), '¥1.23456')
  assert.equal(unitPrice(0.0000123456789), '¥0.0000123456789')
  assert.equal(unitPrice(0), '未配置')
  assert.equal(cacheUnitPrice(0.0000123456789, 0), '¥0.0000123456789（沿用输入）')
  assert.equal(cacheUnitPrice(2, 0.1), '¥0.1')
  assert.equal(cacheUnitPrice(0, 0), '未配置')
  assert.match(modelPriceLine({ price_input: 2, price_cache_hit: 0, price_output: 0 }), /缓存 ¥2（沿用输入） · 输出 未配置/)
})
