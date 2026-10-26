<script setup lang="ts">
/**
 * 单个上游的详情页（2026-10-17）。
 *
 * 为什么把它从 Providers.vue 里独立成一张路由页，而不是列表里的一行展开：
 * 十几个上游时，列表页要同时承担两件事 —— 一屏扫完摘要（余额、endpoint、
 * 模型数、启停），以及把某个上游的全部配置摊开。摊开的内容（凭据表、
 * 模型表、查余额脚本、代理、超时重试）体量大到会挤掉摘要，而两者的
 * 查阅频率差一个数量级：摘要每天扫一遍，配置只在要改时打开一次。
 *
 * 所以这里承接**全部**写操作（换钥、导入/删除模型、查余额、测试、改设置），
 * 列表页只保留读与两个轻量动作（编辑入口、启用开关）。没有第二处状态真相：
 * 列表页不做任何子资源请求。
 *
 * 状态从路由参数拿 provider id，因此刷新与分享链接都成立。
 */
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { toast, confirmBox } from '../ui'
import type {
  Provider,
  Credential,
  UpstreamModel,
  DiscoveredModel,
  ModelTestResult,
  ProviderBalance,
} from '../types'
import AppModal from '../components/AppModal.vue'
import CredentialPanel from '../components/CredentialPanel.vue'
import UpstreamModelPanel from '../components/UpstreamModelPanel.vue'
import { useNetworks, NETWORK_DIRECT_ID } from '../composables/useNetworks'
import { PROTOCOL_OPTIONS, protocolLabel } from '../protocols'
import { RequestGeneration } from '../asyncState'

const route = useRoute()
const router = useRouter()

const providerId = computed(() => String(route.params.id ?? ''))

const provider = ref<Provider | null>(null)
const notFound = ref(false)
const err = ref('')
const loading = ref(true)

const creds = ref<Credential[]>([])
const models = ref<UpstreamModel[]>([])
// 子资源各自的加载失败态。**不能**只弹 toast：失败时 creds/models 仍是空数组，
// 页面于是渲染「无凭据 —— 上游鉴权必需」「无模型 —— 路由必须指向一个上游模型」，
// 把一次网关不可达说成「配置真的没有了」，而空态与失败态的处置动作完全相反
// （一个要新增配置，一个要重试）。
const credsErr = ref('')
const modelsErr = ref('')

function is401(e: unknown): boolean {
  return (e as { status?: number }).status === 401
}

function is404(e: unknown): boolean {
  return (e as { status?: number }).status === 404
}

async function loadProvider() {
  const id = providerId.value
  try {
    const found = await api.provider(id)
    if (id !== providerId.value) return
    notFound.value = false
    provider.value = found
  } catch (e) {
    if (id !== providerId.value) return
    if (is404(e)) {
      notFound.value = true
      provider.value = null
      return
    }
    if (!is401(e)) err.value = '加载上游失败：' + (e as Error).message
  }
}

/** reloadSub 拉取该上游的凭据与模型。silent 时不弹 toast（后台刷新失败不该抢注意力）。 */
async function reloadSub(silent = false) {
  const id = providerId.value
	// 两个子资源各自成败互不牵连：凭据接口挂了不该把模型表也标成失败。
	const [c, m] = await Promise.allSettled([api.credentials(id), api.models(id)])
	if (id !== providerId.value) return
	if (c.status === 'fulfilled') {
		creds.value = c.value
		credsErr.value = ''
	} else if (!is401(c.reason)) {
		credsErr.value = '凭据加载失败：' + (c.reason as Error).message
		if (!silent) toast(credsErr.value, 'err')
	}
	if (m.status === 'fulfilled') {
		models.value = m.value
		modelsErr.value = ''
	} else if (!is401(m.reason)) {
		modelsErr.value = '模型加载失败：' + (m.reason as Error).message
		if (!silent) toast(modelsErr.value, 'err')
	}
}

let loadSeq = 0
async function load() {
  const seq = ++loadSeq
  loading.value = true
  err.value = ''
  await loadProvider()
  if (seq !== loadSeq) return
  if (!notFound.value) await reloadSub()
  if (seq === loadSeq) loading.value = false
}

// ---------- Provider 设置表单 ----------
// 编辑在这里是**页面内表单**而不是弹窗：代理与余额查询那两段说明文字很长，
// 弹窗会被撑成滚动条，且「改一个超时」和「读一遍查余额脚本」被关在同一个
// 遮罩里，翻查成本高于页面内展开。

const pForm = reactive({
  open: false,
  name: '',
  protocol: 'chat',
  endpoint: '',
  timeout_sec: 0,
  max_retries: 0,
  network_id: '',
  balance_spec_json: '',
  saving: false,
})

const { networks, load: loadNetworks } = useNetworks()

// network_id → 显示名。只在有引用的行上用到（直连那一支不调它），
// 查不到就退回短 id：那说明网络刚被删而快照还没重建，硬显示「直连」
// 会把「正在直连」与「引用已失效」混成同一个说法。
const networkLabel = (id: string): string => {
  const n = networks.value.find((x) => x.id === id)
  return n ? n.name : id.slice(0, 8)
}

function fmtTimeoutSec(ms: number): string {
  if (!ms) return '默认'
  const seconds = ms / 1000
  return `${Number.isInteger(seconds) ? seconds : seconds.toFixed(1)} 秒`
}

function fillForm(p: Provider) {
  pForm.name = p.name
  pForm.protocol = p.protocol || 'chat'
  pForm.endpoint = p.endpoint
  pForm.timeout_sec = p.timeout_ms / 1000
  pForm.max_retries = p.max_retries
  pForm.network_id = p.network_id || NETWORK_DIRECT_ID
  pForm.balance_spec_json = p.balance_spec_json
}

