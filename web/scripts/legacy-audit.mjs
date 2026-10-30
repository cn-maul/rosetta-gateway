// 前端"手写存量"审计。
//
// 用法：
//   node scripts/legacy-audit.mjs           报告模式：打印当前存量
//   node scripts/legacy-audit.mjs --gate    门禁模式：React 重写的完成判据，未达标则退出码 1
//   node scripts/legacy-audit.mjs --json    机器可读输出（给 CI 日志/看板用）
//
// 为什么需要它：
//   "把手写部分去掉"这种目标如果没有数字，就一定会在某次 review 里被悄悄放弃。
//   这里把四类手写存量变成可统计的量：
//     1) .vue 文件数（框架没换完）
//     2) 旧 CSS 类名在模板里的引用次数（样式还没交给 Tailwind/令牌）
//     3) 裸原生交互原语 <button>/<input>/<select>/... 的数量（交互没交给组件库）
//     4) 手写 CSS 文件数（只允许 theme.css）
//
// 口径说明：
//   - 旧类名集合 = src/styles.css 里定义的类 + 各 .vue 的 <style> 块里定义的类。
//     从样式源反推，而不是手抄一份清单：清单会过期，样式源不会。
//   - 只在 class="..." / className="..." 这类属性值里按 token 匹配，避免误伤
//     组件名（Button）或 Tailwind 工具类。
//   - sr-only 是 Tailwind 自带类，永久排除，否则这个数字永远归不了零。

import { readdir, readFile, stat } from 'node:fs/promises'
import { existsSync } from 'node:fs'
import { dirname, join, relative, sep } from 'node:path'
import { fileURLToPath } from 'node:url'

const HERE = dirname(fileURLToPath(import.meta.url))
const WEB = join(HERE, '..')
const SRC = join(WEB, 'src')

const EXCLUDE_DIRS = new Set(['node_modules', 'dist', '.git', 'scripts'])
const SKIP_CLASSES = new Set(['sr-only'])

