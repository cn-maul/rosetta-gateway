export type CcSwitchApp = 'codex' | 'claude' | 'opencode' | 'hermes'

interface ImportLinkBase {
  origin: string
  apiKey: string
  name: string
}

interface CcSwitchImportLink extends ImportLinkBase {
  app: CcSwitchApp
  model?: string
}

interface CcSwitchTarget {
  apiFormat: 'openai_chat' | 'openai_responses' | 'anthropic'
  endpoint: (origin: string) => string
}

interface MagpieImportLink extends ImportLinkBase {
  models?: string[]
}

function gatewayOrigin(origin: string): string {
  const url = new URL(origin)
  if (url.protocol !== 'http:' && url.protocol !== 'https:') {
    throw new Error('网关地址必须是 http 或 https')
  }
  return url.origin
}

function providerName(name: string): string {
  const full = `Rosetta Gateway - ${name.trim() || 'API Key'}`
  return Array.from(full).slice(0, 80).join('')
}

/**
 * 每个应用选择它能原生理解的入站协议。网关同时提供 Chat、Responses 和
 * Anthropic 入口，因此不需要让 CC Switch 再做一次本地协议转换：
 * Anthropic 的 base URL 不带 /v1，两个 OpenAI 协议则带 /v1。
 */
const ccSwitchTargets: Record<CcSwitchApp, CcSwitchTarget> = {
  codex: {
    apiFormat: 'openai_responses',
    endpoint: (origin) => `${origin}/v1`,
  },
  claude: {
    apiFormat: 'anthropic',
    endpoint: (origin) => origin,
  },
  opencode: {
    apiFormat: 'openai_chat',
    endpoint: (origin) => `${origin}/v1`,
  },
  hermes: {
    apiFormat: 'openai_chat',
    endpoint: (origin) => `${origin}/v1`,
  },
}

/** CC Switch 的 provider 深链接；应用没有通用供应商，必须指定 app。 */
export function buildCcSwitchImportLink({ origin, apiKey, name, app, model }: CcSwitchImportLink): string {
  const base = gatewayOrigin(origin)
  const target = ccSwitchTargets[app]
  const params = new URLSearchParams({
    resource: 'provider',
    app,
    name: providerName(name),
    endpoint: target.endpoint(base),
    apiKey,
    apiFormat: target.apiFormat,
  })
  if (model?.trim()) params.set('model', model.trim())
  return `ccswitch://v1/import?${params.toString()}`
}

/**
 * Magpie 的网页导入链接。
 *
 * 参数必须放在 # 后：浏览器不会把片段发给 usemagpie.ai，密钥只留在本机。
 */
export function buildMagpieImportLink({ origin, apiKey, name, models }: MagpieImportLink): string {
  const base = gatewayOrigin(origin)
  const params = new URLSearchParams({
    name: providerName(name),
    chat: `${base}/v1`,
    responses: `${base}/v1`,
    anthropic: base,
    key: apiKey,
  })
  const selected = (models ?? []).map((m) => m.trim()).filter(Boolean)
  if (selected.length) params.set('models', selected.join(','))
  return `https://usemagpie.ai/import#${params.toString()}`
}
