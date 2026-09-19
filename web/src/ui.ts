import { reactive } from 'vue'

// ---------- Toast ----------

export interface ToastItem {
  id: number
  text: string
  kind: 'ok' | 'err'
}

export const toasts = reactive<ToastItem[]>([])
let toastSeq = 0
let toastTimer: ReturnType<typeof setTimeout> | undefined

export function toast(text: string, kind: 'ok' | 'err' = 'ok') {
  // 单例语义：新 toast 顶替旧的（与旧版 index.html 行为一致）
  toasts.splice(0, toasts.length, { id: ++toastSeq, text, kind })
  if (toastTimer) clearTimeout(toastTimer)
  toastTimer = setTimeout(() => toasts.splice(0, toasts.length), 5200)
}

// ---------- 确认对话框（替代 window.confirm，渲染可控且不冻结截图） ----------

export interface ConfirmState {
  open: boolean
  title: string
  body: string
  danger: boolean
  confirmLabel: string
  resolve: ((v: boolean) => void) | null
}

export const confirmState = reactive<ConfirmState>({
  open: false,
  title: '',
  body: '',
  danger: false,
  confirmLabel: '确认',
  resolve: null,
})

export function confirmBox(opts: {
  title: string
  body?: string
  danger?: boolean
  confirmLabel?: string
}): Promise<boolean> {
  return new Promise((resolve) => {
    confirmState.title = opts.title
    confirmState.body = opts.body ?? ''
    confirmState.danger = opts.danger ?? false
    confirmState.confirmLabel = opts.confirmLabel ?? '确认'
    confirmState.resolve = resolve
    confirmState.open = true
  })
}

export function settleConfirm(v: boolean) {
  confirmState.open = false
  confirmState.resolve?.(v)
  confirmState.resolve = null
}

// ---------- Admin Token（鉴权 modal） ----------

export const authState = reactive({
  needToken: false, // 401 后弹出输入框
  callback: null as ((ok: boolean) => void) | null,
})
