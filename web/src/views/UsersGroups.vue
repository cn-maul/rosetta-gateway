<script setup lang="ts">
/**
 * 用户与分组（仅管理员可见）。
 *
 * # 为什么合并成一个页面（2026-10-10）
 *
 * 分组不是独立实体，而是**用户的属性**（users.group_id）。拆成两页会让人
 * 在「这个人在哪个组」与「这个组有哪些人」之间来回跳：给一个新人定分组，
 * 要先记住组名 → 去分组页确认 → 回用户页填 → 再回去核对成员数。
 * 页内两个页签解决，且**数据一次取完**（两边本来就互相引用，分开时每页
 * 各拉一次同样的 groups）。
 *
 * # 用户侧三条不能错的规则（都由后端强制，这里只是配合呈现）
 *
 * 1. 管理员**不能对自己**降权/禁用/删除（会把自己锁在门外）。
 *    后端回 400，界面因此把按钮置灰并给提示 —— 但真正的拦截在后端。
 * 2. 禁用 / 改角色会让该用户的**所有会话立即失效**（auth_version 递增），
 *    所以这两个操作要提示管理员这一点。
 * 3. 密码**不设默认值**。建号时密码可留空，账号需自行设置或由管理员重置。
 *    留空账号无法登录（后端明确拒绝），这是刻意的。
 *
 * 用户列表刻意做成**只读**的：分组、角色、显示名、额度这些字段一律走
 * 「编辑」弹窗，行内不再挂下拉，连角色切换也不给按钮 —— 升级/降级都会让
 * 该用户所有会话立即失效，属于要看着后果再点确认的动作。
 *
 * # 分组侧两条最容易踩错的产品语义
 *
 * 1. **空白名单 = 不限制**，不是「什么都看不到」。新建的组默认就是空的，
 *    所以「建了组但没配模型」= 权限比不分组还宽。
 * 2. **组里还有人、还有密钥时不能删**（后端 409）。删掉会让那批账号与密钥
 *    从「受限」变成「不受限」—— 一次删除操作等于给一组人**扩权**，而且是
 *    静默的：账号还在、密钥还能用，只是模型白名单凭空消失了。
 *    所以两类绑定都在界面上分别显示数量，且任一 count > 0 时**不给删除入口**：
 *    后端一定会拒绝，让管理员先按一次确认、再收到一句 409，等于把
 *    「我早就知道会失败」包装成一次操作。
 *
 * # 白名单是精确匹配
 *
 * 模型清单直接从 /admin/api/model-names 取（与后端校验同源），不给自由
 * 输入框 —— 拼错一个字符的结果是「这个模型谁都看不到」而界面上毫无异常。
 */
import { computed, onMounted, ref } from 'vue'
import { api, ApiFail } from '../api'
import type { Group, User, UserRole, UserStatus } from '../types'
import { fmtTokens, fmtDateTime, fmtBalance, fmtRemainder } from '../fmt'
import { toast, confirmBox } from '../ui'
import { checkPasswordStrength } from '../password'
import AppModal from '../components/AppModal.vue'
import ModelPicker from '../components/ModelPicker.vue'

/** 页签。顺序按「先看人、再看规则」—— 定分组的前提是知道要给谁定。 */
const TABS = [
  { key: 'users', label: '用户' },
  { key: 'groups', label: '分组' },
] as const
type TabKey = (typeof TABS)[number]['key']
const tab = ref<TabKey>('users')

const users = ref<User[]>([])
/** 分组清单，用于把 group_id 渲染成名字、以及在表单里选择。 */
const groups = ref<Group[]>([])
/** 可选模型名，来自 /admin/api/model-names —— 与后端白名单校验**同一个数据源**。
 *
 *  刻意不用 /admin/api/routes：那是另一个来源，两处一旦漂移就会出现
 *  「界面能勾、但保存时被后端拒」的错位。 */
const modelOptions = ref<string[]>([])
const loading = ref(false)
const err = ref('')

const showCreate = ref(false)
const cName = ref('')
const cPassword = ref('')
const cRole = ref<UserRole>('user')
const cGroup = ref('')
const cQuota = ref<number>(0)
const cErr = ref('')
const cBusy = ref(false)

const pwdFor = ref<User | null>(null)
const pwdValue = ref('')
const pwdErr = ref('')
const pwdBusy = ref(false)

/** 编辑弹窗的表单状态。editing 指向正在改的那一行，null = 弹窗关闭。 */
const editing = ref<User | null>(null)
const eRole = ref<UserRole>('user')
const eGroup = ref('')
const eQuota = ref<number>(0)
const eRemark = ref('')
const eErr = ref('')
const eBusy = ref(false)

