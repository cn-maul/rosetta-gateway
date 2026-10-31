import { Button } from '@heroui/react'
import { copyText } from '../../fmt'
import { toast } from '@heroui/react'
import { AppModal } from '../../ui/AppModal'

export interface PlainKeyBox {
  open: boolean
  key: string
  name: string
}

/** 创建后的明文展示；之后仍可从列表再次复制。
 * dismissable=false：明文弹窗必须由用户显式关闭，避免误点遮罩后
 * 还得重新发起读取请求。关闭时由父级抹掉明文（前端审计 M5）：
 * 明文不应在弹窗关闭后继续驻留 —— 后端保留了加密副本，用户仍可再复制。 */
export function PlainKeyModal({
  box,
  onClose,
}: {
  box: PlainKeyBox
  onClose: () => void
}) {
  const copy = async () => {
    const ok = await copyText(box.key)
    if (ok) toast.success('已复制到剪贴板')
    else toast.danger('复制失败，请手动选择复制')
  }
  return (
    <AppModal
      open={box.open}
      onClose={onClose}
      title="密钥已创建"
      maxWidth="520px"
      dismissable={false}
      footer={
        <div className="flex justify-end gap-2">
          <Button onPress={() => void copy()}>复制密钥</Button>
          <Button variant="primary" onPress={onClose}>
            我已保存
          </Button>
        </div>
      }
    >
      <p className="text-[13px] leading-relaxed">
        「{box.name}」的完整密钥如下。密钥已加密保存，之后仍可在列表里点「复制」再次获取：
      </p>
      <div className="bg-default-soft mt-3 rounded-lg p-3">
        <div className="font-mono text-[13px] break-all">{box.key}</div>
        <div className="text-warning mt-2 text-xs">请立即复制并妥善保管，不要提交到代码仓库。</div>
      </div>
    </AppModal>
  )
}
