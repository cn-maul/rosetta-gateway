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

export function AppDrawer({ open, onClose, title, footer, children }: AppDrawerProps) {
  return (
    <Drawer isOpen={open} onOpenChange={(v) => !v && onClose()}>
      <Drawer.Backdrop>
        <Drawer.Content placement="right" className="w-full max-w-[680px]">
          <Drawer.Dialog>
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
