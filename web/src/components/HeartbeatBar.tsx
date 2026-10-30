import { useLayoutEffect, useMemo, useRef, useState } from 'react'
import type { ProbeTimelineBucket } from '../types'
import { heartbeatBucketTexts, type HeartbeatBucket } from '../heartbeatText'

/**
 * 连通率条形图（语义对齐旧 HeartbeatBar.vue，2026-10-16 起替代 SVG 折线：
 * 折线把「停机 5 小时」画成一条直通 100% 的斜线；条形图每个探测桶一根
 * 胶囊，坏的直接是红的）。
 *
 * 旧版用 canvas + ResizeObserver 自绘；React 版桶数有限（后端单查询
 * ≤24 个模型 × 固定桶数），改用 DOM 胶囊 + Tailwind 工具类，hover 缩放、
 * tooltip（title）与屏幕阅读器列表都由 DOM 直接承担。桶的铺排、几何
 * 不变式与四档配色与旧版逐条对应。
 */

// 尺寸常量沿用旧版：胶囊最宽 10px（拥挤时压到 3px），拉伸上限 24px；
// pad 拉伸上限 10px。BEAT_H 是胶囊高度。
const BEAT_W_MAX = 10
const BEAT_W_STRETCH = 24
const BEAT_W_MIN = 3
const BEAT_H = 30
const PAD_MAX = 4
const PAD_STRETCH = 10
const PAD_MIN = 1

/**
 * 把稀疏桶铺成等距 beat 序列：从 end=floor(now/step)*step 向前铺
 * total=hours/bucketMinutes 根；桶按 ts 对齐（后端 UTC 整点桶），缺失补
 * { rate:null, bucket:null }。「没有探测记录」与「成功率 0」必须分开。
 */
function buildBeats(buckets: readonly ProbeTimelineBucket[], hours: number, bucketMinutes: number): HeartbeatBucket[] {
  const step = bucketMinutes * 60000
  const total = Math.max(1, Math.round((hours * 3600000) / step))
  const end = Math.floor(Date.now() / step) * step
  const byTs = new Map<number, ProbeTimelineBucket>()
  for (const b of buckets) byTs.set(Math.floor(b.ts / step) * step, b)
  const beats: HeartbeatBucket[] = []
  for (let i = total - 1; i >= 0; i--) {
    const ts = end - i * step
    const bucket = byTs.get(ts) ?? null
    beats.push({ ts, rate: bucket && bucket.count > 0 ? bucket.ok / bucket.count : null, bucket })
  }
  return beats
}

/**
 * 几何：关键不变式 beatCount × full ≤ boxWidth（full=boxWidth/beatCount）。
 * 桶少 → 拉伸（先加 pad 再放宽胶囊，上限 BEAT_W_STRETCH）；桶多 → 先压
 * 间距（PAD_MAX-1 递减到 PAD_MIN）再压胶囊宽（下限 BEAT_W_MIN）。
 */
function beatGeom(n: number, boxWidth: number): { beatCount: number; pad: number; beatW: number } {
  const slots = Math.floor(boxWidth / (BEAT_W_MAX + PAD_MAX * 2))
  const beatCount = Math.max(1, Math.min(n, slots))
  const full = boxWidth / beatCount
  if (n <= beatCount) {
    const pad = Math.min(PAD_STRETCH, Math.max(PAD_MIN, (full - BEAT_W_MAX) / 2))
    return { beatCount, pad, beatW: Math.min(BEAT_W_STRETCH, full - pad * 2) }
  }
  let pad = PAD_MAX - 1
  while (pad > PAD_MIN && beatCount * (BEAT_W_MIN + pad * 2) > boxWidth) pad -= 1
  return { beatCount, pad, beatW: Math.max(BEAT_W_MIN, full - PAD_MIN * 2) }
}

/** 四档配色：无记录 / 全成功 / 多数成功 / 其余。走语义 token，暗色自动跟随。 */
function beatColor(beat: HeartbeatBucket): string {
  if (beat.rate === null) return 'var(--hb-empty)'
  if (beat.rate >= 1) return 'var(--color-success)'
  if (beat.rate >= 0.5) return 'var(--color-warning)'
  return 'var(--color-danger)'
}

