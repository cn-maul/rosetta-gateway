import { useEffect, useState } from 'react'
import { Button, toast } from '@heroui/react'
import { api, ApiFail } from '../api'
import { confirmBox } from '../ui/confirm'
import { GroupFormModal, GroupModelsModal } from './users/GroupModals'
import {
  SetupLinkModal,
  UserCreateModal,
  UserEditModal,
  UserPasswordModal,
  type SetupLinkInfo,
} from './users/UserModals'
import { GroupsTable, UsersTable } from './users/UserTables'
import { boundCounts, deleteBlockedTitle } from './users/groupUtils'
import type { Group, User } from '../types'

/**
 * 用户与分组（仅管理员）。
 *
 * 分组不是独立实体而是用户的属性（users.group_id），两页签数据一次取完、
 * 互相引用（用户的分组名、分组的 member_count 都来自对方）。
 * 页签条由宿主页（AccessControl）持有，这里只渲染面板；表格与六个弹窗
 * 拆在 users/ 子目录，这里只保留动作编排。
 */
export function UsersGroups({ tab }: { tab: 'users' | 'groups' }) {
  const [users, setUsers] = useState<User[]>([])
  const [groups, setGroups] = useState<Group[]>([])
  // 可选模型名来自 /admin/api/model-names —— 与后端白名单校验同一个数据源。
  // 不用路由清单：两处一旦漂移就会出现「界面能勾、保存被后端拒」的错位。
  const [modelOptions, setModelOptions] = useState<string[]>([])
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')

  // ---------- 弹窗开关 ----------
  const [createOpen, setCreateOpen] = useState(false)
  const [editFor, setEditFor] = useState<User | null>(null)
  const [pwdFor, setPwdFor] = useState<User | null>(null)
  const [setupLink, setSetupLink] = useState<SetupLinkInfo | null>(null)
  const [groupFormOpen, setGroupFormOpen] = useState(false)
  const [editingGroup, setEditingGroup] = useState<Group | null>(null)
  const [modelsFor, setModelsFor] = useState<Group | null>(null)

  // 正在飞行中的行操作：user id → 语义标签。防连点必须落在行上，
  // 不能靠一个全局 busy：点不同行本就该各自进行。
  const [pending, setPending] = useState<Map<string, string>>(new Map())
  const [deleting, setDeleting] = useState(false)

  const setRowPending = (id: string, label: string | null) => {
    setPending((m) => {
      const next = new Map(m)
      if (label === null) next.delete(id)
      else next.set(id, label)
      return next
    })
  }

  const load = async () => {
    setLoading(true)
    setErr('')
    try {
      // 三份数据一次取完：分开请求会让「改了谁的分组」之后两个表的新鲜度
      // 对不上 —— member_count 是服务端算好的字段，不重拉分组表就还是旧数字。
      const [u, g, names] = await Promise.all([api.users(), api.groups(), api.modelNames()])
      setUsers(u)
      setGroups(g)
      setModelOptions(names.models)
    } catch (e) {
      if (e instanceof ApiFail && e.status === 401) return
      setErr(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
  }, [])

  /** patch 一发并刷新列表。返回是否成功：失败时这里已弹了错误 toast，
   * 调用方再喊一句「已禁用」会让人以为操作成功了一半。 */
  const patch = async (id: string, b: Parameters<typeof api.updateUser>[1]): Promise<boolean> => {
    if (pending.has(id)) return false
    setRowPending(id, '处理中…')
    try {
      await api.updateUser(id, b)
      await load()
      return true
    } catch (e) {
      toast.danger(e instanceof ApiFail ? e.message : '操作失败')
      return false
    } finally {
      setRowPending(id, null)
    }
  }

  const toggleStatus = async (u: User) => {
    if (u.is_self) {
      toast.danger('不能禁用自己的账号')
      return
    }
    const next = u.status === 'active' ? 'disabled' : 'active'
    // 只有「禁用」要确认：它是不可逆的会话失效，误点一次等于把同事从所有
    // 会话里踢出去。启用只是解除状态，再要一次确认纯属打扰。
    if (next === 'disabled') {
      const ok = await confirmBox({
        title: '禁用用户',
        body: `将禁用「${u.username}」。他当前的所有登录状态会立即失效，需要管理员重新启用才能再登录。`,
        danger: true,
        confirmLabel: '禁用',
      })
      if (!ok) return
    }
    const done = await patch(u.id, { status: next })
    if (!done) return
    // 成功后 load() 已把列表整个换掉，u 是旧快照，不能再读 u.status 判断结果。
    if (next === 'disabled') toast.success('已禁用，该用户的所有登录状态失效')
    else toast.success('已启用')
  }

  const askDelete = async (u: User) => {
    if (u.is_self) {
      toast.danger('不能删除自己的账号')
      return
    }
    if (pending.has(u.id)) return
    const ok = await confirmBox({
      title: '删除用户',
      body: `将删除「${u.username}」及其名下 ${u.key_count} 把密钥。用量历史会保留。`,
      danger: true,
      confirmLabel: '删除',
    })
    if (!ok) return
    setRowPending(u.id, '删除中…')
    try {
      await api.deleteUser(u.id)
      toast.success('已删除')
      await load()
    } catch (e) {
      toast.danger(e instanceof ApiFail ? e.message : '删除失败')
    } finally {
      setRowPending(u.id, null)
    }
  }

  /** 生成一次性设密链接。明文只存在于本次响应和这个弹窗里：
   * 后端只保存 token 哈希，关闭弹窗时前端也立刻抹掉明文 URL。 */
  const generateSetupLink = async (u: User) => {
    if (pending.has(u.id)) return
    setRowPending(u.id, '生成中…')
    try {
      const link = await api.createPasswordSetupLink(u.id)
      setSetupLink({
        user: u,
        url: new URL(link.url_path, window.location.origin).href,
        expiresAt: link.expires_at,
      })
    } catch (e) {
      toast.danger(e instanceof ApiFail ? e.message : '生成设密链接失败')
    } finally {
      setRowPending(u.id, null)
    }
  }

  const askDeleteGroup = async (g: Group) => {
    // 前置拦截，而不是「先确认再等一个 409」：后端一定会拒绝，
    // 让管理员为一个注定失败的操作多按一次确认毫无意义。
    const c = boundCounts(g)
    if (c.users > 0 || c.keys > 0) {
      toast.danger(deleteBlockedTitle(g))
      return
    }
    if (deleting) return
    const ok = await confirmBox({
      title: '删除分组',
      body: `将删除「${g.name}」及其模型白名单。当前没有账号或访问密钥绑定在该组上。`,
      danger: true,
      confirmLabel: '删除',
    })
    if (!ok) return
    setDeleting(true)
    try {
      await api.deleteGroup(g.id)
      toast.success('已删除')
      await load()
    } catch (e) {
      // 409 的文案是后端给的（含人数、密钥数与处置办法），原样展示比
      // 「删除失败」有用得多。走到这里只可能是并发绑定。
      toast.danger(e instanceof ApiFail ? e.message : '删除失败')
    } finally {
      setDeleting(false)
    }
  }

  return (
    <div>
      <div className="mb-3.5 flex justify-end">
        {tab === 'users' ? (
          <Button variant="primary" size="sm" onPress={() => setCreateOpen(true)}>
            新建用户
          </Button>
        ) : (
          <Button
            variant="primary"
            size="sm"
            onPress={() => {
              setEditingGroup(null)
              setGroupFormOpen(true)
            }}
          >
            新建分组
          </Button>
        )}
      </div>

      {tab === 'users' ? (
        <UsersTable
          users={users}
          groups={groups}
          loading={loading}
          err={err}
          pending={pending}
          onEdit={setEditFor}
          onPassword={(u) => (u.has_password ? setPwdFor(u) : void generateSetupLink(u))}
          onToggle={(u) => void toggleStatus(u)}
          onDelete={(u) => void askDelete(u)}
        />
      ) : (
        <GroupsTable
          groups={groups}
          loading={loading}
          err={err}
          deleting={deleting}
          onModels={setModelsFor}
          onEditGroup={(g) => {
            setEditingGroup(g)
            setGroupFormOpen(true)
          }}
          onDeleteGroup={(g) => void askDeleteGroup(g)}
        />
      )}

      <UserCreateModal
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        groups={groups}
        onSaved={load}
        onNeedsSetupLink={(created) => generateSetupLink(created)}
      />
      <SetupLinkModal info={setupLink} onClose={() => setSetupLink(null)} />
      <UserEditModal user={editFor} onClose={() => setEditFor(null)} groups={groups} onSaved={load} />
      <UserPasswordModal user={pwdFor} onClose={() => setPwdFor(null)} onSaved={load} />
      <GroupFormModal open={groupFormOpen} editing={editingGroup} onClose={() => setGroupFormOpen(false)} onSaved={load} />
      <GroupModelsModal group={modelsFor} modelOptions={modelOptions} onClose={() => setModelsFor(null)} onSaved={load} />
    </div>
  )
}
