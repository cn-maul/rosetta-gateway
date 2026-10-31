import { useCallback, useState } from 'react'

/**
 * 界面偏好的持久化（localStorage）。
 *
 * 只存「用户上次怎么看的」，不存任何业务数据：
 * 区间、粒度、每页条数这类选择，下次进页面应该还在 —— 每次回来都要
 * 重新点一遍「最近 7 天」是很消耗耐心的动作。
 *
 * 三条纪律：
 * 1. 读写一律 try/catch。隐私模式、配额满、被人手动改坏，都不该让页面崩。
 * 2. 读出来的值必须过校验（parse 返回 null 就回落默认值）。旧版本写进去的
 *    枚举值可能已经不存在了，宁可回落也不能把脏值塞给组件。
 * 3. URL 永远优先于偏好。偏好只是「没带参数时的默认值」，可分享的链接不受它影响。
 */

const PREFIX = 'rosetta.pref.'

/** 把任意读到的值收窄成合法枚举，非法一律 null（交给调用方回落默认值）。 */
export function oneOf<T>(options: readonly T[]) {
  return (raw: unknown): T | null => (options.includes(raw as T) ? (raw as T) : null)
}

/** 整数且在指定范围内才接受。 */
export function oneOfInt(options: readonly number[]) {
  return (raw: unknown): number | null =>
    typeof raw === 'number' && Number.isInteger(raw) && options.includes(raw) ? raw : null
}

/** 布尔值专用：'true' 之类的字符串不算数，避免旧版本写入的脏值被当成 true。 */
export function asBool(raw: unknown): boolean | null {
  return typeof raw === 'boolean' ? raw : null
}

/** 读偏好。没有、读不出、校验不过，都是返回 fallback。 */
export function readPref<T>(key: string, fallback: T, parse: (raw: unknown) => T | null): T {
  if (typeof window === 'undefined') return fallback
  try {
    const raw = window.localStorage.getItem(PREFIX + key)
    if (raw === null) return fallback
    const value = parse(JSON.parse(raw) as unknown)
    return value === null ? fallback : value
  } catch {
    return fallback
  }
}

/** 写偏好。失败静默 —— 记不住偏好是可接受的降级，弹错误提示不是。 */
export function writePref(key: string, value: unknown): void {
  if (typeof window === 'undefined') return
  try {
    window.localStorage.setItem(PREFIX + key, JSON.stringify(value))
  } catch {
    /* 隐私模式 / 配额满：偏好记不住就算了 */
  }
}

/** 与 useState 同形，只是初始值来自 localStorage、每次变更落回 localStorage。 */
export function usePersistedState<T>(
  key: string,
  fallback: T,
  parse: (raw: unknown) => T | null,
): [T, (next: T) => void] {
  const [value, setValue] = useState<T>(() => readPref(key, fallback, parse))
  const update = useCallback(
    (next: T) => {
      setValue(next)
      writePref(key, next)
    },
    [key],
  )
  return [value, update]
}
