import { useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
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
  Switch,
  Table,
  TextField,
  toast,
} from '@heroui/react'
import { api } from '../api'
import { NETWORK_DIRECT_ID, useNetworks } from '../useNetworks'
import { PROTOCOL_OPTIONS, protocolLabel } from '../protocols'
import { PageHeader } from '../ui/PageHeader'
import { DataTable } from '../ui/DataTable'
import { AppModal } from '../ui/AppModal'
import { StatusChip } from '../ui/StatusChip'
import type { ProtocolProbeEntry } from '../types'
import { confirmBox } from '../ui/confirm'
import type { Provider } from '../types'

const is401 = (e: unknown): boolean => (e as { status?: number }).status === 401

/**
 * 上游服务页（语义对齐旧 Providers.vue）：搜索 + 状态筛选 + 列表 +
 * 新建弹窗 + 行内启停/删除。列表仅取摘要，凭据、模型与余额请求留在
 * 上游详情页。q/status 都在 URL query 上，刷新与分享不丢口径。
 */

const STATUS_OPTIONS = [
  { key: '', label: '全部状态' },
  { key: 'ready', label: '凭据就绪' },
  { key: 'unready', label: '未就绪' },
  { key: 'disabled', label: '已停用' },
  { key: 'unknown', label: '状态未知' },
]

function providerState(p: Provider): 'ready' | 'unready' | 'disabled' | 'unknown' {
  return !p.enabled ? 'disabled' : p.ready === false ? 'unready' : p.ready === true ? 'ready' : 'unknown'
}

function readyLabel(p: Provider): string {
  if (!p.enabled) return '已停用'
  return p.ready === true ? '凭据就绪' : p.ready === false ? '未就绪' : '状态未知'
}

function readyTone(p: Provider): 'ok' | 'err' | 'dim' {
  if (!p.enabled) return 'dim'
  return p.ready === true ? 'ok' : p.ready === false ? 'err' : 'dim'
}

function isDirectProvider(p: Provider): boolean {
  return !p.network_id || p.network_id === NETWORK_DIRECT_ID
}

