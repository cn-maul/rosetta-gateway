import { useEffect, useState } from 'react'
import { Button, Input, Label, TextField, toast } from '@heroui/react'
import { api, ApiFail } from '../../api'
import { AppModal } from '../../ui/AppModal'
import { fmtTokens, fmtDateTime, fmtBalance, copyText } from '../../fmt'
import { checkPasswordStrength } from '../../password'
import { GroupSelect, RoleSelect } from './UserSelects'
import type { Group, User, UserRole } from '../../types'

interface UserCreateModalProps {
  open: boolean
  onClose: () => void
  groups: Group[]
  /** 创建成功后由弹窗调用（父级刷新列表）。 */
  onSaved: () => void | Promise<void>
  /** 密码留空创建成功时调用：父级接着生成一次性设密链接。 */
  onNeedsSetupLink: (created: User) => void | Promise<void>
}

/** 新建用户：横版弹窗，字段两列排布。dismissable=false：
 * 表单填到一半误点遮罩就全丢，关闭只走「取消 / 创建」两个明确出口。 */
export function UserCreateModal({ open, onClose, groups, onSaved, onNeedsSetupLink }: UserCreateModalProps) {
  const [name, setName] = useState('')
  const [password, setPassword] = useState('')
  const [role, setRole] = useState<UserRole>('user')
  const [group, setGroup] = useState('')
  const [quota, setQuota] = useState('0')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (!open) return
    setName('')
    setPassword('')
    setRole('user')
    setGroup('')
    setQuota('0')
    setErr('')
  }, [open])

  const create = async () => {
    if (busy) return
    setErr('')
    if (!name.trim()) {
      setErr('用户名必填')
      return
    }
    // 管理员空密码不是「强度不足」，是安全边界：空密码 admin 会让免鉴权的
    // bootstrap 引导判定重新命中。必须提交前拦掉，而不是等后端回 400。
    if (role === 'admin' && !password) {
      setErr('创建管理员必须设置初始密码（管理员不允许空密码账号）')
      return
    }
    // 普通用户初始密码可留空（建出「尚未设置密码」的账号），但填了就必须合规。
    if (password) {
      const weak = checkPasswordStrength(password)
      if (weak) {
        setErr(weak)
        return
      }
    }
    setBusy(true)
    const needsSetupLink = !password
    try {
      const created = await api.createUser({
        username: name.trim(),
        // 密码留空则不传 —— 后端接受空密码，建出「尚未设置密码」的账号。
        password: password || undefined,
        role,
        group_id: group || undefined,
        quota_tokens: Number(quota) || 0,
      })
      onClose()
      setName('')
      setPassword('')
      setRole('user')
      setGroup('')
      setQuota('0')
      await onSaved()
      toast.success('已创建')
      if (needsSetupLink) await onNeedsSetupLink(created)
    } catch (e) {
      setErr(e instanceof ApiFail ? e.message : '创建失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <AppModal
      open={open}
      onClose={onClose}
      title="新建用户"
      maxWidth="720px"
      dismissable={false}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onPress={onClose}>
            取消
          </Button>
          <Button variant="primary" isDisabled={busy} onPress={() => void create()}>
            {busy ? '创建中…' : '创建'}
          </Button>
        </div>
      }
    >
      <div className="grid grid-cols-1 gap-x-4.5 gap-y-3 sm:grid-cols-2">
        <TextField aria-label="用户名" isRequired value={name} onChange={setName}>
          <Label>用户名 *</Label>
          <Input placeholder="3-32 位字母数字-_." autoComplete="off" />
          <span className="text-muted text-[11px]">登录用。不支持中文与空格。</span>
        </TextField>
        <TextField aria-label="初始密码" value={password} onChange={setPassword}>
          <Label>初始密码</Label>
          <Input type="password" autoComplete="new-password" placeholder="留空则生成一次性设密链接" />
          {role === 'admin' && !password ? (
            <span className="text-warning text-[11px]">创建管理员必须设置初始密码。</span>
          ) : (
            <span className="text-muted text-[11px]">留空后生成一次性设密链接；用户完成设置前不能登录。</span>
          )}
        </TextField>
        <div>
          <Label>角色</Label>
          <div className="mt-1.5">
            <RoleSelect value={role} onChange={setRole} />
          </div>
          <span className="text-muted mt-1 block text-[11px]">管理员可见全部页面与所有用户的数据。</span>
        </div>
        <div>
          <Label>分组</Label>
          <div className="mt-1.5">
            <GroupSelect value={group} onChange={setGroup} groups={groups} />
          </div>
          <span className="text-muted mt-1 block text-[11px]">分组决定能用哪些模型。未分组 = 不限制。</span>
        </div>
        <TextField aria-label="额度上限（token）" value={quota} onChange={setQuota}>
          <Label>额度上限（token）</Label>
          <Input type="number" min={0} step={1} placeholder="0" />
          <span className="text-muted text-[11px]">0 = 不限</span>
        </TextField>
      </div>
      {err ? (
        <div className="text-danger mt-3 text-[12.5px]" role="alert">
          {err}
        </div>
      ) : null}
    </AppModal>
  )
}

