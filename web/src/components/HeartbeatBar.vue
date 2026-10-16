<script setup lang="ts">
/**
 * 连通率条形图（Uptime Kuma HeartbeatBar 的本土化实现）。
 *
 * 为什么换成它、而不是继续用原来的折线图：
 *
 *   折线图把「成功率」当成连续量来画，于是「上午 100% / 停机 5 小时 /
 *   下午恢复」会被画成一条直通 100% 的斜线 —— 故障时段在图上消失。
 *   原实现靠「缺口断线」绕过，但缺口要额外补时间轴、断线仍是折线，
 *   「哪一刻开始坏的」始终读不出来。
 *
 *   条形图把每个探测桶画成一根独立的胶囊：坏的就是红的，一眼定位。
 *   这与连通率的语义也才对得上 —— 它是「这一段时间通不通」，
 *   不是「某个连续函数的采样」。
 *
 * 实现要点（参考 uptime-kuma src/components/HeartbeatBar.vue，MIT）：
 *   - canvas 绘制而非 DOM：上百根条用 div 会把卡片拖垮；
 *   - devicePixelRatio 缩放，否则高清屏上糊；
 *   - 悬停的那根放大 1.5 倍并显示 tooltip，其余压暗；
 *   - 颜色全部走主题变量，暗色模式自动跟随。
 *
 * 与上游的差异：上游的 beat 有 UP/DOWN/PENDING/MAINTENANCE 四态，
 * 我们的探测桶只有「该窗全部成功 / 有失败 / 无记录」三态，
 * 所以没有 pending 与 maintenance 分支。
 */
import { ref, computed, watch, onMounted, onBeforeUnmount, nextTick } from 'vue'
import type { ProbeTimelineBucket } from '../types'

const props = defineProps<{
  /** 探测桶（稀疏：无记录的桶不在其中），由调用方填满时间轴。 */
  buckets: ProbeTimelineBucket[]
  /** 跨度（小时）。用于把稀疏桶铺成连续序列，缺口渲染成「无记录」。 */
  hours: number
  /** 每个桶代表多少分钟。默认 60 = 按小时聚合。 */
  bucketMinutes?: number
}>()

// ---------- 尺寸常量（与上游同值，观感才一致） ----------
const BEAT_W = 10
const BEAT_H = 30
const HOVER_SCALE = 1.5
const PAD = 4

const wrap = ref<HTMLDivElement | null>(null)
const canvas = ref<HTMLCanvasElement | null>(null)

// maxBeats 是容器放得下的条数 —— 由容器宽度反推，不是写死的：
// 卡片宽度随视口变，写死会让条被裁掉或右侧留白。
const maxBeats = ref(0)
const hovered = ref(-1)
const move = ref(false)

interface Beat {
  /** null = 该桶无探测记录（不是 0%，必须与失败区分）。 */
  rate: number | null
  bucket: ProbeTimelineBucket | null
}

/** 可见的条序列：最新的在右边，左侧用 null 补齐历史。 */
const beats = computed<Beat[]>(() => {
  const step = (props.bucketMinutes ?? 60) * 60000
  const total = Math.max(1, Math.round((props.hours * 3600000) / step))
  const byTs = new Map<number, ProbeTimelineBucket>()
  for (const b of props.buckets) byTs.set(b.ts, b)

  // 时间轴末端对齐到当前小时，前端才能与后端的桶边界一致
  // （后端按 UTC 小时取整，这里用本地小时会有半小时错位）。
  const now = Date.now()
  const end = Math.floor(now / step) * step
  const out: Beat[] = []
  for (let i = total - 1; i >= 0; i--) {
    const ts = end - i * step
    const b = byTs.get(ts)
    out.push(b ? { rate: b.ok / b.count, bucket: b } : { rate: null, bucket: null })
  }
  return out.slice(-total)
})

// 只画放得下的部分。上游用右侧对齐 + 左侧留白（numPadding），我们这里
// 让条带**左对齐铺满**容器：左右两端各有一个「多久前 / 现在」的标注，
// 留白会让左标注指向一段空白，读起来像是「那段没有数据」。
const visible = computed<{ beat: Beat; index: number }[]>(() => {
  const start = Math.max(0, beats.value.length - maxBeats.value)
  return beats.value.slice(start).map((beat, i) => ({ beat, index: start + i }))
})

const canvasW = computed(() => visible.value.length * (BEAT_W + PAD * 2))
const canvasH = computed(() => BEAT_H * HOVER_SCALE)

// ---------- 绘制 ----------

