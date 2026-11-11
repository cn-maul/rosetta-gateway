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
  /**
   * 该上游选择的出站网络（networks.id）。空串 = 直连。
   *
   * 代理地址是多个上游共用的一份配置，所以放在 networks 表里而不是每个
   * 上游各存一份 URL —— 改一次要改 N 处，且手写 URL 极易打错。
   */
  network_id: string
  /**
   * 所选网络实际指向的代理地址（**只读派生值**）。
   *
   * 前端只往回写 network_id；这个字段只用来在界面上显示「这条网络
   * 到底指向哪个地址」，不必再发一次请求去查网络清单。
   * 直连时为空串。
   */
  proxy_url: string
  /**
   * 余额查询的显式配置（JSON 串），空串 = 不配（走内置自动判定）。
   *
   * 内置判定只认 OpenAI 官方与 DeepSeek；接中转站、自建网关或
   * Moonshot / OpenRouter 这类自带余额端点的上游时，在这里填 path 与
   * field。只在编辑上游时的高级选项里出现 —— 新建时留空即可。
   */
  balance_spec_json: string
  created_at: number
  updated_at: number
  // ready 是**运行时**状态（后端快照），不是库里的配置字段：
  // 库只记录配了什么，快照才反映「上游池实际建出了什么」。
  // ready=false 表示一条可用凭据都没有 —— 请求打过去必然失败，
  // 而在改造前这个状态只躺在日志里，界面上看不出来。
  // 只在 enabled 时有意义：已停用的 provider 本来就不参与池构建。
  ready: boolean
  // ready_reason 面向运维的一句话原因（凭据解密失败 / 读不到凭据清单 / 无凭据）。
  ready_reason?: string
  /**
   * model_count 是**已导入本库清单**的模型条数，不是这个上游「理论上支持
   * 多少个」。后者要去问上游 /models 才知道，那是一次带凭据的网络请求，
   * 不该挂在列表上（十几个上游就是十几次，且大多数上游压根不支持）。
   *
   * cred_count 同理：已配置的凭据条数。
   *
   * 两者由后端在列表查询里用相关子查询一次算出，不额外往返。
   */
  model_count?: number
  cred_count?: number
  /**
   * latency_ms / latency_p95_ms / latency_samples 是**探针测得的**网关到该
   * 上游的往返耗时（窗口 = probes.window_size × interval_sec，与健康判定
   * 同一口径；单位毫秒）。
   *
   * latency_samples = 0 表示窗口内没有成功的探测记录（从未探测、探测全
   * 失败、或探针功能关闭）—— 界面显示「—」而不是「0 ms」，否则「一条都
   * 没测到」会被读成「快到测不出来」。失败样本不计入平均：连不上的那条
   * latency 是超时等待时间，混进去会让故障上游看起来很健康。
   */
  latency_ms?: number
  latency_p95_ms?: number
  latency_samples?: number
}

// 新增上游只需这三项 + 可选 api_key；slug、启用、超时、重试由后端决定。
export interface ProviderCreatePayload {
  name: string
  protocol: string
  endpoint: string
  quirks_json?: string
  /** 出站网络（networks.id）；留空或 "direct" = 直连（新建的默认值）。 */
  network_id?: string
  /**
   * 余额查询的显式配置（JSON 字符串，不是对象 —— 后端整串存、整串校验）。
   * 留空 = 不配，回落到内置的自动判定（OpenAI 官方 / DeepSeek）。
   *
   * 只有接中转站、自建网关、或 Moonshot/OpenRouter 这类自带余额端点的
   * 上游时才需要填。形如：
   * `{"path":"/credits","field":"data.remaining","currency":"USD"}`
   */
  balance_spec_json?: string
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
  /**
   * 出站网络。传空串或 "direct" 是**换回直连**，不是「不改」——
   * 把上游从代理切回直连必须能表达。
   */
  network_id?: string
  /** 余额查询配置。传空串是**清空**（回到自动判定），与 proxy_url 同语义。 */
  balance_spec_json?: string
}

