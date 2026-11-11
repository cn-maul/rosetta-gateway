import { useLocation, useSearchParams } from 'react-router-dom'
import { Tabs, ToggleButton, ToggleButtonGroup } from '@heroui/react'
import { api, isAdmin } from '../api'
import { PageHeader } from '../ui/PageHeader'
import { UsageByGroup } from '../components/UsageByGroup'
import { UsageByUser } from '../components/UsageByUser'
import { UsageFailuresPanel } from '../components/UsageFailuresPanel'
import { ModelGroups } from '../components/ModelGroups'
import { UsageDetail } from './UsageDetail'
import { oneOfInt, readPref, writePref } from '../ui/usePersistedState'

/**
 * 用量与费用页（语义对齐旧 UsageStats.vue）：一个页面挂「按上游 / 按模型 /
 * 按密钥 /（管理员）统计归并」四个页签，tab 与 days 都在 URL query 上 ——
 * 分享/刷新/回退时统计口径不丢。/history（请求日志）也由本组件承载：
 * 全宽布局 + UsageDetail 表格，不渲染页签。
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
  const logs = useLocation().pathname === '/history'

  const tabs = [
    { key: 'providers', label: '按上游' },
    { key: 'models', label: '按模型' },
    ...(admin ? [{ key: 'users', label: '按用户' }] : []),
    { key: 'keys', label: '按密钥' },
    // 失败原因与上面几个不是同一类维度：那些看「花了多少」，这个看
    // 「坏在哪」。放在用量页而不是单开一页，是因为它沿用了这里的时间
    // 范围控件与作用域（普通用户只看自己的失败）。
    { key: 'failures', label: '失败原因' },
    ...(admin ? [{ key: 'groups', label: '统计归并' }] : []),
  ]

  const rawTab = searchParams.get('tab') ?? 'providers'
  const tab = tabs.some((t) => t.key === rawTab) ? rawTab : 'providers'
  const daysRaw = Number(searchParams.get('days'))
  // 时间范围记在本地，且按页面分开记：请求日志是排障视角（默认近 7 天），
  // 用量页是看账视角（默认今天），两者混用一份偏好会互相打扰。
  const daysPrefKey = logs ? 'history.days' : 'usage.days'
  const daysDefault = logs ? 7 : 1
  const days = DAY_OPTIONS.includes(daysRaw)
    ? daysRaw
    : readPref(daysPrefKey, daysDefault, oneOfInt(DAY_OPTIONS))

  const scope = admin ? '全站' : '仅你自己'
  const title = logs ? '调用日志' : '用量与费用'
  const subtitle = logs
    ? `逐条查看调用结果、耗时与记录费用 · ${scope}`
    : tab === 'groups'
      ? `将跨上游的同名模型合并统计；不改变路由、转发或权限 · ${scope}`
      : tab === 'users'
        ? `按用户汇总消费与用量；含已删除用户的历史记录 · ${scope}`
        : tab === 'failures'
          ? `按错误码与上游统计失败构成；用来判断是不是某个上游在坏 · ${scope}`
          : ''

  const setTab = (key: string) => {
    const next = new URLSearchParams(searchParams)
    next.set('tab', key)
    setSearchParams(next)
  }
  const setDays = (value: number) => {
    writePref(daysPrefKey, value)
    // 换时间范围必须清掉页码：旧页码在新窗口里很可能落在界外。
    const next = new URLSearchParams(searchParams)
    next.set('days', String(value))
    next.delete('page')
    setSearchParams(next)
  }

  return (
    <>
      <PageHeader
        title={title}
        sub={subtitle || undefined}
        actions={
          logs || tab !== 'groups' ? (
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

      {logs ? (
        <UsageDetail days={days} key={days} />
      ) : (
        <Tabs
          aria-label="用量分析维度"
          selectedKey={tab}
          onSelectionChange={(key) => setTab(String(key))}
        >
          <Tabs.ListContainer>
            <Tabs.List>
              {tabs.map((t) => (
                <Tabs.Tab key={t.key} id={t.key}>
                  <Tabs.Indicator />
                  {t.label}
                </Tabs.Tab>
              ))}
            </Tabs.List>
          </Tabs.ListContainer>
          <Tabs.Panel id={tab}>
            {tab === 'groups' && admin ? <ModelGroups /> : null}
            {tab === 'providers' ? (
              <UsageByGroup key="providers" firstCol="上游名称" kind="provider" fetch={fetchProviders} days={days} />
            ) : null}
            {tab === 'models' ? (
              <UsageByGroup key="models" firstCol="模型" kind="model" fetch={fetchModels} days={days} />
            ) : null}
            {tab === 'users' && admin ? <UsageByUser days={days} /> : null}
            {tab === 'keys' ? (
              <UsageByGroup key="keys" firstCol="密钥" kind="key" fetch={fetchKeys} days={days} />
            ) : null}
            {tab === 'failures' ? <UsageFailuresPanel days={days} /> : null}
          </Tabs.Panel>
        </Tabs>
      )}
    </>
  )
}
