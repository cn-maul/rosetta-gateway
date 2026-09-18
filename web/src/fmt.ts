// 展示格式化工具

export function fmtNum(n: number): string {
  if (!Number.isFinite(n)) return '0'
  return new Intl.NumberFormat('zh-CN').format(n)
}

export function fmtTokens(n: number): string {
  if (!Number.isFinite(n)) return '0'
  if (n >= 1e9) return (n / 1e9).toFixed(2).replace(/\.?0+$/, '') + 'B'
  if (n >= 1e6) return (n / 1e6).toFixed(2).replace(/\.?0+$/, '') + 'M'
  if (n >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, '') + 'K'
  return String(n)
}

export function fmtDate(unixSec: number): string {
  if (!unixSec) return '—'
  const d = new Date(unixSec * 1000)
  const p = (x: number) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

export function fmtDateTime(unixSec: number): string {
  if (!unixSec) return '—'
  const d = new Date(unixSec * 1000)
  const p = (x: number) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

export function fmtExpiry(unixSec: number): string {
  if (!unixSec) return '永不过期'
  const diff = unixSec * 1000 - Date.now()
  if (diff <= 0) return '已过期'
  const days = Math.floor(diff / 86400000)
  if (days >= 1) return `${days} 天后过期`
  const hours = Math.floor(diff / 3600000)
  return `${Math.max(1, hours)} 小时后过期`
}

/** unix 秒 → <input type="datetime-local"> 值（本地时区） */
export function unixToLocalInput(unixSec: number): string {
  if (!unixSec) return ''
  const d = new Date(unixSec * 1000)
  const p = (x: number) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`
}

/** <input type="datetime-local"> 值 → unix 秒（0 表示未设置） */
export function localInputToUnix(v: string): number {
  if (!v) return 0
  const t = new Date(v).getTime()
  return Number.isFinite(t) ? Math.floor(t / 1000) : 0
}

export async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    // 非安全上下文（http://）下 clipboard API 不可用，退回 execCommand
    try {
      const ta = document.createElement('textarea')
      ta.value = text
      ta.style.position = 'fixed'
      ta.style.opacity = '0'
      document.body.appendChild(ta)
      ta.select()
      const ok = document.execCommand('copy')
      document.body.removeChild(ta)
      return ok
    } catch {
      return false
    }
  }
}
