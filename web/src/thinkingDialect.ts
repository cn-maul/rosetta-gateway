/**
 * 思考方言选项。
 *
 * 与 Go 侧 internal/dialect 的 All 列表一一对应。这里只放「给人看的标签」，
 * 真正的字段拼写规则全在后端 —— 前端绝不能自己拼 thinking/enable_thinking，
 * 否则同一个模型会出现两套口径，而错误的那个会在某次上游升级后开始 400。
 */
export interface ThinkingDialectOption {
  value: string
  label: string
  /** 一句话说明这家怎么写，供管理员核对而不是靠记忆。 */
  hint: string
}

export const THINKING_DIALECT_OPTIONS: ThinkingDialectOption[] = [
  { value: '', label: '自动识别', hint: '按模型名推断，认不出来就不下发厂商私有字段' },
  { value: 'openai', label: 'OpenAI', hint: 'reasoning_effort，没有 thinking 字段' },
  { value: 'anthropic', label: 'Anthropic', hint: 'thinking + budget_tokens / output_config.effort' },
  { value: 'deepseek', label: 'DeepSeek', hint: 'thinking 对象 + reasoning_effort（low/high/max）' },
  { value: 'glm', label: 'GLM（智谱）', hint: 'thinking 对象；5.3 系列不能关闭思考' },
  { value: 'qwen', label: 'Qwen（通义）', hint: 'enable_thinking 开关' },
  { value: 'minimax', label: 'MiniMax', hint: 'thinking 对象；M3.1-Flash 不能关闭' },
  { value: 'kimi', label: 'Kimi（月之暗面）', hint: 'K3 只能调 reasoning_effort，不能关' },
  { value: 'passthrough', label: '原样透传', hint: '只发协议标准字段，绝不发明厂商字段' },
]

/** 方言值 → 标签；认不出就原样显示，避免界面上出现空白格。 */
export function thinkingDialectLabel(value: string | undefined): string {
  if (!value) return '自动识别'
  return THINKING_DIALECT_OPTIONS.find((o) => o.value === value)?.label ?? value
}

/** 识别来源的一句话解释，直接显示在选中的方言旁边。 */
export function thinkingDialectSourceHint(source: string | undefined): string {
  switch (source) {
    case 'manual':
      return '手动指定'
    case 'detected':
      return '按模型名自动识别'
    case 'fallback':
      return '认不出方言，只发标准字段'
    default:
      return ''
  }
}
