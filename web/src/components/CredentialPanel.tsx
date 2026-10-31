import { DataTable } from '../ui/DataTable'
import { Table } from '@heroui/react'
import { Button } from '@heroui/react'
import { StatusChip } from '../ui/StatusChip'
import { Switch } from '@heroui/react'
import type { Credential, ProviderBalance, CredentialBalance } from '../types'

/**
 * 凭据面板：某个上游下的多把 API 凭据（详情页概览旁的一列）。
 *
 * 与旧 CredentialPanel.vue 相同的取舍：刻意**只搬展示**不加抽象 ——
 * 数据源与操作回调都从父组件传入（props + 回调），父组件仍是唯一的
 * 写路径持有者。抽组件只减少单文件长度，不引入第二处状态真相。
 */

interface CredentialPanelProps {
  providerId: string
  creds: Credential[]
  /** 该上游的余额查询结果；未点过「查余额」时为 undefined。 */
  balance?: ProviderBalance
  balanceBusy: boolean
  /** 正在提交启停的凭据 id：只给那一行的开关上防连点。 */
  toggling: string[]
  /** 该上游是否配了自定义余额查询（有 spec 走自定义，没有则走内置判定）。 */
  balanceSpecConfigured: boolean
  error?: string
  onRetry?: () => void
  onAdd: () => void
  onQueryBalance: () => void
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

// balanceLine 把一条凭据的余额压成一行。
//
// 分三个状态措辞，因为三者的后续动作互不相干：
//   - unsupported：这个上游压根没有余额查询方式 → 去它自己的后台看；
//   - error：查了但失败 → 401 换 key、超时重试；
//   - ok：拿到数 → 数字 + 时刻 + 上游附注。
// 混成一句「查询失败」会让管理员在三种完全不同的排查方向之间瞎猜。
function balanceLine(r: CredentialBalance): string {
  if (r.status === 'unsupported') return r.message || '该上游不支持余额查询'
  if (r.status === 'unknown') {
    const at = r.fetched_at ? new Date(r.fetched_at).toLocaleTimeString() : ''
    return [r.message || '余额不足以下结论', r.detail, at].filter(Boolean).join(' · ')
  }
  if (r.status === 'error') return r.message || '查询失败'
  const at = r.fetched_at ? new Date(r.fetched_at).toLocaleTimeString() : ''
  const head = `${(r.amount ?? 0).toFixed(2)} ${r.currency ?? ''}`.trim()
  return [head, at, r.detail].filter(Boolean).join(' · ')
}

export function CredentialPanel({
  providerId,
  creds,
  balance,
  balanceBusy,
  toggling,
  balanceSpecConfigured,
  error,
  onRetry,
  onAdd,
  onQueryBalance,
  onEdit,
  onRemove,
  onToggle,
}: CredentialPanelProps) {
  /** 只接受属于当前上游的响应；跨 provider 的旧响应一律视为没有结果。 */
  const currentBalance =
    balance && balance.provider_id === providerId ? balance : undefined

  function balanceFor(credentialId: string): CredentialBalance | undefined {
    return currentBalance?.results.find((x) => x.credential_id === credentialId)
  }

  // balanceModeFor 说明这次查询走的是哪条路径。
  //
  // 为什么要显示：自动判定只覆盖 OpenAI 官方与 DeepSeek 两家，其余上游
  // 一律「不支持」。当管理员看到「不支持」时，他需要知道的是「这是内置判定
  // 的结论」还是「我配了但没匹配上」—— 两者的下一步完全不同
  // （前者去填 balance_spec_json，后者去改配置）。
  function balanceModeFor(): string {
    if (!currentBalance) return ''
    if (currentBalance.status !== 'unsupported') return ''
    return balanceSpecConfigured
      ? '按自定义配置查询，但上游没有返回可用数据 —— 检查 path / field 是否与上游文档一致'
      : '未配置余额查询方式，且内置判定不认识这个上游（内置支持 OpenAI 官方与 DeepSeek）'
  }

  const mode = balanceModeFor()

  return (
    <section className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">凭据（{creds.length}）</h2>
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm" onPress={onAdd}>
            添加凭据
          </Button>
          <Button
            variant="outline"
            size="sm"
            isDisabled={balanceBusy}
            aria-label="实时向该上游查询每把凭据的余额（不自动刷新）"
            onPress={onQueryBalance}
          >
            {balanceBusy ? '查询中…' : '查余额'}
          </Button>
        </div>
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
        {mode ? <p className="text-muted mb-2 text-xs">{mode}</p> : null}
        <Table aria-label="上游凭据">
          <Table.ScrollContainer>
            <Table.Content className="min-w-[560px]">
              <Table.Header>
                <Table.Column isRowHeader>凭据</Table.Column>
                <Table.Column>上游余额</Table.Column>
                <Table.Column>操作</Table.Column>
              </Table.Header>
              <Table.Body>
                {creds.map((c) => {
                  const r = currentBalance ? balanceFor(c.id) : undefined
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
                      {/* 余额显示在凭据行内而不是另起一表：余额挂在 key 上，
                           放远了就失去「哪一把快没钱了」这个唯一有用的读法。
                           没点过「查余额」时留白 —— 它不是 0，
                           两者必须可区分：一个是「没查」，一个是「确实没钱」。 */}
                      <Table.Cell>
                        {currentBalance ? (
                          r ? (
                            <span
                              className={
                                r.status === 'ok'
                                  ? 'text-success text-xs'
                                  : r.status === 'error'
                                    ? 'text-danger text-xs'
                                    : 'text-warning text-xs'
                              }
                            >
                              {balanceLine(r)}
                            </span>
                          ) : (
                            <span className="text-muted text-xs">—（无结果）</span>
                          )
                        ) : (
                          <span className="text-muted text-xs">—</span>
                        )}
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
                            <Switch.Control>
                              <Switch.Thumb />
                            </Switch.Control>
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
