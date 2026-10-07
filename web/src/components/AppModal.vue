<script lang="ts">
// 所有 AppModal 实例共用一把页面锁。确认框可能叠在页面弹窗上；引用计数可避免
// 先关闭其中一个时过早恢复背景交互或 body 滚动。
let pageLockCount = 0
let previousBodyOverflow = ''
let appRoot: HTMLElement | null = null
let appRootWasInert = false

function lockBackground(): void {
  if (pageLockCount === 0) {
    previousBodyOverflow = document.body.style.overflow
    document.body.style.overflow = 'hidden'

    appRoot = document.getElementById('app')
    appRootWasInert = appRoot?.hasAttribute('inert') ?? false
    appRoot?.setAttribute('inert', '')
  }
  pageLockCount += 1
}

function unlockBackground(): void {
  if (pageLockCount === 0) return
  pageLockCount -= 1
  if (pageLockCount !== 0) return

  document.body.style.overflow = previousBodyOverflow
  if (!appRootWasInert) appRoot?.removeAttribute('inert')
  appRoot = null
}
</script>

<script setup lang="ts">
import { nextTick, onUnmounted, ref, watch } from 'vue'

const props = defineProps<{
  open: boolean
  title: string
  maxWidth?: string
  dismissable?: boolean
}>()

const emit = defineEmits<{ close: [] }>()

/** 挂载弹窗内容的容器：打开时把焦点放进第一个可聚焦控件，
 *  键盘用户不必从页面头部一路 Tab 摸进弹窗。 */
const sheet = ref<HTMLElement | null>(null)
let active = false
let previouslyFocused: HTMLElement | null = null

function activateModal() {
  if (active) return
  active = true
  previouslyFocused = document.activeElement instanceof HTMLElement ? document.activeElement : null
  lockBackground()
  window.addEventListener('keydown', onKeydown)
  nextTick(() => {
    // 顺序即语义：表单类弹窗第一个控件就是输入框，确认类第一个是「取消」
    // —— 把默认焦点放在取消上，回车连按也不会误触发危险操作。
    const first = sheet.value?.querySelector<HTMLElement>('input, select, textarea, button')
    first?.focus()
  })
}

function deactivateModal() {
  if (!active) return
  active = false
  window.removeEventListener('keydown', onKeydown)
  unlockBackground()

  const target = previouslyFocused
  previouslyFocused = null
  nextTick(() => {
    // 弹窗关闭后把键盘用户送回触发点；若另一个弹窗仍使它 inert，则不强行聚焦。
    if (target?.isConnected && !target.closest('[inert]')) target.focus()
  })
}

function onKeydown(e: KeyboardEvent) {
  // Esc 与「点遮罩」同一资格：dismissable=false 的表单弹窗两者都不响应，
  // 关闭只能走弹窗内自己的取消 / 确定按钮。
  // 监听挂在 window 而不是 scrim 元素：焦点尚未进入弹窗时 Esc 也要能用。
  if (e.key === 'Escape' && props.dismissable !== false) {
    emit('close')
    return
  }
  // 焦点陷阱：Tab / Shift+Tab 只在弹窗内循环，不许跑到弹窗背后的页面上去。
  if (e.key !== 'Tab') return
  const nodes = sheet.value?.querySelectorAll<HTMLElement>(
    'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
  )
  if (!nodes || nodes.length === 0) return
  const first = nodes[0]
  const last = nodes[nodes.length - 1]
  const active = document.activeElement
  const inside = active instanceof HTMLElement && sheet.value!.contains(active)
  if (e.shiftKey) {
    // 焦点在第一个（或压根不在弹窗内）时 Shift+Tab 回卷到最后一个
    if (!inside || active === first) {
      e.preventDefault()
      last.focus()
    }
  } else if (!inside || active === last) {
    e.preventDefault()
    first.focus()
  }
}

watch(
  () => props.open,
  (open) => {
    if (open) activateModal()
    else deactivateModal()
  },
  { immediate: true },
)

onUnmounted(deactivateModal)
</script>

<template>
  <Teleport to="body">
    <div v-if="open" class="scrim" @click.self="dismissable !== false && emit('close')">
      <div ref="sheet" class="sheet" :style="{ maxWidth: maxWidth ?? '560px' }" role="dialog" aria-modal="true">
        <div class="sheet-title">{{ title }}</div>
        <slot />
      </div>
    </div>
  </Teleport>
</template>
