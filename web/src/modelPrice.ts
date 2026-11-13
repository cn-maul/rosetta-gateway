// 单价与结算金额分开格式化，保留小额单价的有效数字。
export function unitPrice(value: number): string {
  return Number.isFinite(value) && value > 0
    ? `¥${value.toLocaleString('zh-CN', { maximumSignificantDigits: 15 })}`
    : '未配置'
}

export function cacheUnitPrice(input: number, cache: number): string {
  return cache > 0 ? unitPrice(cache)
    : input > 0 ? `${unitPrice(input)}（沿用输入）` : '未配置'
}
