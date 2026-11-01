import { useCallback, useState } from 'react'
import { useLoaderData, useRevalidator, type LoaderFunctionArgs } from 'react-router-dom'
import { Button, Table, TextField, Input, Label, toast } from '@heroui/react'
import { api } from '../api'
import { fmtTimeMs } from '../fmt'
import { confirmBox } from '../ui/confirm'
import { AppModal } from '../ui/AppModal'
import { DataTable } from '../ui/DataTable'
import { PageHeader } from '../ui/PageHeader'
import { StatusChip, type StatusTone } from '../ui/StatusChip'
import type { Network } from '../types'

/**
 * 出站网络（管理员）：跨上游共用的代理地址清单（语义对齐旧 Networks.vue）。
 * 直连不是一条记录（保留 id 'direct'，由上游表单作为第一项展示），不在这张表里。
 */

interface NetworksData {
  networks: Network[]
  /** 加载失败时保留空清单并把消息带回来（不 throw，错误条内可重试）。 */
  error?: string
}

export async function networksLoader(_args: LoaderFunctionArgs): Promise<NetworksData> {
  try {
    return { networks: await api.networks() }
  } catch (e) {
    return { networks: [], error: '加载代理失败：' + (e as Error).message }
  }
}

/** 401 由 api 层统一处理（清令牌 → AppRoot watcher 送登录页），页面不再提示。 */
function is401(e: unknown): boolean {
  return (e as { status?: number }).status === 401
}

// 2026-11-01：连通性不再靠「测试」按钮，改由探针后台定期测速（与供应商
// 延迟列同一个窗口、同一张表的口径）。界面因此只读后端给的摘要，页面自己
// 不再持有任何「刚才那次测试」的临时状态 —— 那样关掉页面就丢失，而代理坏
// 掉通常恰恰发生在没人盯着这个页面的时候。
const LATENCY_OK_MS = 300
const LATENCY_WARN_MS = 1000

/** 有成功样本才算「测到了」；0 表示窗口内没有可用样本，界面显示「—」。 */
function hasLatency(n: Network): boolean {
  return (n.latency_samples ?? 0) > 0
}

function latencyText(n: Network): string {
  if (!hasLatency(n)) return '—'
  const ms = n.latency_ms ?? 0
  return ms < 1000 ? `${Math.round(ms)} ms` : `${(ms / 1000).toFixed(2)} s`
}

function latencyTone(n: Network): StatusTone {
  // 没有成功样本时不能一律 dim：最近一次就失败的代理必须显红，否则「测不
  // 通」和「没测过」在列表上是同一个灰点。
  if (!hasLatency(n)) return n.last_probe_at && n.last_probe_ok === false ? 'err' : 'dim'
  const ms = n.latency_ms ?? 0
  return ms <= LATENCY_OK_MS ? 'ok' : ms <= LATENCY_WARN_MS ? 'warn' : 'err'
}

function latencyTitle(n: Network): string {
  const parts: string[] = []
  if (hasLatency(n)) {
    parts.push(`平均往返 ${latencyText(n)}`)
    parts.push(`P95 ${Math.round(n.latency_p95_ms ?? 0)} ms`, `${n.latency_samples} 个样本`)
  } else if (!n.last_probe_at) {
    parts.push('还没有探测记录（探针按 probes.interval_sec 定时测速）')
  } else {
    parts.push('窗口内没有成功的探测记录')
  }
  if (n.last_probe_at) {
    parts.push(`最近一次 ${n.last_probe_ok ? '成功' : '失败'} · ${fmtTimeMs(n.last_probe_at)}`)
  }
  if (n.last_probe_error) parts.push(n.last_probe_error)
  return parts.join(' · ')
}

/** 失败原因单独露出一行：只藏在 tooltip 里等于看不见。 */
function probeErrorLine(n: Network): string {
  if (n.last_probe_at && n.last_probe_ok === false && n.last_probe_error) return n.last_probe_error
  return ''
}

