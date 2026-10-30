import { useEffect, useMemo, useState } from 'react'
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
  Table,
  TextField,
  toast,
} from '@heroui/react'
import { api, ApiFail } from '../api'
import { confirmBox } from '../ui/confirm'
import { AppModal } from '../ui/AppModal'
import { DataTable } from '../ui/DataTable'
import { StatusChip } from '../ui/StatusChip'
import { fmtTokens, fmtDateTime, fmtBalance, fmtRemainder, copyText } from '../fmt'
import { checkPasswordStrength } from '../password'
import { ModelPicker } from './ModelPicker'
import type { Group, User, UserRole } from '../types'

/** 分组 id → 名字。每行渲染都要查一次，用 Map 而不是对每行 find 一遍。 */
function useGroupNames(groups: Group[]) {
  return useMemo(() => {
    const m = new Map<string, string>()
    for (const g of groups) m.set(g.id, g.name)
    return m
  }, [groups])
}

/** 组里「挡删除」的两类绑定。users.group_id 与 access_keys.group_id 都是
 * ON DELETE SET NULL，两类引用各自都能让后端拒绝删除 —— 只统计账号数会
 * 漏掉「组里没人、但有一把密钥指定了分组覆盖」的情形，那时界面显示可删、
 * 点下去却必然 409。 */
function boundCounts(g: Group): { users: number; keys: number } {
  return { users: g.member_count ?? 0, keys: g.key_count ?? 0 }
}

function canDeleteGroup(g: Group): boolean {
  const c = boundCounts(g)
  return c.users === 0 && c.keys === 0
}

/** 删除被挡时的 title：必须说清是哪一类绑定挡的、以及怎么解 ——
 * 账号要迁组（本页用户页签就能改），密钥要解除「分组覆盖」（密钥页的字段）。 */
function deleteBlockedTitle(g: Group): string {
  const c = boundCounts(g)
  if (c.users > 0 && c.keys > 0) {
    return `该组下还有 ${c.users} 个账号和 ${c.keys} 把访问密钥。请先把账号移到别的组，并解除这些密钥的分组覆盖。`
  }
  if (c.users > 0) {
    return `该组下还有 ${c.users} 个账号。请先在「用户」页签把他们移到别的组或移出分组，否则删除会被后端拒绝。`
  }
  if (c.keys > 0) {
    return `该组下还有 ${c.keys} 把访问密钥指定了分组覆盖。请先到「访问密钥」解除这些密钥的分组覆盖。`
  }
  return ''
}

const ROLE_OPTIONS: { id: UserRole; label: string }[] = [
  { id: 'user', label: '普通用户' },
  { id: 'admin', label: '管理员' },
]

/** 角色下拉。value 是受控值；disabledSelf 为真时置灰（不能改自己的角色）。 */
function RoleSelect({
  value,
  onChange,
  disabledSelf = false,
}: {
  value: UserRole
  onChange: (v: UserRole) => void
  disabledSelf?: boolean
}) {
  const select = (
    <Select
      aria-label="角色"
      selectedKey={value}
      onSelectionChange={(key) => {
        if (key !== null) onChange(String(key) as UserRole)
      }}
    >
      <SelectTrigger>
        <SelectValue />
        <SelectIndicator />
      </SelectTrigger>
      <SelectPopover>
        <ListBox aria-label="角色">
          {ROLE_OPTIONS.map((r) => (
            <ListBoxItem key={r.id} id={r.id}>
              {r.label}
            </ListBoxItem>
          ))}
        </ListBox>
      </SelectPopover>
    </Select>
  )
  if (!disabledSelf) return select
  // SelectTrigger 没有 isDisabled：外包一层禁掉交互并保留可读性。
  return (
    <div aria-disabled className="pointer-events-none opacity-60">
      {select}
    </div>
  )
}

