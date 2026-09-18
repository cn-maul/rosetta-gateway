// 把 web/dist 同步到 internal/webui/dist（go:embed 的数据源）。
// 用法：npm run sync（或 node sync-embed.mjs）
import { rmSync, cpSync, mkdirSync, existsSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const src = join(here, 'dist')
const dst = join(here, '..', 'internal', 'webui', 'dist')

if (!existsSync(src)) {
  console.error('[sync] web/dist 不存在 —— 先运行 npm run build')
  process.exit(1)
}

rmSync(dst, { recursive: true, force: true })
mkdirSync(dirname(dst), { recursive: true })
cpSync(src, dst, { recursive: true })
console.log('[sync] web/dist -> internal/webui/dist 完成')
