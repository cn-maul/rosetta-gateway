import type { ReactNode } from 'react'
import { Card } from '@heroui/react'

// 带标题的内容块（替代旧 .panel / .card 组合）。
interface SectionCardProps {
  title?: ReactNode
  description?: ReactNode
  actions?: ReactNode
  children: ReactNode
}

export function SectionCard({ title, description, actions, children }: SectionCardProps) {
  const hasHead = title !== undefined || actions !== undefined
  return (
    <Card>
      {hasHead ? (
        <Card.Header>
          <div className="flex w-full items-start justify-between gap-3">
            <div>
              {title ? <Card.Title>{title}</Card.Title> : null}
              {description ? <Card.Description>{description}</Card.Description> : null}
            </div>
            {actions ? <div className="flex flex-wrap items-center gap-2">{actions}</div> : null}
          </div>
        </Card.Header>
      ) : null}
      <Card.Content>{children}</Card.Content>
    </Card>
  )
}
