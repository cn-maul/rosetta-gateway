import { useEffect, useState } from 'react'
import { Button, Input, Label, ListBox, ListBoxItem, Select, SelectIndicator, SelectPopover, SelectTrigger, SelectValue, TextField, toast } from '@heroui/react'
import { api, isAdmin } from '../../api'
import { AppModal } from '../../ui/AppModal'
import { ModelPicker } from '../ModelPicker'
import { daysLeft, expiresAtFromDays } from './keyUtils'
import type { AccessKey, Group } from '../../types'

/** 新建后返回给父级用于展示明文；父级负责刷新列表。 */
interface KeyCreated {
  name: string
  plaintext_key: string
}

/** 分组覆盖下拉（仅管理员表单里渲染）。空串 = 沿用归属用户所属分组。 */
export function GroupCoverSelect({
  value,
  onChange,
  groupOptions,
}: {
  value: string
  onChange: (v: string) => void
  groupOptions: Group[]
}) {
  return (
    <Select
      aria-label="分组覆盖"
      selectedKey={value === '' ? '__none__' : value}
      onSelectionChange={(key) => {
        if (key === null) return
        const v = String(key)
        onChange(v === '__none__' ? '' : v)
      }}
    >
      <SelectTrigger>
        <SelectValue />
        <SelectIndicator />
      </SelectTrigger>
      <SelectPopover>
        <ListBox aria-label="分组覆盖">
          <ListBoxItem key="none" id="__none__">
            （沿用归属用户所属的分组）
          </ListBoxItem>
          {groupOptions.map((g) => (
            <ListBoxItem key={g.id} id={g.id}>
              {g.name}
              {g.models.length === 0 ? '（未限制模型）' : ''}
            </ListBoxItem>
          ))}
        </ListBox>
      </SelectPopover>
    </Select>
  )
}

interface BaseProps {
  onClose: () => void
  modelOptions: string[]
  modelsRestricted: boolean
  groupOptions: Group[]
}

/** 新建访问密钥。表单填一半别丢，遮罩/Esc 都不关（dismissable=false）。
 * 创建成功后把明文交给父级（onCreated），由父级决定展示与刷新。 */
