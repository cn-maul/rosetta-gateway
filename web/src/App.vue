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
          <span class="side-sub">管理后台</span>
        </div>
      </div>

      <!-- admin-only 页签对普通用户隐藏。真正的拦截在路由守卫与后端，
           这里只是不让用户看到点进去才发现没权限的入口。

           顺序按「从常看到配置」排（2026-10-10 调整）：
           总览 → 访问密钥 → 钱包与充值 → 上游与模型 → 路由 → 用户与分组 → 调用历史 → 设置。
           admin 部分的动线是「看概况 → 配上游 → 配路由 → 管人 → 查账 → 改设置」，
           每一步都建立在前面步骤的产物上。个人功能（密钥 / 钱包）放在 admin 区
           之外，因为普通用户只有它们。 -->
      <nav class="side-nav" aria-label="管理后台导航">
        <RouterLink class="side-item" to="/" :title="railCollapsed ? '总览' : undefined">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <rect width="7" height="9" x="3" y="3" rx="1" />
            <rect width="7" height="5" x="14" y="3" rx="1" />
            <rect width="7" height="9" x="14" y="12" rx="1" />
            <rect width="7" height="5" x="3" y="16" rx="1" />
          </svg>
          <span v-if="!railCollapsed">总览</span>
        </RouterLink>
        <RouterLink class="side-item" to="/keys" :title="railCollapsed ? '访问密钥' : undefined">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <path d="M2 18v3c0 .6.4 1 1 1h4v-3h3v-3h2l1.4-1.4a6.5 6.5 0 1 0-4-4Z" />
            <circle cx="16.5" cy="7.5" r=".5" fill="currentColor" />
          </svg>
          <span v-if="!railCollapsed">访问密钥</span>
        </RouterLink>
        <!-- 调用历史移进 admin 区（2026-10-10）：用户给的侧边栏顺序里它是第 5 项，
             排在「用户与分组」之后，属于管理员查账的动线。普通用户看自己的
             消费记录走「钱包与充值」页，不必进这个完整台账。 -->
        <!-- 「可用模型」页已删除（2026-10-10）：它是「我能用哪些模型」的只读清单，
             而管理员不再调用 API，也就用不上这份清单。普通用户仍可在
             「访问密钥」页的可用模型选择器里看到自己被授权的模型
             （ModelPicker，数据源同一个 /model-names 端点）。
             路由 /models 与 Models.vue 一并删除。 -->
        <RouterLink class="side-item" to="/profile" :title="railCollapsed ? '钱包与充值' : undefined">
          <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <path d="M18 20a6 6 0 0 0-12 0" />
            <circle cx="12" cy="10" r="4" />
            <circle cx="12" cy="12" r="10" />
          </svg>
          <span v-if="!railCollapsed">钱包与充值</span>
        </RouterLink>
        <template v-if="isAdmin()">
          <RouterLink class="side-item" to="/providers" :title="railCollapsed ? '上游与模型' : undefined">
            <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <rect width="20" height="8" x="2" y="2" rx="2" ry="2" />
              <rect width="20" height="8" x="2" y="14" rx="2" ry="2" />
              <line x1="6" x2="6.01" y1="6" y2="6" />
              <line x1="6" x2="6.01" y1="18" y2="18" />
            </svg>
            <span v-if="!railCollapsed">上游与模型</span>
          </RouterLink>
          <RouterLink class="side-item" to="/routes" :title="railCollapsed ? '路由' : undefined">
            <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <circle cx="6" cy="19" r="3" />
              <path d="M9 19h8.5a3.5 3.5 0 0 0 0-7h-11a3.5 3.5 0 0 1 0-7H15" />
              <circle cx="18" cy="5" r="3" />
            </svg>
            <span v-if="!railCollapsed">路由</span>
          </RouterLink>
          <!-- 「用户」与「分组」合并（2026-10-10）：分组是用户的属性
               （users.group_id），拆成两个页面得在「这个人在哪个组」与
               「这个组有哪些人」之间来回跳。页内两个 tab 解决。 -->
          <RouterLink class="side-item" to="/users" :title="railCollapsed ? '用户与分组' : undefined">
            <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2" />
              <circle cx="9" cy="7" r="4" />
              <path d="M22 21v-2a4 4 0 0 0-3-3.87" />
              <path d="M16 3.13a4 4 0 0 1 0 7.75" />
            </svg>
            <span v-if="!railCollapsed">用户与分组</span>
          </RouterLink>
          <RouterLink class="side-item" to="/history" :title="railCollapsed ? '调用历史' : undefined">
            <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8" />
              <path d="M3 3v5h5" />
              <path d="M12 7v5l4 2" />
            </svg>
            <span v-if="!railCollapsed">调用历史</span>
          </RouterLink>
          <RouterLink class="side-item" to="/settings" :title="railCollapsed ? '设置' : undefined">
            <svg viewBox="0 0 24 24" width="17" height="17" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <path d="M12.22 2h-.44a2 2 0 0 0-2 2v.18a2 2 0 0 1-1 1.73l-.43.25a2 2 0 0 1-2 0l-.15-.08a2 2 0 0 0-2.73.73l-.22.38a2 2 0 0 0 .73 2.73l.15.1a2 2 0 0 1 1 1.72v.51a2 2 0 0 1-1 1.74l-.15.09a2 2 0 0 0-.73 2.73l.22.38a2 2 0 0 0 2.73.73l.15-.08a2 2 0 0 1 2 0l.43.25a2 2 0 0 1 1 1.73V20a2 2 0 0 0 2 2h.44a2 2 0 0 0 2-2v-.18a2 2 0 0 1 1-1.73l.43-.25a2 2 0 0 1 2 0l.15.08a2 2 0 0 0 2.73-.73l.22-.39a2 2 0 0 0-.73-2.73l-.15-.08a2 2 0 0 1-1-1.74v-.5a2 2 0 0 1 1-1.74l.15-.09a2 2 0 0 0 .73-2.73l-.22-.38a2 2 0 0 0-2.73-.73l-.15.08a2 2 0 0 1-2 0l-.43-.25a2 2 0 0 1-1-1.73V4a2 2 0 0 0-2-2z" />
              <circle cx="12" cy="12" r="3" />
            </svg>
            <span v-if="!railCollapsed">设置</span>
          </RouterLink>
        </template>
      </nav>

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

