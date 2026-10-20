<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { toasts, confirmState, settleConfirm } from './ui'
import { isAdmin, loadSession, session, logout as apiLogout } from './api'
import { reapplyGuard } from './router'
import AppModal from './components/AppModal.vue'

const route = useRoute()
const router = useRouter()

// ---------- 侧边栏（布局参考 hirezo） ----------
//
// 左侧可收起 rail（品牌 + 图标导航 + 版本/收起钮），右侧主列
// （工具栏 + 滚动内容区）。收起状态持久化到 localStorage；
// ≤900px 视口强制图标栏（224px 会吃掉平板宽度），但用户自己的
// 选择在回到宽屏后仍然生效 —— narrow 只影响显示，不写回存储。
const COLLAPSE_KEY = 'rosetta_gw_sidebar_collapsed'
const collapsed = ref(false)
try {
  collapsed.value = localStorage.getItem(COLLAPSE_KEY) === '1'
} catch {
  /* 隐私模式下可能读不了 localStorage，按默认展开 */
}
const narrow = ref(window.matchMedia('(max-width: 900px)').matches)
let mq: MediaQueryList | null = null
function onNarrowChange(e: MediaQueryListEvent) {
  narrow.value = e.matches
}
const railCollapsed = computed(() => collapsed.value || narrow.value)

function toggleCollapse() {
  collapsed.value = !collapsed.value
  try {
    localStorage.setItem(COLLAPSE_KEY, collapsed.value ? '1' : '0')
  } catch {
    /* 忽略 */
  }
}

// 内容区在 .content 里滚（不是 window）：顶栏的发丝线在滚过 4px 后出现。
// 监听器必须挂在模板 ref 的 watch 上而不是 onMounted —— 外壳只在
// loadSession 完成后渲染，onMounted 时 contentRef 还是 null。
const scrolled = ref(false)
const contentRef = ref<HTMLElement | null>(null)
watch(contentRef, (el, _prev, onCleanup) => {
  if (!el) return
  const onScroll = () => {
    scrolled.value = el.scrollTop > 4
  }
  el.addEventListener('scroll', onScroll, { passive: true })
  onCleanup(() => el.removeEventListener('scroll', onScroll))
})

// 路由切换把内容区滚回顶部；顶栏阴影随之复位。
watch(
  () => route.path,
  () => {
    contentRef.value?.scrollTo({ top: 0 })
    scrolled.value = false
  },
)

// 导航按任务与角色组织，图标沿用项目的描边风格。
const icons = {
  overview: 'M3 3h7v7H3z M14 3h7v7h-7z M3 14h7v7H3z M14 14h7v7h-7z',
  history: 'M4 3h16v18H4z M8 7h8 M8 12h8 M8 17h5',
  usage: 'M4 20V10 M12 20V4 M20 20v-7',
  routes: 'M3 12h7 M10 12l5-7h6 M10 12l5 7h6 M17 3l4 2-4 2 M17 17l4 2-4 2',
  providers: 'M3 3h18v7H3z M3 14h18v7H3z M7 7h.01 M7 18h.01',
  networks: 'M12 3a9 9 0 1 0 0 18 9 9 0 1 0 0-18 M3 12h18 M12 3c3 4 3 14 0 18 M12 3c-3 4-3 14 0 18',
  users: 'M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2 M9 3a4 4 0 1 0 0 8 4 4 0 0 0 0-8 M17 4a4 4 0 0 1 0 7 M22 21v-2a4 4 0 0 0-3-3.87',
  wallet: 'M3 5h17v15H3z M3 5V3h14 M15 11h6v5h-6z',
  settings: 'M12 3v3 M12 18v3 M3 12h3 M18 12h3 M5.6 5.6l2.1 2.1 M16.3 16.3l2.1 2.1 M5.6 18.4l2.1-2.1 M16.3 7.7l2.1-2.1 M12 8a4 4 0 1 0 0 8 4 4 0 0 0 0-8',
  models: 'M12 3l9 5v8l-9 5-9-5V8z M3 8l9 5 9-5 M12 13v8',
  keys: 'M14 4a6 6 0 1 1-4 10l-7 7v-5l7-7a6 6 0 0 1 4-5 M17 7h.01',
}
const navigation = computed(() => isAdmin() ? [
  { label: '运行', items: [
    { to: '/', label: '网关总览', icon: icons.overview },
    { to: '/history', label: '请求日志', icon: icons.history },
    { to: '/usage', label: '用量与费用', icon: icons.usage },
  ] },
  { label: '网关配置', items: [
    { to: '/networks', label: '出站网络', icon: icons.networks },
    { to: '/routes', label: '模型与路由', icon: icons.routes },
    { to: '/providers', label: '上游服务', icon: icons.providers },
    { to: '/users', label: '用户与权限', icon: icons.users },
  ] },
  { label: '管理', items: [
    // 管理员也要一条独立的「API 密钥」入口。此前只有「用户与权限」一条，
    // 而两个路由共用一个组件：/users?tab=keys 停在密钥页签时，侧栏高亮的
    // 却是「用户与权限」，浏览器标题与页面 h1 也各说各话（同一个页面三重身份）。
    { to: '/keys', label: 'API 密钥', icon: icons.keys },
    { to: '/wallet', label: '账户与充值', icon: icons.wallet },
    { to: '/settings', label: '系统设置', icon: icons.settings },
  ] },
] : [
  { label: '我的工作台', items: [
    { to: '/', label: '我的总览', icon: icons.overview },
    { to: '/models', label: '可用模型', icon: icons.models },
    { to: '/keys', label: 'API 密钥', icon: icons.keys },
    { to: '/history', label: '我的调用', icon: icons.history },
    { to: '/wallet', label: '余额与账单', icon: icons.wallet },
  ] },
])
const locationLabel = computed(() => {
  if (route.path === '/account') return '账号 / 安全设置'
  for (const group of navigation.value) {
    const item = group.items.find(i => i.to === route.path || (i.to !== '/' && route.path.startsWith(i.to + '/')))
    if (item) return (isAdmin() ? '管理控制台' : '个人工作台') + ' / ' + group.label
  }
  return isAdmin() ? '管理控制台' : '个人工作台'
})

