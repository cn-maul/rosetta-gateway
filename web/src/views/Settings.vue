<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { api, saveToken } from '../api'
import { toast } from '../ui'
import AppModal from '../components/AppModal.vue'

const loading = ref(true)
const saving = ref(false)
const form = reactive({
  default_context_window: 8192,
  default_max_output_tokens: 4096,
  // 运行时全局默认（超时 + 故障转移策略）。原先散落在每条路由上，现统一在此配置。
  upstream_timeout_ms: 120000,
  stream_idle_timeout_ms: 60000,
  stream_first_token_timeout_ms: 30000,
  failover_max_targets: 3,
  failover_failure_threshold: 3,
})

// 密码设置相关
const passwordForm = reactive({
  open: false,
  currentPassword: '',
  newPassword: '',
  confirmPassword: '',
})
const hasPassword = ref(false)
// 当前凭据来源："password_file" | "config_token" | "none" | "locked"。
// 决定「当前密码」框里该填什么：设过密码填密码，否则填 config 的 admin_token。
const authSource = ref('')
// 提交锁：防双击并发两次 password/set（两次写盘 + 两次 rename，没有意义且徒增竞态）。
const changing = ref(false)

async function load() {
  loading.value = true
  try {
    const s = await api.settings()
    form.default_context_window = s.default_context_window
    form.default_max_output_tokens = s.default_max_output_tokens
    form.upstream_timeout_ms = s.upstream_timeout_ms
    form.stream_idle_timeout_ms = s.stream_idle_timeout_ms
    form.stream_first_token_timeout_ms = s.stream_first_token_timeout_ms
    form.failover_max_targets = s.failover_max_targets
    form.failover_failure_threshold = s.failover_failure_threshold
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('加载设置失败：' + (e as Error).message, 'err')
  } finally {
    loading.value = false
  }
  await loadPasswordStatus()
}

