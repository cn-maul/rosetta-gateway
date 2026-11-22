import { useCallback, useEffect, useRef, useState } from 'react'
import { Avatar, Button, Table } from '@heroui/react'
import { api } from '../api'
import { fmtMoney, fmtNum, fmtTokens } from '../fmt'
import { rangeBounds } from '../range'
import { RequestGeneration } from '../asyncState'
import type { UsageByUserEntry } from '../types'
import { DataTable } from '../ui/DataTable'
import { StatStrip } from '../ui/StatStrip'
import { StatusChip } from '../ui/StatusChip'
import { userInitial, userRowName, userRowTag } from '../userDisplay'
import { CHART_COLORS, ChartEmpty, ChartTooltipBox, TooltipRow, chartShade, shareText } from '../ui/chart'
import { Bar, BarChart, Cell, Pie, PieChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'

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
            <div className="text-muted text-xs">
              {truncated ? (
                <>
                  按消费展示前{" "}
                  <b className="text-foreground tabular-nums">{fmtNum(rows.length)}</b> 位，共{" "}
                  {fmtNum(total ?? 0)} 位用户
                </>
              ) : (
                <>当前区间共 {fmtNum(rows.length)} 位用户</>
              )}
            </div>
            <Button size="sm" variant="secondary" isDisabled={loading} onPress={load}>
              {loading ? '加载中…' : '刷新'}
            </Button>
          </div>
          <StatStrip
            ariaLabel={truncated ? `前 ${rows.length} 位用户汇总，共 ${total} 位` : '当前范围汇总'}
            items={[
              { label: '请求', value: fmtNum(totals.count) },
              { label: 'Token', value: fmtTokens(totals.tokens) },
              { label: '记录费用', value: `¥${fmtMoney(totals.cost)}` },
              {
                label: truncated ? '范围' : '用户',
                value: truncated
                  ? `${fmtNum(rows.length)} / ${fmtNum(total ?? 0)}`
                  : fmtNum(rows.length),
                hint: truncated ? '合计只覆盖当前展示的行' : '有消费记录的用户',
              },
            ]}
          />
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
      <UserMix rows={rows} total={totals.cost} />
    </DataTable>
  )
}

/**
 * 消费集中度：前 8 名用户 + 其余合并。
 *
 * 财务上最常问的两个问题分别是「谁花得最多」和「头部几家占了多少」。
 * 表格能答前者，环形图能答后者；把两者放在同一张卡里，读者不必自己
 * 把十几行百分比加起来做心算。
 */
function UserMix({ rows, total }: { rows: UsageByUserEntry[]; total: number }) {
  const sorted = [...rows].sort((a, b) => b.cost - a.cost)
  const top = sorted.slice(0, 8)
  const rest = sorted.slice(8)
  const restCost = rest.reduce((a, r) => a + r.cost, 0)
  const data = top
    .filter((r) => r.cost > 0)
    .map((r) => ({ key: r.key || '__orphan__', name: userRowName(r), value: r.cost }))
  if (restCost > 0) data.push({ key: '__rest__', name: `其余 ${rest.length} 位`, value: restCost })
  if (data.length === 0) {
    return (
      <div className="mt-4 rounded-xl border border-separator p-3">
        <div className="text-muted text-[11.5px]">消费集中度</div>
        <ChartEmpty text="当前区间没有已计价消费" />
      </div>
    )
  }
  // 中心读数只算前 N 位本人；分母 total 仍是全部已计价消费。
  const topData = data.filter((d) => d.key !== '__rest__')
  const sum = topData.reduce((a, d) => a + d.value, 0)
  return (
    <div className="mt-4 grid items-start gap-3 rounded-2xl border border-separator p-3 lg:grid-cols-[minmax(0,1fr)_250px]">
      <div>
        <div className="text-muted mb-2 text-[11.5px]">消费最高的用户（元）</div>
        <div
          className="h-[190px]"
          role="img"
          aria-label={`消费最高的 ${top.length} 位用户`}
        >
          <ResponsiveContainer width="100%" height="100%">
            <BarChart
              data={top.map((r) => ({ ...r, label: userRowName(r) }))}
              layout="vertical"
              margin={{ top: 0, right: 30, bottom: 0, left: 4 }}
            >
              <XAxis type="number" tick={{ fill: CHART_COLORS.muted, fontSize: 10.5 }} axisLine={false} tickLine={false} tickFormatter={(v: number) => `¥${fmtMoney(v)}`} />
              <YAxis
                type="category"
                dataKey="label"
                width={92}
                tick={{ fill: CHART_COLORS.foreground, fontSize: 10.5 }}
                axisLine={false}
                tickLine={false}
                tickFormatter={(value: string) => (value.length > 10 ? value.slice(0, 9) + '…' : value)}
              />
              <Bar dataKey="cost" radius={[0, 4, 4, 0]} maxBarSize={14}>
                {top.map((r, i) => (
                  <Cell key={r.key || '__orphan__'} fill={chartShade(CHART_COLORS.accent, i)} />
                ))}
              </Bar>
              <Tooltip
                cursor={{ fill: CHART_COLORS.accentSoft }}
                content={({ active, payload }) => {
                  if (!active || !payload?.length) return null
                  const row = payload[0].payload as UsageByUserEntry
                  return (
                    <ChartTooltipBox title={userRowName(row)}>
                      <TooltipRow label="消费" value={`¥${fmtMoney(row.cost)}`} />
                      <TooltipRow label="占比" value={shareText(row.cost, total)} />
                      <TooltipRow label="请求" value={fmtNum(row.count)} />
                    </ChartTooltipBox>
                  )
                }}
              />
            </BarChart>
          </ResponsiveContainer>
        </div>
      </div>
      <div className="relative h-[190px]">
        <ResponsiveContainer width="100%" height="100%">
          <PieChart>
            <Pie data={data} dataKey="value" nameKey="name" innerRadius={52} outerRadius={78} paddingAngle={1.5} stroke="none">
              {data.map((d, i) => (
                <Cell key={d.key} fill={d.key === '__rest__' ? CHART_COLORS.muted : chartShade(CHART_COLORS.accent, i)} />
              ))}
            </Pie>
            <Tooltip
              content={({ active, payload }) => {
                if (!active || !payload?.length) return null
                const row = payload[0].payload as (typeof data)[number]
                return (
                  <ChartTooltipBox title={row.name}>
                    <TooltipRow label="消费" value={`¥${fmtMoney(row.value)}`} />
                    <TooltipRow label="占比" value={shareText(row.value, total)} />
                  </ChartTooltipBox>
                )
              }}
            />
          </PieChart>
        </ResponsiveContainer>
        <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center">
          <div className="text-[17px] leading-none font-semibold tabular-nums">{shareText(sum, total)}</div>
          <div className="text-muted mt-1 text-[10.5px]">前 {topData.length} 位</div>
        </div>
      </div>
    </div>
  )
}
