<script setup lang="ts">
/**
 * 上游与模型 / 路由 / 模型归组（管理员，一个页面，三个页签）。
 *
 * # 为什么三者放在一起
 *
 * 这三样是同一条**数据面配置链**上的三段，从「上游有什么」一路收敛到
 * 「客户端能用什么名字调到什么」：
 *
 *   上游与模型（provider / upstream_models，物理层：某家的某个 model_id）
 *     → 路由（routes + route_targets，公开名 → 有序上游链）
 *       → 模型归组（model_groups，跨上游同名模型的统计身份）
 *
 * 配置时的实际动作几乎总是横跨其中两处：加一个新模型要「上游与模型」建行、
 * 「路由」起公开名；把三家同款模型并进一组要在「上游与模型」选中行、
 * 去「模型归组」指派。原来它们是三个侧栏项，改一次配置要来回跳三次。
 *
 * 页签顺序**就是链的顺序**（上游 → 路由 → 归组），不是按字母或按重要性 ——
 * 这样「下一步该去哪一页」不用重新想。
 *
 * # 为什么「模型归组」不是第四段
 *
 * 归组是**统计展示层**，不参与路由（见 ModelGroups.vue 与 store.go group_id
 * 列注释）。把它排在路由之后、并在这里写明「不改变转发」，是为了避免
 * 「归组影响线上流量」这个误解 —— 它与前两段的性质不同，位置也刻意分开。
 *
 * # 刷新只重建当前页签
 *
 * 与 AccessControl.vue 同一口径：三个面板的端点互不相同（providers /
 * routes / model-groups），全刷既浪费又会打出一堆本轮用不上的请求。
 */
import { computed, ref } from 'vue'
import Tabs from '../components/Tabs.vue'
import Providers from './Providers.vue'
import Routes from './Routes.vue'
import ModelGroups from './ModelGroups.vue'

type TabKey = 'providers' | 'routes' | 'model-groups'

/** 页签。顺序即上面那条配置链：上游 → 路由 → 归组。 */
const TABS = [
  { key: 'providers', label: '上游与模型' },
  { key: 'routes', label: '路由' },
  { key: 'model-groups', label: '模型归组' },
] as const

const tab = ref<TabKey>('providers')
const tabsRef = ref<{ panelIdFor: (k: string) => string; tabIdFor: (k: string) => string } | null>(null)

/** 每个页签的副标题。 */
const SUB = computed(() => {
  switch (tab.value) {
    case 'providers':
      return '上游服务、凭据与上游模型管理'
    case 'routes':
      return '公开模型名 → 有序上游链，按 position 依次尝试，命中故障自动转移'
    default:
      return '跨上游的同名模型编成一组，调用统计按组名合并（不影响路由与转发）'
  }
})

/** 刷新用的重建计数（见文件头关于「只重建当前面板」的说明）。 */
const bump = ref(0)

const panelKey = computed(() => `${tab.value}:${bump.value}`)

const currentPanel = computed(() => {
  switch (tab.value) {
    case 'routes':
      return Routes
    case 'model-groups':
      return ModelGroups
    default:
      return Providers
  }
})
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h1>上游与模型</h1>
        <div class="sub">{{ SUB }}</div>
      </div>
      <div class="head-actions">
        <button class="btn" @click="bump++">刷新</button>
      </div>
    </div>

    <Tabs ref="tabsRef" v-model="tab" :tabs="TABS" aria-label="上游与模型 / 路由 / 模型归组" />

    <div
      :id="tabsRef?.panelIdFor(tab)"
      role="tabpanel"
      :aria-labelledby="tabsRef?.tabIdFor(tab)"
      tabindex="0"
    >
      <component :is="currentPanel" :key="panelKey" />
    </div>
  </div>
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
  font-size: 17px;
}
.sub {
  color: var(--text-3);
  font-size: 12.5px;
}
</style>