function draw() {
  const c = canvas.value
  if (!c) return
  const ctx = c.getContext('2d')
  if (!ctx) return

  const dpr = window.devicePixelRatio || 1
  const w = canvasW.value
  const h = canvasH.value
  c.width = w * dpr
  c.height = h * dpr
  c.style.width = `${w}px`
  c.style.height = `${h}px`
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
  ctx.clearRect(0, 0, w, h)

  // 颜色从主题变量读，暗色模式自动跟随。取不到就退回中性灰 ——
  // 这里不能写死色值，否则换主题时条形会变成一块突兀的色斑。
  const cs = getComputedStyle(document.documentElement)
  const pick = (name: string, fb: string) => cs.getPropertyValue(name).trim() || fb
  const colors = {
    empty: pick('--hb-empty', 'oklch(0.90 0.004 286)'),
    down: pick('--danger', 'oklch(0.65 0.23 26)'),
    warn: pick('--warning', 'oklch(0.78 0.16 72)'),
    up: pick('--success', 'oklch(0.73 0.19 151)'),
  }

  const centerY = h / 2
  const full = BEAT_W + PAD * 2

  visible.value.forEach(({ beat }, i) => {
    const x = i * full + PAD
    const isHover = i === hovered.value && beat.rate !== null

    let w = BEAT_W
    let hh = BEAT_H
    let ox = x
    if (isHover) {
      w *= HOVER_SCALE
      hh *= HOVER_SCALE
      ox = x - (w - BEAT_W) / 2
    }
    const oy = centerY - hh / 2
    // 胶囊形：圆角取宽度的一半（上游同法）。
    const r = w / 2

    ctx.globalAlpha = beat.rate === null ? 0.55 : 1
    ctx.fillStyle = beatColor(beat.rate, colors)
    roundRect(ctx, ox, oy, w, hh, r)
    ctx.fill()
    ctx.globalAlpha = 1
  })
}

function beatColor(rate: number | null, colors: Record<string, string>): string {
  // 无记录与「全失败」必须可区分：前者是「没探测」，后者是「坏了」。
  // 都画成红色会让「凌晨没跑探测」被读成「凌晨全线故障」。
  if (rate === null) return colors.empty
  if (rate >= 1) return colors.up
  if (rate >= 0.5) return colors.warn
  return colors.down
}

function roundRect(
  ctx: CanvasRenderingContext2D,
  x: number,
  y: number,
  w: number,
  h: number,
  r: number,
) {
  ctx.beginPath()
  ctx.moveTo(x + r, y)
  ctx.lineTo(x + w - r, y)
  ctx.quadraticCurveTo(x + w, y, x + w, y + r)
  ctx.lineTo(x + w, y + h - r)
  ctx.quadraticCurveTo(x + w, y + h, x + w - r, y + h)
  ctx.lineTo(x + r, y + h)
  ctx.quadraticCurveTo(x, y + h, x, y + r)
  ctx.lineTo(x, y + r)
  ctx.quadraticCurveTo(x, y, x + r, y)
  ctx.closePath()
}

// ---------- 交互 ----------

const tip = ref<{ show: boolean; x: number; y: number; flip: boolean; text: string }>({
  show: false,
  x: 0,
  y: 0,
  flip: false,
  text: '',
})

