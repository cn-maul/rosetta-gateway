// resolver hook 本体：补扩展名 + 把 vue 换成 stub。
// 由 tsResolve.mjs 经 module.register 加载，不要直接 import 它。

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
  // `vue` 在测试里只用到 reactive()。给一个最小实现，避免把整个 Vue
  // 运行时拖进 Node 测试进程（那会带来 SFC 编译等一堆无关依赖）。
  if (specifier === 'vue') {
    return {
      url: pathToFileURL(path.join(STUB_DIR, 'vue.mjs')).href,
      shortCircuit: true,
    }
  }
  const hit = tryResolve(specifier, context)
  if (hit) return hit
  return next(specifier, context)
}

