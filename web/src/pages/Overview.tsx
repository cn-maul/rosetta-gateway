import { useCallback, useEffect, useRef, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import {
  Alert,
  Button,
  ListBox,
  ListBoxItem,
  Select,
  SelectIndicator,
  SelectPopover,
  SelectTrigger,
  SelectValue,
  ToggleButton,
  ToggleButtonGroup,
} from '@heroui/react'
import {
  Area,
  Bar,
  CartesianGrid,
  Cell,
  ComposedChart,
  BarChart,
  Pie,
  PieChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { api } from '../api'
import { RequestGeneration } from '../asyncState'
import { fmtMoney, fmtNum, fmtPercent, fmtSec, fmtSpeed, fmtTokens } from '../fmt'
import * as timeRange from '../range'
import { oneOf, usePersistedState } from '../ui/usePersistedState'
import type { Stats, UsageGroupEntry } from '../types'
import { PageHeader } from '../ui/PageHeader'
import { SectionCard } from '../ui/SectionCard'
import { SkeletonBlock } from '../ui/SkeletonBlock'
import { StatStrip } from '../ui/StatStrip'
import { bucketForPoint, buildUsageSeries, type UsageBucket } from '../usageSeries'
import {
  CHART_COLORS,
  ChartEmpty,
  ChartTooltipBox,
  TooltipRow,
  shareText,
} from '../ui/chart'

/**
 * 总览：先回答「走势」，再回答「稳不稳/慢在哪」，最后才给精确数字。
 *
 * 图表是主视图而不是装饰：曲线负责形状（哪天断流、成本是否同步上涨），
 * 结果环负责质量问题，性能图把「慢在等首字」与「慢在吐字」拆开。七项指标
 * 收成底部一条紧凑读数，既保留精确值，又不让七个同尺寸大卡占掉首屏。
 *
 * 点击趋势桶会进入调用日志——图形发现问题，日志继续定位，这是本次重塑的核心
 * 动线。时间区间仍严格共用 range.ts，图表、指标和日志说的是同一段时间。
 */

const RANGES = [
  { key: '1d', label: '今天', days: 1 },
  { key: '3d', label: '近 3 天', days: 3 },
  { key: '7d', label: '近 7 天', days: 7 },
  { key: '14d', label: '近 14 天', days: 14 },
  { key: '1m', label: '近 1 月', days: 30 },
  { key: '1y', label: '近 1 年', days: 365 },
  { key: 'all', label: '全部', days: 0 },
]
type RangeKey = string
const RANGE_KEYS = RANGES.map((r) => r.key)
const rangeDays = (key: string) => RANGES.find((r) => r.key === key)?.days ?? 1
const rangeLabel = (key: string) => RANGES.find((r) => r.key === key)?.label ?? ''

type TrendMetric = 'tokens' | 'count' | 'cost' | 'quality' | 'performance'
const TREND_METRICS: { key: TrendMetric; label: string }[] = [
  { key: 'tokens', label: 'Token' },
  { key: 'count', label: '请求' },
  { key: 'cost', label: '费用' },
  { key: 'quality', label: '成功率' },
  { key: 'performance', label: '速度' },
]
const TREND_KEYS = TREND_METRICS.map((m) => m.key)

const is401 = (e: unknown) => (e as { status?: number }).status === 401
const bounds = (key: string) => timeRange.rangeBounds(rangeDays(key))

/** 成功率：分母只算「有结论」的请求（成功 + 失败），取消单独看。 */
function successRate(s: Stats): number {
  const denom = s.success_count + s.error_count
  return denom > 0 ? s.success_count / denom : 0
}

export function Overview() {
  const [searchParams, setSearchParams] = useSearchParams()
  const navigate = useNavigate()
  const [lastRange, setLastRange] = usePersistedState<RangeKey>('overview.range', '3d', oneOf(RANGE_KEYS))
  const [lastMetric, setLastMetric] = usePersistedState<TrendMetric>(
    'overview.metric',
    'tokens',
    oneOf(TREND_KEYS),
  )
  const rawRange = searchParams.get('range') ?? lastRange
  const rangeKey: RangeKey = RANGES.some((r) => r.key === rawRange) ? rawRange : '3d'
  useEffect(() => {
    if (lastRange !== rangeKey) setLastRange(rangeKey)
  }, [rangeKey, lastRange, setLastRange])
  const setRangeKey = (key: string) => {
    const next = new URLSearchParams(searchParams)
    next.set('range', key)
    setSearchParams(next)
  }

  const statsGen = useRef(new RequestGeneration())
  const trendGen = useRef(new RequestGeneration())
  const spendGen = useRef(new RequestGeneration())
  const [stats, setStats] = useState<Stats | null>(null)
  const [statsErr, setStatsErr] = useState('')
  const [statsLoading, setStatsLoading] = useState(true)
  const [byDay, setByDay] = useState<UsageGroupEntry[] | null>(null)
  const [trendErr, setTrendErr] = useState('')
  const [trendLoading, setTrendLoading] = useState(true)
  const [byModel, setByModel] = useState<UsageGroupEntry[] | null>(null)
  const [spendErr, setSpendErr] = useState('')
  const [spendLoading, setSpendLoading] = useState(true)
  const [metric, setMetric] = useState<TrendMetric>(lastMetric)

  const loadStats = useCallback(async (requested: string) => {
    const generation = statsGen.current.begin()
    setStatsLoading(true)
    setStatsErr('')
    try {
      const { from, to } = bounds(requested)
      const s = await api.stats(from, to, requested === 'all')
      if (!statsGen.current.isCurrent(generation)) return
      setStats(s)
    } catch (e) {
      if (statsGen.current.isCurrent(generation) && !is401(e)) {
        setStatsErr('统计加载失败：' + (e as Error).message)
      }
    } finally {
      if (statsGen.current.isCurrent(generation)) setStatsLoading(false)
    }
  }, [])

  const loadTrend = useCallback(async (requested: string) => {
    const generation = trendGen.current.begin()
    setTrendLoading(true)
    setTrendErr('')
    try {
      const { from, to } = bounds(requested)
      const page = await api.usageByDay(from, to)
      if (!trendGen.current.isCurrent(generation)) return
      setByDay(page.records)
    } catch (e) {
      if (trendGen.current.isCurrent(generation) && !is401(e)) {
        setTrendErr('趋势加载失败：' + (e as Error).message)
      }
    } finally {
      if (trendGen.current.isCurrent(generation)) setTrendLoading(false)
    }
  }, [])

  // 费用去向：与趋势图同一个区间。总览只回答「钱花在哪个模型」，
  // 更细的按上游/用户拆解交给用量页，避免首页堆满同源表格。
  const loadSpend = useCallback(async (requested: string) => {
    const generation = spendGen.current.begin()
    setSpendLoading(true)
    setSpendErr('')
    try {
      const { from, to } = bounds(requested)
      const page = await api.usageByModel(from, to, 100)
      if (!spendGen.current.isCurrent(generation)) return
      setByModel(page.records)
    } catch (e) {
      if (spendGen.current.isCurrent(generation) && !is401(e)) {
        setSpendErr('费用去向加载失败：' + (e as Error).message)
      }
    } finally {
      if (spendGen.current.isCurrent(generation)) setSpendLoading(false)
    }
  }, [])

  useEffect(() => {
    void loadStats(rangeKey)
    void loadTrend(rangeKey)
    void loadSpend(rangeKey)
  }, [loadStats, loadTrend, loadSpend, rangeKey])

  const refresh = () => {
    void loadStats(rangeKey)
    void loadTrend(rangeKey)
    void loadSpend(rangeKey)
  }

  const buckets = buildUsageSeries({
    byDay: byDay ?? [],
    days: rangeDays(rangeKey),
    firstRecordAt: stats?.lifetime?.first_record_at ?? 0,
  })

  const openDay = (bucket: UsageBucket) => {
    // 图表桶可能是日 / 周 / 月。这里传绝对时间窗，日志页不再用「最近 N 天」
    // 反推，否则点一个上个月的月桶会跳成最近 30 天，图和日志说的不是同一段。
    // 起止由 usageSeries 直接给，不再从标签字符串反解析日期（标签里有 `*`、
    // `~`、中文注释，字符串解析迟早会错位）。
    const start = bucket.startTs
    const end = bucket.endTs
    const next = new URLSearchParams()
    next.set('from', String(start))
    next.set('to', String(end))
    void navigate({ pathname: '/history', search: next.toString() })
  }

  return (
    <>
      <PageHeader
        title="总览"
        sub={stats ? rangeLabel(rangeKey) : undefined}
        actions={
          <>
            <Select
              aria-label="统计时间范围"
              className="w-[110px]"
              selectedKey={rangeKey}
              onSelectionChange={(key) => {
                if (key !== null) setRangeKey(String(key))
              }}
            >
              <SelectTrigger>
                <SelectValue />
                <SelectIndicator />
              </SelectTrigger>
              <SelectPopover>
                <ListBox aria-label="统计时间范围选项">
                  {RANGES.map((r) => (
                    <ListBoxItem key={r.key} id={r.key}>
                      {r.label}
                    </ListBoxItem>
                  ))}
                </ListBox>
              </SelectPopover>
            </Select>
            <Button variant="secondary" size="sm" isDisabled={statsLoading && trendLoading} onPress={refresh}>
              刷新
            </Button>
          </>
        }
      />

      {statsErr ? (
        <Alert status={stats ? 'warning' : 'danger'} className="mb-4">
          <Alert.Content>
            <Alert.Title>
              {statsErr}
              {stats ? ' · 正在显示上次成功结果' : ''}
            </Alert.Title>
          </Alert.Content>
          {!stats ? (
            <Button size="sm" variant="tertiary" isDisabled={statsLoading} onPress={() => void loadStats(rangeKey)}>
              重试
            </Button>
          ) : null}
        </Alert>
      ) : null}

      {stats && stats.cost_pending_count > 0 ? (
        <Alert status="warning" className="mb-4">
          <Alert.Content>
            <Alert.Title>
              {fmtNum(stats.cost_pending_count)} 条请求尚未计价，区间费用被低估
            </Alert.Title>
            <Alert.Description>
              这些记录落库时没有可用单价。到「设置 → 模型价格」补齐后，后台对账会按差额补扣；
              在补齐前，费用图与合计只统计已计价部分。
            </Alert.Description>
          </Alert.Content>
        </Alert>
      ) : null}

      {/* 侧栏 300px 在 lg 就够放，不必等到 xl：否则 1024–1280 的笔记本上
          环形图会独占一整行，卡片里大片留白。 */}
      <div className="grid items-stretch gap-4 lg:grid-cols-[minmax(0,1fr)_300px]">
        <SectionCard
          title="调用趋势"
          description="点击任意时间点，带着同一区间进入调用日志"
          actions={
            <ToggleButtonGroup
              aria-label="趋势指标"
              selectionMode="single"
              disallowEmptySelection
              selectedKeys={[metric]}
              onSelectionChange={(keys) => {
                const value = [...keys][0]
                if (value === undefined) return
                const next = String(value) as TrendMetric
                setMetric(next)
                setLastMetric(next)
              }}
            >
              {TREND_METRICS.map((m) => (
                <ToggleButton key={m.key} id={m.key} size="sm">
                  {m.label}
                </ToggleButton>
              ))}
            </ToggleButtonGroup>
          }
        >
          {trendLoading && !byDay ? (
            <SkeletonBlock className="h-[300px] w-full" />
          ) : trendErr && !byDay ? (
            <Alert status="danger">
              <Alert.Content>
                <Alert.Title>{trendErr}</Alert.Title>
              </Alert.Content>
              <Button size="sm" variant="tertiary" onPress={() => void loadTrend(rangeKey)}>
                重试
              </Button>
            </Alert>
          ) : buckets.length === 0 ? (
            <ChartEmpty />
          ) : (
            <TrendChart buckets={buckets} metric={metric} onPick={openDay} />
          )}
          {trendErr && byDay ? (
            <div className="text-warning mt-3 text-[11.5px]" role="status">
              {trendErr} · 正在显示上次成功结果
            </div>
          ) : null}
        </SectionCard>

        <SectionCard title="请求结果" description="成功 / 失败 / 取消">
          <OutcomeChart stats={stats} loading={statsLoading} rangeLabel={rangeLabel(rangeKey)} />
        </SectionCard>
      </div>

      <div className="mt-4 grid gap-4 lg:grid-cols-[minmax(0,1.55fr)_minmax(280px,1fr)]">
        <SectionCard title="性能构成" description="首字等待与生成阶段分开看">
          <PerformanceChart stats={stats} loading={statsLoading} />
        </SectionCard>
        <SectionCard title="缓存命中" description="缓存命中占输入 token 的比例">
          <CacheChart stats={stats} loading={statsLoading} />
        </SectionCard>
      </div>

      <div className="mt-4">
        <SectionCard
          title="费用去向"
          description="同一区间内，钱花在哪些模型上；看完整拆解去「用量与费用」"
          actions={
            <Button variant="ghost" size="sm" onPress={() => void navigate('/usage?tab=models')}>
              查看全部模型
            </Button>
          }
        >
          <SpendChart rows={byModel} loading={spendLoading} error={spendErr} />
        </SectionCard>
      </div>

      <div className="mt-4">
        <Metrics stats={stats} loading={statsLoading} />
      </div>
    </>
  )
}

function SpendChart({
  rows,
  loading,
  error,
}: {
  rows: UsageGroupEntry[] | null
  loading: boolean
  error: string
}) {
  if (loading && !rows) return <SkeletonBlock className="h-[260px] w-full" />
  if (error && !rows) {
    return (
      <Alert status="danger">
        <Alert.Content>
          <Alert.Title>{error}</Alert.Title>
        </Alert.Content>
      </Alert>
    )
  }
  const top = [...(rows ?? [])]
    .filter((r) => r.cost > 0)
    .sort((a, b) => b.cost - a.cost)
    .slice(0, 8)
  if (top.length === 0) {
    return <ChartEmpty text={rows && rows.length > 0 ? '该区间还没有已计价费用' : '所选区间内没有调用记录'} />
  }
  const total = top.reduce((a, r) => a + r.cost, 0)
  const max = top[0].cost
  const data = top.map((r) => ({ ...r, label: r.name || r.key || '未归属' }))
  return (
    <div>
      <div className="h-[260px]" role="img" aria-label={`费用最高的 ${top.length} 个模型，合计 ¥${fmtMoney(total)}`}>
        <ResponsiveContainer width="100%" height="100%">
          <BarChart data={data} layout="vertical" margin={{ top: 2, right: 36, bottom: 2, left: 4 }}>
            <CartesianGrid stroke={CHART_COLORS.grid} strokeDasharray="3 5" horizontal={false} />
            <XAxis
              type="number"
              domain={[0, max * 1.08]}
              tick={{ fill: CHART_COLORS.muted, fontSize: 10.5 }}
              axisLine={false}
              tickLine={false}
              tickFormatter={(v: number) => `¥${fmtMoney(v)}`}
            />
            <YAxis
              type="category"
              dataKey="label"
              width={140}
              tick={{ fill: CHART_COLORS.foreground, fontSize: 11 }}
              axisLine={false}
              tickLine={false}
              tickFormatter={(value: string) =>
                value.length > 18 ? value.slice(0, 17) + '…' : value
              }
            />
            <Tooltip
              cursor={{ fill: CHART_COLORS.accentSoft }}
              content={({ active, payload }) => {
                if (!active || !payload?.length) return null
                const row = payload[0].payload as (typeof data)[number]
                return (
                  <ChartTooltipBox title={row.label}>
                    <TooltipRow label="费用" value={`¥${fmtMoney(row.cost)}`} />
                    <TooltipRow label="占比" value={shareText(row.cost, total)} />
                    <TooltipRow label="请求" value={fmtNum(row.count)} />
                    <TooltipRow label="Token" value={fmtTokens(row.tokens)} />
                    <TooltipRow
                      label="成功率"
                      value={row.ok + row.err > 0 ? `${fmtPercent(row.ok / (row.ok + row.err))}%` : '—'}
                    />
                  </ChartTooltipBox>
                )
              }}
            />
            <Bar dataKey="cost" fill={CHART_COLORS.cost} radius={[0, 4, 4, 0]} maxBarSize={20} />
          </BarChart>
        </ResponsiveContainer>
      </div>
      <div className="text-muted mt-2 text-[11.5px]">
        前 {top.length} 个模型合计 ¥{fmtMoney(total)}；未配置价格的模型不会出现在这里。
      </div>
    </div>
  )
}

function TrendChart({
  buckets,
  metric,
  onPick,
}: {
  buckets: UsageBucket[]
  metric: TrendMetric
  onPick: (bucket: UsageBucket) => void
}) {
  // 数据只做图表适配，不改桶的语义：日/周/月仍由 buildUsageSeries 决定。
  const data = buckets.map((b) => ({
    ...b,
    // 没有「有结论」的样本（整桶都是取消）时保持 null，让成功率曲线断开。
    // 写成 0 会在图上画出一个 0% 的谷底，看起来像上游全挂了。
    rate: b.ok + b.err > 0 ? b.ok / (b.ok + b.err) : null,
    // 速度同样是「无样本 ≠ 0」：没有请求的桶不能画成速度跌到 0。
    tps: b.count > 0 ? b.tpsSum / b.count : null,
  }))
  const picker = (state: { activeIndex?: number | string | null; activeLabel?: string | number }) => {
    const bucket = bucketForPoint(buckets, state)
    if (bucket) onPick(bucket)
  }
  // 只有一个时间桶时（「今天」/刚上线）折线没有可连接的线段，只剩一个孤立
  // 的数据点；把圆点显式画出来，否则整张图看起来像没加载出来。
  const singleDot = buckets.length === 1
  const emptyText =
    metric === 'quality' && data.every((d) => d.rate === null)
      ? '所选区间没有可计算成功率的请求（可能全部被取消）'
      : metric === 'performance' && data.every((d) => d.tps === null)
        ? '所选区间没有可计算速度的请求'
        : ''

  return (
    <div role="img" aria-label={`${metricLabel(metric)}趋势，共 ${buckets.length} 个时间桶`}>
      {emptyText ? (
        <div className="flex h-[300px] items-center justify-center">
          <ChartEmpty text={emptyText} />
        </div>
      ) : (
      <ResponsiveContainer width="100%" height={300}>
        <ComposedChart
          data={data}
          margin={{ top: 10, right: 8, bottom: 0, left: -10 }}
          className="cursor-pointer"
          onClick={(state) => picker(state as { activeIndex?: number | string | null; activeLabel?: string | number })}
        >
          <defs>
            <linearGradient id="trend-fill" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor={CHART_COLORS.accent} stopOpacity={0.3} />
              <stop offset="100%" stopColor={CHART_COLORS.accent} stopOpacity={0.02} />
            </linearGradient>
          </defs>
          <CartesianGrid stroke={CHART_COLORS.grid} strokeDasharray="3 5" vertical={false} />
          <XAxis
            dataKey="key"
            tickFormatter={(value: string) => buckets.find((b) => b.key === value)?.labFull ?? value}
            tick={{ fill: CHART_COLORS.muted, fontSize: 10.5 }}
            axisLine={{ stroke: CHART_COLORS.grid }}
            tickLine={false}
            interval="preserveStartEnd"
            minTickGap={18}
          />
          <YAxis
            tick={{ fill: CHART_COLORS.muted, fontSize: 10.5 }}
            axisLine={false}
            tickLine={false}
            width={48}
            tickFormatter={(v: number) => metricAxis(v, metric)}
          />
          <Tooltip
            cursor={{ stroke: CHART_COLORS.muted, strokeDasharray: '3 4' }}
            content={({ active, payload }) => {
              if (!active || !payload?.length) return null
              const row = payload[0].payload as (typeof data)[number]
              return (
                <ChartTooltipBox title={row.tip}>
                  <TooltipRow label="请求" value={fmtNum(row.count)} />
                  <TooltipRow label="Token" value={fmtTokens(row.tokens)} />
                  <TooltipRow label="费用" value={`¥${fmtMoney(row.cost)}`} />
                  <TooltipRow
                    label="成功率"
                    value={row.rate === null ? '—（无结论样本）' : `${fmtPercent(row.rate)}%`}
                  />
                  {row.tps !== null && row.tps > 0 ? (
                    <TooltipRow label="速度" value={`${fmtSpeed(row.tps)} t/s`} />
                  ) : null}
                </ChartTooltipBox>
              )
            }}
          />
          {metric === 'tokens' ? (
            <Area
              type="monotone"
              dataKey="tokens"
              stroke={CHART_COLORS.accent}
              strokeWidth={2}
              fill="url(#trend-fill)"
              dot={singleDot ? { r: 5, fill: CHART_COLORS.accent, strokeWidth: 0 } : false}
              activeDot={{ r: 4 }}
            />
          ) : metric === 'cost' ? (
            <Area
              type="monotone"
              dataKey="cost"
              stroke={CHART_COLORS.cost}
              strokeWidth={2}
              fill="url(#trend-fill)"
              dot={singleDot ? { r: 5, fill: CHART_COLORS.cost, strokeWidth: 0 } : false}
            />
          ) : metric === 'quality' ? (
            <Area
              type="monotone"
              dataKey="rate"
              stroke={CHART_COLORS.success}
              strokeWidth={2}
              fill="url(#trend-fill)"
              // 成功率是比例，不像用量可以补 0。只在有样本的桶上打点，
              // 空档由断线表达；否则稀疏样本会被一根斜线连成「中间掉到 0」。
              dot={{ r: 3, fill: CHART_COLORS.success, strokeWidth: 0 }}
              connectNulls={false}
            />
          ) : metric === 'performance' ? (
            <Area
              type="monotone"
              dataKey="tps"
              stroke={CHART_COLORS.info}
              strokeWidth={2}
              fill="url(#trend-fill)"
              dot={{ r: 3, fill: CHART_COLORS.info, strokeWidth: 0 }}
              connectNulls={false}
            />
          ) : (
            <Bar dataKey="count" fill={CHART_COLORS.accent} radius={[3, 3, 0, 0]} maxBarSize={28} />
          )}
        </ComposedChart>
      </ResponsiveContainer>
      )}
      <div className="text-muted mt-2 text-[11.5px]">
        {metric === 'tokens' && '曲线越高表示消耗的 Token 越多；点击任意点查看对应日期的请求。'}
        {metric === 'count' && '柱高是请求次数；短请求也可能很多，请与 Token 曲线一起看。'}
        {metric === 'cost' && '费用按落库时固化的金额汇总，改价不会回写历史。'}
        {metric === 'quality' && '成功率为成功 /（成功 + 失败），取消不算失败；整桶没有结论样本时曲线断开。'}
        {metric === 'performance' &&
          '曲线是成功请求的加权输出速度；速度下降但 Token 稳定，通常说明上游在变慢。无请求的时段断开，不显示成 0。'}
      </div>
    </div>
  )
}

function metricLabel(metric: TrendMetric): string {
  return TREND_METRICS.find((m) => m.key === metric)?.label ?? '用量'
}

function metricAxis(v: number, metric: TrendMetric): string {
  if (metric === 'cost') return `¥${fmtMoney(v)}`
  if (metric === 'tokens' || metric === 'count') return fmtTokens(v)
  if (metric === 'quality') return `${fmtPercent(v)}%`
  return fmtSpeed(v)
}

function OutcomeChart({
  stats,
  loading,
  rangeLabel,
}: {
  stats: Stats | null
  loading: boolean
  rangeLabel: string
}) {
  if (loading && !stats) return <SkeletonBlock className="h-[220px] w-full" />
  if (!stats) return <ChartEmpty text="还没有样本" />
  const total = stats.total_requests
  const parts = [
    { name: '成功', value: stats.success_count, color: CHART_COLORS.success },
    { name: '失败', value: stats.error_count, color: CHART_COLORS.danger },
    { name: '取消', value: stats.canceled_count, color: CHART_COLORS.muted },
  ].filter((p) => p.value > 0)
  if (parts.length === 0) return <ChartEmpty text="所选区间内没有请求" />
  const hasVerdict = stats.success_count + stats.error_count > 0
  const success = successRate(stats)
  return (
    // 这张卡常驻 300px 侧栏：并排会让图例只剩 ~90px，中文被拆成单字换行。
    // 上下堆叠在窄栏里更稳；图表高度用 flex 撑满，与左侧趋势图同高，
    // 不让首屏出现「左边一张高卡、右边一截空白」。
    <div className="flex h-full flex-col">
      <div className="relative h-[200px] shrink-0">
        <ResponsiveContainer width="100%" height="100%">
          <PieChart>
            <Pie data={parts} dataKey="value" nameKey="name" innerRadius={64} outerRadius={88} paddingAngle={2} stroke="none">
              {parts.map((p) => (
                <Cell key={p.name} fill={p.color} />
              ))}
            </Pie>
            <Tooltip
              content={({ active, payload }) => {
                if (!active || !payload?.length) return null
                const row = payload[0].payload as (typeof parts)[number]
                return (
                  <ChartTooltipBox title={row.name}>
                    <TooltipRow label="请求" value={fmtNum(row.value)} />
                    <TooltipRow label="占比" value={shareText(row.value, total)} />
                  </ChartTooltipBox>
                )
              }}
            />
          </PieChart>
        </ResponsiveContainer>
        <div className="pointer-events-none absolute inset-0 flex flex-col items-center justify-center">
          <div className="text-[26px] leading-none font-semibold tabular-nums">
            {hasVerdict ? `${fmtPercent(success)}%` : '—'}
          </div>
          <div className="text-muted mt-1 text-[11px]">{hasVerdict ? '成功率' : '无结论样本'}</div>
        </div>
      </div>
      <div className="mt-1 flex flex-col gap-1.5">
        {parts.map((p) => (
          <div key={p.name} className="flex items-center justify-between gap-3 text-[12px]">
            <span className="text-muted flex items-center gap-2">
              <span className="h-2 w-2 rounded-full" style={{ background: p.color }} />
              {p.name}
            </span>
            <span className="whitespace-nowrap tabular-nums">
              {fmtNum(p.value)} <span className="text-muted">({shareText(p.value, total)})</span>
            </span>
          </div>
        ))}
      </div>
      <p className="text-muted mt-auto pt-3 text-[11px] leading-relaxed">
        {rangeLabel}内成功、失败与取消的分布；取消是客户端主动断开，不计入成功率分母。
      </p>
    </div>
  )
}

function PerformanceChart({ stats, loading }: { stats: Stats | null; loading: boolean }) {
  if (loading && !stats) return <SkeletonBlock className="h-[150px] w-full" />
  if (!stats) return <ChartEmpty text="还没有样本" />
  const ttfb = stats.ttfb_samples > 0 ? stats.avg_ttfb_ms / 1000 : 0
  const gen = stats.gen_samples > 0 ? stats.avg_gen_ms / 1000 : 0
  if (ttfb + gen === 0) return <ChartEmpty text="还没有耗时样本" />
  const rows = [
    { name: '首字等待', value: ttfb, color: CHART_COLORS.info },
    { name: '生成', value: gen, color: CHART_COLORS.accent },
  ]
  return (
    <div>
      <div className="flex h-6 overflow-hidden rounded-lg bg-default-soft" aria-hidden="true">
        {rows.map((r) => (
          <div
            key={r.name}
            style={{ width: `${(r.value / (ttfb + gen)) * 100}%`, background: r.color }}
            title={`${r.name} ${fmtSec(r.value * 1000)}s`}
          />
        ))}
      </div>
      <div className="mt-4 grid grid-cols-2 gap-3">
        {rows.map((r) => (
          <div key={r.name}>
            <div className="text-muted text-[11.5px]">{r.name}</div>
            <div className="mt-1 text-xl font-semibold tabular-nums">{fmtSec(r.value * 1000)}s</div>
            <div className="text-muted mt-1 text-[11px]">
              {r.name === '首字等待' ? `${fmtNum(stats.ttfb_samples)} 个样本` : `${fmtNum(stats.gen_samples)} 个样本`}
            </div>
          </div>
        ))}
      </div>
    </div>
  )
}

function CacheChart({ stats, loading }: { stats: Stats | null; loading: boolean }) {
  if (loading && !stats) return <SkeletonBlock className="h-[150px] w-full" />
  if (!stats) return <ChartEmpty text="还没有样本" />
  const hit = stats.input_tokens > 0 ? stats.cache_hit_rate : 0
  return (
    <div className="flex h-[150px] flex-col justify-center">
      <div className="text-[28px] leading-none font-semibold tabular-nums">
        {stats.input_tokens > 0 ? `${fmtPercent(hit)}%` : '—'}
      </div>
      <div className="mt-2 text-[12px] text-muted">
        {fmtTokens(stats.cached_tokens)} / {fmtTokens(stats.input_tokens)} 输入 token
      </div>
      <div className="mt-4 h-2 overflow-hidden rounded-full bg-default-soft">
        <div className="h-full rounded-full" style={{ width: `${Math.max(0, Math.min(100, hit * 100))}%`, background: CHART_COLORS.accent }} />
      </div>
    </div>
  )
}

function Metrics({ stats, loading }: { stats: Stats | null; loading: boolean }) {
  if (loading && !stats) {
    return (
      <div className="grid grid-cols-2 gap-px overflow-hidden rounded-2xl border border-separator bg-separator md:grid-cols-4 lg:grid-cols-7" role="status" aria-label="正在读取指标">
        {Array.from({ length: 7 }, (_, i) => (
          <div key={i} className="bg-surface p-3.5">
            <SkeletonBlock className="h-3 w-14" />
            <SkeletonBlock className="mt-2 h-6 w-20" />
          </div>
        ))}
      </div>
    )
  }
  if (!stats) return null
  const denom = stats.success_count + stats.error_count
  const rate = successRate(stats)
  const items = [
    { label: '请求数', value: fmtNum(stats.total_requests) },
    { label: 'Token', value: fmtTokens(stats.total_tokens) },
    { label: '记录费用', value: `¥${fmtMoney(stats.cost)}` },
    {
      label: '成功率',
      value: denom > 0 ? `${fmtPercent(rate)}%` : '—',
      tone: denom > 0 && rate < 0.95 ? ('warning' as const) : undefined,
    },
    {
      label: 'TPS',
      value: stats.throughput_samples > 0 ? `${fmtSpeed(stats.avg_tokens_per_sec)} t/s` : '—',
    },
    { label: '首字延迟', value: stats.ttfb_samples > 0 ? `${fmtSec(stats.avg_ttfb_ms)}s` : '—' },
    { label: '生成耗时', value: stats.gen_samples > 0 ? `${fmtSec(stats.avg_gen_ms)}s` : '—' },
  ]
  return <StatStrip ariaLabel="总览精确指标" className="lg:grid-cols-7" items={items} />
}
