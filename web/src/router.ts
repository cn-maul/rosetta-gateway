import { createRouter, createWebHashHistory, type RouteLocationNormalized, type RouteLocationRaw } from 'vue-router'
import { isAdmin, session } from './api'

// hash 路由：go:embed 单二进制场景下无需服务端 fallback 配置
const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/login', name: 'login', component: () => import('./views/Login.vue'), meta: { title: '登录', public: true } },
    { path: '/', name: 'overview', component: () => import('./views/Overview.vue'), meta: { title: '总览' } },
    { path: '/history', name: 'history', component: () => import('./views/UsageStats.vue'), meta: { title: '请求日志' } },
    { path: '/usage', name: 'usage', component: () => import('./views/UsageStats.vue'), meta: { title: '用量与费用' } },
    { path: '/models', name: 'models', component: () => import('./views/Models.vue'), meta: { title: '可用模型' } },
    { path: '/wallet', name: 'wallet', component: () => import('./views/Wallet.vue'), meta: { title: '余额与账单' } },
    { path: '/profile', name: 'profile', redirect: { name: 'wallet' } },
    { path: '/keys', name: 'keys', component: () => import('./views/AccessControl.vue'), meta: { title: 'API 密钥' } },
    { path: '/users', name: 'users', component: () => import('./views/AccessControl.vue'), meta: { title: '用户与权限', adminOnly: true } },
    {
      path: '/providers', name: 'providers', component: () => import('./views/Providers.vue'),
      meta: { title: '上游服务', adminOnly: true },
      // 旧页签地址继续定位到对应任务。
      beforeEnter: to => to.query.tab === 'routes' ? { name: 'routes' }
        : to.query.tab === 'model-groups' ? { name: 'usage', query: { tab: 'groups' } } : true,
    },
    { path: '/routes', name: 'routes', component: () => import('./views/Routes.vue'), meta: { title: '模型与路由', adminOnly: true } },
    { path: '/model-groups', redirect: { name: 'usage', query: { tab: 'groups' } } },
    { path: '/providers/:id', name: 'provider-detail', component: () => import('./views/ProviderDetail.vue'), meta: { title: '上游详情', adminOnly: true } },
    { path: '/health', redirect: { name: 'models' } },
    { path: '/account', name: 'account', component: () => import('./views/AccountSettings.vue'), meta: { title: '账号设置' } },
    {
      path: '/settings', name: 'settings', component: () => import('./views/Settings.vue'),
      meta: { title: '系统设置', adminOnly: true },
    },
    { path: '/:pathMatch(.*)*', redirect: '/' },
  ],
})

