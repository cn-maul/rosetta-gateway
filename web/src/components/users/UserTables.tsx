import { Button, Table } from '@heroui/react'
import { DataTable } from '../../ui/DataTable'
import { StatusChip } from '../../ui/StatusChip'
import { fmtTokens, fmtBalance, fmtRemainder } from '../../fmt'
import { canDeleteGroup, deleteBlockedTitle, useGroupNames } from './groupUtils'
import type { Group, User } from '../../types'

export type UserRowPending = Map<string, string>

export interface UsersTableProps {
  users: User[]
  groups: Group[]
  loading: boolean
  err: string
  /** 正在飞行中的行操作：user id → 语义标签。防连点必须落在行上，
   * 不能靠一个全局 busy：点不同行本就该各自进行。 */
  pending: UserRowPending
  onEdit: (u: User) => void
  /** 重置该用户的密码，由父级分派。 */
  onPassword: (u: User) => void
  onToggle: (u: User) => void
  onDelete: (u: User) => void
}

export function UsersTable({ users, groups, loading, err, pending, onEdit, onPassword, onToggle, onDelete }: UsersTableProps) {
  const groupNames = useGroupNames(groups)
  /** 组被删掉、或 id 查不到时显示「未知分组」而不是空 —— 空看起来像
   * 「未分组」，而「未分组 = 不限制模型」，两者含义完全不同。 */
  const groupName = (id: string) => (id ? (groupNames.get(id) ?? '未知分组') : '')
  /** 判据是清单里没有这个 id，不是「名字等于未知分组」—— 真有组叫这个名字
   * 时，按名字判断会把它的成员全误报成分组已不存在。 */
  const isUnknownGroup = (id: string) => !!id && !groupNames.has(id)
  const isPending = (id: string) => pending.has(id)
  const rowLabel = (id: string, fallback: string) => pending.get(id) ?? fallback

  return (
    // empty 必须由数据算：写成常量 true 会让空态分支永远命中，表格本体
    // 一次都不渲染（「用户列表里没人了」就是这么来的）。
    <DataTable label="用户与权限组" loading={loading} error={err} hasData={users.length > 0} empty={users.length === 0} emptyText="还没有用户">
      <Table>
        <Table.ScrollContainer>
          <Table.Content aria-label="用户与权限组">
            <Table.Header>
              <Table.Column isRowHeader>用户名</Table.Column>
              <Table.Column>分组</Table.Column>
              <Table.Column className="text-right">密钥数量</Table.Column>
              <Table.Column className="text-right">总 token 用量</Table.Column>
              <Table.Column className="text-right">余额</Table.Column>
              <Table.Column className="text-right">操作</Table.Column>
            </Table.Header>
            <Table.Body>
              {users.map((u) => (
                <Table.Row key={u.id}>
                  <Table.Cell>
                    <div className="flex flex-wrap items-center gap-1.5">
                      <span className="font-medium font-mono">{u.username}</span>
                      <StatusChip tone={u.role === 'admin' ? 'ok' : 'dim'}>
                        {u.role === 'admin' ? '管理员' : '普通用户'}
                      </StatusChip>
                      {u.is_self ? <StatusChip tone="info">你自己</StatusChip> : null}
                      {u.status !== 'active' ? <StatusChip tone="dim">已禁用</StatusChip> : null}
                      {!u.has_password ? <StatusChip tone="dim">未设密码</StatusChip> : null}
                    </div>
                  </Table.Cell>
                  <Table.Cell>
                    {u.group_id ? groupName(u.group_id) : '未分组'}
                    {isUnknownGroup(u.group_id) ? (
                      <span className="ml-1.5">
                        <StatusChip tone="err">分组已不存在</StatusChip>
                      </span>
                    ) : null}
                  </Table.Cell>
                  <Table.Cell className="text-right tabular-nums">{u.key_count}</Table.Cell>
                  <Table.Cell className="text-right tabular-nums">
                    {fmtTokens(u.used_tokens)}
                    {u.quota_tokens > 0 ? <span className="text-muted"> / {fmtTokens(u.quota_tokens)}</span> : null}
                  </Table.Cell>
                  <Table.Cell className="text-right tabular-nums">
                    {/* 「不限」与「0.00 元」是相反的两种状态，绝不能都渲染成 0。 */}
                    <span className={u.balance_unlimited ? 'text-muted' : ''}>
                      {fmtBalance(u.balance_cents, u.balance_unlimited)}
                    </span>
                    {/* 待结算余数：低单价时一次调用不足一分，余额长时间不动。
                        不看到它管理员会以为扣费坏了。 */}
                    {fmtRemainder(u.balance_remainder ?? 0) ? (
                      <span className="text-muted ml-1 tabular-nums">+{fmtRemainder(u.balance_remainder ?? 0)}</span>
                    ) : null}
                    {!u.balance_unlimited && u.balance_cents === 0 ? (
                      <div>
                        <StatusChip tone="err">已用尽</StatusChip>
                      </div>
                    ) : null}
                  </Table.Cell>
                  <Table.Cell>
                    <div className="flex flex-wrap items-center justify-end gap-1">
                      <Button variant="ghost" size="sm" isDisabled={isPending(u.id)} onPress={() => onEdit(u)}>
                        编辑
                      </Button>
                      <span
                        title={u.status !== 'active' ? '请先启用该用户' : '重置该用户的密码'}
                      >
                        <Button
                          variant="ghost"
                          size="sm"
                          isDisabled={u.status !== 'active' || isPending(u.id)}
                          onPress={() => onPassword(u)}
                        >
                          {rowLabel(u.id, '重置密码')}
                        </Button>
                      </span>
                      <span title={u.is_self ? '不能禁用自己的账号' : '禁用后该用户的所有登录状态立即失效'}>
                        <Button
                          variant="ghost"
                          size="sm"
                          isDisabled={u.is_self || isPending(u.id)}
                          onPress={() => onToggle(u)}
                        >
                          {rowLabel(u.id, u.status === 'active' ? '禁用' : '启用')}
                        </Button>
                      </span>
                      <span title={u.is_self ? '不能删除自己' : undefined}>
                        <Button
                          variant="danger"
                          size="sm"
                          isDisabled={u.is_self || isPending(u.id)}
                          onPress={() => onDelete(u)}
                        >
                          {rowLabel(u.id, '删除')}
                        </Button>
                      </span>
                    </div>
                  </Table.Cell>
                </Table.Row>
              ))}
            </Table.Body>
          </Table.Content>
        </Table.ScrollContainer>
      </Table>
    </DataTable>
  )
}

