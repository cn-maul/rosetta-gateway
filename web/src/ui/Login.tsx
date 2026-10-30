import { useState, useSyncExternalStore, type FormEvent } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { Alert, Button, Card, Checkbox, Input, Label, TextField, toast } from '@heroui/react'
import { ApiFail, bootstrapSetup, loadSession, login, session } from '../api'
import { checkPasswordStrength } from '../password'

/**
 * 登录页。三种形态互斥，由 session 状态决定（语义对齐旧 Login.vue，不要合并）：
 *
 *   1. offline（!backendReady）—— /session 探测失败，**不渲染任何表单**；
 *   2. degraded —— 网关已响应但 /bootstrap 读取失败，只给重试；
 *   3. setup（needsSetup）—— 全新部署，唯一一次免鉴权设密码；
 *   4. login —— 普通用户名 + 密码。
 */
export function Login() {
  const me = useSyncExternalStore(session.subscribe, () => session.get().me)
  const navigate = useNavigate()
  const location = useLocation()

  const username = useState('')
  const password = useState('')
  const newPwd = useState('')
  const confirmPwd = useState('')
  const showPwd = useState(false)
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')

  const mode = (() => {
    const s = session.get()
    if (!s.backendReady) return 'offline'
    if (s.bootstrapCheckFailed) return 'degraded'
    if (s.needsSetup) return 'setup'
    return 'login'
  })()

  /** 只接受站内相对路径：redirect 来自 URL query，不校验就是开放重定向。 */
  async function leaveToBack() {
    const params = new URLSearchParams(location.search)
    const back = params.get('redirect') ?? '/'
    await navigate(
      back.startsWith('/') && !back.startsWith('//') ? back : '/',
      { replace: true },
    )
  }

  async function submitLogin(e: FormEvent) {
    e.preventDefault()
    if (submitting) return
    setError('')
    if (!username[0].trim() || !password[0]) {
      setError('请填写用户名与密码')
      return
    }
    setSubmitting(true)
    try {
      await login(username[0].trim(), password[0])
      toast.success('已登录')
      await leaveToBack()
    } catch (err) {
      // 后端对「用户不存在」与「密码错误」返回同一句话（防枚举设计），
      // 原样透出，**不要**前端再判断是哪种错 —— 那等于重造账号枚举 oracle。
      setError(err instanceof ApiFail ? err.message : '登录失败')
    } finally {
      setSubmitting(false)
    }
  }

  async function submitSetup(e: FormEvent) {
    e.preventDefault()
    if (submitting) return
    setError('')
    const weak = checkPasswordStrength(newPwd[0])
    if (weak) {
      setError(weak)
      return
    }
    // 二次确认：这个入口一辈子只开一次，输错没有「再来一次」。
    if (newPwd[0] !== confirmPwd[0]) {
      setError('两次输入的新密码不一致')
      return
    }
    setSubmitting(true)
    try {
      await bootstrapSetup(newPwd[0])
      toast.success('密码已设置')
      await leaveToBack()
    } catch (err) {
      setError(err instanceof ApiFail ? err.message : '设置密码失败')
    } finally {
      setSubmitting(false)
    }
  }

  async function retryStartup() {
    setError('')
    session.set({ checked: false })
    await loadSession()
  }

  // 已登录的人不该再看见登录页（loader 也会拦，这里兜一层渲染态）
  if (me) return null

  const s = session.get()
  const type = showPwd[0] ? ('text' as const) : ('password' as const)

  return (
    <main className="grid min-h-dvh place-items-center bg-background p-6 text-foreground">
      <Card className="w-full max-w-[360px]">
        <Card.Content className="flex flex-col items-center gap-1 p-6 pt-7">
          <div className="bg-accent text-accent-foreground grid h-10 w-10 place-items-center rounded-[10px] text-xl font-bold">
            R
          </div>
          <h1 className="pt-3 text-[17px] font-semibold">rosetta-gateway</h1>
          <div className="text-muted pb-3 text-xs">网关管理后台</div>

          {mode === 'offline' && (
            <Alert status="warning" className="w-full">
              <Alert.Content>
                <Alert.Title>无法连接到网关</Alert.Title>
                <Alert.Description>
                  本机服务没有响应，或当前页面指向的地址不对。请确认网关进程正在运行、监听地址正确，然后刷新本页面。
                </Alert.Description>
              </Alert.Content>
            </Alert>
          )}

          {mode === 'degraded' && (
            <div className="w-full">
              <Alert status="warning" className="w-full">
                <Alert.Content>
                  <Alert.Title>无法读取启动状态</Alert.Title>
                  <Alert.Description>网关已响应，但首次设置状态读取失败，暂时无法确定应显示登录还是设密。</Alert.Description>
                </Alert.Content>
              </Alert>
              <Button variant="primary" className="mt-4 w-full" onPress={retryStartup}>
                重试
              </Button>
            </div>
          )}

          {mode === 'setup' && (
            <form className="flex w-full flex-col" onSubmit={submitSetup}>
              <Alert status="accent" className="mb-3 w-full">
                <Alert.Content>
                  <Alert.Title>首次设置密码</Alert.Title>
                  <Alert.Description>
                    为第一个管理员账号 <code>{s.setupUsername}</code>{' '}
                    设置密码，设完直接进入后台。**只有这一次机会**，提交后这个入口永久关闭。
                  </Alert.Description>
                </Alert.Content>
              </Alert>

              <div className="flex items-baseline justify-between gap-2">
                <Label>新密码</Label>
                <Checkbox isSelected={showPwd[0]} onChange={showPwd[1]}>
                  显示密码
                </Checkbox>
              </div>
              <TextField
                aria-label="新密码"
                className="mt-1"
                value={newPwd[0]}
                onChange={newPwd[1]}
              >
                <Input type={type} autoComplete="new-password" />
              </TextField>
              {checkPasswordStrength(newPwd[0]) && (
                <div className="text-muted mt-1.5 text-xs">{checkPasswordStrength(newPwd[0])}</div>
              )}

              <Label className="mt-3">确认新密码</Label>
              <TextField
                aria-label="确认新密码"
                className="mt-1"
                value={confirmPwd[0]}
                onChange={confirmPwd[1]}
              >
                <Input type={type} autoComplete="new-password" />
              </TextField>

              {error && (
                <div className="text-danger mt-2.5 text-xs" role="alert">
                  {error}
                </div>
              )}

              <Button type="submit" variant="primary" className="mt-5 w-full" isDisabled={submitting}>
                {submitting ? '设置中…' : '设置密码并进入'}
              </Button>
            </form>
          )}

          {mode === 'login' && (
            <form className="flex w-full flex-col" onSubmit={submitLogin}>
              <Label className="mt-1">用户名</Label>
              <TextField
                aria-label="用户名"
                className="mt-1"
                value={username[0]}
                onChange={username[1]}
              >
                <Input
                  autoComplete="username"
                  autoCapitalize="off"
                  autoCorrect="off"
                  spellCheck={false}
                />
              </TextField>

              <Label className="mt-3">密码</Label>
              <TextField
                aria-label="密码"
                className="mt-1"
                value={password[0]}
                onChange={password[1]}
              >
                <Input type="password" autoComplete="current-password" />
              </TextField>

              {error && (
                <div className="text-danger mt-2.5 text-xs" role="alert">
                  {error}
                </div>
              )}

              <Button type="submit" variant="primary" className="mt-5 w-full" isDisabled={submitting}>
                {submitting ? '登录中…' : '登录'}
              </Button>
            </form>
          )}
        </Card.Content>
      </Card>
    </main>
  )
}
