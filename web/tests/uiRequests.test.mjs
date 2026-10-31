import test from 'node:test'
import assert from 'node:assert/strict'
import { readFile } from 'node:fs/promises'
import { fileURLToPath } from 'node:url'

// 2026-10-31 的一批界面调整，逐条钉住「用户明确要求过的东西」。
//
// 这些都不是重构不变量，而是**需求不变量**：需求里点名的文案、位置、增删，
// 很容易在下一次「顺手整理」里被悄悄改回去。每条都写清它守住的是哪一句要求。

const SRC = fileURLToPath(new URL('../src', import.meta.url))
const read = (rel) => readFile(new URL('../src/' + rel, import.meta.url), 'utf8')

test('调用日志：时间带年份，协议列用上下箭头', async () => {
  const fmt = await read('fmt.ts')
  assert.match(fmt, /export function fmtTimeFull/, '缺少 fmtTimeFull：请求日志的时间必须带年份')
  assert.match(fmt, /getFullYear\(\)/, 'fmtTimeFull 没有输出年份')

  const page = await read('pages/UsageDetail.tsx')
  assert.ok(
    page.includes('fmtTimeFull(h.ts)'),
    '调用日志的时间单元格没用 fmtTimeFull —— 用户要求看到完整的 2026-10-31 21:59:04',
  )
  assert.ok(!page.includes('fmtTimeMs(h.ts)'), '时间单元格退回了不带年份的 fmtTimeMs')
  // 协议格：↑ 上行 / ↓ 下行，两个字换成箭头（读完仍靠 sr-only）。
  assert.ok(page.includes('aria-hidden="true" className="text-muted">↑'), '协议列缺少 ↑ 箭头')
  assert.ok(page.includes('aria-hidden="true" className="text-muted">↓'), '协议列缺少 ↓ 箭头')
  assert.ok(page.includes('<span className="sr-only">上行</span>'), '↑ 缺少读屏用的「上行」')
  assert.ok(page.includes('<span className="sr-only">下行</span>'), '↓ 缺少读屏用的「下行」')
  assert.ok(!page.includes('>上游</span>'), '协议列仍在用「上游」两个字，应改成箭头')
})

test('侧边栏与页标题的精简措辞', async () => {
  const nav = await read('ui/nav.ts')
  assert.ok(nav.includes("label: '供应商'"), '侧栏应显示「供应商」而不是「上游服务」')
  assert.ok(nav.includes("label: '路由'"), '侧栏应显示「路由」而不是「模型与路由」')
  assert.ok(nav.includes("label: '设置'"), '侧栏应显示「设置」而不是「系统设置」')
  assert.ok(!nav.includes("'模型与路由'"), '「模型与路由」应已精简为「路由」')
  assert.ok(!nav.includes("'系统设置'"), '「系统设置」应已精简为「设置」')
  // 管理员与普通用户看到同一句「调用日志」。
  const logLabels = nav.match(/label: '调用日志'/g) ?? []
  assert.equal(logLabels.length, 2, 'ADMIN_NAV 与 USER_NAV 都应显示「调用日志」')
  assert.ok(!nav.includes("'我的调用'"), '「我的调用」应已统一为「调用日志」')

  const router = await read('router.tsx')
  assert.ok(router.includes("title: '调用日志'"), '路由 handle 标题没跟着改')
  assert.ok(!router.includes("title: '我的调用'") && !router.includes("'请求日志'"))
})

