import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

// 弹窗在「数据还没到」的那一刻就会渲染：主文件为了拿到稳定的 hook 顺序，
// 通常无条件渲染弹窗、只用 open 控制显隐。这时 props 里的实体可能是 null，
// 用 `provider!` 这种非空断言只是把 TS 的嘴堵上，运行时照样在 render 期
// 读 null.name 把整页炸进 ErrorBoundary（用户实测过一次）。
//
// 守住两条：①弹窗自己的 props 允许 null 并在 render 期短路；
// ②调用点不许再写非空断言。
const src = (p) => readFileSync(new URL(p, import.meta.url), 'utf8')

test('ProviderEditModal 允许 provider 为 null 并在 render 期短路', () => {
  const modal = src('../src/components/provider/ProviderEditModal.tsx')
  assert.match(modal, /provider: Provider \| null/, 'ProviderEditModal 的 provider 必须可空')
  assert.match(modal, /if \(!provider\) return null/, 'ProviderEditModal 必须在 render 期短路')
  // effect 里也要判：open 变 true 与 provider 到达不是同一帧。
  assert.match(modal, /if \(!open \|\| !provider\) return/)
})

test('ProviderDetail 不用非空断言喂 ProviderEditModal', () => {
  const page = src('../src/pages/ProviderDetail.tsx')
  assert.match(page, /provider=\{provider\}/)
  assert.doesNotMatch(page, /provider=\{provider!\}/)
})
