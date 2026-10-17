<script setup lang="ts">
import AppModal from './AppModal.vue'
withDefaults(defineProps<{ open: boolean; title: string; maxWidth?: string }>(), { maxWidth: '680px' })
defineEmits<{ close: [] }>()
</script>

<template>
  <AppModal :open="open" :title="title" :max-width="maxWidth" placement="right" @close="$emit('close')">
    <slot />
    <template #footer>
      <div class="drawer-actions"><slot name="footer"><button class="btn btn-ghost" @click="$emit('close')">关闭</button></slot></div>
    </template>
  </AppModal>
</template>

<style scoped>
.drawer-actions { display: flex; justify-content: flex-end; flex-wrap: wrap; gap: 10px; }
</style>
