<script setup lang="ts">
/**
 * 上游模型连通率（管理员）。每个模型一张卡片，卡内是它自己的连通率曲线。
 *
 * 数据来自后台定时探测（internal/probe.Runner），本页只读 model_probes 表。
 * 探测请求 max_tokens=1、不经 usage_recorder，不扣任何网关用户的钱。
 *
 * 为什么是「一模型一卡」而不是「一堆模型挤在一张图上」：几十条曲线叠在
 * 同一坐标系里没法读 —— 既看不出单条曲线的形状，也看不出「谁在什么时候
 * 挂了」。拆成卡片后每张图只讲一个模型，且共用同一条时间轴，仍可横向对比。
 *
 * 纯 SVG 绘制，不引图表库（项目本来也没有；NAS 自用部署不值得让单二进制
 * 多背几百 KB 依赖）。
 */
import { computed, onMounted, ref } from 'vue'
import { api } from '../api'
import { toast } from '../ui'
import type { ProbeHealth, ProbeTimelineBucket } from '../types'

const err = ref('')
const loading = ref(true)
const healths = ref<ProbeHealth[]>([])
/** 用户勾选要显示的模型 id。空 = 一个都不画（界面给提示，不自动补）。 */
const picked = ref<string[]>([])
/** 所有桶（多模型一次查回来），按 model_id 分组缓存。 */
const bucketsByModel = ref<Record<string, ProbeTimelineBucket[]>>({})
const days = ref(7)
const granularity = ref<'hour' | 'day'>('hour')
const timelineLoading = ref(false)

/** 有探测记录的模型排在前面：没数据的新模型不该占掉默认展示位。 */
const orderedHealths = computed(() =>
  [...healths.value].sort((a, b) => {
    const ao = a.window_count > 0 ? 0 : 1
    const bo = b.window_count > 0 ? 0 : 1
    return ao - bo || a.model_id.localeCompare(b.model_id)
  }),
)

async function loadHealths() {
  loading.value = true
  err.value = ''
  try {
    healths.value = await api.probeModels()
    // 默认展示前 6 个有数据的模型：一次给一屏，但不至于铺满整页。
    // 上限 6 是有意的 —— 24 个上限是后端防查询过重，前端再收窄一档，
    // 因为一屏 24 张卡既看不清也用不上（同一 provider 的模型曲线往往同形）。
    if (picked.value.length === 0) {
      picked.value = orderedHealths.value
        .filter((h) => h.window_count > 0)
        .slice(0, 6)
        .map((h) => h.model_id)
    }
    // 清掉已经不存在的模型（被删/改名），否则请求会带上幽灵 id。
    const alive = new Set(healths.value.map((h) => h.model_id))
    picked.value = picked.value.filter((id) => alive.has(id))
    await loadTimeline()
  } catch (e) {
    if ((e as { status?: number }).status === 401) return
    err.value = '加载健康数据失败：' + (e as Error).message
    toast('加载健康数据失败：' + (e as Error).message, 'err')
  } finally {
    loading.value = false
  }
}

async function loadTimeline() {
  if (picked.value.length === 0) {
    bucketsByModel.value = {}
    return
  }
  timelineLoading.value = true
  try {
    const all = await api.probeTimeline(picked.value, days.value, granularity.value)
    // 按 model_id 分组：一次请求回来的桶要拆给各张卡片。
    const grouped: Record<string, ProbeTimelineBucket[]> = {}
    for (const b of all) {
      ;(grouped[b.model_id] ??= []).push(b)
    }
    bucketsByModel.value = grouped
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('加载时间轴失败：' + (e as Error).message, 'err')
  } finally {
    timelineLoading.value = false
  }
}

function togglePicked(id: string): void {
  const next = picked.value.includes(id)
    ? picked.value.filter((x) => x !== id)
    : [...picked.value, id]
  picked.value = next
  loadTimeline()
}

function pickAll(): void {
  picked.value = orderedHealths.value.filter((h) => h.window_count > 0).map((h) => h.model_id)
  loadTimeline()
}

function clearPicked(): void {
  picked.value = []
  loadTimeline()
}

onMounted(loadHealths)

function healthById(id: string): ProbeHealth | undefined {
  return healths.value.find((h) => h.model_id === id)
}

// ---------- 折线图（纯 SVG） ----------

const CH_W = 320 // viewBox 宽；卡片小，用小一点的坐标系，CSS 再拉伸
const CH_H = 110
const PAD_L = 6
const PAD_R = 6
const PAD_T = 8
const PAD_B = 8

interface LinePoint {
  ts: number
  rate: number | null // null = 该桶无探测记录（缺口，不是 0%）
  bucket: ProbeTimelineBucket | null
}

