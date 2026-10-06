<script setup lang="ts">
/**
 * 登录页。三种形态**互斥**，由 session.backendReady / session.needsSetup 决定。
 *
 * 为什么必须互斥：一屏里同时出现「无法连接」和一个登录框，用户会去填那张
 * 永远提交不上去的表单；引导期同时摆出「用户名 + 密码」和「设新密码」也一样，
 * 两者提交的是完全不同的端点，填错的那张只会得到一句莫名其妙的报错。
 * 所以这里收敛成一个 mode computed，让「不重叠」成为结构上的保证，
 * 而不是靠改条件时的自觉。
 *
 *   1. offline（!backendReady）—— /session 探测失败，网关根本没起来。
 *      只说「查进程 / 查地址」，**不渲染任何表单**。
 *
 *   2. setup（needsSetup）—— 全新部署，/bootstrap 建出了第一个管理员但还没设密码。
 *      这是**唯一一次**免鉴权设密码的机会：提交成功后窗口就此关闭，
 *      界面上不该再留下任何「设密码」入口。
 *
 *   3. login（其他）—— 普通用户名 + 密码。
 *
 * 已删除、不要重新引入的东西：
 *   - 曾经的双通道鉴权（配置里的管理员令牌字段 + 独立的令牌文件）已整体删除，
 *     管理面只剩 users 表 + 会话一种形态。**不要**在这里再加一条不带用户表的
 *     免鉴权通道：绕开 users 表就等于绕开了用户状态、角色与改密审计。
 *   - 曾要求运维去配「会话密钥」环境变量的整段提示。多用户开关已经并入
     users 表，不再靠环境变量决定要不要登录，加回来只会误导。
 *   - 曾有的「引导账号先登录、再被送去个人页设密码」两步流程。
 *     现在 /bootstrap 一步完成设密码 + 登录（见 api.ts 的 bootstrapSetup）。
 */
import { computed, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ApiFail, bootstrapSetup, login, session } from '../api'
import { toast } from '../ui'

const route = useRoute()
const router = useRouter()

const mode = computed<'offline' | 'setup' | 'login'>(() => {
  if (!session.backendReady) return 'offline'
  if (session.needsSetup) return 'setup'
  return 'login'
})

const username = ref('')
const password = ref('')
const newPwd = ref('')
const confirmPwd = ref('')
const submitting = ref(false)
const error = ref('')

/**
 * 密码强度提示，与后端 userauth.ValidatePassword 同一套门槛
 * （≥8 字符 + 字母/数字/符号中至少两类），也和 Profile.vue 里的那份一致。
 *
 * 这只是提前告知，**不替代**服务端校验 —— 绕过前端直接打接口是常态。
 * 刻意保持三处同源：门槛漂移会让用户在这里看着「强度合规」却被后端拒绝。
 */
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

/**
 * 认证成功后离开本页。
 *
 * 必须离开：此前只 toast 不跳转，于是 session.me 已填好、导航栏也出现了，
 * 但 hash 仍停在 /login —— 用户看到的是「已登录」的外壳 + 一个空登录表单，
 * 像是登录没生效。router 守卫救不了：它只拦「未登录」，
 * 不负责把已登录的人送走。
 *
 * replace 而非 push：登录页不该留在历史里，否则后退会又看见它。
 */
async function leaveToBack() {
  const back = typeof route.query.redirect === 'string' ? route.query.redirect : '/'
  // 只接受站内相对路径：query 来自 URL，外站地址会变成开放重定向。
  // 「/」开头且非「//」开头 —— 后者会被浏览器当成协议相对地址
  // （//evil.com）跳到外站。
  await router.replace(back.startsWith('/') && !back.startsWith('//') ? back : '/')
}

async function submitLogin() {
  if (submitting.value) return
  error.value = ''
  if (!username.value.trim() || !password.value) {
    error.value = '请填写用户名与密码'
    return
  }
  submitting.value = true
  try {
    await login(username.value.trim(), password.value)
    toast('已登录', 'ok')
    await leaveToBack()
  } catch (e) {
    // 后端对「用户不存在」与「密码错误」返回同一句话（防枚举设计），
    // 这里原样透出即可，**不要**自己判断是哪种错再改文案 ——
    // 那等于在前端重新造一个账号枚举 oracle。
    error.value = e instanceof ApiFail ? e.message : '登录失败'
  } finally {
    submitting.value = false
  }
}

async function submitSetup() {
  if (submitting.value) return
  error.value = ''
  const weak = checkStrength(newPwd.value)
  if (weak) {
    error.value = weak
    return
  }
  // 二次确认：这个入口一辈子只开一次，输错没有「再来一次」，
  // 所以宁可多按一次回车，也不能让用户拿一个记错的密码把自己锁在外面。
  if (newPwd.value !== confirmPwd.value) {
    error.value = '两次输入的新密码不一致'
    return
  }
  submitting.value = true
  try {
    await bootstrapSetup(newPwd.value)
    toast('密码已设置', 'ok')
    await leaveToBack()
  } catch (e) {
    // 同上：后端文案原样透出。前端不区分「已被初始化」和「密码不合规」。
    error.value = e instanceof ApiFail ? e.message : '设置密码失败'
  } finally {
    submitting.value = false
  }
}
</script>

