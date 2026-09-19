<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import { toast, confirmBox } from '../ui'
import type { Provider, UpstreamModel, Route } from '../types'
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

const form = reactive({
  open: false,
  editing: '',
  public_name: '',
  provider_id: '',
  upstream_model_id: '',
  priority: 0,
  fallback_route_id: '',
  enabled: true,
})

function openRoute(r?: Route) {
  form.editing = r?.id ?? ''
  form.public_name = r?.public_name ?? ''
  form.provider_id = r?.provider_id ?? ''
  form.upstream_model_id = r?.upstream_model_id ?? ''
  form.priority = r?.priority ?? 0
  form.fallback_route_id = r?.fallback_route_id ?? ''
  form.enabled = r?.enabled ?? true
  form.open = true
}

function onProviderChange() {
  // 切换 provider 后，已选模型可能不属于新 provider，清空让用户重选
  const m = mmap.value[form.upstream_model_id]
  if (m && m.provider_id !== form.provider_id) form.upstream_model_id = ''
}

async function submitRoute() {
  if (!form.public_name.trim() || !form.provider_id || !form.upstream_model_id) {
    toast('公开模型名、上游、上游模型为必填项', 'err')
    return
  }
  const body = {
    public_name: form.public_name.trim(),
    provider_id: form.provider_id,
    upstream_model_id: form.upstream_model_id,
    priority: Number(form.priority) || 0,
    fallback_route_id: form.fallback_route_id,
    enabled: form.enabled,
  }
  try {
    if (form.editing) await api.updateRoute(form.editing, body)
    else await api.createRoute(body)
    form.open = false
    toast(form.editing ? '路由已更新' : '路由已创建')
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('保存失败：' + (e as Error).message, 'err')
  }
}

async function toggleRoute(r: Route) {
  try {
    await api.updateRoute(r.id, {
      public_name: r.public_name,
      provider_id: r.provider_id,
      upstream_model_id: r.upstream_model_id,
      priority: r.priority,
      fallback_route_id: r.fallback_route_id,
      enabled: !r.enabled,
    })
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
        <div class="sub">公开模型名 → 上游模型的映射，按 priority 升序择优</div>
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
              <span v-if="r.priority" class="badge">优先级 {{ r.priority }}</span>
            </div>
            <div class="row-sub">
              {{ providerMap[r.provider_id]?.name ?? r.provider_id }} →
              <span class="mono">{{ modelLabel(r) }}</span>
              <template v-if="r.fallback_route_id">
                · 兜底 {{ providerMap[routes.find((x) => x.id === r.fallback_route_id)?.provider_id ?? '']?.name ?? '' }}
                <span class="mono">{{ routes.find((x) => x.id === r.fallback_route_id)?.public_name ?? r.fallback_route_id }}</span>
              </template>
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
          <div class="field">
            <label>上游 *</label>
            <select v-model="form.provider_id" class="select" @change="onProviderChange">
              <option value="" disabled>选择上游</option>
              <option v-for="p in providers" :key="p.id" :value="p.id">{{ p.name }} ({{ p.slug }})</option>
            </select>
          </div>
          <div class="field">
            <label>上游模型 *</label>
            <select v-model="form.upstream_model_id" class="select" :disabled="!form.provider_id">
              <option value="" disabled>选择模型</option>
              <option v-for="m in modelsOf(form.provider_id)" :key="m.id" :value="m.id">
                {{ m.display_name ? `${m.display_name} (${m.model_id})` : m.model_id }}
              </option>
            </select>
            <span class="tip">仅列出所选上游下的模型</span>
          </div>
          <div class="field">
            <label>优先级</label>
            <input v-model.number="form.priority" class="input num" type="number" min="0" step="1" />
            <span class="tip">数字越小越优先，0 为默认</span>
          </div>
          <div class="field">
            <label>兜底路由</label>
            <select v-model="form.fallback_route_id" class="select">
              <option value="">无</option>
              <option v-for="r in routes.filter((x) => x.id !== form.editing)" :key="r.id" :value="r.id">
                {{ r.public_name }}
              </option>
            </select>
          </div>
          <div class="field span2">
            <label class="check-line">
              <input v-model="form.enabled" type="checkbox" />
              启用该路由
            </label>
          </div>
        </div>
        <div class="form-actions">
          <button type="button" class="btn btn-ghost" @click="form.open = false">取消</button>
          <button type="submit" class="btn btn-primary">保存</button>
        </div>
      </form>
    </AppModal>
  </main>
</template>
