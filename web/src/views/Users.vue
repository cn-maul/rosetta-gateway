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
 */
import { onMounted, ref } from 'vue'
import { api, ApiFail } from '../api'
import type { Group, User, UserRole, UserStatus } from '../types'
import { fmtTokens, fmtDateTime } from '../fmt'
import { toast, confirmBox } from '../ui'

const users = ref<User[]>([])
/** 分组清单，用于把 group_id 渲染成名字、以及在表单里选择。 */
const groups = ref<Group[]>([])
const loading = ref(false)
const err = ref('')

const showCreate = ref(false)
const cName = ref('')
const cDisplay = ref('')
const cPassword = ref('')
const cRole = ref<UserRole>('user')
const cGroup = ref('')
const cQuota = ref<number>(0)
const cRemark = ref('')
const cErr = ref('')
const cBusy = ref(false)

const pwdFor = ref<User | null>(null)
const pwdValue = ref('')
const pwdErr = ref('')

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

/** groupName 把 group_id 渲染成组名。
 *
 * 组被删掉、或这个 id 在清单里查不到时显示「未知分组」而不是空 ——
 * 空会看起来像「未分组」，而「未分组 = 不限制模型」，两者含义完全不同。 */
function groupName(id: string): string {
  if (!id) return ''
  const g = groups.value.find((x) => x.id === id)
  return g ? g.name : '未知分组'
}

/** changeGroup 换组/移出分组。
 *
 * 传空串是**明确的「移出分组」**（后端用指针区分「不传」与「传空」），
 * 所以这里显式传 ""，不能用 undefined。 */
async function changeGroup(u: User, id: string) {
  try {
    await api.updateUser(u.id, { group_id: id })
    toast(id ? `已移到「${groupName(id)}」` : '已移出分组（模型不再受限）', 'ok')
    await load()
  } catch (e) {
    toast(e instanceof ApiFail ? e.message : '操作失败', 'err')
    await load()
  }
}

