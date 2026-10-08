<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { api, isAdmin, session } from '../api'
import { toast, confirmBox } from '../ui'
import { fmtDateTime, fmtTokens, copyText } from '../fmt'
import type { AccessKey, Group, KeyCreateResponse, User } from '../types'
import AppModal from '../components/AppModal.vue'
import ModelPicker from '../components/ModelPicker.vue'

const err = ref('')
const loading = ref(true)
const keys = ref<AccessKey[]>([])
// key 级模型白名单的可选项。**惰性加载**：只在打开表单时拉一次，
// 因为多数访问者只是来看一眼列表，不该为这个多打一个请求。
const modelOptions = ref<string[]>([])
const modelsRestricted = ref(false)
let modelsLoaded = false
// 分组下拉（key 级分组覆盖，仅管理员）。惰性加载。
const groupOptions = ref<Group[]>([])

/** 当前登录者，用于在新建表单里回显「这把密钥归你」。
 *
 * 不拉 /users 来做归属选择：归属不是可选项，后端 Create 强制
 * owner = 创建者自己并静默忽略请求里的 user_id，PATCH 的载荷里也没有
 * user_id —— 所以这里要显示的只是「我自己」，那正是 session 里已有的。
 * 拉一次全量用户列表既没意义，普通用户还会因此吃一个 403。 */
const meName = computed(() => session.me?.username ?? '')

/** 新建表单里给普通用户看的额度说明。
 *
 * 显示真实数字而不是一句「受限」：用户建完 key 后在列表里会看到
 * 「x / 5000」这样的额度，先在这里给出同一个数，界面才自洽。 */
const quotaHint = computed(() => {
  const q = session.me?.quota_tokens ?? 0
  return q > 0 ? fmtTokens(q) : '不限（管理员未为你设置额度上限）'
})

const DAY_MS = 86_400_000

/**
 * 剩余天数 ⇄ 绝对时间戳。
 *
 * 界面用「天数」而不是 datetime-local：后者的值是**无时区的本地时间**，
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
  return n > 0 ? Date.now() + n * DAY_MS : 0
}

/** isExpired 是否已过期。
 *
 * expires_at = 0 是「永不过期」，必须与「刚好过期」区分开：
 * 数据面对前者放行、对后者一律 403，两者混为一谈会把好端端的密钥当成死的。 */
function isExpired(k: AccessKey): boolean {
  return k.expires_at > 0 && k.expires_at <= Date.now()
}

/** rateText 把限速拼成一行。两个上限都是 0 时返回空串，
 * 由界面自己决定显示「不限」—— 空串和「不限」在语义上不是一回事，
 * 不能让一个函数替界面做这个决定。 */
function rateText(k: AccessKey): string {
  const parts: string[] = []
  if ((k.rpm_limit ?? 0) > 0) parts.push(`${k.rpm_limit} req/min`)
  if ((k.tpm_limit ?? 0) > 0) parts.push(`${fmtTokens(k.tpm_limit)} tok/min`)
  return parts.join(' · ')
}

/** anyLimit 这把 key 是否设了任何一种限制。用来兜底显示「不限」——
 * 四种限制全空时必须明说，否则「什么都没配」会被读成「没配成功」。 */
function anyLimit(k: AccessKey): boolean {
  return (
    k.allowed_models.length > 0 ||
    !!k.allowed_ips ||
    !!k.group_id ||
    (k.rpm_limit ?? 0) > 0 ||
    (k.tpm_limit ?? 0) > 0
  )
}

async function ensureModelOptions() {
  if (modelsLoaded) return
  try {
    const r = await api.modelNames()
    modelOptions.value = r.models
    modelsRestricted.value = r.restricted
    modelsLoaded = true
  } catch {
    // 拉不到就把选择器留空并说明原因，不阻塞建 key ——
    // 白名单是可选的收紧手段，不该成为建 key 的前置条件。
    modelOptions.value = []
  }
}

// 一次性明文密钥展示
const plainKeyBox = reactive<{ open: boolean; key: string; name: string }>({ open: false, key: '', name: '' })

// ---------- 按用户筛选 + 按用量降序 ----------
//
// 两个维度**互相独立**：筛选决定「看谁的行」，排序决定「行怎么排」。
// 用户常在同一次排查里同时用两个 —— 先筛出某个人的全部 key，再在其中
// 找最耗的那把。所以这里不把排序做成筛选下拉里的一个选项，而是让筛选
// 只管筛选、排序恒定生效（与「用量降序」这条硬需求对齐）。

