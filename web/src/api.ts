// 管理后台 API 客户端
//
// 关键约定（源自后端 handler 实现，改代码前必读）：
//
// 1. 所有 PATCH 接口都是「字段级部分更新」：
//    - 请求体里**出现的**字段才会被写入，未出现的字段保持原值；
//    - 空串 / 0 是**合法值**，会被真正落库（空串在库里落 NULL）。
//      例如 priority: 0 能真的把优先级改回 0，fallback_route_id: "" 能真的清空兜底路由。
//    - 必填字段（name / endpoint / model_id / public_name / provider_id /
//      upstream_model_id / protocol / api_key）显式传空串会返回 400，而不是被静默忽略。
//    所以：只发你要改的字段即可，不必回传完整对象。
//
// 2. 任何写操作（create/update/delete）成功后都必须 POST /admin/api/reload，
//    否则内存快照不刷新，新资源对 /v1 不可见。用 mutate() 统一封装。
// 3. 错误响应统一为 {"error":{"message":...,"type":...}}。
// 4. 鉴权头：Authorization: Bearer <admin_token>，401 时弹 token 输入框。

import { reactive } from 'vue'
import { authState } from './ui'
import type { ApiError } from './types'

const TOKEN_KEY = 'rosetta_gw_admin_token'

export const auth = reactive({
  token: localStorage.getItem(TOKEN_KEY) ?? '',
  ready: false,
})

export function saveToken(t: string) {
  auth.token = t
  if (t) localStorage.setItem(TOKEN_KEY, t)
  else localStorage.removeItem(TOKEN_KEY)
}

export class ApiFail extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {}
  if (auth.token) headers['Authorization'] = 'Bearer ' + auth.token
  if (body !== undefined) headers['Content-Type'] = 'application/json'

  const res = await fetch('/admin/api' + path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
  })

  if (res.status === 401) {
    authState.needToken = true
    throw new ApiFail(401, '需要管理员令牌')
  }

  const text = await res.text()
  let data: any = null
  try {
    data = text ? JSON.parse(text) : null
  } catch {
    /* 非 JSON 响应（理论上只有错误时会这样） */
  }

  if (!res.ok) {
    const err = data?.error as ApiError | undefined
    throw new ApiFail(res.status, err?.message ?? `请求失败 (HTTP ${res.status})`)
  }
  return data as T
}

export const get = <T>(path: string) => request<T>('GET', path)
export const post = <T>(path: string, body?: unknown) => request<T>('POST', path, body ?? {})
export const put = <T>(path: string, body: unknown) => request<T>('PUT', path, body)
export const patch = <T>(path: string, body: unknown) => request<T>('PATCH', path, body)
export const del = <T>(path: string) => request<T>('DELETE', path)

/**
 * 写操作统一入口：成功后自动 reload 内存快照。
 * reload 失败不静默 —— 明确提示「已保存但需手动刷新快照」。
 */
export async function mutate<T>(fn: () => Promise<T>): Promise<T> {
  const result = await fn()
  try {
    await post('/reload')
  } catch (e) {
    throw new ApiFail(
      0,
      '资源已保存，但刷新内存快照失败：' + (e instanceof Error ? e.message : String(e)) +
        '。请稍后在页面上手动触发一次写操作或重启网关。',
    )
  }
  return result
}

// ---------- 业务端点封装 ----------

import type {
  Provider,
  ProviderCreatePayload,
  ProviderUpdatePayload,
  Credential,
  UpstreamModel,
  DiscoveredModel,
  ModelImportItem,
  Route,
  AccessKey,
  KeyCreateResponse,
  Settings,
  Stats,
  UsageGroupEntry,
  UsageHistoryEntry,
} from './types'

export const api = {
  // providers
  providers: () => get<Provider[]>('/providers'),
  createProvider: (b: ProviderCreatePayload) => mutate(() => post<Provider>('/providers', b)),
  updateProvider: (id: string, b: ProviderUpdatePayload) => mutate(() => patch<Provider>(`/providers/${id}`, b)),
  deleteProvider: (id: string) => mutate(() => del(`/providers/${id}`)),
  testProvider: (id: string) => post<{ status?: string; message?: string }>(`/providers/${id}/test`),

  // credentials（挂在 provider 下）
  credentials: (providerId: string) => get<Credential[]>(`/providers/${providerId}/credentials`),
  createCredential: (providerId: string, b: { label: string; api_key: string; weight?: number }) =>
    mutate(() => post<Credential>(`/providers/${providerId}/credentials`, b)),
  updateCredential: (id: string, b: { label?: string; api_key?: string; weight?: number; enabled?: boolean }) =>
    mutate(() => patch<Credential>(`/credentials/${id}`, b)),
  deleteCredential: (id: string) => mutate(() => del(`/credentials/${id}`)),

  // upstream models（挂在 provider 下；discover 实时拉取上游 /models 列表）
  models: (providerId: string) => get<UpstreamModel[]>(`/providers/${providerId}/models`),
  createModel: (providerId: string, b: Partial<UpstreamModel>) =>
    mutate(() => post<UpstreamModel>(`/providers/${providerId}/models`, b)),
  updateModel: (id: string, b: Partial<UpstreamModel>) => mutate(() => patch<UpstreamModel>(`/models/${id}`, b)),
  deleteModel: (id: string) => mutate(() => del(`/models/${id}`)),
  discoverModels: (providerId: string) =>
    post<{ status: string; message?: string; models?: DiscoveredModel[] }>(`/providers/${providerId}/models/discover`),
  importModels: (providerId: string, models: ModelImportItem[]) =>
    mutate(() => post<{ status: string; imported: number }>(`/providers/${providerId}/models/import`, { models })),

  // routes
  routes: () => get<Route[]>('/routes'),
  createRoute: (b: Partial<Route>) => mutate(() => post<Route>('/routes', b)),
  updateRoute: (id: string, b: Partial<Route>) => mutate(() => patch<Route>(`/routes/${id}`, b)),
  deleteRoute: (id: string) => mutate(() => del(`/routes/${id}`)),

  // access keys
  keys: () => get<AccessKey[]>('/keys'),
  createKey: (b: { name: string }) =>
    mutate(() => post<KeyCreateResponse>('/keys', b)),
  updateKey: (id: string, b: { name?: string; enabled?: boolean }) => mutate(() => patch<AccessKey>(`/keys/${id}`, b)),
  deleteKey: (id: string) => mutate(() => del(`/keys/${id}`)),

  // stats & usage（注意：usage 端点的 from/to 是【毫秒】时间戳）
  stats: () => get<Stats>('/stats'),
  settings: () => get<Settings>('/settings'),
  saveSettings: (b: Settings) => put<Settings>('/settings', b),
  usageByDay: (days = 14) => {
    const to = Date.now()
    const from = to - days * 86400_000
    return get<UsageGroupEntry[]>(`/usage/by-day?from=${from}&to=${to}`)
  },
  usageByModel: () => get<UsageGroupEntry[]>('/usage/by-model?limit=10'),
  usageByKey: () => get<UsageGroupEntry[]>('/usage/by-key?limit=10'),
  usageHistory: (days = 7, limit = 200) => {
    const to = Date.now()
    const from = to - days * 86400_000
    return get<UsageHistoryEntry[]>(`/usage/history?from=${from}&to=${to}&limit=${limit}`)
  },
}
