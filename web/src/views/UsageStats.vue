<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api, isAdmin } from '../api'
import Tabs from '../components/Tabs.vue'
import UsageByGroup from '../components/UsageByGroup.vue'
import UsageDetail from './UsageDetail.vue'
import ModelGroups from './ModelGroups.vue'

const route = useRoute()
const router = useRouter()
// 面板的 id / aria-labelledby 要与 Tabs 发出的 aria-controls 配对，
// 所以要拿到 Tabs 实例（模板 ref）。
const tabsRef = ref<{ panelIdFor: (k: string) => string; tabIdFor: (k: string) => string } | null>(null)
const logs = computed(() => route.name === 'history')
const tabs = computed(() => [
  { key: 'providers', label: '按上游' },
  { key: 'models', label: '按模型' },
  { key: 'keys', label: '按密钥' },
  ...(isAdmin() ? [{ key: 'groups', label: '统计归并' }] : []),
])
const tab = computed({
  get: () => tabs.value.find(t => t.key === route.query.tab)?.key ?? 'providers',
  set: key => { void router.push({ query: { ...route.query, tab: key } }) },
})
const days = computed({
  get: () => [1, 7, 30].includes(Number(route.query.days)) ? Number(route.query.days) : 7,
  set: value => { void router.push({ query: { ...route.query, days: String(value), page: undefined } }) },
})
const scope = computed(() => isAdmin() ? '全站' : '仅你自己')
const title = computed(() => logs.value ? (isAdmin() ? '请求日志' : '我的调用') : '用量与费用')
const subtitle = computed(() => logs.value ? '逐条查看调用结果、耗时与记录费用'
  : tab.value === 'groups' ? '将跨上游的同名模型合并统计；不改变路由、转发或权限'
  : '按维度比较请求量、Tokens、质量与记录费用')
const fetchProviders = (from: number, to: number) => api.usageByProvider(from, to)
const fetchModels = (from: number, to: number) => api.usageByModel(from, to)
const fetchKeys = (from: number, to: number) => api.usageByKey(from, to)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div><h1>{{ title }}</h1><div class="sub">{{ subtitle }} · {{ scope }}</div></div>
      <div v-if="logs || tab !== 'groups'" class="head-actions">
        <label class="sr-only" for="usage-days">时间范围</label>
        <select id="usage-days" v-model.number="days" class="select w-auto">
          <option :value="1">近 1 天</option><option :value="7">近 7 天</option><option :value="30">近 30 天</option>
        </select>
      </div>
    </div>
    <UsageDetail v-if="logs" :days="days" />
    <template v-else>
    <Tabs ref="tabsRef" v-model="tab" :tabs="tabs" aria-label="用量分析维度" />
    <div
      :id="tabsRef?.panelIdFor(tab)"
      role="tabpanel"
      :aria-labelledby="tabsRef?.tabIdFor(tab)"
      tabindex="0"
    >
      <ModelGroups v-if="tab === 'groups' && isAdmin()" />
      <UsageByGroup v-else-if="tab === 'providers'" key="providers" first-col="上游名称" kind="provider" :fetch="fetchProviders" :days="days" />
      <UsageByGroup v-else-if="tab === 'models'" key="models" first-col="模型" kind="model" :fetch="fetchModels" :days="days" />
      <UsageByGroup v-else key="keys" first-col="密钥" kind="key" :fetch="fetchKeys" :days="days" />
    </div>
    </template>
  </div>
</template>

<style scoped>
.w-auto { width: auto; }
</style>
