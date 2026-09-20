/// <reference types="vite/client" />

declare module '*.vue' {
  import type { DefineComponent } from 'vue'
  const component: DefineComponent<{}, {}, any>
  export default component
}

// 构建时由 vite.config.ts 的 define 注入（见该文件「版本号单一来源」注释）
declare const __APP_VERSION__: string
declare const __ROSETTA_VERSION__: string
