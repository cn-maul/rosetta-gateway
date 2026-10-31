import { useCallback, useEffect, useRef, useState } from 'react'
import { Button } from '@heroui/react'
import { Link } from 'react-router-dom'
import { api } from '../api'
import { RequestGeneration } from '../asyncState'
import { fmtSec, fmtTimeMs, statusLabel } from '../fmt'
import type { UsageHistoryEntry } from '../types'
import { statusTone, totalTimeTone, upstreamName, UsageDetailDrawer } from './UsageDetailDrawer'
import { StatusChip } from '../ui/StatusChip'
import { SkeletonBlock } from '../ui/SkeletonBlock'

/**
 * 首屏内嵌的「最近请求」表（P1 登录即答案 / P2 一屏一问）：
 * 管理员看最近的失败请求（不用再去日志页筛 status），普通用户看最近的
 * 调用。只拉今天的一小页；行点击打开与请求日志共用的详情抽屉。
 */
export function RecentRequests({ admin }: { admin: boolean }) {
  const genRef = useRef(new RequestGeneration())
  const [rows, setRows] = useState<UsageHistoryEntry[]>([])
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [selected, setSelected] = useState<UsageHistoryEntry | null>(null)

  const load = useCallback(async () => {
    const generation = genRef.current.begin()
    setLoading(true)
    setErr('')
    try {
      const page = await api.usageHistory(1, admin ? 8 : 10, 0, admin ? { status: 'error' } : undefined)
      if (!genRef.current.isCurrent(generation)) return
      setRows(page.records)
    } catch (e) {
      if (genRef.current.isCurrent(generation) && (e as { status?: number }).status !== 401) {
        setErr('加载失败：' + (e as Error).message)
      }
    } finally {
      if (genRef.current.isCurrent(generation)) setLoading(false)
    }
  }, [admin])

  useEffect(() => {
    void load()
  }, [load])

  return (
    <section aria-label={admin ? '最近异常请求' : '最近调用'}>
      <div className="mb-3 flex flex-wrap items-end justify-between gap-4">
        <div>
          <h2 className="text-[17px] font-semibold tracking-tight">{admin ? '最近异常请求' : '最近调用'}</h2>
          <p className="mt-1 text-xs text-muted">
            {admin ? '今天 0 点以来失败的调用 —— 正常请求不进这张表' : '今天 0 点以来最近 ' + 10 + ' 条调用'}
          </p>
        </div>
        <Link
          to={admin ? '/history?status=error' : '/history'}
          className="text-sm text-accent hover:underline"
        >
          {admin ? '查看全部失败' : '查看全部调用'} →
        </Link>
      </div>

      {err ? (
        <div className="flex items-center gap-3 rounded-lg border border-warning/30 bg-warning-soft px-4 py-2.5 text-sm" role="alert">
          <span>{err}</span>
          <Button variant="tertiary" size="sm" onPress={() => void load()}>
            重试
          </Button>
        </div>
      ) : loading && rows.length === 0 ? (
        <div className="divide-y divide-separator rounded-2xl border border-separator bg-background-secondary" role="status">
          {Array.from({ length: 4 }, (_, r) => (
            <div key={r} className="flex items-center gap-4 px-4 py-3.5">
              <SkeletonBlock className="h-3.5 w-[130px]" />
              <SkeletonBlock className="h-3.5 flex-1" />
              <SkeletonBlock className="h-3.5 w-[90px]" />
            </div>
          ))}
        </div>
      ) : rows.length === 0 ? (
        <p className="rounded-xl border border-success/30 bg-success-soft px-4 py-3 text-sm">
          {admin ? '今天 0 点以来没有失败请求。整体运行正常 —— 请求明细见请求日志。' : '今天还没有调用记录。发一次请求后这里会出现明细。'}
        </p>
      ) : (
        <div className="overflow-hidden rounded-2xl border border-separator bg-background-secondary">
          <ul className="divide-y divide-separator">
            {rows.map((h) => (
              <li key={h.request_id || h.ts}>
                <Button
                  variant="ghost"
                  onPress={() => setSelected(h)}
                  className="grid w-full grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-3 rounded-none px-4 py-2.5 text-left text-sm font-normal hover:bg-background-tertiary"
                  aria-label={`${fmtTimeMs(h.ts)} ${h.public_model || ''} 详情`}
                >
                  <span className="font-mono text-xs text-muted">{fmtTimeMs(h.ts)}</span>
                  <span className="min-w-0 truncate">
                    <span className="font-mono text-[13px]">{h.public_model || '—'}</span>
                    <span className="ml-2 text-xs text-muted">{upstreamName(h)}</span>
                  </span>
                  <span className="flex items-center gap-2.5">
                    <span className={`text-xs tabular-nums ${totalTimeTone(h)}`}>{fmtSec(h.latency_ms)} s</span>
                    <StatusChip tone={statusTone(h.status)}>{statusLabel(h.status)}</StatusChip>
                  </span>
                </Button>
              </li>
            ))}
          </ul>
        </div>
      )}

      <UsageDetailDrawer entry={selected} onClose={() => setSelected(null)} />
    </section>
  )
}
