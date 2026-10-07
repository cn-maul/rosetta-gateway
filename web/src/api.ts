// 管理后台 API 客户端
//
// 关键约定（源自后端 handler 实现，改代码前必读）：
//
// 1. 所有 PATCH 接口都是「字段级部分更新」：
//    - 请求体里**出现的**字段才会被写入，未出现的字段保持原值；
//    - 空串 / 0 是**合法值**，会被真正落库。
//    - 必填字段（name / endpoint / model_id / public_name / provider_id /
//      upstream_model_id / protocol / api_key）显式传空串会返回 400，而不是被静默忽略。
//    所以：只发你要改的字段即可，不必回传完整对象。
//
//    注意：故障转移的策略参数（尝试预算 / 熔断阈值 / 各类超时）**不是**按路由的
//    部分更新字段，它们在「设置」页统一配置（见 saveSettings）。
//
// 2. 任何写操作（create/update/delete）成功后都必须 POST /admin/api/reload，
//    否则内存快照不刷新，新资源对 /v1 不可见。用 mutate() 统一封装。
// 3. 错误响应统一为 {"error":{"message":...,"type":...}}。
// 4. 鉴权头：Authorization: Bearer <会话令牌>。401 只有一个含义——会话无效，
//    清本地令牌并把人送回登录页。

import { reactive } from 'vue'
import type { ApiError, BootstrapStatus, Me, SessionStatus, LoginResult, ConfigImportResult } from './types'

const TOKEN_KEY = 'rosetta_gw_admin_token'

// sessionStorage 而非 localStorage（AUDIT P2-29）。
//
// 差别只在「关掉标签页后是否还在」：localStorage 里的令牌会**长期驻留**，
// 于是任何能在这个源上执行脚本的东西（供应链投毒的依赖、XSS）都能在
// 用户关掉页面很久之后仍偷到一枚 8 小时有效期的凭证；sessionStorage
// 随标签页关闭即消失，暴露窗口从「8 小时」缩到「这个标签页开着的时间」。
//
// 代价是关掉标签页就要重新登录 —— 对一个局域网管理后台可以接受，
// 而这恰恰是 localStorage 唯一能买到的东西。
//
// 关于「多标签页不同步」：不再需要 storage 事件做同步，因为登出现在
// **服务端真的吊销**了（Logout 递增 auth_version，该用户全部旧令牌立即
// 失效）。A 标签页登出后，B 标签页的下一次请求就会拿到 401，走既有的
// 「清本地令牌 + 跳登录页」路径。为此在浏览器里维护一份跨标签页的
// 「登出了」标记是多余的 —— 标记本身还要处理「什么时候该清」的边界，
// 而服务端已经给了唯一权威答案。
export const auth = reactive({
  token: sessionStorage.getItem(TOKEN_KEY) ?? '',
})

export function saveToken(t: string) {
  auth.token = t
  if (t) sessionStorage.setItem(TOKEN_KEY, t)
  else sessionStorage.removeItem(TOKEN_KEY)
}

/**
 * 当前登录用户。
 *
 * 存**内存**而非 localStorage：刷新后需要重新调 /me 确认，
 * 顺手就验证了会话是否还有效（auth_version 变了、被禁用了都会在这里暴露）。
 * 把 is_admin 缓存起来是刻意的 —— 界面要据此隐藏 admin-only 入口，
 * 而每次渲染都问一遍后端既慢又会让模板难写。
 */
export const session = reactive({
  me: null as Me | null,
  /**
   * needsSetup 为真表示系统里存在一个「已建出但还没设密码」的管理员，
   * 登录页据此渲染「首次设置密码」表单而不是登录表单。
   *
   * 2026-10-06 起管理面统一到 users 表：admin_token 与 admin_auth.json
   * 两条通道已删除，第一个管理员由 /admin/api/bootstrap 建出来。
   */
  needsSetup: false,
  /** 待初始化的账号名，供表单标题显示（「为 admin 设置密码」）。 */
  setupUsername: '',
  /** /session 探测是否成功。false 时界面显示「无法连接」而不是登录框。 */
  backendReady: false,
  checked: false,
})

/**
 * isAdmin 便捷读取。
 *
 * 现在只有一种部署形态（users 表 + 会话），所以判定退化成最直接的一行：
 * 看当前身份是不是管理员。未登录时为 false。
 */
export function isAdmin(): boolean {
  return session.me?.is_admin ?? false
}

