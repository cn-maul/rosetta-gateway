import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Link } from 'react-router-dom'
import { Button, Spinner } from '@heroui/react'
import { api, ApiFail, isAdmin } from '../api'
import { unitPrice, cacheUnitPrice } from '../modelPrice'
import type { ModelMeta, ModelPrice } from '../types'
import { PageHeader } from '../ui/PageHeader'
import { SectionCard } from '../ui/SectionCard'

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

  // 单价表与映射表都转 Map 而不用下标：models[i] 与 prices[i] 平行数组，
  // 排序后用下标绑定会「A 模型显示 B 模型的价格」—— 错位比空值更难察觉。
  const [priceMap, setPriceMap] = useState<Map<string, ModelPrice>>(new Map())
  const [metaMap, setMetaMap] = useState<Map<string, ModelMeta>>(new Map())

  /** 展示顺序 = 字典序：同一份集合若每次渲染顺序不同，用户的心智模型会漂移。 */
  const sorted = useMemo(() => [...models].filter(Boolean).sort(), [models])

  const providerOf = useCallback((name: string) => metaMap.get(name)?.provider_name ?? '', [metaMap])
  const priceOf = useCallback((name: string) => priceMap.get(name), [priceMap])

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
  const unsetClass = (unset: boolean) => (unset ? 'font-normal text-muted/70' : 'font-semibold')

  return (
    <>
      <PageHeader
        title="可用模型"
        sub="公开模型清单 · 首选参考价（只读）"
        actions={
          <Button variant="secondary" size="sm" isDisabled={loading} onPress={() => void load()}>
            刷新
          </Button>
        }
      />

      <p className="mb-4 text-xs leading-relaxed text-muted">
        参考价指向首选目标（位置 0）；发生故障转移时，费用以请求日志中的实际目标记录为准。
        {admin ? (
          <Link to="/routes" className="text-accent hover:underline">
            {' '}
            管理模型与路由 →
          </Link>
        ) : null}
      </p>

      <SectionCard title="模型清单" description={sorted.length ? `共 ${sorted.length} 个` : undefined}>
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
              {sorted.map((m) => (
                <article key={m} className="flex flex-col gap-2.5 rounded-2xl border border-separator bg-background-secondary px-4 py-3.5">
                  <div className="min-w-0">
                    <div className="break-all font-mono text-[13.5px] leading-snug font-semibold">{m}</div>
                    <div className="mt-0.5 text-xs text-muted">
                      {providerOf(m) ? `${providerOf(m)} · ` : ''}
                      首选参考价（元 / 百万 tokens）
                    </div>
                  </div>
                  <div className="flex flex-col gap-[3px] text-[12.5px]">
                    <PriceRow label="输入" value={priceCell(priceOf(m)?.price_input)} unset={(priceOf(m)?.price_input ?? 0) <= 0} unsetClass={unsetClass} />
                    <PriceRow label="输出" value={priceCell(priceOf(m)?.price_output)} unset={(priceOf(m)?.price_output ?? 0) <= 0} unsetClass={unsetClass} />
                    {/* 「其中:」前缀不能省：缓存命中是**输入的一部分**
                        （cached ⊆ input），不是额外成本。 */}
                    <PriceRow
                      label="其中:缓存命中"
                      value={cacheUnitPrice(priceOf(m)?.price_input ?? 0, priceOf(m)?.price_cache_hit ?? 0)}
                      unset={(priceOf(m)?.price_cache_hit ?? 0) <= 0 && (priceOf(m)?.price_input ?? 0) <= 0}
                      unsetClass={unsetClass}
                    />
                  </div>
                </article>
              ))}
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
            <li>看不到某个模型不一定是故障：路由被停用、或模型名拼错，都会让它不出现在这份清单里。</li>
          </ul>
        </SectionCard>
      </div>
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