export function HeartbeatBar({
  buckets,
  hours,
  bucketMinutes = 60,
}: {
  buckets: readonly ProbeTimelineBucket[]
  hours: number
  bucketMinutes?: number
}) {
  const boxRef = useRef<HTMLDivElement>(null)
  const [boxWidth, setBoxWidth] = useState(0)
  useLayoutEffect(() => {
    const el = boxRef.current
    if (!el) return
    const ro = new ResizeObserver((entries) => {
      for (const e of entries) setBoxWidth(e.contentRect.width)
    })
    ro.observe(el)
    return () => ro.disconnect()
  }, [])

  const beats = useMemo(() => buildBeats(buckets, hours, bucketMinutes), [buckets, hours, bucketMinutes])
  // 跨度与桶宽都必须跟着 bucketMinutes 走：旧版曾把跨度写成一次性 const、
  // 桶宽写死 60 分钟，切换档位后整张图是错的 —— 这里由调用方传同一来源。
  const geom = boxWidth > 0 ? beatGeom(beats.length, boxWidth) : null
  const visible = useMemo(() => (geom ? beats.slice(beats.length - geom.beatCount) : []), [beats, geom])

  const fmtTs = (ts: number) => {
    const d = new Date(ts)
    const day = `${d.getMonth() + 1}-${d.getDate()}`
    return bucketMinutes === 1440 ? day : `${day} ${String(d.getHours()).padStart(2, '0')}:00`
  }
  const texts = useMemo(() => heartbeatBucketTexts(visible, fmtTs), [visible, bucketMinutes]) // eslint-disable-line react-hooks/exhaustive-deps

  const spanMin = geom ? geom.beatCount * bucketMinutes : 0
  const totalLabel = spanMin >= 1440 ? `${Math.round(spanMin / 1440)} 天前` : `${Math.round(spanMin / 60)} 小时前`
  const withData = visible.filter((b) => b.bucket).length
  const dropped = beats.length - visible.length
  const ariaText =
    `连通率：显示 ${visible.length} 个时段、${withData} 个有记录` +
    (dropped > 0 ? `、更早 ${dropped} 个时段因宽度不足未显示` : '')

  return (
    <div ref={boxRef} aria-label={ariaText}>
      <div className="flex items-end" style={{ height: BEAT_H, gap: geom?.pad ?? 0 }}>
        {visible.map((beat, i) => (
          <div
            key={beat.ts}
            title={texts[i]}
            className={beat.bucket ? 'transition-transform hover:scale-150' : undefined}
            style={{
              flex: 'none',
              width: Math.max(1, Math.round(geom?.beatW ?? 0)),
              height: BEAT_H,
              borderRadius: 3,
              backgroundColor: beatColor(beat),
            }}
          />
        ))}
      </div>
      <div className="mt-1 flex justify-between text-[10px] text-muted">
        <span>{totalLabel}</span>
        <span>现在</span>
      </div>
      <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-[10px] text-muted">
        <LegendDot color="var(--color-success)" label="正常" />
        <LegendDot color="var(--color-warning)" label="部分失败" />
        <LegendDot color="var(--color-danger)" label="失败" />
        <LegendDot color="var(--hb-empty)" label="未探测" />
      </div>
      {/* 键盘/屏幕阅读器等价物：旧版是 canvas + 方向键漫游焦点；DOM 版直接
          给出与 tooltip 同文案的隐藏列表，读屏器可用自己的键逐项浏览。 */}
      <ul className="sr-only">
        {texts.map((t, i) => (
          <li key={i}>{t}</li>
        ))}
      </ul>
    </div>
  )
}

function LegendDot({ color, label }: { color: string; label: string }) {
  return (
    <span className="inline-flex items-center gap-1">
      <span aria-hidden="true" className="inline-block h-2 w-2 rounded-sm" style={{ backgroundColor: color }} />
      {label}
    </span>
  )
}
