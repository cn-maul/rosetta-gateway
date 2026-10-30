import { useCallback, useRef, useState } from 'react'
import { api } from './api'
import type { Network } from './types'

/** 直连的保留 id。它不是一条 networks 记录（见后端 store.NetworkDirectID）。 */
export const NETWORK_DIRECT_ID = 'direct'

/**
 * 出站网络下拉的共享数据源（语义对齐旧 useNetworks.vue composable）。
 *
 * 「新建上游」与「编辑上游」两个表单都要同一份下拉；React 版各路由各自
 * 挂一份 state，但读取口径保持一致：进入页面和打开表单时各查一次远端，
 * 刻意不做跨页面永久缓存——网络可以在别的标签页或会话里新增，缓存住
 * 首份快照会让下拉永久停在旧列表上。
 */
export function useNetworks() {
  const [networks, setNetworks] = useState<Network[]>([])
  const [loading, setLoading] = useState(false)
  const seqRef = useRef(0)

  /**
   * 重新读取网络清单。返回错误文案（下拉场景不该被清单读取失败阻塞，
   * 退化成「只有直连」即可）；loadSeq 防并发乱序。
   */
  const load = useCallback(async (): Promise<string | null> => {
    const seq = ++seqRef.current
    setLoading(true)
    try {
      const result = await api.networks()
      if (seq === seqRef.current) setNetworks(result)
      return null
    } catch (e) {
      if (seq === seqRef.current) setNetworks([])
      return e instanceof Error ? e.message : String(e)
    } finally {
      if (seq === seqRef.current) setLoading(false)
    }
  }, [])

  return { networks, loading, load }
}
