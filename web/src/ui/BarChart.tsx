import type { UsageBucket } from '../usageSeries'
import { labelStrideFor } from '../usageSeries'

/**
 * 用量趋势柱状图：一根柱子一个时间桶（日 / 周 / 月），用 CSS 高度画，不引图表库。
 *
 * 为什么不用 SVG：柱子数量等于桶数（最多约 365 根按日 / 十几个按月），DOM 一根
 * 一个 div 完全撑得住；而 Tailwind 的语义色 token 在暗色主题下会自动跟随，
 * SVG 里要写 fill="var(...)" 反而更绕。柱高用百分比，容器定了高度即可，
 * 不需要量宽，也就没有 ResizeObserver 那一套。
 *
 * 交互刻意只有「悬停看当天」和「横轴抽稀」两件事：图上要回答的是「哪天塌了」，
 * 精确读数由调用日志页按天筛选承担。
 */
export function BarChart({
  label,
  buckets,
  formatTokens,
  formatCount,
  height = 150,
}: {
  /** 无障碍名称，如「近 7 天用量」。 */
  label: string
  /** 按时间**升序**排列（最早在左）。 */
  buckets: UsageBucket[]
  formatTokens: (tokens: number) => string
  formatCount: (count: number) => string
  height?: number
}) {
  if (buckets.length === 0) {
    return (
      <div className="text-muted flex items-center justify-center py-12 text-[12.5px]">所选区间内没有调用记录</div>
    )
  }

  // 分母至少为 1：全零时不能除以 0，也不能让「全零」变成「最高值 0」的负高度。
  const max = buckets.reduce((m, b) => (b.tokens > m ? b.tokens : m), 0)
  const denom = max > 0 ? max : 1
  // 桶多时改用窄标签：此时每列已窄于完整标签（09-30），继续用会互相压叠。
  const dense = buckets.length > 20
  const stride = labelStrideFor(buckets.length)

  return (
    <figure className="m-0">
      <figcaption className="sr-only">
        {label}：共 {buckets.length} 个时间桶，最高 {formatTokens(max)}
      </figcaption>
      {/* 逐柱的数字不念给读屏（365 个数没法听），只在 aria-label 给总结。 */}
      <div role="img" aria-label={`${label}，${buckets.length} 个时间桶，最高 ${formatTokens(max)}`}>
        <div className="flex items-end gap-1.5" style={{ height }}>
          {buckets.map((b) => {
            // 有值至少 2%：0.5% 的柱子在真机高 DPI 下会被舍入成看不见，
            // 「那天有 1 次调用」与「那天没有」就分不清了。
            const pct = Math.max(2, (b.tokens / denom) * 100)
            const tip = `${b.tip} · ${formatCount(b.count)} 次 · ${formatTokens(b.tokens)} tok`
            return (
              <div key={b.key} className="group relative flex h-full min-w-0 flex-1 items-end justify-center">
                <div
                  className={`w-full max-w-[34px] rounded-t-[4px] transition-[height] duration-500 ${
                    b.tokens === 0 ? 'bg-separator' : 'bg-accent'
                  }`}
                  style={{ height: `${pct}%` }}
                />
                {/* 悬停提示：v1.5.3 用 CSS ::after + data-tip，这里用 group-hover
                    渲染同一份文案。绝对定位不参与布局，所以不改变柱宽。 */}
                <span className="bg-overlay text-overlay-foreground border-separator pointer-events-none absolute bottom-full left-1/2 z-10 mb-1.5 hidden -translate-x-1/2 rounded-lg border px-2 py-1 font-mono text-[11px] whitespace-nowrap group-hover:block">
                  {tip}
                </span>
              </div>
            )
          })}
        </div>
        {/* 横轴：抽稀到约 12 个标签。dim 的标签保留占位（visibility:hidden），
            否则 flex 宽度会被抽稀后的标签重新分配，柱子与标签就错位了。 */}
        <div className="mt-2 flex gap-1.5">
          {buckets.map((b, i) => (
            <span
              key={b.key}
              className={`min-w-0 flex-1 text-center font-mono text-[10.5px] whitespace-nowrap ${
                i % stride === 0 ? 'text-muted' : 'invisible'
              }`}
            >
              {dense ? b.labShort : b.labFull}
            </span>
          ))}
        </div>
      </div>
    </figure>
  )
}
