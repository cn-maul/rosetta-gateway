import { Suspense, useEffect, useRef, useState, useSyncExternalStore } from 'react'
import { NavLink, Outlet, useLocation, useMatches, useNavigate } from 'react-router-dom'
import { Avatar, Button, Dropdown, Label, toast } from '@heroui/react'
import { LogOut, Moon, PanelLeft, Sun, UserRound } from 'lucide-react'
import { ApiFail, isAdmin, logout, session } from '../api'
import { useTheme } from './theme'
import { BrandMark } from './BrandMark'
import { loadBranding, useBranding } from './branding'
import { ADMIN_NAV, USER_NAV, type NavGroup } from './nav'
import { PageFallback } from './PageFallback'

const COLLAPSE_KEY = 'rosetta_gw_sidebar_collapsed'

function readCollapsed(): boolean {
  try {
    return localStorage.getItem(COLLAPSE_KEY) === '1'
  } catch {
    /* 隐私模式下读不了 localStorage，按默认展开 */
  }
  return false
}

function persistCollapsed(v: boolean) {
  try {
    localStorage.setItem(COLLAPSE_KEY, v ? '1' : '0')
  } catch {
    /* 忽略 */
  }
}

function useNarrow() {
  const [narrow, setNarrow] = useState(() => window.matchMedia('(max-width: 900px)').matches)
  useEffect(() => {
    const mq = window.matchMedia('(max-width: 900px)')
    const onChange = (e: MediaQueryListEvent) => setNarrow(e.matches)
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [])
  return narrow
}

/** 侧栏高亮用的路径：/users?tab=keys 实际停在密钥页签，该高亮「API 密钥」。 */
function activeNavTo(pathname: string, search: string): string {
  if (pathname === '/users' && new URLSearchParams(search).get('tab') === 'keys') return '/keys'
  return pathname
}

function locationLabel(pathname: string, admin: boolean, nav: NavGroup[]): string {
  if (pathname === '/account') return '账号 / 安全设置'
  for (const group of nav) {
    const hit = group.items.find(
      (i) => i.to === pathname || (i.to !== '/' && pathname.startsWith(i.to + '/')),
    )
    if (hit) return (admin ? '管理控制台' : '个人工作台') + ' / ' + group.label
  }
  return admin ? '管理控制台' : '个人工作台'
}

export function Shell() {
  const me = useSyncExternalStore(session.subscribe, () => session.get().me)
  const navigate = useNavigate()
  const location = useLocation()
  const { dark, toggle } = useTheme()
  const branding = useBranding()

  // 侧栏品牌与登录页共用同一份站点设置；已登录说明后端在跑，这里补一次
  // 拉取（登录页那次可能因为点了「直接输地址」而没发生）。
  useEffect(() => {
    void loadBranding()
  }, [])

  // ≤900px 视口强制图标栏（224px 会吃掉平板宽度），但用户自己的选择在
  // 回到宽屏后仍然生效 —— narrow 只影响显示，不写回存储。
  const [collapsed, setCollapsed] = useState(readCollapsed)
  const narrow = useNarrow()
  const railCollapsed = collapsed || narrow
  const toggleCollapse = () =>
    setCollapsed((prev) => {
      persistCollapsed(!prev)
      return !prev
    })

  const admin = isAdmin()
  const nav = admin ? ADMIN_NAV : USER_NAV
  const activeTo = activeNavTo(location.pathname, location.search)

  // 内容区在容器里滚（不是 window）：顶栏的发丝线在滚过 4px 后出现；
  // 路由切换把内容区滚回顶部。
  const contentRef = useRef<HTMLElement | null>(null)
  const [scrolled, setScrolled] = useState(false)
  useEffect(() => {
    const el = contentRef.current
    if (!el) return
    const onScroll = () => setScrolled(el.scrollTop > 4)
    el.addEventListener('scroll', onScroll, { passive: true })
    return () => el.removeEventListener('scroll', onScroll)
  }, [])
  useEffect(() => {
    contentRef.current?.scrollTo({ top: 0 })
    setScrolled(false)
  }, [location.pathname])

  const doLogout = async () => {
    try {
      await logout()
    } catch (e) {
      // 服务端吊销失败时本地会话必须保留：否则界面看起来已退出，而复制出去的
      // Bearer token 在自然过期前仍可用（审计 A7-03）。跳转由 401 watcher 统一负责。
      toast.danger(e instanceof ApiFail ? e.message : '登出失败，请重试')
      return
    }
    void navigate({ pathname: '/login' })
  }

  return (
    <div className="flex h-dvh bg-background text-foreground">
      <aside
        className={
          'flex shrink-0 flex-col border-separator border-r transition-[width] ' +
          (railCollapsed ? 'w-14' : 'w-56')
        }
      >
        <div className="flex items-center gap-2.5 px-3 py-4">
          <BrandMark size={36} />
          {!railCollapsed && (
            <div className="min-w-0">
              <div className="truncate text-sm font-semibold">{branding.name}</div>
              <div className="text-muted truncate text-xs">
                {admin ? '管理控制台' : '个人工作台'}
              </div>
            </div>
          )}
        </div>

        <nav aria-label="网关导航" className="min-h-0 flex-1 overflow-y-auto px-2">
          {nav.map((group) => (
            <div key={group.label} className="mb-1">
              <div className="border-separator my-2 border-t">
                {!railCollapsed && (
                  <div className="text-muted px-2 pt-1.5 text-[10.5px] tracking-wider">{group.label}</div>
                )}
              </div>
              {group.items.map((item) => {
                const active =
                  item.to === activeTo || (item.to !== '/' && activeTo.startsWith(item.to + '/'))
                return (
                  <NavLink
                    key={item.to}
                    to={item.to}
                    title={railCollapsed ? item.label : undefined}
                    aria-label={item.label}
                    aria-current={active ? 'page' : undefined}
                    className={
                      'mb-0.5 flex items-center gap-2.5 rounded-lg px-2.5 py-2 text-sm ' +
                      (railCollapsed ? 'justify-center' : '') +
                      (active
                        ? ' bg-surface-secondary text-foreground font-medium'
                        : ' text-muted hover:bg-surface-secondary/60 hover:text-foreground')
                    }
                  >
                    <item.icon size={18} strokeWidth={1.75} aria-hidden />
                    {!railCollapsed && <span>{item.label}</span>}
                  </NavLink>
                )
              })}
            </div>
          ))}
        </nav>

        <div className="border-separator border-t px-2 py-3">
          <div className={'flex items-center gap-1 px-2.5 ' + (railCollapsed ? 'flex-col' : 'justify-between')}>
            <span className={railCollapsed ? 'hidden' : 'text-muted text-[10.5px]'}>
              v{__APP_VERSION__} · rosetta {__ROSETTA_VERSION__}
            </span>
            <Button
              isIconOnly
              size="sm"
              variant="ghost"
              aria-label={railCollapsed ? '展开侧边栏' : '收起侧边栏'}
              onPress={toggleCollapse}
            >
              <PanelLeft size={16} aria-hidden />
            </Button>
          </div>
        </div>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <header
          className={
            'border-separator z-20 flex h-12 shrink-0 items-center gap-3 border-b px-4 ' +
            (scrolled ? 'shadow-sm' : '')
          }
        >
          <span className="text-muted truncate text-[12.5px]">
            {locationLabel(location.pathname, admin, nav)}
          </span>
          <div className="ms-auto flex items-center gap-1">
            {me && (
              <Dropdown>
                <Dropdown.Trigger>
                  <Button
                    variant="ghost"
                    size="sm"
                    aria-label={`${me.username}（${me.role === 'admin' ? '管理员' : '普通用户'}）`}
                  >
                    <Avatar size="sm">
                      <Avatar.Fallback>{me.username.slice(0, 1)}</Avatar.Fallback>
                    </Avatar>
                    <span className="text-[12.5px]">{me.username}</span>
                  </Button>
                </Dropdown.Trigger>
                <Dropdown.Popover>
                  <Dropdown.Menu
                    onAction={(key) => {
                      if (key === 'account') void navigate({ pathname: '/account' })
                      else if (key === 'logout') void doLogout()
                    }}
                  >
                    <Dropdown.Item id="who" textValue={me.username}>
                      <Label>
                        {me.username}（{me.role === 'admin' ? '管理员' : '普通用户'}）
                      </Label>
                    </Dropdown.Item>
                    <Dropdown.Item id="account" textValue="账号设置">
                      <UserRound size={14} aria-hidden />
                      <Label>账号设置</Label>
                    </Dropdown.Item>
                    <Dropdown.Item id="logout" textValue="退出登录">
                      <LogOut size={14} aria-hidden />
                      <Label>退出登录</Label>
                    </Dropdown.Item>
                  </Dropdown.Menu>
                </Dropdown.Popover>
              </Dropdown>
            )}
            <Button
              isIconOnly
              size="sm"
              variant="ghost"
              aria-label={dark ? '切换到亮色模式' : '切换到暗色模式'}
              onPress={toggle}
            >
              {dark ? <Sun size={16} aria-hidden /> : <Moon size={16} aria-hidden />}
            </Button>
          </div>
        </header>

        <main ref={contentRef} className="min-h-0 flex-1 overflow-y-auto">
          <div className={useMatches().some((m) => (m.handle as { wide?: boolean } | null)?.wide) ? 'p-gutter' : 'mx-auto max-w-app p-gutter'}>
            {/* 路由级懒加载：页面 chunk 到齐前先铺骨架，内容区不会空白 */}
            <Suspense fallback={<PageFallback />}>
              <Outlet />
            </Suspense>
          </div>
        </main>
      </div>
    </div>
  )
}
