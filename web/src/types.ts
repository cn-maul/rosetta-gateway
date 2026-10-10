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
  /**
   * 该模型**真实支持**的思考挡位（强度升序）。
   *
   * **空数组 = 未配置**，不是「不支持思考」—— 未配置时数据面不干预思考
   * 强度，客户端发什么就按 rosetta 的三档归一处理。界面必须据此区分
   * 「没填」与「填了空」。
   */
  effort_levels: string[]
  /**
   * 配置里出现但网关不认识的原样值（厂商私有写法）。
   * 必须显示出来：丢掉它就等于让「配了但不生效」变成无从排查的静默失败。
   */
  unknown_effort_levels?: string[]
  /**
   * 该模型能否思考，**三态**：
   *
   * - `undefined`（字段缺席）= 未配置，网关不干预，客户端维持既有行为；
   * - `true` = 支持；
   * - `false` = 确定不支持，数据面会剥掉请求上的思考配置。
   *
   * 界面必须能表达第三种状态（清除）。把「不知道」显示成「不支持」会让
   * 所有未配置的模型凭空失去思考能力 —— 那是一次静默的能力回退。
   */
  supports_thinking?: boolean
  tokens_per_sec?: number // 近 5 次真实调用的平均输出速度；未调用过则不下发
  ttfb_ms?: number // 近 5 次流式调用的平均首字延迟（毫秒）；非流式/未测得则不下发
  success_rate?: number // 近 100 次调用成功率（0~1）
  call_count?: number // 成功率样本量（≤100）；缺失/0 表示未调用过
  // 没有 created_at：upstream_models 表压根没有这一列，接口也不下发。
  // 曾在这里声明过一次，导致列表里永远渲染出一个「—」日期列。
  // 也没有 default_extra_json：存了但没有任何地方拿它构造请求，已随列一并摘除。
}

/**
 * 单模型可用性探测的结果。
 *
 * 注意 `status` 与 HTTP 状态码是两件事：探测失败（凭据错、模型下架、无权限、
 * 上游 5xx）时 HTTP 仍是 200 —— 那是**探测的结论**，不是管理接口调用失败。
 * 只有「id 不存在」才是 404。前端因此必须看 status 而不是 res.ok。
 */
export interface ModelTestResult {
  status: 'ok' | 'error'
  /** 被探测的上游模型名，原样回显（用于把结果与行对上）。 */
  model_id: string
  provider_id: string
  message: string
  /** 端到端往返耗时（毫秒），含上游生成 1 个 token 的时间。 */
  latency_ms: number
  /** 上游回报的真实用量。探测会真实计费，带出来让成本可见。 */
  input_tokens?: number
  output_tokens?: number
  /**
   * 上游用 HTTP 200 + 错误体回绝（部分中转网关如此）。
   * 为真时需向用户说明「HTTP 成功却报错」，否则看起来像网关自己坏了。
   */
  in_band?: boolean
}

/** 单条凭据在上游侧的余额查询结果（对应 internal/upstream.CredentialBalance）。 */
export interface CredentialBalance {
  credential_id: string
  label: string
  /** ok=查到；error=查了但失败（凭据/网络）；unsupported=这个上游没有余额查询方式。 */
  status: 'ok' | 'error' | 'unsupported'
  message?: string
  /**
   * 上游报告的可用额度，**未换算成元**（上游可能报 USD，汇率随时在动，
   * 网关编一个换算值等于撒谎）。单位见 currency。
   */
  amount?: number
  currency?: string
  /** 上游附带的补充说明（总额度/已用/额度不足…），原样显示。 */
  detail?: string
  /** 取数时刻（毫秒时间戳）。余额是点按查询、不是后台轮询，界面要能说清它有多旧。 */
  fetched_at?: number
}

/** GET /admin/api/providers/{id}/balance 的响应。 */
export interface ProviderBalance {
  provider_id: string
  provider: string
  /** ok=全部查到；partial=部分失败；error=全部失败；unsupported=该上游不支持。 */
  status: 'ok' | 'partial' | 'error' | 'unsupported'
  message?: string
  results: CredentialBalance[]
}

/**
 * PATCH /models/{id} 的载荷。
 *
 * 刻意**不是** Partial<UpstreamModel>：响应里的 effort_levels 是数组（读回来
 * 方便渲染），而写入用的是逗号分隔字符串（与库里那一列、导出文件同形）。
 * 两者共用一个类型会逼得调用方在两种形状间来回转换 —— 而转换点一旦分散到
 * 各调用方，就一定有人会传错的那一种，且编译器不再拦。
 *
 * 其余字段是 Partial：未出现的字段保持原值，空串 / 0 是合法的显式值。
 */
