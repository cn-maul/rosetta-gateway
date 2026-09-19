<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import { toast, confirmBox } from '../ui'
import { fmtDate, fmtSpeed, fmtSec } from '../fmt'
import type { Provider, Credential, UpstreamModel, DiscoveredModel } from '../types'
import AppModal from '../components/AppModal.vue'

const loading = ref(true)
const providers = ref<Provider[]>([])
const expandedId = ref('')
const credsMap = reactive<Record<string, Credential[]>>({})
const modelsMap = reactive<Record<string, UpstreamModel[]>>({})
const testing = ref('')

async function load() {
  loading.value = true
  try {
    providers.value = await api.providers()
    // 已展开的行刷新子资源
    if (expandedId.value) await openExpand(expandedId.value, true)
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('加载失败：' + (e as Error).message, 'err')
  } finally {
    loading.value = false
  }
}

async function openExpand(id: string, silent = false) {
  if (expandedId.value === id && !silent) {
    expandedId.value = ''
    return
  }
  expandedId.value = id
  try {
    const [c, m] = await Promise.all([api.credentials(id), api.models(id)])
    credsMap[id] = c
    modelsMap[id] = m
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('加载子资源失败：' + (e as Error).message, 'err')
  }
}

// ---------- Provider 表单 ----------
// 新建只填 显示名称 / 协议 / Endpoint（可选 api_key）；
// 编辑时可展开高级选项，单独调整 slug（只读）、超时、重试。

const pForm = reactive({
  open: false,
  editing: '' as string,
  name: '',
  protocol: 'openai-chat',
  endpoint: '',
  api_key: '',
  slug: '',
  timeout_ms: 0,
  max_retries: 0,
  showAdvanced: false,
})

function openProvider(p?: Provider) {
  pForm.editing = p?.id ?? ''
  pForm.name = p?.name ?? ''
  pForm.protocol = p?.protocol || 'openai-chat'
  pForm.endpoint = p?.endpoint ?? ''
  pForm.api_key = ''
  pForm.slug = p?.slug ?? ''
  pForm.timeout_ms = p?.timeout_ms ?? 0
  pForm.max_retries = p?.max_retries ?? 0
  pForm.showAdvanced = false
  pForm.open = true
}

async function submitProvider() {
  if (!pForm.endpoint.trim()) {
    toast('Endpoint 为必填项', 'err')
    return
  }
  try {
    if (pForm.editing) {
      // PATCH 为非空覆盖语义：编辑时回传完整字段
      await api.updateProvider(pForm.editing, {
        name: pForm.name.trim() || pForm.endpoint.trim(),
        protocol: pForm.protocol,
        endpoint: pForm.endpoint.trim(),
        slug: pForm.slug.trim(),
        timeout_ms: Number(pForm.timeout_ms) || 0,
        max_retries: Number(pForm.max_retries) || 0,
      })
      toast('上游已更新')
    } else {
      await api.createProvider({
        name: pForm.name.trim(),
        protocol: pForm.protocol,
        endpoint: pForm.endpoint.trim(),
        api_key: pForm.api_key.trim() || undefined,
      })
      toast('上游已创建')
    }
    pForm.open = false
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('保存失败：' + (e as Error).message, 'err')
  }
}

async function toggleProvider(p: Provider) {
  try {
    // PATCH 为部分更新语义，发送完整字段
    await api.updateProvider(p.id, {
      slug: p.slug, name: p.name, protocol: p.protocol, endpoint: p.endpoint,
      timeout_ms: p.timeout_ms, max_retries: p.max_retries, enabled: !p.enabled,
    })
    toast(!p.enabled ? '已启用' : '已停用')
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('操作失败：' + (e as Error).message, 'err')
  }
}