/**
 * 侧栏高亮用的路径。
 *
 * 两个路由共用 AccessControl.vue：管理员在 /users?tab=keys 时停的其实是
 * 密钥页签，此时该高亮「API 密钥」而不是「用户与权限」。这里把该形态
 * 归一到 /keys，高亮与页面内容、浏览器标题三者保持一致。
 */
function activeNavTo(): string {
  if (route.name === 'users' && route.query.tab === 'keys') return '/keys'
  return route.path
}

// ---------- 身份 ----------
//
// 管理面只有一条通道：users 表里的账号 + 会话。App 不再持有任何凭据 UI，
// 登录、首次初始化与登出都落在 Login.vue 与 api.ts 里。
//
// 不要再引入「把密码先存 localStorage 再靠 401 弹框重输」这套反模式：
// 它要求前端自己保存密码，且对话框 dismissable=false，密码一错就把用户
// 永久困在里面。config.json 的 admin_token / admin_auth.json 两条旁路
// 已随之删除，别再回来。

// 版本号：构建时由 vite.config.ts 的 define 注入（单一来源：package.json / go.mod），
// 这里先落到本地常量再交给模板，避免依赖模板内联替换。
const appVersion = __APP_VERSION__
const rosettaVersion = __ROSETTA_VERSION__

// ---------- 亮/暗主题 ----------
// 偏好存 localStorage，值为 'dark' / 'light' / 'system'。
// 用'system' 而不是把当前系统的解析结果固化进去：固化后首次访问就把
// 「跟随系统」变成一个具体值，此后 OS 切主题本网关再也不跟随（实测 bug）。
const THEME_KEY = 'rosetta_gw_theme'
const isDark = ref(false)
let mediaQuery: MediaQueryList | null = null

function applyTheme(dark: boolean) {
  isDark.value = dark
  document.documentElement.setAttribute('data-theme', dark ? 'dark' : 'light')
  // color-scheme 必须与 data-theme 同步：它决定表单控件、滚动条等
  // 浏览器原生 UI 的配色，不同步就会出现「深色页面 + 浅色下拉框」。
  const meta = document.querySelector('meta[name=color-scheme]')
  if (meta) meta.setAttribute('content', dark ? 'dark' : 'light')
}

function persistTheme(v: 'dark' | 'light' | 'system') {
  try {
    localStorage.setItem(THEME_KEY, v)
  } catch {
    /* 隐私模式下可能写不了，忽略 */
  }
}

function readStoredTheme(): 'dark' | 'light' | 'system' {
  try {
    const saved = localStorage.getItem(THEME_KEY)
    if (saved === 'dark' || saved === 'light') return saved
  } catch {
    /* 忽略 */
  }
  return 'system'
}

function onThemePreferenceChange(e: MediaQueryListEvent) {
  if (readStoredTheme() === 'system') applyTheme(e.matches)
}

function initTheme() {
  mediaQuery = window.matchMedia('(prefers-color-scheme: dark)')
  // 运行期跟随 OS：只有当前处于「跟随系统」时才响应系统主题变化。
  mediaQuery.addEventListener('change', onThemePreferenceChange)
  const stored = readStoredTheme()
  applyTheme(stored === 'dark' || (stored === 'system' && mediaQuery.matches))
}

