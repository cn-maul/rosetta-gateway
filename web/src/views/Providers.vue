<script setup lang="ts">
import DataTable from '../components/DataTable.vue'
// 列表仅取摘要，凭据、模型与余额请求留在上游详情页。
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { toast, confirmBox } from '../ui'
import type { Provider } from '../types'
import AppModal from '../components/AppModal.vue'
import { useQueryText } from '../composables/useQueryText'

const router = useRouter()
const route = useRoute()
const search = useQueryText('q')
const status = useQueryText('status')
const saving = ref(false)
const pending = ref(new Set<string>())
const filtered = computed(() => providers.value.filter(p => {
  const q = search.value.trim().toLowerCase()
  const matches = !q || [p.name, p.slug, p.endpoint, p.protocol].some(v => v.toLowerCase().includes(q))
  const state = !p.enabled ? 'disabled' : p.ready === false ? 'unready' : p.ready === true ? 'ready' : 'unknown'
  return matches && (!status.value || status.value === state)
}))
function readyLabel(p: Provider) {
  if (!p.enabled) return '已停用'
  return p.ready === true ? '凭据就绪' : p.ready === false ? '未就绪' : '状态未知'
}
function clearFilters() { void router.replace({ query: { ...route.query, q: undefined, status: undefined } }) }
let loadSeq = 0

const err = ref('')
const loading = ref(true)
const providers = ref<Provider[]>([])

