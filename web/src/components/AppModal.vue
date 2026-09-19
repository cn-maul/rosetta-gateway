<script setup lang="ts">
defineProps<{
  open: boolean
  title: string
  maxWidth?: string
  dismissable?: boolean
}>()

const emit = defineEmits<{ close: [] }>()
</script>

<template>
  <Teleport to="body">
    <div v-if="open" class="scrim" @click.self="dismissable !== false && emit('close')">
      <div class="sheet" :style="{ maxWidth: maxWidth ?? '560px' }" role="dialog" aria-modal="true">
        <div class="sheet-title">{{ title }}</div>
        <slot />
      </div>
    </div>
  </Teleport>
</template>
