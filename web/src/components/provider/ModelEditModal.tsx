import { useEffect, useMemo, useState } from 'react'
import { Button, Input, Label, TextField, ToggleButton, ToggleButtonGroup, toast } from '@heroui/react'
import { api } from '../../api'
import { AppModal } from '../../ui/AppModal'
import { confirmBox } from '../../ui/confirm'
import { PriceField } from '../price/priceShared'
import { TokenField } from '../../ui/TokenField'
import { parseTokenCount } from '../../tokenCount'
import type { DiscoveredModel, UpstreamModel } from '../../types'

const is401 = (e: unknown): boolean => (e as { status?: number }).status === 401

type Thinking = 'unset' | 'yes' | 'no'

const knownEffortLevels = ['minimal', 'low', 'medium', 'high', 'xhigh', 'max'] as const

const thinkingOptions: { v: Thinking; label: string }[] = [
  { v: 'unset', label: '未配置' },
  { v: 'yes', label: '支持' },
  { v: 'no', label: '不支持' },
]

/**
 * 上游模型编辑弹窗（语义对齐旧 ProviderDetail.vue 的 mForm.editingId 分支）。
 * original 为 null 时父组件不应渲染本弹窗；探测添加走 ModelDiscoverModal。
 */
export function ModelEditModal({
  original,
  open,
  onClose,
  onSaved,
}: {
  original: UpstreamModel
  open: boolean
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const [modelId, setModelId] = useState('')
  const [contextWindow, setContextWindow] = useState('0')
  const [maxOutputTokens, setMaxOutputTokens] = useState('0')
  const [priceInput, setPriceInput] = useState('0')
  const [priceCacheHit, setPriceCacheHit] = useState('0')
  const [priceOutput, setPriceOutput] = useState('0')
  const [effortCsv, setEffortCsv] = useState('')
  const [thinking, setThinkingState] = useState<Thinking>('unset')
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    if (!open) return
    setModelId(original.model_id)
    setContextWindow(String(original.context_window ?? 0))
    setMaxOutputTokens(String(original.max_output_tokens ?? 0))
    setPriceInput(String(original.price_input ?? 0))
    setPriceCacheHit(String(original.price_cache_hit ?? 0))
    setPriceOutput(String(original.price_output ?? 0))
    // unknown 档位附在后面：它们是网关不认、但可能是上游认得的厂商私有值，
    // 编辑时必须原样带回去，否则点一次保存就把它们悄悄抹掉了。
    setEffortCsv(
      [...(original.effort_levels ?? []), ...(original.unknown_effort_levels ?? [])].join(','),
    )
    setThinkingState(
      original.supports_thinking === undefined
        ? 'unset'
        : original.supports_thinking
          ? 'yes'
          : 'no',
    )
  }, [open, original])

  const hasEffortLevel = (l: string): boolean =>
    effortCsv.split(',').some((x) => x.trim() === l)

  // 用集合而不是字符串拼装算勾选态：CSV 是有序文本，includes 会把「xhigh」
  // 误判为含「high」（子串包含），那是会点错的。
  const toggleEffortLevel = (l: string) => {
    const parts = effortCsv
      .split(',')
      .map((x) => x.trim())
      .filter(Boolean)
    const i = parts.indexOf(l)
    if (i >= 0) parts.splice(i, 1)
    else parts.push(l)
    const rank = (x: string) => {
      const idx = knownEffortLevels.indexOf(x as (typeof knownEffortLevels)[number])
      return idx < 0 ? knownEffortLevels.length : idx
    }
    parts.sort((a, b) => rank(a) - rank(b))
    setEffortCsv(parts.join(','))
  }

  // 明确「不支持思考」时清空挡位；反过来不成立（「支持」不必配挡位）。
  // 否则界面上会出现「不支持思考，却配了 high/xhigh」，而其后果是档位仍被
  // 当成有效能力披露给 /v1/models，客户端以为能选、选了没效果。
  const setThinking = (v: Thinking) => {
    setThinkingState(v)
    if (v === 'no') setEffortCsv('')
  }

  const submit = async () => {
    if (saving) return
    if (!modelId.trim()) {
      toast.danger('上游模型名（model_id）为必填项')
      return
    }
    // 这两个数允许写 128K / 1M（厂商文档就是这么写的）。解析失败要当场报错，
    // 不能落到 Number()||0 —— 那会把打错的后缀变成「未设置」，错误沉底。
    const ctxTokens = parseTokenCount(contextWindow)
    const outTokens = parseTokenCount(maxOutputTokens)
    if (ctxTokens === null) {
      toast.danger('上下文窗口无法识别，请填数字或 128K / 1M 这类写法')
      return
    }
    if (outTokens === null) {
      toast.danger('最大输出无法识别，请填数字或 128K / 1M 这类写法')
      return
    }
    const prices = [priceInput, priceCacheHit, priceOutput].map(Number)
    if (prices.some((v) => !Number.isFinite(v) || v < 0)) {
      toast.danger('单价必须是大于等于 0 的数字')
      return
    }
    setSaving(true)
    try {
      // 「原来配过价、现在三档全填 0」= 清除。判断里带上 price_cache_creation：
      // 那一档界面不再暴露，但库里可能还有值，清除时得一并清掉（下面显式
      // 传 0），否则会留下一个看不见的价在继续计费。
      const clearing =
        [original.price_input, original.price_cache_hit, original.price_cache_creation, original.price_output].some(
          (v) => v > 0,
        ) && prices.every((v) => v === 0)
      if (clearing) {
        const ok = await confirmBox({
          title: '清除该模型的全部价格？',
          body: '之后的调用将按 0 元计费，历史费用不变。',
          confirmLabel: '清除价格并保存',
        })
        if (!ok) return
      }
      await api.updateModel(original.id, {
        model_id: modelId.trim(),
        context_window: ctxTokens,
        max_output_tokens: outTokens,
        price_input: prices[0],
        price_cache_hit: prices[1],
        price_output: prices[2],
        // 缓存写入价界面上不再暴露（2026-10-31）：不清除时**不发这个字段**，
        // PATCH 的 nil 语义 = 保持原值；清除时显式发 0 把它一并清掉。
        price_cache_creation: clearing ? 0 : undefined,
        // 空串 = 清除（回到未配置），与界面上「一个都不勾选」是同一个动作。
        effort_levels: effortCsv,
        // 三态映射：unset → null（后端解成「清除」）。不能省略字段 ——
        // 省略是「不改」，「切回未配置并保存」必须真的写下去。
        supports_thinking: thinking === 'unset' ? null : thinking === 'yes',
      })
      onClose()
      toast.success('模型已更新')
      await onSaved()
    } catch (e) {
      if (!is401(e)) toast.danger('保存失败：' + (e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <AppModal
      open={open}
      onClose={onClose}
      title="编辑上游模型"
      maxWidth="500px"
      dismissable={false}
      footer={
        <>
          <Button variant="ghost" isDisabled={saving} onPress={onClose}>
            取消
          </Button>
          <Button variant="primary" isDisabled={saving} onPress={() => void submit()}>
            {saving ? '保存中…' : '保存'}
          </Button>
        </>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void submit()
        }}
        className="flex flex-col gap-4"
      >
        <TextField aria-label="上游模型名（model_id）" isRequired value={modelId} onChange={setModelId}>
          <Label>上游模型名（model_id）*</Label>
          <Input placeholder="deepseek-chat" className="font-mono" autoComplete="off" />
        </TextField>
        <div className="grid grid-cols-2 gap-4">
          <TokenField label="上下文窗口" value={contextWindow} onChange={setContextWindow} />
          <TokenField label="最大输出" value={maxOutputTokens} onChange={setMaxOutputTokens} />
        </div>
        <fieldset className="min-w-0" disabled={saving}>
          <div className="text-muted text-[11px]">单价（元 / 1M tokens）</div>
          {/* 三档而不是四档：缓存写入价（Anthropic cache_creation）2026-10-31
              起不再在界面上暴露 —— 绝大多数上游不给这个数，留一格空着反而
              让人以为自己漏填。未配置时计费侧按输入价算，需要特殊定价的
              仍可由配置导入写入。 */}
          <div className="mt-2 grid grid-cols-3 gap-2">
            {PriceField('输入', priceInput, setPriceInput)}
            {PriceField('缓存命中', priceCacheHit, setPriceCacheHit)}
            {PriceField('输出', priceOutput, setPriceOutput)}
          </div>
          <div className="mt-4">
            <div className="text-muted text-[11px]">思考模式</div>
            <ToggleButtonGroup
              aria-label="思考模式"
              className="mt-2"
              selectionMode="single"
              disallowEmptySelection
              selectedKeys={[thinking]}
              onSelectionChange={(keys) => {
                const v = [...keys][0]
                if (v !== undefined) setThinking(String(v) as Thinking)
              }}
            >
              {thinkingOptions.map((o) => (
                <ToggleButton key={o.v} id={o.v} size="sm">
                  {o.label}
                </ToggleButton>
              ))}
            </ToggleButtonGroup>
          </div>
          {thinking !== 'no' ? (
            <div className="mt-4">
              <div className="text-muted text-[11px]">思考挡位</div>
              <ToggleButtonGroup
                aria-label="思考挡位"
                className="mt-2"
                selectionMode="multiple"
                selectedKeys={knownEffortLevels.filter((l) => hasEffortLevel(l))}
                onSelectionChange={(keys) => {
                  const next = new Set([...keys].map(String))
                  for (const l of knownEffortLevels) {
                    if (next.has(l) !== hasEffortLevel(l)) toggleEffortLevel(l)
                  }
                }}
              >
                {knownEffortLevels.map((l) => (
                  <ToggleButton key={l} id={l} size="sm">
                    {l}
                  </ToggleButton>
                ))}
              </ToggleButtonGroup>
            </div>
          ) : null}
        </fieldset>
      </form>
    </AppModal>
  )
}

/**
 * 探测 + 多选胶囊式添加弹窗（语义对齐旧 ProviderDetail.vue 的 mForm 非 editing 分支）。
 * 打开时状态清零，每次都从「获取模型列表」重新开始。
 *
 * 另有「手动添加模型」一档：有些上游不实现 /models（或凭据没开列表权限），
 * 探测永远拿不到东西，但模型本身可用。这一档直接调同一个导入接口，只填
 * model_id 也能落库（context_window / max_output_tokens 留 0 时后端回落到
 * 模型默认值，见 internal/admin/model_handler.go 的 ImportModels）。
 */
export function ModelDiscoverModal({
  providerId,
  open,
  onClose,
  onSaved,
}: {
  providerId: string
  open: boolean
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const [discovering, setDiscovering] = useState(false)
  const [discovered, setDiscovered] = useState<DiscoveredModel[]>([])
  const [discoverMsg, setDiscoverMsg] = useState('')
  const [filter, setFilter] = useState('')
  const [selected, setSelected] = useState<DiscoveredModel[]>([])
  const [importing, setImporting] = useState(false)
  // 手动添加这一档：探测拿不到列表（上游没实现 /models，或凭据没开列表权限）时，
  // 管理员仍然要能把模型名写进去。
  const [manual, setManual] = useState(false)
  const [mModelId, setMModelId] = useState('')
  const [mDisplayName, setMDisplayName] = useState('')
  const [mContextWindow, setMContextWindow] = useState('0')
  const [mMaxOutput, setMMaxOutput] = useState('0')
  // 手动档也带单价：现场添加模型的人通常手里就有价目表，逼他「先建空模型、
  // 再到列表里逐条补价」是白跑一趟。三档与编辑弹窗一致（无缓存写入）。
  const [mPriceInput, setMPriceInput] = useState('0')
  const [mPriceCacheHit, setMPriceCacheHit] = useState('0')
  const [mPriceOutput, setMPriceOutput] = useState('0')

  useEffect(() => {
    if (!open) return
    setDiscovered([])
    setSelected([])
    setDiscoverMsg('')
    setFilter('')
    setManual(false)
    setMModelId('')
    setMDisplayName('')
    setMContextWindow('0')
    setMMaxOutput('0')
    setMPriceInput('0')
    setMPriceCacheHit('0')
    setMPriceOutput('0')
  }, [open])

  const filteredDiscovered = useMemo(() => {
    const q = filter.trim().toLowerCase()
    if (!q) return discovered
    return discovered.filter(
      (d) => d.model_id.toLowerCase().includes(q) || (d.display_name ?? '').toLowerCase().includes(q),
    )
  }, [discovered, filter])

  const runDiscover = async () => {
    setDiscovering(true)
    setDiscovered([])
    setSelected([])
    setDiscoverMsg('')
    try {
      const r = await api.discoverModels(providerId)
      if (r.status === 'ok') {
        setDiscovered(r.models ?? [])
        if ((r.models ?? []).length === 0) setDiscoverMsg('上游未返回任何模型')
      } else {
        setDiscoverMsg(r.message || '获取失败')
      }
    } catch (e) {
      setDiscoverMsg((e as Error).message)
    } finally {
      setDiscovering(false)
    }
  }

  const isSelected = (mid: string) => selected.some((s) => s.model_id === mid)

  const toggleSelect = (d: DiscoveredModel) => {
    setSelected((prev) =>
      prev.some((s) => s.model_id === d.model_id)
        ? prev.filter((s) => s.model_id !== d.model_id)
        : [...prev, d],
    )
  }

  const removeSelected = (mid: string) => {
    setSelected((prev) => prev.filter((s) => s.model_id !== mid))
  }

  const confirmImport = async () => {
    const list = selected
    if (list.length === 0) return
    setImporting(true)
    try {
      const r = await api.importModels(
        providerId,
        list.map((d) => ({
          model_id: d.model_id,
          display_name: d.display_name ?? '',
          context_window: d.context_window ?? 0,
          max_output_tokens: d.max_output_tokens ?? 0,
          // 上游自报的思考挡位原样透传：闸门在后端（空串 = 上游没报 =
          // 留空给管理员手工配），前端再判一次只会出现两套口径。
          // supports_thinking 用 ?? 而不是 ||：false 是有意义的值
          // （「上游明确说不支持」），|| 会把它抹掉。
          effort_levels: d.effort_levels ?? '',
          supports_thinking: d.supports_thinking ?? undefined,
        })),
      )
      toast.success(`已添加 ${r.imported} 个模型`)
      onClose()
      await onSaved()
    } catch (e) {
      if (!is401(e)) toast.danger('添加失败：' + (e as Error).message)
    } finally {
      setImporting(false)
    }
  }

  /** 手动添加：model_id 必填，其余留 0 由后端回落到模型默认值。 */
  const submitManual = async () => {
    const id = mModelId.trim()
    if (!id) {
      toast.danger('上游模型名（model_id）为必填项')
      return
    }
    // 与编辑弹窗同一口径：128K / 1M 可识别，识别不了要当场说，不能静默变 0。
    const ctxTokens = parseTokenCount(mContextWindow)
    const outTokens = parseTokenCount(mMaxOutput)
    if (ctxTokens === null) {
      toast.danger('上下文窗口无法识别，请填数字或 128K / 1M 这类写法')
      return
    }
    if (outTokens === null) {
      toast.danger('最大输出无法识别，请填数字或 128K / 1M 这类写法')
      return
    }
    const prices = [mPriceInput, mPriceCacheHit, mPriceOutput].map(Number)
    if (prices.some((v) => !Number.isFinite(v) || v < 0)) {
      toast.danger('单价必须是大于等于 0 的数字')
      return
    }
    setImporting(true)
    try {
      const r = await api.importModels(providerId, [
        {
          model_id: id,
          display_name: mDisplayName.trim(),
          context_window: ctxTokens,
          max_output_tokens: outTokens,
          // 0 = 不配价（后端保持价格列为 NULL）。四项都填 0 与「留空」等价，
          // 不会落成「确认免费」的 0 元条目。
          price_input: prices[0],
          price_cache_hit: prices[1],
          price_output: prices[2],
        },
      ])
      toast.success(`已添加 ${r.imported} 个模型`)
      onClose()
      await onSaved()
    } catch (e) {
      if (!is401(e)) toast.danger('添加失败：' + (e as Error).message)
    } finally {
      setImporting(false)
    }
  }

  return (
    <AppModal
      open={open}
      onClose={onClose}
      title="添加上游模型"
      maxWidth="560px"
      dismissable={false}
      footer={
        <>
          <Button variant="ghost" isDisabled={importing} onPress={onClose}>
            取消
          </Button>
          <Button
            variant="primary"
            isDisabled={importing || (manual ? !mModelId.trim() : !selected.length)}
            onPress={() => void (manual ? submitManual() : confirmImport())}
          >
            {importing ? '添加中…' : manual ? '添加' : `添加（${selected.length}）`}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <div className="flex items-center gap-2">
          <Button
            variant="secondary"
            size="sm"
            isDisabled={discovering || manual}
            onPress={() => void runDiscover()}
          >
            {discovering ? '获取中…' : '获取模型列表'}
          </Button>
          <Button
            variant={manual ? 'secondary' : 'tertiary'}
            size="sm"
            isDisabled={importing}
            onPress={() => setManual((v) => !v)}
          >
            {manual ? '返回获取列表' : '手动添加模型'}
          </Button>
          {!manual && discovered.length ? (
            <TextField aria-label="搜索模型" value={filter} onChange={setFilter} className="flex-1">
              <Input type="search" placeholder="搜索模型…" autoComplete="off" />
            </TextField>
          ) : null}
        </div>
        {discoverMsg && !manual ? (
          <p className="text-danger text-xs" role="alert">
            {discoverMsg}
          </p>
        ) : null}

        {manual ? (
          <form
            onSubmit={(e) => {
              e.preventDefault()
              void submitManual()
            }}
            className="flex flex-col gap-3"
          >
            <TextField
              aria-label="上游模型名（model_id）"
              isRequired
              value={mModelId}
              onChange={setMModelId}
            >
              <Label>上游模型名（model_id）*</Label>
              <Input placeholder="deepseek-chat" className="font-mono" autoComplete="off" />
            </TextField>
            <TextField aria-label="显示名称" value={mDisplayName} onChange={setMDisplayName}>
              <Label>显示名称</Label>
              <Input placeholder="留空 = 用模型名" autoComplete="off" />
            </TextField>
            <div className="grid grid-cols-2 gap-3">
              <TokenField
                label="上下文窗口"
                value={mContextWindow}
                onChange={setMContextWindow}
                placeholder="0 = 用默认值"
              />
              <TokenField
                label="最大输出"
                value={mMaxOutput}
                onChange={setMMaxOutput}
                placeholder="0 = 用默认值"
              />
            </div>
            <fieldset className="min-w-0" disabled={importing}>
              <div className="text-muted text-[11px]">单价（元 / 1M tokens）</div>
              <div className="mt-2 grid grid-cols-3 gap-2">
                {PriceField('输入', mPriceInput, setMPriceInput)}
                {PriceField('缓存命中', mPriceCacheHit, setMPriceCacheHit)}
                {PriceField('输出', mPriceOutput, setMPriceOutput)}
              </div>
            </fieldset>
            <span className="text-muted text-[11px]">
              上游不实现 /models 或不给列表权限时用这一档。上下文窗口与最大输出填 0
              会套用「模型默认值」；单价留 0 = 不配价（之后可到列表里改），思考挡位请在
              添加后到列表里逐个编辑。
            </span>
          </form>
        ) : (
          <>
            {selected.length ? (
              <div className="flex flex-wrap gap-2">
                {selected.map((s) => (
                  <span
                    key={s.model_id}
                    className="bg-default-soft flex items-center gap-1 rounded-full py-0.5 pr-1 pl-2.5 text-xs"
                  >
                    <span className="font-mono">{s.model_id}</span>
                    <Button
                      variant="ghost"
                      isIconOnly
                      size="sm"
                      aria-label={`移除 ${s.model_id}`}
                      className="h-5 min-w-5 rounded-full"
                      onPress={() => removeSelected(s.model_id)}
                    >
                      ×
                    </Button>
                  </span>
                ))}
              </div>
            ) : null}

            {discovered.length ? (
              /* 单列：模型名普遍偏长（带厂商前缀的如 deepseek/deepseek-chat 更长），
                 两列会把名字截断还挤掉右侧的「思考 / ctx」提示。 */
              <div className="grid max-h-[320px] grid-cols-1 gap-1.5 overflow-y-auto">
                {filteredDiscovered.map((d) => (
                  <Button
                    key={d.model_id}
                    variant={isSelected(d.model_id) ? 'secondary' : 'ghost'}
                    aria-label={
                      isSelected(d.model_id) ? `取消选择 ${d.model_id}` : `选择 ${d.model_id}`
                    }
                    className="h-auto w-full justify-start px-2 py-1.5 text-left"
                    onPress={() => toggleSelect(d)}
                  >
                    <span className="flex w-full min-w-0 items-center gap-2 text-xs">
                      <span className="shrink-0">{isSelected(d.model_id) ? '✓' : '+'}</span>
                      <span className="truncate font-mono">{d.model_id}</span>
                      <span className="text-muted ml-auto shrink-0 text-[10px]">
                        {d.effort_levels ? `思考 ${d.effort_levels}` : '思考 待配置'}
                        {d.context_window || d.max_output_tokens
                          ? ` · ${d.context_window ? d.context_window + ' ctx' : ''}${
                              d.context_window && d.max_output_tokens ? ' / ' : ''
                            }${d.max_output_tokens ? d.max_output_tokens + ' out' : ''}`
                          : ''}
                      </span>
                    </span>
                  </Button>
                ))}
              </div>
            ) : !discovering && !discoverMsg ? (
              <p className="text-muted py-4 text-center text-xs">
                点击「获取模型列表」拉取上游可用模型；上游不支持列表时用「手动添加模型」
              </p>
            ) : null}
          </>
        )}
      </div>
    </AppModal>
  )
}
