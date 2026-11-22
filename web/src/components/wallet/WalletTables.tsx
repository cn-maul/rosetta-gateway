import { useNavigate } from 'react-router-dom'
import { Button, Spinner, Table } from '@heroui/react'
import { Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import { DataTable } from '../../ui/DataTable'
import { StatusChip } from '../../ui/StatusChip'
import { fmtNum, fmtTokens, fmtMoney, fmtTimeMs } from '../../fmt'
import type { TopupRecord, UsageGroupEntry } from '../../types'
import { CHART_COLORS, ChartTooltipBox, TooltipRow } from '../../ui/chart'
import { buildUsageSeries, type UsageBucket } from '../../usageSeries'

const softError = (text: string) => (
  <p className="bg-warning-soft text-warning-soft-foreground mb-3.5 rounded-lg px-3 py-2 text-[12.5px]" role="alert">
    {text}
  </p>
)

const emptyBox = (title: string, detail?: string) => (
  <div className="py-10 text-center">
    <div className="text-muted text-2xl">∅</div>
    <div className="mt-1.5">{title}</div>
    {detail ? (
      <div className="text-muted mx-auto mt-2.5 max-w-[46em] text-[12.5px]">{detail}</div>
    ) : null}
  </div>
)

function GotoHistoryLink() {
  const navigate = useNavigate()
  return (
    <div className="mt-3.5 flex justify-end">
      <Button variant="ghost" size="sm" onPress={() => void navigate('/history')}>
        查看逐条调用明细 →
      </Button>
    </div>
  )
}

/**
 * 普通用户的消费趋势：钱包页原本只有一张表，看不到「哪段花得多」。
 * 与总览共用同一套补零与合并规则：90 天窗口按周合并（13 根），缺失的桶
 * 补 0；逐日精确值仍由下面的表格给。图例标题按实际桶宽写，不能让
 * 「每日」的标题配一根整周合计的柱子。
 */
function DayCostChart({ byDay, days }: { byDay: UsageGroupEntry[]; days: number }) {
  const rows = buildUsageSeries({ byDay, days, firstRecordAt: 0 })
  const max = rows.reduce((m, r) => Math.max(m, r.cost), 0)
  const unit = days > 31 ? '按周' : '每日'
  if (max <= 0) return null
  return (
    <div className="border-separator bg-surface-secondary mb-4 rounded-2xl border p-3.5">
      <div className="text-muted mb-1 flex items-center justify-between text-[11.5px]">
        <span>{unit}消耗</span>
        <span>最高 ¥{fmtMoney(max)}</span>
      </div>
      <div className="h-[180px]" role="img" aria-label={`${unit}消耗趋势`}>
        <ResponsiveContainer width="100%" height="100%">
          <BarChart data={rows} margin={{ top: 8, right: 4, bottom: 0, left: 2 }}>
            <CartesianGrid stroke={CHART_COLORS.grid} strokeDasharray="3 5" vertical={false} />
            <XAxis
              dataKey="key"
              tick={{ fill: CHART_COLORS.muted, fontSize: 10 }}
              axisLine={{ stroke: CHART_COLORS.grid }}
              tickLine={false}
              minTickGap={28}
              tickFormatter={(v: string) => rows.find((r) => r.key === v)?.labFull ?? v.slice(5)}
            />
            <YAxis
              tick={{ fill: CHART_COLORS.muted, fontSize: 10 }}
              axisLine={false}
              tickLine={false}
              width={46}
              tickFormatter={(v: number) => `¥${fmtMoney(v)}`}
            />
            <Tooltip
              cursor={{ fill: CHART_COLORS.accentSoft }}
              content={({ active, payload, label }) => {
                if (!active || !payload?.length) return null
                const row = payload[0].payload as UsageBucket
                return (
                  <ChartTooltipBox title={row.tip}>
                    <TooltipRow label="费用" value={row.cost > 0 ? `¥${fmtMoney(row.cost)}` : '—'} />
                    <TooltipRow label="请求" value={fmtNum(row.count)} />
                    <TooltipRow label="Token" value={fmtTokens(row.tokens)} />
                  </ChartTooltipBox>
                )
              }}
            />
            <Bar dataKey="cost" fill={CHART_COLORS.accent} radius={[3, 3, 0, 0]} maxBarSize={10} />
          </BarChart>
        </ResponsiveContainer>
      </div>
    </div>
  )
}

/**
 * 管理员的「消费去向」入口。
 *
 * 这里原本是一张和「用量与费用 → 按用户」完全同源的汇总表：同样的区间、
 * 同样的字段、同样的用户顺序，只是少了几列筛选。两处并存时，读账的人会
 * 在两份数字之间来回核对，却无法从钱包页回答「这个月钱花在谁身上」之外的
 * 任何问题。钱包页保留余额、充值与流水的职责；分析动线交给用量页。
 */
export function SpendOverviewCard({
  totalCost,
  totalCalls,
  totalTokens,
}: {
  totalCost: number
  totalCalls: number
  totalTokens: number
}) {
  const navigate = useNavigate()
  return (
    <div className="border-separator bg-surface-secondary rounded-2xl border p-5">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="grid min-w-0 gap-4 sm:grid-cols-3">
          <div>
            <div className="text-muted text-[12px]">全部历史消费</div>
            <div className="mt-1 text-[28px] leading-tight font-semibold tabular-nums">¥{fmtMoney(totalCost)}</div>
            <div className="text-muted mt-1 text-[12px]">按落库金额固化</div>
          </div>
          <div>
            <div className="text-muted text-[12px]">Token 消耗</div>
            <div className="mt-1 text-[20px] leading-tight font-semibold tabular-nums">{fmtTokens(totalTokens)}</div>
            <div className="text-muted mt-1 text-[12px]">输入 + 输出 + 缓存</div>
          </div>
          <div>
            <div className="text-muted text-[12px]">调用次数</div>
            <div className="mt-1 text-[20px] leading-tight font-semibold tabular-nums">{fmtNum(totalCalls)}</div>
            <div className="text-muted mt-1 text-[12px]">全部历史</div>
          </div>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button variant="primary" size="sm" onPress={() => void navigate('/usage?tab=users')}>
            按用户查看用量
          </Button>
          <Button variant="secondary" size="sm" onPress={() => void navigate('/history')}>
            逐条调用日志
          </Button>
        </div>
      </div>
      <p className="text-muted mt-3 max-w-[52em] text-[12px] leading-relaxed">
        钱包页只负责余额、充值与流水；按用户、按模型、按上游的拆解在「用量与费用」里，
        那里还能切换近 1 / 7 / 30 天并看到成功率与性能。
      </p>
    </div>
  )
}

/** 普通用户的按天消耗记录。额度刻意保持弱化单行且与余额分开：
 * 额度耗尽换 key 即可，余额耗尽必须充值，放一起会被读成同一种东西。 */
export function ByDayTable({
  byDay,
  error,
  loadErr,
  loading,
  days,
  usedTokens,
  quotaTokens,
}: {
  byDay: UsageGroupEntry[]
  error: string
  loadErr: string
  loading: boolean
  days: number
  usedTokens: number
  quotaTokens: number
}) {
  const quotaExhausted = quotaTokens > 0 && usedTokens >= quotaTokens
  return (
    <div>
      <div className="text-muted mb-3.5 text-[12.5px]">消耗记录 · 近 {days} 天，按天列出</div>
      {loading ? (
        <Spinner role="status">加载中…</Spinner>
      ) : loadErr ? (
        softError(loadErr)
      ) : error ? (
        softError(error)
      ) : byDay.length === 0 ? (
        emptyBox(`近 ${days} 天没有消耗记录`)
      ) : (
        <>
          <DayCostChart byDay={byDay} days={days} />
          <DataTable label="账户账单" loading={loading} hasData={true}>
            <Table>
              <Table.ScrollContainer>
                <Table.Content aria-label="账户账单">
                  <Table.Header>
                    <Table.Column isRowHeader>日期</Table.Column>
                    <Table.Column className="text-right">调用次数</Table.Column>
                    <Table.Column className="text-right">Tokens</Table.Column>
                    <Table.Column className="text-right">当日费用</Table.Column>
                  </Table.Header>
                  <Table.Body>
                    {/* 后端 by-day 已按日期升序返回，这里不再排序。 */}
                    {byDay.map((d) => (
                      <Table.Row key={d.key}>
                        <Table.Cell>
                          <span className="font-mono">{d.key}</span>
                        </Table.Cell>
                        <Table.Cell className="text-right tabular-nums">{fmtNum(d.count)}</Table.Cell>
                        <Table.Cell className="text-right tabular-nums">{fmtTokens(d.tokens)}</Table.Cell>
                        {/* cost=0 显示「—」：那通常是未配价，而「未配置」不等于「免费」。 */}
                        <Table.Cell className="text-right tabular-nums">{d.cost > 0 ? '¥' + fmtMoney(d.cost) : '—'}</Table.Cell>
                      </Table.Row>
                    ))}
                  </Table.Body>
                </Table.Content>
              </Table.ScrollContainer>
            </Table>
          </DataTable>
          <GotoHistoryLink />
        </>
      )}
      <div className="border-separator mt-4.5 flex flex-wrap items-baseline gap-2.5 border-t pt-3.5 text-[12.5px]">
        <span className="text-muted">Token 配额</span>
        <span className="tabular-nums">
          已用 {fmtTokens(usedTokens)}
          {quotaTokens > 0 ? ` / ${fmtTokens(quotaTokens)}` : '（不限）'}
        </span>
        <span className="text-muted">额度按 token 计量，耗尽后更换密钥即可；余额按人民币计量，耗尽需充值。</span>
        {quotaExhausted ? <StatusChip tone="err">额度已用尽</StatusChip> : null}
      </div>
    </div>
  )
}

/** 充值记录：两种身份共用一张表，作用域由服务端决定。 */
export function TopupTable({
  topups,
  total,
  error,
  loading,
  admin,
}: {
  topups: TopupRecord[]
  total: number
  error: string
  loading: boolean
  admin: boolean
}) {
  return (
    <div>
      <div className="text-muted mb-3.5 text-[12.5px]">
        {admin ? `全部用户 · 最近 ${topups.length} 条` : `最近 ${topups.length} 条`}
      </div>
      {loading ? (
        <Spinner role="status">加载中…</Spinner>
      ) : error ? (
        softError(error)
      ) : topups.length === 0 ? (
        emptyBox('暂无充值记录', admin ? '还没有任何充值操作。' : '还没有人为你的账户充值。')
      ) : (
        <>
          <DataTable label="账户账单" loading={loading} hasData={true}>
            <Table>
              <Table.ScrollContainer>
                <Table.Content aria-label="账户账单">
                  <Table.Header>
                    <Table.Column isRowHeader>时间</Table.Column>
                    {admin ? <Table.Column>用户</Table.Column> : null}
                    <Table.Column className="text-right">金额</Table.Column>
                    <Table.Column className="text-right">调整后余额</Table.Column>
                    {admin ? <Table.Column>操作者</Table.Column> : null}
                    <Table.Column>备注</Table.Column>
                  </Table.Header>
                  <Table.Body>
                    {topups.map((t) => (
                      <Table.Row key={t.id}>
                        <Table.Cell>
                          <span className="font-mono">{fmtTimeMs(t.ts)}</span>
                        </Table.Cell>
                        {admin ? (
                          <Table.Cell>
                            <span className="font-mono">{t.username || '—'}</span>
                          </Table.Cell>
                        ) : null}
                        {/* 符号是金额列的全部信息量：充值与扣减走同一个端点。 */}
                        <Table.Cell className={`text-right tabular-nums${t.delta_cents < 0 ? ' text-muted' : ''}`}>
                          {t.delta_cents > 0 ? '+' : '−'}¥{fmtMoney(Math.abs(t.delta_cents) / 100)}
                        </Table.Cell>
                        <Table.Cell className="text-right tabular-nums">¥{fmtMoney(t.balance_after / 100)}</Table.Cell>
                        {admin ? (
                          <Table.Cell>
                            <span className="font-mono">{t.operator_username || '—'}</span>
                          </Table.Cell>
                        ) : null}
                        <Table.Cell className="text-muted">{t.remark || '—'}</Table.Cell>
                      </Table.Row>
                    ))}
                  </Table.Body>
                </Table.Content>
              </Table.ScrollContainer>
            </Table>
          </DataTable>
          {/* 截断提示对两种身份都要显示：否则看着 50 行以为那就是全部。 */}
          {total > topups.length ? (
            <div className="text-muted mt-3.5 flex justify-end text-[12px]">
              仅显示最近 {topups.length} / {total} 条
            </div>
          ) : null}
        </>
      )}
    </div>
  )
}
