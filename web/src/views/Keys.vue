<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import { toast, confirmBox } from '../ui'
import { fmtNum, fmtExpiry, fmtDateTime, unixToLocalInput, localInputToUnix, copyText } from '../fmt'
import type { AccessKey, KeyCreateResponse } from '../types'
import AppModal from '../components/AppModal.vue'

const loading = ref(true)
const keys = ref<AccessKey[]>([])

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
  quota_tokens: 0,
  rpm_limit: 0,
  tpm_limit: 0,
  expires_at: '', // datetime-local 字符串；空 = 永不过期
})

function openCreate() {
  form.name = ''
  form.quota_tokens = 0
  form.rpm_limit = 0
  form.tpm_limit = 0
  form.expires_at = ''
  form.open = true
}

async function submitCreate() {
  if (!form.name.trim()) {
    toast('名称为必填项', 'err')
    return
  }
  try {
    const created = await api.createKey({
      name: form.name.trim(),
      quota_tokens: Number(form.quota_tokens) || 0,
      rpm_limit: Number(form.rpm_limit) || 0,
      tpm_limit: Number(form.tpm_limit) || 0,
      expires_at: localInputToUnix(form.expires_at),
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

async function copyKey() {
  const ok = await copyText(plainKeyBox.key)
  toast(ok ? '已复制到剪贴板' : '复制失败，请手动选择复制', ok ? 'ok' : 'err')
}

// ---------- 编辑表单 ----------
// 注意：后端 PATCH 为「非空才覆盖」，数字 0 不会写入。
// 因此配额/限速字段无法通过编辑清零 —— 界面提示用户如需取消限额请删除重建。

const eForm = reactive({
  open: false,
  id: '',
  name: '',
  quota_tokens: 0,
  rpm_limit: 0,
  tpm_limit: 0,
  expires_at: '',
})

function openEdit(k: AccessKey) {
  eForm.id = k.id
  eForm.name = k.name
  eForm.quota_tokens = k.quota_tokens
  eForm.rpm_limit = k.rpm_limit
  eForm.tpm_limit = k.tpm_limit
  eForm.expires_at = unixToLocalInput(k.expires_at)
  eForm.open = true
}

async function submitEdit() {
  if (!eForm.name.trim()) {
    toast('名称为必填项', 'err')
    return
  }
  try {
    await api.updateKey(eForm.id, {
      name: eForm.name.trim(),
      quota_tokens: Number(eForm.quota_tokens) || 0,
      rpm_limit: Number(eForm.rpm_limit) || 0,
      tpm_limit: Number(eForm.tpm_limit) || 0,
      expires_at: localInputToUnix(eForm.expires_at),
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
    await api.updateKey(k.id, {
      name: k.name,
      enabled: !k.enabled,
      expires_at: k.expires_at,
      quota_tokens: k.quota_tokens,
      rpm_limit: k.rpm_limit,
      tpm_limit: k.tpm_limit,
    })
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
              <span v-if="k.expires_at" class="badge" :class="{ 'badge-err': k.expires_at * 1000 < Date.now() }">
                {{ fmtExpiry(k.expires_at) }}
              </span>
            </div>
            <div class="row-sub mono">{{ k.key_prefix }}</div>
            <div class="row-sub num">
              用量 {{ fmtNum(k.used_tokens) }}<template v-if="k.quota_tokens"> / {{ fmtNum(k.quota_tokens) }} tokens</template>
              <template v-if="k.rpm_limit"> · RPM ≤ {{ k.rpm_limit }}</template>
              <template v-if="k.tpm_limit"> · TPM ≤ {{ k.tpm_limit }}</template>
              · 建于 {{ fmtDateTime(k.created_at) }}
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
          <div class="field">
            <label>Token 配额</label>
            <input v-model.number="form.quota_tokens" class="input num" type="number" min="0" step="1000" placeholder="0 = 不限" />
          </div>
          <div class="field">
            <label>过期时间</label>
            <input v-model="form.expires_at" class="input" type="datetime-local" />
            <span class="tip">留空 = 永不过期</span>
          </div>
          <div class="field">
            <label>RPM 限制</label>
            <input v-model.number="form.rpm_limit" class="input num" type="number" min="0" placeholder="0 = 不限" />
          </div>
          <div class="field">
            <label>TPM 限制</label>
            <input v-model.number="form.tpm_limit" class="input num" type="number" min="0" placeholder="0 = 不限" />
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
          <div class="field">
            <label>Token 配额</label>
            <input v-model.number="eForm.quota_tokens" class="input num" type="number" min="0" step="1000" />
            <span class="tip">设为 0 不会清除已有限额（需删除重建）</span>
          </div>
          <div class="field">
            <label>过期时间</label>
            <input v-model="eForm.expires_at" class="input" type="datetime-local" />
          </div>
          <div class="field">
            <label>RPM 限制</label>
            <input v-model.number="eForm.rpm_limit" class="input num" type="number" min="0" />
          </div>
          <div class="field">
            <label>TPM 限制</label>
            <input v-model.number="eForm.tpm_limit" class="input num" type="number" min="0" />
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
