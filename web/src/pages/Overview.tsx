import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import {
  Button,
  ListBox,
  ListBoxItem,
  ProgressBar,
  ProgressCircle,
  Select,
  SelectIndicator,
  SelectPopover,
  SelectTrigger,
  SelectValue,
} from '@heroui/react'
import { api, isAdmin } from '../api'
import { RequestGeneration } from '../asyncState'
import { fmtMoney, fmtNum, fmtPercent, fmtRemainder, fmtBalance, fmtSec, fmtSpeed, fmtTimeMs, fmtTokens } from '../fmt'
import * as timeRange from '../range'
import { oneOf, usePersistedState } from '../ui/usePersistedState'
import type { Provider, Stats, StatsOverview, UsageGroupEntry } from '../types'
import { PageHeader } from '../ui/PageHeader'
import { SectionCard } from '../ui/SectionCard'
import { StatusChip } from '../ui/StatusChip'
import { SkeletonBlock } from '../ui/SkeletonBlock'
import { Sparkline } from '../ui/Sparkline'
import { useInView } from '../ui/useInView'
import { RecentRequests } from '../components/RecentRequests'

/**
 * 总览页（U1 首屏仪表盘，docs/frontend-ux-overhaul-plan.md §U1）：
 * 第一屏只回答两个问题——「现在正常吗」（状态条 + 最近异常/调用）和
 * 「今天花了多少」（KPI，默认近 1 天）；趋势、待办、配置五卡全部降到
 * 第二屏（滚动可见才拉 byDay）。旧版语义（文案、口径、降级行为）逐字
 * 保留，只是重排与图形化。
 */

