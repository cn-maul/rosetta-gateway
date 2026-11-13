import { DataTable } from '../ui/DataTable'
import { Table } from '@heroui/react'
import { Button } from '@heroui/react'
import { StatusChip } from '../ui/StatusChip'
import { Switch } from '@heroui/react'
import type { Credential } from '../types'

/**
 * 凭据面板：某个上游下的多把 API 凭据（详情页概览旁的一列）。
 *
 * 与旧 CredentialPanel.vue 相同的取舍：刻意**只搬展示**不加抽象 ——
 * 数据源与操作回调都从父组件传入（props + 回调），父组件仍是唯一的
 * 写路径持有者。抽组件只减少单文件长度，不引入第二处状态真相。
 */

interface CredentialPanelProps {
  creds: Credential[]
  /** 正在提交启停的凭据 id：只给那一行的开关上防连点。 */
  toggling: string[]
  error?: string
  onRetry?: () => void
  onAdd: () => void
  onEdit: (cred: Credential) => void
  onRemove: (cred: Credential) => void
  onToggle: (cred: Credential) => void
}

// isPending 判某一行是否正在提交启停。
//
// 启停是「切换」语义（同一上游同时只启用一把），所以连点一次的后果是
// **两次相反的切换**：第一次把 A 打开（同时关掉 B），第二次又把 A 关掉。
// 用户想要的结果和最终状态完全相反，且期间请求池真的换了 key ——
// 那种「我明明点了一下启用，怎么又没了」没有任何自我修复的余地。

export function CredentialPanel({
  creds,
  toggling,
  error,
  onRetry,
  onAdd,
  onEdit,
  onRemove,
  onToggle,
}: CredentialPanelProps) {
  return (
    <section className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">凭据（{creds.length}）</h2>
        <Button variant="outline" size="sm" onPress={onAdd}>
          添加凭据
        </Button>
      </div>

      <DataTable
        label="上游凭据"
        error={error}
        hasData={creds.length > 0}
        onRetry={onRetry}
        emptyText="无凭据 —— 上游鉴权必需"
      >
        {/* 「同时只启用一把」的提示必须与表格**并存**，不能写成二选一。
             旧版 bug：它占着 v-else 的位置，凭据数 ≥ 2 时渲染的是这句提示、
             表格整个不渲染 —— 两把 key 从界面上消失，而后端数据完好、
             调用照常成功。它是**跨行**的约束，必须写在开关旁边而不是藏进文档。 */}
        {creds.length > 1 ? (
          <p className="text-muted mb-2 text-xs">
            同一上游同时只启用一把 key：打开其中一把会自动停用其它。
          </p>
        ) : null}
        <Table aria-label="上游凭据">
          <Table.ScrollContainer>
            <Table.Content className="min-w-[560px]">
              <Table.Header>
                <Table.Column isRowHeader>凭据</Table.Column>
                <Table.Column>操作</Table.Column>
              </Table.Header>
              <Table.Body>
                {creds.map((c) => {
                  return (
                    <Table.Row key={c.id}>
                      <Table.Cell>
                        <span className="flex items-center gap-2">
                          <span className="font-medium">{c.label || c.id}</span>
                          <StatusChip tone={c.enabled ? 'ok' : 'dim'}>
                            {c.enabled ? '启用' : '停用'}
                          </StatusChip>
                        </span>
                      </Table.Cell>
                      <Table.Cell>
                        <div className="flex items-center gap-1">
                          {/* 启停开关放操作列最前：它是这一行最高频的动作，
                               而「换钥 / 删除」是低频且有后果的 ——
                               位置靠内一层就少一次误点。 */}
                          <Switch
                            isSelected={c.enabled}
                            isDisabled={toggling.includes(c.id)}
                            aria-label={
                              c.enabled
                                ? `停用 ${c.label || c.id}`
                                : `启用 ${c.label || c.id}（同一上游同时只启用一把，启用它会自动停用其它）`
                            }
                            onChange={() => onToggle(c)}
                          >
                            <Switch.Content>
                              <Switch.Control>
                                <Switch.Thumb />
                              </Switch.Control>
                            </Switch.Content>
                          </Switch>
                          <Button variant="outline" size="sm" onPress={() => onEdit(c)}>
                            换钥
                          </Button>
                          <Button variant="danger-soft" size="sm" onPress={() => onRemove(c)}>
                            删除
                          </Button>
                        </div>
                      </Table.Cell>
                    </Table.Row>
                  )
                })}
              </Table.Body>
            </Table.Content>
          </Table.ScrollContainer>
        </Table>
      </DataTable>
    </section>
  )
}
