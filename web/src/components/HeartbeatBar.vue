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
import { heartbeatBucketTexts } from '../heartbeatText'

const props = defineProps<{
  /** 探测桶（稀疏：无记录的桶不在其中），由调用方填满时间轴。 */
  buckets: ProbeTimelineBucket[]
  /** 跨度（小时）。用于把稀疏桶铺成连续序列，缺口渲染成「无记录」。 */
  hours: number
  /** 每个桶代表多少分钟。默认 60 = 按小时聚合。 */
  bucketMinutes?: number
}>()

// ---------- 尺寸常量 ----------
//
// 条宽**随条数自适应**，不写死 10px：168 根按 10px+4px 间距放不进一张
// ~290px 的模型卡（右侧最近的时段会被裁掉，而那恰好最该看见），只有
// 7 根时又该宽松些。所以给区间，由 beatGeom 按容器宽度与桶数取值。
const BEAT_W_MAX = 10
// 桶少时拉伸的上限：两根就占满一行时条宽到 24px 就成了色块，
// 再宽只是把同一段颜色涂大，不增加信息量。
const BEAT_W_STRETCH = 24
const BEAT_W_MIN = 3
const BEAT_H = 30
const HOVER_SCALE = 1.5
// 间距跟着条宽一起缩，否则条一多就只剩条宽的缝，看起来是块实心色带。
const PAD_MAX = 4
// 拉伸时的间距上限（见 beatGeom）。
const PAD_STRETCH = 10
const PAD_MIN = 1

const wrap = ref<HTMLDivElement | null>(null)
const canvas = ref<HTMLCanvasElement | null>(null)

// boxWidth 是容器量到的宽度，是**唯一的尺寸输入**。条数与单根排版都由
// 它推导（beatCount / beatGeom），而 canvas 走绝对定位、不参与布局 ——
// 于是不存在「量宽度 → 改条数 → canvas 变窄 → 容器变窄 → 再量」的回路，
// 下面的 ResizeObserver 也就不可能自我触发。
const boxWidth = ref(0)
const hovered = ref(-1)
const focused = ref(-1)

interface Beat {
  /** null = 该桶无探测记录（不是 0%，必须与失败区分）。 */
  rate: number | null
  bucket: ProbeTimelineBucket | null
  /**
   * 该桶的起始时刻（毫秒），**无记录时也有值**。
   *
   * 为什么不能从 bucket.ts 取：稀疏桶里「没有记录」的那些格子
   * bucket 根本是 null，于是它们的时刻无从得知 —— tooltip 只能说
   * 「没有探测记录」却说不出是**哪一格**没有，悬停一整排灰条时
   * 每根给的提示都一样，等于没提示。
   */
  ts: number
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
    out.push(b ? { rate: b.ok / b.count, bucket: b, ts } : { rate: null, bucket: null, ts })
  }
  return out.slice(-total)
})

/**
 * beatCount 这一屏画几根。
 *
 * 取「能放下的最大根数」与「实际有的桶数」的小者 —— 条多了只画最近
 * N 根（旧的挤掉），条少了就只画有的那些。
 */
const beatCount = computed(() => {
  const capacity = Math.floor(Math.max(1, boxWidth.value) / (BEAT_W_MAX + PAD_MAX * 2))
  return Math.max(1, Math.min(beats.value.length, capacity))
})

/**
 * beatGeom 单根条的排版尺寸（条宽 + 间距）。
 *
 * 关键不变式：**beatCount × full ≤ boxWidth**。n 与 full 由同一个
 * boxWidth 推导，所以不可能失配 —— 此前是「measure 量宽度、draw 另算
 * 宽度」两处独立算，必然分叉（canvas 画得比容器宽，右侧最近的时段被
 * overflow 裁掉，恰好是最该看见的一段）。
 *
 * 什么时候压尺寸：只有当**桶数超过容量**、不得不缩到不足理想宽度时才压。
 * 策略是先压间距再压条宽 —— 挤一挤通常还能放下；压到 BEAT_W_MIN 仍不行
 * 就认了（宁可少画几根），再压就成了实心色带，失去「一段时间一根」的
 * 可读性。
 *
 * 什么时候拉伸：桶数**少于容量**时，把间距放大到恰好铺满整行 ——
 * 否则「168 小时前 …… 现在」这两个标注之间会空出一大段，而「现在」
 * 那一端并没有贴着最后一条，看着像数据没画满。
 */
