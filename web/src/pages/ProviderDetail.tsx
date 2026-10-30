import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import {
  Button,
  Input,
  Label,
  ListBox,
  ListBoxItem,
  Select,
  SelectIndicator,
  SelectPopover,
  SelectTrigger,
  SelectValue,
  Spinner,
  Switch,
  TextArea,
  TextField,
  ToggleButton,
  ToggleButtonGroup,
  toast,
} from '@heroui/react'
import { api } from '../api'
import { NETWORK_DIRECT_ID, useNetworks } from '../useNetworks'
import { PROTOCOL_OPTIONS, protocolLabel } from '../protocols'
import { RequestGeneration } from '../asyncState'
import { PageHeader } from '../ui/PageHeader'
import { SectionCard } from '../ui/SectionCard'
import { AppModal } from '../ui/AppModal'
import { StatusChip } from '../ui/StatusChip'
import { confirmBox } from '../ui/confirm'
import { CredentialPanel } from '../components/CredentialPanel'
import { UpstreamModelPanel } from '../components/UpstreamModelPanel'
import type {
  Provider,
  Credential,
  UpstreamModel,
  DiscoveredModel,
  ModelTestResult,
  ProviderBalance,
} from '../types'

const is401 = (e: unknown): boolean => (e as { status?: number }).status === 401
const is404 = (e: unknown): boolean => (e as { status?: number }).status === 404

/**
 * 单个上游的详情页（语义对齐旧 ProviderDetail.vue）。
 *
 * 为什么从 Providers 列表里独立成一张路由页：十几个上游时，列表页要同时
 * 承担「一屏扫完摘要」与「把某个上游的全部配置摊开」，两者的查阅频率差一个
 * 数量级。这里承接**全部**写操作（换钥、导入/删除模型、查余额、改设置），
 * 列表页只保留读与两个轻量动作。没有第二处状态真相：列表页不做任何子资源
 * 请求。
 *
 * 状态从路由参数拿 provider id，因此刷新与分享链接都成立。
 */

