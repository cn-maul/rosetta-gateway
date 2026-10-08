<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { api } from '../api'
import { toast } from '../ui'
import { fmtNum, fmtTokens, fmtSpeed, fmtSec, fmtPercent, fmtMoney, fmtBalance, fmtRemainder } from '../fmt'
// 命名空间导入并改名：下面有一个组件本地的 `const range = ref<RangeKey>(...)`
//（当前选中的档位），同名会把这里的 range 遮住，导致 range.rangeBounds 被当成
// 那个 ref。改用 timeRange 既避开冲突，也顺带说明它管的是「时间区间」不是「档位」。
import * as timeRange from '../range'
import type { Stats, UsageGroupEntry } from '../types'

const err = ref('')
const loading = ref(true)
const stats = ref<Stats | null>(null)
const byDay = ref<UsageGroupEntry[]>([])
const byModel = ref<UsageGroupEntry[]>([])
const byKey = ref<UsageGroupEntry[]>([])
const byProvider = ref<UsageGroupEntry[]>([])

// ---------- 时间范围选择 ----------
// key 传给后端换算 from/to；label 显示在下拉里。
// days 为 0 表示「全部历史」（from=0）。
type RangeKey = '1d' | '7d' | '14d' | '1m' | '1y' | 'all'
const RANGES: { key: RangeKey; label: string; days: number }[] = [
  { key: '1d', label: '近 1 天', days: 1 },
  { key: '7d', label: '近 7 天', days: 7 },
  { key: '14d', label: '近 14 天', days: 14 },
  { key: '1m', label: '近 1 月', days: 30 },
  { key: '1y', label: '近 1 年', days: 365 },
  { key: 'all', label: '全部', days: 0 },
]
const range = ref<RangeKey>('14d')
const rangeOpen = ref(false)

// 区间口径统一走 range.ts：days=1 是**今天 0 点**起算，不是滚动 24 小时。
// 以前这里写的是 to - days * 86400_000，于是下拉写着「近 1 天」、数字算的却是
// 「从现在往前 24 小时」：早上 0~8 点的调用明明算今天，却落在窗口之外。
function rangeBounds(key: RangeKey): { from: number; to: number } {
  const days = RANGES.find((r) => r.key === key)?.days ?? 14
  return timeRange.rangeBounds(days)
}

function rangeLabel(key: RangeKey): string {
  return RANGES.find((r) => r.key === key)?.label ?? ''
}

// 柱状图的一个桶。labFull / labShort 是**预先算好的展示标签**，模板不再做字符串切片
// （切片散在模板里，改桶宽时极易忘掉一处，标签就会露出原始日期串）。
type Bucket = {
  key: string // v-for 的 key（取桶内首日，天然唯一）
  count: number
  tokens: number
  labFull: string // 桌面标签：按日 MM-DD / 按月 YYYY-MM
  labShort: string // 窄屏标签：只留日或月，避免把窄列撑破
  tip: string // 悬停提示
}

