<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { api, ApiFail, session, saveToken } from '../api'
import { toast, confirmBox } from '../ui'
import { fmtMoney, fmtNum } from '../fmt'
import { checkPasswordStrength } from '../password'
import AppModal from '../components/AppModal.vue'
import Tabs from '../components/Tabs.vue'
import type {
  Provider,
  UpstreamModel,
  ConfigExportFile,
  ConfigImportResult,
  PruneResult,
} from '../types'

// ---------- 分类页：设置项按分类分页展示，一次只看一类 ----------
//
// 「审计日志」页签已移除（用户要求）。后端 GET /admin/api/audit 端点与
// api.ts 的 api.audit() 一并保留 —— 它们是运维取证的正路，删掉的是
// 界面上的那一块，不是能力本身（见 task 报告里「api.audit 去留」一节）。
type TabKey = 'model' | 'runtime' | 'price' | 'password' | 'transfer'

const TABS: { key: TabKey; label: string; sub: string }[] = [
  { key: 'model', label: '模型默认', sub: '全局默认值；模型容量探测不到时回落到这里' },
  { key: 'runtime', label: '运行时', sub: '各类超时与自动故障转移的全局默认，按「谁在保护什么」分块' },
  // 这里曾有一个「安全」页，放的是独立于 users 表的管理密码（另有一份凭据文件兜底）。
  // 统一认证后管理密码就是 users 表里某个用户的密码，已由「我的账号」（自助改）
  // 与「用户管理」（管理员重置）各自承担，所以整块删除 —— 不要在这里加回跳转提示之类的残件。
  // 改价只影响**此后**的用量：历史费用在落库时已按当时的价固化，
  // 所以这里改的是「往后怎么算」，不是「重算历史」。
  { key: 'price', label: '模型价格', sub: '按供应商 × 模型配置单价；改价只对之后的用量生效，不改写历史费用' },
  // 修改密码（2026-10-13 从「钱包与充值」页挪来）。
  //
  // 改密与钱无关，放在钱包页只是因为那里是普通用户唯一的自助页面 ——
  // 于是用户要改密码得先"想起来"它在钱包里。设置页才是它该在的地方。
  //
  // 位置在导入导出**之前**：导入导出是低频的运维动作，改密比它常用。
  { key: 'password', label: '修改密码', sub: '修改当前登录账号的密码；成功后需要重新登录' },
  {
    key: 'transfer',
    label: '导入导出',
    sub: '导出供应商与模型配置，或从文件导入。只影响供应商与模型，不含路由与密钥',
  },
]
const tab = ref<TabKey>('model')

const tabSub = computed(() => TABS.find((t) => t.key === tab.value)?.sub ?? '')

// ---------- 修改密码 ----------
//
// 2026-10-13 从「钱包与充值」页挪来。改密与钱无关，放在钱包页只是因为
// 那里是普通用户唯一的自助页面 —— 于是用户要改密码得先"想起来"它在钱包里。
//
// 与 UsersGroups 里管理员「重置他人密码」是**两件事**：这里改的是
// 当前登录者自己的密码，且需要旧密码；那边是管理员强制设置，不需要旧的。
// 两者共用的只有强度校验（password.ts 的同一份实现）。
const router = useRouter()
const oldPwd = ref('')
const newPwd = ref('')
const confirmPwd = ref('')
const pwdBusy = ref(false)
const pwdErr = ref('')
const newPwdErr = ref('')

// 强度门槛与后端 userauth.ValidatePassword 同一套（见 password.ts）。
// 刻意与后端保持一致但**不替代**它：前端只是提前告知，真正的强度判断
// 必须在服务端做（绕过前端直接打接口是常态）。
function checkStrength(p: string) {
  return checkPasswordStrength(p)
}

async function submitPwd() {
  if (pwdBusy.value) return
  pwdErr.value = ''
  newPwdErr.value = checkStrength(newPwd.value)
  if (newPwdErr.value) return
  if (newPwd.value !== confirmPwd.value) {
    pwdErr.value = '两次输入的新密码不一致'
    return
  }
  pwdBusy.value = true
  try {
    await api.changeMyPassword(oldPwd.value, newPwd.value)
    // 后端在此递增了 auth_version，**当前这个会话已经失效**（ChangePassword
    // 还顺手清了会话 cookie）。延迟跳转的那段时间里界面还在、任何点击都会打到
    // 一个必然 401 的接口上；所以立刻清掉本地会话并跳登录页 ——
    // 改密成功与「需要重新登录」是同一件事，不该拆成两步。
    //
    // 不调 logout()：那个端点也会（按用户）递增 auth_version 并清 cookie，
    // 而我们已经处在「会话已失效」的状态下，再发一请求只会拿到 401，
    // 还会在 api.ts 的 401 分支里再清一次令牌。本地 saveToken('') 就够了。
    saveToken('')
    session.me = null
    toast('密码已更新，请用新密码重新登录', 'ok')
    oldPwd.value = newPwd.value = confirmPwd.value = ''
    // redirect 指回设置页：重新登录后回到这里，而不是被丢到总览再自己找回来。
    await router.replace({ name: 'login', query: { redirect: '/settings' } })
  } catch (e) {
    pwdErr.value = e instanceof ApiFail ? e.message : '改密失败'
  } finally {
    pwdBusy.value = false
  }
}

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
const transferBusy = ref(false)
const exportPass = ref('')

// 导入分两步：先读文件并干跑，把「会发生什么」摆出来，确认后才真写。
// 导入是改一个可能正在跑流量的库的动作，不能一击生效。
const importFile = ref<File | null>(null)
const importParsed = ref<ConfigExportFile | null>(null)
const importPass = ref('')
const importPreview = ref<ConfigImportResult | null>(null)
const importError = ref('')

function resetImport() {
  importFile.value = null
  importParsed.value = null
  importPass.value = ''
  importPreview.value = null
  importError.value = ''
}

async function doExport() {
  // 与后端同口径的前置校验：后端也会 400，这里先拦一层省一次往返，
  // 并把按钮的可用态与这条规则对齐（见模板 disabled）。
  if (exportPass.value.trim().length < 8) {
    toast('请先设置至少 8 位的加密口令：导出文件包含全部上游 API Key', 'err')
    return
  }
  transferBusy.value = true
  try {
    await api.exportConfig(exportPass.value)
    toast('导出已开始下载，请确认文件已保存', 'ok')
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('导出失败：' + (e as Error).message, 'err')
  } finally {
    transferBusy.value = false
  }
}

// onPickFile 读文件并立刻干跑一次，让用户先看到结果再决定要不要真导入。
async function onPickFile(ev: Event) {
  resetImport()
  const f = (ev.target as HTMLInputElement).files?.[0]
  if (!f) return
  importFile.value = f
  try {
    const text = await f.text()
    const parsed = JSON.parse(text) as ConfigExportFile
    if (!parsed || typeof parsed.version !== 'number') {
      importError.value = '这不是有效的导出文件（缺少版本号）'
      return
    }
    importParsed.value = parsed
    // 加密文件没有口令就没法看内容，先让用户填，而不是直接报一句错。
    if (parsed.encrypted && !importPass.value) return
    await runImport(true)
  } catch (e) {
    importError.value = '读取文件失败：' + (e as Error).message
  }
}

