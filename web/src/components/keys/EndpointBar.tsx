import { useState } from 'react'
import {
  Button,
  ListBox,
  ListBoxItem,
  Select,
  SelectIndicator,
  SelectPopover,
  SelectTrigger,
  SelectValue,
  toast,
} from '@heroui/react'
import { Copy } from 'lucide-react'
import { copyText } from '../../fmt'

/**
 * 密钥页顶部的调用地址：默认 OpenAI Chat，可切到 Responses / Anthropic。
 *
 * 显示的是 **base URL**（`<origin>/v1`），而不是 `/v1/chat/completions`
 * 这类完整请求路径：用户复制它是去填客户端里的「API Base / Endpoint」字段，
 * 各家客户端自己会拼后面的路径；贴完整路径反而要被手工截断。
 *
 * 三种协议的 base URL 有两处同为 `/v1`，只显示 base 时切换协议界面毫无变化
 * —— 于是协议名留在左侧下拉里作为唯一区分信号，右侧地址保持干净。
 */
type ApiFormat = 'openai_chat' | 'openai_responses' | 'anthropic'

const API_FORMATS: { key: ApiFormat; label: string; path: string }[] = [
  { key: 'openai_chat', label: 'OpenAI Chat', path: '/v1' },
  { key: 'openai_responses', label: 'OpenAI Responses', path: '/v1' },
  { key: 'anthropic', label: 'Anthropic', path: '/v1' },
]

export function EndpointBar({ action }: { action?: React.ReactNode }) {
  // 默认 OpenAI Chat 是绝大多数 OpenAI 兼容客户端的接入形态。
  const [format, setFormat] = useState<ApiFormat>('openai_chat')
  const chosen = API_FORMATS.find((f) => f.key === format) ?? API_FORMATS[0]
  const endpoint = window.location.origin + chosen.path

  const copyEndpoint = async () => {
    const ok = await copyText(endpoint)
    if (ok) toast.success('Endpoint 已复制到剪贴板')
    else toast.danger('复制失败，请检查浏览器剪贴板权限')
  }

  return (
    <div className="border-separator bg-background-secondary mb-3.5 flex flex-wrap items-center gap-2 rounded-xl border px-3 py-2">
      <span className="text-muted text-xs">Endpoint</span>
      <Select
        aria-label="Endpoint 协议"
        className="w-auto min-w-[170px]"
        selectedKey={format}
        onSelectionChange={(key) => {
          if (key !== null) setFormat(String(key) as ApiFormat)
        }}
      >
        <SelectTrigger>
          <SelectValue />
          <SelectIndicator />
        </SelectTrigger>
        <SelectPopover>
          <ListBox aria-label="Endpoint 协议">
            {API_FORMATS.map((f) => (
              <ListBoxItem key={f.key} id={f.key}>
                {f.label}
              </ListBoxItem>
            ))}
          </ListBox>
        </SelectPopover>
      </Select>
      <code className="min-w-0 flex-1 truncate font-mono text-[12.5px]" title={endpoint}>
        {endpoint}
      </code>
      <span title="复制 Endpoint">
        <Button
          isIconOnly
          variant="ghost"
          size="sm"
          aria-label="复制 Endpoint"
          onPress={() => void copyEndpoint()}
        >
          <Copy size={16} strokeWidth={1.75} aria-hidden />
        </Button>
      </span>
      {action ? <div className="ml-auto">{action}</div> : null}
    </div>
  )
}
