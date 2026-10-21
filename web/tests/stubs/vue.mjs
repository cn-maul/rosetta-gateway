// `vue` 的最小 stub，仅供 Node 测试 import `src/api.ts` 用。
//
// api.ts 只用 reactive() 造两个普通对象（auth / session），然后读写它们的
// 属性。它**不依赖 Vue 的响应式调度** —— 那些是界面渲染的事。所以这里
// 返回原对象即可：属性读写语义完全一致，而不必把整个 Vue 运行时
// （连带 SFC 编译链）拖进测试进程。
//
// 只实现用到的导出。将来 api.ts 若开始用 ref/computed/watch，这个 stub
// 要跟着补 —— 那时更该考虑的是「api.ts 为什么需要 Vue 的响应式原语」。

export function reactive(obj) {
  return obj
}

export function ref(v) {
  return { value: v }
}

export function computed(fn) {
  return { get value() { return fn() } }
}

export function readonly(obj) {
  return obj
}

export default { reactive, ref, computed, readonly }
