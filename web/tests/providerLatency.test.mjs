import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'

// 2026-11-01 需求：供应商列表把「配置状态」那一列换成**探针测得的延迟**，
// 同样用胶囊包裹。
//
// 这条需求有三处容易在后续整理里退回去的地方，逐条钉住：
//   1) 列头文案必须真的换掉（留着「配置状态」就等于没做）；
//   2) 没有成功样本时必须显示「—」而不是 0 ms —— 「没测到」与「快到 0」是两件事；
//   3) 延迟必须来自后端探针字段（latency_ms / latency_samples），
//      不能前端自己拿 updated_at 之类的东西编一个数出来。

const read = (rel) => readFile(new URL('../src/' + rel, import.meta.url), 'utf8')

test('供应商列表：配置状态列换成延迟胶囊', async () => {
  const page = await read('pages/Providers.tsx')

  assert.ok(page.includes('<Table.Column>延迟</Table.Column>'), '列头没有改成「延迟」')
  assert.ok(
    !page.includes('<Table.Column>配置状态</Table.Column>'),
    '「配置状态」列又回来了 —— 用户明确要求把它换成延迟',
  )
  assert.ok(
    page.includes('<StatusChip tone={latencyTone(p)}>{latencyText(p)}</StatusChip>'),
    '延迟单元格不是胶囊形态（StatusChip），或没有用 latencyText/latencyTone',
  )
})

test('供应商列表：没有成功样本时显示「—」，不显示 0 ms', async () => {
  const page = await read('pages/Providers.tsx')

  assert.match(
    page,
    /function hasLatency\(p: Provider\): boolean \{\s*return \(p\.latency_samples \?\? 0\) > 0\s*\}/,
    '缺少 hasLatency：样本数为 0 时必须能识别出来',
  )
  assert.match(
    page,
    /if \(!hasLatency\(p\)\) return '—'/,
    'latencyText 在无样本时没有回落「—」—— 「从未探测」会被印成 0 ms',
  )
  // 后端字段必须是探针口径，不是前端算术。
  assert.ok(page.includes('p.latency_samples'), '没有使用后端的 latency_samples')
  assert.ok(page.includes('p.latency_ms'), '没有使用后端的 latency_ms')
})

test('供应商列表：ready=false 的故障原因仍然可见（没有被 tooltip 吞掉）', async () => {
  const page = await read('pages/Providers.tsx')

  // ready=false 时那一行红字必须还在 DOM 里：配置状态搬进 title 是信息降级，
  // 但「凭据没建起来」是故障，只藏在悬停提示里等于看不见。
  assert.match(
    page,
    /\{p\.ready === false \? \(/,
    'ready=false 的原因行被删了：它不能只活在 tooltip 里',
  )
  assert.ok(page.includes('ready_reason'), '原因行没有读 ready_reason')
})
