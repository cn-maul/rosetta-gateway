import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'

/** 筛选直接派生自 URL，刷新与返回时保留同一份状态。 */
export function useQueryText(key: string, fallback = '') {
  const route = useRoute()
  const router = useRouter()
  return computed({
    get: () => typeof route.query[key] === 'string' ? route.query[key] as string : fallback,
    set: value => {
      void router.replace({ query: { ...route.query, [key]: value === fallback ? undefined : value } })
    },
  })
}