/** 当前筛选的归属用户 id。空串 = 不过滤。
 *
 *  值域用 **user_id 而不是 username**：username 可被管理员改名
 *  （Users 页不允许改，但那只是界面约束），拿名字当主键等于让筛选
 *  在改名后静默失配。user_id 是不可变的主键。 */
const fUser = ref('')

/** 筛选下拉的选项：可归属的用户清单。**惰性加载**，与 modelOptions /
 *  groupOptions 同一套口径（只在打开表单/首次筛选时才拉）。
 *
 *  **普通用户看不到这个下拉**：/admin/api/users 是 admin-only
 *  （user_handler.ListUsers 走 requireAdmin），普通用户调它只会白吃一个
 *  403，而他们的列表本来就只有自己的 key（key_handler.List 对非管理员
 *  强制按 user_id 收窄），筛「按用户」对他们恒等于「全部用户」。
 *  给一个只有自己一个选项的下拉是纯噪音，所以按 isAdmin() 隐藏，
 *  连请求都不发。 */
const userOptions = ref<User[]>([])
let usersLoaded = false

/** ensureUserOptions 拉一次用户清单供筛选下拉用。
 *
 *  失败**不阻塞列表**：筛选是可选的收窄手段，拉不到就让下拉只剩
 *  「全部用户」，其余功能照常（与 ensureModelOptions 同口径）。
 *
 *  **这里不过滤管理员**：userOptions 同时承担「把 user_id 翻译成用户名」
 *  的职责（列表行要显示归属），过滤掉会让管理员的 key 在列表里显示成
 *  「用户已不存在」—— 那是假的。过滤只发生在**下拉选项**那一层，
 *  见 userFilterOptions。 */
async function ensureUserOptions() {
  if (usersLoaded || !isAdmin()) return
  try {
    userOptions.value = await api.users()
    usersLoaded = true
  } catch {
    /* 拉不到就只留「全部用户」，不阻塞列表与其它操作 */
  }
}

/**
 * 无归属 key 的筛选哨兵值。
 *
 * 用一个前端自造的哨兵而不是拿空串当哨兵：空串是「不过滤」的约定，
 * 复用它会让「筛无归属」和「筛全部」塌成同一个状态，下拉里也就没法
 * 显示「无归属（不可用）」这个可回选项。带前缀是为了保证不与任何真实
 * user_id 相撞（user_id 是短哈希/生成的，不含这个形态）。
 */
const ORPHAN_FILTER = '\u0000orphan'

/** hasOrphanKeys 列表里是否存在无归属的 key。
 *
 *  没有它就不给「无归属」那个选项 —— 一个永远筛不出东西的选项比没有
 *  更糟（用户会以为是自己看错了）。判据读的是**未筛选**的 keys：
 *  筛选到别的人时无归属 key 同样存在于库里，选项不该跟着消失。 */
const hasOrphanKeys = computed(() => keys.value.some((k) => !k.user_id))

/**
 * userFilterOptions 下拉选项：清单里有谁就列谁，外加**持有 key 但不在
 * 用户清单里**的 id。
 *
 * 为什么要在用户清单之外补这些：列表里可能存在 user_id 非空、但那个用户
 * 已从库里消失的 key（用户被删、迁移遗留）。这些行在任何用户项下都
 * 不出现，用户会以为它们凭空消失了 —— 「看不见」与「不存在」在排障
 * 时是两个完全不同的结论。补一项并标注「用户已不存在」，让这批行始终
 * 可达，且一眼看出归属是坏的。
 *
 * 无归属（user_id 为空）不走这里，它有独立的哨兵项（ORPHAN_FILTER），
 * 因为它的语义与「某个已不存在的用户」不同：前者是**永远鉴权 401**，
 * 后者只是账号没了。
 */
const userFilterOptions = computed(() => {
  // 不做 `known` 的过滤：它必须认识**所有**用户（含管理员），否则把
  // 管理员的 key 误判成「用户已不存在」（见 ensureUserOptions 的注释）。
  const known = new Set(userOptions.value.map((u) => u.id))
  const orphanIds = new Set<string>()
  for (const k of keys.value) {
    if (!k.user_id) continue
    if (!known.has(k.user_id)) orphanIds.add(k.user_id)
  }
  const rows: { id: string; username: string }[] = [
    // 管理员不进下拉（2026-10-10 用户要求）：他已经不能建 key、不能被
    // 认领 key，所以这个选项**永远筛不出东西** —— 一个点了没反应的选项
    // 比没有它更糟，用户会以为是自己看错了（与 ORPHAN_FILTER 只在该类
    // 记录存在时才出现同一个理由）。
    //
    // 过滤只发生在这里，不影响把 user_id 翻译成用户名。
    // 这是**展示层**的便利，不是权限：真正的拦截在后端（PATCH 认领会 400）。
    ...userOptions.value
      .filter((u) => u.role !== 'admin')
      .map((u) => ({ id: u.id, username: u.username })),
    ...[...orphanIds].map((id) => ({ id, username: `${id}（用户已不存在）` })),
  ]
  // 按显示名排：下拉是「找人」用的，字典序比按用量/创建时间都好找。
  // localeCompare 显式给 'zh-CN'：用户名允许中文（登录名受限，但备注名/显示名
  // 不受限），默认 locale 在不同浏览器上顺序不一致。
  return rows.sort((a, b) => a.username.localeCompare(b.username, 'zh-CN'))
})

