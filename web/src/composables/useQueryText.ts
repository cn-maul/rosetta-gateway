import { computed, onScopeDispose, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'

/**
 * 筛选状态以 URL 为准，刷新与返回时保留同一份状态。
 *
 * 默认每次 set 立即 `router.replace`；文本搜索传 `debounceMs` 后先在本地
 * 草稿里更新、停止输入再写 URL。原因：搜索框绑定的就是这个 computed，
 * 不防抖时「deepseek」会触发 8 次路由跳转/组件重算，而 replace 虽然不增长
 * 历史栈，仍会让每次击键都跑一遍路由与派生计算。
 *
 * 本地草稿同时监听 URL：浏览器前进/后退或别的控件清理参数时，输入框会
 * 跟着回到 URL 里的值，不会出现「界面显示 A、实际筛选按 B」。
 */
export function useQueryText(
  key: string,
  fallback = '',
  opts: { debounceMs?: number } = {},
) {
  const route = useRoute()
  const router = useRouter()
  const read = () =>
    typeof route.query[key] === 'string' ? route.query[key] as string : fallback
  const local = ref(read())
  const debounceMs = opts.debounceMs ?? 0

  watch(
    () => route.query[key],
    () => {
      const next = read()
      if (next !== local.value) local.value = next
    },
  )

  let timer: ReturnType<typeof setTimeout> | undefined
  const stopTimer = () => {
    if (timer !== undefined) {
      clearTimeout(timer)
      timer = undefined
    }
  }
  const write = (value: string) => {
    void router.replace({
      query: { ...route.query, [key]: value === fallback ? undefined : value },
    })
  }

  onScopeDispose(stopTimer)

  return computed({
    get: () => local.value,
    set: value => {
      local.value = value
      if (debounceMs <= 0) {
        stopTimer()
        write(value)
        return
      }
      stopTimer()
      timer = setTimeout(() => {
        timer = undefined
        write(local.value)
      }, debounceMs)
    },
  })
}
