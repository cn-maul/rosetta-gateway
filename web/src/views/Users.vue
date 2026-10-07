<script setup lang="ts">
/**
 * 用户管理（仅管理员可见）。
 *
 * 三条不能错的规则，都由后端强制、这里只是配合呈现：
 *   1. 管理员**不能对自己**降权/禁用/删除（会把自己锁在门外）。
 *      后端回 400，界面因此把按钮置灰并给提示 —— 但真正的拦截在后端。
 *   2. 禁用 / 改角色会让该用户的**所有会话立即失效**（auth_version 递增），
 *      所以这两个操作要提示管理员这一点。
 *   3. 密码**不设默认值**。建号时密码可留空，账号需自行设置或由管理员重置。
 *      留空账号无法登录（后端明确拒绝），这是刻意的。
 *
 * 列表刻意做成**只读**的：分组、角色、显示名、额度这些字段一律走「编辑」弹窗，
 * 行内不再挂下拉，连角色切换也不给按钮 —— 升级/降级都会让该用户所有会话立即失效，
 * 属于要看着后果再点确认的动作，不该由表格里的一次单击顺手完成。
 * 表格因此只留四个操作：编辑、重置密码、禁用/启用、删除。
 */
import { computed, onMounted, ref } from 'vue'
import { api, ApiFail } from '../api'
import type { Group, User, UserRole, UserStatus } from '../types'
import { fmtTokens, fmtDateTime, fmtBalance } from '../fmt'
import { toast, confirmBox } from '../ui'
import { checkPasswordStrength } from '../password'
import AppModal from '../components/AppModal.vue'

const users = ref<User[]>([])
/** 分组清单，用于把 group_id 渲染成名字、以及在表单里选择。 */
const groups = ref<Group[]>([])
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

// ---- 余额充值弹窗 ----
//
// 独立于「编辑用户」弹窗：余额是**高频**操作（充值与改角色/分组完全不是一类
// 动作），混进编辑弹窗会让每次改备注都要面对一屏无关字段，且两者共用一个
// 保存按钮时，误触的代价从「改个备注」升级成「动钱」。
const balFor = ref<User | null>(null)
/** 输入的是**元**（浮点），提交前换算成分。整数分才是账目单位。 */
const balAmount = ref<string>('')
const balErr = ref('')
const balBusy = ref(false)

async function load() {
  loading.value = true
  err.value = ''
  try {
    const [u, g] = await Promise.all([api.users(), api.groups()])
    users.value = u
    groups.value = g
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

function openBalance(u: User) {
  balErr.value = ''
  balAmount.value = ''
  balFor.value = u
}

/**
 * 元 → 整数分。四舍五入，与后端 store.YuanToCents 同一口径。
 *
 * 刻意在这里换算而不是把浮点元发给后端：余额是反复累加的账目，
 * 浮点的二进制表示无法精确表达十进制小数，而「发什么」必须在**界面这一侧**
 * 就定死，否则后端换一个换算函数，同一笔充值在对账时就会差一分钱。
 */
function yuanToCents(input: string): number | null {
  const n = Number(input)
  if (!Number.isFinite(n)) return null
  return Math.round(n * 100)
}

async function submitBalance() {
  const u = balFor.value
  if (!u) return
  balErr.value = ''
  if (balBusy.value) return
  const cents = yuanToCents(balAmount.value)
  if (cents === null || Number.isNaN(cents)) {
    balErr.value = '请输入一个金额'
    return
  }
  if (cents === 0) {
    // 后端也拒 0，但这里先说清：空输入框提交上来就是空串 → 0 分，
    // 而「调平」几乎一定是手滑而不是意图。
    balErr.value = '金额不能为 0'
    return
  }
  balBusy.value = true
  // 余额变更**也要挂 pending Map**：它是会动钱的写操作，连点两下就是双倍充值。
  // 复用本文件已有的那一套（见上方 setPending 的说明），不另起一个标志位 ——
  // 两个标志位各自置灰各自的按钮，却挡不住「充值点着的时候余额列表正在刷新」
  // 这种跨按钮的竞态。
  if (isPending(u.id)) {
    balBusy.value = false
    balErr.value = '该用户还有操作正在进行，请稍候'
    return
  }
  setPending(u.id, '充值中…')
  try {
    const updated = await api.adjustUserBalance(u.id, cents)
    const verb = cents > 0 ? '充值' : '扣减'
    toast(`${verb}成功，当前余额 ${fmtBalance(updated.balance_cents, updated.balance_unlimited)}`, 'ok')
    balFor.value = null
    await load()
  } catch (e) {
    balErr.value = e instanceof ApiFail ? e.message : '操作失败'
  } finally {
    setPending(u.id, null)
    balBusy.value = false
  }
}

onMounted(load)
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>用户</h1>
        <div class="sub">账号、配额与登录状态</div>
      </div>
      <div class="head-actions">
        <button class="btn" :disabled="loading" @click="load">刷新</button>
        <button class="btn btn-primary" @click="showCreate = true">新建用户</button>
      </div>
    </div>

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
                <span v-if="!u.balance_unlimited && u.balance_cents === 0" class="badge badge-err">
                  已用尽
                </span>
              </td>
              <td class="c-act">
                <div class="row-actions">
                  <button class="btn btn-sm" :disabled="isPending(u.id)" @click="openEdit(u)">编辑</button>
                  <button class="btn btn-sm" :disabled="isPending(u.id)" @click="openBalance(u)">充值</button>
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

    <!-- 充值：会动钱，所以两件事必须写在界面上而不是靠后端 400 兜底 ——
         ①「相对调整」的语义（填 100 是充 100，不是把余额设成 100）；
         ② 给**不限额**用户充值会把「不限」切成有限额。后者最容易被忽略：
         管理员以为在「送钱」，实际把对方从无限额度切成了一个具体的数额。 -->
    <AppModal :open="!!balFor" :title="balFor ? `充值 · ${balFor.username}` : '充值'" max-width="460px" :dismissable="false">
        <div class="msub">
          当前余额
          <b>{{ balFor ? fmtBalance(balFor.balance_cents, balFor.balance_unlimited) : '—' }}</b>
        </div>

        <div v-if="balFor?.balance_unlimited" class="fhint warn">
          该用户当前<strong>不限额</strong>。充值会把它切换成有限额 —— 充值后他只能使用填入的金额。
        </div>

        <label class="flabel" for="ba">金额（元）</label>
        <input id="ba" v-model="balAmount" class="input" type="number" step="0.01" placeholder="例如 100" />
        <div class="fhint">
          填<strong>正数</strong>充值，填<strong>负数</strong>扣减。这是**相对调整**：填 100 是「加 100 元」，
          不是「把余额设成 100 元」—— 误操作不会清零。
        </div>
        <!-- 预览：把即将发生的变化说清楚，而不是让管理员在提交后才在
             列表里数位数。 -->
        <div v-if="yuanToCents(balAmount)" class="fhint">
          调整后约为
          <b>{{ fmtBalance((balFor?.balance_cents ?? 0) + (yuanToCents(balAmount) ?? 0), false) }}</b>
        </div>
        <div v-if="balErr" class="fhint err">{{ balErr }}</div>
        <div class="form-actions">
          <button class="btn" @click="balFor = null">取消</button>
          <button class="btn btn-primary" :disabled="balBusy" @click="submitBalance">
            {{ balBusy ? '处理中…' : '确定' }}
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

</style>
