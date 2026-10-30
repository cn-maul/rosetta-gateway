import type { ReactNode } from 'react'
import { Alert, Button, EmptyState, Spinner } from '@heroui/react'

/**
 * 列表页统一外壳（语义对齐旧 DataTable.vue）：工具条 + 加载态 + 错误态 +
 * 空态 + 表格本体。表格的列语义与行内操作由页面自己给（children 传
 * HeroUI <Table>），这里只负责「一段数据区的四种状态」不该各页各写一遍。
 *
 * 与旧版的差异：错误后保留已加载数据继续展示（hasData），错误条内给重试。
 */
interface DataTableProps {
  /** 可访问名：表格 aria-label 与横向滚动提示共用。 */
  label: string
  loading?: boolean
  error?: string
  /** 已有数据时错误不整页替换，只加一条告警。 */
  hasData?: boolean
  empty?: boolean
  emptyText?: string
  /** 空态文案下方的动作区（如「清除筛选」）。仅空态（非错误/加载）时渲染。 */
  emptyAction?: ReactNode
  onRetry?: () => void
  toolbar?: ReactNode
  footer?: ReactNode
  children: ReactNode
}

export function DataTable({
  label,
  loading = false,
  error,
  hasData = false,
  empty = false,
  emptyText = '暂无数据',
  emptyAction,
  onRetry,
  toolbar,
  footer,
  children,
}: DataTableProps) {
  return (
    <section aria-busy={loading || undefined}>
      {toolbar ? <div className="mb-4">{toolbar}</div> : null}

      {error && (
        <Alert status="danger" className="mb-3">
          <Alert.Content>
            <Alert.Title>
              {error}
              {hasData ? ' · 保留上次成功结果' : ''}
            </Alert.Title>
          </Alert.Content>
          {onRetry && (
            <Button size="sm" variant="tertiary" isDisabled={loading} onPress={onRetry}>
              重试
            </Button>
          )}
        </Alert>
      )}

      {loading && !hasData ? (
        <div className="text-muted flex items-center justify-center gap-2 py-10" role="status">
          <Spinner size="sm" /> 加载中…
        </div>
      ) : empty && !error ? (
        <EmptyState>
          {emptyText}
          {emptyAction ? <div className="mt-4">{emptyAction}</div> : null}
        </EmptyState>
      ) : (
        <>
          {loading && hasData && (
            <div className="text-muted px-1 pb-1 text-xs" role="status">
              正在刷新…
            </div>
          )}
          {children}
          {footer}
        </>
      )}
    </section>
  )
}