export interface UpstreamModelUpdate {
  model_id?: string
  display_name?: string
  enabled?: boolean
  context_window?: number
  max_output_tokens?: number
  /** 逗号分隔的挡位原文；空串 = 清除（回到「未配置」）。 */
  effort_levels?: string
  /**
   * 「能否思考」开关的三态：字段缺席 = 不改；true/false = 显式声明；
   * **null = 清除**（回到「未配置」）。
   *
   * 清除必须能表达 —— 界面上「把勾去掉并保存」就是一个明确动作，而把它
   * 映射成 false 在语义上完全相反。
   */
  supports_thinking?: boolean | null
  // 单价（元 / 百万 tokens）也走这一个 PATCH：Settings 页「模型价格」正是
  // 按 provider_id + model_id 直接改这三列，与模型编辑共用同一个端点。
  price_input?: number
  price_cache_hit?: number
  price_output?: number
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
  role: UserRole
  status: UserStatus
  quota_tokens: number
  used_tokens: number
  /**
   * 我的余额（分）与「不限额」标志。
   *
   * 语义与 User.balance_cents 逐字相同：`balance_unlimited = true` 才是不限额，
   * `0 分`是「真没钱」（会被 402 拒绝）。显示层必须区分这两者。
   */
  balance_cents: number
  balance_unlimited: boolean
  /**
   * 不足一分的待结算余数（微元，1e-6 元）。
   *
   * 余额按**分**扣减，而单价可能远低于一分（实测 3 元/百万 token 时，
   * 一次一万 token 的调用只有 5 厘），于是余额会长时间不动。
   * 界面上必须把它显示出来，否则「余额没变」会被读成「没扣钱」——
   * 实际上钱已消费，只是还没攒够一分。
   */
  balance_remainder: number
  must_set_password: boolean
  is_admin: boolean
  session_enabled: boolean
}

/** GET /admin/api/users 的响应项。 */
export interface User {
  id: string
  username: string
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
  /**
   * 账户余额，单位**分**（人民币）。
   *
   * 与 quota_tokens 的「0 = 不限」刻意相反：`balance_unlimited = true`
   * 才是不限额；`balance_cents = 0` 是「账户里确实一分钱都没有」，
   * 它会触发 402。两种状态一个放行一个拒绝，绝不能塌成同一个值。
   *
   * 界面必须据此显示「不限」而不是「0.00 元」—— 后者会被读成「没钱了」，
   * 而实际含义是「不受余额限制」。判定一律看 balance_unlimited。
   */
  balance_cents: number
  balance_unlimited: boolean
  /** 不足一分的待结算余数（微元）。语义见 Me.balance_remainder。 */
  balance_remainder: number
  auth_version: number
  remark?: string
  has_password: boolean
  key_count: number
  created_at: number
  last_login_at: number
  is_self: boolean
}

// ---- 充值流水（balance_topups）----
//
// 对应 GET /admin/api/topups。字段名与 internal/store/topup_dao.go 里
// store.Topup 的 json tag **逐字对齐**（2026-10-11 已核对实际实现），
// 外加端点补的 username（被充值者的用户名）。
//
// ⚠️ **作用域口径待 Lead 决策**（详见 AUDIT/task-admin-wallet.md）：
// 服务端 ListTopups 当前对**管理员也**按自己的 id 收窄 —— 而管理员不能被
// 充值（AdjustBalance 已拦下），所以管理员调用它恒返回空列表。
// 用户需求是「管理员看到所有用户的充值流水」，两者相反。
// 扩作用域是服务端改动，本任务不自行修改；接口**形状**已按实际实现锁定，
// 因此联调没有阻塞，只有「管理员看得到几张行」这一个待决问题。
export interface TopupRecord {
  id: string
  /** 被充值的用户（不是操作者）。 */
  user_id: string
  /** 被充值用户的用户名，由端点补上。 */
  username: string
  /**
   * 本次增减额（分）。正数 = 充值，负数 = 扣减。
   *
   * 刻意存相对量而不是「调整前/后两个绝对值」：余额是相对变动，流水要回答
   * 的是「发生了什么变化」。逐笔核对应以 delta_cents 求和，而不是逐行比
   * balance_after —— 并发充值下后者不保证首尾相接（见 store.Topup 的注释）。
   */
  delta_cents: number
  /** 本次调整**之后**的余额（分），由 UPDATE ... RETURNING 回读的权威值。 */
  balance_after: number
  /**
   * 本次是否把一个**不限额**用户切成了有限额。
   *
   * 不显示它的话，「+100 元」会被读成普通的加钱，而它实际可能是一次
   * 额度降级（不限 → 100 元），那对用户是实打实的限制收紧。
   */
  was_unlimited: boolean
  /** 操作者（管理员）的 id 与用户名。 */
  operator_id: string
  operator_username: string
  /** 毫秒时间戳。 */
  ts: number
  /** 管理员可选备注（充值原因、工单号…）。可空。 */
  remark?: string
}

