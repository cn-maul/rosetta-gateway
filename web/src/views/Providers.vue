<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import { toast, confirmBox } from '../ui'
import { fmtDate } from '../fmt'
import type { Provider, Credential, UpstreamModel } from '../types'
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

const pForm = reactive({ open: false, editing: '' as string, slug: '', name: '', protocol: 'openai', endpoint: '', timeout_ms: 30000, max_retries: 2, enabled: true })

function openProvider(p?: Provider) {
  pForm.editing = p?.id ?? ''
  pForm.slug = p?.slug ?? ''
  pForm.name = p?.name ?? ''
  pForm.protocol = p?.protocol || 'openai'
  pForm.endpoint = p?.endpoint ?? ''
  pForm.timeout_ms = p?.timeout_ms || 30000
  pForm.max_retries = p?.max_retries ?? 2
  pForm.enabled = p?.enabled ?? true
  pForm.open = true
}

async function submitProvider() {
  if (!pForm.slug.trim() || !pForm.endpoint.trim()) {
    toast('slug 与 endpoint 为必填项', 'err')
    return
  }
  const body = {
    slug: pForm.slug.trim(),
    name: pForm.name.trim() || pForm.slug.trim(),
    protocol: pForm.protocol.trim(),
    endpoint: pForm.endpoint.trim(),
    timeout_ms: Number(pForm.timeout_ms) || 0,
    max_retries: Number(pForm.max_retries) || 0,
    enabled: pForm.enabled,
  }
  try {
    if (pForm.editing) await api.updateProvider(pForm.editing, body)
    else await api.createProvider(body)
    pForm.open = false
    toast(pForm.editing ? '上游已更新' : '上游已创建')
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
    toast(`连通性测试：${r.message || r.status || '完成'}`, r.status === 'ok' || !r.message ? 'ok' : 'err')
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

// ---------- 模型表单 ----------

const mForm = reactive({ open: false, provider: '' as string, editing: '' as string, model_id: '', display_name: '', context_window: 0, max_output_tokens: 0, enabled: true })

function openModel(providerId: string, m?: UpstreamModel) {
  mForm.provider = providerId
  mForm.editing = m?.id ?? ''
  mForm.model_id = m?.model_id ?? ''
  mForm.display_name = m?.display_name ?? ''
  mForm.context_window = m?.context_window ?? 0
  mForm.max_output_tokens = m?.max_output_tokens ?? 0
  mForm.enabled = m?.enabled ?? true
  mForm.open = true
}

async function submitModel() {
  if (!mForm.model_id.trim()) {
    toast('上游模型名（model_id）为必填项', 'err')
    return
  }
  const body = {
    model_id: mForm.model_id.trim(),
    display_name: mForm.display_name.trim(),
    context_window: Number(mForm.context_window) || 0,
    max_output_tokens: Number(mForm.max_output_tokens) || 0,
    enabled: mForm.enabled,
  }
  try {
    if (mForm.editing) await api.updateModel(mForm.editing, body)
    else await api.createModel(mForm.provider, body)
    mForm.open = false
    toast(mForm.editing ? '模型已更新' : '模型已创建')
    await openExpand(mForm.provider, true)
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('保存失败：' + (e as Error).message, 'err')
  }
}

async function removeModel(m: UpstreamModel) {
  const ok = await confirmBox({
    title: `删除模型「${m.display_name || m.model_id}」？`,
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
                  <span class="mini-main mono">
                    {{ m.model_id }}<template v-if="m.display_name"> · {{ m.display_name }}</template>
                  </span>
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
            <label>Slug（唯一标识）*</label>
            <input v-model="pForm.slug" class="input mono" placeholder="deepseek" :disabled="!!pForm.editing" />
          </div>
          <div class="field">
            <label>显示名称</label>
            <input v-model="pForm.name" class="input" placeholder="DeepSeek" />
          </div>
          <div class="field">
            <label>协议</label>
            <select v-model="pForm.protocol" class="select">
              <option value="openai">openai</option>
              <option value="anthropic">anthropic</option>
            </select>
          </div>
          <div class="field">
            <label>Endpoint *</label>
            <input v-model="pForm.endpoint" class="input mono" placeholder="https://api.deepseek.com" />
          </div>
          <div class="field">
            <label>超时（毫秒）</label>
            <input v-model.number="pForm.timeout_ms" class="input num" type="number" min="0" step="1000" />
          </div>
          <div class="field">
            <label>最大重试</label>
            <input v-model.number="pForm.max_retries" class="input num" type="number" min="0" max="10" />
          </div>
          <div class="field span2">
            <label class="check-line">
              <input v-model="pForm.enabled" type="checkbox" />
              启用该上游
            </label>
          </div>
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

    <!-- 模型表单 -->
    <AppModal
      :open="mForm.open"
      :title="mForm.editing ? '编辑上游模型' : '添加上游模型'"
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
          <div class="field span2">
            <label>显示名称</label>
            <input v-model="mForm.display_name" class="input" placeholder="DeepSeek Chat" />
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
  </main>
</template>

