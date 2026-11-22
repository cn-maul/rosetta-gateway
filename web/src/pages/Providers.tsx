import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
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
import { SectionCard } from '../ui/SectionCard'
import { StatusChip } from '../ui/StatusChip'
import { CHART_COLORS, ChartEmpty, ChartTooltipBox, TooltipRow, shareText } from '../ui/chart'
import { rangeBounds } from '../range'
import { fmtNum, fmtTokens, fmtPercent } from '../fmt'
import { Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import type { ProtocolProbeEntry, UsageGroupEntry } from '../types'
import { confirmBox } from '../ui/confirm'
import type { Provider } from '../types'

const is401 = (e: unknown): boolean => (e as { status?: number }).status === 401

/**
 * 上游服务页（语义对齐旧 Providers.vue）：搜索 + 状态筛选 + 列表 +
 * 新建弹窗 + 行内启停/删除。列表仅取摘要，凭据与模型请求留在
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

function readyTone(p: Provider): 'ok' | 'warn' | 'err' | 'dim' {
  if (!p.enabled) return 'dim'
  return p.ready === true ? 'ok' : p.ready === false ? 'err' : 'warn'
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
  // 近 7 天各上游的 Token 占比：列表只回答「配了什么」，
  // 这张图回答「流量实际压在谁身上」。单独请求且失败静默降级 ——
  // 它是辅助判断，不该让整张上游表变成错误态。
  const [mix, setMix] = useState<UsageGroupEntry[] | null>(null)
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
    const { from, to } = rangeBounds(7)
    api
      .usageByProvider(from, to, 50)
      .then((page) => setMix(page.records))
      .catch(() => setMix(null))
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
            列表只取摘要；凭据与模型请求在 <b>配置</b> 里逐个上游管理。
          </p>
        }
      >
        <Table aria-label="上游列表">
          <Table.ScrollContainer>
            <Table.Content className="min-w-[760px]">
              <Table.Header>
                <Table.Column isRowHeader className="whitespace-nowrap">上游 / Endpoint</Table.Column>
                <Table.Column className="w-[96px] whitespace-nowrap">网络状态</Table.Column>
                <Table.Column className="min-w-[210px] whitespace-nowrap">凭据状态</Table.Column>
                <Table.Column className="w-[128px] whitespace-nowrap">操作</Table.Column>
                <Table.Column className="w-[64px] whitespace-nowrap">启用</Table.Column>
              </Table.Header>
              <Table.Body>
                {filtered.map((p) => (
                  <Table.Row key={p.id}>
                    <Table.Cell>
                      <Button
                        variant="ghost"
                        aria-label={`查看 ${p.name} 详情`}
                        onPress={() => goDetail(p)}
                        className="block h-auto w-full min-w-0 px-2 py-1 text-left"
                      >
                        <span className="block truncate font-medium">{p.name}</span>
                        <span className="text-muted block truncate font-mono text-xs">{p.endpoint}</span>
                        <span className="text-muted mt-0.5 block truncate text-xs">
                          {protocolLabel(p.protocol)} · {p.model_count ?? '—'} 模型 / {p.cred_count ?? '—'} 凭据
                        </span>
                      </Button>
                    </Table.Cell>
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
                      <StatusChip tone={readyTone(p)}>{readyLabel(p)}</StatusChip>
                      {p.ready === false ? (
                        <span className="text-muted mt-0.5 block max-w-[260px] text-xs leading-snug">
                          {p.ready_reason || '未建立可用凭据池，请检查凭据'}
                        </span>
                      ) : null}
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
                        <Switch.Content>
                          <Switch.Control>
                            <Switch.Thumb />
                          </Switch.Control>
                        </Switch.Content>
                      </Switch>
                    </Table.Cell>
                  </Table.Row>
                ))}
              </Table.Body>
            </Table.Content>
          </Table.ScrollContainer>
        </Table>
      </DataTable>

      <div className="mt-4">
        <SectionCard title="上游负载（近 7 天）" description="Token 占比与成功率，回答「流量实际压在谁身上」">
          <ProviderMix rows={mix} />
        </SectionCard>
      </div>

      <CreateProviderModal
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        networks={networks}
        onCreated={async () => {
          setCreateOpen(false)
        }}
        onOpenProvider={(p) => {
          goDetail(p)
        }}
      />
    </>
  )
}

/**
 * 上游负载：横向条形图只画 Token 占比，成功率放进提示框。
 *
 * 不把「请求数」和「Token」画成两组柱：两者常常同向，同向的两组柱只会
 * 让图变宽而不会多回答一个问题。这里要回答的是「谁占大头」，
 * 一个维度 + 提示框里的成功率足够了。
 */
function ProviderMix({ rows }: { rows: UsageGroupEntry[] | null }) {
  if (rows === null) return <ChartEmpty text="负载数据暂时不可用（不影响上面的配置列表）" />
  const top = [...rows]
    .filter((r) => r.tokens > 0)
    .sort((a, b) => b.tokens - a.tokens)
    .slice(0, 8)
  if (top.length === 0) return <ChartEmpty text="近 7 天还没有调用记录" />
  const total = rows.reduce((a, r) => a + r.tokens, 0)
  const share = top.reduce((a, r) => a + r.tokens, 0)
  const data = top.map((r) => ({ ...r, label: r.name || r.key || '未归属上游' }))
  const max = Math.max(...data.map((d) => d.tokens))
  return (
    <div>
      <div className="h-[220px]" role="img" aria-label={`近 7 天 Token 占比最高的 ${data.length} 个上游`}>
        <ResponsiveContainer width="100%" height="100%">
          <BarChart data={data} layout="vertical" margin={{ top: 2, right: 48, bottom: 2, left: 4 }}>
            <CartesianGrid stroke={CHART_COLORS.grid} strokeDasharray="3 5" horizontal={false} />
            <XAxis
              type="number"
              domain={[0, max * 1.1]}
              tick={{ fill: CHART_COLORS.muted, fontSize: 10.5 }}
              axisLine={false}
              tickLine={false}
              tickFormatter={(v: number) => fmtTokens(v)}
            />
            <YAxis
              type="category"
              dataKey="label"
              width={128}
              tick={{ fill: CHART_COLORS.foreground, fontSize: 11 }}
              axisLine={false}
              tickLine={false}
              tickFormatter={(value: string) => (value.length > 14 ? value.slice(0, 13) + '…' : value)}
            />
            <Tooltip
              cursor={{ fill: CHART_COLORS.accentSoft }}
              content={({ active, payload }) => {
                if (!active || !payload?.length) return null
                const row = payload[0].payload as (typeof data)[number]
                return (
                  <ChartTooltipBox title={row.label}>
                    <TooltipRow label="Token" value={fmtTokens(row.tokens)} />
                    <TooltipRow label="占比" value={shareText(row.tokens, total)} />
                    <TooltipRow label="请求" value={fmtNum(row.count)} />
                    <TooltipRow
                      label="成功率"
                      value={row.ok + row.err > 0 ? `${fmtPercent(row.ok / (row.ok + row.err))}%` : '—'}
                    />
                  </ChartTooltipBox>
                )
              }}
            />
            <Bar dataKey="tokens" fill={CHART_COLORS.accent} radius={[0, 4, 4, 0]} maxBarSize={18} />
          </BarChart>
        </ResponsiveContainer>
      </div>
      <p className="text-muted mt-2 text-[11.5px]">
        前 {data.length} 个上游占近 7 天 Token 的 {shareText(share, total)}；没有流量的上游不占位。
      </p>
    </div>
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
  onOpenProvider,
}: {
  open: boolean
  onClose: () => void
  networks: { id: string; name: string; proxy_url: string }[]
  onCreated: () => Promise<void> | void
  onOpenProvider: (provider: Provider) => void
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
      const created = await api.createProvider({
        name: name.trim(),
        protocol,
        endpoint: endpoint.trim(),
        api_key: apiKey.trim() || undefined,
        network_id: networkId,
      })
      toast.success('上游已创建，继续添加模型')
      void onCreated()
      onOpenProvider(created)
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
            {saving ? '保存中…' : '创建并配置'}
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
        <TextField aria-label="显示名称" value={name} onChange={setName} className="col-span-2">
          <Label>显示名称</Label>
          <Input placeholder="DeepSeek（留空则用 Endpoint）" autoComplete="off" />
        </TextField>
        <TextField aria-label="Endpoint" isRequired value={endpoint} onChange={setEndpoint} className="col-span-2">
          <Label>Endpoint *</Label>
          <Input placeholder="https://api.deepseek.com" className="font-mono" autoComplete="off" />
        </TextField>
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
          <Link to="/networks" className="text-link mt-1 text-[11px]">
            管理系统代理
          </Link>
        </div>
        <TextField aria-label="API Key" value={apiKey} onChange={setApiKey} className="col-span-2">
          <Label>API Key</Label>
          <Input type="password" placeholder="sk-...（可留空，稍后在详情页添加）" className="font-mono" autoComplete="new-password" />
          <span className="mt-1 block text-[11px] text-muted">将作为第一把凭据启用。</span>
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
            <span className="text-muted text-[11px]">使用上方地址、代理与密钥检测入口</span>
          </div>
          {probe ? (
            <div className="flex flex-wrap items-center gap-1.5" role="status">
              {probe.map((r) => (
                <span key={r.protocol} title={`${r.message}（HTTP ${r.status}）`}>
                  <StatusChip tone={probeTone(r)}>
                    {protocolLabel(r.protocol, r.protocol)}
                    {probeLabel(r)}
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
        {/* 协议放在探测之后：先确认真实入口，再决定用哪种接口方式。
            仍然保留手动选择，检测不可用时用户可以自行指定。 */}
        <div className="col-span-2 flex flex-col gap-1">
          <Label>接口方式</Label>
          <Select
            aria-label="接口方式"
            selectedKey={protocol}
            onSelectionChange={(key) => key !== null && setProtocol(String(key))}
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
          <span className="text-muted text-[11px]">检测后自动选中第一个可用入口，也可手动指定。</span>
        </div>
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

function probeLabel(r: ProtocolProbeEntry): string {
  if (!r.ok) return ' 未发现'
  if (r.status === 401 || r.status === 403) return ' 鉴权失败'
  return ' 入口可用'
}
