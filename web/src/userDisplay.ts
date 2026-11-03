import type { UsageByUserEntry } from './types'

/**
 * 按用户汇总的两处表格共用：钱包页「消费汇总」与用量页「按用户」。
 *
 * 这两件事（显示名、异常标记）一旦各写一份，删除用户与无归属用户在同一
 * 产品里就会出现两种说法 —— 而它们的区别恰恰是读账时最容易被误读的地方。
 */

/** 一行的展示名：用户已删除（key 非空而 user_name 空）回退显示 user_id；
 * 无归属（key 为空）显示「无归属」。两者数据形状相同但含义不同。 */
export const userRowName = (r: UsageByUserEntry) => (r.user_name ? r.user_name : r.key || '无归属')

export type UserRowTag = { text: string; tone: 'dim' | 'info' }

/** 行标记：已删除 / 管理员历史用量 / 无归属。都显式标注，不靠名字空缺意会。 */
export const userRowTag = (r: UsageByUserEntry): UserRowTag | null => {
  if (!r.key) return { text: '无归属', tone: 'dim' }
  if (!r.user_name) return { text: '已删除', tone: 'dim' }
  if (r.role === 'admin') return { text: '管理员·历史', tone: 'info' }
  return null
}

/** 头像首字：按码点取，中文/emoji 不会把代理对劈成半个字符。 */
export function userInitial(r: UsageByUserEntry): string {
  return Array.from(userRowName(r))[0] ?? '?'
}