function fmtTs(ts: number): string {
  const d = new Date(ts)
  const p = (n: number) => `${n}`.padStart(2, '0')
  return `${p(d.getMonth() + 1)}/${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

function tipText(beat: Beat): string {
  if (!beat.bucket) return `${fmtTs(beatTs(beat))} · 没有探测记录`
  const pct = Math.round((beat.rate ?? 0) * 100)
  return `${fmtTs(beat.bucket.ts)} · 成功率 ${pct}% (${beat.bucket.ok}/${beat.bucket.count}) · 平均 ${beat.bucket.latency}ms`
}

// 无记录的条也要能说出自己的时间，否则 tooltip 是空的。
function beatTs(beat: Beat): number {
  return beat.bucket?.ts ?? 0
}

function onMove(e: MouseEvent) {
  const c = canvas.value
  if (!c) return
  const rect = c.getBoundingClientRect()
  const full = BEAT_W + PAD * 2
  const i = Math.floor((e.clientX - rect.left) / full)
  const hit = visible.value[i]
  if (!hit || hit.beat.rate === null) {
    hovered.value = -1
    tip.value.show = false
    return
  }
  if (hovered.value !== i) hovered.value = i
  // tooltip 默认在 canvas **下方**：卡片里 canvas 上方是标题与说明文字，
  // 弹在上面会正好盖住「这个模型叫什么、图例什么意思」。上方空间不够时
  // 才翻到上面去（视口顶部的情况）。
  const below = window.innerHeight - rect.bottom
  const above = rect.top
  // 再往下推 22px：canvas 正下方是「多久前 / 现在」那行标注，
  // tooltip 贴上去会把它压掉，而那两个标注是读图的方向参照。
  tip.value = {
    show: true,
    x: rect.left + i * full + full / 2,
    y: (below > 90 || below > above ? rect.bottom : rect.top) + 22,
    flip: below <= 90 && above > below,
    text: tipText(hit.beat),
  }
}

function onLeave() {
  hovered.value = -1
  tip.value.show = false
}

// ---------- 生命周期 ----------

function measure() {
  if (!wrap.value) return
  maxBeats.value = Math.floor(wrap.value.clientWidth / (BEAT_W + PAD * 2))
}

let ro: ResizeObserver | null = null
let themeObserver: MutationObserver | null = null

onMounted(() => {
  measure()
  // 上游用 window.resize，这里用 ResizeObserver：卡片宽度会随侧栏折叠、
  // 窗口缩放变化，前者拿不到。
  if (typeof ResizeObserver !== 'undefined' && wrap.value) {
    ro = new ResizeObserver(() => {
      measure()
      void nextTick(draw)
    })
    ro.observe(wrap.value)
  }
  // 主题切换要重画：颜色是绘制时从 CSS 变量读的，不重画就还是旧主题的色。
  themeObserver = new MutationObserver(() => void nextTick(draw))
  themeObserver.observe(document.documentElement, {
    attributes: true,
    attributeFilter: ['data-theme'],
  })
  void nextTick(draw)
})

onBeforeUnmount(() => {
  ro?.disconnect()
  themeObserver?.disconnect()
})

// 数据或悬停变化都要重画。用 watch 而不是 computed 里画 ——
// 绘制有副作用，塞进 computed 会让 Vue 的缓存语义变味。
watch(
  () => [visible.value, hovered.value] as const,
  () => void nextTick(draw),
  { deep: true },
)
</script>

<template>
  <div ref="wrap" class="hb-wrap">
    <div class="hb-bar">
      <canvas
        ref="canvas"
        class="hb-canvas"
        :aria-label="`连通率：${visible.filter((v) => v.beat.rate !== null).length} 个探测窗`"
        role="img"
        @mousemove="onMove"
        @mouseleave="onLeave"
      />
    </div>
    <!-- 左右两端的时间标注：只有一根根条时，「哪边是现在」是读不出来的。
         上游用「首个/末个 beat 至今多久」，这里用绝对时刻更直白。 -->
    <div class="hb-scale">
      <span>{{ hours }} 小时前</span>
      <span>现在</span>
    </div>
    <div
      v-if="tip.show"
      class="hb-tip"
      :class="{ flip: tip.flip }"
      :style="{ left: `${tip.x}px`, top: `${tip.y}px` }"
      role="tooltip"
    >
      {{ tip.text }}
    </div>
  </div>
</template>

<style scoped>
.hb-wrap {
  position: relative;
  width: 100%;
  /* 上下留白给 hover 放大用：不放大会被 overflow 裁掉一截。 */
  padding: calc(var(--beat-h, 30px) * 0.25) 0;
}
.hb-bar {
  --hb-empty: oklch(0.90 0.004 286);
  overflow: hidden;
  width: 100%;
  white-space: nowrap;
}
[data-theme='dark'] .hb-bar {
  /* 暗色下「无记录」要能看出是色块而不是破洞。 */
  --hb-empty: oklch(0.34 0.008 286);
}
.hb-canvas {
  display: block;
}
.hb-scale {
  display: flex;
  justify-content: space-between;
  margin-top: 6px;
  font-size: 11.5px;
  color: var(--muted);
}
.hb-tip {
  position: fixed;
  z-index: 60;
  transform: translate(-50%, 8px);
  padding: 5px 9px;
  border-radius: var(--radius-sm);
  background: var(--eclipse);
  color: var(--snow);
  font-size: 11.5px;
  line-height: 1.45;
  white-space: nowrap;
  pointer-events: none;
  box-shadow: 0 4px 14px rgba(0, 0, 0, 0.22);
}
/* 翻到上方时改成向上偏移，别盖住 canvas 本身。 */
.hb-tip.flip {
  transform: translate(-50%, calc(-100% - 8px));
}
</style>
