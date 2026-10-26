import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

function source(path) {
  return readFileSync(new URL(path, import.meta.url), 'utf8')
}

test('R10 ProviderDetail scopes balance to provider generation', () => {
  const s = source('../src/views/ProviderDetail.vue')
  assert.match(s, /const balanceRequests = new RequestGeneration\(\)/)
  assert.match(s, /result\.provider_id !== id/)
  assert.match(s, /credentialBalanceFor/)
  assert.match(s, /balanceRequests\.invalidate\(\)/)
})

test('R10 Models binds timeline results to request generation and selection', () => {
  const s = source('../src/views/Models.vue')
  assert.match(s, /const timelineRequests = new RequestGeneration\(\)/)
  assert.match(s, /selectionKey\(ids, requestedDays, requestedGranularity\)/)
  assert.match(s, /timelineRequests\.isCurrent\(generation\)/)
  assert.match(s, /timelineSelectionKey\.value !== currentKey/)
})

test('R10 Settings loads a direct price tab immediately and gates empty state', () => {
  const s = source('../src/views/Settings.vue')
  assert.match(s, /watch\(\s*tab,[\s\S]*?immediate:\s*true[\s\S]*?\n\)/)
  assert.match(s, /pricesState === 'loading' \|\| pricesState === 'not-loaded'/)
  assert.match(s, /v-else-if="pricesLoaded && totalModelCount === 0"/)
})

test('R10 ModelPicker select-all preserves hidden selections', () => {
  const s = source('../src/components/ModelPicker.vue')
  assert.match(s, /import \{ unionSelection \} from '\.\.\/modelSelection'/)
  assert.match(s, /q \? unionSelection\(props\.modelValue, visible\.value\)/)
})
