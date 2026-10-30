import { useSearchParams } from 'react-router-dom'
import { Tabs, ToggleButton, ToggleButtonGroup } from '@heroui/react'
import { api, isAdmin } from '../api'
import { PageHeader } from '../ui/PageHeader'
import { UsageByGroup } from '../components/UsageByGroup'
import { ModelGroups } from '../components/ModelGroups'

/**
 * 用量与费用页（语义对齐旧 UsageStats.vue）：一个页面挂「按上游 / 按模型 /
 * 按密钥 /（管理员）统计归并」四个页签，tab 与 days 都在 URL query 上 ——
 * 分享/刷新/回退时统计口径不丢。/history（请求日志）在旧版也由本组件承载，
 * React 版等 UsageDetail 重建后接入同一模式。
 */

const DAY_OPTIONS = [1, 7, 30]
const DAY_LABELS: Record<number, string> = { 1: '近 1 天', 7: '近 7 天', 30: '近 30 天' }

// fetch 作为模块级稳定引用传给 UsageByGroup，避免 tab 切换/重渲染时重触发加载。
const fetchProviders = (from: number, to: number) => api.usageByProvider(from, to)
const fetchModels = (from: number, to: number) => api.usageByModel(from, to)
const fetchKeys = (from: number, to: number) => api.usageByKey(from, to)

export function UsageStats() {
  const [searchParams, setSearchParams] = useSearchParams()
  const admin = isAdmin()

  const tabs = [
    { key: 'providers', label: '按上游' },
    { key: 'models', label: '按模型' },
    { key: 'keys', label: '按密钥' },
    ...(admin ? [{ key: 'groups', label: '统计归并' }] : []),
  ]

  const rawTab = searchParams.get('tab') ?? 'providers'
  const tab = tabs.some((t) => t.key === rawTab) ? rawTab : 'providers'
  const daysRaw = Number(searchParams.get('days'))
  const days = DAY_OPTIONS.includes(daysRaw) ? daysRaw : 1

  const scope = admin ? '全站' : '仅你自己'
  const subtitle =
    tab === 'groups' ? `将跨上游的同名模型合并统计；不改变路由、转发或权限 · ${scope}` : ''

  const setTab = (key: string) => {
    const next = new URLSearchParams(searchParams)
    next.set('tab', key)
    setSearchParams(next)
  }
  const setDays = (value: number) => {
    // 换时间范围必须清掉页码：旧页码在新窗口里很可能落在界外。
    const next = new URLSearchParams(searchParams)
    next.set('days', String(value))
    next.delete('page')
    setSearchParams(next)
  }

  return (
    <>
      <PageHeader
        title="用量与费用"
        sub={subtitle || undefined}
        actions={
          tab !== 'groups' ? (
            <ToggleButtonGroup
              aria-label="时间范围"
              selectionMode="single"
              disallowEmptySelection
              selectedKeys={[String(days)]}
              onSelectionChange={(keys) => {
                const value = [...keys][0]
                if (value !== undefined) setDays(Number(value))
              }}
            >
              {DAY_OPTIONS.map((d) => (
                <ToggleButton key={d} id={String(d)} size="sm">
                  {DAY_LABELS[d]}
                </ToggleButton>
              ))}
            </ToggleButtonGroup>
          ) : undefined
        }
      />

      <Tabs
        aria-label="用量分析维度"
        selectedKey={tab}
        onSelectionChange={(key) => setTab(String(key))}
      >
        <Tabs.List>
          {tabs.map((t) => (
            <Tabs.Tab key={t.key} id={t.key}>
              {t.label}
            </Tabs.Tab>
          ))}
        </Tabs.List>
        <Tabs.Panel id={tab}>
          {tab === 'groups' && admin ? <ModelGroups /> : null}
          {tab === 'providers' ? (
            <UsageByGroup key="providers" firstCol="上游名称" kind="provider" fetch={fetchProviders} days={days} />
          ) : null}
          {tab === 'models' ? (
            <UsageByGroup key="models" firstCol="模型" kind="model" fetch={fetchModels} days={days} />
          ) : null}
          {tab === 'keys' ? (
            <UsageByGroup key="keys" firstCol="密钥" kind="key" fetch={fetchKeys} days={days} />
          ) : null}
        </Tabs.Panel>
      </Tabs>
    </>
  )
}
