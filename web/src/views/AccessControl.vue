<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { isAdmin } from '../api'
import Tabs from '../components/Tabs.vue'
import UsersGroups from './UsersGroups.vue'
import Keys from './Keys.vue'

type TabKey = 'users' | 'groups' | 'keys'

const route = useRoute()
const router = useRouter()
const TABS = computed<readonly { key: TabKey; label: string }[]>(() =>
  isAdmin() ? [
    { key: 'users', label: '用户' }, { key: 'groups', label: '权限分组' }, { key: 'keys', label: 'API 密钥' },
  ] : [{ key: 'keys', label: 'API 密钥' }],
)
const tab = computed<TabKey>({
  get: () => !isAdmin() || route.name === 'keys' ? 'keys'
    : route.query.tab === 'groups' || route.query.tab === 'keys' ? route.query.tab : 'users',
  set: (key) => { void router.push({ path: '/users', query: { tab: key } }) },
})
/** 每个页签自己的副标题：宿主统一给，避免三处各写一遍标题区。 */
const SUB = computed(() => {
  switch (tab.value) {
    case 'users':
      return '账号、配额、登录状态与模型可见范围'
    case 'groups':
      return '模型白名单按组收窄 —— 空白名单表示不限制'
    default:
      return isAdmin()
        ? '客户端调用 /v1 时使用的 sk-gw- 密钥'
        : '客户端调用 /v1 时使用的 sk-gw- 密钥 · 归属恒为你自己'
  }
})

/** 刷新用的重建计数。递增即让当前面板整体重挂载。 */
const bump = ref(0)

/** panelKey 同时包含页签与重建计数：切页签会换组件，刷新会换同组件的新实例。 */
const panelKey = computed(() => `${tab.value}:${bump.value}`)

/** 当前页签该渲染哪个面板。密钥页签是 Keys，其余（用户/分组）是 UsersGroups。 */
const currentPanel = computed(() => (tab.value === 'keys' ? Keys : UsersGroups))
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>{{ isAdmin() ? '用户与权限' : 'API 密钥' }}</h1>
        <div class="sub">{{ SUB }}</div>
      </div>
      <div class="head-actions">
        <button class="btn" @click="bump++">刷新</button>
      </div>
    </div>

    <Tabs v-if="isAdmin()" v-model="tab" :tabs="TABS" aria-label="用户 / 分组 / 访问密钥" />

    <!--
      v-bind 只在非密钥页签时传 tab：Keys 没有这个 prop，硬传会在
      Vue 的 fallthrough attrs 里冒出一个 tab 属性落到它的根元素上。
    -->
    <component
      :is="currentPanel"
      :key="panelKey"
      v-bind="tab === 'keys' ? {} : { tab }"
    />
  </main>
</template>

<style scoped>
.page-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 14px;
  margin-bottom: 14px;
}
.page-head h1 {
  margin: 0 0 4px;
  font-size: 24px;
}
.sub {
  color: var(--text-3);
  font-size: 12.5px;
}
</style>