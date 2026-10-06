<script setup lang="ts">
/**
 * 模型白名单选择器（多用户改造 P1）。
 *
 * 抽成组件而不是各处内联，是因为它承载一条**反直觉但关键**的语义：
 * **一个都不选 = 不限制**（不是「什么都看不到」）。这句话只该有一个地方说，
 * 复制到第二个界面时很容易被顺手改成「拒绝全部」，那时用户的直觉和系统的
 * 行为就反了，而且没有任何报错。
 *
 * 选项由调用方传入（`/admin/api/model-names`，已按身份收窄），**不给自由输入**：
 * 白名单是精确匹配，拼错一个字符的后果是「这个模型谁都看不到」而界面毫无异常。
 */
const props = defineProps<{
  modelValue: string[]
  options: string[]
  /** 清单本身已被上游收窄（如普通用户只拿到本组内的模型）时置 true，
   *  用来提示「你看到的不是全部模型」。 */
  restricted?: boolean
}>()

const emit = defineEmits<{ (e: 'update:modelValue', v: string[]): void }>()

function toggle(name: string) {
  const next = props.modelValue.includes(name)
    ? props.modelValue.filter((m) => m !== name)
    : [...props.modelValue, name]
  emit('update:modelValue', next)
}

/** 全选：按 options 的顺序给出确定性结果，避免出现「顺序取决于勾选顺序」的集合。 */
function selectAll() {
  emit('update:modelValue', [...props.options].sort())
}

function clearAll() {
  emit('update:modelValue', [])
}
</script>

<template>
  <div class="mp">
    <div class="mp-head">
      <span class="mp-count">
        <template v-if="modelValue.length === 0">未选 —— 不限制（可见全部模型）</template>
        <template v-else>已选 {{ modelValue.length }} / {{ options.length }}</template>
      </span>
      <span class="mp-btns">
        <button type="button" class="btn btn-sm btn-ghost" :disabled="options.length === 0" @click="selectAll">
          全选
        </button>
        <button type="button" class="btn btn-sm btn-ghost" :disabled="modelValue.length === 0" @click="clearAll">
          清空
        </button>
      </span>
    </div>

    <div v-if="options.length === 0" class="mp-empty">
      当前没有可用的公开模型。请先到「上游与模型 / 路由」里建好路由。
    </div>
    <div v-else class="mp-list">
      <label v-for="m in options" :key="m" class="mp-item">
        <input type="checkbox" :checked="modelValue.includes(m)" @change="toggle(m)" />
        <span class="mono">{{ m }}</span>
      </label>
    </div>

    <div v-if="restricted" class="tip">
      这份清单已被你的分组收窄过 —— 你最多只能用到这些模型。
    </div>
    <div class="tip">一个都不选 = 不限制；与分组白名单取交集，两者都限制时更严的生效。</div>
  </div>
</template>

<style scoped>
.mp {
  border: 1px solid var(--border);
  border-radius: var(--r-thumb);
  padding: 8px;
}
.mp-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  margin-bottom: 6px;
}
.mp-count { font-size: 12px; color: var(--muted); }
.mp-btns { display: flex; gap: 6px; }
.mp-list {
  max-height: 180px;
  overflow-y: auto;
}
.mp-item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 5px 6px;
  border-radius: var(--r-thumb);
  font-size: 13px;
  cursor: pointer;
}
.mp-item:hover { background: var(--muted); }
.mp-empty { font-size: 12px; color: var(--muted); padding: 10px 4px; }
</style>