async function removeProvider(p: Provider) {
  const ok = await confirmBox({
    title: `删除上游「${p.name}」？`,
    body: '将同时影响其下模型与路由的可用性；关联数据不会自动清理。',
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return
  try {
    await api.deleteProvider(p.id)
    toast('已删除')
    if (expandedId.value === p.id) expandedId.value = ''
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('删除失败：' + (e as Error).message, 'err')
  }
}

async function testProvider(p: Provider) {
  testing.value = p.id
  try {
    const r = await api.testProvider(p.id)
    toast(r.message || (r.status === 'ok' ? '连接正常' : '测试失败'), r.status === 'ok' ? 'ok' : 'err')
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('测试失败：' + (e as Error).message, 'err')
  } finally {
    testing.value = ''
  }
}

// ---------- 凭据表单 ----------

const cForm = reactive({ open: false, provider: '' as string, editing: '' as string, label: '', api_key: '', weight: 1 })

function openCred(providerId: string, c?: Credential) {
  cForm.provider = providerId
  cForm.editing = c?.id ?? ''
  cForm.label = c?.label ?? ''
  cForm.api_key = ''
  cForm.weight = c?.weight || 1
  cForm.open = true
}

async function submitCred() {
  if (!cForm.editing && !cForm.api_key) {
    toast('api_key 为必填项', 'err')
    return
  }
  try {
    if (cForm.editing) {
      await api.updateCredential(cForm.editing, { label: cForm.label.trim(), weight: Number(cForm.weight) || 1, ...(cForm.api_key ? { api_key: cForm.api_key } : {}) })
    } else {
      await api.createCredential(cForm.provider, { label: cForm.label.trim(), api_key: cForm.api_key, weight: Number(cForm.weight) || 1 })
    }
    cForm.open = false
    toast(cForm.editing ? '凭据已更新' : '凭据已创建')
    await openExpand(cForm.provider, true)
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('保存失败：' + (e as Error).message, 'err')
  }
}

async function removeCred(c: Credential) {
  const ok = await confirmBox({ title: `删除凭据「${c.label || c.id}」？`, danger: true, confirmLabel: '删除' })
  if (!ok) return
  try {
    await api.deleteCredential(c.id)
    toast('已删除')
    await openExpand(c.provider_id, true)
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('删除失败：' + (e as Error).message, 'err')
  }
}

// ---------- 模型：编辑表单 ----------

const mForm = reactive({ open: false, provider: '' as string, editing: '' as string, model_id: '', context_window: 0, max_output_tokens: 0, enabled: true })

function openModel(providerId: string, m?: UpstreamModel) {
  mForm.provider = providerId
  mForm.editing = m?.id ?? ''
  mForm.model_id = m?.model_id ?? ''
  mForm.context_window = m?.context_window ?? 0
  mForm.max_output_tokens = m?.max_output_tokens ?? 0
  mForm.enabled = m?.enabled ?? true
  // 重置「添加」流程状态
  discovered.value = []
  selected.value = []
  discoverMsg.value = ''
  filter.value = ''
  mForm.open = true
}

async function submitModel() {
  if (!mForm.editing) return
  if (!mForm.model_id.trim()) {
    toast('上游模型名（model_id）为必填项', 'err')
    return
  }
  const body = {
    model_id: mForm.model_id.trim(),
    context_window: Number(mForm.context_window) || 0,
    max_output_tokens: Number(mForm.max_output_tokens) || 0,
    enabled: mForm.enabled,
  }
  try {
    await api.updateModel(mForm.editing, body)
    mForm.open = false
    toast('模型已更新')
    await openExpand(mForm.provider, true)
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('保存失败：' + (e as Error).message, 'err')
  }
}

// ---------- 模型：探测 + 多选胶囊式添加 ----------

const discovering = ref(false)
const discovered = ref<DiscoveredModel[]>([])
const discoverMsg = ref('')
const importing = ref(false)
const filter = ref('')
// 已选中的模型（胶囊池），一次获取后可点击多条加入
const selected = ref<DiscoveredModel[]>([])

const filteredDiscovered = computed(() => {
  const q = filter.value.trim().toLowerCase()
  if (!q) return discovered.value
  return discovered.value.filter(
    (d) => d.model_id.toLowerCase().includes(q) || (d.display_name ?? '').toLowerCase().includes(q),
  )
})

function isSelected(id: string) {
  return selected.value.some((s) => s.model_id === id)
}

function toggleSelect(d: DiscoveredModel) {
  const i = selected.value.findIndex((s) => s.model_id === d.model_id)
  if (i >= 0) selected.value.splice(i, 1)
  else selected.value.push(d)
}

function removeSelected(id: string) {
  const i = selected.value.findIndex((s) => s.model_id === id)
  if (i >= 0) selected.value.splice(i, 1)
}

async function runDiscover() {
  discovering.value = true
  discovered.value = []
  selected.value = []
  discoverMsg.value = ''
  try {
    const r = await api.discoverModels(mForm.provider)
    if (r.status === 'ok') {
      discovered.value = r.models ?? []
      if (discovered.value.length === 0) discoverMsg.value = '上游未返回任何模型'
    } else {
      discoverMsg.value = r.message || '获取失败'
    }
  } catch (e) {
    discoverMsg.value = (e as Error).message
  } finally {
    discovering.value = false
  }
}

async function confirmImport() {
  const list = selected.value
  if (list.length === 0) return
  importing.value = true
  try {
    const r = await api.importModels(
      mForm.provider,
      list.map((d) => ({
        model_id: d.model_id,
        display_name: d.display_name ?? '',
        context_window: d.context_window ?? 0,
        max_output_tokens: d.max_output_tokens ?? 0,
      })),
    )
    toast(`已添加 ${r.imported} 个模型`)
    mForm.open = false
    await openExpand(mForm.provider, true)
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('添加失败：' + (e as Error).message, 'err')
  } finally {
    importing.value = false
  }
}

async function removeModel(m: UpstreamModel) {
  const ok = await confirmBox({
    title: `删除模型「${m.model_id}」？`,
    body: '引用它的路由将无法解析，请先清理相关路由。',
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return
  try {
    await api.deleteModel(m.id)
    toast('已删除')
    await openExpand(m.provider_id, true)
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
        <h1>上游与模型</h1>
        <div class="sub">上游服务、凭据与上游模型管理</div>
      </div>
      <div class="head-actions">
        <button class="btn" :disabled="loading" @click="load">刷新</button>
        <button class="btn btn-primary" @click="openProvider()">新建上游</button>
      </div>
    </div>

    <div class="panel">
      <div v-if="loading && providers.length === 0" class="loading">加载中…</div>
      <div v-else-if="providers.length === 0" class="empty">
        <div class="big">⌘</div>
        还没有上游服务，点击右上角「新建上游」开始
      </div>
      <div v-else class="row-list">
        <template v-for="p in providers" :key="p.id">
          <div class="row">
            <div class="row-main">
              <div class="row-title">
                {{ p.name }}
                <span class="badge" :class="p.enabled ? 'badge-live' : 'badge-off'">{{ p.enabled ? '启用' : '停用' }}</span>
                <span class="badge">{{ p.protocol }}</span>
              </div>
              <div class="sub-row mono">{{ p.slug }} · {{ p.endpoint }}</div>
            </div>
            <div class="row-side">
              <button class="btn btn-sm btn-ghost" :disabled="testing === p.id" @click="testProvider(p)">
                {{ testing === p.id ? '测试中…' : '测试' }}
              </button>
              <button class="btn btn-sm btn-ghost" @click="openExpand(p.id)">
                {{ expandedId === p.id ? '收起' : '展开' }}
              </button>
              <button class="btn btn-sm btn-ghost" @click="openProvider(p)">编辑</button>
              <button class="btn btn-sm btn-danger" @click="removeProvider(p)">删除</button>
              <button class="switch" :class="{ on: p.enabled }" :title="p.enabled ? '停用' : '启用'" @click="toggleProvider(p)"></button>
            </div>
          </div>

          <div v-if="expandedId === p.id" class="expand">
            <div class="expand-grid">
              <div class="expand-col">
                <h4>
                  凭据（{{ (credsMap[p.id] ?? []).length }}）
                  <button class="btn btn-sm" @click="openCred(p.id)">添加凭据</button>
                </h4>
                <div v-if="(credsMap[p.id] ?? []).length === 0" class="empty" style="padding: 14px 0">
                  无凭据 —— 上游鉴权必需
                </div>
                <div v-for="c in credsMap[p.id] ?? []" :key="c.id" class="mini-row">
                  <span class="badge" :class="c.enabled ? 'badge-live' : 'badge-off'">{{ c.enabled ? '启用' : '停用' }}</span>
                  <span class="mini-main">{{ c.label || c.id }}</span>
                  <span class="mono" style="color: var(--text-3)">w{{ c.weight }}</span>
                  <span class="badge">{{ c.status }}</span>
                  <span class="mono" style="color: var(--text-4)">{{ fmtDate(c.created_at) }}</span>
                  <button class="btn btn-sm btn-ghost" @click="openCred(p.id, c)">换钥</button>
                  <button class="btn btn-sm btn-danger" @click="removeCred(c)">删</button>
                </div>
              </div>

              <div class="expand-col">
                <h4>
                  上游模型（{{ (modelsMap[p.id] ?? []).length }}）
                  <button class="btn btn-sm" @click="openModel(p.id)">添加模型</button>
                </h4>
                <div v-if="(modelsMap[p.id] ?? []).length === 0" class="empty" style="padding: 14px 0">
                  无模型 —— 路由必须指向一个上游模型
                </div>
                <div v-for="m in modelsMap[p.id] ?? []" :key="m.id" class="mini-row">
                  <span class="badge" :class="m.enabled ? 'badge-live' : 'badge-off'">{{ m.enabled ? '启用' : '停用' }}</span>
                  <span class="mini-main mono">{{ m.model_id }}</span>
                  <span class="col-spd">{{ m.tokens_per_sec ? fmtSpeed(m.tokens_per_sec) + ' tok/s' : '' }}</span>
                  <span class="col-ttfb">{{ m.ttfb_ms ? fmtSec(m.ttfb_ms) + ' s' : '' }}</span>
                  <span class="col-sr"><span v-if="m.call_count" class="sr" :class="{ warn: (m.success_rate ?? 1) < 1 }">{{ Math.round((m.success_rate ?? 0) * 100) }}%</span></span>
                  <span class="mono" style="color: var(--text-4)">{{ fmtDate(m.created_at) }}</span>
                  <button class="btn btn-sm btn-ghost" @click="openModel(p.id, m)">编辑</button>
                  <button class="btn btn-sm btn-danger" @click="removeModel(m)">删</button>
                </div>
              </div>
            </div>
          </div>
        </template>
      </div>
    </div>

    <!-- Provider 表单 -->
    <AppModal
      :open="pForm.open"
      :title="pForm.editing ? '编辑上游' : '新建上游'"
      @close="pForm.open = false"
    >
      <form @submit.prevent="submitProvider">
        <div class="form-grid">
          <div class="field">
            <label>显示名称</label>
            <input v-model="pForm.name" class="input" placeholder="DeepSeek（留空则用 Endpoint）" />
          </div>
          <div class="field">
            <label>协议</label>
            <select v-model="pForm.protocol" class="select">
              <option value="openai-chat">openai-chat</option>
              <option value="anthropic">anthropic</option>
              <option value="openai-responses">openai-responses</option>
            </select>
          </div>
          <div class="field span2">
            <label>Endpoint *</label>
            <input v-model="pForm.endpoint" class="input mono" placeholder="https://api.deepseek.com" />
          </div>
          <div v-if="!pForm.editing" class="field span2">
            <label>API Key</label>
            <input v-model="pForm.api_key" class="input mono" type="password" placeholder="sk-...（可留空，稍后单独添加）" autocomplete="new-password" />
            <span class="tip">添加即启用；slug、超时与重试将自动使用默认值</span>
          </div>

          <template v-if="pForm.editing">
            <div class="field span2">
              <button type="button" class="btn btn-sm btn-ghost" @click="pForm.showAdvanced = !pForm.showAdvanced">
                {{ pForm.showAdvanced ? '收起高级选项' : '展开高级选项' }}
              </button>
            </div>
            <template v-if="pForm.showAdvanced">
              <div class="field">
                <label>Slug</label>
                <input :value="pForm.slug" class="input mono" disabled />
                <span class="tip">系统生成，不可修改</span>
              </div>
              <div class="field">
                <label>超时（毫秒）</label>
                <input v-model.number="pForm.timeout_ms" class="input num" type="number" min="0" step="1000" />
                <span class="tip">0 = 使用全局默认</span>
              </div>
              <div class="field">
                <label>最大重试</label>
                <input v-model.number="pForm.max_retries" class="input num" type="number" min="0" max="10" />
                <span class="tip">0 = 使用全局默认</span>
              </div>
            </template>
          </template>
        </div>
        <div class="form-actions">
          <button type="button" class="btn btn-ghost" @click="pForm.open = false">取消</button>
          <button type="submit" class="btn btn-primary">保存</button>
        </div>
      </form>
    </AppModal>

    <!-- 凭据表单 -->
    <AppModal
      :open="cForm.open"
      :title="cForm.editing ? '更新凭据' : '添加凭据'"
      max-width="480px"
      @close="cForm.open = false"
    >
      <form @submit.prevent="submitCred">
        <div class="form-grid">
          <div class="field span2">
            <label>标签</label>
            <input v-model="cForm.label" class="input" placeholder="主账号 / 备用池" />
          </div>
          <div class="field span2">
            <label>API Key {{ cForm.editing ? '（留空则不更换）' : '*' }}</label>
            <input v-model="cForm.api_key" class="input mono" type="password" placeholder="sk-..." autocomplete="new-password" />
            <span class="tip">保存后加密入库，仅显示掩码，不再可见明文</span>
          </div>
          <div class="field">
            <label>权重</label>
            <input v-model.number="cForm.weight" class="input num" type="number" min="1" max="100" />
            <span class="tip">多凭据按权重轮询</span>
          </div>
        </div>
        <div class="form-actions">
          <button type="button" class="btn btn-ghost" @click="cForm.open = false">取消</button>
          <button type="submit" class="btn btn-primary">保存</button>
        </div>
      </form>
    </AppModal>

    <!-- 模型：编辑单条 -->
    <AppModal
      v-if="mForm.editing"
      :open="mForm.open"
      title="编辑上游模型"
      max-width="500px"
      @close="mForm.open = false"
    >
      <form @submit.prevent="submitModel">
        <div class="form-grid">
          <div class="field span2">
            <label>上游模型名（model_id）*</label>
            <input v-model="mForm.model_id" class="input mono" placeholder="deepseek-chat" />
            <span class="tip">上游 API 接受的真实模型名</span>
          </div>
          <div class="field">
            <label>上下文窗口</label>
            <input v-model.number="mForm.context_window" class="input num" type="number" min="0" step="1024" placeholder="0 = 未设置" />
          </div>
          <div class="field">
            <label>最大输出</label>
            <input v-model.number="mForm.max_output_tokens" class="input num" type="number" min="0" step="256" placeholder="0 = 未设置" />
          </div>
          <div class="field span2">
            <label class="check-line">
              <input v-model="mForm.enabled" type="checkbox" />
              启用该模型
            </label>
          </div>
        </div>
        <div class="form-actions">
          <button type="button" class="btn btn-ghost" @click="mForm.open = false">取消</button>
          <button type="submit" class="btn btn-primary">保存</button>
        </div>
      </form>
    </AppModal>

    <!-- 模型：探测 + 多选胶囊式添加 -->
    <AppModal
      v-else
      :open="mForm.open"
      title="添加上游模型"
      max-width="560px"
      @close="mForm.open = false"
    >
      <div class="add-model">
        <div class="am-toolbar">
          <button type="button" class="btn btn-sm" :disabled="discovering" @click="runDiscover">
            {{ discovering ? '获取中…' : '获取模型列表' }}
          </button>
          <input
            v-if="discovered.length"
            v-model="filter"
            class="input input-sm"
            placeholder="搜索模型…"
            style="flex: 1"
          />
        </div>
        <span v-if="discoverMsg" class="tip" style="color: var(--danger)">{{ discoverMsg }}</span>

        <!-- 已选胶囊池：点击列表项加入，点 × 移除 -->
        <div v-if="selected.length" class="pill-pool">
          <span v-for="s in selected" :key="s.model_id" class="pill">
            <span class="mono">{{ s.model_id }}</span>
            <button type="button" class="pill-x" title="移除" @click="removeSelected(s.model_id)">×</button>
          </span>
        </div>

        <!-- 可选列表 -->
        <div v-if="discovered.length" class="discover-list">
          <button
            v-for="d in filteredDiscovered"
            :key="d.model_id"
            type="button"
            class="discover-item"
            :class="{ active: isSelected(d.model_id) }"
            @click="toggleSelect(d)"
          >
            <span class="di-check">{{ isSelected(d.model_id) ? '✓' : '+' }}</span>
            <span class="mono">{{ d.model_id }}</span>
            <span class="di-cap">{{ d.context_window ? d.context_window + ' ctx' : '' }}{{ d.context_window && d.max_output_tokens ? ' / ' : '' }}{{ d.max_output_tokens ? d.max_output_tokens + ' out' : '' }}</span>
          </button>
        </div>
        <div v-else-if="!discovering && !discoverMsg" class="empty" style="padding: 18px 0">
          点击「获取模型列表」拉取上游可用模型
        </div>
      </div>
      <div class="form-actions">
        <button type="button" class="btn btn-ghost" @click="mForm.open = false">取消</button>
        <button
          type="button"
          class="btn btn-primary"
          :disabled="!selected.length || importing"
          @click="confirmImport"
        >
          {{ importing ? '添加中…' : `添加（${selected.length}）` }}
        </button>
      </div>
    </AppModal>
  </main>
</template>

<style scoped>
/* 模型速度列：固定宽度 + 右对齐，跨行成列 */
.col-spd {
  flex: none;
  width: 74px;
  text-align: right;
  font-size: 11px;
  color: var(--text-3);
  white-space: nowrap;
}
/* 模型首字用时列：与速度列等宽、同款右对齐，两栏并排才整齐 */
.col-ttfb {
  flex: none;
  width: 74px;
  text-align: right;
  font-size: 11px;
  color: var(--text-3);
  white-space: nowrap;
}
/* 模型成功率列：固定宽度 + 右对齐 */
.col-sr {
  flex: none;
  width: 56px;
  display: flex;
  justify-content: flex-end;
}
.sr {
  font-size: 11px;
  padding: 1px 6px;
  border-radius: 999px;
  background: var(--bg-2, rgba(46, 160, 67, 0.16));
  color: var(--ok, #3fb950);
}
.sr.warn {
  background: var(--bg-2, rgba(218, 149, 29, 0.18));
  color: var(--warn, #d9942a);
}
.add-model {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.am-toolbar {
  display: flex;
  gap: 8px;
  align-items: center;
}
.pill-pool {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
}
.pill {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 4px 6px 4px 10px;
  font-size: 12px;
  border-radius: 999px;
  background: var(--bg-2, rgba(79, 124, 255, 0.14));
  border: 1px solid var(--accent, #4f7cff);
  color: var(--text-1, inherit);
}
.pill-x {
  border: none;
  background: transparent;
  color: inherit;
  cursor: pointer;
  font-size: 14px;
  line-height: 1;
  padding: 0 2px;
  opacity: 0.7;
}
.pill-x:hover {
  opacity: 1;
}
.discover-list {
  margin-top: 8px;
  max-height: 300px;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 4px;
  border: 1px solid var(--border, #2a2a2a);
  border-radius: 8px;
  padding: 6px;
}
.discover-item {
  display: flex;
  gap: 8px;
  align-items: center;
  justify-content: flex-start;
  text-align: left;
  background: transparent;
  border: 1px solid transparent;
  border-radius: 6px;
  padding: 6px 8px;
  font-size: 12px;
  cursor: pointer;
  color: var(--text-2, inherit);
}
.discover-item:hover {
  background: var(--bg-2, rgba(255, 255, 255, 0.04));
}
.discover-item.active {
  border-color: var(--accent, #4f7cff);
  color: var(--text-1, inherit);
}
.di-check {
  width: 16px;
  text-align: center;
  color: var(--accent, #4f7cff);
}
.di-cap {
  margin-left: auto;
  color: var(--text-4, #888);
  white-space: nowrap;
}
</style>


