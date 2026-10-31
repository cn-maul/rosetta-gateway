import { ProgressCircle } from '@heroui/react'
import { fmtPercent } from '../fmt'

/**
 * 比例圆环（P7）：成功率、就绪率这类 0–1 比例的图形化。色阶随阈值——
 * ≥0.9 正常、≥0.7 警示、否则危险；ratio<0（无样本）画空环。中心放百分比。
 */
export function RateRing({
  ratio,
  size = 'md',
  label,
  className = '',
}: {
  /** 0–1；<0 表示无样本（画空环） */
  ratio: number
  size?: 'sm' | 'md' | 'lg'
  label: string
  className?: string
}) {
  const valid = ratio >= 0
  const color = !valid || ratio >= 0.9 ? 'text-success' : ratio >= 0.7 ? 'text-warning' : 'text-danger'
  return (
    <div className={`relative inline-flex items-center justify-center ${className}`}>
      <ProgressCircle value={valid ? Math.min(100, Math.max(0, ratio * 100)) : 0} size={size} aria-label={label}>
        <ProgressCircle.Track>
          <ProgressCircle.TrackCircle />
          <ProgressCircle.FillCircle className={color} />
        </ProgressCircle.Track>
      </ProgressCircle>
      <span className={`absolute text-[10px] font-semibold tabular-nums ${valid ? color : 'text-muted'}`}>
        {valid ? fmtPercent(ratio) : '—'}
      </span>
    </div>
  )
}
