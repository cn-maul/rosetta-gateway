import {
  Box,
  ChartColumn,
  FileText,
  Globe,
  KeyRound,
  LayoutDashboard,
  Route,
  Server,
  Settings,
  Users,
  Wallet,
  type LucideIcon,
} from 'lucide-react'

// 侧栏导航：按使用频率组织（P1 登录即答案——高频入口排前面，组名是任务
// 语义不是页面类型）。/users 与 /keys 共用同一组件，高亮、标题、h1 必须
// 跟当前页签走（见 Shell 的 activeNavTo）。
export interface NavItem {
  to: string
  label: string
  icon: LucideIcon
}

export interface NavGroup {
  label: string
  items: NavItem[]
}

export const ADMIN_NAV: NavGroup[] = [
  {
    label: '常用',
    items: [
      { to: '/', label: '网关总览', icon: LayoutDashboard },
      { to: '/history', label: '调用日志', icon: FileText },
      { to: '/usage', label: '用量与费用', icon: ChartColumn },
      { to: '/keys', label: 'API 密钥', icon: KeyRound },
      { to: '/wallet', label: '账户与充值', icon: Wallet },
    ],
  },
  {
    label: '管理配置',
    items: [
      { to: '/providers', label: '供应商', icon: Server },
      { to: '/routes', label: '路由', icon: Route },
      { to: '/users', label: '用户与权限', icon: Users },
      { to: '/networks', label: '代理', icon: Globe },
      { to: '/settings', label: '设置', icon: Settings },
    ],
  },
]

export const USER_NAV: NavGroup[] = [
  {
    label: '我的工作台',
    items: [
      { to: '/', label: '我的总览', icon: LayoutDashboard },
      { to: '/history', label: '调用日志', icon: FileText },
      { to: '/usage', label: '消耗与费用', icon: ChartColumn },
      { to: '/keys', label: 'API 密钥', icon: KeyRound },
      { to: '/wallet', label: '余额与账单', icon: Wallet },
      { to: '/models', label: '可用模型', icon: Box },
    ],
  },
]
