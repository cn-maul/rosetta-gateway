/**
 * 密码强度校验（前端侧的唯一实现）。
 *
 * # 为什么单独成一个模块
 *
 * 强度门槛在**后端**（internal/userauth/password.go 的 ValidatePassword），
 * 前端这份只是提前告知，永远不会替代它 —— 绕过前端直接打接口是常态。
 * 但「提前告知」只有在**两处门槛一致**时才有意义：漂移的后果是用户在这里
 * 看着「强度符合要求」却被后端拒绝，且得不到任何解释。
 *
 * 曾经这份逻辑在 Profile.vue 与 Login.vue 里各抄一遍，Users.vue 的重置
 * 密码则只查了长度。三份实现必然漂移，所以收敛到这里：改门槛只改一处。
 *
 * # 分类必须与 Go 对齐（不是 ASCII 近似）
 *
 * 后端按**码点**逐个分类，顺序是：
 *
 *   unicode.IsLower → unicode.IsUpper → unicode.IsDigit → default(其他)
 *
 * 早期的前端实现用 `r >= 'a' && r <= 'z'` 这类 ASCII 范围判断，于是：
 *   - 「é」「中」在 Go 里前者算 lower、后者算 other，前端却一律算 other；
 *   - 「２」（全角数字）在 Go 里算 digit，前端算 other。
 * 结果是「éaéaéaéa」这类口令在前端只算一类（被判弱）、后端却算两类（放行），
 * 或者反过来 —— 前端放行、后端回 400。
 *
 * 所以这里用 Unicode property escapes 按**单个码点**分类，与 Go 的三个
 * 判定同义：\p{Ll} = Ll（IsLower）、\p{Lu} = Lu（IsUpper）、\p{Nd} = Nd（IsDigit），
 * 其余落进 other。
 */

/** 小写字母：Go unicode.IsLower（Unicode 类别 Ll）。 */
const RE_LOWER = /\p{Ll}/u
/** 大写字母：Go unicode.IsUpper（Unicode 类别 Lu）。 */
const RE_UPPER = /\p{Lu}/u
/** 十进制数字：Go unicode.IsDigit（Unicode 类别 Nd）。 */
const RE_DIGIT = /\p{Nd}/u

/**
 * 检查新密码是否达到后端的最低强度。
 *
 * @returns 空串 = 通过；否则是一句可以直接显示给用户的原因。
 *
 * 空密码返回空串是刻意的：它是「尚未设置密码」的合法状态，是否必填由
 * 调用点决定（创建普通用户可留空、重置密码则必填），不是强度问题。
 */
export function checkPasswordStrength(p: string): string {
  if (!p) return ''
  // 按**码点**计数（与 Go 的 []rune 同义）：代理对算一个字符，
  // 用 .length 会把一个 emoji 数成 2。
  const runes = [...p]
  if (runes.length < 8) return '至少 8 个字符'
  let lower = false
  let upper = false
  let digit = false
  let other = false
  for (const r of runes) {
    // 分支顺序与 Go 的 switch 一致：满足前一类就不归到后面，
    // 顺序不同会让边界字符（如某些既非大小写也非数字的字符）分类不同。
    if (RE_LOWER.test(r)) lower = true
    else if (RE_UPPER.test(r)) upper = true
    else if (RE_DIGIT.test(r)) digit = true
    else other = true
  }
  const classes = [lower, upper, digit, other].filter(Boolean).length
  if (classes < 2) return '需包含字母/数字/符号中的至少两类'
  return ''
}

/** 便捷判定：密码是否通过后端同款强度门槛。 */
export function isStrongPassword(p: string): boolean {
  return checkPasswordStrength(p) === ''
}
