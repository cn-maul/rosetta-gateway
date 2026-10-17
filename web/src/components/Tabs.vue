<script setup lang="ts">
/**
 * Tabs —— 按 HeroUI 的 tabs 组件仿写（Vue 3 版）。
 *
 * 上游参考：
 *   packages/react/src/components/tabs/tabs.tsx
 *   packages/styles/components/tabs.css
 *
 * # 与上一版的关键差别
 *
 * 上一版是「每个 tab 自己变色」（.tab.active 改背景）—— 那不是 HeroUI
 * 的做法。HeroUI 的形态是：
 *
 *   - 外层是**胶囊容器**（.tabs__list-container：bg-default + 大圆角）；
 *   - 选中态由一个**独立的滑动指示器**（.tabs__indicator）承担：
 *     bg-segment + shadow-surface，250ms 过渡 translate/width；
 *   - tab 本身始终是透明的，只改文字颜色（muted → segment-foreground）。
 *
 * 差别不是观感细节：滑动指示器需要容器 position:relative、指示器
 * position:absolute 且 z-index 低于文字 —— 一套完全不同的结构。
 * 用「改自己的背景」永远做不出指示器滑动的效果。
 *
 * 本组件因此把指示器做成一个真正的 DOM 元素，按选中项的位置/宽度定位
 * （用 ResizeObserver 跟随尺寸变化，避免写死宽度）。
 *
 * # 状态属性沿用 RAC 约定
 *
 * 选中态用 data-selected="true" 而不是 .active class —— 与上游 CSS 的
 * 选择器写法一致（tab.css 里就是 &[data-selected="true"]）。
 * 这样将来若有更多状态（disabled / hovered / focus-visible），
 * 命名规则是现成的，不用重新发明。
 */
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'

export type TabItem = { key: string; label: string }

const props = withDefaults(
  defineProps<{
    /** 只读数组：各页面的 TABS 都是 `as const` 常量，不该为传参而复制。 */
    tabs: readonly TabItem[]
    modelValue: string
    ariaLabel?: string
  }>(),
  { ariaLabel: '页面分区' },
)

const emit = defineEmits<{ 'update:modelValue': [key: string] }>()

// 指示器定位：记录每个 tab 按钮的位置，指示器据此移动。
const listRef = ref<HTMLElement | null>(null)
const indicator = ref({ left: 0, width: 0 })

function measure() {
  const el = listRef.value
  if (!el) return
  const active = el.querySelector<HTMLElement>('[data-selected="true"]')
  if (!active) {
    indicator.value = { left: 0, width: 0 }
    return
  }
  indicator.value = { left: active.offsetLeft, width: active.offsetWidth }
}

function onTabKey(event: KeyboardEvent, key: string) {
  const index = props.tabs.findIndex(t => t.key === key)
  let next = index
  if (event.key === 'ArrowRight') next = (index + 1) % props.tabs.length
  else if (event.key === 'ArrowLeft') next = (index - 1 + props.tabs.length) % props.tabs.length
  else if (event.key === 'Home') next = 0
  else if (event.key === 'End') next = props.tabs.length - 1
  else return
  event.preventDefault()
  const item = props.tabs[next]
  if (!item) return
  emit('update:modelValue', item.key)
  listRef.value?.querySelectorAll<HTMLButtonElement>('[role="tab"]')[next]?.focus()
}

// 选中项变化、列表内容变化、容器尺寸变化都要重新测量。
watch(() => props.modelValue, () => nextTick(measure))
watch(() => props.tabs, () => nextTick(measure))

let ro: ResizeObserver | null = null
function attach() {
  if (typeof ResizeObserver === 'undefined') return
  ro = new ResizeObserver(() => measure())
  if (listRef.value) ro.observe(listRef.value)
}
onBeforeUnmount(() => ro?.disconnect())

// 首次挂载后测量一次（此时 DOM 已就位）。
watch(listRef, (el) => {
  if (!el) return
  nextTick(() => {
    measure()
    attach()
  })
})
</script>

<template>
  <div class="tabs" data-orientation="horizontal">
    <!-- 胶囊容器 -->
    <div class="tabs__list-container">
      <!-- 滑动指示器：绝对定位在容器里，z-index 低于 tab 文字 -->
      <span
        class="tabs__indicator"
        aria-hidden="true"
        :style="{ transform: `translateX(${indicator.left}px)`, width: `${indicator.width}px` }"
      />
      <div ref="listRef" class="tabs__list" role="tablist" :aria-label="props.ariaLabel" data-orientation="horizontal">
        <button
          v-for="t in props.tabs"
          :key="t.key"
          class="tabs__tab"
          type="button"
          role="tab"
          :data-selected="t.key === props.modelValue"
          :aria-selected="t.key === props.modelValue"
          :tabindex="t.key === props.modelValue ? 0 : -1"
          @keydown="onTabKey($event, t.key)"
          @click="emit('update:modelValue', t.key)"
        >
          {{ t.label }}
        </button>
      </div>
    </div>
  </div>
</template>
