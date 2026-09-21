<script setup lang="ts">
import { onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { toasts, confirmState, settleConfirm, authState } from './ui'
import { saveToken } from './api'
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
// 三条铁律：
//   1. 绝不「存下来就刷新」。必须先把候选密码送去 /admin/api/auth/verify 验证，
//      通过才落 localStorage。否则密码一错就会被 401 弹回同一个对话框，
//      而该对话框 dismissable=false，用户会被永久困在里面 ——
//      这正是「输入密码后又让输入，一直重复」的成因。
//   2. 校验失败必须显示原因，不能静默刷新。
//   3. 是否「首次设置」由后端 first_setup 决定，不靠前端猜。

const tokenInput = ref('')
const isFirstSetup = ref(false)
const authError = ref('')
const submitting = ref(false)

async function checkStatus() {
  try {
    const res = await fetch('/admin/api/password/check')
    if (!res.ok) return
    const data = await res.json()
    isFirstSetup.value = data.first_setup ?? !data.has_password
  } catch (e) {
    console.error('检查密码状态失败:', e)
  }
}

async function submit() {
  const pwd = tokenInput.value.trim()
  if (!pwd || submitting.value) return

  authError.value = ''
  submitting.value = true
  try {
    if (isFirstSetup.value) await setupPassword(pwd)
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
      res.status === 401 ? '密码错误，请重新输入' : `校验失败（HTTP ${res.status}）`
    return
  }

  saveToken(pwd)
  authState.needToken = false
  tokenInput.value = ''
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
  authState.needToken = false
  tokenInput.value = ''
  window.location.reload()
}

// 对话框「刚打开」时才探测一次状态，避免 401 风暴里反复请求。
let wasNeedToken = false
watch(
  () => authState.needToken,
  (need) => {
    if (need && !wasNeedToken) checkStatus()
    wasNeedToken = need
  },
)

onMounted(checkStatus)

// 版本号：构建时由 vite.config.ts 的 define 注入（单一来源：package.json / go.mod），
// 这里先落到本地常量再交给模板，避免依赖模板内联替换。
const appVersion = __APP_VERSION__
const rosettaVersion = __ROSETTA_VERSION__
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
      <div class="nav-actions"></div>
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
  <AppModal :open="confirmState.open" :title="confirmState.title" max-width="420px">
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

  <!-- 管理员密码：首次设置 / 输入 -->
  <AppModal
    :open="authState.needToken"
    :title="isFirstSetup ? '设置管理密码' : '需要管理员密码'"
    max-width="440px"
    :dismissable="false"
  >
    <div class="sheet-body">
      <template v-if="isFirstSetup">
        首次使用，请设置管理密码（至少 6 位），用于保护管理后台。
      </template>
      <template v-else>请输入管理密码。密码仅保存在本机浏览器中。</template>
    </div>
    <p v-if="authError" class="auth-error" role="alert">{{ authError }}</p>
    <form style="margin-top: 14px" @submit.prevent="submit">
      <div class="field">
        <label>管理密码</label>
        <input
          v-model="tokenInput"
          class="input mono"
          type="password"
          :placeholder="isFirstSetup ? '至少 6 位' : '请输入密码'"
          autocomplete="off"
          autofocus
          @input="authError = ''"
        />
      </div>
      <div class="form-actions">
        <button class="btn btn-primary" type="submit" :disabled="!tokenInput.trim() || submitting">
          {{ submitting ? '验证中…' : isFirstSetup ? '设置密码' : '保存并继续' }}
        </button>
      </div>
    </form>
  </AppModal>
</template>
