<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { toasts, confirmState, settleConfirm } from './ui'
import { isAdmin, loadSession, session, logout as apiLogout } from './api'
import { reapplyGuard } from './router'
import AppModal from './components/AppModal.vue'

const route = useRoute()
const router = useRouter()
const scrolled = ref(false)
const onScroll = () => (scrolled.value = window.scrollY > 4)
onMounted(() => window.addEventListener('scroll', onScroll, { passive: true }))
onUnmounted(() => window.removeEventListener('scroll', onScroll))

// 窄屏页签滚动跟随（选中页签滚到可视区中央）
const tabsScroll = ref<HTMLElement | null>(null)
watch(
  () => route.path,
  async () => {
    const ts = tabsScroll.value
    if (!ts || ts.scrollWidth <= ts.clientWidth) return
    await Promise.resolve()
    const active = ts.querySelector('.router-link-active') as HTMLElement | null
    if (!active) return
    const target = active.offsetLeft - (ts.clientWidth - active.offsetWidth) / 2
    ts.scrollTo({ left: Math.max(0, target), behavior: 'smooth' })
  },
)

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

function initTheme() {
  mediaQuery = window.matchMedia('(prefers-color-scheme: dark)')
  // 运行期跟随 OS：只有当前处于「跟随系统」时才响应系统主题变化。
  mediaQuery.addEventListener('change', (e) => {
    if (readStoredTheme() === 'system') applyTheme(e.matches)
  })
  applyTheme(mediaQuery.matches)
}

// toggleTheme 是**显式**选择：用户点一下就固化，不再跟随系统。
// 想回到「跟随系统」需要清掉 localStorage 里的键（或后续加一个三态切换）。
function toggleTheme() {
  const next = !isDark.value
  applyTheme(next)
  persistTheme(next ? 'dark' : 'light')
}

/**
 * showShell 决定是否渲染后台外壳（导航栏 + 页脚）。
 *
 * 三种情形一律为 false：
 *   - 探测未完成 → 先显示空白，避免刷新时闪一下完整后台再跳登录页；
 *   - 后端不可达 → 后端自己会渲染「无法连接」，外壳里的每个请求都会失败，
 *     渲染出来只是一个必然报错的空壳；
 *   - 未登录 → 只剩登录页。
 *
 * 注意它**只管外壳**。<RouterView> 有意留在这个 v-if 之外：登录页也是
 * 一条路由，被包进来就等于「未登录时唯一该出现的页面被守卫挡掉」，
 * 表现为整页空白（登录后同一份构建正常，故不是数据问题）。
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
  <!-- 未登录时不渲染后台外壳：只剩登录页自己的头部。
       showShell 的判定刻意包含 session.checked —— 探测完成前先显示空白，
       避免刷新页面时闪一下完整后台再跳登录页。 -->
  <template v-if="showShell">
  <header class="nav" :class="{ scrolled }">
    <div class="nav-in">
      <div class="brand">
        <span class="brand-mark">RG</span>
        <span class="brand-txt">Rosetta Gateway</span>
        <span class="brand-sub">管理后台</span>
      </div>
      <div ref="tabsScroll" class="tabs-scroll">
        <nav class="tabs">
          <RouterLink class="tab" to="/">总览</RouterLink>
          <RouterLink class="tab" to="/keys">访问密钥</RouterLink>
          <RouterLink class="tab" to="/history">调用历史</RouterLink>
          <RouterLink class="tab" to="/profile">我的账号</RouterLink>
          <!-- admin-only 页签对普通用户隐藏。真正的拦截在路由守卫与后端，
               这里只是不让用户看到点进去才发现没权限的入口。 -->
          <template v-if="isAdmin()">
            <RouterLink class="tab" to="/users">用户</RouterLink>
            <RouterLink class="tab" to="/groups">分组</RouterLink>
            <RouterLink class="tab" to="/providers">上游与模型</RouterLink>
            <RouterLink class="tab" to="/routes">路由</RouterLink>
            <RouterLink class="tab" to="/settings">设置</RouterLink>
          </template>
        </nav>
      </div>
      <div class="nav-actions">
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
        <!-- 登出：只有一种形态了。让后端清会话后回登录页，
             别再引入「只清本机令牌然后重载」的旁路 —— 那要求前端持有密码。 -->
        <span v-if="session.me" class="who" :title="`${session.me.username}（${session.me.role === 'admin' ? '管理员' : '普通用户'}）`">
          {{ session.me.display_name || session.me.username }}
        </span>
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
      </div>
    </div>
  </header>
  </template>

  <!-- RouterView 刻意留在 showShell 之外。登录页本身也是一条路由，而它原先
       被包在 showShell 的 v-if 里 —— 未登录时 showShell 为 false，
       于是唯一该出现的登录页被自己的守卫挡掉：浏览器里 #app 的 innerHTML
       退化成两个空注释节点，document.body.innerText 长度为 0（实测白屏）。
       现在只有导航栏与页脚跟着 showShell 走，路由内容一律照常渲染。 -->
  <RouterView />

  <template v-if="showShell">
  <!-- 页脚版本号（构建时注入，非运行时接口） -->
  <footer class="foot">
    <span>rosetta-gateway v{{ appVersion }}</span>
    <span class="foot-sep" aria-hidden="true">·</span>
    <span>rosetta {{ rosettaVersion }}</span>
  </footer>
  </template>

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

