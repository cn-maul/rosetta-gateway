import test from 'node:test'
import assert from 'node:assert/strict'
import { readdir, readFile } from 'node:fs/promises'
import { join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

// 反手写门禁（重构方案 §6 / D9）：
//   「去掉手写部分」如果没有数字，就会在某次 review 里被悄悄放弃。
//   这里守住四条线：
//     1) 不许 .vue 回潮；
//     2) 手写 CSS 只允许 theme.css（令牌覆盖）；
//     3) tsx 里不许出现裸原生交互原语（一律用 HeroUI / RAC 组件）；
//     4) src 不许再依赖任何 UI 框架（vue 等）。
// 例外必须在下方 allowlist 里逐条登记并写明理由。

const SRC = fileURLToPath(new URL('../src', import.meta.url))

const EXCLUDE_DIRS = new Set(['node_modules', 'dist'])
const ALLOWED_CSS = new Set(['theme.css'])

// 路径前缀 -> 允许的原语正则（每条都要有理由；当前为空）
const PRIMITIVE_ALLOWLIST = []

const RAW_PRIMITIVE = /<(button|input|select|textarea|table|details|dialog)\b/g

async function walk(dir, out = []) {
  for (const e of await readdir(dir, { withFileTypes: true })) {
    const full = join(dir, e.name)
    if (e.isDirectory()) {
      if (EXCLUDE_DIRS.has(e.name)) continue
      await walk(full, out)
    } else {
      out.push(full)
    }
  }
  return out
}

const rel = (f) => relative(SRC, f).split(sep).join('/')

test('R-sweep src 里不存在 .vue 文件', async () => {
  const files = await walk(SRC)
  const vue = files.filter((f) => f.endsWith('.vue'))
  assert.deepEqual(vue, [], '发现 .vue 回潮：' + vue.join(', '))
})

test('R-sweep 手写 CSS 只允许 theme.css', async () => {
  const files = await walk(SRC)
  const css = files.filter((f) => f.endsWith('.css')).map(rel)
  const extra = css.filter((f) => !ALLOWED_CSS.has(f))
  assert.deepEqual(extra, [], '出现新的手写 CSS 文件（应改为组件或工具类）：' + extra.join(', '))
})

test('R-sweep tsx 里没有裸原生交互原语', async () => {
  const files = (await walk(SRC)).filter((f) => f.endsWith('.tsx'))
  const violations = []
  for (const f of files) {
    const src = await readFile(f, 'utf8')
    for (const m of src.matchAll(RAW_PRIMITIVE)) {
      const name = rel(f)
      const allowed = PRIMITIVE_ALLOWLIST.some((a) => name.startsWith(a.prefix) && a.pattern.test(m[0] + '>'))
      if (!allowed) violations.push(name + ': ' + m[0])
    }
  }
  assert.deepEqual(violations, [], '发现裸交互原语（应改用 HeroUI 组件）：\n' + violations.join('\n'))
})

test('R-sweep src 不依赖 UI 框架', async () => {
  const files = (await walk(SRC)).filter((f) => /\.(ts|tsx)$/.test(f))
  const hits = []
  for (const f of files) {
    const src = await readFile(f, 'utf8')
    if (/from ['"]vue['"]/.test(src) || /from ['"]vue-router/.test(src)) hits.push(rel(f))
  }
  assert.deepEqual(hits, [], '发现 UI 框架依赖回潮：' + hits.join(', '))
})