const RANGES = [
  { key: '1d', label: '今天', days: 1 },
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

const is401 = (e: unknown) => (e as { status?: number }).status === 401

// 区间口径统一走 range.ts：days=1 是今天 0 点起算，不是滚动 24 小时。
// 图和数字必须共用同一个起点函数（rangeStart/localMidnight），否则会出现
// 「数字是今天的数据、图上却有昨天一截」的口径漂移。

// 柱状图的一个桶。labFull / labShort 是预先算好的展示标签，模板不再做字符串切片。
type Bucket = { key: string; count: number; tokens: number; labFull: string; labShort: string; tip: string }

function buildDaySeries(byDay: UsageGroupEntry[], stats: Stats | null, rangeKey: string): Bucket[] {
  const map = new Map(byDay.map((e) => [e.key, e]))
  const days = rangeDays(rangeKey)
  const now = Date.now()

  // 全部历史：起点优先用终身累计里的 first_record_at（明细剪掉 30 天后
  // byDay 只剩 30 天，用 byDay 最早一天当起点会凭空丢掉更早的历史）。
  let start: Date
  if (days === 0) {
    const first = stats?.lifetime?.first_record_at
    if (first && first > 0) {
      start = new Date(first)
    } else {
      const keys = byDay.map((e) => e.key).sort()
      const earliest = keys[0]
      start = earliest ? new Date(earliest + 'T00:00:00') : timeRange.localMidnight(30, now)
    }
  } else {
    start = new Date(timeRange.rangeStart(days, now))
  }

  // 逐日补齐。日期串走 localDayKey：与后端 dayExpr（localtime）同一口径，
  // 键必须与 byDay 的 key 逐字相同，否则查不到、每根柱子都被补成 0。
  const daily: { date: string; count: number; tokens: number }[] = []
  const cursor = new Date(start.getFullYear(), start.getMonth(), start.getDate())
  const end = timeRange.localMidnight(0, now)
  while (cursor.getTime() <= end.getTime()) {
    const date = timeRange.localDayKey(cursor.getTime())
    const e = map.get(date)
    daily.push({ date, count: e?.count ?? 0, tokens: e?.tokens ?? 0 })
    cursor.setDate(cursor.getDate() + 1)
  }

  // 桶宽：≤31 天按日；1 年按周；全部按月。
  const bucketDays = days === 0 ? 30 : days > 31 ? 7 : 1
  if (bucketDays === 1) {
    return daily.map((d) => ({
      key: d.date,
      count: d.count,
      tokens: d.tokens,
      labFull: d.date.slice(5),
      labShort: d.date.slice(8),
      tip: d.date.slice(5),
    }))
  }

  // 周桶/月桶的窄标签用「N月」：一年期的横轴是看月份推进，只写「日」读不出月份。
  const monthLabel = (dateStr: string) => `${parseInt(dateStr.slice(5, 7), 10)}月`
  const monthly = bucketDays > 7
  const out: Bucket[] = []
  for (let i = 0; i < daily.length; i += bucketDays) {
    const slice = daily.slice(i, i + bucketDays)
    const first = slice[0]
    const last = slice[slice.length - 1]
    // 尾桶不足整桶宽：标签加 * 标记、tip 写明实际天数；刻意不按桶宽做高度
    // 归一——那会把「只有 2 天的量」放大成「一整桶的量」，数据语义就变了。
    const partial = slice.length < bucketDays
    const mark = partial ? '*' : ''
    const partialNote = partial ? `（部分桶：仅 ${slice.length} 天）` : ''
    // 月桶跨月时标签补上止月：只写首月会被读成整桶都落在首月。
    const sameMonth = first.date.slice(0, 7) === last.date.slice(0, 7)
    let labFullMonth = first.date.slice(0, 7)
    if (!sameMonth) {
      labFullMonth +=
        first.date.slice(0, 4) === last.date.slice(0, 4)
          ? `~${last.date.slice(5, 7)}`
          : `~${last.date.slice(0, 7)}`
    }
    const m1 = parseInt(first.date.slice(5, 7), 10)
    const m2 = parseInt(last.date.slice(5, 7), 10)
    out.push({
      key: first.date,
      count: slice.reduce((a, b) => a + b.count, 0),
      tokens: slice.reduce((a, b) => a + b.tokens, 0),
      labFull: (monthly ? labFullMonth : first.date.slice(5)) + mark,
      labShort: (monthly ? (sameMonth ? monthLabel(first.date) : `${m1}~${m2}月`) : monthLabel(first.date)) + mark,
      tip: `${first.date.slice(5)} ~ ${last.date.slice(5)}${partialNote}`,
    })
  }
  return out
}

// 首页只列「需要动手」的事项；正常的配置不在待办区重复一遍。
type AttentionItem = { level: 'error' | 'warn'; title: string; detail: string; to: string }

export function Overview() {
  const [searchParams, setSearchParams] = useSearchParams()
  const admin = isAdmin()

  // 上次看的时间范围记在本地：下次进来还是同一档，不用每次重新点。
  // URL 仍优先 —— 带 ?range= 的链接行为完全不变。
  const [lastRange, setLastRange] = usePersistedState<RangeKey>('overview.range', '1d', oneOf(RANGE_KEYS))
  const rawRange = searchParams.get('range') ?? lastRange
  const rangeKey: RangeKey = RANGES.some((r) => r.key === rawRange) ? rawRange : '1d'
  useEffect(() => {
    if (lastRange !== rangeKey) setLastRange(rangeKey)
  }, [rangeKey, lastRange, setLastRange])
  const setRangeKey = (key: string) => {
    const next = new URLSearchParams(searchParams)
    next.set('range', key)
    setSearchParams(next)
  }

  const statsGen = useRef(new RequestGeneration())
  const [stats, setStats] = useState<Stats | null>(null)
  // 近 7 天对照（仅「今天」档拉）：KPI 副行给「近 7 天日均」，让今天的高峰
  // 或低谷一眼可辨，不用换档对比。
  const [stats7, setStats7] = useState<Stats | null>(null)
  const [statsErr, setStatsErr] = useState('')
  const [statsLoading, setStatsLoading] = useState(true)
  const [loadedAt, setLoadedAt] = useState(0)

  // byDay 只服务趋势图（第二屏），滚到可见才拉。null = 还没加载过。
  const trendGen = useRef(new RequestGeneration())
  const [byDay, setByDay] = useState<UsageGroupEntry[] | null>(null)
  const [trendErr, setTrendErr] = useState('')
  const [trendLoading, setTrendLoading] = useState(false)
  const [trendRef, trendInView] = useInView<HTMLDivElement>()

  const gatewayGen = useRef(new RequestGeneration())
  const [overview, setOverview] = useState<StatsOverview | null>(null)
  const [providers, setProviders] = useState<Provider[]>([])
  const [overviewError, setOverviewError] = useState('')
  const [providerError, setProviderError] = useState('')
  const [gatewayLoading, setGatewayLoading] = useState(admin)
  const [gatewayAt, setGatewayAt] = useState(0)

  const loadStats = useCallback(async (requested: string) => {
    const generation = statsGen.current.begin()
    setStatsLoading(true)
    setStatsErr('')
    try {
      const { from, to } = timeRange.rangeBounds(rangeDays(requested))
      // 管理员始终带终身累计：状态条的累计用量不能随区间切换而消失。
      const requests: [Promise<Stats>, Promise<Stats> | null] = [api.stats(from, to, isAdmin()), null]
      if (requested === '1d') {
        const w = timeRange.rangeBounds(7)
        requests[1] = api.stats(w.from, w.to, false)
      }
      const [s, s7] = await Promise.all([requests[0], requests[1] ?? Promise.resolve(null)])
      if (!statsGen.current.isCurrent(generation)) return
      setStats(s)
      setStats7(s7)
      setLoadedAt(Date.now())
    } catch (e) {
      // 失败保留上次结果，由 stale-bar 如实说明。
      if (statsGen.current.isCurrent(generation) && !is401(e)) setStatsErr('统计加载失败：' + (e as Error).message)
    } finally {
      if (statsGen.current.isCurrent(generation)) setStatsLoading(false)
    }
  }, [])

  const loadTrend = useCallback(
    async (requested: string) => {
      const generation = trendGen.current.begin()
      setTrendLoading(true)
      setTrendErr('')
      try {
        const { from, to } = timeRange.rangeBounds(rangeDays(requested))
        const page = await api.usageByDay(from, to)
        if (!trendGen.current.isCurrent(generation)) return
        setByDay(page.records)
      } catch (e) {
        if (trendGen.current.isCurrent(generation) && !is401(e)) setTrendErr('趋势加载失败：' + (e as Error).message)
      } finally {
        if (trendGen.current.isCurrent(generation)) setTrendLoading(false)
      }
    },
    [],
  )

  // 当前配置独立于历史统计；任一数据源失败只把自己的状态置为未知。
  const loadGateway = useCallback(async () => {
    if (!isAdmin()) return
    const generation = gatewayGen.current.begin()
    setGatewayLoading(true)
    const [o, p] = await Promise.allSettled([api.statsOverview(), api.providers()])
    if (!gatewayGen.current.isCurrent(generation)) return
    if (o.status === 'fulfilled') {
      setOverview(o.value)
      setOverviewError('')
    } else {
      setOverview(null)
      setOverviewError(o.reason instanceof Error ? o.reason.message : '当前配置读取失败')
    }
    if (p.status === 'fulfilled') {
      setProviders(p.value)
      setProviderError('')
    } else {
      setProviders([])
      setProviderError(p.reason instanceof Error ? p.reason.message : '上游状态读取失败')
    }
    setGatewayAt(Date.now())
    setGatewayLoading(false)
  }, [])

  useEffect(() => {
    void loadStats(rangeKey)
    setByDay(null)
  }, [loadStats, rangeKey])
  // 滚到趋势区才拉按日明细；换档后重新等可见触发。
  useEffect(() => {
    if (trendInView && byDay === null && !trendLoading) void loadTrend(rangeKey)
  }, [trendInView, byDay, trendLoading, loadTrend, rangeKey])
  useEffect(() => {
    void loadGateway()
  }, [loadGateway])

  const refresh = () => {
    void loadStats(rangeKey)
    if (byDay !== null) void loadTrend(rangeKey)
    void loadGateway()
  }

  const unready = useMemo(() => providers.filter((p) => p.enabled && p.ready === false), [providers])
  const unknown = useMemo(
    () => providers.filter((p) => p.enabled && typeof (p.ready as boolean | null) !== 'boolean'),
    [providers],
  )
  const lifetimeTokens = stats?.lifetime?.total_tokens ?? null
  const lifetimeCost = stats?.lifetime?.cost_total ?? null

  const attentionItems = useMemo<AttentionItem[]>(() => {
    const items: AttentionItem[] = []
    const o = overview
    const s = stats
    if (o) {
      if (o.providers_unready > 0) {
        items.push({
          level: 'error',
          title: `${fmtNum(o.providers_unready)} 个已启用上游未就绪`,
          detail: '凭据池为空或无法建立上游客户端，相关模型请求会失败。',
          to: '/providers?status=unready',
        })
      }
      if (o.providers_unknown > 0) {
        items.push({
          level: 'warn',
          title: `${fmtNum(o.providers_unknown)} 个上游运行状态未知`,
          detail: '当前快照缺少这些上游的就绪信息，请检查网关日志或重新刷新。',
          to: '/providers?status=unknown',
        })
      }
      if (o.routes_total === 0) {
        items.push({ level: 'error', title: '还没有公开模型路由', detail: '客户端目前没有可调用的模型名。', to: '/routes' })
      } else if (o.routes_enabled === 0) {
        items.push({
          level: 'error',
          title: '所有公开模型路由均已停用',
          detail: '上游即使正常，客户端也无法发起模型调用。',
          to: '/routes',
        })
      }
    }
    if (s?.cost_pending_count) {
      items.push({
        level: 'warn',
        title: `${fmtNum(s.cost_pending_count)} 条费用待重算`,
        detail: '这些记录当前按 0 元显示，不能据此判断模型免费。',
        to: '/history',
      })
    }
    if (s && s.total_requests > 0 && s.success_count === 0) {
      items.push({
        level: 'error',
        title: '所选区间没有成功请求',
        detail: `共 ${fmtNum(s.total_requests)} 次调用，其中失败 ${fmtNum(s.error_count)} 次、取消 ${fmtNum(s.canceled_count)} 次。`,
        to: '/history',
      })
    }
    return items
  }, [overview, stats])
  const errorCount = attentionItems.filter((i) => i.level === 'error').length

  const daySeries = useMemo(
    () => (byDay ? buildDaySeries(byDay, stats, rangeKey) : null),
    [byDay, stats, rangeKey],
  )
  // 横轴最多显示约 12 个标签，其余弱化。步长按实际桶数自适应。
  const labelStride = Math.max(1, Math.ceil((daySeries?.length ?? 1) / 12))
  const maxTokens = Math.max(1, ...(daySeries ?? []).map((d) => d.tokens))
  // 普通用户选「全部」档拿不到 stats.lifetime（admin-only），起点退到 byDay
  // 最早一天（后端默认只返回 100 行）。标记出来是为了让界面如实说明覆盖窗口。
  const lifetimeTruncated =
    rangeKey === 'all' && !(stats?.lifetime?.first_record_at && stats.lifetime.first_record_at > 0) && (byDay?.length ?? 0) > 0

  // 账号级余额与配额不等于密钥、白名单或上游的完整可用性。
  const readiness = useMemo(() => {
    if (admin || !stats) return null
    const unlimited = stats.balance_unlimited ?? false
    const cents = stats.balance_cents ?? 0
    const quota = stats.quota_tokens ?? 0
    const used = stats.used_tokens ?? 0
    if (!unlimited && cents <= 0) {
      return {
        tone: 'err' as const,
        title: '现在发请求会被拒绝（余额已耗尽）',
        detail: '调用会返回 402。就余额和额度而言，需先充值才能继续。',
        linkTo: '/wallet',
        linkText: '去充值',
      }
    }
    if (quota > 0 && used >= quota) {
      return {
        tone: 'err' as const,
        title: '现在发请求会被拒绝（额度已用尽）',
        detail: `已用 ${fmtTokens(used)} / 额度 ${fmtTokens(quota)} tokens。请联系管理员调整账号配额，换密钥不会恢复账号额度。`,
        linkTo: '/wallet',
        linkText: '查看账号配额',
      }
    }
    if (quota > 0 && used > 0) {
      const pct = Math.round((used / quota) * 100)
      if (pct >= 80) {
        return {
          tone: 'warn' as const,
          title: `额度即将用尽（已用 ${pct}%）`,
          detail: `已用 ${fmtTokens(used)} / 额度 ${fmtTokens(quota)} tokens。`,
          linkTo: '/wallet',
          linkText: '查看账号配额',
        }
      }
    }
    return {
      tone: 'ok' as const,
      title: '就余额和额度而言，现在可以调用',
      detail: '密钥是否启用、模型是否在你的白名单内，请分别查看密钥页与可用模型页。',
      linkTo: '',
      linkText: '',
    }
  }, [admin, stats])

  const readinessCls =
    readiness?.tone === 'err'
      ? 'border-danger/30 bg-danger-soft'
      : readiness?.tone === 'warn'
        ? 'border-warning/30 bg-warning-soft'
        : 'border-success/30 bg-success-soft'
  const readinessDot =
    readiness?.tone === 'err' ? 'bg-danger' : readiness?.tone === 'warn' ? 'bg-warning' : 'bg-success'

  const successRatio = useMemo(() => {
    if (!stats) return -1
    const finished = stats.success_count + stats.error_count
    return finished > 0 ? stats.success_count / finished : -1
  }, [stats])

  const quota = stats?.quota_tokens ?? 0
  const quotaUsed = stats?.used_tokens ?? 0
  const quotaPct = quota > 0 ? Math.min(100, Math.round((quotaUsed / quota) * 100)) : 0

  return (
    <>
      <PageHeader
        title={admin ? '网关总览' : '我的总览'}
        sub={admin ? '现在正常吗、今天跑了多少——细节在第二屏与各页' : '我的调用、费用与账户状态'}
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
            <Button variant="secondary" size="sm" isDisabled={statsLoading && gatewayLoading} onPress={refresh}>
              刷新
            </Button>
          </>
        }
      />

      {/* 第一屏 · 状态条（admin）：网关侧「现在正常吗」，不随统计范围变化。 */}
      {admin ? (
        <section aria-label="当前状态" className="mb-3">
          {gatewayLoading && !overview ? (
            <div className="grid grid-cols-2 gap-3 lg:grid-cols-4" role="status" aria-label="正在读取当前配置">
              {Array.from({ length: 4 }, (_, i) => (
                <div key={i} className="flex min-h-[104px] flex-col gap-2 rounded-2xl border border-separator bg-background-secondary p-4">
                  <SkeletonBlock className="h-3 w-16" />
                  <SkeletonBlock className="h-8 w-24" />
                  <SkeletonBlock className="h-3 w-20" />
                </div>
              ))}
            </div>
          ) : overview ? (
            <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
              <StatusCell to="/providers" label="上游就绪" ratio={overview.providers_enabled > 0 ? overview.providers_ready / overview.providers_enabled : -1}
                value={`${fmtNum(overview.providers_ready)} / ${fmtNum(overview.providers_enabled)}`}
                foot={overview.providers_unready ? `${fmtNum(overview.providers_unready)} 个未就绪` : overview.providers_unknown ? `${fmtNum(overview.providers_unknown)} 个状态未知` : '已启用上游全部就绪'}
                footWarn={!!overview.providers_unready}
              />
              <StatusCell to="/routes" label="公开模型" ratio={overview.routes_total > 0 ? overview.routes_enabled / overview.routes_total : -1}
                value={`${fmtNum(overview.routes_enabled)} / ${fmtNum(overview.routes_total)}`}
                foot={`上游模型 ${fmtNum(overview.upstream_models_total)} 个`}
              />
              <StatusCell
                label="需要处理"
                ratio={-1}
                value={errorCount > 0 ? String(errorCount) : '✓'}
                valueError={errorCount > 0}
                foot={attentionItems.length ? `${fmtNum(attentionItems.length)} 项待办 · 详见下方` : '没有发现需要处理的问题'}
                footWarn={attentionItems.length > 0}
              />
              <div className="flex min-h-[104px] flex-col rounded-2xl border border-separator bg-background-secondary p-4">
                <span className="text-xs text-muted">累计用量</span>
                <strong className="mt-1 text-2xl font-semibold leading-none tabular-nums">
                  {lifetimeTokens === null ? '—' : fmtTokens(lifetimeTokens)}
                  <small className="ml-1.5 text-xs font-normal text-muted">tokens</small>
                </strong>
                <span className="mt-auto pt-2 text-[11px] text-muted">
                  累计费用 {lifetimeCost === null ? '—' : '¥' + fmtMoney(lifetimeCost)} · 含已归档历史
                </span>
              </div>
            </div>
          ) : null}
          {gatewayAt && !gatewayLoading ? (
            <p className="mt-1.5 text-right text-[11px] text-muted">状态更新于 {new Date(gatewayAt).toLocaleTimeString()}</p>
          ) : null}
        </section>
      ) : null}

      {statsErr ? (
        <div className="mb-3 flex items-center gap-3 rounded-lg border border-warning/30 bg-warning-soft px-4 py-2.5 text-sm" role="alert">
          <span>
            {stats ? '保留上次成功结果 · ' : ''}
            {statsErr}
          </span>
          <Button variant="tertiary" size="sm" isDisabled={statsLoading} onPress={() => void loadStats(rangeKey)}>
            重试
          </Button>
        </div>
      ) : null}

      {/* 第一屏 · 个人余额与配额（用户） */}
      {!admin && stats ? (
        <>
          {readiness ? (
            <div className={`mb-3 flex items-center gap-3 rounded-xl border px-4 py-3 text-sm ${readinessCls}`} role="status">
              <span className={`h-2 w-2 shrink-0 rounded-full ${readinessDot}`} aria-hidden="true" />
              <div className="min-w-0 flex-1">
                <div className="font-medium">{readiness.title}</div>
                <div className="mt-0.5 text-xs text-muted">{readiness.detail}</div>
              </div>
              {readiness.linkTo ? (
                <Link to={readiness.linkTo} className="shrink-0 text-sm font-medium text-accent hover:underline">
                  {readiness.linkText}
                </Link>
              ) : null}
            </div>
          ) : null}
          <section aria-label="账户状态" className="mb-3 flex flex-wrap items-center justify-between gap-6 rounded-2xl border border-separator bg-background-secondary p-5">
            <div>
              <div className="text-xs text-muted">当前账户余额</div>
              <div className="mt-1 text-3xl font-semibold tabular-nums">{fmtBalance(stats.balance_cents, stats.balance_unlimited)}</div>
              {fmtRemainder(stats.balance_remainder) ? <div className="mt-1 text-xs text-muted">含 {fmtRemainder(stats.balance_remainder)} 待结算</div> : null}
            </div>
            {quota > 0 ? (
              <div className="min-w-[220px] flex-1 sm:max-w-xs">
                <div className="flex items-baseline justify-between text-xs">
                  <span className="text-muted">账号配额</span>
                  <span className="tabular-nums">
                    {fmtTokens(quotaUsed)} / {fmtTokens(quota)}
                  </span>
                </div>
                <ProgressBar value={Math.min(quotaUsed, quota)} maxValue={quota} aria-label="账号配额使用" className="mt-1.5">
                  <ProgressBar.Track className="relative h-2 overflow-hidden rounded-full bg-default-soft">
                    <ProgressBar.Fill style={{ width: `${quotaPct}%` }} />
                  </ProgressBar.Track>
                </ProgressBar>
              </div>
            ) : null}
            <Link to="/wallet" className="text-sm text-accent hover:underline">
              余额与账单 →
            </Link>
          </section>
        </>
      ) : null}

      {/* 第一屏 · KPI：默认「今天」，一眼看出量、质、费。 */}
      <section aria-label="运行数字" className="mb-4">
        <div className="mb-3 flex flex-wrap items-end justify-between gap-4">
          <div>
            <h2 className="text-[17px] font-semibold tracking-tight">{rangeLabel(rangeKey)}的运行数字</h2>
            <p className="mt-1 text-xs text-muted">{admin ? '全站请求、费用与缓存' : '我的请求、费用与缓存'}</p>
          </div>
          <span className="text-[11px] text-muted">
            {statsLoading && stats ? '正在加载所选范围，暂显上次结果' : loadedAt ? '更新于 ' + new Date(loadedAt).toLocaleTimeString() : ''}
          </span>
        </div>
        {statsLoading && !stats ? (
          <div className="grid grid-cols-2 gap-3 lg:grid-cols-4" role="status" aria-label="正在加载统计">
            {Array.from({ length: 4 }, (_, i) => (
              <div key={i} className="flex min-h-[118px] flex-col gap-2.5 rounded-2xl border border-separator bg-background-secondary p-4.5">
                <SkeletonBlock className="h-3 w-14" />
                <SkeletonBlock className="h-8 w-28" />
                <SkeletonBlock className="h-3 w-full" />
                <SkeletonBlock className="h-3 w-3/4" />
              </div>
            ))}
          </div>
        ) : stats ? (
          <>
            <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
              <KpiCard
                title="请求数"
                value={fmtNum(stats.total_requests)}
                note={
                  rangeKey === '1d' && stats7
                    ? `近 7 天日均 ${fmtNum(Math.round(stats7.total_requests / 7))}`
                    : `成功 ${fmtNum(stats.success_count)} · 失败 ${fmtNum(stats.error_count)}${stats.canceled_count ? ` · 取消 ${fmtNum(stats.canceled_count)}` : ''}`
                }
                spark={daySeries && daySeries.length > 1 ? daySeries.map((d) => d.count) : undefined}
              />
              <KpiCard
                title="成功率"
                ring={successRatio}
                value={successRatio >= 0 ? fmtPercent(successRatio) + '%' : '—'}
                error={stats.error_count > 0}
                note={`失败 ${fmtNum(stats.error_count)}${stats.canceled_count ? ` · 取消 ${fmtNum(stats.canceled_count)}` : ''}`}
              />
              <KpiCard
                title="Token 用量"
                value={fmtTokens(stats.total_tokens)}
                note={
                  rangeKey === '1d' && stats7
                    ? `近 7 天日均 ${fmtTokens(Math.round(stats7.total_tokens / 7))}`
                    : `输入 ${fmtTokens(stats.input_tokens)} · 输出 ${fmtTokens(stats.output_tokens)}`
                }
                spark={daySeries && daySeries.length > 1 ? daySeries.map((d) => d.tokens) : undefined}
              />
              <KpiCard
                title="费用"
                value={'¥' + fmtMoney(stats.cost)}
                note={
                  rangeKey === '1d' && stats7
                    ? `近 7 天日均 ¥${fmtMoney(stats7.cost / 7)}${stats.cost_pending_count ? ` · 待重算 ${fmtNum(stats.cost_pending_count)} 条` : ''}`
                    : `按调用当时单价固化${stats.cost_pending_count ? ` · 待重算 ${fmtNum(stats.cost_pending_count)} 条` : ''}`
                }
              />
            </div>
            <div className="mt-3 grid grid-cols-1 divide-y divide-separator rounded-2xl border border-separator bg-background-secondary sm:grid-cols-3 sm:divide-x sm:divide-y-0">
              <PerfCell label="平均首字" value={stats.ttfb_samples ? fmtSec(stats.avg_ttfb_ms) + ' s' : '—'} note={stats.ttfb_samples ? `${fmtNum(stats.ttfb_samples)} 个成功流式样本` : '暂无可用样本'} />
              <PerfCell label="平均输出速度" value={stats.throughput_samples ? fmtSpeed(stats.avg_tokens_per_sec) + ' tok/s' : '—'} note={stats.throughput_samples ? `${fmtNum(stats.throughput_samples)} 个成功样本` : '暂无可用样本'} />
              <PerfCell
                label="缓存命中率"
                value={stats.input_tokens > 0 ? fmtPercent(stats.cache_hit_rate) + '%' : '—'}
                note={`命中 ${fmtTokens(stats.cached_tokens)} · 写入 ${fmtTokens(stats.cached_creation_tokens)} / 输入 ${fmtTokens(stats.input_tokens)}`}
              />
            </div>
          </>
        ) : null}
      </section>

      {/* 第一屏 · 最近请求：管理员盯失败，用户看自己的调用。 */}
      <div className="mb-6">
        <RecentRequests admin={admin} />
      </div>

      {/* 第二屏：趋势 + 待办 + 配置五卡。滚到可见才拉 byDay。 */}
      <div ref={trendRef}>
        <div className="mb-4 grid grid-cols-1 items-stretch gap-3.5 lg:grid-cols-[minmax(0,1.65fr)_minmax(280px,0.75fr)]">
          <SectionCard
            title="Token 用量趋势"
            description={`${rangeLabel(rangeKey)} · ${rangeKey === 'all' ? '每桶 30 天' : rangeKey === '1y' ? '按周聚合' : '按日聚合'}`}
            actions={
              <Link to="/usage?tab=models" className="text-sm text-accent hover:underline">
                按模型分析 →
              </Link>
            }
          >
            {trendErr ? (
              <div className="flex items-center gap-3 rounded-lg border border-warning/30 bg-warning-soft px-4 py-2.5 text-sm" role="alert">
                <span>{trendErr}</span>
                <Button variant="tertiary" size="sm" onPress={() => void loadTrend(rangeKey)}>
                  重试
                </Button>
              </div>
            ) : trendLoading && !daySeries ? (
              <div className="space-y-3" role="status" aria-label="趋势加载中">
                <div className="flex h-40 items-end gap-[3px]">
                  {Array.from({ length: 16 }, (_, i) => (
                    <SkeletonBlock key={i} className="h-full flex-1" />
                  ))}
                </div>
                <SkeletonBlock className="h-3 w-full" />
              </div>
            ) : daySeries && stats ? (
              <>
                {stats.total_requests === 0 ? (
                  <p className="rounded-lg bg-background-tertiary px-4 py-2.5 text-xs text-muted">
                    这个时间范围内没有请求记录。可切换时间范围，或到请求日志查看其他时段。
                  </p>
                ) : null}
                {lifetimeTruncated ? (
                  <p className="mt-2 text-[11px] text-muted">趋势仅覆盖当前返回的最近 {byDay?.length} 个有记录日期，可能不含全部历史。</p>
                ) : null}
                <div className="mt-4 flex h-40 items-end gap-[2px]">
                  {daySeries.map((d) => (
                    <div key={d.key} className="flex h-full min-w-0 flex-1 items-end" title={`${d.tip} · ${fmtNum(d.count)} 次 · ${fmtTokens(d.tokens)} tok`}>
                      <div
                        className={`w-full rounded-t-sm ${d.tokens ? 'bg-accent' : 'bg-default-soft'}`}
                        style={{ height: `${Math.max(2, (d.tokens / maxTokens) * 100)}%` }}
                      />
                    </div>
                  ))}
                </div>
                <div className={`mt-2 flex ${daySeries.length > 20 ? 'gap-0' : 'gap-[2px]'}`}>
                  {daySeries.map((d, i) => (
                    <span key={d.key} className={`min-w-0 flex-1 text-center text-[10px] leading-tight ${i % labelStride !== 0 ? 'text-muted/50' : 'text-muted'}`}>
                      <span className="hidden sm:inline">{d.labFull}</span>
                      <span className="sm:hidden">{d.labShort}</span>
                    </span>
                  ))}
                </div>
                <div className="mt-3 flex flex-wrap gap-x-7 gap-y-3 border-t border-separator pt-4 text-xs text-muted">
                  <span>
                    输入 <strong className="font-medium text-foreground">{fmtTokens(stats.input_tokens)}</strong>
                  </span>
                  <span>
                    输出 <strong className="font-medium text-foreground">{fmtTokens(stats.output_tokens)}</strong>
                  </span>
                  <span>
                    缓存命中 <strong className="font-medium text-foreground">{stats.input_tokens > 0 ? fmtPercent(stats.cache_hit_rate) + '%' : '—'}</strong>
                  </span>
                  <span>
                    费用 <strong className="font-medium text-foreground">¥{fmtMoney(stats.cost)}</strong>
                  </span>
                </div>
              </>
            ) : null}
          </SectionCard>

          {admin ? (
            <SectionCard title="需要处理" description="按影响程度排序">
              {attentionItems.length ? (
                <div className="mt-1 space-y-1.5">
                  {attentionItems.map((item) => (
                    <Link key={item.title} to={item.to} className="grid grid-cols-[8px_minmax(0,1fr)_auto] items-start gap-2.5 rounded-lg px-2.5 py-2.5 hover:bg-background-tertiary">
                      <span className={`mt-1.5 h-1.5 w-1.5 rounded-full ${item.level === 'error' ? 'bg-danger' : 'bg-warning'}`} aria-hidden="true" />
                      <span className="min-w-0">
                        <strong className="text-xs font-semibold">{item.title}</strong>
                        <span className="mt-1 block text-[11px] leading-relaxed text-muted">{item.detail}</span>
                      </span>
                      <span className="mt-0.5 text-muted" aria-hidden="true">
                        →
                      </span>
                    </Link>
                  ))}
                </div>
              ) : (
                <div className="space-y-2 px-2.5 py-5">
                  <span className="grid h-6 w-6 place-items-center rounded-full bg-background-tertiary text-accent">✓</span>
                  <strong className="text-[13px]">没有发现需要处理的问题</strong>
                  <p className="text-[11px] text-muted">上游凭据、公开路由与费用状态均正常。</p>
                </div>
              )}
              <div className="mt-4 flex justify-between gap-2 border-t border-separator pt-3 text-[11px] text-muted">
                <span>最近一次成功请求</span>
                <strong className="text-right font-medium text-foreground">
                  {stats?.last_success_at ? fmtTimeMs(stats.last_success_at) : '所选区间无成功请求'}
                </strong>
              </div>
            </SectionCard>
          ) : (
            <SectionCard title="接下来">
              <div className="space-y-2.5 py-1 text-sm">
                <Link to="/history" className="block rounded-lg px-2.5 py-2 hover:bg-background-tertiary">
                  查看我的调用 →
                </Link>
                <Link to="/usage?tab=models" className="block rounded-lg px-2.5 py-2 hover:bg-background-tertiary">
                  按模型分析用量 →
                </Link>
                <Link to="/keys" className="block rounded-lg px-2.5 py-2 hover:bg-background-tertiary">
                  管理 API 密钥 →
                </Link>
              </div>
            </SectionCard>
          )}
        </div>

        {/* 第二屏 · 配置现状（admin）：配置开关与运行时就绪情况。 */}
        {admin && overview ? (
          <section className="mb-6">
            {providerError ? (
              <div className="mb-2 flex items-center gap-3 rounded-lg border border-warning/30 bg-warning-soft px-4 py-2.5 text-sm" role="alert">
                <span>{providerError}</span>
                <Button variant="tertiary" size="sm" onPress={() => void loadGateway()}>
                  重试
                </Button>
              </div>
            ) : null}
            {overviewError ? (
              <div className="mb-2 flex items-center gap-3 rounded-lg border border-warning/30 bg-warning-soft px-4 py-2.5 text-sm" role="alert">
                <span>当前配置刷新失败，显示上次结果：{overviewError}</span>
                <Button variant="tertiary" size="sm" onPress={() => void loadGateway()}>
                  重试
                </Button>
              </div>
            ) : null}
            <div className="grid grid-cols-2 gap-3 lg:grid-cols-4 xl:grid-cols-5">
              <CurrentCard to="/providers" label="上游" value={fmtNum(overview.providers_enabled)} small={`/ ${fmtNum(overview.providers_total)}`} note="启用 / 共"
                foot={`凭据就绪 ${fmtNum(overview.providers_ready)}${overview.providers_unready ? ` · 未就绪 ${fmtNum(overview.providers_unready)}` : ''}${overview.providers_unknown ? ` · 状态未知 ${fmtNum(overview.providers_unknown)}` : ''}`}
                footWarn={overview.providers_unready > 0}
              />
              <CurrentCard to="/routes" label="公开模型" value={fmtNum(overview.routes_enabled)} small={`/ ${fmtNum(overview.routes_total)}`} note="启用 / 共" foot={`上游模型 ${fmtNum(overview.upstream_models_total)} 个`} />
              <CurrentCard to="/users" label="用户" value={fmtNum(overview.users_enabled)} small={`/ ${fmtNum(overview.users_total)}`} note="启用 / 共" foot="禁用账号无法调用" />
              <CurrentCard to="/keys" label="API Key" value={fmtNum(overview.keys_enabled)} small={`/ ${fmtNum(overview.keys_total)}`} note="启用 / 共" foot="禁用且过期密钥立即失效" />
              <div className="col-span-2 flex min-h-[120px] flex-col rounded-2xl border border-separator bg-background-secondary p-4 lg:col-span-4 xl:col-span-1">
                <span className="text-xs text-muted">凭据异常</span>
                <strong className={`mt-1.5 text-3xl font-semibold leading-none tabular-nums ${overview.providers_unready ? 'text-danger' : 'text-success'}`}>
                  {overview.providers_unready ? fmtNum(overview.providers_unready) : '0'}
                </strong>
                <span className="mt-1.5 text-[11px] text-muted">{overview.providers_unready ? '个上游需要处理' : '全部上游就绪'}</span>
                <span className="mt-auto pt-3 text-[11px] leading-relaxed text-muted">
                  此状态不代表实时推理成功，需结合请求成功率判断。
                </span>
              </div>
            </div>
            <div className="mt-3 space-y-2">
              {unready.slice(0, 3).map((p) => (
                <div key={p.id} className="flex items-center gap-3 rounded-lg bg-background-tertiary px-4 py-3 text-sm">
                  <StatusChip tone="err">未就绪</StatusChip>
                  <div className="min-w-0 flex-1 break-all">
                    <strong>{p.name}</strong>
                    <div className="text-xs text-muted">{p.ready_reason || '未建立可用凭据池，请检查凭据配置'}</div>
                  </div>
                  <Link to={`/providers/${p.id}`} className="shrink-0 text-sm font-medium text-accent hover:underline">
                    检查凭据
                  </Link>
                </div>
              ))}
              {unready.length > 3 ? (
                <Link to="/providers?status=unready" className="inline-block text-sm text-accent hover:underline">
                  查看全部 {unready.length} 个未就绪上游 →
                </Link>
              ) : null}
              {unknown.length ? (
                <div className="flex items-center gap-3 rounded-lg border border-warning/30 bg-warning-soft px-4 py-2.5 text-sm">
                  <span>{unknown.length} 个已启用上游状态未知</span>
                  <Link to="/providers?status=unknown" className="text-accent hover:underline">
                    查看
                  </Link>
                </div>
              ) : null}
            </div>
          </section>
        ) : null}

        {admin ? (
          <div className="flex flex-wrap gap-6 px-0.5 pb-4 text-sm">
            <Link to="/history" className="text-accent hover:underline">
              查看请求日志 →
            </Link>
            <Link to="/usage?tab=models" className="text-accent hover:underline">
              按模型分析用量 →
            </Link>
          </div>
        ) : null}
      </div>
    </>
  )
}