/**
 * loadSession 探测后端并拉取当前身份。
 *
 * 必须在应用启动时调一次。三个结果都要处理：
 *   - /session 不可用 → backendReady=false，界面显示「无法连接」；
 *   - 未登录 → me=null，界面显示登录页；
 *   - 已登录 → me=<用户>。
 *
 * 首次登录的引导状态（needs_setup）也在这里一并取：登录页要靠它决定
 * 渲染「设密码」还是「登录」。放在这里而不是 Login.vue 里单独调，
 * 是为了让 App 的导航守卫与 Login 的表单**看到同一份状态** ——
 * 两处各判一次就会出现「守卫认为已初始化、登录页还在要密码」。
 */
export async function loadSession(): Promise<void> {
  // 部署可用性**只**由 /session 决定，绝不能被 /me 或 /bootstrap 的成败影响。
  //
  // 为什么必须隔离：/session 免鉴权，永远不会 401/429。其余两者会。
  // 混在一个 try 里时，/me 只要抛一个非 401 的错（429 限速、500、网络抖动），
  // 异常就会冒到上层，而上层会把状态兜底成「未登录」——
  // 于是已登录用户被踢回登录页，且没有任何错误信息。
  try {
    await get<SessionStatus>('/session')
    session.backendReady = true
  } catch {
    session.backendReady = false
    session.checked = true
    return
  }

  try {
    // 先问引导状态再问身份：全新部署时 /me 必然 401，若顺序反了，
    // 我们会在「未登录」的错误分支里提前 return，needs_setup 永远拿不到，
    // 登录页就会显示一个永远登不进去的输入框。
    const bs = await get<BootstrapStatus>('/bootstrap')
    session.needsSetup = bs.needs_setup
    session.setupUsername = bs.username ?? ''
  } catch {
    // 拿不到引导状态不是致命问题：按「已初始化」处理，用户会看到登录表单。
    // 真实情况下（全新部署）它一定能拿到 —— 该端点免鉴权。
    session.needsSetup = false
    session.setupUsername = ''
  }

  try {
    session.me = await get<Me>('/me')
  } catch {
    // 未登录（401）是正常状态，不是错误 —— 交给调用方渲染登录页。
    // 其余状态码（429/500）同样只表示「拿不到身份」，一律降级成未登录。
    session.me = null
  } finally {
    session.checked = true
  }
}

/**
 * login 用用户名+密码换取会话令牌。
 *
 * 成功后必须重新拉一次 /me：token 里有 auth_version 的副本，
 * 而界面要的是「服务端此刻怎么看我」（角色、额度、是否被禁用），
 * 不能只信 token 里那份签发时的快照。
 */
export async function login(username: string, password: string) {
  const r = await post<LoginResult>('/login', { username, password })
  saveToken(r.token)
  session.me = await get<Me>('/me')
  return r
}

/**
 * bootstrapSetup 为「已建出但未设密码」的管理员设置密码并**直接登录**。
 *
 * 一步完成是刻意的：旧流程要先用 admin_token 进后台、再找到 admin、
 * 再重置密码 —— 三步，且中间那步需要知道「去用户管理里找谁」。
 * 统一认证后那条路已经不存在，引导必须自成闭环。
 *
 * 成功后 needs_setup 立刻置 false：这个免鉴权窗口就此关闭，
 * 界面上不该再有任何「设密码」的入口。
 */
export async function bootstrapSetup(password: string) {
  const r = await post<LoginResult>('/bootstrap', { password })
  saveToken(r.token)
  session.me = await get<Me>('/me')
  session.needsSetup = false
  session.setupUsername = ''
  return r
}

/** logout 清除本地会话。后端清 cookie，失败也不阻塞前端登出。 */
export async function logout() {
  try {
    await post('/logout')
  } finally {
    saveToken('')
    session.me = null
  }
}

export class ApiFail extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(method: string, path: string, body?: unknown, timeoutMs = 30_000): Promise<T> {
  const headers: Record<string, string> = {}
  if (auth.token) headers['Authorization'] = 'Bearer ' + auth.token
  if (body !== undefined) headers['Content-Type'] = 'application/json'

