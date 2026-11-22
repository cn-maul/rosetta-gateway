import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { Button, Input, Spinner, Table, TextField } from '@heroui/react'
import { Copy } from 'lucide-react'
import { api, ApiFail, isAdmin } from '../api'
import { unitPrice, cacheUnitPrice } from '../modelPrice'
import { copyText } from '../fmt'
import type { ModelMeta, ModelPrice } from '../types'
import { PageHeader } from '../ui/PageHeader'
import { SectionCard } from '../ui/SectionCard'
import { StatusChip } from '../ui/StatusChip'
import { CHART_COLORS, ChartEmpty, ChartTooltipBox, TooltipRow } from '../ui/chart'
import { Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'

/**
 * 可用模型（全体可见的只读清单）。
 *
 * 本页只回答一个问题：**我实际能用哪些模型、各档单价是多少**。
 * 清单由网关按身份算出（分组白名单 ∩ 密钥白名单），因此空清单**绝不能**
 * 写成「不限制」—— 那会把「路由没配 / 白名单无交集」说成配置成功。
 *
 * 这里没有勾选控件：本页没有待保存的状态。
 */

export function Models() {
  const admin = isAdmin()

  const pageGenRef = useRef(0)
  const [models, setModels] = useState<string[]>([])
  /** 清单是否已被所属分组的白名单收窄。false = 这是你此刻能看到的全部。 */
  const [restricted, setRestricted] = useState(false)
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [query, setQuery] = useState('')

  // 单价表与映射表都转 Map 而不用下标：models[i] 与 prices[i] 平行数组，
  // 排序后用下标绑定会「A 模型显示 B 模型的价格」—— 错位比空值更难察觉。
  const [priceMap, setPriceMap] = useState<Map<string, ModelPrice>>(new Map())
  const [metaMap, setMetaMap] = useState<Map<string, ModelMeta>>(new Map())

  /** 展示顺序 = 字典序：同一份集合若每次渲染顺序不同，用户的心智模型会漂移。 */
  const sorted = useMemo(() => [...models].filter(Boolean).sort(), [models])
  const providerOf = useCallback((name: string) => metaMap.get(name)?.provider_name ?? '', [metaMap])
  const priceOf = useCallback((name: string) => priceMap.get(name), [priceMap])
  const visible = useMemo(() => {
    const q = query.trim().toLowerCase()
    if (!q) return sorted
    return sorted.filter((m) => m.toLowerCase().includes(q) || providerOf(m).toLowerCase().includes(q))
  }, [sorted, query, providerOf])

  const load = useCallback(async () => {
    const generation = ++pageGenRef.current
    setLoading(true)
    setErr('')
    try {
      const r = await api.modelNames()
      if (generation !== pageGenRef.current) return
      setModels(r.models)
      setRestricted(r.restricted)
      setPriceMap(new Map(r.prices.map((p) => [p.name, p])))
      setMetaMap(new Map(r.meta.map((m) => [m.name, m])))
    } catch (e) {
      if (generation !== pageGenRef.current) return
      if (e instanceof ApiFail && e.status === 401) return
      // 失败必须留下可见错误态：只弹 toast 的话，页面会渲染成空清单，
      // 而空清单在这页有明确含义（「你一个模型都调不了」）。
      setErr('加载可用模型失败：' + (e instanceof Error ? e.message : String(e)))
    } finally {
      if (generation === pageGenRef.current) setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
    // 只在挂载时拉一次：本页是只读清单，没有外部输入会改变它。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const priceCell = (v: number | undefined) => unitPrice(v ?? 0)

  return (
    <>
      <PageHeader
        title="可用模型"
        actions={
          <Button variant="secondary" size="sm" isDisabled={loading} onPress={() => void load()}>
            刷新
          </Button>
        }
      />

      <SectionCard
        title="模型清单"
        description={sorted.length ? `${sorted.length} 个` : undefined}
        actions={
          admin ? (
            <Link to="/routes" className="text-link text-xs hover:underline">
              管理路由
            </Link>
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
            <div className="mb-3 flex flex-wrap items-center gap-2">
              <TextField aria-label="搜索模型" value={query} onChange={setQuery} className="min-w-[220px] flex-1">
                <Input type="search" placeholder="搜索模型或供应商" autoComplete="off" />
              </TextField>
              <span className="text-muted text-xs">{visible.length} / {sorted.length}</span>
              {restricted ? <StatusChip tone="info">受分组限制</StatusChip> : null}
            </div>
            <Table aria-label="可用模型与参考价">
              <Table.ScrollContainer>
                <Table.Content className="min-w-[760px]">
                  <Table.Header>
                    <Table.Column isRowHeader>模型</Table.Column>
                    <Table.Column>输入</Table.Column>
                    <Table.Column>缓存命中</Table.Column>
                    <Table.Column>输出</Table.Column>
                    <Table.Column>操作</Table.Column>
                  </Table.Header>
                  <Table.Body>
                    {visible.map((m) => (
                      <Table.Row key={m}>
                        <Table.Cell>
                          <span className="font-mono text-[12.5px] break-all">{m}</span>
                          {providerOf(m) ? <span className="text-muted block text-[11.5px]">{providerOf(m)}</span> : null}
                        </Table.Cell>
                        <Table.Cell>
                          <PriceCell value={priceCell(priceOf(m)?.price_input)} unset={(priceOf(m)?.price_input ?? 0) <= 0} />
                        </Table.Cell>
                        <Table.Cell>
                          <PriceCell
                            value={cacheUnitPrice(priceOf(m)?.price_input ?? 0, priceOf(m)?.price_cache_hit ?? 0)}
                            unset={(priceOf(m)?.price_cache_hit ?? 0) <= 0 && (priceOf(m)?.price_input ?? 0) <= 0}
                          />
                        </Table.Cell>
                        <Table.Cell>
                          <PriceCell value={priceCell(priceOf(m)?.price_output)} unset={(priceOf(m)?.price_output ?? 0) <= 0} />
                        </Table.Cell>
                        <Table.Cell>
                          <Button isIconOnly variant="ghost" size="sm" aria-label={`复制模型名 ${m}`} onPress={() => void copyText(m)}>
                            <Copy size={15} aria-hidden />
                          </Button>
                        </Table.Cell>
                      </Table.Row>
                    ))}
                  </Table.Body>
                </Table.Content>
              </Table.ScrollContainer>
            </Table>
            <p className="text-muted mt-2 text-[11.5px]">
              元 / 百万 tokens · 首选目标参考价 · 未配置不等于免费
            </p>
          </>
        )}
      </SectionCard>
      {sorted.length > 0 ? (
        <div className="mt-4">
          <SectionCard
            title="输入与输出单价对比"
            description="按输出价排序，横向比较「同样一百万 token 谁更贵」"
          >
            <PriceMix models={visible} priceOf={priceOf} />
          </SectionCard>
        </div>
      ) : null}
    </>
  )
}

/**
 * 价格对比图：输入价与输出价并排。
 *
 * 这张图只对**同时配置了输入与输出价**的模型出柱：把「未配置」画成 0
 * 会让免费看起来像最便宜，而真相是没人填过价 —— 两种结论的处置动作
 * 完全相反（一个放心用，一个要去找管理员补价格）。
 */
function PriceMix({
  models,
  priceOf,
}: {
  models: string[]
  priceOf: (name: string) => ModelPrice | undefined
}) {
  const data = models
    .map((name) => {
      const p = priceOf(name)
      return {
        label: name,
        input: p?.price_input ?? 0,
        output: p?.price_output ?? 0,
      }
    })
    .filter((d) => d.input > 0 && d.output > 0)
    .sort((a, b) => b.output - a.output)
    .slice(0, 10)
  if (data.length === 0) {
    return <ChartEmpty text="可见模型里还没有同时配置输入与输出价的条目" />
  }
  return (
    <div>
      <div className="h-[260px]" role="img" aria-label={`输入与输出单价最高的 ${data.length} 个模型`}>
        <ResponsiveContainer width="100%" height="100%">
          <BarChart
            data={data}
            layout="vertical"
            margin={{ top: 2, right: 56, bottom: 2, left: 4 }}
            barCategoryGap="28%"
          >
            <CartesianGrid stroke={CHART_COLORS.grid} strokeDasharray="3 5" horizontal={false} />
            <XAxis
              type="number"
              tick={{ fill: CHART_COLORS.muted, fontSize: 10.5 }}
              axisLine={false}
              tickLine={false}
              tickFormatter={(v: number) => `¥${unitPrice(v).replace('¥', '')}`}
            />
            <YAxis
              type="category"
              dataKey="label"
              width={150}
              tick={{ fill: CHART_COLORS.foreground, fontSize: 10.5 }}
              axisLine={false}
              tickLine={false}
              tickFormatter={(value: string) => (value.length > 18 ? value.slice(0, 17) + '…' : value)}
            />
            <Tooltip
              cursor={{ fill: CHART_COLORS.accentSoft }}
              content={({ active, payload }) => {
                if (!active || !payload?.length) return null
                const row = payload[0].payload as (typeof data)[number]
                return (
                  <ChartTooltipBox title={row.label}>
                    <TooltipRow color={CHART_COLORS.accent} label="输入" value={unitPrice(row.input)} />
                    <TooltipRow color={CHART_COLORS.cost} label="输出" value={unitPrice(row.output)} />
                  </ChartTooltipBox>
                )
              }}
            />
            <Bar dataKey="input" fill={CHART_COLORS.accent} radius={[0, 3, 3, 0]} maxBarSize={8} />
            <Bar dataKey="output" fill={CHART_COLORS.cost} radius={[0, 3, 3, 0]} maxBarSize={8} />
          </BarChart>
        </ResponsiveContainer>
      </div>
      <p className="text-muted mt-2 text-[11.5px]">
        元 / 百万 tokens，按输出价降序取前 {data.length} 个；未配置价格的模型不进图，避免把「没填」画成免费。
      </p>
    </div>
  )
}

function PriceCell({
  value,
  unset,
}: {
  value: string
  unset: boolean
}) {
  return (
    <span className={`font-mono text-[12.5px] tabular-nums ${unset ? 'text-muted' : 'font-semibold'}`}>{value}</span>
  )
}
