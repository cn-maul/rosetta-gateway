<script setup lang="ts">
import DataTable from '../components/DataTable.vue'
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import { toast, confirmBox } from '../ui'
import type { RouteCatalogProvider, RouteCatalogEntry, UpstreamModel, Route, RouteTargetInput, Settings } from '../types'
import { fmtSec } from '../fmt'
import { modelPriceLine } from '../modelPrice'
import AppModal from '../components/AppModal.vue'
import DetailDrawer from '../components/DetailDrawer.vue'
import { useRoute, useRouter } from 'vue-router'
import { useQueryText } from '../composables/useQueryText'

const err = ref('')
const loading = ref(true)
const routes = ref<RouteCatalogEntry[]>([])
const providers = ref<RouteCatalogProvider[]>([])
// upstream_models.id（短哈希外键）→ 模型对象；Resolve 时拿的是这个外键
const mmap = ref<Record<string, UpstreamModel>>({})
const runtime = ref<Settings | null>(null)
const runtimeError = ref('')
const runtimeLoading = ref(false)

const providerMap = computed(() => {
  const m: Record<string, RouteCatalogProvider> = {}
  for (const p of providers.value) m[p.id] = p
  return m
})

const route = useRoute()
const router = useRouter()
const search = useQueryText('q')
const status = useQueryText('status')
const selectedId = useQueryText('route')
const selected = computed(() => routes.value.find(r => r.id === selectedId.value))
const previewTargets = computed(() => selected.value?.targets ?? [])
const pending = ref(new Set<string>())
const filtered = computed(() => routes.value.filter(r => {
  const q = search.value.trim().toLowerCase()
  const matches = !q || [r.public_name, providerMap.value[r.provider_id]?.name ?? '', modelLabel(r)].some(v => v.toLowerCase().includes(q))
  return matches && (!status.value || status.value === 'enabled' && r.enabled || status.value === 'disabled' && !r.enabled || status.value === 'failover' && r.failover_enabled)
}))
function clearFilters() { void router.replace({ query: { ...route.query, q: undefined, status: undefined } }) }
function inspect(r: Route) { selectedId.value = r.id }
function editSelected() {
  const r = selected.value
  selectedId.value = ''
  if (r) void openRoute(r)
}
function targetIssue(providerId: string, modelId: string): string {
  const p = providerMap.value[providerId]
  const m = mmap.value[modelId]
  if (!p) return '上游不存在'
  if (!p.enabled) return '上游已停用'
  if (p.ready === false) return p.ready_reason || '上游凭据未就绪'
  if (!m) return '上游模型不存在'
  if (!m.enabled) return '上游模型已停用'
  if (typeof p.ready !== 'boolean') return '凭据状态未知'
  return ''
}
let loadSeq = 0

// 某个 provider 下可被路由选择的模型
function modelsOf(providerId: string): UpstreamModel[] {
  return Object.values(mmap.value).filter((m) => m.provider_id === providerId)
}

function modelLabel(routeRow: Route): string {
  const m = mmap.value[routeRow.upstream_model_id]
  if (m) return m.display_name ? `${m.display_name} (${m.model_id})` : m.model_id
  // 兜底：外键悬空时至少显示路由自身信息
  return routeRow.upstream_model_id
}

function modelPrice(modelId: string): string {
  const m = mmap.value[modelId]
  if (!m) return '价格未知'
  return modelPriceLine(m)
}

async function load() {
  const seq = ++loadSeq
  loading.value = true
  err.value = ''
  try {
    const catalog = await api.routeCatalog()
    if (seq !== loadSeq) return
    routes.value = catalog.routes
    providers.value = catalog.providers
    mmap.value = Object.fromEntries(catalog.models.map(m => [m.id, m]))
  } catch (e) {
    if (seq !== loadSeq) return
    if ((e as { status?: number }).status === 401) return
    // 失败**必须留下可见的错误态**：只弹 toast 的话，页面保留空列表/
    // 空表格，呈现成「暂无数据」—— 而真实原因是请求失败了。运维会据此
    // 判断「今天没有流量」，进而排除掉网关/上游故障这个真正的方向。
    // 与 Users/Groups 的持久错误态同口径（见其 v-else-if="err"）。
    err.value = '加载路由失败：' + (e as Error).message
    toast('加载失败：' + (e as Error).message, 'err')
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}

async function loadRuntime() {
  runtimeLoading.value = true
  runtimeError.value = ''
  try {
    runtime.value = await api.settings()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) runtimeError.value = '全局运行时策略暂不可读'
  } finally {
    runtimeLoading.value = false
  }
}