  // 超时：网关若挂起（连接不响应），没有 signal 的 fetch 会一直 pending，
  // 页面永远停在「加载中」。AbortSignal.timeout 到点主动中断，让 finally 收场。
  // 默认 30s 对管理端足够：这些接口不代理 /v1 长对话。例外是真的要打上游的
  // 端点（test/discover）：慢上游可能吃满设置页配的 upstream_timeout_ms，
  // 调用方按需传更长的 timeoutMs，别在这里一刀切。
  let res: Response
  try {
    res = await fetch('/admin/api' + path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(timeoutMs),
    })
  } catch (e) {
    const name = e instanceof DOMException ? e.name : ''
    if (name === 'TimeoutError' || name === 'AbortError') {
      throw new ApiFail(0, '请求超时，请稍后重试')
    }
    throw new ApiFail(0, '网络错误：' + (e instanceof Error ? e.message : String(e)))
  }

  if (res.status === 401) {
    // 401 只有一个含义：**会话无效**（未登录 / 过期 / 密码已改 / 账号被禁用）。
    // 统一认证后没有第二种情况了 —— 此前还有一个 admin_token 通道需要区分，
    // 那个通道已删除。
    //
    // 清掉本地令牌并把 me 置空，让 App.vue 的 watch 把人送回登录页。
    // 不能「什么都不做只弹框」：界面会停在一个永远 401 的空壳上。
    saveToken('')
    session.me = null
    throw new ApiFail(401, '登录已失效，请重新登录')
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

export const get = <T>(path: string, timeoutMs?: number) => request<T>('GET', path, undefined, timeoutMs)

/**
 * 下载一个文件（导出配置、调用历史 CSV 用）。
 *
 * 为什么不用 <a href> / window.open 直链：管理 API 的凭据由前端显式带
 * Authorization 头（令牌在 sessionStorage），不依赖登录时种下的会话 cookie
 * —— 直链在 cookie 缺失（过期被清、脚本环境）时会拿到一个 401 的 JSON
 * 而不是文件，用户只会看到「下载了个打不开的东西」。走 fetch 还能复用
 * request() 的 401 处理：会话失效时当场清令牌送回登录页。
 */
async function download(method: string, path: string, body?: unknown, fallbackName = 'rosetta-config.json'): Promise<void> {
  const headers: Record<string, string> = {}
  if (auth.token) headers['Authorization'] = 'Bearer ' + auth.token
  if (body !== undefined) headers['Content-Type'] = 'application/json'

  let res: Response
  try {
    res = await fetch('/admin/api' + path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(60_000),
    })
  } catch (e) {
    const name = e instanceof DOMException ? e.name : ''
    if (name === 'TimeoutError' || name === 'AbortError') {
      throw new ApiFail(0, '导出超时，请稍后重试')
    }
    throw new ApiFail(0, '网络错误：' + (e instanceof Error ? e.message : String(e)))
  }

  if (res.status === 401) {
    // 与 request() 同语义：401 只代表会话失效，必须原样处理，
    // 否则页面停在一个永远失败的导出按钮上。
    saveToken('')
    session.me = null
    throw new ApiFail(401, '登录已失效，请重新登录')
  }
  if (!res.ok) {
    // 错误响应仍是 JSON，形状与其他端点一致。
    let msg = `导出失败 (HTTP ${res.status})`
    try {
      const data = await res.json()
      if (data?.error?.message) msg = data.error.message
    } catch {
      /* 非 JSON 错误体，保留通用文案 */
    }
    throw new ApiFail(res.status, msg)
  }

  // 服务端给的文件名形如 rosetta-config-20261006-230815.json
  const cd = res.headers.get('Content-Disposition') ?? ''
  const m = /filename="?([^";]+)"?/.exec(cd)
  const filename = m ? m[1] : fallbackName

  const blob = await res.blob()
  const url = URL.createObjectURL(blob)
  try {
    const a = document.createElement('a')
    a.href = url
    a.download = filename
    document.body.appendChild(a)
    a.click()
    a.remove()
  } finally {
    // 立刻 revoke：配置清单可能含全部上游凭据，让它尽可能短地留在内存里。
    // 延后到下一轮事件循环 revoke 是为了确保点击已经派发。
    setTimeout(() => URL.revokeObjectURL(url), 0)
  }
}
export const post = <T>(path: string, body?: unknown, timeoutMs?: number) =>
  request<T>('POST', path, body ?? {}, timeoutMs)
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
    // 401 必须原样上抛。旧实现把它包成 status 0，于是所有 view 的
    // `status !== 401` 守卫全部失效 —— 令牌过期时会在「请重新登录」的弹窗之上
    // 再叠一条错误 toast，真正的原因（令牌失效）被淹没在噪音里。
    if (e instanceof ApiFail && e.status === 401) throw e
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
  RouteTarget,
  RouteTargetInput,
  AccessKey,
  AuditEntry,
  KeyCreateResponse,
  Settings,
  Stats,
  UsageGroupEntry,
  UsageHistoryPage,
  HistoryFilters,
  PruneResult,
  RecomputeUsageResult,
  User,
  UserRole,
  UserStatus,
  Group,
} from './types'

