import { useEffect, useMemo, useRef, useState } from 'react'
import {
  Button,
  Checkbox,
  ListBox,
  ListBoxItem,
  Select,
  Spinner,
  Table,
  TextField,
  Input,
  Label,
  toast,
} from '@heroui/react'
import { api } from '../api'
import { confirmBox } from '../ui/confirm'
import { AppModal } from '../ui/AppModal'
import { DataTable } from '../ui/DataTable'
import { StatusChip } from '../ui/StatusChip'
import { fmtNum } from '../fmt'
import { unitPrice, cacheUnitPrice } from '../modelPrice'
import type { Provider, UpstreamModel } from '../types'

/**
 * 模型价格（管理员）：按「供应商 × 模型」配置单价（语义对齐旧 Settings.vue
 * 的 price 页签）。价格挂在「供应商的模型」上 —— 同一个 model_id 在不同
 * 供应商下的实际结算价可能不同。
 */

const PRICE_PAGE_SIZE = 10

type PriceRow = { providerId: string; providerName: string; model: UpstreamModel }

// 列表 = **价格条目**：只有配过价的模型才出现在这里。
// 没配价的模型不是「0 元的条目」，而是「没有条目」—— 费用侧本来就按 0 元计，
// 界面上列出它们只会把「新增价格」的入口淹没在一排不可删的行里。
function isPriced(m: UpstreamModel): boolean {
  return m.price_input > 0 || m.price_cache_hit > 0 || m.price_cache_creation > 0 || m.price_output > 0
}

// 单价单元格：条目里的某一档没填（如缓存命中留空=按输入价计）时写「未配置」，
// 不写 0.00 —— 那会被读成「确认这一档免费」。
const priceCell = (v: number): string => unitPrice(v)

interface PriceFormState {
  open: boolean
  /** true = 改既有条目：供应商与模型锁定，只动价格。 */
  editing: boolean
  providerId: string
  modelId: string
  price_input: string
  price_cache_hit: string
  price_cache_creation: string
  price_output: string
  saving: boolean
}

