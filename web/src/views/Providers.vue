<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import { toast, confirmBox } from '../ui'
import { fmtSpeed, fmtSec, fmtPercent } from '../fmt'
import type { Provider, Credential, UpstreamModel, DiscoveredModel, ModelTestResult, ProviderBalance, CredentialBalance } from '../types'
import AppModal from '../components/AppModal.vue'

const err = ref('')
const loading = ref(true)
const providers = ref<Provider[]>([])
const expandedId = ref('')
const credsMap = reactive<Record<string, Credential[]>>({})
const modelsMap = reactive<Record<string, UpstreamModel[]>>({})
// testing / modelTestResults（在 testModel 附近定义）见那里的说明。

/**
 * shortEndpoint 把过长的 Endpoint 压成「头 … 尾」。
 *
 * 为什么必须压：Endpoint 是这行里最长的字段，不压就会把整张表撑宽到需要
 * 横向滚动 —— 而横滚会把右侧的「操作」列推出视野，那一列恰好是这一行里
 * 唯一有交互的东西。为了一条只读文本牺牲操作入口，是最亏的交换。
 *
 * 为什么掐中间而不是尾部省略（…）：一个地址最有辨识度的是**两端** ——
 * 域名说明打的是谁，路径尾巴说明打到哪。尾部省略会把「chat/completions」
 * 这半截丢掉，留下的 `https://api.deepseek.com/v1/…` 反而不如掐中间有用。
 * 完整地址仍在 title 里，鼠标悬停可见。
 */
function shortEndpoint(s: string): string {
  if (s.length <= 44) return s
  return s.slice(0, 22) + '…' + s.slice(-20)
}

async function load() {
  loading.value = true
  err.value = ''
  try {
    providers.value = await api.providers()
    // 已展开的行刷新子资源
    if (expandedId.value) await openExpand(expandedId.value, true)
  } catch (e) {
    if ((e as { status?: number }).status === 401) return
    // 失败**必须留下可见的错误态**：只弹 toast 的话，页面保留空列表/
    // 空表格，呈现成「暂无数据」—— 而真实原因是请求失败了。运维会据此
    // 判断「今天没有流量」，进而排除掉网关/上游故障这个真正的方向。
    // 与 Users/Groups 的持久错误态同口径（见其 v-else-if="err"）。
    err.value = '加载上游失败：' + (e as Error).message
    toast('加载失败：' + (e as Error).message, 'err')
  } finally {
    loading.value = false
  }
}

async function openExpand(id: string, silent = false) {
  if (expandedId.value === id && !silent) {
    expandedId.value = ''
    return
  }
  expandedId.value = id
  try {
    const [c, m] = await Promise.all([api.credentials(id), api.models(id)])
    credsMap[id] = c
    modelsMap[id] = m
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('加载子资源失败：' + (e as Error).message, 'err')
  }
}

// ---------- Provider 表单 ----------
// 新建只填 显示名称 / 协议 / Endpoint（可选 api_key）；
// 编辑时可展开高级选项，单独调整 slug（只读）、超时、重试。

const pForm = reactive({
  open: false,
  editing: '' as string,
  name: '',
  protocol: 'openai-chat',
  endpoint: '',
  api_key: '',
  slug: '',
  timeout_ms: 0,
  max_retries: 0,
  showAdvanced: false,
})

function openProvider(p?: Provider) {
  pForm.editing = p?.id ?? ''
  pForm.name = p?.name ?? ''
  pForm.protocol = p?.protocol || 'openai-chat'
  pForm.endpoint = p?.endpoint ?? ''
  pForm.api_key = ''
  pForm.slug = p?.slug ?? ''
  pForm.timeout_ms = p?.timeout_ms ?? 0
  pForm.max_retries = p?.max_retries ?? 0
  pForm.showAdvanced = false
  pForm.open = true
}

async function submitProvider() {
  if (!pForm.endpoint.trim()) {
    toast('Endpoint 为必填项', 'err')
    return
  }
  try {
    if (pForm.editing) {
      // PATCH 为字段级部分更新：只发要改的字段；timeout_ms/max_retries 传 0 即「回到全局默认」
      await api.updateProvider(pForm.editing, {
        name: pForm.name.trim() || pForm.endpoint.trim(),
        protocol: pForm.protocol,
        endpoint: pForm.endpoint.trim(),
        timeout_ms: Number(pForm.timeout_ms) || 0,
        max_retries: Number(pForm.max_retries) || 0,
      })
      toast('上游已更新')
    } else {
      await api.createProvider({
        name: pForm.name.trim(),
        protocol: pForm.protocol,
        endpoint: pForm.endpoint.trim(),
        api_key: pForm.api_key.trim() || undefined,
      })
      toast('上游已创建')
    }
    pForm.open = false
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('保存失败：' + (e as Error).message, 'err')
  }
}

async function toggleProvider(p: Provider) {
  try {
    // 只发改动字段：其余字段后端保持原值
    await api.updateProvider(p.id, { enabled: !p.enabled })
    toast(!p.enabled ? '已启用' : '已停用')
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('操作失败：' + (e as Error).message, 'err')
  }
}

