// 主题三态（dark / light / system）。
//
// 语义必须与 public/theme-init.js 保持一致：读 localStorage.rosetta_gw_theme，
// 'system'（或未设置）跟随系统，显式 dark/light 固化。theme-init.js 负责
// 首帧防闪（同步脚本），这里负责运行期：切换、系统主题跟随、meta color-scheme 同步。
// 改主题语义时两处要同步改。
import { useEffect, useState } from 'react'

export type ThemePref = 'dark' | 'light' | 'system'

const THEME_KEY = 'rosetta_gw_theme'

export function readStoredTheme(): ThemePref {
  try {
    const saved = localStorage.getItem(THEME_KEY)
    if (saved === 'dark' || saved === 'light') return saved
  } catch {
    /* 隐私模式下可能读不了 localStorage */
  }
  return 'system'
}

function persistTheme(v: ThemePref) {
  try {
    localStorage.setItem(THEME_KEY, v)
  } catch {
    /* 忽略 */
  }
}

function applyTheme(dark: boolean) {
  document.documentElement.setAttribute('data-theme', dark ? 'dark' : 'light')
  // color-scheme 必须与 data-theme 同步：它决定表单控件、滚动条等
  // 浏览器原生 UI 的配色，不同步会出现「深色页面 + 浅色下拉框」。
  const meta = document.querySelector('meta[name=color-scheme]')
  if (meta) meta.setAttribute('content', dark ? 'dark' : 'light')
}

function resolveDark(pref: ThemePref, systemDark: boolean): boolean {
  return pref === 'dark' || (pref === 'system' && systemDark)
}

export function useTheme() {
  const [dark, setDark] = useState(() =>
    resolveDark(readStoredTheme(), window.matchMedia('(prefers-color-scheme: dark)').matches),
  )

  useEffect(() => {
    const mq = window.matchMedia('(prefers-color-scheme: dark)')
    // 首次挂载就把 data-theme / meta 对齐（theme-init.js 只处理了 dark 分支）
    const initial = resolveDark(readStoredTheme(), mq.matches)
    applyTheme(initial)
    setDark(initial)
    // 运行期跟随系统：只有当前处于「跟随系统」时才响应系统主题变化
    const onChange = () => {
      if (readStoredTheme() !== 'system') return
      const d = mq.matches
      applyTheme(d)
      setDark(d)
    }
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [])

  // toggle 是显式选择：点一下就固化，不再跟随系统
  const toggle = () =>
    setDark((prev) => {
      const next = !prev
      applyTheme(next)
      persistTheme(next ? 'dark' : 'light')
      return next
    })

  return { dark, toggle }
}
