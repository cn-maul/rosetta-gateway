<script setup lang="ts">
/**
 * 用量统计（原「调用历史」，2026-10-16 改名并拆成三个子页）。
 *
 * # 为什么改名
 *
 * 原页只有「逐条明细」，名字叫调用历史。现在它长出了汇总维度 ——
 * 「按上游」「按模型」两张表回答的是「钱花在哪儿、哪个上游值得留」，
 * 而明细回答的是「刚才那次为什么报错」。三者是同一份数据的三种读法，
 * 放在一个页面下比三个平级侧栏项更合理：切页签比切页面更快，
 * 而且共用同一个时间范围，用户不会在三个页面各选一次。
 *
 * 老书签 /history **重定向**到本页（见 router.ts），落在「调用明细」
 * 子页 —— 与合并前的落点一致，不给老书签一个「默认落到汇总页」的行为改变。
 *
 * # 时间范围为什么放在父页面
 *
 * 三个子页必须看同一个窗口。各带一个选择器的话，用户在「上游」页选了
 * 30 天、切到「明细」页又看到默认 7 天，两页数字对不上且没有提示 ——
 * 而「上游花了 300 元、明细只有 70 元」这种矛盾会被当成网关算错账。
 */
import { computed, ref } from 'vue'
import { api, isAdmin } from '../api'
import Tabs from '../components/Tabs.vue'
import UsageByGroup from '../components/UsageByGroup.vue'
import UsageDetail from './UsageDetail.vue'

type TabKey = 'detail' | 'providers' | 'models'

/** 页签顺序即阅读顺序：先看明细（排障），再看汇总（决策）。 */
const TABS = [
  { key: 'detail', label: '调用明细' },
  { key: 'providers', label: '上游' },
  { key: 'models', label: '模型' },
] as const

const tab = ref<TabKey>('detail')

// 口径随身份变化，必须写在界面上（沿用原调用历史页的说明）：
// 管理员看到全站，普通用户只看到自己的。后端按会话身份强制收窄
// （usage_handler.scopeUserID），不是前端过滤。
const scopeNote = computed(() => (isAdmin() ? '全站' : '仅你自己'))

const days = ref(7)
const ranges = [
  { label: '近 1 天', value: 1 },
  { label: '近 7 天', value: 7 },
  { label: '近 30 天', value: 30 },
]

const SUB = computed(() => {
  switch (tab.value) {
    case 'providers':
      return `按上游汇总用量、费用与质量 · ${scopeNote.value}`
    case 'models':
      return `按模型汇总用量、费用与质量（归组后的模型按组名合并）· ${scopeNote.value}`
    default:
      return `逐条调用明细 · ${scopeNote.value}`
  }
})

// 上游页按 provider_id 分组。provider_id 是内部 id，界面上要显示的是
// 人能认出的名字 —— 后端 by-provider 目前只下发 id（见 usage_handler 的
// GroupByProvider），所以这里用「有则显示 id」的处理：宁可显示内部 id，
// 也不能留空让用户以为是「没数据」。
function fetchProviders(from: number, to: number) {
  return api.usageByProvider(from, to)
}

function fetchModels(from: number, to: number) {
  return api.usageByModel(from, to)
}
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>用量统计</h1>
        <div class="sub">{{ SUB }}</div>
      </div>
      <div class="head-actions">
        <select v-model.number="days" class="select w-auto">
          <option v-for="r in ranges" :key="r.value" :value="r.value">{{ r.label }}</option>
        </select>
      </div>
    </div>

    <Tabs v-model="tab" :tabs="TABS" aria-label="用量统计 / 调用明细 / 上游 / 模型" />

    <UsageDetail v-if="tab === 'detail'" :days="days" />
    <UsageByGroup
      v-else-if="tab === 'providers'"
      first-col="上游名称"
      kind="provider"
      :fetch="fetchProviders"
      :days="days"
    />
    <UsageByGroup v-else first-col="模型" kind="model" :fetch="fetchModels" :days="days" />
  </main>
</template>

<style scoped>
.w-auto {
  width: auto;
}
</style>
