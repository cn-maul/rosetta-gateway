import { useMemo, type Key } from 'react'
import { Button, ListBox, ListBoxItem, Label, Select, SelectIndicator, SelectPopover, SelectTrigger, SelectValue } from '@heroui/react'
import { AppModal } from '../../ui/AppModal'
import type { Provider, UpstreamModel } from '../../types'
import { PriceField, isPriced, type PriceFormState } from './priceShared'

/**
 * 新增 / 编辑价格条目弹窗（U2 拆分自 PriceSettings.tsx，语义不变）。
 * 表单填一半别丢：遮罩 / Esc 都不关（dismissable=false 由父级 AppModal 控制）。
 */
export function PriceEditModal({
  form,
  onPatch,
  onClose,
  providers,
  modelsMap,
  onSave,
  onDelete,
}: {
  form: PriceFormState
  onPatch: (patch: Partial<PriceFormState>) => void
  onClose: () => void
  providers: Provider[]
  modelsMap: Record<string, UpstreamModel[]>
  onSave: () => void
  onDelete: () => void
}) {
  const modalModels = useMemo(() => modelsMap[form.providerId] ?? [], [modelsMap, form.providerId])

  // 换供应商必须清掉已选模型：否则上一家的 modelId 会跟着提交，
  // 把价格写到另一个供应商的模型上（两个下拉看着不匹配，数据却落库了）。
  const onPriceProviderChange = (key: Key | null) => {
    onPatch({ providerId: key === null ? '' : String(key), modelId: '' })
  }

  // 新增只列「还没有价格条目」的模型（有条目的去列表里编辑，避免同一条目两处入口）；
  // 编辑时模型锁定，选项就是那一个。
  const modalModelOptions = useMemo(
    () => (form.editing ? modalModels : modalModels.filter((m) => !isPriced(m))),
    [form.editing, modalModels],
  )

  // 编辑弹窗里的「删除价格」按钮：以**库里已存**的价格为准（不是输入框里的草稿），
  // 否则把输入清空后按钮就消失了，恰恰是在最需要删除的时候。
  const modalHasPrice = useMemo(() => {
    if (!form.modelId) return false
    const m = modalModels.find((x) => x.id === form.modelId)
    return !!m && isPriced(m)
  }, [modalModels, form.modelId])

  return (
    <AppModal
      open={form.open}
      onClose={onClose}
      title={form.editing ? '编辑模型价格' : '新增模型价格'}
      maxWidth="520px"
      dismissable={false}
      footer={
        <div className="flex w-full items-center justify-between gap-2">
          {modalHasPrice ? (
            <Button variant="danger" isDisabled={form.saving} onPress={onDelete}>
              删除价格
            </Button>
          ) : (
            <span />
          )}
          <div className="flex gap-2">
            <Button variant="ghost" isDisabled={form.saving} onPress={onClose}>
              取消
            </Button>
            <Button variant="primary" isDisabled={form.saving || !form.modelId} onPress={() => void onSave()}>
              {form.saving ? '保存中…' : '保存'}
            </Button>
          </div>
        </div>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void onSave()
        }}
        className="grid grid-cols-2 gap-4"
      >
        <div className="col-span-2 flex flex-col gap-1">
          <Label>供应商 *</Label>
          <Select aria-label="供应商" isDisabled={form.editing} selectedKey={form.providerId || null} onSelectionChange={onPriceProviderChange}>
            <SelectTrigger>
              <SelectValue />
              <SelectIndicator />
            </SelectTrigger>
            <SelectPopover>
              <ListBox aria-label="供应商">
                {providers.map((p) => (
                  <ListBoxItem key={p.id} id={p.id}>
                    {p.name || p.slug}
                  </ListBoxItem>
                ))}
              </ListBox>
            </SelectPopover>
          </Select>
          <span className="text-muted text-[11px]">同一个 model_id 在不同供应商下价格可能不同</span>
        </div>
        <div className="col-span-2 flex flex-col gap-1">
          <Label>模型 *</Label>
          <Select
            aria-label="模型"
            isDisabled={form.editing}
            selectedKey={form.modelId || null}
            onSelectionChange={(key) => key !== null && onPatch({ modelId: String(key) })}
          >
            <SelectTrigger>
              <SelectValue />
              <SelectIndicator />
            </SelectTrigger>
            <SelectPopover>
              <ListBox aria-label="模型">
                {modalModelOptions.map((m) => (
                  <ListBoxItem key={m.id} id={m.id}>
                    {m.model_id}
                  </ListBoxItem>
                ))}
              </ListBox>
            </SelectPopover>
          </Select>
          <span className="text-muted text-[11px]">
            {form.editing
              ? '条目归属的供应商与模型不可改；要换模型请先删除再新增'
              : modalModels.length > 0 && modalModelOptions.length === 0
                ? '该供应商下的模型都已有价格条目，请到列表中编辑'
                : '只列出还没有价格条目的模型'}
          </span>
        </div>
        {PriceField('输入价格（元/1M）', form.price_input, (v) => onPatch({ price_input: v }), '缓存未命中的输入 token')}
        {PriceField('缓存命中价格（元/1M）', form.price_cache_hit, (v) => onPatch({ price_cache_hit: v }), '留空/填 0 时按输入价计')}
        {PriceField('缓存写入价格（元/1M）', form.price_cache_creation, (v) => onPatch({ price_cache_creation: v }), 'Anthropic cache creation；留空/填 0 时按输入价计')}
        {PriceField('输出价格（元/1M）', form.price_output, (v) => onPatch({ price_output: v }))}
      </form>
    </AppModal>
  )
}
