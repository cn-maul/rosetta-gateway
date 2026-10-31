import { useLocation, useNavigate } from 'react-router-dom'
import { Tabs } from '@heroui/react'
import { isAdmin } from '../api'
import { PageHeader } from '../ui/PageHeader'
import { Keys } from '../components/Keys'
import { UsersGroups } from '../components/UsersGroups'

type TabKey = 'users' | 'groups' | 'keys'

/**
 * 用户 / 分组 / 密钥的宿主页签。
 *
 * 页标题与副标题都按**当前页签**算，而不是按角色。若标题用 isAdmin() 决定，
 * 管理员访问 /keys 时 h1 会是「用户与权限」—— 与浏览器标题（router meta.title
 * 是「API 密钥」）、侧栏高亮各说各话。
 */
export function AccessControl() {
  const navigate = useNavigate()
  const location = useLocation()
  // 页签状态从路由来：keys 走 /keys，users/groups 走 /users?tab=…。
  // 页签切换落到与该页签同名的路由，侧栏高亮、前进后退、分享链接才都一致。
  const tab: TabKey =
    !isAdmin() || location.pathname === '/keys'
      ? 'keys'
      : location.search.includes('tab=groups')
        ? 'groups'
        : 'users'
  const setTab = (key: TabKey) => {
    void navigate(key === 'keys' ? '/keys' : `/users?tab=${key}`)
  }
  const tabs: { key: TabKey; label: string }[] = isAdmin()
    ? [
        { key: 'users', label: '用户' },
        { key: 'groups', label: '权限分组' },
        { key: 'keys', label: 'API 密钥' },
      ]
    : [{ key: 'keys', label: 'API 密钥' }]

  const heading = tab === 'keys' ? 'API 密钥' : '用户与权限'
  const sub =
    tab === 'users'
      ? '账号、配额、登录状态与模型可见范围'
      : tab === 'groups'
        ? '模型白名单按组收窄 —— 空白名单表示不限制'
        : isAdmin()
          ? '客户端调用 /v1 时使用的 sk-gw- 密钥'
          : '客户端调用 /v1 时使用的 sk-gw- 密钥 · 归属恒为你自己'

  return (
    <div>
      {/* 页头不放刷新按钮：三个面板进入即拉数，写操作后各自重载自己的数据；
          手动刷新只会让人怀疑「刚才那次保存是不是没生效」。要强制重取就切页签。 */}
      <PageHeader title={heading} sub={sub} />
      {isAdmin() ? (
        <Tabs
          aria-label="用户 / 分组 / 访问密钥"
          selectedKey={tab}
          onSelectionChange={(key) => {
            if (key !== null) setTab(String(key) as TabKey)
          }}
        >
          <Tabs.List>
            {tabs.map((t) => (
              <Tabs.Tab key={t.key} id={t.key}>
                {t.label}
              </Tabs.Tab>
            ))}
          </Tabs.List>
          <Tabs.Panel id={tab}>
            {tab === 'keys' ? <Keys /> : <UsersGroups tab={tab === 'users' ? 'users' : 'groups'} />}
          </Tabs.Panel>
        </Tabs>
      ) : (
        <Keys />
      )}
    </div>
  )
}
