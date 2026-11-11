import { useCallback, useEffect, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
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
} from '@heroui/react'
import { api } from '../api'
import { RequestGeneration } from '../asyncState'
import { fmtNum, fmtPercent, fmtSec, fmtSpeed, fmtTokens } from '../fmt'
import * as timeRange from '../range'
import { oneOf, usePersistedState } from '../ui/usePersistedState'
import type { Stats, UsageGroupEntry } from '../types'
import { PageHeader } from '../ui/PageHeader'
import { SectionCard } from '../ui/SectionCard'
import { BarChart } from '../ui/BarChart'
import { SkeletonBlock } from '../ui/SkeletonBlock'
import { UsageFailuresPanel } from '../components/UsageFailuresPanel'
import { buildUsageSeries } from '../usageSeries'

/**
 * 网关总览：**只看七个指标**（都按「所选区间」口径）外加一张按天的趋势表。
 *
 * 2026-10-31 按用户要求重写：上一版的状态条 / 配置五卡 / 待办卡 / 最近请求 /
 * 余额卡 / 柱状图全部移除 —— 那些内容各有专页，总览只回答
 * 「这段时间跑了多少、快不快、稳不稳」。
 *
 * # 七项指标与它们的口径
 *
 *   1. 请求数        total_requests（含失败与取消，即「来了多少请求」）
 *   2. Token 用量     total_tokens（输入 + 输出）
 *   3. 成功率         success / (success + error)。**取消不算分母** ——
 *                    canceled 是客户端主动断开，既不是上游故障也不是网关失败，
 *                    算进去会让成功率随用户按 Ctrl-C 的次数浮动
 *   4. 缓存命中率     cached_tokens / input_tokens（分母只取输入侧）
 *   5. TPS            成功请求的加权输出速度（token/s）
 *   6. 平均首字延迟    等多久开始吐字
 *   7. 生成耗时        从首字到结束（latency − ttfb，只算成功样本）
 *
 * 第 6、7 项刻意分开给：合成一个「平均总耗时」之后，「慢在等首字」和
 * 「慢在吐字」在界面上分不出来，而处理方式完全不同（换路由 vs 换更快的
 * 模型 / 调小 max_tokens）。两数相加 ≈ 平均总耗时，卡片副行直接给这个和，
 * 省得用户自己算。
 *
 * 带样本数的指标（TPS / 首字 / 生成）在样本为 0 时显示「—」而不是 0：
 * 「没有样本」与「快到 0 秒」是两件事，显示成 0 会让人以为链路飞快。
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

// 区间口径统一走 range.ts（days=1 = 今天 0 点起算，不是滚动 24 小时）。
// 指标卡与趋势表必须共用同一个起点函数，否则会出现「合计是今天、表里却
// 有昨天一行」的口径漂移 —— 这两块现在是同一个数字的两种视图。
const bounds = (key: string) => timeRange.rangeBounds(rangeDays(key))

/** 成功率：分母只算「有结论」的请求（成功 + 失败），取消单独看。 */
function successRate(s: Stats): number {
  const denom = s.success_count + s.error_count
  return denom > 0 ? s.success_count / denom : 0
}

