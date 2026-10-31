import { Skeleton } from '@heroui/react'

/**
 * 骨架占位基座（P6 占位先行）：统一圆角与动画节奏，页面结构先渲染、
 * 数据到了再填。className 给尺寸（h-4 w-24 等），加载完成后由调用方
 * 条件渲染替换为真实内容——骨架尺寸要与真实内容一致，避免加载完跳动。
 */
export function SkeletonBlock({ className = '' }: { className?: string }) {
  return <Skeleton className={`rounded-md bg-default-soft ${className}`} aria-hidden="true" />
}
