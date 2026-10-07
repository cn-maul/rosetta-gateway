// 展示格式化工具

/**
 * 去掉 toFixed 补出来的小数尾 0（"1.50" → "1.5"，"2.00" → "2"）。
 *
 * 只砍小数点**之后**的 0。早先的写法是 `/\.?0+$/`，那个可选的 `\.` 会把整数
 * 部分的 0 一起吃掉："1000.00" 里从第一个 0 起一路匹配到末尾，结果变成 "1"
 * —— 一个整好跨到下一档的数量被显示成上一档的 1 倍，读数是反的。
 */
function trimZero(s: string): string {
  if (!s.includes('.')) return s
  return s.replace(/0+$/, '').replace(/\.$/, '')
}

export function fmtNum(n: number): string {
  if (!Number.isFinite(n)) return '0'
  return new Intl.NumberFormat('zh-CN').format(n)
}

// 数量级档位（从大到小）：K 档一位小数，M / B 档两位（沿用旧口径）。
const TOKEN_TIERS = [
  { base: 1e9, suffix: 'B', digits: 2 },
  { base: 1e6, suffix: 'M', digits: 2 },
  { base: 1e3, suffix: 'K', digits: 1 },
] as const

/**
 * Token 数量 → 紧凑展示（K / M / B）。
 *
 * 档位按**取整后的展示值**判定，而不是按原始值判定。
 *
 * 否则边界会给出两种写法：999,999 落进 K 档 → (999.999).toFixed(1) = "1000.0"
 * → 显示「1000K」，而只多 1 的 1,000,000 显示「1M」。同一数轴上相邻的两段，
 * 一个写成 1000K、一个写成 1M，而且「1000K」看着比「1M」大 —— 阈值附近
 * 的读数是反的。现在只要取整后进位到了 1000，就交给上一档重新格式化
 * （999,999 → 「1M」，999,999,999 → 「1B」）。
 */
export function fmtTokens(n: number): string {
  if (!Number.isFinite(n)) return '0'
  // 额度差值可能为负（剩余额度等），负号提到档位之外，别让它混进数量级判断
  const sign = n < 0 ? '-' : ''
  const v = Math.abs(n)
  let i = TOKEN_TIERS.findIndex((t) => v >= t.base)
  if (i < 0) return sign + String(v)
  for (; i > 0; i--) {
    const t = TOKEN_TIERS[i]
    const scaled = v / t.base
    // 用「格式化后再解析回数字」判断是否进位：与界面上真正显示的值同源
    if (Number(scaled.toFixed(t.digits)) < 1000) return sign + trimZero(scaled.toFixed(t.digits)) + t.suffix
  }
  // 走到 B 档（最大单位）就没有更大的单位可退了，照实显示（1000B 以上也只能是 B）
  const top = TOKEN_TIERS[0]
  return sign + trimZero((v / top.base).toFixed(top.digits)) + top.suffix
}

/**
 * 小数位随量级递减：越大越粗、越小越细（速度 token/s 与秒数共用）。
 *
 * 与 fmtTokens 同一个坑 —— 档位必须由**取整后的值**判定。9.996 原本走
 * 「<10 → 两位小数」档显示「10.00」，而更大的 10 却显示「10」：更小的值
 * 反倒多出两位小数，两档在边界处给出不一致的精度。
 */
function fmtScaled(v: number): string {
  if (v >= 100) return String(Math.round(v))
  if (v >= 10) {
    const s = v.toFixed(1)
    return Number(s) >= 100 ? String(Math.round(Number(s))) : trimZero(s)
  }
  const s = v.toFixed(2)
  // 两位小数档进位到 10 以上时改用一位小数档的写法（9.996 → "10" 而不是 "10.00"）
  return Number(s) >= 10 ? trimZero(Number(s).toFixed(1)) : s
}

// 输出速度（token/s）：整数级不带小数，个位数保留一位，过小则显式两位。
export function fmtSpeed(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0'
  return fmtScaled(n)
}

// 首字延迟：毫秒 → 秒，展示口径与速度一致（大数少位、小数多位）。
export function fmtSec(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '0'
  return fmtScaled(ms / 1000)
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

/**
 * 账户余额（**分**）→ 展示字符串。
 *
 * # 为什么必须有这个函数，而不能各页自己 fmtMoney(cents/100)
 *
 * 余额有两个状态，含义**相反**：
 *   - unlimited = true  → 不限额，永远放行
 *   - 0 分              → 账户里真的一分钱都没有，会被 402 拒绝
 *
 * 两者若都渲染成「0.00 元」，界面上就再也分不出「随便用」与「用不了」——
 * 而它们的处置动作完全相反（一个不用管，一个要去充值）。所以判定一律走
 * `unlimited` 标志，绝不拿数值代替；这个函数就是把这条约定收在一处。
 *
 * `cents` 用整数分而非元：余额是反复累加的账目，浮点元的尾数会让
 * 「还剩多少钱」的显示与后端的整数比较对不上（见 store.User.BalanceCents）。
 *
 * @param cents 余额（分）
 * @param unlimited 是否不限额。为 true 时**cents 被忽略**，直接返回「不限」。
 */
export function fmtBalance(cents: number, unlimited: boolean): string {
  if (unlimited) return '不限'
  return fmtMoney(cents / 100)
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
