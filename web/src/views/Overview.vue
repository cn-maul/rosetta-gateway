<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { api } from '../api'
import { toast } from '../ui'
import { fmtNum, fmtTokens, fmtSpeed, fmtSec, fmtPercent, fmtMoney } from '../fmt'
import type { Stats, UsageGroupEntry } from '../types'

const loading = ref(true)
const stats = ref<Stats | null>(null)
const byDay = ref<UsageGroupEntry[]>([])
const byModel = ref<UsageGroupEntry[]>([])
const byKey = ref<UsageGroupEntry[]>([])

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

function rangeBounds(key: RangeKey): { from: number; to: number } {
  const to = Date.now()
  const days = RANGES.find((r) => r.key === key)?.days ?? 14
  const from = days === 0 ? 0 : to - days * 86400_000
  return { from, to }
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
  const now = new Date()
  const p = (x: number) => String(x).padStart(2, '0')
  const fmt = (d: Date) => `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`

  // 全部历史：以数据里最早一天为起点，否则以「今天 - days」为起点。
  let start: Date
  if (days === 0) {
    const keys = byDay.value.map((e) => e.key).sort()
    const earliest = keys[0]
    start = earliest ? new Date(earliest + 'T00:00:00') : new Date(now.getFullYear(), now.getMonth(), now.getDate() - 30)
  } else {
    start = new Date(now.getFullYear(), now.getMonth(), now.getDate() - (days - 1))
  }

  // 逐日补齐。
  const daily: { date: string; count: number; tokens: number }[] = []
  const cursor = new Date(start.getFullYear(), start.getMonth(), start.getDate())
  const end = new Date(now.getFullYear(), now.getMonth(), now.getDate())
  while (cursor <= end) {
    const date = fmt(cursor)
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
    out.push({
      key: first.date,
      count: slice.reduce((a, b) => a + b.count, 0),
      tokens: slice.reduce((a, b) => a + b.tokens, 0),
      labFull: monthly ? first.date.slice(0, 7) : first.date.slice(5),
      labShort: monthLabel(first.date),
      tip: monthly ? first.date.slice(0, 7) : `${first.date.slice(5)} ~ ${last.date.slice(5)}`,
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

async function load() {
  loading.value = true
  try {
    const { from, to } = rangeBounds(range.value)
    const [s, d, m, k] = await Promise.all([
      api.stats(from, to),
      api.usageByDay(from, to),
      api.usageByModel(from, to),
      api.usageByKey(from, to),
    ])
    stats.value = s
    byDay.value = d
    byModel.value = m
    byKey.value = k
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('加载总览失败：' + (e as Error).message, 'err')
  } finally {
    loading.value = false
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
        <!-- 费用：后端按各模型【当前】单价实时估算（元），改价会改写历史区间的结果；
             未配置价格的模型按 0 计，所以它不是账单，是「照这个价算大概花多少」。 -->
        <div
          class="stat-card"
          title="按模型当前单价实时估算（元）；未配置价格的模型不计费"
        >
          <div class="k">费用</div>
          <div class="v num">¥{{ fmtMoney(stats.cost) }}</div>
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
        <div class="expand-grid">
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