/** GET /admin/api/topups 的响应（分页）。 */
export interface TopupPage {
  records: TopupRecord[]
  /** 过滤后的总条数（不受 limit 影响），供「共 N 条」显示。 */
  total: number
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
  key_count: number
  created_at: number
}

/**
 * 一个公开模型名的三项单价（元 / 百万 tokens）。
 *
 * **0 = 未配置，不是「免费」**：界面必须显示「未配置」而不是 0.00，
 * 否则会被读成「确认这一档不要钱」——而未配置时实际计费按 0（见
 * store.priceUsage），两者恰好重合但**意图完全不同**：
 * 一个是「算过了，就是免费」，一个是「还没人填」。与 Settings.vue 的
 * priceCell 同口径。
 */
export interface ModelPrice {
  name: string
  price_input: number
  price_output: number
  price_cache_hit: number
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
  //
  // 普通用户拿到的是**自己**的合计（2026-10-10 起）—— 此前服务端对普通用户
  // 强制归零，依据是「网关不做计费结算」，但余额计费落地后该前提已不成立。
  cost: number
  /**
   * 当前登录者的账户余额（分）与待结算余数（微元），随 stats 一并下发。
   *
   * 放在 stats 而不是让前端再调 /me：总览本来就在拉 stats，省一次请求；
   * 且余额与费用是同一件事的两面，放一张卡片里才读得通。
   *
   * 语义与 Me 的同名字段逐字相同：`balance_unlimited = true` 才是不限额，
   // `0 分`是「真没钱」。
   */
  balance_cents: number
  balance_remainder: number
  balance_unlimited: boolean
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
  /**
   * 固化费用合计（元），SUM(cost_total)。
   *
   * 2026-10-10 新增，四个维度都有（后端 groupBy 是共用实现）。
   * 钱包页的「消耗记录」按天列总额就靠它；此前 by-day 只给 count/tokens，
   * 前端拿不到钱，只能显示 token 而显示不出「这一天花了多少」。
   *
   * 口径与 Stats.cost 逐字一致：都读落库当时固化的金额，改价不回溯历史。
   * 所以「按天求和 == 区间合计」在构造上成立，不靠两边对齐。
   *
   * 未配价模型的记录 cost 恒 0 —— 与「未配置 ≠ 免费」是同一件事，
   * 显示层要靠别的信息（调用历史里逐条的费用）解释，不能在这里猜。
   */
  cost: number
}

/**
 * GET /admin/api/usage/by-user 的一行：**一个用户**的消费汇总。
 *
 * 存在的理由：钱包页要「消费汇总一表一行一个用户」。现有的 by-* 端点
 * （by-key / by-model / by-provider / by-day）都没有用户这一维 ——
 * 它们的分组键分别是密钥、模型、供应商、日期。
 *
 * 与 UsageGroupEntry 是两个类型而不是给那个加可选字段：那一行的 name 是
 * 「密钥名」，这一行的 user_name 是「用户名」，且这一行必须带 cost。
 * 合并成一个类型的话，by-key 端点就会下发一个恒为 0 的 cost ——
 * 一个字段在某个端点上恒为 0，读代码的人无法分辨「没有这个字段」与
 * 「有但还没算完」。
 */
export interface UsageByUserEntry {
  /**
   * usage_records.user_id —— **冗余固化**的归属（多用户改造 P0）。
   *
   * 空串 = **无归属**：迁移前的无主 key 产生的历史用量。它不是一个具体
   * 用户，界面上必须显示成「无归属」而不是留白。
   */
  key: string
  /**
   * 用户名（JOIN users 得到）。**空串有两种含义，靠 key 区分**：
   *   - key 非空而 user_name 空 → 该用户**已被删除**（用量是历史事实，行仍在）；
   *   - key 本身为空            → 无归属用量。
   * 第一种情况前端必须回退显示 key —— 留白会被读成「无归属」，而
   * 「用户已删除」与「无归属」是两件不同的事。
   */
  user_name: string
  /**
   * 用户角色（admin / user）；用户已删除时为空。
   *
   * 管理员**不该**出现在这张表里（后端已拦下管理员调用模型），但升级前
   * 可能有他的历史用量。那一行是真实发生过的消费，不能静默过滤掉
   * （过滤会让本表合计小于全站合计 = 对不上账），所以照实显示并**加标记**。
   */
  role: string
  /** 请求次数。归档行的 request_count 已聚合，故是 SUM 而非行数。 */
  count: number
  /** total_tokens 之和。 */
  tokens: number
  /** 该用户固化的消费合计（**元**）。这是这张表的主角。 */
  cost: number
}

