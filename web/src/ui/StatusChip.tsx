import type { ReactNode } from 'react'
import { Chip } from '@heroui/react'

// 状态 → 语义变体的唯一映射（替代散落各页的 .badge / .badge-warn / .dim）。
// 调用点只声明业务状态，不挑颜色。
const TONE_VARIANT = {
  ok: 'success',
  warn: 'warning',
  err: 'danger',
  info: 'accent',
  dim: 'default',
} as const

export type StatusTone = keyof typeof TONE_VARIANT

export function StatusChip({
  tone,
  size = 'sm',
  children,
}: {
  tone: StatusTone
  size?: 'sm' | 'md' | 'lg'
  children: ReactNode
}) {
  return (
    <Chip color={TONE_VARIANT[tone]} size={size}>
      {children}
    </Chip>
  )
}
