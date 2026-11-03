import { useMemo, useState } from 'react'
import { Button, Checkbox, SearchField } from '@heroui/react'
import { unionSelection } from '../modelSelection'

/**
 * 模型白名单选择器（语义对齐旧 ModelPicker.vue）。
 *
 * 承载一条反直觉但关键的语义：**一个都不选 = 不限制**（不是「什么都看不到」）。
 * 这句话只该有一个地方说，复制到第二个界面时很容易被顺手改成「拒绝全部」，
 * 那时用户的直觉和系统的行为就反了，而且没有任何报错。
 *
 * 选项由调用方传入（/admin/api/model-names，已按身份收窄），不给自由输入：
 * 白名单是精确匹配，拼错一个字符的后果是「这个模型谁都看不到」而界面毫无异常。
 */
interface ModelPickerProps {
  value: string[]
  onChange: (v: string[]) => void
  options: string[]
  /** 清单本身已被上游收窄（如普通用户只拿到本组内的模型）时置 true，
   *  用来提示「你看到的不是全部模型」。 */
  restricted?: boolean
}

/** 超过这个数量才显示搜索框：几个模型时它是噪音，几十个时才是刚需。 */
const SEARCH_THRESHOLD = 12

export function ModelPicker({ value, onChange, options, restricted = false }: ModelPickerProps) {
  // 选项的确定性顺序。与后端同源（模型名由后端按字典序给出），但这里再排一次
  // 是有意的：组件的语义是「集合」，而界面是「列表」—— 同一份集合每次渲染若
  // 顺序不同，用户勾选一项后整列会跳位，下一次点击就点到了别的模型上。
  const sortedOptions = useMemo(() => [...options].sort(), [options])
  const [query, setQuery] = useState('')
  const showSearch = sortedOptions.length > SEARCH_THRESHOLD
  // 过滤后的清单。大小写不敏感：模型名里有大量 gpt-4o / GPT-4O 混写。
  const visible = useMemo(() => {
    const q = query.trim().toLowerCase()
    if (!q) return sortedOptions
    return sortedOptions.filter((m) => m.toLowerCase().includes(q))
  }, [sortedOptions, query])
  // 已选集合。每个勾选框都要查一次，用 Set 而不是对每项 includes 一遍。
  const picked = useMemo(() => new Set(value), [value])

  const toggle = (name: string) => {
    const next = picked.has(name)
      ? value.filter((m) => m !== name)
      : // 追加到末尾而不是就地插入：value 是集合，顺序无语义，
        // 保持「按勾选顺序追加」的既有行为，交给后端/调用方按需排序。
        [...value, name]
    onChange(next)
  }

  // 全选：有搜索词时只追加当前可见的那些（集合并集）。隐藏的已选项必须
  // 原样保留：这个组件写回的是 key / group 的模型白名单，丢掉隐藏项等于
  // 在不展示的情况下撤销权限。原实现无条件发整个清单，于是「打搜索词 →
  // 点全选」会勾中所有模型 —— 对「配置白名单」这个用例恰好是放宽而非收紧。
  const selectAll = () => {
    const q = query.trim()
    onChange(q ? unionSelection(value, visible) : [...sortedOptions])
  }

  const clearAll = () => onChange([])

  return (
    <div className="border-separator rounded-lg border p-2">
      <div className="mb-1.5 flex items-center justify-between gap-2">
        <span className="text-muted text-xs">
          {value.length === 0 ? '未选 —— 不限制（可见全部模型）' : `已选 ${value.length} / ${options.length}`}
        </span>
        <span className="flex gap-1.5">
          <Button
            variant="ghost"
            size="sm"
            isDisabled={options.length === 0 || (query.trim() !== '' && visible.length === 0)}
            onPress={selectAll}
          >
            {query.trim() ? '全选当前' : '全选'}
          </Button>
          <Button variant="ghost" size="sm" isDisabled={value.length === 0} onPress={clearAll}>
            清空
          </Button>
        </span>
      </div>

      {options.length === 0 ? (
        <div className="text-muted px-1 py-2.5 text-xs">当前没有可用的公开模型。请先到「上游与模型 / 路由」里建好路由。</div>
      ) : (
        <>
          {/* 搜索只在大清单时出现；它只做过滤，绝不「匹配就选中」——
              一旦让搜索词去改 value，「搜一下看看」就变成了一次不可逆的写操作。 */}
          {showSearch ? (
            <SearchField aria-label="搜索模型名" value={query} onChange={setQuery} onClear={() => setQuery('')} />
          ) : null}
          <div className="max-h-[180px] overflow-y-auto">
            {visible.map((m) => (
              <Checkbox
                key={m}
                isSelected={picked.has(m)}
                onChange={() => toggle(m)}
                className="w-full"
              >
                {/* HeroUI 3 的 Checkbox 是复合组件：不渲染 Content/Control/Indicator
                    就只剩一个空 div —— 既看不见方框，也没有 input，整行点不动。
                    只有「全选 / 清空」这类按钮还生效，于是表现为「只能全选或清空」。 */}
                <Checkbox.Content className="hover:bg-default-soft w-full gap-2 rounded px-1.5 py-1">
                  <Checkbox.Control>
                    <Checkbox.Indicator />
                  </Checkbox.Control>
                  <span className="font-mono text-[13px]">{m}</span>
                </Checkbox.Content>
              </Checkbox>
            ))}
            {visible.length === 0 ? <div className="text-muted px-1 py-2.5 text-xs">没有匹配「{query}」的模型。</div> : null}
          </div>
          {showSearch && query ? (
            <div className="mt-1 text-[11.5px] opacity-70">
              已按「{query}」过滤，显示 {visible.length} / {options.length} 个。勾选状态不受影响。
            </div>
          ) : null}
        </>
      )}

      {restricted ? <div className="mt-1.5 text-[11.5px] opacity-70">这份清单已被你的分组收窄过 —— 你最多只能用到这些模型。</div> : null}
      {/* 关键 tip：弱化色而非危险色 —— 它是一条语义说明，不是警告。 */}
      <div className="mt-1.5 text-[11.5px] opacity-70">一个都不选 = 不限制；与分组白名单取交集，两者都限制时更严的生效。</div>
    </div>
  )
}
