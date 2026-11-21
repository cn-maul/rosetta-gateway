import { Input, Label, TextField, ToggleButton, ToggleButtonGroup } from '@heroui/react'
import { fmtTokens } from '../../fmt'
import { TokenField } from '../../ui/TokenField'

/**
 * 密钥限额的四种口径。**同时只能启用一种** —— 后端 validateLimitMode 会拒
 * 绝同时非 0 的组合，前端在这里用单选表达同一条规则，避免用户填完才挨 400。
 *
 * 互斥的理由：四种口径的计量对象完全不同（请求数/分钟、token/分钟、请求数/
 * 天、token/天）。同时开两种时，429 只能告诉调用方「超限了」，却说不清是
 * 哪一条拦的，排查入口直接翻倍。
 */
export type LimitMode = 'none' | 'rpm' | 'tpm' | 'daily_requests' | 'daily_tokens'

export const LIMIT_MODE_OPTIONS: { key: LimitMode; label: string }[] = [
  { key: 'none', label: '不限' },
  { key: 'rpm', label: '每分钟请求' },
  { key: 'tpm', label: '每分钟 token' },
  { key: 'daily_requests', label: '每天请求' },
  { key: 'daily_tokens', label: '每天 token' },
]

/** 从四档现值推断当前启用的是哪一种。历史脏数据里可能同时非 0，此时按
 *  RPM 优先给出一个确定答案，真正的冲突由后端在保存时挡下。 */
export function detectLimitMode(v: {
  rpm: number
  tpm: number
  dailyRequests: number
  dailyTokens: number
}): LimitMode {
  if (v.rpm > 0) return 'rpm'
  if (v.tpm > 0) return 'tpm'
  if (v.dailyRequests > 0) return 'daily_requests'
  if (v.dailyTokens > 0) return 'daily_tokens'
  return 'none'
}

export interface LimitState {
  mode: LimitMode
  rpm: string
  tpm: string
  dailyRequests: string
  dailyTokens: string
}

/**
 * 把限额状态转成请求字段。
 *
 * 关键点：**未选中的三档一律显式发 0**。PATCH 的 nil 语义是「保持原值」，
 * 只发选中那一档的话，用户从「每天 token」切到「每分钟请求」时旧值会留在
 * 库里 —— 后端互斥校验随即拒绝这次保存，而报错指向的是用户没碰的字段。
 */
export function limitPayload(s: LimitState): {
  rpm_limit: number
  tpm_limit: number
  daily_request_limit: number
  daily_token_limit: number
} {
  const n = (raw: string) => {
    const v = Number(raw)
    return Number.isFinite(v) && v > 0 ? Math.floor(v) : 0
  }
  return {
    rpm_limit: s.mode === 'rpm' ? n(s.rpm) : 0,
    tpm_limit: s.mode === 'tpm' ? n(s.tpm) : 0,
    daily_request_limit: s.mode === 'daily_requests' ? n(s.dailyRequests) : 0,
    daily_token_limit: s.mode === 'daily_tokens' ? n(s.dailyTokens) : 0,
  }
}

/** 校验选中那一档的输入。返回错误文案，空串 = 通过。 */
export function validateLimit(s: LimitState): string {
  const positive = (raw: string) => {
    const v = Number(raw)
    return Number.isFinite(v) && v > 0 && Number.isInteger(v)
  }
  switch (s.mode) {
    case 'rpm':
      return positive(s.rpm) ? '' : '每分钟请求数必须是正整数'
    case 'tpm':
      return positive(s.tpm) ? '' : '每分钟 token 数必须是正整数'
    case 'daily_requests':
      return positive(s.dailyRequests) ? '' : '每天请求数必须是正整数'
    case 'daily_tokens':
      return positive(s.dailyTokens) ? '' : '每天 token 数必须是正整数'
    default:
      return ''
  }
}

/**
 * 限额选择区：一排互斥的模式按钮 + 当前模式对应的一个输入框。
 *
 * 只渲染**当前模式**的输入框，而不是四个并排再禁用三个 —— 后者会让人以为
 * 可以分别设置，而实际规则是四选一。
 */
export function LimitFields({
  state,
  onChange,
}: {
  state: LimitState
  onChange: (next: LimitState) => void
}) {
  const setMode = (mode: LimitMode) => onChange({ ...state, mode })
  return (
    <div>
      <div className="text-muted mb-1.5 text-[11px]">限额方式（同时只能启用一种）</div>
      <ToggleButtonGroup
        aria-label="限额方式"
        selectionMode="single"
        disallowEmptySelection
        selectedKeys={[state.mode]}
        onSelectionChange={(keys) => {
          const v = [...keys][0]
          if (v !== undefined) setMode(String(v) as LimitMode)
        }}
      >
        {LIMIT_MODE_OPTIONS.map((o) => (
          <ToggleButton key={o.key} id={o.key} size="sm">
            {o.label}
          </ToggleButton>
        ))}
      </ToggleButtonGroup>

      <div className="mt-3">
        {state.mode === 'rpm' ? (
          <TextField
            aria-label="每分钟请求数"
            value={state.rpm}
            onChange={(v) => onChange({ ...state, rpm: v })}
          >
            <Label>每分钟请求数</Label>
            <Input type="number" min={1} step={1} placeholder="如 60" />
            <span className="text-muted text-[11px]">超过后 /v1 返回 429，下个整分钟恢复。</span>
          </TextField>
        ) : null}
        {state.mode === 'tpm' ? (
          <TokenField
            label="每分钟 token 数"
            value={state.tpm}
            onChange={(v) => onChange({ ...state, tpm: v })}
            placeholder="如 100K"
            hint="按请求前估算预占、请求后按真实用量校正；可写 100K / 1M。"
          />
        ) : null}
        {state.mode === 'daily_requests' ? (
          <TextField
            aria-label="每天请求数"
            value={state.dailyRequests}
            onChange={(v) => onChange({ ...state, dailyRequests: v })}
          >
            <Label>每天请求数</Label>
            <Input type="number" min={1} step={1} placeholder="如 1000" />
            <span className="text-muted text-[11px]">
              按本地日界计数，跨天自动归零；计数落库，重启不清零。
            </span>
          </TextField>
        ) : null}
        {state.mode === 'daily_tokens' ? (
          <TokenField
            label="每天 token 数"
            value={state.dailyTokens}
            onChange={(v) => onChange({ ...state, dailyTokens: v })}
            placeholder="如 5M"
            hint="按本地日界计数，跨天自动归零；计数落库，重启不清零。可写 1M / 1B。"
          />
        ) : null}
        {state.mode === 'none' ? (
          <span className="text-muted text-[11px]">
            不限制速率与每日用量。终身 Token 配额仍然独立生效。
          </span>
        ) : null}
      </div>
    </div>
  )
}

/** 限额的一行摘要（列表里显示）。 */
export function limitSummary(state: LimitState): string {
  switch (state.mode) {
    case 'rpm':
      return `${state.rpm || '0'} 次/分钟`
    case 'tpm':
      return `${fmtTokens(Number(state.tpm) || 0)} token/分钟`
    case 'daily_requests':
      return `${state.dailyRequests || '0'} 次/天`
    case 'daily_tokens':
      return `${fmtTokens(Number(state.dailyTokens) || 0)} token/天`
    default:
      return '不限'
  }
}
