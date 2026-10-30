import { useCallback, useState } from 'react'
import { useLoaderData, useRevalidator, type LoaderFunctionArgs } from 'react-router-dom'
import { Button, Table, TextField, Input, Label, toast } from '@heroui/react'
import { api } from '../api'
import { confirmBox } from '../ui/confirm'
import { AppModal } from '../ui/AppModal'
import { DataTable } from '../ui/DataTable'
import { PageHeader } from '../ui/PageHeader'
import { StatusChip, type StatusTone } from '../ui/StatusChip'
import type { Network, NetworkTestResult } from '../types'

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

// 连通性结果只保留在当前页面会话：它描述的是「刚才那次测试」，不是配置本身。
interface TestState {
  pending: boolean
  result: NetworkTestResult | null
}

function testVisual(state: TestState | undefined): { tone: StatusTone; label: string; title?: string } {
  if (!state) return { tone: 'dim', label: '未测试' }
  if (state.pending) return { tone: 'warn', label: '测试中…' }
  if (state.result?.status === 'ok') return { tone: 'ok', label: `正常 · ${state.result.latency_ms}ms` }
  return { tone: 'err', label: '失败', title: state.result?.message }
}

export function Networks() {
  const initial = useLoaderData() as NetworksData
  const revalidator = useRevalidator()

  // 刷新/变更后的重读：交给 revalidator 重跑 loader；错误信息也随 loader 返回。
  const reload = useCallback(() => void revalidator.revalidate(), [revalidator])

  const [testStates, setTestStates] = useState<Record<string, TestState>>({})
  const setTest = (id: string, state: TestState | undefined) =>
    setTestStates((prev) => {
      const next = { ...prev }
      if (state === undefined) delete next[id]
      else next[id] = state
      return next
    })

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
        setTest(editing, undefined)
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
      setTest(n.id, undefined)
      toast.success('代理已删除')
      reload()
    } catch (e) {
      if (!is401(e)) toast.danger('删除失败：' + (e as Error).message)
    }
  }

  async function testProxy(n: Network) {
    const state = testStates[n.id]
    if (state?.pending) return
    setTest(n.id, { pending: true, result: null })
    try {
      const result = await api.testNetwork(n.id)
      setTest(n.id, { pending: false, result })
      if (result.status !== 'ok') toast.danger('测试失败：' + result.message)
    } catch (e) {
      if (is401(e)) {
        setTest(n.id, undefined)
        return
      }
      setTest(n.id, {
        pending: false,
        result: { status: 'error', message: '管理接口调用失败：' + (e as Error).message, latency_ms: 0 },
      })
      toast.danger('测试失败：' + (e as Error).message)
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
                <Table.Column>名称</Table.Column>
                <Table.Column>代理地址</Table.Column>
                <Table.Column>使用中的上游</Table.Column>
                <Table.Column>联通状态</Table.Column>
                <Table.Column>操作</Table.Column>
              </Table.Header>
              <Table.Body>
                {rows.map((n) => {
                  const v = testVisual(testStates[n.id])
                  const testing = testStates[n.id]?.pending === true
                  return (
                    <Table.Row key={n.id}>
                      <Table.Cell>{n.name}</Table.Cell>
                      <Table.Cell>
                        <span className="font-mono break-all">{n.proxy_url}</span>
                      </Table.Cell>
                      <Table.Cell className="text-right">{n.provider_count}</Table.Cell>
                      <Table.Cell>
                        <StatusChip tone={v.tone} title={v.title}>
                          {v.label}
                        </StatusChip>
                      </Table.Cell>
                      <Table.Cell>
                        <div className="flex justify-end gap-1">
                          <Button size="sm" variant="ghost" isDisabled={testing} onPress={() => void testProxy(n)}>
                            {testing ? '测试中…' : '测试'}
                          </Button>
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
