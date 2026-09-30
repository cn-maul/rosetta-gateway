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
 *
 * 两条下限保护：
 *   - 命中率极低但不为 0 时不显示成「0」，否则「几乎没命中」和「完全没有」无法区分；
 *   - 99.5% 不许进位成「100%」。四舍五入到 100 会把「有失败」说成「全成功」，
 *     而调用方往往同时按 `rate < 1` 打了警告色 —— 于是界面出现「100%」配橙色告警
 *     这种自相矛盾的显示。小于 1 时一律向下取到一位小数。
 */
export function fmtPercent(rate: number): string {
  if (!Number.isFinite(rate) || rate <= 0) return '0'
  if (rate >= 1) return '100'
  const p = rate * 100
  if (p >= 10) {
    const r = Math.round(p)
    if (r >= 100) return (Math.floor(p * 10) / 10).toFixed(1)
    return String(r)
  }
  if (p >= 0.1) return p.toFixed(1)
  return '<0.1'
}

/**
 * 金额（元）→ 展示字符串（不带符号，单位由调用方标注）。
 *
 * 大额（≥1 元）固定两位小数、带千分位；小额必须留住有效数字 ——
 * 一次几十万 token 的请求往往只值几分钱，一刀切成两位小数会显示成「0.00」，
 * 与同一屏「有流量」的指标自相矛盾，所以 <1 元时最多保留 4 位小数。
 */
export function fmtMoney(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0.00'
  const opts: Intl.NumberFormatOptions =
    n >= 1
      ? { minimumFractionDigits: 2, maximumFractionDigits: 2 }
      : { minimumFractionDigits: 2, maximumFractionDigits: 4 }
  return new Intl.NumberFormat('zh-CN', opts).format(n)
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
 * overflow / canceled / error），界面统一中文化，未知值原样显示以便排查。
 */
export function statusLabel(status: string): string {
  switch (status) {
    case 'ok':
      return '正常'
    case 'truncated':
      return '截断'
    case 'overflow':
      return '超限'
    case 'canceled':
      return '已取消'
    case 'error':
      return '失败'
    default:
      return status || '—'
  }
}

/**
 * 状态徽章样式。
 *
 * 三类语义，别再退回「非 ok 即异常」的二元判断：
 *   - ok        → 绿色（正常）
 *   - canceled  → 中性灰。客户端主动断开既不是网关的错，也不是上游的错，
 *                 统计口径里同样不计入错误率；标成暖橙会让「用户关了个页面」
 *                 看起来像线上故障。
 *   - 其余      → 暖橙（截断 / 超限 / 失败，都是需要关注的异常）
 */
export function statusBadge(status: string): string {
  if (status === 'ok') return 'badge-live'
  if (status === 'canceled') return 'badge-off'
  return 'badge-err'
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
