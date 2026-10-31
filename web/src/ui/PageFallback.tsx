import { SkeletonBlock } from './SkeletonBlock'

/**
 * 路由懒加载的兜底骨架。
 *
 * 页面 chunk 要等 JS 到齐才渲染，没有这个兜底时点导航会看到「内容区一片空白」——
 * 空白的观感是「页面坏了」，骨架的观感是「正在来」。这就是占位先行的全部意义。
 * 形状照着最典型的「标题 + 指标卡 + 表格」页面来，避免加载完成时整体跳动。
 */
export function PageFallback() {
  return (
    <div className="flex flex-col gap-4" role="status" aria-label="页面加载中">
      <SkeletonBlock className="h-8 w-52" />
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {[0, 1, 2, 3].map((i) => (
          <SkeletonBlock key={i} className="h-20 w-full" />
        ))}
      </div>
      <SkeletonBlock className="h-11 w-full" />
      <SkeletonBlock className="h-64 w-full" />
    </div>
  )
}