// 路由守卫：两件事。
//
// 1. 未登录 → 去登录页。
// 2. 要访问 admin-only 页而当前不是管理员 → 回个人总览（旧用户入口回密钥页）。
//
// 判定提成 guardDecision：除了 beforeEach，App.vue 在 loadSession 完成后
// 还要**重判一次当前路由**（reapplyGuard）—— 两处共用同一套规则，
// 各写一份迟早漂移，漂移的后果是同一个 URL 有两种归宿。
//
// 刻意不在 session.checked 为 false 时拦截：刷新页面时 loadSession
// 还没跑完，那时 session.me 必然是空的，若照「未登录」处理就会在
// 启动瞬间把已登录用户弹到登录页。
//
// 另：前端隐藏入口**不构成权限控制**。真正的拦截在后端
// （每个端点的 requireAdmin + 作用域过滤），这里只是避免让用户
// 撞到无意义的 403。
function guardDecision(to: RouteLocationNormalized): true | RouteLocationRaw {
  if (!session.checked) return true

  // 已登录的人不该再看见登录页。会话过期把人踢回这里后重新登录，
  // 或刷新时 hash 还停在 /login —— 两种情况都该直接放行进后台，
  // 否则页面停在登录表单上，看起来像「登录没生效」（导航栏其实已经在了）。
  if (to.meta.public && session.me) {
    // 回跳目标只接受站内相对路径（单斜杠开头）：redirect 来自 URL query，
    // 不校验就是开放重定向 —— 受害者被送到 /login?redirect=//evil.com，
    // 登录后这个 replace 把他交给外站，而且是从我们自己的域名出发的，
    // 钓鱼链接可信度高得多。// 开头是协议相对地址，必须显式排除。
    const r = to.query.redirect
    return typeof r === 'string' && r.startsWith('/') && !r.startsWith('//') ? r : '/'
  }
  if (to.meta.public) return true
  // session.checked 之前一律放行（见上方说明）：此时 session.me 必然是 null，
  // 若照「未登录」处理，刷新页面就会被弹到登录页 ——
  // 哪怕会话完全有效（App.vue 那侧的 watch 也有同样的坑，见其注释）。
  //
  // 判定只看 me 是否为空。过去这里还附加过「是否启用了多用户」的开关，
  // 用来在单密码模式下跳过登录；那条通道已删除，现在只有一种部署形态，
  // 再留开关就等于凭空多出一条不登录就能用的分支。
  if (!session.me) {
    // 带上来源页（2026-10-10 修复的 P2）：原先一律裸跳登录页，
    // 于是「后端短暂不可达 → 被送去登录页 → 用户恢复后重登」这条路上，
    // 原本要去的那一页被静默丢弃，只能落到默认首页。App.vue 那条
    // 「运行中掉登录」的 watch 一直会带 redirect，只有启动期的
    // reapplyGuard 这条不带 —— 两处口径不一致，补齐即可。
    //
    // to.meta.public 为真时**不**带：那时已经在登录页上，
    // 把 /login 自己塞进 redirect 会让登录后跳回登录页。
    //
    // 无开放重定向风险：Login.vue 的 leaveToBack 与上面那段
    // 公开页回跳用的是同一套校验（单斜杠开头且排除 //）。
    return { name: 'login', query: to.meta.public ? {} : { redirect: to.fullPath } }
  }
  // 个人改密的旧地址对两种角色都映射到账号页，先于管理权限判断。
  if (to.name === 'settings' && to.query.tab === 'password') return { name: 'account' }
  if (to.meta.adminOnly && !isAdmin()) {
    return { name: to.name === 'users' ? 'keys' : 'overview' }
  }
  return true
}

router.beforeEach((to) => guardDecision(to))

function updateDocumentTitle(to: RouteLocationNormalized) {
  const titles: Record<string, string> = {
    overview: isAdmin() ? '网关总览' : '我的总览',
    history: isAdmin() ? '请求日志' : '我的调用',
    wallet: isAdmin() ? '账户与充值' : '余额与账单',
    // /keys 与 /users 共用 AccessControl.vue，但浏览器标题必须跟着当前
    // 页签走：管理员在 /users?tab=keys 时停在「API 密钥」页签，标题却仍是
    // meta.title 的「用户与权限」—— 与组件 h1（已按页签取值）不一致。
    keys: 'API 密钥',
    users: '用户与权限',
  }
  const pageTitle = titles[String(to.name)] ?? (typeof to.meta.title === 'string' ? to.meta.title : '')
  document.title = pageTitle ? pageTitle + ' · Rosetta Gateway' : 'Rosetta Gateway'
}
router.afterEach(updateDocumentTitle)

/**
 * 探测（loadSession）完成后用守卫规则重判**当前**路由。
 *
 * 为什么必须：beforeEach 只在导航时跑，而硬刷新不产生导航 —— 刷新瞬间
 * checked=false 又必然放行。两件事叠加，启动期没人再纠正路由：
 *   - 未登录的人停在受保护页，看到没有导航栏的裸页面
 *     （实测：打开 /admin/ 停在总览页，没人送去登录页）；
 *   - 普通用户手输 admin-only 地址，停在「需要管理员权限」的 toast 上，
 *     而不是被送回自己的账号页。
 * App.vue 在 loadSession 之后调一次本函数，把这两类启动期错位纠正掉。
 */
export function reapplyGuard(): void {
  const cur = router.currentRoute.value
  const d = guardDecision(cur)
  if (d !== true) void router.replace(d)
  else updateDocumentTitle(cur)
}

export default router