async function removeProvider(p: Provider) {
  const ok = await confirmBox({
    title: `删除上游「${p.name}」？`,
    body: '将同时影响其下模型与路由的可用性；关联数据不会自动清理。',
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return
  try {
    await api.deleteProvider(p.id)
    toast('已删除')
    if (expandedId.value === p.id) expandedId.value = ''
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('删除失败：' + (e as Error).message, 'err')
  }
}

// 「测试」按钮的客户端超时（FE-06）：测试走的是真实的上游调用，耗时由上游与
// 设置页配的 upstream_timeout_ms 决定，不能沿用管理 API 的默认 30s。
// 取「运行时非流式超时 + 5s 余量」，让后端先一步超时、把真实的失败原因带回来，
// 而不是被浏览器端掐成一句「请求超时」。设置读取失败时退回 150s 保守值。
const testTimeoutMs = ref(150_000 + 5_000)

// 正在飞行中的探测：**按行 id 的集合**，不是单个字符串（2026-10-10 修复的 P1）。
//
// 单一字符串装不下两个并行的测试：一次探测最长可达 upstream_timeout + 余量
// （真实打上游），期间点第二行的「测试」会把标记冲成后者，于是
//   - 前一行的按钮立刻解禁（判定是 testing === id），可以再点一次，
//     对同一个正在挣扎的上游并发两个探测；
//   - 先结束的那轮在 finally 里清空**同一个**标记，把仍在跑的那行也解锁。
// 结果是「测试中…」显示错误，且对病态上游的探测次数失去上限。
//
// 集合 + 按 id 清除后，每一行只受自己的测试约束。
//
// 现在只服务上游模型行的「测试」：provider 行上的那个按钮已按需求移除
// （api.testProvider 与后端 /providers/{id}/test 都保留，见 AUDIT 报告）。
const testing = ref(new Set<string>())

function isTesting(id: string): boolean {
  return testing.value.has(id)
}
function setTesting(id: string, on: boolean): void {
  // 整体替换而不是 add/delete：Vue 的 ref 对 Set 的深层响应式需要
  // 重新赋值才必然触发（Proxy 的就地修改在部分场景下漏更新）。
  const next = new Set(testing.value)
  if (on) next.add(id)
  else next.delete(id)
  testing.value = next
}

// 每行的探测结果，按 upstream_models.id 索引。
//
// 为什么留住结果而不是只弹 toast：探测会**真实计费**，且失败原因是管理员
// 唯一能据以行动的信息（404 改模型名、403 开权限、401 换 key）。
// toast 5.2 秒后消失，用户来不及照着改配置；而且并发点几行时单例 toast
// 会被后来的顶掉，只剩最后一条。
const modelTestResults = reactive<Record<string, ModelTestResult>>({})

async function testModel(m: UpstreamModel) {
  if (isTesting(m.id)) return // 防连点：同一行已有探测在跑
  setTesting(m.id, true)
  delete modelTestResults[m.id] // 先清旧结果，避免把上一轮的绿/红留在屏幕上
  try {
    modelTestResults[m.id] = await api.testModel(m.id, testTimeoutMs.value)
  } catch (e) {
    if ((e as { status?: number }).status === 401) return
    // 走到这里说明**管理接口本身**失败了（网络/超时/5xx），与「模型不可用」
    // 不是一回事 —— 后者是 200 + status=error，由上面的分支处理。
    // 分开显示，否则会把网关自己的问题读成上游模型的问题。
    modelTestResults[m.id] = {
      status: 'error',
      model_id: m.model_id,
      provider_id: m.provider_id,
      message: '管理接口调用失败：' + (e as Error).message,
      latency_ms: 0,
    }
  } finally {
    setTesting(m.id, false) // 只清自己这一行
  }
}

// testResultLine 把探测结果压成界面上那一行。
//
// 成功：`可用 · 1234ms · 输出 1 tok`。耗时与用量都带出来，因为探测的
// 成本与体感全在这两个数上 —— 只说「可用」就没法区分「可用但很慢」。
// 失败：原样给出上游原因；InBand 的错误额外加一句说明，否则
// 「HTTP 是成功的却报错」会看起来像网关自己坏了。
function testResultLine(r: ModelTestResult): string {
  if (r.status === 'ok') {
    const parts = [`可用`, `${r.latency_ms}ms`]
    if (r.output_tokens) parts.push(`输出 ${r.output_tokens} tok`)
    return parts.join(' · ')
  }
  return r.in_band ? `${r.message}（上游以 HTTP 200 返回错误）` : r.message
}

// ---------- 凭据表单 ----------

// 表单里**没有 weight**：需求明确要求凭据区域去掉权重。
//
// 权重没有被删除 —— `provider_credentials.weight` 列还在，负载均衡
// （upstream.Pool.selectWeighted）照常按它轮询，后端也照常接受
// PATCH weight。这里只是不再提供输入口，于是：
//   - 新建凭据由后端兜底为权重 1（credential_handler.go 的 `weight <= 0 → 1`）；
//   - 已有凭据的权重**保持原值**（PATCH 不传 weight = nil = 不改），
//     不会被静默重置成 1。
// 副作用是界面上再也改不了权重（多凭据配额分配只能靠后端/API），
// 这是用户明确要求的结果，已在 AUDIT/task-provider-ui.md 里记明。
const cForm = reactive({ open: false, provider: '' as string, editing: '' as string, label: '', api_key: '' })

function openCred(providerId: string, c?: Credential) {
  cForm.provider = providerId
  cForm.editing = c?.id ?? ''
  cForm.label = c?.label ?? ''
  cForm.api_key = ''
  cForm.open = true
}

async function submitCred() {
  if (!cForm.editing && !cForm.api_key) {
    toast('api_key 为必填项', 'err')
    return
  }
  try {
    if (cForm.editing) {
      // 刻意不带 weight：nil 语义是「保持原值」，正是这里要的。
      await api.updateCredential(cForm.editing, { label: cForm.label.trim(), ...(cForm.api_key ? { api_key: cForm.api_key } : {}) })
    } else {
      await api.createCredential(cForm.provider, { label: cForm.label.trim(), api_key: cForm.api_key })
    }
    cForm.open = false
    toast(cForm.editing ? '凭据已更新' : '凭据已创建')
    await openExpand(cForm.provider, true)
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('保存失败：' + (e as Error).message, 'err')
  }
}

async function removeCred(c: Credential) {
  const ok = await confirmBox({ title: `删除凭据「${c.label || c.id}」？`, danger: true, confirmLabel: '删除' })
  if (!ok) return
  try {
    await api.deleteCredential(c.id)
    toast('已删除')
    await openExpand(c.provider_id, true)
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('删除失败：' + (e as Error).message, 'err')
  }
}

// ---------- 凭据余额 ----------

// 余额是**点按查询**的结果，不是后台轮询的快照。
//
// 为什么不做自动刷新：那是一个会被据此做充值决策的数字。让它可能显示
// 十分钟前的值，比不给这个功能更糟 —— 管理员看到「还剩 300 元」而实际已经
// 见底，据此决定「不用充」，问题在第二天才暴露。所以这里只保留手动查询，
// 并把取数时刻一并显示出来，让人知道这个数字有多旧。
//
// 每行一个独立标记（而不是全局单个）：多凭据可以并发点，否则点第二行会
// 被第一行的「查询中…」挡住，而两者的等待时间是各自的。
const balances = reactive<Record<string, ProviderBalance>>({})
const balanceBusy = ref(new Set<string>())

function isBalanceBusy(id: string): boolean {
  return balanceBusy.value.has(id)
}
function setBalanceBusy(id: string, on: boolean): void {
  const next = new Set(balanceBusy.value)
  if (on) next.add(id)
  else next.delete(id)
  balanceBusy.value = next
}

async function queryBalance(providerId: string) {
  if (isBalanceBusy(providerId)) return
  setBalanceBusy(providerId, true)
  try {
    balances[providerId] = await api.providerBalance(providerId)
  } catch (e) {
    if ((e as { status?: number }).status === 401) return
    // 管理接口本身失败与「上游余额查不到」是两件事：前者是网关的问题，
    // 后者在响应里是 status=error。分开显示，否则管理员会去换 key 而真正
    // 该做的是重试。
    balances[providerId] = {
      provider_id: providerId,
      provider: '',
      status: 'error',
      message: '管理接口调用失败：' + (e as Error).message,
      results: [],
    }
  } finally {
    setBalanceBusy(providerId, false)
  }
}

// balanceLine 把一条凭据的余额压成一行。
//
// 分三个状态措辞，因为三者的后续动作互不相干：
//   - unsupported：这个上游压根没有余额查询方式 → 去它自己的后台看；
//   - error：查了但失败 → 401 换 key、超时重试；
//   - ok：拿到数 → 数字 + 时刻 + 上游附注。
// 混成一句「查询失败」会让管理员在三种完全不同的排查方向之间瞎猜。
function balanceLine(r: CredentialBalance): string {
  if (r.status === 'unsupported') return r.message || '该上游不支持余额查询'
  if (r.status === 'error') return r.message || '查询失败'
  const at = r.fetched_at ? new Date(r.fetched_at).toLocaleTimeString() : ''
  const head = `${(r.amount ?? 0).toFixed(2)} ${r.currency ?? ''}`.trim()
  return [head, at, r.detail].filter(Boolean).join(' · ')
}

function balanceClass(r: CredentialBalance): string {
  if (r.status === 'ok') return 'bal-ok'
  return r.status === 'unsupported' ? 'bal-na' : 'test-err'
}

// ---------- 模型：编辑表单 ----------

// 模型编辑表单。价格不在这里配 —— 单价属于「设置 → 模型价格」分类，
// 按 供应商 × 模型 维度管理（同一 model_id 不同供应商可以不同价）。
const mForm = reactive({
  open: false,
  provider: '' as string,
  editing: '' as string,
  model_id: '',
  context_window: 0,
  max_output_tokens: 0,
  enabled: true,
  // effortCsv 是「思考挡位」的逗号分隔原文，与后端列、导出文件同形。
  // 空串 = 未配置（网关不干预思考强度）。
  effortCsv: '',
  // thinking 是「能否思考」的三态。必须显式建模成三态而不是布尔加一个
  // 「未配置」隐含态：checkbox 天然只有 on/off，而 on/off 与
  // 「支持 / 不支持」都对应不上第三种状态（未配置）。用 string 三态
  // ('unset' | 'yes' | 'no') 是唯一诚实的表示。
  thinking: 'unset' as 'unset' | 'yes' | 'no',
})

// knownEffortLevels 是网关认识的挡位（强度升序），必须与后端
// internal/effort.KnownLevels 一致。**刻意不放在后端下发**：这份清单是
// 网关的常量而非某个部署的配置，多一次请求换一份常量不值得；而一旦写死在
// 两处，后端加档时前端会静默少一个可选项 —— 那种故障没有任何症状。
const knownEffortLevels = ['minimal', 'low', 'medium', 'high', 'xhigh'] as const

// thinkingOptions 是「能否思考」的三态选项。措辞刻意用「未配置」而不是
// 「自动」/「默认」—— 后两者暗示网关会替你决定，而「未配置」的含义是
// 「网关不干预」，这是两件不同的事。
const thinkingOptions = [
  { v: 'unset' as const, label: '未配置' },
  { v: 'yes' as const, label: '支持' },
  { v: 'no' as const, label: '不支持' },
]

function openModel(providerId: string, m?: UpstreamModel) {
  mForm.provider = providerId
  mForm.editing = m?.id ?? ''
  mForm.model_id = m?.model_id ?? ''
  mForm.context_window = m?.context_window ?? 0
  mForm.max_output_tokens = m?.max_output_tokens ?? 0
  mForm.enabled = m?.enabled ?? true
  // unknown 档位附在后面：它们是网关不认、但可能是上游认得的厂商私有值，
  // 编辑时必须原样带回去，否则点一次保存就把它们悄悄抹掉了。
  mForm.effortCsv = [...(m?.effort_levels ?? []), ...(m?.unknown_effort_levels ?? [])].join(',')
  mForm.thinking =
    m?.supports_thinking === undefined ? 'unset' : m.supports_thinking ? 'yes' : 'no'
  // 重置「添加」流程状态
  discovered.value = []
  selected.value = []
  discoverMsg.value = ''
  filter.value = ''
  mForm.open = true
}

// toggleEffortLevel 勾/取消某个挡位。
//
// 用集合而不是字符串拼装来算勾选态：CSV 是有序文本，直接 includes 会把
// 「xhigh」误判为含「high」（子串包含），那是会点错的。
function hasEffortLevel(l: string): boolean {
  return mForm.effortCsv.split(',').some((x) => x.trim() === l)
}

function toggleEffortLevel(l: string) {
  const parts = mForm.effortCsv.split(',').map((x) => x.trim()).filter(Boolean)
  const i = parts.indexOf(l)
  if (i >= 0) parts.splice(i, 1)
  else parts.push(l)
  // 按强度升序写回：CSV 是人手抄的，同一组挡位的两种顺序在库里应当是同一个值。
  const rank = (x: string) => {
    const i = knownEffortLevels.indexOf(x as (typeof knownEffortLevels)[number])
    return i < 0 ? knownEffortLevels.length : i
  }
  parts.sort((a, b) => rank(a) - rank(b))
  mForm.effortCsv = parts.join(',')
}

// setThinking 设定「能否思考」三态，并把挡位与它保持自洽。
//
// 自洽规则只有一条，且方向是刻意的：**明确不支持思考时清空挡位**。
// 反过来不成立 —— 「支持思考」不必配挡位（客户端可以用上游默认）。
// 不这样收敛的话，界面上会出现「不支持思考，却配了 high/xhigh」这种
// 自相矛盾的状态，而它的实际后果是：网关照常剥掉思考配置，档位却仍被
// 当成有效能力披露给 /v1/models，于是客户端以为能选、选了没效果。
// 这正是本次要消灭的那类沉默故障，所以宁可让配置保持自洽。
function setThinking(v: 'unset' | 'yes' | 'no') {
  mForm.thinking = v
  if (v === 'no') mForm.effortCsv = ''
}

async function submitModel() {
  if (!mForm.editing) return
  if (!mForm.model_id.trim()) {
    toast('上游模型名（model_id）为必填项', 'err')
    return
  }
  const body = {
    model_id: mForm.model_id.trim(),
    context_window: Number(mForm.context_window) || 0,
    max_output_tokens: Number(mForm.max_output_tokens) || 0,
    enabled: mForm.enabled,
    // 空串 = 清除（回到未配置），不是「保持原值」—— 与界面上的「一个都不
    // 勾选」是同一个动作，后端据此落 NULL。
    effort_levels: mForm.effortCsv,
    // 三态映射：unset → null（后端解成「清除」，落 NULL = 未配置）。
    // 不能省掉这个字段 —— 省略是「不改」，而界面上「切回未配置并保存」
    // 必须真的写下去，否则这个动作在下次打开表单时会「弹回来」。
    supports_thinking: mForm.thinking === 'unset' ? null : mForm.thinking === 'yes',
  }
  try {
    await api.updateModel(mForm.editing, body)
    mForm.open = false
    toast('模型已更新')
    await openExpand(mForm.provider, true)
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('保存失败：' + (e as Error).message, 'err')
  }
}

// ---------- 模型：探测 + 多选胶囊式添加 ----------

const discovering = ref(false)
const discovered = ref<DiscoveredModel[]>([])
const discoverMsg = ref('')
const importing = ref(false)
const filter = ref('')
// 已选中的模型（胶囊池），一次获取后可点击多条加入
const selected = ref<DiscoveredModel[]>([])

const filteredDiscovered = computed(() => {
  const q = filter.value.trim().toLowerCase()
  if (!q) return discovered.value
  return discovered.value.filter(
    (d) => d.model_id.toLowerCase().includes(q) || (d.display_name ?? '').toLowerCase().includes(q),
  )
})

function isSelected(id: string) {
  return selected.value.some((s) => s.model_id === id)
}

function toggleSelect(d: DiscoveredModel) {
  const i = selected.value.findIndex((s) => s.model_id === d.model_id)
  if (i >= 0) selected.value.splice(i, 1)
  else selected.value.push(d)
}

function removeSelected(id: string) {
  const i = selected.value.findIndex((s) => s.model_id === id)
  if (i >= 0) selected.value.splice(i, 1)
}

async function runDiscover() {
  discovering.value = true
  discovered.value = []
  selected.value = []
  discoverMsg.value = ''
  try {
    const r = await api.discoverModels(mForm.provider)
    if (r.status === 'ok') {
      discovered.value = r.models ?? []
      if (discovered.value.length === 0) discoverMsg.value = '上游未返回任何模型'
    } else {
      discoverMsg.value = r.message || '获取失败'
    }
  } catch (e) {
    discoverMsg.value = (e as Error).message
  } finally {
    discovering.value = false
  }
}

async function confirmImport() {
  const list = selected.value
  if (list.length === 0) return
  importing.value = true
  try {
    const r = await api.importModels(
      mForm.provider,
      list.map((d) => ({
        model_id: d.model_id,
        display_name: d.display_name ?? '',
        context_window: d.context_window ?? 0,
        max_output_tokens: d.max_output_tokens ?? 0,
      })),
    )
    toast(`已添加 ${r.imported} 个模型`)
    mForm.open = false
    await openExpand(mForm.provider, true)
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('添加失败：' + (e as Error).message, 'err')
  } finally {
    importing.value = false
  }
}

async function removeModel(m: UpstreamModel) {
  const ok = await confirmBox({
    title: `删除模型「${m.model_id}」？`,
    body: '引用它的路由将无法解析，请先清理相关路由。',
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return
  try {
    await api.deleteModel(m.id)
    toast('已删除')
    await openExpand(m.provider_id, true)
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('删除失败：' + (e as Error).message, 'err')
  }
}

onMounted(() => {
  load()
  // 设置只需 upstream_timeout_ms 一个字段：拉不到（罕见）就保持保守值，
  // 不阻塞列表加载。
  api
    .settings()
    .then((s) => {
      if (s.upstream_timeout_ms > 0) testTimeoutMs.value = s.upstream_timeout_ms + 5_000
    })
    .catch(() => {
      /* 保持 150s 保守值 */
    })
})
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>上游与模型</h1>
        <div class="sub">上游服务、凭据与上游模型管理</div>
      </div>
      <div class="head-actions">
        <button class="btn" :disabled="loading" @click="load">刷新</button>
        <button class="btn btn-primary" @click="openProvider()">新建上游</button>
      </div>
    </div>

    <div class="panel">
      <div v-if="loading && providers.length === 0" class="loading">加载中…</div>
      <!--加载失败**必须**与「确实没有数据」在界面上可区分：把请求失败呈现成
           「暂无数据」会让运维误判为无流量，从而排除掉网关/上游故障这个方向。
           与 Users/Groups 的错误态同口径。 -->
      <div v-else-if="err" class="empty"><div class="big">⚠</div>{{ err }}</div>
      <div v-else-if="providers.length === 0" class="empty">
        <div class="big">⌘</div>
        还没有上游服务，点击右上角「新建上游」开始
      </div>
      <div v-else class="tbl-wrap">
        <table class="tbl">
          <thead>
            <tr>
              <th>上游</th>
              <th>协议</th>
              <th>Endpoint</th>
              <th class="c-act">操作</th>
            </tr>
          </thead>
          <!-- 每个上游自带一个 tbody，展开区作为该 tbody 里的第二个 <tr>。
               不能把展开区写成 tbody 之外的 div —— 那不是合法的表格结构，
               浏览器会把 tbody 之间的孤立行吞掉或重排。 -->
          <tbody v-for="p in providers" :key="p.id">
            <tr>
              <td>
                <div class="cell-key">
                  <span class="name">{{ p.name }}</span>
                  <span class="badge" :class="p.enabled ? 'badge-live' : 'badge-off'">{{ p.enabled ? '启用' : '停用' }}</span>
                  <!-- 运行时未就绪：库里配得好好的，但上游池没建出任何可用凭据，
                       请求打过去必然失败。改造前这个状态只进日志，界面上完全看不出来，
                       运维只能靠「莫名 500」反推。只在「已启用」时标 ——
                       停用是主动行为，不是故障。 -->
                  <span
                    v-if="p.enabled && !p.ready"
                    class="badge badge-err"
                    :title="p.ready_reason || '没有可用的上游凭据'"
                  >
                    未就绪
                  </span>
                </div>
                <div class="sub-line mono">{{ p.slug }}</div>
              </td>
              <td><span class="badge">{{ p.protocol }}</span></td>
              <td>
                <span class="mono ep" :title="p.endpoint">{{ shortEndpoint(p.endpoint) }}</span>
                <!-- 未就绪的原因贴着 Endpoint：它说的就是「这个地址打不通」，
                     拆到别的列会让人对不上是哪个上游出的问题。 -->
                <div v-if="p.enabled && !p.ready && p.ready_reason" class="sub-line err">
                  {{ p.ready_reason }}
                </div>
              </td>
              <td class="c-act">
                <div class="row-actions">
                  <!-- provider 级的「测试」按钮已按需求移除。
                       它拉的是 /models，只能证明「endpoint + 凭据通」，
                       证明不了任何具体模型能推理 —— 而模型行现在各有自己的
                       「测试」（发一次最小真实推理）。留着它反而提供一个
                       比模型级测试更弱的绿色信号，容易被当成「都正常」。
                       后端 POST /providers/{id}/test 与 api.testProvider
                       都保留（见 AUDIT/task-provider-ui.md 的理由）。 -->
                  <button class="btn btn-sm btn-ghost" @click="openExpand(p.id)">
                    {{ expandedId === p.id ? '收起' : '展开' }}
                  </button>
                  <button class="btn btn-sm btn-ghost" @click="openProvider(p)">编辑</button>
                  <button class="btn btn-sm btn-danger" @click="removeProvider(p)">删除</button>
                  <button class="switch" :class="{ on: p.enabled }" :title="p.enabled ? '停用' : '启用'" @click="toggleProvider(p)"></button>
                </div>
              </td>
            </tr>

            <tr v-if="expandedId === p.id" class="expand-row">
              <td colspan="4">
                <div class="expand-grid">
                  <div class="expand-col">
                    <h4>
                      凭据（{{ (credsMap[p.id] ?? []).length }}）
                      <button class="btn btn-sm" @click="openCred(p.id)">添加凭据</button>
                      <button
                        class="btn btn-sm btn-ghost"
                        :disabled="isBalanceBusy(p.id)"
                        title="实时向该上游查询每把凭据的余额（不自动刷新）"
                        @click="queryBalance(p.id)"
                      >
                        {{ isBalanceBusy(p.id) ? '查询中…' : '查余额' }}
                      </button>
                    </h4>
                    <div v-if="(credsMap[p.id] ?? []).length === 0" class="empty sub-empty">
                      无凭据 —— 上游鉴权必需
                    </div>
                    <table v-else class="tbl sub-tbl">
                      <thead>
                        <tr>
                          <th>凭据</th>
                          <th>上游余额</th>
                          <th class="c-act">操作</th>
                        </tr>
                      </thead>
                      <tbody>
                        <!-- 权重 / 状态 / 创建时间三列已按需求移除。
                             注意 weight 在后端仍然生效（负载均衡按它轮询），
                             这里去掉的只是展示；见 AUDIT/task-provider-ui.md
                             里记的副作用：界面上不再能调整权重。
                             「状态」列显示的是 provider_credentials.status
                             （healthy / cooling，随冷却态变化），创建时间来自
                             created_at —— 两列都是只读展示，去掉不影响任何逻辑。
                             「凭据」单元格的内容**未改动**（标签 + 启停徽标），
                             这次只做需求要求的删除，不额外添加展示。 -->
                        <tr v-for="c in credsMap[p.id] ?? []" :key="c.id">
                          <td>
                            <div class="cell-key">
                              <span class="name">{{ c.label || c.id }}</span>
                              <span class="badge" :class="c.enabled ? 'badge-live' : 'badge-off'">
                                {{ c.enabled ? '启用' : '停用' }}
                              </span>
                            </div>
                          </td>
                          <!-- 余额显示在凭据行内而不是另起一表：余额挂在 key 上，
                               放远了就失去「哪一把快没钱了」这个唯一有用的读法。
                               没点过「查余额」时留白 —— 它不是 0，
                               两者必须可区分：一个是「没查」，一个是「确实没钱」。 -->
                          <td>
                            <div v-if="balances[p.id]" class="bal-cell">
                              <div
                                v-for="r in balances[p.id].results.filter((x) => x.credential_id === c.id)"
                                :key="r.credential_id"
                                class="bal-line"
                                :class="balanceClass(r)"
                              >
                                {{ balanceLine(r) }}
                              </div>
                              <span v-if="!balances[p.id].results.some((x) => x.credential_id === c.id)" class="dim">
                                —（无结果）
                              </span>
                            </div>
                            <span v-else class="dim">—</span>
                          </td>
                          <td class="c-act">
                            <div class="row-actions">
                              <button class="btn btn-sm btn-ghost" @click="openCred(p.id, c)">换钥</button>
                              <button class="btn btn-sm btn-danger" @click="removeCred(c)">删除</button>
                            </div>
                          </td>
                        </tr>
                      </tbody>
                    </table>
                  </div>

                  <div class="expand-col">
                    <h4>
                      上游模型（{{ (modelsMap[p.id] ?? []).length }}）
                      <button class="btn btn-sm" @click="openModel(p.id)">添加模型</button>
                    </h4>
                    <div v-if="(modelsMap[p.id] ?? []).length === 0" class="empty sub-empty">
                      无模型 —— 路由必须指向一个上游模型
                    </div>
                    <table v-else class="tbl sub-tbl">
                      <thead>
                        <tr>
                          <th>模型</th>
                          <th class="num-h">速度(tok/s)</th>
                          <th class="num-h">首字(s)</th>
                          <th class="num-h">成功率</th>
                          <th class="c-act">操作</th>
                        </tr>
                      </thead>
                      <tbody>
                        <tr v-for="m in modelsMap[p.id] ?? []" :key="m.id">
                          <td>
                            <div class="cell-key">
                              <span class="name mono">{{ m.model_id }}</span>
                              <span class="badge" :class="m.enabled ? 'badge-live' : 'badge-off'">
                                {{ m.enabled ? '启用' : '停用' }}
                              </span>
                            </div>
                            <!-- 思考挡位。这不是可选的展示项：agent 工具靠
                                 /v1/models 的 supported_reasoning 决定给用户开几档，
                                 那份数据就来自这一列的配置。不显示的话，
                                 「客户端以为支持、实际被网关静默降级」无从追查。 -->
                            <!-- 思考能力。两者（能否思考 / 有哪几档）是同一个问题的
                                 两面，必须一起显示：只显示挡位会让人以为「配了档位
                                 就等于支持思考」，而数据面在「不支持思考」时是会
                                 直接把思考配置剥掉的。 -->
                            <div v-if="m.supports_thinking !== undefined" class="sub-line">
                              思考：{{ m.supports_thinking ? '支持' : '不支持' }}
                            </div>
                            <div v-if="m.effort_levels?.length" class="sub-line">
                              挡位：{{ m.effort_levels.join(' · ') }}
                              <span v-if="m.unknown_effort_levels?.length" class="dim">
                                （未识别：{{ m.unknown_effort_levels.join('、') }}）
                              </span>
                            </div>
                            <!-- 探测结果留在行内而不是只弹 toast：探测会真实计费，
                                 失败原因是管理员唯一能据以行动的信息，而 toast
                                 5.2 秒后消失、并发时还会互相顶掉。 -->
                            <div
                              v-if="modelTestResults[m.id]"
                              class="sub-line"
                              :class="modelTestResults[m.id].status === 'ok' ? 'test-ok' : 'test-err'"
                            >
                              {{ testResultLine(modelTestResults[m.id]) }}
                            </div>
                          </td>
                          <td class="num-h dim">{{ m.tokens_per_sec ? fmtSpeed(m.tokens_per_sec) : '—' }}</td>
                          <td class="num-h dim">{{ m.ttfb_ms ? fmtSec(m.ttfb_ms) : '—' }}</td>
                          <td class="num-h">
                            <span v-if="m.call_count" class="sr" :class="{ warn: (m.success_rate ?? 1) < 1 }">
                              {{ fmtPercent(m.success_rate ?? 1) }}%
                            </span>
                            <span v-else class="dim">—</span>
                          </td>
                          <td class="c-act">
                            <div class="row-actions">
                              <!-- 快速测试该模型是否可用：真实发一次 max_tokens=1
                                   的推理请求。与已移除的 provider 级测试不同，
                                   这个能证明「这个 model_id 现在真的能推理」
                                   （模型下架/无权限/名字写错都会红）。 -->
                              <button
                                class="btn btn-sm btn-ghost"
                                :disabled="isTesting(m.id)"
                                :title="'真实调用一次（max_tokens=1）验证该模型可用'"
                                @click="testModel(m)"
                              >
                                {{ isTesting(m.id) ? '测试中…' : '测试' }}
                              </button>
                              <button class="btn btn-sm btn-ghost" @click="openModel(p.id, m)">编辑</button>
                              <button class="btn btn-sm btn-danger" @click="removeModel(m)">删除</button>
                            </div>
                          </td>
                        </tr>
                      </tbody>
                    </table>
                  </div>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- Provider 表单：表单填一半别丢，遮罩/Esc 都不关 -->
    <AppModal
      :open="pForm.open"
      :title="pForm.editing ? '编辑上游' : '新建上游'"
      :dismissable="false"
    >
      <form @submit.prevent="submitProvider">
        <div class="form-grid">
          <div class="field">
            <label>显示名称</label>
            <input v-model="pForm.name" class="input" placeholder="DeepSeek（留空则用 Endpoint）" />
          </div>
          <div class="field">
            <label>协议</label>
            <select v-model="pForm.protocol" class="select">
              <option value="openai-chat">openai-chat</option>
              <option value="anthropic">anthropic</option>
              <option value="openai-responses">openai-responses</option>
            </select>
          </div>
          <div class="field span2">
            <label>Endpoint *</label>
            <input v-model="pForm.endpoint" class="input mono" placeholder="https://api.deepseek.com" />
          </div>
          <div v-if="!pForm.editing" class="field span2">
            <label>API Key</label>
            <input v-model="pForm.api_key" class="input mono" type="password" placeholder="sk-...（可留空，稍后单独添加）" autocomplete="new-password" />
            <span class="tip">添加即启用；slug、超时与重试将自动使用默认值</span>
          </div>

          <template v-if="pForm.editing">
            <div class="field span2">
              <button type="button" class="btn btn-sm btn-ghost" @click="pForm.showAdvanced = !pForm.showAdvanced">
                {{ pForm.showAdvanced ? '收起高级选项' : '展开高级选项' }}
              </button>
            </div>
            <template v-if="pForm.showAdvanced">
              <div class="field">
                <label>Slug</label>
                <input :value="pForm.slug" class="input mono" disabled />
                <span class="tip">系统生成，不可修改</span>
              </div>
              <div class="field">
                <label>超时（毫秒）</label>
                <input v-model.number="pForm.timeout_ms" class="input num" type="number" min="0" step="1" />
                <span class="tip">0 = 使用全局默认</span>
              </div>
              <div class="field">
                <label>最大重试</label>
                <input v-model.number="pForm.max_retries" class="input num" type="number" min="0" max="10" step="1" />
                <span class="tip">
                  仅作用于<strong>幂等</strong>请求（如连通性测试时的 GET /models）。
                  对话是 POST，SDK 不会重试 —— 重试由「故障转移链换目标」承担。
                </span>
              </div>
            </template>
          </template>
        </div>
        <div class="form-actions">
          <button type="button" class="btn btn-ghost" @click="pForm.open = false">取消</button>
          <button type="submit" class="btn btn-primary">保存</button>
        </div>
      </form>
    </AppModal>

    <!-- 凭据表单：表单填一半别丢，遮罩/Esc 都不关 -->
    <AppModal
      :open="cForm.open"
      :title="cForm.editing ? '更新凭据' : '添加凭据'"
      max-width="480px"
      :dismissable="false"
    >
      <form @submit.prevent="submitCred">
        <div class="form-grid">
          <div class="field span2">
            <label>标签</label>
            <input v-model="cForm.label" class="input" placeholder="主账号 / 备用池" />
          </div>
          <div class="field span2">
            <label>API Key {{ cForm.editing ? '（留空则不更换）' : '*' }}</label>
            <input v-model="cForm.api_key" class="input mono" type="password" placeholder="sk-..." autocomplete="new-password" />
            <span class="tip">保存后加密入库，仅显示掩码，不再可见明文</span>
          </div>
          <!-- 权重输入框已按需求移除。weight 列与负载均衡逻辑都还在，
               只是界面上不再提供入口；已有凭据的权重保持原值不被改写。 -->
        </div>
        <div class="form-actions">
          <button type="button" class="btn btn-ghost" @click="cForm.open = false">取消</button>
          <button type="submit" class="btn btn-primary">保存</button>
        </div>
      </form>
    </AppModal>

    <!-- 模型：编辑单条（表单填一半别丢，遮罩/Esc 都不关） -->
    <AppModal
      v-if="mForm.editing"
      :open="mForm.open"
      title="编辑上游模型"
      max-width="500px"
      :dismissable="false"
    >
      <form @submit.prevent="submitModel">
        <div class="form-grid">
          <div class="field span2">
            <label>上游模型名（model_id）*</label>
            <input v-model="mForm.model_id" class="input mono" placeholder="deepseek-chat" />
            <span class="tip">上游 API 接受的真实模型名</span>
          </div>
          <div class="field">
            <label>上下文窗口</label>
            <input v-model.number="mForm.context_window" class="input num" type="number" min="0" step="1" placeholder="0 = 未设置" />
          </div>
          <div class="field">
            <label>最大输出</label>
            <input v-model.number="mForm.max_output_tokens" class="input num" type="number" min="0" step="1" placeholder="0 = 未设置" />
          </div>
          <div class="field span2">
            <span class="tip">单价（元/1M tokens）在「设置 → 模型价格」里按供应商 × 模型配置</span>
          </div>
          <!-- 「能否思考」用三态而不是 checkbox：这是不少工具侧的那个开关，
               而它的第三态（未配置）有真实语义 —— 网关不干预。把未配置
               显示成「不支持」会让所有没配过的模型凭空失去思考能力。 -->
          <div class="field span2">
            <label>思考模式</label>
            <div class="tri-switch" role="radiogroup" aria-label="思考模式">
              <button
                v-for="opt in thinkingOptions"
                :key="opt.v"
                type="button"
                class="btn btn-sm"
                :class="mForm.thinking === opt.v ? 'btn-primary' : 'btn-ghost'"
                role="radio"
                :aria-checked="mForm.thinking === opt.v"
                @click="setThinking(opt.v)"
              >
                {{ opt.label }}
              </button>
            </div>
            <span class="tip">
              「未配置」= 网关不干预，客户端要什么就发什么；「不支持」= 数据面会
              剥掉请求上的思考配置（并随 /v1/models 下发该声明）。该声明同时注入
              SDK 的模型档案。
            </span>
          </div>

          <!-- 思考挡位。用勾选而不是自由文本：档位名拼错（"x-hig"）在库里
               就成了一个网关不认、上游也未必认的值，而它的表现是「配了不生效」
               —— 那正是这一行要消灭的失败。勾选保证写进去的每个值都有定义。

               只在「明确支持思考」或「未配置」时出现：声明不支持思考却配着档位
               是自相矛盾的（setThinking 会顺手清空它），没有可展示的余地。 -->
          <div v-if="mForm.thinking !== 'no'" class="field span2">
            <label>思考挡位</label>
            <div class="effort-picker">
              <button
                v-for="l in knownEffortLevels"
                :key="l"
                type="button"
                class="btn btn-sm"
                :class="hasEffortLevel(l) ? 'btn-primary' : 'btn-ghost'"
                :aria-pressed="hasEffortLevel(l)"
                @click="toggleEffortLevel(l)"
              >
                {{ l }}
              </button>
            </div>
            <span class="tip">
              勾选该模型**真实支持**的思考强度档位。一个都不勾 = 未配置，网关不干预强度。
              客户端选了不在此列表里的档位时，网关会就近夹取（例如要 xhigh 而模型只支持
              high）并在日志留痕；档位**原样**发往上游（xhigh 不会被压成 high）。
              这份列表也会随 /v1/models 下发给 agent 工具（supported_reasoning）。
            </span>
          </div>
          <div class="field span2">
            <label class="check-line">
              <input v-model="mForm.enabled" type="checkbox" />
              启用该模型
            </label>
          </div>
        </div>
        <div class="form-actions">
          <button type="button" class="btn btn-ghost" @click="mForm.open = false">取消</button>
          <button type="submit" class="btn btn-primary">保存</button>
        </div>
      </form>
    </AppModal>

    <!-- 模型：探测 + 多选胶囊式添加（勾选状态是填到一半的「表单」，同样不许误关） -->
    <AppModal
      v-else
      :open="mForm.open"
      title="添加上游模型"
      max-width="560px"
      :dismissable="false"
    >
      <div class="add-model">
        <div class="am-toolbar">
          <button type="button" class="btn btn-sm" :disabled="discovering" @click="runDiscover">
            {{ discovering ? '获取中…' : '获取模型列表' }}
          </button>
          <input
            v-if="discovered.length"
            v-model="filter"
            class="input input-sm"
            placeholder="搜索模型…"
            style="flex: 1"
          />
        </div>
        <span v-if="discoverMsg" class="tip" style="color: var(--danger)">{{ discoverMsg }}</span>

        <!-- 已选胶囊池：点击列表项加入，点 × 移除 -->
        <div v-if="selected.length" class="pill-pool">
          <span v-for="s in selected" :key="s.model_id" class="pill">
            <span class="mono">{{ s.model_id }}</span>
            <button type="button" class="pill-x" title="移除" @click="removeSelected(s.model_id)">×</button>
          </span>
        </div>

        <!-- 可选列表 -->
        <div v-if="discovered.length" class="discover-list">
          <button
            v-for="d in filteredDiscovered"
            :key="d.model_id"
            type="button"
            class="discover-item"
            :class="{ active: isSelected(d.model_id) }"
            @click="toggleSelect(d)"
          >
            <span class="di-check">{{ isSelected(d.model_id) ? '✓' : '+' }}</span>
            <span class="mono">{{ d.model_id }}</span>
            <span class="di-cap">{{ d.context_window ? d.context_window + ' ctx' : '' }}{{ d.context_window && d.max_output_tokens ? ' / ' : '' }}{{ d.max_output_tokens ? d.max_output_tokens + ' out' : '' }}</span>
          </button>
        </div>
        <div v-else-if="!discovering && !discoverMsg" class="empty" style="padding: 18px 0">
          点击「获取模型列表」拉取上游可用模型
        </div>
      </div>
      <div class="form-actions">
        <button type="button" class="btn btn-ghost" @click="mForm.open = false">取消</button>
        <button
          type="button"
          class="btn btn-primary"
          :disabled="!selected.length || importing"
          @click="confirmImport"
        >
          {{ importing ? '添加中…' : `添加（${selected.length}）` }}
        </button>
      </div>
    </AppModal>
  </main>
</template>

<style scoped>
/* 表格末线的去留见 styles.css 的共享块（多 tbody 结构的理由记在那里）。 */
/* 单元格骨架（.cell-key）用全局的（styles.css），这里只留页面私有部分。 */
.sub-line.err { color: var(--danger); }
/* 单模型探测结果。成功/失败用与全局错误态一致的语义色，
   且允许换行：上游的失败原因是整句话，nowrap 会把它截断成看不全的半截。 */
.test-ok { color: var(--success); }
.test-err { color: var(--danger); white-space: normal; }
/* 凭据余额行。ok 用成功色，unsupported 用中性灰（它是「不适用」而不是故障，
   染成红色会让人以为要去修一个根本不存在的问题）。允许换行：余额附注常是
   一整句上游文案（额度不足、下周重置），nowrap 会把它截成看不懂的半截。 */
.bal-line { white-space: normal; }
.bal-ok { color: var(--success); }
.bal-na { color: var(--text-4); }
.bal-cell { min-width: 0; }
/* 挡位选择器：按钮按强度从弱到强排成一行，间距足够让「勾了哪几个」一眼可分。 */
.effort-picker {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-bottom: 4px;
}
/* 三态选择器与挡位选择器同一套按钮，靠 tri-switch 的类名区分间距与语义；
   独立成类是为了让「这是互斥的三选一」在 CSS 上也可读（radiogroup 的角色
   由模板给出，这里只管视觉）。 */
.tri-switch {
  display: flex;
  gap: 6px;
  margin-bottom: 4px;
}
/* Endpoint 的兜底宽度：shortEndpoint 已经把绝大多数地址掐到 44 字符内，
   这条 max-width 只兜极端情况（超长自定义路径），真正的主战场是 JS 截断，
   因为 CSS 的尾部省略会把路径尾巴吃掉。 */
.ep { display: inline-block; max-width: 360px; overflow: hidden; text-overflow: ellipsis; vertical-align: bottom; }
/* 展开区：整行铺满。它是详情而不是一条记录，所以不吃表格的行高亮。 */
.expand-row:hover { background: transparent; }
.expand-row > td { padding: 14px 12px 18px; white-space: normal; }
/* 子表比主表更紧凑：两层同尺寸表头会把展开区撑成一堵墙，
   而凭据/模型往往一列就是十几行。 */
.sub-tbl { font-size: 12.5px; }
.sub-tbl th,
.sub-tbl td { padding: 6px 8px; }
.sub-empty { padding: 14px 0; }
.sr {
  font-size: 11px;
  padding: 1px 7px;
  border-radius: var(--r-chip, 6px);
  background: var(--success-soft);
  color: var(--success-soft-foreground);
}
.sr.warn {
  background: var(--warning-soft);
  color: var(--warning-soft-foreground);
}
.add-model {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.am-toolbar {
  display: flex;
  gap: 8px;
  align-items: center;
}
.pill-pool {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}
.pill {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 4px 6px 4px 10px;
  font-size: 12px;
  border-radius: var(--r-pill, 999px);
  background: var(--accent-soft);
  border: 1px solid color-mix(in oklab, var(--accent) 30%, transparent);
  color: var(--accent-soft-foreground);
}
.pill-x {
  border: none;
  background: transparent;
  color: inherit;
  cursor: pointer;
  font-size: 14px;
  line-height: 1;
  padding: 0 2px;
  opacity: 0.7;
}
.pill-x:hover {
  opacity: 1;
}
.discover-list {
  margin-top: 8px;
  max-height: 300px;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 4px;
  border: 1px solid var(--border);
  border-radius: var(--r-thumb, 12px);
  padding: 6px;
}
.discover-item {
  display: flex;
  gap: 8px;
  align-items: center;
  justify-content: flex-start;
  text-align: left;
  background: transparent;
  border: 1px solid transparent;
  border-radius: var(--r-chip, 6px);
  padding: 6px 8px;
  font-size: 12px;
  cursor: pointer;
  color: var(--text-2, inherit);
}
.discover-item:hover {
  background: var(--default);
}
.discover-item.active {
  border-color: var(--accent);
  color: var(--text);
}
.di-check {
  width: 16px;
  text-align: center;
  color: var(--accent);
}
.di-cap {
  margin-left: auto;
  color: var(--text-4);
  white-space: nowrap;
}
</style>


