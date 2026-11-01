import { useMemo } from 'react'

/**
 * 迷你柱状图：一组「日期 → 数值」的直方图，用 CSS 高度画，不引入图表库。
 *
 * 为什么不用 SVG：这里的柱数等于区间天数（最多 365 根），DOM 一根一个
 * div 完全撑得住，而 Tailwind 的语义色 token（--color-success 等）在暗色
 * 主题下会自动跟随；SVG 里要写 fill="var(...)" 反而更绕。柱高用百分比，
 * 容器定了高度就行，不需要量宽，也就没有 ResizeObserver 那一套。
 *
 * 数据为 0 与「没有数据」刻意画成一样（柱子高度 0），但 title 文案不同：
 * 看板上「那天没调用」和「那天调用数是 0」没有区别，不值得为它加分支。
 */
export function BarChart({
  label,
  points,
  format,
  toneClass = 'bg-accent',
  height = 64,
}: {
  /** 无障碍名称，如「按天请求数」。 */
  label: string
  /** 按时间**升序**排列；key 用于悬停提示与横轴首尾。 */
  points: { key: string; value: number }[]
  /** 数值的展示形式（悬停提示与读屏文案用）。 */
  format: (value: number) => string
  /** 柱子的底色工具类，默认主色。 */
  toneClass?: string
  height?: number
}) {
  const max = useMemo(() => points.reduce((m, p) => (p.value > m ? p.value : m), 0), [points])
  const first = points.length > 0 ? points[0].key : ''
  const last = points.length > 1 ? points[points.length - 1].key : ''

  if (points.length === 0) {
    return (
      <div className="text-muted flex h-16 items-center justify-center text-[12.5px]">所选区间内没有调用记录</div>
    )
  }

  return (
    <figure className="m-0">
      <figcaption className="sr-only">
        {label}：共 {points.length} 个数据点，最高 {format(max)}
      </figcaption>
      {/* role="img" + aria-label：读屏用户拿到的是「最高多少」这一句总结，
          逐根柱子的数字靠下面的「最新一天」摘要行补充，而不是念 365 个数。 */}
      <div
        role="img"
        aria-label={`${label}，${points.length} 个数据点，最高 ${format(max)}`}
        className="flex items-end gap-px"
        style={{ height }}
      >
        {points.map((p) => {
          // 有值至少给 2% 高度：0.5% 的柱子在高 DPI 屏上会被舍入成看不见，
          // 而「那天有 1 个请求」和「那天没有请求」在图上必须能分辨。
          const pct = max > 0 && p.value > 0 ? Math.max(2, (p.value / max) * 100) : 0
          return (
            <div
              key={p.key}
              title={`${p.key} · ${format(p.value)}`}
              className={`min-w-px flex-1 rounded-t-[2px] ${p.value > 0 ? toneClass : 'bg-separator'}`}
              style={{ height: `${p.value > 0 ? pct : 1.5}%` }}
            />
          )
        })}
      </div>
      {first ? (
        <div className="text-muted mt-1 flex justify-between font-mono text-[10.5px]">
          <span>{first}</span>
          {last ? <span>{last}</span> : null}
        </div>
      ) : null}
    </figure>
  )
}
