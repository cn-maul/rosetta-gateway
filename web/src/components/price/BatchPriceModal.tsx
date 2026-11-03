import { useEffect, useMemo, useState } from 'react'
import { Button, Checkbox, Input, TextField, toast } from '@heroui/react'
import { api } from '../../api'
import { confirmBox } from '../../ui/confirm'
import { AppModal } from '../../ui/AppModal'
import { StatusChip } from '../../ui/StatusChip'
import type { Provider, UpstreamModel } from '../../types'
import { BATCH_RENDER_CAP_STEP, PriceField, ProviderSelect, isPriced, type PriceRow } from './priceShared'

/**
 * 批量设置价格弹窗（U2 拆分自 PriceSettings.tsx，语义不变）：
 * 把同一组价格应用到勾选的多个模型。模型选择器在弹窗内（含未配价的模型）。
 *
 * # 为什么选择器必须是「全部模型」而不是价格列表
 *
 * 价格列表只列已配价的模型。若把选择器做在列表行勾选上，会形成死锁：
 * 想让一个没配过价的模型拿到价格，就必须先在列表里勾它 —— 但它不在列表里。
 * 所以数据源是 allModels（含未配价的）；列表行的勾选仍保留（批量改一批已有
 * 条目时更顺手），打开弹窗时搬进来。
 */
