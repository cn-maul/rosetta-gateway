<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, ApiFail, session, saveToken } from '../api'
import { toast } from '../ui'
import { checkPasswordStrength } from '../password'

const router = useRouter()
const oldPwd = ref('')
const newPwd = ref('')
const confirmPwd = ref('')
const busy = ref(false)
const err = ref('')

async function submit() {
  if (busy.value) return
  err.value = checkPasswordStrength(newPwd.value)
  if (err.value) return
  if (newPwd.value !== confirmPwd.value) {
    err.value = '两次输入的新密码不一致'
    return
  }
  busy.value = true
  try {
    await api.changeMyPassword(oldPwd.value, newPwd.value)
    // 后端改密即撤销会话，本地同步清除，避免继续使用已失效的身份。
    saveToken('')
    session.me = null
    toast('密码已更新，请用新密码重新登录')
    oldPwd.value = newPwd.value = confirmPwd.value = ''
    await router.replace({ name: 'login', query: { redirect: '/account' } })
  } catch (e) {
    err.value = e instanceof ApiFail ? e.message : '修改密码失败'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <main class="page">
    <div class="page-head"><div><h1>账号设置</h1><div class="sub">{{ session.me?.username }} · 管理自己的登录安全</div></div></div>
    <section class="panel account-panel">
      <div class="panel-title">修改密码<span class="hint">修改后需要重新登录</span></div>
      <form class="account-form" @submit.prevent="submit">
        <div class="field"><label for="current-password">当前密码</label><input id="current-password" v-model="oldPwd" class="input" type="password" autocomplete="current-password" required /></div>
        <div class="field"><label for="new-password">新密码</label><input id="new-password" v-model="newPwd" class="input" type="password" autocomplete="new-password" required /><span class="tip">{{ checkPasswordStrength(newPwd) || '强度符合要求' }}</span></div>
        <div class="field"><label for="confirm-password">确认新密码</label><input id="confirm-password" v-model="confirmPwd" class="input" type="password" autocomplete="new-password" required /></div>
        <p v-if="err" class="form-error" role="alert">{{ err }}</p>
        <div><button class="btn btn-primary" :disabled="busy">{{ busy ? '提交中…' : '修改密码' }}</button></div>
      </form>
    </section>
  </main>
</template>

<style scoped>
.account-form { display: grid; gap: 18px; max-width: 480px; padding: 22px; }
.form-error { color: var(--danger); font-size: 13px; }
</style>
