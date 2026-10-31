import { ListBox, ListBoxItem, Select, SelectIndicator, SelectPopover, SelectTrigger, SelectValue } from '@heroui/react'
import type { Group, UserRole } from '../../types'

const ROLE_OPTIONS: { id: UserRole; label: string }[] = [
  { id: 'user', label: '普通用户' },
  { id: 'admin', label: '管理员' },
]

/** 角色下拉。value 是受控值；disabledSelf 为真时置灰（不能改自己的角色）。 */
export function RoleSelect({
  value,
  onChange,
  disabledSelf = false,
}: {
  value: UserRole
  onChange: (v: UserRole) => void
  disabledSelf?: boolean
}) {
  const select = (
    <Select
      aria-label="角色"
      selectedKey={value}
      onSelectionChange={(key) => {
        if (key !== null) onChange(String(key) as UserRole)
      }}
    >
      <SelectTrigger>
        <SelectValue />
        <SelectIndicator />
      </SelectTrigger>
      <SelectPopover>
        <ListBox aria-label="角色">
          {ROLE_OPTIONS.map((r) => (
            <ListBoxItem key={r.id} id={r.id}>
              {r.label}
            </ListBoxItem>
          ))}
        </ListBox>
      </SelectPopover>
    </Select>
  )
  if (!disabledSelf) return select
  // SelectTrigger 没有 isDisabled：外包一层禁掉交互并保留可读性。
  return (
    <div aria-disabled className="pointer-events-none opacity-60">
      {select}
    </div>
  )
}

/** 分组下拉（创建/编辑共用）。空串 = 未分组 = 不受模型白名单限制。 */
export function GroupSelect({
  value,
  onChange,
  groups,
}: {
  value: string
  onChange: (v: string) => void
  groups: Group[]
}) {
  return (
    <Select
      aria-label="分组"
      selectedKey={value === '' ? '__none__' : value}
      onSelectionChange={(key) => {
        if (key === null) return
        const v = String(key)
        onChange(v === '__none__' ? '' : v)
      }}
    >
      <SelectTrigger>
        <SelectValue />
        <SelectIndicator />
      </SelectTrigger>
      <SelectPopover>
        <ListBox aria-label="分组">
          <ListBoxItem key="none" id="__none__">
            未分组（模型不受限）
          </ListBoxItem>
          {groups.map((g) => (
            <ListBoxItem key={g.id} id={g.id}>
              {g.name}
              {g.models.length === 0 ? '（未限制模型）' : ''}
            </ListBoxItem>
          ))}
        </ListBox>
      </SelectPopover>
    </Select>
  )
}
