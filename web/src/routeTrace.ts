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
  // 模型名可含 / 和 :，只拆第一个 /；结果以最后的箭头为界。
  const arrow = value.lastIndexOf(' -> ')
  const colon = value.indexOf(':')
  if (colon <= 0 || arrow <= colon) return unparsed(value)
  const index = value.slice(0, colon)
  if (!/^[1-9]\d*$/.test(index)) return unparsed(value)
  const attempt = Number(index)
  if (!Number.isSafeInteger(attempt)) return unparsed(value)
  const target = value.slice(colon + 1, arrow)
  const slash = target.indexOf('/')
  if (slash <= 0 || slash === target.length - 1) return unparsed(value)
  const provider = target.slice(0, slash)
  const model = target.slice(slash + 1)
  const result = value.slice(arrow + 4)
  const separator = result.indexOf(':')
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

export function parseRouteTrace(raw: string | null | undefined): TraceStep[] {
  return (raw ?? '').split(';').map(v => v.trim()).filter(Boolean).map(parseTraceStep)
}