// 把后端按日聚合的结果补齐成连续序列（缺的日期补 0），保证柱状图横轴连续。
// 范围 ≤ 31 天按日展示；更大范围按周/月聚合，避免横轴过密。
const daySeries = computed<Bucket[]>(() => {
  const map = new Map(byDay.value.map((e) => [e.key, e]))
  const days = RANGES.find((r) => r.key === range.value)?.days ?? 14
  const now = Date.now()

  // 全部历史：起点优先用终身累计里的 first_record_at。
  //
  // 修复前这里是「byDay 里最早的那一天」—— 明细剪掉 30 天以后，byDay 只剩
  // 30 天，趋势图会凭空丢掉更早的历史（usage_totals.first_record_at 存的就是
  // 这个值，HANDOFF.md §「总览全部档」要求的正是改用它）。
  // 拿不到（普通用户 / 没请求 / 无数据）时回退到旧逻辑。
  let start: Date
  if (days === 0) {
    const first = stats.value?.lifetime?.first_record_at
    if (first && first > 0) {
      start = new Date(first)
    } else {
      const keys = byDay.value.map((e) => e.key).sort()
      const earliest = keys[0]
      // 兜底（连 byDay 都空）：同样用本地日历日，now 是毫秒时间戳。
      start = earliest
        ? new Date(earliest + 'T00:00:00')
        : timeRange.localMidnight(30, now)
    }
  } else {
    // 柱子的起点必须由 rangeStart() 给，**不能**在这里另算一份。
    //
    // 这两处一旦各写各的就会漂移：rangeBounds 改成「今天 0 点起」而柱子还按
    // 「24 小时前」画，就会得到「数字是今天的数据、图上却有昨天一截」——用户
    // 读图得到的结论和读数字得到的结论相反。这正是本次要修的病根，所以图和
    // 数字必须共用同一个起点函数（localMidnight 同样处理跨月/跨年/夏令时）。
    start = new Date(timeRange.rangeStart(days, now))
  }

  // 逐日补齐。日期串走 timeRange.localDayKey：与后端 store.dayExpr（localtime）
  // 同一口径，本地日历日键必须与 byDay 的 key 逐字相同，否则 map 查不到、
  // 每根柱子都被补成 0 —— 一张「全零图」比报错更难查。
  const daily: { date: string; count: number; tokens: number }[] = []
  const cursor = new Date(start.getFullYear(), start.getMonth(), start.getDate())
  const end = timeRange.localMidnight(0, now)
  while (cursor <= end) {
    const date = timeRange.localDayKey(cursor.getTime())
    const e = map.get(date)
    daily.push({ date, count: e?.count ?? 0, tokens: e?.tokens ?? 0 })
    cursor.setDate(cursor.getDate() + 1)
  }

  // 桶宽：≤31 天按日；1 年按周；全部按月。
  const bucketDays = days === 0 ? 30 : days > 31 ? 7 : 1
  if (bucketDays === 1) {
    return daily.map((d) => ({
      key: d.date,
      count: d.count,
      tokens: d.tokens,
      labFull: d.date.slice(5), // 09-30
      labShort: d.date.slice(8), // 30
      tip: d.date.slice(5),
    }))
  }

  // 周桶 / 月桶的窄标签用「N月」：一年期的横轴本来就是看月份推进，
  // 只写「日」（01/05/10…）根本读不出是哪个月。
  const monthLabel = (dateStr: string) => `${parseInt(dateStr.slice(5, 7), 10)}月`

  const out: Bucket[] = []
  const monthly = bucketDays > 7
  for (let i = 0; i < daily.length; i += bucketDays) {
    const slice = daily.slice(i, i + bucketDays)
    const first = slice[0]
    const last = slice[slice.length - 1]
    // 尾桶不足整桶宽（「近 1 年」末尾常只剩几天）：标签加 * 标记、tip 写明实际
    // 天数 —— 不加标记的话，一个只有 2 天的桶会被读成「流量腰斩」。刻意**不**
    // 按桶宽做高度归一：那会把「只有 2 天的量」放大成「一整桶的量」，数据语义就变了。
    const partial = slice.length < bucketDays
    const mark = partial ? '*' : ''
    const partialNote = partial ? `（部分桶：仅 ${slice.length} 天）` : ''
    // 月桶跨月时标签补上止月：只写首月「2026-03」会被读成整桶都落在 3 月。
    // 同年给「2026-03~04」，跨年给完整两段，避免「~04」被误读成日期里的日。
    const sameMonth = first.date.slice(0, 7) === last.date.slice(0, 7)
    let labFullMonth = first.date.slice(0, 7)
    if (!sameMonth) {
      labFullMonth +=
        first.date.slice(0, 4) === last.date.slice(0, 4)
          ? `~${last.date.slice(5, 7)}`
          : `~${last.date.slice(0, 7)}`
    }
    const m1 = parseInt(first.date.slice(5, 7), 10)
    const m2 = parseInt(last.date.slice(5, 7), 10)
    out.push({
      key: first.date,
      count: slice.reduce((a, b) => a + b.count, 0),
      tokens: slice.reduce((a, b) => a + b.tokens, 0),
      labFull: (monthly ? labFullMonth : first.date.slice(5)) + mark,
      labShort: (monthly ? (sameMonth ? monthLabel(first.date) : `${m1}~${m2}月`) : monthLabel(first.date)) + mark,
      // 月桶的 tip 也给真实日期范围（与周桶同口径），悬停即可确认桶的覆盖范围。
      tip: `${first.date.slice(5)} ~ ${last.date.slice(5)}${partialNote}`,
    })
  }
  return out
})