// buildPoints 把稀疏桶补成连续时间轴。
//
// # 为什么必须补缺口
//
// 后端对「该时间窗没有探测记录」的桶**不返回**（没探测 ≠ 全失败，两者必须
// 可区分）。而折线天然连接相邻点 —— 于是「上午正常 / 停机 5 小时 / 下午恢复」
// 会被画成一条从 100% 直通 100% 的斜线，**故障时段在图上彻底消失**，正好抹掉
// 这个页面存在的理由。所以缺口必须是 null，渲染成断线。
function buildPoints(buckets: ProbeTimelineBucket[]): LinePoint[] {
  const bucketMs = granularity.value === 'day' ? 86400000 : 3600000
  const now = Date.now()
  const from = now - days.value * 86400000
  const byTs = new Map<number, ProbeTimelineBucket>()
  for (const b of buckets) byTs.set(b.ts, b)

  const out: LinePoint[] = []
  const start = Math.floor(from / bucketMs) * bucketMs
  for (let ts = start; ts <= now; ts += bucketMs) {
    const b = byTs.get(ts)
    out.push({ ts, rate: b && b.count > 0 ? b.ok / b.count : null, bucket: b ?? null })
  }
  return out
}

function pxFor(n: number, i: number): number {
  const d = Math.max(n - 1, 1)
  return PAD_L + (i / d) * (CH_W - PAD_L - PAD_R)
}
function pyFor(rate: number): number {
  const h = CH_H - PAD_T - PAD_B
  return PAD_T + (1 - rate) * h
}

/** 一条曲线的渲染结果：若干段折线（缺口断开）+ 有效点。 */
interface Curve {
  segments: { pts: LinePoint[]; coords: string }[]
  // dots 带原始 point：悬停要显示该桶的成功率/延迟，光有 x/y 不够。
  dots: { x: number; y: number; rate: number; point: LinePoint }[]
  hasData: boolean
}

function buildCurve(buckets: ProbeTimelineBucket[]): Curve {
  const points = buildPoints(buckets)
  const n = points.length
  const segments: Curve['segments'] = []
  const dots: Curve['dots'] = []
  let cur: LinePoint[] = []
  let curStart = 0

  const flush = () => {
    if (!cur.length) return
    const coords = cur
      .map((p, k) => `${pxFor(n, curStart + k).toFixed(1)},${pyFor(p.rate!).toFixed(1)}`)
      .join(' ')
    segments.push({ pts: cur, coords })
    cur = []
  }

  points.forEach((p, i) => {
    if (p.rate === null) {
      flush()
      return
    }
    if (!cur.length) curStart = i
    cur.push(p)
    dots.push({ x: pxFor(n, i), y: pyFor(p.rate), rate: p.rate, point: p })
  })
  flush()

  return { segments, dots, hasData: dots.length > 0 }
}

