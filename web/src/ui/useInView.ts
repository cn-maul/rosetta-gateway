import { useEffect, useRef, useState } from 'react'

/**
 * 视口可见检测（P3 按需加载）：元素滚进视口才触发重数据的加载
 * （趋势的 byDay、Wallet 的流水表）。不可见一次也不拉；IO 不可用
 * （老浏览器/测试环境）退化为立即可见。
 */
export function useInView<T extends Element>(): [React.RefObject<T | null>, boolean] {
  const ref = useRef<T | null>(null)
  const [inView, setInView] = useState(false)
  useEffect(() => {
    const el = ref.current
    if (!el || inView) return
    if (typeof IntersectionObserver === 'undefined') {
      setInView(true)
      return
    }
    const io = new IntersectionObserver(
      (entries) => {
        if (entries.some((e) => e.isIntersecting)) {
          setInView(true)
          io.disconnect()
        }
      },
      { rootMargin: '120px' },
    )
    io.observe(el)
    return () => io.disconnect()
  }, [inView])
  return [ref, inView]
}