const beatGeom = computed(() => {
  const w = Math.max(1, boxWidth.value)
  const n = beatCount.value
  // 铺满整行：full 取整数除不尽时的余数给最后一根，误差不累积。
  const full = w / n

  // 桶少 → 拉伸。间距上限放宽到 PAD_STRETCH，否则只有两三根时会挤在左边。
  if (beats.value.length <= Math.floor(w / (BEAT_W_MAX + PAD_MAX * 2))) {
    const pad = Math.min(PAD_STRETCH, Math.max(PAD_MIN, (full - BEAT_W_MAX) / 2))
    return { beatW: Math.min(BEAT_W_STRETCH, full - pad * 2), pad, full }
  }

  // 桶多 → 先压间距。
  for (let pad = PAD_MAX - 1; pad >= PAD_MIN; pad--) {
    if (BEAT_W_MAX + pad * 2 <= full) return { beatW: BEAT_W_MAX, pad, full }
  }
  // 再压条宽，但守住 MIN。
  const beatW = Math.max(BEAT_W_MIN, full - PAD_MIN * 2)
  return { beatW, pad: PAD_MIN, full }
})

// 可见条数：最近 beatCount 根，条形按 beatGeom 铺满整行宽度。
//
// 「左对齐铺满」是刻意的：左右两端各有一个「多久前 / 现在」标注，若只把
// 条排在左边，右端标注就会指着一段空白，读起来像「这段时间没数据」。
const visible = computed<{ beat: Beat; index: number }[]>(() => {
  const n = beatCount.value
  const start = Math.max(0, beats.value.length - n)
  return beats.value.slice(start).map((beat, i) => ({ beat, index: start + i }))
})

const canvasW = computed(() => Math.max(1, boxWidth.value))
const canvasH = computed(() => BEAT_H * HOVER_SCALE)

/**
 * totalLabel 是左端那根标注的文字（「168 小时前」/「7 天前」）。
 *
 * 单位由**桶宽**决定而不是固定小时：调用方可能按天分桶（bucketMinutes
 * = 1440），那时逐根数小时没有意义，跟页面上的「近 7 天」对不上。
 */
const totalLabel = computed(() => {
  const stepMin = props.bucketMinutes ?? 60
  // 用**画出来的**根数而不是 hours 算跨度：桶多于容量时只画最近 N 根
  // （见 beatCount），左端标注若仍按 hours 报「168 小时前」，它指着的
  // 那一格压根没画出来 —— 标注与图不一致比标注缺失更糟。
  const n = beatCount.value
  const spanMin = n * stepMin
  if (stepMin % 1440 === 0 || spanMin >= 1440) {
    const d = spanMin / 1440
    return `${Number.isInteger(d) ? d : d.toFixed(1)} 天前`
  }
  const h = spanMin / 60
  return `${Number.isInteger(h) ? h : h.toFixed(1)} 小时前`
})

// ---------- 绘制 ----------

/**
 * ariaText 是给读屏软件的一句话描述。
 *
 * 三件事必须都说出来：图上画了几个时段、其中几个有数据、以及**有没有被
 * 截断**。少了截断信息，读屏用户听到「3 个探测窗」会以为这段时间就只
 * 探测过 3 次，而实际上是窗口太窄只画得出 3 根。
 */