// ---------- 表单 ----------

interface TargetRow {
  provider_id: string
  upstream_model_id: string
  enabled: boolean
}

const form = reactive({
  open: false,
  editing: '',
  public_name: '',
  enabled: true,
  // 是否对这条路由启用故障转移。
  // 策略参数（尝试预算 / 熔断阈值 / 各类超时）是全局的，在「设置」页配置。
  failover_enabled: false,
  targets: [] as TargetRow[],
})

// 目标链加载失败的说明。非空 = 禁止保存。
//
// 为什么不能像别的展示字段那样「加载失败就留空」：保存走的是 PUT 整体替换
// （按数组顺序重排 position），一份伪造出来的单目标链写回去，等于把 position 1..N
// 全部删掉 —— 界面显示「保存成功」，实际故障转移链已经没了，且没有撤销路径。
const chainError = ref('')
const saveError = ref('')

/**
 * 目标链的两个「能不能保存」状态，缺一不可：
 *
 *   - chainLoading：链是**异步**取的。打开弹窗到响应回来这段窗口里，表单里
 *     只有一条由 route 主目标列拼出来的占位行 —— 此时放行保存，等于拿一份
 *     伪造的单目标链去做 PUT 整体替换。
 *   - chainReady：只有「本次打开的链确实取到了」才为真。加载失败（chainError）
 *     和「会话已失效」都停在假，永不放行。
 *
 * 光有 chainError 不够（它覆盖不到加载中），光有 chainLoading 也不够（加载
 * 结束后失败态会把它清回 false，继而放行）。两者叠起来才是「安全」。
 */
const chainLoading = ref(false)
const chainReady = ref(false)

/**
 * 打开弹窗的请求序号。
 *
 * 连点两条路由的「编辑」会并发两个 routeTargets 请求，而响应顺序不保证与点击
 * 顺序一致。没有序号时，慢到的**旧**响应会最后写入 form.targets —— 弹窗于是
 * 显示「A 路由的公开名 + B 路由的上游链」。这份串了的表单一旦保存，PUT 会把
 * B 的链整体写到 A 上（position 1..N 被重排覆盖），界面上完全看不出发生过什么。
 */
let chainSeq = 0

// 保存中：挡住回车连点造成的重复提交（每次提交都会整体替换一次链）。
const saving = ref(false)

// 链未就绪期间禁止一切改动与保存（覆盖「加载中」与「加载失败」两种情形）
const chainBlocked = computed(() => chainLoading.value || !chainReady.value || saving.value)

function blankTarget(): TargetRow {
  return { provider_id: '', upstream_model_id: '', enabled: true }
}

async function openRoute(r?: Route) {
  const seq = ++chainSeq
  form.open = true
  form.targets = []
  form.editing = r?.id ?? ''
  form.public_name = r?.public_name ?? ''
  form.enabled = r?.enabled ?? true
  form.failover_enabled = r?.failover_enabled ?? false
  chainError.value = ''
  saveError.value = ''
  // 每次打开都先回到「未就绪」：任何一次重新打开、以及任何一次并发的旧响应，
  // 在没有拿到本轮链之前都不允许保存。
  chainReady.value = false
  chainLoading.value = false

  if (r) {
    chainLoading.value = true
    try {
      const ts = await api.routeTargets(r.id)
      if (seq !== chainSeq) return // 期间又打开了别的路由，本响应已过时，丢弃
      form.targets = ts.length
        ? [...ts].sort((a, b) => a.position - b.position).map((t) => ({ provider_id: t.provider_id, upstream_model_id: t.upstream_model_id, enabled: t.enabled }))
        : [{ provider_id: r.provider_id, upstream_model_id: r.upstream_model_id, enabled: true }]
      chainReady.value = true
    } catch (e) {
      if (seq !== chainSeq) return
      if ((e as { status?: number }).status === 401) {
        // 401 只代表会话失效（api 侧已清令牌、App 会送回登录页）。
        // 这里必须**关掉弹窗**：留在打开状态的话，屏幕停在一份永远加载不出来的
        // 表单上，而真正的原因（请重新登录）只在 toast 里。
        form.open = false
        return
      }
      // 载荷仍是「主目标单行」，但只用于展示；chainReady 保持假，保存被拦住。
      chainError.value =
        '无法读取该路由的上游链：' +
        (e as Error).message +
        '。为避免用不完整的数据覆盖已有配置，保存已被禁用 —— 请先刷新页面重试。'
      form.targets = [{ provider_id: r.provider_id, upstream_model_id: r.upstream_model_id, enabled: true }]
    } finally {
      // 只清自己那一次的加载态：被丢弃的旧响应不能把新一轮的加载态提前抹掉，
      // 否则「点第二条路由」的瞬间按钮就解禁了 —— 而链还在路上。
      if (seq === chainSeq) chainLoading.value = false
    }
  } else {
    // 新建：链由用户现场填，没有「读回来」这一步，直接算就绪。
    form.targets = [blankTarget()]
    chainReady.value = true
  }
}

