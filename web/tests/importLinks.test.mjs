import test from 'node:test'
import assert from 'node:assert/strict'
import { buildCcSwitchImportLink, buildMagpieImportLink } from '../src/importLinks.ts'

test('CC Switch Codex import uses the gateway Responses endpoint and exact key', () => {
  const href = buildCcSwitchImportLink({
    origin: 'https://gw.example.com/',
    apiKey: 'sk-gw-a+b/c',
    name: 'Team Key',
    app: 'codex',
    model: 'gpt-test',
  })
  const url = new URL(href)
  assert.equal(url.protocol, 'ccswitch:')
  assert.equal(url.host, 'v1')
  assert.equal(url.pathname, '/import')
  assert.equal(url.searchParams.get('resource'), 'provider')
  assert.equal(url.searchParams.get('app'), 'codex')
  assert.equal(url.searchParams.get('name'), 'Rosetta Gateway - Team Key')
  assert.equal(url.searchParams.get('endpoint'), 'https://gw.example.com/v1')
  assert.equal(url.searchParams.get('apiKey'), 'sk-gw-a+b/c')
  assert.equal(url.searchParams.get('apiFormat'), 'openai_responses')
  assert.equal(url.searchParams.get('model'), 'gpt-test')
})

test('CC Switch Claude Code import uses the gateway Anthropic endpoint', () => {
  const href = buildCcSwitchImportLink({
    origin: 'http://192.168.1.10:8666',
    apiKey: 'sk-gw-claude',
    name: 'LAN Key',
    app: 'claude',
  })
  const url = new URL(href)
  assert.equal(url.searchParams.get('app'), 'claude')
  assert.equal(url.searchParams.get('endpoint'), 'http://192.168.1.10:8666')
  assert.equal(url.searchParams.get('apiFormat'), 'anthropic')
})

test('CC Switch direct-format clients use Chat without local routing', () => {
  for (const app of ['opencode', 'hermes']) {
    const href = buildCcSwitchImportLink({
      origin: 'http://192.168.1.10:8666',
      apiKey: 'sk-gw-chat',
      name: app,
      app,
    })
    const url = new URL(href)
    assert.equal(url.searchParams.get('endpoint'), 'http://192.168.1.10:8666/v1')
    assert.equal(url.searchParams.get('apiFormat'), 'openai_chat')
  }
})

test('CC Switch import omits an empty model', () => {
  const href = buildCcSwitchImportLink({
    origin: 'http://127.0.0.1:8666',
    apiKey: 'sk-gw-test',
    name: 'Local',
    app: 'opencode',
    model: ' ',
  })
  assert.equal(new URL(href).searchParams.has('model'), false)
})

test('Magpie import uses a fragment and advertises all three gateway protocols', () => {
  const href = buildMagpieImportLink({
    origin: 'https://gw.example.com',
    apiKey: 'sk-gw-secret',
    name: 'Team Key',
    models: ['model-b', 'model-a'],
  })
  const url = new URL(href)
  assert.equal(url.origin, 'https://usemagpie.ai')
  assert.equal(url.pathname, '/import')
  assert.equal(url.search, '')

  const params = new URLSearchParams(url.hash.slice(1))
  assert.equal(params.get('name'), 'Rosetta Gateway - Team Key')
  assert.equal(params.get('chat'), 'https://gw.example.com/v1')
  assert.equal(params.get('responses'), 'https://gw.example.com/v1')
  assert.equal(params.get('anthropic'), 'https://gw.example.com')
  assert.equal(params.get('key'), 'sk-gw-secret')
  assert.equal(params.get('models'), 'model-b,model-a')
})

test('Magpie import omits models so Magpie can query /v1/models', () => {
  const href = buildMagpieImportLink({
    origin: 'https://gw.example.com',
    apiKey: 'sk-gw-secret',
    name: 'Team Key',
  })
  assert.equal(new URLSearchParams(new URL(href).hash.slice(1)).has('models'), false)
})