/**
 * visibleKeys = 先按用户筛选，再按用量**降序**。
 *
 * 为什么在这里排而不是让后端排：列表接口 GET /keys 返回的是全量
 * （管理员）或本人（普通用户），**没有排序参数**，而 used_tokens
 * 每条都有 —— 纯前端排一次即可，不需要新接口（也不该为此动后端）。
 *
 * 降序的理由是运维读这张表的实际用途：找「谁在烧 token」。
 * 按创建时间排时最耗的那把可能沉在第 40 行，按用量排它必在第一屏。
 *
 * **稳定排序**：used_tokens 相等时必须给一个确定的兜底键，否则行的
 * 顺序取决于引擎的排序实现 —— 两把都是 0 用量的 key 在刷新前后可能
 * 互换位置，看起来像「列表自己跳了」（用户对这种抖动的第一反应是
 * 「刚才那把 key 是不是出问题了」）。兜底键取 created_at 升序、
 * 再取 id 升序：id 是后端生成的不变主键，最后一道保证顺序唯一且确定。
 */
const visibleKeys = computed(() => {
  let list = keys.value
  if (fUser.value === ORPHAN_FILTER) list = keys.value.filter((k) => !k.user_id)
  else if (fUser.value) list = keys.value.filter((k) => k.user_id === fUser.value)
  // slice() 先断链：不改排序结果，只是不去就地排原始数组
  // （keys 也被 recomputeUsage 就地改过 used_tokens，就地排会让
  //   列表顺序依赖「用户点过哪些重算按钮」这种无关的历史）。
  return list.slice().sort((a, b) => {
    if (b.used_tokens !== a.used_tokens) return b.used_tokens - a.used_tokens
    if (a.created_at !== b.created_at) return a.created_at - b.created_at
    return a.id < b.id ? -1 : a.id > b.id ? 1 : 0
  })
})

/** 筛选后的空态与「本来就没有 key」必须是两句话。 */
const filteredEmpty = computed(() => keys.value.length > 0 && visibleKeys.value.length === 0)

function resetFilter() {
  fUser.value = ''
}

/** 切筛选器时兜底把用户清单拉齐（首次点击才发请求）。 */
function onUserFilterOpen() {
  void ensureUserOptions()
}

onMounted(() => {
  void load()
  // 管理员进页面就把筛选用的用户清单拉好：下拉是这一页的新增控件，
  // 第一次点开时若还没加载，选项会是空的「只有全部用户」，看起来像
  // 「没有别的用户」。一次请求换掉这个首屏惊喜，代价可以接受
  // （普通用户直接跳过，不会因此吃 403）。
  void ensureUserOptions()
})

async function load() {
  loading.value = true
  err.value = ''
  try {
    keys.value = await api.keys()
  } catch (e) {
    if ((e as { status?: number }).status === 401) return
    // 失败**必须留下可见的错误态**：只弹 toast 的话，页面保留空列表/
    // 空表格，呈现成「暂无数据」—— 而真实原因是请求失败了。运维会据此
    // 判断「今天没有流量」，进而排除掉网关/上游故障这个真正的方向。
    // 与 Users/Groups 的持久错误态同口径（见其 v-else-if="err"）。
    err.value = '加载密钥失败：' + (e as Error).message
    toast('加载失败：' + (e as Error).message, 'err')
  } finally {
    loading.value = false
  }
}

// ---------- 创建表单 ----------

const form = reactive({
  open: false,
  name: '',
  quota: 0,
  rpm: 0,
  tpm: 0,
  // key 级模型白名单。**空 = 不限制**（不是「什么都不允许」）。
  models: [] as string[],
  // P2 三项：有效期（天数，0 = 永不过期）/ 来源 IP 白名单 / 分组覆盖。
  days: 0,
  ips: '',
  groupId: '',
})

function openCreate() {
  form.name = ''
  form.quota = 0
  form.rpm = 0
  form.tpm = 0
  form.models = []
  form.days = 0
  form.ips = ''
  form.groupId = ''
  form.open = true
  void ensureModelOptions()
  void ensureGroupOptions()
}

