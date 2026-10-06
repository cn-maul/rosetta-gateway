// 后端契约类型（与 internal/admin/*.go 的 response 结构一一对应）

export interface Provider {
  id: string
  slug: string
  name: string
  protocol: string
  endpoint: string
  enabled: boolean
  timeout_ms: number
  max_retries: number
  quirks_json: string
  created_at: number
  // ready 是**运行时**状态（后端快照），不是库里的配置字段：
  // 库只记录配了什么，快照才反映「上游池实际建出了什么」。
  // ready=false 表示一条可用凭据都没有 —— 请求打过去必然失败，
  // 而在改造前这个状态只躺在日志里，界面上看不出来。
  // 只在 enabled 时有意义：已停用的 provider 本来就不参与池构建。
  ready: boolean
  // ready_reason 面向运维的一句话原因（凭据解密失败 / 读不到凭据清单 / 无凭据）。
  ready_reason?: string
}

// 新增上游只需这三项 + 可选 api_key；slug、启用、超时、重试由后端决定。
export interface ProviderCreatePayload {
  name: string
  protocol: string
  endpoint: string
  api_key?: string
}

// PATCH /providers/{id} 的载荷：
// 只发要改的字段，未出现的字段后端保持原值；传 0 / "" 是显式的「改回默认 / 清空」。
// 刻意不含 slug —— 该字段创建后不可修改，后端即使收到也忽略。
// 也不含 id / created_at —— 服务端管理的只读字段。
export interface ProviderUpdatePayload {
  name?: string
  protocol?: string
  endpoint?: string
  enabled?: boolean
  timeout_ms?: number
  max_retries?: number
  quirks_json?: string
}

// discover 端点返回的上游模型条目；后端已把探测不到的容量替换为设置里的默认值
export interface DiscoveredModel {
  model_id: string
  display_name?: string
  context_window?: number
  max_output_tokens?: number
}

// 批量导入的单条模型
export interface ModelImportItem {
  model_id: string
  display_name?: string
  context_window?: number
  max_output_tokens?: number
}

// 全局设置（与 internal/admin/settings_handler.go 对应）。
// Get 返回的是**生效值**：DB 未配置时回显 config.json 的值。
export interface Settings {
  default_context_window: number
  default_max_output_tokens: number

  // 运行时全局默认（超时 + 故障转移策略）。原先散落在每条路由上，现统一在此配置。
  upstream_timeout_ms: number
  stream_idle_timeout_ms: number
  stream_first_token_timeout_ms: number
  failover_max_targets: number
  failover_failure_threshold: number
}

export interface Credential {
  id: string
  provider_id: string
  label: string
  api_key_mask: string // 形如 abcd...wxyz，明文永不下发
  enabled: boolean
  weight: number
  status: string
  created_at: number
}

export interface UpstreamModel {
  id: string
  provider_id: string
  model_id: string
  display_name: string
  enabled: boolean
  context_window: number
  max_output_tokens: number
  // 单价（元 / 百万 tokens），0 = 未配置（费用按 0 计）。
  // price_input 是缓存未命中的输入价，price_cache_hit 是缓存命中的输入价，
  // price_output 是输出价。
  price_input: number
  price_cache_hit: number
  price_output: number
  tokens_per_sec?: number // 近 5 次真实调用的平均输出速度；未调用过则不下发
  ttfb_ms?: number // 近 5 次流式调用的平均首字延迟（毫秒）；非流式/未测得则不下发
  success_rate?: number // 近 100 次调用成功率（0~1）
  call_count?: number // 成功率样本量（≤100）；缺失/0 表示未调用过
  // 没有 created_at：upstream_models 表压根没有这一列，接口也不下发。
  // 曾在这里声明过一次，导致列表里永远渲染出一个「—」日期列。
  // 也没有 default_extra_json：存了但没有任何地方拿它构造请求，已随列一并摘除。
}

export interface Route {
  id: string
  public_name: string
  provider_id: string
  upstream_model_id: string // 外键 → upstream_models.id（短哈希）；= 链首主目标
  enabled: boolean
  created_at: number

  // 是否启用自动故障转移。链成员与顺序见 route_targets。
  // 策略参数（尝试预算/熔断阈值/超时）是全局的，在「设置」页配置。
  failover_enabled: boolean
}

// 一条 route 的有序上游目标（position 越小越先尝试）
export interface RouteTarget {
  id: string
  route_id: string
  provider_id: string
  provider_name: string
  upstream_model_id: string
  model_id: string
  position: number
  enabled: boolean
}

