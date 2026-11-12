import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'

// 2026-11-01 需求：代理页去掉「测试」按钮，改成后台定期自动测速。
//
// 这条需求有三个容易退回去的地方，逐条钉住：
//   1) 「测试」按钮与它的临时状态不能回来 —— 手动测试测的是「刚点那一下」，
//      而代理坏掉通常发生在没人点按钮的时候；
//   2) 延迟必须来自后端探针字段（latency_ms / latency_samples / last_probe_*），
//      不能前端编一个数或用 updated_at 之类顶替；
//   3) 「测不通」与「没测过」必须能区分 —— 都画成灰点等于把故障藏起来。

const read = (rel) => readFile(new URL('../src/' + rel, import.meta.url), 'utf8')

test('代理列表：不再有「测试」按钮，也不再依赖手动测试接口', async () => {
  const page = await read('pages/Networks.tsx')

  assert.ok(
    !page.includes('api.testNetwork'),
    '页面还在调 api.testNetwork —— 探测已改成后台定时，前端不该有手动触发入口',
  )
  assert.ok(!page.includes('NetworkTestResult'), '页面还依赖 NetworkTestResult：那是手动测试的返回类型')
  assert.ok(
    !/<Button[^>]*>\s*测试\s*<\/Button>/.test(page),
    '「测试」按钮又回来了',
  )

  // 手动测试端点已从后端删除，前端封装与类型也不该继续存在。
  const api = await read('api.ts')
  assert.ok(!api.includes('testNetwork'), 'api.testNetwork 应随后端端点一起删除')
  const types = await read('types.ts')
  assert.ok(!types.includes('NetworkTestResult'), 'types.ts 不应再保留手动测试的返回类型')
})

test('代理列表：延迟来自探针字段，无样本显示「—」', async () => {
  const page = await read('pages/Networks.tsx')

  assert.ok(page.includes('<Table.Column>延迟</Table.Column>'), '列头没有改成「延迟」')
  assert.ok(page.includes('<StatusChip tone={latencyTone(n)}>{latencyText(n)}</StatusChip>'), '延迟单元格不是胶囊形态')
  assert.match(
    page,
    /function hasLatency\(n: Network\): boolean \{\s*return \(n\.latency_samples \?\? 0\) > 0\s*\}/,
    '缺少 hasLatency：样本数为 0 时必须能识别出来',
  )
  assert.match(page, /if \(!hasLatency\(n\)\) return '—'/, '没有成功样本时必须显示「—」，不能显示 0 ms')
  assert.ok(page.includes('n.latency_ms'), '延迟不是来自后端的 latency_ms')
})

test('代理列表：最近一次失败要显红并露出原因，「没测过」保持灰', async () => {
  const page = await read('pages/Networks.tsx')

  assert.match(
    page,
    /if \(!hasLatency\(n\)\) return n\.last_probe_at && n\.last_probe_ok === false \? 'err' : 'dim'/,
    '失败与没测过必须区分：一直失败的代理不能画成「未探测」的灰点',
  )
  assert.match(page, /function probeErrorLine\(n: Network\): string \{/, '缺少失败原因行：原因只藏在 tooltip 里等于看不见')
  assert.ok(page.includes('{probeError ? ('), '失败原因没有渲染出来')
})