const emptyPrices = { price_input: '', price_cache_hit: '', price_cache_creation: '', price_output: '' }

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
  const [batchSaving, setBatchSaving] = useState(false)
  const [batchProvider, setBatchProvider] = useState('')
  const [batchSearch, setBatchSearch] = useState('')
  const [batchSelected, setBatchSelected] = useState<Set<string>>(new Set())
  const [batchForm, setBatchForm] = useState({ ...emptyPrices })

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

  const modalModels = useMemo(() => modelsMap[pForm.providerId] ?? [], [modelsMap, pForm.providerId])

  // 换供应商必须清掉已选模型：否则上一家的 modelId 会跟着提交，
  // 把价格写到另一个供应商的模型上（两个下拉看着不匹配，数据却落库了）。
  const onPriceProviderChange = (key: React.Key | null) => {
    setPForm((f) => ({ ...f, providerId: key === null ? '' : String(key), modelId: '' }))
  }

  // 新增只列「还没有价格条目」的模型（有条目的去列表里编辑，避免同一条目两处入口）；
  // 编辑时模型锁定，选项就是那一个。
  const modalModelOptions = useMemo(
    () => (pForm.editing ? modalModels : modalModels.filter((m) => !isPriced(m))),
    [pForm.editing, modalModels],
  )

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
      price_cache_creation: String(r.model.price_cache_creation),
      price_output: String(r.model.price_output),
      saving: false,
    })
  }

  const savePrice = async () => {
    if (!pForm.modelId) {
      toast.danger('请选择要设置价格的模型')
      return
    }
    const body = {
      price_input: Number(pForm.price_input) || 0,
      price_cache_hit: Number(pForm.price_cache_hit) || 0,
      price_cache_creation: Number(pForm.price_cache_creation) || 0,
      price_output: Number(pForm.price_output) || 0,
    }
    if (body.price_input < 0 || body.price_cache_hit < 0 || body.price_cache_creation < 0 || body.price_output < 0) {
      toast.danger('单价不能为负数')
      return
    }
    // 四项全 0 = 不生成条目（列表里不会出现该行），属于「什么都没填」而不是有效价格。
    if (body.price_input === 0 && body.price_cache_hit === 0 && body.price_cache_creation === 0 && body.price_output === 0) {
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
  const modalModel = useMemo(() => modalModels.find((m) => m.id === pForm.modelId), [modalModels, pForm.modelId])
  const modalHasPrice = !!modalModel && isPriced(modalModel)

  const deletePriceFromModal = async () => {
    const m = modalModel
    if (!m) return
    const p = providers.find((x) => x.id === pForm.providerId)
    const label = `${p ? p.name || p.slug : ''} / ${m.model_id}`
    if (!(await deletePrice(m.id, label))) return
    setPForm((f) => ({ ...f, open: false }))
    await loadPrices()
  }

  // ---------- 批量设置价格 ----------

  // batchTargets 是弹窗里可选的全部模型（跨 provider 扁平，含未配价的）。
  //
  // # 为什么必须是「全部模型」而不是 priceRows
  //
  // 初版把选择器做在价格列表的行勾选上，而价格列表**只列已配价的模型**。
  // 于是形成死锁：想让一个没配过价的模型拿到价格，就必须先在列表里勾它 ——
  // 但它不在列表里；想让它进列表，就得先给它配价 —— 而配价正是做不到的那
  // 一步。所以选择器搬进弹窗，数据源改成 allModels（含未配价的）。列表行的
  // 勾选仍然保留 —— 它在「批量改一批已有条目」时更顺手。
  const batchTargets = useMemo<PriceRow[]>(() => {
    const q = batchSearch.trim().toLowerCase()
    const out: PriceRow[] = []
    for (const p of providers) {
      if (batchProvider && p.id !== batchProvider) continue
      for (const m of modelsMap[p.id] ?? []) {
        if (q && !m.model_id.toLowerCase().includes(q) && !(m.display_name ?? '').toLowerCase().includes(q)) continue
        out.push({ providerId: p.id, providerName: p.name || p.slug, model: m })
      }
    }
    return out
  }, [providers, modelsMap, batchProvider, batchSearch])

  const toggleBatchSelected = (id: string) => {
    setBatchSelected((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }
  // 「全选」按**当前筛选**（供应商 + 搜索词），不是全库 —— 用户筛了供应商
  // 再点全选，期望的是「把这家全选上」，而不是无视筛选把别家也拉进来。
  const selectAllBatch = () => {
    setBatchSelected((prev) => {
      const next = new Set(prev)
      for (const r of batchTargets) next.add(r.model.id)
      return next
    })
  }

  // batchSelectedModels 按已勾 id 反查模型行（**不受筛选影响**）。
  //
  // 刻意不用 batchTargets 过滤已勾集合：筛选一变，已勾的行就凭空消失，
  // 用户切回原筛选才发现「我刚才勾的怎么没了」。按 id 反查保证勾选稳定。
  const batchSelectedModels = useMemo<PriceRow[]>(() => {
    const byId = new Map<string, PriceRow>()
    for (const p of providers) {
      for (const m of modelsMap[p.id] ?? []) {
        byId.set(m.id, { providerId: p.id, providerName: p.name || p.slug, model: m })
      }
    }
    const out: PriceRow[] = []
    for (const id of batchSelected) {
      const row = byId.get(id)
      if (row) out.push(row)
    }
    return out
  }, [providers, modelsMap, batchSelected])

  // 打开批量弹窗。seedRow 是「复制到…」的来源行（用它的价格做初值）。
  //
  // 目标集合取 seedRow（点它就是为了复制到「别的模型」，所以默认**取消**
  // 它自己的勾选 —— 否则「复制」会把它自己也包含进去，覆盖是幂等的所以
  // 不出错，但用户看到「已选 1 个」会以为自己没选对）。没有 seedRow 时
  // （工具条入口）把列表里已勾的搬进来。
  const openBatchPrice = (seedRow?: PriceRow) => {
    const seed = seedRow ?? selectedPriceRows[0] ?? pricePageRows[0]
    if (!seed) {
      toast.danger('没有可用的价格条目')
      return
    }
    setBatchForm({
      price_input: String(seed.model.price_input),
      price_cache_hit: String(seed.model.price_cache_hit),
      price_cache_creation: String(seed.model.price_cache_creation),
      price_output: String(seed.model.price_output),
    })
    // 筛选重置到「全部」：来源行可能来自某个供应商筛选下，直接沿用会让
    // 用户在弹窗里看不到其它供应商的模型（而跨供应商复制正是主要用途）。
    setBatchProvider('')
    setBatchSearch('')
    setBatchSelected(seedRow ? new Set() : new Set(priceSelected))
    setBatchOpen(true)
  }

  // 提交批量设置。目标 = 弹窗内勾选的全部模型（含未配价的）。
  const saveBatchPrice = async () => {
    const targets = batchSelectedModels
    if (targets.length === 0) {
      toast.danger('请先勾选要设置价格的模型')
      return
    }
    const body = {
      price_input: Number(batchForm.price_input) || 0,
      price_cache_hit: Number(batchForm.price_cache_hit) || 0,
      price_cache_creation: Number(batchForm.price_cache_creation) || 0,
      price_output: Number(batchForm.price_output) || 0,
    }
    if (body.price_input < 0 || body.price_cache_hit < 0 || body.price_cache_creation < 0 || body.price_output < 0) {
      toast.danger('单价不能为负数')
      return
    }
    // 四项全 0 = 清除价格。这里不拦（批量清除是合法动作），但确认文案要讲清。
    const clearing =
      body.price_input === 0 && body.price_cache_hit === 0 && body.price_cache_creation === 0 && body.price_output === 0
    // 确认框里列出前几个模型名：跨供应商批量改价最容易勾错行，名字是唯一
    // 能让用户在点确认前核对「到底要改谁」的信息。
    const preview = targets.slice(0, 3).map((r) => r.model.model_id).join('、')
    const more = targets.length > 3 ? ` 等 ${targets.length} 个` : ''
    const ok = await confirmBox({
      title: clearing ? `清除 ${targets.length} 个模型的价格？` : `为 ${targets.length} 个模型设置价格？`,
      body: clearing
        ? `涉及：${preview}${more}。清除后这些模型此后的用量按 0 元计；已经产生的费用按当时的价固化，不受影响。`
        : `涉及：${preview}${more}。只影响之后的调用，历史费用不回溯。`,
      confirmLabel: clearing ? '清除' : '应用',
    })
    if (!ok) return
    setBatchSaving(true)
    try {
      await api.batchUpdateModelPrices(targets.map((r) => r.model.id), body)
      toast.success(clearing ? `已清除 ${targets.length} 个模型的价格` : `已为 ${targets.length} 个模型设置价格`)
      setBatchOpen(false)
      setPriceSelected(new Set())
      await loadPrices()
    } catch (e) {
      if ((e as { status?: number }).status !== 401) toast.danger('批量设置失败：' + (e as Error).message)
    } finally {
      setBatchSaving(false)
    }
  }

  const PriceField = (
    label: string,
    value: string,
    onChange: (v: string) => void,
    tip?: string,
  ) => (
    <TextField aria-label={label} value={value} onChange={onChange}>
      <Label>{label}</Label>
      <Input type="number" min={0} step="0.001" placeholder="0 = 不计费" />
      {tip ? <span className="text-muted text-[11px]">{tip}</span> : null}
    </TextField>
  )

  const providerSelect = (
    value: string,
    onChange: (v: string) => void,
    ariaLabel: string,
    className?: string,
    disabled?: boolean,
  ) => (
    <Select
      aria-label={ariaLabel}
      className={className}
      isDisabled={disabled}
      selectedKey={value || 'all'}
      onSelectionChange={(key) => {
        if (key === null) return
        onChange(key === 'all' ? '' : String(key))
      }}
    >
      <Select.Trigger>
        <Select.Value />
        <Select.Indicator />
      </Select.Trigger>
      <Select.Popover>
        <ListBox aria-label={ariaLabel}>
          <ListBoxItem id="all">全部供应商</ListBoxItem>
          {providers.map((p) => (
            <ListBoxItem key={p.id} id={p.id}>
              {p.name || p.slug}
            </ListBoxItem>
          ))}
        </ListBox>
      </Select.Popover>
    </Select>
  )

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

      <div className="mb-4 flex flex-wrap items-center gap-2.5">
        {providerSelect(priceProvider, setPriceProvider, '供应商筛选', 'w-[180px]')}
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
      </div>

      {loading && !loaded ? (
        <div className="text-muted flex items-center justify-center gap-2 py-10" role="status">
          <Spinner size="sm" /> 加载中…
        </div>
      ) : err ? (
        <div className="mb-3 flex items-center gap-3 rounded-lg border border-danger/30 bg-danger-soft px-3 py-2 text-sm text-danger-foreground" role="alert">
          {err}
          <Button size="sm" variant="tertiary" isDisabled={loading} onPress={() => void loadPrices()}>
            重试
          </Button>
        </div>
      ) : loaded && totalModelCount === 0 ? (
        <div className="text-muted py-10 text-center text-sm">还没有任何模型 —— 请先在「上游」页添加模型</div>
      ) : priceRows.length === 0 ? (
        <div className="text-muted py-10 text-center text-sm">
          {priceProvider || priceSearch.trim()
            ? '没有匹配的价格条目'
            : '暂无价格条目 —— 点右上角「新增价格」配置单价；未配置价格的模型，费用一律按 0 元计'}
        </div>
      ) : (
        <>
          <DataTable label="模型价格" hasData>
            <Table>
              <Table.ScrollContainer>
                <Table.Content aria-label="模型价格">
                  <Table.Header>
                    <Table.Column className="w-8" />
                    <Table.Column>供应商</Table.Column>
                    <Table.Column>模型</Table.Column>
                    <Table.Column className="text-right">输入（元/1M）</Table.Column>
                    <Table.Column className="text-right">缓存命中（元/1M）</Table.Column>
                    <Table.Column className="text-right">缓存写入（元/1M）</Table.Column>
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
                        <Table.Cell
                          className={
                            r.model.price_cache_creation <= 0 && r.model.price_input <= 0
                              ? 'text-right text-muted'
                              : 'text-right'
                          }
                        >
                          {cacheUnitPrice(r.model.price_input, r.model.price_cache_creation)}
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

      {/* ---------- 价格：新增 / 编辑（表单填一半别丢，遮罩/Esc 都不关） ---------- */}
      <AppModal
        open={pForm.open}
        onClose={() => setPForm((f) => ({ ...f, open: false }))}
        title={pForm.editing ? '编辑模型价格' : '新增模型价格'}
        maxWidth="520px"
        dismissable={false}
        footer={
          <div className="flex w-full items-center justify-between gap-2">
            {/* 库里已有价才给删除入口（以库为准，不看输入框草稿） */}
            {modalHasPrice ? (
              <Button variant="danger" isDisabled={pForm.saving} onPress={() => void deletePriceFromModal()}>
                删除价格
              </Button>
            ) : (
              <span />
            )}
            <div className="flex gap-2">
              <Button variant="ghost" isDisabled={pForm.saving} onPress={() => setPForm((f) => ({ ...f, open: false }))}>
                取消
              </Button>
              <Button variant="primary" isDisabled={pForm.saving || !pForm.modelId} onPress={() => void savePrice()}>
                {pForm.saving ? '保存中…' : '保存'}
              </Button>
            </div>
          </div>
        }
      >
        <form
          onSubmit={(e) => {
            e.preventDefault()
            void savePrice()
          }}
          className="grid grid-cols-2 gap-4"
        >
          <div className="col-span-2 flex flex-col gap-1">
            <Label>供应商 *</Label>
            <Select
              aria-label="供应商"
              isDisabled={pForm.editing}
              selectedKey={pForm.providerId || null}
              onSelectionChange={onPriceProviderChange}
            >
              <Select.Trigger>
                <Select.Value />
                <Select.Indicator />
              </Select.Trigger>
              <Select.Popover>
                <ListBox aria-label="供应商">
                  {providers.map((p) => (
                    <ListBoxItem key={p.id} id={p.id}>
                      {p.name || p.slug}
                    </ListBoxItem>
                  ))}
                </ListBox>
              </Select.Popover>
            </Select>
            <span className="text-muted text-[11px]">同一个 model_id 在不同供应商下价格可能不同</span>
          </div>
          <div className="col-span-2 flex flex-col gap-1">
            <Label>模型 *</Label>
            <Select
              aria-label="模型"
              isDisabled={pForm.editing}
              selectedKey={pForm.modelId || null}
              onSelectionChange={(key) => key !== null && setPForm((f) => ({ ...f, modelId: String(key) }))}
            >
              <Select.Trigger>
                <Select.Value />
                <Select.Indicator />
              </Select.Trigger>
              <Select.Popover>
                <ListBox aria-label="模型">
                  {modalModelOptions.map((m) => (
                    <ListBoxItem key={m.id} id={m.id}>
                      {m.model_id}
                    </ListBoxItem>
                  ))}
                </ListBox>
              </Select.Popover>
            </Select>
            <span className="text-muted text-[11px]">
              {pForm.editing
                ? '条目归属的供应商与模型不可改；要换模型请先删除再新增'
                : modalModels.length > 0 && modalModelOptions.length === 0
                  ? '该供应商下的模型都已有价格条目，请到列表中编辑'
                  : '只列出还没有价格条目的模型'}
            </span>
          </div>
          {PriceField('输入价格（元/1M）', pForm.price_input, (v) => setPForm((f) => ({ ...f, price_input: v })), '缓存未命中的输入 token')}
          {PriceField('缓存命中价格（元/1M）', pForm.price_cache_hit, (v) => setPForm((f) => ({ ...f, price_cache_hit: v })), '留空/填 0 时按输入价计')}
          {PriceField('缓存写入价格（元/1M）', pForm.price_cache_creation, (v) => setPForm((f) => ({ ...f, price_cache_creation: v })), 'Anthropic cache creation；留空/填 0 时按输入价计')}
          {PriceField('输出价格（元/1M）', pForm.price_output, (v) => setPForm((f) => ({ ...f, price_output: v })))}
        </form>
      </AppModal>

      {/* ---------- 批量设置价格：把同一组价格应用到勾选的多个模型。模型选择器在弹窗内（含未配价的模型）。 ---------- */}
      <AppModal
        open={batchOpen}
        onClose={() => setBatchOpen(false)}
        title="批量设置价格"
        maxWidth="640px"
        dismissable={false}
        footer={
          <div className="flex w-full justify-end gap-2">
            <Button variant="ghost" isDisabled={batchSaving} onPress={() => setBatchOpen(false)}>
              取消
            </Button>
            <Button
              variant="primary"
              isDisabled={batchSaving || batchSelectedModels.length === 0}
              onPress={() => void saveBatchPrice()}
            >
              {batchSaving ? '应用中…' : `应用到 ${batchSelectedModels.length} 个模型`}
            </Button>
          </div>
        }
      >
        <form
          onSubmit={(e) => {
            e.preventDefault()
            void saveBatchPrice()
          }}
          className="flex flex-col gap-4"
        >
          <div className="flex flex-col gap-1">
            <span className="text-[13px] font-medium">目标模型（已选 {batchSelectedModels.length} 个）</span>
            <div className="overflow-hidden rounded-lg border border-separator">
              <div className="bg-background-secondary flex items-center gap-1.5 border-b border-separator p-2">
                {providerSelect(batchProvider, setBatchProvider, '批量目标供应商', 'w-[150px]')}
                <TextField aria-label="搜索模型" value={batchSearch} onChange={setBatchSearch} className="flex-1">
                  <Input placeholder="搜索模型…" autoComplete="off" />
                </TextField>
                <Button variant="ghost" size="sm" onPress={selectAllBatch}>
                  全选
                </Button>
                <Button variant="ghost" size="sm" onPress={() => setBatchSelected(new Set())}>
                  清空
                </Button>
              </div>
              <div className="max-h-[240px] overflow-y-auto py-1">
                {batchTargets.map((r) => (
                  <label
                    key={r.model.id}
                    className="hover:bg-hover flex cursor-pointer items-center gap-2 px-2.5 py-1"
                  >
                    <Checkbox
                      aria-label={`选择 ${r.model.model_id}`}
                      isSelected={batchSelected.has(r.model.id)}
                      onChange={() => toggleBatchSelected(r.model.id)}
                    />
                    <span className="truncate font-mono text-[13px]">{r.model.model_id}</span>
                    <span className="text-muted ml-auto shrink-0 text-[11px]">{r.providerName}</span>
                    {/* 已配价的标出来：批量覆盖它们是有意还是误勾，一眼可辨 */}
                    {isPriced(r.model) ? <StatusChip tone="dim">已配价</StatusChip> : null}
                  </label>
                ))}
                {batchTargets.length === 0 ? (
                  <div className="text-muted px-2.5 py-2 text-xs">没有匹配的模型</div>
                ) : null}
              </div>
            </div>
            <span className="text-muted text-[11px]">
              含未配价的模型 —— 一次把同一批模型的价格配齐。四项全 0 = 清除价格。
            </span>
          </div>
          <div className="grid grid-cols-2 gap-4">
            {PriceField('输入价格（元/1M）', batchForm.price_input, (v) => setBatchForm((f) => ({ ...f, price_input: v })))}
            {PriceField('缓存命中价格（元/1M）', batchForm.price_cache_hit, (v) => setBatchForm((f) => ({ ...f, price_cache_hit: v })), '0 = 同输入价')}
            {PriceField('缓存写入价格（元/1M）', batchForm.price_cache_creation, (v) => setBatchForm((f) => ({ ...f, price_cache_creation: v })), '0 = 同输入价')}
            {PriceField('输出价格（元/1M）', batchForm.price_output, (v) => setBatchForm((f) => ({ ...f, price_output: v })), '四项全填 0 = 清除这些模型的价格（不是设为免费）')}
          </div>
        </form>
      </AppModal>
    </div>
  )
}
