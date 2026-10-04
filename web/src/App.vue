<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { toasts, confirmState, settleConfirm, authState } from './ui'
import { saveToken, auth } from './api'
import AppModal from './components/AppModal.vue'

const route = useRoute()
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

// ---------- 管理员凭据 ----------
//
// 凭据优先级（唯一事实来源，见 internal/adminauth）：
//   admin_auth.json 里的「用户密码」 > config.json 的 admin_token（或 ADMIN_TOKEN 环境变量）。
//   一旦在「设置」页设置过密码，admin_token 立即失效 —— 两者不是并存，是覆盖。
//
// 三条铁律：
//   1. 绝不「存下来就刷新」。必须先把候选密码送去 /admin/api/auth/verify 验证，
//      通过才落 localStorage。否则密码一错就会被 401 弹回同一个对话框，
//      而该对话框 dismissable=false，用户会被永久困在里面 ——
//      这正是「输入密码后又让输入，一直重复」的成因。
//   2. 校验失败必须显示原因，不能静默刷新。
//   3. 是否「首次设置」由后端 first_setup 决定，不靠前端猜。

const tokenInput = ref('')
const confirmInput = ref('')
const isFirstSetup = ref(false)
// 凭据来源（后端 /password/check 的 source）：
//   "none" | "config_token" | "password_file" | "locked"
// 前端用它把「首次设置 / 令牌登录 / 密码登录」三种形态彻底分开，
// 而不是都渲染成同一个「请输入密码」框。
const authSource = ref('')
// 锁定态：凭据文件存在但不可用（损坏/读不出）。后端此时 has_password 仍为 true
// （否则 password/set 的引导窗口会向所有人敞开），所以**必须优先看 locked**，
// 否则界面会一直让用户去猜一个永远不可能对的密码。
const lockedMessage = ref('')
const authError = ref('')
const submitting = ref(false)

// authMode 是弹窗的形态判定，模板与文案全部由它驱动。
type AuthMode = 'locked' | 'setup' | 'token' | 'login'
const authMode = computed<AuthMode>(() => {
  if (lockedMessage.value) return 'locked'
  if (isFirstSetup.value) return 'setup'
  return authSource.value === 'config_token' ? 'token' : 'login'
})

const authBadge = computed(() => {
  switch (authMode.value) {
    case 'setup':
      return '首次初始化'
    case 'token':
      return '使用配置令牌'
    case 'login':
      return '登录'
    default:
      return '已锁定'
  }
})

const authTitle = computed(() => {
  switch (authMode.value) {
    case 'setup':
      return '设置管理密码'
    case 'token':
      return '使用配置令牌登录'
    case 'login':
      return '登录管理后台'
    default:
      return '管理后台已锁定'
  }
})

async function checkStatus() {
  try {
    const res = await fetch('/admin/api/password/check')
    if (!res.ok) return
    const data = await res.json()
    isFirstSetup.value = data.first_setup ?? !data.has_password
    authSource.value = data.source ?? ''
    lockedMessage.value = data.locked ? data.message || '管理凭据不可用，后台已锁定。' : ''
  } catch (e) {
    console.error('检查密码状态失败:', e)
  }
}

async function submit() {
  if (authMode.value === 'locked') return
  const pwd = tokenInput.value.trim()
  if (!pwd || submitting.value) return

  authError.value = ''
  // 首次设置要求二次确认：这是「创建凭据」而不是「登录」，输错了没有第二个人能救。
  if (authMode.value === 'setup') {
    if (pwd.length < 6) {
      authError.value = '密码长度至少 6 位'
      return
    }
    if (pwd !== confirmInput.value.trim()) {
      authError.value = '两次输入的密码不一致'
      return
    }
  }

  submitting.value = true
  try {
    if (authMode.value === 'setup') await setupPassword(pwd)
    else await login(pwd)
  } finally {
    submitting.value = false
  }
}

