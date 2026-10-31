import type { ReactNode } from 'react'
import { Drawer } from '@heroui/react'

// 右侧详情抽屉（替代旧 DetailDrawer + AppModal placement=right）。
interface AppDrawerProps {
  open: boolean
  onClose: () => void
  title: string
  footer?: ReactNode
  children: ReactNode
}

/**
 * 宽度必须落在 `Drawer.Dialog`（面板本体）上，不能加在外层 `Drawer.Content` 上。
 *
 * `.drawer__content` 是 `position:fixed; inset:0; display:flex;
 * justify-content:flex-end` 的**全屏 overlay**，它自己不是面板。把
 * `w-full max-w-[680px]` 加在它身上，缩窄的是这个 overlay —— `inset:0` 把它的
 * left 钉在 0，于是 680px 的定位区从屏幕左边缘开始排，里面的面板跟着靠左，
 * 右侧空出一大块，看起来就是「弹窗出现在屏幕中间偏左」（用户实测的现象）。
 *
 * 真正的面板是 `.drawer__dialog`（`position:relative; display:flex`，自身无宽度）。
 * 宽度给它是正确的：overlay 保持全屏，flex-end 把 680px 面板推到右边。
 * 用 style 而非 className，是因为这里要的是一条确定的像素上限，不该受
 * 插槽类名合并顺序影响。
 */
const dialogStyle = { width: '100%', maxWidth: 680 }

export function AppDrawer({ open, onClose, title, footer, children }: AppDrawerProps) {
  return (
    <Drawer isOpen={open} onOpenChange={(v) => !v && onClose()}>
      <Drawer.Backdrop>
        <Drawer.Content placement="right">
          <Drawer.Dialog style={dialogStyle}>
            <Drawer.Header>
              <Drawer.Heading>{title}</Drawer.Heading>
              <Drawer.CloseTrigger aria-label="关闭详情" />
            </Drawer.Header>
            <Drawer.Body>{children}</Drawer.Body>
            {footer ? <Drawer.Footer>{footer}</Drawer.Footer> : null}
          </Drawer.Dialog>
        </Drawer.Content>
      </Drawer.Backdrop>
    </Drawer>
  )
}