/**
 * GET /admin/api/usage/by-user 的响应：当页行 + **有消费的用户总数**。
 *
 * 为什么这一个 by-* 端点返回对象而它的三个兄弟返回裸数组：那些由总览页调用、
 * 固定 limit=10 画「Top 10」条形图，截断是**既定语义**；而这张表是按金额
 * 读账的，一旦用户数超过 limit，被截断的表与完整的表在界面上完全同形 ——
 * 把「前 N 名」读成「全部用户」会直接得出错误结论。所以必须能把
 * 「显示前 N / 共 M」说出来。与 CSV 导出的 X-Export-Truncated 同一个原则：
 * **截断必须可见**。
 */
export interface UsageByUserPage {
  records: UsageByUserEntry[]
  /** 有消费的用户总数（去重），**不受 limit 影响**。 */
  total: number
}

// 调用历史（/usage/history）的一行：一次真实请求
export interface UsageHistoryEntry {
  ts: number // 毫秒时间戳
  public_model: string // 调用模型（对外名）
  upstream_model: string // 实际模型（底层上游名）
  key_name: string // 调用密钥名称（密钥删除后为空）
  key_id: string // 调用密钥 id（回退显示用）
  total_tokens: number
  /**
   * 本次**生成**出来的 token 数（后端 usage_records.output_tokens）。
   *
   * 与 total_tokens 的区别是本列的关键：total_tokens = 输入 + 输出，而输入
   * 是请求侧送进去的、不是模型吐出来的。用总量算速度会把速度虚高好几倍
   * （1 万输入 / 2 百输出时，用总量算出来是真实生成速度的 50 倍）。
   */
  output_tokens: number
  /**
   * 本次请求的平均输出速度（token/s），**由后端算好下发**。
   *
   * 为什么不让前端拿 output_tokens / (latency_ms/1000) 现算：公式必须与
   * 总览页「平均速度」逐字一致（后端 store.GetRecentThroughput 的
   * `SUM(output_tokens) * 1000.0 / NULLIF(SUM(latency_ms), 0)`），
   * 前端再写一遍就等于同一公式存在两个实现，改一处忘一处时两个页面会
   * 静默分叉 —— 而对不上时没人知道该信哪个。除零边界同理，只留在后端一处。
   *
   * **0 = 没有可算的样本**（output_tokens=0 的上游未报 usage / 非流式短请求，
   * 或 latency_ms=0 的错误请求），不是「速度为零」。所以渲染时必须显示 '—'
   * 而不是 '0.0' —— 后者会被读成「这次生成极慢」。
   */
  tps: number
  ttfb_ms: number // 首字节时间（毫秒）
  latency_ms: number // 总耗时（毫秒）
  status: string // 'ok' 或错误码
  /**
   * 是否为流式调用。
   *
   * 必须标注的原因：流式与非流式的 ttfb 含义完全不同 —— 非流式的首字时间
   * 大致等于总耗时（响应一次性回来），流式才是「多久出第一个字」。混在一列
   * 不标注，运维无法判断「首字 15 秒」是模型慢还是压根没用流式。
   */
  stream: boolean
  /**
   * 该条用量的固化费用（元），落库当时按当时单价算好并固化。
   *
   * 与账单、报表同源；改价不回溯历史。单次可能远低于一分（单价低时），
   * 显示出来才能解释「报表在涨、余额按分扣减却不动」。
   */
  cost: number
}

// 调用历史的分页结果：当页明细 + 过滤后的总条数（用于计算总页数）
export interface UsageHistoryPage {
  records: UsageHistoryEntry[]
  total: number
}

/**
 * 调用历史列表 / CSV 导出共用的排障过滤器（后端 usageHistoryFilters）。
 * 三个维度都是**精确匹配**；undefined / 空串 = 不过滤。
 * keyId 是密钥 id（不是前缀），界面上用密钥下拉把名字翻成 id。
 */
export interface HistoryFilters {
  status?: string
  model?: string
  keyId?: string
}

/**
 * POST /usage/prune 的响应：一次归档剪枝的影响面（internal/store.PruneResult）。
 * skipped=true 不是错误 —— 水位已到位 / 参数不合理都会走这条，原因在 reason。
 */
export interface PruneResult {
  rollup_rows: number // 写进按天累计表的聚合分组数
  deleted_rows: number // 从明细表删除的行数
  cutoff_day: string // 保留窗口第一天（本地 YYYY-MM-DD）
  pruned_through_day: string // 归档水位：已剪到含哪一天
  skipped: boolean
  reason?: string
}

/** POST /keys/{id}/recompute-usage 的响应：重算后的已用量。 */
export interface RecomputeUsageResult {
  status: string
  used_tokens: number
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
  /** 逗号分隔的挡位原文（与库里那列同形）；空 = 未配置。 */
  effort_levels?: string
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
