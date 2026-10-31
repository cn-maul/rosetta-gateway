import { Input, Label, TextField } from '@heroui/react'
import { tokenCountHint } from '../tokenCount'

/**
 * 以 token 计的数量输入框（上下文窗口、最大输出等）。
 *
 * # 为什么不用数字输入框
 *
 * 这两个数在模型卡与厂商文档里几乎总是写成 `128K` / `1M`。HTML 的 number
 * 输入框会把 `K` 这个字符直接吃掉，框子看起来像被清空，保存下去就是 0
 * （= 未设置），于是错得悄无声息。改成文本输入 + 提交时解析，用户照抄文档
 * 即可。
 *
 * 解析口径见 `src/tokenCount.ts`：1024 进制，`128K` = 131072。
 *
 * 提示文字只在用户写了后缀或分组分隔符时出现 —— 纯数字再把结果回显一遍
 * 是噪音，输入框下方多一行字反而要人分辨「这行是不是警告」。
 */
export function TokenField({
  label,
  value,
  onChange,
  placeholder,
}: {
  label: string
  value: string
  onChange: (next: string) => void
  placeholder?: string
}) {
  const hint = tokenCountHint(value)
  return (
    <TextField aria-label={label} value={value} onChange={onChange}>
      <Label>{label}</Label>
      <Input inputMode="text" placeholder={placeholder ?? '0 = 未设置'} autoComplete="off" />
      {hint ? <span className="text-muted text-[11px]">{hint}</span> : null}
    </TextField>
  )
}
