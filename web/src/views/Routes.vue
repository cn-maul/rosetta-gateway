<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import { toast, confirmBox } from '../ui'
import type { Provider, UpstreamModel, Route, RouteTargetInput } from '../types'
import AppModal from '../components/AppModal.vue'

const loading = ref(true)
const routes = ref<Route[]>([])
const providers = ref<Provider[]>([])
// upstream_models.id（短哈希外键）→ 模型对象；Resolve 时拿的是这个外键
const mmap = ref<Record<string, UpstreamModel>>({})

const providerMap = computed(() => {
  const m: Record<string, Provider> = {}
  for (const p of providers.value) m[p.id] = p
  return m
})

// 某个 provider 下可被路由选择的模型
function modelsOf(providerId: string): UpstreamModel[] {
  return Object.values(mmap.value).filter((m) => m.provider_id === providerId)
}

function modelLabel(routeRow: Route): string {
  const m = mmap.value[routeRow.upstream_model_id]
  if (m) return m.display_name ? `${m.display_name} (${m.model_id})` : m.model_id
  // 兜底：外键悬空时至少显示路由自身信息
  return routeRow.id
}

async function load() {
  loading.value = true
  try {
    const [r, p] = await Promise.all([api.routes(), api.providers()])
    routes.value = r
    providers.value = p
    // 模型端点按 provider 分片：并行拉取所有 provider 的模型建索引
    const all = await Promise.all(p.map((x) => api.models(x.id).catch(() => [])))
    const map: Record<string, UpstreamModel> = {}
    for (const list of all) for (const m of list) map[m.id] = m
    mmap.value = map
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('加载失败：' + (e as Error).message, 'err')
  } finally {
    loading.value = false
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

function blankTarget(): TargetRow {
  return { provider_id: '', upstream_model_id: '', enabled: true }
}

async function openRoute(r?: Route) {
  form.editing = r?.id ?? ''
  form.public_name = r?.public_name ?? ''
  form.enabled = r?.enabled ?? true
  form.failover_enabled = r?.failover_enabled ?? false
  chainError.value = ''

  if (r) {
    try {
      const ts = await api.routeTargets(r.id)
      form.targets = ts.length
        ? ts.map((t) => ({ provider_id: t.provider_id, upstream_model_id: t.upstream_model_id, enabled: t.enabled }))
        : [{ provider_id: r.provider_id, upstream_model_id: r.upstream_model_id, enabled: true }]
    } catch (e) {
      // 载荷仍是「主目标单行」，但只用于展示；chainError 会拦住保存。
      chainError.value =
        '无法读取该路由的上游链：' +
        (e as Error).message +
        '。为避免用不完整的数据覆盖已有配置，保存已被禁用 —— 请先刷新页面重试。'
      form.targets = [{ provider_id: r.provider_id, upstream_model_id: r.upstream_model_id, enabled: true }]
    }
  } else {
    form.targets = [blankTarget()]
  }
  form.open = true
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

function onTargetProviderChange(row: TargetRow) {
  // 切 provider 后原模型可能不属于它，清空让用户重选
  const m = mmap.value[row.upstream_model_id]
  if (m && m.provider_id !== row.provider_id) row.upstream_model_id = ''
}

async function submitRoute() {
  const targets = form.targets
  if (chainError.value) {
    toast('上游链未成功加载，已禁止保存（请先刷新页面）', 'err')
    return
  }
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
    // 目标链独立于 route 基础字段整体替换（按当前顺序重排 position）。
    await api.saveRouteTargets(routeId, chain)

    form.open = false
    toast(isNew ? '路由已创建' : '路由已更新')
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) {
      // isNew=true 时 route 已落库、只是链没写全；其余情况才是真正的「保存失败」。
      const hint = isNew ? '路由已创建，但上游链写入失败：' : '保存失败：'
      toast(hint + (e as Error).message, 'err')
    }
  }
}

async function toggleRoute(r: Route) {
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
  }
}

