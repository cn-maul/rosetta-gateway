// Promise 式确认框（对齐旧 ui.ts 的 confirmBox 契约，17 个调用点原样保留）。
//
//   const ok = await confirmBox({ title: '删除？', danger: true })
//
// 渲染由 <ConfirmDialog />（全局唯一实例，挂在 AppRoot）承担；
// 这里只维护「当前待确认的请求」。同一时刻只有一个确认框：
// 上一个若还没结算，先按「取消」了结它 —— 直接覆盖 resolve 会把那个
// Promise 永远挂起（表现为「点了删除，界面卡住」）。
export interface ConfirmOptions {
  title: string
  body?: string
  danger?: boolean
  confirmLabel?: string
}

interface Pending extends Required<ConfirmOptions> {
  resolve: (v: boolean) => void
}

let current: Pending | null = null
const listeners = new Set<() => void>()

function notify() {
  listeners.forEach((f) => f())
}

export function confirmBox(opts: ConfirmOptions): Promise<boolean> {
  return new Promise((resolve) => {
    current?.resolve(false)
    current = {
      title: opts.title,
      body: opts.body ?? '',
      danger: opts.danger ?? false,
      confirmLabel: opts.confirmLabel ?? '确认',
      resolve,
    }
    notify()
  })
}

export function settleConfirm(v: boolean) {
  const cur = current
  current = null
  if (cur) {
    cur.resolve(v)
    notify()
  }
}

export function subscribeConfirm(listener: () => void): () => void {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

export function getConfirm(): Pending | null {
  return current
}