// 横轴最多显示约 12 个标签，其余隐藏。步长按实际桶数自适应 ——
// 写死 `i % 3` 在「近 1 年 / 全部」这种几十上百个桶时会挤成一团竖排碎字。
const labelStride = computed(() => Math.max(1, Math.ceil(daySeries.value.length / 12)))

const maxTokens = computed(() => Math.max(1, ...daySeries.value.map((d) => d.tokens)))

const topModelMax = computed(() => Math.max(1, ...byModel.value.map((e) => e.tokens)))
const topKeyMax = computed(() => Math.max(1, ...byKey.value.map((e) => e.tokens)))
const topProviderMax = computed(() => Math.max(1, ...byProvider.value.map((e) => e.tokens)))

// 请求序号守卫：快速切换时间范围会并发多个 load()，晚到的旧响应若写回状态，
// 会把新范围的数据覆盖成旧范围的（下拉显示「近 1 天」、表里却是「近 1 年」）。
// 只有「仍是最新序号」的请求才提交状态与收场 loading。
let reqSeq = 0

async function load() {
  const seq = ++reqSeq
  loading.value = true
  err.value = ''
  try {
    const { from, to } = rangeBounds(range.value)
    // 「全部」档额外要终身累计：明细被剪掉之后，byDay 的最早一天不再是
    // 真正的起点（只剩 30 天），first_record_at 才是（见 stats_handler）。
    const wantLifetime = range.value === 'all'
    const [s, d, m, k, p] = await Promise.all([
      api.stats(from, to, wantLifetime),
      api.usageByDay(from, to),
      api.usageByModel(from, to),
      api.usageByKey(from, to),
      api.usageByProvider(from, to),
    ])
    if (seq !== reqSeq) return // 期间切换了新范围，本响应已过时
    stats.value = s
    byDay.value = d
    byModel.value = m
    byKey.value = k
    byProvider.value = p
  } catch (e) {
    if (seq !== reqSeq) return
    if ((e as { status?: number }).status === 401) return
    // 总览有四个并发请求，任一失败整页都拿不到数据。不留错误态的话，
    // 页面呈现的是「加载中…」消失后的空卡片，运维会读成「今天没有流量」，
    // 恰好把网关/上游故障这个方向排除掉 —— 而这正是需要排查的方向。
    err.value = '加载总览失败：' + (e as Error).message
    toast(err.value, 'err')
  } finally {
    if (seq === reqSeq) loading.value = false
  }
}

function selectRange(key: RangeKey) {
  if (key === range.value) {
    rangeOpen.value = false
    return
  }
  range.value = key
  rangeOpen.value = false
  load()
}

// 点击组件外部 / 按 Esc 收起下拉。
// 只靠「选项自己 @click.stop」还不够：点空白处、切到别的控件时同样该收起。
const rangeEl = ref<HTMLElement | null>(null)

function onDocClick(e: MouseEvent) {
  if (!rangeOpen.value) return
  if (rangeEl.value && !rangeEl.value.contains(e.target as Node)) rangeOpen.value = false
}
function onKeydown(e: KeyboardEvent) {
  if (e.key === 'Escape') rangeOpen.value = false
}

