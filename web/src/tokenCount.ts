// 上下文窗口 / 最大输出这类「以 token 计的数量」的输入解析。
//
// # 为什么需要单独一层
//
// 这两个数在业界习惯里几乎从不写成完整数字：模型卡上写「128K 上下文」，
// 文档里写「1M」，报错信息里写「131072」。但 `<input type="number">` 只
// 接受纯数字 —— 管理员照着厂商页面敲 128K，浏览器会把这个字符直接丢掉，
// 输入框看起来像被清空，保存下去就是 0（= 未设置，回落到默认值）。这不是
// 「少个便利功能」，而是**静默写错数据**。
//
// 解决办法是把输入框退回文本类型，在提交时用这里解析。解析口径固定为
// 1024 进制：131072 与 128K 等价，1048576 与 1M 等价 —— 这与厂商实际发布
// 的上下文窗口数值一致（128 * 1024 = 131072）。
//
// # 边界
//
// 只认「十进制数字 + 可选 K/M 后缀」，不认「万」「亿」：后者是中文数量级
// 而不是 token 计量的习惯写法，猜错一个数量级（万 = 10000 还是 1024 的倍数）
// 比拒绝解析更危险。小数允许（1.5M = 1572864），四舍五入到整数。

const SUFFIX_SCALE: Record<string, number> = {
  k: 1024,
  m: 1024 * 1024,
}

/**
 * 解析 token 数量输入。
 *
 * 接受：`131072`、`128K`、`128k`、`1M`、`1.5M`、`1 024 000`（空格/逗号/下划线分隔）。
 * 返回 `null` 表示无法解析（空串、负号、非数字、未知后缀）。
 *
 * 刻意不把「无法解析」当成 0：0 在这两个字段上有确定含义（未设置、回落默认
 * 值），而解析失败是输入错误，两者混在一起会让打错的字默默变成默认值。
 */
export function parseTokenCount(raw: string): number | null {
  const cleaned = raw.replace(/[\s,_]/g, '').trim()
  if (!cleaned) return null
  const m = /^(\d+(?:\.\d+)?)([kKmM]?)$/.exec(cleaned)
  if (!m) return null
  const value = Number(m[1])
  if (!Number.isFinite(value)) return null
  const scale = m[2] ? SUFFIX_SCALE[m[2].toLowerCase()] : 1
  const tokens = Math.round(value * scale)
  if (!Number.isSafeInteger(tokens) || tokens < 0) return null
  return tokens
}

/**
 * 把已解析的数量写成人类读法，用于输入框下方的即时反馈。
 *
 * 只在「用户确实写了后缀」时才需要，纯数字再回显一遍是噪音 —— 但调用方
 * 拿到的永远是同一个字符串，判断放在这里，免得每个调用点各写一遍。
 */
export function tokenCountHint(raw: string): string {
  const parsed = parseTokenCount(raw)
  if (parsed === null || parsed === 0) return ''
  // 纯数字不需要回显：它本身已经是要存的值，再显示一遍是噪音。
  // 分组写法（131 072 / 131,072）要回显 —— 用户写成分组就是想要可读性。
  if (/^\d+$/.test(raw.trim())) return ''
  return '= ' + parsed.toLocaleString('en-US') + ' tokens'
}
