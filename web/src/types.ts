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

// 全局设置（与 internal/admin/settings_handler.go 对应）
export interface Settings {
  default_context_window: number
  default_max_output_tokens: number
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
  default_extra_json: string
  tokens_per_sec?: number // 近 5 次真实调用的平均输出速度；未调用过则不下发
  ttfb_ms?: number // 近 5 次流式调用的平均首字延迟（毫秒）；非流式/未测得则不下发
  success_rate?: number // 近 100 次调用成功率（0~1）
  call_count?: number // 成功率样本量（≤100）；缺失/0 表示未调用过
  created_at: number
}

export interface Route {
  id: string
  public_name: string
  provider_id: string
  upstream_model_id: string // 外键 → upstream_models.id（短哈希）
  enabled: boolean
  priority: number
  fallback_route_id: string
  extra_json: string
  created_at: number
}

export interface AccessKey {
  id: string
  key_prefix: string
  name: string
  enabled: boolean
  quota_tokens: number
  used_tokens: number
  created_at: number
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

// 通用错误响应：{"error":{"message":"...","type":"invalid_request_error"}}
export interface ApiError {
  message: string
  type: string
}
