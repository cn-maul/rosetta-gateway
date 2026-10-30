import type { ReactNode } from 'react'

// 页面标题区：h1 + 副标题 + 右侧动作列（旧 .page-head 语义）。
export function PageHeader({ title, sub, actions }: { title: string; sub?: ReactNode; actions?: ReactNode }) {
  return (
    <header className="mb-3.5 flex items-start justify-between gap-3.5">
      <div>
        <h1 className="text-2xl font-semibold">{title}</h1>
        {sub ? <div className="text-muted mt-1 text-[12.5px]">{sub}</div> : null}
      </div>
      {actions ? <div className="flex flex-wrap items-center gap-2">{actions}</div> : null}
    </header>
  )
}