// toggleTheme 是**显式**选择：用户点一下就固化，不再跟随系统。
// 想回到「跟随系统」需要清掉 localStorage 里的键（或后续加一个三态切换）。
function toggleTheme() {
  const next = !isDark.value
  applyTheme(next)
  persistTheme(next ? 'dark' : 'light')
}

/**
 * showShell 决定是否渲染后台外壳（侧栏 + 顶栏 + 内容区）。
 *
 * 三种情形一律为 false：
 *   - 探测未完成 → 先显示空白，避免刷新时闪一下完整后台再跳登录页；
 *   - 后端不可达 → 后端自己会渲染「无法连接」，外壳里的每个请求都会失败，
 *     渲染出来只是一个必然报错的空壳；
 *   - 未登录 → 只剩登录页。
 *
 * 模板里外壳（v-if）与登录页（v-else）是互斥的两个分支，同一时刻
 * 只挂载一个 RouterView。曾经把登录页放进无 else 的 v-if 里，
 * 未登录时它被外壳的判定整个藏掉 —— #app 只剩空注释节点（实测白屏）。
 */
const showShell = computed(() => {
  if (!session.checked) return false
  if (!session.backendReady) return false
  return !!session.me
})

/** 登出：让后端清掉会话，再回登录页。 */
async function doLogout() {
  try {
    await apiLogout()
  } finally {
    void router.push({ name: 'login' })
  }
}

onMounted(async () => {
  initTheme()
  mq = window.matchMedia('(max-width: 900px)')
  mq.addEventListener('change', onNarrowChange)
  // loadSession 一次性问完「后端在不在 / 有没有建出管理员 / 我是谁」，
  // 内部已把各种失败降级成状态（backendReady / needsSetup / me），
  // 不往外抛 —— 所以这里不需要 try/catch。
  await loadSession()

  // 探测完成后用守卫的同一套规则重判当前路由（硬刷新不触发 beforeEach，
  // 而 checked=false 时守卫又必然放行 —— 见 router.ts 的 reapplyGuard）：
  // 「已登录却停在 /login」被送回后台，「未登录停在受保护页」被送去登录，
  // 「普通用户停在 admin-only 页」被送回我的账号。
  reapplyGuard()
})

onUnmounted(() => {
  mq?.removeEventListener('change', onNarrowChange)
  mediaQuery?.removeEventListener('change', onThemePreferenceChange)
})

// 登录态失效时（api.ts 在 401 里把 session.me 置空）把人送回登录页。
// watch 而不是事件总线：api.ts 已经把状态改好了，这里只管导航。
//
// 只管**运行中**的掉登录：me 从非空变回 null 时源值才会改变、回调才触发。
// 启动探测期间 me 前后都是 null、值不变，这条 watch 对全新访问不动作 ——
// 那次纠正由 loadSession 之后的 reapplyGuard 负责（见 router.ts），
// 两处各管一段，别合并：合并后要么漏掉启动期（值不变的转变不可见），
// 要么在探测返回前就触发、把刚登录的人弹回登录页（曾表现为「刷新就掉登录」）。
// 带 redirect：登录成功后 Login.vue 靠它回到原来想去的那一页。
watch(
  () => (session.me ? route.name : 'login'),
  (v) => {
    if (v === 'login' && route.name !== 'login') {
      void router.push({ name: 'login', query: { redirect: route.fullPath } })
    }
  },
)
</script>

