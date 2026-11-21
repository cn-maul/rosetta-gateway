import { Input, Label, TextField } from '@heroui/react'
import { tokenCountHint } from '../tokenCount'

/**
 * 以 token 计的数量输入框（上下文窗口、最大输出、配额等）。
 *
 * # 为什么不用数字输入框
 *
 * 这类数在模型卡与厂商文档里几乎总是写成 `128K` / `1M` / `1B`。HTML 的 number
 * 输入框会把 `K` 这个字符直接吃掉，框子看起来像被清空，保存下去就是 0
 * （= 未设置），于是错得悄无声息。改成文本输入 + 提交时解析，用户照抄文档
 * 即可。
 *
 * 解析口径见 `src/tokenCount.ts`：1024 进制，`128K` = 131072，`1M` = 1048576。
 *
 * 提示文字只在用户写了后缀或分组分隔符时出现 —— 纯数字再把结果回显一遍
 * 是噪音，输入框下方多一行字反而要人分辨「这行是不是警告」。
 */
export function TokenField({
  label,
  value,
  onChange,
  placeholder,
  hint,
}: {
  label: string
  value: string
  onChange: (next: string) => void
  placeholder?: string
  /** 额外说明（例如「0 = 不限」）。与「= N tokens」的回显分行展示。 */
  hint?: string
}) {
  const parsedHint = tokenCountHint(value)
  return (
    <TextField aria-label={label} value={value} onChange={onChange}>
      <Label>{label}</Label>
      <Input inputMode="text" placeholder={placeholder ?? '0 = 未设置'} autoComplete="off" />
      {parsedHint ? <span className="text-muted text-[11px]">{parsedHint}</span> : null}
      {hint ? <span className="text-muted text-[11px]">{hint}</span> : null}
    </TextField>
  )
}
