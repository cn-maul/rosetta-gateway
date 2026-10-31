import { useEffect, useState } from 'react'
import { Button, Input, Label, TextField, toast } from '@heroui/react'
import { api } from '../../api'
import { AppModal } from '../../ui/AppModal'
import type { Credential } from '../../types'

const is401 = (e: unknown): boolean => (e as { status?: number }).status === 401

/**
 * 凭据创建/更新弹窗（语义对齐旧 ProviderDetail.vue 的 credForm）。
 * editing 为 null 表示新建；更新时 api_key 留空 = 不更换。
 */
export function CredentialFormModal({
  providerId,
  editing,
  open,
  onClose,
  onSaved,
}: {
  providerId: string
  editing: Credential | null
  open: boolean
  onClose: () => void
  onSaved: () => Promise<void>
}) {
  const [label, setLabel] = useState('')
  const [apiKey, setApiKey] = useState('')
  const [saving, setSaving] = useState(false)

  useEffect(() => {
    if (!open) return
    setLabel(editing?.label ?? '')
    setApiKey('')
  }, [open, editing])

  const submit = async () => {
    if (!editing && !apiKey) {
      toast.danger('api_key 为必填项')
      return
    }
    setSaving(true)
    try {
      if (editing) {
        await api.updateCredential(editing.id, {
          label: label.trim(),
          ...(apiKey ? { api_key: apiKey } : {}),
        })
      } else {
        await api.createCredential(providerId, {
          label: label.trim(),
          api_key: apiKey,
        })
      }
      onClose()
      toast.success(editing ? '凭据已更新' : '凭据已创建')
      await onSaved()
    } catch (e) {
      if (!is401(e)) toast.danger('保存失败：' + (e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  return (
    <AppModal
      open={open}
      onClose={onClose}
      title={editing ? '更新凭据' : '添加凭据'}
      maxWidth="480px"
      dismissable={false}
      footer={
        <>
          <Button variant="ghost" isDisabled={saving} onPress={onClose}>
            取消
          </Button>
          <Button variant="primary" isDisabled={saving} onPress={() => void submit()}>
            保存
          </Button>
        </>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void submit()
        }}
        className="flex flex-col gap-4"
      >
        <TextField aria-label="标签" value={label} onChange={setLabel}>
          <Label>标签</Label>
          <Input placeholder="主账号 / 备用池" autoComplete="off" />
        </TextField>
        <TextField
          aria-label={editing ? 'API Key（留空则不更换）' : 'API Key'}
          isRequired={!editing}
          value={apiKey}
          onChange={setApiKey}
        >
          <Label>API Key {editing ? '（留空则不更换）' : '*'}</Label>
          <Input
            type="password"
            placeholder="sk-..."
            className="font-mono"
            autoComplete="new-password"
          />
          <span className="text-muted mt-1 block text-[11px]">
            保存后加密入库，仅显示掩码，不再可见明文
          </span>
        </TextField>
        {!editing ? (
          <p className="text-muted text-[11px] leading-relaxed">
            同一上游同时只启用一把 key：若该上游已有启用的凭据，新建的这把会先以
            <b>停用</b>状态保存 —— 确认可用后在凭据列表里打开它的开关即可切换。
          </p>
        ) : null}
      </form>
    </AppModal>
  )
}