// discover 端点返回的上游模型条目；后端已把探测不到的容量替换为设置里的默认值
export interface DiscoveredModel {
  model_id: string
  display_name?: string
  context_window?: number
  max_output_tokens?: number
  /**
   * 上游 /models 自报的思考挡位，逗号分隔；**空 = 上游没报**
   * （不是「不支持思考」，两者必须分开处理）。
   *
   * 上游报了就随导入一起落库，省掉每次手工勾；没报则留空，
   * 由管理员按模型文档自行配置。
   */
  effort_levels?: string
  /** 三态：undefined = 上游没报（网关不干预）；true/false = 上游明确表态。 */
  supports_thinking?: boolean
  /** 按模型名推断的思考方言，供添加时预填。 */
  thinking_dialect?: string
  /** 上面那个值是「推断的（detected）」还是「兜底的（fallback）」。 */
  thinking_dialect_source?: string
}

// 批量导入的单条模型
export interface ModelImportItem {
  model_id: string
  display_name?: string
  context_window?: number
  max_output_tokens?: number
  /** 见 DiscoveredModel.effort_levels。 */
  effort_levels?: string
  /** 见 DiscoveredModel.supports_thinking。 */
  supports_thinking?: boolean
  /**
   * 思考方言：deepseek / glm / qwen / minimax / kimi / openai / anthropic /
   * passthrough。空串 = 自动（后端按 model_id 推断）。
   */
  thinking_dialect?: string
  /** 能否关闭思考的实测纠正；缺席 = 按厂商文档表。 */
  can_disable_thinking?: boolean
  /**
   * 单价三档（元 / 1M tokens）。只有「手动添加模型」这一档会填：
   * 探测导入时上游 /models 从不报价格。留空或填 0 = 不配价（库里落 NULL，
   * 与「确认免费」的 0 元不是一回事）。
   *
   * 缓存写入价刻意不在导入接口里 —— 界面上已经不再暴露它，未配置时计费
   * 侧按输入价算（internal/store/pricing.go 的 EffectivePrices）。
   */
  price_input?: number
  price_cache_hit?: number
  price_output?: number
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
  context_window: number
  max_output_tokens: number
  // 单价（元 / 百万 tokens），0 = 未配置（费用按 0 计）。
  // price_input 是缓存未命中的输入价，price_cache_hit 是缓存命中的输入价，
  // price_cache_creation 是缓存写入的输入价，price_output 是输出价。
  price_input: number
  price_cache_hit: number
  price_cache_creation: number
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
  /**
   * 思考方言（见 ModelImportItem.thinking_dialect）。
   *
   * 空串 = 自动推断；后端算好的生效值在 effective_thinking_dialect，
   * 来源在 thinking_dialect_source —— 前端自己不重复推断，避免两套口径。
   */
  thinking_dialect: string
  effective_thinking_dialect: string
  thinking_dialect_source: string
  /**
   * 能否关闭思考的**实测纠正**。
   *
   * 缺席 = 按厂商文档表（GLM-5.3 / Kimi K3 / MiniMax-M3.1-Flash 不能关）；
   * true/false = 以实测为准。中转站会归一化各家写法，文档表在那条链路上
   * 未必成立 —— 生效值见 effective_can_disable_thinking。
   */
  can_disable_thinking?: boolean
  /** 后端按「实测纠正优先，否则文档表」算好的最终结果。 */
  effective_can_disable_thinking: boolean
  /** effective 的来源：manual（实测纠正）/ detected（厂商文档表）。 */
  can_disable_thinking_source: string
  tokens_per_sec?: number // 近 5 次真实调用的平均输出速度；未调用过则不下发
  ttfb_ms?: number // 近 5 次流式调用的平均首字延迟（毫秒）；非流式/未测得则不下发
  success_rate?: number // 近 100 次调用成功率（0~1）
  call_count?: number // 成功率样本量（≤100）；缺失/0 表示未调用过
  /**
   * 模型归组 id（跨上游的同一个模型合并统计）。空/缺席 = 未归组。
   * 组名由前端用 modelGroups() 全量列表本地解析（组是小表）。
   */
  group_id?: string
  /** 引用该模型的路由链数量；0 = 没被任何链引用，可以自由删除。 */
  route_count?: number
  /**
   * 是否是某条链的**链首**（首选目标）。
   *
   * true 时删除按钮置灰：链的顺序表达「先试谁」的显式配置，删掉首选等于
   * 悄悄改了这条路由的流量分配，所以后端回 409 要求先去「路由」页改。
   * 非链首的成员可以直接删，删除会自动把它从所有链里摘掉。
   */
  is_route_head?: boolean
  // 没有 created_at：upstream_models 表压根没有这一列，接口也不下发。
  // 曾在这里声明过一次，导致列表里永远渲染出一个「—」日期列。
  // 也没有 default_extra_json：存了但没有任何地方拿它构造请求，已随列一并摘除。
}

