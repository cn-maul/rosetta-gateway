<script setup lang="ts">
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { api, saveToken } from '../api'
import { toast, confirmBox } from '../ui'
import { fmtMoney } from '../fmt'
import AppModal from '../components/AppModal.vue'
import type { Provider, UpstreamModel } from '../types'

// ---------- 分类页：设置项按分类分页展示，一次只看一类 ----------
type TabKey = 'model' | 'runtime' | 'security' | 'price'

const TABS: { key: TabKey; label: string; sub: string }[] = [
  { key: 'model', label: '模型默认', sub: '全局默认值；模型容量探测不到时回落到这里' },
  { key: 'runtime', label: '运行时', sub: '各类超时与自动故障转移的全局默认' },
  { key: 'security', label: '安全', sub: '管理后台的登录凭据' },
  { key: 'price', label: '模型价格', sub: '按供应商 × 模型配置单价，总览「费用」按此实时估算' },
]
const tab = ref<TabKey>('model')

const tabSub = computed(() => TABS.find((t) => t.key === tab.value)?.sub ?? '')

const loading = ref(true)
const saving = ref(false)
const form = reactive({
  default_context_window: 8192,
  default_max_output_tokens: 4096,
  // 运行时全局默认（超时 + 故障转移策略）。原先散落在每条路由上，现统一在此配置。
  upstream_timeout_ms: 120000,
  stream_idle_timeout_ms: 60000,
  stream_first_token_timeout_ms: 30000,
  failover_max_targets: 3,
  failover_failure_threshold: 3,
})

// 密码设置相关
const passwordForm = reactive({
  open: false,
  currentPassword: '',
  newPassword: '',
  confirmPassword: '',
})
const hasPassword = ref(false)
// 当前凭据来源："password_file" | "config_token" | "none" | "locked"。
// 决定「当前密码」框里该填什么：设过密码填密码，否则填 config 的 admin_token。
const authSource = ref('')
// 提交锁：防双击并发两次 password/set（两次写盘 + 两次 rename，没有意义且徒增竞态）。
const changing = ref(false)

async function load() {
  loading.value = true
  try {
    const s = await api.settings()
    form.default_context_window = s.default_context_window
    form.default_max_output_tokens = s.default_max_output_tokens
    form.upstream_timeout_ms = s.upstream_timeout_ms
    form.stream_idle_timeout_ms = s.stream_idle_timeout_ms
    form.stream_first_token_timeout_ms = s.stream_first_token_timeout_ms
    form.failover_max_targets = s.failover_max_targets
    form.failover_failure_threshold = s.failover_failure_threshold
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('加载设置失败：' + (e as Error).message, 'err')
  } finally {
    loading.value = false
  }
  await loadPasswordStatus()
}

// 凭据状态是**独立**信息源（/password/check 免鉴权），绝不能和 settings 共用 try。
// 共用时 settings 一旦失败就会连坐：界面显示「尚未设置密码」并藏起「当前密码」输入框，
// 于是用户以为在改密码，实际提交的是「首次设置」请求。
async function loadPasswordStatus() {
  try {
    const res = await fetch('/admin/api/password/check')
    if (!res.ok) return
    const data = await res.json()
    hasPassword.value = !!data.has_password
    authSource.value = data.source ?? ''
  } catch (e) {
    console.error('检查密码状态失败:', e)
  }
}

