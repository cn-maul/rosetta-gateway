import type { ReactNode } from 'react'
import { Button, Switch } from '@heroui/react'
import { protocolLabel } from '../../protocols'
import { SectionCard } from '../../ui/SectionCard'
import { StatusChip } from '../../ui/StatusChip'
import type { Network, Provider } from '../../types'

function fmtTimeoutSec(ms: number): string {
  if (!ms) return '默认'
  const seconds = ms / 1000
  return `${Number.isInteger(seconds) ? seconds : seconds.toFixed(1)} 秒`
}

/** 概览键值行：标签固定宽 84px，值 flex:1 并在超长时省略。
 *  固定宽是这一排的读法所在 —— 六个字段的标签左对齐成一条竖线，
 *  眼睛才能横着扫下来。完整值在 title 里，改值走「编辑」。 */
function KvRow({ k, title, children }: { k: string; title?: string; children: ReactNode }) {
  return (
    <div className="border-separator flex min-w-0 items-center gap-3 border-b py-1.5 last:border-b-0">
      <span className="text-muted w-[84px] shrink-0 text-[12.5px]">{k}</span>
      <span title={title} className="min-w-0 flex-1 truncate text-[13px]">
        {children}
      </span>
    </div>
  )
}

/**
 * 上游概览卡（语义对齐旧 ProviderDetail.vue 的概览 SectionCard）。
 * 标题不重复写一遍名字 —— 页头 h1 已经是主标题。编辑/删除/启停都收在这里：
 * 改配置是低频动作，位置固定好找。
 */
export function ProviderOverviewCard({
  provider,
  networks,
  toggling,
  onEdit,
  onRemove,
  onToggle,
}: {
  provider: Provider
  networks: Network[]
  toggling: boolean
  onEdit: () => void
  onRemove: () => void
  onToggle: () => void
}) {
  const nameById = new Map(networks.map((n) => [n.id, n.name]))
  const networkLabel = (nid: string) => nameById.get(nid) ?? nid.slice(0, 8)

  return (
    <SectionCard
      title="概览"
      actions={
        <>
          <Button variant="outline" size="sm" onPress={onEdit}>
            编辑
          </Button>
          {/* 删除不给描边的中性样式：它和「编辑」并排，长一个样就得靠读字
              分辨。danger-soft 用红底把破坏性动作单独标出来。 */}
          <Button variant="danger-soft" size="sm" onPress={onRemove}>
            删除
          </Button>
          <Switch
            isSelected={provider.enabled}
            isDisabled={toggling}
            aria-label={`${provider.enabled ? '停用' : '启用'} ${provider.name || provider.slug}`}
            onChange={onToggle}
          >
            <Switch.Content>
              <Switch.Control>
                <Switch.Thumb />
              </Switch.Control>
            </Switch.Content>
          </Switch>
        </>
      }
    >
      <div className="flex flex-col">
        <KvRow k="类型">
          <StatusChip tone="dim">{protocolLabel(provider.protocol)}</StatusChip>
        </KvRow>
        {/* Endpoint 的可辨识信息在路径尾巴上，掐尾省略（…）会把
            「chat/completions」这半截丢掉 —— 换行而不是省略。 */}
        <KvRow k="Endpoint">
          <span className="font-mono break-all">{provider.endpoint}</span>
        </KvRow>
        <KvRow k="代理" title={provider.proxy_url || '直连（不经过代理）'}>
          {provider.network_id ? (
            <>
              {networkLabel(provider.network_id)}{' '}
              <span className="text-muted font-mono text-xs">（{provider.proxy_url}）</span>
            </>
          ) : (
            <span className="text-muted">直连（不经过代理）</span>
          )}
        </KvRow>
        <KvRow k="超时 / 重试">
          <span className="text-muted">
            {fmtTimeoutSec(provider.timeout_ms)} /{' '}
            {provider.max_retries ? `${provider.max_retries} 次` : '默认'}
          </span>
        </KvRow>
        <KvRow k="余额查询">
          {provider.balance_spec_json?.trim() ? (
            <StatusChip tone="info">自定义</StatusChip>
          ) : (
            <span className="text-muted">未配置</span>
          )}
        </KvRow>
      </div>
    </SectionCard>
  )
}