// 允许裸露的原生原语：路径前缀 -> 正则（每条都要有理由）
const PRIMITIVE_ALLOWLIST = [
  // 文件选择没有对应的 HeroUI 组件，RAC 的 FileTrigger 内部就是原生 input[type=file]
  { prefix: 'ui/FileImport', pattern: /^<input[^>]*type=["']file["']/ },
]

async function walk(dir, out = []) {
  let entries
  try {
    entries = await readdir(dir, { withFileTypes: true })
  } catch {
    return out
  }
  for (const e of entries) {
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

function classTokens(fileNames) {
  // 从 CSS 源码里抽类名定义
  const names = new Set()
  for (const css of fileNames) {
    for (const m of css.matchAll(/\.(-?[a-zA-Z_][\w-]*)/g)) {
      const name = m[1]
      if (!SKIP_CLASSES.has(name)) names.add(name)
    }
  }
  return names
}

function tokens(text) {
  return text.split(/\s+/).filter(Boolean)
}

function stripPseudo(token) {
  // .btn:hover -> btn；.side-item.router-link-active 之类由 token 化处理
  return token.replace(/[:>+~\[].*$/, '').replace(/\./g, ' ')
}

async function main() {
  const files = await walk(SRC)
  const rel = (f) => relative(SRC, f).split(sep).join('/')

  const vueFiles = files.filter((f) => f.endsWith('.vue'))
  const tsFiles = files.filter((f) => f.endsWith('.ts') || f.endsWith('.tsx'))
  const cssFiles = files.filter((f) => f.endsWith('.css'))

  // ---- 1) 手写 CSS ----
  const handwrittenCss = []
  for (const f of cssFiles) {
    const name = rel(f)
    const lines = (await readFile(f, 'utf8')).split('\n').length
    // styles.css 单独统计（它是重点清除对象），这里只数「其它」手写 CSS
    if (name !== 'theme.css' && name !== 'styles.css') handwrittenCss.push({ file: name, lines })
  }
  const stylesCssPath = join(SRC, 'styles.css')
  const stylesCssLines = existsSync(stylesCssPath)
    ? (await readFile(stylesCssPath, 'utf8')).split('\n').length
    : 0

  // ---- 2) 旧类名集合 ----
  const cssSources = []
  if (existsSync(stylesCssPath)) cssSources.push(await readFile(stylesCssPath, 'utf8'))
  for (const f of vueFiles) {
    const s = await readFile(f, 'utf8')
    for (const m of s.matchAll(/<style[^>]*>([\s\S]*?)<\/style>/g)) cssSources.push(m[1])
  }
  const legacyNames = classTokens(cssSources)

  // ---- 3) 统计 ----
  const classHits = new Map() // class -> count
  const hitsByFile = new Map()
  let classAttrInVue = 0
  const primitives = []
  const allowedPrimitives = []

  const scan = async (f, isVue) => {
    const src = await readFile(f, 'utf8')
    const name = rel(f)

    if (isVue) {
      for (const m of src.matchAll(/class="([^"]*)"/g)) {
        classAttrInVue += 1
        for (const raw of tokens(m[1])) {
          for (const t of tokens(stripPseudo(raw))) {
            if (!legacyNames.has(t)) continue
            classHits.set(t, (classHits.get(t) ?? 0) + 1)
            hitsByFile.set(name, (hitsByFile.get(name) ?? 0) + 1)
          }
        }
      }
    } else {
      for (const m of src.matchAll(/className="([^"]*)"/g)) {
        for (const raw of tokens(m[1])) {
          for (const t of tokens(stripPseudo(raw))) {
            if (!legacyNames.has(t)) continue
            classHits.set(t, (classHits.get(t) ?? 0) + 1)
            hitsByFile.set(name, (hitsByFile.get(name) ?? 0) + 1)
          }
        }
      }
    }

    for (const m of src.matchAll(/<(button|input|select|textarea|table|details|dialog)\b[^>]*>/g)) {
      const tag = m[0]
      const allowed = PRIMITIVE_ALLOWLIST.some((a) => name.startsWith(a.prefix) && a.pattern.test(tag))
      if (allowed) allowedPrimitives.push(name)
      else primitives.push({ file: name, tag: tag.slice(0, 60) })
    }
  }

  for (const f of vueFiles) await scan(f, true)
  for (const f of tsFiles.filter((f) => f.endsWith('.tsx'))) await scan(f, false)

  const legacyHits = [...classHits.values()].reduce((a, b) => a + b, 0)
  const top = [...classHits.entries()].sort((a, b) => b[1] - a[1]).slice(0, 12)

  const report = {
    vueFiles: vueFiles.length,
    tsxFiles: tsFiles.filter((f) => f.endsWith('.tsx')).length,
    tsFiles: tsFiles.length,
    stylesCssLines,
    handwrittenCssFiles: handwrittenCss.length,
    handwrittenCssLines: handwrittenCss.reduce((a, c) => a + c.lines, 0),
    legacyClassDistinct: classHits.size,
    legacyClassHits: legacyHits,
    classAttrInVue,
    rawPrimitives: primitives.length,
    allowedPrimitives: allowedPrimitives.length,
    topLegacyClasses: top.map(([k, v]) => k + ' x' + v),
    handwrittenCss: handwrittenCss.map((c) => c.file + ' (' + c.lines + ' 行)'),
    worstFiles: [...hitsByFile.entries()].sort((a, b) => b[1] - a[1]).slice(0, 8).map(([k, v]) => k + ' -> ' + v),
  }

  if (process.argv.includes('--json')) {
    console.log(JSON.stringify(report, null, 2))
  } else {
    const pad = (s, n) => String(s).padEnd(n)
    console.log('')
    console.log('  前端手写存量审计 · ' + new Date().toISOString().slice(0, 19).replace('T', ' '))
    console.log('  ' + '-'.repeat(52))
    console.log('  ' + pad('.vue 文件', 26) + report.vueFiles)
    console.log('  ' + pad('.tsx 文件', 26) + report.tsxFiles)
    console.log('  ' + pad('styles.css 行数', 26) + report.stylesCssLines)
    console.log('  ' + pad('其它手写 CSS 文件', 26) + report.handwrittenCssFiles + (report.handwrittenCssFiles ? ' (' + report.handwrittenCssLines + ' 行)' : ''))
    console.log('  ' + pad('旧类名引用次数', 26) + report.legacyClassHits + ' (' + report.legacyClassDistinct + ' 个不同类名)')
    console.log('  ' + pad('裸交互原语', 26) + report.rawPrimitives + (report.allowedPrimitives ? ' (白名单 ' + report.allowedPrimitives + ')' : ''))
    console.log('  ' + '-'.repeat(52))
    if (report.topLegacyClasses.length) console.log('  旧类名 Top: ' + report.topLegacyClasses.join('  '))
    if (report.worstFiles.length) console.log('  最重的文件: ' + report.worstFiles.join('  '))
    if (report.handwrittenCss.length) console.log('  手写 CSS: ' + report.handwrittenCss.join('  '))
    console.log('')
  }

  if (process.argv.includes('--gate')) {
    const failures = []
    if (report.vueFiles > 0) failures.push('仍有 ' + report.vueFiles + ' 个 .vue 文件')
    if (report.stylesCssLines > 0) failures.push('styles.css 仍存在（' + report.stylesCssLines + ' 行）')
    if (report.handwrittenCssFiles > 0) failures.push('仍有手写 CSS 文件：' + report.handwrittenCss.join(', '))
    if (report.legacyClassHits > 0) failures.push('仍有 ' + report.legacyClassHits + ' 处旧类名引用')
    if (report.rawPrimitives > 0) failures.push('仍有 ' + report.rawPrimitives + ' 个裸交互原语')
    if (failures.length) {
      console.error('  门禁未通过：')
      for (const f of failures) console.error('   - ' + f)
      process.exit(1)
    }
    console.log('  门禁通过：手写存量已清零。')
  }
}

main().catch((e) => {
  console.error(e)
  process.exit(1)
})
