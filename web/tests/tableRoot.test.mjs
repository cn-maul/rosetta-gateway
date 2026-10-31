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

// react-aria-components（HeroUI Table 的底座）在客户端渲染时硬性要求：
// 只要一张表渲染出至少一列，就必须有**且仅由** isRowHeader 标出的行首列。
// 库源码：react-aria-components/dist/private/Table.mjs:145
//   if (this.rowHeaderColumnKeys.size === 0 && this.columns.length > 0 && !isSSR)
//     throw new Error('A table must have at least one Column with the isRowHeader prop set to true')
// SSR 不触发这条断言，所以 `npm run build`、vite preview、单测全都看不见它 ——
// 只有真浏览器点到那个页面才整页崩成 ErrorBoundary。
//
// 这里按「每张表的表头都要标一个行首列」来守：<Table.Header> 的数量
// 必须被 isRowHeader 的数量覆盖。空的占位列（如批量选择框列）不能当行首。
test('每张表都标了 isRowHeader 行首列', async () => {
  const files = await walk(SRC)
  const violations = []
  for (const f of files) {
    const src = await readFile(f, 'utf8')
    const headers = count(src, /<Table\.Header[\s>]/g)
    if (headers === 0) continue
    const rowHeaders = count(src, /\bisRowHeader\b/g)
    if (rowHeaders < headers) {
      violations.push(`${rel(f)}: 表头 ${headers} 处，isRowHeader ${rowHeaders} 处`)
    }
  }
  assert.deepEqual(
    violations,
    [],
    '缺少 isRowHeader 的表在浏览器里会整页崩：\n' + violations.join('\n'),
  )
})
