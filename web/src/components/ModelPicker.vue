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
import { computed, ref } from 'vue'

const props = defineProps<{
  modelValue: string[]
  options: string[]
  /** 清单本身已被上游收窄（如普通用户只拿到本组内的模型）时置 true，
   *  用来提示「你看到的不是全部模型」。 */
  restricted?: boolean
}>()

const emit = defineEmits<{ (e: 'update:modelValue', v: string[]): void }>()

/**
 * 选项的**确定性顺序**。
 *
 * 与后端同源（模型名由后端按字典序给出），但这里再排一次是有意的：
 * 组件的语义是「集合」，而界面是「列表」—— 同一份集合每次渲染若顺序不同，
 * 用户勾选一项后整列会跳位，下一次点击就点到了别的模型上。
 * 排序让「第 3 项」始终指向同一个模型。
 */
const sortedOptions = computed(() => [...props.options].sort())

/**
 * 搜索：只做**过滤**，绝不做「匹配就选中」。
 *
 * 过滤是纯展示的，关掉搜索框即恢复全量，选中的集合不受影响；
 * 一旦让搜索词去改 modelValue，「搜一下看看」就变成了一次不可逆的写操作。
 */
const query = ref('')
/** 超过这个数量才显示搜索框：几个模型时它是噪音，几十个时才是刚需。 */
const SEARCH_THRESHOLD = 12

const showSearch = computed(() => sortedOptions.value.length > SEARCH_THRESHOLD)

/** 过滤后的清单。大小写不敏感：模型名里有大量 `gpt-4o` / `GPT-4O` 混写。 */
const visible = computed(() => {
  const q = query.value.trim().toLowerCase()
  if (!q) return sortedOptions.value
  return sortedOptions.value.filter((m) => m.toLowerCase().includes(q))
})

/** 已选集合。每个勾选框都要查一次，用 Set 而不是对每项 includes 一遍。 */
const picked = computed(() => new Set(props.modelValue))

function toggle(name: string) {
  const next = picked.value.has(name)
    ? props.modelValue.filter((m) => m !== name)
    : // 追加到末尾而不是就地插入：modelValue 是集合，顺序无语义，
      // 保持「按勾选顺序追加」的既有行为，交给后端/调用方按需排序。
      [...props.modelValue, name]
  emit('update:modelValue', next)
}

/**
 * 全选：按 options 的顺序给出确定性结果，避免出现「顺序取决于勾选顺序」的集合。
 *
 * 有搜索词时只全选**当前可见**的那些（2026-10-10 修复的 P2）。
 *
 * 原实现无条件发整个 sortedOptions，于是「打了搜索词 → 点全选」这个完全
 * 自然的操作，实际效果是勾中**所有**模型。对「配置模型」这个用例（给
 * 分组配白名单）后果是反的：用户是在**收紧**范围，而「全选所有」恰好
 * 等于取消这个组的限制（一个都不选与全都选，在后端都是「不限制」），
 * 而界面上看不出任何差别。
 *
 * 根因是按钮与搜索框并排，位置暗示了「作用于当前看到的这些」。
 */
function selectAll() {
  const q = query.value.trim()
  emit('update:modelValue', q ? [...visible.value] : [...sortedOptions.value])
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
        <button
          type="button"
          class="btn btn-sm btn-ghost"
          :disabled="options.length === 0 || (query.trim() !== '' && visible.length === 0)"
          @click="selectAll"
        >
          {{ query.trim() ? '全选当前' : '全选' }}
        </button>
        <button type="button" class="btn btn-sm btn-ghost" :disabled="modelValue.length === 0" @click="clearAll">
          清空
        </button>
      </span>
    </div>

    <div v-if="options.length === 0" class="mp-empty">
      当前没有可用的公开模型。请先到「上游与模型 / 路由」里建好路由。
    </div>
    <template v-else>
      <!-- 搜索只在大清单时出现：模型名动辄几十上百个，逐个扫一遍靠肉眼找
           是不现实的，但几个模型时这个输入框只是占地方。 -->
      <input
        v-if="showSearch"
        v-model="query"
        class="input mp-search"
        type="search"
        placeholder="搜索模型名"
        aria-label="搜索模型名"
      />
      <div class="mp-list">
        <label v-for="m in visible" :key="m" class="mp-item">
          <input type="checkbox" :checked="picked.has(m)" @change="toggle(m)" />
          <span class="mono">{{ m }}</span>
        </label>
        <div v-if="visible.length === 0" class="mp-empty">
          没有匹配「{{ query }}」的模型。
        </div>
      </div>
      <div v-if="showSearch && query" class="mp-filtered">
        已按「{{ query }}」过滤，显示 {{ visible.length }} / {{ options.length }} 个。
        勾选状态不受影响。
      </div>
    </template>

    <div v-if="restricted" class="mp-tip">
      这份清单已被你的分组收窄过 —— 你最多只能用到这些模型。
    </div>
    <!-- 关键 tip 单独成类：它是「一个都不选 = 不限制」这条反直觉语义的
         唯一出处，用弱化色（--text-3）与正文区分开即可 ——
         一旦染成 danger/warn，读起来就成了「这样做有风险」，语义正好反了。
         不复用全局 .field .tip：那条规则只在 .field 内生效，而本组件
         也被用在非 .field 容器里（Groups 的弹窗），换个位置样式就丢了。 -->
    <div class="mp-tip mp-key-tip">一个都不选 = 不限制；与分组白名单取交集，两者都限制时更严的生效。</div>
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
/* 搜索框：全局 .input 的高度对这个位置偏高，压到 30px 让它像「列表的筛子」
   而不是又一个表单字段。 */
.mp-search {
  height: 30px;
  margin-bottom: 6px;
  font-size: 12px;
}
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
/* 悬浮底色用 --hover（= --surface-secondary）而不是 --muted：
   --muted 是**文字**色，拿它当背景会把这一行压得比卡片还暗，
   深色模式下尤其明显（曾表现为「鼠标一放上去整行变黑块」）。 */
.mp-item:hover { background: var(--hover); }
.mp-empty { font-size: 12px; color: var(--muted); padding: 10px 4px; }
/* 关键 tip：弱化色而非危险色 —— 它是一条语义说明，不是警告。 */
.mp-key-tip { color: var(--text-3); }
/* 过滤提示：只在搜索生效时出现，说明「这只是过滤、没改变选中」。 */
.mp-filtered { margin-top: 4px; font-size: 11.5px; color: var(--text-4); }
/* 本组件的提示自己定义，不依赖全局 .field .tip —— 那条规则只在 .field
   容器内生效，而本组件也被用在 Groups 的弹窗里（非 .field），
   换个位置样式就会整个消失。 */
.mp-tip { font-size: 11.5px; color: var(--text-3); margin-top: 6px; }
</style>