/** 状态条单元格：图形优先（P7）——比例给圆环，异常给红色计数。 */
function StatusCell({
  to,
  label,
  ratio,
  value,
  foot,
  footWarn,
  valueError,
}: {
  to?: string
  label: string
  /** 0–1；<0 不画环 */
  ratio: number
  value: string
  foot: string
  footWarn?: boolean
  valueError?: boolean
}) {
  const body = (
    <>
      <span className="text-xs text-muted">{label}</span>
      <span className="mt-1 flex items-center gap-3">
        <strong className={`text-2xl font-semibold leading-none tabular-nums ${valueError ? 'text-danger' : ''}`}>{value}</strong>
        {ratio >= 0 ? <RingGlyph ratio={ratio} label={label} /> : null}
      </span>
      <span className={`mt-auto pt-2 text-[11px] leading-relaxed ${footWarn ? 'text-warning' : 'text-muted'}`}>{foot}</span>
    </>
  )
  if (to) {
    return (
      <Link to={to} className="flex min-h-[104px] flex-col rounded-2xl border border-separator bg-background-secondary p-4 transition-colors hover:border-accent/45">
        {body}
      </Link>
    )
  }
  return <div className="flex min-h-[104px] flex-col rounded-2xl border border-separator bg-background-secondary p-4">{body}</div>
}

