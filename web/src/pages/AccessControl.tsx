import { useState } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { Tabs, Button } from '@heroui/react'
import { isAdmin } from '../api'
import { PageHeader } from '../ui/PageHeader'
import { Placeholder } from '../ui/Placeholder'
import { Keys } from '../components/Keys'

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
  // 刷新用的重建计数。递增即让当前面板整体重挂载，各面板内部的加载逻辑
  // 都在挂载时执行，不需要暴露刷新方法。
  const [bump, setBump] = useState(0)

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
      <PageHeader
        title={heading}
        sub={sub}
        actions={
          <Button variant="ghost" size="sm" onPress={() => setBump((b) => b + 1)}>
            刷新
          </Button>
        }
      />
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
            {/* key 同时包含页签与重建计数：切页签换组件，刷新换同组件的新实例。 */}
            <div key={`${tab}:${bump}`}>
              {tab === 'keys' ? (
                <Keys />
              ) : (
                <Placeholder name={tab === 'users' ? '用户管理' : '权限分组'} />
              )}
            </div>
          </Tabs.Panel>
        </Tabs>
      ) : (
        <div key={`keys:${bump}`}>
          <Keys />
        </div>
      )}
    </div>
  )
}
