import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'

// 2026-11-01 需求：总览页改叫「总览」，用量趋势不再用表格而用图表。
//
// 图表不是「更好看的表格」：它把「哪天塌了」变成一眼可见的形状，代价是
// 失去逐行精确读数（那件事由调用日志页按天筛选承担）。所以这里要守住的是
// **表格真的没有了**，而不是两种视图并存 —— 并存会让页面又长又重。

const read = (rel) => readFile(new URL('../src/' + rel, import.meta.url), 'utf8')

test('导航与页面标题统一叫「总览」', async () => {
  const nav = await read('ui/nav.ts')
  const router = await read('router.tsx')
  const page = await read('pages/Overview.tsx')

  assert.ok(!nav.includes('网关总览'), '侧栏里还写着「网关总览」')
  assert.ok(nav.includes("label: '总览'"), '侧栏导航没有改成「总览」')
  assert.ok(!router.includes('网关总览'), '路由 handle 标题里还写着「网关总览」')
  assert.ok(page.includes('title="总览"'), '页面内 PageHeader 没有改成「总览」')
})

test('用量趋势用图表，不再渲染表格', async () => {
  const page = await read('pages/Overview.tsx')

  assert.ok(page.includes("from '../ui/BarChart'"), '没有引入 BarChart')
  assert.ok(page.includes('<BarChart'), '趋势区没有渲染柱状图')
  // 趋势表已经下线：留下 DataTable / Table 只会让两套视图同时存在。
  assert.ok(!page.includes('<DataTable'), '趋势区仍在用 DataTable —— 表格应被图表取代')
  assert.ok(!page.includes('<Table'), '趋势区仍在渲染 Table')
  assert.ok(!page.includes("from '../ui/DataTable'"), 'DataTable 的 import 没有清掉')
})

test('柱状图对「没有数据」与「值为 0」都不会画出误导性高度', async () => {
  const chart = await read('ui/BarChart.tsx')

  // 无数据的柱子给 1.5% 的贴底高度（能看见「这天没有」），有值至少 2%
  // （0.5% 在高 DPI 屏上会被舍入成看不见，1 个请求与 0 个就分不清了）。
  assert.match(chart, /p\.value > 0 \? Math\.max\(2, \(p\.value \/ max\) \* 100\) : 0/, '柱高计算丢失了最小值保护')
  assert.ok(chart.includes("style={{ height: `${p.value > 0 ? pct : 1.5}%` }}"), '无数据柱没有贴底高度')
  assert.ok(chart.includes('role="img"'), '图表缺少 role="img"，读屏用户拿不到总结')
  assert.ok(chart.includes('aria-label={`${label}，${points.length} 个数据点，最高 ${format(max)}`}'))
})