export function KeyCreateModal({
  open,
  onClose,
  modelOptions,
  modelsRestricted,
  groupOptions,
  meName,
  onCreated,
}: BaseProps & {
  open: boolean
  /** 归属用户显示名（只读字段用）。 */
  meName: string
  onCreated: (created: KeyCreated) => void | Promise<void>
}) {
  const [name, setName] = useState('')
  const [quota, setQuota] = useState('0')
  const [rpm, setRpm] = useState('0')
  const [tpm, setTpm] = useState('0')
  const [models, setModels] = useState<string[]>([])
  const [days, setDays] = useState('0')
  const [ips, setIps] = useState('')
  const [groupId, setGroupId] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (!open) return
    setName('')
    setQuota('0')
    setRpm('0')
    setTpm('0')
    setModels([])
    setDays('0')
    setIps('')
    setGroupId('')
  }, [open])

  const submit = async () => {
    // 在途守卫：连点两次「创建」会真的建出两把同名密钥（后端每次都生成新
    // key，没有幂等键），而第二把的明文会被第二次成功的弹窗顶掉、无法找回。
    if (busy) return
    if (!name.trim()) {
      toast.danger('名称为必填项')
      return
    }
    setBusy(true)
    try {
      const created = await api.createKey({
        name: name.trim(),
        // quota/rpm/tpm 只在管理员时透传：普通用户填了也会被后端封顶/清零，
        // 照发只是让界面显示「设了」而实际没生效 —— 静默失效比直接不显示更糟。
        ...(isAdmin()
          ? {
              quota_tokens: Number(quota) || 0,
              rpm_limit: Number(rpm) || 0,
              tpm_limit: Number(tpm) || 0,
            }
          : {}),
        allowed_models: models,
        expires_at: expiresAtFromDays(Number(days)),
        allowed_ips: ips.trim(),
        // 分组覆盖仍只给管理员：它能指向更宽松的组，是收紧手段不是自助配置。
        group_id: isAdmin() ? groupId : undefined,
      })
      onClose()
      await onCreated(created)
    } catch (e) {
      if ((e as { status?: number }).status !== 401) toast.danger('创建失败：' + (e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  /** 新建表单里给普通用户看的额度说明：显示规则而不是一个会停更的数字
   * （本页拿不到会刷新的配额值；真正的「已用 / 配额」展示在钱包页）。 */
  const quotaHint = '由管理员统一配置，详见「钱包与充值」页'

  return (
    <AppModal
      open={open}
      onClose={onClose}
      title="新建访问密钥"
      maxWidth="500px"
      dismissable={false}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onPress={onClose}>
            取消
          </Button>
          <Button variant="primary" isDisabled={busy} onPress={() => void submit()}>
            {busy ? '创建中…' : '创建'}
          </Button>
        </div>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void submit()
        }}
        className="flex flex-col gap-4"
      >
        <TextField aria-label="名称" isRequired value={name} onChange={setName}>
          <Label>名称 *</Label>
          <Input placeholder="cursor-主力 / 内部测试" autoComplete="off" />
        </TextField>
        {/* 归属不是一个可选字段，界面上不给任何改归属的入口：任何人（含管理员）
            建 key 都只归属自己（后端 Create 强制 owner = 调用者并静默忽略
            user_id），PATCH 载荷里根本没有 user_id。要替别人建，请让对方自行创建。 */}
        <TextField aria-label="归属用户" value={meName} onChange={() => {}}>
          <Label>归属用户</Label>
          <Input disabled />
          <span className="text-muted text-[11px]">密钥始终归属你自己。需要给别人用，请让对方自行创建。</span>
        </TextField>
        <div>
          <span className="mb-1.5 block text-[13px] font-medium">可用模型</span>
          <ModelPicker value={models} onChange={setModels} options={modelOptions} restricted={modelsRestricted} />
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          <TextField aria-label="有效期（天）" value={days} onChange={setDays}>
            <Label>有效期（天）</Label>
            <Input type="number" min={0} step={1} placeholder="0" />
            <span className="text-muted text-[11px]">0 = 永不过期。到期后该密钥请求返回 403。</span>
          </TextField>
          <TextField aria-label="来源 IP 白名单" value={ips} onChange={setIps}>
            <Label>来源 IP 白名单</Label>
            <Input placeholder="留空 = 不限制" autoComplete="off" />
            <span className="text-muted text-[11px]">
              逗号分隔的 CIDR 或单 IP，如 <code>10.0.0.0/8, 203.0.113.7</code>。按直连地址判定，不认{' '}
              <code>X-Forwarded-For</code>。
            </span>
          </TextField>
        </div>
        {isAdmin() ? (
          <div>
            <Label>分组覆盖</Label>
            <div className="mt-1.5">
              <GroupCoverSelect value={groupId} onChange={setGroupId} groupOptions={groupOptions} />
            </div>
            <span className="text-muted mt-1.5 block text-[11px]">
              让这把密钥用另一个组的模型范围，常用于「给某个人的某把密钥单独收紧」。
            </span>
          </div>
        ) : null}
        {isAdmin() ? (
          <div className="grid gap-4 sm:grid-cols-3">
            <TextField aria-label="Token 配额" value={quota} onChange={setQuota}>
              <Label>Token 配额</Label>
              <Input type="number" min={0} step={1} placeholder="0" />
              <span className="text-muted text-[11px]">累计 input+output token 上限，用尽后 /v1 返回 429；0 = 不限</span>
            </TextField>
            <TextField aria-label="RPM 限速" value={rpm} onChange={setRpm}>
              <Label>RPM 限速</Label>
              <Input type="number" min={0} step={1} placeholder="0" />
              <span className="text-muted text-[11px]">每分钟请求数上限；0 = 不限</span>
            </TextField>
            <TextField aria-label="TPM 限速" value={tpm} onChange={setTpm}>
              <Label>TPM 限速</Label>
              <Input type="number" min={0} step={1} placeholder="0" />
              <span className="text-muted text-[11px]">每分钟 token 上限（估算预占+事后校正）；0 = 不限</span>
            </TextField>
          </div>
        ) : (
          <TextField aria-label="Token 配额" value={quotaHint} onChange={() => {}}>
            <Label>Token 配额</Label>
            <Input disabled />
            <span className="text-muted text-[11px]">
              你的密钥额度不能超过管理员为你设置的用户级配额。有效期与 IP 白名单可以自己设 ——
              那只会让你的密钥更受限，不会更宽松。
            </span>
          </TextField>
        )}
      </form>
    </AppModal>
  )
}

/** 编辑密钥：表单填一半别丢，遮罩/Esc 都不关。保存成功后 onSaved → 父级刷新。 */
export function KeyEditModal({
  keyRow: k,
  onClose,
  modelOptions,
  modelsRestricted,
  groupOptions,
  onSaved,
}: BaseProps & {
  keyRow: AccessKey | null
  onSaved: () => void | Promise<void>
}) {
  const [name, setName] = useState('')
  const [quota, setQuota] = useState('0')
  const [rpm, setRpm] = useState('0')
  const [tpm, setTpm] = useState('0')
  const [models, setModels] = useState<string[]>([])
  const [days, setDays] = useState('0')
  // 打开编辑框时的天数快照。保存时只在用户显式改过天数后才发送 expires_at：
  // 总是发送会把「改个名字」变成「把截止时间重锚定到现在」（剩余不足 1 天的 key
  // 被顺延），已过期的 key 会被 0 值静默改写成「永不过期」—— 等于凭空复活。
  // PATCH 语义下「不传 = 保持原值」正是这里需要的。
  const [daysOrig, setDaysOrig] = useState('0')
  // 打开编辑框时这把 key 是否已过期。影响编辑框里的提示文案：
  // 「已过期」与「永不过期」在 daysLeft 里都是 0，不看这个标记就会混为一谈。
  const [keyExpired, setKeyExpired] = useState(false)
  const [ips, setIps] = useState('')
  const [groupId, setGroupId] = useState('')

  useEffect(() => {
    if (!k) return
    setName(k.name)
    setQuota(String(k.quota_tokens ?? 0))
    setRpm(String(k.rpm_limit ?? 0))
    setTpm(String(k.tpm_limit ?? 0))
    setModels([...(k.allowed_models ?? [])])
    const d = String(daysLeft(k.expires_at ?? 0))
    setDays(k.expires_at > 0 ? d : '0')
    setDaysOrig(k.expires_at > 0 ? d : '0')
    setKeyExpired((k.expires_at ?? 0) > 0 && k.expires_at <= Date.now())
    setIps(k.allowed_ips ?? '')
    setGroupId(k.group_id ?? '')
  }, [k])

  const submit = async () => {
    if (!k || !name.trim()) {
      toast.danger('名称为必填项')
      return
    }
    try {
      await api.updateKey(k.id, {
        name: name.trim(),
        // 配额与限速与「新建」同一口径：只有管理员能设/改。普通用户编辑自己的
        // key 时若也带上它们，服务端 guardNoLoosening 会以 403 拒绝任何「放宽」
        // —— 用户只想改个名字，却因为一个自己无权设置的字段被拒，整次保存
        // （含改名）全部丢失。不发这个字段 = 保持原值（PATCH 语义）。
        ...(isAdmin()
          ? {
              quota_tokens: Number(quota) || 0,
              rpm_limit: Number(rpm) || 0,
              tpm_limit: Number(tpm) || 0,
            }
          : {}),
        allowed_models: models,
        // 改过天数才发，保持原值就不带这个字段。
        ...(Number(days) !== Number(daysOrig) ? { expires_at: expiresAtFromDays(Number(days)) } : {}),
        allowed_ips: ips.trim(),
        ...(isAdmin() ? { group_id: groupId } : {}),
      })
      onClose()
      toast.success('密钥已更新')
      await onSaved()
    } catch (e) {
      if ((e as { status?: number }).status !== 401) toast.danger('保存失败：' + (e as Error).message)
    }
  }

  return (
    <AppModal
      open={k !== null}
      onClose={onClose}
      title="编辑密钥"
      maxWidth="500px"
      dismissable={false}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onPress={onClose}>
            取消
          </Button>
          <Button variant="primary" onPress={() => void submit()}>
            保存
          </Button>
        </div>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault()
          void submit()
        }}
        className="flex flex-col gap-4"
      >
        <TextField aria-label="名称" isRequired value={name} onChange={setName}>
          <Label>名称 *</Label>
          <Input autoComplete="off" />
        </TextField>
        <div>
          <span className="mb-1.5 block text-[13px] font-medium">可用模型</span>
          <ModelPicker value={models} onChange={setModels} options={modelOptions} restricted={modelsRestricted} />
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          <TextField aria-label="有效期（天）" value={days} onChange={setDays}>
            <Label>有效期（天）</Label>
            <Input type="number" min={0} step={1} placeholder="0" />
            {keyExpired ? (
              <span className="text-muted text-[11px]">
                该密钥已过期。改动天数会重新设定有效期（从现在起 N 天）；不改动则维持过期状态。
              </span>
            ) : (
              <span className="text-muted text-[11px]">
                0 = 永不过期。改动天数会按「从现在起 N 天」重设截止时间；不改动则保持原值。
              </span>
            )}
          </TextField>
          <TextField aria-label="来源 IP 白名单" value={ips} onChange={setIps}>
            <Label>来源 IP 白名单</Label>
            <Input placeholder="留空 = 不限制" autoComplete="off" />
            <span className="text-muted text-[11px]">逗号分隔的 CIDR 或单 IP。写错会让这把密钥从所有地址都连不上。</span>
          </TextField>
        </div>
        {isAdmin() ? (
          <div>
            <Label>分组覆盖</Label>
            <div className="mt-1.5">
              <GroupCoverSelect value={groupId} onChange={setGroupId} groupOptions={groupOptions} />
            </div>
            <span className="text-muted mt-1.5 block text-[11px]">这把密钥用哪个组的模型范围。</span>
          </div>
        ) : null}
        {isAdmin() ? (
          <div className="grid gap-4 sm:grid-cols-3">
            <TextField aria-label="Token 配额" value={quota} onChange={setQuota}>
              <Label>Token 配额</Label>
              <Input type="number" min={0} step={1} placeholder="0" />
              <span className="text-muted text-[11px]">累计 input+output token 上限，用尽后 /v1 返回 429；0 = 不限</span>
            </TextField>
            <TextField aria-label="RPM 限速" value={rpm} onChange={setRpm}>
              <Label>RPM 限速</Label>
              <Input type="number" min={0} step={1} placeholder="0" />
              <span className="text-muted text-[11px]">每分钟请求数上限；0 = 不限</span>
            </TextField>
            <TextField aria-label="TPM 限速" value={tpm} onChange={setTpm}>
              <Label>TPM 限速</Label>
              <Input type="number" min={0} step={1} placeholder="0" />
              <span className="text-muted text-[11px]">每分钟 token 上限（估算预占+事后校正）；0 = 不限</span>
            </TextField>
          </div>
        ) : null}
      </form>
    </AppModal>
  )
}