export function Overview() {
  const [searchParams, setSearchParams] = useSearchParams()

  // 上次看的时间范围记在本地：下次进来还是同一档。URL 仍优先，
  // 带 ?range= 的链接行为完全不变。
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
  const trendGen = useRef(new RequestGeneration())
  const [stats, setStats] = useState<Stats | null>(null)
  const [statsErr, setStatsErr] = useState('')
  const [statsLoading, setStatsLoading] = useState(true)
  const [byDay, setByDay] = useState<UsageGroupEntry[] | null>(null)
  const [trendErr, setTrendErr] = useState('')
  const [trendLoading, setTrendLoading] = useState(true)

  // 两个数据源各自加载、各自报错：区间合计失败不该把趋势表也清空，反之亦然
  //（其中一个 500 时用户仍能看到另一半，并知道坏的是哪一半）。
  const loadStats = useCallback(async (requested: string) => {
    const generation = statsGen.current.begin()
    setStatsLoading(true)
    setStatsErr('')
    try {
      const { from, to } = bounds(requested)
      // 「全部」档要终身累计：明细剪枝之后 byDay 只剩最近 N 天，图的起点得靠
      // lifetime.first_record_at，否则更早的历史整段从图上消失。
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

  useEffect(() => {
    void loadStats(rangeKey)
    void loadTrend(rangeKey)
  }, [loadStats, loadTrend, rangeKey])

  const refresh = () => {
    void loadStats(rangeKey)
    void loadTrend(rangeKey)
  }

  return (
    <>
      <PageHeader
        title="总览"
        sub={`${rangeLabel(rangeKey)}的调用量与性能，逐日走势见下方图表`}
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

      {statsErr && !stats ? (
        <Alert status="danger" className="mb-3">
          <Alert.Content>
            <Alert.Title>{statsErr}</Alert.Title>
          </Alert.Content>
          <Button size="sm" variant="tertiary" isDisabled={statsLoading} onPress={() => void loadStats(rangeKey)}>
            重试
          </Button>
        </Alert>
      ) : null}
      {statsErr && stats ? (
        <div className="text-warning mb-3 text-[12.5px]" role="status">
          {statsErr} · 下面是上次成功的结果
        </div>
      ) : null}

      <Metrics stats={stats} loading={statsLoading} />

      <div className="mt-4">
        <SectionCard title={`${rangeLabel(rangeKey)}用量`}>
          <UsageTrend
            byDay={byDay}
            days={rangeDays(rangeKey)}
            firstRecordAt={stats?.lifetime?.first_record_at ?? 0}
            loading={trendLoading}
            error={trendErr}
            onRetry={() => void loadTrend(rangeKey)}
          />
        </SectionCard>
      </div>

      {/* 失败原因排在用量趋势之后：总览的主线是「跑了多少」，这一块回答
          「坏在哪」，放在最后作为收尾。它自己取数、自己报错，与上面两个
          数据源互不影响 —— 失败查询挂了不该让用量数字也跟着空掉。 */}
      <div className="mt-4">
        <SectionCard title="失败原因">
          <UsageFailuresPanel days={rangeDays(rangeKey)} />
        </SectionCard>
      </div>
    </>
  )
}

/**
 * 用量趋势：**一张**柱状图，柱高是当天的 token 用量。
 *
 * 2026-11-01 按用户要求把原来的八列表格换成图表；随后又按「v1.5.3 那样只有
 * 一张图」的要求，把上一版做的七张小图合成一张。理由：总览要回答的是
 * 「这段时间的用量走势」，字段一多就要七张图并排，看的人反而要来回扫；
 * 单张图上「哪天塌了 / 哪天暴涨」一眼可见，其余指标的走势由上面的七张
 * 指标卡给数字，调用日志页按天筛选给精确读数。
 *
 * 柱高用 token 而不是请求数：同一根柱子的悬停提示里两个都给了，而柱高只能
 * 表达一个量 —— token 才是「用了多少资源」，请求数一句话可以多打十个短请求。
 */
function UsageTrend({
  byDay,
  days,
  firstRecordAt,
  loading,
  error,
  onRetry,
}: {
  byDay: UsageGroupEntry[] | null
  days: number
  firstRecordAt: number
  loading: boolean
  error: string
  onRetry: () => void
}) {
  if (loading && !byDay) {
    return (
      <div className="flex flex-col gap-2" role="status" aria-label="趋势加载中">
        <SkeletonBlock className="h-[150px] w-full" />
        <SkeletonBlock className="h-3 w-full" />
      </div>
    )
  }
  if (error && !byDay) {
    return (
      <Alert status="danger">
        <Alert.Content>
          <Alert.Title>{error}</Alert.Title>
        </Alert.Content>
        <Button size="sm" variant="tertiary" isDisabled={loading} onPress={onRetry}>
          重试
        </Button>
      </Alert>
    )
  }

  const rows = byDay ?? []
  const buckets = buildUsageSeries({ byDay: rows, days, firstRecordAt })
  // 最新一根柱子对应的「当天」数字：图形回答形状，数字回答现在。
  const latestRow = rows.length > 0 ? rows[rows.length - 1] : null

  return (
    <>
      {error ? (
        <div className="text-warning mb-3 text-[12.5px]" role="status">
          {error} · 下面是上次成功的结果
        </div>
      ) : null}
      <BarChart label="用量趋势" buckets={buckets} formatTokens={fmtTokens} formatCount={fmtNum} />
      {latestRow ? (
        <div className="text-muted mt-3 flex flex-wrap items-center gap-x-4 gap-y-1 text-[11.5px]">
          <span className="font-mono">{latestRow.key}</span>
          <span>{fmtNum(latestRow.count)} 次调用</span>
          <span>{fmtTokens(latestRow.tokens)} tokens</span>
          <span>
            成功率{' '}
            {latestRow.ok + latestRow.err > 0
              ? `${fmtPercent(latestRow.ok / (latestRow.ok + latestRow.err))}%`
              : '—'}
          </span>
        </div>
      ) : null}
    </>
  )
}

/** 七个指标卡。骨架与真值同尺寸，切换区间时不跳动。 */
function Metrics({ stats, loading }: { stats: Stats | null; loading: boolean }) {
  if (loading && !stats) {
    return (
      <div className="grid grid-cols-2 gap-3 md:grid-cols-3 lg:grid-cols-4" role="status" aria-label="正在读取指标">
        {Array.from({ length: 7 }, (_, i) => (
          <div
            key={i}
            className="flex min-h-[86px] flex-col gap-2 rounded-2xl border border-separator bg-background-secondary p-4"
          >
            <SkeletonBlock className="h-3 w-16" />
            <SkeletonBlock className="h-8 w-24" />
          </div>
        ))}
      </div>
    )
  }
  if (!stats) return null

  const denom = stats.success_count + stats.error_count
  const rate = successRate(stats)

  // 只有数字，不带副行小字。七张卡各挂一行说明，整屏就退化成一列行文，
  // 「一眼扫过七个数字」这件事反而变慢；那行字想说的「样本够不够」，
  // 在主值为 0 样本时已经显「—」讲清楚了。
  return (
    <div className="grid grid-cols-2 gap-3 md:grid-cols-3 lg:grid-cols-4">
      <Metric label="请求数" value={fmtNum(stats.total_requests)} />
      <Metric label="Token 用量" value={fmtTokens(stats.total_tokens)} />
      <Metric
        label="成功率"
        value={denom > 0 ? `${fmtPercent(rate)}%` : '—'}
        tone={denom > 0 && rate < 0.95 ? 'warn' : undefined}
      />
      <Metric
        label="缓存命中率"
        value={stats.input_tokens > 0 ? `${fmtPercent(stats.cache_hit_rate)}%` : '—'}
      />
      <Metric
        label="TPS"
        value={stats.throughput_samples > 0 ? fmtSpeed(stats.avg_tokens_per_sec) : '—'}
      />
      <Metric label="平均首字延迟" value={stats.ttfb_samples > 0 ? `${fmtSec(stats.avg_ttfb_ms)}s` : '—'} />
      <Metric label="生成耗时（首字之后）" value={stats.gen_samples > 0 ? `${fmtSec(stats.avg_gen_ms)}s` : '—'} />
    </div>
  )
}

function Metric({ label, value, tone }: { label: string; value: string; tone?: 'warn' }) {
  return (
    <div className="flex min-h-[86px] flex-col justify-between rounded-2xl border border-separator bg-background-secondary p-4">
      <div className="text-muted text-[12px]">{label}</div>
      <div className={`text-[26px] leading-tight font-semibold tabular-nums ${tone === 'warn' ? 'text-warning' : ''}`}>
        {value}
      </div>
    </div>
  )
}