// 凭据状态是**独立**信息源（/password/check 免鉴权），绝不能和 settings 共用 try。
// 共用时 settings 一旦失败就会连坐：界面显示「尚未设置密码」并藏起「当前密码」输入框，
// 于是用户以为在改密码，实际提交的是「首次设置」请求。
async function loadPasswordStatus() {
  try {
    const res = await fetch('/admin/api/password/check')
    if (!res.ok) return
    const data = await res.json()
    hasPassword.value = !!data.has_password
    authSource.value = data.source ?? ''
  } catch (e) {
    console.error('检查密码状态失败:', e)
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

  // 运行时默认同样必须是 >= 1 的整数：超时值会直接喂给看门狗与 context.WithTimeout，
  // 0/负数会变成「立即超时」，把全部转发打挂（后端也有同一份硬校验）。
  const nums: [string, number][] = [
    ['非流式超时', Number(form.upstream_timeout_ms)],
    ['流式空闲超时', Number(form.stream_idle_timeout_ms)],
    ['流式首字超时', Number(form.stream_first_token_timeout_ms)],
  ]
  for (const [label, v] of nums) {
    if (!Number.isFinite(v) || v < 1) {
      toast(label + '必须为 >= 1 的毫秒数', 'err')
      return
    }
  }
  const maxTargets = Number(form.failover_max_targets)
  const threshold = Number(form.failover_failure_threshold)
  if (!Number.isFinite(maxTargets) || maxTargets < 1) {
    toast('最多尝试目标数必须 >= 1', 'err')
    return
  }
  if (!Number.isFinite(threshold) || threshold < 1) {
    toast('失败熔断阈值必须 >= 1', 'err')
    return
  }

  saving.value = true
  try {
    // saveSettings 走 mutate()：保存后自动 reload，转发路径即时用上新值（无需重启）。
    await api.saveSettings({
      default_context_window: ctx,
      default_max_output_tokens: out,
      upstream_timeout_ms: Number(form.upstream_timeout_ms),
      stream_idle_timeout_ms: Number(form.stream_idle_timeout_ms),
      stream_first_token_timeout_ms: Number(form.stream_first_token_timeout_ms),
      failover_max_targets: maxTargets,
      failover_failure_threshold: threshold,
    })
    toast('设置已保存并即时生效')
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
  if (changing.value) return
  if (passwordForm.newPassword.length < 6) {
    toast('新密码长度至少为6位', 'err')
    return
  }
  if (passwordForm.newPassword !== passwordForm.confirmPassword) {
    toast('两次输入的新密码不一致', 'err')
    return
  }
  // 已有凭据时 password/set 必须带旧凭据才放行。旧实现只在「填了当前密码」时才加
  // Authorization 头，留空就直接发出去 —— 拿回一个 401，用户看到的是「设置密码失败」，
  // 完全猜不到是「没填当前密码」。这里当场拦住，并说清原因。
  if (hasPassword.value && !passwordForm.currentPassword) {
    toast('请先填写当前密码', 'err')
    return
  }

  changing.value = true
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

    // 网关错误时 body 应是 JSON，但代理层 502/504 之类不是 —— 直接 res.json() 会抛
    // SyntaxError，被下面的 catch 裹成一句和真实原因无关的提示。
    const data = await res.json().catch(() => null)
    if (res.ok) {
      toast(data?.message || '密码已更新')
      passwordForm.open = false
      hasPassword.value = true
      // 旧凭据即刻失效（校验优先使用新密码），必须当场换掉本地令牌，
      // 否则下一次请求就 401 —— 表现为「改完密码反而被锁在外面」。
      saveToken(passwordForm.newPassword)
    } else {
      toast(data?.error?.message || `设置密码失败（HTTP ${res.status}）`, 'err')
    }
  } catch (e) {
    toast('设置密码失败：' + (e as Error).message, 'err')
  } finally {
    changing.value = false
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
        <h2 class="section-h span2">模型容量默认</h2>
        <div class="field span2">
          <label>默认上下文窗口（tokens）</label>
          <input v-model.number="form.default_context_window" class="input num" type="number" min="1" step="1" />
          <span class="tip">添加模型时若上游未暴露 context_length，则用此值</span>
        </div>
        <div class="field span2">
          <label>默认最大输出（tokens）</label>
          <input v-model.number="form.default_max_output_tokens" class="input num" type="number" min="1" step="1" />
          <span class="tip">添加模型时若上游未暴露 max_completion_tokens，则用此值</span>
        </div>

        <h2 class="section-h span2">超时（毫秒）</h2>
        <div class="field span2">
          <label>非流式整体超时</label>
          <input v-model.number="form.upstream_timeout_ms" class="input num" type="number" min="1" step="1" />
          <span class="tip">一次非流式请求最多等多久；超时后按可转移错误处理（有机会切换下一个上游）</span>
        </div>
        <div class="field span2">
          <label>流式首字超时（TTFT）</label>
          <input v-model.number="form.stream_first_token_timeout_ms" class="input num" type="number" min="1" step="1" />
          <span class="tip">首字迟迟不来即掐流并切换下一个上游；仅在尚未写出任何字节时生效</span>
        </div>
        <div class="field span2">
          <label>流式空闲超时</label>
          <input v-model.number="form.stream_idle_timeout_ms" class="input num" type="number" min="1" step="1" />
          <span class="tip">已开始出字后，多久没有任何新事件即判定卡死（按截断收尾）</span>
        </div>

        <h2 class="section-h span2">自动故障转移</h2>
        <div class="field span2">
          <label>最多尝试目标数</label>
          <input v-model.number="form.failover_max_targets" class="input num" type="number" min="1" step="1" />
          <span class="tip">一次请求最多打某条路由链上的几个目标。是否启用按路由单独开关（路由页）</span>
        </div>
        <div class="field span2">
          <label>失败熔断阈值</label>
          <input v-model.number="form.failover_failure_threshold" class="input num" type="number" min="1" step="1" />
          <span class="tip">某目标连续失败几次后临时摘除（60 秒冷却），期间请求自动让位给链上下一个目标</span>
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
            <template v-if="!hasPassword">尚未设置密码（任何人都能打开后台）</template>
            <template v-else-if="authSource === 'config_token'">
              当前使用 <code>config.json</code> 的 <code>admin_token</code> 登录
            </template>
            <template v-else-if="authSource === 'locked'">凭据文件已损坏，后台处于锁定态</template>
            <template v-else>已设置密码（存放在 admin_auth.json）</template>
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
            <span class="tip">
              <template v-if="authSource === 'config_token'">
                当前凭据来自配置文件，这里请填 <code>config.json</code> 的 <code>admin_token</code>（或 ADMIN_TOKEN 环境变量的值）。
                设置新密码后该令牌立即失效。
              </template>
              <template v-else>填写你之前设置的管理密码。</template>
            </span>
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
          <button type="submit" class="btn btn-primary" :disabled="changing">{{ changing ? '保存中…' : '确定' }}</button>
        </div>
      </form>
    </AppModal>
  </main>
</template>

<style scoped>
/* 分区小标题：把一个长表单切成「模型容量 / 超时 / 故障转移」三块 */
.section-h {
  font-size: 13px;
  font-weight: 650;
  color: var(--text-2);
  margin: 6px 0 -2px;
  padding-bottom: 6px;
  border-bottom: 1px solid var(--hairline);
}
</style>
