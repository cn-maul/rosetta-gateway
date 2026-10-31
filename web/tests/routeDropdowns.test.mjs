import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, join } from 'node:path'

// 路由编辑弹窗里的两个下拉：上游服务、上游模型。
//
// 为什么值得一条门禁：这两处曾经写成 `名称 (slug)` / `显示名 (model_id)`，
// 而 slug 未填时会被后端补成 "provider"、display_name 又常常只是 model_id
// 换个大小写 —— 于是下拉里每一项都把同一个信息排两遍，还多一对括号，选中
// 之后 SelectValue 显示的也是那串重复文本。用户要的是「上游名字」与「上游
// 模型实际名字」，一对一。

const here = dirname(fileURLToPath(import.meta.url))
const routesSrc = join(here, '..', 'src', 'pages', 'Routes.tsx')

const src = readFileSync(routesSrc, 'utf8')

/** 取出某个 aria-label 的 ListBox 内容（到下一个 `</ListBox>` 为止）。 */
function listBoxBody(ariaLabel) {
  const start = src.indexOf(`<ListBox aria-label="${ariaLabel}">`)
  assert.ok(start >= 0, `找不到「${ariaLabel}」下拉`)
  const end = src.indexOf('</ListBox>', start)
  assert.ok(end > start, `「${ariaLabel}」下拉没有闭合标签`)
  return src.slice(start, end)
}

test('编辑路由：上游服务下拉只写名字', () => {
  const body = listBoxBody('上游服务')
  assert.ok(body.includes('{p.name}'), '上游服务下拉应显示 p.name')
  assert.ok(!body.includes('p.slug'), '上游服务下拉不应再拼 slug（未填时会是 "provider"）')
  assert.ok(!/\(\{/.test(body), '上游服务下拉不应带括号补充说明')
})

test('编辑路由：上游模型下拉只写真实模型名', () => {
  const body = listBoxBody('上游模型')
  assert.ok(body.includes('{m.model_id}'), '上游模型下拉应显示 m.model_id')
  assert.ok(
    !body.includes('m.display_name'),
    '上游模型下拉不应再拼 display_name（多数时候与 model_id 只差大小写）',
  )
  assert.ok(!/\(\{/.test(body), '上游模型下拉不应带括号补充说明')
})