function fmtTimeoutSec(ms: number): string {
  if (!ms) return '默认'
  const seconds = ms / 1000
  return `${Number.isInteger(seconds) ? seconds : seconds.toFixed(1)} 秒`
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

type Thinking = 'unset' | 'yes' | 'no'

const knownEffortLevels = ['minimal', 'low', 'medium', 'high', 'xhigh', 'max'] as const

const thinkingOptions: { v: Thinking; label: string }[] = [
  { v: 'unset', label: '未配置' },
  { v: 'yes', label: '支持' },
  { v: 'no', label: '不支持' },
]

export function ProviderDetail() {
  const { id = '' } = useParams()
  const navigate = useNavigate()
  const [searchParams, setSearchParams] = useSearchParams()

  const [provider, setProvider] = useState<Provider | null>(null)
  const [notFound, setNotFound] = useState(false)
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(true)

  const [creds, setCreds] = useState<Credential[]>([])
  const [models, setModels] = useState<UpstreamModel[]>([])
  // 子资源各自的加载失败态。**不能**只弹 toast：失败时 creds/models 仍是空数组，
  // 页面于是渲染「无凭据 —— 上游鉴权必需」「无模型 —— 路由必须指向一个上游模型」，
  // 把一次网关不可达说成「配置真的没有了」，而空态与失败态的处置动作完全相反
  // （一个要新增配置，一个要重试）。
  const [credsErr, setCredsErr] = useState('')
  const [modelsErr, setModelsErr] = useState('')

  const [togglingProvider, setTogglingProvider] = useState(false)
  const [credToggling, setCredToggling] = useState<Set<string>>(new Set())
  const [testing, setTesting] = useState<Set<string>>(new Set())

  // 余额是点按查询、不是轮询快照：这是一个会被据此决定「要不要充值」的数字，
  // 显示十分钟前的值比不显示更糟。取数时刻随结果一起显示。
  const [balance, setBalance] = useState<ProviderBalance | undefined>(undefined)
  const [balanceBusy, setBalanceBusy] = useState(false)
  const balanceRequests = useRef(new RequestGeneration())

  // 模型探测的客户端超时必须大于运行时的上游超时，否则后端还没报真实原因，
  // 浏览器先掐成一句无信息的「请求超时」。设置读取失败时退回保守值。
  const [testTimeoutMs, setTestTimeoutMs] = useState(150_000 + 5_000)

  // 网络清单不参与成败判定：只用于概览「代理」行与编辑表单下拉的展示辅助，
  // 读不到时那两处退化即可，不该让整页变成错误态。
  const { networks, load: loadNetworks } = useNetworks()

  const networkLabel = useMemo(() => {
    const map: Record<string, string> = {}
    for (const n of networks) map[n.id] = n.name
    return (nid: string) => map[nid] ?? nid.slice(0, 8)
  }, [networks])

  const loadSeqRef = useRef(0)
  const idRef = useRef(id)
  idRef.current = id

  /** reloadSub 拉取该上游的凭据与模型。silent 时不弹 toast（后台刷新失败不该抢注意力）。 */
  const reloadSub = async (silent = false) => {
    const pid = idRef.current
    // 两个子资源各自成败互不牵连：凭据接口挂了不该把模型表也标成失败。
    const [c, m] = await Promise.allSettled([api.credentials(pid), api.models(pid)])
    if (pid !== idRef.current) return
    if (c.status === 'fulfilled') {
      setCreds(c.value)
      setCredsErr('')
    } else if (!is401(c.reason)) {
      const msg = '凭据加载失败：' + (c.reason as Error).message
      setCredsErr(msg)
      if (!silent) toast.danger(msg)
    }
    if (m.status === 'fulfilled') {
      setModels(m.value)
      setModelsErr('')
    } else if (!is401(m.reason)) {
      const msg = '模型加载失败：' + (m.reason as Error).message
      setModelsErr(msg)
      if (!silent) toast.danger(msg)
    }
  }

  const load = async () => {
    const pid = id
    const seq = ++loadSeqRef.current
    setLoading(true)
    setErr('')
    let found = true
    try {
      const result = await api.provider(pid)
      if (pid !== idRef.current || seq !== loadSeqRef.current) return
      setProvider(result)
      setNotFound(false)
    } catch (e) {
      if (pid !== idRef.current || seq !== loadSeqRef.current) return
      if (is404(e)) {
        found = false
        setProvider(null)
        setNotFound(true)
        return
      }
      if (!is401(e)) setErr('加载上游失败：' + (e as Error).message)
    }
    if (pid !== idRef.current || seq !== loadSeqRef.current) return
    if (found) await reloadSub()
    if (seq === loadSeqRef.current) setLoading(false)
  }

  // 换 URL 里的 id 时重载：详情页可以被直接链接到任意上游，也可以在本页
  // 与列表之间来回跳。不监听的话，上一次的数据会留在页面上配到另一个 id。
  useEffect(() => {
    setProvider(null)
    setNotFound(false)
    setCreds([])
    setModels([])
    setCredsErr('')
    setModelsErr('')
    setBalance(undefined)
    setBalanceBusy(false)
    balanceRequests.current.invalidate()
    setEditOpen(false)
    setCredForm((f) => ({ ...f, open: false }))
    setMForm((f) => ({ ...f, open: false }))
    void load()
    void loadNetworks()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id])

  // 只取设置里的上游超时这一个字段；拉不到就保持保守值，不阻塞页面。
  useEffect(() => {
    api
      .settings()
      .then((s) => {
        if (s.upstream_timeout_ms > 0) setTestTimeoutMs(s.upstream_timeout_ms + 5_000)
      })
      .catch(() => {
        /* 保持 150s 保守值 */
      })
  }, [])

  // ---------- 概览动作 ----------

  const toggleProvider = async () => {
    const p = provider
    // 在途守卫：连点会发出 enable 再 disable，而两个响应各自提示一次成功，
    // 最终状态取决于返回顺序。
    if (!p || togglingProvider) return
    setTogglingProvider(true)
    try {
      // 只发改动字段：其余字段后端保持原值（PATCH 是字段级部分更新）。
      await api.updateProvider(p.id, { enabled: !p.enabled })
      toast.success(!p.enabled ? '已启用' : '已停用')
      await loadProviderOnly()
    } catch (e) {
      if (!is401(e)) toast.danger('操作失败：' + (e as Error).message)
    } finally {
      setTogglingProvider(false)
    }
  }

  const loadProviderOnly = async () => {
    const pid = idRef.current
    try {
      const result = await api.provider(pid)
      if (pid !== idRef.current) return
      setProvider(result)
    } catch (e) {
      if (pid !== idRef.current) return
      if (is404(e)) {
        setProvider(null)
        setNotFound(true)
        return
      }
      if (!is401(e)) setErr('加载上游失败：' + (e as Error).message)
    }
  }

  const removeProvider = async () => {
    const p = provider
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
      toast.success('已删除')
      // 删掉当前页面的对象后没有可停留的地方，回列表。
      navigate({ pathname: '/providers', search: searchParams.toString() })
    } catch (e) {
      if (!is401(e)) toast.danger('删除失败：' + (e as Error).message)
    }
  }

  // ---------- 基础信息编辑 ----------
  // 弹窗而不是页面内表单：代理与余额查询那两段说明是**修改时才需要读**的，
  // 常驻在页面里只会把凭据与模型往下挤。弹窗把「读摘要」与「改配置」
  // 这两件事彻底分开。

  const [editOpen, setEditOpen] = useState(false)
  const [pName, setPName] = useState('')
  const [pProtocol, setPProtocol] = useState('chat')
  const [pEndpoint, setPEndpoint] = useState('')
  const [pTimeoutSec, setPTimeoutSec] = useState('0')
  const [pMaxRetries, setPMaxRetries] = useState('0')
  const [pNetworkId, setPNetworkId] = useState(NETWORK_DIRECT_ID)
  const [pBalanceSpec, setPBalanceSpec] = useState('')
  const [pSaving, setPSaving] = useState(false)

  /** openEdit 每次都从当前 provider 重新填表：上一次取消时留下的半截输入
   *  不应该在下次打开时还在（那会让人以为「我刚才明明填了」）。 */
  const openEdit = () => {
    const p = provider
    if (!p) return
    setPName(p.name)
    setPProtocol(p.protocol || 'chat')
    setPEndpoint(p.endpoint)
    setPTimeoutSec(String(p.timeout_ms / 1000))
    setPMaxRetries(String(p.max_retries))
    setPNetworkId(p.network_id || NETWORK_DIRECT_ID)
    setPBalanceSpec(p.balance_spec_json)
    setEditOpen(true)
    void loadNetworks()
  }

  // dirty 是「保存」按钮的启用条件。表单里存字符串、比较时按提交口径解析，
  // 两边必须同一套换算（秒 ↔ 毫秒、Number() 的 0 兜底）。
  const editDirty = provider
    ? pName !== provider.name ||
      pProtocol !== (provider.protocol || 'chat') ||
      pEndpoint !== provider.endpoint ||
      (Number(pTimeoutSec) || 0) !== provider.timeout_ms / 1000 ||
      (Number(pMaxRetries) || 0) !== provider.max_retries ||
      pNetworkId !== (provider.network_id || NETWORK_DIRECT_ID) ||
      pBalanceSpec !== provider.balance_spec_json
    : false

  /** closeEdit：有未保存改动时二次确认。这一页的表单里有 endpoint 与代理 ——
   *  误关掉一个改了一半的代理配置不致命，但「以为存上了、其实没有」才是真的坑。 */
  const closeEdit = async () => {
    if (editDirty) {
      const discard = await confirmBox({
        title: '放弃未保存的改动？',
        body: '基础信息有未保存的修改，关闭后这些修改会丢失。',
        confirmLabel: '放弃改动',
      })
      if (!discard) return
    }
    setEditOpen(false)
  }

  const saveProvider = async () => {
    const p = provider
    if (!p) return
    if (!pEndpoint.trim()) {
      toast.danger('Endpoint 为必填项')
      return
    }
    setPSaving(true)
    try {
      await api.updateProvider(p.id, {
        name: pName.trim() || pEndpoint.trim(),
        protocol: pProtocol,
        endpoint: pEndpoint.trim(),
        timeout_ms: Math.max(0, Math.round((Number(pTimeoutSec) || 0) * 1000)),
        max_retries: Number(pMaxRetries) || 0,
        // 网络与余额查询配置**每次都发**，空串 = 清空。省略字段的语义是
        // 「保持原值」，那会让「换回直连」「删掉自定义余额脚本」这两个动作
        // 永远发不出去 —— 必须显式把空串写回去。
        network_id: pNetworkId,
        balance_spec_json: pBalanceSpec.trim(),
      })
      toast.success('上游已更新')
      // 关闭弹窗在重载**之后**：loadProvider 会同步 provider。顺序反了的话
      // 表单会先按旧值填一次再跳一次。
      await loadProviderOnly()
      setEditOpen(false)
    } catch (e) {
      if (!is401(e)) toast.danger('保存失败：' + (e as Error).message)
    } finally {
      setPSaving(false)
    }
  }

  // ---------- 凭据 ----------

  const [credForm, setCredForm] = useState({
    open: false,
    editingId: '',
    label: '',
    api_key: '',
    saving: false,
  })

  const openCred = (c?: Credential) => {
    setCredForm({
      open: true,
      editingId: c?.id ?? '',
      label: c?.label ?? '',
      api_key: '',
      saving: false,
    })
  }

  const submitCred = async () => {
    if (!credForm.editingId && !credForm.api_key) {
      toast.danger('api_key 为必填项')
      return
    }
    setCredForm((f) => ({ ...f, saving: true }))
    try {
      if (credForm.editingId) {
        await api.updateCredential(credForm.editingId, {
          label: credForm.label.trim(),
          ...(credForm.api_key ? { api_key: credForm.api_key } : {}),
        })
      } else {
        await api.createCredential(idRef.current, {
          label: credForm.label.trim(),
          api_key: credForm.api_key,
        })
      }
      const wasEditing = !!credForm.editingId
      setCredForm((f) => ({ ...f, open: false }))
      toast.success(wasEditing ? '凭据已更新' : '凭据已创建')
      await Promise.all([reloadSub(), loadProviderOnly()])
    } catch (e) {
      if (!is401(e)) toast.danger('保存失败：' + (e as Error).message)
    } finally {
      setCredForm((f) => ({ ...f, saving: false }))
    }
  }

  const removeCred = async (c: Credential) => {
    const ok = await confirmBox({
      title: `删除凭据「${c.label || c.id}」？`,
      body: '删除后该上游可能变为不可用（若已无其他启用的凭据），指向它的路由会失败转移。',
      danger: true,
      confirmLabel: '删除',
    })
    if (!ok) return
    try {
      await api.deleteCredential(c.id)
      toast.success('已删除')
      await reloadSub()
    } catch (e) {
      if (!is401(e)) toast.danger('删除失败：' + (e as Error).message)
    }
  }

  const toggleCred = async (c: Credential) => {
    if (credToggling.has(c.id)) return
    const turningOn = !c.enabled
    // 启是「切换」语义，连点两次的结果与点一次完全相反，且期间池真的换过 key。
    // 先数一遍本来开着的是谁：这次请求会把它们一并关掉，而响应只描述这一把。
    const previouslyOn = creds.filter((x) => x.enabled && x.id !== c.id)

    setCredToggling((prev) => new Set(prev).add(c.id))
    try {
      await api.updateCredential(c.id, { enabled: turningOn })
      if (turningOn && previouslyOn.length > 0) {
        toast.success(`已切到「${c.label || c.id}」，原启用的 ${previouslyOn.length} 把已停用`)
      } else {
        toast.success(turningOn ? '已启用' : '已停用')
      }
      // 必须整表重载：同 provider 的其它行状态也变了，而 PATCH 响应只带这一把。
      await reloadSub()
    } catch (e) {
      if (!is401(e)) toast.danger('操作失败：' + (e as Error).message)
    } finally {
      setCredToggling((prev) => {
        const next = new Set(prev)
        next.delete(c.id)
        return next
      })
    }
  }

  // ---------- 余额 ----------

  const queryBalance = async () => {
    if (balanceBusy) return
    const pid = idRef.current
    const generation = balanceRequests.current.begin()
    setBalanceBusy(true)
    try {
      const result = await api.providerBalance(pid)
      if (
        !balanceRequests.current.isCurrent(generation) ||
        pid !== idRef.current ||
        result.provider_id !== pid
      )
        return
      setBalance(result)
    } catch (e) {
      if (is401(e)) return
      if (!balanceRequests.current.isCurrent(generation) || pid !== idRef.current) return
      // 管理接口失败与「上游余额查不到」是两件事：后者在响应里是
      // status=error。分开显示，否则管理员会去换 key 而真正该做的是重试。
      setBalance({
        provider_id: pid,
        provider: '',
        status: 'error',
        message: '管理接口调用失败：' + (e as Error).message,
        results: [],
      })
    } finally {
      if (balanceRequests.current.isCurrent(generation)) setBalanceBusy(false)
    }
  }

  const balanceSpecConfigured = !!provider?.balance_spec_json?.trim()

  // ---------- 模型：编辑 / 探测添加 ----------

  const [mForm, setMForm] = useState({
    open: false,
    editingId: '',
    model_id: '',
    context_window: '0',
    max_output_tokens: '0',
    price_input: '0',
    price_cache_hit: '0',
    price_cache_creation: '0',
    price_output: '0',
    effortCsv: '',
    thinking: 'unset' as Thinking,
    saving: false,
  })
  const [discovering, setDiscovering] = useState(false)
  const [discovered, setDiscovered] = useState<DiscoveredModel[]>([])
  const [discoverMsg, setDiscoverMsg] = useState('')
  const [importing, setImporting] = useState(false)
  const [mFilter, setMFilter] = useState('')
  const [selected, setSelected] = useState<DiscoveredModel[]>([])

  const filteredDiscovered = useMemo(() => {
    const q = mFilter.trim().toLowerCase()
    if (!q) return discovered
    return discovered.filter(
      (d) => d.model_id.toLowerCase().includes(q) || (d.display_name ?? '').toLowerCase().includes(q),
    )
  }, [discovered, mFilter])

  const openModel = (m?: UpstreamModel) => {
    setMForm({
      open: true,
      editingId: m?.id ?? '',
      model_id: m?.model_id ?? '',
      context_window: String(m?.context_window ?? 0),
      max_output_tokens: String(m?.max_output_tokens ?? 0),
      price_input: String(m?.price_input ?? 0),
      price_cache_hit: String(m?.price_cache_hit ?? 0),
      price_cache_creation: String(m?.price_cache_creation ?? 0),
      price_output: String(m?.price_output ?? 0),
      saving: false,
      // unknown 档位附在后面：它们是网关不认、但可能是上游认得的厂商私有值，
      // 编辑时必须原样带回去，否则点一次保存就把它们悄悄抹掉了。
      effortCsv: [...(m?.effort_levels ?? []), ...(m?.unknown_effort_levels ?? [])].join(','),
      thinking:
        m?.supports_thinking === undefined ? 'unset' : m.supports_thinking ? 'yes' : 'no',
    })
    setDiscovered([])
    setSelected([])
    setDiscoverMsg('')
    setMFilter('')
  }

  const hasEffortLevel = (l: string): boolean =>
    mForm.effortCsv.split(',').some((x) => x.trim() === l)

  // 用集合而不是字符串拼装算勾选态：CSV 是有序文本，includes 会把「xhigh」
  // 误判为含「high」（子串包含），那是会点错的。
  const toggleEffortLevel = (l: string) => {
    const parts = mForm.effortCsv
      .split(',')
      .map((x) => x.trim())
      .filter(Boolean)
    const i = parts.indexOf(l)
    if (i >= 0) parts.splice(i, 1)
    else parts.push(l)
    const rank = (x: string) => {
      const idx = knownEffortLevels.indexOf(x as (typeof knownEffortLevels)[number])
      return idx < 0 ? knownEffortLevels.length : idx
    }
    parts.sort((a, b) => rank(a) - rank(b))
    setMForm((f) => ({ ...f, effortCsv: parts.join(',') }))
  }

  // 明确「不支持思考」时清空挡位；反过来不成立（「支持」不必配挡位）。
  // 否则界面上会出现「不支持思考，却配了 high/xhigh」，而其后果是档位仍被
  // 当成有效能力披露给 /v1/models，客户端以为能选、选了没效果。
  const setThinking = (v: Thinking) => {
    setMForm((f) => ({ ...f, thinking: v, effortCsv: v === 'no' ? '' : f.effortCsv }))
  }

  const submitModel = async () => {
    if (mForm.saving || !mForm.editingId) return
    if (!mForm.model_id.trim()) {
      toast.danger('上游模型名（model_id）为必填项')
      return
    }
    const prices = [
      mForm.price_input,
      mForm.price_cache_hit,
      mForm.price_cache_creation,
      mForm.price_output,
    ].map(Number)
    if (prices.some((v) => !Number.isFinite(v) || v < 0)) {
      toast.danger('单价必须是大于等于 0 的数字')
      return
    }
    setMForm((f) => ({ ...f, saving: true }))
    try {
      const original = models.find((m) => m.id === mForm.editingId)
      if (
        original &&
        [original.price_input, original.price_cache_hit, original.price_cache_creation, original.price_output].some(
          (v) => v > 0,
        ) &&
        prices.every((v) => v === 0)
      ) {
        const ok = await confirmBox({
          title: '清除该模型的全部价格？',
          body: '之后的调用将按 0 元计费，历史费用不变。',
          confirmLabel: '清除价格并保存',
        })
        if (!ok) return
      }
      await api.updateModel(mForm.editingId, {
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
      setMForm((f) => ({ ...f, open: false }))
      toast.success('模型已更新')
      await reloadSub()
    } catch (e) {
      if (!is401(e)) toast.danger('保存失败：' + (e as Error).message)
    } finally {
      setMForm((f) => ({ ...f, saving: false }))
    }
  }

  const runDiscover = async () => {
    setDiscovering(true)
    setDiscovered([])
    setSelected([])
    setDiscoverMsg('')
    try {
      const r = await api.discoverModels(idRef.current)
      if (r.status === 'ok') {
        setDiscovered(r.models ?? [])
        if ((r.models ?? []).length === 0) setDiscoverMsg('上游未返回任何模型')
      } else {
        setDiscoverMsg(r.message || '获取失败')
      }
    } catch (e) {
      setDiscoverMsg((e as Error).message)
    } finally {
      setDiscovering(false)
    }
  }

  const isSelected = (mid: string) => selected.some((s) => s.model_id === mid)

  const toggleSelect = (d: DiscoveredModel) => {
    setSelected((prev) =>
      prev.some((s) => s.model_id === d.model_id)
        ? prev.filter((s) => s.model_id !== d.model_id)
        : [...prev, d],
    )
  }

  const removeSelected = (mid: string) => {
    setSelected((prev) => prev.filter((s) => s.model_id !== mid))
  }

  const confirmImport = async () => {
    const list = selected
    if (list.length === 0) return
    setImporting(true)
    try {
      const r = await api.importModels(
        idRef.current,
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
      toast.success(`已添加 ${r.imported} 个模型`)
      setMForm((f) => ({ ...f, open: false }))
      // 概览的模型数来自列表接口的 model_count，所以必须刷新 provider，
      // 否则添加后这页的计数会停在旧值。
      await Promise.all([reloadSub(), loadProviderOnly()])
    } catch (e) {
      if (!is401(e)) toast.danger('添加失败：' + (e as Error).message)
    } finally {
      setImporting(false)
    }
  }

  const removeModel = async (m: UpstreamModel) => {
    if (m.is_route_head) {
      toast.danger('该模型是某条路由的首选目标，不能直接删除 —— 请先到「路由」页改掉那条路由的首选')
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
      toast.success('已删除')
      await Promise.all([reloadSub(), loadProviderOnly()])
    } catch (e) {
      if (!is401(e)) toast.danger('删除失败：' + (e as Error).message)
    }
  }

  const testModel = async (m: UpstreamModel) => {
    if (testing.has(m.id)) return
    setTesting((prev) => new Set(prev).add(m.id))
    try {
      const result = await api.testModel(m.id, testTimeoutMs)
      const message = modelTestToast(m, result)
      toast[message.includes('· 正常') ? 'success' : 'danger'](message)
    } catch (e) {
      if (is401(e)) return
      toast.danger(`${m.model_id} · 测试失败：${(e as Error).message}`)
    } finally {
      setTesting((prev) => {
        const next = new Set(prev)
        next.delete(m.id)
        return next
      })
    }
  }

  // 从路由详情跳转时可携带 ?model=upstream_models.id，直接打开对应模型编辑。
  // 消费掉 query，避免刷新或返回时重复弹出同一个编辑框。
  useEffect(() => {
    const target = searchParams.get('model')
    if (typeof target !== 'string' || !target || !models.length) return
    const model = models.find((m) => m.id === target && m.provider_id === idRef.current)
    if (!model) return
    openModel(model)
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        next.delete('model')
        return next
      },
      { replace: true },
    )
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [models, searchParams])

  const credTogglingIds = useMemo(() => [...credToggling], [credToggling])
  const backTo = { pathname: '/providers', search: searchParams.toString() }

  return (
    <div className="flex flex-col gap-5">
      <PageHeader
        title={provider?.name ?? '上游详情'}
        sub={
          <Link to={backTo} className="text-link text-[12.5px] hover:underline">
            ← 上游服务
          </Link>
        }
        actions={
          <Button variant="secondary" isDisabled={loading} onPress={() => void load()}>
            刷新
          </Button>
        }
      />

      {loading ? (
        <div className="flex items-center justify-center gap-2 py-16" role="status">
          <Spinner />
          加载中…
        </div>
      ) : notFound ? (
        <SectionCard>
          <div className="py-10 text-center">
            <p className="text-muted text-sm">该上游不存在（可能已被删除）</p>
            <Button variant="tertiary" size="sm" className="mt-3" onPress={() => navigate(backTo)}>
              返回列表
            </Button>
          </div>
        </SectionCard>
      ) : err ? (
        <SectionCard>
          <div className="py-10 text-center text-danger text-sm" role="alert">
            {err}
          </div>
        </SectionCard>
      ) : provider ? (
        <>
          {provider.enabled && provider.ready !== true ? (
            <div
              role="status"
              className="border-warning/30 bg-warning-soft text-warning-foreground rounded-md border px-3 py-2 text-sm"
            >
              <b>{provider.ready === false ? '未就绪' : '状态未知'}</b> ·{' '}
              {provider.ready_reason ||
                (provider.ready === false
                  ? '未建立可用凭据池，请检查下方凭据配置'
                  : '暂未取得运行时状态')}
            </div>
          ) : null}

          {/* 概览与凭据并排，模型表单独占满下一行：概览只有六行只读键值、
               凭据表只有三列，都天然适合窄栏；并排后首屏能同时看到「这个上游
               是谁」和「它现在用哪把 key」—— 排查「这个上游怎么不通」正是从
               这两处一起看的。模型表有五列且可能是行数最多的块，本来就该占满。 */}
          <div className="grid items-start gap-5 lg:grid-cols-2">
            <SectionCard
              title="概览"
              // 标题不重复写一遍名字 —— PageHeader 的 h1 已经是主标题。
              // 编辑/删除/启停都收在概览卡上：改配置是低频动作，位置固定好找。
              actions={
                <>
                  <Button variant="secondary" size="sm" onPress={openEdit}>
                    编辑
                  </Button>
                  <Button variant="ghost" size="sm" onPress={() => void removeProvider()}>
                    删除
                  </Button>
                  <Switch
                    isSelected={provider.enabled}
                    isDisabled={togglingProvider}
                    aria-label={`${provider.enabled ? '停用' : '启用'} ${provider.name || provider.slug}`}
                    onChange={() => void toggleProvider()}
                  >
                    <Switch.Control>
                      <Switch.Thumb />
                    </Switch.Control>
                  </Switch>
                </>
              }
            >
              <div className="flex flex-col">
                <KvRow k="类型">
                  <StatusChip tone="dim">{protocolLabel(provider.protocol)}</StatusChip>
                </KvRow>
                {/* Endpoint 的可辨识信息在路径尾巴上，掐尾省略（…）会把
                     「chat/completions」这半截丢掉 —— 换行而不是省略。 */}
                <KvRow k="Endpoint">
                  <span className="font-mono break-all">{provider.endpoint}</span>
                </KvRow>
                <KvRow k="代理" title={provider.proxy_url || '直连（不经过代理）'}>
                  {provider.network_id ? (
                    <>
                      {networkLabel(provider.network_id)}{' '}
                      <span className="text-muted font-mono text-xs">（{provider.proxy_url}）</span>
                    </>
                  ) : (
                    <span className="text-muted">直连（不经过代理）</span>
                  )}
                </KvRow>
                <KvRow k="Slug">
                  <span className="text-muted font-mono">{provider.slug}</span>
                </KvRow>
                <KvRow k="超时 / 重试">
                  <span className="text-muted">
                    {fmtTimeoutSec(provider.timeout_ms)} /{' '}
                    {provider.max_retries ? `${provider.max_retries} 次` : '默认'}
                  </span>
                </KvRow>
                <KvRow k="余额查询">
                  {provider.balance_spec_json?.trim() ? (
                    <StatusChip tone="info">自定义</StatusChip>
                  ) : (
                    <span className="text-muted">未配置</span>
                  )}
                </KvRow>
              </div>
            </SectionCard>

            {/* 余额刻意不单独成块：它本来就逐凭据显示（同一把 key 可能只剩一把
                 有余额），汇总成一个数字反而要自己对应是哪把。 */}
            <CredentialPanel
              providerId={provider.id}
              creds={creds}
              balance={balance}
              balanceBusy={balanceBusy}
              toggling={credTogglingIds}
              balanceSpecConfigured={balanceSpecConfigured}
              error={credsErr}
              onRetry={() => void reloadSub(true)}
              onAdd={() => openCred()}
              onQueryBalance={() => void queryBalance()}
              onEdit={(c) => openCred(c)}
              onRemove={(c) => void removeCred(c)}
              onToggle={(c) => void toggleCred(c)}
            />
          </div>

          <UpstreamModelPanel
            models={models}
            testing={testing}
            error={modelsErr}
            onRetry={() => void reloadSub(true)}
            onAdd={() => openModel()}
            onTest={(m) => void testModel(m)}
            onEdit={(m) => openModel(m)}
            onRemove={(m) => void removeModel(m)}
          />
        </>
      ) : null}

      {/* ---------- 基础信息编辑 ---------- */}
      <AppModal
        open={editOpen}
        onClose={() => void closeEdit()}
        title="编辑基础信息"
        maxWidth="560px"
        dismissable={false}
        footer={
          <>
            <Button variant="ghost" isDisabled={pSaving} onPress={() => void closeEdit()}>
              取消
            </Button>
            <Button
              variant="primary"
              isDisabled={pSaving || !editDirty}
              onPress={() => void saveProvider()}
            >
              {pSaving ? '保存中…' : '保存'}
            </Button>
          </>
        }
      >
        <form
          onSubmit={(e) => {
            e.preventDefault()
            void saveProvider()
          }}
          className="grid grid-cols-2 gap-4"
        >
          <TextField aria-label="显示名称" value={pName} onChange={setPName}>
            <Label>显示名称</Label>
            <Input placeholder="DeepSeek" autoComplete="off" />
          </TextField>
          <Select
            aria-label="类型"
            selectedKey={pProtocol}
            onSelectionChange={(key) => key !== null && setPProtocol(String(key))}
          >
            <SelectTrigger>
              <SelectValue />
              <SelectIndicator />
            </SelectTrigger>
            <SelectPopover>
              <ListBox aria-label="协议类型">
                {PROTOCOL_OPTIONS.map((o) => (
                  <ListBoxItem key={o.value} id={o.value}>
                    {o.label}
                  </ListBoxItem>
                ))}
              </ListBox>
            </SelectPopover>
          </Select>
          <TextField
            aria-label="Endpoint"
            isRequired
            value={pEndpoint}
            onChange={setPEndpoint}
            className="col-span-2"
          >
            <Label>Endpoint *</Label>
            <Input placeholder="https://api.deepseek.com" className="font-mono" autoComplete="off" />
          </TextField>
          <TextField aria-label="超时（秒）" value={pTimeoutSec} onChange={setPTimeoutSec}>
            <Label>超时（秒）</Label>
            <Input type="number" min={0} step={0.1} />
          </TextField>
          <TextField aria-label="最大重试次数" value={pMaxRetries} onChange={setPMaxRetries}>
            <Label>最大重试次数</Label>
            <Input type="number" min={0} max={10} step={1} />
          </TextField>
          <Select
            aria-label="代理"
            className="col-span-2"
            selectedKey={pNetworkId}
            onSelectionChange={(key) => key !== null && setPNetworkId(String(key))}
          >
            <SelectTrigger>
              <SelectValue />
              <SelectIndicator />
            </SelectTrigger>
            <SelectPopover>
              <ListBox aria-label="出站网络">
                <ListBoxItem id={NETWORK_DIRECT_ID}>直连（不经过代理）</ListBoxItem>
                {networks.map((n) => (
                  <ListBoxItem key={n.id} id={n.id}>
                    {n.name} — {n.proxy_url}
                  </ListBoxItem>
                ))}
              </ListBox>
            </SelectPopover>
          </Select>
          <div className="col-span-2 flex flex-col gap-1">
            <Label>余额查询（可选）</Label>
            <TextArea
              aria-label="余额查询（可选）"
              rows={2}
              spellCheck={false}
              className="font-mono"
              placeholder='{"path":"/credits","field":"data.remaining","currency":"USD"}'
              value={pBalanceSpec}
              onChange={(e) => setPBalanceSpec(e.target.value)}
            />
          </div>
        </form>
      </AppModal>

      {/* ---------- 凭据表单 ---------- */}
      <AppModal
        open={credForm.open}
        onClose={() => setCredForm((f) => ({ ...f, open: false }))}
        title={credForm.editingId ? '更新凭据' : '添加凭据'}
        maxWidth="480px"
        dismissable={false}
        footer={
          <>
            <Button
              variant="ghost"
              isDisabled={credForm.saving}
              onPress={() => setCredForm((f) => ({ ...f, open: false }))}
            >
              取消
            </Button>
            <Button
              variant="primary"
              isDisabled={credForm.saving}
              onPress={() => void submitCred()}
            >
              保存
            </Button>
          </>
        }
      >
        <form
          onSubmit={(e) => {
            e.preventDefault()
            void submitCred()
          }}
          className="flex flex-col gap-4"
        >
          <TextField
            aria-label="标签"
            value={credForm.label}
            onChange={(v) => setCredForm((f) => ({ ...f, label: v }))}
          >
            <Label>标签</Label>
            <Input placeholder="主账号 / 备用池" autoComplete="off" />
          </TextField>
          <TextField
            aria-label={credForm.editingId ? 'API Key（留空则不更换）' : 'API Key'}
            isRequired={!credForm.editingId}
            value={credForm.api_key}
            onChange={(v) => setCredForm((f) => ({ ...f, api_key: v }))}
          >
            <Label>API Key {credForm.editingId ? '（留空则不更换）' : '*'}</Label>
            <Input
              type="password"
              placeholder="sk-..."
              className="font-mono"
              autoComplete="new-password"
            />
            <span className="text-muted mt-1 block text-[11px]">
              保存后加密入库，仅显示掩码，不再可见明文
            </span>
          </TextField>
          {!credForm.editingId ? (
            <p className="text-muted text-[11px] leading-relaxed">
              同一上游同时只启用一把 key：若该上游已有启用的凭据，新建的这把会先以
              <b>停用</b>状态保存 —— 确认可用后在凭据列表里打开它的开关即可切换。
            </p>
          ) : null}
        </form>
      </AppModal>

      {/* ---------- 模型：编辑单条 ---------- */}
      {mForm.editingId ? (
        <AppModal
          open={mForm.open}
          onClose={() => setMForm((f) => ({ ...f, open: false }))}
          title="编辑上游模型"
          maxWidth="500px"
          dismissable={false}
          footer={
            <>
              <Button
                variant="ghost"
                isDisabled={mForm.saving}
                onPress={() => setMForm((f) => ({ ...f, open: false }))}
              >
                取消
              </Button>
              <Button
                variant="primary"
                isDisabled={mForm.saving}
                onPress={() => void submitModel()}
              >
                {mForm.saving ? '保存中…' : '保存'}
              </Button>
            </>
          }
        >
          <form
            onSubmit={(e) => {
              e.preventDefault()
              void submitModel()
            }}
            className="flex flex-col gap-4"
          >
            <TextField
              aria-label="上游模型名（model_id）"
              isRequired
              value={mForm.model_id}
              onChange={(v) => setMForm((f) => ({ ...f, model_id: v }))}
            >
              <Label>上游模型名（model_id）*</Label>
              <Input placeholder="deepseek-chat" className="font-mono" autoComplete="off" />
            </TextField>
            <div className="grid grid-cols-2 gap-4">
              <TextField
                aria-label="上下文窗口"
                value={mForm.context_window}
                onChange={(v) => setMForm((f) => ({ ...f, context_window: v }))}
              >
                <Label>上下文窗口</Label>
                <Input type="number" min={0} step={1} placeholder="0 = 未设置" />
              </TextField>
              <TextField
                aria-label="最大输出"
                value={mForm.max_output_tokens}
                onChange={(v) => setMForm((f) => ({ ...f, max_output_tokens: v }))}
              >
                <Label>最大输出</Label>
                <Input type="number" min={0} step={1} placeholder="0 = 未设置" />
              </TextField>
            </div>
            <fieldset className="min-w-0" disabled={mForm.saving}>
              <div className="text-muted text-[11px]">单价（元 / 1M tokens）</div>
              <div className="mt-2 grid grid-cols-3 gap-2">
                <PriceField
                  label="输入"
                  value={mForm.price_input}
                  onChange={(v) => setMForm((f) => ({ ...f, price_input: v }))}
                />
                <PriceField
                  label="缓存命中"
                  value={mForm.price_cache_hit}
                  onChange={(v) => setMForm((f) => ({ ...f, price_cache_hit: v }))}
                />
                <PriceField
                  label="缓存写入"
                  value={mForm.price_cache_creation}
                  onChange={(v) => setMForm((f) => ({ ...f, price_cache_creation: v }))}
                />
                <PriceField
                  label="输出"
                  value={mForm.price_output}
                  onChange={(v) => setMForm((f) => ({ ...f, price_output: v }))}
                />
              </div>
              <div className="mt-4">
                <div className="text-muted text-[11px]">思考模式</div>
                <ToggleButtonGroup
                  aria-label="思考模式"
                  className="mt-2"
                  selectionMode="single"
                  disallowEmptySelection
                  selectedKeys={[mForm.thinking]}
                  onSelectionChange={(keys) => {
                    const v = [...keys][0]
                    if (v !== undefined) setThinking(String(v) as Thinking)
                  }}
                >
                  {thinkingOptions.map((o) => (
                    <ToggleButton key={o.v} id={o.v} size="sm">
                      {o.label}
                    </ToggleButton>
                  ))}
                </ToggleButtonGroup>
              </div>
              {mForm.thinking !== 'no' ? (
                <div className="mt-4">
                  <div className="text-muted text-[11px]">思考挡位</div>
                  <ToggleButtonGroup
                    aria-label="思考挡位"
                    className="mt-2"
                    selectionMode="multiple"
                    selectedKeys={knownEffortLevels.filter((l) => hasEffortLevel(l))}
                    onSelectionChange={(keys) => {
                      const next = new Set([...keys].map(String))
                      for (const l of knownEffortLevels) {
                        if (next.has(l) !== hasEffortLevel(l)) toggleEffortLevel(l)
                      }
                    }}
                  >
                    {knownEffortLevels.map((l) => (
                      <ToggleButton key={l} id={l} size="sm">
                        {l}
                      </ToggleButton>
                    ))}
                  </ToggleButtonGroup>
                </div>
              ) : null}
            </fieldset>
          </form>
        </AppModal>
      ) : (
        /* ---------- 模型：探测 + 多选胶囊式添加 ---------- */
        <AppModal
          open={mForm.open}
          onClose={() => setMForm((f) => ({ ...f, open: false }))}
          title="添加上游模型"
          maxWidth="560px"
          dismissable={false}
          footer={
            <>
              <Button
                variant="ghost"
                isDisabled={importing}
                onPress={() => setMForm((f) => ({ ...f, open: false }))}
              >
                取消
              </Button>
              <Button
                variant="primary"
                isDisabled={!selected.length || importing}
                onPress={() => void confirmImport()}
              >
                {importing ? '添加中…' : `添加（${selected.length}）`}
              </Button>
            </>
          }
        >
          <div className="flex flex-col gap-3">
            <div className="flex items-center gap-2">
              <Button
                variant="secondary"
                size="sm"
                isDisabled={discovering}
                onPress={() => void runDiscover()}
              >
                {discovering ? '获取中…' : '获取模型列表'}
              </Button>
              {discovered.length ? (
                <TextField
                  aria-label="搜索模型"
                  value={mFilter}
                  onChange={setMFilter}
                  className="flex-1"
                >
                  <Input type="search" placeholder="搜索模型…" autoComplete="off" />
                </TextField>
              ) : null}
            </div>
            {discoverMsg ? (
              <p className="text-danger text-xs" role="alert">
                {discoverMsg}
              </p>
            ) : null}

            {selected.length ? (
              <div className="flex flex-wrap gap-2">
                {selected.map((s) => (
                  <span
                    key={s.model_id}
                    className="bg-default-soft flex items-center gap-1 rounded-full py-0.5 pr-1 pl-2.5 text-xs"
                  >
                    <span className="font-mono">{s.model_id}</span>
                    <Button
                      variant="ghost"
                      isIconOnly
                      size="sm"
                      aria-label={`移除 ${s.model_id}`}
                      className="h-5 min-w-5 rounded-full"
                      onPress={() => removeSelected(s.model_id)}
                    >
                      ×
                    </Button>
                  </span>
                ))}
              </div>
            ) : null}

            {discovered.length ? (
              <div className="grid max-h-[320px] grid-cols-1 gap-1.5 overflow-y-auto sm:grid-cols-2">
                {filteredDiscovered.map((d) => (
                  <Button
                    key={d.model_id}
                    variant={isSelected(d.model_id) ? 'secondary' : 'ghost'}
                    aria-label={isSelected(d.model_id) ? `取消选择 ${d.model_id}` : `选择 ${d.model_id}`}
                    className="h-auto w-full justify-start px-2 py-1.5 text-left"
                    onPress={() => toggleSelect(d)}
                  >
                    <span className="flex w-full min-w-0 items-center gap-2 text-xs">
                      <span className="shrink-0">{isSelected(d.model_id) ? '✓' : '+'}</span>
                      <span className="truncate font-mono">{d.model_id}</span>
                      <span className="text-muted ml-auto shrink-0 text-[10px]">
                        {d.effort_levels ? `思考 ${d.effort_levels}` : '思考 待配置'}
                        {d.context_window || d.max_output_tokens
                          ? ` · ${d.context_window ? d.context_window + ' ctx' : ''}${
                              d.context_window && d.max_output_tokens ? ' / ' : ''
                            }${d.max_output_tokens ? d.max_output_tokens + ' out' : ''}`
                          : ''}
                      </span>
                    </span>
                  </Button>
                ))}
              </div>
            ) : !discovering && !discoverMsg ? (
              <p className="text-muted py-4 text-center text-xs">
                点击「获取模型列表」拉取上游可用模型
              </p>
            ) : null}
          </div>
        </AppModal>
      )}
    </div>
  )
}

/** 概览键值行：标签固定宽 84px，值 flex:1 并在超长时省略。
 *  固定宽是这一排的读法所在 —— 六个字段的标签左对齐成一条竖线，
 *  眼睛才能横着扫下来。完整值在 title 里，改值走「编辑」。 */
function KvRow({
  k,
  title,
  children,
}: {
  k: string
  title?: string
  children: ReactNode
}) {
  return (
    <div className="border-separator flex min-w-0 items-center gap-3 border-b py-1.5 last:border-b-0">
      <span className="text-muted w-[84px] shrink-0 text-[12.5px]">{k}</span>
      <span title={title} className="min-w-0 flex-1 truncate text-[13px]">
        {children}
      </span>
    </div>
  )
}

function PriceField({
  label,
  value,
  onChange,
}: {
  label: string
  value: string
  onChange: (v: string) => void
}) {
  return (
    <TextField aria-label={label} value={value} onChange={onChange}>
      <Label>{label}</Label>
      <Input type="number" min={0} step="any" placeholder="未配置" />
    </TextField>
  )
}
