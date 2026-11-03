import { useCallback, useEffect, useRef, useState } from 'react'
import { Avatar, Button, Table } from '@heroui/react'
import { api } from '../api'
import { fmtMoney, fmtNum, fmtTokens } from '../fmt'
import { rangeBounds } from '../range'
import { RequestGeneration } from '../asyncState'
import type { UsageByUserEntry } from '../types'
import { DataTable } from '../ui/DataTable'
import { StatusChip } from '../ui/StatusChip'
import { userInitial, userRowName, userRowTag } from '../userDisplay'

/**
 * 用量与费用的「按用户」面板（管理员）。
 *
 * 与钱包页的「消费汇总」共用同一份显示名/标记规则（src/userDisplay.ts），
 * 但时间口径不同：这里跟着页面上的近 1/7/30 天走，钱包页固定全部历史。
 * total 是后端给的有消费用户数，截断时必须显式说明 —— 否则「前 N 名」
 * 会被读成「全部用户」，这是读账时最直接的错误结论。
 */
const BY_USER_LIMIT = 200

export function UsageByUser({ days }: { days: number }) {
  const [rows, setRows] = useState<UsageByUserEntry[]>([])
  const [total, setTotal] = useState<number | null>(null)
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(true)
  const genRef = useRef(new RequestGeneration())

  const load = useCallback(async () => {
    const requests = genRef.current
    const generation = requests.begin()
    setLoading(true)
    setErr('')
    try {
      const { from, to } = rangeBounds(days)
      const page = await api.usageByUser(from, to, BY_USER_LIMIT)
      if (requests.isCurrent(generation)) {
        setRows(page.records)
        setTotal(page.total)
      }
    } catch (e) {
      if (!requests.isCurrent(generation)) return
      setErr('加载按用户统计失败：' + (e as Error).message)
      setRows([])
      setTotal(null)
    } finally {
      if (requests.isCurrent(generation)) setLoading(false)
    }
  }, [days])

  useEffect(() => {
    void load()
  }, [load])

  const truncated = total !== null && total > rows.length
  const totals = rows.reduce(
    (acc, row) => {
      acc.cost += row.cost
      acc.count += row.count
      acc.tokens += row.tokens
      return acc
    },
    { cost: 0, count: 0, tokens: 0 },
  )

  return (
    <DataTable
      label="按用户用量统计"
      loading={loading}
      error={err}
      empty={rows.length === 0}
      emptyText="该时段没有用户消费记录"
      onRetry={load}
      toolbar={
        <div className="flex flex-col gap-2">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div className="flex flex-wrap items-center gap-2">
              <span className="border-separator bg-default-soft text-muted inline-flex items-center gap-1 rounded-full border px-2.5 py-1 text-xs whitespace-nowrap">
                {truncated ? (
                  <>
                    前 <b className="text-foreground tabular-nums">{fmtNum(rows.length)}</b> /
                    <b className="text-foreground tabular-nums">{fmtNum(total ?? 0)}</b> 位用户
                  </>
                ) : (
                  <>
                    <b className="text-foreground tabular-nums">{fmtNum(rows.length)}</b> 位用户
                  </>
                )}
              </span>
              <span className="border-separator bg-default-soft text-muted inline-flex items-center gap-1 rounded-full border px-2.5 py-1 text-xs whitespace-nowrap">
                <b className="text-foreground tabular-nums">{fmtNum(totals.count)}</b> 次请求
              </span>
              <span className="border-separator bg-default-soft text-muted inline-flex items-center gap-1 rounded-full border px-2.5 py-1 text-xs whitespace-nowrap">
                <b className="text-foreground tabular-nums">{fmtTokens(totals.tokens)}</b> Tokens
              </span>
              <span className="border-separator bg-default-soft text-muted inline-flex items-center gap-1 rounded-full border px-2.5 py-1 text-xs whitespace-nowrap">
                <b className="text-foreground tabular-nums">¥{fmtMoney(totals.cost)}</b> 记录费用
              </span>
            </div>
            <Button size="sm" variant="secondary" isDisabled={loading} onPress={load}>
              {loading ? '加载中…' : '刷新'}
            </Button>
          </div>
          {truncated ? (
            <div
              className="text-muted border-separator bg-default-soft rounded-lg border px-2.5 py-1.5 text-xs"
              role="note"
            >
              按消费只显示前 {fmtNum(rows.length)} 位，共 {fmtNum(total ?? 0)} 位用户；上面的合计是这{' '}
              {fmtNum(rows.length)} 位的合计，不是整个区间的合计。
            </div>
          ) : null}
        </div>
      }
    >
      <Table>
        <Table.ScrollContainer>
          <Table.Content aria-label="按用户用量统计，可横向滚动">
            <Table.Header>
              <Table.Column isRowHeader>用户</Table.Column>
              <Table.Column className="text-right">消费</Table.Column>
              <Table.Column className="text-right">调用次数</Table.Column>
              <Table.Column className="text-right">Tokens</Table.Column>
              <Table.Column className="text-right">消费占比</Table.Column>
            </Table.Header>
            <Table.Body>
              {rows.map((r) => {
                const tag = userRowTag(r)
                const share = totals.cost > 0 ? (r.cost / totals.cost) * 100 : 0
                const shareText =
                  totals.cost <= 0 ? '—' : share > 0 && share < 0.1 ? '<0.1%' : `${share.toFixed(1)}%`
                return (
                  <Table.Row key={r.key || '__orphan__'}>
                    <Table.Cell>
                      <div className="flex items-center gap-2.5">
                        <Avatar size="sm">
                          <Avatar.Fallback>{userInitial(r)}</Avatar.Fallback>
                        </Avatar>
                        <div className="min-w-0">
                          <div className={`truncate text-sm font-semibold ${!r.user_name ? 'font-mono' : ''}`}>
                            {userRowName(r)}
                          </div>
                          {tag ? (
                            <div className="mt-1">
                              <StatusChip tone={tag.tone}>{tag.text}</StatusChip>
                            </div>
                          ) : null}
                        </div>
                      </div>
                    </Table.Cell>
                    <Table.Cell className="text-right tabular-nums">
                      <div className="text-sm font-semibold">{r.cost > 0 ? '¥' + fmtMoney(r.cost) : '—'}</div>
                    </Table.Cell>
                    <Table.Cell className="text-right tabular-nums">{fmtNum(r.count)}</Table.Cell>
                    <Table.Cell className="text-right tabular-nums">{fmtTokens(r.tokens)}</Table.Cell>
                    <Table.Cell className="min-w-40">
                      <div className="flex items-center justify-end gap-2">
                        <span className="text-muted text-xs tabular-nums">{shareText}</span>
                        <div className="bg-default-soft h-1 w-16 overflow-hidden rounded-full" aria-hidden="true">
                          <span
                            className="bg-accent block h-full rounded-full"
                            style={{ width: `${Math.max(0, Math.min(100, share))}%` }}
                          />
                        </div>
                      </div>
                    </Table.Cell>
                  </Table.Row>
                )
              })}
            </Table.Body>
          </Table.Content>
        </Table.ScrollContainer>
      </Table>
    </DataTable>
  )
}