// 整体替换链时提交的元素：position 由数组顺序决定，enabled 省略即 true
export interface RouteTargetInput {
  provider_id: string
  upstream_model_id: string
  enabled?: boolean
}

export interface AccessKey {
  id: string
  key_prefix: string
  name: string
  enabled: boolean
  /**
   * 归属用户（多用户改造）。空串 = **无归属**。
   *
   * 无归属的 key 会被迁移退役（store.retireOrphanKeys）、鉴权直接 401，
   * 界面上必须显式标注「无归属（不可用）」—— 否则运维会以为它还能用。
   */
  user_id: string
  username?: string
  quota_tokens: number
  used_tokens: number
  rpm_limit: number // 每分钟请求数上限，0 = 不限
  tpm_limit: number // 每分钟 token 上限，0 = 不限
  /**
   * key 级模型白名单（P1）。
   *
   * **空数组 = 不限制**（不是「一个都不允许」）。与用户所属组的白名单求交，
   * 且 key 级只能更紧 —— 所以这个值非空时，用户实际可用的模型是
   * 「组白名单 ∩ 这个列表」。
   */
  allowed_models: string[]
  /**
   * 有效期截止（毫秒时间戳，P2）。0 = 永不过期。
   *
   * 过期后该 key 的请求直接 403 `key_expired`（不是 401）—— key 本身有效，
   * 客户端看到 401 的第一反应是「重新配一把」，那是误导。
   */
  expires_at: number
  /**
   * 来源 IP 白名单（P2）：逗号分隔的 CIDR 或单 IP。空串 = 不限制。
   *
   * 只按**直连对端地址**判定，不读 `X-Forwarded-For`（那个头由客户端填写，
   * 采信它等于白名单可伪造）。网关前面有反向代理时，判定的是代理的地址。
   */
  allowed_ips: string
  /**
   * key 级分组覆盖（P2）。空串 = 沿用归属用户所属的分组。
   * 只有管理员能设置 —— 否则用户可以指向更宽松的组来绕过自己组的限制。
   */
  group_id: string
  created_at: number
}

// ---- 多用户与会话（对应 internal/admin/user_handler.go、user_admin_handler.go）----

/** GET /admin/api/session 的响应：登录是否启用。免鉴权。 */
export interface SessionStatus {
  enabled: boolean
  /** 未启用时提示运维配置的环境变量名。 */
  env: string
  min_len: number
}

export type UserRole = 'admin' | 'user'
export type UserStatus = 'active' | 'disabled'

/** POST /admin/api/login 与 POST /admin/api/bootstrap 的响应（同形）。 */
export interface LoginResult {
  token: string
  expires_at: number
  username: string
  role: UserRole
  /**
   * 保留字段但统一认证后恒为 false：首次登录由 /admin/api/bootstrap 引导，
   * 不再存在「登录成功但还得去别处设密码」的中间态。
 */
  must_set_password: boolean
}

/**
 * GET /admin/api/bootstrap 的响应。
 *
 * needs_setup 为真表示系统里有一个「已建出但还没设密码」的管理员，
 * 登录页据此渲染「首次设置密码」表单。
 */
export interface BootstrapStatus {
  needs_setup: boolean
  /** 待初始化的账号名，供表单标题显示。needs_setup 为 false 时为空。 */
  username?: string
  session_enabled: boolean
}

/** GET /admin/api/me 的响应。 */
export interface Me {
  username: string
  display_name: string
  role: UserRole
  status: UserStatus
  quota_tokens: number
  used_tokens: number
  must_set_password: boolean
  is_admin: boolean
  session_enabled: boolean
}

/** GET /admin/api/users 的响应项。 */
export interface User {
  id: string
  username: string
  display_name: string
  role: UserRole
  status: UserStatus
  /**
   * 所属分组（P1）。空串 = 未分组 = **不受模型白名单限制**。
   *
   * 与「分到一个没配白名单的组」效果相同 —— 两者都是「不限制」。
   * 界面上要讲清这点，否则会有人以为「没分组」是一种限制。
   */
  group_id: string
  quota_tokens: number
  used_tokens: number
  auth_version: number
  remark?: string
  has_password: boolean
  key_count: number
  created_at: number
  last_login_at: number
  is_self: boolean
}

// ---- 分组与模型白名单（P1，对应 internal/admin/group_handler.go）----

export interface Group {
  id: string
  name: string
  description: string
  /**
   * 该组的公开模型名白名单。
   *
   * **空数组 = 该组不限制可见范围**（不是「什么都看不到」）。
   * 新建的组默认就是空的 —— 界面上必须提示这一点，否则会出现
   * 「建了组、忘了配模型、以为收紧了权限，实际放得更开」。
   */
  models: string[]
  member_count: number
  created_at: number
}

