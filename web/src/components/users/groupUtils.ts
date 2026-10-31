import { useMemo } from 'react'
import type { Group } from '../../types'

/** 分组 id → 名字。每行渲染都要查一次，用 Map 而不是对每行 find 一遍。 */
export function useGroupNames(groups: Group[]) {
  return useMemo(() => {
    const m = new Map<string, string>()
    for (const g of groups) m.set(g.id, g.name)
    return m
  }, [groups])
}

/** 组里「挡删除」的两类绑定。users.group_id 与 access_keys.group_id 都是
 * ON DELETE SET NULL，两类引用各自都能让后端拒绝删除 —— 只统计账号数会
 * 漏掉「组里没人、但有一把密钥指定了分组覆盖」的情形，那时界面显示可删、
 * 点下去却必然 409。 */
export function boundCounts(g: Group): { users: number; keys: number } {
  return { users: g.member_count ?? 0, keys: g.key_count ?? 0 }
}

export function canDeleteGroup(g: Group): boolean {
  const c = boundCounts(g)
  return c.users === 0 && c.keys === 0
}

/** 删除被挡时的 title：必须说清是哪一类绑定挡的、以及怎么解 ——
 * 账号要迁组（本页用户页签就能改），密钥要解除「分组覆盖」（密钥页的字段）。 */
export function deleteBlockedTitle(g: Group): string {
  const c = boundCounts(g)
  if (c.users > 0 && c.keys > 0) {
    return `该组下还有 ${c.users} 个账号和 ${c.keys} 把访问密钥。请先把账号移到别的组，并解除这些密钥的分组覆盖。`
  }
  if (c.users > 0) {
    return `该组下还有 ${c.users} 个账号。请先在「用户」页签把他们移到别的组或移出分组，否则删除会被后端拒绝。`
  }
  if (c.keys > 0) {
    return `该组下还有 ${c.keys} 把访问密钥指定了分组覆盖。请先到「访问密钥」解除这些密钥的分组覆盖。`
  }
  return ''
}
