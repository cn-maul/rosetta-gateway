import { useEffect, useRef, useSyncExternalStore } from 'react'
import { Outlet, useLocation, useMatches, useNavigate } from 'react-router-dom'
import { Spinner } from '@heroui/react'
import { isAdmin, session } from '../api'
import { ConfirmDialog } from './ConfirmDialog'
import { Shell } from './Shell'
import type { RouteHandle } from '../router'

// 角色相关的标题与旧 router.ts 的 updateDocumentTitle 对齐：
// 同一路由在管理员与普通用户眼里叫法不同。
const ROLE_TITLES: [string, (admin: boolean) => string][] = [
  ['/', (admin) => (admin ? '网关总览' : '我的总览')],
  ['/history', () => '调用日志'],
  ['/wallet', (admin) => (admin ? '账户与充值' : '余额与账单')],
]

function useDocumentTitle() {
  const matches = useMatches()
  const location = useLocation()
  useEffect(() => {
    const leaf = matches[matches.length - 1]
    let title = (leaf?.handle as RouteHandle | undefined)?.title ?? ''
    const override = ROLE_TITLES.find(([p]) => p === location.pathname)
    if (override) title = override[1](isAdmin())
    document.title = title ? title + ' · Rosetta Gateway' : 'Rosetta Gateway'
  }, [matches, location.pathname])
}

// 运行中掉登录（api.ts 在 401 里把 me 置空）→ 送回登录页并带上 redirect。
// 只看「非空 → 空」的转变：启动探测期间 me 前后都是 null，那条转变不动作 ——
// 启动期的纠偏由路由 loader 负责（硬刷新也会跑 loader，无需旧版的 reapplyGuard）。
function useSessionExpiryWatcher() {
  const navigate = useNavigate()
  const location = useLocation()
  const me = useSyncExternalStore(session.subscribe, () => session.get().me)
  const prev = useRef(me)
  useEffect(() => {
    const wasLoggedIn = prev.current !== null
    prev.current = me
    if (wasLoggedIn && me === null && location.pathname !== '/login') {
      void navigate({
        pathname: '/login',
        search: '?redirect=' + encodeURIComponent(location.pathname + location.search),
      })
    }
  }, [me, location.pathname, location.search, navigate])
}

export function AppRoot() {
  useDocumentTitle()
  useSessionExpiryWatcher()
  const location = useLocation()
  // 登录页没有侧栏可言；其余路由都住在 Shell（侧栏 + 顶栏 + 页脚版本号）里。
  // Shell 渲染 <Outlet/>，页面组件由此进入内容区。
  const isPublic = location.pathname === '/login'
  return (
    <>
      <ConfirmDialog />
      {isPublic ? <Outlet /> : <Shell />}
    </>
  )
}

// 探测未完成时显示极简 splash（对齐旧 App.vue：避免刷新时闪一下完整后台再跳登录页）
export function Splash() {
  return (
    <div
      className="grid min-h-dvh place-items-center bg-background"
      role="status"
      aria-label="加载中"
    >
      <Spinner />
    </div>
  )
}
