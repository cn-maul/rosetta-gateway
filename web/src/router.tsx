import { createHashRouter, Navigate } from 'react-router-dom'
import { Placeholder } from './ui/Placeholder'

/**
 * 路由骨架（P1）：路径与元信息逐条对齐旧 router.ts 的路由表，
 * 页面全部暂指 Placeholder，P2 起逐个替换成真实组件。
 *
 * 与旧版的差异（方案 D2）：
 *  - hash 路由保留（go:embed 单二进制无服务端 fallback）；
 *  - meta 改叫 handle（react-router 惯例），adminOnly/public 沿用；
 *  - 旧 beforeEach 守卫（guardDecision/reapplyGuard）将在 P2 改写为
 *    路由级 loader：loader 在首次渲染前执行，硬刷新不再有「守卫没跑」窗口。
 */
export interface RouteHandle {
  title: string
  public?: boolean
  adminOnly?: boolean
}

const page = (name: string, handle: RouteHandle) => ({
  element: <Placeholder name={name} />,
  handle,
})

export const router = createHashRouter([
  { path: '/login', ...page('登录', { title: '登录', public: true }) },
  { path: '/set-password', ...page('设置密码', { title: '设置密码', public: true }) },
  { index: true, ...page('网关总览', { title: '网关总览' }) },
  { path: '/history', ...page('请求日志', { title: '请求日志' }) },
  { path: '/usage', ...page('用量与费用', { title: '用量与费用' }) },
  { path: '/models', ...page('可用模型', { title: '可用模型' }) },
  { path: '/wallet', ...page('余额与账单', { title: '余额与账单' }) },
  { path: '/profile', element: <Navigate to="/wallet" replace /> },
  { path: '/keys', ...page('API 密钥', { title: 'API 密钥' }) },
  { path: '/users', ...page('用户与权限', { title: '用户与权限', adminOnly: true }) },
  { path: '/providers', ...page('上游服务', { title: '上游服务', adminOnly: true }) },
  { path: '/routes', ...page('模型与路由', { title: '模型与路由', adminOnly: true }) },
  { path: '/model-groups', element: <Navigate to="/usage?tab=groups" replace /> },
  { path: '/networks', ...page('代理', { title: '代理', adminOnly: true }) },
  { path: '/providers/:id', ...page('上游详情', { title: '上游详情', adminOnly: true }) },
  { path: '/health', element: <Navigate to="/models" replace /> },
  { path: '/account', ...page('账号设置', { title: '账号设置' }) },
  { path: '/settings', ...page('系统设置', { title: '系统设置', adminOnly: true }) },
  { path: '*', element: <Navigate to="/" replace /> },
])
