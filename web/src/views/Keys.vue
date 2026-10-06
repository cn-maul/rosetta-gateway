<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { api, isAdmin } from '../api'
import { toast, confirmBox } from '../ui'
import { fmtDateTime, fmtTokens, copyText } from '../fmt'
import type { AccessKey, Group, KeyCreateResponse, User } from '../types'
import AppModal from '../components/AppModal.vue'
import ModelPicker from '../components/ModelPicker.vue'

const loading = ref(true)
const keys = ref<AccessKey[]>([])
// 管理员建 key 时可选归属。普通用户不显示这个选择 ——
// 后端会忽略请求里的 user_id 并强制归为自己（见 key_handler.Create）。
const userOptions = ref<User[]>([])
// key 级模型白名单的可选项。**惰性加载**：只在打开表单时拉一次，
// 因为多数访问者只是来看一眼列表，不该为这个多打一个请求。
const modelOptions = ref<string[]>([])
const modelsRestricted = ref(false)
let modelsLoaded = false
// 分组下拉（key 级分组覆盖，仅管理员）。与「归属用户」同理惰性加载。
const groupOptions = ref<Group[]>([])

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

async function load() {
  loading.value = true
  try {
    keys.value = await api.keys()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('加载失败：' + (e as Error).message, 'err')
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
  // 归属用户 id（仅管理员可选）。留空 = 建出无归属 key，
  // 而那把 key 会被退役、鉴权 401 —— 所以表单里默认引导管理员选一个。
  userId: '',
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
  form.userId = ''
  form.models = []
  form.days = 0
  form.ips = ''
  form.groupId = ''
  form.open = true
  void ensureModelOptions()
  void ensureGroupOptions()
  // 惰性拉用户列表：普通用户不该调 /users（会 403），
  // 所以只在管理员点开新建表单时才拉。
  if (isAdmin() && userOptions.value.length === 0) {
    void api
      .users()
      .then((us) => (userOptions.value = us))
      .catch(() => {
        /* 拉不到就只影响下拉框，不阻塞建 key */
      })
  }
}

async function submitCreate() {
  if (!form.name.trim()) {
    toast('名称为必填项', 'err')
    return
  }
  try {
    const created = await api.createKey({
      name: form.name.trim(),
      quota_tokens: Number(form.quota) || 0,
      rpm_limit: Number(form.rpm) || 0,
      tpm_limit: Number(form.tpm) || 0,
      // 仅管理员需要传；普通用户传了也会被后端忽略并强制归自己
      user_id: isAdmin() ? form.userId || undefined : undefined,
      allowed_models: form.models,
      expires_at: expiresAtFromDays(form.days),
      allowed_ips: form.ips.trim(),
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
      quota_tokens: Number(eForm.quota) || 0,
      rpm_limit: Number(eForm.rpm) || 0,
      tpm_limit: Number(eForm.tpm) || 0,
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

onMounted(load)
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
        <button class="btn btn-primary" @click="openCreate">新建密钥</button>
      </div>
    </div>

    <div class="panel">
      <div v-if="loading && keys.length === 0" class="loading">加载中…</div>
      <div v-else-if="keys.length === 0" class="empty">
        <div class="big">◇</div>
        还没有访问密钥
      </div>
      <div v-else class="row-list">
        <div v-for="k in keys" :key="k.id" class="row">
          <div class="row-main">
            <div class="row-title">
              {{ k.name }}
              <span class="badge" :class="k.enabled ? 'badge-live' : 'badge-off'">{{ k.enabled ? '启用' : '停用' }}</span>
              <span v-if="k.quota_tokens > 0 && k.used_tokens >= k.quota_tokens" class="badge badge-off">配额已用尽</span>
              <!-- 白名单非空才标：空 = 不限制，标出来反而让人以为受限。 -->
              <span v-if="k.allowed_models.length > 0" class="badge badge-live">
                限 {{ k.allowed_models.length }} 个模型
              </span>
              <!-- 已过期：最需要被一眼看到的状态。它仍然 enabled，
                   但数据面一律 403，所以界面上必须与「停用」区分开。 -->
              <span v-if="k.expires_at > 0 && k.expires_at <= Date.now()" class="badge badge-off">
                已过期
              </span>
              <span v-if="k.allowed_ips" class="badge badge-accent" title="只允许指定来源网段调用">限源</span>
              <span v-if="k.group_id" class="badge badge-accent" title="该密钥使用指定分组的模型范围">分组覆盖</span>
              <!-- 无归属必须显式标注：这类 key 会被迁移退役、鉴权直接 401，
                   不标注的话运维会以为它还能用。 -->
              <span v-if="!k.user_id" class="badge badge-off" title="无归属的密钥会被退役，鉴权返回 401">
                无归属（不可用）
              </span>
            </div>
            <div class="row-sub mono">{{ k.key_prefix }}</div>
            <div v-if="k.user_id" class="row-sub">
              归属：<span class="mono">{{ k.username || k.user_id }}</span>
            </div>
            <div class="row-sub num">
              建于 {{ fmtDateTime(k.created_at) }}
              <template v-if="k.expires_at > 0">· 到期 {{ fmtDateTime(k.expires_at) }}（剩 {{ daysLeft(k.expires_at) }} 天）</template>
            </div>
            <div v-if="k.allowed_ips" class="row-sub mono">允许来源：{{ k.allowed_ips }}</div>
            <div class="row-sub num">
              用量：{{ fmtTokens(k.used_tokens) }}
              <template v-if="k.quota_tokens > 0">/ {{ fmtTokens(k.quota_tokens) }}</template>
              <template v-else>· 不限</template>
              <template v-if="(k.rpm_limit ?? 0) > 0 || (k.tpm_limit ?? 0) > 0">
                · 限速：<template v-if="(k.rpm_limit ?? 0) > 0">{{ k.rpm_limit }} req/min</template>
                <template v-if="(k.rpm_limit ?? 0) > 0 && (k.tpm_limit ?? 0) > 0"> & </template>
                <template v-if="(k.tpm_limit ?? 0) > 0">{{ fmtTokens(k.tpm_limit) }} tok/min</template>
              </template>
            </div>
          </div>
          <div class="row-side">
            <button class="btn btn-sm btn-ghost" @click="openEdit(k)">编辑</button>
            <button class="btn btn-sm btn-danger" @click="removeKey(k)">删除</button>
            <button class="switch" :class="{ on: k.enabled }" :title="k.enabled ? '停用' : '启用'" @click="toggleKey(k)"></button>
          </div>
        </div>
      </div>
    </div>

    <!-- 创建表单 -->
    <AppModal :open="form.open" title="新建访问密钥" max-width="500px" @close="form.open = false">
      <form @submit.prevent="submitCreate">
        <div class="form-grid">
          <div class="field span2">
            <label>名称 *</label>
            <input v-model="form.name" class="input" placeholder="cursor-主力 / 内部测试" />
          </div>
          <!-- 归属选择仅管理员可见。普通用户建的 key 自动归自己，
               不需要也不允许指定归属（后端会忽略这个字段）。 -->
          <div v-if="isAdmin()" class="field span2">
            <label>归属用户</label>
            <select v-model="form.userId" class="input">
              <option value="">（不指定 —— 该密钥不可用）</option>
              <option v-for="u in userOptions" :key="u.id" :value="u.id">
                {{ u.display_name || u.username }}（{{ u.username }}）
              </option>
            </select>
            <span class="tip">
              不指定归属的密钥会被迁移退役、鉴权直接返回 401。请务必选择。
            </span>
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
          <div class="field span2">
            <label>Token 配额</label>
            <input v-model.number="form.quota" class="input num" type="number" min="0" step="1" placeholder="0" />
            <span class="tip">累计 input+output token 上限，用尽后 /v1 返回 429；0 = 不限</span>
          </div>
          <div class="field">
            <label>RPM 限速</label>
            <input v-model.number="form.rpm" class="input num" type="number" min="0" step="1" placeholder="0" />
            <span class="tip">每分钟请求数上限；0 = 不限</span>
          </div>
          <div class="field">
            <label>TPM 限速</label>
            <input v-model.number="form.tpm" class="input num" type="number" min="0" step="1" placeholder="0" />
            <span class="tip">每分钟 token 上限（估算预占+事后校正）；0 = 不限</span>
          </div>
        </div>
        <div class="form-actions">
          <button type="button" class="btn btn-ghost" @click="form.open = false">取消</button>
          <button type="submit" class="btn btn-primary">创建</button>
        </div>
      </form>
    </AppModal>

    <!-- 一次性明文展示 -->
    <AppModal :open="plainKeyBox.open" title="密钥已创建" max-width="520px">
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

    <!-- 编辑表单 -->
    <AppModal :open="eForm.open" title="编辑密钥" max-width="500px" @close="eForm.open = false">
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
          <div class="field span2">
            <label>Token 配额</label>
            <input v-model.number="eForm.quota" class="input num" type="number" min="0" step="1" placeholder="0" />
            <span class="tip">累计 input+output token 上限，用尽后 /v1 返回 429；0 = 不限</span>
          </div>
          <div class="field">
            <label>RPM 限速</label>
            <input v-model.number="eForm.rpm" class="input num" type="number" min="0" step="1" placeholder="0" />
            <span class="tip">每分钟请求数上限；0 = 不限</span>
          </div>
          <div class="field">
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