function closeEditor() {
  if (saving.value) return
  ++chainSeq
  chainLoading.value = false
  chainReady.value = false
  form.open = false
}

function addTarget() {
  form.targets.push(blankTarget())
}

function removeTarget(i: number) {
  if (form.targets.length <= 1) {
    toast('至少保留一个上游目标', 'err')
    return
  }
  form.targets.splice(i, 1)
}

function moveTarget(i: number, dir: -1 | 1) {
  const j = i + dir
  if (j < 0 || j >= form.targets.length) return
  const [row] = form.targets.splice(i, 1)
  form.targets.splice(j, 0, row)
}

/**
 * 一键把某项置顶到链首（2026-10-14）。
 *
 * 「未开故障转移」时请求**只打链首**，所以要换掉当前生效的上游，
 * 本质就是「把另一个模型挪到 position 0」。链长的时候靠 ↑ 要连点好几次，
 * 而这是个高频动作（上游挂了/换了供应商就切一次），值得一个专门的按钮。
 */
function pinTarget(i: number) {
  if (i <= 0 || i >= form.targets.length) return
  const [row] = form.targets.splice(i, 1)
  form.targets.unshift(row)
}

function onTargetProviderChange(row: TargetRow) {
  // 切 provider 后原模型可能不属于它，清空让用户重选
  const m = mmap.value[row.upstream_model_id]
  if (!m || m.provider_id !== row.provider_id) row.upstream_model_id = ''
}

async function submitRoute() {
  const targets = form.targets
  if (chainLoading.value) {
    toast('上游链仍在加载，请稍候', 'err')
    return
  }
  if (!chainReady.value) {
    // 与 chainError 同因：链没读回来就保存 = 用一份伪造的单目标链整体替换。
    // chainError 非空时它已经把原因写在弹窗里了，这里只补一句动作被拦下。
    toast('上游链未成功加载，已禁止保存（请先刷新页面）', 'err')
    return
  }
  if (saving.value) return
  if (!form.public_name.trim()) {
    toast('公开模型名为必填项', 'err')
    return
  }
  if (targets.length === 0 || targets.some((t) => !t.provider_id || !t.upstream_model_id)) {
    toast('每个上游目标都要选好 provider 与模型', 'err')
    return
  }

  // 链首即 route 的主目标列（后端 Create/Update 仍要求这两列非空）。
  const head = targets[0]
  const routeBody: Partial<Route> = {
    public_name: form.public_name.trim(),
    provider_id: head.provider_id,
    upstream_model_id: head.upstream_model_id,
    enabled: form.enabled,
    failover_enabled: form.failover_enabled,
  }
  const chain: RouteTargetInput[] = targets.map((t) => ({
    provider_id: t.provider_id,
    upstream_model_id: t.upstream_model_id,
    enabled: t.enabled,
  }))

  let isNew = false
  let routeSaved = false
  saveError.value = ''
  saving.value = true
  try {
    let routeId = form.editing
    if (routeId) {
      await api.updateRoute(routeId, routeBody)
    } else {
      const created = await api.createRoute(routeBody)
      routeId = created.id
      isNew = true
      // 走到这里 route 已经在库里了（后端建 route 时会种一条链首目标）。
      // 后面若写链失败，必须如实说「已建但链不完整」，不能报一句「保存失败」
      // 让用户以为什么都没发生 —— 他会再点一次「新建」，然后撞 public_name 冲突。
      form.editing = routeId
    }
    routeSaved = true
    // 目标链独立于 route 基础字段整体替换（按当前顺序重排 position）。
    await api.saveRouteTargets(routeId, chain)

    form.open = false
    toast(isNew ? '路由已创建' : '路由已更新')
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) {
      // isNew=true 时 route 已落库、只是链没写全；其余情况才是真正的「保存失败」。
      const hint = routeSaved
        ? (isNew ? '路由已创建，但完整目标链未保存：' : '路由基础信息已保存，但完整目标链未保存：')
        : '保存失败：'
      saveError.value = hint + (e as Error).message + (routeSaved ? '。请保留当前表单并重试保存；若退出，请先刷新检查当前配置。' : '')
      toast(hint + (e as Error).message, 'err')
    }
  } finally {
    saving.value = false
  }
}

