import type { StatusTone } from './ui/StatusChip'

/**
 * 思考档位的展示口径（强度色阶 + 文案），两处共用：
 *  1. 上游详情 → 模型表「思考」列（声明这家上游支持哪些档位）；
 *  2. 请求日志 → 模式列（这次请求**实际下发**的档位）。
 *
 * 同一份映射必须两处一致：否则「模型支持 high」与「这次用了 high」
 * 会显示成两种颜色，读的人会以为它们不是同一个东西。
 */

/**
 * 挡位颜色＝**强度**色阶（弱→强：灰→蓝→绿→琥珀→红），与健康状态无关。
 * 之所以复用语义色，是因为 HeroUI 的 Chip 只有这几档色板；为避免误读，
 * 颜色不是唯一线索 —— 胶囊里始终写着挡位名。
 */
const EFFORT_TONE: Record<string, StatusTone> = {
  none: 'dim',
  minimal: 'dim',
  low: 'info',
  medium: 'ok',
  high: 'warn',
  xhigh: 'err',
  max: 'err',
}

/** 未知档位（厂商私有值）回落 dim：能看见文字，但不冒充已知强度。 */
export function effortTone(level: string): StatusTone {
  return EFFORT_TONE[level.trim().toLowerCase()] ?? 'dim'
}

/**
 * 把落库的 effort 值翻成人读文案。
 *
 * 落库值有三种形态（见后端 ingress.EffortLabel）：
 *  - `low` / `medium` / `high` / `xhigh` …：夹取后实际下发的档位；
 *  - `budget:8192`：anthropic 的预算式思考，透传的是 token 预算而非档位；
 *  - 空串：没要求思考（旧记录同样为空）。
 */
export function effortLabel(effort: string): string {
  const raw = effort.trim()
  if (!raw) return ''
  const budget = /^budget:(\d+)$/i.exec(raw)
  if (budget) return `预算 ${budget[1]}`
  return raw.toLowerCase()
}

/** 胶囊的悬停说明：解释「记的是实际下发的值」这件事。 */
export function effortTitle(effort: string): string {
  const raw = effort.trim()
  if (!raw) return '这次请求没有要求思考强度'
  const budget = /^budget:(\d+)$/i.exec(raw)
  if (budget) {
    return `思考：按 anthropic 的 budget_tokens 透传，预算 ${budget[1]} token（网关不折算档位）`
  }
  return `思考强度：${raw.toLowerCase()}（网关夹取后实际下发给上游的值，不是客户端请求的原始值）`
}
