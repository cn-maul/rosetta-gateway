import { useCallback, useEffect, useState } from 'react'
import { Button, Checkbox, Table, TextField, Input, Label, toast } from '@heroui/react'
import { api } from '../api'
import { confirmBox } from '../ui/confirm'
import { AppModal } from '../ui/AppModal'
import { DataTable } from '../ui/DataTable'
import type { ModelGroup, Provider, UpstreamModel } from '../types'

/**
 * 模型归组（管理员）：把不同上游的「同一个模型」标成一组，用量统计按组名合并
 * （语义对齐旧 ModelGroups.vue）。
 *
 * 数据面不读归组（纯统计/展示层），这里的任何改动都不影响路由与转发 ——
 * 改错了也不会把流量打挂。
 */

/** 401 由 api 层统一处理（清令牌 → AppRoot watcher 送登录页），页面不再提示。 */
function is401(e: unknown): boolean {
  return (e as { status?: number }).status === 401
}

export function ModelGroups() {
  const [groups, setGroups] = useState<ModelGroup[]>([])
  // 全部上游模型（跨 provider 扁平列表）：指派时按 provider 分组供选择。
  const [allModels, setAllModels] = useState<UpstreamModel[]>([])
  // 上游列表：只为了把 provider_id 翻成人类可读的名字。
  const [providers, setProviders] = useState<Provider[]>([])
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(true)

  /** 返回新组列表（供成员弹窗在局部刷新后把 group 指回同 id 新引用）；401 静默。 */
  const load = useCallback(async (): Promise<ModelGroup[]> => {
    setLoading(true)
    setErr('')
    try {
      const [g, m, p] = await Promise.all([api.modelGroups(), api.allModels(), api.providers()])
      setGroups(g)
      setAllModels(m)
      setProviders(p)
      return g
    } catch (e) {
      if (is401(e)) return []
      setErr('加载模型组失败：' + (e as Error).message)
      toast.danger('加载模型组失败：' + (e as Error).message)
      return []
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
  }, [load])

  // providerId → 显示名。必须来自 /providers（带真实 name/slug）：provider_id 是
  // 32 位哈希，直接显示就是乱码。回退顺序 name → slug → 短 id。
  const providerName: Record<string, string> = {}
  for (const p of providers) providerName[p.id] = p.name || p.slug || p.id.slice(0, 8)

  // ---------- 建组 / 改名 ----------

  const [formOpen, setFormOpen] = useState(false)
  const [editing, setEditing] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [saving, setSaving] = useState(false)
  const [formError, setFormError] = useState('')

  function openCreate() {
    setEditing('')
    setDisplayName('')
    setFormError('')
    setFormOpen(true)
  }

  function openRename(g: ModelGroup) {
    setEditing(g.id)
    setDisplayName(g.display_name)
    setFormError('')
    setFormOpen(true)
  }

  async function submitGroup() {
    const name = displayName.trim()
    if (!name) {
      setFormError('组显示名不能为空')
      return
    }
    setSaving(true)
    try {
      if (editing) {
        await api.updateModelGroup(editing, name)
        toast.success('组已改名')
      } else {
        const created = await api.createModelGroup(name)
        // 建完立刻进入「添加成员」，而不是让用户再点一次 —— 空组没有任何作用。
        setFormOpen(false)
        const fresh = await load()
        openMembers(fresh.find((g) => g.id === created.id) ?? created)
        return
      }
      setFormOpen(false)
      await load()
    } catch (e) {
      if (!is401(e)) toast.danger('保存失败：' + (e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  async function removeGroup(g: ModelGroup) {
    const ok = await confirmBox({
      title: `删除模型组「${g.display_name}」？`,
      // 讲清后果：删组不删模型，只是组内模型回到未归组、统计重新分开。
      body: `组内 ${g.member_count} 个模型不会被删除，它们回到「未归组」，用量重新按各自名字分开统计。`,
      danger: true,
      confirmLabel: '删除',
    })
    if (!ok) return
    try {
      await api.deleteModelGroup(g.id)
      toast.success('模型组已删除')
      await load()
    } catch (e) {
      if (!is401(e)) toast.danger('删除失败：' + (e as Error).message)
    }
  }

  // ---------- 成员管理（指派 / 移出）----------

  const [membersOpen, setMembersOpen] = useState(false)
  const [group, setGroup] = useState<ModelGroup | null>(null)
  const [addIds, setAddIds] = useState<Set<string>>(new Set())

  function openMembers(g: ModelGroup) {
    setGroup(g)
    setAddIds(new Set())
    setMembersOpen(true)
  }

  // 当前组成员：group_id === 本组的模型。
  const currentMembers = group ? allModels.filter((mm) => mm.group_id === group.id) : []
  // 可加入的候选：还没进任何组的模型。一个模型最多属一组（group_id 单列），
  // 已在别组的要先把那行移出去，不能在这一页跨组「抢」。
  const availableModels = allModels.filter((mm) => !mm.group_id)

  function toggleAdd(id: string) {
    setAddIds((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  async function applyMembers() {
    if (!group) return
    const ids = [...addIds]
    if (ids.length === 0) {
      setMembersOpen(false)
      return
    }
    try {
      await api.assignModelsToGroup(group.id, ids)
      toast.success(`已把 ${ids.length} 个模型并入「${group.display_name}」`)
      setMembersOpen(false)
      await load()
    } catch (e) {
      if (!is401(e)) toast.danger('指派失败：' + (e as Error).message)
    }
  }

  async function removeMember(mm: UpstreamModel) {
    if (!group) return
    try {
      await api.unassignModelsFromGroup([mm.id])
      toast.success(`已把「${mm.model_id}」移出组`)
      // 局部刷新：重取组列表 + 模型列表，currentMembers 随之更新；
      // load 后原 group 对象换了引用，把弹窗里的 group 指回新的同 id 项。
      const fresh = await load()
      setGroup(fresh.find((g) => g.id === group.id) ?? group)
    } catch (e) {
      if (!is401(e)) toast.danger('移出失败：' + (e as Error).message)
    }
  }

  // 候选/成员行的可读标签「供应商 / 模型名」；provider 刚删或接口部分失败时
  // 回退也截短 id —— 退回完整 32 位哈希就是用户看到的那串乱码。
  function labelFor(mm: UpstreamModel): string {
    const name = providerName[mm.provider_id] ?? mm.provider_id.slice(0, 8)
    return `${name} / ${mm.model_id}`
  }

  return (
    <>
      <p className="text-muted mb-3 max-w-2xl text-xs">
        把不同上游的「同一个模型」（叫法往往不同）编成一组，调用统计按组名合并显示。归组只影响统计展示，
        <b className="text-foreground">不改变路由与转发</b>。一个模型最多属于一组。
      </p>

      <DataTable
        label="模型分组"
        loading={loading}
        error={err}
        hasData={groups.length > 0}
        empty={groups.length === 0}
        emptyText="还没有模型组 —— 点上方「新建模型组」，再把各上游的同名模型并进来"
        onRetry={load}
        toolbar={
          <Button variant="primary" size="sm" onPress={openCreate}>
            新建模型组
          </Button>
        }
      >
        <Table>
          <Table.ScrollContainer>
            <Table.Content aria-label="模型分组列表">
              <Table.Header>
                <Table.Column isRowHeader>组名（统计里显示这个名字）</Table.Column>
                <Table.Column>成员数</Table.Column>
                <Table.Column>操作</Table.Column>
              </Table.Header>
              <Table.Body>
                {groups.map((g) => (
                  <Table.Row key={g.id}>
                    <Table.Cell>{g.display_name}</Table.Cell>
                    <Table.Cell className="text-right tabular-nums">{g.member_count}</Table.Cell>
                    <Table.Cell>
                      <div className="flex gap-1">
                        <Button size="sm" variant="ghost" onPress={() => openMembers(g)}>
                          成员
                        </Button>
                        <Button size="sm" variant="ghost" onPress={() => openRename(g)}>
                          改名
                        </Button>
                        <Button size="sm" variant="danger" onPress={() => void removeGroup(g)}>
                          删除
                        </Button>
                      </div>
                    </Table.Cell>
                  </Table.Row>
                ))}
              </Table.Body>
            </Table.Content>
          </Table.ScrollContainer>
        </Table>
      </DataTable>

      {/* 建组 / 改名 */}
      <AppModal
        open={formOpen}
        onClose={() => setFormOpen(false)}
        title={editing ? '改名' : '新建模型组'}
        maxWidth="420px"
        dismissable={false}
        footer={
          <>
            <Button variant="tertiary" isDisabled={saving} onPress={() => setFormOpen(false)}>
              取消
            </Button>
            <Button type="submit" variant="primary" isDisabled={saving} onPress={() => void submitGroup()}>
              {saving ? '保存中…' : '保存'}
            </Button>
          </>
        }
      >
        <form
          className="flex flex-col gap-4"
          onSubmit={(e) => {
            e.preventDefault()
            void submitGroup()
          }}
        >
          <TextField aria-label="组显示名" isRequired value={displayName} onChange={setDisplayName}>
            <Label>组显示名 *</Label>
            <Input placeholder="DeepSeek V3" autoComplete="off" />
          </TextField>
          <p className="text-muted text-xs">统计页与模型名会显示成这个名字，全库唯一</p>
          {formError && (
            <div className="text-danger text-xs" role="alert">
              {formError}
            </div>
          )}
        </form>
      </AppModal>

      {/* 成员管理 */}
      <AppModal
        open={membersOpen}
        onClose={() => setMembersOpen(false)}
        title={group ? `「${group.display_name}」的成员` : '成员'}
        maxWidth="560px"
        maxHeight="640px"
        dismissable={false}
        footer={
          <>
            <Button variant="tertiary" onPress={() => setMembersOpen(false)}>
              关闭
            </Button>
            <Button variant="primary" isDisabled={addIds.size === 0} onPress={() => void applyMembers()}>
              应用（加 {addIds.size} 个）
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-4">
          <section>
            <h3 className="mb-1.5 text-sm font-semibold">当前成员（{currentMembers.length}）</h3>
            {currentMembers.length === 0 ? (
              <p className="text-muted text-xs">组里还没有模型</p>
            ) : (
              currentMembers.map((mm) => (
                <div key={mm.id} className="flex items-center justify-between gap-2 py-0.5">
                  <span className="truncate font-mono text-sm">{labelFor(mm)}</span>
                  <Button size="sm" variant="ghost" onPress={() => void removeMember(mm)}>
                    移出
                  </Button>
                </div>
              ))
            )}
          </section>

          <section>
            <h3 className="mb-1.5 text-sm font-semibold">添加到本组（勾选后点「应用」）</h3>
            {availableModels.length === 0 ? (
              <p className="text-muted text-xs">没有未归组的模型可添加</p>
            ) : (
              availableModels.map((mm) => (
                <Checkbox
                  key={mm.id}
                  isSelected={addIds.has(mm.id)}
                  onChange={() => toggleAdd(mm.id)}
                  className="w-full"
                >
                  <Checkbox.Content className="w-full gap-2 py-0.5">
                    <Checkbox.Control>
                      <Checkbox.Indicator />
                    </Checkbox.Control>
                    <span className="truncate font-mono text-sm">{labelFor(mm)}</span>
                  </Checkbox.Content>
                </Checkbox>
              ))
            )}
          </section>
        </div>
      </AppModal>
    </>
  )
}
