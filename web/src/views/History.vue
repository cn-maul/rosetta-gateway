<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api } from '../api'
import { toast } from '../ui'
import { fmtTokens, fmtTimeMs } from '../fmt'
import type { UsageHistoryEntry } from '../types'

const loading = ref(true)
const rows = ref<UsageHistoryEntry[]>([])
const days = ref(7)

const ranges = [
  { label: '近 1 天', value: 1 },
  { label: '近 7 天', value: 7 },
  { label: '近 30 天', value: 30 },
]

async function load() {
  loading.value = true
  try {
    rows.value = await api.usageHistory(days.value, 200)
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('加载调用历史失败：' + (e as Error).message, 'err')
  } finally {
    loading.value = false
  }
}

onMounted(load)
defineExpose({ load })
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>调用历史</h1>
        <div class="sub">最近的请求明细（最多 200 条，按时间倒序）</div>
      </div>
      <div class="head-actions">
        <select v-model.number="days" class="select" style="width: auto" @change="load">
          <option v-for="r in ranges" :key="r.value" :value="r.value">{{ r.label }}</option>
        </select>
        <button class="btn" :disabled="loading" @click="load">刷新</button>
      </div>
    </div>

    <div class="panel">
      <div v-if="loading && rows.length === 0" class="loading">加载中…</div>
      <div v-else-if="rows.length === 0" class="empty">
        <div class="big">⌗</div>
        该时间范围内暂无调用记录
      </div>
      <div v-else class="tbl-wrap">
        <table class="tbl">
          <thead>
            <tr>
              <th class="c-time">时间</th>
              <th>调用模型</th>
              <th>实际模型</th>
              <th>调用密钥</th>
              <th class="num-h">Tokens</th>
              <th class="c-st">状态</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="(h, i) in rows" :key="i">
              <td class="mono c-time">{{ fmtTimeMs(h.ts) }}</td>
              <td class="mono">{{ h.public_model || '—' }}</td>
              <td class="mono dim">{{ h.upstream_model || '—' }}</td>
              <td>{{ h.key_name || h.key_id || '—' }}</td>
              <td class="num-h">{{ fmtTokens(h.total_tokens) }}</td>
              <td class="c-st">
                <span class="badge" :class="h.status === 'ok' ? 'badge-live' : 'badge-off'">
                  {{ h.status === 'ok' ? '正常' : h.status }}
                </span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </main>
</template>

<style scoped>
.tbl-wrap {
  overflow-x: auto;
}
.tbl {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
}
.tbl th,
.tbl td {
  text-align: left;
  padding: 9px 12px;
  white-space: nowrap;
}
.tbl thead th {
  color: var(--text-3);
  font-weight: 500;
  font-size: 12px;
  border-bottom: 1px solid var(--hairline, rgba(0, 0, 0, 0.07));
}
.tbl tbody tr {
  border-bottom: 1px solid var(--hairline, rgba(0, 0, 0, 0.05));
}
.tbl tbody tr:last-child {
  border-bottom: none;
}
.tbl tbody tr:hover {
  background: var(--hover, #fbfbfd);
}
.c-time {
  color: var(--text-2);
}
.c-st {
  width: 72px;
}
.num-h {
  text-align: right;
}
.dim {
  color: var(--text-3);
}
</style>
