import { useEffect, useMemo, useRef, useState } from 'react'
import { Button, Checkbox, Input, Table, TextField, toast } from '@heroui/react'
import { api } from '../api'
import { confirmBox } from '../ui/confirm'
import { DataTable } from '../ui/DataTable'
import { SkeletonBlock } from '../ui/SkeletonBlock'
import { fmtNum } from '../fmt'
import { cacheUnitPrice } from '../modelPrice'
import type { Provider, UpstreamModel } from '../types'
import { BatchPriceModal } from './price/BatchPriceModal'
import { PriceEditModal } from './price/PriceEditModal'
import {
  PRICE_PAGE_SIZE,
  PRICE_PROVIDER_GATE,
  ProviderSelect,
  emptyPrices,
  isPriced,
  priceCell,
  type PriceFormState,
  type PriceRow,
} from './price/priceShared'

/**
 * 模型价格（管理员）：按「供应商 × 模型」配置单价（语义对齐旧 Settings.vue
 * 的 price 页签）。价格挂在「供应商的模型」上 —— 同一个 model_id 在不同
 * 供应商下的实际结算价可能不同。
 *
 * 弹窗在 price/ 子目录；供应商数量达到门限时先选供应商再看列表
 * （首屏只渲染一家的条目）。
 */
export function PriceSettings({ refreshSignal }: { refreshSignal: number }) {
  const [providers, setProviders] = useState<Provider[]>([])
  const [modelsMap, setModelsMap] = useState<Record<string, UpstreamModel[]>>({})
  const [loading, setLoading] = useState(false)
  const [loaded, setLoaded] = useState(false)
  const [err, setErr] = useState('')

  const [priceProvider, setPriceProvider] = useState('') // '' = 全部供应商
  const [priceSearch, setPriceSearch] = useState('')
  const [pricePage, setPricePage] = useState(1)
  // 选中集合按模型 id 存（Set）：同一模型在筛选/翻页后仍要保留勾选。
  const [priceSelected, setPriceSelected] = useState<Set<string>>(new Set())

  const [pForm, setPForm] = useState<PriceFormState>({
    open: false, editing: false, providerId: '', modelId: '', ...emptyPrices, saving: false,
  })

  const [batchOpen, setBatchOpen] = useState(false)
  const [batchSeed, setBatchSeed] = useState<PriceRow | null>(null)
  const [batchClearSelection, setBatchClearSelection] = useState(false)
  // 打开瞬间冻结的列表勾选（弹窗初始化用，之后列表勾选变化不影响弹窗）。
  const [batchInitialSelected, setBatchInitialSelected] = useState<Set<string>>(new Set())

  const loadSeq = useRef(0)

  const loadPrices = async () => {
    const seq = ++loadSeq.current
    setLoading(true)
    setErr('')
    try {
      const ps = await api.providers()
      // 一次取回全部模型再本地按 provider_id 分组，替代逐 provider 取模型的 1+N 请求
      const all = await api.allModels()
      if (loadSeq.current !== seq) return
      const grouped: Record<string, UpstreamModel[]> = {}
      for (const m of all) (grouped[m.provider_id] ??= []).push(m)
      setProviders(ps)
      setModelsMap(grouped)
      setLoaded(true)
    } catch (e) {
      if (loadSeq.current !== seq) return
      if ((e as { status?: number }).status === 401) return
      // 失败必须留成可见的 err 态而不是只弹 toast：否则下面的
      // 「还没有任何模型 —— 请先到上游页添加模型」会照常渲染，
      // 把一次接口失败说成「你没配模型」，而处置动作正好相反。
      const msg = '加载模型价格失败：' + (e as Error).message
      setErr(msg)
      toast.danger(msg)
    } finally {
      setLoading(false)
    }
  }

  // 挂载即拉一次（设置页切到价格页签时本组件才挂载），外层「刷新」按钮
  // 通过 refreshSignal 触发重拉。
  useEffect(() => {
    void loadPrices()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refreshSignal])

  const togglePriceSelected = (id: string) => {
    setPriceSelected((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }
  // 全选当前页（不是全部）：批量操作要的是「我眼前这几行」，跨页全选容易
  // 误伤 —— 用户看不到被选中的行，也就无法核对。
  const selectPricePage = () => {
    setPriceSelected((prev) => {
      const next = new Set(prev)
      for (const r of pricePageRows) next.add(r.model.id)
      return next
    })
  }

  // 筛选条件一变就回到第一页：留在旧页码会看到空表（页码已超出新结果集）。
  useEffect(() => {
    setPricePage(1)
  }, [priceProvider, priceSearch])

  const priceRows = useMemo<PriceRow[]>(() => {
    const q = priceSearch.trim().toLowerCase()
    const rows: PriceRow[] = []
    for (const p of providers) {
      if (priceProvider && p.id !== priceProvider) continue
      for (const m of modelsMap[p.id] ?? []) {
        if (!isPriced(m)) continue
        if (q && !m.model_id.toLowerCase().includes(q) && !(m.display_name ?? '').toLowerCase().includes(q)) continue
        rows.push({ providerId: p.id, providerName: p.name || p.slug, model: m })
      }
    }
    return rows
  }, [providers, modelsMap, priceProvider, priceSearch])

  const priceTotalPages = Math.max(1, Math.ceil(priceRows.length / PRICE_PAGE_SIZE))
  const pricePageRows = useMemo(() => {
    const start = (pricePage - 1) * PRICE_PAGE_SIZE
    return priceRows.slice(start, start + PRICE_PAGE_SIZE)
  }, [priceRows, pricePage])

  // 模型总数（跨全部供应商，不随筛选变化）：空态要区分
  // 「一个模型都没有」和「有模型但还没配价」。
  const totalModelCount = useMemo(
    () => providers.reduce((n, p) => n + (modelsMap[p.id]?.length ?? 0), 0),
    [providers, modelsMap],
  )

  // 已勾选的模型行（跨页，用于确认弹窗计数）。
  const selectedPriceRows = useMemo(
    () => priceRows.filter((r) => priceSelected.has(r.model.id)),
    [priceRows, priceSelected],
  )

  // ---------- 新增 / 编辑条目 ----------

  const openAddPrice = () => {
    setPForm({ open: true, editing: false, providerId: '', modelId: '', ...emptyPrices, saving: false })
    // 默认停在「还有模型没配价」的第一个供应商，省掉一次手动切换
    const withStock = providers.find((p) => (modelsMap[p.id] ?? []).some((m) => !isPriced(m)))
    const pid = (withStock ?? providers[0])?.id ?? ''
    setPForm((f) => ({ ...f, providerId: pid }))
  }

  const openEditPrice = (r: PriceRow) => {
    setPForm({
      open: true,
      editing: true,
      providerId: r.providerId,
      modelId: r.model.id,
      price_input: String(r.model.price_input),
      price_cache_hit: String(r.model.price_cache_hit),
      price_output: String(r.model.price_output),
      saving: false,
    })
  }

  const savePrice = async () => {
    if (!pForm.modelId) {
      toast.danger('请选择要设置价格的模型')
      return
    }
    // 三档，缓存写入价不出现（见 priceShared.emptyPrices 的注释）。PATCH 不带
    // 这个字段 = 保持原值，所以改价不会顺手清掉库里已有的缓存写入价。
    const body = {
      price_input: Number(pForm.price_input) || 0,
      price_cache_hit: Number(pForm.price_cache_hit) || 0,
      price_output: Number(pForm.price_output) || 0,
    }
    if (body.price_input < 0 || body.price_cache_hit < 0 || body.price_output < 0) {
      toast.danger('单价不能为负数')
      return
    }
    // 三档全 0 = 不生成条目（列表里不会出现该行），属于「什么都没填」而不是有效价格。
    if (body.price_input === 0 && body.price_cache_hit === 0 && body.price_output === 0) {
      toast.danger('至少填写一项单价')
      return
    }
    setPForm((f) => ({ ...f, saving: true }))
    try {
      await api.updateModel(pForm.modelId, body)
      toast.success('价格已保存')
      setPForm((f) => ({ ...f, open: false }))
      await loadPrices()
    } catch (e) {
      if ((e as { status?: number }).status !== 401) toast.danger('保存失败：' + (e as Error).message)
    } finally {
      setPForm((f) => ({ ...f, saving: false }))
    }
  }

  // 删除价格：行内按钮与编辑弹窗共用。确认后把四项单价清零（= 未配置）。
  // 这里显式带上 price_cache_creation: 0 —— 「删除」是用户对整条价格记录的
  // 明确动作，界面上看不见的第四档也得一起清掉，否则会留一个隐形价继续计费。
  //
  // 文案必须说清「只影响以后」：历史费用在落库时已按当时的价固化，删价不会
  // 把已经算好的金额抹掉。写成「该模型的用量按 0 元计」会让人以为历史费用
  // 会一起归零 —— 那是界面在说谎，且一删就查不回来。
  const deletePrice = async (modelId: string, label: string): Promise<boolean> => {
    const ok = await confirmBox({
      title: `删除「${label}」的价格？`,
      body: '删除后该模型此后的用量按 0 元计；已经产生的费用按当时的价固化，不受影响。',
      danger: true,
      confirmLabel: '删除',
    })
    if (!ok) return false
    try {
      await api.updateModel(modelId, { price_input: 0, price_cache_hit: 0, price_cache_creation: 0, price_output: 0 })
      toast.success('价格已删除')
      return true
    } catch (e) {
      if ((e as { status?: number }).status !== 401) toast.danger('删除失败：' + (e as Error).message)
      return false
    }
  }

  const clearPrice = async (r: PriceRow) => {
    if (await deletePrice(r.model.id, `${r.providerName} / ${r.model.model_id}`)) {
      await loadPrices()
    }
  }

  // 编辑弹窗里的「删除价格」：以**库里已存**的价格为准（不是输入框里的草稿），
  // 否则把输入清空后按钮就消失了，恰恰是在最需要删除的时候。
  const deletePriceFromModal = async () => {
    const m = (modelsMap[pForm.providerId] ?? []).find((x) => x.id === pForm.modelId)
    if (!m) return
    const p = providers.find((x) => x.id === pForm.providerId)
    const label = `${p ? p.name || p.slug : ''} / ${m.model_id}`
    if (!(await deletePrice(m.id, label))) return
    setPForm((f) => ({ ...f, open: false }))
    await loadPrices()
  }

  // ---------- 批量设置价格 ----------

  // 打开批量弹窗。seedRow 是「复制到…」的来源行（用它的价格做初值）。
  //
  // 目标集合取 seedRow（点它就是为了复制到「别的模型」，所以默认**取消**
  // 它自己的勾选 —— 否则「复制」会把它自己也包含进去，覆盖是幂等的所以
  // 不出错，但用户看到「已选 1 个」会以为自己没选对）。没有 seedRow 时
  // （工具条入口）把列表里已勾的搬进来；价格初值用首行兜底。
  const openBatchPrice = (seedRow?: PriceRow) => {
    const priceSource = seedRow ?? selectedPriceRows[0] ?? pricePageRows[0]
    if (!priceSource) {
      toast.danger('没有可用的价格条目')
      return
    }
    setBatchSeed(priceSource)
    setBatchClearSelection(!!seedRow)
    setBatchInitialSelected(new Set(priceSelected))
    setBatchOpen(true)
  }

  const afterBatchSaved = () => {
    setBatchOpen(false)
    setPriceSelected(new Set())
    void loadPrices()
  }

  // ---------- 供应商门控（U2-price）：供应商很多时先选一家再看列表 ----------

  const needsGate = loaded && providers.length >= PRICE_PROVIDER_GATE

  // 门控选择器：供应商卡片（名称 + 模型数 + 已配价数），点选进入列表。
  const gatedProviders = useMemo(() => {
    const q = priceSearch.trim().toLowerCase()
    return providers
      .map((p) => {
        const models = modelsMap[p.id] ?? []
        return { p, modelCount: models.length, pricedCount: models.filter(isPriced).length }
      })
      .filter(({ p }) => !q || (p.name || p.slug).toLowerCase().includes(q))
  }, [providers, modelsMap, priceSearch])

  return (
    <div>
      <div className="mb-4 flex items-start justify-between gap-3">
        <div>
          <div className="text-[15px] font-semibold">模型价格</div>
          <div className="text-muted mt-1 max-w-[620px] text-xs leading-relaxed">
            只列出已配置的价格条目；没配价格的模型不在此列，费用一律按 0 元计。
            单位：元 / 百万 tokens，缓存命中留空则按输入价计。
            同一模型在不同供应商下价格可能不同，按「供应商 × 模型」分别配置。
          </div>
        </div>
        <Button
          variant="primary"
          size="sm"
          isDisabled={!loaded || providers.length === 0}
          onPress={openAddPrice}
        >
          新增价格
        </Button>
      </div>

      {needsGate && !priceProvider ? (
        /* 门控屏：供应商太多时，先选一家再展示条目（列表/搜索/批量入口都压后）。 */
        <div>
          <TextField aria-label="搜索供应商" value={priceSearch} onChange={setPriceSearch} className="mb-3 w-[280px]">
            <Input placeholder={`搜索 ${providers.length} 家供应商…`} autoComplete="off" />
          </TextField>
          <div className="grid max-h-[480px] grid-cols-[repeat(auto-fill,minmax(min(100%,240px),1fr))] gap-2.5 overflow-y-auto pr-1">
            {gatedProviders.map(({ p, modelCount, pricedCount }) => (
              <Button
                key={p.id}
                variant="secondary"
                className="flex h-auto flex-col items-start gap-1 rounded-xl border border-separator bg-background-secondary p-3.5 text-left font-normal hover:border-accent/45"
                onPress={() => setPriceProvider(p.id)}
              >
                <span className="truncate text-sm font-medium">{p.name || p.slug}</span>
                <span className="text-[11px] text-muted">
                  {fmtNum(modelCount)} 个模型 · {fmtNum(pricedCount)} 个已配价
                </span>
              </Button>
            ))}
            {gatedProviders.length === 0 ? (
              <div className="text-muted col-span-full py-8 text-center text-sm">没有匹配的供应商</div>
            ) : null}
          </div>
          <p className="text-muted mt-3 text-[11px]">
            共 {fmtNum(providers.length)} 家供应商 —— 已按「先选供应商」展示；选择后可在此筛选条目、批量设置价格。
          </p>
        </div>
      ) : (
        <>
          <div className="mb-4 flex flex-wrap items-center gap-2.5">
            <ProviderSelect providers={providers} value={priceProvider} onChange={setPriceProvider} ariaLabel="供应商筛选" className="w-[180px]" />
            <TextField aria-label="搜索价格条目" value={priceSearch} onChange={setPriceSearch} className="w-[220px]">
              <Input placeholder="搜索价格条目…" autoComplete="off" />
            </TextField>
            {/* 批量入口。**不再要求先勾选**：弹窗内自带的模型选择器含未配价的模型，
                 而列表只列已配价的 —— 让人「先在这里勾」等于要求目标已经配过价，
                 恰好把批量配价最该解决的场景排除在外。 */}
            <Button variant="secondary" size="sm" onPress={() => openBatchPrice()}>
              批量设置价格
            </Button>
            {priceSelected.size > 0 ? (
              <span className="text-muted text-xs">已勾选 {priceSelected.size} 个（将带入弹窗）</span>
            ) : null}
            {needsGate && priceProvider ? (
              <Button variant="tertiary" size="sm" onPress={() => setPriceProvider('')}>
                返回供应商选择
              </Button>
            ) : null}
            {pricePageRows.length > 0 ? (
              <Button variant="ghost" size="sm" className="ml-auto" onPress={selectPricePage}>
                全选本页
              </Button>
            ) : null}
          </div>

          {err ? (
            <div className="mb-3 flex items-center gap-3 rounded-lg border border-danger/30 bg-danger-soft px-3 py-2 text-sm text-danger-foreground" role="alert">
              {err}
              <Button size="sm" variant="tertiary" isDisabled={loading} onPress={() => void loadPrices()}>
                重试
              </Button>
            </div>
          ) : loaded && totalModelCount === 0 ? (
            <div className="text-muted py-10 text-center text-sm">还没有任何模型 —— 请先在「上游」页添加模型</div>
          ) : priceRows.length === 0 ? (
            loading && !loaded ? (
              <div className="space-y-2" role="status" aria-label="加载中">
                {Array.from({ length: 6 }, (_, i) => (
                  <div key={i} className="flex items-center gap-4 rounded-lg border border-separator px-4 py-3">
                    <SkeletonBlock className="h-4 w-28" />
                    <SkeletonBlock className="h-4 w-44" />
                    <SkeletonBlock className="ml-auto h-4 w-16" />
                    <SkeletonBlock className="h-4 w-16" />
                    <SkeletonBlock className="h-4 w-24" />
                  </div>
                ))}
              </div>
            ) : (
              <div className="text-muted py-10 text-center text-sm">
                {priceProvider || priceSearch.trim()
                  ? '没有匹配的价格条目'
                  : '暂无价格条目 —— 点右上角「新增价格」配置单价；未配置价格的模型，费用一律按 0 元计'}
              </div>
            )
          ) : (
            <>
              <DataTable label="模型价格" hasData loading={loading && !loaded}>
                <Table>
                  <Table.ScrollContainer>
                    <Table.Content aria-label="模型价格">
                      <Table.Header>
                        <Table.Column className="w-8" />
                        <Table.Column isRowHeader>供应商</Table.Column>
                        <Table.Column>模型</Table.Column>
                        <Table.Column className="text-right">输入（元/1M）</Table.Column>
                        <Table.Column className="text-right">缓存命中（元/1M）</Table.Column>
                        <Table.Column className="text-right">输出（元/1M）</Table.Column>
                        <Table.Column className="w-[210px]">操作</Table.Column>
                      </Table.Header>
                      <Table.Body>
                        {pricePageRows.map((r) => (
                          <Table.Row key={r.model.id}>
                            <Table.Cell>
                              <Checkbox
                                aria-label={`选择 ${r.model.model_id}`}
                                isSelected={priceSelected.has(r.model.id)}
                                onChange={() => togglePriceSelected(r.model.id)}
                              />
                            </Table.Cell>
                            <Table.Cell>{r.providerName}</Table.Cell>
                            <Table.Cell>
                              <span className="font-mono">{r.model.model_id}</span>
                              {r.model.display_name ? (
                                <span className="text-muted">（{r.model.display_name}）</span>
                              ) : null}
                            </Table.Cell>
                            <Table.Cell className={r.model.price_input <= 0 ? 'text-right text-muted' : 'text-right'}>
                              {priceCell(r.model.price_input)}
                            </Table.Cell>
                            <Table.Cell
                              className={
                                r.model.price_cache_hit <= 0 && r.model.price_input <= 0
                                  ? 'text-right text-muted'
                                  : 'text-right'
                              }
                            >
                              {cacheUnitPrice(r.model.price_input, r.model.price_cache_hit)}
                            </Table.Cell>
                            <Table.Cell className={r.model.price_output <= 0 ? 'text-right text-muted' : 'text-right'}>
                              {priceCell(r.model.price_output)}
                            </Table.Cell>
                            <Table.Cell>
                              <div className="flex gap-1.5">
                                <Button variant="ghost" size="sm" onPress={() => openEditPrice(r)}>
                                  编辑
                                </Button>
                                {/* 复制到…：以这行的价格为初值打开批量弹窗，在弹窗里勾选
                                     目标模型（含未配价的）。这条路径不依赖列表勾选 —— 列表只列
                                     已配价的模型，而批量配价的意义正是把没配价的也配进来。 */}
                                <Button
                                  variant="ghost"
                                  size="sm"
                                  aria-label={`以 ${r.model.model_id} 的价格为初值，批量应用到其它模型`}
                                  onPress={() => openBatchPrice(r)}
                                >
                                  复制到…
                                </Button>
                                {/* 列表里只有已配价的条目，删除始终可用；删完该行即从列表消失 */}
                                <Button variant="danger" size="sm" onPress={() => void clearPrice(r)}>
                                  删除价格
                                </Button>
                              </div>
                            </Table.Cell>
                          </Table.Row>
                        ))}
                      </Table.Body>
                    </Table.Content>
                  </Table.ScrollContainer>
                </Table>
              </DataTable>

              <div className="mt-3 flex items-center justify-center gap-2">
                <Button variant="secondary" size="sm" isDisabled={pricePage <= 1} onPress={() => setPricePage(1)}>
                  首页
                </Button>
                <Button variant="secondary" size="sm" isDisabled={pricePage <= 1} onPress={() => setPricePage((p) => p - 1)}>
                  上一页
                </Button>
                <span className="text-muted text-xs">
                  第 <b>{pricePage}</b> / {priceTotalPages} 页 · 共 {fmtNum(priceRows.length)} 条
                </span>
                <Button
                  variant="secondary"
                  size="sm"
                  isDisabled={pricePage >= priceTotalPages}
                  onPress={() => setPricePage((p) => p + 1)}
                >
                  下一页
                </Button>
                <Button
                  variant="secondary"
                  size="sm"
                  isDisabled={pricePage >= priceTotalPages}
                  onPress={() => setPricePage(priceTotalPages)}
                >
                  末页
                </Button>
              </div>
            </>
          )}
        </>
      )}

      {/* ---------- 价格：新增 / 编辑 ---------- */}
      <PriceEditModal
        form={pForm}
        onPatch={(patch) => setPForm((f) => ({ ...f, ...patch }))}
        onClose={() => setPForm((f) => ({ ...f, open: false }))}
        providers={providers}
        modelsMap={modelsMap}
        onSave={() => void savePrice()}
        onDelete={() => void deletePriceFromModal()}
      />

      {/* ---------- 批量设置价格 ---------- */}
      <BatchPriceModal
        open={batchOpen}
        onClose={() => setBatchOpen(false)}
        onSaved={afterBatchSaved}
        providers={providers}
        modelsMap={modelsMap}
        seed={batchSeed}
        clearSelection={batchClearSelection}
        initialSelected={batchInitialSelected}
      />
    </div>
  )
}
