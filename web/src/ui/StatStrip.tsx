import type { ReactNode } from 'react'

/**
 * 页面级读数带：总览七项指标、用量页的合计都走同一个外壳。
 *
 * 之前用量页把合计做成几颗胶囊、总览做成一条指标带，同一类信息有两种外观。
 * 这里固定成「一条带 + 细分隔线」：数值仍能横着比较，但不再像筛选控件。
 */
export interface StatItem {
  label: string
  value: ReactNode
  hint?: ReactNode
  tone?: 'warning' | 'danger'
}

export function StatStrip({
  items,
  loading = false,
  ariaLabel,
  className = '',
}: {
  items: StatItem[]
  loading?: boolean
  ariaLabel?: string
  className?: string
}) {
  return (
    <div
      className={
        'grid grid-cols-2 gap-px overflow-hidden rounded-2xl border border-separator bg-separator md:grid-cols-4 ' +
        className
      }
      role="group"
      aria-label={ariaLabel}
      aria-busy={loading || undefined}
    >
      {items.map((item) => (
        <div key={item.label} className="bg-surface px-3.5 py-3">
          <div className="text-muted text-[11px]">{item.label}</div>
          <div
            className={
              'mt-1 text-[17px] leading-tight font-semibold tabular-nums ' +
              (item.tone === 'warning' ? 'text-warning ' : '') +
              (item.tone === 'danger' ? 'text-danger ' : '')
            }
          >
            {item.value}
          </div>
          {item.hint ? <div className="text-muted mt-1 text-[10.5px]">{item.hint}</div> : null}
        </div>
      ))}
    </div>
  )
}
