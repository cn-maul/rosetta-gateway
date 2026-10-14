<script setup lang="ts">
/**
 * 上游模型连通率（管理员）。按时间轴展示每个模型的探测成功率。
 *
 * 数据来自后台定时探测（internal/probe.Runner），本页只读 model_probes 表。
 * 探测请求 max_tokens=1、不经 usage_recorder，不扣任何网关用户的钱。
 *
 * 热力图用纯 CSS 条带（项目没有任何图表库，Overview 的条形图也是纯 div）：
 * 每列一个时间桶，颜色深浅 = 成功率，空白 = 该桶没探测记录（≠ 全失败）。
 */
import { computed, onMounted, ref } from 'vue'
import { api } from '../api'
import { toast } from '../ui'
import type { ProbeHealth, ProbeTimelineBucket } from '../types'

const err = ref('')
const loading = ref(true)
const healths = ref<ProbeHealth[]>([])
const selected = ref('')
const timeline = ref<ProbeTimelineBucket[]>([])
const days = ref(7)
const granularity = ref<'hour' | 'day'>('hour')
const timelineLoading = ref(false)

async function loadHealths() {
  loading.value = true
  err.value = ''
  try {
    healths.value = await api.probeModels()
    // 默认选中第一个有探测记录的模型（否则时间轴页打开是空的）。
    if (!selected.value) {
      const first = healths.value.find((h) => h.window_count > 0)
      if (first) selected.value = first.model_id
    }
    if (selected.value) await loadTimeline()
  } catch (e) {
    if ((e as { status?: number }).status === 401) return
    err.value = '加载健康数据失败：' + (e as Error).message
    toast('加载健康数据失败：' + (e as Error).message, 'err')
  } finally {
    loading.value = false
  }
}

async function loadTimeline() {
  if (!selected.value) return
  timelineLoading.value = true
  try {
    timeline.value = await api.probeTimeline(selected.value, days.value, granularity.value)
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('加载时间轴失败：' + (e as Error).message, 'err')
  } finally {
    timelineLoading.value = false
  }
}

onMounted(loadHealths)

// 健康态徽章 class（与总览/历史同一套 badge 语义）。
function healthClass(h: string): string {
  switch (h) {
    case 'healthy':
      return 'badge-live'
    case 'down':
      return 'badge-off'
    case 'degraded':
      return 'badge-warn'
    default:
      return 'badge-dim'
  }
}
function healthLabel(h: string): string {
  switch (h) {
    case 'healthy':
      return '正常'
    case 'down':
      return '不可用'
    case 'degraded':
      return '不稳定'
    default:
      return '未探测'
  }
}

// ---------- 折线图（纯 SVG，不用图表库） ----------

// 项目没有任何图表库（Overview 的条形图也是纯 div + inline width）。
// 引 charting 库对 NAS 自用部署不划算 —— 一个 go:embed 的单二进制因此要
// 多背几百 KB 依赖，而这里只需要一条折线 + 一片断点。

const CH_W = 900 // viewBox 宽（实际宽度由 CSS 拉伸，等比缩放）
const CH_H = 220 // viewBox 高
const PAD_L = 38 // 左边距：给「100%」「50%」「0」刻度留位置
const PAD_R = 10
const PAD_T = 12
const PAD_B = 26 // 底边距：X 轴标签

// 一个「点」= 一个时间桶，可能有数据也可能没有。
interface LinePoint {
  ts: number
  rate: number | null // null = 该桶没有探测记录（缺口，不是 0%）
  bucket: ProbeTimelineBucket | null
}