/**
 * 模型归组：把不同上游的「同一个模型」（叫法往往不同）标成一组，
 * 用量统计按组名合并显示。
 *
 * 与 `Group`（用户分组）是**完全不同的概念**，别混用：那个管的是
 * 模型白名单与配额归属，这个纯粹是统计展示层的别名合并。
 */
export interface ModelGroup {
  id: string
  display_name: string
  member_count: number
  created_at?: number
}

/**
 * 组内「当前模型行已不存在、但历史用量仍在」的成员。
 *
 * 删掉上游后 upstream_models 行会被级联删除，而归组归属按
 * (provider_id, model_id) 留在独立表里，统计才能继续按组合并。
 * provider_name 优先取当前上游名，上游已删时回退到用量里固化的旧名字。
 */
export interface ModelGroupHistoryMember {
  provider_id: string
  model_id: string
  provider_name: string
  count: number
  tokens: number
}

/**
 * 一条出站网络（可复用的代理配置）。
 *
 * 代理地址天然是多个上游共用的，所以它是一份**清单**而不是每个上游各填
 * 一次 URL：改一次要改 N 处，且手写 URL 极易打错（少个斜杠、端口写错都
 * 不报错，只是连不上）。
 *
 * 直连不是一条 Network —— 它是保留 id 'direct'，由前端作为下拉第一项
 * 硬编码（后端 ListNetworks 不返回它，因为它没有可配置的内容）。
 */
export interface Network {
  id: string
  name: string
  proxy_url: string
  /** 引用该网络的上游数（派生值）。删网络前用它提示影响面。 */
  provider_count: number
  created_at?: number

  /**
   * 以下是**探针测得**的连通性摘要（2026-11-01 起代理由后台定时测速，
   * 不再有「测试」按钮）。窗口与供应商延迟同一口径：
   * probes.window_size × interval_sec。
   *
   * latency_samples = 0 表示窗口内没有成功样本 → 界面显示「—」，不能
   * 显示 0ms（「一次都没通」与「快到 0」是两件事）。此时
   * last_probe_ok / last_probe_error 仍然有用：它们回答「没测过」还是
   * 「一直在失败」。
   */
  latency_ms?: number
  latency_p95_ms?: number
  latency_samples?: number
  last_probe_ok?: boolean
  last_probe_error?: string
  /** 最近一次探测时间（毫秒）；0 或缺失 = 从未探测。 */
  last_probe_at?: number
}

/**
 * 代理连通性测试结果。
 *
 * status 与 HTTP 状态码是两件事：代理不可达、认证失败或目标不可达时管理
 * 接口仍返回 200，只把失败写在 status/message 里。
 */
export interface NetworkTestResult {
  status: 'ok' | 'error'
  message: string
  latency_ms: number
}

/**
 * 上游模型健康摘要（总览健康卡数据源）。
 *
 * health 四态：unknown（窗口内无探测）/ healthy / degraded / down。
 * 判定口径与后端 probe.Classify 一致：连续失败 ≥3 判 down，最近成功且
 * 窗口成功率 ≥80% 判 healthy，其余 degraded。
 */
export interface ProbeHealth {
  model_id: string
  provider_id: string
  health: 'unknown' | 'healthy' | 'degraded' | 'down'
  success_rate: number // 窗口内成功率（0~1）；从未探测为 0
  window_count: number
  window_ok: number
  consecutive_fail: number
  last_ts: number // 最近探测时间（毫秒）；0 = 从未探测
  last_error?: string // 最近失败原因（最近成功则为空）
}

/** 连通率时间轴的一个分桶（某模型某时间窗）。桶内无探测记录的行不返回。 */
export interface ProbeTimelineBucket {
  /** 模型 id —— 一次可查多个模型，前端靠它把桶分流到各自的卡片。 */
  model_id: string
  ts: number // 桶起点（毫秒）
  count: number
  ok: number
  latency: number // 平均延迟（毫秒）
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
  /**
   * 后端把上游错误翻成的**下一步动作**（失败时才有）。
   *
   * message 是上游英文原文 —— 准确，但不回答「所以呢」。而 401/403/404/402/429
   * 各自对应完全不同的动作（换 key / 开权限 / 改模型名 / 充值 / 等一会），
   * 直接点破能省掉用户自己把英文错误翻译成操作判断这一步。
   */
  hint?: string
}

