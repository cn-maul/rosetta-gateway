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
import { useRoute, useRouter } from 'vue-router'
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

const route = useRoute()
const router = useRouter()

const providerId = computed(() => String(route.params.id ?? ''))

const provider = ref<Provider | null>(null)
const notFound = ref(false)
const err = ref('')
const loading = ref(true)

const creds = ref<Credential[]>([])
const models = ref<UpstreamModel[]>([])

function is401(e: unknown): boolean {
  return (e as { status?: number }).status === 401
}

async function loadProvider() {
  const id = providerId.value
  try {
    const all = await api.providers()
    // 列表接口是唯一按 id 取单个 provider 的入口（后端没有 GET
    // /providers/{id}）。十几个上游一次全取的成本可以接受，且顺带保证
    // 这里的 ready/enable 状态与列表页看到的是同一份数据。
    if (id !== providerId.value) return
    const found = all.find((p) => p.id === id)
    if (!found) {
      notFound.value = true
      provider.value = null
      return
    }
    notFound.value = false
    provider.value = found
  } catch (e) {
    if (id !== providerId.value) return
    if (!is401(e)) err.value = '加载上游失败：' + (e as Error).message
  }
}

/** reloadSub 拉取该上游的凭据与模型。silent 时不弹 toast（后台刷新失败不该抢注意力）。 */
async function reloadSub(silent = false) {
  const id = providerId.value
  try {
    const [c, m] = await Promise.all([api.credentials(id), api.models(id)])
    if (id !== providerId.value) return
    creds.value = c
    models.value = m
  } catch (e) {
    if (id !== providerId.value) return
    if (!is401(e) && !silent) toast('加载子资源失败：' + (e as Error).message, 'err')
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
  protocol: 'openai-chat',
  endpoint: '',
  slug: '',
  timeout_ms: 0,
  max_retries: 0,
  proxy_url: '',
  balance_spec_json: '',
  saving: false,
})

function fillForm(p: Provider) {
  pForm.name = p.name
  pForm.protocol = p.protocol || 'openai-chat'
  pForm.endpoint = p.endpoint
  pForm.slug = p.slug
  pForm.timeout_ms = p.timeout_ms
  pForm.max_retries = p.max_retries
  pForm.proxy_url = p.proxy_url
  pForm.balance_spec_json = p.balance_spec_json
}

function isFormDirty(p: Provider): boolean {
  return (
    pForm.name !== p.name ||
    pForm.protocol !== (p.protocol || 'openai-chat') ||
    pForm.endpoint !== p.endpoint ||
    pForm.timeout_ms !== p.timeout_ms ||
    pForm.max_retries !== p.max_retries ||
    pForm.proxy_url !== p.proxy_url ||
    pForm.balance_spec_json !== p.balance_spec_json
  )
}

