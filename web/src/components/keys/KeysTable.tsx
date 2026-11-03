import { Copy } from 'lucide-react'
import { Button, Switch, Table } from '@heroui/react'
import { fmtDateTime, fmtTokens } from '../../fmt'
import { DataTable } from '../../ui/DataTable'
import { StatusChip } from '../../ui/StatusChip'
import { ccSwitchIcon, magpieIcon } from './importTargets'
import { anyLimit, daysLeft, isExpired, rateText } from './keyUtils'
import type { AccessKey } from '../../types'

/** 密钥清单表。行内动作全部上抛，本组件不持有任何业务状态。 */
export function KeysTable({
  visibleKeys,
  loading,
  err,
  filteredEmpty,
  toolbar,
  emptyAction,
  toggling,
  copyingId,
  importBusy,
  onCopy,
  onCcPicker,
  onMagpieImport,
  onEdit,
  onDelete,
  onToggle,
}: {
  visibleKeys: AccessKey[]
  loading: boolean
  err: string
  /** 筛选条件非空但筛不出任何行：空态给「清除筛选」而不是「去创建」。 */
  filteredEmpty: boolean
  toolbar?: React.ReactNode
  emptyAction?: React.ReactNode
  toggling: Set<string>
  copyingId: string
  /** 这一行是否有导入动作在途。 */
  importBusy: (k: AccessKey) => boolean
  onCopy: (k: AccessKey) => void
  onCcPicker: (k: AccessKey) => void
  onMagpieImport: (k: AccessKey) => void
  onEdit: (k: AccessKey) => void
  onDelete: (k: AccessKey) => void
  onToggle: (k: AccessKey) => void
}) {
  return (
    <DataTable
      label="访问密钥"
      loading={loading}
      error={err}
      hasData={visibleKeys.length > 0}
      empty={filteredEmpty}
      emptyText={filteredEmpty ? undefined : '还没有访问密钥'}
      emptyAction={emptyAction}
      skeletonRows={6}
      skeletonCols={6}
      toolbar={toolbar}
    >
      <Table>
        <Table.ScrollContainer>
          <Table.Content aria-label="调用密钥">
            <Table.Header>
              <Table.Column isRowHeader>名称</Table.Column>
              <Table.Column>归属用户</Table.Column>
              <Table.Column>限制</Table.Column>
              {/* 表头标出方向：这一列按 used_tokens 降序，箭头就是排的方向。 */}
              <Table.Column className="text-right">用量 ↓</Table.Column>
              <Table.Column>有效期</Table.Column>
              <Table.Column className="text-right">操作</Table.Column>
            </Table.Header>
            <Table.Body>
              {visibleKeys.map((k) => (
                <Table.Row key={k.id}>
                  <Table.Cell>
                    <div className="flex items-center gap-2">
                      <span className="font-medium">{k.name}</span>
                      <StatusChip tone={k.enabled ? 'ok' : 'dim'}>{k.enabled ? '启用' : '停用'}</StatusChip>
                    </div>
                    <div className="text-muted font-mono text-[11.5px]">{k.key_prefix}</div>
                  </Table.Cell>
                  <Table.Cell>
                    {/* 无归属必须显式标注：这类 key 会被迁移退役、鉴权直接 401，
                        不标注的话运维会以为它还能用。 */}
                    {!k.user_id ? (
                      <StatusChip
                        tone="dim"
                        title="迁移遗留的无归属密钥已被停用，调用时鉴权直接返回 401，建议删除"
                      >
                        已停用
                      </StatusChip>
                    ) : (
                      <span className="font-mono text-[12.5px]">{k.username || k.user_id}</span>
                    )}
                  </Table.Cell>
                  <Table.Cell>
                    <div className="flex flex-wrap items-center gap-1.5">
                      {/* 白名单非空才标：空 = 不限制，标出来反而让人以为受限。 */}
                      {k.allowed_models.length > 0 ? (
                        <StatusChip tone="ok">限 {k.allowed_models.length} 个模型</StatusChip>
                      ) : null}
                      {k.allowed_ips ? (
                        <StatusChip tone="info" title={`只允许 ${k.allowed_ips} 调用`}>
                          限源
                        </StatusChip>
                      ) : null}
                      {k.group_id ? (
                        <StatusChip tone="info" title="该密钥使用指定分组的模型范围">
                          分组覆盖
                        </StatusChip>
                      ) : null}
                      {rateText(k) ? <span className="text-muted text-xs">{rateText(k)}</span> : null}
                      {!anyLimit(k) ? <span className="text-muted text-xs">不限</span> : null}
                    </div>
                  </Table.Cell>
                  <Table.Cell className="text-right tabular-nums">
                    {fmtTokens(k.used_tokens)}
                    <span className="text-muted"> / {k.quota_tokens > 0 ? fmtTokens(k.quota_tokens) : '不限'}</span>
                    {k.quota_tokens > 0 && k.used_tokens >= k.quota_tokens ? (
                      <div>
                        <StatusChip tone="err">配额已用尽</StatusChip>
                      </div>
                    ) : null}
                  </Table.Cell>
                  <Table.Cell>
                    {k.expires_at > 0 ? (
                      <div className="flex items-center gap-1.5">
                        {/* 已过期：最需要被一眼看到的状态。它仍然 enabled，
                            但数据面一律 403，必须与「停用」区分开。 */}
                        {isExpired(k) ? <StatusChip tone="err">已过期</StatusChip> : null}
                        <span>
                          到期 {fmtDateTime(k.expires_at)}
                          {!isExpired(k) ? `（剩 ${daysLeft(k.expires_at)} 天）` : ''}
                        </span>
                      </div>
                    ) : (
                      <span className="text-muted text-xs">永不过期</span>
                    )}
                    <div className="text-muted text-[11.5px]">建于 {fmtDateTime(k.created_at)}</div>
                  </Table.Cell>
                  <Table.Cell>
                    <div className="flex flex-wrap items-center justify-end gap-1">
                      {/* 三个操作固定显示在每行末尾。老密钥只有哈希、没有可回读
                          的密文，按钮保留但禁用，避免整列操作随密钥创建时间跳变。 */}
                      <span title={k.can_copy ? '把完整密钥复制到剪贴板' : '这把密钥创建于加密副本功能上线前，明文已不可恢复；请新建密钥'}>
                        <Button
                          isIconOnly
                          variant="ghost"
                          size="sm"
                          isDisabled={!k.can_copy || copyingId === k.id}
                          aria-label="复制 Key"
                          onPress={() => onCopy(k)}
                        >
                          <Copy size={16} strokeWidth={1.75} aria-hidden />
                        </Button>
                      </span>
                      <span title={k.can_copy ? '选择要导入 CC Switch 的客户端' : '这把密钥的明文已不可恢复，无法导入；请新建密钥'}>
                        <Button
                          isIconOnly
                          variant="ghost"
                          size="sm"
                          isDisabled={!k.can_copy || importBusy(k)}
                          aria-label="导入 CC Switch"
                          onPress={() => onCcPicker(k)}
                        >
                          <img src={ccSwitchIcon} alt="" width={16} height={16} className="object-contain" />
                        </Button>
                      </span>
                      <span title={k.can_copy ? '把网关地址和这把密钥添加到本机 Magpie' : '这把密钥的明文已不可恢复，无法导入；请新建密钥'}>
                        <Button
                          isIconOnly
                          variant="ghost"
                          size="sm"
                          isDisabled={!k.can_copy || importBusy(k)}
                          aria-label="导入 Magpie"
                          onPress={() => onMagpieImport(k)}
                        >
                          <img src={magpieIcon} alt="" width={17} height={17} className="icon-invert-dark object-contain" />
                        </Button>
                      </span>
                      <Button variant="ghost" size="sm" onPress={() => onEdit(k)}>
                        编辑
                      </Button>
                      <Button variant="danger" size="sm" onPress={() => onDelete(k)}>
                        删除
                      </Button>
                      <Switch
                        isSelected={k.enabled}
                        isDisabled={toggling.has(k.id)}
                        aria-label={(k.enabled ? '停用 ' : '启用 ') + (k.name || k.key_prefix)}
                        onChange={() => onToggle(k)}
                      >
                        <Switch.Content>
                          <Switch.Control>
                            <Switch.Thumb />
                          </Switch.Control>
                        </Switch.Content>
                      </Switch>
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