export interface AuditEntry {
  id: number
  ts: number
  actor: string
  remote: string
  method: string
  path: string
  status: number
  fields: string
}

export interface KeyCreateResponse extends AccessKey {
  plaintext_key: string // 仅创建时返回一次
}

export interface Stats {
  total_requests: number
  total_tokens: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number // 缓存读取命中的输入 token 累计
  cache_hit_rate: number // 0~1，= cached_tokens / input_tokens；无输入样本时为 0
  error_count: number
  avg_tokens_per_sec: number
  avg_ttfb_ms: number
  // 费用（元）：每条用量落库**当时**按模型单价算好并固化的金额求和
  // （usage_records.cost_total）。改价只影响此后的记录，不回溯历史。
  cost: number
  // lifetime 为**终身累计**（表 B usage_totals）。仅管理员、且仅当请求带了
  // include_lifetime=true 时出现 —— 缺席 = 这次没要终身数据。
  // 普通用户拿不到：表 B 是不分用户的全局单行，给了就是别人的数字。
  lifetime?: {
    request_count: number
    success_count: number
    error_count: number
    total_tokens: number
    cost_total: number
    // 全局最早一条用量的毫秒时间戳；0 = 从未有过记录
    first_record_at: number
    // 明细已被剪到的水位日（YYYY-MM-DD）；空 = 未剪过
    pruned_through_day: string
  }
}

// usage 分组端点（by-day / by-model / by-key / by-provider）的统一返回行
export interface UsageGroupEntry {
  key: string // by-day 时为 YYYY-MM-DD；by-model 时为底层上游模型名；其余为分组维度值
  count: number // 请求次数
  tokens: number // total_tokens 之和
  name?: string // by-key：密钥名称（缺失时前端回退显示 key）
}

// 调用历史（/usage/history）的一行：一次真实请求
export interface UsageHistoryEntry {
  ts: number // 毫秒时间戳
  public_model: string // 调用模型（对外名）
  upstream_model: string // 实际模型（底层上游名）
  key_name: string // 调用密钥名称（密钥删除后为空）
  key_id: string // 调用密钥 id（回退显示用）
  total_tokens: number
  ttfb_ms: number // 首字节时间（毫秒）
  latency_ms: number // 总耗时（毫秒）
  status: string // 'ok' 或错误码
}

// 调用历史的分页结果：当页明细 + 过滤后的总条数（用于计算总页数）
export interface UsageHistoryPage {
  records: UsageHistoryEntry[]
  total: number
}

// 通用错误响应：{"error":{"message":"...","type":"invalid_request_error"}}
export interface ApiError {
  message: string
  type: string
}

// ---------- 供应商 / 模型的导入导出 ----------

/**
 * 导出文件的结构。明文模式下三个数组有值；加密模式下只有 body/salt 有值，
 * 其余为空 —— 真正的内容在 body 里，用口令解开后是同一种结构。
 *
 * 前端要读它只是为了「告诉用户这份文件里有什么」以及判断要不要口令，
 * 不参与解析密文（那是后端的事，前端不该持有解密逻辑）。
 */
export interface ConfigExportFile {
  version: number
  exported_at: number
  encrypted?: boolean
  body?: string
  kdf?: string
  cipher?: string
  salt?: string
  iterations?: number
  providers?: ConfigExportProvider[]
  credentials?: ConfigExportCredential[]
  models?: ConfigExportModel[]
}

export interface ConfigExportProvider {
  slug: string
  name: string
  protocol: string
  endpoint: string
  enabled: boolean
  timeout_ms: number
  max_retries: number
  quirks_json?: string
}

export interface ConfigExportCredential {
  provider_slug: string
  label: string
  api_key: string
  enabled: boolean
  weight: number
}

export interface ConfigExportModel {
  provider_slug: string
  model_id: string
  display_name?: string
  enabled: boolean
  context_window?: number
  max_output_tokens?: number
  supports_thinking?: boolean
  price_input?: number
  price_cache_hit?: number
  price_output?: number
}

/**
 * 导入结果。**逐项**而不是只给总数：导入是「改一个可能正在服务线上」的库，
 * 用户需要知道哪几条进去了、哪几条被改名、哪几条被跳过。
 */
export interface ConfigImportResult {
  dry_run: boolean
  providers_created: number
  /** 原 slug → 改名后的 slug。只有发生冲突的才有条目。 */
  providers_renamed?: Record<string, string>
  models_created: number
  models_skipped: number
  credentials_added: number
  credentials_skipped: number
  warnings?: string[]
  notes?: string[]
}
