import { Button, toast } from '@heroui/react'
import { Copy } from 'lucide-react'
import { copyText } from '../../fmt'

/**
 * 密钥页顶部的调用地址。三种协议共用同一个 base URL（/v1），所以这里不再放
 * 一个不改变任何结果的协议下拉；协议差异在「导入客户端」里按应用实际配置。
 */

export function EndpointBar({ action }: { action?: React.ReactNode }) {
  const endpoint = window.location.origin + '/v1'

  const copyEndpoint = async () => {
    const ok = await copyText(endpoint)
    if (ok) toast.success('Endpoint 已复制到剪贴板')
    else toast.danger('复制失败，请检查浏览器剪贴板权限')
  }

  return (
    <div className="border-separator bg-background-secondary mb-3.5 flex flex-wrap items-center gap-2 rounded-xl border px-3 py-2">
      <span className="text-muted text-xs">Base URL</span>
      <code className="min-w-0 flex-1 truncate font-mono text-[12.5px]" title={endpoint}>
        {endpoint}
      </code>
      <span title="复制 Endpoint">
        <Button
          isIconOnly
          variant="ghost"
          size="sm"
          aria-label="复制 Base URL"
          onPress={() => void copyEndpoint()}
        >
          <Copy size={16} strokeWidth={1.75} aria-hidden />
        </Button>
      </span>
      {action ? <div className="ml-auto">{action}</div> : null}
    </div>
  )
}