/** 分组下拉（创建/编辑共用）。空串 = 未分组 = 不受模型白名单限制。 */
function GroupSelect({
  value,
  onChange,
  groups,
}: {
  value: string
  onChange: (v: string) => void
  groups: Group[]
}) {
  return (
    <Select
      aria-label="分组"
      selectedKey={value === '' ? '__none__' : value}
      onSelectionChange={(key) => {
        if (key === null) return
        const v = String(key)
        onChange(v === '__none__' ? '' : v)
      }}
    >
      <SelectTrigger>
        <SelectValue />
        <SelectIndicator />
      </SelectTrigger>
      <SelectPopover>
        <ListBox aria-label="分组">
          <ListBoxItem key="none" id="__none__">
            未分组（模型不受限）
          </ListBoxItem>
          {groups.map((g) => (
            <ListBoxItem key={g.id} id={g.id}>
              {g.name}
              {g.models.length === 0 ? '（未限制模型）' : ''}
            </ListBoxItem>
          ))}
        </ListBox>
      </SelectPopover>
    </Select>
  )
}

/**
 * 用户与分组（仅管理员）。
 *
 * 分组不是独立实体而是用户的属性（users.group_id），两页签数据一次取完、
 * 互相引用（用户的分组名、分组的 member_count 都来自对方）。
 * 页签条由宿主页（AccessControl）持有，这里只渲染面板。
 */
