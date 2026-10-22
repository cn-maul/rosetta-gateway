import test from 'node:test'
import assert from 'node:assert/strict'
import { API_ROUTES } from '../src/generated/routes.ts'

// # 兼容性注意（未本地验证）
//
// 本机 Node 26；CI 的 web job 固定在 22.12（见 ci.yml 的 node-version）。
// `--experimental-strip-types`、`--import`、`module.register` 的 loader 协议
// 在 22.12 都已存在，但**组合行为未在 22.12 上实测**。若 CI 上报
// ERR_INVALID_RETURN_PROPERTY_VALUE 或 loader 相关错误，先看
// tests/tsResolveHooks.mjs 的 shortCircuit 字段 —— 那是版本间差异最大的一处。

// 前端 API 契约测试（2026-10-21 补 A11-P2-3）。
//
// # 为什么需要它
//
// 仓库里原来只有一个纯函数测试（routeTrace / modelPrice）。而
// `api.ts` 的 `request()` 是整个管理面的出口：它负责拼 `/admin/api`
// 前缀、带 Authorization 头、401 时清令牌、把错误信封翻成 ApiFail、
// 给每个请求装超时信号。这些行为**一个都不在 TypeScript 的类型里** ——
// vue-tsc 全绿也照样能把这些做错，而做错的表现全是运行时的：
//   - 前缀漏一段 → 每个请求 404，界面一片「请求失败」；
//   - 401 不清令牌 → 界面停在永远 401 的空壳上；
//   - 毫秒/秒搞反 → 用量页查到空数据，而参数明明「看起来合法」。
//
// 这个文件盯的是「前端怎么发请求」这一侧的契约；后端字段契约由
// internal/server 的真实 mux 契约测试盯。两侧合起来才闭合。
//
// # 怎么在没有浏览器的情况下测
//
// `api.ts` 依赖三个全局：`fetch`、`sessionStorage`、`AbortSignal.timeout`。
// Node 22+ 三者都有（sessionStorage 没有，用一个最小 stub 代替）。
// 这里不 import vue —— `auth`/`session` 是从 'vue' 的 reactive 造的，
// 而 Node 跑不了 SFC；用一个等价的 stub 注入即可（见下面的 installGlobals）。

const TOKEN_KEY = 'rosetta_gw_admin_token'

/** 记录每个 fetch 调用的入参，供断言 URL / method / headers / body。 */
function installGlobals({ token = '' } = {}) {
  const calls = []
  const store = new Map()
  if (token) store.set(TOKEN_KEY, token)

  globalThis.sessionStorage = {
    getItem: (k) => (store.has(k) ? store.get(k) : null),
    setItem: (k, v) => store.set(k, String(v)),
    removeItem: (k) => store.delete(k),
  }
  globalThis.__calls = calls
  globalThis.fetch = async (url, init) => {
    calls.push({ url, init })
    const responder = globalThis.__responder
    return responder ? responder(url, init) : jsonResponse(200, {})
  }
  return calls
}

function jsonResponse(status, body) {
  return {
    ok: status >= 200 && status < 300,
    status,
    text: async () => (body === undefined ? '' : JSON.stringify(body)),
  }
}

/** 每个用例都重新 import，让模块级状态（auth/session）回到初始值。 */
async function freshApi() {
  const mod = await import(`../src/api.ts?bust=${Date.now()}-${Math.random()}`)
  return mod
}

/** 收尾：清掉本用例注入的 responder，避免泄漏给下一个用例。 */
function resetResponder() {
  globalThis.__responder = undefined
}

test('GET 拼 /admin/api 前缀并带 Authorization 头', async () => {
  installGlobals({ token: 'tok-123' })
  const { api } = await freshApi()

  await api.providers()

  const [call] = globalThis.__calls
  assert.equal(call.url, '/admin/api/providers', '前缀必须是 /admin/api')
  assert.equal(call.init.method, 'GET')
  assert.equal(call.init.headers['Authorization'], 'Bearer tok-123')
  // GET 不带 Content-Type：没有 body 时设它是错的，会触发不必要的 CORS 预检。
  assert.equal(call.init.headers['Content-Type'], undefined)
})