async function load() {
  const seq = ++loadSeq
  loading.value = true
  err.value = ''
  try {
    const result = await api.providers()
    if (seq === loadSeq) providers.value = result
  } catch (e) {
    if (seq !== loadSeq) return
    if ((e as { status?: number }).status === 401) return
    // 失败必须留下可见错误态：呈现成「暂无数据」会让运维误判为无流量，
    // 从而排除掉网关/上游故障这个真正的方向。
    err.value = '加载上游失败：' + (e as Error).message
    toast('加载失败：' + (e as Error).message, 'err')
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}

// ---------- 新建表单 ----------
// 新建只填 显示名称 / 协议 / Endpoint（+ 可选 api_key）。
// slug、启用、超时与重试由后端给默认值；代理与余额查询可以事后在详情页改，
// 但它们不在这里问 —— 新建时这两个对大多数上游是空的。

const pForm = reactive({
  open: false,
  name: '',
  protocol: 'openai-chat',
  endpoint: '',
  api_key: '',
})

function openCreate() {
  pForm.name = ''
  pForm.protocol = 'openai-chat'
  pForm.endpoint = ''
  pForm.api_key = ''
  pForm.open = true
}

async function submitProvider() {
  if (saving.value) return
  if (!pForm.endpoint.trim()) {
    toast('Endpoint 为必填项', 'err')
    return
  }
  saving.value = true
  try {
    await api.createProvider({
      name: pForm.name.trim(),
      protocol: pForm.protocol,
      endpoint: pForm.endpoint.trim(),
      api_key: pForm.api_key.trim() || undefined,
    })
    toast('上游已创建')
    pForm.open = false
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('保存失败：' + (e as Error).message, 'err')
  } finally {
    saving.value = false
  }
}

function goDetail(p: Provider) {
  router.push({ name: 'provider-detail', params: { id: p.id }, query: route.query })
}

async function toggleProvider(p: Provider) {
  if (pending.value.has(p.id)) return
  pending.value.add(p.id)
  try {
    // 只发改动字段：其余字段后端保持原值（PATCH 是字段级部分更新）。
    await api.updateProvider(p.id, { enabled: !p.enabled })
    toast(!p.enabled ? '已启用' : '已停用')
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('操作失败：' + (e as Error).message, 'err')
  } finally {
    pending.value.delete(p.id)
  }
}

async function removeProvider(p: Provider) {
  if (pending.value.has(p.id)) return
  const ok = await confirmBox({
    title: `删除上游「${p.name}」？`,
    body: '将同时影响其下模型与路由的可用性；关联数据不会自动清理。',
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok || pending.value.has(p.id)) return
  pending.value.add(p.id)
  try {
    await api.deleteProvider(p.id)
    toast('已删除')
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('删除失败：' + (e as Error).message, 'err')
  } finally {
    pending.value.delete(p.id)
  }
}

onMounted(load)
</script>

<template>
  <div class="page providers-page">
    <div class="page-head">
      <div><h1>上游服务</h1><div class="sub">接入服务、管理凭据与上游模型 · 全站配置</div></div>
      <div class="head-actions"><button class="btn" :disabled="loading" @click="load">刷新</button><button class="btn btn-primary" @click="openCreate">新建上游</button></div>
    </div>
    <div class="data-toolbar">
      <label class="search-field"><span class="sr-only">搜索上游</span><input v-model="search" class="input" type="search" placeholder="搜索名称、协议或 Endpoint"/></label>
      <label><span class="sr-only">上游状态</span><select v-model="status" class="select"><option value="">全部状态</option><option value="ready">凭据就绪</option><option value="unready">未就绪</option><option value="disabled">已停用</option><option value="unknown">状态未知</option></select></label>
      <span class="result-count">{{ filtered.length }} / {{ providers.length }} 个上游</span>
    </div>
    <p class="context-note">“凭据就绪”表示已构建可用凭据池；实时推理是否成功需结合模型探测与请求日志判断。</p>
    <DataTable class="panel table-panel" label="上游列表" :loading="loading" :error="err" :has-data="!!providers.length" :empty="!filtered.length" @retry="load">
      <template #empty><template v-if="!providers.length">尚未接入上游。新建后添加凭据与模型，再配置路由。</template><template v-else>没有匹配的上游<button class="btn btn-sm" @click="clearFilters">清除筛选</button></template></template>
        <table class="tbl providers-table">
          <thead><tr><th>上游 / Endpoint</th><th>协议</th><th>配置状态</th><th class="num-h">模型 / 凭据</th><th class="c-act">操作</th></tr></thead>
          <tbody>
            <tr v-for="p in filtered" :key="p.id">
              <td class="provider-identity"><button class="text-link name-link" @click="goDetail(p)">{{ p.name || p.slug }}</button><div class="sub-line mono endpoint">{{ p.endpoint }}</div><span v-if="p.proxy_url" class="sub-line">经代理转发</span></td>
              <td><span class="badge">{{ p.protocol }}</span></td>
              <td class="provider-state"><span class="badge" :class="!p.enabled ? 'badge-off' : p.ready === false ? 'badge-st-error' : p.ready === true ? 'badge-live' : 'badge-dim'">{{ readyLabel(p) }}</span><div v-if="p.enabled && p.ready !== true" class="sub-line state-reason">{{ p.ready_reason || (p.ready === false ? '未建立可用凭据池，请检查凭据' : '暂未取得运行时状态') }}</div></td>
              <td class="num"><span>{{ p.model_count ?? '—' }}</span><span class="dim"> / </span><span>{{ p.cred_count ?? '—' }}</span></td>
              <td class="c-act"><div class="row-actions">
                <button class="btn btn-act" @click="goDetail(p)">配置</button>
                <button class="switch" :class="{ on: p.enabled }" role="switch" :aria-checked="p.enabled" :aria-label="(p.enabled ? '停用 ' : '启用 ') + p.name" :disabled="pending.has(p.id)" @click="toggleProvider(p)"></button>
                <details class="row-menu"><summary :aria-label="'更多操作：' + p.name">⋯</summary><div class="row-menu-content"><button class="btn btn-sm btn-danger" :disabled="pending.has(p.id)" @click="removeProvider(p)">删除上游</button></div></details>
              </div></td>
            </tr>
          </tbody>
        </table>
    </DataTable>

    <AppModal :open="pForm.open" title="新建上游" :dismissable="false">
      <form @submit.prevent="submitProvider">
        <div class="form-grid">
          <div class="field">
            <label for="cgw-lProviders-1">显示名称</label>
            <input v-model="pForm.name" class="input" placeholder="DeepSeek（留空则用 Endpoint）" id="cgw-lProviders-1"/>
          </div>
          <div class="field">
            <label for="cgw-lProviders-2">协议</label>
            <select v-model="pForm.protocol" class="select" id="cgw-lProviders-2">
              <option value="openai-chat">openai-chat</option>
              <option value="anthropic">anthropic</option>
              <option value="openai-responses">openai-responses</option>
            </select>
          </div>
          <div class="field span2">
            <label for="cgw-lProviders-3">Endpoint *</label>
            <input v-model="pForm.endpoint" class="input mono" placeholder="https://api.deepseek.com" id="cgw-lProviders-3"/>
          </div>
          <div class="field span2">
            <label for="cgw-lProviders-4">API Key</label>
            <input v-model="pForm.api_key" class="input mono" type="password" placeholder="sk-...（可留空，稍后在详情页添加）" autocomplete="new-password" id="cgw-lProviders-4"/>
            <span class="tip">
              添加即启用；slug、超时与重试将自动使用默认值。
              代理与余额查询可在创建后到详情页设置。
            </span>
          </div>
        </div>
        <div class="form-actions">
          <button type="button" class="btn btn-ghost" :disabled="saving" @click="pForm.open = false">取消</button>
          <button type="submit" class="btn btn-primary" :disabled="saving">{{ saving ? '保存中…' : '创建上游' }}</button>
        </div>
      </form>
    </AppModal>
  </div>
</template>

<style scoped>
.providers-table { min-width: 760px; }
.provider-identity { width: 34%; min-width: 240px; }
.name-link { font-weight: 650; text-align: left; overflow-wrap: anywhere; }
.endpoint { color: var(--muted); font-size: 12px; overflow-wrap: anywhere; }
.provider-state { width: 25%; min-width: 180px; }
.state-reason { max-width: 320px; white-space: normal; color: var(--danger); font-size: 12px; }
@media (max-width: 640px) {
  .providers-table { min-width: 0; table-layout: fixed; }
  .providers-table th:nth-child(2), .providers-table td:nth-child(2),
  .providers-table th:nth-child(4), .providers-table td:nth-child(4),
  .providers-table th:nth-child(5), .providers-table td:nth-child(5) { display: none; }
  .provider-identity { min-width: 0; width: 56%; }
  .provider-state { min-width: 0; width: 44%; }
  .provider-state .badge { padding: 0 6px; }
  .name-link { white-space: normal; }
}
</style>