onMounted(() => {
  load()
  document.addEventListener('click', onDocClick)
  document.addEventListener('keydown', onKeydown)
})
onUnmounted(() => {
  document.removeEventListener('click', onDocClick)
  document.removeEventListener('keydown', onKeydown)
})
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>总览</h1>
        <div class="sub">网关运行状态与{{ rangeLabel(range) }}用量</div>
      </div>
      <div class="head-actions">
        <!-- 时间范围下拉。
             注意 menu 上的 @click.stop：选项按钮的点击会冒泡到外层这个
             toggle 容器，导致 selectRange 里刚设好的 rangeOpen=false 又被翻回
             true —— 表现就是「选完不收起」。 -->
        <div ref="rangeEl" class="range-select" @click="rangeOpen = !rangeOpen">
          <span class="range-current">{{ rangeLabel(range) }}</span>
          <span class="range-caret">▾</span>
          <div v-if="rangeOpen" class="range-menu" @click.stop>
            <button
              v-for="r in RANGES"
              :key="r.key"
              class="range-item"
              :class="{ active: r.key === range }"
              @click="selectRange(r.key)"
            >
              {{ r.label }}
            </button>
          </div>
        </div>
        <button class="btn" :disabled="loading" @click="load">刷新</button>
      </div>
    </div>

    <div v-if="loading && !stats" class="loading">加载中…</div>
    <!-- 与 Users/Groups 同口径：加载失败与「确实没有数据」必须可区分。 -->
    <div v-else-if="err" class="empty"><div class="big">⚠</div>{{ err }}</div>

    <template v-if="stats">
      <div class="stat-grid">
        <div class="stat-card">
          <div class="k">总请求</div>
          <div class="v num">{{ fmtNum(stats.total_requests) }}</div>
        </div>
        <div class="stat-card">
          <div class="k">总 Tokens</div>
          <div class="v num">{{ fmtTokens(stats.total_tokens) }}</div>
        </div>
        <!-- 费用：按每条用量**落库当时**的模型单价算好并固化（cost_total），再求和。
             所以改价不会回头改写已经发生的费用 —— 拿它和上游账单核对时，
             两边对不上的原因要去查当时的价格，而不是现在的价格。
             未配置价格的模型按 0 计，因此它不是账单，是「按各自当时的价算大概花多少」。 -->
        <div
          class="stat-card"
          title="按各次调用当时的单价固化计算（元）；之后改价不会改写历史费用，未配置价格的模型不计费"
        >
          <div class="k">费用</div>
          <div class="v num">¥{{ fmtMoney(stats.cost) }}</div>
        </div>
        <!-- 余额：与「费用」放在一起是刻意的 —— 它们是同一件事的两面
             （花了多少 / 还剩多少），分开看总有一半说不清。
             待结算余数指「已消费但不足一分、还没从余额扣掉」的部分：
             余额按分扣减，单价低时一次调用不足一分，不显示它就会出现
             「费用在涨、余额不动」。 -->
        <div
          class="stat-card"
          title="账户余额（元）。不足一分的消费先攒着，攒够一分后扣除"
        >
          <div class="k">余额</div>
          <div class="v num">
            {{ fmtBalance(stats.balance_cents ?? 0, stats.balance_unlimited ?? false) }}
          </div>
          <div v-if="fmtRemainder(stats.balance_remainder ?? 0)" class="k sub">
            含 {{ fmtRemainder(stats.balance_remainder ?? 0) }} 待结算
          </div>
        </div>
        <div class="stat-card">
          <div class="k">输入 Tokens</div>
          <div class="v num">{{ fmtTokens(stats.input_tokens) }}</div>
        </div>
        <div class="stat-card">
          <div class="k">输出 Tokens</div>
          <div class="v num">{{ fmtTokens(stats.output_tokens) }}</div>
        </div>
        <div
          class="stat-card"
          :title="`缓存读取 ${fmtTokens(stats.cached_tokens)} / 输入 ${fmtTokens(stats.input_tokens)} tok`"
        >
          <div class="k">缓存命中率</div>
          <div class="v num">
            {{ fmtPercent(stats.cache_hit_rate) }}<span class="unit">%</span>
          </div>
        </div>
        <div class="stat-card">
          <div class="k">平均速度</div>
          <div class="v num">
            {{ fmtSpeed(stats.avg_tokens_per_sec) }}<span class="unit">tok/s</span>
          </div>
        </div>
        <div class="stat-card">
          <div class="k">首字用时</div>
          <div class="v num">
            {{ fmtSec(stats.avg_ttfb_ms) }}<span class="unit">s</span>
          </div>
        </div>
        <div class="stat-card" :class="{ 'is-err': stats.error_count > 0 }">
          <div class="k">错误数</div>
          <div class="v num">{{ fmtNum(stats.error_count) }}</div>
        </div>
      </div>

      <div class="panel">
        <div class="panel-title">
          {{ rangeLabel(range) }}用量
          <span class="hint">{{ rangeLabel(range) === '全部' ? '按月聚合' : rangeLabel(range) === '近 1 年' ? '按周聚合' : '按日聚合' }} · 悬停查看详情</span>
        </div>
        <div class="bars">
          <div v-for="d in daySeries" :key="d.key" class="bar-col">
            <div class="bar-track">
              <div
                class="bar"
                :class="{ dim: d.tokens === 0 }"
                :style="{ height: Math.max(2, (d.tokens / maxTokens) * 100) + '%' }"
                :data-tip="`${d.tip} · ${fmtNum(d.count)} 次 · ${fmtTokens(d.tokens)} tok`"
              ></div>
            </div>
          </div>
        </div>
        <div class="bar-labels" :class="{ dense: daySeries.length > 20 }">
          <span v-for="(d, i) in daySeries" :key="d.key" class="bar-lab" :class="{ dim: i % labelStride !== 0 }">
            <span class="bar-full">{{ d.labFull }}</span>
            <span class="bar-short">{{ d.labShort }}</span>
          </span>
        </div>
      </div>

      <div class="panel">
        <div class="expand-grid is-3col">
          <div class="expand-col">
            <h4>模型用量 Top 10</h4>
            <div v-if="byModel.length === 0" class="empty">暂无用量记录</div>
            <div v-for="e in byModel" :key="e.key" class="mini-row">
              <span class="mini-main mono">{{ e.key }}</span>
              <span class="badge badge-accent num">{{ fmtNum(e.count) }} 次</span>
              <div
                class="bar"
                style="width: 72px; flex: none; border-radius: 3px"
                :style="{ height: '8px', opacity: 0.35 + 0.65 * (e.tokens / topModelMax) }"
              ></div>
              <span class="num" style="width: 64px; text-align: right; flex: none">{{ fmtTokens(e.tokens) }}</span>
            </div>
          </div>
          <div class="expand-col">
            <h4>密钥用量 Top 10</h4>
            <div v-if="byKey.length === 0" class="empty">暂无用量记录</div>
            <div v-for="e in byKey" :key="e.key" class="mini-row">
              <span class="mini-main">{{ e.name || e.key }}</span>
              <span class="badge badge-accent num">{{ fmtNum(e.count) }} 次</span>
              <div
                class="bar"
                style="width: 72px; flex: none; border-radius: 3px"
                :style="{ height: '8px', opacity: 0.35 + 0.65 * (e.tokens / topKeyMax) }"
              ></div>
              <span class="num" style="width: 64px; text-align: right; flex: none">{{ fmtTokens(e.tokens) }}</span>
            </div>
          </div>
          <div class="expand-col">
            <h4>供应商用量 Top 10</h4>
            <div v-if="byProvider.length === 0" class="empty">暂无用量记录</div>
            <div v-for="e in byProvider" :key="e.key" class="mini-row">
              <span class="mini-main mono">{{ e.key }}</span>
              <span class="badge badge-accent num">{{ fmtNum(e.count) }} 次</span>
              <div
                class="bar"
                style="width: 72px; flex: none; border-radius: 3px"
                :style="{ height: '8px', opacity: 0.35 + 0.65 * (e.tokens / topProviderMax) }"
              ></div>
              <span class="num" style="width: 64px; text-align: right; flex: none">{{ fmtTokens(e.tokens) }}</span>
            </div>
          </div>
        </div>
      </div>
    </template>
  </main>