function isFormDirty(p: Provider): boolean {
  return (
    pForm.name !== p.name ||
    pForm.protocol !== (p.protocol || 'chat') ||
    pForm.endpoint !== p.endpoint ||
    pForm.timeout_sec !== p.timeout_ms / 1000 ||
    pForm.max_retries !== p.max_retries ||
    pForm.network_id !== (p.network_id || NETWORK_DIRECT_ID) ||
    pForm.balance_spec_json !== p.balance_spec_json
  )
}

async function saveProvider() {
  const p = provider.value
  if (!p) return
  if (!pForm.endpoint.trim()) {
    toast('Endpoint 为必填项', 'err')
    return
  }
  pForm.saving = true
  try {
    await api.updateProvider(p.id, {
      name: pForm.name.trim() || pForm.endpoint.trim(),
      protocol: pForm.protocol,
      endpoint: pForm.endpoint.trim(),
      timeout_ms: Math.max(0, Math.round((Number(pForm.timeout_sec) || 0) * 1000)),
      max_retries: Number(pForm.max_retries) || 0,
      // 网络与余额查询配置**每次都发**，空串 = 清空。省略字段的语义是
      // 「保持原值」，那会让「换回直连」「删掉自定义余额脚本」这两个动作
      // 永远发不出去 —— 必须显式把空串写回去。
      network_id: pForm.network_id,
      balance_spec_json: pForm.balance_spec_json.trim(),
    })
    toast('上游已更新')
    // 关闭弹窗在重载**之后**：loadProvider 会同步 provider，watch 里随之
    // 重新 fillForm。顺序反了的话表格会先按旧值填一次再跳一次。
    await loadProvider()
    pForm.open = false
  } catch (e) {
    if (!is401(e)) toast('保存失败：' + (e as Error).message, 'err')
  } finally {
    pForm.saving = false
  }
}

/**
 * openEdit 打开编辑弹窗。
 *
 * 每次都从当前 provider 重新填表：上一次取消时留下的半截输入不应该
 * 在下次打开时还在（那会让人以为「我刚才明明填了」）。
 */
/**
 * dirty 是弹窗里「保存」按钮的启用条件。
 *
 * 提成 computed 而不是模板里直接调 isFormDirty(provider)：provider 是
 * `Provider | null`，模板里的类型收窄不会跨过函数参数，vue-tsc 会报错。
 * 而弹窗只在 provider 存在时才会被打开（见 openEdit），所以 null 时
 * 一律按「不脏」处理 —— 按钮禁用，与实际行为一致。
 */
const dirty = computed(() => (provider.value ? isFormDirty(provider.value) : false))

function openEdit() {
  const p = provider.value
  if (!p) return
  fillForm(p)
  pForm.open = true
  void loadNetworks()
}

/**
 * closeEdit 关闭弹窗。
 *
 * 有未保存改动时二次确认：这一页的表单里有 endpoint 与代理 —— 误关掉
 * 一个改了一半的代理配置不致命，但「以为存上了、其实没有」才是真的坑，
 * 所以拦一下。
 */
async function closeEdit() {
  const p = provider.value
  if (p && isFormDirty(p)) {
    const discard = await confirmBox({
      title: '放弃未保存的改动？',
      body: '基础信息有未保存的修改，关闭后这些修改会丢失。',
      confirmLabel: '放弃改动',
    })
    if (!discard) return
  }
  pForm.open = false
}

async function toggleProvider() {
  const p = provider.value
  // 在途守卫：连点会发出 enable 再 disable，而两个响应各自提示一次成功，
  // 最终状态取决于返回顺序。
  if (!p || togglingProvider.value) return
  togglingProvider.value = true
  try {
    await api.updateProvider(p.id, { enabled: !p.enabled })
    toast(!p.enabled ? '已启用' : '已停用')
    await loadProvider()
  } catch (e) {
    if (!is401(e)) toast('操作失败：' + (e as Error).message, 'err')
  } finally {
    togglingProvider.value = false
  }
}

async function removeProvider() {
  const p = provider.value
  if (!p) return
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
    // 删掉当前页面的对象后没有可停留的地方，回列表。
    router.push({ name: 'providers', query: route.query })
  } catch (e) {
    if (!is401(e)) toast('删除失败：' + (e as Error).message, 'err')
  }
}

// ---------- 模型探测 ----------
// 客户端超时必须大于运行时的上游超时，否则后端还没报真实原因，浏览器
// 先掐成一句无信息的「请求超时」。设置读取失败时退回保守值。
const testTimeoutMs = ref(150_000 + 5_000)

const testing = ref(new Set<string>())

function isTesting(id: string): boolean {
  return testing.value.has(id)
}
function setTesting(id: string, on: boolean): void {
  const next = new Set(testing.value)
  if (on) next.add(id)
  else next.delete(id)
  testing.value = next
}

function modelTestToast(m: UpstreamModel, r: ModelTestResult): string {
  if (r.status === 'ok') {
    const parts = [`${m.model_id} · 正常`, `${r.latency_ms}ms`]
    if (r.output_tokens) parts.push(`输出 ${r.output_tokens} tok`)
    return parts.join(' · ')
  }
  const base = r.in_band ? `${r.message}（上游以 HTTP 200 返回错误）` : r.message
  return `${m.model_id} · 测试失败：${r.hint ? `${base} → ${r.hint}` : base}`
}

