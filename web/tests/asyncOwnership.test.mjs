import test from 'node:test'
import assert from 'node:assert/strict'
import { RequestGeneration, selectionKey } from '../src/asyncState.ts'
import { unionSelection } from '../src/modelSelection.ts'

test('R10 only the latest generation may publish state', async () => {
  const requests = new RequestGeneration()
  const state = { providerId: 'A', balance: '' }

  const first = requests.begin()
  const second = requests.begin()

  await Promise.resolve().then(() => {
    if (requests.isCurrent(first)) state.balance = 'balance-A'
  })
  await Promise.resolve().then(() => {
    if (requests.isCurrent(second)) state.balance = 'balance-B'
  })

  assert.equal(state.balance, 'balance-B')
})

test('R10 invalidating on entity navigation rejects an in-flight response', async () => {
  const requests = new RequestGeneration()
  const generation = requests.begin()

  requests.invalidate()
  await Promise.resolve().then(() => {
    assert.equal(requests.isCurrent(generation), false)
  })
})

test('R10 timeline responses bind to selection and axis', () => {
  assert.equal(selectionKey(['m2', 'm1'], 7, 'hour'), selectionKey(['m1', 'm2'], 7, 'hour'))
  assert.notEqual(selectionKey(['m1'], 7, 'hour'), selectionKey(['m1'], 30, 'hour'))
  assert.notEqual(selectionKey(['m1'], 7, 'hour'), selectionKey(['m1'], 7, 'day'))
  assert.notEqual(selectionKey(['m1'], 7, 'hour'), selectionKey(['m1', 'm2'], 7, 'hour'))
})

test('R10 select-all filtered is a union and keeps hidden selections', () => {
  assert.deepEqual(unionSelection(['a', 'b'], ['c', 'a']), ['a', 'b', 'c'])
  assert.deepEqual(unionSelection([], ['c']), ['c'])
  assert.deepEqual(unionSelection(['a'], []), ['a'])
})
