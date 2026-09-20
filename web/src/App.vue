<script setup lang="ts">
import { onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { toasts, confirmState, settleConfirm, authState } from './ui'
import { auth, saveToken } from './api'
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

// token 输入框
const tokenInput = ref('')
function submitToken() {
  saveToken(tokenInput.value.trim())
  authState.needToken = false
  tokenInput.value = ''
  // 重载页面，用新 token 重新拉取数据（hash 路由下停留当前页）
  window.location.reload()
}

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

  <!-- 管理员令牌输入 -->
  <AppModal :open="authState.needToken" title="需要管理员令牌" max-width="440px" :dismissable="false">
    <div class="sheet-body">
      请输入网关配置中的管理员令牌（<span class="mono">admin_token</span>），用于访问管理
      API。令牌保存在本机浏览器中。
    </div>
    <form style="margin-top: 14px" @submit.prevent="submitToken">
      <div class="field">
        <label>管理员令牌</label>
        <input
          v-model="tokenInput"
          class="input mono"
          type="password"
          placeholder="admin token"
          autocomplete="off"
          autofocus
        />
      </div>
      <div class="form-actions">
        <button class="btn btn-primary" type="submit" :disabled="!tokenInput.trim()">保存并继续</button>
      </div>
    </form>
  </AppModal>
</template>