// points 把后端返回的稀疏桶补成**连续**的时间轴。
//
// # 为什么必须补缺口（这是折线图最容易做错的地方）
//
// 后端对「该时间窗内没有探测记录」的桶**不返回**（store.ProbeTimeline 的
// 设计：没探测 ≠ 全失败，两者必须可区分）。而折线图天然会把相邻两个点
// 连起来 —— 于是「上午 9 点正常、10 点到 14 点网关挂了没探测、15 点恢复」
// 会被画成一条从 100% 直通 100% 的斜线，**故障时段在图上完全消失**。
//
// 那正好抹掉了这个页面存在的理由：用户要看的就是「什么时候连不上」。
// 所以缺口必须是 null，并渲染成断线。
const points = computed<LinePoint[]>(() => {
  const bucketMs = granularity.value === 'day' ? 86400000 : 3600000
  const now = Date.now()
  const from = now - days.value * 86400000
  const byTs = new Map<number, ProbeTimelineBucket>()
  for (const b of timeline.value) byTs.set(b.ts, b)

  const out: LinePoint[] = []
  // 从「整桶对齐」的起点开始，避免第一个点悬空在坐标轴左侧之外。
  const start = Math.floor(from / bucketMs) * bucketMs
  for (let ts = start; ts <= now; ts += bucketMs) {
    const b = byTs.get(ts)
    out.push({
      ts,
      rate: b && b.count > 0 ? b.ok / b.count : null,
      bucket: b ?? null,
    })
  }
  return out
})

// 坐标映射：x 按时间线性铺满绘图区，y 把 0~100% 映射到下~上。
function px(i: number): number {
  const n = Math.max(points.value.length - 1, 1)
  return PAD_L + (i / n) * (CH_W - PAD_L - PAD_R)
}
function py(rate: number): number {
  const h = CH_H - PAD_T - PAD_B
  return PAD_T + (1 - rate) * h
}

// segments 把点切成若干条**连续有数据**的折线段（缺口处断开）。
function segments(): LinePoint[][] {
  const segs: LinePoint[][] = []
  let cur: LinePoint[] = []
  for (const p of points.value) {
    if (p.rate === null) {
      if (cur.length) segs.push(cur)
      cur = []
      continue
    }
    cur.push(p)
  }
  if (cur.length) segs.push(cur)
  return segs
}

// polylinePoints 把一段渲染成 SVG 的 points 属性。
function polylinePoints(seg: LinePoint[]): string {
  return seg
    .map((p, k) => `${px(k + pointOffset(seg)).toFixed(1)},${py(p.rate!).toFixed(1)}`)
    .join(' ')
}

// pointOffset 段内首点在整体 points 里的下标 —— polyline 的 x 必须用
// **全局**位置，否则每段都会从左边框重新画起。
function pointOffset(seg: LinePoint[]): number {
  const first = seg[0]?.ts
  const idx = points.value.findIndex((p) => p.ts === first)
  return idx < 0 ? 0 : idx
}

// X 轴刻度：最多 6 个，均匀取样，避免标签糊成一片。
const xTicks = computed(() => {
  const ps = points.value
  if (ps.length === 0) return []
  const step = Math.max(1, Math.floor(ps.length / 6))
  const out: { i: number; label: string }[] = []
  for (let i = 0; i < ps.length; i += step) {
    const d = new Date(ps[i].ts)
    const label =
      granularity.value === 'day'
        ? `${d.getMonth() + 1}/${d.getDate()}`
        : `${String(d.getHours()).padStart(2, '0')}:00`
    out.push({ i, label })
  }
  return out
})

// 悬停：命中最近的点（SVG 上直接用不可见的大 hit 区，避免逐点算鼠标坐标）。
const hoverIdx = ref(-1)
function onMove(e: MouseEvent): void {
  const el = e.currentTarget as SVGElement
  const rect = el.getBoundingClientRect()
  const ratio = (e.clientX - rect.left) / rect.width
  const n = points.value.length
  if (n === 0) return
  const i = Math.round(ratio * (n - 1))
  hoverIdx.value = Math.max(0, Math.min(n - 1, i))
}
const hovered = computed(() => (hoverIdx.value >= 0 ? points.value[hoverIdx.value] : null))

// bucketLabel 格式化一个桶的时间标签（用于 tooltip 与 X 轴）。
function fmtTs(ts: number): string {
  const d = new Date(ts)
  const hh = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
  if (granularity.value === 'day') return `${d.getMonth() + 1}/${d.getDate()}`
  return hh
}

// 汇总：当前选中模型的整体成功率。
const selectedSummary = computed(() => {
  const h = healths.value.find((x) => x.model_id === selected.value)
  if (!h) return null
  return h
})
</script>