async function submitCreate() {
  if (!form.name.trim()) {
    toast('名称为必填项', 'err')
    return
  }
  try {
    const created = await api.createKey({
      name: form.name.trim(),
      // quota/rpm/tpm 只在管理员时透传：普通用户填了也会被后端封顶/清零，
      // 照发只是让界面显示「设了」而实际没生效 —— 静默失效比直接不显示更糟。
      ...(isAdmin()
        ? {
            quota_tokens: Number(form.quota) || 0,
            rpm_limit: Number(form.rpm) || 0,
            tpm_limit: Number(form.tpm) || 0,
          }
        : {}),
      allowed_models: form.models,
      expires_at: expiresAtFromDays(form.days),
      allowed_ips: form.ips.trim(),
      // 分组覆盖仍只给管理员：它能指向更宽松的组，是收紧手段不是自助配置。
      group_id: isAdmin() ? form.groupId : undefined,
    })
    form.open = false
    plainKeyBox.key = (created as KeyCreateResponse).plaintext_key
    plainKeyBox.name = created.name
    plainKeyBox.open = true
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('创建失败：' + (e as Error).message, 'err')
  }
}

async function ensureGroupOptions() {
  if (!isAdmin() || groupOptions.value.length > 0) return
  try {
    groupOptions.value = await api.groups()
  } catch {
    /* 拉不到就不显示这个下拉，不阻塞其它配置 */
  }
}

async function copyKey() {
  const ok = await copyText(plainKeyBox.key)
  toast(ok ? '已复制到剪贴板' : '复制失败，请手动选择复制', ok ? 'ok' : 'err')
}

// ---------- 编辑表单 ----------

const eForm = reactive({
  open: false,
  id: '',
  name: '',
  quota: 0,
  rpm: 0,
  tpm: 0,
  // 同上：空 = 不限制。保存时总是显式回传当前勾选，
  // 所以「取消勾选」= 传 [] = 清除限制（后端 PATCH 语义：字段缺席才是保持原值）。
  models: [] as string[],
  days: 0,
  // 打开编辑框时的天数快照。保存时只在用户**显式改过**天数后才发送 expires_at：
  // 总是发送会把「改个名字」变成「把截止时间重锚定到现在」（剩余不足 1 天的 key
  // 被顺延），而已过期的 key 会被 daysLeft 的 0 值静默改写成「永不过期」——
  // 等于凭空复活。PATCH 语义下「不传 = 保持原值」正是这里需要的。
  daysOrig: 0,
  // 打开编辑框时这把 key 是否已过期（expires_at 在过去）。影响编辑框里的提示文案：
  // 「已过期」与「永不过期」在 daysLeft 里都是 0，不看这个标记就会混为一谈。
  keyExpired: false,
  ips: '',
  groupId: '',
})

function openEdit(k: AccessKey) {
  eForm.id = k.id
  eForm.name = k.name
  eForm.quota = k.quota_tokens ?? 0
  eForm.rpm = k.rpm_limit ?? 0
  eForm.tpm = k.tpm_limit ?? 0
  eForm.models = [...(k.allowed_models ?? [])]
  eForm.days = daysLeft(k.expires_at ?? 0)
  eForm.daysOrig = eForm.days
  eForm.keyExpired = (k.expires_at ?? 0) > 0 && k.expires_at <= Date.now()
  eForm.ips = k.allowed_ips ?? ''
  eForm.groupId = k.group_id ?? ''
  eForm.open = true
  void ensureModelOptions()
  void ensureGroupOptions()
}

async function submitEdit() {
  if (!eForm.name.trim()) {
    toast('名称为必填项', 'err')
    return
  }
  try {
    await api.updateKey(eForm.id, {
      name: eForm.name.trim(),
      // 配额与限速与「新建」同一口径：**只有管理员能设/改**（2026-10-10 修复的 P1）。
      //
      // 原实现无条件发这三个字段，于是普通用户编辑自己的 key 时也会带上它们，
      // 而服务端 guardNoLoosening 会以 403 拒绝任何「放宽」—— 用户只想改个名字，
      // 却因为一个自己无权设置的字段被拒，整次保存（含改名）全部丢失，
      // 界面上只剩一句服务端原文，且没有标出是哪个输入框。
      //
      // 更糟的是「清空」：普通用户把管理员设过的值删掉（'' → 0）同样是放宽
      // （0 = 不限），也会 403 —— 一个看起来无害的操作必然失败。
      //
      // 不发这个字段 = 保持原值（PATCH 语义），与新建弹窗的处理一致。
      ...(isAdmin()
        ? {
            quota_tokens: Number(eForm.quota) || 0,
            rpm_limit: Number(eForm.rpm) || 0,
            tpm_limit: Number(eForm.tpm) || 0,
          }
        : {}),
      allowed_models: eForm.models,
      // 见 eForm.daysOrig 注释：改过天数才发，保持原值就不带这个字段。
      ...(Number(eForm.days) !== Number(eForm.daysOrig)
        ? { expires_at: expiresAtFromDays(eForm.days) }
        : {}),
      allowed_ips: eForm.ips.trim(),
      ...(isAdmin() ? { group_id: eForm.groupId } : {}),
    })
    eForm.open = false
    toast('密钥已更新')
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('保存失败：' + (e as Error).message, 'err')
  }
}

