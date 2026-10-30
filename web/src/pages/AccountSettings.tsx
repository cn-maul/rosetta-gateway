import { useState, useSyncExternalStore } from 'react'
import { useNavigate } from 'react-router-dom'
import { Button, Input, Label, TextField, toast } from '@heroui/react'
import { api, ApiFail, saveToken, session } from '../api'
import { checkPasswordStrength } from '../password'
import { PageHeader } from '../ui/PageHeader'
import { SectionCard } from '../ui/SectionCard'

export function AccountSettings() {
  const navigate = useNavigate()
  const me = useSyncExternalStore(session.subscribe, session.get)

  const [oldPwd, setOldPwd] = useState('')
  const [newPwd, setNewPwd] = useState('')
  const [confirmPwd, setConfirmPwd] = useState('')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')

  const submit = async () => {
    if (busy) return
    const strengthErr = checkPasswordStrength(newPwd)
    if (strengthErr) {
      setErr(strengthErr)
      return
    }
    if (newPwd !== confirmPwd) {
      setErr('两次输入的新密码不一致')
      return
    }
    setBusy(true)
    try {
      await api.changeMyPassword(oldPwd, newPwd)
      // 后端改密即撤销会话，本地同步清除，避免继续使用已失效的身份。
      saveToken('')
      session.set({ me: null })
      toast.success('密码已更新，请用新密码重新登录')
      setOldPwd('')
      setNewPwd('')
      setConfirmPwd('')
      await navigate('/login?redirect=/account', { replace: true })
    } catch (e) {
      setErr(e instanceof ApiFail ? e.message : '修改密码失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <div>
      <PageHeader title="账号设置" sub={`${me.me?.username ?? ''} · 管理自己的登录安全`} />
      <SectionCard title="修改密码" description="修改后需要重新登录">
        <form
          className="flex max-w-[480px] flex-col gap-4.5"
          onSubmit={(e) => {
            e.preventDefault()
            void submit()
          }}
        >
          <TextField aria-label="当前密码" isRequired value={oldPwd} onChange={setOldPwd}>
            <Label>当前密码</Label>
            <Input type="password" autoComplete="current-password" />
          </TextField>
          <TextField aria-label="新密码" isRequired value={newPwd} onChange={setNewPwd}>
            <Label>新密码</Label>
            <Input type="password" autoComplete="new-password" />
            <span className="text-muted text-[11px]">{checkPasswordStrength(newPwd) || '强度符合要求'}</span>
          </TextField>
          <TextField aria-label="确认新密码" isRequired value={confirmPwd} onChange={setConfirmPwd}>
            <Label>确认新密码</Label>
            <Input type="password" autoComplete="new-password" />
          </TextField>
          {err ? (
            <div className="text-danger text-[13px]" role="alert">
              {err}
            </div>
          ) : null}
          <div>
            <Button type="submit" variant="primary" isDisabled={busy}>
              {busy ? '提交中…' : '修改密码'}
            </Button>
          </div>
        </form>
      </SectionCard>
    </div>
  )
}
