// 「思考强度」胶囊的源码门禁。
//
// 这条链路跨了四层（落库 → DTO → types.ts → 胶囊），任何一层忘了接，
// 症状都是**同一种**：请求日志里那枚胶囊永远不出现，看起来像「这次请求
// 确实没思考」，而不是「功能没接上」。类型系统只能保证字段在，保不住
// 展示层的判断与配色。
//
// 守三件事：
//  1) 舱位与模型表共用同一份强度色阶（两处颜色必须一致，否则同一个 high
//     在两个页面是两个颜色，读的人会以为不是同一个东西）；
//  2) 落库值为空串时不渲染胶囊（旧记录没有这一列，不能凭空显示一枚空胶囊）；
//  3) budget:8192 这种预算式思考要有专门文案，不能直接把 `budget:8192`
//     原样糊在界面上。

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { effortLabel, effortTone } from '../src/effortTone.ts'

const WEB = join(dirname(fileURLToPath(import.meta.url)), '..')
const read = (rel) => readFile(join(WEB, rel), 'utf8')

test('思考档位色阶在模型表与请求日志之间共用一份', async () => {
  const panel = await read('src/components/UpstreamModelPanel.tsx')
  assert.match(
    panel,
    /import \{ effortTone \} from '\.\.\/effortTone'/,
    '模型表必须从 effortTone 取色阶，不能自带一份映射',
  )
  assert.ok(
    !/const EFFORT_TONE/.test(panel),
    '模型表里不该再有一份 EFFORT_TONE —— 两处各写一份必然漂移',
  )
})

test('落库值为空串时不渲染胶囊', async () => {
  const page = await read('src/pages/UsageDetail.tsx')
  // 模式列里那枚胶囊必须挂在 h.effort 的真值判断上。
  assert.match(
    page,
    /\{h\.effort \? \(/,
    '模式列的思考胶囊必须以 h.effort 为真值条件，空串（旧记录）不显示',
  )
  assert.match(page, /tone=\{effortTone\(h\.effort\)\}/, '胶囊配色必须走 effortTone')
})

test('预算式思考翻成「预算 N」而不是原样糊 budget:8192', () => {
  assert.equal(effortLabel('budget:8192'), '预算 8192')
  assert.equal(effortLabel('  budget:500  '), '预算 500')
})

test('已知档位原样显示，未知档位回落灰而不是假装是强档位', () => {
  assert.equal(effortLabel('xhigh'), 'xhigh')
  assert.equal(effortLabel(''), '')
  // 强度色阶：弱档灰/蓝，中档绿，强档琥珀/红。
  assert.equal(effortTone('none'), 'dim')
  assert.equal(effortTone('low'), 'info')
  assert.equal(effortTone('medium'), 'ok')
  assert.equal(effortTone('high'), 'warn')
  assert.equal(effortTone('max'), 'err')
  // 厂商私有挡位：能看见名字，但不冒充已知强度。
  assert.equal(effortTone('turbo'), 'dim')
})
