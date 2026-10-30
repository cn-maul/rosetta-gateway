import type { ReactNode } from 'react'
import { Modal } from '@heroui/react'

// 受控弹窗（对齐旧 AppModal 对外形态：open/title/maxWidth/maxHeight/dismissable）。
// 焦点陷阱、背景滚动锁、Esc、多层堆叠全部交给 React Aria ——
// 旧版 145 行手写实现（inert / 焦点陷阱 / 层级递增）已整体删除，不要带回来。
interface AppModalProps {
  open: boolean
  onClose: () => void
  title: string
  maxWidth?: string
  /** 覆盖默认高度的「内容很长」的表单弹窗；仍受 92vh 兜底。 */
  maxHeight?: string
  /** false 时 Esc 与点遮罩都不关闭（危险表单防误触）。 */
  dismissable?: boolean
  footer?: ReactNode
  children: ReactNode
}

export function AppModal({
  open,
  onClose,
  title,
  maxWidth = '560px',
  maxHeight,
  dismissable = true,
  footer,
  children,
}: AppModalProps) {
  return (
    <Modal isOpen={open} onOpenChange={(v) => !v && dismissable && onClose()}>
      <Modal.Backdrop isDismissable={dismissable}>
        <Modal.Container>
          <Modal.Dialog
            style={{
              maxWidth,
              ...(maxHeight ? { maxHeight: `min(92vh, ${maxHeight})` } : {}),
            }}
          >
            <Modal.Header>
              <Modal.Heading>{title}</Modal.Heading>
            </Modal.Header>
            <Modal.Body>{children}</Modal.Body>
            {footer ? <Modal.Footer>{footer}</Modal.Footer> : null}
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </Modal>
  )
}