test('写操作序列化 JSON body 并设 Content-Type', async () => {
  installGlobals()
  const { api } = await freshApi()

  await api.createProvider({ name: 'p', endpoint: 'https://x.example' })

  const [call] = globalThis.__calls
  assert.equal(call.init.method, 'POST')
  assert.equal(call.init.headers['Content-Type'], 'application/json')
  assert.deepEqual(JSON.parse(call.init.body), { name: 'p', endpoint: 'https://x.example' })
})

test('PATCH 用 HTTP PATCH（字段级部分更新的载体）', async () => {
  installGlobals()
  const { api } = await freshApi()

  await api.updateProvider('p1', { name: '改名' })

  const [call] = globalThis.__calls
  assert.equal(call.init.method, 'PATCH')
  assert.equal(call.url, '/admin/api/providers/p1')
})

test('DELETE 不带 body', async () => {
  installGlobals()
  const { api } = await freshApi()

  await api.deleteProvider('p1')

  const [call] = globalThis.__calls
  assert.equal(call.init.method, 'DELETE')
  assert.equal(call.init.body, undefined)
})

test('每个请求都装超时信号（挂起的网关不该让页面永久转圈）', async () => {
  installGlobals()
  const { api } = await freshApi()

  await api.providers()

  const [call] = globalThis.__calls
  assert.ok(call.init.signal, 'request() 必须带 AbortSignal')
  assert.equal(typeof call.init.signal.aborted, 'boolean')
})

test('401 清令牌并清身份，抛出带 401 的 ApiFail', async () => {
  installGlobals({ token: 'stale' })
  globalThis.__responder = () => jsonResponse(401, { error: { message: '登录已失效' } })
  const { api, ApiFail } = await freshApi()

  await assert.rejects(() => api.providers(), (err) => {
    assert.ok(err instanceof ApiFail)
    assert.equal(err.status, 401)
    return true
  })

  // 关键：不能只弹个框。令牌必须真的被清掉，否则界面停在永远 401 的空壳上。
  assert.equal(globalThis.sessionStorage.getItem(TOKEN_KEY), null)
  resetResponder()
})

test('错误信封的 message 被翻成 ApiFail.message', async () => {
  installGlobals()
  globalThis.__responder = () =>
    jsonResponse(400, { error: { message: 'display_name 不能为空', type: 'invalid_request' } })
  const { api, ApiFail } = await freshApi()

  await assert.rejects(() => api.createModelGroup(''), (err) => {
    assert.ok(err instanceof ApiFail)
    assert.equal(err.status, 400)
    assert.equal(err.message, 'display_name 不能为空')
    return true
  })
  resetResponder()
})

test('错误信封缺失时退回 HTTP 状态文案', async () => {
  installGlobals()
  globalThis.__responder = () => jsonResponse(500, undefined)
  const { api, ApiFail } = await freshApi()

  await assert.rejects(() => api.providers(), (err) => {
    assert.ok(err instanceof ApiFail)
    assert.equal(err.status, 500)
    assert.match(err.message, /500/)
    return true
  })
  resetResponder()
})

test('usage 端点的 from/to 是毫秒，不是秒', async () => {
  installGlobals()
  const { api } = await freshApi()

  // 用固定时刻：毫秒与秒的差别在数值上就是 1000 倍，肉眼可辨。
  const from = 1_700_000_000_000
  const to = 1_700_000_900_000
  await api.usageByModel(from, to, 50)

  const [call] = globalThis.__calls
  const url = new URL(call.url, 'http://x')
  assert.equal(url.searchParams.get('from'), String(from), 'from 必须原样透传毫秒值')
  assert.equal(url.searchParams.get('to'), String(to))
  assert.equal(url.searchParams.get('limit'), '50')
})

test('from=0 表示全部历史（后端据此区分「显式 0」与「没传」）', async () => {
  installGlobals()
  const { api } = await freshApi()

  await api.usageByUser(0, 1_700_000_000_000, 100)

  const [call] = globalThis.__calls
  const url = new URL(call.url, 'http://x')
  // 必须是 "0" 而不是空串 / 省略：省略会让后端回落到近 7 天，
  // 而钱包页要的是全部历史。
  assert.equal(url.searchParams.get('from'), '0')
  assert.ok(url.pathname.endsWith('/usage/by-user'))
})