// 已有密码：先验证，再保存。
async function login(pwd: string) {
  let res: Response
  try {
    res = await fetch('/admin/api/auth/verify', {
      headers: { Authorization: 'Bearer ' + pwd },
    })
  } catch (e) {
    authError.value = '无法连接网关：' + (e as Error).message
    return
  }

  if (!res.ok) {
    authError.value =
      res.status === 401
        ? authMode.value === 'token'
          ? '配置令牌不正确。请核对 config.json 的 admin_token（或 ADMIN_TOKEN 环境变量）。'
          : '密码错误，请重新输入'
        : `校验失败（HTTP ${res.status}）`
    return
  }

  saveToken(pwd)
  authState.needToken = false
  tokenInput.value = ''
  confirmInput.value = ''
  // 此前所有请求都因 401 失败，页面数据是空的，必须重载才能拿到真实数据。
  window.location.reload()
}

// 首次使用：设置密码。后端写完立即生效（凭据是运行时状态），无需重启。
async function setupPassword(pwd: string) {
  let res: Response
  try {
    res = await fetch('/admin/api/password/set', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password: pwd }),
    })
  } catch (e) {
    authError.value = '无法连接网关：' + (e as Error).message
    return
  }

  const data = await res.json().catch(() => null)
  if (!res.ok) {
    authError.value = data?.error?.message || `设置密码失败（HTTP ${res.status}）`
    return
  }

  saveToken(pwd)
  isFirstSetup.value = false
  authSource.value = 'password_file'
  authState.needToken = false
  tokenInput.value = ''
  confirmInput.value = ''
  window.location.reload()
}

// 对话框「刚打开」时才探测一次状态，避免 401 风暴里反复请求。
// 同时清空输入：上一次的残值（尤其是二次确认框）不该跨会话带过来。
let wasNeedToken = false
watch(
  () => authState.needToken,
  (need) => {
    if (need && !wasNeedToken) {
      tokenInput.value = ''
      confirmInput.value = ''
      authError.value = ''
      checkStatus()
    }
    wasNeedToken = need
  },
)

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

// 退出登录：清掉本地令牌后重载（与登录成功后的 reload 对称）。
// 令牌只存 localStorage、每次请求以 Bearer 携带，网关侧无会话状态，清掉即登出；
// 重载后首个受限请求会 401，自动弹回凭据框。
function logout() {
  saveToken('')
  window.location.reload()
}

onMounted(() => {
  initTheme()
  checkStatus()
})
</script>

