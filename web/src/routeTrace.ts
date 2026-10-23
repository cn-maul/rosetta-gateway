// 与 cmd/gateway/main.go appendRouteTrace 的记录格式对应。
// 编号是运行时决策顺序，不能用来推断静态链位置或实际发出了几次请求。
export interface TraceStep {
  raw: string
  attempt: number | null
  provider: string
  model: string
  outcome: string
  outcomeLabel: string
  explanation: string
  reason: string
  reasonLabel: string
  parsed: boolean
}

const outcomes: Record<string, { label: string; explanation: string }> = {
  skip: { label: '跳过', explanation: '此目标本次未进入上游调用。' },
  fail: { label: '无法调用', explanation: '未取得可用凭据，未向此目标发起调用。' },
  ok: { label: '完成响应', explanation: '' },
  committed_fail: { label: '响应开始后失败', explanation: '内容已开始返回，无法再切换目标。' },
  fail_switch: { label: '失败，允许转移', explanation: '错误符合转移条件；是否继续尝试还受后备目标和预算限制。' },
  fail_terminal: { label: '失败，停止转移', explanation: '此错误不符合转移条件，本次不再选择后备目标。' },
}
const reasons: Record<string, string> = {
  probe_busy_or_cooling: '目标仍在熔断冷却，或半开探测名额已被占用',
  no_healthy_credential: '没有可用的健康凭据',
  upstream_error: '上游响应、解析或连接发生错误；具体原因请结合错误详情判断',
  upstream_timeout: '上游超时或连接失败',
  upstream_auth_error: '上游鉴权失败，请检查凭据或权限',
  upstream_quota_exhausted: '上游额度耗尽',
  rate_limit_exceeded: '触发上游限流',
  model_not_found: '上游模型或端点不存在',
  context_length_exceeded: '请求超出上下文限制',
  invalid_request_error: '上游拒绝了请求参数',
  internal_error: '网关内部错误，请结合错误详情排查',
}

function unparsed(raw: string): TraceStep {
  return { raw, attempt: null, provider: '', model: '', outcome: '', outcomeLabel: '原始记录', explanation: '', reason: '', reasonLabel: '', parsed: false }
}

export function parseTraceStep(raw: string): TraceStep {
  const value = raw.trim()
  // 转义的 "/"、";"、"\"" 不能参与分隔；用 unescape 后的逻辑读取字段。
  const arrow = findUnescapedLast(value, ' -> ')
  const colon = findUnescaped(value, ':', 0)
  if (colon <= 0 || arrow <= colon) return unparsed(value)
  const index = value.slice(0, colon)
  if (!/^[1-9]\d*$/.test(index)) return unparsed(value)
  const attempt = Number(index)
  if (!Number.isSafeInteger(attempt)) return unparsed(value)
  const target = value.slice(colon + 1, arrow)
  const slash = findUnescaped(target, '/', 0)
  if (slash <= 0 || slash === target.length - 1) return unparsed(value)
  const provider = unescapeRouteTraceComponent(target.slice(0, slash))
  const model = unescapeRouteTraceComponent(target.slice(slash + 1))
  const result = value.slice(arrow + 4)
  const separator = findUnescaped(result, ':', 0)
  const outcome = separator < 0 ? result : result.slice(0, separator)
  const reason = separator < 0 ? '' : result.slice(separator + 1)
  if (!outcome) return unparsed(value)
  const known = Object.hasOwn(outcomes, outcome) ? outcomes[outcome] : undefined
  return {
    raw: value, attempt, provider, model, outcome, reason,
    outcomeLabel: known?.label ?? `未知结果：${outcome}`,
    explanation: known?.explanation ?? '该结果未在当前界面定义，请查阅原始记录。',
    reasonLabel: Object.hasOwn(reasons, reason) ? reasons[reason] : reason,
    parsed: true,
  }
}

/** 找第一个未转义的分隔符；反斜杠用于转义下一个字符。 */
function findUnescaped(value: string, needle: string, from: number): number {
  for (let i = from; i <= value.length - needle.length; i++) {
    if (isEscaped(value, i)) continue
    if (value.startsWith(needle, i)) return i
  }
  return -1
}

/** 找最后一个未转义的分隔符；模型名中的箭头不能成为结果分隔点。 */
function findUnescapedLast(value: string, needle: string): number {
  for (let i = value.length - needle.length; i >= 0; i--) {
    if (isEscaped(value, i)) continue
    if (value.startsWith(needle, i)) return i
  }
  return -1
}

function isEscaped(value: string, index: number): boolean {
  let backslashes = 0
  for (let i = index - 1; i >= 0 && value[i] === '\\'; i--) backslashes++
  return backslashes % 2 === 1
}

/** 与 Go 侧 escapeRouteTraceComponent 对称：只还原本格式用到的转义。 */
function unescapeRouteTraceComponent(value: string): string {
  let out = ''
  for (let i = 0; i < value.length; i++) {
    const ch = value[i]
    if (ch === '\\' && i + 1 < value.length && ['\\', ';', '/'].includes(value[i + 1])) {
      out += value[++i]
    } else {
      out += ch
    }
  }
  return out
}

export function parseRouteTrace(raw: string | null | undefined): TraceStep[] {
  const steps: string[] = []
  let start = 0
  const value = raw ?? ''
  for (let i = 0; i < value.length; i++) {
    if (value[i] === ';' && !isEscaped(value, i)) {
      steps.push(value.slice(start, i).trim())
      start = i + 1
    }
  }
  steps.push(value.slice(start).trim())
  return steps.filter(Boolean).map(parseTraceStep)
}
