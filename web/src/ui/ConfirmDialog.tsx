import { useSyncExternalStore } from 'react'
import { AlertDialog, Button } from '@heroui/react'
import { getConfirm, settleConfirm, subscribeConfirm } from './confirm'

// 全局唯一确认框宿主（挂在 AppRoot）。焦点默认落在「取消」上：
// 表单类弹窗第一个控件就是取消，回车连按也不会误触发危险操作。
export function ConfirmDialog() {
  const pending = useSyncExternalStore(subscribeConfirm, getConfirm)
  return (
    <AlertDialog isOpen={pending !== null} onOpenChange={(v) => !v && settleConfirm(false)}>
      <AlertDialog.Backdrop>
        <AlertDialog.Container>
          <AlertDialog.Dialog>
            <AlertDialog.Header>
              <AlertDialog.Heading>{pending?.title ?? ''}</AlertDialog.Heading>
            </AlertDialog.Header>
            {pending?.body ? <AlertDialog.Body>{pending.body}</AlertDialog.Body> : null}
            <AlertDialog.Footer>
              <Button variant="tertiary" onPress={() => settleConfirm(false)}>
                取消
              </Button>
              <Button variant={pending?.danger ? 'danger' : 'primary'} onPress={() => settleConfirm(true)}>
                {pending?.confirmLabel ?? '确认'}
              </Button>
            </AlertDialog.Footer>
          </AlertDialog.Dialog>
        </AlertDialog.Container>
      </AlertDialog.Backdrop>
    </AlertDialog>
  )
}
