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

export interface Credential {
  id: string
  provider_id: string
  label: string
  api_key_mask: string // 恒为 "encrypted"，明文永不下发
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
  expires_at: number // unix 秒，0 = 永不过期
  quota_tokens: number
  used_tokens: number
  rpm_limit: number
  tpm_limit: number
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
  error_count: number
}

// usage 分组端点（by-day / by-model / by-key / by-provider）的统一返回行
export interface UsageGroupEntry {
  key: string // by-day 时为 YYYY-MM-DD；其余为分组维度值
  count: number // 请求次数
  tokens: number // total_tokens 之和
}

// 通用错误响应：{"error":{"message":"...","type":"invalid_request_error"}}
export interface ApiError {
  message: string
  type: string
}
