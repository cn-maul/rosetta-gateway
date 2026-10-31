import { fmtTokens } from '../../fmt'
import type { AccessKey } from '../../types'

const DAY_MS = 86_400_000

/**
 * 剩余天数 ⇄ 绝对时间戳。
 *
 * 界面用「天数」而不是 datetime-local：后者的值是无时区的本地时间，
 * 存成时间戳要经过一次 implicitly-local 的转换，夏令时/时区偏移下会出现
 * 「设了 30 天，实际少了 3 小时」这类难解释的偏差。天数没有这个问题，
 * 而且对「这张券还有多久」也更直观。
 */
export function daysLeft(expiresAt: number): number {
  if (!expiresAt) return 0
  return Math.max(0, Math.ceil((expiresAt - Date.now()) / DAY_MS))
}

export function expiresAtFromDays(days: number): number {
  const n = Number(days) || 0
  // 输入框 min=0 只拦得住点选，拦不住手输 -5。负数会算成一个过去的时间戳
  // → 密钥一建就过期；已有期限时还会被服务端判成「放宽限制」而 403，
  // 报错信息与真实原因（输了个负数）完全无关。钳到 0（永不过期）。
  if (n <= 0) return 0
  return Date.now() + n * DAY_MS
}

/** expires_at = 0 是「永不过期」，必须与「刚好过期」区分开：
 * 数据面对前者放行、对后者一律 403，两者混为一谈会把好端端的密钥当成死的。 */
export function isExpired(k: AccessKey): boolean {
  return k.expires_at > 0 && k.expires_at <= Date.now()
}

/** 限速拼成一行。两个上限都是 0 时返回空串，由界面自己决定显示「不限」
 * —— 空串和「不限」在语义上不是一回事，不能让一个函数替界面做这个决定。 */
export function rateText(k: AccessKey): string {
  const parts: string[] = []
  if ((k.rpm_limit ?? 0) > 0) parts.push(`${k.rpm_limit} req/min`)
  if ((k.tpm_limit ?? 0) > 0) parts.push(`${fmtTokens(k.tpm_limit)} tok/min`)
  return parts.join(' · ')
}

/** 这把 key 是否设了任何一种限制。四种限制全空时必须明说「不限」，
 * 否则「什么都没配」会被读成「没配成功」。 */
export function anyLimit(k: AccessKey): boolean {
  return (
    k.allowed_models.length > 0 ||
    !!k.allowed_ips ||
    !!k.group_id ||
    (k.rpm_limit ?? 0) > 0 ||
    (k.tpm_limit ?? 0) > 0
  )
}

/**
 * 无归属 key 的筛选哨兵值。空串是「不过滤」的约定，复用它会让「筛无归属」
 * 和「筛全部」塌成同一个状态；带 \u0000 前缀保证不与任何真实 user_id 相撞。
 */
export const ORPHAN_FILTER = '\u0000orphan'
export const ALL_USERS_FILTER = '__all__'
