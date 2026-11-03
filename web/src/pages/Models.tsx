import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import {
  Button,
  Checkbox,
  ListBox,
  ListBoxItem,
  Select,
  SelectIndicator,
  SelectPopover,
  SelectTrigger,
  SelectValue,
  Spinner,
} from '@heroui/react'
import { api, ApiFail, isAdmin } from '../api'
import { RequestGeneration, selectionKey } from '../asyncState'
import { unitPrice, cacheUnitPrice } from '../modelPrice'
import { fmtPercent } from '../fmt'
import type { ModelMeta, ModelPrice, ProbeHealth, ProbeTimelineBucket } from '../types'
import { PageHeader } from '../ui/PageHeader'
import { SectionCard } from '../ui/SectionCard'
import { StatusChip } from '../ui/StatusChip'
import { AppModal } from '../ui/AppModal'
import { HeartbeatBar } from '../components/HeartbeatBar'
import { oneOf, oneOfInt, usePersistedState } from '../ui/usePersistedState'

/**
 * 可用模型（全体可见的只读清单）。每个模型一张卡片，卡内同时显示
 * **单价**与**连通率**。
 *
 * # 为什么把连通率并进这张卡（2026-10-14 页面合并）
 *
 * 合之前是两个页面：「可用模型」按**公开路由名**列价格，「连通率」按
 * **upstream_models.id** 画曲线。两者问的其实是同一个问题的两半 ——
 * 「我要用的这个模型，此刻多少钱、此刻通不通」。现在一张卡给全，且
 * **价格与连通率指向同一个上游模型**（后端 meta 与 prices 同取故障转移链
 * 的 position 0，见 group_handler.go）。
 *
 * # 连通率怎么对上号
 *
 * 探测数据按 upstream_models.id 组织，本页清单按公开名组织 —— 主键不同，
 * 靠 model-names 的 meta[i].upstream_model_id 映射。映射**可能缺**，三种
 * 情况与各自该显示什么都必须区分开（见 lineState/lineNote）：
 *   - 该路由解析不出可用目标 → 不画曲线，写明原因；画一条空图等于把
 *     「没配好」呈现成「探测不出问题」。
 *   - 探测端点按身份收窄后拿不到 → 显示「未探测」。
 *   - 拿得到 → 画曲线。
 *
 * # 这页和 ModelPicker 讲的不是同一件事
 *
 * ModelPicker 是**写**白名单的地方（「一个都不选 = 不限制」）。本页回答
 * 的是：**我实际能用哪些模型、多少钱、此刻通不通**。空清单**绝不能**写成
 * 「不限制」—— 那会把「路由没配 / 白名单无交集」说成配置成功；同样不能
 * 给勾选控件，这里没有待保存的状态。
 */

const DAY_OPTIONS = [
  { key: '7', label: '近 7 天', days: 7 },
  { key: '14', label: '近 14 天', days: 14 },
  { key: '30', label: '近 30 天', days: 30 },
]

type LineState = 'ok' | 'no-upstream' | 'probe-failed' | 'not-probed'

function healthTone(h: string): 'ok' | 'err' | 'warn' | 'dim' {
  switch (h) {
    case 'healthy':
      return 'ok'
    case 'down':
      return 'err'
    case 'degraded':
      return 'warn'
    default:
      return 'dim'
  }
}
function healthLabel(h: string): string {
  switch (h) {
    case 'healthy':
      return '正常'
    case 'down':
      return '不可用'
    case 'degraded':
      return '不稳定'
    default:
      return '未探测'
  }
}