<template>
  <main class="page">
    <div class="price-head">
      <div>
        <div class="price-title">上游模型连通率</div>
        <div class="tip">
          后台每 10 分钟用最小请求（max_tokens=1）探测一次各上游模型。连续失败 3 次判「不可用」，
          最近成功且窗口成功率 ≥80% 判「正常」。探测不扣网关用户的钱。
        </div>
      </div>
    </div>

    <div v-if="loading" class="loading">加载中…</div>
    <div v-else-if="err" class="empty">{{ err }}</div>
    <div v-else-if="healths.length === 0" class="empty">还没有可探测的模型 —— 请先在「上游与模型」页添加</div>

    <template v-else>
      <!-- 模型列表 + 健康态 -->
      <div class="tbl-wrap">
        <table class="tbl">
          <thead>
            <tr>
              <th>模型</th>
              <th>状态</th>
              <th class="num-h">窗口成功率</th>
              <th class="num-h">探测次数</th>
              <th class="num-h">连续失败</th>
              <th>最近失败原因</th>
              <th class="c-op">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="h in healths" :key="h.model_id" :class="{ sel: h.model_id === selected }">
              <td class="mono">{{ h.model_id }}</td>
              <td>
                <span class="badge" :class="healthClass(h.health)">{{ healthLabel(h.health) }}</span>
              </td>
              <td class="num-h">{{ h.window_count > 0 ? Math.round(h.success_rate * 100) + '%' : '—' }}</td>
              <td class="num-h">{{ h.window_count }}</td>
              <td class="num-h">{{ h.consecutive_fail }}</td>
              <td class="dim">{{ h.last_error || '—' }}</td>
              <td class="c-op">
                <button class="btn btn-sm btn-ghost" @click="selected = h.model_id; loadTimeline()">看时间轴</button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- 时间轴 -->
      <div class="timeline-card">
        <div class="timeline-h">
          <span class="timeline-title">
            时间轴：<span class="mono">{{ selected }}</span>
            <span v-if="selectedSummary" class="dim">
              · 整体成功率 {{ selectedSummary.window_count > 0 ? Math.round(selectedSummary.success_rate * 100) + '%' : '—' }}
            </span>
          </span>
          <div class="timeline-controls">
            <select v-model="days" class="select" @change="loadTimeline">
              <option :value="7">近 7 天</option>
              <option :value="14">近 14 天</option>
              <option :value="30">近 30 天</option>
            </select>
            <select v-model="granularity" class="select" @change="loadTimeline">
              <option value="hour">按小时</option>
              <option value="day">按天</option>
            </select>
          </div>
        </div>

        <div v-if="timelineLoading" class="loading">加载时间轴…</div>
        <div v-else-if="timeline.length === 0" class="empty">该时间范围内没有探测记录</div>
        <div v-else class="chart-wrap">
          <!-- 折线图（纯 SVG）。缺口断线：没有探测记录的时段不连线，
               否则会把「故障期间」画成一条横跨的斜线，正好抹掉要看的东西。 -->
          <svg
            class="chart"
            :viewBox="`0 0 ${CH_W} ${CH_H}`"
            preserveAspectRatio="none"
            @mousemove="onMove"
            @mouseleave="hoverIdx = -1"
          >
            <!-- 横向网格与 Y 刻度：0 / 50% / 100% -->
            <g class="grid">
              <line :x1="PAD_L" :x2="CH_W - PAD_R" :y1="py(1)" :y2="py(1)" />
              <line :x1="PAD_L" :x2="CH_W - PAD_R" :y1="py(0.5)" :y2="py(0.5)" />
              <line :x1="PAD_L" :x2="CH_W - PAD_R" :y1="py(0)" :y2="py(0)" />
              <!-- 健康阈值线（80%）：判定 healthy 的那条线，画出来让用户
                   知道「线在阈值之下 = 那个时刻模型算不稳定」。 -->
              <line class="thr" :x1="PAD_L" :x2="CH_W - PAD_R" :y1="py(0.8)" :y2="py(0.8)" />
            </g>
            <g class="ylab">
              <text :x="PAD_L - 6" :y="py(1) + 3">100%</text>
              <text :x="PAD_L - 6" :y="py(0.5) + 3">50%</text>
              <text :x="PAD_L - 6" :y="py(0) + 3">0</text>
            </g>

            <!-- 折线：按段渲染，缺口处自然断开 -->
            <polyline
              v-for="(seg, si) in segments()"
              :key="si"
              class="line"
              :points="polylinePoints(seg)"
            />
            <!-- 数据点：只画有数据的点 -->
            <circle
              v-for="(p, i) in points"
              v-show="p.rate !== null"
              :key="'pt' + i"
              class="dot"
              :class="{ bad: p.rate !== null && p.rate < 0.5 }"
              :cx="px(i)"
              :cy="py(p.rate ?? 0)"
              r="2"
            />

            <!-- 悬停指示 -->
            <g v-if="hovered">
              <line class="cross" :x1="px(hoverIdx)" :x2="px(hoverIdx)" :y1="PAD_T" :y2="CH_H - PAD_B" />
              <circle
                v-if="hovered.rate !== null"
                class="hover-dot"
                :cx="px(hoverIdx)"
                :cy="py(hovered.rate)"
                r="3.5"
              />
            </g>

            <!-- X 轴刻度 -->
            <g class="xlab">
              <text v-for="t in xTicks" :key="t.i" :x="px(t.i)" :y="CH_H - 8">{{ t.label }}</text>
            </g>
          </svg>

          <!-- 悬停读数（放图外，不遮挡曲线） -->
          <div class="chart-read">
            <template v-if="hovered">
              <span class="mono">{{ fmtTs(hovered.ts) }}</span>
              <template v-if="hovered.bucket">
                成功率 <b>{{ Math.round((hovered.rate ?? 0) * 100) }}%</b>
                · {{ hovered.bucket.ok }}/{{ hovered.bucket.count }} 次 · 平均 {{ hovered.bucket.latency }}ms
              </template>
              <template v-else><span class="dim">该时段没有探测记录</span></template>
            </template>
            <span v-else class="dim">悬停查看某一时刻的连通率</span>
          </div>
        </div>
      </div>
    </template>
  </main>
