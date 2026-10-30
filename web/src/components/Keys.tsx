import { useEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react'
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
  Switch,
  Table,
  TextField,
  toast,
} from '@heroui/react'
import { Copy } from 'lucide-react'
import { api, isAdmin, session } from '../api'
import { confirmBox } from '../ui/confirm'
import { AppModal } from '../ui/AppModal'
import { DataTable } from '../ui/DataTable'
import { StatusChip } from '../ui/StatusChip'
import { fmtDateTime, fmtTokens, copyText } from '../fmt'
import { buildCcSwitchImportLink, buildMagpieImportLink, type CcSwitchApp } from '../importLinks'
import { ModelPicker } from './ModelPicker'
import type { AccessKey, Group, User } from '../types'
import ccSwitchIcon from '../assets/icons/cc-switch.png'
import magpieIcon from '../assets/icons/magpie.svg'
import codexIcon from '../assets/icons/codex.svg'
import claudeIcon from '../assets/icons/claude.svg'
import opencodeIcon from '../assets/icons/opencode.svg'
import hermesIcon from '../assets/icons/hermes.png'

const DAY_MS = 86_400_000

/**
 * 剩余天数 ⇄ 绝对时间戳。
 *
 * 界面用「天数」而不是 datetime-local：后者的值是无时区的本地时间，
 * 存成时间戳要经过一次 implicitly-local 的转换，夏令时/时区偏移下会出现
 * 「设了 30 天，实际少了 3 小时」这类难解释的偏差。天数没有这个问题，
 * 而且对「这张券还有多久」也更直观。
 */
function daysLeft(expiresAt: number): number {
  if (!expiresAt) return 0
  return Math.max(0, Math.ceil((expiresAt - Date.now()) / DAY_MS))
}

function expiresAtFromDays(days: number): number {
  const n = Number(days) || 0
  // 输入框 min=0 只拦得住点选，拦不住手输 -5。负数会算成一个过去的时间戳
  // → 密钥一建就过期；已有期限时还会被服务端判成「放宽限制」而 403，
  // 报错信息与真实原因（输了个负数）完全无关。钳到 0（永不过期）。
  if (n <= 0) return 0
  return Date.now() + n * DAY_MS
}

/** expires_at = 0 是「永不过期」，必须与「刚好过期」区分开：
 * 数据面对前者放行、对后者一律 403，两者混为一谈会把好端端的密钥当成死的。 */
function isExpired(k: AccessKey): boolean {
  return k.expires_at > 0 && k.expires_at <= Date.now()
}

/** 限速拼成一行。两个上限都是 0 时返回空串，由界面自己决定显示「不限」
 * —— 空串和「不限」在语义上不是一回事，不能让一个函数替界面做这个决定。 */
function rateText(k: AccessKey): string {
  const parts: string[] = []
  if ((k.rpm_limit ?? 0) > 0) parts.push(`${k.rpm_limit} req/min`)
  if ((k.tpm_limit ?? 0) > 0) parts.push(`${fmtTokens(k.tpm_limit)} tok/min`)
  return parts.join(' · ')
}

/** 这把 key 是否设了任何一种限制。四种限制全空时必须明说「不限」，
 * 否则「什么都没配」会被读成「没配成功」。 */
function anyLimit(k: AccessKey): boolean {
  return (
    k.allowed_models.length > 0 ||
    !!k.allowed_ips ||
    !!k.group_id ||
    (k.rpm_limit ?? 0) > 0 ||
    (k.tpm_limit ?? 0) > 0
  )
}

/**
 * 无归属 key 的筛选哨兵值。空串是「不过滤」的约定，复用它会让「筛无归属」
 * 和「筛全部」塌成同一个状态；带 \u0000 前缀保证不与任何真实 user_id 相撞。
 */
const ORPHAN_FILTER = '\u0000orphan'
const ALL_USERS_FILTER = '__all__'

/** CC Switch 可导入的客户端清单（自定义协议目标各不相同）。 */
const CC_APPS: { id: CcSwitchApp; name: string; icon: string; detail: string; invertDark?: boolean }[] = [
  { id: 'codex', name: 'Codex', icon: codexIcon, detail: 'OpenAI Responses', invertDark: true },
  { id: 'claude', name: 'Claude Code', icon: claudeIcon, detail: 'Anthropic Messages' },
  { id: 'opencode', name: 'OpenCode', icon: opencodeIcon, detail: 'OpenAI Chat' },
  { id: 'hermes', name: 'Hermes', icon: hermesIcon, detail: 'OpenAI Chat' },
]

