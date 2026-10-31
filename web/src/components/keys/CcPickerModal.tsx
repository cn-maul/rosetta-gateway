import { Button } from '@heroui/react'
import { AppModal } from '../../ui/AppModal'
import { CC_APPS } from './importTargets'
import type { AccessKey } from '../../types'
import type { CcSwitchApp } from '../../importLinks'

/** CC Switch 客户端选择弹窗。选择后由父级发起导入（要拿明文、开链接）。 */
export function CcPickerModal({
  keyRow,
  importingAction,
  importingApp,
  onClose,
  onPick,
}: {
  keyRow: AccessKey | null
  importingAction: string
  importingApp: CcSwitchApp | null
  onClose: () => void
  onPick: (k: AccessKey, app: CcSwitchApp) => void
}) {
  return (
    <AppModal
      open={keyRow !== null}
      onClose={onClose}
      title="导入 CC Switch"
      maxWidth="520px"
    >
      <p className="text-muted mb-3.5 text-[13px] leading-relaxed">
        选择要添加为供应商的客户端。导入后会打开 CC Switch 让你核对配置。
      </p>
      <div className="flex flex-col gap-2" role="group" aria-label="CC Switch 客户端">
        {CC_APPS.map((app) => (
          <Button
            key={app.id}
            variant="ghost"
            isDisabled={!!importingAction}
            onPress={() => keyRow && onPick(keyRow, app.id)}
            className="border-separator hover:bg-accent-soft hover:border-accent h-auto w-full justify-between border px-3.5 py-2.5"
          >
            <span className="flex min-w-0 items-center gap-3">
              <img src={app.icon} alt="" width={34} height={34} className={`object-contain${app.invertDark ? ' icon-invert-dark' : ''}`} />
              <span className="flex min-w-0 flex-col items-start gap-0.5 text-left">
                <span className="text-sm font-semibold">{app.name}</span>
                <span className="text-muted text-xs">{app.detail}</span>
              </span>
            </span>
            <span className="text-muted text-xs">{importingApp === app.id ? '导入中…' : '→'}</span>
          </Button>
        ))}
      </div>
      <div className="text-muted mt-3 text-xs leading-relaxed">
        每次只导入一个客户端；点击后请以 CC Switch 中显示的地址和密钥为准。
      </div>
    </AppModal>
  )
}