test('by-day 不带 limit（按天是时间序列，不能被截断）', async () => {
  installGlobals()
  const { api } = await freshApi()

  await api.usageByDay(0, 1_700_000_000_000)

  const [call] = globalThis.__calls
  const url = new URL(call.url, 'http://x')
  assert.equal(url.searchParams.get('limit'), null, 'by-day 不应带 limit')
  assert.ok(url.pathname.endsWith('/usage/by-day'))
})

test('stats 的 include_lifetime 只在显式开启时出现', async () => {
  installGlobals()
  const { api } = await freshApi()

  await api.stats(0, 1_700_000_000_000, false)
  let url = new URL(globalThis.__calls[0].url, 'http://x')
  assert.equal(url.searchParams.get('include_lifetime'), null)

  globalThis.__calls.length = 0
  await api.stats(0, 1_700_000_000_000, true)
  url = new URL(globalThis.__calls[0].url, 'http://x')
  assert.equal(url.searchParams.get('include_lifetime'), 'true')
})

test('调用历史的过滤维度与时间范围一起进 query', async () => {
  installGlobals()
  const { api } = await freshApi()

  await api.usageHistory(7, 20, 40, { status: 'error', model: 'flash', keyId: 'k1' })

  const [call] = globalThis.__calls
  const url = new URL(call.url, 'http://x')
  assert.ok(url.pathname.endsWith('/usage/history'))
  assert.equal(url.searchParams.get('status'), 'error')
  assert.equal(url.searchParams.get('model'), 'flash')
  assert.equal(url.searchParams.get('key_id'), 'k1')
  assert.equal(url.searchParams.get('limit'), '20')
  assert.equal(url.searchParams.get('offset'), '40')
  // from/to 必须是毫秒级的时间戳（13 位），不是秒（10 位）。
  assert.match(url.searchParams.get('from'), /^\d{13}$/)
  assert.match(url.searchParams.get('to'), /^\d{13}$/)
})

test('未设置的过滤维度不拼进 query（后端视为不过滤）', async () => {
  installGlobals()
  const { api } = await freshApi()

  await api.usageHistory(7, 20, 0)

  const [call] = globalThis.__calls
  const url = new URL(call.url, 'http://x')
  assert.equal(url.searchParams.get('status'), null)
  assert.equal(url.searchParams.get('model'), null)
  assert.equal(url.searchParams.get('key_id'), null)
})

test('路由目标的 PUT 走 targets 端点', async () => {
  installGlobals()
  const { api } = await freshApi()

  await api.saveRouteTargets('r1', [
    { provider_id: 'p1', upstream_model_id: 'm1', enabled: true },
    { provider_id: 'p2', upstream_model_id: 'm2', enabled: false },
  ])

  const [call] = globalThis.__calls
  assert.equal(call.init.method, 'PUT')
  assert.equal(call.url, '/admin/api/routes/r1/targets')
  assert.equal(JSON.parse(call.init.body).targets.length, 2)
})

test('模型白名单按组写入用 PUT 到 models 子路径', async () => {
  installGlobals()
  const { api } = await freshApi()

  await api.setGroupModels('g1', ['flash', 'pro'])

  const [call] = globalThis.__calls
  assert.equal(call.init.method, 'PUT')
  assert.equal(call.url, '/admin/api/groups/g1/models')
})

test('生产路由快照完整且免鉴权端点集合固定', () => {
  assert.equal(API_ROUTES.length, 107, '路由增删必须同步承认契约变化')
  assert.equal(new Set(API_ROUTES.map((r) => r.pattern)).size, 107, '路由 pattern 不得重复')

  const publicRoutes = API_ROUTES
    .filter((r) => r.public)
    .map((r) => r.pattern)
    .sort()
  assert.deepEqual(publicRoutes, [
    'GET /admin/api/bootstrap',
    'GET /admin/api/session',
    'POST /admin/api/bootstrap',
    'POST /admin/api/login',
    'POST /admin/api/password-setup',
    'POST /admin/api/password-setup/check',
  ])

  const patterns = new Set(API_ROUTES.map((r) => r.pattern))
  for (const pattern of [
    'POST /v1/chat/completions',
    'GET /v1/models',
    'GET /healthz',
    'GET /admin/',
    'PUT /admin/api/routes/{id}/targets',
    'GET /admin/api/usage/history.csv',
  ]) {
    assert.ok(patterns.has(pattern), `缺少生产路由 ${pattern}`)
  }
})