export function Providers() {
  const [searchParams, setSearchParams] = useSearchParams()
  const navigate = useNavigate()

  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [providers, setProviders] = useState<Provider[]>([])
  const [pending, setPending] = useState<Set<string>>(new Set())
  const [createOpen, setCreateOpen] = useState(false)
  const loadSeqRef = useRef(0)

  // 网络清单不参与 load() 的成败判定：它只是展示辅助（行内的网络名与
  // 新建表单的下拉），读不到时那两处退化即可，不该让整页变成错误态。
  const { networks, load: loadNetworks } = useNetworks()

  const q = searchParams.get('q') ?? ''
  const status = searchParams.get('status') ?? ''

  // 搜索框走本地草稿 + 200ms 防抖回写 URL，避免每敲一键压一条历史。
  const [searchDraft, setSearchDraft] = useState(q)
  const debounceRef = useRef<number | undefined>(undefined)
  useEffect(() => {
    setSearchDraft(q)
  }, [q])
  useEffect(() => {
    if (searchDraft === q) return
    window.clearTimeout(debounceRef.current)
    debounceRef.current = window.setTimeout(() => {
      setSearchParams((prev) => {
        const next = new URLSearchParams(prev)
        if (searchDraft) next.set('q', searchDraft)
        else next.delete('q')
        return next
      })
    }, 200)
    return () => window.clearTimeout(debounceRef.current)
  }, [searchDraft, q, setSearchParams])

  const networkLabel = useMemo(() => {
    const map: Record<string, string> = {}
    for (const n of networks) map[n.id] = n.name
    return (id: string) => map[id] ?? id.slice(0, 8)
  }, [networks])

  const filtered = useMemo(
    () =>
      providers.filter((p) => {
        const needle = q.trim().toLowerCase()
        const matches =
          !needle ||
          [
            p.name,
            p.slug,
            p.endpoint,
            p.protocol,
            protocolLabel(p.protocol),
            isDirectProvider(p) ? '直连' : `代理 ${networkLabel(p.network_id)}`,
          ].some((v) => v.toLowerCase().includes(needle))
        return matches && (!status || status === providerState(p))
      }),
    [providers, q, status, networkLabel],
  )

  const load = async () => {
    const seq = ++loadSeqRef.current
    setLoading(true)
    setErr('')
    try {
      const result = await api.providers()
      if (seq === loadSeqRef.current) setProviders(result)
    } catch (e) {
      if (seq !== loadSeqRef.current) return
      if (is401(e)) return
      // 失败必须留下可见错误态：呈现成「暂无数据」会让运维误判为无流量，
      // 从而排除掉网关/上游故障这个真正的方向。
      setErr('加载上游失败：' + (e as Error).message)
      toast.danger('加载失败：' + (e as Error).message)
    } finally {
      if (seq === loadSeqRef.current) setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    void loadNetworks()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const openCreate = () => {
    setCreateOpen(true)
    void loadNetworks()
  }

  const clearFilters = () => {
    setSearchDraft('')
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev)
      next.delete('q')
      next.delete('status')
      return next
    })
  }

  const setPendingId = (id: string, on: boolean) => {
    setPending((prev) => {
      const next = new Set(prev)
      if (on) next.add(id)
      else next.delete(id)
      return next
    })
  }

  const goDetail = (p: Provider) => {
    navigate({ pathname: `/providers/${p.id}`, search: searchParams.toString() })
  }

  const toggleProvider = async (p: Provider) => {
    if (pending.has(p.id)) return
    setPendingId(p.id, true)
    try {
      // 只发改动字段：其余字段后端保持原值（PATCH 是字段级部分更新）。
      await api.updateProvider(p.id, { enabled: !p.enabled })
      toast.success(!p.enabled ? '已启用' : '已停用')
      await load()
    } catch (e) {
      if (!is401(e)) toast.danger('操作失败：' + (e as Error).message)
    } finally {
      setPendingId(p.id, false)
    }
  }

  const removeProvider = async (p: Provider) => {
    if (pending.has(p.id)) return
    const ok = await confirmBox({
      title: `删除上游「${p.name}」？`,
      body: '将同时影响其下模型与路由的可用性；关联数据不会自动清理。',
      danger: true,
      confirmLabel: '删除',
    })
    if (!ok || pending.has(p.id)) return
    setPendingId(p.id, true)
    try {
      await api.deleteProvider(p.id)
      toast.success('已删除')
      await load()
    } catch (e) {
      if (!is401(e)) toast.danger('删除失败：' + (e as Error).message)
    } finally {
      setPendingId(p.id, false)
    }
  }

  return (
    <>
      <PageHeader
        title="供应商"
        actions={
          <>
            <Button variant="secondary" isDisabled={loading} onPress={() => void load()}>
              刷新
            </Button>
            <Button variant="primary" onPress={openCreate}>
              新建上游
            </Button>
          </>
        }
      />

      <div className="mb-4 flex flex-wrap items-center gap-3">
        <TextField aria-label="搜索上游" value={searchDraft} onChange={setSearchDraft} className="min-w-[220px] flex-1">
          <Input type="search" placeholder="搜索名称、类型、网络或 Endpoint" autoComplete="off" />
        </TextField>
        <Select
          aria-label="上游状态"
          className="w-[150px]"
          selectedKey={status}
          onSelectionChange={(key) => {
            if (key === null) return
            setSearchParams((prev) => {
              const next = new URLSearchParams(prev)
              if (key) next.set('status', String(key))
              else next.delete('status')
              return next
            })
          }}
        >
          <SelectTrigger>
            <SelectValue />
            <SelectIndicator />
          </SelectTrigger>
          <SelectPopover>
            <ListBox aria-label="上游状态">
              {STATUS_OPTIONS.map((o) => (
                <ListBoxItem key={o.key} id={o.key}>
                  {o.label}
                </ListBoxItem>
              ))}
            </ListBox>
          </SelectPopover>
        </Select>
        <span className="text-xs text-muted">
          {filtered.length} / {providers.length} 个上游
        </span>
      </div>

      <DataTable
        label="上游列表"
        loading={loading}
        error={err}
        hasData={!!providers.length}
        empty={!filtered.length}
        onRetry={() => void load()}
        emptyText={
          providers.length ? '没有匹配的上游' : '尚未接入上游。新建后添加凭据与模型，再配置路由。'
        }
        emptyAction={
          providers.length ? (
            <Button variant="tertiary" size="sm" onPress={clearFilters}>
              清除筛选
            </Button>
          ) : null
        }
        footer={
          <p className="text-muted text-xs">
            列表只取摘要；凭据、模型与余额请求在 <b>配置</b> 里逐个上游管理。
          </p>
        }
      >
        <Table aria-label="上游列表">
          <Table.ScrollContainer>
            <Table.Content className="min-w-[920px]">
              <Table.Header>
                <Table.Column isRowHeader>上游 / Endpoint</Table.Column>
                <Table.Column>类型</Table.Column>
                <Table.Column>网络状态</Table.Column>
                <Table.Column>配置状态</Table.Column>
                <Table.Column>模型 / 凭据</Table.Column>
                <Table.Column>操作</Table.Column>
                <Table.Column>启用</Table.Column>
              </Table.Header>
              <Table.Body>
                {filtered.map((p) => (
                  <Table.Row key={p.id}>
                    <Table.Cell>
                      <Button
                        variant="ghost"
                        aria-label={`查看 ${p.name} 详情`}
                        onPress={() => goDetail(p)}
                        className="block h-auto w-full px-2 py-1 text-left"
                      >
                        <span className="block font-medium">{p.name}</span>
                        <span className="text-muted block font-mono text-xs">{p.endpoint}</span>
                      </Button>
                    </Table.Cell>
                    <Table.Cell>{protocolLabel(p.protocol)}</Table.Cell>
                    <Table.Cell>
                      {isDirectProvider(p) ? (
                        <StatusChip tone="dim" title="直接连接，不经过代理">
                          直连
                        </StatusChip>
                      ) : (
                        <StatusChip tone="info" title={networkTitle(p, networkLabel)}>
                          代理 · {networkLabel(p.network_id)}
                        </StatusChip>
                      )}
                    </Table.Cell>
                    <Table.Cell>
                      <span title={p.ready === false ? p.ready_reason || '未建立可用凭据池，请检查凭据' : p.ready === true ? undefined : '暂未取得运行时状态'}>
                        <StatusChip tone={readyTone(p)}>{readyLabel(p)}</StatusChip>
                      </span>
                      {p.ready === false ? (
                        <span className="text-muted mt-0.5 block max-w-[220px] truncate text-xs">
                          {p.ready_reason || '未建立可用凭据池，请检查凭据'}
                        </span>
                      ) : null}
                    </Table.Cell>
                    <Table.Cell className="text-right">
                      <span className="font-mono text-xs">
                        {p.model_count ?? '—'} / {p.cred_count ?? '—'}
                      </span>
                    </Table.Cell>
                    <Table.Cell>
                      <div className="flex items-center gap-1">
                        <Button variant="tertiary" size="sm" onPress={() => goDetail(p)}>
                          配置
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          isDisabled={pending.has(p.id)}
                          onPress={() => void removeProvider(p)}
                        >
                          删除
                        </Button>
                      </div>
                    </Table.Cell>
                    <Table.Cell>
                      <Switch
                        isSelected={p.enabled}
                        isDisabled={pending.has(p.id)}
                        aria-label={`启用 ${p.name}`}
                        onChange={() => void toggleProvider(p)}
                      >
                        <Switch.Control>
                          <Switch.Thumb />
                        </Switch.Control>
                      </Switch>
                    </Table.Cell>
                  </Table.Row>
                ))}
              </Table.Body>
            </Table.Content>
          </Table.ScrollContainer>
        </Table>
      </DataTable>

      <CreateProviderModal
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        networks={networks}
        onCreated={async () => {
          setCreateOpen(false)
          await load()
        }}
      />
    </>
  )
}