async function toggleKey(k: AccessKey) {
  try {
    await api.updateKey(k.id, { enabled: !k.enabled })
    toast(!k.enabled ? '已启用' : '已停用')
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('操作失败：' + (e as Error).message, 'err')
  }
}

async function removeKey(k: AccessKey) {
  const ok = await confirmBox({
    title: `删除密钥「${k.name}」？`,
    body: '使用该密钥的客户端将立即收到 401，且明文密钥不可恢复。',
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return
  try {
    await api.deleteKey(k.id)
    toast('已删除')
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('删除失败：' + (e as Error).message, 'err')
  }
}

// ---------- 重算用量 ----------
// used_tokens 由数据库触发器单调累加、没有回退路径，一旦因 bug 偏高，
// 配额预检会让这把 key 永远 429（死 key）。这里是唯一的界面自愈入口
// （POST /keys/{id}/recompute-usage），按调用明细重算并覆盖计数。
const recomputing = ref('')

async function recomputeUsage(k: AccessKey) {
  recomputing.value = k.id
  try {
    const r = await api.recomputeKeyUsage(k.id)
    // 就地用返回的修正值刷新该行：其余行的状态（展开的弹窗等）不受打扰
    k.used_tokens = r.used_tokens
    toast(`已重算「${k.name}」用量：${fmtTokens(r.used_tokens)} tokens`)
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('重算失败：' + (e as Error).message, 'err')
  } finally {
    recomputing.value = ''
  }
}
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>访问密钥</h1>
        <div class="sub">客户端调用 /v1 时使用的 sk-gw- 密钥</div>
      </div>
      <div class="head-actions">
        <button class="btn" :disabled="loading" @click="load">刷新</button>
        <!-- 自助发 key 对所有登录用户开放。归属恒为创建者自己（后端忽略
             请求里的 user_id），额度被封顶到用户级配额、限速被清零 ——
             所以「重建一把绕开限制」这条路不成立，管理员仍可对具体某把
             key 施加收紧策略（见 key_handler.Create）。 -->
        <button class="btn btn-primary" @click="openCreate">新建密钥</button>
      </div>
    </div>

    <div class="panel">
      <!-- 按用户筛选。布局贴 History 页的同一套 .filters（flex + wrap），
           视觉上两张表的筛选区是同一种东西，用户不需要重新认一遍。
           仅管理员可见：普通用户的列表本来就只有自己（后端按 user_id 收窄），
           「按用户筛」对他们恒等于「全部用户」，且 /admin/api/users 只有
           管理员能调 —— 给他们一个恒等的下拉没有意义。 -->
      <div v-if="isAdmin()" class="filters">
        <select
          v-model="fUser"
          class="select w-auto"
          title="按归属用户筛选"
          @focus="onUserFilterOpen"
          @change="onUserFilterOpen"
        >
          <option value="">全部用户</option>
          <option v-for="u in userFilterOptions" :key="u.id" :value="u.id">{{ u.username }}</option>
          <!-- 无归属的 key 不会被任何用户项覆盖，单独给一项，否则这批行
               永远筛不出来（「看不见」会被读成「不存在」）。 -->
          <option v-if="hasOrphanKeys" :value="ORPHAN_FILTER">无归属（不可用）</option>
        </select>
        <button v-if="fUser" class="btn btn-sm btn-ghost" :disabled="loading" @click="resetFilter">
          重置筛选
        </button>
        <span class="filters-hint">
          共 {{ visibleKeys.length }} / {{ keys.length }} 把 · 按用量降序
        </span>
      </div>

      <div v-if="loading && keys.length === 0" class="loading">加载中…</div>
      <!--加载失败**必须**与「确实没有数据」在界面上可区分：把请求失败呈现成
           「暂无数据」会让运维误判为无流量，从而排除掉网关/上游故障这个方向。
           与 Users/Groups 的错误态同口径。 -->
      <div v-else-if="err" class="empty"><div class="big">⚠</div>{{ err }}</div>
      <div v-else-if="keys.length === 0" class="empty">
        <div class="big">◇</div>
        还没有访问密钥
      </div>
      <!-- 筛选后为空 ≠ 本来就没有 key：两句话必须分开，否则用户会把
           「这个人的 key 都在别的用户名下」读成「他一把 key 都没有」。 -->
      <div v-else-if="filteredEmpty" class="empty">
        <div class="big">⌗</div>
        没有匹配当前筛选条件的密钥
        <div class="err-retry">
          <button class="btn btn-sm" @click="resetFilter">清除筛选</button>
        </div>
      </div>
      <div v-else class="tbl-wrap">
        <table class="tbl">
          <thead>
            <tr>
              <th>名称</th>
              <th>归属用户</th>
              <th>限制</th>
              <!-- 表头标出方向：这一列是按 used_tokens 降序排的，
                   箭头的朝向就是排的方向，不写出来用户只能靠点表头试探。 -->
              <th class="num-h">用量 ↓</th>
              <th>有效期</th>
              <th class="c-act">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="k in visibleKeys" :key="k.id">
              <td>
                <div class="cell-key">
                  <span class="name">{{ k.name }}</span>
                  <span class="badge" :class="k.enabled ? 'badge-live' : 'badge-off'">
                    {{ k.enabled ? '启用' : '停用' }}
                  </span>
                </div>
                <div class="sub-line mono">{{ k.key_prefix }}</div>
              </td>
              <td>
                <!-- 无归属必须显式标注：这类 key 会被迁移退役、鉴权直接 401，
                     不标注的话运维会以为它还能用。 -->
                <span v-if="!k.user_id" class="badge badge-off" title="无归属的密钥会被退役，鉴权返回 401">
                  无归属（不可用）
                </span>
                <span v-else class="mono">{{ k.username || k.user_id }}</span>
              </td>
              <td>
                <!-- 白名单非空才标：空 = 不限制，标出来反而让人以为受限。 -->
                <span v-if="k.allowed_models.length > 0" class="badge badge-live">
                  限 {{ k.allowed_models.length }} 个模型
                </span>
                <span v-if="k.allowed_ips" class="badge badge-accent" :title="`只允许 ${k.allowed_ips} 调用`">限源</span>
                <span v-if="k.group_id" class="badge badge-accent" title="该密钥使用指定分组的模型范围">分组覆盖</span>
                <span v-if="rateText(k)" class="dim">{{ rateText(k) }}</span>
                <span v-if="!anyLimit(k)" class="dim">不限</span>
              </td>
              <td class="num-h">
                {{ fmtTokens(k.used_tokens) }}
                <span class="dim">/ {{ k.quota_tokens > 0 ? fmtTokens(k.quota_tokens) : '不限' }}</span>
                <span v-if="k.quota_tokens > 0 && k.used_tokens >= k.quota_tokens" class="badge badge-err">
                  配额已用尽
                </span>
              </td>
              <td>
                <div v-if="k.expires_at > 0" class="cell-exp">
                  <!-- 已过期：最需要被一眼看到的状态。它仍然 enabled，
                       但数据面一律 403，所以界面上必须与「停用」区分开。 -->
                  <span v-if="isExpired(k)" class="badge badge-err">已过期</span>
                  <span>
                    到期 {{ fmtDateTime(k.expires_at) }}<template v-if="!isExpired(k)">（剩 {{ daysLeft(k.expires_at) }} 天）</template>
                  </span>
                </div>
                <span v-else class="dim">永不过期</span>
                <div class="sub-line">建于 {{ fmtDateTime(k.created_at) }}</div>
              </td>
              <td class="c-act">
                <div class="row-actions">
                  <button class="btn btn-sm btn-ghost" :disabled="recomputing === k.id" title="按调用明细重算已用 tokens（配额计数异常时使用）" @click="recomputeUsage(k)">
                    {{ recomputing === k.id ? '重算中…' : '重算用量' }}
                  </button>
                  <button class="btn btn-sm btn-ghost" @click="openEdit(k)">编辑</button>
                  <button class="btn btn-sm btn-danger" @click="removeKey(k)">删除</button>
                  <button class="switch" :class="{ on: k.enabled }" :title="k.enabled ? '停用' : '启用'" @click="toggleKey(k)"></button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- 创建表单：表单填一半别丢，遮罩/Esc 都不关 -->
    <AppModal :open="form.open" title="新建访问密钥" max-width="500px" :dismissable="false">
      <form @submit.prevent="submitCreate">
        <div class="form-grid">
          <div class="field span2">
            <label>名称 *</label>
            <input v-model="form.name" class="input" placeholder="cursor-主力 / 内部测试" />
          </div>
          <!-- 归属**不是**一个可选字段，界面上也不给任何改归属的入口：
               任何人（含管理员）建 key 都只归属自己（后端 Create 强制
               owner = 调用者，并**静默忽略**请求里的 user_id），而
               PATCH /keys/{id} 的载荷里根本没有 user_id（见 api.ts 的
               updateKey）—— 归属只能通过那条 admin-only 的认领路径改变，
               前端从不暴露它。要替别人建，请让对方自行创建。 -->
          <div class="field span2">
            <label>归属用户</label>
            <input class="input" :value="meName" disabled />
            <span class="tip">密钥始终归属你自己。需要给别人用，请让对方自行创建。</span>
          </div>
          <div class="field span2">
            <label>可用模型</label>
            <ModelPicker v-model="form.models" :options="modelOptions" :restricted="modelsRestricted" />
          </div>
          <div class="field">
            <label>有效期（天）</label>
            <input v-model.number="form.days" class="input num" type="number" min="0" step="1" placeholder="0" />
            <span class="tip">0 = 永不过期。到期后该密钥请求返回 403。</span>
          </div>
          <div class="field">
            <label>来源 IP 白名单</label>
            <input v-model="form.ips" class="input" placeholder="留空 = 不限制" />
            <span class="tip">逗号分隔的 CIDR 或单 IP，如 <code>10.0.0.0/8, 203.0.113.7</code>。按直连地址判定，不认 <code>X-Forwarded-For</code>。</span>
          </div>
          <div v-if="isAdmin()" class="field span2">
            <label>分组覆盖</label>
            <select v-model="form.groupId" class="input">
              <option value="">（沿用归属用户所属的分组）</option>
              <option v-for="g in groupOptions" :key="g.id" :value="g.id">
                {{ g.name }}{{ g.models.length === 0 ? '（未限制模型）' : '' }}
              </option>
            </select>
            <span class="tip">让这把密钥用另一个组的模型范围，常用于「给某个人的某把密钥单独收紧」。</span>
          </div>
          <!-- 配额与限速仅管理员可设。普通用户的额度被封顶到「用户级配额」，
               限速由网关统一控制 —— 表单里不给他们这两个输入框，
               而不是给一个填了也不生效的框（静默失效最容易让人误判）。 -->
          <div v-if="isAdmin()" class="field span2">
            <label>Token 配额</label>
            <input v-model.number="form.quota" class="input num" type="number" min="0" step="1" placeholder="0" />
            <span class="tip">累计 input+output token 上限，用尽后 /v1 返回 429；0 = 不限</span>
          </div>
          <div v-if="isAdmin()" class="field">
            <label>RPM 限速</label>
            <input v-model.number="form.rpm" class="input num" type="number" min="0" step="1" placeholder="0" />
            <span class="tip">每分钟请求数上限；0 = 不限</span>
          </div>
          <div v-if="isAdmin()" class="field">
            <label>TPM 限速</label>
            <input v-model.number="form.tpm" class="input num" type="number" min="0" step="1" placeholder="0" />
            <span class="tip">每分钟 token 上限（估算预占+事后校正）；0 = 不限</span>
          </div>
          <div v-else class="field span2">
            <label>Token 配额</label>
            <input class="input num" disabled :value="quotaHint" />
            <span class="tip">
              你的密钥额度不能超过管理员为你设置的用户级配额。下面的有效期与 IP
              白名单可以自己设 —— 那只会让你的密钥更受限，不会更宽松。
            </span>
          </div>
        </div>
        <div class="form-actions">
          <button type="button" class="btn btn-ghost" @click="form.open = false">取消</button>
          <button type="submit" class="btn btn-primary">创建</button>
        </div>
      </form>
    </AppModal>

    <!-- 一次性明文展示 -->
    <!-- dismissable=false：明文密钥只展示一次，误点遮罩就再也看不到了 ——
     必须显式走「我已保存」。这也是 AppModal dismissable prop 的第一个使用者。 -->
    <AppModal :open="plainKeyBox.open" title="密钥已创建" max-width="520px" :dismissable="false">
      <div class="sheet-body">
        「{{ plainKeyBox.name }}」的完整密钥如下，<b>仅此一次展示</b>，关闭后无法再次查看：
      </div>
      <div class="plainkey">
        <div class="mono">{{ plainKeyBox.key }}</div>
        <div class="warn">请立即复制并妥善保管，不要提交到代码仓库。</div>
      </div>
      <div class="form-actions">
        <button class="btn" @click="copyKey">复制密钥</button>
        <button class="btn btn-primary" @click="plainKeyBox.open = false">我已保存</button>
      </div>
    </AppModal>

    <!-- 编辑表单：表单填一半别丢，遮罩/Esc 都不关 -->
    <AppModal :open="eForm.open" title="编辑密钥" max-width="500px" :dismissable="false">
      <form @submit.prevent="submitEdit">
        <div class="form-grid">
          <div class="field span2">
            <label>名称 *</label>
            <input v-model="eForm.name" class="input" />
          </div>
          <div class="field span2">
            <label>可用模型</label>
            <ModelPicker v-model="eForm.models" :options="modelOptions" :restricted="modelsRestricted" />
          </div>
          <div class="field">
            <label>有效期（天）</label>
            <input v-model.number="eForm.days" class="input num" type="number" min="0" step="1" placeholder="0" />
            <span v-if="eForm.keyExpired" class="tip">该密钥已过期。改动天数会重新设定有效期（从现在起 N 天）；不改动则维持过期状态。</span>
            <span v-else class="tip">0 = 永不过期。改动天数会按「从现在起 N 天」重设截止时间；不改动则保持原值。</span>
          </div>
          <div class="field">
            <label>来源 IP 白名单</label>
            <input v-model="eForm.ips" class="input" placeholder="留空 = 不限制" />
            <span class="tip">逗号分隔的 CIDR 或单 IP。写错会让这把密钥从所有地址都连不上。</span>
          </div>
          <div v-if="isAdmin()" class="field span2">
            <label>分组覆盖</label>
            <select v-model="eForm.groupId" class="input">
              <option value="">（沿用归属用户所属的分组）</option>
              <option v-for="g in groupOptions" :key="g.id" :value="g.id">
                {{ g.name }}{{ g.models.length === 0 ? '（未限制模型）' : '' }}
              </option>
            </select>
            <span class="tip">这把密钥用哪个组的模型范围。</span>
          </div>
          <!-- 与「新建」弹窗同一口径：配额与限速仅管理员可设，普通用户不给这两个框
               （2026-10-10 修复的 P1）。服务端 guardNoLoosening 允许普通用户
               「收紧」但拒绝「放宽」，界面上给出可输入的框却让服务端决定收不收，
               只会让一次改名之类的正常保存被 403 整个打掉。
               普通用户的服务端额度仍按「用户级配额」封顶，见 submitEdit 的注释。 -->
          <div v-if="isAdmin()" class="field span2">
            <label>Token 配额</label>
            <input v-model.number="eForm.quota" class="input num" type="number" min="0" step="1" placeholder="0" />
            <span class="tip">累计 input+output token 上限，用尽后 /v1 返回 429；0 = 不限</span>
          </div>
          <div v-if="isAdmin()" class="field">
            <label>RPM 限速</label>
            <input v-model.number="eForm.rpm" class="input num" type="number" min="0" step="1" placeholder="0" />
            <span class="tip">每分钟请求数上限；0 = 不限</span>
          </div>
          <div v-if="isAdmin()" class="field">
            <label>TPM 限速</label>
            <input v-model.number="eForm.tpm" class="input num" type="number" min="0" step="1" placeholder="0" />
            <span class="tip">每分钟 token 上限（估算预占+事后校正）；0 = 不限</span>
          </div>
        </div>
        <div class="form-actions">
          <button type="button" class="btn btn-ghost" @click="eForm.open = false">取消</button>
          <button type="submit" class="btn btn-primary">保存</button>
        </div>
      </form>
    </AppModal>
  </main>
</template>

<style scoped>
/* 名称与有效期两列各两行（主信息 + 压暗的补充信息），行高因此天然一致。
   .cell-key 骨架用全局的（styles.css），有效期是本页特有的，留这里。 */
.cell-exp { display: flex; align-items: center; gap: 6px; flex-wrap: nowrap; }

/* 筛选行：与 History.vue 的 .filters 同款（flex + wrap + 10px gap），
   两页的筛选区看起来是同一类控件，肌肉记忆可以跨页复用。 */
.w-auto {
  width: auto;
}
.filters {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  margin-bottom: 14px;
}
/* 计数与排序说明：压在筛选行末尾，弱化色。
   「共 N / M 把 · 按用量降序」常驻的理由是排序方向必须**始终**可见 ——
   用户翻两页找不到预期那把 key 时，第一反应是「这个列表到底怎么排的」。
   只在首次渲染时说一次是没用的，他早忘了。 */
.filters-hint {
  margin-left: auto;
  font-size: 12px;
  color: var(--text-3);
}
</style>
