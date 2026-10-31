// 站点品牌（站点名 + LOGO）：登录页与侧栏共用同一份数据。
//
// 为什么要一个 store 而不是各自 fetch：登录页和侧栏不是同时挂载的（登录前只有
// 登录页），但它们读的是同一个后端设置；把它做成 useSyncExternalStore 的三态
// （加载中 / 已取到 / 取值失败回落默认），两处就能共享一次请求，且管理页保存后
// 调 refresh() 能立刻把侧栏品牌刷成新值，不用整页刷新。
//
// 失败语义：GET /site 是公开端点，失败（服务未起、反代 502）时**回落默认品牌**
// 而不是报错 —— 品牌栏不该成为「登录页打不开」的原因。
import { useSyncExternalStore } from 'react'

export interface Branding {
  name: string
  logo: string
}

/** 后端没配置时用的名字。与 vite 里的包名无关：这是给人看的默认值。 */
export const DEFAULT_SITE_NAME = 'Rosetta Gateway'

interface BrandState {
  branding: Branding
  loaded: boolean
}

let state: BrandState = { branding: { name: DEFAULT_SITE_NAME, logo: '' }, loaded: false }
const listeners = new Set<() => void>()
let inflight: Promise<void> | null = null

function emit() {
  for (const fn of listeners) fn()
}

function subscribe(fn: () => void): () => void {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

/**
 * 拉一次站点品牌。并发调用合并成一次请求（登录页挂载 + 侧栏挂载可能同时触发）。
 * force=true 用于管理页保存后强制重取。
 */
export function loadBranding(force = false): Promise<void> {
  if (inflight) return inflight
  if (state.loaded && !force) return Promise.resolve()
  inflight = (async () => {
    try {
      const r = await fetch('/admin/api/site', { credentials: 'same-origin' })
      if (!r.ok) throw new Error(`HTTP ${r.status}`)
      const body = (await r.json()) as { site_name?: string; site_logo?: string }
      state = {
        // 后端存空串表示「没配置」，这里换成默认名：把所有「空串 → 默认值」的
        // 判断收在唯一一处，组件里就不用到处写 `|| DEFAULT_SITE_NAME`。
        branding: {
          name: (body.site_name ?? '').trim() || DEFAULT_SITE_NAME,
          logo: body.site_logo ?? '',
        },
        loaded: true,
      }
      emit()
    } catch {
      // 取不到就保持默认品牌，并把 loaded 置真：否则每次渲染都会重试，
      // 网关没起时登录页会打出成串的失败请求。
      state = { branding: { name: DEFAULT_SITE_NAME, logo: '' }, loaded: true }
      emit()
    } finally {
      inflight = null
    }
  })()
  return inflight
}

/** 管理页保存成功后调用：把新品牌直接写进 store，省一次往返也省闪烁。 */
export function setBranding(next: Branding) {
  state = {
    branding: {
      name: (next.name ?? '').trim() || DEFAULT_SITE_NAME,
      logo: next.logo ?? '',
    },
    loaded: true,
  }
  emit()
}

export function getBranding(): Branding {
  return state.branding
}

export function useBranding(): Branding {
  return useSyncExternalStore(subscribe, getBranding, getBranding)
}