export function UsersGroups({ tab }: { tab: 'users' | 'groups' }) {
  const [users, setUsers] = useState<User[]>([])
  const [groups, setGroups] = useState<Group[]>([])
  // 可选模型名来自 /admin/api/model-names —— 与后端白名单校验同一个数据源。
  // 不用路由清单：两处一旦漂移就会出现「界面能勾、保存被后端拒」的错位。
  const [modelOptions, setModelOptions] = useState<string[]>([])
  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')

  // ---------- 新建用户 ----------
  const [cOpen, setCOpen] = useState(false)
  const [cName, setCName] = useState('')
  const [cPassword, setCPassword] = useState('')
  const [cRole, setCRole] = useState<UserRole>('user')
  const [cGroup, setCGroup] = useState('')
  const [cQuota, setCQuota] = useState('0')
  const [cErr, setCErr] = useState('')
  const [cBusy, setCBusy] = useState(false)

  // ---------- 重置密码 ----------
  const [pwdFor, setPwdFor] = useState<User | null>(null)
  const [pwdValue, setPwdValue] = useState('')
  const [pwdErr, setPwdErr] = useState('')
  const [pwdBusy, setPwdBusy] = useState(false)

  // ---------- 一次性设密链接 ----------
  const [setupLinkFor, setSetupLinkFor] = useState<User | null>(null)
  const [setupLinkURL, setSetupLinkURL] = useState('')
  const [setupLinkExpiresAt, setSetupLinkExpiresAt] = useState(0)
  const [setupLinkBusy, setSetupLinkBusy] = useState(false)

  // ---------- 编辑用户 ----------
  const [editing, setEditing] = useState<User | null>(null)
  const [eRole, setERole] = useState<UserRole>('user')
  const [eGroup, setEGroup] = useState('')
  const [eQuota, setEQuota] = useState('0')
  const [eRemark, setERemark] = useState('')
  const [eErr, setEErr] = useState('')
  const [eBusy, setEBusy] = useState(false)

  // ---------- 分组表单 ----------
  const [showForm, setShowForm] = useState(false)
  const [editingGroup, setEditingGroup] = useState<Group | null>(null)
  const [fName, setFName] = useState('')
  const [fDesc, setFDesc] = useState('')
  const [fErr, setFErr] = useState('')
  const [fBusy, setFBusy] = useState(false)

  // ---------- 分组模型白名单 ----------
  const [modelsFor, setModelsFor] = useState<Group | null>(null)
  const [picked, setPicked] = useState<string[]>([])
  const [mErr, setMErr] = useState('')
  const [mBusy, setMBusy] = useState(false)

  // 正在飞行中的行操作：user id → 语义标签。防连点必须落在行上，
  // 不能靠一个全局 busy：点不同行本就该各自进行。
  const [pending, setPending] = useState<Map<string, string>>(new Map())
  const [deleting, setDeleting] = useState(false)

  const isPending = (id: string) => pending.has(id)
  const setRowPending = (id: string, label: string | null) => {
    setPending((m) => {
      const next = new Map(m)
      if (label === null) next.delete(id)
      else next.set(id, label)
      return next
    })
  }

  const groupNames = useGroupNames(groups)

  /** 组被删掉、或 id 查不到时显示「未知分组」而不是空 —— 空看起来像
   * 「未分组」，而「未分组 = 不限制模型」，两者含义完全不同。 */
  const groupName = (id: string) => (id ? (groupNames.get(id) ?? '未知分组') : '')
  /** 判据是清单里没有这个 id，不是「名字等于未知分组」—— 真有组叫这个名字
   * 时，按名字判断会把它的成员全误报成分组已不存在。 */
  const isUnknownGroup = (id: string) => !!id && !groupNames.has(id)

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
    if (isPending(id)) return false
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

  const openEdit = (u: User) => {
    setEErr('')
    setERole(u.role)
    setEGroup(u.group_id || '')
    setEQuota(String(u.quota_tokens || 0))
    setERemark(u.remark || '')
    setEditing(u)
  }

  const saveEdit = async () => {
    const u = editing
    if (!u || eBusy) return
    setEErr('')
    // 后端也会拒（400），先挡住是为了不让人白等一个来回。
    if (u.is_self && eRole !== 'admin') {
      setEErr('不能降低自己的权限（会把自己锁在门外）')
      return
    }
    if (eRole === 'admin' && u.role !== 'admin' && !u.has_password) {
      setEErr('该账号尚未设置密码，请先生成一次性设密链接，再升级为管理员')
      return
    }
    if (Number(eQuota) < 0) {
      setEErr('额度上限不能为负')
      return
    }
    const roleChanged = eRole !== u.role
    setEBusy(true)
    try {
      await api.updateUser(u.id, {
        role: eRole,
        // 空串 = 明确的「移出分组」（后端用指针区分「不传」与「传空」），
        // 不能用 undefined，否则「清空分组」这个动作表达不出去。
        group_id: eGroup,
        quota_tokens: Number(eQuota) || 0,
        remark: eRemark.trim(),
      })
      toast.success(roleChanged ? '已保存，该用户的登录状态已失效' : '已保存')
      setEditing(null)
      await load()
    } catch (e) {
      setEErr(e instanceof ApiFail ? e.message : '保存失败')
    } finally {
      setEBusy(false)
    }
  }

  const create = async () => {
    if (cBusy) return
    setCErr('')
    if (!cName.trim()) {
      setCErr('用户名必填')
      return
    }
    // 管理员空密码不是「强度不足」，是安全边界：空密码 admin 会让免鉴权的
    // bootstrap 引导判定重新命中。必须提交前拦掉，而不是等后端回 400。
    if (cRole === 'admin' && !cPassword) {
      setCErr('创建管理员必须设置初始密码（管理员不允许空密码账号）')
      return
    }
    // 普通用户初始密码可留空（建出「尚未设置密码」的账号），但填了就必须合规。
    if (cPassword) {
      const weak = checkPasswordStrength(cPassword)
      if (weak) {
        setCErr(weak)
        return
      }
    }
    setCBusy(true)
    const needsSetupLink = !cPassword
    try {
      const created = await api.createUser({
        username: cName.trim(),
        // 密码留空则不传 —— 后端接受空密码，建出「尚未设置密码」的账号。
        password: cPassword || undefined,
        role: cRole,
        group_id: cGroup || undefined,
        quota_tokens: Number(cQuota) || 0,
      })
      setCOpen(false)
      setCName('')
      setCPassword('')
      setCRole('user')
      setCGroup('')
      setCQuota('0')
      await load()
      toast.success('已创建')
      if (needsSetupLink) await generateSetupLink(created)
    } catch (e) {
      setCErr(e instanceof ApiFail ? e.message : '创建失败')
    } finally {
      setCBusy(false)
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
    if (isPending(u.id)) return
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

  const resetPassword = async () => {
    const u = pwdFor
    if (!u || pwdBusy) return
    setPwdErr('')
    // 与后端 ValidatePassword 同一套门槛：只查长度的话，「88888888」在界面上
    // 合格、提交后被回 400，管理员只会看到一句「重置失败」。
    const weak = checkPasswordStrength(pwdValue)
    if (weak) {
      setPwdErr(weak)
      return
    }
    setPwdBusy(true)
    try {
      await api.resetUserPassword(u.id, pwdValue)
      toast.success('密码已重置，该用户需重新登录')
      setPwdFor(null)
      setPwdValue('')
      await load()
    } catch (e) {
      setPwdErr(e instanceof ApiFail ? e.message : '重置失败')
    } finally {
      setPwdBusy(false)
    }
  }

  /** 生成一次性设密链接。明文只存在于本次响应和这个弹窗里：
   * 后端只保存 token 哈希，关闭弹窗时前端也立刻抹掉明文 URL。 */
  const generateSetupLink = async (u: User) => {
    if (setupLinkBusy || isPending(u.id)) return
    setRowPending(u.id, '生成中…')
    setSetupLinkBusy(true)
    try {
      const link = await api.createPasswordSetupLink(u.id)
      setSetupLinkFor(u)
      setSetupLinkURL(new URL(link.url_path, window.location.origin).href)
      setSetupLinkExpiresAt(link.expires_at)
    } catch (e) {
      toast.danger(e instanceof ApiFail ? e.message : '生成设密链接失败')
    } finally {
      setSetupLinkBusy(false)
      setRowPending(u.id, null)
    }
  }

  const closeSetupLink = () => {
    setSetupLinkFor(null)
    setSetupLinkURL('')
    setSetupLinkExpiresAt(0)
  }

  const copySetupLink = async () => {
    const ok = await copyText(setupLinkURL)
    if (ok) toast.success('设密链接已复制')
    else toast.danger('复制失败，请手动选择复制')
  }

  const openReset = (u: User) => {
    setPwdErr('')
    setPwdValue('')
    setPwdFor(u)
  }

  // ---- 分组管理 ----

  const openCreateGroup = () => {
    setEditingGroup(null)
    setFName('')
    setFDesc('')
    setFErr('')
    setShowForm(true)
  }

  const openEditGroup = (g: Group) => {
    setEditingGroup(g)
    setFName(g.name)
    setFDesc(g.description)
    setFErr('')
    setShowForm(true)
  }

  const submitGroupForm = async () => {
    if (fBusy) return
    setFErr('')
    if (!fName.trim()) {
      setFErr('组名必填')
      return
    }
    setFBusy(true)
    try {
      if (editingGroup) {
        await api.updateGroup(editingGroup.id, { name: fName.trim(), description: fDesc.trim() })
        toast.success('已保存')
      } else {
        await api.createGroup({ name: fName.trim(), description: fDesc.trim() })
        // 建组即提示「此刻不限制」：空白名单 = 不限制，没人配模型时
        // 这个组比不分组还宽，必须当场说破。
        toast.success('已创建。注意：新组未配模型白名单，此时该组不限制模型可见范围。')
      }
      setShowForm(false)
      await load()
    } catch (e) {
      setFErr(e instanceof ApiFail ? e.message : '保存失败')
    } finally {
      setFBusy(false)
    }
  }

  const openModels = (g: Group) => {
    setModelsFor(g)
    setPicked([...g.models])
    setMErr('')
  }

  const submitModels = async () => {
    const g = modelsFor
    if (!g || mBusy) return
    setMErr('')
    setMBusy(true)
    try {
      await api.setGroupModels(g.id, [...picked])
      toast.success(picked.length === 0 ? '已清空白名单（该组不再限制模型）' : '白名单已保存')
      setModelsFor(null)
      await load()
    } catch (e) {
      setMErr(e instanceof ApiFail ? e.message : '保存失败')
    } finally {
      setMBusy(false)
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
          <Button variant="primary" size="sm" onPress={() => setCOpen(true)}>
            新建用户
          </Button>
        ) : (
          <Button variant="primary" size="sm" onPress={openCreateGroup}>
            新建分组
          </Button>
        )}
      </div>

      {tab === 'users' ? (
        <DataTable label="用户与权限组" loading={loading} error={err} hasData={users.length > 0} empty={true} emptyText="还没有用户">
          <Table>
            <Table.ScrollContainer>
              <Table.Content aria-label="用户与权限组">
                <Table.Header>
                  <Table.Column>用户名</Table.Column>
                  <Table.Column>分组</Table.Column>
                  <Table.Column className="text-right">密钥数量</Table.Column>
                  <Table.Column className="text-right">总 token 用量</Table.Column>
                  <Table.Column className="text-right">余额</Table.Column>
                  <Table.Column className="text-right">操作</Table.Column>
                </Table.Header>
                <Table.Body>
                  {users.map((u) => (
                    <Table.Row key={u.id}>
                      <Table.Cell>
                        <div className="flex flex-wrap items-center gap-1.5">
                          <span className="font-medium font-mono">{u.username}</span>
                          <StatusChip tone={u.role === 'admin' ? 'ok' : 'dim'}>
                            {u.role === 'admin' ? '管理员' : '普通用户'}
                          </StatusChip>
                          {u.is_self ? <StatusChip tone="info">你自己</StatusChip> : null}
                          {u.status !== 'active' ? <StatusChip tone="dim">已禁用</StatusChip> : null}
                          {!u.has_password ? <StatusChip tone="dim">未设密码</StatusChip> : null}
                        </div>
                      </Table.Cell>
                      <Table.Cell>
                        {u.group_id ? groupName(u.group_id) : '未分组'}
                        {isUnknownGroup(u.group_id) ? (
                          <span className="ml-1.5">
                            <StatusChip tone="err">分组已不存在</StatusChip>
                          </span>
                        ) : null}
                      </Table.Cell>
                      <Table.Cell className="text-right tabular-nums">{u.key_count}</Table.Cell>
                      <Table.Cell className="text-right tabular-nums">
                        {fmtTokens(u.used_tokens)}
                        {u.quota_tokens > 0 ? <span className="text-muted"> / {fmtTokens(u.quota_tokens)}</span> : null}
                      </Table.Cell>
                      <Table.Cell className="text-right tabular-nums">
                        {/* 「不限」与「0.00 元」是相反的两种状态，绝不能都渲染成 0。 */}
                        <span className={u.balance_unlimited ? 'text-muted' : ''}>
                          {fmtBalance(u.balance_cents, u.balance_unlimited)}
                        </span>
                        {/* 待结算余数：低单价时一次调用不足一分，余额长时间不动。
                            不看到它管理员会以为扣费坏了。 */}
                        {fmtRemainder(u.balance_remainder ?? 0) ? (
                          <span className="text-muted ml-1 tabular-nums">+{fmtRemainder(u.balance_remainder ?? 0)}</span>
                        ) : null}
                        {!u.balance_unlimited && u.balance_cents === 0 ? (
                          <div>
                            <StatusChip tone="err">已用尽</StatusChip>
                          </div>
                        ) : null}
                      </Table.Cell>
                      <Table.Cell>
                        <div className="flex flex-wrap items-center justify-end gap-1">
                          <Button variant="ghost" size="sm" isDisabled={isPending(u.id)} onPress={() => openEdit(u)}>
                            编辑
                          </Button>
                          <span
                            title={
                              u.status !== 'active'
                                ? '请先启用该用户'
                                : u.has_password
                                  ? '重置该用户的密码'
                                  : '生成 24 小时有效的一次性设密链接'
                            }
                          >
                            <Button
                              variant="ghost"
                              size="sm"
                              isDisabled={u.status !== 'active' || isPending(u.id)}
                              onPress={() => (u.has_password ? openReset(u) : void generateSetupLink(u))}
                            >
                              {isPending(u.id) ? '处理中…' : u.has_password ? '改密' : '设密'}
                            </Button>
                          </span>
                          <span title={u.is_self ? '不能禁用自己的账号' : '禁用后该用户的所有登录状态立即失效'}>
                            <Button
                              variant="ghost"
                              size="sm"
                              isDisabled={u.is_self || isPending(u.id)}
                              onPress={() => void toggleStatus(u)}
                            >
                              {isPending(u.id) ? '处理中…' : u.status === 'active' ? '禁用' : '启用'}
                            </Button>
                          </span>
                          <span title={u.is_self ? '不能删除自己' : undefined}>
                            <Button
                              variant="danger"
                              size="sm"
                              isDisabled={u.is_self || isPending(u.id)}
                              onPress={() => void askDelete(u)}
                            >
                              {isPending(u.id) ? '处理中…' : '删除'}
                            </Button>
                          </span>
                        </div>
                      </Table.Cell>
                    </Table.Row>
                  ))}
                </Table.Body>
              </Table.Content>
            </Table.ScrollContainer>
          </Table>
        </DataTable>
      ) : (
        <>
          <div className="text-muted mb-3.5 text-[12.5px] leading-relaxed">
            未分组、或所在组未配白名单 → <strong className="text-foreground">不限制</strong>模型可见范围。
            白名单与账号自己的密钥白名单<b>取交集</b>，密钥那一层只会更紧。
          </div>
          <DataTable label="用户与权限组" loading={loading} error={err} hasData={groups.length > 0} empty={true} emptyText="还没有分组 —— 当前所有密钥都能看到全部模型">
            <Table>
              <Table.ScrollContainer>
                <Table.Content aria-label="用户分组">
                  <Table.Header>
                    <Table.Column>分组</Table.Column>
                    <Table.Column>说明</Table.Column>
                    <Table.Column>模型白名单</Table.Column>
                    <Table.Column className="text-right">操作</Table.Column>
                  </Table.Header>
                  <Table.Body>
                    {groups.map((g) => (
                      <Table.Row key={g.id}>
                        <Table.Cell>
                          <div className="flex flex-wrap items-center gap-1.5">
                            <span className="font-medium">{g.name}</span>
                            {/* 账号与密钥分开显示：两者都会阻止删除，但处置办法不同
                                （前者迁组、后者解除分组覆盖），合成一个数字就没法
                                告诉管理员该去哪改。 */}
                            <StatusChip tone="dim">{g.member_count} 个账号</StatusChip>
                            <StatusChip tone="dim">{g.key_count} 把密钥</StatusChip>
                            {g.models.length === 0 ? (
                              <StatusChip tone="info">未限制模型</StatusChip>
                            ) : (
                              <StatusChip tone="ok">{g.models.length} 个模型</StatusChip>
                            )}
                          </div>
                        </Table.Cell>
                        <Table.Cell className="text-muted">{g.description || '—'}</Table.Cell>
                        <Table.Cell>
                          <span className="font-mono text-[12.5px]">
                            {g.models.length === 0 ? '（未配置白名单 = 不限制）' : g.models.join('、')}
                          </span>
                        </Table.Cell>
                        <Table.Cell>
                          <div className="flex flex-wrap items-center justify-end gap-1">
                            <Button variant="ghost" size="sm" onPress={() => openModels(g)}>
                              配置模型
                            </Button>
                            <Button variant="ghost" size="sm" onPress={() => openEditGroup(g)}>
                              改名
                            </Button>
                            <span title={deleteBlockedTitle(g) || undefined}>
                              <Button
                                variant="danger"
                                size="sm"
                                isDisabled={!canDeleteGroup(g) || deleting}
                                onPress={() => void askDeleteGroup(g)}
                              >
                                删除
                              </Button>
                            </span>
                          </div>
                        </Table.Cell>
                      </Table.Row>
                    ))}
                  </Table.Body>
                </Table.Content>
              </Table.ScrollContainer>
            </Table>
          </DataTable>
        </>
      )}

      {/* 新建用户：横版弹窗，字段两列排布。dismissable=false：
          表单填到一半误点遮罩就全丢，关闭只走「取消 / 创建」两个明确出口。 */}
      <AppModal
        open={cOpen}
        onClose={() => setCOpen(false)}
        title="新建用户"
        maxWidth="720px"
        dismissable={false}
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onPress={() => setCOpen(false)}>
              取消
            </Button>
            <Button variant="primary" isDisabled={cBusy} onPress={() => void create()}>
              {cBusy ? '创建中…' : '创建'}
            </Button>
          </div>
        }
      >
        <div className="grid grid-cols-1 gap-x-4.5 gap-y-3 sm:grid-cols-2">
          <TextField aria-label="用户名" isRequired value={cName} onChange={setCName}>
            <Label>用户名 *</Label>
            <Input placeholder="3-32 位字母数字-_." autoComplete="off" />
            <span className="text-muted text-[11px]">登录用。不支持中文与空格。</span>
          </TextField>
          <TextField aria-label="初始密码" value={cPassword} onChange={setCPassword}>
            <Label>初始密码</Label>
            <Input type="password" autoComplete="new-password" placeholder="留空则生成一次性设密链接" />
            {cRole === 'admin' && !cPassword ? (
              <span className="text-warning text-[11px]">创建管理员必须设置初始密码。</span>
            ) : (
              <span className="text-muted text-[11px]">留空后生成一次性设密链接；用户完成设置前不能登录。</span>
            )}
          </TextField>
          <div>
            <Label>角色</Label>
            <div className="mt-1.5">
              <RoleSelect value={cRole} onChange={setCRole} />
            </div>
            <span className="text-muted mt-1 block text-[11px]">管理员可见全部页面与所有用户的数据。</span>
          </div>
          <div>
            <Label>分组</Label>
            <div className="mt-1.5">
              <GroupSelect value={cGroup} onChange={setCGroup} groups={groups} />
            </div>
            <span className="text-muted mt-1 block text-[11px]">分组决定能用哪些模型。未分组 = 不限制。</span>
          </div>
          <TextField aria-label="额度上限（token）" value={cQuota} onChange={setCQuota}>
            <Label>额度上限（token）</Label>
            <Input type="number" min={0} step={1} placeholder="0" />
            <span className="text-muted text-[11px]">0 = 不限</span>
          </TextField>
        </div>
        {cErr ? (
          <div className="text-danger mt-3 text-[12.5px]" role="alert">
            {cErr}
          </div>
        ) : null}
      </AppModal>

      {/* 一次性设密链接。链接里包含明文 token，关闭时必须清掉组件状态。 */}
      <AppModal
        open={setupLinkFor !== null}
        onClose={closeSetupLink}
        title={setupLinkFor ? `设密链接 · ${setupLinkFor.username}` : '设密链接'}
        maxWidth="620px"
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onPress={closeSetupLink}>
              关闭
            </Button>
            <Button variant="primary" onPress={() => void copySetupLink()}>
              复制链接
            </Button>
          </div>
        }
      >
        <p className="text-muted text-[12.5px] leading-relaxed">
          将下面的链接发给用户。链接 <b>24 小时</b>内有效且只能使用一次；重新生成会让旧链接立即失效。
        </p>
        <Input
          aria-label="设密链接"
          className="mt-3 font-mono text-[12px]"
          value={setupLinkURL}
          onChange={() => {}}
          readOnly
          onFocus={(e) => e.target.select()}
        />
        {setupLinkExpiresAt ? (
          <div className="text-muted mt-2 text-[11px]">有效至 {fmtDateTime(setupLinkExpiresAt)}</div>
        ) : null}
        <div className="text-warning mt-2 text-[11px]">链接等同于一次性凭据，不要发到公开群组或长期留存。</div>
      </AppModal>

      {/* 编辑用户。同样不可点外关闭。 */}
      <AppModal
        open={editing !== null}
        onClose={() => setEditing(null)}
        title={editing ? `编辑用户 · ${editing.username}` : '编辑用户'}
        maxWidth="720px"
        dismissable={false}
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onPress={() => setEditing(null)}>
              取消
            </Button>
            <Button variant="primary" isDisabled={eBusy} onPress={() => void saveEdit()}>
              {eBusy ? '保存中…' : '保存'}
            </Button>
          </div>
        }
      >
        {editing ? (
          <>
            <div className="text-muted mb-3.5 text-[12px]">
              密钥 {editing.key_count} 把 · 已用 {fmtTokens(editing.used_tokens)} token · 余额{' '}
              {fmtBalance(editing.balance_cents, editing.balance_unlimited)}
              {editing.last_login_at ? ` · 上次登录 ${fmtDateTime(editing.last_login_at)}` : ''}
            </div>
            <div className="grid grid-cols-1 gap-x-4.5 gap-y-3 sm:grid-cols-2">
              <div>
                <Label>角色</Label>
                <div className="mt-1.5">
                  <RoleSelect
                    value={eRole}
                    onChange={setERole}
                    disabledSelf={editing.is_self}
                  />
                </div>
                {editing.is_self ? (
                  <span className="text-warning mt-1 block text-[11px]">不能降低自己的权限。</span>
                ) : !editing.has_password ? (
                  <span className="text-warning mt-1 block text-[11px]">该账号未设密码，不能升为管理员。</span>
                ) : (
                  <span className="text-muted mt-1 block text-[11px]">
                    管理员可见全部页面与所有用户的数据。改角色会让该用户所有登录状态立即失效。
                  </span>
                )}
              </div>
              <div>
                <Label>分组</Label>
                <div className="mt-1.5">
                  <GroupSelect value={eGroup} onChange={setEGroup} groups={groups} />
                </div>
                <span className="text-muted mt-1 block text-[11px]">分组决定能用哪些模型。未分组 = 不限制。</span>
              </div>
              <TextField aria-label="额度上限（token）" value={eQuota} onChange={setEQuota}>
                <Label>额度上限（token）</Label>
                <Input type="number" min={0} step={1} placeholder="0" />
                <span className="text-muted text-[11px]">0 = 不限。已用 {fmtTokens(editing.used_tokens)}。</span>
              </TextField>
              <TextField aria-label="备注" value={eRemark} onChange={setERemark}>
                <Label>备注</Label>
                <Input placeholder="可选，仅管理员可见" autoComplete="off" />
                <span className="text-muted text-[11px]">用户名不可改。</span>
              </TextField>
            </div>
            {eErr ? (
              <div className="text-danger mt-3 text-[12.5px]" role="alert">
                {eErr}
              </div>
            ) : null}
          </>
        ) : null}
      </AppModal>

      {/* 重置密码 */}
      <AppModal
        open={pwdFor !== null}
        onClose={() => setPwdFor(null)}
        title={pwdFor ? `重置密码 · ${pwdFor.username}` : '重置密码'}
        maxWidth="460px"
        dismissable={false}
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onPress={() => setPwdFor(null)}>
              取消
            </Button>
            <Button variant="primary" isDisabled={pwdBusy} onPress={() => void resetPassword()}>
              {pwdBusy ? '重置中…' : '确定'}
            </Button>
          </div>
        }
      >
        <div className="text-warning text-[12.5px]">重置后该用户的所有登录状态立即失效。</div>
        <TextField aria-label="新密码" isRequired value={pwdValue} onChange={setPwdValue}>
          <Label>新密码</Label>
          <Input type="password" autoComplete="new-password" />
          {pwdValue && checkPasswordStrength(pwdValue) ? (
            <span className="text-muted text-[11px]">{checkPasswordStrength(pwdValue)}</span>
          ) : (
            <span className="text-muted text-[11px]">至少 8 个字符，且含字母/数字/符号中的至少两类。</span>
          )}
        </TextField>
        {pwdErr ? (
          <div className="text-danger mt-2 text-[12.5px]" role="alert">
            {pwdErr}
          </div>
        ) : null}
      </AppModal>

      {/* 新建 / 改名分组 */}
      <AppModal
        open={showForm}
        onClose={() => setShowForm(false)}
        title={editingGroup ? '编辑分组' : '新建分组'}
        maxWidth="460px"
        dismissable={false}
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onPress={() => setShowForm(false)}>
              取消
            </Button>
            <Button variant="primary" isDisabled={fBusy} onPress={() => void submitGroupForm()}>
              {fBusy ? '保存中…' : '保存'}
            </Button>
          </div>
        }
      >
        <TextField aria-label="组名" isRequired value={fName} onChange={setFName}>
          <Label>组名 *</Label>
          <Input placeholder="研发 / 外包 / 试用" autoComplete="off" />
        </TextField>
        <TextField aria-label="说明" value={fDesc} onChange={setFDesc}>
          <Label>说明</Label>
          <Input placeholder="可选" autoComplete="off" />
        </TextField>
        {!editingGroup ? (
          <div className="text-warning mt-2 text-[11.5px]">
            新建的组默认<b>不限制</b>模型。建好后请到「配置模型」里勾选允许的模型。
          </div>
        ) : null}
        {fErr ? (
          <div className="text-danger mt-2 text-[12.5px]" role="alert">
            {fErr}
          </div>
        ) : null}
      </AppModal>

      {/* 配置模型白名单：勾选状态同样不该被误点遮罩清空。 */}
      <AppModal
        open={modelsFor !== null}
        onClose={() => setModelsFor(null)}
        title={modelsFor ? `模型白名单 · ${modelsFor.name}` : '模型白名单'}
        maxWidth="560px"
        dismissable={false}
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onPress={() => setModelsFor(null)}>
              取消
            </Button>
            <Button variant="primary" isDisabled={mBusy} onPress={() => void submitModels()}>
              {mBusy ? '保存中…' : `保存（已选 ${picked.length}）`}
            </Button>
          </div>
        }
      >
        {modelOptions.length === 0 ? (
          <div className="text-warning text-[12.5px]">
            还没有任何路由（公开模型名）。请先到「上游与模型 / 路由」里建好。
          </div>
        ) : (
          <ModelPicker value={picked} onChange={setPicked} options={modelOptions} />
        )}
        {mErr ? (
          <div className="text-danger mt-2 text-[12.5px]" role="alert">
            {mErr}
          </div>
        ) : null}
      </AppModal>
    </div>
  )
}