export function BatchPriceModal({
  open,
  onClose,
  onSaved,
  providers,
  modelsMap,
  /** 价格初值来源行（父级保证打开时非空；「复制到…」用它，工具条入口用首行兜底）。 */
  seed,
  /** true = seed 是用户显式点「复制到…」带来的：勾选从零开始（复制目标不该包含来源行本身）。 */
  clearSelection,
  /** 从列表勾选带入的初始勾选集合（clearSelection=false 时用）。 */
  initialSelected,
}: {
  open: boolean
  onClose: () => void
  /** 保存成功后回调（父级负责重拉数据、清列表勾选）。 */
  onSaved: () => void
  providers: Provider[]
  modelsMap: Record<string, UpstreamModel[]>
  seed: PriceRow | null
  clearSelection: boolean
  initialSelected: Set<string>
}) {
  const [form, setForm] = useState({ price_input: '', price_cache_hit: '', price_output: '' })
  const [provider, setProvider] = useState('')
  const [search, setSearch] = useState('')
  const [selected, setSelected] = useState<Set<string>>(new Set())
  const [saving, setSaving] = useState(false)
  // 渲染窗口：全选一家大供应商可能有几千个模型，全部渲染会卡死弹窗。
  const [renderCap, setRenderCap] = useState(BATCH_RENDER_CAP_STEP)

  // 打开时初始化：来源行给价格初值；筛选重置到「全部」——来源行可能来自某个
  // 供应商筛选下，直接沿用会让用户在弹窗里看不到其它供应商的模型（而跨供应商
  // 复制正是主要用途）。
  useEffect(() => {
    if (!open) return
    if (seed) {
      setForm({
        price_input: String(seed.model.price_input),
        price_cache_hit: String(seed.model.price_cache_hit),
        price_output: String(seed.model.price_output),
      })
    }
    setSelected(clearSelection ? new Set() : new Set(initialSelected))
    setProvider('')
    setSearch('')
    setRenderCap(BATCH_RENDER_CAP_STEP)
    // initialSelected/seed 只在打开瞬间取值，避免打开期间父级状态变化重置表单。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open])

  // 弹窗里可选的全部模型（跨 provider 扁平，含未配价的）。
  const targets = useMemo<PriceRow[]>(() => {
    const q = search.trim().toLowerCase()
    const out: PriceRow[] = []
    for (const p of providers) {
      if (provider && p.id !== provider) continue
      for (const m of modelsMap[p.id] ?? []) {
        if (q && !m.model_id.toLowerCase().includes(q) && !(m.display_name ?? '').toLowerCase().includes(q)) continue
        out.push({ providerId: p.id, providerName: p.name || p.slug, model: m })
      }
    }
    return out
  }, [providers, modelsMap, provider, search])

  const toggleSelected = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }
  // 「全选」按**当前筛选**（供应商 + 搜索词），不是全库 —— 用户筛了供应商
  // 再点全选，期望的是「把这家全选上」，而不是无视筛选把别家也拉进来。
  const selectAll = () => {
    setSelected((prev) => {
      const next = new Set(prev)
      for (const r of targets) next.add(r.model.id)
      return next
    })
  }

  // 已勾模型按 id 反查（**不受筛选影响**）：筛选一变就丢已勾行的话，
  // 用户切回原筛选才发现「我刚才勾的怎么没了」。按 id 反查保证勾选稳定。
  const selectedModels = useMemo<PriceRow[]>(() => {
    const byId = new Map<string, PriceRow>()
    for (const p of providers) {
      for (const m of modelsMap[p.id] ?? []) {
        byId.set(m.id, { providerId: p.id, providerName: p.name || p.slug, model: m })
      }
    }
    const out: PriceRow[] = []
    for (const id of selected) {
      const row = byId.get(id)
      if (row) out.push(row)
    }
    return out
  }, [providers, modelsMap, selected])

  const save = async () => {
    const targetsRows = selectedModels
    if (targetsRows.length === 0) {
      toast.danger('请先勾选要设置价格的模型')
      return
    }
    // 只发三档：缓存写入价界面上不再暴露，后端对它是指针语义（不带 = 保持
    // 原值），所以批量改价不会顺手清掉别人配过的缓存写入价。
    const body = {
      price_input: Number(form.price_input) || 0,
      price_cache_hit: Number(form.price_cache_hit) || 0,
      price_output: Number(form.price_output) || 0,
    }
    if (body.price_input < 0 || body.price_cache_hit < 0 || body.price_output < 0) {
      toast.danger('单价不能为负数')
      return
    }
    // 三档全 0 = 清除价格。这里不拦（批量清除是合法动作），但确认文案要讲清。
    const clearing = body.price_input === 0 && body.price_cache_hit === 0 && body.price_output === 0
    // 确认框里列出前几个模型名：跨供应商批量改价最容易勾错行，名字是唯一
    // 能让用户在点确认前核对「到底要改谁」的信息。
    const preview = targetsRows.slice(0, 3).map((r) => r.model.model_id).join('、')
    const more = targetsRows.length > 3 ? ` 等 ${targetsRows.length} 个` : ''
    const ok = await confirmBox({
      title: clearing ? `清除 ${targetsRows.length} 个模型的价格？` : `为 ${targetsRows.length} 个模型设置价格？`,
      body: clearing
        ? `涉及：${preview}${more}。清除后这些模型此后的用量按 0 元计；已经产生的费用按当时的价固化，不受影响。`
        : `涉及：${preview}${more}。只影响之后的调用，历史费用不回溯。`,
      confirmLabel: clearing ? '清除' : '应用',
    })
    if (!ok) return
    setSaving(true)
    try {
      await api.batchUpdateModelPrices(targetsRows.map((r) => r.model.id), body)
      toast.success(clearing ? `已清除 ${targetsRows.length} 个模型的价格` : `已为 ${targetsRows.length} 个模型设置价格`)
      onSaved()
    } catch (e) {
      if ((e as { status?: number }).status !== 401) toast.danger('批量设置失败：' + (e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  const visibleTargets = targets.slice(0, renderCap)
  const hiddenCount = targets.length - visibleTargets.length

  return (
    <AppModal
      open={open}
      onClose={onClose}
      title="批量设置价格"
      maxWidth="640px"
      dismissable={false}
      footer={
        <div className="flex w-full justify-end gap-2">
          <Button variant="ghost" isDisabled={saving} onPress={onClose}>
            取消
          </Button>
          <Button variant="primary" isDisabled={saving || selectedModels.length === 0} onPress={() => void save()}>
            {saving ? '应用中…' : `应用到 ${selectedModels.length} 个模型`}
          </Button>
        </div>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void save()
        }}
        className="flex flex-col gap-4"
      >
        <div className="flex flex-col gap-1">
          <span className="text-[13px] font-medium">目标模型（已选 {selectedModels.length} 个）</span>
          <div className="overflow-hidden rounded-lg border border-separator">
            <div className="bg-background-secondary flex flex-wrap items-center gap-1.5 border-b border-separator p-2">
              <ProviderSelect providers={providers} value={provider} onChange={setProvider} ariaLabel="批量目标供应商" className="w-[150px]" />
              <TextField aria-label="搜索模型" value={search} onChange={setSearch} className="min-w-[140px] flex-1">
                <Input placeholder="搜索模型…" autoComplete="off" />
              </TextField>
              <Button variant="ghost" size="sm" onPress={selectAll}>
                全选
              </Button>
              <Button variant="ghost" size="sm" onPress={() => setSelected(new Set())}>
                清空
              </Button>
            </div>
            <div className="max-h-[240px] overflow-y-auto py-1">
              {visibleTargets.map((r) => (
                // 整行就是 Checkbox 的 label：Content 会渲染 <label>，
                // 外面再套一层 <label> 是嵌套 label（HTML 不允许），
                // 而且真正可点的只有内层那个。
                <Checkbox
                  key={r.model.id}
                  isSelected={selected.has(r.model.id)}
                  onChange={() => toggleSelected(r.model.id)}
                  className="w-full"
                >
                  <Checkbox.Content className="hover:bg-hover w-full gap-2 px-2.5 py-1">
                    <Checkbox.Control>
                      <Checkbox.Indicator />
                    </Checkbox.Control>
                    <span className="truncate font-mono text-[13px]">{r.model.model_id}</span>
                    <span className="text-muted ml-auto shrink-0 text-[11px]">{r.providerName}</span>
                    {/* 已配价的标出来：批量覆盖它们是有意还是误勾，一眼可辨 */}
                    {isPriced(r.model) ? <StatusChip tone="dim">已配价</StatusChip> : null}
                  </Checkbox.Content>
                </Checkbox>
              ))}
              {targets.length === 0 ? <div className="text-muted px-2.5 py-2 text-xs">没有匹配的模型</div> : null}
            </div>
            {hiddenCount > 0 ? (
              <div className="bg-background-secondary flex items-center justify-between gap-2 border-t border-separator px-2.5 py-1.5 text-[11px] text-muted">
                <span>还有 {hiddenCount} 个模型未显示 —— 继续筛选缩小范围，或分批渲染</span>
                <Button variant="tertiary" size="sm" onPress={() => setRenderCap((c) => c + BATCH_RENDER_CAP_STEP)}>
                  显示更多
                </Button>
              </div>
            ) : null}
          </div>
          <span className="text-muted text-[11px]">含未配价的模型 —— 一次把同一批模型的价格配齐。三项全 0 = 清除价格。</span>
        </div>
        <div className="grid grid-cols-2 gap-4">
          {PriceField('输入价格（元/1M）', form.price_input, (v) => setForm((f) => ({ ...f, price_input: v })))}
          {PriceField('缓存命中价格（元/1M）', form.price_cache_hit, (v) => setForm((f) => ({ ...f, price_cache_hit: v })), '0 = 同输入价')}
          {PriceField('输出价格（元/1M）', form.price_output, (v) => setForm((f) => ({ ...f, price_output: v })), '三项全填 0 = 清除这些模型的价格（不是设为免费）')}
        </div>
      </form>
    </AppModal>
  )
}