async function removeRoute(r: Route) {
  const ok = await confirmBox({
    title: `删除路由「${r.public_name}」？`,
    body: '删除后该公开模型名立即不可用（客户端会收到 404 model_not_found）。',
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return
  try {
    await api.deleteRoute(r.id)
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
        <h1>路由</h1>
        <div class="sub">公开模型名 → 有序上游链，按 position 依次尝试，命中故障自动转移</div>
      </div>
      <div class="head-actions">
        <button class="btn" :disabled="loading" @click="load">刷新</button>
        <button class="btn btn-primary" @click="openRoute()">新建路由</button>
      </div>
    </div>

    <div class="panel">
      <div v-if="loading && routes.length === 0" class="loading">加载中…</div>
      <div v-else-if="routes.length === 0" class="empty">
        <div class="big">⇢</div>
        还没有路由，客户端将无法调用任何模型
      </div>
      <div v-else class="row-list">
        <div v-for="r in routes" :key="r.id" class="row">
          <div class="row-main">
            <div class="row-title">
              <span class="mono">{{ r.public_name }}</span>
              <span class="badge" :class="r.enabled ? 'badge-live' : 'badge-off'">{{ r.enabled ? '启用' : '停用' }}</span>
              <span v-if="r.failover_enabled" class="badge">故障转移</span>
            </div>
            <div class="row-sub">
              {{ providerMap[r.provider_id]?.name ?? r.provider_id }} →
              <span class="mono">{{ modelLabel(r) }}</span>
              <span v-if="r.failover_enabled" class="muted"> · 链式多上游，失败自动切换</span>
            </div>
          </div>
          <div class="row-side">
            <button class="btn btn-sm btn-ghost" @click="openRoute(r)">编辑</button>
            <button class="btn btn-sm btn-danger" @click="removeRoute(r)">删除</button>
            <button class="switch" :class="{ on: r.enabled }" :title="r.enabled ? '停用' : '启用'" @click="toggleRoute(r)"></button>
          </div>
        </div>
      </div>
    </div>

    <AppModal :open="form.open" :title="form.editing ? '编辑路由' : '新建路由'" @close="form.open = false">
      <form @submit.prevent="submitRoute">
        <div class="form-grid">
          <div class="field span2">
            <label>公开模型名 *</label>
            <input v-model="form.public_name" class="input mono" placeholder="gpt-4o" />
            <span class="tip">客户端在 /v1/chat/completions 里填的 model 名</span>
          </div>
        </div>

        <!-- 有序上游链 -->
        <div class="chain">
          <div class="chain-head">
            <label class="section-label">上游链（自上而下依次尝试）</label>
            <button type="button" class="btn btn-sm btn-ghost" :disabled="!!chainError" @click="addTarget">+ 添加目标</button>
          </div>
          <div v-for="(t, i) in form.targets" :key="i" class="chain-row">
            <span class="chain-idx">{{ i + 1 }}</span>
            <select v-model="t.provider_id" class="select" @change="onTargetProviderChange(t)">
              <option value="" disabled>选择上游</option>
              <option v-for="p in providers" :key="p.id" :value="p.id">{{ p.name }} ({{ p.slug }})</option>
            </select>
            <select v-model="t.upstream_model_id" class="select" :disabled="!t.provider_id">
              <option value="" disabled>选择模型</option>
              <option v-for="m in modelsOf(t.provider_id)" :key="m.id" :value="m.id">
                {{ m.display_name ? `${m.display_name} (${m.model_id})` : m.model_id }}
              </option>
            </select>
            <label class="check-inline" title="是否在链中启用">
              <input v-model="t.enabled" type="checkbox" /> 启用
            </label>
            <div class="chain-ops">
              <button type="button" class="btn btn-sm btn-ghost" :disabled="!!chainError || i === 0" @click="moveTarget(i, -1)">↑</button>
              <button type="button" class="btn btn-sm btn-ghost" :disabled="!!chainError || i === form.targets.length - 1" @click="moveTarget(i, 1)">↓</button>
              <button type="button" class="btn btn-sm btn-danger" :disabled="!!chainError" @click="removeTarget(i)">删</button>
            </div>
          </div>
        </div>

        <!-- 故障转移开关（策略参数在「设置」页统一配置） -->
        <div class="failover">
          <label class="check-line">
            <input v-model="form.failover_enabled" type="checkbox" />
            启用自动故障转移（关闭则只打链首一个目标）
          </label>
          <p v-if="form.failover_enabled" class="tip" style="margin-top: 6px">
            尝试目标数、失败熔断阈值与各类超时是<strong>全局设置</strong>，请到
            <RouterLink to="/settings">设置</RouterLink> 页调整。
          </p>
        </div>

        <div class="form-grid">
          <div class="field span2">
            <label class="check-line">
              <input v-model="form.enabled" type="checkbox" />
              启用该路由
            </label>
          </div>
        </div>

        <p v-if="chainError" class="chain-error" role="alert">{{ chainError }}</p>

        <div class="form-actions">
          <button type="button" class="btn btn-ghost" @click="form.open = false">取消</button>
          <button type="submit" class="btn btn-primary" :disabled="!!chainError">保存</button>
        </div>
      </form>
    </AppModal>
  </main>
</template>

<style scoped>
.chain {
  margin: 12px 0;
  padding: 12px;
  border: 1px solid var(--border);
  border-radius: var(--r-thumb, 12px);
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
  grid-template-columns: auto 1fr 1fr auto auto;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}
.chain-idx {
  width: 20px;
  text-align: center;
  opacity: 0.6;
}
.chain-ops {
  display: flex;
  gap: 4px;
}
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
.muted {
  opacity: 0.6;
}
/* 链加载失败提示：warning（暖橙）是设计系统里的专用告警色 */
.chain-error {
  margin: 12px 0 0;
  padding: 10px 12px;
  border-radius: var(--r-chip, 6px);
  background: var(--warning-soft);
  color: var(--warning-soft-foreground);
  font-size: 13px;
  line-height: 1.5;
}
</style>
