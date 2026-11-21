import { Button } from '@heroui/react'
import { AppModal } from '../../ui/AppModal'
import { CC_APPS, magpieIcon } from './importTargets'
import type { AccessKey } from '../../types'
import type { CcSwitchApp } from '../../importLinks'

/**
 * 客户端导入弹窗：CC Switch 的四个应用 + Magpie。
 *
 * 两种导入的落地方式不同（自定义协议 vs 网页转交），但对用户来说是同一个
 * 动作「把这把 key 装进本机客户端」，所以放在同一层选择，而不是一个按钮
 * 只开 CC Switch、Magpie 藏在更多菜单里。
 */
export function CcPickerModal({
  keyRow,
  importingAction,
  importingApp,
  onClose,
  onPick,
  onPickMagpie,
}: {
  keyRow: AccessKey | null
  importingAction: string
  importingApp: CcSwitchApp | null
  onClose: () => void
  onPick: (k: AccessKey, app: CcSwitchApp) => void
  onPickMagpie: (k: AccessKey) => void
}) {
  const magpieBusy = !!keyRow && importingAction === `${keyRow.id}:magpie`
  return (
    <AppModal
      open={keyRow !== null}
      onClose={onClose}
      title="导入客户端"
      maxWidth="520px"
    >
      <p className="text-muted mb-3.5 text-[13px] leading-relaxed">
        选择目标客户端，导入后请在客户端内核对地址与密钥。
      </p>
      <div className="flex flex-col gap-2" role="group" aria-label="可导入的客户端">
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
        <Button
          variant="ghost"
          isDisabled={!!importingAction}
          onPress={() => keyRow && onPickMagpie(keyRow)}
          className="border-separator hover:bg-accent-soft hover:border-accent h-auto w-full justify-between border px-3.5 py-2.5"
        >
          <span className="flex min-w-0 items-center gap-3">
            <img src={magpieIcon} alt="" width={34} height={34} className="icon-invert-dark object-contain" />
            <span className="flex min-w-0 flex-col items-start gap-0.5 text-left">
              <span className="text-sm font-semibold">Magpie</span>
              <span className="text-muted text-xs">网页导入 · 自动识别协议</span>
            </span>
          </span>
          <span className="text-muted text-xs">{magpieBusy ? '导入中…' : '→'}</span>
        </Button>
      </div>
      <div className="text-muted mt-3 text-xs leading-relaxed">
        每次只导入一个客户端。Magpie 会另开标签页，浏览器拦截时请允许弹出窗口。
      </div>
    </AppModal>
  )
}
