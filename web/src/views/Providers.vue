<script setup lang="ts">
import DataTable from '../components/DataTable.vue'
// 列表仅取摘要，凭据、模型与余额请求留在上游详情页。
import { computed, onMounted, reactive, ref } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { toast, confirmBox } from '../ui'
import type { Provider } from '../types'
import AppModal from '../components/AppModal.vue'
import { useQueryText } from '../composables/useQueryText'
import { useNetworks, NETWORK_DIRECT_ID } from '../composables/useNetworks'
import { PROTOCOL_OPTIONS, protocolLabel } from '../protocols'

const router = useRouter()
const route = useRoute()
const search = useQueryText('q', '', { debounceMs: 200 })
const status = useQueryText('status')
const saving = ref(false)
const pending = ref(new Set<string>())
const filtered = computed(() => providers.value.filter(p => {
  const q = search.value.trim().toLowerCase()
  const matches = !q || [
    p.name,
    p.slug,
    p.endpoint,
    p.protocol,
    protocolLabel(p.protocol),
    isDirectProvider(p) ? '直连' : `代理 ${networkLabel.value(p.network_id)}`,
  ].some(v => v.toLowerCase().includes(q))
  const state = !p.enabled ? 'disabled' : p.ready === false ? 'unready' : p.ready === true ? 'ready' : 'unknown'
  return matches && (!status.value || status.value === state)
}))
function readyLabel(p: Provider) {
  if (!p.enabled) return '已停用'
  return p.ready === true ? '凭据就绪' : p.ready === false ? '未就绪' : '状态未知'
}
function clearFilters() {
  search.value = ''
  status.value = ''
}
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
// 新建只填 显示名称 / 协议 / Endpoint（+ 可选 api_key + 可选网络）。
// slug、启用、超时与重试由后端给默认值；余额查询可以事后在详情页改，
// 但它不在这里问 —— 新建时它对大多数上游是空的。
//
// 网络却在新建时就问：它是**默认值**（直连），不是可留空的空值。
// 「默认直连、不用管」的人看得到下拉停在第一项；需要代理的人当场就能
// 选，不必先建完上游再跳去详情页改一遍。

const { networks, load: loadNetworks } = useNetworks()

const pForm = reactive({
  open: false,
  name: '',
  protocol: 'chat',
  endpoint: '',
  api_key: '',
  // 默认直连：绝大多数上游不需要代理。存 NETWORK_DIRECT_ID 而不是空串，
  // 是为了让「下拉停在哪一项」与「提交的值」是同一个东西 —— 空串在下拉里
  // 找不到对应 option，会显示成第一项却提交另一个值。
  network_id: NETWORK_DIRECT_ID,
})

