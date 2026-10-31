import { useEffect, useState } from 'react'
import {
  Button,
  Input,
  Label,
  ListBox,
  ListBoxItem,
  Select,
  SelectIndicator,
  SelectPopover,
  SelectTrigger,
  SelectValue,
  TextArea,
  TextField,
  toast,
} from '@heroui/react'
import { api } from '../../api'
import { NETWORK_DIRECT_ID } from '../../useNetworks'
import { PROTOCOL_OPTIONS } from '../../protocols'
import { AppModal } from '../../ui/AppModal'
import { confirmBox } from '../../ui/confirm'
import type { Network, Provider } from '../../types'

const is401 = (e: unknown): boolean => (e as { status?: number }).status === 401

/**
 * 基础信息编辑弹窗（语义对齐旧 ProviderDetail.vue 的 editOpen 表单）。
 *
 * 表单状态收进弹窗内部：每次打开都从 provider 重新填表，上一次取消时留下
 * 的半截输入不应该在下次打开时还在（那会让人以为「我刚才明明填了」）。
 */
export function ProviderEditModal({
  provider,
  open,
  onClose,
  networks,
  onSaved,
}: {
  provider: Provider
  open: boolean
  onClose: () => void
  networks: Network[]
  onSaved: () => Promise<void>
}) {
  const [name, setName] = useState('')
  const [protocol, setProtocol] = useState('chat')
  const [endpoint, setEndpoint] = useState('')
  const [timeoutSec, setTimeoutSec] = useState('0')
  const [maxRetries, setMaxRetries] = useState('0')
  const [networkId, setNetworkId] = useState(NETWORK_DIRECT_ID)
  const [balanceSpec, setBalanceSpec] = useState('')
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    if (!open) return
    setName(provider.name)
    setProtocol(provider.protocol || 'chat')
    setEndpoint(provider.endpoint)
    setTimeoutSec(String(provider.timeout_ms / 1000))
    setMaxRetries(String(provider.max_retries))
    setNetworkId(provider.network_id || NETWORK_DIRECT_ID)
    setBalanceSpec(provider.balance_spec_json)
  }, [open, provider])

  // dirty 是「保存」按钮的启用条件。表单里存字符串、比较时按提交口径解析，
  // 两边必须同一套换算（秒 ↔ 毫秒、Number() 的 0 兜底）。
  const dirty =
    name !== provider.name ||
    protocol !== (provider.protocol || 'chat') ||
    endpoint !== provider.endpoint ||
    (Number(timeoutSec) || 0) !== provider.timeout_ms / 1000 ||
    (Number(maxRetries) || 0) !== provider.max_retries ||
    networkId !== (provider.network_id || NETWORK_DIRECT_ID) ||
    balanceSpec !== provider.balance_spec_json

  /** 有未保存改动时二次确认。这一页的表单里有 endpoint 与代理 ——
   *  误关掉一个改了一半的代理配置不致命，但「以为存上了、其实没有」才是真的坑。 */
  const closeEdit = async () => {
    if (dirty) {
      const discard = await confirmBox({
        title: '放弃未保存的改动？',
        body: '基础信息有未保存的修改，关闭后这些修改会丢失。',
        confirmLabel: '放弃改动',
      })
      if (!discard) return
    }
    onClose()
  }

  const save = async () => {
    if (!endpoint.trim()) {
      toast.danger('Endpoint 为必填项')
      return
    }
    setSaving(true)
    try {
      await api.updateProvider(provider.id, {
        name: name.trim() || endpoint.trim(),
        protocol,
        endpoint: endpoint.trim(),
        timeout_ms: Math.max(0, Math.round((Number(timeoutSec) || 0) * 1000)),
        max_retries: Number(maxRetries) || 0,
        // 网络与余额查询配置**每次都发**，空串 = 清空。省略字段的语义是
        // 「保持原值」，那会让「换回直连」「删掉自定义余额脚本」这两个动作
        // 永远发不出去 —— 必须显式把空串写回去。
        network_id: networkId,
        balance_spec_json: balanceSpec.trim(),
      })
      toast.success('上游已更新')
      // 关闭弹窗在重载**之后**：重载会同步 provider。顺序反了的话
      // 表单会先按旧值填一次再跳一次。
      await onSaved()
      onClose()
    } catch (e) {
      if (!is401(e)) toast.danger('保存失败：' + (e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <AppModal
      open={open}
      onClose={() => void closeEdit()}
      title="编辑基础信息"
      maxWidth="560px"
      dismissable={false}
      footer={
        <>
          <Button variant="ghost" isDisabled={saving} onPress={() => void closeEdit()}>
            取消
          </Button>
          <Button variant="primary" isDisabled={saving || !dirty} onPress={() => void save()}>
            {saving ? '保存中…' : '保存'}
          </Button>
        </>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void save()
        }}
        className="grid grid-cols-2 gap-4"
      >
        <TextField aria-label="显示名称" value={name} onChange={setName}>
          <Label>显示名称</Label>
          <Input placeholder="DeepSeek" autoComplete="off" />
        </TextField>
        <Select
          aria-label="类型"
          selectedKey={protocol}
          onSelectionChange={(key) => key !== null && setProtocol(String(key))}
        >
          <SelectTrigger>
            <SelectValue />
            <SelectIndicator />
          </SelectTrigger>
          <SelectPopover>
            <ListBox aria-label="协议类型">
              {PROTOCOL_OPTIONS.map((o) => (
                <ListBoxItem key={o.value} id={o.value}>
                  {o.label}
                </ListBoxItem>
              ))}
            </ListBox>
          </SelectPopover>
        </Select>
        <TextField
          aria-label="Endpoint"
          isRequired
          value={endpoint}
          onChange={setEndpoint}
          className="col-span-2"
        >
          <Label>Endpoint *</Label>
          <Input placeholder="https://api.deepseek.com" className="font-mono" autoComplete="off" />
        </TextField>
        <TextField aria-label="超时（秒）" value={timeoutSec} onChange={setTimeoutSec}>
          <Label>超时（秒）</Label>
          <Input type="number" min={0} step={0.1} />
        </TextField>
        <TextField aria-label="最大重试次数" value={maxRetries} onChange={setMaxRetries}>
          <Label>最大重试次数</Label>
          <Input type="number" min={0} max={10} step={1} />
        </TextField>
        <Select
          aria-label="代理"
          className="col-span-2"
          selectedKey={networkId}
          onSelectionChange={(key) => key !== null && setNetworkId(String(key))}
        >
          <SelectTrigger>
            <SelectValue />
            <SelectIndicator />
          </SelectTrigger>
          <SelectPopover>
            <ListBox aria-label="出站网络">
              <ListBoxItem id={NETWORK_DIRECT_ID}>直连（不经过代理）</ListBoxItem>
              {networks.map((n) => (
                <ListBoxItem key={n.id} id={n.id}>
                  {n.name} — {n.proxy_url}
                </ListBoxItem>
              ))}
            </ListBox>
          </SelectPopover>
        </Select>
        <div className="col-span-2 flex flex-col gap-1">
          <Label>余额查询（可选）</Label>
          <TextArea
            aria-label="余额查询（可选）"
            rows={2}
            spellCheck={false}
            className="font-mono"
            placeholder='{"path":"/credits","field":"data.remaining","currency":"USD"}'
            value={balanceSpec}
            onChange={(e) => setBalanceSpec(e.target.value)}
          />
        </div>
      </form>
    </AppModal>
  )
}