/** 单条凭据在上游侧的余额查询结果（对应 internal/upstream.CredentialBalance）。 */
export interface CredentialBalance {
  credential_id: string
  label: string
  /**
   * ok=额度与用量都已确认；unknown=只拿到部分事实，不能给出精确可用额；
   * error=查了但失败（凭据/网络）；unsupported=这个上游没有余额查询方式。
   */
  status: 'ok' | 'unknown' | 'error' | 'unsupported'
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
  /** ok=全部确认；partial=部分失败或未知；unknown=全部未知；error=全部失败；unsupported=该上游不支持。 */
  status: 'ok' | 'partial' | 'unknown' | 'error' | 'unsupported'
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
  /**
   * 思考方言：缺席 = 不改；空串 = 改回自动（按模型名推断）；
   * 已知值 = 管理员手工指定，推断不再覆盖它。
   */
  thinking_dialect?: string
  /**
   * 能否关闭思考的实测纠正，三态：缺席 = 不改；true/false = 以实测为准；
   * **null = 清除**（回到厂商文档表）。
   */
  can_disable_thinking?: boolean | null
  // 单价（元 / 百万 tokens）也走这一个 PATCH：Settings 页「模型价格」正是
  // 按 provider_id + model_id 直接改这几列，与模型编辑共用同一个端点。
  price_input?: number
  price_cache_hit?: number
  /** 缓存写入（Anthropic cache_creation）的输入价；0 = 未配置，按输入价计。 */
  price_cache_creation?: number
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
  /** 是否有加密副本可再次读取明文。升级前的历史密钥为 false。 */
  can_copy: boolean
  name: string
  enabled: boolean
  /**
   * 归属用户（多用户改造）。空串 = **无归属**。
   *
   * 当前写入路径必须提供归属用户。空串只可能来自损坏数据，鉴权会直接
   * 返回 401；界面必须显式标注「无归属（不可用）」，不能把它当成兼容状态。
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

/**
 * POST /admin/api/users/{id}/password-setup 的响应。
 *
 * token 只在这次响应里出现一次，服务端只保存哈希。`url_path` 把 token 放在
 * hash fragment 中，所以首次 HTTP 请求不会把 token 发到服务器访问日志里。
 */
export interface PasswordSetupLink {
  url_path: string
  expires_at: number
}

/**
 * POST /admin/api/password-setup/check 的响应。
 *
 * 无效、过期、已使用、账号被禁用都统一返回 valid=false，不区分具体原因，
 * 避免公开端点变成账号/链接枚举 oracle。
 */
export interface PasswordSetupStatus {
  valid: boolean
  username?: string
  expires_at?: number
}

/** GET /admin/api/me 的响应。 */
export interface Me {
  username: string
  display_name: string
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
// 作用域按身份收窄：管理员拿**全站**流水，普通用户只看自己。
// 见 `internal/admin/user_admin_handler.go` 的 ListTopups。
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
  /** 缓存写入价；0 = 未配置（按 price_input 计）。 */
  price_cache_creation: number
}

/**
 * 把公开模型名连到上游模型行，供探测数据挂到同一张卡片上。
 *
 * 与 ModelPrice 同为 model-names 的平行数组元素（meta[i] ↔ models[i]）。
 *
 * 只含上游模型行 id 与供应商名 —— 刻意不给 endpoint / 凭据 / 协议：
 * model-names 普通用户也可读，多给一个字段就多一条可枚举的信息。
 */
export interface ModelMeta {
  name: string
  /** upstream_models.id（探测表的外键）。空 = 该路由解析不出可用目标。 */
  upstream_model_id: string
  provider_name: string
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
  plaintext_key: string // 创建响应里的明文；之后可通过 /keys/{id}/secret 再取
}

export interface KeySecretResponse {
  plaintext_key: string
}

export interface Stats {
  total_requests: number
  /** 成功请求数（status='ok'）。成功率的分母是 success_count + error_count。 */
  success_count: number
  /** 真正失败数：status 不在 ok/canceled 之列。 */
  error_count: number
  /** 客户端主动断开，既不算成功也不算失败。 */
  canceled_count: number
  total_tokens: number
  input_tokens: number
  output_tokens: number
  cached_tokens: number // 缓存读取命中的输入 token 累计
  /** 写入缓存的输入 token 累计（Anthropic cache_creation）；OpenAI 系恒为 0。 */
  cached_creation_tokens: number
  cache_hit_rate: number // 0~1，= cached_tokens / input_tokens；无输入样本时为 0
  /** 所选区间内成功请求的加权输出速度（token/s）。 */
  avg_tokens_per_sec: number
  /** 参与速度加权的成功样本数；0 = 暂无样本。 */
  throughput_samples: number
  /** 所选区间内成功流式请求的平均首字延迟（毫秒）。 */
  avg_ttfb_ms: number
  /** 参与首字平均的成功样本数；0 = 暂无样本。 */
  ttfb_samples: number
  /**
   * 所选区间内「首字之后」的平均生成耗时（毫秒）= 平均(latency − ttfb)。
   *
   * 与 avg_ttfb_ms 分开下发是刻意的：TTFB 回答「等多久开始吐字」，
   * 生成耗时回答「开始吐字之后还要等多久」。两者相加 ≈ 平均总耗时，
   * 但合成一个数字之后，「慢在等首字」和「慢在吐字」在界面上就分不出来，
   * 而这两种情况该做的事完全不同（换路由 vs 换更快的模型/调小 max_tokens）。
   *
   * 只对 status='ok' 且测到过首字、且 latency>ttfb 的请求取平均；
   * 必须与 gen_samples 一起读：gen_samples=0 时这个值恒为 0，
   * 含义是「没有可用样本」而不是「生成瞬时完成」。
   */
  avg_gen_ms: number
  /** 参与生成耗时平均的请求数；0 = 暂无样本。 */
  gen_samples: number
  // 费用（元）：每条用量落库**当时**按模型单价算好并固化的金额求和
  // （usage_records.cost_total）。改价只影响此后的记录，不回溯历史。
  //
  // 普通用户拿到的是**自己**的合计（2026-10-10 起）—— 此前服务端对普通用户
  // 强制归零，依据是「网关不做计费结算」，但余额计费落地后该前提已不成立。
  cost: number
  /** 费用仍为 0、等待重算的记录数；与“未配置价格”的可核对信号。 */
  cost_pending_count: number
  /** 所选区间最近一次成功请求的毫秒时间戳；0 = 无成功请求。 */
  last_success_at: number
  /** 本次统计实际采用的查询边界（毫秒）。 */
  requested_from: number
  requested_to: number
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
  /**
   * 当前登录者的用户级 Token 配额与已用量（2026-10-11 前端审计 H1 的收尾）。
   *
   * 此前前端只能从 /me 拿这两个字段，而 /me 是**登录快照** —— 页面开着不放时
   * 「已用量」会长期停更。现在随 stats 一并下发，与余额同一套刷新时机。
   *
   * `quota_tokens = 0` 是**不限**（与 users.quota_tokens 的既有口径一致），
   * 前端据此决定是否显示「额度已用尽」徽章。
   *
   * 已用量的口径与后端配额**判定**完全一致（同一个累加列），因此界面显示的
   * 数字与「为什么我被拒了」不会互相矛盾。
   */
  quota_tokens: number
  used_tokens: number
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

/**
 * GET /admin/api/stats/overview 的响应。
 *
 * 当前配置计数与统计时间范围无关。上游额外区分“已启用”与“凭据就绪”：
 * 前者只是配置开关，后者才决定请求是否真的有机会到达上游。
 */
export interface StatsOverview {
  providers_total: number
  providers_enabled: number
  providers_ready: number
  providers_unready: number
  providers_unknown: number
  routes_total: number
  routes_enabled: number
  upstream_models_total: number
  users_total: number
  users_enabled: number
  keys_total: number
  keys_enabled: number
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
  /** 成功请求数（status='ok'）。canceled 既不算成功也不算失败。 */
  ok: number
  /** 失败请求数（status 不在 ok/canceled 之列）。 */
  err: number
  /**
   * 平均首字延迟（毫秒），只对**真正流式**的请求取平均。
   *
   * 必须与 n_ttfb 一起读：n_ttfb=0 时这个值恒为 0，含义是
   * 「没有流式请求、无从得知」而不是「首字快得不需要时间」。
   */
  avg_ttfb_ms: number
  /** 参与上面那个平均的请求数。 */
  n_ttfb: number
  /**
   * 平均输出速度（token/秒）。
   *
   * 只统计 status='ok' 的请求：canceled 的 latency 是「断开前跑了多久」，
   * 拿它当分母会把平均速度压低，而那个请求根本没跑完。
   */
  tps: number
  /**
   * 缓存读取命中的输入 token 合计。
   *
   * 与 Stats.cached_tokens 同口径（**不过滤 status**）：前缀缓存衡量的是
   * 「这次请求的输入有多少是命中的」，与这次请求最后成功与否无关 ——
   * 失败请求同样消耗了（可能被缓存的）输入。趋势表要靠它判断
   * 「今天的命中率相比上周掉了」。
   */
  cached_tokens: number
  /**
   * 缓存命中率（0~1）= cached_tokens / input_tokens。
   *
   * 分母是**输入**而不是总量：缓存只作用于输入侧，把输出 token 算进分母
   * 只会让命中率随输出长度浮动。与 Stats.cache_hit_rate 逐字同口径，
   * 所以「按天加权求和 == 区间合计」成立；input_tokens 为 0 时取 0。
   */
  cache_hit_rate: number
  /**
   * 「首字之后」的平均生成耗时（毫秒），与 Stats.avg_gen_ms 同口径：
   * 只算 status='ok'、测到过首字、且 latency>ttfb 的请求。
   *
   * 与 avg_ttfb_ms 相加 ≈ 平均总耗时。n_gen=0 时恒为 0，含义是
   * 「无样本」而不是「生成瞬时完成」。
   */
  avg_gen_ms: number
  /** 参与生成耗时平均的请求数；0 = 暂无样本。 */
  n_gen: number
}

/**
 * 四个 by-* 端点（by-day / by-model / by-key / by-provider）的响应：
 * 当页行 + 分组总数（2026-10-31 起统一成这个形状）。
 *
 * 为什么不是裸数组：by-key / by-model / by-provider 都吃 limit（默认 100、
 * 上限 1000），而用量统计页会把行求和印成「当前范围汇总」。分组数一旦超过
 * limit，那个合计只是前 N 名的合计 —— 界面上它与全量合计完全同形，只表现为
 * 「和顶部那张汇总卡对不上」。所以后端下发 total（**不受 limit 影响**），
 * 前端才能说出「显示前 N / 共 M」，并把那个合计改称「前 N 名合计」。
 *
 * by-day 不带 limit（时间序列必须覆盖整个区间），它的 total 恒等于行数。
 */
export interface UsageGroupPage {
  records: UsageGroupEntry[]
  /** 分组总数，不受 limit 影响。 */
  total: number
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
   * 空串 = **无归属的异常数据**。当前 schema 的用量必须固化到具体用户；
   * 这里仍保留失败态展示，避免损坏记录被误读成某个正常账号。
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
  /** 实际命中的上游配置 id；历史记录中可能为空。 */
  provider_id: string
  /**
   * 实际命中的上游展示名，落库时固化。
   *
   * 旧记录（升级前）为空，页面回退到 provider_id，并明确标成未记录名称。
   * 不能靠当前 providers 表反查：上游改名后历史会被改写成新名字，
   * 删除后整列又会突然变成空。
   */
  provider_name: string
  /**
   * 入站协议：chat / response / anthropic。
   *
   * 它回答的是“这次请求从哪个入口进来”，与 upstream_model 所属上游的
   * 协议不是同一个概念。页面用短标签展示，详情里保留原值。
   */
  ingress_protocol: string
  /**
   * 出站（连接上游）协议：chat / response / anthropic。
   *
   * 与 ingress_protocol 成对阅读：Responses 进、Chat 出说明网关做了协议
   * 转换；两者相同则是同协议直通。
   */
  upstream_protocol: string
  /**
   * 客户端来源地址，落库时固化。
   *
   * 后端按 trusted_proxy_cidrs 解析可信代理链后记录；旧记录为空，
   * 页面显示为未记录，不能用当前部署配置反推。
   */
  request_ip: string
  key_name: string // 调用密钥名称；落库时固化，密钥删除后仍是原来的名字
  key_id: string // 调用密钥 id（回退显示用）
  /**
   * 这条调用归属哪个用户（usage_records.user_id，落库时固化）。
   *
   * 密钥名**没有唯一约束**：两个用户各建一把都叫「主力」是合法的，
   * 此时只靠 key_name 无法区分。归属用户是唯一的区分维度。
   *
   * user_name 是**当前**用户名（users.username，可改）；用户已删除时为空，
   * 页面回退显示 user_id，而不是留白（留白会被读成「无归属」）。
   */
  user_id: string
  user_name: string
  /**
   * key_deleted / provider_deleted 表示这一行的密钥 / 上游**已经不在库里**。
   *
   * 名字是落库时固化的历史事实（key_name / provider_name），所以删掉对象
   * 不会让名字消失；但「还在不在」这件事前端判断不了 —— 空名字既可能是
   * 旧记录也可能是被删。由后端两个 EXISTS 子查询给出，界面据此显示
   * 「已删除 · 原名」，而不是把一串 sha256 id 当成名字。
   */
  key_deleted: boolean
  provider_deleted: boolean
  /**
   * 输入侧 token 总量（请求上下文、工具定义等），包含缓存命中与缓存写入。
   * 页面上的“新增输入”按 input_tokens - cached_tokens - cached_creation_tokens
   * 展示，避免把缓存 token 误读成本次新增处理的输入。
   */
  input_tokens: number
  total_tokens: number
  /**
   * 本次**生成**出来的 token 数（后端 usage_records.output_tokens）。
   *
   * 与 total_tokens 的区别是本列的关键：total_tokens = 输入 + 输出，而输入
   * 是请求侧送进去的、不是模型吐出来的。用总量算速度会把速度虚高好几倍
   * （1 万输入 / 2 百输出时，用总量算出来是真实生成速度的 50 倍）。
   */
  output_tokens: number
  /** 命中缓存的输入 token；0 表示未报缓存命中或确实没有命中。 */
  cached_tokens: number
  /** 写入缓存的输入 token（Anthropic cache_creation）；OpenAI 系恒为 0。 */
  cached_creation_tokens: number
  /** 推理模型消耗的思考 token；非推理模型通常为 0。 */
  reasoning_tokens: number
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
  /**
   * 失败三件套（2026-10-14 新增）：成功记录三者皆空/0。
   *
   * http_status 给量级（429 限流 vs 401 换 key vs 5xx 上游坏了），
   * error_code 是网关归一化短码，error_detail 是上游原文（脱敏 + 8KB 截断）。
   * 详情只存在于明细层 —— 超出保留期（默认 30 天）的记录只有短码没有原文。
   */
  http_status: number
  error_code?: string
  error_detail?: string
  /**
   * 失败决策轨迹（2026-10-17）：链上每个目标的决策与原因，形如
   * "1:deepseek/deepseek-chat -> fail_switch:upstream_error; 2:openai/gpt-4o -> ok"。
   *
   * 只在发生过转移或非首跳结束时才有内容。它回答 error_code 回答不了的问题：
   * 这次为什么走了备用上游，或者为什么明明失败了却没有换上游。
   */
  route_trace?: string
  /** 网关/上游链路请求标识；旧记录或未透传时为空串。 */
  request_id: string
  /**
   * 这次请求**实际下发**的思考档位（2026-10-31 新增）。
   *
   * 存的是网关夹取之后真正写到上游请求里的值，不是客户端请求的值 ——
   * 上游只支持 low/high 而客户端要 xhigh 时，这里记 low。空串 = 没要求思考。
   * anthropic 的预算式思考记 `budget:<n>`（n 是透传的 budget_tokens）。
   * 旧记录没有这一列，同样为空串，列表里就不显示胶囊。
   */
  effort: string
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
  /** 归属用户 id（管理员按人筛）。普通用户即使传别人的 id 也只会得到空集。 */
  userId?: string
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
  model_groups?: ConfigExportModelGroup[]
  networks?: ConfigExportNetwork[]
  /** v4 新增：用户分组（白名单为空 = 不限制）。 */
  user_groups?: ConfigExportUserGroup[]
  /** v4 新增：用户，含口令哈希与余额。 */
  users?: ConfigExportUser[]
  /** v4 新增：访问密钥，含**明文**密钥。 */
  access_keys?: ConfigExportAccessKey[]
  /** v4 新增：路由与故障转移链。 */
  routes?: ConfigExportRoute[]
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
  network_name?: string
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
  context_window?: number
  max_output_tokens?: number
  supports_thinking?: boolean
  /** 逗号分隔的挡位原文（与库里那列同形）；空 = 未配置。 */
  effort_levels?: string
  /**
   * 思考方言（见 UpstreamModel.thinking_dialect）；空 = 自动推断。
   *
   * 导出必须带上它：方言是照厂商文档配出来的运维知识，换台机器重建时
   * 丢了就只能靠模型名重新猜，而中转站改过名的模型猜不出来。
   */
  thinking_dialect?: string
  /**
   * 能否关闭思考的实测纠正：缺席 = 按厂商文档表，true/false = 以实测为准。
   * 导出必须带上它 —— 中转站归一化写法后，文档表在这条链路上可能是错的。
   */
  can_disable_thinking?: boolean
  price_input?: number
  price_cache_hit?: number
  price_cache_creation?: number
  price_output?: number
  group_name?: string
}

export interface ConfigExportModelGroup {
  display_name: string
}

export interface ConfigExportNetwork {
  name: string
  proxy_url: string
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
  /** v4 新增：下面四段各自计数，因为冲突语义不同（见后端 importResponse 注释）。 */
  user_groups_created: number
  users_created: number
  users_skipped: number
  access_keys_created: number
  access_keys_skipped: number
  routes_created: number
  routes_skipped: number
  warnings?: string[]
  notes?: string[]
}

/**
 * v4 导入导出：用户分组。白名单为空 = **不限制**（不是全拒），
 * 所以导入侧只在文件确实带了成员时才写。
 */
export interface ConfigExportUserGroup {
  name: string
  description?: string
  models?: string[]
}

/**
 * v4 导入导出：用户。`balance_cents` 缺省 = **不限额**（库里落 NULL），
 * 与 0（余额耗尽）是两种不同的状态，不能合并。
 */
export interface ConfigExportUser {
  username: string
  display_name?: string
  /** PBKDF2 自描述串（算法与迭代数写在串里），跨机可直接校验。 */
  password_hash?: string
  role?: string
  status?: string
  quota_tokens?: number
  used_tokens?: number
  balance_cents?: number
  remark?: string
  group_name?: string
}

/**
 * v4 导入导出：访问密钥。带**明文**是因为库里只存 sha256 与密文，
 * 迁移后只有哈希等于把密钥永久作废。
 */
export interface ConfigExportAccessKey {
  key: string
  name: string
  user?: string
  enabled?: boolean
  quota_tokens?: number
  rpm_limit?: number
  tpm_limit?: number
  allowed_models?: string[]
  expires_at?: number
  allowed_ips?: string
  group_name?: string
}

/** v4 导入导出：路由。公开名撞名时**跳过不改名**（那是调用方代码里的地址）。 */
export interface ConfigExportRoute {
  public_name: string
  enabled: boolean
  failover_enabled: boolean
  targets?: ConfigExportRouteTarget[]
}

export interface ConfigExportRouteTarget {
  provider_slug: string
  model_id: string
  enabled: boolean
}

// Admin route catalog: provider selection needs no endpoint or credentials.
export interface RouteCatalogProvider {
  id: string
  slug: string
  name: string
  enabled: boolean
  ready: boolean | null
  ready_reason?: string
}
export interface RouteCatalogEntry extends Route {
  targets: RouteTarget[]
  chain: { total: number; enabled: number; standby: number }
}
export interface RouteCatalog {
  routes: RouteCatalogEntry[]
  providers: RouteCatalogProvider[]
  models: UpstreamModel[]
  generated_at: number
}

// ---------- 站点品牌（2026-10-31） ----------
//
// 登录页要在**没有会话**时读到它，所以后端 GET /admin/api/site 是公开端点，
// 字段只有这两项 —— 公开端点不能顺带把其它设置带出去。
// 空串语义：未配置。前端把空站点名换成默认名（见 ui/branding.ts），
// 空 LOGO 则退回首字母方块（见 ui/BrandMark.tsx）。
export interface SiteBranding {
  site_name: string
  site_logo: string
}

// ---------- 上游协议探测（2026-10-31） ----------
//
// 「新建上游」时拿 endpoint + key 去问上游：三种协议里这台机器支持哪几种。
// ok 的判据是**路径存在**（状态码不是 404/405），所以 401/403 也算存在，
// 此时 message 会提示「路径存在但鉴权未通过」，避免被读成「协议可用」。
export interface ProtocolProbeEntry {
  protocol: string
  ok: boolean
  status: number
  message: string
}
export interface ProtocolProbeResult {
  results: ProtocolProbeEntry[]
}
