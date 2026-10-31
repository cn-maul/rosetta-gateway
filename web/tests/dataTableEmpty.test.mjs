// DataTable 的 empty 必须是「由数据算出的布尔」，不能是字面量。
//
// 这个坑的形态：DataTable 的分支是
//   loading && !hasData ? 骨架 : empty && !error ? 空态 : 表格
// 一旦调用方写 `empty={true}`，第二支永远命中 —— 表格本体一次都不渲染。
// 页面其它地方（外层 `users.length === 0 ? … : <>` 之类）看着都对，只有
// 列表永远是空的，用户会以为数据没了（2026-11-01 用户报「用户列表里没有
// 用户了」，库里数据好好的）。
//
// 走源码断言而不是渲染测试：要守的是「调用点的实参形状」，HeroUI 表体
// 涉及 react-aria 的 collection 提交，为一个布尔把整棵树搬进 node 不值当。

import assert from 'node:assert/strict'
import { readFileSync, readdirSync, statSync } from 'node:fs'
import { dirname, join, relative } from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

const SRC = join(dirname(fileURLToPath(import.meta.url)), '..', 'src')

function tsxFiles(dir) {
  const out = []
  for (const name of readdirSync(dir)) {
    const p = join(dir, name)
    if (statSync(p).isDirectory()) out.push(...tsxFiles(p))
    else if (name.endsWith('.tsx')) out.push(p)
  }
  return out
}

// 取出从 `<DataTable` 到该标签结束 `>` 的属性文本。
//
// 不能简单 indexOf('>')：属性值里的 `hasData={users.length > 0}` 也含 `>`，
// 一截就断在那里，后面的 empty 根本看不到 —— 第一版就是这么漏掉的（证伪
// 时只有第二条用例红，通用扫描静默放过）。改成跟踪花括号深度与引号，
// 深度为 0 且不在引号内时的 `>` 才是标签结束。
function dataTableProps(src, from) {
  const start = src.indexOf('<DataTable', from)
  if (start === -1) return null
  let depth = 0
  let quote = ''
  for (let i = start; i < src.length; i += 1) {
    const ch = src[i]
    if (quote) {
      if (ch === quote) quote = ''
      continue
    }
    if (ch === '"' || ch === "'" || ch === '`') quote = ch
    else if (ch === '{') depth += 1
    else if (ch === '}') depth -= 1
    else if (ch === '>' && depth === 0) return { start, text: src.slice(start, i + 1) }
  }
  return { start, text: src.slice(start) }
}

test('每个 <DataTable 的 empty 实参都由数据算，不是字面量 true', () => {
  const offenders = []
  for (const file of tsxFiles(SRC)) {
    const src = readFileSync(file, 'utf8')
    let at = 0
    for (;;) {
      const hit = dataTableProps(src, at)
      if (!hit) break
      at = hit.start + 1
      const m = hit.text.match(/\bempty=\{([^}]*)\}/)
      if (!m) continue
      const arg = m[1].trim()
      if (arg === 'true' || arg === 'false') {
        offenders.push(`${relative(join(SRC, '..'), file)}: empty={${arg}}`)
      }
    }
  }
  assert.deepEqual(
    offenders,
    [],
    'empty 写成字面量会让 DataTable 永远走空态分支、表格永不渲染：\n  ' + offenders.join('\n  '),
  )
})

test('用户表与钱包三张表的 empty 跟着行数走', () => {
  const users = readFileSync(join(SRC, 'components/users/UserTables.tsx'), 'utf8')
  assert.match(users, /empty=\{users\.length === 0\}/, '用户表的 empty 必须看 users.length')
  assert.match(users, /empty=\{groups\.length === 0\}/, '分组表的 empty 必须看 groups.length')

  const wallet = readFileSync(join(SRC, 'components/wallet/WalletTables.tsx'), 'utf8')
  // 三张表各自的空态在外层用 byUser/byDay/topups 判过了，所以 DataTable 层
  // 不给 empty（默认 false）即可 —— 关键是不能再传字面量 true。
  assert.doesNotMatch(wallet, /empty=\{true\}/, '钱包三张表不应再传字面量 empty')
  assert.equal(
    (wallet.match(/<DataTable label="账户账单"/g) ?? []).length,
    3,
    '钱包三张表的 DataTable 数量应为 3',
  )
})