/** 编辑用户。同样不可点外关闭。角色/分组/额度/备注在这里收口；
 * 用户名不可改，改密走独立弹窗。 */
export function UserEditModal({
  user,
  onClose,
  groups,
  onSaved,
}: {
  user: User | null
  onClose: () => void
  groups: Group[]
  onSaved: () => void | Promise<void>
}) {
  const [role, setRole] = useState<UserRole>('user')
  const [group, setGroup] = useState('')
  const [quota, setQuota] = useState('0')
  const [remark, setRemark] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (!user) return
    setRole(user.role)
    setGroup(user.group_id || '')
    setQuota(String(user.quota_tokens || 0))
    setRemark(user.remark || '')
    setErr('')
  }, [user])

  const save = async () => {
    const u = user
    if (!u || busy) return
    setErr('')
    // 后端也会拒（400），先挡住是为了不让人白等一个来回。
    if (u.is_self && role !== 'admin') {
      setErr('不能降低自己的权限（会把自己锁在门外）')
      return
    }
    if (role === 'admin' && u.role !== 'admin' && !u.has_password) {
      setErr('该账号尚未设置密码，请先生成一次性设密链接，再升级为管理员')
      return
    }
    if (Number(quota) < 0) {
      setErr('额度上限不能为负')
      return
    }
    const roleChanged = role !== u.role
    setBusy(true)
    try {
      await api.updateUser(u.id, {
        role,
        // 空串 = 明确的「移出分组」（后端用指针区分「不传」与「传空」），
        // 不能用 undefined，否则「清空分组」这个动作表达不出去。
        group_id: group,
        quota_tokens: Number(quota) || 0,
        remark: remark.trim(),
      })
      toast.success(roleChanged ? '已保存，该用户的登录状态已失效' : '已保存')
      onClose()
      await onSaved()
    } catch (e) {
      setErr(e instanceof ApiFail ? e.message : '保存失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <AppModal
      open={user !== null}
      onClose={onClose}
      title={user ? `编辑用户 · ${user.username}` : '编辑用户'}
      maxWidth="720px"
      dismissable={false}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onPress={onClose}>
            取消
          </Button>
          <Button variant="primary" isDisabled={busy} onPress={() => void save()}>
            {busy ? '保存中…' : '保存'}
          </Button>
        </div>
      }
    >
      {user ? (
        <>
          <div className="text-muted mb-3.5 text-[12px]">
            密钥 {user.key_count} 把 · 已用 {fmtTokens(user.used_tokens)} token · 余额{' '}
            {fmtBalance(user.balance_cents, user.balance_unlimited)}
            {user.last_login_at ? ` · 上次登录 ${fmtDateTime(user.last_login_at)}` : ''}
          </div>
          <div className="grid grid-cols-1 gap-x-4.5 gap-y-3 sm:grid-cols-2">
            <div>
              <Label>角色</Label>
              <div className="mt-1.5">
                <RoleSelect value={role} onChange={setRole} disabledSelf={user.is_self} />
              </div>
              {user.is_self ? (
                <span className="text-warning mt-1 block text-[11px]">不能降低自己的权限。</span>
              ) : !user.has_password ? (
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
                <GroupSelect value={group} onChange={setGroup} groups={groups} />
              </div>
              <span className="text-muted mt-1 block text-[11px]">分组决定能用哪些模型。未分组 = 不限制。</span>
            </div>
            <TextField aria-label="额度上限（token）" value={quota} onChange={setQuota}>
              <Label>额度上限（token）</Label>
              <Input type="number" min={0} step={1} placeholder="0" />
              <span className="text-muted text-[11px]">0 = 不限。已用 {fmtTokens(user.used_tokens)}。</span>
            </TextField>
            <TextField aria-label="备注" value={remark} onChange={setRemark}>
              <Label>备注</Label>
              <Input placeholder="可选，仅管理员可见" autoComplete="off" />
              <span className="text-muted text-[11px]">用户名不可改。</span>
            </TextField>
          </div>
          {err ? (
            <div className="text-danger mt-3 text-[12.5px]" role="alert">
              {err}
            </div>
          ) : null}
        </>
      ) : null}
    </AppModal>
  )
}

/** 重置密码。与后端 ValidatePassword 同一套门槛：只查长度的话，
 * 「88888888」在界面上合格、提交后被回 400，管理员只会看到「重置失败」。 */
export function UserPasswordModal({
  user,
  onClose,
  onSaved,
}: {
  user: User | null
  onClose: () => void
  onSaved: () => void | Promise<void>
}) {
  const [value, setValue] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (user) {
      setValue('')
      setErr('')
    }
  }, [user])

  const reset = async () => {
    const u = user
    if (!u || busy) return
    setErr('')
    const weak = checkPasswordStrength(value)
    if (weak) {
      setErr(weak)
      return
    }
    setBusy(true)
    try {
      await api.resetUserPassword(u.id, value)
      toast.success('密码已重置，该用户需重新登录')
      onClose()
      await onSaved()
    } catch (e) {
      setErr(e instanceof ApiFail ? e.message : '重置失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <AppModal
      open={user !== null}
      onClose={onClose}
      title={user ? `重置密码 · ${user.username}` : '重置密码'}
      maxWidth="460px"
      dismissable={false}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onPress={onClose}>
            取消
          </Button>
          <Button variant="primary" isDisabled={busy} onPress={() => void reset()}>
            {busy ? '重置中…' : '确定'}
          </Button>
        </div>
      }
    >
      <div className="text-warning text-[12.5px]">重置后该用户的所有登录状态立即失效。</div>
      <TextField aria-label="新密码" isRequired value={value} onChange={setValue}>
        <Label>新密码</Label>
        <Input type="password" autoComplete="new-password" />
        {value && checkPasswordStrength(value) ? (
          <span className="text-muted text-[11px]">{checkPasswordStrength(value)}</span>
        ) : (
          <span className="text-muted text-[11px]">至少 8 个字符，且含字母/数字/符号中的至少两类。</span>
        )}
      </TextField>
      {err ? (
        <div className="text-danger mt-2 text-[12.5px]" role="alert">
          {err}
        </div>
      ) : null}
    </AppModal>
  )
}

export interface SetupLinkInfo {
  user: User
  url: string
  expiresAt: number
}

/** 一次性设密链接。链接里包含明文 token，关闭时由父级立刻清掉状态
 * （后端只保存 token 哈希，前端也不留明文）。 */
export function SetupLinkModal({ info, onClose }: { info: SetupLinkInfo | null; onClose: () => void }) {
  if (!info) return null
  const copy = async () => {
    const ok = await copyText(info.url)
    if (ok) toast.success('设密链接已复制')
    else toast.danger('复制失败，请手动选择复制')
  }
  return (
    <AppModal
      open
      onClose={onClose}
      title={`设密链接 · ${info.user.username}`}
      maxWidth="620px"
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onPress={onClose}>
            关闭
          </Button>
          <Button variant="primary" onPress={() => void copy()}>
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
        value={info.url}
        onChange={() => {}}
        readOnly
        onFocus={(e) => e.target.select()}
      />
      {info.expiresAt ? <div className="text-muted mt-2 text-[11px]">有效至 {fmtDateTime(info.expiresAt)}</div> : null}
      <div className="text-warning mt-2 text-[11px]">链接等同于一次性凭据，不要发到公开群组或长期留存。</div>
    </AppModal>
  )
}