function fmtTs(ts: number): string {
  const d = new Date(ts)
  const hh = `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
  if (granularity.value === 'day') return `${d.getMonth() + 1}/${d.getDate()}`
  return hh
}

function curveTitle(p: LinePoint): string {
  if (!p.bucket) return `${fmtTs(p.ts)} · 没有探测记录`
  const rate = Math.round((p.rate ?? 0) * 100)
  return `${fmtTs(p.ts)} · 成功率 ${rate}% (${p.bucket.ok}/${p.bucket.count}) · 平均 ${p.bucket.latency}ms`
}

// 健康态徽章 class。
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
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>连通率</h1>
        <div class="sub">上游模型的定时探测结果 · 每张卡片一个模型</div>
      </div>
      <div class="head-actions">
        <select v-model="days" class="select sel" @change="loadTimeline">
          <option :value="7">近 7 天</option>
          <option :value="14">近 14 天</option>
          <option :value="30">近 30 天</option>
        </select>
        <select v-model="granularity" class="select sel" @change="loadTimeline">
          <option value="hour">按小时</option>
          <option value="day">按天</option>
        </select>
      </div>
    </div>

    <div v-if="loading" class="loading">加载中…</div>
    <div v-else-if="err" class="empty">{{ err }}</div>
    <div v-else-if="healths.length === 0" class="empty">
      还没有可探测的模型 —— 请先在「上游与模型」页添加
    </div>

    <template v-else>
      <!-- 模型选择区：用户自定义显示哪几个 -->
      <div class="card picker">
        <div class="card__header">
          <div class="card__title">
            显示哪些模型
            <span class="dim">（已选 {{ picked.length }} 个）</span>
          </div>
          <div class="card__footer">
            <button class="btn btn-sm btn-ghost" type="button" @click="pickAll">全选</button>
            <button class="btn btn-sm btn-ghost" type="button" @click="clearPicked">清空</button>
          </div>
        </div>
        <div class="picker-list">
          <label v-for="h in orderedHealths" :key="h.model_id" class="picker-item">
            <input
              type="checkbox"
              :checked="picked.includes(h.model_id)"
              @change="togglePicked(h.model_id)"
            />
            <span class="mono picker-name">{{ h.model_id }}</span>
            <span class="badge" :class="healthClass(h.health)">{{ healthLabel(h.health) }}</span>
          </label>
        </div>
      </div>

      <div v-if="timelineLoading" class="loading">加载曲线…</div>
      <div v-else-if="picked.length === 0" class="empty">
        还没选任何模型 —— 上面勾选几个就能看到它们的曲线
      </div>

      <div v-else class="chart-grid">
        <article v-for="id in picked" :key="id" class="card chart-card">
          <div class="card__header">
            <div class="card__title chart-title mono">{{ id }}</div>
            <div class="card__description chart-sub">
              <template v-if="healthById(id)">
                <span class="badge" :class="healthClass(healthById(id)!.health)">
                  {{ healthLabel(healthById(id)!.health) }}
                </span>
                <template v-if="healthById(id)!.window_count > 0">
                  窗口成功率 {{ Math.round(healthById(id)!.success_rate * 100) }}%
                  · 连续失败 {{ healthById(id)!.consecutive_fail }}
                </template>
                <template v-if="healthById(id)!.last_error">
                  · {{ healthById(id)!.last_error }}
                </template>
              </template>
            </div>
          </div>

          <div class="card__content">
            <svg class="chart" :viewBox="`0 0 ${CH_W} ${CH_H}`" preserveAspectRatio="none">
              <!-- 100% / 0% 参考线 + 80% 健康阈值线 -->
              <line class="grid" :x1="PAD_L" :x2="CH_W - PAD_R" :y1="pyFor(1)" :y2="pyFor(1)" />
              <line class="grid" :x1="PAD_L" :x2="CH_W - PAD_R" :y1="pyFor(0)" :y2="pyFor(0)" />
              <line class="thr" :x1="PAD_L" :x2="CH_W - PAD_R" :y1="pyFor(0.8)" :y2="pyFor(0.8)" />

              <!-- 折线按段渲染，缺口自然断开 -->
              <polyline
                v-for="(seg, si) in buildCurve(bucketsByModel[id] ?? []).segments"
                :key="si"
                class="line"
                :points="seg.coords"
              />
              <circle
                v-for="(d, di) in buildCurve(bucketsByModel[id] ?? []).dots"
                :key="'d' + di"
                class="dot"
                :class="{ bad: d.rate < 0.5 }"
                :cx="d.x"
                :cy="d.y"
                r="1.8"
              >
                <title>{{ curveTitle(d.point) }}</title>
              </circle>
            </svg>
            <div v-if="!buildCurve(bucketsByModel[id] ?? []).hasData" class="chart-empty dim">
              该时段没有探测记录
            </div>
          </div>
        </article>
      </div>
    </template>
  </main>
</template>

<style scoped>
.page-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 14px;
  margin-bottom: 14px;
}
.page-head h1 {
  margin: 0 0 4px;
  font-size: 17px;
}
.sub {
  color: var(--text-3);
  font-size: 12.5px;
}
.sel {
  width: 120px;
}
.picker {
  margin-bottom: 14px;
  gap: 10px;
}
.picker .card__header {
  flex-direction: row;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}
.picker-list {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 14px;
}
.picker-item {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 3px 0;
  cursor: pointer;
}
.picker-item input {
  accent-color: var(--accent);
}
.picker-name {
  font-size: 12.5px;
}
.chart-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
  gap: 12px;
}
.chart-card {
  gap: 8px;
  padding: 12px 14px;
  border: 1px solid var(--border);
}
.chart-title {
  font-family: var(--mono);
  font-size: 13px;
  overflow-wrap: anywhere;
}
.chart-sub {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  font-size: 11.5px;
}
.chart {
  width: 100%;
  height: 110px;
  display: block;
}
.chart .grid {
  stroke: var(--border);
  stroke-width: 1;
  vector-effect: non-scaling-stroke;
}
.chart .thr {
  stroke: var(--text-4);
  stroke-dasharray: 3 3;
  opacity: 0.6;
  vector-effect: non-scaling-stroke;
}
.chart .line {
  fill: none;
  stroke: var(--accent);
  stroke-width: 1.4;
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
.chart-empty {
  font-size: 11.5px;
  text-align: center;
  padding: 8px 0;
}
</style>