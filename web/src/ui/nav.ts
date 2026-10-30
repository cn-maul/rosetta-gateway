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

// 侧栏导航：按任务与角色组织（语义对齐旧 App.vue 的 navigation computed，
// 图标从手写 SVG path 换成 lucide）。
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
    label: '运行',
    items: [
      { to: '/', label: '网关总览', icon: LayoutDashboard },
      { to: '/history', label: '请求日志', icon: FileText },
      { to: '/usage', label: '用量与费用', icon: ChartColumn },
    ],
  },
  {
    label: '网关配置',
    items: [
      { to: '/networks', label: '代理', icon: Globe },
      { to: '/routes', label: '模型与路由', icon: Route },
      { to: '/providers', label: '上游服务', icon: Server },
      { to: '/users', label: '用户与权限', icon: Users },
    ],
  },
  {
    label: '管理',
    items: [
      // 管理员也要一条独立的「API 密钥」入口：/users 与 /keys 共用同一组件，
      // 高亮、标题、h1 必须跟当前页签走（见 Shell 的 activeNavTo）。
      { to: '/keys', label: 'API 密钥', icon: KeyRound },
      { to: '/wallet', label: '账户与充值', icon: Wallet },
      { to: '/settings', label: '系统设置', icon: Settings },
    ],
  },
]

export const USER_NAV: NavGroup[] = [
  {
    label: '我的工作台',
    items: [
      { to: '/', label: '我的总览', icon: LayoutDashboard },
      { to: '/models', label: '可用模型', icon: Box },
      { to: '/keys', label: 'API 密钥', icon: KeyRound },
      { to: '/history', label: '我的调用', icon: FileText },
      { to: '/wallet', label: '余额与账单', icon: Wallet },
    ],
  },
]
