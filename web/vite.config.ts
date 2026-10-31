import { readFileSync } from 'node:fs'
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

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
  plugins: [react(), tailwindcss()],
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
    // 分包策略：入口只留应用外壳（Shell / 路由 / 主题 / 版本号），页面按路由懒加载，
    // 第三方按「一起升级、一起失效」的边界切两块 —— 改业务代码时 vendor 缓存仍然有效。
    // Vite 8 用 Rolldown 构建器，manualChunks 已废弃，对应写法是 codeSplitting.groups。
    rolldownOptions: {
      output: {
        codeSplitting: {
          groups: [
            // React 运行时 + 路由：升级节奏与 UI 库无关，单独一块
            {
              name: 'react',
              test: /node_modules[\\/](react|react-dom|scheduler|react-router|react-router-dom|@remix-run)[\\/]/,
            },
            // HeroUI 全家桶（含 react-aria / react-stately 底座）：体积最大、同批升级
            {
              name: 'heroui',
              test: /node_modules[\\/](@heroui|@react-aria|@react-stately|@internationalized|react-aria|react-stately|@formatjs|intl-messageformat)[\\/]/,
            },
            // 其余第三方（lucide 图标等）交给默认自动分包，不硬捆成大块
          ],
        },
      },
    },
  },
})
