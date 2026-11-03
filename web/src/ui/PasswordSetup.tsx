import { useEffect, useState, type FormEvent } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { Button, Card, Checkbox, Input, Label, TextField, toast } from '@heroui/react'
import { ApiFail, checkPasswordSetup, completePasswordSetup } from '../api'
import { checkPasswordStrength } from '../password'
import { fmtDateTime } from '../fmt'

// 一次性设密链接页（语义对齐旧 PasswordSetup.vue）。
export function PasswordSetup() {
  const navigate = useNavigate()
  const location = useLocation()
  const token = new URLSearchParams(location.search).get('token')?.trim() ?? ''

  const [checking, setChecking] = useState(() => token !== '')
  const [valid, setValid] = useState(false)
  const [username, setUsername] = useState('')
  const [expiresAt, setExpiresAt] = useState(0)
  const [password, setPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [showPassword, setShowPassword] = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!token) return
    let cancelled = false
    ;(async () => {
      try {
        const status = await checkPasswordSetup(token)
        if (cancelled) return
        setValid(status.valid)
        setUsername(status.username ?? '')
        setExpiresAt(status.expires_at ?? 0)
      } catch (e) {
        if (!cancelled) setError(e instanceof ApiFail ? e.message : '无法校验设密链接')
      } finally {
        if (!cancelled) setChecking(false)
      }
    })()
    return () => {
      cancelled = true
    }
  }, [token])

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (submitting || !valid) return
    setError('')
    const weak = checkPasswordStrength(password)
    if (weak) {
      setError(weak)
      return
    }
    if (password !== confirmPassword) {
      setError('两次输入的新密码不一致')
      return
    }
    setSubmitting(true)
    try {
      await completePasswordSetup(token, password)
      toast.success('密码已设置，已登录')
      await navigate('/', { replace: true })
    } catch (err) {
      setError(err instanceof ApiFail ? err.message : '设置密码失败')
    } finally {
      setSubmitting(false)
    }
  }

  const type = showPassword ? ('text' as const) : ('password' as const)

  return (
    <main className="grid min-h-dvh place-items-center bg-background p-7 text-foreground">
      <Card className="w-full max-w-[430px]">
        <Card.Content className="flex flex-col p-7">
          <div className="text-muted font-mono text-[10px] tracking-[0.18em]">ROSETTA GATEWAY</div>
          <div className="bg-accent text-accent-foreground mt-4 grid h-10 w-10 place-items-center rounded-xl text-xl font-bold">
            R
          </div>
          <h1 className="pt-3.5 text-[22px] font-semibold">设置登录密码</h1>

          {checking && (
            <div className="text-muted border-separator mt-5 rounded-xl border bg-surface-secondary p-4.5 text-[13px]">
              正在校验一次性链接…
            </div>
          )}

          {!checking && !valid && (
            <div className="text-muted border-separator mt-5 rounded-xl border bg-surface-secondary p-4.5 text-[13px]">
              <strong className="text-foreground">链接无效或已过期</strong>
              <p className="mt-1.5">
                {error || '请联系管理员重新生成一次性设密链接。系统不开放公开注册。'}
              </p>
            </div>
          )}

          {!checking && valid && (
            <form className="mt-1 flex flex-col" onSubmit={submit}>
              <p className="text-[13px]">
                为账号 <code className="bg-surface-secondary rounded px-1 py-0.5 font-mono">{username}</code>{' '}
                设置密码，完成后会直接登录。
              </p>
              {expiresAt !== 0 && (
                <p className="text-muted mt-1 text-xs">
                  本链接有效至 {fmtDateTime(expiresAt)}，只能使用一次。
                </p>
              )}

              <div className="mt-3.5 flex items-baseline justify-between gap-3">
                <Label>新密码</Label>
                <Checkbox isSelected={showPassword} onChange={setShowPassword}>
                  <Checkbox.Content className="gap-2">
                    <Checkbox.Control>
                      <Checkbox.Indicator />
                    </Checkbox.Control>
                    <span>显示密码</span>
                  </Checkbox.Content>
                </Checkbox>
              </div>
              <TextField
                aria-label="新密码"
                className="mt-1"
                value={password}
                onChange={setPassword}
              >
                <Input type={type} autoComplete="new-password" autoFocus />
              </TextField>
              <div className="text-muted mt-1.5 text-[11px]">
                至少 8 个字符，且含字母、数字、符号中的至少两类。
              </div>

              <Label className="mt-3.5">确认新密码</Label>
              <TextField
                aria-label="确认新密码"
                className="mt-1"
                value={confirmPassword}
                onChange={setConfirmPassword}
              >
                <Input type={type} autoComplete="new-password" />
              </TextField>

              {error && (
                <div className="text-danger mt-3 text-xs" role="alert">
                  {error}
                </div>
              )}

              <Button type="submit" variant="primary" className="mt-5 w-full" isDisabled={submitting}>
                {submitting ? '设置中…' : '设置密码并登录'}
              </Button>
            </form>
          )}
        </Card.Content>
      </Card>
    </main>
  )
}
