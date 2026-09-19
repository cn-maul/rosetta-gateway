import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// base './' : 构建产物由 Go go:embed 内嵌到 /admin/ 子路径下服务，
// 相对资源路径才能在任意挂载点正确解析。
export default defineConfig({
  base: './',
  plugins: [vue()],
  server: {
    port: 5173,
    proxy: {
      // 开发模式下前端跑在 vite dev server，
      // 管理 API 代理到本机网关（gateway.ps1 默认端口 8080）
      '/admin/api': {
        target: 'http://127.0.0.1:8080',
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
