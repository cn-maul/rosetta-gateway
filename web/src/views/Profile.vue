<script setup lang="ts">
/**
 * 个人页：查看自己的额度与余额、改自己的密码。
 *
 * # 为什么「改密码」要放在这里而不是设置页
 *
 * 设置页是 admin-only。普通用户能进这个页、能看到的只有自己的信息与
 * 一个改密表单 —— 这就是我们多用户改造里普通用户的全部可操作面。
 *
 * 余额也放在这里：它是普通用户唯一能看到的「我还有多少钱」，而这一页本来
 * 就是普通用户的落地页。放在管理员的用户页等于让余额只能被别人看。
 */
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, ApiFail, saveToken, session } from '../api'
import { fmtTokens, fmtBalance } from '../fmt'
import { toast } from '../ui'
import { checkPasswordStrength } from '../password'

const oldPwd = ref('')
const newPwd = ref('')
const confirmPwd = ref('')
const busy = ref(false)
const err = ref('')

const router = useRouter()

// 必须走 computed 而不是把 session.me 抄一份局部常量：setup 只跑一次，
// 而 me 是在 loadSession() 的 await 之后才填上的。抄常量的后果是整页
// 永远按 null 渲染 —— 管理员被显示成「普通用户 / 已禁用 / 用户名为空」。
const me = computed(() => session.me)

// 密码强度提示与后端 userauth.ValidatePassword 同一套门槛（见 password.ts）。
// 刻意与后端保持一致但**不替代**后端校验 —— 前端只是提前告知，
// 真正的强度判断必须在服务端做（绕过前端直接打接口是常态）。
const newPwdErr = ref('')
function checkStrength(p: string) {
  return checkPasswordStrength(p)
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
    // 后端在此递增了 auth_version，**当前这个会话已经失效**（ChangePassword
    // 还顺手清了会话 cookie）。旧实现用 setTimeout(reload, 1200) 延迟 1.2 秒
    // 才把人送走，那段时间里导航栏还在、表单还在，任何一个点击都会打到
    // 一个必然 401 的接口上；而 App.vue 的 watch 又会在那一刻才把人弹回登录页，
    // 用户看到的是「点了提交 → 界面自己跳了一下」。所以这里**立刻**清掉本地
    // 会话并跳登录页：改密成功与「需要重新登录」是同一件事，不该拆成两步。
    //
    // 不调 logout()：那个端点也会（按用户）递增 auth_version 并清 cookie，
    // 而我们已经处在「会话已失效」的状态下，再发一请求只会拿到 401，
    // 还会在 api.ts 的 401 分支里再清一次令牌。本地 saveToken('') 就够了。
    saveToken('')
    session.me = null
    toast('密码已更新，请用新密码重新登录', 'ok')
    oldPwd.value = newPwd.value = confirmPwd.value = ''
    // 带 redirect：重新登录后回到本页，而不是被丢到总览再自己找回来。
    await router.replace({ name: 'login', query: { redirect: '/profile' } })
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
              <span class="mono">{{ me?.username }}</span>
              <span class="badge" :class="me?.role === 'admin' ? 'badge-live' : 'badge-off'">
                {{ me?.role === 'admin' ? '管理员' : '普通用户' }}
              </span>
              <span v-if="me?.status !== 'active'" class="badge badge-off">已禁用</span>
            </div>
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

        <!-- 余额与额度是两件不同的事，必须分行：额度是 token 计量、耗尽了
             换个 key/账号就行；余额是人民币、耗尽了要去**充值**。塌在一行
             会被读成「同一个东西的两种单位」。 -->
        <div class="row">
          <div class="row-main">
            <div class="row-title">我的余额</div>
            <div class="row-sub num">
              <!-- 「不限」与「0.00 元」含义相反：前者永远放行，后者会被 402
                   拒绝。判定走 balance_unlimited（见 fmtBalance）。 -->
              <template v-if="me?.balance_unlimited">
                不限
                <span class="dim">（不受余额限制）</span>
              </template>
              <template v-else>
                {{ fmtBalance(me?.balance_cents ?? 0, false) }}
              </template>
            </div>
            <div v-if="me && !me.balance_unlimited && me.balance_cents === 0" class="row-sub err-sub">
              余额已用尽，请求会被拒绝（402）。请联系管理员充值。
            </div>
            <div v-else-if="me && !me.balance_unlimited" class="row-sub">
              余额不足时请联系管理员充值。
            </div>
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
        <input id="op" v-model="oldPwd" type="password" class="input" autocomplete="current-password" />

        <label class="flabel" for="np">新密码</label>
        <input id="np" v-model="newPwd" type="password" class="input" autocomplete="new-password" />
        <div v-if="checkStrength(newPwd)" class="fhint warn">{{ checkStrength(newPwd) }}</div>
        <div v-else-if="newPwd" class="fhint ok">强度符合要求</div>

        <label class="flabel" for="cp">确认新密码</label>
        <input id="cp" v-model="confirmPwd" type="password" class="input" autocomplete="new-password" />

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
/* 控件与标签、提示用全局 .input / .flabel / .fhint（styles.css）；此页表单左对齐窄列，只保留宽度限制。 */
.input { max-width: 360px; }
/* 本页的「warn」是密码强度建议而非错误，覆盖全局的危险色回弱化色；
   「ok」也只有本页在用，一并留着。 */
.fhint.warn { color: var(--muted); }
.fhint.ok { color: var(--success); }
/* 余额用尽：这是**唯一**必须显眼的余额文案 —— 它对应后端真实会回的 402，
   而用户若事先不知道，就只能把「请求被拒」当成网关故障去排查。
   语气用危险色而不是弱化色，与 .fhint.warn（密码强度建议）正好相反。 */
.row-sub.err-sub { color: var(--danger); }
button { margin-top: 18px; }
</style>
