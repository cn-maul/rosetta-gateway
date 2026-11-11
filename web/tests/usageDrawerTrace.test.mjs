import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'

// 调用历史的失败排查有两个入口，缺一个就会出现「列表能看出换过目标，
// 点进详情反而什么都不知道」：
//
//   列表（UsageDetail 的 PathFlow）：只画上游名，用来看「换没换」。
//   抽屉（UsageDetailDrawer）：必须展开每个目标的判定与原因。
//
// 抽屉最容易悄悄退化 —— 它原来只渲染 error_detail，而网关本地终止的
// 502（目标冷却 / 无健康凭据）根本没有 error_detail，于是点进去一片空白，
// 正是「只看到 502 和 upstream_error」的由来。
//
// 同时钉住「不许再写第二份解析器」：src/routeTrace.ts 已经处理了转义、
// 模型名里的 " -> "、以及损坏记录的兜底，并有完整测试。复制一份出来，
// 两条轨迟早会在某个边角上不一致。

const read = (rel) => readFile(new URL('../src/' + rel, import.meta.url), 'utf8')

test('抽屉复用 routeTrace 解析器，不另造一份', async () => {
  const src = await read('components/UsageDetailDrawer.tsx')

  assert.match(
    src,
    /import\s*\{[^}]*parseRouteTrace[^}]*\}\s*from\s*'\.\.\/routeTrace'/,
    '抽屉没有复用 src/routeTrace.ts 的解析器',
  )
  assert.doesNotMatch(
    src,
    /(export\s+)?function\s+parseRouteTrace/,
    '抽屉里又实现了一份轨迹解析器 —— 请改用 src/routeTrace.ts',
  )
})

test('抽屉把轨迹的判定结果与原因一起渲染出来', async () => {
  const src = await read('components/UsageDetailDrawer.tsx')

  // 只画上游名等于把原因又丢了一遍，所以这三样必须都出现。
  assert.match(src, /<RouteTrace/, '抽屉没有渲染失败决策轨迹区块')
  assert.match(src, /outcomeLabel/, '轨迹没有展示判定结果')
  assert.match(src, /reasonLabel/, '轨迹没有展示原因说明')
})

test('route_trace 真的下发了，否则抽屉拿不到东西', async () => {
  const go = await readFile(
    new URL('../../internal/admin/usage_handler.go', import.meta.url),
    'utf8',
  )

  assert.ok(
    go.includes("COALESCE(u.route_trace, '')"),
    '调用历史的查询口径里没有 route_trace —— 抽屉永远空白',
  )
  assert.ok(
    go.includes('RouteTrace string `json:"route_trace,omitempty"`'),
    'RouteTrace 没有下发到前端',
  )
})
