<script setup lang="ts">
/**
 * 个人页：查看自己的额度、改自己的密码。
 *
 * # 为什么「改密码」要放在这里而不是设置页
 *
 * 设置页是 admin-only。普通用户能进这个页、能看到的只有自己的信息与
 * 一个改密表单 —— 这就是我们多用户改造里普通用户的全部可操作面。
 */
import { computed, ref } from 'vue'
import { api, ApiFail, session } from '../api'
import { fmtTokens } from '../fmt'
import { toast } from '../ui'

const oldPwd = ref('')
const newPwd = ref('')
const confirmPwd = ref('')
const busy = ref(false)
const err = ref('')

// 必须走 computed 而不是把 session.me 抄一份局部常量：setup 只跑一次，
// 而 me 是在 loadSession() 的 await 之后才填上的。抄常量的后果是整页
// 永远按 null 渲染 —— 管理员被显示成「普通用户 / 已禁用 / 用户名为空」。
const me = computed(() => session.me)

// 密码强度提示与后端 userauth.ValidatePassword 同一套门槛。
// 刻意与后端保持一致但**不替代**后端校验 —— 前端只是提前告知，
// 真正的强度判断必须在服务端做（绕过前端直接打接口是常态）。
const newPwdErr = ref('')
function checkStrength(p: string) {
  if (!p) return ''
  const runes = [...p]
  if (runes.length < 8) return '至少 8 个字符'
  let lower = false, upper = false, digit = false, other = false
  for (const r of runes) {
    if (r >= 'a' && r <= 'z') lower = true
    else if (r >= 'A' && r <= 'Z') upper = true
    else if (r >= '0' && r <= '9') digit = true
    else other = true
  }
  const classes = [lower, upper, digit, other].filter(Boolean).length
  if (classes < 2) return '需包含字母/数字/符号中的至少两类'
  return ''
}

async function submit() {
  if (busy.value) return
  err.value = ''
  newPwdErr.value = checkStrength(newPwd.value)
  if (newPwdErr.value) return
  if (newPwd.value !== confirmPwd.value) {
    err.value = '两次输入的新密码不一致'
    return
  }
  busy.value = true
  try {
    await api.changeMyPassword(oldPwd.value, newPwd.value)
    // 后端会递增 auth_version，所以**当前会话也随之失效**。
    // 提示用户重新登录，而不是让他们对着一个必然 401 的界面发呆。
    toast('密码已更新，请重新登录', 'ok')
    setTimeout(() => location.reload(), 1200)
  } catch (e) {
    err.value = e instanceof ApiFail ? e.message : '改密失败'
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>我的账号</h1>
        <div class="sub">你的身份、额度与密码</div>
      </div>
    </div>

    <div class="panel">
      <div class="row-list">
        <div class="row">
          <div class="row-main">
            <div class="row-title">
              {{ me?.display_name || me?.username }}
              <span class="badge" :class="me?.role === 'admin' ? 'badge-live' : 'badge-off'">
                {{ me?.role === 'admin' ? '管理员' : '普通用户' }}
              </span>
              <span v-if="me?.status !== 'active'" class="badge badge-off">已禁用</span>
            </div>
            <div class="row-sub">用户名：<span class="mono">{{ me?.username }}</span></div>
          </div>
        </div>

        <div class="row">
          <div class="row-main">
            <div class="row-title">我的额度</div>
            <div class="row-sub num">
              已用 {{ fmtTokens(me?.used_tokens ?? 0) }}
              <template v-if="me && me.quota_tokens > 0">
                / {{ fmtTokens(me.quota_tokens) }}
              </template>
            </div>
            <div v-if="me && me.quota_tokens > 0" class="row-sub num">
              剩余 {{ fmtTokens(Math.max(0, me.quota_tokens - me.used_tokens)) }}
            </div>
            <div v-else class="row-sub">未设置额度上限（不限）</div>
          </div>
        </div>
      </div>
    </div>

    <div class="panel">
      <div class="page-head-sm">
        <h2>修改密码</h2>
        <div class="sub">
          修改后当前登录状态会立即失效，需要用新密码重新登录。
        </div>
      </div>
      <form class="form" @submit.prevent="submit">
        <label class="flabel" for="op">当前密码</label>
        <input id="op" v-model="oldPwd" type="password" class="fin" autocomplete="current-password" />

        <label class="flabel" for="np">新密码</label>
        <input id="np" v-model="newPwd" type="password" class="fin" autocomplete="new-password" />
        <div v-if="checkStrength(newPwd)" class="fhint warn">{{ checkStrength(newPwd) }}</div>
        <div v-else-if="newPwd" class="fhint ok">强度符合要求</div>

        <label class="flabel" for="cp">确认新密码</label>
        <input id="cp" v-model="confirmPwd" type="password" class="fin" autocomplete="new-password" />

        <div v-if="err" class="fhint err">{{ err }}</div>

        <button class="btn btn-primary" type="submit" :disabled="busy">
          {{ busy ? '提交中…' : '修改密码' }}
        </button>
      </form>
    </div>
  </main>
</template>

<style scoped>
.page-head-sm { margin-bottom: 12px; }
.page-head-sm h2 { font-size: 15px; margin: 0; }
.form { display: flex; flex-direction: column; align-items: flex-start; }
.flabel { font-size: 12px; color: var(--muted); margin: 10px 0 4px; }
.fin {
  width: 100%;
  max-width: 360px;
  padding: 8px 10px;
  border: 1px solid var(--border);
  border-radius: var(--r-thumb);
  background: var(--background);
  color: var(--foreground);
  font-size: 14px;
}
.fin:focus-visible { outline: 2px solid var(--accent); outline-offset: 1px; }
.fhint { font-size: 12px; margin-top: 4px; }
.fhint.warn { color: var(--muted); }
.fhint.ok { color: var(--success); }
.fhint.err { color: var(--danger); margin-top: 10px; }
button { margin-top: 18px; }
</style>
