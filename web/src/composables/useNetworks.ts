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
const loading = ref(false)
let loadSeq = 0

export function useNetworks() {
  /**
   * 重新读取网络清单。
   *
   * 这里刻意**不做跨页面永久缓存**：网络可以在「出站网络」页、另一个
   * 浏览器标签页或别的管理员会话里新增，缓存住首份快照会让上游表单的
   * 下拉永久停在旧列表上。清单只有几行，进入页面和打开表单时各查一次
   * 远比拿到一份过期选项便宜。
   *
   * loadSeq 只负责防止并发请求乱序：先发的旧请求晚返回时不能覆盖后发的
   * 新结果。
   */
  async function load(): Promise<string | null> {
    const seq = ++loadSeq
    loading.value = true
    try {
      const result = await api.networks()
      if (seq === loadSeq) networks.value = result
      return null
    } catch (e) {
      // 下拉场景不该被清单读取失败阻塞（退化成「只有直连」即可）；但管理页
      // 需要把错误显示出来。统一返回错误文案，由调用方决定是否呈现。
      if (seq === loadSeq) networks.value = []
      return e instanceof Error ? e.message : String(e)
    } finally {
      if (seq === loadSeq) loading.value = false
    }
  }

  return { networks, loading, load, reload: load, NETWORK_DIRECT_ID }
}
