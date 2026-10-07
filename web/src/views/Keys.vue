<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { api, isAdmin, session } from '../api'
import { toast, confirmBox } from '../ui'
import { fmtDateTime, fmtTokens, copyText } from '../fmt'
import type { AccessKey, Group, KeyCreateResponse } from '../types'
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
 * 不再拉 /users：归属不再可选，后端强制为创建者自己，
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
        <!-- 自助发 key 对所有登录用户开放。归属恒为创建者自己（后端忽略
             请求里的 user_id），额度被封顶到用户级配额、限速被清零 ——
             所以「重建一把绕开限制」这条路不成立，管理员仍可对具体某把
             key 施加收紧策略（见 key_handler.Create）。 -->
        <button class="btn btn-primary" @click="openCreate">新建密钥</button>
      </div>
    </div>

    <div class="panel">
      <div v-if="loading && keys.length === 0" class="loading">加载中…</div>
      <!--加载失败**必须**与「确实没有数据」在界面上可区分：把请求失败呈现成
           「暂无数据」会让运维误判为无流量，从而排除掉网关/上游故障这个方向。
           与 Users/Groups 的错误态同口径。 -->
      <div v-else-if="err" class="empty"><div class="big">⚠</div>{{ err }}</div>
      <div v-else-if="keys.length === 0" class="empty">
        <div class="big">◇</div>
        还没有访问密钥
      </div>
      <div v-else class="tbl-wrap">
        <table class="tbl">
          <thead>
            <tr>
              <th>名称</th>
              <th>归属用户</th>
              <th>限制</th>
              <th class="num-h">用量</th>
              <th>有效期</th>
              <th class="c-act">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="k in keys" :key="k.id">
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
          <!-- 归属不在这里选：任何人（含管理员）建 key 都只归属自己，
               后端会强制覆盖并忽略请求里的 user_id。要替别人建，
               建好后在列表里编辑归属（仅管理员可改）。 -->
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

<style scoped>
/* 名称与有效期两列各两行（主信息 + 压暗的补充信息），行高因此天然一致。
   .cell-key 骨架用全局的（styles.css），有效期是本页特有的，留这里。 */
.cell-exp { display: flex; align-items: center; gap: 6px; flex-wrap: nowrap; }
</style>