</template>

<style scoped>
.price-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 14px;
  margin-bottom: 14px;
}
.price-title {
  font-size: 15px;
  font-weight: 600;
  margin-bottom: 4px;
}
.tip {
  color: var(--text-3);
  font-size: 12px;
  max-width: 680px;
  line-height: 1.5;
}
tr.sel td {
  background: var(--accent-soft);
}
.timeline-card {
  margin-top: 18px;
  padding: 16px 18px;
  border: 1px solid var(--border);
  border-radius: var(--r-card);
  background: var(--surface);
}
.timeline-h {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 14px;
  flex-wrap: wrap;
}
.timeline-title {
  font-size: 13px;
  font-weight: 600;
}
.timeline-controls {
  display: flex;
  gap: 8px;
}
/* 折线图（纯 SVG，viewBox 等比拉伸） */
.chart-wrap {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.chart {
  width: 100%;
  height: 220px;
  display: block;
  cursor: crosshair;
}
.chart .grid line {
  stroke: var(--border);
  stroke-width: 1;
  vector-effect: non-scaling-stroke;
}
/* 80% 健康阈值线：虚线，弱化但可辨 —— 「线在它之下 = 那时算不稳定」。 */
.chart .grid line.thr {
  stroke: var(--text-4);
  stroke-dasharray: 4 4;
  opacity: 0.7;
}
.chart .ylab text,
.chart .xlab text {
  fill: var(--text-4);
  font-size: 9px;
  text-anchor: end;
}
.chart .xlab text {
  text-anchor: middle;
}
.chart .line {
  fill: none;
  stroke: var(--accent);
  stroke-width: 1.6;
  vector-effect: non-scaling-stroke;
  stroke-linejoin: round;
  stroke-linecap: round;
}
.chart .dot {
  fill: var(--accent);
}
.chart .dot.bad {
  fill: var(--danger);
}
.chart .cross {
  stroke: var(--text-4);
  stroke-width: 1;
  stroke-dasharray: 3 3;
  vector-effect: non-scaling-stroke;
}
.chart .hover-dot {
  fill: var(--accent);
  stroke: var(--surface);
  stroke-width: 2;
  vector-effect: non-scaling-stroke;
}
.chart-read {
  font-size: 12px;
  color: var(--text-3);
  min-height: 18px;
}
</style>
