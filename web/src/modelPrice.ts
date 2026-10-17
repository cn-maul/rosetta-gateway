type ModelPrices = { price_input: number; price_cache_hit: number; price_output: number }

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

export function modelPriceLine(model: ModelPrices): string {
  const cache = cacheUnitPrice(model.price_input, model.price_cache_hit)
  return `输入 ${unitPrice(model.price_input)} · 缓存 ${cache} · 输出 ${unitPrice(model.price_output)}`
}
