// resolver hook 本体：补扩展名（相对 import 不带 .ts 时补全）。
// 由 tsResolve.mjs 经 module.register 加载，不要直接 import 它。
// （曾在这里把 'vue' 换成 stub —— React 重构后 src 不再依赖任何 UI 框架，映射随之删除。）

import { existsSync } from 'node:fs'
import { fileURLToPath, pathToFileURL } from 'node:url'
import path from 'node:path'

const STUB_DIR = path.join(path.dirname(fileURLToPath(import.meta.url)), 'stubs')

function tryResolve(specifier, context) {
  // 只处理相对/绝对路径：裸模块名（node:test、vue）交给下面的映射或默认解析。
  if (!specifier.startsWith('.') && !specifier.startsWith('/') && !specifier.startsWith('file:')) {
    return undefined
  }
  const base = context.parentURL ? new URL(specifier, context.parentURL) : pathToFileURL(specifier)
  const asPath = fileURLToPath(base)
  for (const ext of ['', '.ts', '.mts', '.js', '.mjs']) {
    const candidate = asPath + ext
    if (existsSync(candidate)) {
      return { url: pathToFileURL(candidate).href, shortCircuit: true }
    }
  }
  return undefined
}

export async function resolve(specifier, context, next) {
  const hit = tryResolve(specifier, context)
  if (hit) return hit
  return next(specifier, context)
}

