import { useEffect, useMemo, useRef, useState } from 'react'
import { Button, Spinner, toast } from '@heroui/react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { api } from '../api'
import { useNetworks } from '../useNetworks'
import { RequestGeneration } from '../asyncState'
import { PageHeader } from '../ui/PageHeader'
import { SectionCard } from '../ui/SectionCard'
import { confirmBox } from '../ui/confirm'
import { CredentialPanel } from '../components/CredentialPanel'
import { UpstreamModelPanel } from '../components/UpstreamModelPanel'
import { ProviderOverviewCard } from '../components/provider/ProviderOverviewCard'
import { ProviderEditModal } from '../components/provider/ProviderEditModal'
import { CredentialFormModal } from '../components/provider/CredentialFormModal'
import { ModelEditModal, ModelDiscoverModal } from '../components/provider/ModelEditModal'
import type {
  Provider,
  Credential,
  UpstreamModel,
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

function modelTestToast(m: UpstreamModel, r: ModelTestResult): string {
  if (r.status === 'ok') {
    const parts = [`${m.model_id} · 正常`, `${r.latency_ms}ms`]
    if (r.output_tokens) parts.push(`输出 ${r.output_tokens} tok`)
    return parts.join(' · ')
  }
  const base = r.in_band ? `${r.message}（上游以 HTTP 200 返回错误）` : r.message
  return `${m.model_id} · 测试失败：${r.hint ? `${base} → ${r.hint}` : base}`
}

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
    setCredModal({ open: false, editing: null })
    setModelModal({ open: false, editing: null })
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

  // ---------- 弹窗编排（表单与提交在各弹窗组件内部） ----------

  const [editOpen, setEditOpen] = useState(false)
  const [credModal, setCredModal] = useState<{ open: boolean; editing: Credential | null }>({
    open: false,
    editing: null,
  })
  const [modelModal, setModelModal] = useState<{ open: boolean; editing: UpstreamModel | null }>({
    open: false,
    editing: null,
  })

  // ---------- 凭据动作 ----------

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

  // ---------- 模型动作 ----------

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
    setModelModal({ open: true, editing: model })
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
  const editingModel = modelModal.editing

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
            <ProviderOverviewCard
              provider={provider}
              networks={networks}
              toggling={togglingProvider}
              onEdit={() => setEditOpen(true)}
              onRemove={() => void removeProvider()}
              onToggle={() => void toggleProvider()}
            />

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
              onAdd={() => setCredModal({ open: true, editing: null })}
              onQueryBalance={() => void queryBalance()}
              onEdit={(c) => setCredModal({ open: true, editing: c })}
              onRemove={(c) => void removeCred(c)}
              onToggle={(c) => void toggleCred(c)}
            />
          </div>

          <UpstreamModelPanel
            models={models}
            testing={testing}
            error={modelsErr}
            onRetry={() => void reloadSub(true)}
            onAdd={() => setModelModal({ open: true, editing: null })}
            onTest={(m) => void testModel(m)}
            onEdit={(m) => setModelModal({ open: true, editing: m })}
            onRemove={(m) => void removeModel(m)}
          />
        </>
      ) : null}

      <ProviderEditModal
        provider={provider!}
        open={editOpen && !!provider}
        networks={networks}
        onClose={() => setEditOpen(false)}
        onSaved={loadProviderOnly}
      />

      <CredentialFormModal
        providerId={provider?.id ?? idRef.current}
        editing={credModal.editing}
        open={credModal.open}
        onClose={() => setCredModal((f) => ({ ...f, open: false }))}
        onSaved={async () => {
          await Promise.all([reloadSub(), loadProviderOnly()])
        }}
      />

      {editingModel ? (
        <ModelEditModal
          original={editingModel}
          open={modelModal.open}
          onClose={() => setModelModal((f) => ({ ...f, open: false }))}
          onSaved={() => reloadSub()}
        />
      ) : (
        <ModelDiscoverModal
          providerId={provider?.id ?? idRef.current}
          open={modelModal.open && !editingModel}
          onClose={() => setModelModal((f) => ({ ...f, open: false }))}
          onSaved={async () => {
            // 概览的模型数来自列表接口的 model_count，所以必须刷新 provider，
            // 否则添加后这页的计数会停在旧值。
            await Promise.all([reloadSub(), loadProviderOnly()])
          }}
        />
      )}
    </div>
  )
}
