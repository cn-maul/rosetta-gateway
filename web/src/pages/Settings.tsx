import { useEffect, useMemo, useRef, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import {
  Alert,
  Button,
  Disclosure,
  Input,
  Label,
  Spinner,
  Tabs,
  TextField,
  TextArea,
  toast,
} from '@heroui/react'
import { api } from '../api'
import { confirmBox } from '../ui/confirm'
import { PageHeader } from '../ui/PageHeader'
import { SectionCard } from '../ui/SectionCard'
import { StatusChip } from '../ui/StatusChip'
import { FileImport } from '../ui/FileImport'
import { fmtNum } from '../fmt'
import { MODEL_DEFAULT_FALLBACKS, RUNTIME_DEFAULTS } from '../generated/defaults'
import { PriceSettings } from '../components/PriceSettings'
import { SiteBrandCard } from '../components/settings/SiteBrandCard'
import { TokenField } from '../ui/TokenField'
import { parseTokenCount } from '../tokenCount'
import type { ConfigExportFile, ConfigImportResult, PruneResult } from '../types'

/**
 * 系统设置（管理员）：设置项按分类分页展示，一次只看一类
 * （语义对齐旧 Settings.vue）。
 *
 * 「审计日志」页签已移除（用户要求）。后端 GET /admin/api/audit 端点与
 * api.ts 的 api.audit() 一并保留 —— 它们是运维取证的正路，删掉的是
 * 界面上的那一块，不是能力本身。
 */

type TabKey = 'site' | 'model' | 'runtime' | 'price' | 'transfer'

const TABS: { key: TabKey; label: string; sub: string }[] = [
  { key: 'site', label: '站点', sub: '站点名称与 LOGO：显示在登录页与侧栏左上角' },
  { key: 'model', label: '模型默认', sub: '全局默认值；模型容量探测不到时回落到这里' },
  { key: 'runtime', label: '运行时', sub: '各类超时与自动故障转移的全局默认，按「谁在保护什么」分块' },
  // 这里曾有一个「安全」页，放的是独立于 users 表的管理密码（另有一份凭据文件兜底）。
  // 统一认证后管理密码就是 users 表里某个用户的密码，已由「我的账号」（自助改）
  // 与「用户管理」（管理员重置）各自承担，所以整块删除 —— 不要在这里加回跳转提示之类的残件。
  // 改价只影响**此后**的用量：历史费用在落库时已按当时的价固化，
  // 所以这里改的是「往后怎么算」，不是「重算历史」。
  { key: 'price', label: '模型价格', sub: '按供应商 × 模型配置单价；改价只对之后的用量生效，不改写历史费用' },
  {
    key: 'transfer',
    label: '导入导出',
    sub: '导出供应商与模型配置，或从文件导入。只影响供应商与模型，不含路由与密钥',
  },
]

// 运行时推荐默认值由 Go 侧生成（go generate ./internal/config）。
// 原始定义在 internal/config/config.go；这里不再手抄一份数字。
type RuntimeKey = keyof typeof RUNTIME_DEFAULTS

// 运行时各分类的成员：模板的分组渲染与「这一类是否已偏离推荐值」共用
// 同一份声明。两处各写一遍字段名，迟早会漂移成「按钮恢复的是另一组的值」。
const RUNTIME_GROUPS: Record<'nonstream' | 'streaming' | 'failover', RuntimeKey[]> = {
  nonstream: ['upstream_timeout_ms'],
  streaming: ['stream_first_token_timeout_ms', 'stream_idle_timeout_ms'],
  failover: ['failover_max_targets', 'failover_failure_threshold'],
}

const RUNTIME_FIELD_META: Record<RuntimeKey, { label: string; min: number; max: number; tip: React.ReactNode }> = {
  upstream_timeout_ms: {
    label: '整体超时（毫秒）',
    min: 1,
    max: 86400000,
    tip: (
      <>
        超时后按可转移错误处理 —— 有机会切到下一个上游，而不是直接失败。
        这是<b>单次尝试</b>的预算，不是整请求的上限（见下方「派生上限」）。
      </>
    ),
  },
  stream_first_token_timeout_ms: {
    label: '首字超时（TTFT，毫秒）',
    min: 1,
    max: 86400000,
    tip: (
      <>
        已建立连接后，首个事件迟迟不来即掐流并切换下一个上游。
        <b>仅在尚未写出任何字节时生效</b>；带思考的上游首字本来就慢，别调太紧。
      </>
    ),
  },
  stream_idle_timeout_ms: {
    label: '流式空闲超时（毫秒）',
    min: 1,
    max: 86400000,
    tip: (
      <>
        已开始出字后，多久没有任何新事件即判定卡死（按截断收尾）。
        流已经发给客户端了，此时不会切换上游，只会以截断结束。
      </>
    ),
  },
  failover_max_targets: {
    label: '最多尝试目标数',
    min: 1,
    max: 100,
    tip: (
      <>
        一次请求最多打某条路由链上的几个目标。每多一个，最坏耗时就多乘一截
        （见下方「派生上限」）。是否启用按路由单独开关（路由页）。
      </>
    ),
  },
  failover_failure_threshold: {
    label: '失败熔断阈值',
    min: 1,
    max: 1000,
    tip: (
      <>
        某目标连续失败几次后临时摘除（60 秒冷却），期间请求自动让位给链上下一个目标。
        调高等于继续把请求送给已经坏掉的目标。
      </>
    ),
  },
}

type FormState = Record<RuntimeKey | 'default_context_window' | 'default_max_output_tokens', string>

const initialForm = (): FormState => ({
  default_context_window: String(MODEL_DEFAULT_FALLBACKS.default_context_window),
  default_max_output_tokens: String(MODEL_DEFAULT_FALLBACKS.default_max_output_tokens),
  upstream_timeout_ms: String(RUNTIME_DEFAULTS.upstream_timeout_ms),
  stream_idle_timeout_ms: String(RUNTIME_DEFAULTS.stream_idle_timeout_ms),
  stream_first_token_timeout_ms: String(RUNTIME_DEFAULTS.stream_first_token_timeout_ms),
  failover_max_targets: String(RUNTIME_DEFAULTS.failover_max_targets),
  failover_failure_threshold: String(RUNTIME_DEFAULTS.failover_failure_threshold),
})

// fmtDur 把毫秒说成人话（「5 分钟」而不是「300000」）。
// 超时是给人读量级的值，让管理员对着六位数心算本身就是一处摩擦。
function fmtDur(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '—'
  if (ms % 60_000 === 0) return `${ms / 60_000} 分钟`
  if (ms % 1000 === 0) return `${ms / 1000} 秒`
  return `${ms} 毫秒`
}

export function Settings() {
  const [searchParams, setSearchParams] = useSearchParams()
  const rawTab = searchParams.get('tab')
  // 没带 ?tab= 时落在第一个页签（站点）。其它页（用量与费用、钱包、用户与权限）
  // 的默认值也都是各自列表的第一项；这里原来写死 'model'，于是点「设置」永远先
  // 看到模型默认值，用户在地址栏里也看不出「站点」这一页存在。
  const tab: TabKey = TABS.some((t) => t.key === rawTab) ? (rawTab as TabKey) : TABS[0].key
  const tabSub = TABS.find((t) => t.key === tab)?.sub ?? ''
  const setTab = (key: string) => {
    const next = new URLSearchParams(searchParams)
    next.set('tab', key)
    setSearchParams(next)
  }

  const [form, setForm] = useState<FormState>(initialForm)
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  // err 是**持久**的读取错误，不是 toast。
  //
  // 为什么必须是持久状态：本页面的表单是**可写**的，且保存走全量
  // PUT /settings。GET 失败时 form.* 仍是上面那组兜底常量，于是界面呈现
  // 一份「看起来完整、可以编辑」的配置，实际上全是写死的默认值 —— 运维
  // 看不到任何异常提示（只有一个转瞬即逝的 toast），很自然会点「保存」，
  // 于是真实的生产配置被这组默认值整体覆盖。
  const [err, setErr] = useState('')
  // loadedOnce 记录「至少成功拉过一次真实值」。
  //
  // 为什么必须与 err 分开：err 只在非 401 的 catch 里被赋值，而 load() 的
  // 401 分支直接 return —— 那一瞬间 err 仍为空，只看 err 的保存守卫会放行，
  // 表单却停在兜底常量上，管理员一点保存就把默认值整体 PUT 覆盖到真实配置。
  // 真正的判据是「当前表单里的值是不是真值」，而这件事只有成功加载过才知道。
  const [loadedOnce, setLoadedOnce] = useState(false)

  const [pruning, setPruning] = useState(false)
  const [pruneResult, setPruneResult] = useState<PruneResult | null>(null)

  // ---------- 导入 / 导出 ----------
  //
  // 两个动作都是「文件进、文件出」，没有表单要保存，所以单独一节。
  //
  // 导出策略（2026-10-07 收敛）：**必带凭据 + 必加密**，产物统一是 .json.enc。
  // 曾经「带凭据」是勾选、「加密」是可选项，四种组合里最危险的
  // 「明文 + 带凭据」恰恰是最容易顺手点出来的那种；且两种产物格式让导入侧
  // 要兼容两套形态。现在只有一个开关 —— 口令，没有它就导不出来。
  // 「同时导出 API Key」的勾选随之删除：不带走凭据的导出文件在目标机器上
  // 还要手工补密钥，实际没人这么用。
  const [transferBusy, setTransferBusy] = useState(false)
  const [exportPass, setExportPass] = useState('')

  // 导入分两步：先读文件并干跑，把「会发生什么」摆出来，确认后才真写。
  // 导入是改一个可能正在跑流量的库的动作，不能一击生效。
  const [importFile, setImportFile] = useState<File | null>(null)
  const [importParsed, setImportParsed] = useState<ConfigExportFile | null>(null)
  const [importPass, setImportPass] = useState('')
  const [importPreview, setImportPreview] = useState<ConfigImportResult | null>(null)
  const [importError, setImportError] = useState('')

  // 价格页签是独立组件、自己拉数据；外层「刷新」按钮通过计数器触发它的重拉。
  const [priceRefresh, setPriceRefresh] = useState(0)

  const formRef = useRef(form)
  formRef.current = form

  const load = async () => {
    setLoading(true)
    setErr('')
    try {
      const s = await api.settings()
      setForm({
        default_context_window: String(s.default_context_window),
        default_max_output_tokens: String(s.default_max_output_tokens),
        upstream_timeout_ms: String(s.upstream_timeout_ms),
        stream_idle_timeout_ms: String(s.stream_idle_timeout_ms),
        stream_first_token_timeout_ms: String(s.stream_first_token_timeout_ms),
        failover_max_targets: String(s.failover_max_targets),
        failover_failure_threshold: String(s.failover_failure_threshold),
      })
      setLoadedOnce(true)
    } catch (e) {
      // 401 不设 err，但同样必须让保存守卫失效 —— 否则表单停在兜底常量上
      // 而守卫放行，等于把默认值 PUT 回生产配置。详见 loadedOnce 的注释。
      if ((e as { status?: number }).status === 401) return
      // 记入 err 并保持：表单在此状态下**不可保存**（见 save 的守卫）。
      const msg = (e as Error).message
      setErr(msg)
      toast.danger('加载设置失败：' + msg)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    void load()
  }, [])

  // 两个分类共用一张表单与一个保存接口（PUT /settings 要求全量字段），
  // 所以只校验**当前分类**的字段：另一个分类的值是载入时后端校验过的原值，
  // 不该在保存这一分类时把用户挡下来。
  const save = async (target: 'model' | 'runtime') => {
    const f = formRef.current
    // 默认容量这两个数允许写 128K / 1M（厂商文档就是这么写的），解析口径见
    // src/tokenCount.ts。解析放在这里而不是保存语句里，是为了让「model」分类
    // 的校验和真正发出去的值共用同一次解析 —— 校验过的值与写下去的值必须同源。
    const ctxTokens = parseTokenCount(f.default_context_window)
    const outTokens = parseTokenCount(f.default_max_output_tokens)
    // 两个分类共用一张表单与一个保存接口（PUT /settings 要求全量字段），
    // 所以只校验**当前分类**的字段：另一个分类的值是载入时后端校验过的原值，
    // 不该在保存这一分类时把用户挡下来。
    //
    // 读取失败时 form 里是兜底常量，保存就是拿默认值覆盖生产配置 —— 直接拒绝。
    // 提示要指向「重新加载」，因为这是唯一能解开的状态。
    //
    // 判据是 loadedOnce 而不是 err：401 分支不设 err，只看 err 会在会话失效
    // 的窗口里放行一次「默认值覆盖真实配置」。详见 loadedOnce 的注释。
    if (err || !loadedOnce) {
      toast.danger('设置未能加载，保存已阻止（否则会用默认值覆盖当前配置）。请先重新加载。')
      return
    }
    if (target === 'model') {
      if (ctxTokens === null || outTokens === null) {
        toast.danger('默认上下文与最大输出无法识别，请填数字或 128K / 1M 这类写法')
        return
      }
      if (ctxTokens <= 0 || outTokens <= 0) {
        toast.danger('默认上下文与最大输出必须为正数')
        return
      }
      if (outTokens > ctxTokens) {
        toast.danger('最大输出通常不应超过上下文窗口')
        return
      }
    } else {
      // 运行时默认同样必须是 >= 1 的整数：超时值会直接喂给看门狗与 context.WithTimeout，
      // 0/负数会变成「立即超时」，把全部转发打挂（后端也有同一份硬校验）。
      const nums: [string, number][] = [
        ['非流式超时', Number(f.upstream_timeout_ms)],
        ['流式空闲超时', Number(f.stream_idle_timeout_ms)],
        ['流式首字超时', Number(f.stream_first_token_timeout_ms)],
      ]
      for (const [label, v] of nums) {
        if (!Number.isFinite(v) || v < 1) {
          toast.danger(label + '必须为 >= 1 的毫秒数')
          return
        }
      }
      const maxTargets = Number(f.failover_max_targets)
      const threshold = Number(f.failover_failure_threshold)
      if (!Number.isFinite(maxTargets) || maxTargets < 1) {
        toast.danger('最多尝试目标数必须 >= 1')
        return
      }
      if (!Number.isFinite(threshold) || threshold < 1) {
        toast.danger('失败熔断阈值必须 >= 1')
        return
      }
    }

    setSaving(true)
    try {
      // saveSettings 走 mutate()：保存后自动 reload，转发路径即时用上新值（无需重启）。
      await api.saveSettings({
        default_context_window: ctxTokens ?? 0,
        default_max_output_tokens: outTokens ?? 0,
        upstream_timeout_ms: Number(f.upstream_timeout_ms),
        stream_idle_timeout_ms: Number(f.stream_idle_timeout_ms),
        stream_first_token_timeout_ms: Number(f.stream_first_token_timeout_ms),
        failover_max_targets: Number(f.failover_max_targets),
        failover_failure_threshold: Number(f.failover_failure_threshold),
      })
      toast.success('设置已保存并即时生效')
    } catch (e) {
      if ((e as { status?: number }).status !== 401) toast.danger('保存失败：' + (e as Error).message)
    } finally {
      setSaving(false)
    }
  }

  // ---------- 用量归档维护 ----------
  //
  // 明细默认保留 30 天，每日自动剪枝成按天累计（设计 §4.8）。自动任务之外，
  // 运维还需要一个「现在就要」的入口：排查完问题立即收掉明细，或确认归档
  // 是否真的在跑 —— 否则唯一手段是 curl。
  const runPrune = async () => {
    const ok = await confirmBox({
      title: '立即执行归档剪枝？',
      body: '保留窗口（默认 30 天）之前的调用明细将聚合进按天累计后删除。累计数据保留，但单次调用的明细不可恢复。',
      // 不可恢复的操作必须标成危险操作：文案已说明后果，视觉却与普通按钮
      // 同色，两者矛盾会削弱文案本身的作用。
      danger: true,
      confirmLabel: '执行',
    })
    if (!ok) return
    setPruning(true)
    try {
      const r = await api.pruneUsage()
      setPruneResult(r)
      // skipped 不是失败：水位已到位 / 无待归档数据都会走这条，原因在 reason。
      if (r.skipped) toast.success(`本次跳过：${r.reason || '无待归档的数据'}`)
      else toast.success('归档剪枝已完成')
    } catch (e) {
      if ((e as { status?: number }).status !== 401) toast.danger('归档剪枝失败：' + (e as Error).message)
    } finally {
      setPruning(false)
    }
  }

  async function doExport() {
    // 与后端同口径的前置校验：后端也会 400，这里先拦一层省一次往返，
    // 并把按钮的可用态与这条规则对齐（见模板 disabled）。
    if (exportPass.trim().length < 8) {
      toast.danger('请先设置至少 8 位的加密口令：导出文件包含全部上游 API Key')
      return
    }
    setTransferBusy(true)
    try {
      await api.exportConfig(exportPass)
      toast.success('导出已开始下载，请确认文件已保存')
    } catch (e) {
      if ((e as { status?: number }).status !== 401) toast.danger('导出失败：' + (e as Error).message)
    } finally {
      setTransferBusy(false)
    }
  }

  // onPickFile 读文件并立刻干跑一次，让用户先看到结果再决定要不要真导入。
  async function onPickFile(file: File) {
    resetImport()
    setImportFile(file)
    try {
      const text = await file.text()
      const parsed = JSON.parse(text) as ConfigExportFile
      if (!parsed || typeof parsed.version !== 'number') {
        setImportError('这不是有效的导出文件（缺少版本号）')
        return
      }
      setImportParsed(parsed)
      // 加密文件没有口令就没法看内容，先让用户填，而不是直接报一句错。
      if (parsed.encrypted && !importPass) return
      await runImport(true, parsed)
    } catch (e) {
      setImportError('读取文件失败：' + (e as Error).message)
    }
  }

  function resetImport() {
    setImportFile(null)
    setImportParsed(null)
    setImportPass('')
    setImportPreview(null)
    setImportError('')
  }

  // runImport 调后端导入。dryRun=true 时只算不写。
  async function runImport(dryRun: boolean, parsedOverride?: ConfigExportFile) {
    const parsed = parsedOverride ?? importParsed
    if (!parsed) return
    setTransferBusy(true)
    try {
      const res = await api.importConfig(parsed, importPass, dryRun)
      setImportPreview(res)
      if (!dryRun) {
        toast.success(`导入完成：新增 ${res.providers_created} 个供应商、${res.models_created} 个模型`)
        resetImport()
      }
    } catch (e) {
      if ((e as { status?: number }).status !== 401) {
        setImportError((e as Error).message)
      }
    } finally {
      setTransferBusy(false)
    }
  }

  async function confirmImport() {
    const p = importPreview
    if (!p) return
    const renamed = Object.entries(p.providers_renamed ?? {})
    const ok = await confirmBox({
      title: '确认导入',
      body:
        `将新增 ${p.providers_created} 个供应商、${p.models_created} 个模型` +
        (p.credentials_added ? `、${p.credentials_added} 条凭据` : '') +
        '\n\n' +
        (renamed.length
          ? `以下供应商因重名会自动改名（原数据不受影响）：\n` +
            renamed.map(([a, b]) => `　${a} → ${b}`).join('\n') +
            '\n\n'
          : '') +
        `已存在的同名模型保留库里的配置，不覆盖。`,
      confirmLabel: '导入',
    })
    if (ok) await runImport(false)
  }

  // recNote 给出该字段的推荐值，并在当前值已偏离时点明。
  //
  // 「当前已自定义」是有用的信息：这几个数一旦偏离推荐值，多半是当初为了
  // 某个具体故障调过的。界面上看不出偏离，运维很容易在别的故障里顺手改回去。
  const recNote = (k: RuntimeKey): string => {
    const rec = RUNTIME_DEFAULTS[k]
    const unit =
      k === 'failover_max_targets'
        ? ' 个目标'
        : k === 'failover_failure_threshold'
          ? ' 次连续失败'
          : `（${fmtDur(rec)}）`
    const s = `推荐 ${fmtNum(rec)}${unit}`
    return Number(form[k]) === rec ? s : `${s} · 当前已自定义`
  }

  // groupAtDefaults：整类都等于推荐值时，「恢复推荐默认」按钮禁用。
  // 一个永远可点但什么都不会变的按钮，比没有更让人怀疑它是不是坏了。
  const groupAtDefaults = (keys: RuntimeKey[]): boolean =>
    keys.every((k) => Number(form[k]) === RUNTIME_DEFAULTS[k])

  // resetRuntimeGroup 只改表单，不落库。
  // 恢复默认若顺手就写库，「先看看默认长什么样」与「把线上改成默认」就成了一件事。
  const resetRuntimeGroup = (keys: RuntimeKey[]) => {
    setForm((f) => {
      const next = { ...f }
      for (const k of keys) next[k] = String(RUNTIME_DEFAULTS[k])
      return next
    })
  }

  // derivedTotalBudgetText 把「整请求总预算」换算成可读文本，供设置页展示。
  //
  // 必须与后端 totalRequestBudget（cmd/gateway/main.go）的公式**逐项一致** ——
  // 界面上写一个数、实际跑另一个数，比不写更糟：那会让运维以为总预算可以按
  // 界面显示来推理。改后端公式时这里要一起改。
  //
  // 公式：((max(非流式超时, 首字超时 + 等响应头 60s)) + 5s) × (目标数 + 1)，
  // 再夹在 [30s, 30min]。
  const derivedTotalBudgetText = useMemo(() => {
    const nonStream = Number(form.upstream_timeout_ms)
    const ttft = Number(form.stream_first_token_timeout_ms)
    const targets = Number(form.failover_max_targets)
    if (![nonStream, ttft, targets].every((v) => Number.isFinite(v) && v > 0)) return '—'
    // 等响应头 60s 与 5s 余量是后端的常量，同步自 totalRequestBudget。
    const headerWaitMs = 60_000
    const slackMs = 5_000
    const perAttempt = Math.max(nonStream, ttft + headerWaitMs) + slackMs
    const total = Math.min(Math.max(perAttempt * (targets + 1), 30_000), 30 * 60_000)
    return `${Math.round(total / 1000)} 秒`
  }, [form.upstream_timeout_ms, form.stream_first_token_timeout_ms, form.failover_max_targets])

  const refresh = () => {
    if (tab === 'price') setPriceRefresh((n) => n + 1)
    else void load()
  }

  const loadFailedBlock = (
    <div className="py-2">
      <Alert status="danger">
        <Alert.Content>
          <Alert.Title>设置加载失败：{err}</Alert.Title>
          <Alert.Description>表单中的数值是默认值而非当前配置，已阻止保存以免覆盖。</Alert.Description>
        </Alert.Content>
        <Button size="sm" variant="tertiary" onPress={() => void load()}>
          重新加载
        </Button>
      </Alert>
    </div>
  )

  const loadingBlock = (
    <div className="text-muted flex items-center justify-center gap-2 py-10" role="status">
      <Spinner size="sm" /> 加载中…
    </div>
  )

  // 运行时按「谁在保护什么」分块。这几类的**失效后果不同**：改错超时是
  // 「请求变慢/被切走」，改错故障转移是「一直重试坏目标」。块与块之间的
  // 分隔必须能一眼看出这是两类东西。
  const runtimeGroup = (groupKey: 'nonstream' | 'streaming' | 'failover', title: string, desc: string) => {
    const keys = RUNTIME_GROUPS[groupKey]
    return (
      <SectionCard
        title={title}
        description={desc}
        actions={
          <Button
            variant="ghost"
            size="sm"
            isDisabled={groupAtDefaults(keys)}
            onPress={() => resetRuntimeGroup(keys)}
          >
            恢复推荐默认
          </Button>
        }
      >
        <div className="flex flex-col gap-4">
          {keys.map((k) => {
            const meta = RUNTIME_FIELD_META[k]
            return (
              <TextField key={k} aria-label={meta.label} value={form[k]} onChange={(v) => setForm((f) => ({ ...f, [k]: v }))}>
                <Label>{meta.label}</Label>
                <Input type="number" min={meta.min} max={meta.max} step={1} />
                <span className="text-muted text-[11px] leading-relaxed">{meta.tip}</span>
                <span className="text-muted text-[11px] tabular-nums">{recNote(k)}</span>
              </TextField>
            )
          })}
        </div>
      </SectionCard>
    )
  }

  return (
    <div>
      <PageHeader
        title="设置"
        sub={tabSub}
        actions={
          // 站点与价格两个页签各有自己的数据源，外层这个按钮只重取「设置」本体，
          // 所以在那两个页签上不出现 —— 免得点了没反应。
          tab === 'site' || tab === 'price' ? undefined : (
            <Button variant="secondary" size="sm" isDisabled={loading} onPress={refresh}>
              刷新
            </Button>
          )
        }
      />

      <Tabs aria-label="设置分区" selectedKey={tab} onSelectionChange={(key) => setTab(String(key))}>
        <Tabs.ListContainer>
          <Tabs.List>
            {TABS.map((t) => (
              <Tabs.Tab key={t.key} id={t.key}>
                <Tabs.Indicator />
                {t.label}
              </Tabs.Tab>
            ))}
          </Tabs.List>
        </Tabs.ListContainer>
        <Tabs.Panel id={tab}>
          {/* ---------- 分类 0：站点标识 ----------
              这部分不参与 PUT /settings 的全量保存（字段在另一张表），
              所以它自己读、自己写，也不受下方 loadedOnce 守卫影响。 */}
          {tab === 'site' ? <SiteBrandCard /> : null}

          {/* ---------- 分类 1：模型默认 ---------- */}
          {tab === 'model' ? (
            loading ? (
              loadingBlock
            ) : err ? (
              loadFailedBlock
            ) : (
              // 只有两个字段，所以给它一张卡而不是一排。
              <form
                onSubmit={(e) => {
                  e.preventDefault()
                  void save('model')
                }}
                className="max-w-[680px]"
              >
                <SectionCard
                  title="模型默认值"
                  description="上游没报这两个数时用它兜底。改这里不会影响已导入模型的既有值。"
                >
                  <div className="flex flex-col gap-4">
                    <TokenField
                      label="默认上下文窗口（tokens）"
                      value={form.default_context_window}
                      onChange={(v) => setForm((f) => ({ ...f, default_context_window: v }))}
                    />
                    <span className="text-muted -mt-2 text-[11px]">
                      添加模型时若上游未暴露 context_length，则用此值。可写 128K / 1M。
                    </span>
                    <TokenField
                      label="默认最大输出（tokens）"
                      value={form.default_max_output_tokens}
                      onChange={(v) => setForm((f) => ({ ...f, default_max_output_tokens: v }))}
                    />
                    <span className="text-muted -mt-2 text-[11px]">
                      添加模型时若上游未暴露 max_completion_tokens，则用此值。可写 64K / 1M。
                    </span>
                    <div className="flex justify-end">
                      <Button type="submit" variant="primary" isDisabled={saving}>
                        {saving ? '保存中…' : '保存设置'}
                      </Button>
                    </div>
                  </div>
                </SectionCard>
              </form>
            )
          ) : null}

          {/* ---------- 分类 2：运行时（超时 + 故障转移） ---------- */}
          {tab === 'runtime' ? (
            <div className="max-w-[680px]">
              {loading ? (
                loadingBlock
              ) : err ? (
                loadFailedBlock
              ) : (
                <form
                  onSubmit={(e) => {
                    e.preventDefault()
                    void save('runtime')
                  }}
                  className="flex flex-col gap-3.5"
                >
                  {runtimeGroup('nonstream', '非流式请求超时', '不流式调用（stream=false）时，一次请求最多等多久。超时按可转移错误处理。')}
                  {runtimeGroup('streaming', '流式看门狗', '流式调用时盯住「迟迟不出字」与「出到一半卡死」，两个是不同的时间点。')}
                  {runtimeGroup('failover', '自动故障转移', '一个目标不行时，最多换几个、什么情况下先摘掉它。')}

                  {/* 分类 4：派生值（只读） */}
                  <SectionCard
                    title={
                      <span className="flex items-center gap-2">
                        整请求总预算 <StatusChip tone="dim">自动推导 · 不可配</StatusChip>
                      </span>
                    }
                    description="一次请求跨所有故障转移尝试的总时长上限，由上面三类值推导，不是独立旋钮。"
                  >
                    <div className="flex flex-col gap-1.5">
                      <span className="text-muted text-[12.5px] font-semibold">当前生效</span>
                      <span className="text-xl font-semibold tabular-nums">{derivedTotalBudgetText}</span>
                      <span className="text-muted mt-1 text-[11px] leading-relaxed">
                        公式：（max(非流式整体超时, 首字超时 + 等响应头 60 秒) + 5 秒）×（最多尝试目标数 + 1），
                        再夹在 30 秒 ~ 30 分钟之间。
                        没有它，一串半死的上游可以把一个客户端挂住数分钟，期间持续占用连接、配额预占和故障转移名额。
                        <b>注意</b>：上面那些超时都是<b>单次尝试</b>的预算，不是整请求的。
                      </span>
                    </div>
                  </SectionCard>

                  <div className="flex justify-end">
                    <Button type="submit" variant="primary" isDisabled={saving}>
                      {saving ? '保存中…' : '保存设置'}
                    </Button>
                  </div>
                </form>
              )}

              {/* 用量归档维护：与上面的表单无关（不保存任何设置项，是立即执行的动作），
                   单独成块放在运行时页签下，而不是让运维去 curl /usage/prune。 */}
              {!loading && !err ? (
                <div className="mt-4.5 border-t border-separator pt-3.5">
                  <h3 className="text-sm font-semibold">用量归档</h3>
                  <p className="text-muted my-2 text-[11px] leading-relaxed">
                    调用明细默认保留 30 天，更早的记录由每日定时任务聚合进「按天累计」后删除
                    （累计永久保留）。这里可以手动立即执行一次剪枝，结果实时回显。
                  </p>
                  <Button variant="secondary" size="sm" isDisabled={pruning} onPress={() => void runPrune()}>
                    {pruning ? '剪枝中…' : '立即归档剪枝'}
                  </Button>
                  {pruneResult ? (
                    <div className="text-muted mt-2.5 text-[12.5px]">
                      {pruneResult.skipped ? (
                        <>本次跳过：{pruneResult.reason || '无待归档的数据'}</>
                      ) : (
                        <>
                          已删除明细 <b>{fmtNum(pruneResult.deleted_rows)}</b> 行，写入按天累计{' '}
                          <b>{fmtNum(pruneResult.rollup_rows)}</b> 组；归档水位 {pruneResult.pruned_through_day}
                          （保留窗口自 {pruneResult.cutoff_day} 起）
                        </>
                      )}
                    </div>
                  ) : null}
                </div>
              ) : null}
            </div>
          ) : null}

          {/* ---------- 分类 3：模型价格（独立组件，自己拉数据） ---------- */}
          {tab === 'price' ? <PriceSettings refreshSignal={priceRefresh} /> : null}

          {/* ---------- 分类 4：导入 / 导出 ---------- */}
          {tab === 'transfer' ? (
            <div className="grid gap-4 lg:grid-cols-2">
              <SectionCard title="导出">
                <div className="flex flex-col gap-4">
                  <p className="text-muted text-xs leading-relaxed">
                    导出当前全部供应商、模型、它们的参数（超时、重试、协议、端点、价格）
                    以及<b>上游 API Key</b>。文件用口令整体加密，产物是 <code>.json.enc</code> ——
                    不设口令无法导出。
                    <b>不含路由与下游访问密钥</b>：路由的公开名是给调用方看的契约，
                    跨环境照抄容易撞名；下游密钥属于本库的身份体系，不随文件走。
                  </p>
                  <TextField aria-label="加密口令（必填，至少 8 位）" value={exportPass} onChange={setExportPass}>
                    <Label>加密口令（必填，至少 8 位）</Label>
                    <Input type="password" autoComplete="new-password" placeholder="导入这份文件时要用同一个口令" />
                    <span className="text-muted text-[11px] leading-relaxed">
                      文件里包含全部上游 API Key，口令是唯一的保护 —— 请用区别于登录密码的独立口令，
                      并把口令与文件分开传递。
                    </span>
                  </TextField>
                  <div>
                    <Button
                      variant="primary"
                      isDisabled={transferBusy || exportPass.trim().length < 8}
                      onPress={() => void doExport()}
                    >
                      {transferBusy ? '处理中…' : '导出为加密文件'}
                    </Button>
                  </div>
                </div>
              </SectionCard>

              <SectionCard title="导入">
                <div className="flex flex-col gap-4">
                  <p className="text-muted text-xs leading-relaxed">
                    选择一份此前导出的文件。先「预演」看清会发生什么，确认后才真正写入。
                  </p>
                  <div className="flex items-center gap-2">
                    <FileImport
                      accept=".json,.json.enc,.enc,application/json"
                      label="选择导出文件…"
                      disabled={transferBusy}
                      onPick={(f) => void onPickFile(f)}
                    />
                    {importFile ? <span className="text-muted text-[11px]">已选择：{importFile.name}</span> : null}
                  </div>

                  {importParsed?.encrypted ? (
                    <TextField
                      aria-label="文件口令"
                      value={importPass}
                      onChange={(v) => {
                        setImportPass(v)
                        setImportPreview(null)
                      }}
                    >
                      <Label>文件口令 *</Label>
                      <Input type="password" autoComplete="off" placeholder="导出时设置的那个" />
                      <span className="text-muted text-[11px]">该文件已加密，必须输入导出时用的口令</span>
                    </TextField>
                  ) : null}
                  {importParsed?.encrypted ? (
                    <div>
                      <Button
                        variant="secondary"
                        size="sm"
                        isDisabled={!importPass.trim() || transferBusy}
                        onPress={() => void runImport(true)}
                      >
                        预演导入
                      </Button>
                    </div>
                  ) : null}

                  {importError ? (
                    <div className="text-danger text-xs" role="alert">
                      {importError}
                    </div>
                  ) : null}

                  {importParsed && !importParsed.encrypted ? (
                    <div className="text-muted text-xs">
                      文件内容：{importParsed.providers?.length ?? 0} 个供应商、
                      {importParsed.models?.length ?? 0} 个模型、
                      {importParsed.credentials?.length ?? 0} 条凭据
                      {importParsed.version !== 1 ? (
                        <span className="text-warning">（文件版本 {importParsed.version}，可能来自更新的网关）</span>
                      ) : null}
                    </div>
                  ) : null}

                  {/* 预演结果：逐项说明，而不是只给一个总数 */}
                  {importPreview ? (
                    <div className="flex flex-col gap-2">
                      <h4 className="text-sm font-semibold">
                        {importPreview.dry_run ? '预演结果（尚未写入）' : '导入结果'}
                      </h4>
                      <ul className="text-muted list-disc space-y-0.5 pl-5 text-xs">
                        <li>
                          新增供应商 <b>{importPreview.providers_created}</b>
                        </li>
                        <li>
                          新增模型 <b>{importPreview.models_created}</b>
                        </li>
                        {importPreview.credentials_added ? (
                          <li>
                            新增凭据 <b>{importPreview.credentials_added}</b>
                          </li>
                        ) : null}
                        {importPreview.models_skipped ? (
                          <li>
                            跳过已存在的模型 <b>{importPreview.models_skipped}</b>
                          </li>
                        ) : null}
                      </ul>

                      {Object.keys(importPreview.providers_renamed ?? {}).length ? (
                        <div className="text-muted text-xs">
                          重命名（原有数据不受影响）：
                          <ul className="list-disc space-y-0.5 pl-5">
                            {Object.entries(importPreview.providers_renamed ?? {}).map(([from, to]) => (
                              <li key={from}>
                                <code>{from}</code> → <code>{to}</code>
                              </li>
                            ))}
                          </ul>
                        </div>
                      ) : null}

                      {importPreview.notes?.length ? (
                        <Disclosure>
                          <Disclosure.Heading>
                            <Disclosure.Trigger>说明（{importPreview.notes.length} 条）</Disclosure.Trigger>
                          </Disclosure.Heading>
                          <Disclosure.Content>
                            <Disclosure.Body>
                              <ul className="text-muted list-disc space-y-0.5 pl-5 text-xs">
                                {importPreview.notes.map((n, i) => (
                                  <li key={i}>{n}</li>
                                ))}
                              </ul>
                            </Disclosure.Body>
                          </Disclosure.Content>
                        </Disclosure>
                      ) : null}

                      {importPreview.warnings?.length ? (
                        <Disclosure>
                          <Disclosure.Heading>
                            <Disclosure.Trigger>警告（{importPreview.warnings.length} 条）</Disclosure.Trigger>
                          </Disclosure.Heading>
                          <Disclosure.Content>
                            <Disclosure.Body>
                              <ul className="text-muted list-disc space-y-0.5 pl-5 text-xs">
                                {importPreview.warnings.map((n, i) => (
                                  <li key={i}>{n}</li>
                                ))}
                              </ul>
                            </Disclosure.Body>
                          </Disclosure.Content>
                        </Disclosure>
                      ) : null}

                      {importPreview.dry_run ? (
                        <div>
                          <Button variant="primary" isDisabled={transferBusy} onPress={() => void confirmImport()}>
                            确认导入
                          </Button>
                        </div>
                      ) : (
                        <div>
                          <Button variant="secondary" onPress={resetImport}>
                            再导一个
                          </Button>
                        </div>
                      )}
                    </div>
                  ) : null}
                </div>
              </SectionCard>
            </div>
          ) : null}
        </Tabs.Panel>
      </Tabs>
    </div>
  )
}
