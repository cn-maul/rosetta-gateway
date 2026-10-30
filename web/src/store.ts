// 极简外部可订阅 store（重构方案 D7）。
//
// 取代 Vue 的 reactive()：状态是普通对象，变更走 set()（浅合并），
// 组件用 useSyncExternalStore(subscribe, get) 订阅。刻意不做选择器、
// 中间件、派生缓存 —— 本应用只有两个全局单例（auth/session），
// 多一层抽象就是多一份「框架之外的手写接线」。
export type Store<T extends object> = {
  get: () => T
  set: (patch: Partial<T>) => void
  subscribe: (listener: () => void) => () => void
}

export function createStore<T extends object>(initial: T): Store<T> {
  let state = initial
  const listeners = new Set<() => void>()
  return {
    get: () => state,
    set: (patch) => {
      state = { ...state, ...patch }
      listeners.forEach((f) => f())
    },
    subscribe: (listener) => {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
  }
}
