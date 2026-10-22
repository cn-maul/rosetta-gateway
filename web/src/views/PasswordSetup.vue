<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ApiFail, checkPasswordSetup, completePasswordSetup } from '../api'
import { checkPasswordStrength } from '../password'
import { fmtDateTime } from '../fmt'
import { toast } from '../ui'

const route = useRoute()
const router = useRouter()
const token = computed(() => typeof route.query.token === 'string' ? route.query.token.trim() : '')

const checking = ref(true)
const valid = ref(false)
const username = ref('')
const expiresAt = ref(0)
const password = ref('')
const confirmPassword = ref('')
const showPassword = ref(false)
const submitting = ref(false)
const error = ref('')

onMounted(async () => {
  if (!token.value) {
    checking.value = false
    return
  }
  try {
    const status = await checkPasswordSetup(token.value)
    valid.value = status.valid
    username.value = status.username ?? ''
    expiresAt.value = status.expires_at ?? 0
  } catch (e) {
    error.value = e instanceof ApiFail ? e.message : '无法校验设密链接'
  } finally {
    checking.value = false
  }
})

async function submit() {
  if (submitting.value || !valid.value) return
  error.value = ''
  const weak = checkPasswordStrength(password.value)
  if (weak) {
    error.value = weak
    return
  }
  if (password.value !== confirmPassword.value) {
    error.value = '两次输入的新密码不一致'
    return
  }

  submitting.value = true
  try {
    await completePasswordSetup(token.value, password.value)
    toast('密码已设置，已登录', 'ok')
    await router.replace('/')
  } catch (e) {
    error.value = e instanceof ApiFail ? e.message : '设置密码失败'
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <main class="setup-page">
    <div class="setup-card">
      <div class="setup-kicker">ROSETTA GATEWAY</div>
      <div class="setup-mark">R</div>
      <h1>设置登录密码</h1>

      <div v-if="checking" class="setup-state">
        正在校验一次性链接…
      </div>

      <div v-else-if="!valid" class="setup-state setup-state-error">
        <strong>链接无效或已过期</strong>
        <p>{{ error || '请联系管理员重新生成一次性设密链接。系统不开放公开注册。' }}</p>
      </div>

      <form v-else @submit.prevent="submit">
        <p class="setup-lead">
          为账号 <code>{{ username }}</code> 设置密码，完成后会直接登录。
        </p>
        <p v-if="expiresAt" class="setup-expiry">
          本链接有效至 {{ fmtDateTime(expiresAt) }}，只能使用一次。
        </p>

        <div class="setup-label-row">
          <label for="setup-password">新密码</label>
          <label class="setup-show">
            <input v-model="showPassword" type="checkbox" />
            显示密码
          </label>
        </div>
        <input
          id="setup-password"
          v-model="password"
          :type="showPassword ? 'text' : 'password'"
          class="setup-input"
          autocomplete="new-password"
          autofocus
        />
        <div class="setup-hint">至少 8 个字符，且含字母、数字、符号中的至少两类。</div>

        <label class="setup-label" for="setup-confirm">确认新密码</label>
        <input
          id="setup-confirm"
          v-model="confirmPassword"
          :type="showPassword ? 'text' : 'password'"
          class="setup-input"
          autocomplete="new-password"
        />

        <div v-if="error" class="setup-error" role="alert">{{ error }}</div>

        <button class="btn btn-primary setup-submit" type="submit" :disabled="submitting">
          {{ submitting ? '设置中…' : '设置密码并登录' }}
        </button>
      </form>
    </div>
  </main>
</template>

<style scoped>
.setup-page {
  min-height: 100dvh;
  display: grid;
  place-items: center;
  padding: 28px 18px;
  background:
    radial-gradient(circle at 18% 16%, color-mix(in srgb, var(--accent) 14%, transparent), transparent 34%),
    linear-gradient(145deg, var(--background), var(--surface-secondary));
}
.setup-card {
  width: min(100%, 430px);
  padding: 30px 30px 26px;
  border: 1px solid var(--border);
  border-radius: 18px;
  background: color-mix(in srgb, var(--surface) 94%, transparent);
  box-shadow: 0 22px 70px color-mix(in srgb, #000 15%, transparent);
  display: flex;
  flex-direction: column;
}
.setup-kicker {
  color: var(--muted);
  font-family: var(--mono);
  font-size: 10px;
  letter-spacing: 0.18em;
}
.setup-mark {
  width: 42px;
  height: 42px;
  margin-top: 18px;
  border-radius: 12px;
  display: grid;
  place-items: center;
  background: var(--accent);
  color: var(--accent-foreground);
  font-size: 20px;
  font-weight: 700;
}
h1 {
  margin: 14px 0 6px;
  font-size: 22px;
  letter-spacing: -0.02em;
}
.setup-lead {
  margin: 4px 0 3px;
  color: var(--foreground);
  font-size: 13px;
  line-height: 1.7;
}
.setup-lead code {
  padding: 1px 5px;
  border-radius: 4px;
  background: var(--surface-secondary);
  font-family: var(--mono);
}
.setup-expiry {
  margin: 0 0 14px;
  color: var(--muted);
  font-size: 12px;
}
.setup-label-row {
  display: flex;
  justify-content: space-between;
  align-items: baseline;
  gap: 12px;
  margin-top: 14px;
}
.setup-label,
.setup-label-row label:first-child {
  color: var(--muted);
  font-size: 12px;
}
.setup-label {
  display: block;
  margin: 14px 0 5px;
}
.setup-show {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  color: var(--muted);
  font-size: 12px;
  cursor: pointer;
}
.setup-show input {
  margin: 0;
  accent-color: var(--accent);
}
.setup-input {
  width: 100%;
  box-sizing: border-box;
  padding: 10px 11px;
  border: 1px solid var(--border);
  border-radius: 8px;
  background: var(--background);
  color: var(--foreground);
  font-size: 14px;
}
.setup-input:focus-visible {
  outline: 2px solid var(--accent);
  outline-offset: 1px;
}
.setup-hint {
  margin-top: 6px;
  color: var(--muted);
  font-size: 11px;
}
.setup-error {
  margin-top: 12px;
  color: var(--danger);
  font-size: 12px;
  line-height: 1.5;
}
.setup-submit {
  width: 100%;
  justify-content: center;
  margin-top: 20px;
}
.setup-state {
  margin-top: 20px;
  padding: 18px;
  border: 1px solid var(--border);
  border-radius: 12px;
  background: var(--surface-secondary);
  color: var(--muted);
  font-size: 13px;
  line-height: 1.7;
}
.setup-state-error strong {
  color: var(--foreground);
}
.setup-state-error p {
  margin: 6px 0 0;
}
</style>
