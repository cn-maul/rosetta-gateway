<script lang="ts">
// 所有 AppModal 实例共用一把页面锁。确认框可能叠在页面弹窗上；引用计数可避免
// 先关闭其中一个时过早恢复背景交互或 body 滚动。
let pageLockCount = 0
let previousBodyOverflow = ''
let appRoot: HTMLElement | null = null
let appRootWasInert = false
const modalStack: symbol[] = []
let nextModalLayer = 90

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

const props = withDefaults(defineProps<{
  open: boolean
  title: string
  maxWidth?: string
  /**
   * 可选的最大高度，覆盖全局 `.sheet` 的 `min(86vh, 780px)`。
   *
   * 为什么需要它（2026-10-10）：全局那个上限对流式表单够用，但
   * 「新建路由」这类**内容很长的表单**会一直在弹窗内滚动 —— 用户看到的是
   * 「窗口太短、要滚」，而不是「内容本来就长」。给调用方一个显式覆盖，
   * 比让每个长表单自己去改全局样式安全（全局那个值对短弹窗是对的）。
   *
   * 仍然受 `86vh` 兜底：任何尺寸下都不该有弹窗超出屏幕。
   */
  maxHeight?: string
  dismissable?: boolean
  placement?: 'center' | 'right'
}>(), { dismissable: true })

const emit = defineEmits<{ close: [] }>()

/** 挂载弹窗内容的容器：打开时把焦点放进第一个可聚焦控件，
 *  键盘用户不必从页面头部一路 Tab 摸进弹窗。 */
const sheet = ref<HTMLElement | null>(null)
const modalLayer = ref(90)
let active = false
let previouslyFocused: HTMLElement | null = null

// 每个实例的标识存入模块共享栈；激活顺序同时决定视觉层级和键盘归属。
const modalId = Symbol('app-modal')

function activateModal() {
  if (active) return
  active = true
  modalStack.push(modalId)
  modalLayer.value = ++nextModalLayer
  previouslyFocused = document.activeElement instanceof HTMLElement ? document.activeElement : null
  lockBackground()
  window.addEventListener('keydown', onKeydown)
  nextTick(() => {
    // 顺序即语义：表单类弹窗第一个控件就是输入框，确认类第一个是「取消」
    // —— 把默认焦点放在取消上，回车连按也不会误触发危险操作。
    const first = focusableNodes()[0]
    if (active && modalStack.at(-1) === modalId) (first ?? sheet.value)?.focus()
  })
}

function deactivateModal() {
  if (!active) return
  active = false
  window.removeEventListener('keydown', onKeydown)
  // 移除当前实例后，键盘控制自然交还给剩余的栈顶弹窗。
  const index = modalStack.indexOf(modalId)
  if (index >= 0) modalStack.splice(index, 1)
  unlockBackground()

  const target = previouslyFocused
  previouslyFocused = null
  nextTick(() => {
    // 弹窗关闭后把键盘用户送回触发点；若另一个弹窗仍使它 inert，则不强行聚焦。
    if (target?.isConnected && !target?.closest('[inert]')) target.focus()
  })
}

function focusableNodes(): HTMLElement[] {
  return Array.from(sheet.value?.querySelectorAll<HTMLElement>(
    'a[href], button, input, select, textarea, [tabindex]',
  ) ?? []).filter(node => !node.matches(':disabled, [tabindex="-1"]') && node.getClientRects().length > 0)
}

function onKeydown(e: KeyboardEvent) {
  // 非栈顶弹窗不处理任何按键（焦点与 Esc 归当前可见的那一个）。
  if (modalStack.at(-1) !== modalId) return
  // Esc 与「点遮罩」同一资格：dismissable=false 的表单弹窗两者都不响应，
  // 关闭只能走弹窗内自己的取消 / 确定按钮。
  // 监听挂在 window 而不是 scrim 元素：焦点尚未进入弹窗时 Esc 也要能用。
  if (e.key === 'Escape' && props.dismissable !== false) {
    emit('close')
    return
  }
  // 焦点陷阱：Tab / Shift+Tab 只在弹窗内循环，不许跑到弹窗背后的页面上去。
  if (e.key !== 'Tab') return
  const nodes = focusableNodes()
  if (nodes.length === 0) { e.preventDefault(); sheet.value?.focus(); return }
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
    <div v-if="open" class="scrim" :style="{ zIndex: modalLayer }" :class="{ 'scrim-drawer': placement === 'right' }" @click.self="dismissable !== false && emit('close')">
      <div
        ref="sheet"
        class="sheet"
        :style="{
          maxWidth: maxWidth ?? '560px',
          // 未传时保持全局的 min(86vh, 780px) —— 由 CSS 决定，这里不写死，
          // 免得两处各有一个「默认高度」日后漂移。
          ...(maxHeight ? { maxHeight: `min(92vh, ${maxHeight})` } : {}),
        }"
        tabindex="-1"
        role="dialog"
        aria-modal="true"
        :aria-label="title"
      >
        <div class="sheet-title">{{ title }}<button v-if="placement === 'right' && dismissable !== false" type="button" class="btn btn-ghost btn-sm drawer-close" aria-label="关闭详情" @click="emit('close')">✕</button></div>
        <div v-if="placement === 'right'" class="drawer-body"><slot /></div>
        <slot v-else />
        <div v-if="placement === 'right' && $slots.footer" class="drawer-footer"><slot name="footer" /></div>
      </div>
    </div>
  </Teleport>
</template>