<template>
  <main class="page login-page">
    <div class="login-card">
      <div class="brand-mark">R</div>
      <h1 class="login-title">rosetta-gateway</h1>
      <div class="sub login-sub">网关管理后台</div>

      <!-- 形态 1：网关不可达。这里刻意一个表单都不渲染 ——
           探测已经失败，此时任何提交都只会带回同一个连接错误，
           给输入框等于邀请用户对着一个坏掉的地址反复重试。 -->
      <div v-if="mode === 'offline'" class="login-hint">
        <p class="login-hd">无法连接到网关</p>
        <p>本机服务没有响应，或当前页面指向的地址不对。</p>
        <p>请确认网关进程正在运行、监听地址正确，然后刷新本页面。</p>
      </div>

      <!-- 形态 2：首次部署设密码。没有用户名输入框 —— 账号由 /bootstrap
           建好，用户只能设密码，改名/换账号不是这一步的事。 -->
      <form v-else-if="mode === 'setup'" @submit.prevent="submitSetup">
        <div class="login-hint">
          <p class="login-hd">首次设置密码</p>
          <p>为第一个管理员账号 <code v-if="session.setupUsername">{{ session.setupUsername }}</code> 设置密码，设完直接进入后台。</p>
          <p><strong>只有这一次机会</strong>，提交后这个入口永久关闭。</p>
        </div>

        <label class="login-label" for="np">新密码</label>
        <input id="np" v-model="newPwd" type="password" class="login-input" autocomplete="new-password" />
        <div v-if="checkStrength(newPwd)" class="login-pw-hint">{{ checkStrength(newPwd) }}</div>

        <label class="login-label" for="cp">确认新密码</label>
        <input id="cp" v-model="confirmPwd" type="password" class="login-input" autocomplete="new-password" />

        <div v-if="error" class="login-err">{{ error }}</div>

        <button class="btn btn-primary login-btn" type="submit" :disabled="submitting">
          {{ submitting ? '设置中…' : '设置密码并进入' }}
        </button>
      </form>

      <!-- 形态 3：普通登录 -->
      <form v-else @submit.prevent="submitLogin">
        <label class="login-label" for="u">用户名</label>
        <input id="u" v-model="username" class="login-input" autocomplete="username"
               autocapitalize="off" autocorrect="off" spellcheck="false" />

        <label class="login-label" for="p">密码</label>
        <input id="p" v-model="password" type="password" class="login-input"
               autocomplete="current-password" />

        <div v-if="error" class="login-err">{{ error }}</div>

        <button class="btn btn-primary login-btn" type="submit" :disabled="submitting">
          {{ submitting ? '登录中…' : '登录' }}
        </button>
      </form>
    </div>
  </main>
</template>

<style scoped>
.login-page {
  min-height: 100dvh;
  display: grid;
  place-items: center;
  padding: 24px;
}
.login-card {
  width: 100%;
  max-width: 360px;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--r-card);
  padding: 28px 24px 24px;
  display: flex;
  flex-direction: column;
  align-items: center;
}
.brand-mark {
  width: 40px;
  height: 40px;
  border-radius: 10px;
  display: grid;
  place-items: center;
  font-weight: 700;
  font-size: 20px;
  background: var(--accent);
  color: var(--accent-foreground);
}
.login-title {
  margin: 12px 0 0;
  font-size: 17px;
}
.login-sub {
  margin-bottom: 18px;
}
.login-label {
  align-self: flex-start;
  font-size: 12px;
  color: var(--muted);
  margin: 10px 0 4px;
}
.login-input {
  width: 100%;
  padding: 8px 10px;
  border: 1px solid var(--border);
  border-radius: var(--r-thumb);
  background: var(--background);
  color: var(--foreground);
  font-size: 14px;
}
.login-input:focus-visible {
  outline: 2px solid var(--accent);
  outline-offset: 1px;
}
.login-err {
  align-self: flex-start;
  margin-top: 10px;
  font-size: 12px;
  color: var(--danger);
}
.login-btn {
  width: 100%;
  margin-top: 18px;
  justify-content: center;
}
.login-hint {
  font-size: 13px;
  line-height: 1.7;
  text-align: left;
  width: 100%;
}
.login-hint code {
  font-family: var(--mono);
  background: var(--surface-secondary);
  padding: 1px 4px;
  border-radius: 3px;
  word-break: break-all;
}
.login-hint p {
  margin: 0 0 6px;
}
.login-hint p:last-child {
  margin-bottom: 0;
}
.login-hd {
  font-weight: 600;
  font-size: 14px;
}
.login-pw-hint {
  align-self: flex-start;
  margin-top: 6px;
  font-size: 12px;
  color: var(--muted);
}
</style>
