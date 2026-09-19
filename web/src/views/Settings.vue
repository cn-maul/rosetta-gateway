<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import { toast } from '../ui'

const loading = ref(true)
const saving = ref(false)
const form = reactive({ default_context_window: 8192, default_max_output_tokens: 4096 })

async function load() {
  loading.value = true
  try {
    const s = await api.settings()
    form.default_context_window = s.default_context_window
    form.default_max_output_tokens = s.default_max_output_tokens
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
  </main>
</template>