</template>

<style>
/* 速度卡片：单位与样本说明 */
.stat-card .unit {
  font-size: 12px;
  font-weight: 500;
  margin-left: 4px;
  color: var(--text-3);
}
/* 时间范围下拉 */
.range-select {
  position: relative;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 36px;
  padding: 0 14px;
  border: 1px solid var(--border);
  border-radius: var(--r-btn, 24px);
  background: var(--surface);
  cursor: pointer;
  user-select: none;
  font-size: 13px;
  color: var(--text);
  transition:
    border-color 0.15s var(--ease-smooth),
    background 0.15s var(--ease-smooth);
}
.range-select:hover {
  border-color: var(--accent);
  background: var(--accent-soft);
}
.range-current {
  min-width: 64px;
  text-align: center;
}
.range-caret {
  color: var(--text-3);
  font-size: 11px;
}
.range-menu {
  position: absolute;
  top: calc(100% + 6px);
  right: 0;
  min-width: 120px;
  background: var(--overlay);
  color: var(--overlay-foreground);
  border: 1px solid var(--border);
  border-radius: var(--r-thumb, 12px);
  box-shadow: var(--sh-overlay);
  z-index: 20;
  overflow: hidden;
  padding: 4px;
}
.range-item {
  display: block;
  width: 100%;
  text-align: left;
  padding: 8px 10px;
  border: none;
  background: transparent;
  color: var(--text);
  font-size: 13px;
  border-radius: var(--r-chip, 6px);
  cursor: pointer;
}
.range-item:hover {
  background: var(--default);
}
.range-item.active {
  background: var(--accent);
  color: var(--accent-foreground);
}
/* 三个 by-* 维度并排（expand-grid 默认两列）；窄屏沿用全局的单列降级。
   注意这里的选择器特异性高于 styles.css 里媒体查询内的 .expand-grid，
   所以窄屏降级必须在这里重写一遍。 */