test('普通用户视图：藏上游列、模型只显示路由名', async () => {
  const page = await read('pages/UsageDetail.tsx')
  assert.ok(page.includes('const admin = isAdmin()'), '调用日志没用 isAdmin 区分视图')
  assert.ok(
    page.includes('{admin ? <Table.Column>上游</Table.Column> : null}'),
    '上游列表头没有按身份隐藏',
  )
  // 实际模型（上游模型名）只在管理员视角出现：表体里那一行必须被 admin 守卫包住。
  const body = page.slice(page.indexOf('<Table.Body>'))
  assert.ok(
    body.includes('{admin ? (') && body.includes('h.upstream_model'),
    '实际模型那一行没有按身份隐藏 —— 普通用户不该看到背后用的是哪个模型',
  )
  // 守卫必须紧贴在 upstream_model 那一行之前，而不是文件里任意一处 admin 判断。
  assert.ok(
    /\{admin \? \(\s*<span[^>]*text-muted[^>]*title=\{h\.upstream_model/.test(body),
    '实际模型那一行没被 {admin ? …} 直接包住',
  )
})

test('登录页：品牌 + 主题切换按钮', async () => {
  const login = await read('ui/Login.tsx')
  assert.ok(login.includes("from './BrandMark'"), '登录页没接入 BrandMark')
  assert.ok(login.includes('<BrandMark size={40} />'), '登录页缺少站点标志')
  assert.ok(login.includes('{branding.name}'), '登录页标题没有使用站点名')
  assert.ok(!login.includes('rosetta-gateway</h1>'), '登录页标题仍是写死的包名')
  assert.ok(login.includes("from './theme'"), '登录页没接入主题模块')
  assert.ok(login.includes('<Sun size={16}'), '登录页缺少亮/暗切换按钮的图标')
  assert.ok(login.includes('onPress={toggle}'), '登录页的主题按钮没接 toggle')

  const branding = await read('ui/branding.ts')
  assert.ok(branding.includes("'/admin/api/site'"), '站点品牌应读公开端点 /admin/api/site')
  assert.ok(
    branding.includes('DEFAULT_SITE_NAME'),
    '没配置站点名时必须有默认值（后端空串不能变成空标题）',
  )
})

test('设置页：有「站点」页签，且该页签不显示外层刷新', async () => {
  const settings = await read('pages/Settings.tsx')
  assert.ok(settings.includes("key: 'site'"), '设置页缺少「站点」页签')
  assert.ok(settings.includes('SiteBrandCard'), '站点页签没渲染 SiteBrandCard')
  assert.ok(
    settings.includes("tab === 'site' || tab === 'price' ? undefined :"),
    '站点/价格页签下不应显示外层的「刷新」（它们各有自己的数据源）',
  )

  const card = await read('components/settings/SiteBrandCard.tsx')
  assert.ok(card.includes('api.saveSite'), '站点卡片保存没走 api.saveSite')
  assert.ok(card.includes('setBranding('), '保存后没写进共享品牌 store，侧栏不会立刻变')
  assert.ok(card.includes('MAX_LOGO_BYTES'), 'LOGO 缺少尺寸上限校验（后端也有，但前端要先拦）')
  // 反手写门禁要求文件输入只能出现在 ui/FileImport。
  assert.ok(!/<input\b/.test(card), '站点卡片里手写了原生 input，应改用 ui/FileImport')
})

test('新建上游：协议检测按钮与结论胶囊', async () => {
  const page = await read('pages/Providers.tsx')
  assert.ok(page.includes('api.probeProtocols'), '新建上游弹窗没接协议检测接口')
  assert.ok(page.includes('协议检测'), '缺少「协议检测」按钮文案')
  assert.ok(page.includes('探测中…'), '探测进行中没有反馈文案')
  assert.ok(page.includes('function probeTone'), '缺少探测结论的配色函数')
  // 结论必须渲染成 StatusChip 胶囊，且区分「可用 / 未发现」。
  assert.ok(page.includes("r.ok ? ' 可用' : ' 未发现'"), '探测结论没有渲染成可用/未发现胶囊')
  assert.ok(page.includes("'warn'"), '鉴权未通过（401/403）应有别于「没有这个协议」的颜色')
})

test('API 密钥页：刷新与重算用量两个按钮已删除', async () => {
  const access = await read('pages/AccessControl.tsx')
  assert.ok(!access.includes('setBump'), 'AccessControl 仍有刷新用的重建计数')
  assert.ok(!access.includes('>刷新<'), '「刷新」按钮没删干净')
  assert.ok(!access.includes('PageHeader\n        title={heading}\n        sub={sub}\n        actions'), '页头仍挂着 actions')

  const table = await read('components/keys/KeysTable.tsx')
  assert.ok(!table.includes('重算用量'), '「重算用量」按钮没删干净')
  assert.ok(!table.includes('onRecompute'), 'KeysTable 仍要求 onRecompute prop')

  const keys = await read('components/Keys.tsx')
  assert.ok(!keys.includes('recomputeUsage'), 'Keys 里仍留着 recomputeUsage 实现')
  assert.ok(!keys.includes('recomputing={'), 'Keys 仍在传 recomputing')
  // 后端能力保留：端点封装不该跟着删。
  const api = await read('api.ts')
  assert.ok(api.includes('recomputeKeyUsage'), 'api.recomputeKeyUsage 应保留（只是界面不再暴露）')
})