export function Models() {
  const admin = isAdmin()

  const pageGenRef = useRef(0)
  const timelineRequests = new RequestGeneration()
  const [models, setModels] = useState<string[]>([])
  /** 清单是否已被所属分组的白名单收窄。false = 这是你此刻能看到的全部。 */
  const [restricted, setRestricted] = useState(false)
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')

  // 单价表与映射表都转 Map 而不用下标：models[i] 与 prices[i] 平行数组，
  // 排序后用下标绑定会「A 模型显示 B 模型的价格」—— 错位比空值更难察觉。
  const [priceMap, setPriceMap] = useState<Map<string, ModelPrice>>(new Map())
  const [metaMap, setMetaMap] = useState<Map<string, ModelMeta>>(new Map())

  const [healths, setHealths] = useState<ProbeHealth[]>([])
  const [probeErr, setProbeErr] = useState('')
  const [probeLoading, setProbeLoading] = useState(false)
  const healthByUpstream = useMemo(() => {
    const m = new Map<string, ProbeHealth>()
    for (const h of healths) m.set(h.model_id, h)
    return m
  }, [healths])

  /** 用户勾选要画曲线的公开模型名。空 = 一条都不画（界面给提示，不自动补）。 */
  const [drawn, setDrawn] = useState<string[]>([])
  const [bucketsByUpstream, setBucketsByUpstream] = useState<Record<string, ProbeTimelineBucket[]>>({})
  // 观测窗口与分桶粒度记在本地：同一轮排查里反复进页面不该每次回到默认档。
  // 都过校验 —— 旧版本写过的天数可能已经不在选项里了。
  const [days, setDays] = usePersistedState<number>('models.days', 7, oneOfInt(DAY_OPTIONS.map((o) => Number(o.key))))
  const [granularity, setGranularity] = usePersistedState<'hour' | 'day'>('models.granularity', 'hour', oneOf(['hour', 'day'] as const))
  const [timelineLoading, setTimelineLoading] = useState(false)
  const timelineSelectionKey = useRef('')
  const [configOpen, setConfigOpen] = useState(false)

  /** 展示顺序 = 字典序：同一份集合若每次渲染顺序不同，用户的心智模型会漂移。 */
  const sorted = useMemo(() => [...models].filter(Boolean).sort(), [models])

  const upstreamIdOf = useCallback((name: string) => metaMap.get(name)?.upstream_model_id ?? '', [metaMap])
  const providerOf = useCallback((name: string) => metaMap.get(name)?.provider_name ?? '', [metaMap])
  const priceOf = useCallback((name: string) => priceMap.get(name), [priceMap])
  const healthOf = useCallback(
    (name: string) => {
      const id = upstreamIdOf(name)
      return id ? healthByUpstream.get(id) : undefined
    },
    [upstreamIdOf, healthByUpstream],
  )

  /** 有探测记录的公开名（按清单顺序），用于默认勾选与「全选」。 */
  const withProbe = useMemo(
    () =>
      sorted.filter((n) => {
        const id = upstreamIdOf(n)
        return id && healthByUpstream.get(id)?.window_count
      }),
    [sorted, upstreamIdOf, healthByUpstream],
  )

  const loadTimeline = useCallback(async () => {
    // ids 每次从最新 drawn / meta 现算（勾选与清单变化都会触发重载）。
    const ids = drawn.map(upstreamIdOf).filter(Boolean)
    const requestedDays = days
    const requestedGranularity = granularity
    const requestedSelection = selectionKey(ids, requestedDays, requestedGranularity)
    const generation = timelineRequests.begin()
    timelineSelectionKey.current = requestedSelection
    if (ids.length === 0) {
      setBucketsByUpstream({})
      setProbeErr('')
      setTimelineLoading(false)
      return
    }
    setTimelineLoading(true)
    setProbeErr('')
    try {
      const all = await api.probeTimeline(ids, requestedDays, requestedGranularity)
      const currentKey = selectionKey(drawn.map(upstreamIdOf).filter(Boolean), days, granularity)
      if (!timelineRequests.isCurrent(generation) || timelineSelectionKey.current !== currentKey) return
      const grouped: Record<string, ProbeTimelineBucket[]> = {}
      for (const b of all) (grouped[b.model_id] ??= []).push(b)
      setBucketsByUpstream(grouped)
    } catch (e) {
      const currentKey = selectionKey(drawn.map(upstreamIdOf).filter(Boolean), days, granularity)
      if (!timelineRequests.isCurrent(generation) || timelineSelectionKey.current !== currentKey) return
      // 失败必须**可见**：曲线区会显示成「没有记录」，而那与「真的没记录」
      // 含义完全相反 —— 用户会把一次网关故障读成模型停了。
      setProbeErr('加载连通率失败：' + (e instanceof Error ? e.message : String(e)))
    } finally {
      if (timelineRequests.isCurrent(generation)) setTimelineLoading(false)
    }
  }, [drawn, days, granularity, upstreamIdOf])

  // 勾选、时间范围、粒度任一变化都走这里重载；挂载时 drawn 为空是合法
  // no-op。旧版在每个事件处理器里手动调 loadTimeline，React 版由依赖驱动。
  useEffect(() => {
    void loadTimeline()
  }, [loadTimeline])

  /** 默认画前 6 个有数据的模型：后端单查询上限 24，一屏 24 张曲线既看不清
   *  也用不上（同一供应商的曲线往往同形）。 */
  const defaultDrawn = useCallback(() => {
    setDrawn(withProbe.slice(0, 6))
  }, [withProbe])

  const loadProbes = useCallback(
    async (page: number) => {
      const generation = timelineRequests.begin()
      setProbeLoading(true)
      try {
        // 探测摘要可能失败（老版本后端没这个端点、或权限收窄）。这不该让
        // 整页进错误态 —— 价格仍然完全可用。失败只记 probeErr，连通率一行
        // 显示原因，其余照常。
        const nextHealths = await api.probeModels()
        if (page !== pageGenRef.current || !timelineRequests.isCurrent(generation)) return
        setHealths(nextHealths)
        setProbeErr('')
        setDrawn((prev) => (prev.length > 0 ? prev : withProbe.slice(0, 6)))
      } catch (e) {
        if (!timelineRequests.isCurrent(generation)) return
        if (e instanceof ApiFail && e.status === 401) return
        setProbeErr('加载连通率失败：' + (e instanceof Error ? e.message : String(e)))
      } finally {
        if (page === pageGenRef.current && timelineRequests.isCurrent(generation)) setProbeLoading(false)
      }
    },
    [withProbe],
  )

  const load = useCallback(async () => {
    const generation = ++pageGenRef.current
    timelineRequests.invalidate()
    setLoading(true)
    setErr('')
    try {
      const r = await api.modelNames()
      if (generation !== pageGenRef.current) return
      setModels(r.models)
      setRestricted(r.restricted)
      setPriceMap(new Map(r.prices.map((p) => [p.name, p])))
      setMetaMap(new Map(r.meta.map((m) => [m.name, m])))
      // 清单变了就重算默认勾选：否则会拿着上一轮的模型名去查曲线。
      defaultDrawn()
      await loadProbes(generation)
    } catch (e) {
      if (generation !== pageGenRef.current) return
      if (e instanceof ApiFail && e.status === 401) return
      // 失败必须留下可见错误态：只弹 toast 的话，页面会渲染成空清单，
      // 而空清单在这页有明确含义（「你一个模型都调不了」）。
      setErr('加载可用模型失败：' + (e instanceof Error ? e.message : String(e)))
    } finally {
      if (generation === pageGenRef.current) setLoading(false)
    }
  }, [defaultDrawn, loadProbes])

  useEffect(() => {
    void load()
    // 只在挂载时拉一次：本页是只读清单，没有外部输入会改变它。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const lineState = (name: string): LineState => {
    if (!upstreamIdOf(name)) return 'no-upstream'
    if (probeErr) return 'probe-failed'
    if (!healthOf(name)?.window_count) return 'not-probed'
    return 'ok'
  }

  // 四种状态四种话，每种都指明「该找谁 / 该修哪里」：统一成「暂无数据」
  // 会让管理员无法判断这是配置问题、网关问题，还是刚重启还没探测。
  const lineNote = (name: string): string => {
    switch (lineState(name)) {
      case 'no-upstream':
        return '无可用上游 · 该路由没有指向已启用的上游模型'
      case 'probe-failed':
        return '读取失败 · 探测数据暂时取不到（不影响单价）'
      case 'not-probed':
        return '未探测 · 后台探测尚未覆盖到这个模型'
      default:
        return ''
    }
  }

  // 跨度与桶宽都必须跟着 granularity 走：旧版曾把跨度写成一次性 const、
  // 桶宽写死 60 分钟，切档后整张图是错的。
  const bucketMinutes = granularity === 'day' ? 1440 : 60
  const curveFor = (name: string): ProbeTimelineBucket[] => {
    const id = upstreamIdOf(name)
    return id ? (bucketsByUpstream[id] ?? []) : []
  }

  const priceCell = (v: number | undefined) => unitPrice(v ?? 0)
  const unsetClass = (unset: boolean) => (unset ? 'font-normal text-muted/70' : 'font-semibold')

  return (
    <>
      <PageHeader
        title="可用模型"
        sub="公开模型清单 · 首选参考价与首选目标探测（只读）"
        actions={
          <>
            <Select
              aria-label="连通率时间范围"
              className="w-[110px]"
              selectedKey={String(days)}
              onSelectionChange={(key) => {
                if (key !== null) setDays(Number(key))
              }}
            >
              <SelectTrigger>
                <SelectValue />
                <SelectIndicator />
              </SelectTrigger>
              <SelectPopover>
                <ListBox aria-label="时间范围选项">
                  {DAY_OPTIONS.map((o) => (
                    <ListBoxItem key={o.key} id={o.key}>
                      {o.label}
                    </ListBoxItem>
                  ))}
                </ListBox>
              </SelectPopover>
            </Select>
            <Select
              aria-label="连通率分桶粒度"
              className="w-[100px]"
              selectedKey={granularity}
              onSelectionChange={(key) => {
                if (key !== null) setGranularity(key as 'hour' | 'day')
              }}
            >
              <SelectTrigger>
                <SelectValue />
                <SelectIndicator />
              </SelectTrigger>
              <SelectPopover>
                <ListBox aria-label="分桶粒度选项">
                  <ListBoxItem id="hour">按小时</ListBoxItem>
                  <ListBoxItem id="day">按天</ListBoxItem>
                </ListBox>
              </SelectPopover>
            </Select>
            <Button variant="secondary" size="sm" isDisabled={loading} onPress={() => void load()}>
              刷新
            </Button>
          </>
        }
      />

      <p className="mb-4 text-xs leading-relaxed text-muted">
        参考价与探测均指向首选目标（位置 0）；发生故障转移时，费用以请求日志中的实际目标记录为准。
        {admin ? (
          <Link to="/routes" className="text-accent hover:underline">
            {' '}
            管理模型与路由 →
          </Link>
        ) : null}
      </p>

      <SectionCard
        title="模型清单"
        description={sorted.length ? `共 ${sorted.length} 个` : undefined}
        actions={
          sorted.length ? (
            <Button variant="tertiary" size="sm" onPress={() => setConfigOpen(true)}>
              配置显示曲线（{drawn.length}）
            </Button>
          ) : undefined
        }
      >
        {loading && sorted.length === 0 ? (
          <div className="flex items-center gap-3 py-8 text-sm text-muted" role="status">
            <Spinner size="sm" /> 加载中…
          </div>
        ) : err ? (
          <div className="py-6 text-center text-sm text-danger" role="alert">
            {err}
            <div className="mt-2.5">
              <Button variant="tertiary" size="sm" isDisabled={loading} onPress={() => void load()}>
                重试
              </Button>
            </div>
          </div>
        ) : sorted.length === 0 ? (
          <p className="py-6 text-center text-sm text-muted">
            目前没有可调用的模型。可能的原因：管理员尚未配置路由，或你的分组白名单与现有路由没有交集 ——
            请联系管理员确认。
          </p>
        ) : (
          <>
            {restricted ? (
              <p
                className="mb-4 rounded-lg bg-warning-soft px-3.5 py-2 text-xs leading-relaxed text-warning-soft-foreground"
                role="status"
              >
                这份清单已被你的分组收窄过 —— 下面的模型就是你最多能调用的，其余模型即使知道名字也会被拒绝。
                需要更多模型请联系管理员调整分组白名单。
              </p>
            ) : null}
            <div className="grid grid-cols-[repeat(auto-fill,minmax(min(100%,288px),1fr))] gap-3">
              {sorted.map((m) => {
                const h = healthOf(m)
                const health = h?.health ?? 'unknown'
                const state = lineState(m)
                return (
                  <article key={m} className="flex flex-col gap-2.5 rounded-2xl border border-separator bg-background-secondary px-4 py-3.5">
                    <div className="flex items-start justify-between gap-2.5">
                      <div className="min-w-0 flex-1">
                        <div className="break-all font-mono text-[13.5px] leading-snug font-semibold">{m}</div>
                        <div className="mt-0.5 text-xs text-muted">
                          {providerOf(m) ? `${providerOf(m)} · ` : ''}
                          首选参考价（元 / 百万 tokens）
                        </div>
                      </div>
                      {/* 健康态与近窗成功率贴着模型名右对齐：「这个模型现在
                          正常吗」是扫一眼就该有答案的问题，不该让人先扫过
                          三行单价才看到。 */}
                      <div className="flex flex-none flex-col items-end gap-1 text-right">
                        <StatusChip tone={healthTone(health)}>{healthLabel(health)}</StatusChip>
                        {h?.window_count ? (
                          <span className="text-[11.5px] whitespace-nowrap text-muted tabular-nums">
                            首选探测 {fmtPercent(h.success_rate ?? 0)}%
                          </span>
                        ) : null}
                      </div>
                    </div>
                    <div className="flex flex-col gap-[3px] text-[12.5px]">
                      <PriceRow label="输入" value={priceCell(priceOf(m)?.price_input)} unset={(priceOf(m)?.price_input ?? 0) <= 0} unsetClass={unsetClass} />
                      <PriceRow label="输出" value={priceCell(priceOf(m)?.price_output)} unset={(priceOf(m)?.price_output ?? 0) <= 0} unsetClass={unsetClass} />
                      {/* 「其中:」前缀不能省：缓存命中是**输入的一部分**
                          （cached ⊆ input），不是额外成本。缓存写入价 2026-10-31
                          起不再展示 —— 它未配置时按输入价计，单独列一行只会让
                          人误以为「这里有笔没填的钱」。 */}
                      <PriceRow
                        label="其中:缓存命中"
                        value={cacheUnitPrice(priceOf(m)?.price_input ?? 0, priceOf(m)?.price_cache_hit ?? 0)}
                        unset={(priceOf(m)?.price_cache_hit ?? 0) <= 0 && (priceOf(m)?.price_input ?? 0) <= 0}
                        unsetClass={unsetClass}
                      />
                    </div>
                    {/* 连通率区只在真的要画或要解释画不出来时才出现：未勾选的
                        模型整块不渲染，否则卡片底部凭空多一道空分隔线。 */}
                    {drawn.includes(m) ? (
                      <div className="flex flex-col gap-1.5 border-t border-separator pt-2.5">
                        {state === 'ok' ? (
                          timelineLoading ? (
                            <div className="py-1 text-[11.5px] text-muted">加载中…</div>
                          ) : (
                            <HeartbeatBar buckets={curveFor(m)} hours={days * 24} bucketMinutes={bucketMinutes} />
                          )
                        ) : (
                          <div className="py-1 text-[11.5px] text-muted">{lineNote(m)}</div>
                        )}
                      </div>
                    ) : null}
                  </article>
                )
              })}
            </div>
          </>
        )}
      </SectionCard>

      <div className="mt-4">
        <SectionCard title="关于这份清单">
        <ul className="list-disc space-y-1.5 pl-5 text-[12.5px] leading-relaxed text-muted">
          <li>
            清单由网关按你的身份自动算出，<b className="font-semibold text-foreground">本页只读</b>
            ：能改的地方是「访问密钥」页的模型白名单（只能收紧）和管理员的「分组」页（按组收紧）。
          </li>
          {admin ? (
            <li>你是管理员，看到的是全部已配置的模型名，不受分组白名单约束。</li>
          ) : restricted ? (
            <li>你被分在一个收窄过的分组里，清单已被该分组的白名单过滤。</li>
          ) : (
            <li>你不在任何受限制的分组里，因此这份清单就是全部已配置的模型名。</li>
          )}
          <li>
            密钥上如果单独设了白名单，最终范围取<b className="font-semibold text-foreground">两者取交集</b>，只会比这里更严。
          </li>
          <li>
            单价单位<b className="font-semibold text-foreground">元 / 百万 tokens</b>
            ：输入按「未命中缓存」计，缓存命中另有更低的一档（留空则回退到输入价）。
          </li>
          <li>
            标「<b className="font-semibold text-foreground">未配置</b>」表示这一档还没填单价，<b className="font-semibold text-foreground">不等于免费</b>
            {' '}
            —— 未配置时该项按 0 元计入，实际花费请以「调用历史」里的记录为准。
          </li>
          <li>
            连通率来自后台定时探测（<b className="font-semibold text-foreground">max_tokens=1</b>
            、不扣网关用户的钱），每张卡仅代表目标链位置 0 的<b className="font-semibold text-foreground">首选目标</b>
            ，不能代表整条路由的可用性；故障转移后的费用按实际目标记录。
          </li>
          <li>看不到某个模型不一定是故障：路由被停用、或模型名拼错，都会让它不出现在这份清单里。</li>
        </ul>
        </SectionCard>
      </div>

      <AppModal open={configOpen} onClose={() => setConfigOpen(false)} title="配置显示曲线的模型" maxWidth="620px" dismissable={false}>
        <div className="flex max-h-[60vh] flex-col gap-0.5 overflow-y-auto">
          {sorted.map((m) => (
            <Checkbox
              key={m}
              isSelected={drawn.includes(m)}
              onChange={() => setDrawn((prev) => (prev.includes(m) ? prev.filter((x) => x !== m) : [...prev, m]))}
              className="w-full"
            >
              <Checkbox.Content className="hover:bg-default-soft w-full gap-2 rounded-md px-1 py-[5px]">
                <Checkbox.Control>
                  <Checkbox.Indicator />
                </Checkbox.Control>
                <span className="min-w-0 flex-1 truncate font-mono text-[12.5px]">{m}</span>
                <StatusChip tone={healthTone(healthOf(m)?.health ?? 'unknown')}>{healthLabel(healthOf(m)?.health ?? 'unknown')}</StatusChip>
              </Checkbox.Content>
            </Checkbox>
          ))}
        </div>
        <p className="mt-2 text-[11.5px] leading-relaxed text-muted">
          勾选后会在卡片里画出这段时间的连通率曲线。没勾选的模型仍然显示单价，只是不画曲线。
        </p>
        <div className="mt-3.5 flex justify-end gap-2 border-t border-separator pt-3">
          <Button variant="ghost" size="sm" onPress={() => setDrawn([...withProbe])}>
            全选有数据的
          </Button>
          <Button variant="ghost" size="sm" onPress={() => setDrawn([])}>
            清空
          </Button>
          <Button variant="primary" size="sm" onPress={() => setConfigOpen(false)}>
            完成
          </Button>
        </div>
      </AppModal>
    </>
  )
}

function PriceRow({
  label,
  value,
  unset,
  unsetClass,
}: {
  label: string
  value: string
  unset: boolean
  unsetClass: (unset: boolean) => string
}) {
  return (
    <div className="flex items-baseline justify-between gap-2.5">
      <span className="whitespace-nowrap text-muted">{label}</span>
      <span className={`text-right font-mono tabular-nums ${unsetClass(unset)}`}>{value}</span>
    </div>
  )
}
