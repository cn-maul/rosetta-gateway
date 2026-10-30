import { createHashRouter, Navigate, redirect, type LoaderFunctionArgs } from 'react-router-dom'
import { isAdmin, loadSession, session } from './api'
import { AppRoot, Splash } from './ui/AppRoot'
import { Login } from './ui/Login'
import { PasswordSetup } from './ui/PasswordSetup'
import { Networks, networksLoader } from './pages/Networks'
import { UsageStats } from './pages/UsageStats'
import { Overview } from './pages/Overview'
import { Models } from './pages/Models'
import { Providers } from './pages/Providers'
import { Routes } from './pages/Routes'
import { ProviderDetail } from './pages/ProviderDetail'
import { Placeholder } from './ui/Placeholder'

export interface RouteHandle {
  title: string
  public?: boolean
  adminOnly?: boolean
  /** 内容区是否放开 max-w-app（如请求日志的宽表）。 */
  wide?: boolean
}

// 进程内只探测一次 /session + /me + /bootstrap（对齐旧版「启动时 loadSession 一次」）。
// 登出/登录只改 store 状态，不需要重新探测。
let sessionProbe: Promise<void> | null = null
export function ensureSession(): Promise<void> {
  sessionProbe ??= loadSession()
  return sessionProbe
}

// 旧守卫的等价物，但以 loader 形式在**首次渲染前**执行 ——
// 硬刷新不再有「beforeEach 没跑到」的窗口，旧版的 reapplyGuard 因此可以删除。
const authedLoader = (adminOnly: boolean) => async ({ request }: LoaderFunctionArgs) => {
  await ensureSession()
  const url = new URL(request.url)
  if (!session.get().me) {
    // redirect 只接受站内相对路径（在 loginLoader 里二次校验），防开放重定向
    throw redirect('/login?redirect=' + encodeURIComponent(url.pathname + url.search))
  }
  // 个人改密的旧地址对两种角色都映射到账号页（先于管理员判断，对齐旧守卫）
  if (url.pathname === '/settings' && url.searchParams.get('tab') === 'password') {
    throw redirect('/account')
  }
  if (adminOnly && !isAdmin()) {
    // 旧守卫：普通用户撞 admin-only 页 → /users 回密钥页，其余回总览
    throw redirect(url.pathname === '/users' ? '/keys' : '/')
  }
  return null
}

const loginLoader = async ({ request }: LoaderFunctionArgs) => {
  await ensureSession()
  if (!session.get().me) return null
  // 已登录的人不该停在登录页；回跳目标只接受站内相对路径（单斜杠开头且非 //）
  const url = new URL(request.url)
  const r = url.searchParams.get('redirect')
  throw redirect(r && r.startsWith('/') && !r.startsWith('//') ? r : '/')
}

// P2：页面尚未重建的路由统一指向占位组件，P4 逐个替换。
const page = (name: string, handle: RouteHandle) => ({
  element: <Placeholder name={name} />,
  handle,
  loader: authedLoader(handle.adminOnly === true),
})

// 旧 /providers?tab=… 地址兼容（原 beforeEnter）
const providersLoader = async (args: LoaderFunctionArgs) => {
  const url = new URL(args.request.url)
  const tab = url.searchParams.get('tab')
  if (tab === 'routes') throw redirect('/routes')
  if (tab === 'model-groups') throw redirect('/usage?tab=groups')
  return authedLoader(true)(args)
}

export const router = createHashRouter([
  {
    element: <AppRoot />,
    HydrateFallback: Splash,
    // 根 loader 保证任何导航（含登录页）之前会话探测已完成
    loader: async () => {
      await ensureSession()
      return null
    },
    children: [
      { path: '/login', element: <Login />, handle: { title: '登录', public: true }, loader: loginLoader },
      { path: '/set-password', element: <PasswordSetup />, handle: { title: '设置密码', public: true } },
      {
        index: true,
        element: <Overview />,
        handle: { title: '网关总览' },
        loader: authedLoader(false),
      },
      {
        path: '/history',
        element: <UsageStats />,
        handle: { title: '请求日志', wide: true },
        loader: authedLoader(false),
      },
      {
        path: '/usage',
        element: <UsageStats />,
        handle: { title: '用量与费用' },
        loader: authedLoader(false),
      },
      {
        path: '/models',
        element: <Models />,
        handle: { title: '可用模型' },
        loader: authedLoader(false),
      },
      { path: '/wallet', ...page('余额与账单', { title: '余额与账单' }) },
      { path: '/profile', element: <Navigate to="/wallet" replace /> },
      { path: '/keys', ...page('API 密钥', { title: 'API 密钥' }) },
      { path: '/users', ...page('用户与权限', { title: '用户与权限', adminOnly: true }) },
      {
        path: '/providers',
        element: <Providers />,
        handle: { title: '上游服务', adminOnly: true },
        loader: providersLoader,
      },
      {
        path: '/routes',
        element: <Routes />,
        handle: { title: '模型与路由', adminOnly: true },
        loader: authedLoader(true),
      },
      { path: '/model-groups', element: <Navigate to="/usage?tab=groups" replace /> },
      {
        path: '/networks',
        element: <Networks />,
        handle: { title: '代理', adminOnly: true },
        loader: networksLoader,
      },
      {
        path: '/providers/:id',
        element: <ProviderDetail />,
        handle: { title: '上游详情', adminOnly: true },
        loader: authedLoader(true),
      },
      { path: '/health', element: <Navigate to="/models" replace /> },
      { path: '/account', ...page('账号设置', { title: '账号设置' }) },
      { path: '/settings', ...page('系统设置', { title: '系统设置', adminOnly: true }) },
      { path: '*', element: <Navigate to="/" replace /> },
    ],
  },
])