/** 裸圆环（状态条/成功率的图形化）：色阶 ≥0.9 正常、≥0.7 警示、否则危险。 */
function RingGlyph({ ratio, label }: { ratio: number; label: string }) {
  const color = ratio >= 0.9 ? 'text-success' : ratio >= 0.7 ? 'text-warning' : 'text-danger'
  return (
    <div className="relative inline-flex items-center justify-center">
      <ProgressCircle value={Math.min(100, Math.max(0, ratio * 100))} size="sm" aria-label={`${label} ${fmtPercent(ratio)}%`}>
        <ProgressCircle.Track>
          <ProgressCircle.TrackCircle />
          <ProgressCircle.FillCircle className={color} />
        </ProgressCircle.Track>
      </ProgressCircle>
      <span className={`absolute text-[9px] font-semibold tabular-nums ${color}`}>{fmtPercent(ratio)}</span>
    </div>
  )
}

function KpiCard({
  title,
  value,
  note,
  error,
  ring,
  spark,
}: {
  title: string
  value: string
  note: string
  error?: boolean
  /** 0–1 比例；给值时在数字右侧画环。 */
  ring?: number
  /** 桶序列；给值时在卡片底部画迷你趋势线。 */
  spark?: number[]
}) {
  return (
    <div className="flex min-h-[118px] flex-col gap-1.5 rounded-2xl border border-separator bg-background-secondary p-4.5">
      <div className="text-xs text-muted">{title}</div>
      <div className="flex items-center gap-3">
        <span className={`text-[26px] font-semibold leading-none tabular-nums ${error ? 'text-danger' : ''}`}>{value}</span>
        {ring !== undefined ? <RingGlyph ratio={ring} label={title + ' ' + value} /> : null}
        {spark && spark.length > 1 ? <Sparkline values={spark} className="ml-auto h-7 w-20 text-accent/70" title={title + '趋势'} /> : null}
      </div>
      <div className="text-[11px] leading-relaxed text-muted">{note}</div>
    </div>
  )
}

