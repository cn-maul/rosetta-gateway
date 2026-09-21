<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { api, saveToken } from '../api'
import { toast } from '../ui'
import AppModal from '../components/AppModal.vue'

const loading = ref(true)
const saving = ref(false)
const form = reactive({ default_context_window: 8192, default_max_output_tokens: 4096 })

// 密码设置相关
const passwordForm = reactive({
  open: false,
  currentPassword: '',
  newPassword: '',
  confirmPassword: '',
})
const hasPassword = ref(false)

async function load() {
  loading.value = true
  try {
    const s = await api.settings()
    form.default_context_window = s.default_context_window
    form.default_max_output_tokens = s.default_max_output_tokens
    
    // 检查是否已设置密码
    const pwdCheck = await fetch('/admin/api/password/check')
    if (pwdCheck.ok) {
      const data = await pwdCheck.json()
      hasPassword.value = data.has_password
    }
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('加载设置失败：' + (e as Error).message, 'err')
  } finally {
    loading.value = false
  }
}

async function save() {
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
  saving.value = true
  try {
    await api.saveSettings({ default_context_window: ctx, default_max_output_tokens: out })
    toast('设置已保存')
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
  if (passwordForm.newPassword.length < 6) {
    toast('新密码长度至少为6位', 'err')
    return
  }
  if (passwordForm.newPassword !== passwordForm.confirmPassword) {
    toast('两次输入的新密码不一致', 'err')
    return
  }

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
    
    const data = await res.json()
    if (res.ok) {
      toast(data.message || '密码已更新')
      passwordForm.open = false
      hasPassword.value = true
      // 旧凭据即刻失效（校验优先使用新密码），必须当场换掉本地令牌，
      // 否则下一次请求就 401 —— 表现为「改完密码反而被锁在外面」。
      saveToken(passwordForm.newPassword)
    } else {
      toast(data.error?.message || '设置密码失败', 'err')
    }
  } catch (e) {
    toast('设置密码失败：' + (e as Error).message, 'err')
  }
}

onMounted(load)
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>设置</h1>
        <div class="sub">全局默认值；模型容量探测不到时回落到这里</div>
      </div>
      <div class="head-actions">
        <button class="btn" :disabled="loading" @click="load">刷新</button>
      </div>
    </div>

    <div class="panel">
      <div v-if="loading" class="loading">加载中…</div>
      <form v-else class="form-grid" style="max-width: 560px" @submit.prevent="save">
        <div class="field span2">
          <label>默认上下文窗口（tokens）</label>
          <input v-model.number="form.default_context_window" class="input num" type="number" min="1" step="1024" />
          <span class="tip">添加模型时若上游未暴露 context_length，则用此值</span>
        </div>
        <div class="field span2">
          <label>默认最大输出（tokens）</label>
          <input v-model.number="form.default_max_output_tokens" class="input num" type="number" min="1" step="256" />
          <span class="tip">添加模型时若上游未暴露 max_completion_tokens，则用此值</span>
        </div>
        <div class="field span2 form-actions" style="padding: 0">
          <button type="submit" class="btn btn-primary" :disabled="saving">{{ saving ? '保存中…' : '保存设置' }}</button>
        </div>
      </form>
    </div>

    <div class="panel" style="margin-top: 1rem">
      <h2 style="margin-bottom: 1rem">安全设置</h2>
      <div style="display: flex; align-items: center; gap: 1rem">
        <div>
          <div style="font-weight: 600">管理密码</div>
          <div style="color: var(--text-3); font-size: 13px; margin-top: 4px">
            {{ hasPassword ? '已设置密码' : '尚未设置密码' }}
          </div>
        </div>
        <button class="btn" @click="openPasswordModal">
          {{ hasPassword ? '修改密码' : '设置密码' }}
        </button>
      </div>
    </div>

    <!-- 密码设置弹窗 -->
    <AppModal :open="passwordForm.open" title="设置管理密码" max-width="500px" @close="passwordForm.open = false">
      <form @submit.prevent="changePassword">
        <div class="form-grid">
          <div v-if="hasPassword" class="field span2">
            <label>当前密码 *</label>
            <input v-model="passwordForm.currentPassword" class="input" type="password" placeholder="请输入当前密码" />
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
          <button type="submit" class="btn btn-primary">确定</button>
        </div>
      </form>
    </AppModal>
  </main>
</template>
