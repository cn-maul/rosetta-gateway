import test from 'node:test'
import assert from 'node:assert/strict'
import { readdir, readFile } from 'node:fs/promises'
import { join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

// HeroUI 的 Table 是「必须成套用」的复合组件：`Table.Content` / `Table.Column`
// / `Table.Cell` 都通过 TableContext 取插槽类名（.table__content / .table__column
// / .table__cell）。少了根 `<Table>` 时 TableContext 取到的是默认空对象，
// slots 为 undefined，所有插槽类名静默丢失 —— 页面看起来还在，但表头退回
// 浏览器原生 <th>（text-align:center），和其他页面的表格样式对不上。
//
// 这个错误没有任何类型系统兜底（Table.Content 的 props 不依赖根组件存在），
// 所以用源码断言守：出现 Table.ScrollContainer 的文件，必须有同数量的
// 根 `<Table ...>`。
const SRC = fileURLToPath(new URL('../src', import.meta.url))

async function walk(dir, out = []) {
  for (const e of await readdir(dir, { withFileTypes: true })) {
    const full = join(dir, e.name)
    if (e.isDirectory()) {
      if (e.name === 'node_modules' || e.name === 'dist') continue
      await walk(full, out)
    } else if (e.name.endsWith('.tsx')) {
      out.push(full)
    }
  }
  return out
}

const rel = (f) => relative(SRC, f).split(sep).join('/')
const count = (src, re) => (src.match(re) ?? []).length

test('每处 Table.ScrollContainer 都有配套的 Table 根组件', async () => {
  const files = await walk(SRC)
  const violations = []
  for (const f of files) {
    const src = await readFile(f, 'utf8')
    const scroll = count(src, /<Table\.ScrollContainer[\s>]/g)
    if (scroll === 0) continue
    // `<Table ` / `<Table>` 才算根；`<Table.ScrollContainer` / `<Table.Content`
    // 这类带点的子组件不计。
    const roots = count(src, /<Table(?=[\s>])/g)
    if (roots !== scroll) {
      violations.push(`${rel(f)}: Table.ScrollContainer ${scroll} 处，Table 根 ${roots} 处`)
    }
  }
  assert.deepEqual(violations, [], '缺 Table 根的表格会丢失全部插槽样式：\n' + violations.join('\n'))
})