function PerfCell({ label, value, note }: { label: string; value: string; note: string }) {
  return (
    <div className="px-4 py-3">
      <div className="flex items-baseline justify-between gap-3">
        <span className="text-[11px] text-muted">{label}</span>
        <strong className="text-xl font-medium tabular-nums">{value}</strong>
      </div>
      <div className="mt-0.5 text-[10px] text-muted">{note}</div>
    </div>
  )
}

function CurrentCard({
  to,
  label,
  value,
  small,
  note,
  foot,
  footWarn,
}: {
  to: string
  label: string
  value: string
  small: string
  note: string
  foot: string
  footWarn?: boolean
}) {
  return (
    <Link to={to} className="flex min-h-[120px] flex-col rounded-2xl border border-separator bg-background-secondary p-4 transition-colors hover:border-accent/45">
      <span className="text-xs text-muted">{label}</span>
      <strong className="mt-1.5 text-3xl font-semibold leading-none tabular-nums">
        {value}
        <small className="ml-1.5 text-[13px] font-normal text-muted">{small}</small>
      </strong>
      <span className="mt-1.5 text-[11px] text-muted">{note}</span>
      <span className={`mt-auto pt-3 text-[11px] leading-relaxed text-muted ${footWarn ? 'text-warning' : ''}`}>{foot}</span>
    </Link>
  )
}