<template>
  <!-- 登录页与后台外壳是两个互斥分支，同一时刻只挂载一个 RouterView。
       旧布局把 RouterView 留在 showShell 之外、只包住顶部导航；侧边栏
       外壳包裹整个内容区后改用 v-if / v-else —— 未登录时 v-else 分支的
       登录页照样渲染，不会复现「登录页被自己的外壳挡掉」的白屏
       （那是把 RouterView 放进 v-if 内且没有 else 分支时的事故）。 -->
  <div v-if="showShell" class="shell">
    <aside class="side" :class="{ collapsed: railCollapsed }">
      <div class="side-brand">
        <div class="side-logo">RG</div>
        <div v-if="!railCollapsed" class="side-title">
          <span class="side-name">Rosetta Gateway</span>
          <span class="side-sub">{{ isAdmin() ? '管理控制台' : '个人工作台' }}</span>
        </div>
      </div>

      <nav class="side-nav" aria-label="网关导航">
        <div v-for="group in navigation" :key="group.label" class="nav-group">
          <div class="side-sep sep-label"><span v-if="!railCollapsed" class="side-sep-text">{{ group.label }}</span></div>
          <RouterLink v-for="item in group.items" :key="item.to" class="side-item" :to="item.to" :class="{ 'nav-active': item.to === activeNavTo() || item.to !== '/' && activeNavTo()?.startsWith(item.to + '/') }" :title="railCollapsed ? item.label : undefined" :aria-label="item.label">
            <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path :d="item.icon" /></svg>
            <span v-if="!railCollapsed">{{ item.label }}</span>
          </RouterLink>
        </div>
      </nav>
      <RouterLink to="/account" class="side-item account-link" title="账号设置" aria-label="账号设置">
        <svg viewBox="0 0 24 24" width="18" height="18" fill="none" stroke="currentColor" stroke-width="1.75" aria-hidden="true"><circle cx="12" cy="8" r="4" /><path d="M4 21v-2a8 8 0 0 1 16 0v2" /></svg>
        <span v-if="!railCollapsed">账号设置</span>
      </RouterLink>

      <div class="side-foot">
        <span class="side-version">v{{ appVersion }} · rosetta {{ rosettaVersion }}</span>
        <button
          class="side-collapse"
          :title="railCollapsed ? '展开侧边栏' : '收起侧边栏'"
          :aria-label="railCollapsed ? '展开侧边栏' : '收起侧边栏'"
          @click="toggleCollapse"
        >
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <rect width="18" height="18" x="3" y="3" rx="2" />
            <path d="M9 3v18" />
          </svg>
        </button>
      </div>
    </aside>

    <div class="main-col">
      <header class="topbar" :class="{ scrolled }">
        <!-- 顶栏提供作用域与分区，任务标题由页面 h1 显示。 -->
        <span class="topbar-title">{{ locationLabel }}</span>
        <span
          v-if="session.me"
          class="who"
          :title="`${session.me.username}（${session.me.role === 'admin' ? '管理员' : '普通用户'}）`"
        >
          {{ session.me.username }}
        </span>
        <!-- 亮/暗主题切换 -->
        <button
          class="theme-toggle"
          :title="isDark ? '切换到亮色模式' : '切换到暗色模式'"
          :aria-label="isDark ? '切换到亮色模式' : '切换到暗色模式'"
          @click="toggleTheme"
        >
          <svg v-if="isDark" viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <circle cx="12" cy="12" r="4.5" />
            <path d="M12 2v2.5M12 19.5V22M4.9 4.9l1.8 1.8M17.3 17.3l1.8 1.8M2 12h2.5M19.5 12H22M4.9 19.1l1.8-1.8M17.3 6.7l1.8-1.8" />
          </svg>
          <svg v-else viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" />
          </svg>
        </button>
        <!-- 登出：让后端清掉会话，再回登录页。 -->
        <button
          v-if="session.me"
          class="theme-toggle"
          title="退出登录"
          aria-label="退出登录"
          @click="doLogout"
        >
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" />
            <path d="M16 17l5-5-5-5" />
            <path d="M21 12H9" />
          </svg>
        </button>
      </header>

      <!-- 内容在 .content 里滚而不是整页滚：侧栏与顶栏因此天然固定 -->
      <main ref="contentRef" class="content">
        <RouterView />
      </main>
    </div>
  </div>
  <!-- 探测未完成时**什么都不挂**（2026-10-10 修复的 P2）。
       原实现这里是裸的 v-else：session.checked 还是 false 时，
       v-else 会把 hash 指向的那个页面（硬刷新 /#/ 就是总览）挂上去，
       而此时守卫必然放行（router.ts 的 guardDecision 第一行）、
       后端身份又还没查到 —— 于是受保护页自己发起了请求：
         - 有有效令牌：5 个请求白跑一次（探测完成后组件重建，会再发 5 个）；
         - 无/过期令牌：5 个 401 打出去，api.ts 顺手清掉本地令牌，
           而某些限速器会把这些计入失败。
       现在探测完成前不挂载任何页面：之后要么进外壳，要么被 reapplyGuard
       送去登录页，两条路都不会让受保护页先跑一轮注定失败的请求。 -->
  <div v-else-if="session.checked" class="boot">
    <RouterView />
  </div>

  <!-- Toast -->
  <div
    v-if="toasts.length"
    :key="toasts[0].id"
    class="toast"
    :class="{ err: toasts[0].kind === 'err' }"
    role="status"
    aria-live="polite"
  >
    {{ toasts[0].text }}
  </div>

  <!-- 确认对话框 -->
  <AppModal
    :open="confirmState.open"
    :title="confirmState.title"
    max-width="420px"
    @close="settleConfirm(false)"
  >
    <div class="sheet-body">{{ confirmState.body }}</div>
    <div class="form-actions">
      <button class="btn btn-ghost" @click="settleConfirm(false)">取消</button>
      <button
        class="btn"
        :class="confirmState.danger ? 'btn-danger' : 'btn-primary'"
        @click="settleConfirm(true)"
      >
        {{ confirmState.confirmLabel }}
      </button>
    </div>
  </AppModal>
</template>