async function create() {
  if (cBusy.value) return
  cErr.value = ''
  if (!cName.value.trim()) {
    cErr.value = '用户名必填'
    return
  }
  cBusy.value = true
  try {
    await api.createUser({
      username: cName.value.trim(),
      display_name: cDisplay.value.trim(),
      // 密码留空则不传 —— 后端接受空密码，建出「尚未设置密码」的账号
      password: cPassword.value || undefined,
      role: cRole.value,
      group_id: cGroup.value || undefined,
      quota_tokens: Number(cQuota.value) || 0,
      remark: cRemark.value.trim(),
    })
    toast('已创建', 'ok')
    showCreate.value = false
    cName.value = cDisplay.value = cPassword.value = cRemark.value = ''
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

async function patch(id: string, b: Parameters<typeof api.updateUser>[1]) {
  try {
    await api.updateUser(id, b)
    await load()
  } catch (e) {
    toast(e instanceof ApiFail ? e.message : '操作失败', 'err')
  }
}

function toggleRole(u: User) {
  const next: UserRole = u.role === 'admin' ? 'user' : 'admin'
  if (u.is_self) {
    toast('不能修改自己的角色（会把自己锁在门外）', 'err')
    return
  }
  void patch(u.id, { role: next })
}

function toggleStatus(u: User) {
  if (u.is_self) {
    toast('不能禁用自己的账号', 'err')
    return
  }
  const next: UserStatus = u.status === 'active' ? 'disabled' : 'active'
  if (next === 'disabled') {
    toast('禁用后该用户的所有登录状态会立即失效', 'ok')
  }
  void patch(u.id, { status: next })
}

async function askDelete(u: User) {
  if (u.is_self) {
    toast('不能删除自己的账号', 'err')
    return
  }
  const ok = await confirmBox({
    title: '删除用户',
    body: `将删除「${u.username}」及其名下 ${u.key_count} 把密钥。用量历史会保留。`,
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return
  try {
    await api.deleteUser(u.id)
    toast('已删除', 'ok')
    await load()
  } catch (e) {
    toast(e instanceof ApiFail ? e.message : '删除失败', 'err')
  }
}

async function resetPassword() {
  const u = pwdFor.value
  if (!u) return
  pwdErr.value = ''
  if (pwdValue.value.length < 8) {
    pwdErr.value = '密码至少 8 个字符'
    return
  }
  try {
    await api.resetUserPassword(u.id, pwdValue.value)
    toast('密码已重置，该用户需重新登录', 'ok')
    pwdFor.value = null
    pwdValue.value = ''
    await load()
  } catch (e) {
    pwdErr.value = e instanceof ApiFail ? e.message : '重置失败'
  }
}

function openReset(u: User) {
  pwdErr.value = ''
  pwdValue.value = ''
  pwdFor.value = u
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
      <div v-else class="row-list">
        <div v-for="u in users" :key="u.id" class="row">
          <div class="row-main">
            <div class="row-title">
              {{ u.display_name || u.username }}
              <span v-if="u.is_self" class="badge badge-accent">你自己</span>
              <span class="badge" :class="u.role === 'admin' ? 'badge-live' : 'badge-off'">
                {{ u.role === 'admin' ? '管理员' : '普通用户' }}
              </span>
              <span class="badge" :class="u.status === 'active' ? 'badge-live' : 'badge-off'">
                {{ u.status === 'active' ? '可用' : '已禁用' }}
              </span>
              <span v-if="!u.has_password" class="badge badge-off">未设密码</span>
            </div>
            <div class="row-sub">
              <span class="mono">{{ u.username }}</span>
              <template v-if="u.remark"> · {{ u.remark }}</template>
            </div>
            <div class="row-sub group-cell">
              分组：
              <select
                class="fin fin-sm"
                :value="u.group_id"
                @change="changeGroup(u, ($event.target as HTMLSelectElement).value)"
              >
                <option value="">未分组（模型不受限）</option>
                <option v-for="g in groups" :key="g.id" :value="g.id">
                  {{ g.name }}{{ g.models.length === 0 ? '（未限制模型）' : '' }}
                </option>
              </select>
              <span v-if="u.group_id && groupName(u.group_id) === '未知分组'" class="badge badge-err">
                分组已不存在
              </span>
            </div>
            <div class="row-sub num">
              密钥 {{ u.key_count }} 把 · 用量 {{ fmtTokens(u.used_tokens) }}
              <template v-if="u.quota_tokens > 0">/ {{ fmtTokens(u.quota_tokens) }}</template>
            </div>
            <div v-if="u.last_login_at" class="row-sub num">
              上次登录 {{ fmtDateTime(u.last_login_at) }}
            </div>
          </div>
          <div class="row-actions">
            <button class="btn btn-sm" :disabled="u.is_self" :title="u.is_self ? '不能修改自己的角色' : ''" @click="toggleRole(u)">
              {{ u.role === 'admin' ? '降为用户' : '升为管理员' }}
            </button>
            <button class="btn btn-sm" @click="openReset(u)">重置密码</button>
            <button class="btn btn-sm" :disabled="u.is_self" :title="u.is_self ? '不能禁用自己的账号' : ''" @click="toggleStatus(u)">
              {{ u.status === 'active' ? '禁用' : '启用' }}
            </button>
            <button class="btn btn-sm btn-danger" :disabled="u.is_self" :title="u.is_self ? '不能删除自己' : ''" @click="askDelete(u)">
              删除
            </button>
          </div>
        </div>
      </div>
    </div>

    <!-- 新建用户 -->
    <div v-if="showCreate" class="modal-mask" @click.self="showCreate = false">
      <div class="modal">
        <h2>新建用户</h2>
        <label class="flabel" for="cu">用户名</label>
        <input id="cu" v-model="cName" class="fin" placeholder="3-32 位字母数字-_." />
        <div class="fhint">登录用。不支持中文与空格。</div>

        <label class="flabel" for="cd">显示名</label>
        <input id="cd" v-model="cDisplay" class="fin" placeholder="可选，界面上展示的名字" />

        <label class="flabel" for="cp">初始密码</label>
        <input id="cp" v-model="cPassword" type="password" class="fin" placeholder="留空则由用户自行设置" />
        <div class="fhint">留空的账号无法登录，需重置密码后才能使用。</div>

        <label class="flabel" for="cr">角色</label>
        <select id="cr" v-model="cRole" class="fin">
          <option value="user">普通用户</option>
          <option value="admin">管理员</option>
        </select>

        <label class="flabel" for="cg">分组</label>
        <select id="cg" v-model="cGroup" class="fin">
          <option value="">未分组（模型不受限）</option>
          <option v-for="g in groups" :key="g.id" :value="g.id">
            {{ g.name }}{{ g.models.length === 0 ? '（未限制模型）' : '' }}
          </option>
        </select>
        <div class="fhint">分组决定这个人能用哪些模型。未分组 = 不限制。</div>

        <label class="flabel" for="cq">额度上限（token）</label>
        <input id="cq" v-model.number="cQuota" type="number" min="0" class="fin" />
        <div class="fhint">0 = 不限</div>

        <label class="flabel" for="crm">备注</label>
        <input id="crm" v-model="cRemark" class="fin" placeholder="可选" />

        <div v-if="cErr" class="fhint err">{{ cErr }}</div>

        <div class="modal-actions">
          <button class="btn" @click="showCreate = false">取消</button>
          <button class="btn btn-primary" :disabled="cBusy" @click="create">
            {{ cBusy ? '创建中…' : '创建' }}
          </button>
        </div>
      </div>
    </div>

    <!-- 重置密码 -->
    <div v-if="pwdFor" class="modal-mask" @click.self="pwdFor = null">
      <div class="modal">
        <h2>重置密码</h2>
        <div class="sub">为「{{ pwdFor.username }}」设置新密码</div>
        <div class="fhint warn">重置后该用户的所有登录状态立即失效。</div>
        <label class="flabel" for="np">新密码</label>
        <input id="np" v-model="pwdValue" type="password" class="fin" autocomplete="new-password" />
        <div v-if="pwdErr" class="fhint err">{{ pwdErr }}</div>
        <div class="modal-actions">
          <button class="btn" @click="pwdFor = null">取消</button>
          <button class="btn btn-primary" @click="resetPassword">确定</button>
        </div>
      </div>
    </div>
  </main>
</template>

<style scoped>
.row-actions { display: flex; gap: 6px; flex-wrap: wrap; align-items: flex-start; }
.flabel { display: block; font-size: 12px; color: var(--muted); margin: 12px 0 4px; }
.fin {
  width: 100%;
  padding: 8px 10px;
  border: 1px solid var(--border);
  border-radius: var(--r-thumb);
  background: var(--background);
  color: var(--foreground);
  font-size: 14px;
}
.fin:focus-visible { outline: 2px solid var(--accent); outline-offset: 1px; }
.fhint { font-size: 12px; color: var(--muted); margin-top: 4px; }
/* 分组选择器做成行内小控件：换组是高频操作，塞进弹窗要多点两下。 */
.group-cell { display: flex; align-items: center; gap: 6px; }
.fin-sm { width: auto; padding: 3px 6px; font-size: 12px; }
.fhint.warn { color: var(--danger); }
.fhint.err { color: var(--danger); margin-top: 10px; }
.modal-mask {
  position: fixed; inset: 0; z-index: 50;
  background: var(--backdrop);
  display: grid; place-items: center;
  padding: 20px;
}
.modal {
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--r-sheet);
  padding: 20px;
  width: 100%;
  max-width: 420px;
  max-height: 90dvh;
  overflow-y: auto;
}
.modal h2 { margin: 0 0 4px; font-size: 15px; }
.modal-actions { display: flex; gap: 8px; justify-content: flex-end; margin-top: 20px; }
</style>
