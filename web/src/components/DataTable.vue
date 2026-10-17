<script setup lang="ts">
// Keep native table markup in the slot: pages own column semantics and row
// actions; this component owns the shared scroll region and result states.
withDefaults(defineProps<{
  label: string
  loading?: boolean
  error?: string
  empty?: boolean
  hasData?: boolean
  emptyText?: string
  loadingText?: string
}>(), { emptyText: '暂无数据', loadingText: '加载中…' })
defineEmits<{ retry: [] }>()
</script>

<template>
  <section class="data-table" :aria-label="label" :aria-busy="loading || undefined">
    <div v-if="$slots.toolbar" class="data-toolbar"><slot name="toolbar" /></div>
    <div v-if="error" class="notice-warn" role="alert">
      {{ error }}<span v-if="hasData"> · 保留上次成功结果</span>
      <button class="btn btn-sm" :disabled="loading" @click="$emit('retry')">重试</button>
    </div>
    <div v-if="loading && !hasData" class="loading" role="status">{{ loadingText }}</div>
    <div v-else-if="empty && !error" class="empty"><slot name="empty">{{ emptyText }}</slot></div>
    <template v-else-if="!error || hasData">
      <div v-if="loading" class="table-refresh" role="status">正在刷新…</div>
      <div class="tbl-wrap" role="region" :aria-label="label + '，可横向滚动'" tabindex="0"><slot /></div>
      <div v-if="$slots.footer" class="data-table-footer"><slot name="footer" /></div>
    </template>
  </section>
</template>

<style scoped>
.data-table { min-width: 0; }
.data-toolbar { margin-bottom: 16px; }
.tbl-wrap { max-width: 100%; border-radius: inherit; }
.tbl-wrap:focus-visible { outline: 2px solid var(--accent-link); outline-offset: -2px; }
.table-refresh { padding: 8px 16px; font-size: 12px; color: var(--muted); }
.data-table-footer { border-top: 1px solid var(--border); }
</style>