export function Networks() {
  const initial = useLoaderData() as NetworksData
  const revalidator = useRevalidator()

  // 刷新/变更后的重读：交给 revalidator 重跑 loader；错误信息也随 loader 返回。
  const reload = useCallback(() => void revalidator.revalidate(), [revalidator])

  // ---------- 建 / 改 ----------
  const [formOpen, setFormOpen] = useState(false)
  const [editing, setEditing] = useState<string>('')
  const [name, setName] = useState('')
  const [proxyUrl, setProxyUrl] = useState('')
  const [saving, setSaving] = useState(false)

  function openCreate() {
    setEditing('')
    setName('')
    setProxyUrl('')
    setFormError('')
    setFormOpen(true)
  }

  function openEdit(n: Network) {
    setEditing(n.id)
    setName(n.name)
    setProxyUrl(n.proxy_url)
    setFormError('')
    setFormOpen(true)
  }

  const [formError, setFormError] = useState('')

  async function submit() {
    const trimmedName = name.trim()
    const proxyURL = proxyUrl.trim()
    // 直连已经有（且不需要）一个入口，再建一条空地址的「网络」只会造出
    // 第二种「不代理」的表示 —— 两个字段都必须非空。
    if (!trimmedName) {
      setFormError('代理名称不能为空')
      return
    }
    if (!proxyURL) {
      setFormError('代理地址不能为空')
      return
    }
    setSaving(true)
    try {
      if (editing) {
        await api.updateNetwork(editing, { name: trimmedName, proxy_url: proxyURL })
        toast.success('代理已更新')
      } else {
        await api.createNetwork({ name: trimmedName, proxy_url: proxyURL })
        toast.success('代理已创建')
      }
      setFormOpen(false)
      reload()
    } catch (e) {
      if (!is401(e)) toast.danger('保存失败：' + (e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  async function remove(n: Network) {
    const ok = await confirmBox({
      title: `删除代理「${n.name}」？`,
      // 讲清后果：引用它的上游会回到直连，**不会**被删掉。
      body:
        n.provider_count > 0
          ? `正在使用它的 ${n.provider_count} 个上游会退回「直连」（上游本身不会被删除）。之后可以重新建一条同名代理再选回去。`
          : '当前没有上游使用它。',
      danger: true,
      confirmLabel: '删除',
    })
    if (!ok) return
    try {
      await api.deleteNetwork(n.id)
      toast.success('代理已删除')
      reload()
    } catch (e) {
      if (!is401(e)) toast.danger('删除失败：' + (e as Error).message)
    }
  }

  const rows = initial.networks
  const loading = revalidator.state === 'loading'

  return (
    <>
      <PageHeader
        title="代理"
        actions={
          <>
            <Button variant="secondary" isDisabled={loading} onPress={reload}>
              刷新
            </Button>
            <Button variant="primary" onPress={openCreate}>
              新建代理
            </Button>
          </>
        }
      />

      <DataTable
        label="代理列表"
        loading={loading}
        error={initial.error}
        hasData={rows.length > 0}
        empty={rows.length === 0}
        emptyText="暂无代理"
        onRetry={reload}
      >
        <Table>
          <Table.ScrollContainer>
            <Table.Content aria-label="代理列表，可横向滚动">
              <Table.Header>
                <Table.Column isRowHeader>名称</Table.Column>
                <Table.Column>代理地址</Table.Column>
                <Table.Column>使用中的上游</Table.Column>
                <Table.Column>延迟</Table.Column>
                <Table.Column>操作</Table.Column>
              </Table.Header>
              <Table.Body>
                {rows.map((n) => {
                  const probeError = probeErrorLine(n)
                  return (
                    <Table.Row key={n.id}>
                      <Table.Cell>{n.name}</Table.Cell>
                      <Table.Cell>
                        <span className="font-mono break-all">{n.proxy_url}</span>
                      </Table.Cell>
                      <Table.Cell className="text-right">{n.provider_count}</Table.Cell>
                      <Table.Cell>
                        <span title={latencyTitle(n)}>
                          <StatusChip tone={latencyTone(n)}>{latencyText(n)}</StatusChip>
                        </span>
                        {probeError ? (
                          <span className="text-danger mt-0.5 block max-w-[220px] truncate text-xs" title={probeError}>
                            {probeError}
                          </span>
                        ) : null}
                      </Table.Cell>
                      <Table.Cell>
                        <div className="flex justify-end gap-1">
                          <Button size="sm" variant="ghost" onPress={() => openEdit(n)}>
                            编辑
                          </Button>
                          <Button size="sm" variant="danger" onPress={() => void remove(n)}>
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
      </DataTable>

      <AppModal
        open={formOpen}
        onClose={() => setFormOpen(false)}
        title={editing ? '编辑代理' : '新建代理'}
        maxWidth="520px"
        dismissable={false}
        footer={
          <>
            <Button variant="tertiary" isDisabled={saving} onPress={() => setFormOpen(false)}>
              取消
            </Button>
            <Button type="submit" variant="primary" isDisabled={saving} onPress={() => void submit()}>
              {saving ? '保存中…' : '保存'}
            </Button>
          </>
        }
      >
        <form
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault()
            void submit()
          }}
        >
          <TextField aria-label="代理名称" isRequired value={name} onChange={setName}>
            <Label>名称</Label>
            <Input placeholder="公司代理 / 本机 Clash" />
          </TextField>
          <TextField aria-label="代理地址" isRequired value={proxyUrl} onChange={setProxyUrl}>
            <Label>代理地址</Label>
            <Input placeholder="http://127.0.0.1:7897" className="font-mono" autoComplete="off" />
          </TextField>
          {formError && (
            <div className="text-danger text-xs" role="alert">
              {formError}
            </div>
          )}
        </form>
      </AppModal>
    </>
  )
}
