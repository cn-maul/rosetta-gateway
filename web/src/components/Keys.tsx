import { useEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react'
import { Button, ListBox, ListBoxItem, Select, SelectIndicator, SelectPopover, SelectTrigger, SelectValue, toast } from '@heroui/react'
import { api, isAdmin, session } from '../api'
import { confirmBox } from '../ui/confirm'
import { copyText } from '../fmt'
import { KeysTable } from './keys/KeysTable'
import { KeyCreateModal, KeyEditModal } from './keys/KeyFormModals'
import { PlainKeyModal, type PlainKeyBox } from './keys/PlainKeyModal'
import { CcPickerModal } from './keys/CcPickerModal'
import { useKeyImport } from './keys/useKeyImport'
import { ALL_USERS_FILTER, ORPHAN_FILTER } from './keys/keyUtils'
import type { AccessKey, Group, User } from '../types'

export function Keys() {
  const me = useSyncExternalStore(session.subscribe, session.get)
  const meName = me.me?.username ?? ''

  const [loading, setLoading] = useState(true)
  const [err, setErr] = useState('')
  const [keys, setKeys] = useState<AccessKey[]>([])
  // key 级模型白名单的可选项。惰性加载：多数访问者只是来看一眼列表。
  const [modelOptions, setModelOptions] = useState<string[]>([])
  const [modelsRestricted, setModelsRestricted] = useState(false)
  const modelsLoaded = useRef(false)
  // 分组下拉（key 级分组覆盖，仅管理员）。惰性加载。
  const [groupOptions, setGroupOptions] = useState<Group[]>([])
  // 归属用户筛选（仅管理员）。userOptions 同时承担「把 user_id 翻译成用户名」
  // 的职责，所以这里不过滤管理员；过滤只发生在下拉选项那一层。
  const [userOptions, setUserOptions] = useState<User[]>([])
  const usersLoaded = useRef(false)
  const [fUser, setFUser] = useState('')

  // ---------- 弹窗与行内动作 ----------
  const [cOpen, setCOpen] = useState(false)
  const [editKey, setEditKey] = useState<AccessKey | null>(null)
  const [plainKeyBox, setPlainKeyBox] = useState<PlainKeyBox>({ open: false, key: '', name: '' })
  const [copyingId, setCopyingId] = useState('')
  const [ccPickerKey, setCcPickerKey] = useState<AccessKey | null>(null)

  // 在途集合：同一行的开关在请求返回前不接受第二次点击。
  const [toggling, setToggling] = useState<Set<string>>(new Set())

  const { importingAction, ccImportingApp, importBusy, importKey } = useKeyImport({
    modelOptions,
    onCcImported: () => setCcPickerKey(null),
  })

  const ensureModelOptions = async () => {
    if (modelsLoaded.current) return
    try {
      const r = await api.modelNames()
      setModelOptions(r.models)
      setModelsRestricted(r.restricted)
      modelsLoaded.current = true
    } catch {
      // 拉不到就把选择器留空并说明原因，不阻塞建 key ——
      // 白名单是可选的收紧手段，不该成为建 key 的前置条件。
      setModelOptions([])
    }
  }

  /** 失败不阻塞列表：筛选是可选的收窄手段，拉不到就让下拉只剩「全部用户」。
   * 这里不过滤管理员：userOptions 还负责把 user_id 翻译成用户名，过滤掉会让
   * 管理员的 key 显示成「用户已不存在」—— 那是假的。 */
  const ensureUserOptions = async () => {
    if (usersLoaded.current || !isAdmin()) return
    try {
      setUserOptions(await api.users())
      usersLoaded.current = true
    } catch {
      /* 拉不到就只留「全部用户」，不阻塞列表与其它操作 */
    }
  }

  const ensureGroupOptions = async () => {
    if (!isAdmin() || groupOptions.length > 0) return
    try {
      setGroupOptions(await api.groups())
    } catch {
      /* 拉不到就不显示这个下拉，不阻塞其它配置 */
    }
  }

  const load = async () => {
    setLoading(true)
    setErr('')
    try {
      setKeys(await api.keys())
    } catch (e) {
      if ((e as { status?: number }).status === 401) return
      // 失败必须留下可见的错误态：只弹 toast 的话，页面呈现成「暂无数据」，
      // 运维会据此判断「今天没有流量」，进而排除掉网关/上游故障这个真正方向。
      const msg = (e as Error).message
      setErr('加载密钥失败：' + msg)
      toast.danger('加载失败：' + msg)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
    // 导入链接里的 model 只在本地已有清单里选，不在点击后临时等一个请求：
    // 自定义协议必须尽量保持同一次用户手势内触发，多一个 await 就多一次被浏览器拦下的机会。
    void ensureModelOptions()
    // 管理员进页面就把筛选用的用户清单拉好：第一次点开下拉时若还没加载，
    // 选项会是空的「只有全部用户」，看起来像「没有别的用户」。
    void ensureUserOptions()
  }, [])

  // ---------- 按用户筛选 + 按用量降序 ----------
  // 两个维度互相独立：筛选决定「看谁的行」，排序决定「行怎么排」。
  // 用户常在同一次排查里同时用两个 —— 先筛出某个人的全部 key，再找最耗的那把。

  /** 列表里是否存在无归属的 key。没有它就不给「无归属」选项 ——
   * 一个永远筛不出东西的选项比没有更糟。判据读的是未筛选的 keys。 */
  const hasOrphanKeys = useMemo(() => keys.some((k) => !k.user_id), [keys])

  /** 下拉选项：清单里有谁就列谁（管理员除外 —— 他不能建 key、不能被认领，
   * 这个选项永远筛不出东西），外加「持有 key 但不在用户清单里」的 id。
   * 后者让被删用户的 key 始终可达：排障时「看不见」与「不存在」是两个结论。 */
  const userFilterOptions = useMemo(() => {
    const known = new Set(userOptions.map((u) => u.id))
    const orphanIds = new Set<string>()
    for (const k of keys) {
      if (!k.user_id) continue
      if (!known.has(k.user_id)) orphanIds.add(k.user_id)
    }
    const rows = [
      ...userOptions.filter((u) => u.role !== 'admin').map((u) => ({ id: u.id, username: u.username })),
      ...[...orphanIds].map((id) => ({ id, username: `${id}（用户已不存在）` })),
    ]
    // 按显示名排：下拉是「找人」用的。localeCompare 显式给 zh-CN：
    // 用户名允许中文，默认 locale 在不同浏览器上顺序不一致。
    return rows.sort((a, b) => a.username.localeCompare(b.username, 'zh-CN'))
  }, [userOptions, keys])

  /** 先按用户筛选，再按用量降序。列表接口没有排序参数，前端排一次即可。
   * 降序的理由是运维读这张表的实际用途：找「谁在烧 token」。
   * 稳定排序：used_tokens 相等时用 created_at、再 id 兜底，否则两把 0 用量的
   * key 在刷新前后互换位置，看起来像「列表自己跳了」。 */
  const visibleKeys = useMemo(() => {
    let list = keys
    if (fUser === ORPHAN_FILTER) list = keys.filter((k) => !k.user_id)
    else if (fUser) list = keys.filter((k) => k.user_id === fUser)
    return list.slice().sort((a, b) => {
      if (b.used_tokens !== a.used_tokens) return b.used_tokens - a.used_tokens
      if (a.created_at !== b.created_at) return a.created_at - b.created_at
      return a.id < b.id ? -1 : a.id > b.id ? 1 : 0
    })
  }, [keys, fUser])

  /** 筛选后的空态与「本来就没有 key」必须是两句话。 */
  const filteredEmpty = keys.length > 0 && visibleKeys.length === 0

  const openCreate = () => {
    setCOpen(true)
    void ensureModelOptions()
    void ensureGroupOptions()
  }

  /** 创建成功：先展示明文，再刷新列表。 */
  const onCreated = async (created: { name: string; plaintext_key: string }) => {
    setPlainKeyBox({ open: true, key: created.plaintext_key, name: created.name })
    await load()
  }

  /** 从列表里再次复制完整密钥。明文只在本函数的局部变量与剪贴板里存在：
   * 不写进组件的响应式状态，避免它随列表一起长期驻留、被 devtools 或
   * 同源脚本读到第二个副本。 */
  const copyExistingKey = async (k: AccessKey) => {
    if (copyingId) return
    setCopyingId(k.id)
    try {
      const { plaintext_key } = await api.keySecret(k.id)
      const ok = await copyText(plaintext_key)
      if (ok) toast.success('完整密钥已复制到剪贴板')
      else toast.danger('读取成功，但复制失败，请检查浏览器剪贴板权限')
    } catch (e) {
      if ((e as { status?: number }).status === 401) return
      toast.danger('复制失败：' + (e as Error).message)
    } finally {
      setCopyingId('')
    }
  }

  /** 关闭明文弹窗，并抹掉明文（前端审计 M5）。明文不应在弹窗关闭后继续驻留：
   * 后端保留了加密副本，用户仍可从列表里再次复制。 */
  const closePlainKeyBox = () => setPlainKeyBox({ open: false, key: '', name: '' })

  useEffect(() => closePlainKeyBox, [])

  const openEdit = (k: AccessKey) => {
    setEditKey(k)
    void ensureModelOptions()
    void ensureGroupOptions()
  }

  const toggleKey = async (k: AccessKey) => {
    // 开关是无幂等键的翻转请求，连点两次会发出 enable 再 disable，
    // 最终状态取决于网络返回顺序 —— 用户看到「已启用」却发现 key 是停用的。
    if (toggling.has(k.id)) return
    setToggling((s) => new Set(s).add(k.id))
    try {
      await api.updateKey(k.id, { enabled: !k.enabled })
      toast.success(!k.enabled ? '已启用' : '已停用')
      await load()
    } catch (e) {
      if ((e as { status?: number }).status !== 401) toast.danger('操作失败：' + (e as Error).message)
    } finally {
      setToggling((s) => {
        const next = new Set(s)
        next.delete(k.id)
        return next
      })
    }
  }

  const removeKey = async (k: AccessKey) => {
    const ok = await confirmBox({
      title: `删除密钥「${k.name}」？`,
      body: '使用该密钥的客户端将立即收到 401，且明文密钥不可恢复。',
      danger: true,
      confirmLabel: '删除',
    })
    if (!ok) return
    try {
      await api.deleteKey(k.id)
      toast.success('已删除')
      await load()
    } catch (e) {
      if ((e as { status?: number }).status !== 401) toast.danger('删除失败：' + (e as Error).message)
    }
  }

  const filterToolbar = isAdmin() ? (
    <div className="mb-3.5 flex flex-wrap items-center gap-2.5">
      <Select
        aria-label="按归属用户筛选"
        className="w-auto min-w-[190px]"
        selectedKey={fUser === '' ? ALL_USERS_FILTER : fUser}
        onSelectionChange={(key) => {
          if (key === null) return
          const v = String(key)
          setFUser(v === ALL_USERS_FILTER ? '' : v)
        }}
      >
        <SelectTrigger>
          <SelectValue />
          <SelectIndicator />
        </SelectTrigger>
        <SelectPopover>
          <ListBox aria-label="归属用户">
            <ListBoxItem key="all" id={ALL_USERS_FILTER}>
              全部用户
            </ListBoxItem>
            {userFilterOptions.map((u) => (
              <ListBoxItem key={u.id} id={u.id}>
                {u.username}
              </ListBoxItem>
            ))}
            {hasOrphanKeys ? (
              <ListBoxItem key="orphan" id={ORPHAN_FILTER}>
                已停用的历史密钥（迁移遗留）
              </ListBoxItem>
            ) : null}
          </ListBox>
        </SelectPopover>
      </Select>
      {fUser ? (
        <Button variant="ghost" size="sm" isDisabled={loading} onPress={() => setFUser('')}>
          重置筛选
        </Button>
      ) : null}
      {/* 排序方向必须始终可见：用户翻两页找不到预期那把 key 时，第一反应是
          「这个列表到底怎么排的」。只在首次渲染时说一次是没用的，他早忘了。 */}
      <span className="text-muted ml-auto text-xs">
        共 {visibleKeys.length} / {keys.length} 把 · 按用量降序
      </span>
    </div>
  ) : null

  return (
    <div>
      {/* 自助发 key 只对普通用户开放。管理员不再铸造数据面凭据
         （后端 denyAdminKeyCreate 对管理员一律 403）；隐藏而非禁用：
         一个点下去必然失败的按钮没有存在价值。 */}
      {!isAdmin() ? (
        <div className="mb-3.5 flex justify-end">
          <Button variant="primary" size="sm" onPress={openCreate}>
            新建密钥
          </Button>
        </div>
      ) : null}
      <KeysTable
        visibleKeys={visibleKeys}
        loading={loading}
        err={err}
        filteredEmpty={filteredEmpty}
        toolbar={filterToolbar}
        emptyAction={
          filteredEmpty ? (
            <Button variant="ghost" size="sm" onPress={() => setFUser('')}>
              清除筛选
            </Button>
          ) : undefined
        }
        toggling={toggling}
        copyingId={copyingId}
        importBusy={importBusy}
        onCopy={(k) => void copyExistingKey(k)}
        onCcPicker={(k) => setCcPickerKey(k)}
        onMagpieImport={(k) => void importKey(k, 'magpie')}
        onEdit={openEdit}
        onDelete={(k) => void removeKey(k)}
        onToggle={(k) => void toggleKey(k)}
      />
      <KeyCreateModal
        open={cOpen}
        onClose={() => setCOpen(false)}
        modelOptions={modelOptions}
        modelsRestricted={modelsRestricted}
        groupOptions={groupOptions}
        meName={meName}
        onCreated={onCreated}
      />
      <KeyEditModal
        keyRow={editKey}
        onClose={() => setEditKey(null)}
        modelOptions={modelOptions}
        modelsRestricted={modelsRestricted}
        groupOptions={groupOptions}
        onSaved={load}
      />
      <PlainKeyModal box={plainKeyBox} onClose={closePlainKeyBox} />
      <CcPickerModal
        keyRow={ccPickerKey}
        importingAction={importingAction}
        importingApp={ccImportingApp}
        onClose={() => setCcPickerKey(null)}
        onPick={(k, app) => void importKey(k, 'ccswitch', app)}
      />
    </div>
  )
}