// 两个分类共用一张表单与一个保存接口（PUT /settings 要求全量字段），
// 所以只校验**当前分类**的字段：另一个分类的值是载入时后端校验过的原值，
// 不该在保存这一分类时把用户挡下来。
async function save(target: 'model' | 'runtime') {
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

function openPasswordModal() {
  passwordForm.currentPassword = ''
  passwordForm.newPassword = ''
  passwordForm.confirmPassword = ''
  passwordForm.open = true
}

async function changePassword() {
  if (changing.value) return
  if (passwordForm.newPassword.length < 6) {
    toast('新密码长度至少为6位', 'err')
    return
  }
  if (passwordForm.newPassword !== passwordForm.confirmPassword) {
    toast('两次输入的新密码不一致', 'err')
    return
  }
  // 已有凭据时 password/set 必须带旧凭据才放行。旧实现只在「填了当前密码」时才加
  // Authorization 头，留空就直接发出去 —— 拿回一个 401，用户看到的是「设置密码失败」，
  // 完全猜不到是「没填当前密码」。这里当场拦住，并说清原因。
  if (hasPassword.value && !passwordForm.currentPassword) {
    toast('请先填写当前密码', 'err')
    return
  }

  changing.value = true
  try {
    const headers: Record<string, string> = { 'Content-Type': 'application/json' }
    if (hasPassword.value && passwordForm.currentPassword) {
      headers['Authorization'] = 'Bearer ' + passwordForm.currentPassword
    }

    const res = await fetch('/admin/api/password/set', {
      method: 'POST',
      headers,
      body: JSON.stringify({ password: passwordForm.newPassword }),
    })

    // 网关错误时 body 应是 JSON，但代理层 502/504 之类不是 —— 直接 res.json() 会抛
    // SyntaxError，被下面的 catch 裹成一句和真实原因无关的提示。
    const data = await res.json().catch(() => null)
    if (res.ok) {
      toast(data?.message || '密码已更新')
      passwordForm.open = false
      hasPassword.value = true
      // 旧凭据即刻失效（校验优先使用新密码），必须当场换掉本地令牌，
      // 否则下一次请求就 401 —— 表现为「改完密码反而被锁在外面」。
      saveToken(passwordForm.newPassword)
    } else {
      toast(data?.error?.message || `设置密码失败（HTTP ${res.status}）`, 'err')
    }
  } catch (e) {
    toast('设置密码失败：' + (e as Error).message, 'err')
  } finally {
    changing.value = false
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
    await Promise.all(
      providers.value.map(async (p) => {
        modelsMap[p.id] = await api.models(p.id)
      }),
    )
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

// 删除价格：行内按钮与编辑弹窗共用。确认后把三项单价清零（= 未配置，费用按 0 计）。
async function deletePrice(modelId: string, label: string): Promise<boolean> {
  const ok = await confirmBox({
    title: `删除「${label}」的价格？`,
    body: '删除后该模型的用量在费用统计里按 0 元计。',
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
    <div class="set-tabs">
      <button
        v-for="t in TABS"
        :key="t.key"
        class="set-tab"
        :class="{ active: tab === t.key }"
        type="button"
        @click="tab = t.key"
      >
        {{ t.label }}
      </button>
    </div>

    <!-- 分类 1：模型默认 -->
    <div v-if="tab === 'model'" class="panel">
      <div v-if="loading" class="loading">加载中…</div>
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
      <form v-else class="form-grid" style="max-width: 560px" @submit.prevent="save('runtime')">
        <h2 class="section-h span2">超时（毫秒）</h2>
        <div class="field span2">
          <label>非流式整体超时</label>
          <input v-model.number="form.upstream_timeout_ms" class="input num" type="number" min="1" step="1" />
          <span class="tip">一次非流式请求最多等多久；超时后按可转移错误处理（有机会切换下一个上游）</span>
        </div>
        <div class="field span2">
          <label>流式首字超时（TTFT）</label>
          <input v-model.number="form.stream_first_token_timeout_ms" class="input num" type="number" min="1" step="1" />
          <span class="tip">首字迟迟不来即掐流并切换下一个上游；仅在尚未写出任何字节时生效</span>
        </div>
        <div class="field span2">
          <label>流式空闲超时</label>
          <input v-model.number="form.stream_idle_timeout_ms" class="input num" type="number" min="1" step="1" />
          <span class="tip">已开始出字后，多久没有任何新事件即判定卡死（按截断收尾）</span>
        </div>

        <h2 class="section-h span2">自动故障转移</h2>
        <div class="field span2">
          <label>最多尝试目标数</label>
          <input v-model.number="form.failover_max_targets" class="input num" type="number" min="1" step="1" />
          <span class="tip">一次请求最多打某条路由链上的几个目标。是否启用按路由单独开关（路由页）</span>
        </div>
        <div class="field span2">
          <label>失败熔断阈值</label>
          <input v-model.number="form.failover_failure_threshold" class="input num" type="number" min="1" step="1" />
          <span class="tip">某目标连续失败几次后临时摘除（60 秒冷却），期间请求自动让位给链上下一个目标</span>
        </div>

        <div class="field span2 form-actions" style="padding: 0">
          <button type="submit" class="btn btn-primary" :disabled="saving">
            {{ saving ? '保存中…' : '保存设置' }}
          </button>
        </div>
      </form>
    </div>

    <!-- 分类 3：安全 -->
    <div v-else-if="tab === 'security'" class="panel">
      <div style="display: flex; align-items: center; gap: 1rem">
        <div>
          <div style="font-weight: 600">管理密码</div>
          <div style="color: var(--text-3); font-size: 13px; margin-top: 4px">
            <template v-if="!hasPassword">尚未设置密码（任何人都能打开后台）</template>
            <template v-else-if="authSource === 'config_token'">
              当前使用 <code>config.json</code> 的 <code>admin_token</code> 登录
            </template>
            <template v-else-if="authSource === 'locked'">凭据文件已损坏，后台处于锁定态</template>
            <template v-else>已设置密码（存放在 admin_auth.json）</template>
          </div>
        </div>
        <button class="btn" @click="openPasswordModal">
          {{ hasPassword ? '修改密码' : '设置密码' }}
        </button>
      </div>
    </div>

    <!-- 分类 4：模型价格 -->
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

    <!-- 价格：新增 / 编辑 -->
    <AppModal
      :open="pForm.open"
      :title="pForm.editing ? '编辑模型价格' : '新增模型价格'"
      max-width="520px"
      @close="pForm.open = false"
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

    <!-- 密码设置弹窗 -->
    <AppModal :open="passwordForm.open" title="设置管理密码" max-width="500px" @close="passwordForm.open = false">
      <form @submit.prevent="changePassword">
        <div class="form-grid">
          <div v-if="hasPassword" class="field span2">
            <label>当前密码 *</label>
            <input v-model="passwordForm.currentPassword" class="input" type="password" placeholder="请输入当前密码" />
            <span class="tip">
              <template v-if="authSource === 'config_token'">
                当前凭据来自配置文件，这里请填 <code>config.json</code> 的 <code>admin_token</code>（或 ADMIN_TOKEN 环境变量的值）。
                设置新密码后该令牌立即失效。
              </template>
              <template v-else>填写你之前设置的管理密码。</template>
            </span>
          </div>
          <div class="field span2">
            <label>新密码 *（至少6位）</label>
            <input v-model="passwordForm.newPassword" class="input" type="password" placeholder="请输入新密码" />
          </div>
          <div class="field span2">
            <label>确认新密码 *</label>
            <input v-model="passwordForm.confirmPassword" class="input" type="password" placeholder="请再次输入新密码" />
          </div>
        </div>
        <div class="form-actions">
          <button type="button" class="btn btn-ghost" @click="passwordForm.open = false">取消</button>
          <button type="submit" class="btn btn-primary" :disabled="changing">{{ changing ? '保存中…' : '确定' }}</button>
        </div>
      </form>
    </AppModal>
  </main>
</template>

<style scoped>
/* 分区小标题：把一个长表单切成「超时 / 故障转移」两块 */
.section-h {
  font-size: 13px;
  font-weight: 650;
  color: var(--text-2);
  margin: 6px 0 -2px;
  padding-bottom: 6px;
  border-bottom: 1px solid var(--hairline);
}

/* 分类页签：形态对齐顶栏 .tabs（胶囊分段），选中态用 segment 底色 */
.set-tabs {
  display: flex;
  gap: 2px;
  width: max-content;
  padding: 3px;
  margin-bottom: 14px;
  border-radius: var(--r-pill);
  background: var(--default);
}
.set-tab {
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
.set-tab:hover {
  color: var(--foreground);
}
.set-tab.active {
  color: var(--segment-foreground);
  background: var(--segment);
  box-shadow: var(--field-shadow);
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
.tbl-wrap {
  overflow-x: auto;
}
.tbl {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
}
.tbl th,
.tbl td {
  text-align: left;
  padding: 9px 12px;
  white-space: nowrap;
}
.tbl thead th {
  color: var(--text-3);
  font-weight: 500;
  font-size: 12px;
  border-bottom: 1px solid var(--separator);
}
.tbl tbody tr {
  border-bottom: 1px solid var(--separator);
}
.tbl tbody tr:last-child {
  border-bottom: none;
}
.tbl tbody tr:hover {
  background: var(--hover);
}
.num-h {
  text-align: right;
}
.dim {
  color: var(--text-3);
}
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
.pager {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 8px;
  flex-wrap: wrap;
  margin-top: 14px;
  padding-top: 12px;
  border-top: 1px solid var(--separator);
}
.pager-info {
  font-size: 12.5px;
  color: var(--text-2);
  margin: 0 4px;
}
.pager-info b {
  font-variant-numeric: tabular-nums;
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
