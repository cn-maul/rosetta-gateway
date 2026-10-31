import { ListBox, ListBoxItem, Input, Label, Select, SelectIndicator, SelectPopover, SelectTrigger, SelectValue, TextField } from '@heroui/react'
import { unitPrice } from '../../modelPrice'
import type { UpstreamModel } from '../../types'

/**
 * 价格页签共享件（U2 拆分自 PriceSettings.tsx，语义不变）。
 */

export const PRICE_PAGE_SIZE = 10
/** 超过这个供应商数量，价格页签改为「先选供应商」（方案 §U2-price 拍板）。 */
export const PRICE_PROVIDER_GATE = 500
/** 批量弹窗模型列表的渲染上限：一次全量渲染几千个 Checkbox 会把弹窗卡死；
 * 超出部分靠搜索/筛选缩小范围，或点「显示更多」继续渲染。 */
export const BATCH_RENDER_CAP_STEP = 300

export type PriceRow = { providerId: string; providerName: string; model: UpstreamModel }

// 列表 = **价格条目**：只有配过价的模型才出现在这里。
// 没配价的模型不是「0 元的条目」，而是「没有条目」—— 费用侧本来就按 0 元计，
// 界面上列出它们只会把「新增价格」的入口淹没在一排不可删的行里。
export function isPriced(m: UpstreamModel): boolean {
  return m.price_input > 0 || m.price_cache_hit > 0 || m.price_cache_creation > 0 || m.price_output > 0
}

// 单价单元格：条目里的某一档没填（如缓存命中留空=按输入价计）时写「未配置」，
// 不写 0.00 —— 那会被读成「确认这一档免费」。
export const priceCell = (v: number): string => unitPrice(v)

export interface PriceFormState {
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

export const emptyPrices = { price_input: '', price_cache_hit: '', price_cache_creation: '', price_output: '' }

export function PriceField(
  label: string,
  value: string,
  onChange: (v: string) => void,
  tip?: string,
) {
  return (
    <TextField aria-label={label} value={value} onChange={onChange}>
      <Label>{label}</Label>
      <Input type="number" min={0} step="0.001" placeholder="0 = 不计费" />
      {tip ? <span className="text-muted text-[11px]">{tip}</span> : null}
    </TextField>
  )
}

/** 供应商下拉（列表筛选与批量弹窗共用）：'all' 哨兵表示「全部」。 */
export function ProviderSelect({
  providers,
  value,
  onChange,
  ariaLabel,
  className,
  disabled,
}: {
  providers: { id: string; name: string; slug: string }[]
  value: string
  onChange: (v: string) => void
  ariaLabel: string
  className?: string
  disabled?: boolean
}) {
  return (
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
      <SelectTrigger>
        <SelectValue />
        <SelectIndicator />
      </SelectTrigger>
      <SelectPopover>
        <ListBox aria-label={ariaLabel}>
          <ListBoxItem id="all">全部供应商</ListBoxItem>
          {providers.map((p) => (
            <ListBoxItem key={p.id} id={p.id}>
              {p.name || p.slug}
            </ListBoxItem>
          ))}
        </ListBox>
      </SelectPopover>
    </Select>
  )
}
