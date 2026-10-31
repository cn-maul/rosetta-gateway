import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

// 抽屉宽度只能写在 Dialog 的 style 上，不能写在外层 Content 的 className 上。
//
// 原因是 HeroUI 的 DOM 结构：`.drawer__content` 是 `position:fixed; inset:0;
// display:flex; justify-content:flex-end` 的**全屏 overlay**。把 max-w 加在它身上
// 会让这个 overlay 自己缩成 680px 宽并贴着视口左边（inset:0 定的是 left/top），
// 弹窗于是出现在「屏幕中间偏左」—— 用户实测到的现象。
// 而 `.drawer__dialog` 的插槽类是 tailwind-variants 的函数插槽，composeSlotClassName
// 走的是「函数插槽只返回自己的类名」这条路，Dialog 上的 className 会被丢掉，
// 只有 style 能真正落进 DOM。
//
// 这个错误同样没有类型兜底（className 与 style 都不报错），所以用源码断言守。
const src = (p) => readFileSync(new URL(p, import.meta.url), 'utf8')

test('AppDrawer 把宽度写在 Dialog 的 style 上，不写在 Content 的 className 上', () => {
  const drawer = src('../src/ui/AppDrawer.tsx')
  // Content 上不许出现宽度类（会把全屏 overlay 缩窄）。
  const content = drawer.match(/<Drawer\.Content[^>]*>/)
  assert.ok(content, 'AppDrawer 应当渲染 Drawer.Content')
  assert.doesNotMatch(content[0], /max-w-\[|max-w-\d/, 'Drawer.Content 上不能加 max-width：它会把 overlay 缩窄并贴左')
  assert.doesNotMatch(content[0], /\bw-full\b/, 'Drawer.Content 上不能加 w-full：全屏 overlay 本来就 100% 宽')
  // Dialog 上必须有 style 宽度。
  const dialog = drawer.match(/<Drawer\.Dialog[^>]*>/)
  assert.ok(dialog, 'AppDrawer 应当渲染 Drawer.Dialog')
  assert.match(dialog[0], /style=\{/, 'Drawer.Dialog 必须用 style 传宽度（className 会被插槽丢弃）')
})

test('请求详情抽屉只讲错误，不再重复表格里的字段', () => {
  const drawer = src('../src/components/UsageDetailDrawer.tsx')
  // 表格里已经有：模型、密钥、Token、速度、耗时、费用、协议、IP。
  for (const dup of ['Token 用量', '调用密钥', '公开模型名', '输出速度', '记录费用', '首字 / 总耗时']) {
    assert.ok(!drawer.includes(dup), `抽屉不该再重复表格已显示的「${dup}」`)
  }
  // 错误详情三件套必须还在。
  assert.match(drawer, /上游返回的说明/)
  assert.match(drawer, /完整原始响应/)
  assert.match(drawer, /停在哪/)
})
