import { useCallback, useEffect, useRef, useState } from 'react'
import { Table } from '@heroui/react'
import { api } from '../api'
import { fmtNum, fmtTimeMs } from '../fmt'
import { rangeBounds } from '../range'
import { RequestGeneration } from '../asyncState'
import { DataTable } from '../ui/DataTable'
import type { UsageFailureCodeEntry, UsageFailureUpstreamEntry, UsageFailures } from '../types'

/**
 * 「失败原因」页签：回答两个问题 —— 什么错最多、是不是某个上游在坏。
 *
 * 为什么不复用 UsageByGroup：那个组件的每一行都要算 token、费用、速度，
 * 而这里的分组对象是**失败**。把失败硬塞进 usageGroupEntry 会让 tokens 与
 * cost 恒为 0，读的人会以为「有失败但没花钱」是个有意义的信号。
 *
 * 数据口径由后端定（排除 ok 与 canceled），前端不重算 —— 失败率的公式
 * 只能有一处，理由同 tps：写两遍必然在某次改动后分叉。
 */

/** 失败率配色：>50% 基本等于这家不可用；>10% 已经值得查。 */
function rateTone(rate: number): string {
  if (rate >= 0.5) return 'text-danger'
  if (rate >= 0.1) return 'text-warning'
  return ''
}

function pct(rate: number): string {
  return `${(rate * 100).toFixed(rate >= 0.999 ? 0 : 1)}%`
}

/** 顶部的三枚数字。empty 时也要显示 —— 「这段时间 0 失败」本身就是结论。 */
function Stat({ label, value, hint, tone }: { label: string; value: string; hint?: string; tone?: string }) {
  return (
    <div className="rounded-xl bg-background-tertiary px-3.5 py-3">
      <span className="mb-1 block text-[10.5px] text-muted">{label}</span>
      <strong className={'block text-lg ' + (tone ?? '')}>{value}</strong>
      {hint ? <span className="mt-0.5 block text-[10.5px] text-muted">{hint}</span> : null}
    </div>
  )
}

function CodeTable({ rows, failed }: { rows: UsageFailureCodeEntry[]; failed: number }) {
  return (
    <div className="min-w-0">
      <h3 className="mb-2 text-xs font-semibold text-muted">按错误码</h3>
      <Table>
        <Table.ScrollContainer>
          <Table.Content aria-label="失败错误码构成">
            <Table.Header>
              <Table.Column isRowHeader>错误码</Table.Column>
              <Table.Column>HTTP</Table.Column>
              <Table.Column>次数</Table.Column>
              <Table.Column>占比</Table.Column>
              <Table.Column>最近发生</Table.Column>
            </Table.Header>
            <Table.Body>
              {rows.map((r) => (
                <Table.Row key={r.code + ':' + r.http_status}>
                  <Table.Cell>
                    <code className="font-mono text-xs break-all">{r.code}</code>
                  </Table.Cell>
                  <Table.Cell>
                    <span className="font-mono text-xs">{r.http_status || '—'}</span>
                  </Table.Cell>
                  <Table.Cell>
                    <span className="font-mono text-xs">{fmtNum(r.count)}</span>
                  </Table.Cell>
                  <Table.Cell>
                    {/* 用一条宽度条 + 百分比：单看次数判断不了「这是主因还是零头」。 */}
                    <div className="flex items-center gap-2">
                      <span className="h-1.5 w-14 overflow-hidden rounded-full bg-background-tertiary">
                        <span
                          className="block h-full rounded-full bg-danger"
                          style={{ width: failed > 0 ? Math.max(2, (r.count / failed) * 100) + '%' : '0%' }}
                        />
                      </span>
                      <span className="font-mono text-[11px] text-muted">
                        {failed > 0 ? pct(r.count / failed) : '—'}
                      </span>
                    </div>
                  </Table.Cell>
                  <Table.Cell>
                    <span className="font-mono text-[11px] text-muted">{fmtTimeMs(r.last_ts)}</span>
                  </Table.Cell>
                </Table.Row>
              ))}
            </Table.Body>
          </Table.Content>
        </Table.ScrollContainer>
      </Table>
    </div>
  )
}