const ariaText = computed(() => {
  const shown = visible.value.length
  const withData = visible.value.filter((v) => v.beat.rate !== null).length
  const dropped = beats.value.length - shown
  const head = `连通率：显示 ${shown} 个时段，其中 ${withData} 个有探测记录`
  return dropped > 0 ? `${head}（更早的 ${dropped} 个时段因宽度不足未显示）` : head
})

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
  const { beatW, pad, full } = beatGeom.value

  visible.value.forEach(({ beat }, i) => {
    const x = i * full + pad
    const isHover = i === hovered.value && beat.rate !== null

    let w = beatW
    let hh = BEAT_H
    let ox = x
    if (isHover) {
      w *= HOVER_SCALE
      hh *= HOVER_SCALE
      ox = x - (w - beatW) / 2
    }
    const oy = centerY - hh / 2
    // 胶囊形：圆角取宽度的一半（上游同法）。
    const r = w / 2

    // 四种状态一视同仁：不额外降透明度。此前给「未探测」单独设 0.55
    // 的 alpha，让它在图上比其余三条淡一档，看起来像「禁用/灰掉」而不是
    // 「一个真实的状态」。区分四态靠**颜色**就够了 —— 而且这个透明度差异
    // 让图例里那个实心色块和图上的半透明条对不上。现在图例与图完全一致。
    ctx.fillStyle = beatColor(beat.rate, colors)
    roundRect(ctx, ox, oy, w, hh, r)
    ctx.fill()
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
  // 无记录的桶也要报出**它自己那一格**的时刻：整条时间轴里大部分格子
  // 可能都没记录，若全都显示 1970/01/01 反而误导。
  if (!beat.bucket) return `${fmtTs(beat.ts)} · 没有探测记录`
  const pct = Math.round((beat.rate ?? 0) * 100)
  return `${fmtTs(beat.bucket.ts)} · 成功率 ${pct}% (${beat.bucket.ok}/${beat.bucket.count}) · 平均 ${beat.bucket.latency}ms`
}

const bucketTexts = computed(() => heartbeatBucketTexts(visible.value.map((v) => v.beat), fmtTs))

function onBucketKey(e: KeyboardEvent, index: number) {
  const n = bucketTexts.value.length
  if (n === 0) return
  let next = index
  if (e.key === 'ArrowRight' || e.key === 'ArrowDown') next = (index + 1) % n
  else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') next = (index - 1 + n) % n
  else if (e.key === 'Home') next = 0
  else if (e.key === 'End') next = n - 1
  else if (e.key === 'Escape') {
    focused.value = -1
    return
  } else return
  e.preventDefault()
  focused.value = next
  const nodes = e.currentTarget instanceof HTMLElement
    ? e.currentTarget.parentElement?.querySelectorAll<HTMLElement>('[data-bucket-index]')
    : undefined
  nodes?.[next]?.focus()
}

function onMove(e: MouseEvent) {
  const c = canvas.value
  if (!c) return
  const rect = c.getBoundingClientRect()
  // 用 beatGeom.full 而非写死值：条宽会随卡片宽度变（见 beatGeom），
  // 这里若还用旧常量，鼠标位置算出的下标会与绘制时错位一格。
  const { full } = beatGeom.value
  const i = Math.floor((e.clientX - rect.left) / full)
  const hit = visible.value[i]
  if (!hit) {
    hovered.value = -1
    tip.value.show = false
    return
  }
  // 无记录的桶**也允许悬停**（原先直接跳过）：它们的灰条正是「这段时间
  // 没探测过」这个信息本身，且现在 tooltip 能报出是哪一格 —— 用户要看的
  // 恰恰是「什么时候开始没有记录的」这个边界。绘制时 hover 放大仍只对
  // 有数据的格生效（beat.rate === null 的不放大），避免让人以为灰条
  // 也代表某个成功率。
  if (hovered.value !== i && hit.beat.rate !== null) hovered.value = i
  else if (hit.beat.rate === null) hovered.value = -1
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
  // **只量不写布局**：这里写的是 boxWidth，条数由 beatGeom/visible 推导，
  // 而 canvas 的像素尺寸走 style.width —— canvas 在 .hb-bar 里是
  // block 级内联流元素，它变窄会让容器也变窄，下一次 ResizeObserver
  // 再量到更小的值，于是每帧少几根条，最终塌成一根（此前的真 bug）。
  // 现在 canvas 从布局里拿掉（绝对定位，见样式），容器宽度恒等于卡片宽度，
  // 这条自反馈通路消失。
  const w = wrap.value.clientWidth
  // 宽度没变就别写 ref：ResizeObserver 回调里写同样的值仍可能触发一轮
  // 无谓的重绘，而我们现在每次写值都会引发一次 draw()。
  if (Math.abs(w - boxWidth.value) > 0.5) boxWidth.value = w
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

// 数据、悬停、**容器宽度**变化都要重画。用 watch 而不是 computed 里画 ——
// 绘制有副作用，塞进 computed 会让 Vue 的缓存语义变味。
//
// boxWidth 必须列进来：条数是由它与 beatGeom 推导的（见 visible），
// 只 watch visible 的话，首次 measure() 之后宽度再变，visible 虽会重算，
// 但 deep 比较的是数组引用 —— 保险起见把三个源头都列上。
watch(
  () => [visible.value, hovered.value, boxWidth.value] as const,
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
        :aria-label="ariaText"
        role="img"
        @mousemove="onMove"
        @mouseleave="onLeave"
      />
    </div>
    <!-- 键盘与读屏的等价视图：每根条都是可聚焦列表项，左右/Home/End
         在桶之间移动。它同时承担 hover tooltip 的文本替代，不依赖指针。 -->
    <div class="hb-accessible" role="list" aria-label="连通率探测桶明细">
      <div
        v-for="(text, i) in bucketTexts"
        :key="visible[i]?.beat.ts"
        class="hb-bucket"
        role="listitem"
        data-bucket-index
        :tabindex="i === focused ? 0 : -1"
        :aria-label="text"
        @focus="focused = i"
        @keydown="onBucketKey($event, i)"
      >
        {{ text }}
      </div>
    </div>
    <!-- 左右两端的时间标注：只有一根根条时，「哪边是现在」是读不出来的。
     单位跟着 bucketMinutes 走：按天分桶时写「7 天前」而不是「168 小时前」
     —— 后者要心算一次才知道跨度，且与页面上的「近 7 天」选择器对不上。 -->
    <div class="hb-scale">
      <span>{{ totalLabel }}</span>
      <span>现在</span>
    </div>
    <!-- 图例：四色各有确切含义，不写出来就只能靠猜 —— 而「灰」到底是
         「没探测」还是「0%」这个歧义，正是这套图最容易被读错的地方。
         每项用与 canvas 相同的取色方式画小方块，保证与条的颜色一致。 -->
    <div class="hb-legend">
      <span class="hb-lg"><i class="hb-dot up" />正常</span>
      <span class="hb-lg"><i class="hb-dot warn" />部分失败</span>
      <span class="hb-lg"><i class="hb-dot down" />失败</span>
      <span class="hb-lg"><i class="hb-dot none" />未探测</span>
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
  /* 「无记录」色定义在 wrap 而非 .hb-bar：图例的色块是 .hb-bar 的**兄弟**
     元素上的后代，挂在 .hb-bar 上取不到，图例会退回兜底色、与条形不符。 */
  --hb-empty: oklch(0.78 0.004 286);
}
.hb-bar {
  /* 容器宽度**只由卡片决定**，不参与子元素的反向撑开。
     position:relative + canvas 绝对定位：canvas 的 style.width 是
     JS 算出来的像素值，若留在正常流里，它一变窄容器就跟着变窄，
     ResizeObserver 再量一次 —— 于是条数每帧掉几根，最后塌成一根。
     绝对定位把 canvas 从布局里摘出去，这条自反馈通路就不存在了。

     代价：canvas 不再贡献高度，所以这里必须显式给一个 —— 数值与
     canvasH（BEAT_H × HOVER_SCALE）一致，多出来的那截正好留给悬停放大，
     否则放大的那根会被裁掉半截。 */
  position: relative;
  width: 100%;
  height: calc(30px * 1.5);
}
.hb-canvas {
  display: block;
  /* 左对齐：最新（现在）在最右，左边是历史，与 .hb-scale 的两端标注对齐。 */
  position: absolute;
  inset-inline-start: 0;
  top: 0;
}
/* Visually hidden but focusable: the rendered bars remain the visual contract,
   while every interval stays reachable by keyboard and screen readers. */
.hb-accessible {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip: rect(0, 0, 0, 0);
  white-space: nowrap;
  border: 0;
}
.hb-bucket:focus {
  outline: 2px solid var(--accent);
  outline-offset: 2px;
}
[data-theme='dark'] .hb-wrap {
  /* 暗色下「无记录」要能看出是色块而不是破洞。 */
  --hb-empty: oklch(0.34 0.008 286);
}

.hb-scale {
  display: flex;
  justify-content: space-between;
  margin-top: 6px;
  font-size: 11.5px;
  color: var(--muted);
}
/* 图例：横排、色块在前文字在后，与 canvas 的绘制顺序一致。 */
.hb-legend {
  display: flex;
  flex-wrap: wrap;
  gap: 10px;
  margin-top: 5px;
  font-size: 11px;
  color: var(--text-4);
}
.hb-lg {
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
.hb-dot {
  width: 7px;
  height: 7px;
  /* padding 必须显式清零，且这里不能只靠全局 `* { padding: 0 }`：
     全局那条是 (0,0,0) 特异性，任何一条单类规则都能盖掉它。
     这个类名之前叫 `.empty`，与 styles.css 的空状态 `.empty`
     （padding: 34px 0）撞了 —— 结果 7px 的圆点被撑成上下各 34px
     的竖长椭圆，而同组的 up/warn/down 因为不带这半个类名所以正常。
     类名已改成 `none`，这里再补 padding: 0 兜底：万一将来又和别的
     空状态类重名，形状也不会再被悄悄改掉。 */
  padding: 0;
  flex: none;
  border-radius: 50%;
  display: inline-block;
}
/* 色值与 draw() 里 pick() 读的是同一组变量，改一处必须改另一处 ——
   图例与条形不同色会让图例变成误导。 */
.hb-dot.up { background: var(--success); }
.hb-dot.warn { background: var(--warning); }
.hb-dot.down { background: var(--danger); }
/* 「未探测」用中性灰而不是条形的亮灰（--hb-empty）：在 --surface 底上
   亮灰几乎看不见，图例上就成了「没有这一项」而不是「一个和别的一样
   的小点」。仍然不能染红 —— 那会被读成「故障」。 */
.hb-dot.none { background: var(--default-foreground); opacity: 0.45; }
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
