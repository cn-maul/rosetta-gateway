import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { buildUsageSeries, bucketDaysFor, granularityLabel, labelStrideFor } from '../src/usageSeries.ts'

// 2026-11-01 需求：总览页改叫「总览」，用量趋势不再用表格而用图表。
//
// 图表不是「更好看的表格」：它把「哪天塌了」变成一眼可见的形状，代价是
// 失去逐行精确读数（那件事由调用日志页按天筛选承担）。所以这里要守住的是
// **表格真的没有了**，而不是两种视图并存 —— 并存会让页面又长又重。
//
// 第二轮（同日）：用户指出 v1.5.3 里只有**一张**竖排柱状图，展示近 x 天
// token 用量、悬停看当天数据。上一版做成七张小图属于过度设计，已合回一张。

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

test('用量趋势用一张图表，不再渲染表格，也不再是七张小图', async () => {
  const page = await read('pages/Overview.tsx')

  assert.ok(page.includes("from '../ui/BarChart'"), '没有引入 BarChart')
  assert.ok(page.includes('<BarChart'), '趋势区没有渲染柱状图')
  // 表格已经下线：留下 DataTable / Table 只会让两套视图同时存在。
  assert.ok(!page.includes('<DataTable'), '趋势区仍在用 DataTable —— 表格应被图表取代')
  assert.ok(!page.includes('<Table'), '趋势区仍在渲染 Table')
  assert.ok(!page.includes("from '../ui/DataTable'"), 'DataTable 的 import 没有清掉')
  // 单图：一份柱状图，而不是每个指标一张。
  assert.equal(page.match(/<BarChart/g).length, 1, '用量趋势应当只有一张柱状图')
  assert.ok(!page.includes('TrendCard'), '七张小图的 TrendCard 已废弃，应合回一张')
})

test('柱状图不吃「点」，吃按时间补齐后的桶，且桶里带标签', async () => {
  const chart = await read('ui/BarChart.tsx')

  assert.ok(chart.includes('buckets: UsageBucket[]'), '柱状图应接收 UsageBucket 序列')
  // 有值至少 2%：0.5% 在高 DPI 屏上会被舍入成看不见，1 次调用与 0 次就分不清了。
  assert.ok(chart.includes('Math.max(2, (b.tokens / denom) * 100)'), '柱高计算丢失了最小值保护')
  assert.ok(chart.includes('b.tokens === 0 ? '), '零值柱没有区分底色')
  assert.ok(chart.includes('role="img"'), '图表缺少 role="img"，读屏用户拿不到总结')
  assert.ok(chart.includes('labShort') && chart.includes('labFull'), '横轴没有完整/窄两套标签')
})

test('序列按本地日历日补齐，缺的日期补 0 而不是被跳过', () => {
  const now = new Date(2026, 10, 5, 12, 0, 0).getTime() // 本地 2026-11-05 12:00
  const series = buildUsageSeries({
    byDay: [
      { key: '2026-11-03', count: 5, tokens: 500 },
      { key: '2026-11-05', count: 2, tokens: 200 },
    ],
    days: 3,
    firstRecordAt: 0,
    now,
  })
  assert.equal(series.length, 3, '近 3 天应当是 3 根柱子')
  assert.deepEqual(
    series.map((b) => b.tokens),
    [500, 0, 200],
    '11-04 没有记录时必须补成 0 —— 跳过它会把「断流一天」画成「没断」',
  )
  assert.deepEqual(series.map((b) => b.key), ['2026-11-03', '2026-11-04', '2026-11-05'])
})

test('「全部」档用终身累计的 first_record_at 定起点，不被剪枝后的窗口截断', () => {
  const now = new Date(2026, 10, 5, 12, 0, 0).getTime()
  const firstRecord = new Date(2026, 9, 1, 8, 0, 0).getTime() // 本地 2026-10-01 08:00
  const series = buildUsageSeries({
    byDay: [{ key: '2026-11-05', count: 1, tokens: 10 }],
    days: 0,
    firstRecordAt: firstRecord,
    now,
  })
  // 按 30 天桶合并后起点仍必须是 2026-10-01（而不是 byDay 里最早的 11-05）。
  assert.equal(series[0].key, '2026-10-01', '「全部」档的起点丢了，图会凭空变短')
})

test('桶宽与聚合说明随区间变化：日 / 周 / 月', () => {
  assert.equal(bucketDaysFor(1), 1)
  assert.equal(bucketDaysFor(30), 1)
  assert.equal(bucketDaysFor(365), 7)
  assert.equal(bucketDaysFor(0), 30)
  assert.equal(granularityLabel(7), '按日聚合')
  assert.equal(granularityLabel(365), '按周聚合')
  assert.equal(granularityLabel(0), '按月聚合')
})

test('横轴标签抽稀到约 12 个', () => {
  assert.equal(labelStrideFor(7), 1)
  assert.equal(labelStrideFor(12), 1)
  assert.equal(labelStrideFor(24), 2)
  assert.equal(labelStrideFor(365), 31)
})

test('不足一整桶的尾桶打标记，不按整桶高度归一', () => {
  const now = new Date(2026, 10, 5, 12, 0, 0).getTime()
  const firstRecord = new Date(2026, 9, 20, 8, 0, 0).getTime() // 只有 17 天
  const series = buildUsageSeries({ byDay: [], days: 0, firstRecordAt: firstRecord, now })
  const lastBucket = series[series.length - 1]
  assert.ok(lastBucket.labFull.endsWith('*'), '尾桶是部分桶，标签应带 * 标记')
  assert.ok(lastBucket.tip.includes('部分桶'), '尾桶提示要点明只有几天')
})
