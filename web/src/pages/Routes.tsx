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
import { PageHeader } from '../ui/PageHeader'
import { DataTable } from '../ui/DataTable'
import { AppModal } from '../ui/AppModal'
import { confirmBox } from '../ui/confirm'
import type { RouteCatalogEntry, RouteCatalogProvider, RouteTargetInput, UpstreamModel } from '../types'

const is401 = (e: unknown): boolean => (e as { status?: number }).status === 401

/**
 * 模型与路由页（语义对齐旧 Routes.vue）：公开模型名 → 有序上游目标链。
 * 列表 + 筛选 + 行内启停/故障转移开关 + 编辑弹窗（链整体替换保存）。
 */

const STATUS_OPTIONS = [
  { key: '', label: '全部路由' },
  { key: 'enabled', label: '已启用' },
  { key: 'disabled', label: '已停用' },
  { key: 'failover', label: '已开启故障转移' },
]

interface TargetRow {
  provider_id: string
  upstream_model_id: string
  enabled: boolean
}

function blankTarget(): TargetRow {
  return { provider_id: '', upstream_model_id: '', enabled: true }
}

export function Routes() {
  const [searchParams, setSearchParams] = useSearchParams()
  const navigate = useNavigate()

  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [routes, setRoutes] = useState<RouteCatalogEntry[]>([])
  const [providers, setProviders] = useState<RouteCatalogProvider[]>([])
  // upstream_models.id（短哈希外键）→ 模型对象；Resolve 时拿的是这个外键
  const [mmap, setMmap] = useState<Record<string, UpstreamModel>>({})
  const [pending, setPending] = useState<Set<string>>(new Set())
  const [editor, setEditor] = useState<{ open: boolean; route?: RouteCatalogEntry }>({ open: false })
  const loadSeqRef = useRef(0)

  const q = searchParams.get('q') ?? ''
  const status = searchParams.get('status') ?? ''

  // 搜索框本地草稿 + 200ms 防抖回写 URL（与 Providers 同口径）。
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

  const providerMap = useMemo(() => {
    const m: Record<string, RouteCatalogProvider> = {}
    for (const p of providers) m[p.id] = p
    return m
  }, [providers])

  const modelsOf = (providerId: string): UpstreamModel[] =>
    Object.values(mmap).filter((m) => m.provider_id === providerId)

  // 兜底：外键悬空时至少显示路由自身信息。
  //
  // display_name 与 model_id 多数时候只差大小写（「GLM-5.3-Flash」vs
  // 「glm-5.3-flash」），无条件拼成 `名字 (id)` 只是把同一个词排两遍。只有
  // 两者确实不同（例如显示名取了中文别名）时才值得并排给出。
  const modelLabel = (r: RouteCatalogEntry): string => {
    const m = mmap[r.upstream_model_id]
    if (m) {
      const shown = (m.display_name ?? '').trim()
      if (shown && shown.toLowerCase() !== m.model_id.toLowerCase()) {
        return `${shown} (${m.model_id})`
      }
      return m.model_id
    }
    return r.upstream_model_id
  }

  const filtered = useMemo(
    () =>
      routes.filter((r) => {
        const needle = q.trim().toLowerCase()
        const matches =
          !needle ||
          [r.public_name, providerMap[r.provider_id]?.name ?? '', modelLabel(r)].some((v) =>
            v.toLowerCase().includes(needle),
          )
        return (
          matches &&
          (!status ||
            (status === 'enabled' && r.enabled) ||
            (status === 'disabled' && !r.enabled) ||
            (status === 'failover' && r.failover_enabled))
        )
      }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [routes, q, status, providerMap, mmap],
  )

  const clearFilters = () => {
    setSearchDraft('')
    setSearchParams((prev) => {
      const next = new URLSearchParams(prev)
      next.delete('q')
      next.delete('status')
      return next
    })
  }

  const load = async () => {
    const seq = ++loadSeqRef.current
    setLoading(true)
    setErr('')
    try {
      const catalog = await api.routeCatalog()
      if (seq !== loadSeqRef.current) return
      setRoutes(catalog.routes)
      setProviders(catalog.providers)
      setMmap(Object.fromEntries(catalog.models.map((m) => [m.id, m])))
    } catch (e) {
      if (seq !== loadSeqRef.current) return
      if (is401(e)) return
      // 失败**必须留下可见的错误态**：只弹 toast 的话，页面保留空列表，
      // 呈现成「暂无数据」—— 而真实原因是请求失败了。运维会据此判断
      // 「今天没有流量」，进而排除掉网关/上游故障这个真正的方向。
      setErr('加载路由失败：' + (e as Error).message)
      toast.danger('加载失败：' + (e as Error).message)
    } finally {
      if (seq === loadSeqRef.current) setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const setPendingId = (id: string, on: boolean) => {
    setPending((prev) => {
      const next = new Set(prev)
      if (on) next.add(id)
      else next.delete(id)
      return next
    })
  }

  const toggleRoute = async (r: RouteCatalogEntry) => {
    if (pending.has(r.id)) return
    setPendingId(r.id, true)
    try {
      // 只发要改的那一个字段。PATCH 是字段级部分更新，回传整个对象有两个代价：
      //   1. r 来自列表快照，可能已被别的标签页改过 —— 全量回写会静默回滚那些字段；
      //   2. 回传 provider_id/upstream_model_id 会顺带触发一次「把链首对齐到主目标」，
      //      而启停一个路由本不该动它的目标链。
      await api.updateRoute(r.id, { enabled: !r.enabled })
      toast.success(!r.enabled ? '已启用' : '已停用')
      await load()
    } catch (e) {
      if (!is401(e)) toast.danger('操作失败：' + (e as Error).message)
    } finally {
      setPendingId(r.id, false)
    }
  }

  const toggleFailover = async (r: RouteCatalogEntry) => {
    if (pending.has(r.id)) return
    setPendingId(r.id, true)
    try {
      const next = !r.failover_enabled
      await api.updateRoute(r.id, { failover_enabled: next })
      toast.success(next ? '已开启故障转移' : '已关闭故障转移')
      await load()
    } catch (e) {
      if (!is401(e)) toast.danger('操作失败：' + (e as Error).message)
    } finally {
      setPendingId(r.id, false)
    }
  }

  const removeRoute = async (r: RouteCatalogEntry) => {
    if (pending.has(r.id)) return
    const ok = await confirmBox({
      title: `删除路由「${r.public_name}」？`,
      body: '删除后该公开模型名立即不可用（客户端会收到 404 model_not_found）。',
      danger: true,
      confirmLabel: '删除',
    })
    if (!ok || pending.has(r.id)) return
    setPendingId(r.id, true)
    try {
      await api.deleteRoute(r.id)
      toast.success('已删除')
      await load()
    } catch (e) {
      if (!is401(e)) toast.danger('删除失败：' + (e as Error).message)
    } finally {
      setPendingId(r.id, false)
    }
  }

  return (
    <>
      <PageHeader
        title="路由"
        actions={
          <>
            <Button variant="secondary" isDisabled={loading} onPress={() => void load()}>
              刷新
            </Button>
            <Button variant="primary" onPress={() => setEditor({ open: true })}>
              新建路由
            </Button>
          </>
        }
      />

      <div className="mb-4 flex flex-wrap items-center gap-3">
        <TextField aria-label="搜索路由" value={searchDraft} onChange={setSearchDraft} className="min-w-[220px] flex-1">
          <Input type="search" placeholder="搜索路由名、当前模型或上游" autoComplete="off" />
        </TextField>
        <Select
          aria-label="路由状态"
          className="w-[170px]"
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
            <ListBox aria-label="路由状态">
              {STATUS_OPTIONS.map((o) => (
                <ListBoxItem key={o.key} id={o.key}>
                  {o.label}
                </ListBoxItem>
              ))}
            </ListBox>
          </SelectPopover>
        </Select>
        <span className="text-muted text-xs">
          {filtered.length} / {routes.length} 条路由
        </span>
      </div>

      <DataTable
        label="路由列表"
        loading={loading}
        error={err}
        hasData={!!routes.length}
        empty={!filtered.length}
        onRetry={() => void load()}
        emptyText={
          routes.length
            ? '没有匹配的路由'
            : '尚未配置路由。先接入上游、添加凭据与模型，再创建路由供客户端调用。'
        }
        emptyAction={
          routes.length ? (
            <Button variant="tertiary" size="sm" onPress={clearFilters}>
              清除筛选
            </Button>
          ) : (
            <Button variant="tertiary" size="sm" onPress={() => navigate('/providers')}>
              接入上游
            </Button>
          )
        }
      >
        <Table aria-label="路由列表">
          <Table.ScrollContainer>
            <Table.Content className="min-w-[880px]">
              <Table.Header>
                <Table.Column isRowHeader>路由名</Table.Column>
                <Table.Column>当前服务模型</Table.Column>
                <Table.Column>上游数</Table.Column>
                <Table.Column>操作</Table.Column>
                <Table.Column>故障转移</Table.Column>
                <Table.Column>启用</Table.Column>
              </Table.Header>
              <Table.Body>
                {filtered.map((r) => {
                  const issue = targetIssue(providerMap, mmap, r.provider_id, r.upstream_model_id)
                  return (
                    <Table.Row key={r.id}>
                      <Table.Cell>
                        <span className="font-mono text-[13px]">{r.public_name}</span>
                      </Table.Cell>
                      <Table.Cell>
                        <div>{providerMap[r.provider_id]?.name ?? r.provider_id}</div>
                        <div className="text-muted font-mono text-xs">{modelLabel(r)}</div>
                        {issue ? (
                          <p className="bg-warning-soft text-warning-soft-foreground mt-1 rounded px-2 py-1 text-xs">
                            {issue}
                          </p>
                        ) : null}
                      </Table.Cell>
                      <Table.Cell className="text-right">{r.chain.total} 个</Table.Cell>
                      <Table.Cell>
                        <div className="flex items-center gap-1">
                          <Button variant="tertiary" size="sm" onPress={() => setEditor({ open: true, route: r })}>
                            编辑
                          </Button>
                          <Button variant="ghost" size="sm" isDisabled={pending.has(r.id)} onPress={() => void removeRoute(r)}>
                            删除
                          </Button>
                        </div>
                      </Table.Cell>
                      <Table.Cell>
                        <Switch
                          isSelected={r.failover_enabled}
                          isDisabled={pending.has(r.id)}
                          aria-label={`${r.failover_enabled ? '关闭' : '开启'} ${r.public_name} 的故障转移`}
                          onChange={() => void toggleFailover(r)}
                        >
                          <Switch.Content>
                            <Switch.Control>
                              <Switch.Thumb />
                            </Switch.Control>
                          </Switch.Content>
                        </Switch>
                      </Table.Cell>
                      <Table.Cell>
                        <Switch
                          isSelected={r.enabled}
                          isDisabled={pending.has(r.id)}
                          aria-label={`${r.enabled ? '停用' : '启用'} ${r.public_name}`}
                          onChange={() => void toggleRoute(r)}
                        >
                          <Switch.Content>
                            <Switch.Control>
                              <Switch.Thumb />
                            </Switch.Control>
                          </Switch.Content>
                        </Switch>
                      </Table.Cell>
                    </Table.Row>
                  )
                })}
              </Table.Body>
            </Table.Content>
          </Table.ScrollContainer>
        </Table>
      </DataTable>

      <RouteEditor
        key={editor.route?.id ?? 'new'}
        open={editor.open}
        route={editor.route}
        providers={providers}
        mmap={mmap}
        onClose={() => setEditor({ open: false })}
        onSaved={async () => {
          setEditor({ open: false })
          await load()
        }}
      />
    </>
  )
}

// 与后端 Resolve 的候选过滤保持一致：只看目标与上游是否启用。
// 凭据暂时未就绪仍可能是候选，不应因此把高亮跳到下一行。
function targetRoutable(
  providerMap: Record<string, RouteCatalogProvider>,
  mmap: Record<string, UpstreamModel>,
  providerId: string,
  modelId: string,
): boolean {
  const p = providerMap[providerId]
  const m = mmap[modelId]
  return !!p?.enabled && !!m
}

// 「这一行还不能上线」的原因。空串 = 没问题。
//
// 空 provider / 空模型是**尚未填完**，不是「上游不存在」：新建路由时第一行
// 本来就是空白的，直接按查表落空来判会显示「上游不存在」——读起来像上游被
// 删了，而实际上只是还没选。两者要分开：没填是提示（中性），填了却查不到
// 才是异常（告警）。
function targetIssue(
  providerMap: Record<string, RouteCatalogProvider>,
  mmap: Record<string, UpstreamModel>,
  providerId: string,
  modelId: string,
): string {
  if (!providerId) return ''
  const p = providerMap[providerId]
  if (!p) return '上游不存在'
  if (!p.enabled) return '上游已停用'
  if (p.ready === false) return p.ready_reason || '上游凭据未就绪'
  if (!modelId) return ''
  const m = mmap[modelId]
  if (!m) return '上游模型不存在'
  if (typeof p.ready !== 'boolean') return '凭据状态未知'
  return ''
}

function RouteEditor({
  open,
  route,
  providers,
  mmap,
  onClose,
  onSaved,
}: {
  open: boolean
  route?: RouteCatalogEntry
  providers: RouteCatalogProvider[]
  mmap: Record<string, UpstreamModel>
  onClose: () => void
  onSaved: () => Promise<void> | void
}) {
  const [saving, setSaving] = useState(false)
  const [publicName, setPublicName] = useState('')
  const [failoverEnabled, setFailoverEnabled] = useState(false)
  const [targets, setTargets] = useState<TargetRow[]>([])
  const [enabled, setEnabled] = useState(true)

  // 目标链加载失败的说明。非空 = 禁止保存。
  //
  // 为什么不能像别的展示字段那样「加载失败就留空」：保存走的是 PUT 整体替换
  // （按数组顺序重排 position），一份伪造出来的单目标链写回去，等于把 position 1..N
  // 全部删掉 —— 界面显示「保存成功」，实际故障转移链已经没了，且没有撤销路径。
  const [chainError, setChainError] = useState('')
  const [saveError, setSaveError] = useState('')

  /**
   * 目标链的两个「能不能保存」状态，缺一不可：
   *   - chainLoading：链是**异步**取的。打开弹窗到响应回来这段窗口里，表单里
   *     只有一条由 route 主目标列拼出来的占位行 —— 此时放行保存，等于拿一份
   *     伪造的单目标链去做 PUT 整体替换。
   *   - chainReady：只有「本次打开的链确实取到了」才为真。加载失败（chainError）
   *     和「会话已失效」都停在假，永不放行。
   * 光有 chainError 不够（它覆盖不到加载中），光有 chainLoading 也不够（加载
   * 结束后失败态会把它清回 false，继而放行）。两者叠起来才是「安全」。
   */
  const [chainLoading, setChainLoading] = useState(false)
  const [chainReady, setChainReady] = useState(false)

  /**
   * 打开弹窗的请求序号。连点两条路由的「编辑」会并发两个 routeTargets 请求，
   * 响应顺序不保证与点击顺序一致；没有序号时，慢到的旧响应会最后写入
   * targets —— 串了的表单一旦保存，PUT 会把 B 的链整体写到 A 上。
   */
  const chainSeqRef = useRef(0)

  useEffect(() => {
    if (!open) return
    const seq = ++chainSeqRef.current
    setPublicName(route?.public_name ?? '')
    setFailoverEnabled(route?.failover_enabled ?? false)
    setEnabled(route?.enabled ?? true)
    setTargets([])
    setChainError('')
    setSaveError('')
    setChainReady(false)
    setChainLoading(false)
    if (route) {
      setChainLoading(true)
      void (async () => {
        try {
          const ts = await api.routeTargets(route.id)
          if (seq !== chainSeqRef.current) return // 期间又打开了别的路由，本响应已过时，丢弃
          setTargets(
            ts.length
              ? [...ts]
                  .sort((a, b) => a.position - b.position)
                  .map((t) => ({ provider_id: t.provider_id, upstream_model_id: t.upstream_model_id, enabled: t.enabled }))
              : [{ provider_id: route.provider_id, upstream_model_id: route.upstream_model_id, enabled: true }],
          )
          setChainReady(true)
        } catch (e) {
          if (seq !== chainSeqRef.current) return
          if (is401(e)) {
            // 401 只代表会话失效。这里必须**关掉弹窗**：留在打开状态的话，屏幕
            // 停在一份永远加载不出来的表单上，而真正的原因只在 toast 里。
            onClose()
            return
          }
          // 载荷仍是「主目标单行」，但只用于展示；chainReady 保持假，保存被拦住。
          setChainError(
            '无法读取该路由的上游链：' +
              (e as Error).message +
              '。为避免用不完整的数据覆盖已有配置，保存已被禁用 —— 请先刷新页面重试。',
          )
          setTargets([{ provider_id: route.provider_id, upstream_model_id: route.upstream_model_id, enabled: true }])
        } finally {
          // 只清自己那一次的加载态：被丢弃的旧响应不能把新一轮的加载态提前抹掉。
          if (seq === chainSeqRef.current) setChainLoading(false)
        }
      })()
    } else {
      // 新建：链由用户现场填，没有「读回来」这一步，直接算就绪。
      setTargets([blankTarget()])
      setChainReady(true)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, route])

  // 链未就绪期间禁止一切改动与保存（覆盖「加载中」与「加载失败」两种情形）
  const chainBlocked = chainLoading || !chainReady || saving

  const modelsOf = (providerId: string): UpstreamModel[] =>
    Object.values(mmap).filter((m) => m.provider_id === providerId)

  // 当前请求会优先命中的目标。后端按 position 过滤掉停用的上游/模型后，
  // 取候选链首；故障转移开启时它仍是首个尝试目标，后续目标只在失败时接管。
  const providerMap = providerMapOf(providers)
  const activeTargetIndex = !enabled
    ? -1
    : targets.findIndex((t) => t.enabled && targetRoutable(providerMap, mmap, t.provider_id, t.upstream_model_id))

  const addTarget = () => setTargets((prev) => [...prev, blankTarget()])

  const removeTarget = (i: number) => {
    if (targets.length <= 1) {
      toast.danger('至少保留一个上游目标')
      return
    }
    setTargets((prev) => prev.filter((_, idx) => idx !== i))
  }

  const moveTarget = (i: number, dir: -1 | 1) => {
    const j = i + dir
    if (j < 0 || j >= targets.length) return
    setTargets((prev) => {
      const next = [...prev]
      const [row] = next.splice(i, 1)
      next.splice(j, 0, row)
      return next
    })
  }

  /**
   * 一键把某项置顶到链首（2026-10-14）。
   * 「未开故障转移」时请求**只打链首**，所以要换掉当前生效的上游，
   * 本质就是「把另一个模型挪到 position 0」。链长的时候靠上移要连点好几次，
   * 而这是个高频动作（上游挂了/换了供应商就切一次），值得一个专门的按钮。
   */
  const pinTarget = (i: number) => {
    if (i <= 0 || i >= targets.length) return
    setTargets((prev) => {
      const next = [...prev]
      const [row] = next.splice(i, 1)
      next.unshift(row)
      return next
    })
  }

  const patchTarget = (i: number, patch: Partial<TargetRow>) => {
    setTargets((prev) => prev.map((t, idx) => (idx === i ? { ...t, ...patch } : t)))
  }

  // 切 provider 后原模型可能不属于它，清空让用户重选
  const onTargetProviderChange = (i: number, providerId: string) => {
    const cur = targets[i]
    const m = mmap[cur.upstream_model_id]
    patchTarget(i, {
      provider_id: providerId,
      upstream_model_id: !m || m.provider_id !== providerId ? '' : cur.upstream_model_id,
    })
  }

  const submit = async () => {
    if (chainLoading) {
      toast.danger('上游链仍在加载，请稍候')
      return
    }
    if (!chainReady) {
      // 与 chainError 同因：链没读回来就保存 = 用一份伪造的单目标链整体替换。
      // chainError 非空时它已经把原因写在弹窗里了，这里只补一句动作被拦下。
      toast.danger('上游链未成功加载，已禁止保存（请先刷新页面）')
      return
    }
    if (saving) return
    if (!publicName.trim()) {
      toast.danger('公开模型名为必填项')
      return
    }
    if (targets.length === 0 || targets.some((t) => !t.provider_id || !t.upstream_model_id)) {
      toast.danger('每个上游目标都要选好 provider 与模型')
      return
    }

    // 链首即 route 的主目标列（后端 Create/Update 仍要求这两列非空）。
    const head = targets[0]
    const routeBody = {
      public_name: publicName.trim(),
      provider_id: head.provider_id,
      upstream_model_id: head.upstream_model_id,
      enabled,
      failover_enabled: failoverEnabled,
    }
    const chain: RouteTargetInput[] = targets.map((t) => ({
      provider_id: t.provider_id,
      upstream_model_id: t.upstream_model_id,
      enabled: t.enabled,
    }))

    let isNew = false
    let routeSaved = false
    setSaveError('')
    setSaving(true)
    try {
      let routeId = route?.id ?? ''
      if (routeId) {
        await api.updateRoute(routeId, routeBody)
      } else {
        const created = await api.createRoute(routeBody)
        routeId = created.id
        isNew = true
        // 走到这里 route 已经在库里了（后端建 route 时会种一条链首目标）。
        // 后面若写链失败，必须如实说「已建但链不完整」，不能报一句「保存失败」
        // 让用户以为什么都没发生 —— 他会再点一次「新建」，然后撞 public_name 冲突。
      }
      routeSaved = true
      // 目标链独立于 route 基础字段整体替换（按当前顺序重排 position）。
      await api.saveRouteTargets(routeId, chain)

      toast.success(isNew ? '路由已创建' : '路由已更新')
      await onSaved()
    } catch (e) {
      if (!is401(e)) {
        // isNew=true 时 route 已落库、只是链没写全；其余情况才是真正的「保存失败」。
        const hint = routeSaved
          ? isNew
            ? '路由已创建，但完整目标链未保存：'
            : '路由基础信息已保存，但完整目标链未保存：'
          : '保存失败：'
        setSaveError(hint + (e as Error).message + (routeSaved ? '。请保留当前表单并重试保存；若退出，请先刷新检查当前配置。' : ''))
        toast.danger(hint + (e as Error).message)
      }
    } finally {
      setSaving(false)
    }
  }

  return (
    <AppModal
      open={open}
      onClose={onClose}
      title={route ? '编辑路由' : '新建路由'}
      maxWidth="960px"
      maxHeight="920px"
      dismissable={false}
      footer={
        <>
          <Button variant="ghost" isDisabled={saving} onPress={onClose}>
            取消
          </Button>
          <Button variant="primary" isDisabled={chainBlocked} onPress={() => void submit()}>
            {saving ? '保存中…' : '保存'}
          </Button>
        </>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void submit()
        }}
      >
        <div className="mb-4 grid grid-cols-[minmax(0,1fr)_auto] items-end gap-7">
          <TextField aria-label="公开模型名" isRequired value={publicName} onChange={setPublicName}>
            <Label>公开模型名 *</Label>
            <Input placeholder="gpt-4o" className="font-mono" autoComplete="off" disabled={chainBlocked} />
          </TextField>
          <Switch isSelected={failoverEnabled} isDisabled={chainBlocked} aria-label="自动故障转移" onChange={setFailoverEnabled}>
            <Switch.Content className="flex items-center gap-2">
              <Switch.Control>
                <Switch.Thumb />
              </Switch.Control>
              <span className="text-sm">自动故障转移</span>
            </Switch.Content>
          </Switch>
        </div>

        <div>
          <div className="mb-2.5 flex items-center justify-between">
            <span className="text-sm font-semibold">上游目标</span>
            <Button variant="ghost" size="sm" isDisabled={chainBlocked} onPress={addTarget}>
              + 添加目标
            </Button>
          </div>
          {/* 链的加载态必须可见：否则弹窗里静静躺着一行占位主目标，看着就像
              「这条路由本来就只有一个上游」，用户会直接保存。 */}
          {chainLoading ? (
            <p className="text-muted mb-2 text-[13px]" role="status">
              正在读取上游链…
            </p>
          ) : null}
          <div className="overflow-x-auto rounded-xl border border-separator">
            <Table aria-label="上游目标链">
              <Table.ScrollContainer>
                <Table.Content className="min-w-[760px]">
                  <Table.Header>
                    <Table.Column isRowHeader>序号</Table.Column>
                    <Table.Column>上游服务</Table.Column>
                    <Table.Column>上游模型</Table.Column>
                    <Table.Column>启用</Table.Column>
                    <Table.Column>操作</Table.Column>
                  </Table.Header>
                  <Table.Body>
                    {targets.map((t, i) => {
                      const issue = targetIssue(providerMap, mmap, t.provider_id, t.upstream_model_id)
                      return (
                        <Table.Row key={i} className={i === activeTargetIndex ? 'bg-success-soft' : undefined}>
                          <Table.Cell>
                            {i + 1}
                            {i === activeTargetIndex ? <span className="sr-only">（当前服务目标）</span> : null}
                          </Table.Cell>
                          <Table.Cell>
                            <Select
                              aria-label={`第 ${i + 1} 个目标的上游服务`}
                              className="w-[220px]"
                              isDisabled={chainBlocked}
                              selectedKey={t.provider_id}
                              onSelectionChange={(key) => key !== null && onTargetProviderChange(i, String(key))}
                            >
                              <SelectTrigger>
                                <SelectValue />
                                <SelectIndicator />
                              </SelectTrigger>
                              <SelectPopover>
                                <ListBox aria-label="上游服务">
                                  {providers.map((p) => (
                                    // 只写名字：slug 会自动生成（未填就是 "provider"），
                                    // 与名字并列时多半只是把同一串再写一遍。
                                    <ListBoxItem key={p.id} id={p.id}>
                                      {p.name}
                                    </ListBoxItem>
                                  ))}
                                </ListBox>
                              </SelectPopover>
                            </Select>
                          </Table.Cell>
                          <Table.Cell>
                            <Select
                              aria-label={`第 ${i + 1} 个目标的上游模型`}
                              className="w-[220px]"
                              isDisabled={chainBlocked || !t.provider_id}
                              selectedKey={t.upstream_model_id}
                              onSelectionChange={(key) => key !== null && patchTarget(i, { upstream_model_id: String(key) })}
                            >
                              <SelectTrigger>
                                <SelectValue />
                                <SelectIndicator />
                              </SelectTrigger>
                              <SelectPopover>
                                <ListBox aria-label="上游模型">
                                  {modelsOf(t.provider_id).map((m) => (
                                    // 只写真实模型名（model_id）：display_name 多数时候
                                    // 与它只差大小写，`名字 (id)` 的写法等于把同一个值
                                    // 排两遍。这里要选的是「转发给上游哪一个模型」，
                                    // 该看的本来就是 id。
                                    <ListBoxItem key={m.id} id={m.id}>
                                      {m.model_id}
                                    </ListBoxItem>
                                  ))}
                                </ListBox>
                              </SelectPopover>
                            </Select>
                            {issue ? (
                              <p className="bg-warning-soft text-warning-soft-foreground mt-1 rounded px-2 py-1 text-xs">
                                {issue}
                              </p>
                            ) : null}
                          </Table.Cell>
                          <Table.Cell>
                            <Switch
                              isSelected={t.enabled}
                              isDisabled={chainBlocked}
                              aria-label={`${t.enabled ? '停用' : '启用'}第 ${i + 1} 个目标`}
                              onChange={(v) => patchTarget(i, { enabled: v })}
                            >
                              <Switch.Content>
                                <Switch.Control>
                                  <Switch.Thumb />
                                </Switch.Control>
                              </Switch.Content>
                            </Switch>
                          </Table.Cell>
                          <Table.Cell>
                            <div className="flex flex-wrap items-center gap-1">
                              {/* 置顶：一键挪到链首（未开故障转移时 = 当前生效的上游）。 */}
                              <Button
                                variant="ghost"
                                size="sm"
                                isDisabled={chainBlocked || i === 0}
                                aria-label={i === 0 ? '已在链首' : `置顶第 ${i + 1} 个目标`}
                                onPress={() => pinTarget(i)}
                              >
                                置顶
                              </Button>
                              <Button
                                variant="ghost"
                                size="sm"
                                isDisabled={chainBlocked || i === 0}
                                aria-label={`上移第 ${i + 1} 个目标`}
                                onPress={() => moveTarget(i, -1)}
                              >
                                上移
                              </Button>
                              <Button
                                variant="ghost"
                                size="sm"
                                isDisabled={chainBlocked || i === targets.length - 1}
                                aria-label={`下移第 ${i + 1} 个目标`}
                                onPress={() => moveTarget(i, 1)}
                              >
                                下移
                              </Button>
                              <Button
                                variant="ghost"
                                size="sm"
                                isDisabled={chainBlocked || targets.length === 1}
                                aria-label={targets.length === 1 ? '至少保留一个目标' : `删除第 ${i + 1} 个目标`}
                                onPress={() => removeTarget(i)}
                              >
                                删除
                              </Button>
                            </div>
                          </Table.Cell>
                        </Table.Row>
                      )
                    })}
                  </Table.Body>
                </Table.Content>
              </Table.ScrollContainer>
            </Table>
          </div>
        </div>

        {/* 「启用该路由」勾选框已去掉（2026-10-10 用户要求）。
            改为**默认创建即启用**：新建的路由本来就是拿来用的，先建出一个停用
            状态再让管理员去列表里点一次开关，等于把一个必然动作拆成两步 —— 而
            漏掉第二步的症状是「路由建好了但请求全 404」，且列表上那个小小的
            「停用」徽章很容易被忽略。启停能力没有消失：列表行的开关仍在，
            那里才是「运行中改状态」的正确位置。 */}
        {chainError ? (
          <p className="bg-warning-soft text-warning-soft-foreground mt-3 rounded px-2.5 py-2 text-[13px]" role="alert">
            {chainError}
          </p>
        ) : null}
        {saveError ? (
          <p className="bg-warning-soft text-warning-soft-foreground mt-3 rounded px-2.5 py-2 text-[13px]" role="alert">
            {saveError}
          </p>
        ) : null}
      </form>
    </AppModal>
  )
}

// RouteEditor 里 targetIssue 需要的 providerMap：由 providers 数组现算（弹窗
// 数据量小，不值得再提升一份 state）。
function providerMapOf(providers: RouteCatalogProvider[]): Record<string, RouteCatalogProvider> {
  const m: Record<string, RouteCatalogProvider> = {}
  for (const p of providers) m[p.id] = p
  return m
}