function openCreate() {
  pForm.name = ''
  pForm.protocol = 'chat'
  pForm.endpoint = ''
  pForm.api_key = ''
  pForm.network_id = NETWORK_DIRECT_ID
  pForm.open = true
  void loadNetworks()
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
      network_id: pForm.network_id,
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

// 网络清单不参与 load() 的成败判定：它只是展示辅助（行内的网络名与
// 新建表单的下拉），读不到时那两处退化即可，不该让整页变成错误态。
onMounted(() => {
  void load()
  void loadNetworks()
})

// network_id → 显示名。查不到时退回短 id —— 那说明代理刚被删掉而
// provider 还没重载，不能误显示成「直连」（它其实还没切换回直连）。
const networkLabel = computed(() => {
  const map: Record<string, string> = {}
  for (const n of networks.value) map[n.id] = n.name
  return (id: string) => map[id] ?? id.slice(0, 8)
})

function isDirectProvider(p: Provider): boolean {
  return !p.network_id || p.network_id === NETWORK_DIRECT_ID
}

function networkTitle(p: Provider): string {
  if (isDirectProvider(p)) return '直接连接，不经过代理'
  return p.proxy_url ? `代理地址：${p.proxy_url}` : `代理：${networkLabel.value(p.network_id)}`
}
</script>

<template>
  <div class="page providers-page">
    <div class="page-head">
      <div><h1>上游服务</h1></div>
      <div class="head-actions"><button class="btn" :disabled="loading" @click="load">刷新</button><button class="btn btn-primary" @click="openCreate">新建上游</button></div>
    </div>
    <div class="data-toolbar">
      <label class="search-field"><span class="sr-only">搜索上游</span><input v-model="search" class="input" type="search" placeholder="搜索名称、类型、网络或 Endpoint"/></label>
      <label><span class="sr-only">上游状态</span><select v-model="status" class="select"><option value="">全部状态</option><option value="ready">凭据就绪</option><option value="unready">未就绪</option><option value="disabled">已停用</option><option value="unknown">状态未知</option></select></label>
      <span class="result-count">{{ filtered.length }} / {{ providers.length }} 个上游</span>
    </div>
    <DataTable class="panel table-panel" label="上游列表" :loading="loading" :error="err" :has-data="!!providers.length" :empty="!filtered.length" @retry="load">
      <template #empty><template v-if="!providers.length">尚未接入上游。新建后添加凭据与模型，再配置路由。</template><template v-else>没有匹配的上游<button class="btn btn-sm" @click="clearFilters">清除筛选</button></template></template>
        <table class="tbl providers-table">
          <colgroup>
            <col class="col-provider" />
            <col class="col-type" />
            <col class="col-network" />
            <col class="col-state" />
            <col class="col-metrics" />
            <col class="col-actions" />
            <col class="col-toggle" />
          </colgroup>
          <thead><tr><th>上游 / Endpoint</th><th class="provider-type-cell">类型</th><th>网络状态</th><th>配置状态</th><th class="num-h provider-metrics">模型 / 凭据</th><th class="provider-actions">操作</th><th class="provider-toggle-head">启用</th></tr></thead>
          <tbody>
            <tr v-for="p in filtered" :key="p.id">
              <td class="provider-identity"><span class="name-link">{{ p.name || p.slug }}</span><div class="sub-line mono endpoint">{{ p.endpoint }}</div></td>
              <td class="provider-type-cell"><span class="badge">{{ protocolLabel(p.protocol) }}</span></td>
              <td class="provider-network">
                <span class="badge network-badge" :class="isDirectProvider(p) ? 'badge-dim' : 'badge-accent'" :title="networkTitle(p)">
                  <template v-if="isDirectProvider(p)">直连</template>
                  <template v-else>
                    <span class="network-kind">代理 ·</span>
                    <span class="network-name">{{ networkLabel(p.network_id) }}</span>
                  </template>
                </span>
              </td>
              <td class="provider-state"><span class="badge" :class="!p.enabled ? 'badge-off' : p.ready === false ? 'badge-st-error' : p.ready === true ? 'badge-live' : 'badge-dim'">{{ readyLabel(p) }}</span><div v-if="p.enabled && p.ready !== true" class="sub-line state-reason">{{ p.ready_reason || (p.ready === false ? '未建立可用凭据池，请检查凭据' : '暂未取得运行时状态') }}</div></td>
              <td class="num-h provider-metrics"><span>{{ p.model_count ?? '—' }}</span><span class="dim"> / </span><span>{{ p.cred_count ?? '—' }}</span></td>
              <td class="provider-actions"><div class="row-actions">
                <button class="btn btn-act btn-ghost" @click="goDetail(p)">配置</button>
                <button class="btn btn-act btn-danger" :disabled="pending.has(p.id)" @click="removeProvider(p)">删除</button>
              </div></td>
              <td class="provider-toggle"><button class="switch" :class="{ on: p.enabled }" role="switch" :aria-checked="p.enabled" :aria-label="(p.enabled ? '停用 ' : '启用 ') + p.name" :title="p.enabled ? '停用' : '启用'" :disabled="pending.has(p.id)" @click="toggleProvider(p)"></button></td>
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
            <label for="cgw-lProviders-2">类型</label>
            <select v-model="pForm.protocol" class="select" id="cgw-lProviders-2">
              <option v-for="option in PROTOCOL_OPTIONS" :key="option.value" :value="option.value">{{ option.label }}</option>
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
              余额查询可在创建后到详情页设置。
            </span>
          </div>
          <div class="field span2">
            <label for="cgw-lProviders-5">代理</label>
            <select v-model="pForm.network_id" class="select" id="cgw-lProviders-5">
              <option :value="NETWORK_DIRECT_ID">直连（默认，不经过代理）</option>
              <option v-for="n in networks" :key="n.id" :value="n.id">{{ n.name }} — {{ n.proxy_url }}</option>
            </select>
            <span class="tip">
              默认<b>直连</b>，大多数上游不用改。要走代理就先去
              <RouterLink to="/networks">代理</RouterLink> 建一条（地址填一次到处可用），
              再回来从下拉里选 —— 不用在这里手填 URL，写错不会报错只是连不上。
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
.providers-table { min-width: 1080px; table-layout: fixed; }
.col-provider { width: auto; }
.col-type { width: 80px; }
.col-network { width: 160px; }
.col-state { width: 22%; }
.col-metrics { width: 112px; }
.col-actions { width: 144px; }
.col-toggle { width: 64px; }
.providers-table td { vertical-align: middle; }
.provider-identity { min-width: 280px; }
.name-link { font-weight: 650; text-align: left; overflow-wrap: anywhere; }
.endpoint { color: var(--muted); font-size: 12px; overflow-wrap: anywhere; }
.provider-type-cell { white-space: nowrap; }
.provider-network { min-width: 0; }
.network-badge { max-width: 100%; }
.network-kind { flex: none; }
.network-name { min-width: 0; overflow: hidden; text-overflow: ellipsis; }
.provider-state { min-width: 180px; }
.state-reason { max-width: 320px; white-space: normal; color: var(--danger); font-size: 12px; }
.provider-metrics { white-space: nowrap; }
.provider-actions { text-align: left; white-space: nowrap; }
.provider-toggle-head, .provider-toggle { text-align: left; white-space: nowrap; }
</style>
