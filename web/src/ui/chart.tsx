import type { ReactNode } from 'react'

/**
 * Recharts 的全站主题封装。
 *
 * 图表不直接写十六进制色：HeroUI 的语义令牌会随 data-theme 变化，但 SVG 的
 * fill / stroke 不会读取 Tailwind 的 class 变量。这里统一走 CSS 变量，暗色
 * 主题下自动得到同一套青绿主色与弱化网格，不再出现「页面是暗的、图表还是
 * 白底黑字」的半套主题。
 */
export const CHART_COLORS = {
  accent: 'var(--accent)',
  accentSoft: 'color-mix(in oklab, var(--accent) 18%, transparent)',
  success: 'var(--success)',
  warning: 'var(--warning)',
  danger: 'var(--danger)',
  /** 金额固定用暖金色，与「警告」区分。 */
  cost: 'var(--cost)',
  /** 速度 / 延迟固定用蓝色，与 Token 主色区分。 */
  info: 'var(--info)',
  muted: 'var(--muted)',
  grid: 'var(--separator)',
  surface: 'var(--surface)',
  foreground: 'var(--foreground)',
} as const

export function ChartTooltipBox({ title, children }: { title: ReactNode; children: ReactNode }) {
  return (
    <div className="bg-overlay text-overlay-foreground border-separator min-w-40 rounded-lg border px-3 py-2 text-[11.5px] shadow-lg">
      <div className="text-muted mb-1.5 font-mono text-[10.5px]">{title}</div>
      <div className="flex flex-col gap-1">{children}</div>
    </div>
  )
}

export function TooltipRow({
  color,
  label,
  value,
}: {
  color?: string
  label: string
  value: ReactNode
}) {
  return (
    <div className="flex items-center justify-between gap-5">
      <span className="text-muted flex items-center gap-1.5">
        {color ? (
          <span className="h-1.5 w-1.5 rounded-full" style={{ background: color }} aria-hidden />
        ) : null}
        {label}
      </span>
      <b className="tabular-nums">{value}</b>
    </div>
  )
}

export function ChartEmpty({ text = '所选区间内没有调用记录' }: { text?: string }) {
  return <div className="text-muted flex min-h-[160px] items-center justify-center py-8 text-[12.5px]">{text}</div>
}

/** 同一维度的深浅阶梯；颜色只表达排序，不再引入第二套分类色。 */
export function chartShade(color: string, index: number, from = 88, step = 8): string {
  return `color-mix(in oklab, ${color} ${Math.max(20, from - index * step)}%, transparent)`
}

export function shareText(value: number, total: number): string {
  if (total <= 0) return '—'
  const p = (value / total) * 100
  if (p > 0 && p < 0.1) return '<0.1%'
  return `${p.toFixed(p >= 10 ? 0 : 1)}%`
}
