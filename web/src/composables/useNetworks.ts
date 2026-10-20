/**
 * 出站网络下拉的共享状态。
 *
 * # 为什么提成 composable 而不是各页面各写一遍
 *
 * 「新建上游」与「编辑上游」两个表单都要一份同样的下拉，而它们分属两个
 * 路由（列表页的弹窗 / 详情页的弹窗）。各写一遍的代价不是省几行代码，
 * 而是**两个地方会各自漂移**：一个加了「新建网络」入口而另一个没加，
 * 于是「在新建弹窗里能建网络、在编辑弹窗里不能」—— 这正是用户会撞上的
 * 那个不一致。
 */
import { ref } from 'vue'
import { api } from '../api'
import type { Network } from '../types'

/** 直连的保留 id。它不是一条 networks 记录（见后端 store.NetworkDirectID）。 */
export const NETWORK_DIRECT_ID = 'direct'

// 模块级单例：两个表单是同一份数据的两个视图，各自查一次网络清单
// 只会多打两次同样的请求，且两份数据可能不同步。
const networks = ref<Network[]>([])
const loaded = ref(false)
const loading = ref(false)

export function useNetworks() {
  async function load(force = false) {
    if (loading.value) return
    // 增量场景（新建了一个网络之后）必须能刷新，所以留 force；
    // 但已经加载过就不重复请求 —— 网络清单几乎不变，而两个表单各查一次
    // 是纯粹的浪费。
    if (loaded.value && !force) return
    loading.value = true
    try {
      networks.value = await api.networks()
      loaded.value = true
    } catch {
      // 读失败不抛：下拉退化成「只有直连」，用户仍能建上游 ——
      // 而一个读不到网络清单就让「新建上游」整体不可用，是把一个次要
      // 功能失败升级成主流程阻塞。
      networks.value = []
    } finally {
      loading.value = false
    }
  }

  async function reload() {
    loaded.value = false
    await load(true)
  }

  return { networks, loading, load, reload, NETWORK_DIRECT_ID }
}
