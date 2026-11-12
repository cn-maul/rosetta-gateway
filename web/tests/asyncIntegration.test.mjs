import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

// R10（异步所有权）：把「结果必须绑定到发起它的那次请求」写成源码断言。
// React 重写后断言对象换成 .tsx 新源码，语义不变：
//   - ProviderDetail 的余额查询按 provider id + 请求代际双重归属；
//   - ModelPicker 的「全选」在搜索过滤下用 unionSelection 保住隐藏勾选。
function source(path) {
  return readFileSync(new URL(path, import.meta.url), 'utf8')
}

test('R10 ProviderDetail scopes balance to provider generation', () => {
  const s = source('../src/pages/ProviderDetail.tsx')
  assert.match(s, /balanceRequests = useRef\(new RequestGeneration\(\)\)/)
  assert.match(s, /result\.provider_id !== pid/)
  assert.match(s, /api\.providerBalance\(pid\)/)
  assert.match(s, /balanceRequests\.current\.invalidate\(\)/)
})

test('R10 ModelPicker select-all preserves hidden selections', () => {
  const s = source('../src/components/ModelPicker.tsx')
  assert.match(s, /import \{ unionSelection \} from '\.\.\/modelSelection'/)
  assert.match(s, /unionSelection\(value, visible\)/)
})
