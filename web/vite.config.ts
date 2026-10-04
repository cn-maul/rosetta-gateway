import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 读取同工程内的文件（路径相对本配置文件，即 web/）
function readText(rel: string): string {
  try {
    return readFileSync(new URL(rel, import.meta.url), 'utf8')
  } catch {
    return ''
  }
}

// 版本号单一来源，构建时注入，不在源码里手写：
//   __APP_VERSION__     ← web/package.json 的 version（本项目管理界面/二进制版本）
//   __ROSETTA_VERSION__ ← 仓库根 go.mod 里 rosetta SDK 的版本（配套的上游适配层版本）
const pkgVersion =
  (JSON.parse(readText('./package.json') || '{}') as { version?: string }).version ?? '0.0.0'

const rosettaVersion =
  /\bgithub\.com\/cn-maul\/rosetta\s+(v\d+\.\d+\.\d+\S*)/.exec(readText('../go.mod'))?.[1] ??
  'unknown'

// dev 代理目标：网关配置固定在「可执行文件同级」bin/config.json，端口以其中的 listen 为准，
// 读不到时回退到网关自动生成默认配置所用的 8080（0.0.0.0:8080）。
function gatewayTarget(): string {
  const cfg = JSON.parse(readText('../bin/config.json') || '{}') as { listen?: string }
  const port = cfg.listen?.split(':').pop()?.trim() ?? ''
  return /^\d+$/.test(port) ? `http://127.0.0.1:${port}` : 'http://127.0.0.1:8080'
}

// base './' : 构建产物由 Go go:embed 内嵌到 /admin/ 子路径下服务，
// 相对资源路径才能在任意挂载点正确解析。
export default defineConfig({
  base: './',
  plugins: [vue()],
  // root 固定为工程目录的**相对**解析基准，作用域ID(scopeId)才能跨平台一致。
  //
  // @vitejs/plugin-vue 默认用 .vue 文件的**绝对路径**计算 scoped CSS 的
  // data-v-xxx 哈希。CI 在 ubuntu 上是 /home/runner/work/...，本地是
  // C:\Users\<user>\...，路径不同 → 哈希不同：
  //   旧产物 data-v-54140f4c，CI 上变成 data-v-1fb04c12
  // → CSS/JS 内容变 → content hash 文件名变（C8ncBb6Z → Bhfa2-TX）
  // → CI 的 `git diff --exit-code -- internal/webui/dist` 必然报红，
  // 而 diff 里看到的只是文件名变了，肉眼完全看不出真正原因。
  //
  // 用相对 root 固定哈希输入，使产物逐字节可复现。
  root: fileURLToPath(new URL('.', import.meta.url)),
  define: {
    __APP_VERSION__: JSON.stringify(pkgVersion),
    __ROSETTA_VERSION__: JSON.stringify(rosettaVersion),
  },
  server: {
    port: 5173,
    proxy: {
      // 开发模式下前端跑在 vite dev server，管理 API 代理到本机网关
      '/admin/api': {
        target: gatewayTarget(),
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
    sourcemap: false,
  },
})