async function toggleRoute(r: Route) {
  if (pending.value.has(r.id)) return
  pending.value.add(r.id)
  try {
    // 只发要改的那一个字段。PATCH 是字段级部分更新，回传整个对象有两个代价：
    //   1. r 来自列表快照，可能已被别的标签页改过 —— 全量回写会静默回滚那些字段；
    //   2. 回传 provider_id/upstream_model_id 会顺带触发一次「把链首对齐到主目标」，
    //      而启停一个路由本不该动它的目标链。
    await api.updateRoute(r.id, { enabled: !r.enabled })
    toast(!r.enabled ? '已启用' : '已停用')
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('操作失败：' + (e as Error).message, 'err')
  } finally {
    pending.value.delete(r.id)
  }
}

async function removeRoute(r: Route) {
  if (pending.value.has(r.id)) return
  const ok = await confirmBox({
    title: `删除路由「${r.public_name}」？`,
    body: '删除后该公开模型名立即不可用（客户端会收到 404 model_not_found）。',
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok || pending.value.has(r.id)) return
  pending.value.add(r.id)
  try {
    await api.deleteRoute(r.id)
    if (selectedId.value === r.id) selectedId.value = ''
    toast('已删除')
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('删除失败：' + (e as Error).message, 'err')
  } finally {
    pending.value.delete(r.id)
  }
}

onMounted(() => {
  void load()
  void loadRuntime()
})
</script>

<template>
  <main class="page routes-page">
    <div class="page-head">
      <div><h1>模型与路由</h1><div class="sub">客户端公开模型名 → 首选目标 → 有序后备链 · 全站配置</div></div>
      <div class="head-actions"><button class="btn" :disabled="loading" @click="load">刷新</button><button class="btn btn-primary" @click="openRoute()">新建公开模型</button></div>
    </div>
    <div class="data-toolbar">
      <label class="search-field"><span class="sr-only">搜索模型与路由</span><input v-model="search" class="input" type="search" placeholder="搜索公开名、首选上游或模型" /></label>
      <label><span class="sr-only">路由状态</span><select v-model="status" class="select"><option value="">全部路由</option><option value="enabled">已启用</option><option value="disabled">已停用</option><option value="failover">已开启故障转移</option></select></label>
      <span class="result-count">{{ filtered.length }} / {{ routes.length }} 个公开模型</span>
    </div>
    <div class="context-links"><RouterLink to="/models">模型目录 ↗</RouterLink><RouterLink to="/settings?tab=price">批量管理价格 ↗</RouterLink><span class="dim">单价：元 / 1M tokens · 参考价来自对应上游模型，实际费用取决于命中的目标</span></div>
    <DataTable class="panel table-panel" label="模型与路由列表" :loading="loading" :error="err" :has-data="!!routes.length" :empty="!filtered.length" @retry="load">
      <template #empty><template v-if="!routes.length">尚未配置公开模型。先接入上游、添加凭据与模型，再创建路由供客户端调用。<RouterLink to="/providers" class="btn btn-sm">接入上游</RouterLink></template><template v-else>没有匹配的路由<button class="btn btn-sm" @click="clearFilters">清除筛选</button></template></template>
        <table class="tbl routes-table">
          <thead><tr><th>公开模型名</th><th>首选目标</th><th>转发策略</th><th class="c-act">操作</th></tr></thead>
          <tbody><tr v-for="r in filtered" :key="r.id">
            <td class="route-identity"><button class="text-link name-link mono" @click="inspect(r)">{{ r.public_name }}</button><div class="sub-line"><span class="badge" :class="r.enabled ? 'badge-live' : 'badge-off'">{{ r.enabled ? '已启用' : '已停用' }}</span></div><div class="mobile-route-summary"><div>{{ providerMap[r.provider_id]?.name ?? r.provider_id }} → <span class="mono">{{ modelLabel(r) }}</span></div><span class="sub-line">{{ modelPrice(r.upstream_model_id) }}</span><span>{{ r.chain.total }} 个配置目标 · {{ r.chain.standby }} 个后备</span><span>{{ r.failover_enabled ? '自动故障转移' : '仅首选目标' }}</span><p v-if="targetIssue(r.provider_id, r.upstream_model_id)" class="target-warning">{{ targetIssue(r.provider_id, r.upstream_model_id) }}</p><button class="text-link chain-link" @click="inspect(r)">查看目标链与配置 →</button></div></td>
            <td class="route-target"><div>{{ providerMap[r.provider_id]?.name ?? r.provider_id }}</div><div class="mono sub-line">{{ modelLabel(r) }}</div><div class="sub-line price-line">{{ modelPrice(r.upstream_model_id) }}</div><div v-if="targetIssue(r.provider_id, r.upstream_model_id)" class="sub-line target-warning">{{ targetIssue(r.provider_id, r.upstream_model_id) }}</div></td>
            <td><span class="badge" :class="r.failover_enabled ? 'badge-accent' : 'badge-dim'">{{ r.failover_enabled ? '自动故障转移' : '仅首选目标' }}</span><div class="sub-line dim">{{ r.chain.total }} 个配置目标 · {{ r.chain.enabled }} 个启用 · {{ r.chain.standby }} 个后备</div><button class="text-link sub-line chain-link" @click="inspect(r)">查看完整目标链 →</button></td>
            <td class="c-act"><div class="row-actions"><button class="btn btn-act" @click="openRoute(r)">编辑</button><button class="switch" :class="{ on: r.enabled }" role="switch" :aria-checked="r.enabled" :disabled="pending.has(r.id)" :aria-label="(r.enabled ? '停用 ' : '启用 ') + r.public_name" @click="toggleRoute(r)"></button><details class="row-menu"><summary :aria-label="'更多操作：' + r.public_name">⋯</summary><div class="row-menu-content"><button class="btn btn-sm btn-danger" :disabled="pending.has(r.id)" @click="removeRoute(r)">删除路由</button></div></details></div></td>
          </tr></tbody>
        </table>
    </DataTable>

    <DetailDrawer :open="!!selectedId" :title="selected?.public_name || '目标链详情'" max-width="660px" @close="selectedId = ''">
      <p class="context-note">目标按配置位置排序。{{ selected?.failover_enabled ? '故障转移已开启；实际尝试受全局预算、熔断与启用状态影响。' : '故障转移已关闭，仅使用首选目标；后备目标不会自动接管。' }}</p>
      <p class="context-note">价格单位：元 / 1M tokens。<RouterLink to="/settings?tab=runtime" class="text-link">查看全局转发策略 ↗</RouterLink></p>
      <div v-if="!selected" class="empty">未找到该路由，请关闭后刷新列表。</div>
      <div v-else-if="!previewTargets.length" class="empty">未返回独立目标链，请进入编辑检查主目标配置。</div>
      <ol v-else class="target-timeline">
        <li v-for="(t, i) in previewTargets" :key="t.id" :class="{ 'target-disabled': !t.enabled || (i > 0 && !selected.failover_enabled) }">
          <span class="target-position">{{ i + 1 }}</span>
          <div class="target-content"><div class="target-heading"><strong>{{ i === 0 ? '首选目标' : '后备 ' + i }}</strong><span v-if="!t.enabled" class="badge badge-off">目标停用</span><span v-else-if="i > 0 && !selected.failover_enabled" class="badge badge-off">策略未启用</span></div><RouterLink :to="{ name: 'provider-detail', params: { id: t.provider_id }, query: { model: t.upstream_model_id } }" class="text-link">{{ providerMap[t.provider_id]?.name ?? t.provider_name ?? t.provider_id }} ↗</RouterLink><div class="mono target-model">{{ mmap[t.upstream_model_id]?.model_id ?? t.model_id ?? t.upstream_model_id }}</div><div class="sub-line price-line">{{ modelPrice(t.upstream_model_id) }} · 点击上游名可直接编辑模型</div><p v-if="targetIssue(t.provider_id, t.upstream_model_id)" class="target-warning">{{ targetIssue(t.provider_id, t.upstream_model_id) }}</p></div>
        </li>
      </ol>
      <template #footer><button v-if="selected" class="btn btn-ghost" :disabled="pending.has(selected.id)" @click="toggleRoute(selected)">{{ selected.enabled ? '停用路由' : '启用路由' }}</button><button v-if="selected" class="btn btn-ghost" :disabled="pending.has(selected.id)" @click="removeRoute(selected)">删除路由</button><button class="btn btn-ghost" @click="selectedId = ''">关闭</button><button v-if="selected" class="btn btn-primary" @click="editSelected">编辑目标链</button></template>
    </DetailDrawer>

    <!-- 表单填一半别丢：遮罩/Esc 都不关（:dismissable=false），只能走取消/保存 -->
    <AppModal
      :open="form.open"
      :title="form.editing ? '编辑路由' : '新建路由'"
      max-width="960px"
      max-height="920px"
      :dismissable="false"
    >
      <form class="route-form" @submit.prevent="submitRoute">
        <div class="form-grid">
          <div class="field span2">
            <label>公开模型名 *</label>
            <input v-model="form.public_name" :disabled="chainBlocked" class="input mono" placeholder="gpt-4o" />
            <span class="tip">客户端在 /v1/chat/completions 里填的 model 名</span>
          </div>
        </div>

        <!-- 有序上游链 -->
        <div class="chain">
          <div class="chain-head">
            <label class="section-label">有序目标链</label>
            <button type="button" class="btn btn-sm btn-ghost" :disabled="chainBlocked" @click="addTarget">+ 添加目标</button>
          </div>
          <p class="chain-explanation">{{ form.failover_enabled ? '符合转移条件时按此顺序选择下一目标；停用、熔断和尝试预算会影响实际执行。' : '当前只使用第 1 个目标。后备目标会保留，开启故障转移后才会参与。' }}</p>
          <!-- 链的加载态必须可见：否则弹窗里静静躺着一行占位主目标，看着就像
               「这条路由本来就只有一个上游」，用户会直接保存。 -->
          <p v-if="chainLoading" class="chain-loading" role="status">正在读取上游链…</p>
          <div v-for="(t, i) in form.targets" :key="i" class="chain-row" :class="{ 'chain-row-inactive': !t.enabled || (i > 0 && !form.failover_enabled) }">
            <div class="chain-row-heading">
              <strong class="chain-idx">{{ i + 1 }} · {{ i === 0 ? '首选目标' : '后备目标' }}</strong>
              <span v-if="i > 0 && !form.failover_enabled" class="badge badge-off">暂不参与</span>
              <label class="check-inline"><input v-model="t.enabled" type="checkbox" :disabled="chainBlocked" /> 目标启用</label>
            </div>
            <label class="chain-field"><span>上游服务</span><select v-model="t.provider_id" class="select" :disabled="chainBlocked" @change="onTargetProviderChange(t)">
              <option value="" disabled>选择上游</option>
              <option v-for="p in providers" :key="p.id" :value="p.id">{{ p.name }} ({{ p.slug }})</option>
            </select></label>
            <label class="chain-field"><span>上游模型</span><select v-model="t.upstream_model_id" class="select" :disabled="chainBlocked || !t.provider_id">
              <option value="" disabled>选择模型</option>
              <option v-for="m in modelsOf(t.provider_id)" :key="m.id" :value="m.id">
                {{ m.display_name ? `${m.display_name} (${m.model_id})` : m.model_id }}
              </option>
            </select></label>
            <div v-if="t.upstream_model_id" class="chain-meta">
              <span>{{ modelPrice(t.upstream_model_id) }}</span>
              <span v-if="targetIssue(t.provider_id, t.upstream_model_id)" class="target-warning">{{ targetIssue(t.provider_id, t.upstream_model_id) }}</span>
            </div>
            <div class="chain-ops">
              <!-- 置顶：一键挪到链首（未开故障转移时 = 当前生效的上游）。
                   链长时靠 ↑ 要连点好几次，而换上游是个高频动作。 -->
              <button
                type="button"
                class="btn btn-sm btn-ghost"
                :disabled="chainBlocked || i === 0"
                :title="i === 0 ? '已在链首（未开故障转移时就是当前生效的上游）' : '置顶：设为链首'"
                @click="pinTarget(i)"
              >
                置顶
              </button>
              <button type="button" class="btn btn-sm btn-ghost" :disabled="chainBlocked || i === 0" :aria-label="'上移第 ' + (i + 1) + ' 个目标'" @click="moveTarget(i, -1)">上移</button>
              <button type="button" class="btn btn-sm btn-ghost" :disabled="chainBlocked || i === form.targets.length - 1" :aria-label="'下移第 ' + (i + 1) + ' 个目标'" @click="moveTarget(i, 1)">下移</button>
              <button type="button" class="btn btn-sm btn-ghost" :disabled="chainBlocked" @click="removeTarget(i)">移除</button>
            </div>
          </div>
        </div>

        <!-- 故障转移开关（策略参数在「设置」页统一配置） -->
        <div class="failover">
          <label class="check-line">
            <input v-model="form.failover_enabled" type="checkbox" :disabled="chainBlocked" />
            启用自动故障转移
          </label>
          <p class="tip" style="margin-top: 6px">
            关闭时仅使用链首；开启后也只有符合条件的错误才会转移。流式响应开始返回后无法更换目标。
          </p>
          <div class="runtime-summary" aria-live="polite">
            <template v-if="runtime">
              <span>全局预算 {{ runtime.failover_max_targets }} 个目标</span>
              <span>连续 {{ runtime.failover_failure_threshold }} 次失败熔断</span>
              <span>非流式超时 {{ fmtSec(runtime.upstream_timeout_ms) }}s</span>
              <span>流式首字 / 空闲 {{ fmtSec(runtime.stream_first_token_timeout_ms) }}s / {{ fmtSec(runtime.stream_idle_timeout_ms) }}s</span>
            </template>
            <span v-else-if="runtimeLoading" class="dim">正在读取全局策略…</span>
            <span v-else class="dim">{{ runtimeError || '全局策略未读取' }}<button type="button" class="text-link" :disabled="runtimeLoading" @click="loadRuntime">重新读取</button></span>
          </div>
        </div>

        <!-- 「启用该路由」勾选框已去掉（2026-10-10 用户要求）。
             改为**默认创建即启用**：新建的路由本来就是拿来用的，
             先建出一个停用状态、再让管理员去列表里点一次开关，
             等于把一个必然动作拆成两步 —— 而漏掉第二步的症状是
             「路由建好了但请求全 404」，且列表上那个小小的「停用」徽章
             很容易被忽略。
             启停能力**没有消失**：列表行的开关（toggleRoute）仍然在，
             要停用随时可以停 —— 那里才是「运行中改状态」的正确位置，
             而创建表单里问「要不要启用」本质上是在问一个还没发生的事。 -->
        <p v-if="chainError" class="chain-error" role="alert">{{ chainError }}</p>
        <p v-if="saveError" class="chain-error" role="alert">{{ saveError }}</p>

        <div class="form-actions">
          <button type="button" class="btn btn-ghost" :disabled="saving" @click="closeEditor">取消</button>
          <button type="submit" class="btn btn-primary" :disabled="chainBlocked">
            {{ saving ? '保存中…' : '保存' }}
          </button>
        </div>
      </form>
    </AppModal>
  </main>
</template>

<style scoped>
.chain {
  margin: 12px 0;
}
.chain-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 8px;
}
.section-label {
  font-weight: 600;
}
.chain-row {
  display: grid;
  grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);
  align-items: center;
  gap: 12px;
  margin-top: 12px;
  padding: 16px;
  border: 1px solid var(--border);
  border-radius: 12px;
  background: var(--surface);
}
.chain-row-inactive { background: var(--surface-secondary); }
.chain-row-heading { grid-column: 1 / -1; display: flex; gap: 10px; align-items: center; flex-wrap: wrap; }
.chain-row-heading .check-inline { margin-left: auto; font-size: 12px; }
.chain-field { min-width: 0; display: grid; gap: 6px; font-size: 12px; color: var(--muted); }
.chain-explanation { color: var(--muted); font-size: 12px; line-height: 1.7; margin: 8px 0; }
.chain-idx {
  font-size: 13px;
}
.chain-ops {
  grid-column: 1 / -1;
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}
.chain-meta {
  grid-column: 1 / -1;
  display: flex;
  gap: 10px;
  align-items: center;
  flex-wrap: wrap;
  min-width: 0;
  padding: 2px 0 6px;
  color: var(--muted);
  font-size: 12px;
}
.chain-meta .target-warning { margin: 0; }
.check-inline {
  white-space: nowrap;
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
.failover {
  margin: 12px 0;
  padding: 12px;
  border: 1px dashed var(--border);
  border-radius: var(--r-thumb, 12px);
}
/* 链加载中的说明：与失败提示同位置，避免加载态被读成「链就是这么短」 */
.chain-loading {
  margin: 0 0 8px;
  font-size: 13px;
  color: var(--text-3);
}
/* 链加载失败提示：warning（暖橙）是设计系统里的专用告警色 */
.chain-error {
  padding: 10px 12px;
  border-radius: var(--r-chip, 6px);
  background: var(--warning-soft);
  color: var(--warning-soft-foreground);
  font-size: 13px;
  line-height: 1.5;
}

.routes-table { min-width: 780px; }
.mobile-route-summary { display: none; }
.route-identity { width: 27%; min-width: 200px; }
.route-target { width: 34%; min-width: 230px; overflow-wrap: anywhere; }
.name-link { text-align: left; font-weight: 650; overflow-wrap: anywhere; }
.target-warning { font-size: 12px; color: var(--warning-soft-foreground); white-space: normal; margin: 6px 0 0; }
.chain-link { display: block; margin-top: 8px; font-size: 12px; }
.context-links { display: flex; flex-wrap: wrap; gap: 18px; margin: 12px 0 18px; font-size: 12px; }
.target-timeline { list-style: none; padding: 10px 0; margin: 0; }
.target-timeline li { position: relative; display: flex; gap: 16px; padding-bottom: 24px; }
.target-timeline li:not(:last-child)::before { content: ''; position: absolute; left: 16px; top: 34px; bottom: 0; border-left: 1px solid var(--border); }
.target-position { width: 34px; height: 34px; flex-shrink: 0; border-radius: 50%; display: grid; place-items: center; background: var(--accent-soft); color: var(--accent-soft-foreground); font-weight: 650; }
.target-content { flex: 1; min-width: 0; padding: 12px 16px; border: 1px solid var(--border); border-radius: 12px; }
.target-heading { display: flex; gap: 10px; align-items: center; flex-wrap: wrap; margin-bottom: 10px; }
.target-model { overflow-wrap: anywhere; margin-top: 6px; font-size: 13px; }
.price-line { overflow-wrap: anywhere; }
.runtime-summary { display: flex; gap: 12px; flex-wrap: wrap; align-items: center; margin-top: 12px; padding-top: 10px; border-top: 1px solid var(--border); font-size: 12px; color: var(--muted); }
.route-form .form-actions { position: sticky; bottom: -22px; padding: 16px 0; background: var(--overlay); border-top: 1px solid var(--border); z-index: 1; }
.target-disabled .target-position { color: var(--muted); background: var(--default); }
@media (max-width: 760px) {
  .chain-row { grid-template-columns: minmax(0, 1fr); padding: 12px; }
  .chain-head { gap: 10px; flex-wrap: wrap; }
}

@media (max-width: 640px) {
  .routes-table { min-width: 0; table-layout: fixed; }
  .routes-table th:not(:first-child), .routes-table td:not(:first-child) { display: none; }
  .route-identity { width: 100%; min-width: 0; }
  .mobile-route-summary { display: grid; gap: 8px; margin-top: 12px; font-size: 12px; color: var(--muted); }
  .routes-table .mono { white-space: normal; overflow-wrap: anywhere; }
}
</style>
