/**
 * 手绘 SVG 迷你趋势线（P7 图形优先；零新增依赖，与 HeartbeatBar 同思路）。
 * 只画一条折线，颜色继承文字色（外层给 text-accent 等即可）。少于 2 个点
 * 画不出趋势，直接不渲染。
 */
export function Sparkline({
  values,
  className = 'text-accent',
  width = 96,
  height = 28,
  title,
}: {
  values: number[]
  className?: string
  width?: number
  height?: number
  title?: string
}) {
  if (values.length < 2) return null
  const max = Math.max(...values, 1)
  const step = width / (values.length - 1)
  const points = values
    .map((v, i) => `${(i * step).toFixed(1)},${(height - 2 - (v / max) * (height - 4)).toFixed(1)}`)
    .join(' ')
  return (
    <svg
      viewBox={`0 0 ${width} ${height}`}
      width={width}
      height={height}
      className={className}
      role="img"
      aria-label={title ?? '趋势'}
    >
      <polyline
        points={points}
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinejoin="round"
        strokeLinecap="round"
      />
    </svg>
  )
}
