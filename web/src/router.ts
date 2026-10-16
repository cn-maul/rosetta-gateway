import { createRouter, createWebHashHistory, type RouteLocationNormalized, type RouteLocationRaw } from 'vue-router'
import { isAdmin, session } from './api'

// hash 路由：go:embed 单二进制场景下无需服务端 fallback 配置
const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/login', name: 'login', component: () => import('./views/Login.vue'), meta: { title: '登录', public: true } },
    { path: '/', name: 'overview', component: () => import('./views/Overview.vue'), meta: { title: '总览' } },
    // 「访问密钥」不再是一个独立侧栏项（2026-10-14 页面合并），它是
    // 「用户与分组」页里的第三个页签 —— 那三样东西是一条权限链上的三环，
    // 排查「这把 key 为什么调不通这个模型」必须同时看用户 / 分组 / 密钥。
    //
    // 老书签 /keys **重定向**到 /users，而不是就地渲染新页面：
    // 普通用户进 /users 时默认页签就是「访问密钥」（他们看不到前两个），
    // 管理员进 /users 落在「用户」页签 —— 与合并前的落点一致，不需要额外
    // 记住「该带哪个 query」。刻意**不做** ?tab= 同步：那会让页签状态
    // 同时存在于 URL 与组件两处，改一处忘另一处就会漂移（页签本身已经
    // 有 :key 重建机制，不需要 URL 参与）。
    { path: '/keys', redirect: { name: 'users' } },
    // 「调用历史」恢复给普通用户（2026-10-10）。上一轮按用户给的侧边栏顺序
    // 把它划进了 admin 区，理由是「普通用户看自己的消费记录走钱包页」——
    // 但那是**聚合数**，不是明细。排查「我刚才那次调用为什么报错/为什么这么慢/
    // 为什么花了 0.4 分」需要逐条记录，钱包页的汇总答不了这个问题。
    //
    // 刻意**不带 adminOnly**：后端按会话身份强制收窄作用域
    // （usage_handler.scopeUserID：普通用户只见自己的，管理员空串=全量），
    // 且 history / history.csv 早已在 userAccessibleRoutes 白名单里
    // （internal/server/user_auth.go），并有 authz_matrix 测试钉住。
    // 页内无任何 admin-only 数据源：密钥下拉走 /keys（同样按身份收窄）。
    // 「用量统计」（2026-10-16 由「调用历史」改名并拆成三个子页：
    // 调用明细 / 上游 / 模型）。三个子页共用一个时间范围，放在父页面持有 ——
    // 各带一个选择器的话，用户在「上游」页选了 30 天、切到「明细」页
    // 又看到默认 7 天，两页数字对不上且会被当成网关算错账。
    //
    // 老书签 /history **重定向**到本页并落在「调用明细」子页 ——
    // 与合并前的落点一致，不给老书签一个「默认落到汇总页」的行为改变。
    { path: '/usage', name: 'usage', component: () => import('./views/UsageStats.vue'), meta: { title: '用量统计' } },
    { path: '/history', redirect: { name: 'usage' } },
    // 「可用模型」恢复（2026-10-10）：它一度随管理员退出数据面一起被删，
    // 理由是「管理员用不上这份清单」。这个理由只对管理员成立，却让**普通
    // 用户**也跟着没了入口 —— 而这页恰恰是普通用户最需要的：它回答
    // 「我实际能调哪些模型、单价多少」，ModelPicker 只在打开建 key 表单时
    // 顺带露一眼，且那是「写白名单」的控件、语义完全不同（见 Models.vue
    // 的文件头注释）。
    //
    // 刻意**不带 adminOnly**：普通用户查自己的可用模型与用量同权，
    // 后端 GET /admin/api/model-names 早已在 userAccessibleRoutes 白名单里
    // （见 internal/server/user_auth.go），并按身份收窄返回。
    { path: '/models', name: 'models', component: () => import('./views/Models.vue'), meta: { title: '可用模型' } },
    // 「钱包与充值」。**刻意保留 name/path 为 profile**：路由名已经进了
    // 若干跳转（守卫把非 admin 踢回这里、登出后 push 回这里），改路径会让
    // 老书签 404。展示标题改成「钱包与充值」，见下方 meta.title。
    { path: '/profile', name: 'profile', component: () => import('./views/Wallet.vue'), meta: { title: '钱包与充值' } },
    // /wallet 别名（2026-10-12）：地址栏写着 profile、页面写着「钱包与充值」，
    // 分享链接或做书签时会困惑。加一条语义正确的路径并让 profile 重定向过去，
    // 老书签（/profile）与守卫里已有的 name: 'profile' 跳转都不受影响 ——
    // 重定向是 profile → wallet，而跳转用的是**路由名**而非路径，
    // 所以「踢回钱包」仍然落在 /wallet 上。
    {
      path: '/wallet',
      name: 'wallet',
      component: () => import('./views/Wallet.vue'),
      meta: { title: '钱包与充值' },
    },
    {
      path: '/profile',
      redirect: { name: 'wallet' },
    },
    // 以下 admin-only：普通用户既看不到入口，也会被守卫踢走。
    // meta.adminOnly 让导航栏能按 isAdmin() 隐藏它们，守卫负责兜底——
    // 只做前者的话，手输 URL 就能打开。
    //
    // 「用户 / 分组 / 访问密钥」合并成一个页面（2026-10-14；用户与分组原
    // 已在 2026-10-10 合并过一次）。三者是一条权限链上的三环：账号的分组、
    // 组的模型白名单、key 级的更严覆盖 —— 排查一次「调不通」要同时看三处，
    // 拆成三页就得凭记忆把 id/name 来回对上。
    //
    // **刻意不带 adminOnly**：普通用户在侧栏点这一项进来，看到的是自己的
    // 访问密钥页签（页签按身份收窄，见 AccessControl.vue）。他们在合并前
    // 用的正是 /keys 的能力，这里一比一保留；而 /admin/api/users 与
    // /groups 对他们本就是 admin-only，给入口只会让人白撞一次 403。
    { path: '/users', name: 'users', component: () => import('./views/AccessControl.vue'), meta: { title: '用户与分组' } },
    // 「访问密钥」不再是独立侧栏项，它就是上面那一页的第三个页签。
    // 老书签 /keys 重定向到 /users：普通用户的默认页签就是「访问密钥」，
    // 管理员落在「用户」页签 —— 与合并前的落点一致，不必额外带 query。
    // 刻意**不做** ?tab= 同步：页签状态一旦同时存在于 URL 与组件两处，
    // 改一处忘另一处就会漂移。
    { path: '/keys', redirect: { name: 'users' } },
    // 「上游与模型 / 路由 / 模型归组」合并成一个页面（2026-10-14）。三者是
    // 同一条数据面配置链的相邻三段，页签顺序即链的顺序（上游 → 路由 → 归组）。
    { path: '/providers', name: 'providers', component: () => import('./views/Upstream.vue'), meta: { title: '上游与模型', adminOnly: true } },
    // 老地址并进上面那个页面。**必须留重定向而不是删掉**：老书签与站内
    // /routes 链接不该 404。重定向后统一落在配置链的起点页签。
    { path: '/routes', redirect: { name: 'providers' } },
    { path: '/model-groups', redirect: { name: 'providers' } },
    // 「连通率」不再是独立页面（2026-10-14）：每张模型卡里已经同时显示单价与
    // 连通率曲线（Models.vue）。拆成两页的代价是判断「该换档还是该降价」
    // 得先记下模型名、去另一页按 id 找、再回来对价格。
    //
    // 老书签重定向到「可用模型」——那些链接记着的是「我要看连通率」，
    // 而那张卡片现在就在那一页。
    { path: '/health', redirect: { name: 'models' } },
    { path: '/settings', name: 'settings', component: () => import('./views/Settings.vue'), meta: { title: '设置', adminOnly: true } },
  ],
})

// 路由守卫：两件事。
//
// 1. 未登录 → 去登录页。
// 2. 要访问 admin-only 页而当前不是管理员 → 回自己的账号页。
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
  if (to.meta.adminOnly && !isAdmin()) {
    return { name: 'profile' }
  }
  return true
}

router.beforeEach((to) => guardDecision(to))

router.afterEach((to) => {
  const pageTitle = typeof to.meta.title === 'string' ? to.meta.title : ''
  document.title = pageTitle ? `${pageTitle} · Rosetta Gateway` : 'Rosetta Gateway · 管理后台'
})

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
}

export default router