export interface GroupsTableProps {
  groups: Group[]
  loading: boolean
  err: string
  deleting: boolean
  onModels: (g: Group) => void
  onEditGroup: (g: Group) => void
  onDeleteGroup: (g: Group) => void
}

export function GroupsTable({ groups, loading, err, deleting, onModels, onEditGroup, onDeleteGroup }: GroupsTableProps) {
  return (
    <>
      <div className="text-muted mb-3.5 text-[12.5px] leading-relaxed">
        未分组、或所在组未配白名单 → <strong className="text-foreground">不限制</strong>模型可见范围。
        白名单与账号自己的密钥白名单<b>取交集</b>，密钥那一层只会更紧。
      </div>
      <DataTable
        label="用户与权限组"
        loading={loading}
        error={err}
        hasData={groups.length > 0}
        empty={groups.length === 0}
        emptyText="还没有分组 —— 当前所有密钥都能看到全部模型"
      >
        <Table>
          <Table.ScrollContainer>
            <Table.Content aria-label="用户分组">
              <Table.Header>
                <Table.Column isRowHeader>分组</Table.Column>
                <Table.Column>说明</Table.Column>
                <Table.Column>模型白名单</Table.Column>
                <Table.Column className="text-right">操作</Table.Column>
              </Table.Header>
              <Table.Body>
                {groups.map((g) => (
                  <Table.Row key={g.id}>
                    <Table.Cell>
                      <div className="flex flex-wrap items-center gap-1.5">
                        <span className="font-medium">{g.name}</span>
                        {/* 账号与密钥分开显示：两者都会阻止删除，但处置办法不同
                            （前者迁组、后者解除分组覆盖），合成一个数字就没法
                            告诉管理员该去哪改。 */}
                        <StatusChip tone="dim">{g.member_count} 个账号</StatusChip>
                        <StatusChip tone="dim">{g.key_count} 把密钥</StatusChip>
                        {g.models.length === 0 ? (
                          <StatusChip tone="info">未限制模型</StatusChip>
                        ) : (
                          <StatusChip tone="ok">{g.models.length} 个模型</StatusChip>
                        )}
                      </div>
                    </Table.Cell>
                    <Table.Cell className="text-muted">{g.description || '—'}</Table.Cell>
                    <Table.Cell>
                      <span className="font-mono text-[12.5px]">
                        {g.models.length === 0 ? '（未配置白名单 = 不限制）' : g.models.join('、')}
                      </span>
                    </Table.Cell>
                    <Table.Cell>
                      <div className="flex flex-wrap items-center justify-end gap-1">
                        <Button variant="ghost" size="sm" onPress={() => onModels(g)}>
                          配置模型
                        </Button>
                        <Button variant="ghost" size="sm" onPress={() => onEditGroup(g)}>
                          改名
                        </Button>
                        <span title={deleteBlockedTitle(g) || undefined}>
                          <Button
                            variant="danger"
                            size="sm"
                            isDisabled={!canDeleteGroup(g) || deleting}
                            onPress={() => onDeleteGroup(g)}
                          >
                            删除
                          </Button>
                        </span>
                      </div>
                    </Table.Cell>
                  </Table.Row>
                ))}
              </Table.Body>
            </Table.Content>
          </Table.ScrollContainer>
        </Table>
      </DataTable>
    </>
  )
}