const openRouterSpec =
  '{"path":"/api/v1/credits","field":"data.total_credits","currency":"USD"}'

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
      timeout_ms: Number(pForm.timeout_ms) || 0,
      max_retries: Number(pForm.max_retries) || 0,
      // 代理与余额查询配置**每次都发**，空串 = 清空。省略字段的语义是
      // 「保持原值」，那会让「卸掉代理」「删掉自定义余额脚本」这两个动作
      // 永远发不出去 —— 必须显式把空串写回去。
      proxy_url: pForm.proxy_url.trim(),
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
  if (!p) return
  try {
    await api.updateProvider(p.id, { enabled: !p.enabled })
    toast(!p.enabled ? '已启用' : '已停用')
    await loadProvider()
  } catch (e) {
    if (!is401(e)) toast('操作失败：' + (e as Error).message, 'err')
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

const modelTestResults = reactive<Record<string, ModelTestResult>>({})

async function testModel(m: UpstreamModel) {
  if (isTesting(m.id)) return
  setTesting(m.id, true)
  delete modelTestResults[m.id]
  try {
    modelTestResults[m.id] = await api.testModel(m.id, testTimeoutMs.value)
  } catch (e) {
    if (is401(e)) return
    modelTestResults[m.id] = {
      status: 'error',
      model_id: m.model_id,
      provider_id: m.provider_id,
      message: '管理接口调用失败：' + (e as Error).message,
      latency_ms: 0,
    }
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

async function queryBalance() {
  if (balanceBusy.value) return
  balanceBusy.value = true
  try {
    balance.value = await api.providerBalance(providerId.value)
  } catch (e) {
    if (is401(e)) return
    // 管理接口失败与「上游余额查不到」是两件事：后者在响应里是
    // status=error。分开显示，否则管理员会去换 key 而真正该做的是重试。
    balance.value = {
      provider_id: providerId.value,
      provider: '',
      status: 'error',
      message: '管理接口调用失败：' + (e as Error).message,
      results: [],
    }
  } finally {
    balanceBusy.value = false
  }
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
  price_output: 0,
  enabled: true,
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
  mForm.price_output = m?.price_output ?? 0
  mForm.enabled = m?.enabled ?? true
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
  const prices = [mForm.price_input, mForm.price_cache_hit, mForm.price_output].map(Number)
  if (prices.some((v) => !Number.isFinite(v) || v < 0)) {
    toast('单价必须是大于等于 0 的数字', 'err')
    return
  }
  mForm.saving = true
  try {
    const original = models.value.find((m) => m.id === mForm.editing)
    if (original && [original.price_input, original.price_cache_hit, original.price_output].some((v) => v > 0) && prices.every((v) => v === 0)) {
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
      price_output: prices[2],
      enabled: mForm.enabled,
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
  <main class="page">
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
            <div class="card__header">
              <div class="card__title">
                概览
                <span class="badge" :class="provider.enabled ? 'badge-live' : 'badge-off'">
                  {{ provider.enabled ? '启用' : '停用' }}
                </span>
              </div>
            </div>
            <div class="card__footer ov-actions">
              <button
                class="switch"
                :class="{ on: provider.enabled }"
                :title="provider.enabled ? '停用' : '启用'"
                @click="toggleProvider"
              ></button>
              <button class="btn btn-sm" @click="openEdit">编辑基础信息</button>
              <span class="up-spacer"></span>
              <button class="btn btn-sm btn-danger" @click="removeProvider">删除</button>
            </div>
            <div class="card__content">
              <!-- 键值表：标签固定宽、值 flex:1 并省略。全量值在 title 里，
                   且「编辑基础信息」里能直接看到完整原文。 -->
              <div class="kv">
                <div class="kv-row"><span class="kv-k">协议</span><span class="kv-v"><span class="badge">{{ provider.protocol }}</span></span></div>
                <div class="kv-row"><span class="kv-k">Endpoint</span><span class="kv-v mono kv-ep" :title="provider.endpoint">{{ provider.endpoint }}</span></div>
                <div class="kv-row">
                  <span class="kv-k">出站</span>
                  <span class="kv-v dim" :title="provider.proxy_url || '直连（不经过代理）'">
                    {{ provider.proxy_url ? `代理 ${provider.proxy_url}` : '直连（不经过代理）' }}
                  </span>
                </div>
                <div class="kv-row"><span class="kv-k">Slug</span><span class="kv-v mono dim">{{ provider.slug }}</span></div>
                <div class="kv-row">
                  <span class="kv-k">超时 / 重试</span>
                  <span class="kv-v dim">
                    {{ provider.timeout_ms ? `${provider.timeout_ms} ms` : '默认' }} /
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
            @toggle="toggleCred"
          />
        </div>
      </div>

      <!-- 模型表：整行占满宽度。见上面的说明（五列、四列 nowrap、
           且可能是这一页行数最多的块）。 -->
      <div class="models-card">
        <UpstreamModelPanel
          :models="models"
          :testing="testing"
          :test-results="modelTestResults"
          @add="openModel()"
          @test="testModel"
          @edit="(m) => openModel(m)"
          @remove="removeModel"
        />
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
            <label>显示名称</label>
            <input v-model="pForm.name" class="input" placeholder="DeepSeek" />
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
          <div class="field span2">
            <label>最大重试</label>
            <input v-model.number="pForm.max_retries" class="input num" type="number" min="0" max="10" step="1" />
            <span class="tip">
              仅作用于<strong>幂等</strong>请求（如连通性测试时的 GET /models）。
              对话是 POST，SDK 不会重试 —— 重试由「故障转移链换目标」承担。
            </span>
          </div>

          <div class="field span2">
            <label>代理（可选）</label>
            <input v-model="pForm.proxy_url" class="input mono" placeholder="http://127.0.0.1:7897" autocomplete="off" />
            <span class="tip">
              只对这个上游生效，其它上游不受影响。支持 http / https / socks5 /
              socks5h（<code>socks5h</code> 让 DNS 也走代理，本地 DNS 被污染时用它）。
              留空表示不代理。填错不会阻断请求，但该上游会<strong>退回直连</strong>，
              网关日志里会留一条告警。
              <br />
              <strong>网关跑在 Docker 里时注意</strong>：<code>127.0.0.1</code>
              指的是网关容器自己，不是宿主机。代理也在容器里的话，填
              <code>http://代理容器名:端口</code>（同一 Docker 网络内用容器名互访）；
              代理在宿主机上时才用 <code>host.docker.internal</code>。
            </span>
          </div>

          <div class="field span2">
            <label>余额查询（可选）</label>
            <textarea
              v-model="pForm.balance_spec_json"
              class="input mono"
              rows="2"
              spellcheck="false"
              placeholder='{"path":"/credits","field":"data.remaining","currency":"USD"}'
            ></textarea>
            <span class="tip">
              填 JSON，留空则用内置判定（OpenAI 官方与 DeepSeek 自动识别，其余上报「不支持」）。
              <code>path</code> 必填，是以 <code>/</code> 开头的相对路径；
              <code>field</code> 是余额在响应里的点号路径（默认 <code>balance</code>）；
              <code>currency</code> 只是标单位、不做换算；
              <code>auth</code> 取 <code>bearer</code>（默认）或 <code>anthropic</code>。
              <br />
              例：OpenRouter 是 <code>{{ openRouterSpec }}</code>。
              填错会在保存时直接报错，不会等到点「查询余额」才失败。
            </span>
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
            <label>标签</label>
            <input v-model="cForm.label" class="input" placeholder="主账号 / 备用池" />
          </div>
          <div class="field span2">
            <label>API Key {{ cForm.editing ? '（留空则不更换）' : '*' }}</label>
            <input v-model="cForm.api_key" class="input mono" type="password" placeholder="sk-..." autocomplete="new-password" />
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
            <label>单价（元 / 1M tokens）</label>
            <div class="price-edit-grid">
              <label><span>输入</span><input v-model.number="mForm.price_input" class="input num" type="number" min="0" step="any" placeholder="未配置" /></label>
              <label><span>缓存命中</span><input v-model.number="mForm.price_cache_hit" class="input num" type="number" min="0" step="any" placeholder="未配置" /></label>
              <label><span>输出</span><input v-model.number="mForm.price_output" class="input num" type="number" min="0" step="any" placeholder="未配置" /></label>
            </div>
            <span class="tip">输入价和输出价填 0 就是不计费；缓存命中价填 0 时沿用输入价。改价只影响之后的调用，历史费用按记录时单价保留。</span>
          </div>
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
              剥掉请求上的思考配置（并随 /v1/models 下发该声明）。
            </span>
          </div>
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
              勾选该模型**真实支持**的思考强度档位。一个都不勾 = 未配置。
              客户端选了不在列表里的档位时，网关会就近夹取并在日志留痕。
            </span>
          </div>
          <div class="field span2">
            <label class="check-line">
              <input v-model="mForm.enabled" type="checkbox" />
              启用该模型
            </label>
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
            style="flex: 1"
          />
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
  </main>
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
.ov-actions { gap: 8px; width: 100%; }
/* 把「删除」推到右缘：破坏性动作与「编辑」分居两端，减少误点。 */
.up-spacer { flex: 1; }

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
