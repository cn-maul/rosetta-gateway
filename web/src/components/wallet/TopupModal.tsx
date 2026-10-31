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
  TextField,
  toast,
} from '@heroui/react'
import { api, ApiFail } from '../../api'
import { AppModal } from '../../ui/AppModal'
import { fmtBalance } from '../../fmt'
import type { User } from '../../types'

/** 元 → 整数分，四舍五入（与后端 store.YuanToCents 同口径）。
 * 刻意在界面侧定死换算：浮点元直接发给后端，对账会差一分钱。 */
const yuanToCents = (input: string): number | null => {
  const n = Number(input)
  if (!Number.isFinite(n)) return null
  return Math.round(n * 100)
}

interface TopupModalProps {
  open: boolean
  onClose: () => void
  /** 充值成功后由弹窗调用：父级负责刷新余额与流水。 */
  onSaved: () => void | Promise<void>
}

/** 管理员充值弹窗。「相对调整」的语义与「不限额被切成有限额」必须写在
 * 界面上，不能靠后端 400 兜底。用户清单在打开时才拉：它只服务于这一个
 * 弹窗，无条件拉等于每次进钱包页都多一次 /users（那一页不便宜）。 */
export function TopupModal({ open, onClose, onSaved }: TopupModalProps) {
  const [userId, setUserId] = useState('')
  const [amount, setAmount] = useState('')
  const [remark, setRemark] = useState('')
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [targets, setTargets] = useState<User[]>([])

  useEffect(() => {
    if (!open) return
    setErr('')
    setAmount('')
    setRemark('')
    setUserId('')
    setTargets([])
    let stale = false
    api
      .users()
      .then((r) => {
        if (!stale) setTargets(r)
      })
      .catch((e) => {
        if (stale) return
        if (e instanceof ApiFail && e.status === 401) return
        setErr(e instanceof ApiFail ? e.message : '加载用户列表失败')
      })
    return () => {
      stale = true
    }
  }, [open])

  /** 充值候选只列普通用户：后端拒绝给管理员充值，列出来是界面自己造的死路。 */
  const candidates = targets.filter((u) => u.role !== 'admin')
  const target = candidates.find((u) => u.id === userId) ?? null

  const submit = async () => {
    if (busy) return
    setErr('')
    if (!userId) {
      setErr('请选择要充值的用户')
      return
    }
    const cents = yuanToCents(amount)
    if (cents === null || Number.isNaN(cents)) {
      setErr('请输入一个金额')
      return
    }
    if (cents === 0) {
      // 后端也拒 0，但先说清：空输入框提交上来就是 0 分，而「调平」几乎一定是手滑。
      setErr('金额不能为 0')
      return
    }
    setBusy(true)
    try {
      const updated = await api.adjustUserBalance(userId, cents)
      const verb = cents > 0 ? '充值' : '扣减'
      toast.success(
        `${verb}成功，${updated.username} 当前余额 ${fmtBalance(updated.balance_cents, updated.balance_unlimited)}`,
      )
      onClose()
      await onSaved()
    } catch (e) {
      setErr(e instanceof ApiFail ? e.message : '操作失败')
    } finally {
      setBusy(false)
    }
  }

  return (
    <AppModal
      open={open}
      onClose={onClose}
      title="充值"
      maxWidth="460px"
      dismissable={false}
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onPress={onClose}>
            取消
          </Button>
          <Button variant="primary" isDisabled={busy} onPress={() => void submit()}>
            {busy ? '处理中…' : '确定'}
          </Button>
        </div>
      }
    >
      <div>
        <Label>充值给</Label>
        <div className="mt-1.5">
          <Select
            aria-label="充值给"
            selectedKey={userId === '' ? '__pick__' : userId}
            onSelectionChange={(key) => {
              if (key === null) return
              const v = String(key)
              setUserId(v === '__pick__' ? '' : v)
            }}
          >
            <SelectTrigger>
              <SelectValue />
              <SelectIndicator />
            </SelectTrigger>
            <SelectPopover>
              <ListBox aria-label="充值对象">
                <ListBoxItem key="pick" id="__pick__">
                  请选择用户
                </ListBoxItem>
                {candidates.map((u) => (
                  <ListBoxItem key={u.id} id={u.id}>
                    {u.username} — 当前 {fmtBalance(u.balance_cents, u.balance_unlimited)}
                  </ListBoxItem>
                ))}
              </ListBox>
            </SelectPopover>
          </Select>
        </div>
      </div>
      {candidates.length === 0 ? (
        <div className="text-warning mt-2 text-[11.5px]">
          还没有可充值的普通用户。管理员账户不参与余额计费，也不能给自己充值。
        </div>
      ) : null}
      <TextField aria-label="金额（元）" value={amount} onChange={setAmount}>
        <Label>金额（元）</Label>
        <Input type="number" step="0.01" placeholder="例如 100" />
        <span className="text-muted text-[11px]">
          填<strong>正数</strong>充值，填<strong>负数</strong>扣减。这是相对调整：填 100
          是「加 100 元」，不是「把余额设成 100 元」—— 误操作不会清零。
        </span>
      </TextField>
      <TextField aria-label="备注（可选）" value={remark} onChange={setRemark}>
        <Label>备注（可选）</Label>
        <Input placeholder="充值原因、工单号…" autoComplete="off" />
      </TextField>
      {target?.balance_unlimited ? (
        <div className="text-warning mt-2 text-[11.5px]">
          {target.username} 当前<b>不限额</b>。充值会把它切换成有限额 ——
          充值后他只能使用填入的金额。
        </div>
      ) : null}
      {target && (yuanToCents(amount) ?? 0) !== 0 ? (
        <div className="text-muted mt-2 text-[11.5px]">
          调整后约为{' '}
          <b className="text-foreground">{fmtBalance(target.balance_cents + (yuanToCents(amount) ?? 0), false)}</b>
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