function networkTitle(p: Provider, label: (id: string) => string): string {
  if (isDirectProvider(p)) return '直接连接，不经过代理'
  return p.proxy_url ? `代理地址：${p.proxy_url}` : `代理：${label(p.network_id)}`
}

function CreateProviderModal({
  open,
  onClose,
  networks,
  onCreated,
}: {
  open: boolean
  onClose: () => void
  networks: { id: string; name: string; proxy_url: string }[]
  onCreated: () => Promise<void> | void
}) {
  const [saving, setSaving] = useState(false)
  const [name, setName] = useState('')
  const [protocol, setProtocol] = useState('chat')
  const [endpoint, setEndpoint] = useState('')
  const [apiKey, setApiKey] = useState('')
  // 默认直连：绝大多数上游不需要代理。存 NETWORK_DIRECT_ID 而不是空串，
  // 是为了让「下拉停在哪一项」与「提交的值」是同一个东西——空串在下拉里
  // 找不到对应 option，会显示成第一项却提交另一个值。
  const [networkId, setNetworkId] = useState(NETWORK_DIRECT_ID)
  const [formErr, setFormErr] = useState('')
  // 协议探测：结论是「这台上游支持哪几种协议」，不是错误。probing 期间禁用按钮，
  // 结果留在弹窗里直到用户改动 endpoint/key（改了结论就过期了）。
  const [probing, setProbing] = useState(false)
  const [probe, setProbe] = useState<ProtocolProbeEntry[] | null>(null)

  useEffect(() => {
    if (open) {
      setName('')
      setProtocol('chat')
      setEndpoint('')
      setApiKey('')
      setNetworkId(NETWORK_DIRECT_ID)
      setFormErr('')
      setProbe(null)
      setProbing(false)
    }
  }, [open])

  const probeProtocols = async () => {
    if (probing) return
    if (!endpoint.trim()) {
      setFormErr('先填 Endpoint 再探测')
      return
    }
    setProbing(true)
    setFormErr('')
    setProbe(null)
    try {
      const r = await api.probeProtocols({
        endpoint: endpoint.trim(),
        api_key: apiKey.trim(),
        network_id: networkId,
      })
      setProbe(r.results)
    } catch (e) {
      if (!is401(e)) setFormErr('探测失败：' + (e as Error).message)
    } finally {
      setProbing(false)
    }
  }

  const submit = async () => {
    if (saving) return
    if (!endpoint.trim()) {
      setFormErr('Endpoint 为必填项')
      return
    }
    setSaving(true)
    setFormErr('')
    try {
      await api.createProvider({
        name: name.trim(),
        protocol,
        endpoint: endpoint.trim(),
        api_key: apiKey.trim() || undefined,
        network_id: networkId,
      })
      toast.success('上游已创建')
      await onCreated()
    } catch (e) {
      if (!is401(e)) setFormErr('保存失败：' + (e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <AppModal
      open={open}
      onClose={onClose}
      title="新建上游"
      dismissable={false}
      maxWidth="640px"
      footer={
        <>
          <Button variant="ghost" isDisabled={saving} onPress={onClose}>
            取消
          </Button>
          <Button variant="primary" isDisabled={saving} onPress={() => void submit()}>
            {saving ? '保存中…' : '创建上游'}
          </Button>
        </>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void submit()
        }}
        className="grid grid-cols-2 gap-4"
      >
        <TextField aria-label="显示名称" value={name} onChange={setName}>
          <Label>显示名称</Label>
          <Input placeholder="DeepSeek（留空则用 Endpoint）" autoComplete="off" />
        </TextField>
        {/* Select 自带可见标签：只给 aria-label 时它没有 Label 占位，trigger 会
            顶到格子顶部，和左边带 Label 的输入框错开一行。 */}
        <div className="flex flex-col gap-1">
          <Label>接口方式</Label>
          <Select aria-label="接口方式" selectedKey={protocol} onSelectionChange={(key) => key !== null && setProtocol(String(key))}>
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
        </div>
        <TextField aria-label="Endpoint" isRequired value={endpoint} onChange={setEndpoint} className="col-span-2">
          <Label>Endpoint *</Label>
          <Input placeholder="https://api.deepseek.com" className="font-mono" autoComplete="off" />
        </TextField>
        <TextField aria-label="API Key" value={apiKey} onChange={setApiKey} className="col-span-2">
          <Label>API Key</Label>
          <Input type="password" placeholder="sk-...（可留空，稍后在详情页添加）" className="font-mono" autoComplete="new-password" />
          <span className="mt-1 block text-[11px] text-muted">
            添加即启用；slug、超时与重试将自动使用默认值。余额查询可在创建后到详情页设置。
          </span>
        </TextField>
        {/* 协议检测：填了 Endpoint（和 Key）之后可以先问一次，避免「建完发现
            选错协议」再回头删。结论按协议排成三个胶囊，颜色只表示「这台机器上
            有没有这个协议的入口」——黄/灰不代表这台上游不能用来配这个协议，
            只代表路径不通。 */}
        <div className="col-span-2 flex flex-col gap-1.5">
          <div className="flex flex-wrap items-center gap-2">
            <Button variant="outline" size="sm" isDisabled={probing || saving} onPress={() => void probeProtocols()}>
              {probing ? '探测中…' : '协议检测'}
            </Button>
            <span className="text-muted text-[11px]">用上面的 Endpoint 与 API Key 试三种协议，不消耗额度</span>
          </div>
          {probe ? (
            <div className="flex flex-wrap items-center gap-1.5" role="status">
              {probe.map((r) => (
                <span key={r.protocol} title={`${r.message}（HTTP ${r.status}）`}>
                  <StatusChip tone={probeTone(r)}>
                    {protocolLabel(r.protocol, r.protocol)}
                    {r.ok ? ' 可用' : ' 未发现'}
                  </StatusChip>
                </span>
              ))}
              {probe.some((r) => r.ok) ? (
                <Button
                  variant="ghost"
                  size="sm"
                  onPress={() => {
                    // 选中第一个可用的协议：探测就是为了省掉这一步猜。
                    const first = probe.find((r) => r.ok)
                    if (first) setProtocol(first.protocol)
                  }}
                >
                  用第一个可用的
                </Button>
              ) : (
                <span className="text-muted text-[11px]">三种都没找到入口，检查 Endpoint 是否少了 /v1 或多了路径</span>
              )}
            </div>
          ) : null}
        </div>
        <div className="col-span-2 flex flex-col gap-1">
          <Label>代理</Label>
          <Select
            aria-label="代理"
            selectedKey={networkId}
            onSelectionChange={(key) => key !== null && setNetworkId(String(key))}
          >
            <SelectTrigger>
              <SelectValue />
              <SelectIndicator />
            </SelectTrigger>
            <SelectPopover>
              <ListBox aria-label="出站网络">
                <ListBoxItem id={NETWORK_DIRECT_ID}>直连（默认，不经过代理）</ListBoxItem>
                {networks.map((n) => (
                  <ListBoxItem key={n.id} id={n.id}>
                    {n.name} — {n.proxy_url}
                  </ListBoxItem>
                ))}
              </ListBox>
            </SelectPopover>
          </Select>
        </div>
        <p className="col-span-2 text-[11px] leading-relaxed text-muted">
          默认<b>直连</b>，大多数上游不用改。要走代理就先去 <a className="text-link" href="/networks">代理</a>{' '}
          建一条（地址填一次到处可用），再回来从下拉里选——不用在这里手填 URL，写错不会报错只是连不上。
        </p>
        {formErr ? (
          <div className="col-span-2 text-danger text-xs" role="alert">
            {formErr}
          </div>
        ) : null}
      </form>
    </AppModal>
  )
}

/**
 * 探测结果的胶囊配色。ok → 绿；路径存在但鉴权没过（401/403）→ 黄：
 * 「这台机器有这个协议，钥匙不对」和「没有这个协议」是两种处置。
 */
function probeTone(r: ProtocolProbeEntry): 'ok' | 'warn' | 'dim' {
  if (r.ok && (r.status === 401 || r.status === 403)) return 'warn'
  return r.ok ? 'ok' : 'dim'
}