.expand-grid.is-3col {
  grid-template-columns: 1fr 1fr 1fr;
}
/* 指标卡一行五个（2026-10-10 用户要求：一行三个太占地方）。
   共 10 张卡 → 5×2 正好铺满，没有落单的空位。
   覆盖写在页面内而不是改全局 styles.css 的 .stat-grid：那个类目前只有本页
   在用，但它是全局文件、可能被别处复用；而这一页的卡片数量（10）是这里的
   事实，列数应与它配套，不该让全局样式替某一页的卡片数做决定。 */
.stat-grid {
  grid-template-columns: repeat(5, minmax(0, 1fr));
}
/* 窄屏降级要自己重写：上面的选择器特异性与 styles.css 媒体查询里的
   .stat-grid 相同（都是单类），后加载的页面样式会赢 —— 不写这段的话
   手机上也会挤成五列，卡片里的数字直接溢出。 */
@media (max-width: 1100px) {
  .stat-grid {
    grid-template-columns: repeat(3, minmax(0, 1fr));
  }
}
@media (max-width: 640px) {
  .stat-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}
@media (max-width: 900px) {
  .expand-grid.is-3col {
    grid-template-columns: 1fr;
  }
}
/* 柱状图双形态标签：桌面显示完整日期，窄屏只显示尾部 */
.bar-full {
  display: inline;
}
.bar-short {
  display: none;
}
@media (max-width: 640px) {
  .bar-full {
    display: none;
  }
  .bar-short {
    display: inline;
  }
}
</style>