/**
 * 调用历史列表 / CSV 导出共用的 query 构造。
 *
 * 两个端点在后端共用同一套 WHERE（status / model / key_id 精确过滤），
 * 这里也只写一处，避免「列表带过滤、导出丢过滤」的口径漂移。
 * undefined / 空串的维度不拼进 query（后端视为不过滤）。
 */
function usageHistoryQuery(days: number, filters?: HistoryFilters): string {
  const to = Date.now()
  const from = to - days * 86400_000
  const q = new URLSearchParams({ from: String(from), to: String(to) })
  if (filters?.status) q.set('status', filters.status)
  if (filters?.model) q.set('model', filters.model)
  if (filters?.keyId) q.set('key_id', filters.keyId)
  return q.toString()
}

export const api = {
  // providers
  providers: () => get<Provider[]>('/providers'),
  createProvider: (b: ProviderCreatePayload) => mutate(() => post<Provider>('/providers', b)),
  updateProvider: (id: string, b: ProviderUpdatePayload) => mutate(() => patch<Provider>(`/providers/${id}`, b)),
  deleteProvider: (id: string) => mutate(() => del(`/providers/${id}`)),
  testProvider: (id: string, timeoutMs?: number) =>
    post<{ status?: string; message?: string }>(`/providers/${id}/test`, undefined, timeoutMs),

  // credentials（挂在 provider 下）
  credentials: (providerId: string) => get<Credential[]>(`/providers/${providerId}/credentials`),
  createCredential: (providerId: string, b: { label: string; api_key: string; weight?: number }) =>
    mutate(() => post<Credential>(`/providers/${providerId}/credentials`, b)),
  updateCredential: (id: string, b: { label?: string; api_key?: string; weight?: number; enabled?: boolean }) =>
    mutate(() => patch<Credential>(`/credentials/${id}`, b)),
  deleteCredential: (id: string) => mutate(() => del(`/credentials/${id}`)),

  // upstream models（挂在 provider 下；discover 实时拉取上游 /models 列表）
  models: (providerId: string) => get<UpstreamModel[]>(`/providers/${providerId}/models`),
  // 全部上游模型的扁平列表（跨 provider）。Routes/Settings 用它一次取全，
  // 避免「先取 providers 再逐 provider 取 models」的 1+N 请求。不含吞吐统计。
  allModels: () => get<UpstreamModel[]>('/upstream-models'),
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

  // 故障转移目标链（有序）。整体替换后由 mutate 统一 reload 快照。
  routeTargets: (routeId: string) => get<RouteTarget[]>(`/routes/${routeId}/targets`),
  saveRouteTargets: (routeId: string, targets: RouteTargetInput[]) =>
    mutate(() => put<RouteTarget[]>(`/routes/${routeId}/targets`, { targets })),

  // access keys
  keys: () => get<AccessKey[]>('/keys'),
  createKey: (b: {
    name: string
    quota_tokens?: number
    rpm_limit?: number
    tpm_limit?: number
    /**
     * 归属用户（多用户改造）。
     *
     * 只有管理员需要显式传 —— 普通用户调用时后端会**忽略**这个字段
     * 并强制归为自己（见 internal/admin/key_handler.go 的 Create）。
     * 所以普通用户界面不需要这个输入框。
     */
    user_id?: string
    /**
     * key 级模型白名单（P1）。
     *
     * 空数组 / 不传 = 不限制（与「用户所属组」的白名单求交，且 key 级只能更紧）。
     */
    allowed_models?: string[]
    /** 有效期截止（毫秒时间戳，P2）。0 = 永不过期。 */
    expires_at?: number
    /** 来源 IP 白名单：逗号分隔的 CIDR 或单 IP。空串 = 不限制。 */
    allowed_ips?: string
    /** key 级分组覆盖（P2，仅管理员有效）。空串 = 沿用用户所属分组。 */
    group_id?: string
  }) => mutate(() => post<KeyCreateResponse>('/keys', b)),
  updateKey: (
    id: string,
    b: {
      name?: string
      enabled?: boolean
      quota_tokens?: number
      rpm_limit?: number
      tpm_limit?: number
      /**
       * PATCH 语义：
       *   - 不传（字段缺席）→ 保持原白名单；
       *   - 传 []          → **清除**白名单（回到「不限制」）；
       *   - 传非空数组     → 覆盖。
       * 「清除」用空数组表达是因为界面上的动作就是「取消所有勾选后保存」。
       */
      allowed_models?: string[]
      /** 0 = 改成永不过期；字段不传 = 保持原值。 */
      expires_at?: number
      /** 空串 = 清空白名单（不限制）；字段不传 = 保持原值。 */
      allowed_ips?: string
      /** 空串 = 取消分组覆盖（沿用用户所属分组）；仅管理员可设。 */
      group_id?: string
    },
  ) => mutate(() => patch<AccessKey>(`/keys/${id}`, b)),
  deleteKey: (id: string) => mutate(() => del(`/keys/${id}`)),
  /**
   * 重算某把密钥的 used_tokens（配额漂移的自愈手段）。
   *
   * 刻意**不包 mutate**：它只修正库里的派生计数，不改路由/凭据快照，
   * 没有「改完必须 reload」的语义。
   */
  recomputeKeyUsage: (id: string) => post<RecomputeUsageResult>(`/keys/${id}/recompute-usage`),

  // ---------- 多用户与会话 ----------

  /**
   * 用户列表（仅管理员）。
   *
   * 刻意**不包 mutate**：它不改变运行时快照，改账号不需要 reload。
   * 真正需要 reload 的是「禁用账号」—— 那条在后端自己调了 reload
   * （user_admin_handler.UpdateUser），因为被禁用用户的 key 要立刻失效。
   */
  users: () => get<User[]>('/users'),
  createUser: (b: {
    username: string
    password?: string
    display_name?: string
    role?: UserRole
    /** 分组 id（P1）。不传 / 空串 = 未分组 = 不受模型白名单限制。 */
    group_id?: string
    quota_tokens?: number
    remark?: string
  }) => post<User>('/users', b),
  updateUser: (
    id: string,
    b: {
      display_name?: string
      role?: UserRole
      status?: UserStatus
      /**
       * PATCH 语义：
       *   - 不传 → 保持原分组；
       *   - 传 ""  → 移出分组；
       *   - 传 id  → 换到该组。
       * 「移出分组」必须能用空串表达，因为那是界面上的一个明确动作。
       */
      group_id?: string
      quota_tokens?: number
      remark?: string
    },
  ) => patch<User>(`/users/${id}`, b),
  deleteUser: (id: string) => del(`/users/${id}`),
  /** 管理员重置他人密码。会递增该用户的 auth_version，其所有会话立即失效。 */
  resetUserPassword: (id: string, password: string) =>
    post(`/users/${id}/password`, { password }),
  /** 自助改密码。会递增自己的 auth_version，**当前会话也随即失效**。 */
  changeMyPassword: (old_password: string, new_password: string) =>
    post('/me/password', { old_password, new_password }),
  // 审计日志：管理后台写操作留痕（只记字段名，不记值）
  audit: (limit = 50) => get<{ entries: AuditEntry[]; server_ts: number }>(`/audit?limit=${limit}`),

  // ---------- 分组与模型白名单（P1）----------
  //
  // 全部走 mutate()：模型可见性随内存快照下发，写操作后必须 reload 才生效。
  // （后端还有 server.AutoReload 兜底，这里是前端自己的契约，两边都做不冲突
  //   —— 重复重建只是一次内存重建，而漏掉重建是「改了权限不生效」的静默故障。）

  groups: () => get<Group[]>('/groups'),
  createGroup: (b: { name: string; description?: string }) =>
    mutate(() => post<Group>('/groups', b)),
  updateGroup: (id: string, b: { name?: string; description?: string }) =>
    mutate(() => patch<Group>(`/groups/${id}`, b)),
  /**
   * 删除分组。组里还有账号时后端返回 409（否则那批人的模型权限会被放大），
   * 调用方要把这条错误原样展示给管理员，别吞掉。
   */
  deleteGroup: (id: string) => mutate(() => del(`/groups/${id}`)),
  /**
   * 整体替换某组的模型白名单（PUT 而非 PATCH：勾选后保存是「替换」语义，
   * 取消勾选无法用增量表达）。
   *
   * 传空数组 = 清空白名单 = 该组**不限制**可见范围。
   */
  setGroupModels: (id: string, models: string[]) =>
    mutate(() => put<{ models: string[]; note: string }>(`/groups/${id}/models`, { models })),

  /**
   * 当前调用者可选用的公开模型名清单（普通用户也可用，已按身份收窄）。
   *
   * 刻意不复用 /admin/api/routes：那是 admin-only（会暴露全部路由拓扑），
   * 而 key 级白名单的设计意图就是「用户自己收紧」—— 普通用户读不到清单，
   * 这个功能对他们就等于不存在。
   */
  modelNames: () =>
    get<{ models: string[]; restricted: boolean }>('/model-names').then((r) => ({
      models: r.models ?? [],
      restricted: r.restricted ?? false,
    })),

  // stats & usage（注意：usage 端点的 from/to 是【毫秒】时间戳；from=0 表示全部历史）
  // includeLifetime 会让响应带上 lifetime（表 B 终身累计，含 first_record_at）。
  // 后端只对管理员返回该字段；普通用户传了也只会缺席。
  stats: (from = 0, to = Date.now(), includeLifetime = false) =>
    get<Stats>(`/stats?from=${from}&to=${to}${includeLifetime ? '&include_lifetime=true' : ''}`),
  settings: () => get<Settings>('/settings'),
  // 设置里含运行时全局默认（超时 + 故障转移策略），这些值由快照驱动转发路径，
  // 所以必须走 mutate() 触发 reload 才能即时生效（模型容量默认也一并保存）。
  saveSettings: (b: Settings) => mutate(() => put<Settings>('/settings', b)),

  // ---------- 供应商 / 模型 导入导出 ----------
  //
  // 导出走独立的下载路径而**不是** request<T>：响应是文件而非 JSON，
  // 而 request() 无条件 JSON.parse 一个文件大小的配置清单纯属浪费，
  // 真正需要读 JSON 的只有「导入前预览文件内容」那一步。
  // 导出必加密、必带凭据（2026-10-07）：后端对空口令直接 400。
  exportConfig: (passphrase: string) =>
    download('POST', '/config-export/export', { passphrase }, 'rosetta-config.json.enc'),
  importConfig: (data: unknown, passphrase: string, dryRun: boolean) =>
    post<ConfigImportResult>('/config-export/import', { data, passphrase, dry_run: dryRun }),
  usageByDay: (from: number, to: number) => get<UsageGroupEntry[]>(`/usage/by-day?from=${from}&to=${to}`),
  usageByModel: (from = 0, to = Date.now()) => get<UsageGroupEntry[]>(`/usage/by-model?from=${from}&to=${to}&limit=10`),
  usageByKey: (from = 0, to = Date.now()) => get<UsageGroupEntry[]>(`/usage/by-key?from=${from}&to=${to}&limit=10`),
  usageByProvider: (from = 0, to = Date.now()) =>
    get<UsageGroupEntry[]>(`/usage/by-provider?from=${from}&to=${to}&limit=10`),
  /**
   * 手动触发用量归档剪枝（明细 → 按天累计）。
   *
   * 同样不包 mutate：动的是用量数据，运行时快照不受影响。
   * 返回的 skipped=true 不是错误（水位已到位 / 无待归档数据），原因在 reason。
   */
  pruneUsage: () => post<PruneResult>('/usage/prune'),

  // 调用历史：分页查询（limit = 每页条数，offset = 偏移）。
  // 返回 { records, total }，total 是**过滤后的总条数**（不受分页影响）。
  usageHistory: (days = 7, limit = 20, offset = 0, filters?: HistoryFilters) =>
    get<UsageHistoryPage>(
      `/usage/history?${usageHistoryQuery(days, filters)}&limit=${limit}&offset=${offset}`,
    ),
  // 调用历史 CSV 导出：与列表同参数。响应是文件，走 download() 而非 request()；
  // 行数上限由后端钳制（maxUsageLimit），这里不重复传 limit。
  exportUsageCSV: (days = 7, filters?: HistoryFilters) =>
    download('GET', `/usage/history.csv?${usageHistoryQuery(days, filters)}`, undefined, 'usage-history.csv'),
}