<template>
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
          <RouterLink class="tab" to="/providers">上游与模型</RouterLink>
          <RouterLink class="tab" to="/routes">路由</RouterLink>
          <RouterLink class="tab" to="/keys">访问密钥</RouterLink>
          <RouterLink class="tab" to="/history">调用历史</RouterLink>
          <RouterLink class="tab" to="/settings">设置</RouterLink>
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
        <!-- 退出登录：清掉本机保存的管理凭据并重载。密码只存在浏览器 localStorage，
             网关侧无会话，所以清空 token 即登出；重载后首个受保护请求 401 会弹回登录框。 -->
        <button
          v-if="auth.token"
          class="theme-toggle"
          title="退出登录"
          aria-label="退出登录"
          @click="logout"
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

  <RouterView />

  <!-- 页脚版本号（构建时注入，非运行时接口） -->
  <footer class="foot">
    <span>rosetta-gateway v{{ appVersion }}</span>
    <span class="foot-sep" aria-hidden="true">·</span>
    <span>rosetta {{ rosettaVersion }}</span>
  </footer>

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

  <!-- 管理凭据：锁定 / 首次初始化 / 配置令牌登录 / 密码登录 -->
  <AppModal :open="authState.needToken" :title="authTitle" max-width="460px" :dismissable="false">
    <!-- 形态徽章：一眼分清「创建凭据」和「使用已有凭据」 -->
    <div class="auth-mode" :class="`auth-mode--${authMode}`">
      <span class="auth-dot" aria-hidden="true"></span>
      <span>{{ authBadge }}</span>
    </div>

    <div class="sheet-body">
      <p v-if="authMode === 'locked'">{{ lockedMessage }}</p>

      <template v-else-if="authMode === 'setup'">
        <p>这台网关<strong>还没有任何管理凭据</strong>，此刻谁都能打开后台。请立即设置一个管理密码。</p>
        <ul class="auth-facts">
          <li>至少 6 位；经 PBKDF2 派生后存到可执行文件同级的 <code>admin_auth.json</code>。</li>
          <li>
            设置后 <code>config.json</code> 里的 <code>admin_token</code> 会<strong>立即失效</strong>
            —— 用户密码优先，两者不是并存关系。
          </li>
        </ul>
      </template>

      <template v-else-if="authMode === 'token'">
        <p>后台尚未设置密码，当前凭据来自<strong>配置文件</strong>。</p>
        <ul class="auth-facts">
          <li>请输入 <code>config.json</code> 的 <code>admin_token</code>，或环境变量 <code>ADMIN_TOKEN</code> 的值。</li>
          <li>在「设置」页设置管理密码后，这个令牌会立即失效。</li>
        </ul>
      </template>

      <template v-else>
        <p>请输入你为管理后台设置的<strong>管理密码</strong>。</p>
        <p class="auth-dim">密码只保存在本机浏览器（localStorage），不会写到网关或别处。</p>
      </template>
    </div>

    <p v-if="authError" class="auth-error" role="alert">{{ authError }}</p>

    <!-- 锁定态不给输入框：凭据文件已损坏，输什么都没用，只会让用户怀疑是自己记错了密码。 -->
    <form v-if="authMode !== 'locked'" style="margin-top: 14px" @submit.prevent="submit">
      <div class="field">
        <label>{{ authMode === 'setup' ? '新管理密码' : authMode === 'token' ? '配置令牌' : '管理密码' }}</label>
        <input
          v-model="tokenInput"
          class="input mono"
          type="password"
          :placeholder="authMode === 'setup' ? '至少 6 位' : authMode === 'token' ? 'config.json 中的 admin_token' : '请输入管理密码'"
          autocomplete="off"
          autofocus
          @input="authError = ''"
        />
      </div>

      <!-- 首次设置才要二次确认：这是「创建凭据」，输错了没人能救 -->
      <div v-if="authMode === 'setup'" class="field" style="margin-top: 12px">
        <label>确认密码</label>
        <input
          v-model="confirmInput"
          class="input mono"
          type="password"
          placeholder="再次输入以确认"
          autocomplete="off"
          @input="authError = ''"
        />
      </div>

      <div class="form-actions">
        <button class="btn btn-primary" type="submit" :disabled="!tokenInput.trim() || submitting">
          {{ submitting ? '处理中…' : authMode === 'setup' ? '设置密码并进入' : '登录' }}
        </button>
      </div>
    </form>
  </AppModal>
</template>

<style scoped>
/* 形态徽章：用颜色把「创建凭据」（强调色）与「使用已有凭据」（中性）分开 */
.auth-mode {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  margin-bottom: 12px;
  padding: 3px 10px;
  border-radius: var(--r-pill);
  font-size: 11.5px;
  font-weight: 600;
  letter-spacing: 0.02em;
}
.auth-dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: currentColor;
}
.auth-mode--setup {
  background: color-mix(in srgb, var(--accent) 14%, transparent);
  color: var(--accent);
}
.auth-mode--token {
  background: color-mix(in srgb, var(--heat) 16%, transparent);
  color: var(--heat);
}
.auth-mode--login {
  background: color-mix(in srgb, var(--text) 8%, transparent);
  color: var(--text-2);
}
.auth-mode--locked {
  background: color-mix(in srgb, var(--danger) 14%, transparent);
  color: var(--danger);
}
.auth-facts {
  margin: 8px 0 0;
  padding-left: 18px;
  display: grid;
  gap: 4px;
  font-size: 12.5px;
  color: var(--text-2);
}
.auth-dim {
  margin-top: 8px;
  font-size: 12.5px;
  color: var(--text-3);
}
.auth-mode code,
.auth-facts code {
  font-family: var(--mono);
  font-size: 11.5px;
  padding: 1px 4px;
  border-radius: 4px;
  background: color-mix(in srgb, var(--text) 8%, transparent);
}
</style>