function UpstreamTable({ rows }: { rows: UsageFailureUpstreamEntry[] }) {
  return (
    <div className="min-w-0">
      <h3 className="mb-2 text-xs font-semibold text-muted">按上游</h3>
      <Table>
        <Table.ScrollContainer>
          <Table.Content aria-label="各上游的失败率">
            <Table.Header>
              <Table.Column isRowHeader>上游</Table.Column>
              <Table.Column>失败 / 请求</Table.Column>
              <Table.Column>失败率</Table.Column>
              <Table.Column>最近失败</Table.Column>
            </Table.Header>
            <Table.Body>
              {rows.map((r) => (
                <Table.Row key={r.provider_id}>
                  <Table.Cell>
                    {/* 名字是落库时固化的历史值；旧记录没记名字时回退显示 id，
                        留白会被读成「无归属」，与「名字丢了」是两件事。 */}
                    <span className="text-xs break-all">{r.provider_name || r.provider_id || '未记录'}</span>
                  </Table.Cell>
                  <Table.Cell>
                    <span className="font-mono text-xs">
                      {fmtNum(r.failed)} / {fmtNum(r.total)}
                    </span>
                  </Table.Cell>
                  <Table.Cell>
                    <span className={'font-mono text-xs ' + rateTone(r.rate)}>{pct(r.rate)}</span>
                  </Table.Cell>
                  <Table.Cell>
                    <span className="font-mono text-[11px] text-muted">{fmtTimeMs(r.last_fail_ts)}</span>
                  </Table.Cell>
                </Table.Row>
              ))}
            </Table.Body>
          </Table.Content>
        </Table.ScrollContainer>
      </Table>
    </div>
  )
}

export function UsageFailuresPanel({ days }: { days: number }) {
  const [data, setData] = useState<UsageFailures | null>(null)
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const genRef = useRef(new RequestGeneration())

  const load = useCallback(async () => {
    const requests = genRef.current
    const generation = requests.begin()
    setLoading(true)
    setErr('')
    try {
      // to 现算而不是缓存：页面开着跨过零点时，缓存的 to 会让最新一批失败
      // 永远落在窗口外——而排障恰恰是在「刚刚出问题」时打开这个页面。
      const { from, to } = rangeBounds(days)
      const res = await api.usageFailures(from, to, 20)
      if (requests.isCurrent(generation)) setData(res)
    } catch (e) {
      if (!requests.isCurrent(generation)) return
      // 失败必须留可见错误态：空表与「这段时间真的没有失败」长得一模一样，
      // 用户会把一次查询失败读成「一切正常」——这是排障页最危险的误读。
      setErr('加载失败原因失败：' + (e as Error).message)
      setData(null)
    } finally {
      if (requests.isCurrent(generation)) setLoading(false)
    }
  }, [days])

  useEffect(() => {
    void load()
  }, [load])

  const total = data?.total ?? 0
  const failed = data?.failed ?? 0

  return (
    <div className="space-y-4">
      {/* 统计卡放在 DataTable 外面：空态时 DataTable 会整块替换成空提示，
          而「0 次失败」正是这时最该被看见的结论。 */}
      <div className="grid grid-cols-3 gap-2.5">
        <Stat label="请求总数" value={loading ? '…' : fmtNum(total)} />
        <Stat label="失败" value={loading ? '…' : fmtNum(failed)} tone={failed > 0 ? 'text-danger' : undefined} />
        <Stat
          label="失败率"
          value={loading ? '…' : data ? pct(data.rate) : '—'}
          tone={data ? rateTone(data.rate) : undefined}
          hint={data && failed === 0 && total > 0 ? '这段时间没有失败' : undefined}
        />
      </div>

      <DataTable
        label="失败原因统计"
        loading={loading}
        error={err}
        hasData={!!data}
        empty={!loading && !err && !!data && failed === 0}
        emptyText="该时段没有失败记录"
        onRetry={load}
        skeletonRows={5}
        skeletonCols={4}
        // 覆盖范围必须写出来：明细保留期之外只剩聚合计数，没有 error_code，
        // 不写清楚会让人把「30 天前没有失败」当成结论。
        footer={
          <p className="text-muted mt-3 text-[11px] leading-relaxed">
            只统计明细保留期内的失败（默认 30 天）。更早的记录在归档里只保留总数，无法按错误码与上游拆分。
            失败口径不含客户端主动取消。
          </p>
        }
      >
        <div className="grid grid-cols-1 gap-6 xl:grid-cols-2">
          <CodeTable rows={data?.codes ?? []} failed={failed} />
          <UpstreamTable rows={data?.upstreams ?? []} />
        </div>
      </DataTable>
    </div>
  )
}