// 余额充值弹窗的状态与逻辑（balFor / balAmount / balErr / balBusy /
// openBalance / yuanToCents / submitBalance）已于 2026-10-11 随入口一起
// 移到「钱包与充值」页的管理员形态（web/src/views/Wallet.vue）。
//
// 整块删掉而不是留成不可达代码：本文件里它们唯一的调用点就是那一行的
// 「充值」按钮与它自己的 AppModal，按钮一删就全是死代码 —— 而死代码会让
// 下一个改这一页的人以为「本页也能充值」，重新接回一个已被产品取消的入口。
//
// **没有**一起删的：pending / isPending / setPending。那三个是**共用**的防连点
// 机制，禁用（toggleStatus）与删除（askDelete）都还在用（见各自调用点）。
// 只删充值专属状态，共享机制原样保留 —— 拆掉共享部分会让剩下两个写操作
// 失去连点保护。

async function load() {
  loading.value = true
  err.value = ''
  try {
    // 三份数据一次取完：用户与分组互相引用（用户的 group_name、分组的
    // member_count 都来自对方），分开请求会让「改了谁的分组」之后两个表
    // 的新鲜度对不上 —— 而 member_count 是**服务端算好的字段**，
    // 不重新拉分组表就还是旧数字。
    const [u, g, names] = await Promise.all([api.users(), api.groups(), api.modelNames()])
    users.value = u
    groups.value = g
    modelOptions.value = names.models
  } catch (e) {
    if (e instanceof ApiFail && e.status === 401) return
    err.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

/** 分组 id → 名字。每行渲染都要查一次，用 Map 而不是对每行 find 一遍。 */
const groupNames = computed(() => {
  const m = new Map<string, string>()
  for (const g of groups.value) m.set(g.id, g.name)
  return m
})

/** groupName 把 group_id 渲染成组名。
 *
 * 组被删掉、或这个 id 在清单里查不到时显示「未知分组」而不是空 ——
 * 空会看起来像「未分组」，而「未分组 = 不限制模型」，两者含义完全不同。 */
function groupName(id: string): string {
  if (!id) return ''
  return groupNames.value.get(id) ?? '未知分组'
}

/** isUnknownGroup 判断这个 group_id 在当前分组清单里查不到。
 *
 * 判据是**清单里没有这个 id**，而不是「渲染出来的名字等于『未知分组』」——
 * 后者把一个展示文案当成数据用：真有一个组叫「未知分组」时，它的每个成员
 * 都会被误报成「分组已不存在」，而清单其实一直都在。 */
function isUnknownGroup(id: string): boolean {
  return !!id && !groupNames.value.has(id)
}

/** openEdit 用当前行数据填表。
 *
 * 表单是**快照**：弹窗开着的时候后台可能被别人改过，所以提交时后端仍会校验
 * 「不能降低自己的权限」「空密码账号不能升管理员」这些硬规则。 */
function openEdit(u: User) {
  eErr.value = ''
  eRole.value = u.role
  eGroup.value = u.group_id || ''
  eQuota.value = u.quota_tokens || 0
  eRemark.value = u.remark || ''
  editing.value = u
}

async function saveEdit() {
  const u = editing.value
  if (!u || eBusy.value) return
  eErr.value = ''
  // 下面两条后端也会拒（400），这里先挡住是为了不让人白等一个来回。
  if (u.is_self && eRole.value !== 'admin') {
    eErr.value = '不能降低自己的权限（会把自己锁在门外）'
    return
  }
  if (eRole.value === 'admin' && u.role !== 'admin' && !u.has_password) {
    eErr.value = '该账号尚未设置密码，请先为其重置密码，再升级为管理员'
    return
  }
  if (eQuota.value < 0) {
    eErr.value = '额度上限不能为负'
    return
  }
  const roleChanged = eRole.value !== u.role
  eBusy.value = true
  try {
    await api.updateUser(u.id, {
      role: eRole.value,
      // 空串 = 明确的「移出分组」（后端用指针区分「不传」与「传空」），
      // 所以这里不能用 undefined，否则「清空分组」这个动作表达不出去。
      group_id: eGroup.value,
      quota_tokens: Number(eQuota.value) || 0,
      remark: eRemark.value.trim(),
    })
    toast(roleChanged ? '已保存，该用户的登录状态已失效' : '已保存', 'ok')
    editing.value = null
    await load()
  } catch (e) {
    eErr.value = e instanceof ApiFail ? e.message : '保存失败'
  } finally {
    eBusy.value = false
  }
}

async function create() {
  if (cBusy.value) return
  cErr.value = ''
  if (!cName.value.trim()) {
    cErr.value = '用户名必填'
    return
  }
  // 下面两条后端也会拒（400），这里先挡住是为了不让人白等一个来回。
  //
  // 管理员空密码不是「强度不足」，是**安全边界**：后端建出的空密码 admin
  // 会让「存在 role=admin 且 password_hash=''」这个引导判定重新命中，
  // 而免鉴权的 POST /admin/api/bootstrap 恰好认这个条件 —— 任何能连到端口
  // 的人都能给它设上自己的密码并拿到 admin 会话。所以必须在提交前拦掉，
  // 而不是等后端回 400 再说。
  if (cRole.value === 'admin' && !cPassword.value) {
    cErr.value = '创建管理员必须设置初始密码（管理员不允许空密码账号）'
    return
  }
  // 普通用户的初始密码可以留空（后端接受，建出「尚未设置密码」的账号），
  // 但**填了就必须合规** —— 否则后端回 400，界面只看到一句「创建失败」。
  if (cPassword.value) {
    const weak = checkPasswordStrength(cPassword.value)
    if (weak) {
      cErr.value = weak
      return
    }
  }
  cBusy.value = true
  try {
    await api.createUser({
      username: cName.value.trim(),
      // 密码留空则不传 —— 后端接受空密码，建出「尚未设置密码」的账号
      password: cPassword.value || undefined,
      role: cRole.value,
      group_id: cGroup.value || undefined,
      quota_tokens: Number(cQuota.value) || 0,
    })
    toast('已创建', 'ok')
    showCreate.value = false
    cName.value = cPassword.value = ''
    cRole.value = 'user'
    cGroup.value = ''
    cQuota.value = 0
    await load()
  } catch (e) {
    cErr.value = e instanceof ApiFail ? e.message : '创建失败'
  } finally {
    cBusy.value = false
  }
}

/** 正在飞行中的行操作：user id → 语义标签。
 *
 * 防连点必须落在**行**上，不能靠一个全局 busy：并发点击同一行会发两遍
 * 同义请求（第二遍要么白等、要么把刚改过的状态又翻回去），而点不同行
 * 本就该各自进行。按钮据此置灰并给出「处理中…」，用户因此看得见
 * 「已经点过了」，而不是再点三次以为没生效。 */
const pending = ref(new Map<string, string>())

function isPending(id: string): boolean {
  return pending.value.has(id)
}

function setPending(id: string, label: string | null) {
  if (label === null) pending.value.delete(id)
  else pending.value.set(id, label)
  // Map 是可变对象，Vue 3 的 ref 能跟踪 set/delete，但为稳妥起见换新引用，
  // 免得依赖旧实现的读者踩到「改了 Map 但没重渲染」。
  pending.value = new Map(pending.value)
}

/** patch 打一发 PATCH 并刷新列表。列表上的按钮共用它，失败统一弹 toast。
 *
 * 返回是否成功。调用方需要它才能决定「成功提示」该不该发 ——
 * 失败时这里已经弹了错误 toast，调用方再喊一句「已禁用」会让人以为
 * 操作成功了一半。
 *
 * label 同时用于置灰按钮与防连点：进入前设上、finally 里清掉，
 * 所以任何返回路径（成功 / 失败 / 抛异常）都不会把按钮永久锁死。 */
async function patch(id: string, b: Parameters<typeof api.updateUser>[1]): Promise<boolean> {
  if (isPending(id)) return false
  setPending(id, '处理中…')
  try {
    await api.updateUser(id, b)
    await load()
    return true
  } catch (e) {
    toast(e instanceof ApiFail ? e.message : '操作失败', 'err')
    return false
  } finally {
    setPending(id, null)
  }
}

async function toggleStatus(u: User) {
  if (u.is_self) {
    toast('不能禁用自己的账号', 'err')
    return
  }
  const next: UserStatus = u.status === 'active' ? 'disabled' : 'active'
  // 只有「禁用」要确认。它是一次**不可逆的会话失效**：auth_version 递增后
  // 该用户所有设备上的登录当场作废，且本人不会收到任何提示 ——
  // 误点一次等于把同事从所有会话里踢出去。启用只是解除这个状态，
  // 没有等价的破坏面，再要点一次确认纯属打扰。
  if (next === 'disabled') {
    const ok = await confirmBox({
      title: '禁用用户',
      body: `将禁用「${u.username}」。他当前的所有登录状态会立即失效，需要管理员重新启用才能再登录。`,
      danger: true,
      confirmLabel: '禁用',
    })
    if (!ok) return
  }
  const done = await patch(u.id, { status: next })
  if (!done) return
  // 成功后 load() 已把列表整个换掉，u 是旧快照，不能再读 u.status 判断结果。
  if (next === 'disabled') toast('已禁用，该用户的所有登录状态失效', 'ok')
  else toast('已启用', 'ok')
}

async function askDelete(u: User) {
  if (u.is_self) {
    toast('不能删除自己的账号', 'err')
    return
  }
  if (isPending(u.id)) return
  const ok = await confirmBox({
    title: '删除用户',
    body: `将删除「${u.username}」及其名下 ${u.key_count} 把密钥。用量历史会保留。`,
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return
  setPending(u.id, '删除中…')
  try {
    await api.deleteUser(u.id)
    toast('已删除', 'ok')
    await load()
  } catch (e) {
    toast(e instanceof ApiFail ? e.message : '删除失败', 'err')
  } finally {
    setPending(u.id, null)
  }
}

async function resetPassword() {
  const u = pwdFor.value
  if (!u || pwdBusy.value) return
  pwdErr.value = ''
  // 与后端 userauth.ValidatePassword 同一套门槛（见 password.ts）：
  // 只查长度的话，「88888888」这种口令在界面上算合格、提交后却被后端回 400，
  // 而管理员只会看到一句「重置失败」。
  const weak = checkPasswordStrength(pwdValue.value)
  if (weak) {
    pwdErr.value = weak
    return
  }
  pwdBusy.value = true
  try {
    await api.resetUserPassword(u.id, pwdValue.value)
    toast('密码已重置，该用户需重新登录', 'ok')
    pwdFor.value = null
    pwdValue.value = ''
    await load()
  } catch (e) {
    pwdErr.value = e instanceof ApiFail ? e.message : '重置失败'
  } finally {
    pwdBusy.value = false
  }
}

function openReset(u: User) {
  pwdErr.value = ''
  pwdValue.value = ''
  pwdFor.value = u
}

// ---- 余额充值 ----
//
// openBalance / yuanToCents / submitBalance 已随入口一起移到 Wallet.vue
// （2026-10-11）。yuanToCents 那个「界面侧就把元→分定死」的理由随代码一起
// 搬走了，Wallet.vue 里有一份逐字相同的实现 —— 两处都留一份会让「换一个换算
// 口径」的修改只改到其中一处，于是同一笔充值在两个界面上差一分钱。

onMounted(load)

// ---- 分组管理（2026-10-10 从 Groups.vue 并入）----
//
// 状态与 Groups.vue 逐字保持一致，逻辑也照搬 —— 那边每一条都带着「为什么」
// 的说明，合并时最忌讳的是「顺手简化」，因为那些判断依据在这个页面上
// 依然全部成立（空白名单=不限制、有绑定不可删、白名单精确匹配）。

const showForm = ref(false)
const editingGroup = ref<Group | null>(null)
const fName = ref('')
const fDesc = ref('')
const fErr = ref('')
const fBusy = ref(false)

/** 正在编辑白名单的组；null = 未打开。 */
const modelsFor = ref<Group | null>(null)
const picked = ref<string[]>([])
const mErr = ref('')
const mBusy = ref(false)

/** 删除请求在飞行中。防连点：确认框结算前按钮仍可点，
 *  连点会给同一行发两次 DELETE，第二次必然 404。 */
const deleting = ref(false)

function openCreateGroup() {
  editingGroup.value = null
  fName.value = ''
  fDesc.value = ''
  fErr.value = ''
  showForm.value = true
}

function openEditGroup(g: Group) {
  editingGroup.value = g
  fName.value = g.name
  fDesc.value = g.description
  fErr.value = ''
  showForm.value = true
}

async function submitGroupForm() {
  if (fBusy.value) return
  fErr.value = ''
  if (!fName.value.trim()) {
    fErr.value = '组名必填'
    return
  }
  fBusy.value = true
  try {
    if (editingGroup.value) {
      await api.updateGroup(editingGroup.value.id, {
        name: fName.value.trim(),
        description: fDesc.value.trim(),
      })
      toast('已保存', 'ok')
    } else {
      await api.createGroup({ name: fName.value.trim(), description: fDesc.value.trim() })
      toast('已创建。注意：新组未配模型白名单，此时该组不限制模型可见范围。', 'ok')
    }
    showForm.value = false
    await load()
  } catch (e) {
    fErr.value = e instanceof ApiFail ? e.message : '保存失败'
  } finally {
    fBusy.value = false
  }
}

function openModels(g: Group) {
  modelsFor.value = g
  picked.value = [...g.models]
  mErr.value = ''
}

async function submitModels() {
  const g = modelsFor.value
  if (!g || mBusy.value) return
  mErr.value = ''
  mBusy.value = true
  try {
    await api.setGroupModels(g.id, [...picked.value])
    toast(picked.value.length === 0 ? '已清空白名单（该组不再限制模型）' : '白名单已保存', 'ok')
    modelsFor.value = null
    await load()
  } catch (e) {
    mErr.value = e instanceof ApiFail ? e.message : '保存失败'
  } finally {
    mBusy.value = false
  }
}

/** 删除被后端拒绝的两种绑定。
 *
 * users.group_id 与 access_keys.group_id 都是 ON DELETE SET NULL，
 * 两类引用**各自**都能让 DeleteGroup 回 ErrGroupNotEmpty ——
 * 只统计账号数会漏掉「组里没人、但有一把密钥指定了分组覆盖」的情形，
 * 那时界面显示「可删」，点下去却必然 409。 */
function boundCounts(g: Group): { users: number; keys: number } {
  return { users: g.member_count ?? 0, keys: g.key_count ?? 0 }
}

/** 该组现在能不能删。任一绑定 > 0 时被后端拒绝，界面因此不给入口。 */
function canDeleteGroup(g: Group): boolean {
  const c = boundCounts(g)
  return c.users === 0 && c.keys === 0
}

/** 删除按钮的 title：被挡住时必须说清是**哪一类**绑定挡的、以及怎么解。
 *
 * 只写「不能删除」会让人去翻后端文档；而真正该做的动作取决于类型：
 * 账号要迁走（**本页的用户页签**就能改），密钥要解除「分组覆盖」
 * （Keys 页的那一个字段）。 */
function deleteBlockedTitle(g: Group): string {
  const c = boundCounts(g)
  if (c.users > 0 && c.keys > 0) {
    return `该组下还有 ${c.users} 个账号和 ${c.keys} 把访问密钥。请先把账号移到别的组，并解除这些密钥的分组覆盖。`
  }
  if (c.users > 0) {
    return `该组下还有 ${c.users} 个账号。请先在「用户」页签把他们移到别的组或移出分组，否则删除会被后端拒绝。`
  }
  if (c.keys > 0) {
    return `该组下还有 ${c.keys} 把访问密钥指定了分组覆盖。请先到「访问密钥」解除这些密钥的分组覆盖。`
  }
  return ''
}

async function askDeleteGroup(g: Group) {
  // 前置拦截，而不是「先确认再等一个 409」：后端一定会拒绝，
  // 让管理员为一个注定失败的操作多按一次确认毫无意义。
  const c = boundCounts(g)
  if (c.users > 0 || c.keys > 0) {
    toast(deleteBlockedTitle(g), 'err')
    return
  }
  if (deleting.value) return
  const ok = await confirmBox({
    title: '删除分组',
    body: `将删除「${g.name}」及其模型白名单。当前没有账号或访问密钥绑定在该组上。`,
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return
  deleting.value = true
  try {
    await api.deleteGroup(g.id)
    toast('已删除', 'ok')
    await load()
  } catch (e) {
    // 409 的文案是后端给的（含人数、密钥数与处置办法），原样展示比
    // 「删除失败」有用得多。走到这里只可能是并发：别人在我们这次 load()
    // 之后刚把账号或密钥绑了进来。
    toast(e instanceof ApiFail ? e.message : '删除失败', 'err')
  } finally {
    deleting.value = false
  }
}
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>用户与分组</h1>
        <div class="sub">账号、配额、登录状态与模型可见范围</div>
      </div>
      <div class="head-actions">
        <button class="btn" :disabled="loading" @click="load">刷新</button>
        <button v-if="tab === 'users'" class="btn btn-primary" @click="showCreate = true">新建用户</button>
        <button v-else class="btn btn-primary" @click="openCreateGroup">新建分组</button>
      </div>
    </div>

    <!-- 页签：形态对齐 Settings.vue 的 .set-tabs（胶囊分段），
         不新造一套控件 —— 同一个交互在同一个应用里长得不一样时，
         用户会以为它们行为也不同。 -->
    <div class="ug-tabs">
      <button
        v-for="t in TABS"
        :key="t.key"
        class="ug-tab"
        :class="{ active: tab === t.key }"
        type="button"
        @click="tab = t.key"
      >
        {{ t.label }}
      </button>
    </div>

    <!-- ============ 页签一：用户 ============ -->
    <template v-if="tab === 'users'">
    <div class="panel">
      <div v-if="loading && users.length === 0" class="loading">加载中…</div>
      <div v-else-if="err" class="empty"><div class="big">⚠</div>{{ err }}</div>
      <div v-else-if="users.length === 0" class="empty">
        <div class="big">◇</div>还没有用户
      </div>
      <div v-else class="tbl-wrap">
        <table class="tbl">
          <thead>
            <tr>
              <th>用户名</th>
              <th>分组</th>
              <th class="num-h">密钥数量</th>
              <th class="num-h">总 token 用量</th>
              <th class="num-h">余额</th>
              <th class="c-act">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="u in users" :key="u.id">
              <td>
                <div class="cell-user">
                  <span class="name mono">{{ u.username }}</span>
                  <span class="badge" :class="u.role === 'admin' ? 'badge-live' : 'badge-off'">
                    {{ u.role === 'admin' ? '管理员' : '普通用户' }}
                  </span>
                  <span v-if="u.is_self" class="badge badge-accent">你自己</span>
                  <span v-if="u.status !== 'active'" class="badge badge-off">已禁用</span>
                  <span v-if="!u.has_password" class="badge badge-off">未设密码</span>
                </div>
              </td>
              <td>
                {{ u.group_id ? groupName(u.group_id) : '未分组' }}
                <span v-if="isUnknownGroup(u.group_id)" class="badge badge-err">
                  分组已不存在
                </span>
              </td>
              <td class="num-h">{{ u.key_count }}</td>
              <td class="num-h">
                {{ fmtTokens(u.used_tokens) }}
                <span v-if="u.quota_tokens > 0" class="dim">/ {{ fmtTokens(u.quota_tokens) }}</span>
              </td>
              <!-- 余额：「不限」与「0.00 元」是**相反**的两种状态，绝不能都
                   渲染成 0。判定走 balance_unlimited，不拿数值代替（见 fmtBalance）。 -->
              <td class="num-h">
                <span :class="{ dim: u.balance_unlimited }">
                  {{ fmtBalance(u.balance_cents, u.balance_unlimited) }}
                </span>
                <!-- 待结算余数：余额按分扣减，低单价时一次调用不足一分，
                     于是余额长时间不动。管理员不看到这个就会以为扣费坏了。 -->
                <span v-if="fmtRemainder(u.balance_remainder ?? 0)" class="dim num-h">
                  +{{ fmtRemainder(u.balance_remainder ?? 0) }}
                </span>
                <span v-if="!u.balance_unlimited && u.balance_cents === 0" class="badge badge-err">
                  已用尽
                </span>
              </td>
              <td class="c-act">
                <div class="row-actions">
                  <button class="btn btn-sm" :disabled="isPending(u.id)" @click="openEdit(u)">编辑</button>
                  <button class="btn btn-sm" :disabled="isPending(u.id)" @click="openReset(u)">重置密码</button>
                  <button class="btn btn-sm" :disabled="u.is_self || isPending(u.id)" :title="u.is_self ? '不能禁用自己的账号' : '禁用后该用户的所有登录状态立即失效'" @click="toggleStatus(u)">
                    {{ isPending(u.id) ? '处理中…' : u.status === 'active' ? '禁用' : '启用' }}
                  </button>
                  <button class="btn btn-sm btn-danger" :disabled="u.is_self || isPending(u.id)" :title="u.is_self ? '不能删除自己' : ''" @click="askDelete(u)">
                    {{ isPending(u.id) ? '处理中…' : '删除' }}
                  </button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
    </template>

    <!-- ============ 页签二：分组 ============ -->
    <template v-else>
    <div class="panel note">
      未分组、或所在组未配白名单 → <strong>不限制</strong>模型可见范围。
      白名单与账号自己的密钥白名单<b>取交集</b>，密钥那一层只会更紧。
    </div>

    <div class="panel">
      <div v-if="loading && groups.length === 0" class="loading">加载中…</div>
      <div v-else-if="err" class="empty"><div class="big">⚠</div>{{ err }}</div>
      <div v-else-if="groups.length === 0" class="empty">
        <div class="big">◇</div>还没有分组 —— 当前所有密钥都能看到全部模型
      </div>
      <div v-else class="tbl-wrap">
        <table class="tbl">
          <thead>
            <tr>
              <th>分组</th>
              <th>说明</th>
              <th>模型白名单</th>
              <th class="c-act">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="g in groups" :key="g.id">
              <td>
                <div class="cell-group">
                  <span class="name">{{ g.name }}</span>
                  <!-- 账号与密钥**分开显示**：两者都会阻止删除，但处置办法不同
                       （前者要迁组 —— 就在本页「用户」页签；后者要解除「分组覆盖」），
                       合成一个数字就没法告诉管理员该去哪改。 -->
                  <span class="badge badge-off">{{ g.member_count }} 个账号</span>
                  <span class="badge" :class="g.key_count > 0 ? 'badge-off' : 'badge-off dim-badge'">
                    {{ g.key_count }} 把密钥
                  </span>
                  <span v-if="g.models.length === 0" class="badge badge-warn">未限制模型</span>
                  <span v-else class="badge badge-live">{{ g.models.length }} 个模型</span>
                </div>
              </td>
              <td class="dim">{{ g.description || '—' }}</td>
              <td class="cell-models mono">
                {{ g.models.length === 0 ? '（未配置白名单 = 不限制）' : g.models.join('、') }}
              </td>
              <td class="c-act">
                <div class="row-actions">
                  <button class="btn btn-sm" @click="openModels(g)">配置模型</button>
                  <button class="btn btn-sm" @click="openEditGroup(g)">改名</button>
                  <button class="btn btn-sm btn-danger" :disabled="!canDeleteGroup(g) || deleting" :title="deleteBlockedTitle(g)" @click="askDeleteGroup(g)">删除</button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
    </template>

    <!-- 新建用户：横版弹窗（≈16:9），字段两列排布。
         dismissable=false：表单填到一半误点遮罩就全丢，
         关闭只走「取消 / 创建」两个明确出口。 -->
    <AppModal :open="showCreate" title="新建用户" max-width="720px" :dismissable="false">
        <div class="fgrid">
          <div class="fcell">
            <label class="flabel" for="cu">用户名</label>
            <input id="cu" v-model="cName" class="input" placeholder="3-32 位字母数字-_." />
            <div class="fhint">登录用。不支持中文与空格。</div>
          </div>
          <div class="fcell">
            <label class="flabel" for="cp">初始密码</label>
            <input id="cp" v-model="cPassword" type="password" class="input" autocomplete="new-password" placeholder="留空则由用户自行设置" />
            <div v-if="cRole === 'admin' && !cPassword" class="fhint warn">创建管理员必须设置初始密码。</div>
            <div v-else class="fhint">留空的账号无法登录，需重置密码后才能使用。</div>
          </div>
          <div class="fcell">
            <label class="flabel" for="cr">角色</label>
            <select id="cr" v-model="cRole" class="input">
              <option value="user">普通用户</option>
              <option value="admin">管理员</option>
            </select>
            <div class="fhint">管理员可见全部页面与所有用户的数据。</div>
          </div>
          <div class="fcell">
            <label class="flabel" for="cg">分组</label>
            <select id="cg" v-model="cGroup" class="input">
              <option value="">未分组（模型不受限）</option>
              <option v-for="g in groups" :key="g.id" :value="g.id">
                {{ g.name }}{{ g.models.length === 0 ? '（未限制模型）' : '' }}
              </option>
            </select>
            <div class="fhint">分组决定能用哪些模型。未分组 = 不限制。</div>
          </div>
          <div class="fcell wide">
            <label class="flabel" for="cq">额度上限（token）</label>
            <input id="cq" v-model.number="cQuota" type="number" min="0" class="input" />
            <div class="fhint">0 = 不限</div>
          </div>
        </div>

        <div v-if="cErr" class="fhint err">{{ cErr }}</div>

        <div class="form-actions">
          <button class="btn" @click="showCreate = false">取消</button>
          <button class="btn btn-primary" :disabled="cBusy" @click="create">
            {{ cBusy ? '创建中…' : '创建' }}
          </button>
        </div>
    </AppModal>

    <!-- 编辑用户。同样不可点外关闭：分组、角色这类字段填到一半
         误点遮罩就全丢，关闭只走「取消 / 保存」两个明确出口。 -->
    <AppModal :open="!!editing" :title="editing ? `编辑用户 · ${editing.username}` : '编辑用户'" max-width="720px" :dismissable="false">
        <div class="msub">
          密钥 {{ editing?.key_count ?? 0 }} 把 · 已用 {{ fmtTokens(editing?.used_tokens ?? 0) }} token
          · 余额 {{ editing ? fmtBalance(editing.balance_cents, editing.balance_unlimited) : '—' }}
          <template v-if="editing?.last_login_at"> · 上次登录 {{ fmtDateTime(editing.last_login_at) }}</template>
        </div>

        <div class="fgrid">
          <div class="fcell">
            <label class="flabel" for="er">角色</label>
            <select id="er" v-model="eRole" class="input" :disabled="editing?.is_self || false">
              <option value="user">普通用户</option>
              <option value="admin">管理员</option>
            </select>
            <div v-if="editing?.is_self" class="fhint warn">不能降低自己的权限。</div>
            <div v-else-if="editing && !editing.has_password" class="fhint warn">该账号未设密码，不能升为管理员。</div>
            <div v-else-if="editing" class="fhint">管理员可见全部页面与所有用户的数据。改角色会让该用户所有登录状态立即失效。</div>
          </div>
          <div class="fcell">
            <label class="flabel" for="eg">分组</label>
            <select id="eg" v-model="eGroup" class="input">
              <option value="">未分组（模型不受限）</option>
              <option v-for="g in groups" :key="g.id" :value="g.id">
                {{ g.name }}{{ g.models.length === 0 ? '（未限制模型）' : '' }}
              </option>
            </select>
            <div class="fhint">分组决定能用哪些模型。未分组 = 不限制。</div>
          </div>
          <div class="fcell">
            <label class="flabel" for="eq">额度上限（token）</label>
            <input id="eq" v-model.number="eQuota" type="number" min="0" class="input" />
            <div class="fhint">0 = 不限。已用 {{ fmtTokens(editing?.used_tokens ?? 0) }}。</div>
          </div>
          <div class="fcell">
            <label class="flabel" for="ek">备注</label>
            <input id="ek" v-model="eRemark" class="input" placeholder="可选，仅管理员可见" />
            <div class="fhint">用户名不可改。</div>
          </div>
        </div>

        <div v-if="eErr" class="fhint err">{{ eErr }}</div>

        <div class="form-actions">
          <button class="btn" @click="editing = null">取消</button>
          <button class="btn btn-primary" :disabled="eBusy" @click="saveEdit">
            {{ eBusy ? '保存中…' : '保存' }}
          </button>
        </div>
    </AppModal>

    <!-- 重置密码：与同页另外两个弹窗同一遮罩行为（不可点外关闭）。 -->
    <AppModal :open="!!pwdFor" :title="pwdFor ? `重置密码 · ${pwdFor.username}` : '重置密码'" max-width="460px" :dismissable="false">
        <div class="fhint warn">重置后该用户的所有登录状态立即失效。</div>
        <label class="flabel" for="np">新密码</label>
        <input id="np" v-model="pwdValue" type="password" class="input" autocomplete="new-password" />
        <!-- 与后端同款门槛（password.ts），提交前就说明白要什么，
             而不是等一个 400 回来。 -->
        <div v-if="pwdValue && checkPasswordStrength(pwdValue)" class="fhint">{{ checkPasswordStrength(pwdValue) }}</div>
        <div v-else class="fhint">至少 8 个字符，且含字母/数字/符号中的至少两类。</div>
        <div v-if="pwdErr" class="fhint err">{{ pwdErr }}</div>
        <div class="form-actions">
          <button class="btn" @click="pwdFor = null">取消</button>
          <button class="btn btn-primary" :disabled="pwdBusy" @click="resetPassword">
            {{ pwdBusy ? '重置中…' : '确定' }}
          </button>
        </div>
    </AppModal>

    <!-- 充值弹窗已移走（2026-10-11）：充值入口收敛到「钱包与充值」页的
         管理员形态（web/src/views/Wallet.vue）。原先这里是每个用户行一个
         「充值」按钮，入口散在整张表的每一行里，而这张表的主要用途是看
         「谁是谁 / 谁归哪个组」，动钱的操作混在里面容易被误点。

         这里保留的是**余额只读展示**（上面表格里的「余额」列）—— 那是管理员
         想知道「这个人还有多少钱」时唯一要看的数字，与充值入口在不在无关。 -->

    <!-- 新建 / 改名分组：与本页另外三个弹窗同一套 AppModal；
         表单不可点外关闭（误点遮罩会丢掉刚输入的组名）。 -->
    <AppModal :open="showForm" :title="editingGroup ? '编辑分组' : '新建分组'" max-width="460px" :dismissable="false">
        <label class="flabel" for="gn">组名</label>
        <input id="gn" v-model="fName" class="input" placeholder="研发 / 外包 / 试用" />
        <label class="flabel" for="gd">说明</label>
        <input id="gd" v-model="fDesc" class="input" placeholder="可选" />
        <div v-if="!editingGroup" class="fhint warn">
          新建的组默认<b>不限制</b>模型。建好后请到「配置模型」里勾选允许的模型。
        </div>
        <div v-if="fErr" class="fhint err">{{ fErr }}</div>
        <div class="form-actions">
          <button class="btn" @click="showForm = false">取消</button>
          <button class="btn btn-primary" :disabled="fBusy" @click="submitGroupForm">
            {{ fBusy ? '保存中…' : '保存' }}
          </button>
        </div>
    </AppModal>

    <!-- 配置模型白名单：勾选状态同样不该被误点遮罩清空。 -->
    <AppModal :open="!!modelsFor" :title="modelsFor ? `模型白名单 · ${modelsFor.name}` : '模型白名单'" max-width="560px" :dismissable="false">
        <div v-if="modelOptions.length === 0" class="fhint warn">
          还没有任何路由（公开模型名）。请先到「上游与模型 / 路由」里建好。
        </div>
        <ModelPicker v-else v-model="picked" :options="modelOptions" />

        <div v-if="mErr" class="fhint err">{{ mErr }}</div>
        <div class="form-actions">
          <button class="btn" @click="modelsFor = null">取消</button>
          <button class="btn btn-primary" :disabled="mBusy" @click="submitModels">
            {{ mBusy ? '保存中…' : `保存（已选 ${picked.length}）` }}
          </button>
        </div>
    </AppModal>
  </main>
</template>

<style scoped>
/* 用户名单元格：用户名 + 徽章同一行。身份徽章紧贴用户名，是因为身份是
   看名字时就要一起看到的信息，不该隔一列。 */
.cell-user { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
.cell-user .name { font-weight: 500; }
/* 表单控件与标签、提示用全局 .input / .flabel / .fhint（styles.css），这里只留页面私有部分。 */
/* 备注独占一行：它比其它字段长，挤进两列网格会把「额度上限」推到下面去。 */
.fcell.wide { grid-column: 1 / -1; }
/* 禁用的表单控件要看得见 —— 置灰后仍要能读出「当前值是什么」。 */
.input:disabled { opacity: 0.55; cursor: not-allowed; }
/* 弹窗标题下的说明行（编辑用户）：全局没有对应物，这里补一条。 */
.msub { font-size: 12px; color: var(--muted); margin: -8px 0 10px; }
/* 字段两列一行挤一挤：五个字段三行，弹窗整体约 16:9，不用竖着滚一屏。 */
.fgrid { display: grid; grid-template-columns: 1fr 1fr; gap: 2px 18px; margin-top: 6px; }

/* ---- 页签 ---- */
/* 形态逐字对齐 Settings.vue 的 .set-tabs：同一个交互在同一个应用里
   长得不一样时，用户会以为它们的行为也不同。 */
.ug-tabs {
  display: flex;
  gap: 2px;
  width: max-content;
  padding: 3px;
  margin-bottom: 14px;
  border-radius: var(--r-pill);
  background: var(--default);
}
.ug-tab {
  padding: 6px 16px;
  border: none;
  border-radius: var(--r-pill);
  background: transparent;
  font-size: 13px;
  font-weight: 500;
  color: var(--muted);
  cursor: pointer;
  white-space: nowrap;
  transition:
    color 0.2s var(--ease-out-quart),
    background 0.25s var(--ease-spring);
}
.ug-tab.active {
  background: var(--surface);
  color: var(--foreground);
}

/* ---- 分组页签 ---- */
.note { font-size: 12px; color: var(--muted); line-height: 1.7; }
.note strong { color: var(--foreground); }
.badge-warn { background: var(--accent-soft); color: var(--accent-soft-foreground); }
/* 0 把密钥不是「值得注意的状态」—— 弱化它，免得每行都挂两个同色徽章。 */
.dim-badge { opacity: 0.55; }
.cell-group { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
.cell-group .name { font-weight: 500; }
/* 白名单列表可能很长：主战场是这张表的宽度，宁可让 tbl-wrap 出横向滚动。 */
.cell-models { max-width: 480px; }

</style>