async function testModel(m: UpstreamModel) {
  if (isTesting(m.id)) return
  setTesting(m.id, true)
  try {
    const result = await api.testModel(m.id, testTimeoutMs.value)
    toast(modelTestToast(m, result), result.status === 'ok' ? 'ok' : 'err')
  } catch (e) {
    if (is401(e)) return
    toast(`${m.model_id} · 测试失败：${(e as Error).message}`, 'err')
  } finally {
    setTesting(m.id, false)
  }
}

// ---------- 凭据 ----------

const cForm = reactive({ open: false, editing: '' as string, label: '', api_key: '' })

function openCred(c?: Credential) {
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
      await api.updateCredential(cForm.editing, {
        label: cForm.label.trim(),
        ...(cForm.api_key ? { api_key: cForm.api_key } : {}),
      })
    } else {
      await api.createCredential(providerId.value, {
        label: cForm.label.trim(),
        api_key: cForm.api_key,
      })
    }
    cForm.open = false
    toast(cForm.editing ? '凭据已更新' : '凭据已创建')
    await Promise.all([reloadSub(), loadProvider()])
  } catch (e) {
    if (!is401(e)) toast('保存失败：' + (e as Error).message, 'err')
  }
}

async function removeCred(c: Credential) {
  const ok = await confirmBox({
    title: `删除凭据「${c.label || c.id}」？`,
    body: '删除后该上游可能变为不可用（若已无其他启用的凭据），指向它的路由会失败转移。',
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return
  try {
    await api.deleteCredential(c.id)
    toast('已删除')
    await reloadSub()
  } catch (e) {
    if (!is401(e)) toast('删除失败：' + (e as Error).message, 'err')
  }
}

const credToggling = ref(new Set<string>())
const togglingProvider = ref(false)
const credTogglingIds = computed(() => [...credToggling.value])

function isCredToggling(id: string): boolean {
  return credToggling.value.has(id)
}
function setCredToggling(id: string, on: boolean): void {
  const next = new Set(credToggling.value)
  if (on) next.add(id)
  else next.delete(id)
  credToggling.value = next
}

async function toggleCred(c: Credential) {
  if (isCredToggling(c.id)) return
  const turningOn = !c.enabled
  // 启是「切换」语义，连点两次的结果与点一次完全相反，且期间池真的换过 key。
  // 先数一遍本来开着的是谁：这次请求会把它们一并关掉，而响应只描述这一把。
  const previouslyOn = creds.value.filter((x) => x.enabled && x.id !== c.id)

  setCredToggling(c.id, true)
  try {
    await api.updateCredential(c.id, { enabled: turningOn })
    if (turningOn && previouslyOn.length > 0) {
      toast(`已切到「${c.label || c.id}」，原启用的 ${previouslyOn.length} 把已停用`)
    } else {
      toast(turningOn ? '已启用' : '已停用')
    }
    // 必须整表重载：同 provider 的其它行状态也变了，而 PATCH 响应只带这一把。
    await reloadSub()
  } catch (e) {
    if (!is401(e)) toast('操作失败：' + (e as Error).message, 'err')
  } finally {
    setCredToggling(c.id, false)
  }
}

// ---------- 余额 ----------
// 点按查询，不是轮询快照：这是一个会被据此决定「要不要充值」的数字，
// 可能显示十分钟前的值比不显示更糟。所以只在点时取，并把取数时刻一起显示。
const balance = ref<ProviderBalance | undefined>(undefined)
const balanceBusy = ref(false)
const balanceRequests = new RequestGeneration()

async function queryBalance() {
  if (balanceBusy.value) return
  const id = providerId.value
  const generation = balanceRequests.begin()
  balanceBusy.value = true
  try {
    const result = await api.providerBalance(id)
    if (!balanceRequests.isCurrent(generation) || id !== providerId.value || result.provider_id !== id) return
    balance.value = result
  } catch (e) {
    if (is401(e)) return
    if (!balanceRequests.isCurrent(generation) || id !== providerId.value) return
    // 管理接口失败与「上游余额查不到」是两件事：后者在响应里是
    // status=error。分开显示，否则管理员会去换 key 而真正该做的是重试。
    balance.value = {
      provider_id: id,
      provider: '',
      status: 'error',
      message: '管理接口调用失败：' + (e as Error).message,
      results: [],
    }
  } finally {
    if (balanceRequests.isCurrent(generation)) balanceBusy.value = false
  }
}

function credentialBalanceFor(credentialId: string) {
  const current = provider.value
  const result = balance.value
  if (!current || !result || result.provider_id !== current.id) return undefined
  return result.results.find((x) => x.credential_id === credentialId)
}

const balanceSpecConfigured = computed(() => !!provider.value?.balance_spec_json?.trim())

// ---------- 模型表单 ----------

const mForm = reactive({
  open: false,
  editing: '' as string,
  model_id: '',
  context_window: 0,
  max_output_tokens: 0,
  price_input: 0,
  price_cache_hit: 0,
  price_cache_creation: 0,
  price_output: 0,
  effortCsv: '',
  thinking: 'unset' as 'unset' | 'yes' | 'no',
  saving: false,
})

const knownEffortLevels = ['minimal', 'low', 'medium', 'high', 'xhigh', 'max'] as const

const thinkingOptions = [
  { v: 'unset' as const, label: '未配置' },
  { v: 'yes' as const, label: '支持' },
  { v: 'no' as const, label: '不支持' },
]

const discovering = ref(false)
const discovered = ref<DiscoveredModel[]>([])
const discoverMsg = ref('')
const importing = ref(false)
const filter = ref('')
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

function openModel(m?: UpstreamModel) {
  mForm.editing = m?.id ?? ''
  mForm.model_id = m?.model_id ?? ''
  mForm.context_window = m?.context_window ?? 0
  mForm.max_output_tokens = m?.max_output_tokens ?? 0
  mForm.price_input = m?.price_input ?? 0
  mForm.price_cache_hit = m?.price_cache_hit ?? 0
  mForm.price_cache_creation = m?.price_cache_creation ?? 0
  mForm.price_output = m?.price_output ?? 0
  mForm.saving = false
  // unknown 档位附在后面：它们是网关不认、但可能是上游认得的厂商私有值，
  // 编辑时必须原样带回去，否则点一次保存就把它们悄悄抹掉了。
  mForm.effortCsv = [...(m?.effort_levels ?? []), ...(m?.unknown_effort_levels ?? [])].join(',')
  mForm.thinking =
    m?.supports_thinking === undefined ? 'unset' : m.supports_thinking ? 'yes' : 'no'
  discovered.value = []
  selected.value = []
  discoverMsg.value = ''
  filter.value = ''
  mForm.open = true
}

function hasEffortLevel(l: string): boolean {
  return mForm.effortCsv.split(',').some((x) => x.trim() === l)
}

// 用集合而不是字符串拼装算勾选态：CSV 是有序文本，includes 会把「xhigh」
// 误判为含「high」（子串包含），那是会点错的。
function toggleEffortLevel(l: string) {
  const parts = mForm.effortCsv.split(',').map((x) => x.trim()).filter(Boolean)
  const i = parts.indexOf(l)
  if (i >= 0) parts.splice(i, 1)
  else parts.push(l)
  const rank = (x: string) => {
    const i = knownEffortLevels.indexOf(x as (typeof knownEffortLevels)[number])
    return i < 0 ? knownEffortLevels.length : i
  }
  parts.sort((a, b) => rank(a) - rank(b))
  mForm.effortCsv = parts.join(',')
}

// 明确「不支持思考」时清空挡位；反过来不成立（「支持」不必配挡位）。
// 否则界面上会出现「不支持思考，却配了 high/xhigh」，而其后果是档位仍被
// 当成有效能力披露给 /v1/models，客户端以为能选、选了没效果。
function setThinking(v: 'unset' | 'yes' | 'no') {
  mForm.thinking = v
  if (v === 'no') mForm.effortCsv = ''
}

async function submitModel() {
  if (mForm.saving) return
  if (!mForm.editing) return
  if (!mForm.model_id.trim()) {
    toast('上游模型名（model_id）为必填项', 'err')
    return
  }
  const prices = [mForm.price_input, mForm.price_cache_hit, mForm.price_cache_creation, mForm.price_output].map(Number)
  if (prices.some((v) => !Number.isFinite(v) || v < 0)) {
    toast('单价必须是大于等于 0 的数字', 'err')
    return
  }
  mForm.saving = true
  try {
    const original = models.value.find((m) => m.id === mForm.editing)
    if (original && [original.price_input, original.price_cache_hit, original.price_cache_creation, original.price_output].some((v) => v > 0) && prices.every((v) => v === 0)) {
      const ok = await confirmBox({
        title: '清除该模型的全部价格？',
        body: '之后的调用将按 0 元计费，历史费用不变。',
        confirmLabel: '清除价格并保存',
      })
      if (!ok) return
    }
    await api.updateModel(mForm.editing, {
      model_id: mForm.model_id.trim(),
      context_window: Number(mForm.context_window) || 0,
      max_output_tokens: Number(mForm.max_output_tokens) || 0,
      price_input: prices[0],
      price_cache_hit: prices[1],
      price_cache_creation: prices[2],
      price_output: prices[3],
      // 空串 = 清除（回到未配置），与界面上「一个都不勾选」是同一个动作。
      effort_levels: mForm.effortCsv,
      // 三态映射：unset → null（后端解成「清除」）。不能省略字段 ——
      // 省略是「不改」，「切回未配置并保存」必须真的写下去。
      supports_thinking: mForm.thinking === 'unset' ? null : mForm.thinking === 'yes',
    })
    mForm.open = false
    toast('模型已更新')
    await reloadSub()
  } catch (e) {
    if (!is401(e)) toast('保存失败：' + (e as Error).message, 'err')
  } finally {
    mForm.saving = false
  }
}

async function runDiscover() {
  discovering.value = true
  discovered.value = []
  selected.value = []
  discoverMsg.value = ''
  try {
    const r = await api.discoverModels(providerId.value)
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
      providerId.value,
      list.map((d) => ({
        model_id: d.model_id,
        display_name: d.display_name ?? '',
        context_window: d.context_window ?? 0,
        max_output_tokens: d.max_output_tokens ?? 0,
        // 上游自报的思考挡位原样透传：闸门在后端（空串 = 上游没报 =
        // 留空给管理员手工配），前端再判一次只会出现两套口径。
        // supports_thinking 用 ?? 而不是 ||：false 是有意义的值
        // （「上游明确说不支持」），|| 会把它抹掉。
        effort_levels: d.effort_levels ?? '',
        supports_thinking: d.supports_thinking ?? undefined,
      })),
    )
    toast(`已添加 ${r.imported} 个模型`)
    mForm.open = false
    // 卡片上的模型数来自列表接口的 model_count，所以必须刷新 provider，
    // 否则删一个模型后这页的计数会停在旧值。
    await Promise.all([reloadSub(), loadProvider()])
  } catch (e) {
    if (!is401(e)) toast('添加失败：' + (e as Error).message, 'err')
  } finally {
    importing.value = false
  }
}

async function removeModel(m: UpstreamModel) {
  if (m.is_route_head) {
    toast('该模型是某条路由的首选目标，不能直接删除 —— 请先到「路由」页改掉那条路由的首选', 'err')
    return
  }
  const refs = m.route_count ?? 0
  const ok = await confirmBox({
    title: `删除模型「${m.model_id}」？`,
    body:
      refs > 0
        ? `它被 ${refs} 条路由链作为非首选目标引用 —— 删除后会自动从这些链中移除（其余成员顺序不变），该路由的首选目标不受影响。`
        : '它没有被任何路由链引用，可以直接删除。',
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return
  try {
    await api.deleteModel(m.id)
    toast('已删除')
    await Promise.all([reloadSub(), loadProvider()])
  } catch (e) {
    if (!is401(e)) toast('删除失败：' + (e as Error).message, 'err')
  }
}

onMounted(() => {
  load()
  // 只取设置里的上游超时这一个字段；拉不到就保持保守值，不阻塞页面。
  api
    .settings()
    .then((s) => {
      if (s.upstream_timeout_ms > 0) testTimeoutMs.value = s.upstream_timeout_ms + 5_000
    })
    .catch(() => {
      /* 保持 150s 保守值 */
    })
})

// 换 URL 里的 id 时重载：详情页可以被直接链接到任意上游，也可以在本页
// 与列表之间来回跳。不监听的话，上一次的数据会留在页面上配到另一个 id。
watch(providerId, () => {
  balanceRequests.invalidate()
  balance.value = undefined
  balanceBusy.value = false
  mForm.open = false
  pForm.open = false
  cForm.open = false
  models.value = []
  creds.value = []
  provider.value = null
  load()
})

watch(provider, (p) => {
  if (p) fillForm(p)
})

// 从路由详情跳转时可携带具体 upstream_models.id，直接打开对应模型编辑。
// 消费掉 query，避免刷新或返回时重复弹出同一个编辑框。
watch(
  [models, () => route.query.model],
  ([list, target]) => {
    if (typeof target !== 'string' || !target || !list.length) return
    const model = list.find((m) => m.id === target && m.provider_id === providerId.value)
    if (!model) return
    openModel(model)
    void router.replace({ query: { ...route.query, model: undefined } })
  },
  { immediate: true },
)

function goBack() {
  router.push({ name: 'providers', query: route.query })
}
</script>

<template>
  <!-- 用全局的 .page / .page-head（与其余 10 个页面同一套外壳），而不是本页
       自造一个 padding —— 自造外壳会让这一页的标题区、留白、最大宽度都
       与其它页面不一致，一眼就能看出「这页是另做的」。 -->
  <div class="page">
    <div class="page-head">
      <div>
        <h1>{{ provider?.name ?? '上游详情' }}</h1>
        <div class="sub">
          <RouterLink :to="{ name: 'providers', query: route.query }" class="lnk">← 上游服务</RouterLink>
        </div>
      </div>
      <div class="head-actions">
        <button class="btn" @click="load">刷新</button>
      </div>
    </div>

    <div v-if="loading" class="panel">
      <div class="loading">加载中…</div>
    </div>

    <div v-else-if="notFound" class="panel">
      <div class="empty">
        <div class="big">∅</div>
        该上游不存在（可能已被删除）
        <div style="margin-top: 12px">
          <button class="btn btn-sm" @click="goBack">返回列表</button>
        </div>
      </div>
    </div>

    <div v-else-if="err" class="panel">
      <div class="empty"><div class="big">⚠</div>{{ err }}</div>
    </div>

    <template v-else-if="provider">
      <div v-if="provider.enabled && provider.ready !== true" class="notice-warn" role="status">
        <strong>{{ provider.ready === false ? '未就绪' : '状态未知' }}</strong> · {{ provider.ready_reason || (provider.ready === false ? '未建立可用凭据池，请检查下方凭据配置' : '暂未取得运行时状态') }}
      </div>
      <!-- 概览与凭据并排，模型表单独占满下一行。

           为什么这么分（2026-10-17 调整）：上一版是「概览一整行 + 凭据/模型
           并排」，可概览只有六行只读键值、且右侧空着，而凭据表只有三列、
           天然适合窄栏。两块高度都远小于一整行，并排后首屏能同时看到
           「这个上游是谁」和「它现在用哪把 key」—— 排查「这个上游怎么不
           通」正是从这两处一起看的。

           模型表放下一行占满宽度：它有五列，其中四列是 nowrap（三个
           num-h + 操作列），挤在半屏宽里既压缩模型名又要横向滚动。
           它也是这一页行数最多的块（可能几十个模型），本来就该占满。 -->
      <div class="expand-grid">
        <div class="expand-col">
          <!-- 概览：默认只读，只给一行摘要 + 「编辑」。

               标题**不重复**写一遍名字 —— page-head 的 h1 已经是这一页的
               主标题，面板里再写一次会在同一屏里出现两个同样大的名字。

               为什么默认收起表单：基础配置读的时候几乎不看 —— 超时/重试/
               slug 是「坏了才回头查」的字段。常驻显示只会把真正要看的
               凭据与模型挤出首屏。改配置是低频动作，且「编辑基础信息」
               在标题区，位置固定。 -->
          <div class="card ov-card">
            <div class="card__header ov-head">
              <div class="card__title">概览</div>
              <div class="ov-head-actions">
                <button class="btn btn-sm" @click="openEdit">编辑</button>
                <button class="btn btn-sm btn-danger" @click="removeProvider">删除</button>
                <button
                  class="switch"
                  :class="{ on: provider.enabled }"
                  role="switch"
                  :aria-checked="provider.enabled"
                  :aria-label="(provider.enabled ? '停用 ' : '启用 ') + (provider.name || provider.slug)"
                  :disabled="togglingProvider"
                  :title="provider.enabled ? '停用' : '启用'"
                  @click="toggleProvider"
                ></button>
              </div>
            </div>
            <div class="card__content">
              <!-- 键值表：标签固定宽、值 flex:1 并省略。全量值在 title 里，
                   且「编辑基础信息」里能直接看到完整原文。 -->
              <div class="kv">
                <div class="kv-row"><span class="kv-k">类型</span><span class="kv-v"><span class="badge">{{ protocolLabel(provider.protocol) }}</span></span></div>
                <div class="kv-row"><span class="kv-k">Endpoint</span><span class="kv-v mono kv-ep" :title="provider.endpoint">{{ provider.endpoint }}</span></div>
                <div class="kv-row">
                  <span class="kv-k">代理</span>
                  <span class="kv-v dim" :title="provider.proxy_url || '直连（不经过代理）'">
                    <template v-if="provider.network_id">
                      {{ networkLabel(provider.network_id) }}
                      <span class="mono kv-sub">（{{ provider.proxy_url }}）</span>
                    </template>
                    <template v-else>直连（不经过代理）</template>
                  </span>
                </div>
                <div class="kv-row"><span class="kv-k">Slug</span><span class="kv-v mono dim">{{ provider.slug }}</span></div>
                <div class="kv-row">
                  <span class="kv-k">超时 / 重试</span>
                  <span class="kv-v dim">
                    {{ fmtTimeoutSec(provider.timeout_ms) }} /
                    {{ provider.max_retries ? `${provider.max_retries} 次` : '默认' }}
                  </span>
                </div>
                <div class="kv-row">
                  <span class="kv-k">余额查询</span>
                  <span class="kv-v">
                    <span v-if="provider.balance_spec_json?.trim()" class="badge badge-accent">自定义</span>
                    <span v-else-if="balanceSpecConfigured" class="badge badge-accent">内置</span>
                    <span v-else class="dim">未配置</span>
                  </span>
                </div>
              </div>
            </div>
          </div>
        </div>

        <div class="expand-col">
          <!-- 余额刻意**不**单独成块：它本来就逐凭据显示（同一把 key
               可能只剩一把有余额），汇总成一个数字反而要自己对应是哪把。 -->
          <p v-if="credsErr" class="notice-warn" role="alert">
            {{ credsErr }}
            <button class="btn btn-sm" @click="reloadSub(true)">重试</button>
          </p>
          <CredentialPanel
            :provider-id="provider.id"
            :creds="creds"
            :balance="balance"
            :balance-busy="balanceBusy"
            :toggling="credTogglingIds"
            :balance-spec-configured="balanceSpecConfigured"
            @add="openCred()"
            @query-balance="queryBalance"
            @edit="(c) => openCred(c)"
            @remove="removeCred"
            @toggle="toggleCred"/>
        </div>
      </div>

      <!-- 模型表：整行占满宽度。见上面的说明（五列、四列 nowrap、
           且可能是这一页行数最多的块）。 -->
      <div class="models-card">
        <p v-if="modelsErr" class="notice-warn" role="alert">
          {{ modelsErr }}
          <button class="btn btn-sm" @click="reloadSub(true)">重试</button>
        </p>
        <UpstreamModelPanel
          :models="models"
          :testing="testing"
          @add="openModel()"
          @test="testModel"
          @edit="(m) => openModel(m)"
          @remove="removeModel"/>
      </div>
    </template>

    <!-- 基础信息编辑：弹窗而不是页面内表单。
         这一版的取舍与上一版相反 —— 代理与余额查询那两段说明文字确实很长，
         但它们是**修改时才需要读**的说明，常驻在页面里只会把凭据与模型
         往下挤。弹窗把「读摘要」与「改配置」这两件事彻底分开：默认不读，
         要改时一次性摊开全部字段。 -->
    <AppModal
      :open="pForm.open"
      title="编辑基础信息"
      max-width="560px"
      :dismissable="false"
    >
      <form @submit.prevent="saveProvider">
        <div class="form-grid">
          <div class="field">
            <label for="cgw-lProviderDetail-1">显示名称</label>
            <input v-model="pForm.name" class="input" placeholder="DeepSeek" id="cgw-lProviderDetail-1"/>
          </div>
          <div class="field">
            <label for="cgw-lProviderDetail-2">类型</label>
            <select v-model="pForm.protocol" class="select" id="cgw-lProviderDetail-2">
              <option v-for="option in PROTOCOL_OPTIONS" :key="option.value" :value="option.value">{{ option.label }}</option>
            </select>
          </div>
          <div class="field span2">
            <label for="cgw-lProviderDetail-3">Endpoint *</label>
            <input v-model="pForm.endpoint" class="input mono" placeholder="https://api.deepseek.com" id="cgw-lProviderDetail-3"/>
          </div>
          <div class="field">
            <label for="cgw-lProviderDetail-4">超时（秒）</label>
            <input v-model.number="pForm.timeout_sec" class="input num" type="number" min="0" step="0.1" id="cgw-lProviderDetail-4"/>
          </div>
          <div class="field">
            <label for="cgw-lProviderDetail-5">最大重试次数</label>
            <input v-model.number="pForm.max_retries" class="input num" type="number" min="0" max="10" step="1" id="cgw-lProviderDetail-5"/>
          </div>

          <div class="field span2">
            <label for="cgw-lProviderDetail-6">代理</label>
            <select v-model="pForm.network_id" class="select" id="cgw-lProviderDetail-6">
              <option :value="NETWORK_DIRECT_ID">直连（不经过代理）</option>
              <option v-for="n in networks" :key="n.id" :value="n.id">{{ n.name }} — {{ n.proxy_url }}</option>
            </select>
          </div>

          <div class="field span2">
            <label for="cgw-lProviderDetail-7">余额查询（可选）</label>
            <textarea
              v-model="pForm.balance_spec_json"
              class="input mono"
              rows="2"
              spellcheck="false"
              placeholder='{"path":"/credits","field":"data.remaining","currency":"USD"}' id="cgw-lProviderDetail-7"></textarea>
          </div>
        </div>
        <div class="form-actions">
          <button
            type="button"
            class="btn btn-ghost"
            :disabled="pForm.saving"
            @click="closeEdit"
          >
            取消
          </button>
          <button
            type="submit"
            class="btn btn-primary"
            :disabled="pForm.saving || !dirty"
          >
            {{ pForm.saving ? '保存中…' : '保存' }}
          </button>
        </div>
      </form>
    </AppModal>

    <!-- 凭据表单 -->
    <AppModal
      :open="cForm.open"
      :title="cForm.editing ? '更新凭据' : '添加凭据'"
      max-width="480px"
      :dismissable="false"
    >
      <form @submit.prevent="submitCred">
        <div class="form-grid">
          <div class="field span2">
            <label for="cgw-lProviderDetail-9">标签</label>
            <input v-model="cForm.label" class="input" placeholder="主账号 / 备用池" id="cgw-lProviderDetail-9"/>
          </div>
          <div class="field span2">
            <label for="cgw-lProviderDetail-10">API Key {{ cForm.editing ? '（留空则不更换）' : '*' }}</label>
            <input v-model="cForm.api_key" class="input mono" type="password" placeholder="sk-..." autocomplete="new-password" id="cgw-lProviderDetail-10"/>
            <span class="tip">保存后加密入库，仅显示掩码，不再可见明文</span>
          </div>
          <div v-if="!cForm.editing" class="field span2">
            <span class="tip">
              同一上游同时只启用一把 key：若该上游已有启用的凭据，新建的这把会先以
              <strong>停用</strong>状态保存 —— 确认可用后在凭据列表里打开它的开关即可切换。
            </span>
          </div>
        </div>
        <div class="form-actions">
          <button type="button" class="btn btn-ghost" @click="cForm.open = false">取消</button>
          <button type="submit" class="btn btn-primary">保存</button>
        </div>
      </form>
    </AppModal>

    <!-- 模型：编辑单条 -->
    <AppModal
      v-if="mForm.editing"
      :open="mForm.open"
      title="编辑上游模型"
      max-width="500px"
      :dismissable="false"
    >
      <form @submit.prevent="submitModel">
        <fieldset class="model-edit-fields" :disabled="mForm.saving">
        <div class="form-grid">
          <div class="field span2">
            <label for="cgw-lProviderDetail-11">上游模型名（model_id）*</label>
            <input v-model="mForm.model_id" class="input mono" placeholder="deepseek-chat" id="cgw-lProviderDetail-11"/>
          </div>
          <div class="field">
            <label for="cgw-lProviderDetail-12">上下文窗口</label>
            <input v-model.number="mForm.context_window" class="input num" type="number" min="0" step="1" placeholder="0 = 未设置" id="cgw-lProviderDetail-12"/>
          </div>
          <div class="field">
            <label for="cgw-lProviderDetail-13">最大输出</label>
            <input v-model.number="mForm.max_output_tokens" class="input num" type="number" min="0" step="1" placeholder="0 = 未设置" id="cgw-lProviderDetail-13"/>
          </div>
          <div class="field span2">
            <span class="field-label">单价（元 / 1M tokens）</span>
            <div class="price-edit-grid">
              <label><span>输入</span><input v-model.number="mForm.price_input" class="input num" type="number" min="0" step="any" placeholder="未配置"/></label>
              <label><span>缓存命中</span><input v-model.number="mForm.price_cache_hit" class="input num" type="number" min="0" step="any" placeholder="未配置"/></label>
              <label><span>缓存写入</span><input v-model.number="mForm.price_cache_creation" class="input num" type="number" min="0" step="any" placeholder="未配置"/></label>
              <label><span>输出</span><input v-model.number="mForm.price_output" class="input num" type="number" min="0" step="any" placeholder="未配置"/></label>
            </div>
          </div>
          <div class="field span2">
            <span class="field-label">思考模式</span>
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
          </div>
          <div v-if="mForm.thinking !== 'no'" class="field span2">
            <span class="field-label">思考挡位</span>
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
          </div>
        </div>
        </fieldset>
        <div class="form-actions">
          <button type="button" class="btn btn-ghost" :disabled="mForm.saving" @click="mForm.open = false">取消</button>
          <button type="submit" class="btn btn-primary" :disabled="mForm.saving">{{ mForm.saving ? '保存中…' : '保存' }}</button>
        </div>
      </form>
    </AppModal>

    <!-- 模型：探测 + 多选胶囊式添加 -->
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
            style="flex: 1"/>
        </div>
        <span v-if="discoverMsg" class="tip" style="color: var(--danger)">{{ discoverMsg }}</span>

        <div v-if="selected.length" class="pill-pool">
          <span v-for="s in selected" :key="s.model_id" class="pill">
            <span class="mono">{{ s.model_id }}</span>
            <button type="button" class="pill-x" title="移除" @click="removeSelected(s.model_id)">×</button>
          </span>
        </div>

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
            <span v-if="d.effort_levels" class="badge badge-accent di-lvls">思考 {{ d.effort_levels }}</span>
            <span v-else class="badge di-lvls dim-lvls">思考 待配置</span>
            <span class="di-cap">{{ d.context_window ? d.context_window + ' ctx' : '' }}{{ d.context_window && d.max_output_tokens ? ' / ' : '' }}{{ d.max_output_tokens ? d.max_output_tokens + ' out' : '' }}</span>
          </button>
        </div>
        <div v-else-if="!discovering && !discoverMsg" class="empty" style="padding: 18px 0">
          点击「获取模型列表」拉取上游可用模型
        </div>
      </div>
      <div class="form-actions">
        <button type="button" class="btn btn-ghost" @click="mForm.open = false">取消</button>
        <button type="button" class="btn btn-primary" :disabled="!selected.length || importing" @click="confirmImport">
          {{ importing ? '添加中…' : `添加（${selected.length}）` }}
        </button>
      </div>
    </AppModal>
  </div>
</template>

<style scoped>
/* 外壳（.page / .page-head / .panel）全部来自全局 styles.css，本页只定义
   页内布局。 */
/* 概览里的键值摘要：标签固定宽 84px，值 flex:1 并在超长时省略。
   固定宽是这一排的读法所在 —— 六个字段的标签左对齐成一条竖线，
   眼睛才能横着扫下来；标签宽度随内容变的话每行都会错开。
   完整值在 title 里，改值走「编辑基础信息」。 */
.kv { display: flex; flex-direction: column; }
.price-edit-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 8px; margin-top: 8px; }
.model-edit-fields { border: 0; margin: 0; padding: 0; min-width: 0; }
.price-edit-grid label { display: grid; gap: 6px; font-size: 11px; color: var(--muted); }
@media (max-width: 560px) { .price-edit-grid { grid-template-columns: 1fr; } }
.kv-row {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 7px 2px;
  border-bottom: 1px solid var(--separator);
  min-width: 0;
}
.kv-row:last-child { border-bottom: none; }
.kv-k {
  flex: none;
  width: 84px;
  font-size: 12.5px;
  color: var(--muted);
}
.kv-v {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 13px;
}
/* Endpoint 例外：URL 的可辨识信息在路径尾巴上，掐尾省略（…）会把
   「chat/completions」这半截丢掉。所以换行而不是省略，完整可见。 */
.kv-ep { white-space: normal; word-break: break-all; }

/* 概览「出站网络」那一行：网络名是主体，地址是括注。地址比名字长得多，
   不压成 dim 括注的话，这一行会被地址顶掉而读不出「走的是哪条网络」。 */
.kv-sub { font-size: 12px; color: var(--muted); }

/* 两列并排的「概览 / 凭据」，下方模型表整行占满。沿用全局 .expand-grid。
   窄屏塌成一列：两张表挤在半屏宽时，模型名会被压到只剩省略号，
   而那一列是这一页唯一需要整段读的内容。 */
.expand-grid { margin-bottom: 18px; }
@media (max-width: 1000px) {
  .expand-grid { grid-template-columns: 1fr; gap: 18px; }
}

/* 概览卡：与并排的凭据卡**等高**。网格项默认 stretch，但那只作用于
   .expand-col 这一层 —— 它里面的 .card 还要自己撑满（height:100%），
   否则六行键值表会把卡片撑得比旁边的凭据卡高，而凭据卡顶部对齐、
   底部留一截空白，看起来像没对齐。卡内是 flex column 且 .card__content
   带 flex:1，所以多出来的空间落在键值表下方而不是动作行上。 */
.expand-col > .card { height: 100%; }
.ov-card { gap: 10px; }
.ov-head { flex-direction: row; align-items: center; justify-content: space-between; gap: 12px; }
.ov-head-actions { display: flex; align-items: center; justify-content: flex-end; gap: 8px; }

/* 模型卡：整行宽度，不参与两列网格。去掉 .card 自带的 padding ——
   UpstreamModelPanel 本身就是一个 .card，套两层会多出一圈内边距
   和两层圆角，看起来像「卡里还有一张卡」。 */
.models-card { padding: 0; background: transparent; box-shadow: none; }

.effort-picker, .tri-switch { display: flex; flex-wrap: wrap; gap: 6px; margin-bottom: 4px; }
.add-model { display: flex; flex-direction: column; gap: 10px; }
.am-toolbar { display: flex; gap: 8px; align-items: center; }
.pill-pool { display: flex; flex-wrap: wrap; gap: 6px; }
.pill {
  display: inline-flex; align-items: center; gap: 6px;
  padding: 4px 6px 4px 10px; font-size: 12px;
  border-radius: var(--r-pill, 999px);
  background: var(--accent-soft);
  border: 1px solid color-mix(in oklab, var(--accent) 30%, transparent);
  color: var(--accent-soft-foreground);
}
.pill-x { border: none; background: transparent; color: inherit; cursor: pointer; font-size: 14px; line-height: 1; padding: 0 2px; opacity: 0.7; }
.pill-x:hover { opacity: 1; }
.discover-list {
  margin-top: 8px; max-height: 300px; overflow-y: auto;
  display: flex; flex-direction: column; gap: 4px;
  border: 1px solid var(--border);
  border-radius: var(--r-thumb, 12px);
  padding: 6px;
}
.discover-item {
  display: flex; gap: 8px; align-items: center; justify-content: flex-start;
  text-align: left; background: transparent; border: 1px solid transparent;
  border-radius: var(--r-chip, 6px); padding: 6px 8px; font-size: 12px;
  cursor: pointer; color: var(--text-2, inherit);
}
.discover-item:hover { background: var(--default); }
.discover-item.active { border-color: var(--accent); color: var(--text); }
.di-check { width: 16px; text-align: center; color: var(--accent); }
.di-lvls {
  flex: none; font-weight: 500; margin-right: 2px; max-width: 220px;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.dim-lvls { background: var(--default); color: var(--text-3); font-weight: 400; }
.di-cap { margin-left: auto; color: var(--text-4); white-space: nowrap; }
</style>
