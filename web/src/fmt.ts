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

// 输出速度（token/s）：整数级不带小数，个位数保留一位，过小则显式两位。
export function fmtSpeed(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0'
  if (n >= 100) return String(Math.round(n))
  if (n >= 10) return n.toFixed(1).replace(/\.0$/, '')
  return n.toFixed(2)
}

// 首字延迟：毫秒 → 秒，展示口径与速度一致（大数少位、小数多位）。
export function fmtSec(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '0'
  const s = ms / 1000
  if (s >= 100) return String(Math.round(s))
  if (s >= 10) return s.toFixed(1).replace(/\.0$/, '')
  return s.toFixed(2)
}

/**
 * 比率（0~1）→ 百分比字符串。
 * 保留一位下限保护：命中率极低但不为 0 时不显示成「0」，
 * 否则「几乎没命中」和「完全没有缓存」在界面上无法区分。
 */
export function fmtPercent(rate: number): string {
  if (!Number.isFinite(rate) || rate <= 0) return '0'
  const p = rate * 100
  if (p >= 10) return String(Math.round(p))
  if (p >= 0.1) return p.toFixed(1)
  return '<0.1'
}

// 毫秒时间戳 → 「MM-DD HH:MM:SS」（调用历史表格用）
export function fmtTimeMs(tsMs: number): string {
  if (!tsMs) return '—'
  const d = new Date(tsMs)
  const p = (x: number) => String(x).padStart(2, '0')
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

export function fmtDate(unixSecOrMs: number): string {
  if (!unixSecOrMs) return '—'
  // 判断是秒级还是毫秒级：如果值大于 1e12，则是毫秒级
  const ts = unixSecOrMs > 1e12 ? unixSecOrMs : unixSecOrMs * 1000
  const d = new Date(ts)
  const p = (x: number) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
}

export function fmtDateTime(unixSecOrMs: number): string {
  if (!unixSecOrMs) return '—'
  // 判断是秒级还是毫秒级：如果值大于 1e12，则是毫秒级
  const ts = unixSecOrMs > 1e12 ? unixSecOrMs : unixSecOrMs * 1000
  const d = new Date(ts)
  const p = (x: number) => String(x).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

/**
 * 调用状态的展示标签。库内 status 是机器可读值（ok / truncated /
 * overflow / error），界面统一中文化，未知值原样显示以便排查。
 */
export function statusLabel(status: string): string {
  switch (status) {
    case 'ok':
      return '正常'
    case 'truncated':
      return '截断'
    case 'overflow':
      return '超限'
    case 'error':
      return '失败'
    default:
      return status || '—'
  }
}

/** 非 ok 一律标记为异常（暖橙是设计系统里的专用错误色）。 */
export function statusBadge(status: string): string {
  return status === 'ok' ? 'badge-live' : 'badge-err'
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