type ImportTool = 'ccswitch' | 'magpie'

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

  // ---------- 创建表单 ----------
  const [creating, setCreating] = useState(false)
  const [cOpen, setCOpen] = useState(false)
  const [cName, setCName] = useState('')
  const [cQuota, setCQuota] = useState('0')
  const [cRpm, setCRpm] = useState('0')
  const [cTpm, setCTpm] = useState('0')
  const [cModels, setCModels] = useState<string[]>([])
  const [cDays, setCDays] = useState('0')
  const [cIps, setCIps] = useState('')
  const [cGroupId, setCGroupId] = useState('')

  // ---------- 明文展示 / 复制 / 导入 ----------
  const [plainKeyBox, setPlainKeyBox] = useState<{ open: boolean; key: string; name: string }>({ open: false, key: '', name: '' })
  const [copyingId, setCopyingId] = useState('')
  const [importingAction, setImportingAction] = useState('')
  const [ccImportingApp, setCcImportingApp] = useState<CcSwitchApp | null>(null)
  const [ccPickerKey, setCcPickerKey] = useState<AccessKey | null>(null)

  // ---------- 编辑表单 ----------
  const [eOpen, setEOpen] = useState(false)
  const [eId, setEId] = useState('')
  const [eName, setEName] = useState('')
  const [eQuota, setEQuota] = useState('0')
  const [eRpm, setERpm] = useState('0')
  const [eTpm, setETpm] = useState('0')
  const [eModels, setEModels] = useState<string[]>([])
  const [eDays, setEDays] = useState('0')
  // 打开编辑框时的天数快照。保存时只在用户显式改过天数后才发送 expires_at：
  // 总是发送会把「改个名字」变成「把截止时间重锚定到现在」（剩余不足 1 天的 key
  // 被顺延），已过期的 key 会被 0 值静默改写成「永不过期」—— 等于凭空复活。
  // PATCH 语义下「不传 = 保持原值」正是这里需要的。
  const [eDaysOrig, setEDaysOrig] = useState('0')
  // 打开编辑框时这把 key 是否已过期。影响编辑框里的提示文案：
  // 「已过期」与「永不过期」在 daysLeft 里都是 0，不看这个标记就会混为一谈。
  const [eKeyExpired, setEKeyExpired] = useState(false)
  const [eIps, setEIps] = useState('')
  const [eGroupId, setEGroupId] = useState('')

  // 在途集合：同一行的开关在请求返回前不接受第二次点击。
  const [toggling, setToggling] = useState<Set<string>>(new Set())
  const [recomputing, setRecomputing] = useState('')

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
    setCName('')
    setCQuota('0')
    setCRpm('0')
    setCTpm('0')
    setCModels([])
    setCDays('0')
    setCIps('')
    setCGroupId('')
    setCOpen(true)
    void ensureModelOptions()
    void ensureGroupOptions()
  }

  const submitCreate = async () => {
    // 在途守卫：连点两次「创建」会真的建出两把同名密钥（后端每次都生成新
    // key，没有幂等键），而第二把的明文会被第二次成功的弹窗顶掉、无法找回。
    if (creating) return
    if (!cName.trim()) {
      toast.danger('名称为必填项')
      return
    }
    setCreating(true)
    try {
      const created = await api.createKey({
        name: cName.trim(),
        // quota/rpm/tpm 只在管理员时透传：普通用户填了也会被后端封顶/清零，
        // 照发只是让界面显示「设了」而实际没生效 —— 静默失效比直接不显示更糟。
        ...(isAdmin()
          ? {
              quota_tokens: Number(cQuota) || 0,
              rpm_limit: Number(cRpm) || 0,
              tpm_limit: Number(cTpm) || 0,
            }
          : {}),
        allowed_models: cModels,
        expires_at: expiresAtFromDays(Number(cDays)),
        allowed_ips: cIps.trim(),
        // 分组覆盖仍只给管理员：它能指向更宽松的组，是收紧手段不是自助配置。
        group_id: isAdmin() ? cGroupId : undefined,
      })
      setCOpen(false)
      setPlainKeyBox({ open: true, key: created.plaintext_key, name: created.name })
      await load()
    } catch (e) {
      if ((e as { status?: number }).status !== 401) toast.danger('创建失败：' + (e as Error).message)
    } finally {
      setCreating(false)
    }
  }

  const copyKey = async () => {
    const ok = await copyText(plainKeyBox.key)
    if (ok) toast.success('已复制到剪贴板')
    else toast.danger('复制失败，请手动选择复制')
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

  const importBusy = (k: AccessKey) => importingAction.startsWith(`${k.id}:`)

  /** 打开导入链接。链接只存在于本次点击和本机应用之间，不写入组件状态或持久化存储。 */
  const openImportLink = (href: string) => {
    const a = document.createElement('a')
    a.href = href
    a.hidden = true
    document.body.appendChild(a)
    a.click()
    a.remove()
  }

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
        setCcPickerKey(null)
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

  /** 关闭明文弹窗，并抹掉明文（前端审计 M5）。明文不应在弹窗关闭后继续驻留：
   * 后端保留了加密副本，用户仍可从列表里再次复制。 */
  const closePlainKeyBox = () => setPlainKeyBox({ open: false, key: '', name: '' })

  useEffect(() => closePlainKeyBox, [])

  const openEdit = (k: AccessKey) => {
    setEId(k.id)
    setEName(k.name)
    setEQuota(String(k.quota_tokens ?? 0))
    setERpm(String(k.rpm_limit ?? 0))
    setETpm(String(k.tpm_limit ?? 0))
    setEModels([...(k.allowed_models ?? [])])
    const d = String(daysLeft(k.expires_at ?? 0))
    setEDays(d)
    setEDaysOrig(d)
    setEKeyExpired((k.expires_at ?? 0) > 0 && k.expires_at <= Date.now())
    setEIps(k.allowed_ips ?? '')
    setEGroupId(k.group_id ?? '')
    setEOpen(true)
    void ensureModelOptions()
    void ensureGroupOptions()
  }

  const submitEdit = async () => {
    if (!eName.trim()) {
      toast.danger('名称为必填项')
      return
    }
    try {
      await api.updateKey(eId, {
        name: eName.trim(),
        // 配额与限速与「新建」同一口径：只有管理员能设/改。普通用户编辑自己的
        // key 时若也带上它们，服务端 guardNoLoosening 会以 403 拒绝任何「放宽」
        // —— 用户只想改个名字，却因为一个自己无权设置的字段被拒，整次保存
        // （含改名）全部丢失。不发这个字段 = 保持原值（PATCH 语义）。
        ...(isAdmin()
          ? {
              quota_tokens: Number(eQuota) || 0,
              rpm_limit: Number(eRpm) || 0,
              tpm_limit: Number(eTpm) || 0,
            }
          : {}),
        allowed_models: eModels,
        // 改过天数才发，保持原值就不带这个字段。
        ...(Number(eDays) !== Number(eDaysOrig) ? { expires_at: expiresAtFromDays(Number(eDays)) } : {}),
        allowed_ips: eIps.trim(),
        ...(isAdmin() ? { group_id: eGroupId } : {}),
      })
      setEOpen(false)
      toast.success('密钥已更新')
      await load()
    } catch (e) {
      if ((e as { status?: number }).status !== 401) toast.danger('保存失败：' + (e as Error).message)
    }
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

  // used_tokens 由数据库触发器单调累加、没有回退路径，一旦因 bug 偏高，
  // 配额预检会让这把 key 永远 429（死 key）。这里是唯一的界面自愈入口。
  const recomputeUsage = async (k: AccessKey) => {
    setRecomputing(k.id)
    try {
      const r = await api.recomputeKeyUsage(k.id)
      // 就地用返回的修正值刷新该行：其余行的状态（展开的弹窗等）不受打扰
      setKeys((list) => list.map((it) => (it.id === k.id ? { ...it, used_tokens: r.used_tokens } : it)))
      toast.success(`已重算「${k.name}」用量：${fmtTokens(r.used_tokens)} tokens`)
    } catch (e) {
      if ((e as { status?: number }).status !== 401) toast.danger('重算失败：' + (e as Error).message)
    } finally {
      setRecomputing('')
    }
  }

  /** 新建表单里给普通用户看的额度说明：显示规则而不是一个会停更的数字
   * （本页拿不到会刷新的配额值；真正的「已用 / 配额」展示在钱包页）。 */
  const quotaHint = '由管理员统一配置，详见「钱包与充值」页'

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

  const groupSelect = (value: string, onChange: (v: string) => void) => (
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

      <DataTable
        label="调用密钥"
        loading={loading}
        error={err}
        hasData={keys.length > 0}
        empty={true}
        emptyText={filteredEmpty ? undefined : '还没有访问密钥'}
        emptyAction={
          filteredEmpty ? (
            <Button variant="ghost" size="sm" onPress={() => setFUser('')}>
              清除筛选
            </Button>
          ) : undefined
        }
        toolbar={filterToolbar}
      >
        {filteredEmpty ? null : (
          <Table>
            <Table.ScrollContainer>
              <Table.Content aria-label="调用密钥">
                <Table.Header>
                  <Table.Column>名称</Table.Column>
                  <Table.Column>归属用户</Table.Column>
                  <Table.Column>限制</Table.Column>
                  {/* 表头标出方向：这一列按 used_tokens 降序，箭头就是排的方向。 */}
                  <Table.Column className="text-right">用量 ↓</Table.Column>
                  <Table.Column>有效期</Table.Column>
                  <Table.Column className="text-right">操作</Table.Column>
                </Table.Header>
                <Table.Body>
                  {visibleKeys.map((k) => (
                    <Table.Row key={k.id}>
                      <Table.Cell>
                        <div className="flex items-center gap-2">
                          <span className="font-medium">{k.name}</span>
                          <StatusChip tone={k.enabled ? 'ok' : 'dim'}>{k.enabled ? '启用' : '停用'}</StatusChip>
                        </div>
                        <div className="text-muted font-mono text-[11.5px]">{k.key_prefix}</div>
                      </Table.Cell>
                      <Table.Cell>
                        {/* 无归属必须显式标注：这类 key 会被迁移退役、鉴权直接 401，
                            不标注的话运维会以为它还能用。 */}
                        {!k.user_id ? (
                          <StatusChip
                            tone="dim"
                            title="迁移遗留的无归属密钥已被停用，调用时鉴权直接返回 401，建议删除"
                          >
                            已停用
                          </StatusChip>
                        ) : (
                          <span className="font-mono text-[12.5px]">{k.username || k.user_id}</span>
                        )}
                      </Table.Cell>
                      <Table.Cell>
                        <div className="flex flex-wrap items-center gap-1.5">
                          {/* 白名单非空才标：空 = 不限制，标出来反而让人以为受限。 */}
                          {k.allowed_models.length > 0 ? (
                            <StatusChip tone="ok">限 {k.allowed_models.length} 个模型</StatusChip>
                          ) : null}
                          {k.allowed_ips ? (
                            <StatusChip tone="info" title={`只允许 ${k.allowed_ips} 调用`}>
                              限源
                            </StatusChip>
                          ) : null}
                          {k.group_id ? (
                            <StatusChip tone="info" title="该密钥使用指定分组的模型范围">
                              分组覆盖
                            </StatusChip>
                          ) : null}
                          {rateText(k) ? <span className="text-muted text-xs">{rateText(k)}</span> : null}
                          {!anyLimit(k) ? <span className="text-muted text-xs">不限</span> : null}
                        </div>
                      </Table.Cell>
                      <Table.Cell className="text-right tabular-nums">
                        {fmtTokens(k.used_tokens)}
                        <span className="text-muted"> / {k.quota_tokens > 0 ? fmtTokens(k.quota_tokens) : '不限'}</span>
                        {k.quota_tokens > 0 && k.used_tokens >= k.quota_tokens ? (
                          <div>
                            <StatusChip tone="err">配额已用尽</StatusChip>
                          </div>
                        ) : null}
                      </Table.Cell>
                      <Table.Cell>
                        {k.expires_at > 0 ? (
                          <div className="flex items-center gap-1.5">
                            {/* 已过期：最需要被一眼看到的状态。它仍然 enabled，
                                但数据面一律 403，必须与「停用」区分开。 */}
                            {isExpired(k) ? <StatusChip tone="err">已过期</StatusChip> : null}
                            <span>
                              到期 {fmtDateTime(k.expires_at)}
                              {!isExpired(k) ? `（剩 ${daysLeft(k.expires_at)} 天）` : ''}
                            </span>
                          </div>
                        ) : (
                          <span className="text-muted text-xs">永不过期</span>
                        )}
                        <div className="text-muted text-[11.5px]">建于 {fmtDateTime(k.created_at)}</div>
                      </Table.Cell>
                      <Table.Cell>
                        <div className="flex flex-wrap items-center justify-end gap-1">
                          {/* 三个操作固定显示在每行末尾。老密钥只有哈希、没有可回读
                              的密文，按钮保留但禁用，避免整列操作随密钥创建时间跳变。 */}
                          <span title={k.can_copy ? '把完整密钥复制到剪贴板' : '这把密钥创建于加密副本功能上线前，明文已不可恢复；请新建密钥'}>
                            <Button
                              isIconOnly
                              variant="ghost"
                              size="sm"
                              isDisabled={!k.can_copy || copyingId === k.id}
                              aria-label="复制 Key"
                              onPress={() => void copyExistingKey(k)}
                            >
                              <Copy size={16} strokeWidth={1.75} aria-hidden />
                            </Button>
                          </span>
                          <span title={k.can_copy ? '选择要导入 CC Switch 的客户端' : '这把密钥的明文已不可恢复，无法导入；请新建密钥'}>
                            <Button
                              isIconOnly
                              variant="ghost"
                              size="sm"
                              isDisabled={!k.can_copy || importBusy(k)}
                              aria-label="导入 CC Switch"
                              onPress={() => setCcPickerKey(k)}
                            >
                              <img src={ccSwitchIcon} alt="" width={16} height={16} className="object-contain" />
                            </Button>
                          </span>
                          <span title={k.can_copy ? '把网关地址和这把密钥添加到本机 Magpie' : '这把密钥的明文已不可恢复，无法导入；请新建密钥'}>
                            <Button
                              isIconOnly
                              variant="ghost"
                              size="sm"
                              isDisabled={!k.can_copy || importBusy(k)}
                              aria-label="导入 Magpie"
                              onPress={() => void importKey(k, 'magpie')}
                            >
                              <img src={magpieIcon} alt="" width={17} height={17} className="icon-invert-dark object-contain" />
                            </Button>
                          </span>
                          {/* 不按身份隐藏：端点在白名单内，普通用户有权自查。
                              但要说清它修的是什么 —— 计数异常通常是历史归档或迁移
                              留下的，对普通用户而言点它多半看不到变化。 */}
                          <span title="按调用明细重算已用 tokens。通常无需操作：仅在配额计数与实际用量明显不符时使用（例如历史数据迁移后）">
                            <Button
                              variant="ghost"
                              size="sm"
                              isDisabled={recomputing === k.id}
                              onPress={() => void recomputeUsage(k)}
                            >
                              {recomputing === k.id ? '重算中…' : '重算用量'}
                            </Button>
                          </span>
                          <Button variant="ghost" size="sm" onPress={() => openEdit(k)}>
                            编辑
                          </Button>
                          <Button variant="danger" size="sm" onPress={() => void removeKey(k)}>
                            删除
                          </Button>
                          <Switch
                            isSelected={k.enabled}
                            isDisabled={toggling.has(k.id)}
                            aria-label={(k.enabled ? '停用 ' : '启用 ') + (k.name || k.key_prefix)}
                            onChange={() => void toggleKey(k)}
                          />
                        </div>
                      </Table.Cell>
                    </Table.Row>
                  ))}
                </Table.Body>
              </Table.Content>
            </Table.ScrollContainer>
          </Table>
        )}
      </DataTable>

      {/* CC Switch 客户端选择 */}
      <AppModal
        open={ccPickerKey !== null}
        onClose={() => {
          if (!importingAction) setCcPickerKey(null)
        }}
        title="导入 CC Switch"
        maxWidth="520px"
      >
        <p className="text-muted mb-3.5 text-[13px] leading-relaxed">
          选择要添加为供应商的客户端。导入后会打开 CC Switch 让你核对配置。
        </p>
        <div className="flex flex-col gap-2" role="group" aria-label="CC Switch 客户端">
          {CC_APPS.map((app) => (
            <Button
              key={app.id}
              variant="ghost"
              isDisabled={!!importingAction}
              onPress={() => ccPickerKey && void importKey(ccPickerKey, 'ccswitch', app.id)}
              className="border-separator hover:bg-accent-soft hover:border-accent h-auto w-full justify-between border px-3.5 py-2.5"
            >
              <span className="flex min-w-0 items-center gap-3">
                <img src={app.icon} alt="" width={34} height={34} className={`object-contain${app.invertDark ? ' icon-invert-dark' : ''}`} />
                <span className="flex min-w-0 flex-col items-start gap-0.5 text-left">
                  <span className="text-sm font-semibold">{app.name}</span>
                  <span className="text-muted text-xs">{app.detail}</span>
                </span>
              </span>
              <span className="text-muted text-xs">{ccImportingApp === app.id ? '导入中…' : '→'}</span>
            </Button>
          ))}
        </div>
        <div className="text-muted mt-3 text-xs leading-relaxed">
          每次只导入一个客户端；点击后请以 CC Switch 中显示的地址和密钥为准。
        </div>
      </AppModal>

      {/* 创建表单：表单填一半别丢，遮罩/Esc 都不关（dismissable=false） */}
      <AppModal
        open={cOpen}
        onClose={() => setCOpen(false)}
        title="新建访问密钥"
        maxWidth="500px"
        dismissable={false}
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onPress={() => setCOpen(false)}>
              取消
            </Button>
            <Button variant="primary" isDisabled={creating} onPress={() => void submitCreate()}>
              {creating ? '创建中…' : '创建'}
            </Button>
          </div>
        }
      >
        <form
          onSubmit={(e) => {
            e.preventDefault()
            void submitCreate()
          }}
          className="flex flex-col gap-4"
        >
          <TextField aria-label="名称" isRequired value={cName} onChange={setCName}>
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
            <ModelPicker value={cModels} onChange={setCModels} options={modelOptions} restricted={modelsRestricted} />
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <TextField aria-label="有效期（天）" value={cDays} onChange={setCDays}>
              <Label>有效期（天）</Label>
              <Input type="number" min={0} step={1} placeholder="0" />
              <span className="text-muted text-[11px]">0 = 永不过期。到期后该密钥请求返回 403。</span>
            </TextField>
            <TextField aria-label="来源 IP 白名单" value={cIps} onChange={setCIps}>
              <Label>来源 IP 白名单</Label>
              <Input placeholder="留空 = 不限制" autoComplete="off" />
              <span className="text-muted text-[11px]">
                逗号分隔的 CIDR 或单 IP，如 <code>10.0.0.0/8, 203.0.113.7</code>。按直连地址判定，不认 <code>X-Forwarded-For</code>。
              </span>
            </TextField>
          </div>
          {isAdmin() ? (
            <div>
              <Label>分组覆盖</Label>
              <div className="mt-1.5">{groupSelect(cGroupId, setCGroupId)}</div>
              <span className="text-muted mt-1.5 block text-[11px]">
                让这把密钥用另一个组的模型范围，常用于「给某个人的某把密钥单独收紧」。
              </span>
            </div>
          ) : null}
          {isAdmin() ? (
            <div className="grid gap-4 sm:grid-cols-3">
              <TextField aria-label="Token 配额" value={cQuota} onChange={setCQuota}>
                <Label>Token 配额</Label>
                <Input type="number" min={0} step={1} placeholder="0" />
                <span className="text-muted text-[11px]">累计 input+output token 上限，用尽后 /v1 返回 429；0 = 不限</span>
              </TextField>
              <TextField aria-label="RPM 限速" value={cRpm} onChange={setCRpm}>
                <Label>RPM 限速</Label>
                <Input type="number" min={0} step={1} placeholder="0" />
                <span className="text-muted text-[11px]">每分钟请求数上限；0 = 不限</span>
              </TextField>
              <TextField aria-label="TPM 限速" value={cTpm} onChange={setCTpm}>
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

      {/* 创建后的明文展示；之后仍可从列表再次复制。
          dismissable=false：明文弹窗必须由用户显式关闭，避免误点遮罩后
          还得重新发起读取请求。 */}
      <AppModal
        open={plainKeyBox.open}
        onClose={closePlainKeyBox}
        title="密钥已创建"
        maxWidth="520px"
        dismissable={false}
        footer={
          <div className="flex justify-end gap-2">
            <Button onPress={() => void copyKey()}>复制密钥</Button>
            <Button variant="primary" onPress={closePlainKeyBox}>
              我已保存
            </Button>
          </div>
        }
      >
        <p className="text-[13px] leading-relaxed">
          「{plainKeyBox.name}」的完整密钥如下。密钥已加密保存，之后仍可在列表里点「复制」再次获取：
        </p>
        <div className="bg-default-soft mt-3 rounded-lg p-3">
          <div className="font-mono text-[13px] break-all">{plainKeyBox.key}</div>
          <div className="text-warning mt-2 text-xs">请立即复制并妥善保管，不要提交到代码仓库。</div>
        </div>
      </AppModal>

      {/* 编辑表单：表单填一半别丢，遮罩/Esc 都不关 */}
      <AppModal
        open={eOpen}
        onClose={() => setEOpen(false)}
        title="编辑密钥"
        maxWidth="500px"
        dismissable={false}
        footer={
          <div className="flex justify-end gap-2">
            <Button variant="ghost" onPress={() => setEOpen(false)}>
              取消
            </Button>
            <Button variant="primary" onPress={() => void submitEdit()}>
              保存
            </Button>
          </div>
        }
      >
        <form
          onSubmit={(e) => {
            e.preventDefault()
            void submitEdit()
          }}
          className="flex flex-col gap-4"
        >
          <TextField aria-label="名称" isRequired value={eName} onChange={setEName}>
            <Label>名称 *</Label>
            <Input autoComplete="off" />
          </TextField>
          <div>
            <span className="mb-1.5 block text-[13px] font-medium">可用模型</span>
            <ModelPicker value={eModels} onChange={setEModels} options={modelOptions} restricted={modelsRestricted} />
          </div>
          <div className="grid gap-4 sm:grid-cols-2">
            <TextField aria-label="有效期（天）" value={eDays} onChange={setEDays}>
              <Label>有效期（天）</Label>
              <Input type="number" min={0} step={1} placeholder="0" />
              {eKeyExpired ? (
                <span className="text-muted text-[11px]">该密钥已过期。改动天数会重新设定有效期（从现在起 N 天）；不改动则维持过期状态。</span>
              ) : (
                <span className="text-muted text-[11px]">0 = 永不过期。改动天数会按「从现在起 N 天」重设截止时间；不改动则保持原值。</span>
              )}
            </TextField>
            <TextField aria-label="来源 IP 白名单" value={eIps} onChange={setEIps}>
              <Label>来源 IP 白名单</Label>
              <Input placeholder="留空 = 不限制" autoComplete="off" />
              <span className="text-muted text-[11px]">逗号分隔的 CIDR 或单 IP。写错会让这把密钥从所有地址都连不上。</span>
            </TextField>
          </div>
          {isAdmin() ? (
            <div>
              <Label>分组覆盖</Label>
              <div className="mt-1.5">{groupSelect(eGroupId, setEGroupId)}</div>
              <span className="text-muted mt-1.5 block text-[11px]">这把密钥用哪个组的模型范围。</span>
            </div>
          ) : null}
          {isAdmin() ? (
            <div className="grid gap-4 sm:grid-cols-3">
              <TextField aria-label="Token 配额" value={eQuota} onChange={setEQuota}>
                <Label>Token 配额</Label>
                <Input type="number" min={0} step={1} placeholder="0" />
                <span className="text-muted text-[11px]">累计 input+output token 上限，用尽后 /v1 返回 429；0 = 不限</span>
              </TextField>
              <TextField aria-label="RPM 限速" value={eRpm} onChange={setERpm}>
                <Label>RPM 限速</Label>
                <Input type="number" min={0} step={1} placeholder="0" />
                <span className="text-muted text-[11px]">每分钟请求数上限；0 = 不限</span>
              </TextField>
              <TextField aria-label="TPM 限速" value={eTpm} onChange={setETpm}>
                <Label>TPM 限速</Label>
                <Input type="number" min={0} step={1} placeholder="0" />
                <span className="text-muted text-[11px]">每分钟 token 上限（估算预占+事后校正）；0 = 不限</span>
              </TextField>
            </div>
          ) : null}
        </form>
      </AppModal>
    </div>
  )
}
