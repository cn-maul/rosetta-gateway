import { useEffect, useState } from 'react'
import { Button, Input, Label, TextField, toast } from '@heroui/react'
import { api, ApiFail } from '../../api'
import { AppModal } from '../../ui/AppModal'
import { ModelPicker } from '../ModelPicker'
import type { Group } from '../../types'

/** 新建 / 改名分组。新建成功后刻意提示「此刻不限制」：空白名单 = 不限制，
 * 没人配模型时这个组比不分组还宽，必须当场说破。 */
export function GroupFormModal({
  open,
  editing,
  onClose,
  onSaved,
}: {
  open: boolean
  editing: Group | null
  onClose: () => void
  onSaved: () => void | Promise<void>
}) {
  const [name, setName] = useState('')
  const [desc, setDesc] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (!open) return
    setName(editing?.name ?? '')
    setDesc(editing?.description ?? '')
    setErr('')
  }, [open, editing])

  const submit = async () => {
    if (busy) return
    setErr('')
    if (!name.trim()) {
      setErr('组名必填')
      return
    }
    setBusy(true)
    try {
      if (editing) {
        await api.updateGroup(editing.id, { name: name.trim(), description: desc.trim() })
        toast.success('已保存')
      } else {
        await api.createGroup({ name: name.trim(), description: desc.trim() })
        toast.success('已创建。注意：新组未配模型白名单，此时该组不限制模型可见范围。')
      }
      onClose()
      await onSaved()
    } catch (e) {
      setErr(e instanceof ApiFail ? e.message : '保存失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <AppModal
      open={open}
      onClose={onClose}
      title={editing ? '编辑分组' : '新建分组'}
      maxWidth="460px"
      dismissable={false}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onPress={onClose}>
            取消
          </Button>
          <Button variant="primary" isDisabled={busy} onPress={() => void submit()}>
            {busy ? '保存中…' : '保存'}
          </Button>
        </div>
      }
    >
      <TextField aria-label="组名" isRequired value={name} onChange={setName}>
        <Label>组名 *</Label>
        <Input placeholder="研发 / 外包 / 试用" autoComplete="off" />
      </TextField>
      <TextField aria-label="说明" value={desc} onChange={setDesc}>
        <Label>说明</Label>
        <Input placeholder="可选" autoComplete="off" />
      </TextField>
      {!editing ? (
        <div className="text-warning mt-2 text-[11.5px]">
          新建的组默认<b>不限制</b>模型。建好后请到「配置模型」里勾选允许的模型。
        </div>
      ) : null}
      {err ? (
        <div className="text-danger mt-2 text-[12.5px]" role="alert">
          {err}
        </div>
      ) : null}
    </AppModal>
  )
}

/** 配置模型白名单：勾选状态同样不该被误点遮罩清空。 */
export function GroupModelsModal({
  group,
  modelOptions,
  onClose,
  onSaved,
}: {
  group: Group | null
  modelOptions: string[]
  onClose: () => void
  onSaved: () => void | Promise<void>
}) {
  const [picked, setPicked] = useState<string[]>([])
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (group) {
      setPicked([...group.models])
      setErr('')
    }
  }, [group])

  const submit = async () => {
    const g = group
    if (!g || busy) return
    setErr('')
    setBusy(true)
    try {
      await api.setGroupModels(g.id, [...picked])
      toast.success(picked.length === 0 ? '已清空白名单（该组不再限制模型）' : '白名单已保存')
      onClose()
      await onSaved()
    } catch (e) {
      setErr(e instanceof ApiFail ? e.message : '保存失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <AppModal
      open={group !== null}
      onClose={onClose}
      title={group ? `模型白名单 · ${group.name}` : '模型白名单'}
      maxWidth="560px"
      dismissable={false}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onPress={onClose}>
            取消
          </Button>
          <Button variant="primary" isDisabled={busy} onPress={() => void submit()}>
            {busy ? '保存中…' : `保存（已选 ${picked.length}）`}
          </Button>
        </div>
      }
    >
      {modelOptions.length === 0 ? (
        <div className="text-warning text-[12.5px]">
          还没有任何路由（公开模型名）。请先到「上游与模型 / 路由」里建好。
        </div>
      ) : (
        <ModelPicker value={picked} onChange={setPicked} options={modelOptions} />
      )}
      {err ? (
        <div className="text-danger mt-2 text-[12.5px]" role="alert">
          {err}
        </div>
      ) : null}
    </AppModal>
  )
}