// runImport 调后端导入。dryRun=true 时只算不写。
async function runImport(dryRun: boolean) {
  if (!importParsed.value) return
  transferBusy.value = true
  try {
    const res = await api.importConfig(importParsed.value, importPass.value, dryRun)
    importPreview.value = res
    if (!dryRun) {
      toast(`导入完成：新增 ${res.providers_created} 个供应商、${res.models_created} 个模型`, 'ok')
      resetImport()
      // 原本这里还会 loadAudit()，好让用户切回「审计日志」页签时看到最新记录。
      // 该页签已删除，这个刷新就成了纯浪费的请求（一次 /admin/api/audit
      // + 一次全量解包），没有任何消费方 —— 一并去掉，别留死代码。
    }
  } catch (e) {
    if ((e as { status?: number }).status !== 401) {
      importError.value = (e as Error).message
    }
  } finally {
    transferBusy.value = false
  }
}

async function confirmImport() {
  const p = importPreview.value
  if (!p) return
  const renamed = Object.entries(p.providers_renamed ?? {})
  const ok = await confirmBox({
    title: '确认导入',
    body:
      `将新增 ${p.providers_created} 个供应商、${p.models_created} 个模型` +
      (p.credentials_added ? `、${p.credentials_added} 条凭据` : '') +
      '。\n\n' +
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

const loading = ref(true)
const saving = ref(false)
// err 是**持久**的读取错误（2026-10-10 修复的 P2），不是 toast。
//
// 为什么必须是持久状态：本页面的表单是**可写**的，且保存走全量
// PUT /settings。GET 失败时 form.* 仍是上面那组兜底常量，于是界面呈现
// 一份「看起来完整、可以编辑」的配置，实际上全是写死的默认值 —— 运维
// 看不到任何异常提示（只有一个转瞬即逝的 toast），很自然会点「保存」，
// 于是真实的生产配置被这组默认值整体覆盖。
//
// 其它列表页（Keys/Users/Groups/History/Overview/Models/Routes/Providers）
// 都保留 err 状态正是为此：「请求失败」必须与「没有数据」在界面上可区分。
// 这一页原先是唯一的例外，而它恰好是错值后果最重的一页。
const err = ref('')
// loadedOnce 记录「至少成功拉过一次真实值」。
//
// 为什么必须与 err 分开（2026-10-11 前端审计 P1）：err 只在非 401 的 catch
// 里被赋值，而 load() 的 401 分支直接 return —— 那一瞬间 err 仍为空，
// 于是 `if (err.value) return` 这道保存守卫放行，表单却停在下面这组
// 兜底常量上，管理员一点保存就把默认值整体 PUT 覆盖到真实配置。
//
// 单靠 err 判断「能不能写」是不充分的：真正的判据是「当前表单里的值
// 是不是真值」，而这件事只有成功加载过才知道。
const loadedOnce = ref(false)

// ---------- 运行时推荐默认值（本页唯一真源） ----------
//
// 这一组是「我们认为合适的值」，三处必须逐项一致：这里、后端
// internal/config/config.go 的 setDefaults() 兜底、config.example.json。
// 不是洁癖 —— 界面回显的必须是**正在生效的数**，而 DB 未配置时生效数
// 就是 config 的兜底。界面写一个数、实际跑另一个数，运维会照着界面
// 推理出错误的预期（与下面 derivedTotalBudgetText 同一条纪律）。
//
// 取值理由（2026-10-13）：
//
//	非流式 120s → 300s：非流式没有增量输出可看，只能干等。2 分钟连 8K
//	  tokens 的回复都跑不完（按 ~40 tok/s），于是**正常的慢请求**被判成
//	  超时并触发故障转移 —— 用户侧看到的是「无故变慢 + 结果不对」。
//	  5 分钟覆盖绝大多数非流式生成；再往上派生的总预算会直接撞上
//	  30 分钟上限，推导值就永远显示同一个数，失去意义。
//	TTFT 30s → 60s：带思考/排队的上游（bootstrap 里的 deepseek-reasoner
//	  就是这一类）首字本来就要几十秒，30s 会把正常响应判成超时并切目标。
//	空闲 60s → 120s：已开始出字后，两个事件间隔超过 60s 在慢速流上并不罕见；
//	  调大只影响「真卡死」的发现速度，不影响健康流。
//	目标数 3 / 熔断阈值 3：维持原值。链上第 4 个目标几乎不提升成功率，
//	  却把最坏耗时再乘一截；连续失败 3 次已足够判定目标有问题。
//
// 注意这只影响**兜底**：DB 里已保存过的值、config.json 里显式写过的值
// 都不受它影响（老部署的行为因此完全不变），要换成新值在页面上点保存即可。
const RUNTIME_DEFAULTS = {
  upstream_timeout_ms: 300_000,
  stream_first_token_timeout_ms: 60_000,
  stream_idle_timeout_ms: 120_000,
  failover_max_targets: 3,
  failover_failure_threshold: 3,
}
type RuntimeKey = keyof typeof RUNTIME_DEFAULTS

// 运行时各分类的成员：模板的分组渲染与「这一类是否已偏离推荐值」共用
// 同一份声明。两处各写一遍字段名，迟早会漂移成「按钮恢复的是另一组的值」。
const RUNTIME_GROUPS: Record<'nonstream' | 'streaming' | 'failover', RuntimeKey[]> = {
  nonstream: ['upstream_timeout_ms'],
  streaming: ['stream_first_token_timeout_ms', 'stream_idle_timeout_ms'],
  failover: ['failover_max_targets', 'failover_failure_threshold'],
}

const form = reactive({
  // 与后端 internal/store/settings_dao.go 的兜底常量保持一致：
  // GET 失败时界面也不该回落到明显偏小的早期值（会被误当成真实生效值）。
  default_context_window: 131072,
  default_max_output_tokens: 65536,
  // 运行时全局默认（超时 + 故障转移策略）。原先散落在每条路由上，现统一在此配置。
  upstream_timeout_ms: RUNTIME_DEFAULTS.upstream_timeout_ms,
  stream_idle_timeout_ms: RUNTIME_DEFAULTS.stream_idle_timeout_ms,
  stream_first_token_timeout_ms: RUNTIME_DEFAULTS.stream_first_token_timeout_ms,
  failover_max_targets: RUNTIME_DEFAULTS.failover_max_targets,
  failover_failure_threshold: RUNTIME_DEFAULTS.failover_failure_threshold,
})

// fmtDur 把毫秒说成人话（「5 分钟」而不是「300000」）。
// 超时是给人读量级的值，让管理员对着六位数心算本身就是一处摩擦。
function fmtDur(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '—'
  if (ms % 60_000 === 0) return `${ms / 60_000} 分钟`
  if (ms % 1000 === 0) return `${ms / 1000} 秒`
  return `${ms} 毫秒`
}

// recNote 给出该字段的推荐值，并在当前值已偏离时点明。
//
// 「当前已自定义」是有用的信息：这几个数一旦偏离推荐值，多半是当初为了
// 某个具体故障调过的。界面上看不出偏离，运维很容易在别的故障里顺手改回去。
function recNote(k: RuntimeKey): string {
  const rec = RUNTIME_DEFAULTS[k]
  const s = `推荐 ${fmtNum(rec)}（${fmtDur(rec)}）`
  return Number(form[k]) === rec ? s : `${s} · 当前已自定义`
}

// groupAtDefaults：整类都等于推荐值时，「恢复推荐默认」按钮禁用。
// 一个永远可点但什么都不会变的按钮，比没有更让人怀疑它是不是坏了。
function groupAtDefaults(keys: RuntimeKey[]): boolean {
  return keys.every((k) => Number(form[k]) === RUNTIME_DEFAULTS[k])
}

// resetRuntimeGroup 只改表单，不落库。
// 恢复默认若顺手就写库，「先看看默认长什么样」与「把线上改成默认」就成了一件事。
function resetRuntimeGroup(keys: RuntimeKey[]) {
  for (const k of keys) form[k] = RUNTIME_DEFAULTS[k]
}

// derivedTotalBudgetText 把「整请求总预算」换算成可读文本，供设置页展示。
//
// 必须与后端 totalRequestBudget（cmd/gateway/main.go）的公式**逐项一致** ——
// 界面上写一个数、实际跑另一个数，比不写更糟：那会让运维以为总预算可以按
// 界面显示来推理。改后端公式时这里要一起改。
//
// 公式：((max(非流式超时, 首字超时 + 等响应头 60s)) + 5s) × (目标数 + 1)，
// 再夹在 [30s, 30min]。
const derivedTotalBudgetText = computed(() => {
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
})

async function load() {
  loading.value = true
  err.value = ''
  try {
    const s = await api.settings()
    form.default_context_window = s.default_context_window
    form.default_max_output_tokens = s.default_max_output_tokens
    form.upstream_timeout_ms = s.upstream_timeout_ms
    form.stream_idle_timeout_ms = s.stream_idle_timeout_ms
    form.stream_first_token_timeout_ms = s.stream_first_token_timeout_ms
    form.failover_max_targets = s.failover_max_targets
    form.failover_failure_threshold = s.failover_failure_threshold
    loadedOnce.value = true
  } catch (e) {
    // 401 不设 err，但同样必须让保存守卫失效 —— 否则表单停在兜底常量上
    // 而守卫放行，等于把默认值 PUT 回生产配置。详见 loadedOnce 的注释。
    if ((e as { status?: number }).status === 401) return
    // 记入 err 并保持：表单在此状态下**不可保存**（见 save 的守卫与模板）。
    err.value = (e as Error).message
    toast('加载设置失败：' + err.value, 'err')
  } finally {
    loading.value = false
  }
}

// 两个分类共用一张表单与一个保存接口（PUT /settings 要求全量字段），
// 所以只校验**当前分类**的字段：另一个分类的值是载入时后端校验过的原值，
// 不该在保存这一分类时把用户挡下来。
async function save(target: 'model' | 'runtime') {
  // 读取失败时 form 里是兜底常量，保存就是拿默认值覆盖生产配置 —— 直接拒绝。
  // 提示要指向「重新加载」，因为这是唯一能解开的状态。
  //
  // 判据是 loadedOnce 而不是 err：401 分支不设 err，只看 err 会在会话失效
  // 的窗口里放行一次「默认值覆盖真实配置」。详见 loadedOnce 的注释。
  if (err.value || !loadedOnce.value) {
    toast('设置未能加载，保存已阻止（否则会用默认值覆盖当前配置）。请先重新加载。', 'err')
    return
  }
  if (target === 'model') {
    const ctx = Number(form.default_context_window)
    const out = Number(form.default_max_output_tokens)
    if (!Number.isFinite(ctx) || ctx <= 0 || !Number.isFinite(out) || out <= 0) {
      toast('默认上下文与最大输出必须为正整数', 'err')
      return
    }
    if (out > ctx) {
      toast('最大输出通常不应超过上下文窗口', 'err')
      return
    }
  } else {
    // 运行时默认同样必须是 >= 1 的整数：超时值会直接喂给看门狗与 context.WithTimeout，
    // 0/负数会变成「立即超时」，把全部转发打挂（后端也有同一份硬校验）。
    const nums: [string, number][] = [
      ['非流式超时', Number(form.upstream_timeout_ms)],
      ['流式空闲超时', Number(form.stream_idle_timeout_ms)],
      ['流式首字超时', Number(form.stream_first_token_timeout_ms)],
    ]
    for (const [label, v] of nums) {
      if (!Number.isFinite(v) || v < 1) {
        toast(label + '必须为 >= 1 的毫秒数', 'err')
        return
      }
    }
    const maxTargets = Number(form.failover_max_targets)
    const threshold = Number(form.failover_failure_threshold)
    if (!Number.isFinite(maxTargets) || maxTargets < 1) {
      toast('最多尝试目标数必须 >= 1', 'err')
      return
    }
    if (!Number.isFinite(threshold) || threshold < 1) {
      toast('失败熔断阈值必须 >= 1', 'err')
      return
    }
  }

  saving.value = true
  try {
    // saveSettings 走 mutate()：保存后自动 reload，转发路径即时用上新值（无需重启）。
    await api.saveSettings({
      default_context_window: Number(form.default_context_window),
      default_max_output_tokens: Number(form.default_max_output_tokens),
      upstream_timeout_ms: Number(form.upstream_timeout_ms),
      stream_idle_timeout_ms: Number(form.stream_idle_timeout_ms),
      stream_first_token_timeout_ms: Number(form.stream_first_token_timeout_ms),
      failover_max_targets: Number(form.failover_max_targets),
      failover_failure_threshold: Number(form.failover_failure_threshold),
    })
    toast('设置已保存并即时生效')
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('保存失败：' + (e as Error).message, 'err')
  } finally {
    saving.value = false
  }
}

// ---------- 用量归档维护 ----------
//
// 明细默认保留 30 天，每日自动剪枝成按天累计（设计 §4.8）。自动任务之外，
// 运维还需要一个「现在就要」的入口：排查完问题立即收掉明细，或确认归档
// 是否真的在跑 —— 否则唯一手段是 curl。
const pruning = ref(false)
const pruneResult = ref<PruneResult | null>(null)

async function runPrune() {
  const ok = await confirmBox({
    title: '立即执行归档剪枝？',
    body: '保留窗口（默认 30 天）之前的调用明细将聚合进按天累计后删除。累计数据保留，但单次调用的明细不可恢复。',
    // 不可恢复的操作必须标成危险操作（2026-10-11 前端审计）：
    // 文案已说明后果，视觉却与普通按钮同色，两者矛盾会削弱文案本身的作用。
    danger: true,
    confirmLabel: '执行',
  })
  if (!ok) return
  pruning.value = true
  try {
    pruneResult.value = await api.pruneUsage()
    const r = pruneResult.value
    // skipped 不是失败：水位已到位 / 无待归档数据都会走这条，原因在 reason。
    toast(r.skipped ? `本次跳过：${r.reason || '无待归档的数据'}` : '归档剪枝已完成')
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('归档剪枝失败：' + (e as Error).message, 'err')
  } finally {
    pruning.value = false
  }
}

// ---------- 模型价格 ----------
//
// 价格挂在「供应商的模型」上（upstream_models 的价格列）：同一个 model_id
// 在不同供应商下的实际结算价可能不同，所以主键语义是 provider × model，
// 界面上也必须先选供应商、再选它下面的模型。
const providers = ref<Provider[]>([])
const modelsMap = reactive<Record<string, UpstreamModel[]>>({})
const pricesLoading = ref(false)
const pricesLoaded = ref(false)

const priceProvider = ref('') // '' = 全部供应商
const priceSearch = ref('')
const pricePage = ref(1)
const PRICE_PAGE_SIZE = 10

type PriceRow = { providerId: string; providerName: string; model: UpstreamModel }

// 列表 = **价格条目**：只有配过价的模型才出现在这里。
// 没配价的模型不是「0 元的条目」，而是「没有条目」—— 费用侧本来就按 0 元计，
// 界面上列出它们只会把「新增价格」的入口淹没在一排不可删的行里。
const priceRows = computed<PriceRow[]>(() => {
  const q = priceSearch.value.trim().toLowerCase()
  const rows: PriceRow[] = []
  for (const p of providers.value) {
    if (priceProvider.value && p.id !== priceProvider.value) continue
    for (const m of modelsMap[p.id] ?? []) {
      if (!isPriced(m)) continue
      if (
        q &&
        !m.model_id.toLowerCase().includes(q) &&
        !(m.display_name ?? '').toLowerCase().includes(q)
      )
        continue
      rows.push({ providerId: p.id, providerName: p.name || p.slug, model: m })
    }
  }
  return rows
})

const priceTotalPages = computed(() => Math.max(1, Math.ceil(priceRows.value.length / PRICE_PAGE_SIZE)))

const pricePageRows = computed(() => {
  const start = (pricePage.value - 1) * PRICE_PAGE_SIZE
  return priceRows.value.slice(start, start + PRICE_PAGE_SIZE)
})

// 筛选条件一变就回到第一页：留在旧页码会看到空表（页码已超出新结果集）。
watch([priceProvider, priceSearch], () => {
  pricePage.value = 1
})

function isPriced(m: UpstreamModel): boolean {
  return m.price_input > 0 || m.price_cache_hit > 0 || m.price_output > 0
}

// 单价单元格：条目里的某一档没填（如缓存命中留空=按输入价计）时写「未配置」，
// 不写 0.00 —— 那会被读成「确认这一档免费」。
function priceCell(v: number): string {
  return v > 0 ? fmtMoney(v) : '未配置'
}

// 模型总数（跨全部供应商，不随筛选变化）：空态要区分
// 「一个模型都没有」和「有模型但还没配价」。
const totalModelCount = computed(() =>
  providers.value.reduce((n, p) => n + (modelsMap[p.id]?.length ?? 0), 0),
)

async function loadPrices() {
  pricesLoading.value = true
  try {
    providers.value = await api.providers()
    // 一次取回全部模型再本地按 provider_id 分组，替代逐 provider 取模型的 1+N 请求
    const all = await api.allModels()
    const grouped: Record<string, UpstreamModel[]> = {}
    for (const m of all) (grouped[m.provider_id] ??= []).push(m)
    for (const p of providers.value) modelsMap[p.id] = grouped[p.id] ?? []
    pricesLoaded.value = true
  } catch (e) {
    if ((e as { status?: number }).status !== 401)
      toast('加载模型价格失败：' + (e as Error).message, 'err')
  } finally {
    pricesLoading.value = false
  }
}

// 价格分类懒加载：进页面不必为一个可能不看的分类多打一轮接口。
watch(tab, (k) => {
  if (k === 'price' && !pricesLoaded.value) loadPrices()
})

const pForm = reactive({
  open: false,
  editing: false, // true = 改既有条目：供应商与模型锁定，只动价格
  providerId: '',
  modelId: '',
  price_input: 0,
  price_cache_hit: 0,
  price_output: 0,
  saving: false,
})

const modalModels = computed(() => modelsMap[pForm.providerId] ?? [])

// 换供应商必须清掉已选模型：否则上一家的 modelId 会跟着提交，
// 把价格写到另一个供应商的模型上（两个下拉看着不匹配，数据却落库了）。
function onPriceProviderChange() {
  pForm.modelId = ''
}

// 新增只列「还没有价格条目」的模型（有条目的去列表里编辑，避免同一条目两处入口）；
// 编辑时模型锁定，选项就是那一个。
const modalModelOptions = computed(() =>
  pForm.editing ? modalModels.value : modalModels.value.filter((m) => !isPriced(m)),
)

function resetPriceForm() {
  pForm.providerId = ''
  pForm.modelId = ''
  pForm.price_input = 0
  pForm.price_cache_hit = 0
  pForm.price_output = 0
}

function openAddPrice() {
  resetPriceForm()
  pForm.editing = false
  // 默认停在「还有模型没配价」的第一个供应商，省掉一次手动切换
  const withStock = providers.value.find((p) =>
    (modelsMap[p.id] ?? []).some((m) => !isPriced(m)),
  )
  pForm.providerId = (withStock ?? providers.value[0])?.id ?? ''
  pForm.open = true
}

function openEditPrice(r: PriceRow) {
  pForm.editing = true
  pForm.providerId = r.providerId
  pForm.modelId = r.model.id
  pForm.price_input = r.model.price_input
  pForm.price_cache_hit = r.model.price_cache_hit
  pForm.price_output = r.model.price_output
  pForm.open = true
}

// 新增流程里换模型不需要回填：可选的都是还没配价的模型（回填也全是 0），
// 已有价的条目走「编辑」，那条路径按库里的现值填（见 openEditPrice）。

async function savePrice() {
  if (!pForm.modelId) {
    toast('请选择要设置价格的模型', 'err')
    return
  }
  const body = {
    price_input: Number(pForm.price_input) || 0,
    price_cache_hit: Number(pForm.price_cache_hit) || 0,
    price_output: Number(pForm.price_output) || 0,
  }
  if (body.price_input < 0 || body.price_cache_hit < 0 || body.price_output < 0) {
    toast('单价不能为负数', 'err')
    return
  }
  // 三项全 0 = 不生成条目（列表里不会出现该行），属于「什么都没填」而不是有效价格。
  if (body.price_input === 0 && body.price_cache_hit === 0 && body.price_output === 0) {
    toast('至少填写一项单价', 'err')
    return
  }
  pForm.saving = true
  try {
    await api.updateModel(pForm.modelId, body)
    toast('价格已保存')
    pForm.open = false
    await loadPrices()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('保存失败：' + (e as Error).message, 'err')
  } finally {
    pForm.saving = false
  }
}

// 删除价格：行内按钮与编辑弹窗共用。确认后把三项单价清零（= 未配置）。
//
// 文案必须说清「只影响以后」：历史费用在落库时已按当时的价固化，删价不会
// 把已经算好的金额抹掉。写成「该模型的用量按 0 元计」会让人以为历史费用
// 会一起归零 —— 那是界面在说谎，且一删就查不回来。
async function deletePrice(modelId: string, label: string): Promise<boolean> {
  const ok = await confirmBox({
    title: `删除「${label}」的价格？`,
    body: '删除后该模型此后的用量按 0 元计；已经产生的费用按当时的价固化，不受影响。',
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return false
  try {
    await api.updateModel(modelId, { price_input: 0, price_cache_hit: 0, price_output: 0 })
    toast('价格已删除')
    return true
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('删除失败：' + (e as Error).message, 'err')
    return false
  }
}

async function clearPrice(r: PriceRow) {
  if (await deletePrice(r.model.id, `${r.providerName} / ${r.model.model_id}`)) {
    await loadPrices()
  }
}

// 编辑弹窗里的「删除价格」：以**库里已存**的价格为准（不是输入框里的草稿），
// 否则把输入清空后按钮就消失了，恰恰是在最需要删除的时候。
const modalModel = computed(() => modalModels.value.find((m) => m.id === pForm.modelId))
const modalHasPrice = computed(() => !!modalModel.value && isPriced(modalModel.value))

async function deletePriceFromModal() {
  const m = modalModel.value
  if (!m) return
  const p = providers.value.find((x) => x.id === pForm.providerId)
  const label = `${p ? p.name || p.slug : ''} / ${m.model_id}`
  if (!(await deletePrice(m.id, label))) return
  pForm.open = false
  await loadPrices()
}

async function refresh() {
  if (tab.value === 'price') await loadPrices()
  else await load()
}

onMounted(() => {
  load()
})
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>设置</h1>
        <div class="sub">{{ tabSub }}</div>
      </div>
      <div class="head-actions">
        <button class="btn" :disabled="loading && tab !== 'price'" @click="refresh">刷新</button>
      </div>
    </div>

    <!-- 分类页签：设置项按类别分页，避免一页塞满互不相干的表单 -->
    <!-- 页签改用共享 Tabs（2026-10-12）：原先是 .set-tabs + 手写 v-for，
         与 Wallet / UsersGroups 各写一份。 -->
    <Tabs v-model="tab" :tabs="TABS" aria-label="设置分区" />

    <!-- 分类 1：模型默认 -->
    <div v-if="tab === 'model'" class="panel">
      <div v-if="loading" class="loading">加载中…</div>
      <!-- 读取失败：显示持久错误态而不是那组兜底常量。
           必须让「没读到」与「读到了」在界面上明确区分 —— 这里的表单可写，
           拿着默认值点保存会覆盖真实配置（见 err 的注释）。 -->
      <div v-else-if="err" class="empty">
        <div class="big">⚠</div>
        设置加载失败：{{ err }}
        <div style="margin-top: 10px">表单中的数值是默认值而非当前配置，已阻止保存以免覆盖。</div>
        <button class="btn btn-primary" type="button" style="margin-top: 10px" @click="load">重新加载</button>
      </div>
      <form v-else class="form-grid" style="max-width: 560px" @submit.prevent="save('model')">
        <div class="field span2">
          <label>默认上下文窗口（tokens）</label>
          <input v-model.number="form.default_context_window" class="input num" type="number" min="1" step="1" />
          <span class="tip">添加模型时若上游未暴露 context_length，则用此值</span>
        </div>
        <div class="field span2">
          <label>默认最大输出（tokens）</label>
          <input v-model.number="form.default_max_output_tokens" class="input num" type="number" min="1" step="1" />
          <span class="tip">添加模型时若上游未暴露 max_completion_tokens，则用此值</span>
        </div>
        <div class="field span2 form-actions" style="padding: 0">
          <button type="submit" class="btn btn-primary" :disabled="saving">
            {{ saving ? '保存中…' : '保存设置' }}
          </button>
        </div>
      </form>
    </div>

    <!-- 分类 2：运行时（超时 + 故障转移） -->
    <div v-else-if="tab === 'runtime'" class="panel">
      <div v-if="loading" class="loading">加载中…</div>
      <div v-else-if="err" class="empty">
        <div class="big">⚠</div>
        设置加载失败：{{ err }}
        <div style="margin-top: 10px">表单中的数值是默认值而非当前配置，已阻止保存以免覆盖。</div>
        <button class="btn btn-primary" type="button" style="margin-top: 10px" @click="load">重新加载</button>
      </div>
      <!-- 运行时按「谁在保护什么」分块（2026-10-13）。
           原来 5 个旋钮挤在一张表里，只靠两个小标题分隔；更要紧的是它们
           **失效后果不同**：改错超时是「请求变慢/被切走」，改错故障转移是
           「一直重试坏目标」。块与块之间的分隔必须能一眼看出这是两类东西，
           所以用带边框的卡片而不是行内标题。 -->
      <form v-else class="rt-form" @submit.prevent="save('runtime')">
        <!-- 分类 1：非流式 -->
        <section class="rt-card">
          <div class="rt-card-h">
            <div class="rt-card-t">
              <h3>非流式请求超时</h3>
              <p>不流式调用（stream=false）时，一次请求最多等多久。超时按可转移错误处理。</p>
            </div>
            <button
              type="button"
              class="btn btn-ghost btn-sm"
              :disabled="groupAtDefaults(RUNTIME_GROUPS.nonstream)"
              @click="resetRuntimeGroup(RUNTIME_GROUPS.nonstream)"
            >
              恢复推荐默认
            </button>
          </div>
          <div class="field">
            <label>整体超时（毫秒）</label>
            <input
              v-model.number="form.upstream_timeout_ms"
              class="input num"
              type="number"
              min="1"
              max="86400000"
              step="1"
            />
            <span class="tip">
              超时后按可转移错误处理 —— 有机会切到下一个上游，而不是直接失败。
              这是<b>单次尝试</b>的预算，不是整请求的上限（见下方「派生上限」）。
            </span>
            <span class="tip rec">{{ recNote('upstream_timeout_ms') }}</span>
          </div>
        </section>

        <!-- 分类 2：流式 -->
        <section class="rt-card">
          <div class="rt-card-h">
            <div class="rt-card-t">
              <h3>流式看门狗</h3>
              <p>流式调用时盯住「迟迟不出字」与「出到一半卡死」，两个是不同的时间点。</p>
            </div>
            <button
              type="button"
              class="btn btn-ghost btn-sm"
              :disabled="groupAtDefaults(RUNTIME_GROUPS.streaming)"
              @click="resetRuntimeGroup(RUNTIME_GROUPS.streaming)"
            >
              恢复推荐默认
            </button>
          </div>
          <div class="field">
            <label>首字超时（TTFT，毫秒）</label>
            <input
              v-model.number="form.stream_first_token_timeout_ms"
              class="input num"
              type="number"
              min="1"
              max="86400000"
              step="1"
            />
            <span class="tip">
              已建立连接后，首个事件迟迟不来即掐流并切换下一个上游。
              <b>仅在尚未写出任何字节时生效</b>；带思考的上游首字本来就慢，别调太紧。
            </span>
            <span class="tip rec">{{ recNote('stream_first_token_timeout_ms') }}</span>
          </div>
          <div class="field">
            <label>流式空闲超时（毫秒）</label>
            <input
              v-model.number="form.stream_idle_timeout_ms"
              class="input num"
              type="number"
              min="1"
              max="86400000"
              step="1"
            />
            <span class="tip">
              已开始出字后，多久没有任何新事件即判定卡死（按截断收尾）。
              流已经发给客户端了，此时不会切换上游，只会以截断结束。
            </span>
            <span class="tip rec">{{ recNote('stream_idle_timeout_ms') }}</span>
          </div>
        </section>

        <!-- 分类 3：故障转移 -->
        <section class="rt-card">
          <div class="rt-card-h">
            <div class="rt-card-t">
              <h3>自动故障转移</h3>
              <p>一个目标不行时，最多换几个、什么情况下先摘掉它。</p>
            </div>
            <button
              type="button"
              class="btn btn-ghost btn-sm"
              :disabled="groupAtDefaults(RUNTIME_GROUPS.failover)"
              @click="resetRuntimeGroup(RUNTIME_GROUPS.failover)"
            >
              恢复推荐默认
            </button>
          </div>
          <div class="field">
            <label>最多尝试目标数</label>
            <input
              v-model.number="form.failover_max_targets"
              class="input num"
              type="number"
              min="1"
              max="100"
              step="1"
            />
            <span class="tip">
              一次请求最多打某条路由链上的几个目标。每多一个，最坏耗时就多乘一截
              （见下方「派生上限」）。是否启用按路由单独开关（路由页）。
            </span>
            <span class="tip rec">{{ recNote('failover_max_targets') }}</span>
          </div>
          <div class="field">
            <label>失败熔断阈值</label>
            <input
              v-model.number="form.failover_failure_threshold"
              class="input num"
              type="number"
              min="1"
              max="1000"
              step="1"
            />
            <span class="tip">
              某目标连续失败几次后临时摘除（60 秒冷却），期间请求自动让位给链上下一个目标。
              调高等于继续把请求送给已经坏掉的目标。
            </span>
            <span class="tip rec">{{ recNote('failover_failure_threshold') }}</span>
          </div>
        </section>

        <!-- 分类 4：派生值（只读） -->
        <section class="rt-card ro">
          <div class="rt-card-h">
            <div class="rt-card-t">
              <h3>
                整请求总预算
                <span class="badge badge-off">自动推导 · 不可配</span>
              </h3>
              <p>一次请求跨所有故障转移尝试的总时长上限，由上面三类值推导，不是独立旋钮。</p>
            </div>
          </div>
          <!-- 不说明的后果是运维把「非流式整体超时」当成整请求上限，
               调小它却发现流式请求照样能挂很久 —— 那正是修复前「设置旋钮管不到
               流式」造成的误解。 -->
          <div class="derived">
            <span class="derived-k">当前生效</span>
            <span class="derived-v">{{ derivedTotalBudgetText }}</span>
            <span class="tip">
              公式：（max(非流式整体超时, 首字超时 + 等响应头 60 秒) + 5 秒）×（最多尝试目标数 + 1），
              再夹在 30 秒 ~ 30 分钟之间。
              没有它，一串半死的上游可以把一个客户端挂住数分钟，期间持续占用连接、配额预占和故障转移名额。
              <b>注意</b>：上面那些超时都是<b>单次尝试</b>的预算，不是整请求的。
            </span>
          </div>
        </section>

        <div class="rt-actions">
          <button type="submit" class="btn btn-primary" :disabled="saving">
            {{ saving ? '保存中…' : '保存设置' }}
          </button>
        </div>
      </form>

      <!-- 用量归档维护：与上面的表单无关（不保存任何设置项，是立即执行的动作），
           单独成块放在运行时页签下，而不是让运维去 curl /usage/prune。 -->
      <div v-if="!loading" class="maint">
        <h3 class="maint-h">用量归档</h3>
        <p class="tip">
          调用明细默认保留 30 天，更早的记录由每日定时任务聚合进「按天累计」后删除
          （累计永久保留）。这里可以手动立即执行一次剪枝，结果实时回显。
        </p>
        <button class="btn" :disabled="pruning" @click="runPrune">
          {{ pruning ? '剪枝中…' : '立即归档剪枝' }}
        </button>
        <div v-if="pruneResult" class="prune-result">
          <template v-if="pruneResult.skipped">本次跳过：{{ pruneResult.reason || '无待归档的数据' }}</template>
          <template v-else>
            已删除明细 <b>{{ fmtNum(pruneResult.deleted_rows) }}</b> 行，写入按天累计
            <b>{{ fmtNum(pruneResult.rollup_rows) }}</b> 组；归档水位
            {{ pruneResult.pruned_through_day }}（保留窗口自 {{ pruneResult.cutoff_day }} 起）
          </template>
        </div>
      </div>
    </div>

    <!-- 分类 3：导入 / 导出（页签顺序见 TABS） -->
    <div v-else-if="tab === 'transfer'" class="panel">
      <div class="xfer-grid">
        <!-- 导出 -->
        <section class="xfer-card">
          <h3>导出</h3>
          <p class="xfer-note">
            导出当前全部供应商、模型、它们的参数（超时、重试、协议、端点、价格）
            以及<b>上游 API Key</b>。文件用口令整体加密，产物是
            <code>.json.enc</code> —— 不设口令无法导出。
            <b>不含路由与下游访问密钥</b>：路由的公开名是给调用方看的契约，
            跨环境照抄容易撞名；下游密钥属于本库的身份体系，不随文件走。
          </p>

          <div class="field">
            <label>加密口令（必填，至少 8 位）</label>
            <input
              v-model="exportPass"
              class="input"
              type="password"
              autocomplete="new-password"
              placeholder="导入这份文件时要用同一个口令"
            />
            <span class="tip">
              文件里包含全部上游 API Key，口令是唯一的保护 —— 请用区别于登录密码的独立口令，
              并把口令与文件分开传递。
            </span>
          </div>

          <button
            class="btn btn-primary"
            type="button"
            :disabled="transferBusy || exportPass.trim().length < 8"
            @click="doExport"
          >
            {{ transferBusy ? '处理中…' : '导出为加密文件' }}
          </button>
        </section>

        <!-- 导入 -->
        <section class="xfer-card">
          <h3>导入</h3>
          <p class="xfer-note">
            选择一份此前导出的文件。先「预演」看清会发生什么，确认后才真正写入。
          </p>

          <div class="field">
            <label>导出文件</label>
            <input class="input" type="file" accept=".json,.json.enc,.enc,application/json" @change="onPickFile" />
            <span v-if="importFile" class="tip">已选择：{{ importFile.name }}</span>
          </div>

          <div v-if="importParsed?.encrypted" class="field">
            <label>文件口令 *</label>
            <input
              v-model="importPass"
              class="input"
              type="password"
              autocomplete="off"
              placeholder="导出时设置的那个"
              @input="importPreview = null"
            />
            <span class="tip">该文件已加密，必须输入导出时用的口令</span>
            <button
              class="btn btn-sm"
              type="button"
              :disabled="!importPass.trim() || transferBusy"
              @click="runImport(true)"
            >
              预演导入
            </button>
          </div>

          <div v-if="importError" class="err-line">{{ importError }}</div>

          <div v-if="importParsed && !importParsed.encrypted" class="xfer-file-stat">
            文件内容：{{ importParsed.providers?.length ?? 0 }} 个供应商、
            {{ importParsed.models?.length ?? 0 }} 个模型、
            {{ importParsed.credentials?.length ?? 0 }} 条凭据
            <span v-if="importParsed.version !== 1" class="warn-line">
              （文件版本 {{ importParsed.version }}，可能来自更新的网关）
            </span>
          </div>

          <!-- 预演结果：逐项说明，而不是只给一个总数 -->
          <div v-if="importPreview" class="xfer-preview">
            <h4>
              {{
                importPreview.dry_run ? '预演结果（尚未写入）' : '导入结果'
              }}
            </h4>
            <ul>
              <li>新增供应商 <b>{{ importPreview.providers_created }}</b></li>
              <li>新增模型 <b>{{ importPreview.models_created }}</b></li>
              <li v-if="importPreview.credentials_added">
                新增凭据 <b>{{ importPreview.credentials_added }}</b>
              </li>
              <li v-if="importPreview.models_skipped">
                跳过已存在的模型 <b>{{ importPreview.models_skipped }}</b>
              </li>
            </ul>

            <div v-if="Object.keys(importPreview.providers_renamed ?? {}).length" class="xfer-renames">
              重命名（原有数据不受影响）：
              <ul>
                <li v-for="[from, to] in Object.entries(importPreview.providers_renamed ?? {})" :key="from">
                  <code>{{ from }}</code> → <code>{{ to }}</code>
                </li>
              </ul>
            </div>

            <details v-if="importPreview.notes?.length" class="xfer-notes">
              <summary>说明（{{ importPreview.notes.length }} 条）</summary>
              <ul><li v-for="(n, i) in importPreview.notes" :key="i">{{ n }}</li></ul>
            </details>

            <details v-if="importPreview.warnings?.length" class="xfer-notes">
              <summary>警告（{{ importPreview.warnings.length }} 条）</summary>
              <ul><li v-for="(n, i) in importPreview.warnings" :key="i">{{ n }}</li></ul>
            </details>

            <button
              v-if="importPreview.dry_run"
              class="btn btn-primary"
              type="button"
              :disabled="transferBusy"
              @click="confirmImport"
            >
              确认导入
            </button>
            <button v-else class="btn" type="button" @click="resetImport">再导一个</button>
          </div>
        </section>
      </div>
    </div>

    <!-- 修改密码（2026-10-13 从「钱包与充值」页挪来）。
         必须在模型价格那个「兜底 v-else」之前，否则永远轮不到它。 -->
    <div v-else-if="tab === 'password'" class="panel">
      <div class="panel-title">
        修改密码
        <span class="hint">修改后当前登录状态会立即失效，需要用新密码重新登录</span>
      </div>
      <form class="form" style="max-width: 420px" @submit.prevent="submitPwd">
        <label class="flabel" for="op">当前密码</label>
        <input id="op" v-model="oldPwd" type="password" class="input" autocomplete="current-password" />

        <label class="flabel" for="np">新密码</label>
        <input id="np" v-model="newPwd" type="password" class="input" autocomplete="new-password" />
        <!-- 强度提示在提交前就给出：等后端 400 再说一次，用户已经白填了一遍。
             但前端提示只作提前告知，真正的判定仍在服务端（见 checkStrength）。 -->
        <div v-if="checkStrength(newPwd)" class="fhint warn">{{ checkStrength(newPwd) }}</div>
        <div v-else-if="newPwd" class="fhint ok">强度符合要求</div>

        <label class="flabel" for="cp">确认新密码</label>
        <input id="cp" v-model="confirmPwd" type="password" class="input" autocomplete="new-password" />

        <div v-if="pwdErr" class="fhint err">{{ pwdErr }}</div>

        <button class="btn btn-primary" type="submit" :disabled="pwdBusy">
          {{ pwdBusy ? '提交中…' : '修改密码' }}
        </button>
      </form>
    </div>

    <!-- 分类 4：模型价格（兜底分支 v-else，页签顺序见 TABS） -->
    <div v-else class="panel">
      <div class="price-head">
        <div>
          <div class="price-title">模型价格</div>
          <div class="tip">
            只列出已配置的价格条目；没配价格的模型不在此列，费用一律按 0 元计。
            单位：元 / 百万 tokens，缓存命中留空则按输入价计。
            同一模型在不同供应商下价格可能不同，按「供应商 × 模型」分别配置。
          </div>
        </div>
        <button
          class="btn btn-primary"
          type="button"
          :disabled="providers.length === 0"
          @click="openAddPrice"
        >
          新增价格
        </button>
      </div>

      <div class="price-filters">
        <select v-model="priceProvider" class="select price-sel">
          <option value="">全部供应商</option>
          <option v-for="p in providers" :key="p.id" :value="p.id">{{ p.name || p.slug }}</option>
        </select>
        <input v-model="priceSearch" class="input" placeholder="搜索价格条目…" />
      </div>

      <div v-if="pricesLoading" class="loading">加载中…</div>
      <div v-else-if="totalModelCount === 0" class="empty">还没有任何模型 —— 请先在「上游」页添加模型</div>
      <div v-else-if="priceRows.length === 0" class="empty">
        <template v-if="priceProvider || priceSearch.trim()">没有匹配的价格条目</template>
        <template v-else>
          暂无价格条目 —— 点右上角「新增价格」配置单价；未配置价格的模型，费用一律按 0 元计
        </template>
      </div>
      <template v-else>
        <div class="tbl-wrap">
          <table class="tbl">
            <thead>
              <tr>
                <th>供应商</th>
                <th>模型</th>
                <th class="num-h">输入（元/1M）</th>
                <th class="num-h">缓存命中（元/1M）</th>
                <th class="num-h">输出（元/1M）</th>
                <th class="c-op">操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="r in pricePageRows" :key="r.model.id">
                <td>{{ r.providerName }}</td>
                <td class="mono">
                  {{ r.model.model_id }}
                  <span v-if="r.model.display_name" class="dim">（{{ r.model.display_name }}）</span>
                </td>
                <td class="num-h" :class="{ unset: r.model.price_input <= 0 }">
                  {{ priceCell(r.model.price_input) }}
                </td>
                <td class="num-h" :class="{ unset: r.model.price_cache_hit <= 0 }">
                  {{ priceCell(r.model.price_cache_hit) }}
                </td>
                <td class="num-h" :class="{ unset: r.model.price_output <= 0 }">
                  {{ priceCell(r.model.price_output) }}
                </td>
                <td class="c-op">
                  <button class="btn btn-sm btn-ghost" type="button" @click="openEditPrice(r)">编辑</button>
                  <!-- 列表里只有已配价的条目，删除始终可用；删完该行即从列表消失 -->
                  <button
                    class="btn btn-sm btn-danger"
                    type="button"
                    title="删除该价格条目"
                    @click="clearPrice(r)"
                  >
                    删除价格
                  </button>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <div class="pager">
          <button class="btn btn-sm" :disabled="pricePage <= 1" @click="pricePage = 1">首页</button>
          <button class="btn btn-sm" :disabled="pricePage <= 1" @click="pricePage--">上一页</button>
          <span class="pager-info">
            第 <b>{{ pricePage }}</b> / {{ priceTotalPages }} 页 · 共 {{ priceRows.length }} 条
          </span>
          <button class="btn btn-sm" :disabled="pricePage >= priceTotalPages" @click="pricePage++">下一页</button>
          <button
            class="btn btn-sm"
            :disabled="pricePage >= priceTotalPages"
            @click="pricePage = priceTotalPages"
          >
            末页
          </button>
        </div>
      </template>
    </div>

    <!-- 价格：新增 / 编辑（表单填一半别丢，遮罩/Esc 都不关） -->
    <AppModal
      :open="pForm.open"
      :title="pForm.editing ? '编辑模型价格' : '新增模型价格'"
      max-width="520px"
      :dismissable="false"
    >
      <form @submit.prevent="savePrice">
        <div class="form-grid">
          <div class="field span2">
            <label>供应商 *</label>
            <select
              v-model="pForm.providerId"
              class="select"
              :disabled="pForm.editing"
              @change="onPriceProviderChange"
            >
              <option value="" disabled>请选择供应商</option>
              <option v-for="p in providers" :key="p.id" :value="p.id">{{ p.name || p.slug }}</option>
            </select>
            <span class="tip">同一个 model_id 在不同供应商下价格可能不同</span>
          </div>
          <div class="field span2">
            <label>模型 *</label>
            <select v-model="pForm.modelId" class="select" :disabled="pForm.editing">
              <option value="" disabled>{{ pForm.providerId ? '请选择模型' : '请先选择供应商' }}</option>
              <option v-for="m in modalModelOptions" :key="m.id" :value="m.id">{{ m.model_id }}</option>
            </select>
            <span class="tip">
              <template v-if="pForm.editing">条目归属的供应商与模型不可改；要换模型请先删除再新增</template>
              <template v-else-if="modalModels.length > 0 && modalModelOptions.length === 0">
                该供应商下的模型都已有价格条目，请到列表中编辑
              </template>
              <template v-else>只列出还没有价格条目的模型</template>
            </span>
          </div>
          <div class="field">
            <label>输入价格（元/1M）</label>
            <input
              v-model.number="pForm.price_input"
              class="input num"
              type="number"
              min="0"
              step="0.001"
              placeholder="0 = 不计费"
            />
            <span class="tip">缓存未命中的输入 token</span>
          </div>
          <div class="field">
            <label>缓存命中价格（元/1M）</label>
            <input
              v-model.number="pForm.price_cache_hit"
              class="input num"
              type="number"
              min="0"
              step="0.001"
              placeholder="0 = 同输入价"
            />
            <span class="tip">留空/填 0 时按输入价计</span>
          </div>
          <div class="field span2">
            <label>输出价格（元/1M）</label>
            <input
              v-model.number="pForm.price_output"
              class="input num"
              type="number"
              min="0"
              step="0.001"
              placeholder="0 = 不计费"
            />
          </div>
        </div>
        <div class="form-actions" :class="{ spread: modalHasPrice }">
          <!-- 库里已有价才给删除入口（以库为准，不看输入框草稿） -->
          <button
            v-if="modalHasPrice"
            type="button"
            class="btn btn-danger"
            @click="deletePriceFromModal"
          >
            删除价格
          </button>
          <div class="actions-right">
            <button type="button" class="btn btn-ghost" @click="pForm.open = false">取消</button>
            <button type="submit" class="btn btn-primary" :disabled="pForm.saving || !pForm.modelId">
              {{ pForm.saving ? '保存中…' : '保存' }}
            </button>
          </div>
        </div>
      </form>
    </AppModal>
  </main>
</template>

<style scoped>
/* 运行时分类块（2026-10-13）
   分隔用「卡片」而不是行内小标题：这几类的失效后果不同（改错超时 =
   请求变慢/被切走，改错故障转移 = 一直重试坏目标），视觉上必须能一眼
   分出这是两类东西，才不会在改一个值时顺手以为在调另一个。 */
.rt-form {
  display: flex;
  flex-direction: column;
  gap: 14px;
  max-width: 680px;
}
.rt-card {
  padding: 16px 18px;
  border: 1px solid var(--border);
  border-radius: var(--r-card);
  background: var(--surface);
}
.rt-card-h {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 14px;
  margin-bottom: 14px;
}
.rt-card-h h3 {
  display: flex;
  align-items: center;
  gap: 8px;
  margin: 0;
  font-size: 14px;
  font-weight: 600;
}
.rt-card-h p {
  /* 说明文字限宽：铺满整行会把右侧的「恢复推荐默认」推到很远，
     眼睛要在一条长行里来回找按钮。 */
  max-width: 46ch;
  margin: 5px 0 0;
  font-size: 12px;
  line-height: 1.55;
  color: var(--text-3);
}
.rt-card-h .btn {
  flex: none;
}
.rt-card .field + .field {
  margin-top: 14px;
}
/* 推荐值单独一行：它是「参考」而不是「约束」，与描述文字同色系但更轻，
   避免看起来像一条必须遵守的规则。 */
.rt-card .rec {
  color: var(--text-4);
  font-variant-numeric: tabular-nums;
}
/* 只读派生块：没有输入框，用弱一档的底色与可配卡片区分。 */
.rt-card.ro {
  background: var(--field-background);
}
.derived {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.derived-k {
  font-size: 12.5px;
  font-weight: 550;
  color: var(--muted);
}
.derived-v {
  font-size: 20px;
  font-weight: 650;
  font-variant-numeric: tabular-nums;
  letter-spacing: -0.01em;
}
.derived .tip {
  margin-top: 4px;
  line-height: 1.6;
}
.rt-actions {
  display: flex;
  justify-content: flex-end;
}

/* 用量归档维护块：与上方表单分隔，宽度对齐上面的卡片列 */
.maint {
  max-width: 680px;
  margin-top: 18px;
  padding-top: 14px;
  border-top: 1px solid var(--hairline);
}
.maint-h {
  margin: 0;
  font-size: 14px;
  font-weight: 600;
}
.maint .tip {
  margin: 8px 0 12px;
  line-height: 1.6;
}
.prune-result {
  margin-top: 10px;
  font-size: 12.5px;
  color: var(--text-2);
}
.prune-result b {
  font-variant-numeric: tabular-nums;
}

/* 窄屏：卡片头部的按钮换到标题下方，别把说明文字挤成一条缝 */
@media (max-width: 640px) {
  .rt-card-h {
    flex-direction: column;
    gap: 10px;
  }
  .rt-card-h .btn {
    align-self: flex-start;
  }
  .rt-actions {
    justify-content: stretch;
  }
}

/* 价格分类：标题行 + 筛选行 */
.price-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 14px;
  margin-bottom: 14px;
}
.price-title {
  font-size: 15px;
  font-weight: 650;
  color: var(--text);
  margin-bottom: 4px;
}
.price-head .tip {
  max-width: 620px;
  line-height: 1.5;
}
.price-filters {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 14px;
}
.price-sel {
  width: 180px;
  flex: none;
}
/* 未配置的单价：明确写「未配置」并弱化，区别于真实价格 */
.unset {
  color: var(--text-4);
}

/* 价格表：与「调用历史」的表格同款 */
.c-op {
  width: 186px;
}
/* 弹窗底部：左侧「删除价格」，右侧取消/保存 */
.form-actions.spread {
  justify-content: space-between;
}
.actions-right {
  display: flex;
  gap: 10px;
}

@media (max-width: 720px) {
  .price-filters {
    flex-wrap: wrap;
  }
  .price-sel {
    width: 100%;
  }
}
</style>
