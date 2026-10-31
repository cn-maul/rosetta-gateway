import { useState } from 'react'
import { toast } from '@heroui/react'
import { api } from '../../api'
import { buildCcSwitchImportLink, buildMagpieImportLink, type CcSwitchApp } from '../../importLinks'
import type { ImportTool } from './importTargets'
import type { AccessKey } from '../../types'

/** 打开导入链接。链接只存在于本次点击和本机应用之间，不写入组件状态或持久化存储。 */
function openImportLink(href: string) {
  const a = document.createElement('a')
  a.href = href
  a.hidden = true
  document.body.appendChild(a)
  a.click()
  a.remove()
}

/**
 * 「把密钥导入本机客户端」的整套动作：取明文 → 拼链接 → 交给应用。
 *
 * 明文只在函数局部变量里流转，绝不进 React 状态：导入链接本身就是一次性的，
 * 留在状态里只会多出一份可被 devtools 读到的副本。
 */
export function useKeyImport({
  modelOptions,
  onCcImported,
}: {
  /** 用于给没设白名单的密钥挑一个「看起来能用」的默认模型。 */
  modelOptions: string[]
  /** CC Switch 导入成功后关闭选择弹窗。 */
  onCcImported?: () => void
}) {
  const [importingAction, setImportingAction] = useState('')
  const [ccImportingApp, setCcImportingApp] = useState<CcSwitchApp | null>(null)

  const importBusy = (k: AccessKey) => importingAction.startsWith(`${k.id}:`)

  /** CC Switch 只能导入到具体应用（没有通用供应商）。模型不是必填，
   * 但空着会让部分应用导入后没有可选模型，所以优先带一把当前可用模型。 */
  const preferredImportModel = (k: AccessKey): string => {
    if (k.allowed_models.length) return k.allowed_models[0]
    return modelOptions[0] ?? ''
  }

  const importKey = async (k: AccessKey, tool: ImportTool, ccApp?: CcSwitchApp) => {
    if (importingAction) return
    // 先把待开启的窗口放到用户手势的同一个调用栈里。Magpie 要先经过自己的
    // usemagpie.ai/import 页面再唤起桌面应用；等拿到密钥后才 window.open 会被
    // 弹窗拦截器当成非用户触发的行为。CC Switch 是自定义协议，不需要预开标签页。
    const magpieWindow = tool === 'magpie' ? window.open('', '_blank') : null
    if (tool === 'magpie' && !magpieWindow) {
      toast.danger('浏览器拦截了导入页，请允许本站打开新窗口后重试')
      return
    }
    setImportingAction(`${k.id}:${tool}`)
    setCcImportingApp(tool === 'ccswitch' ? (ccApp ?? 'codex') : null)
    try {
      const { plaintext_key } = await api.keySecret(k.id)
      const origin = window.location.origin
      if (tool === 'ccswitch') {
        openImportLink(
          buildCcSwitchImportLink({
            origin,
            apiKey: plaintext_key,
            name: k.name,
            app: ccApp ?? 'codex',
            model: preferredImportModel(k),
          }),
        )
        onCcImported?.()
        toast.success('已发起唤起 CC Switch，请在其中核对后确认导入')
      } else {
        const href = buildMagpieImportLink({
          origin,
          apiKey: plaintext_key,
          name: k.name,
          models: k.allowed_models,
        })
        // 空白页只是同一标签页的占位；导航后才把带密钥的链接交给 Magpie 页面。
        // 显式清空 opener，避免导入页反过来操作网关管理界面。
        if (magpieWindow) {
          magpieWindow.opener = null
          magpieWindow.location.replace(href)
        }
        toast.success('已打开 Magpie 导入页，请在其中核对后确认添加')
      }
    } catch (e) {
      magpieWindow?.close()
      if ((e as { status?: number }).status === 401) return
      toast.danger(`导入 ${tool === 'ccswitch' ? 'CC Switch' : 'Magpie'} 失败：` + (e as Error).message)
    } finally {
      setImportingAction('')
      setCcImportingApp(null)
    }
  }

  return { importingAction, ccImportingApp, importBusy, importKey }
}
