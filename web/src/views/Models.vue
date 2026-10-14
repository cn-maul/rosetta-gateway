<script setup lang="ts">
/**
 * 可用模型（全体可见的只读清单）。
 *
 * # 这页和 ModelPicker 讲的不是同一件事，别把两边的语义混过来
 *
 * ModelPicker 是**写**白名单的地方，它承载一条反直觉但关键的规则：
 * 「一个都不选 = 不限制」。所以它在空清单时说「未限制」，是**故意**的 ——
 * 那是配置态下「没勾」的正确读法。
 *
 * 而本页回答的是另一个问题：**我实际能用哪些模型**。这里的清单是接口按身份
 * 收窄后的结果（管理员=全部路由名，普通用户=所属组白名单 ∩ 全部路由名，
 * 见 GroupHandler.ModelNames），不是任何人的待保存配置。于是：
 *
 *   - 空清单**绝不能**写成「不限制」。空的可能只有两种：路由一个都没配，
 *     或组白名单与路由无交集。后者返回全集就会构成**静默放宽**（后端测试
 *     TestModelNames_NormalUserScopedByGroup 专门钉住了这一点），界面若把空
 *     说成「不限制」等于把那个失败方向又讲了一遍，用户会以为自己是全权限，
 *     实则一把模型都调不通。空 = 你一个模型都调不了，处置办法是找管理员。
 *   - 同样不能给勾选控件。这里没有待保存的状态，给了复选框只会让人以为
 *     勾一下就生效了；能改白名单的是「访问密钥」页，管理员改「分组」页。
 *
 * # restricted 是必答项
 *
 * 后端只在清单**真的被组白名单收窄过**时置 true。它回答的是「你看到的
 * 是不是全部」—— 收窄过就得说，否则用户会拿这一页当全集去排查
 * 「为什么某模型调不通」。
 */
import { computed, onMounted, ref } from 'vue'
import { api, ApiFail, isAdmin } from '../api'
import type { ModelPrice } from '../types'
import { fmtMoney } from '../fmt'
import { toast } from '../ui'

const models = ref<string[]>([])
/** 清单是否已被所属分组的白名单收窄。false = 这是你此刻能看到的全部。 */
const restricted = ref(false)
const loading = ref(false)
const err = ref('')

/**
 * 单价表：公开名 → 价格。
 *
 * 后端给的是与 models **平行**的数组（models[i] 对应 prices[i]），这里转成
 * Map：排序 `sorted` 时名字与价格会一起动，用下标绑定就必须在每次排序后
 * 重新 zip，一次疏忽就是「A 模型显示着 B 模型的价格」—— 而价格只差几倍时
 * 没人会发现。转成 Map 后查找与排序彻底解耦。
 *
 * 找不到名字说明后端漏发了这一项（契约由后端测试钉住），此时 priceOf
 * 返回 undefined，界面显示「未配置」—— 那是个**可见的异常**，
 * 好过拿一个猜出来的价格让用户按错的数做预算。
 */
const priceMap = ref(new Map<string, ModelPrice>())

/**
 * 展示顺序 = 字典序。
 *
 * 与 ModelPicker 同样的理由：同一份集合若每次渲染顺序不同，用户的心智模型
 * 「第 3 项是哪个模型」就会漂移。后端本来也按字典序给出，这里再排一次是为了
 * 不依赖那个实现细节（顺带滤掉任何意外的空串）。
 */
const sorted = computed(() => [...models.value].filter(Boolean).sort())

/** 管理员不受分组白名单约束（后端 ModelNames 里的同一判定）。 */
const isAdminUser = computed(() => isAdmin())

function priceOf(name: string): ModelPrice | undefined {
  return priceMap.value.get(name)
}

/**
 * 单价单元格：**0 显示「未配置」而不是 0.00**。
 *
 * 与 Settings.vue 的 priceCell 同一口径、同一理由：没填单价时实际计费按 0
 * （见 store.priceUsage），但「算过了，就是免费」与「还没人填」是两件
 * 不同的事 —— 显示 0.00 会让用户以为这一档确认免费，据此做预算就错了。
 * 「未配置」配弱化色，是为了让它读起来像一条**说明**而不是一个价格。
 */
function priceCell(v: number | undefined): string {
  if (v === undefined || !(v > 0)) return '未配置'
  return fmtMoney(v)
}

async function load() {
  loading.value = true
  err.value = ''
  try {
    const r = await api.modelNames()
    models.value = r.models
    restricted.value = r.restricted
    priceMap.value = new Map(r.prices.map((p) => [p.name, p]))
  } catch (e) {
    if (e instanceof ApiFail && e.status === 401) return
    // 失败必须留下可见错误态：只弹 toast 的话，页面会渲染成空清单，
    // 而空清单在这页有明确含义（「你一个模型都调不了」）——
    // 那会把一次网关故障呈现成一次权限收紧，用户会去联系管理员，
    // 而真正该修的是网关。与 Users/Keys/History 的 v-else-if="err" 同口径。
    err.value = '加载可用模型失败：' + (e instanceof Error ? e.message : String(e))
    toast('加载可用模型失败：' + (e instanceof Error ? e.message : String(e)), 'err')
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>可用模型</h1>
        <div class="sub">你能调用的模型名清单（只读）</div>
      </div>
      <div class="head-actions">
        <button class="btn" :disabled="loading" @click="load">刷新</button>
      </div>
    </div>

    <div class="panel">
      <div class="panel-title">
        <span>模型清单</span>
        <span class="hint">
          <template v-if="sorted.length">{{ sorted.length }} 个</template>
        </span>
      </div>

      <div v-if="loading && sorted.length === 0" class="loading">加载中…</div>

      <!--加载失败**必须**与「确实没有可用模型」可区分：后者有明确处置
           办法（联系管理员配白名单），把请求失败呈现成同一句话，会让用户
           在网关故障时去催管理员 —— 与 Users/Keys/History 同口径。 -->
      <div v-else-if="err" class="empty">
        <div class="big">⚠</div>
        {{ err }}
        <div class="err-retry">
          <button class="btn btn-sm" :disabled="loading" @click="load">重试</button>
        </div>
      </div>

      <!-- 空 = 你一把都调不了。注意这里**不写**「不限制」：那是 ModelPicker
           「一个都不选」的规则，属于配置态，与本页无关（见文件头注释）。 -->
      <div v-else-if="sorted.length === 0" class="empty">
        <div class="big">∅</div>
        <!--
          空清单有两种成因（2026-10-11 前端审计 M2）：管理员一个路由都没配，
          或分组白名单与现有路由无交集。原文案只写了后者，未分组用户
          （未分组 = 不受白名单限制）会拿到一条指向错误方向的建议 ——
          去找管理员配白名单，而真正的原因是根本没配路由。
        -->
        目前没有可调用的模型。可能的原因：管理员尚未配置路由，或你的分组白名单与现有路由没有交集 —— 请联系管理员确认。
      </div>

      <template v-else>
        <!-- restricted 显式提示，且必须排在清单**上方**：它是读这份清单的
             前置条件（这份不是全集），写在下面会被当成脚注。 -->
        <p v-if="restricted" class="notice-warn" role="status">
          这份清单已被你的分组收窄过 —— 下面的模型就是你最多能调用的，其余模型即使知道名字也会被拒绝。
          需要更多模型请联系管理员调整分组白名单。
        </p>

<!-- 卡片式清单（2026-10-14）：每个模型一张 HeroUI 风格的 card。

           **只读语义不变**：不放复选框（没有待保存的状态），也不给自由输入
           （白名单是精确匹配）。换的只是呈现方式 —— 表格适合「逐行比对数字」，
           而这份清单的实际用法是「确认我有哪些模型可用、单价大概多少」，
           卡片在窄屏与模型数多时都更好扫。

           单价单位**写进每张卡**而不是只在表头：表头式的单位在卡片布局里
           没有承载位置，而价格最容易被误读的就是单位（见上方 th-unit 注释）。 -->
        <div class="model-grid">
          <article v-for="m in sorted" :key="m" class="card model-card">
            <div class="card__header">
              <div class="card__title model-name mono">{{ m }}</div>
              <!-- 描述位放「单价是否已配齐」：三档全未配置时给一句提示，
                   避免一整片「未配置」被读成「这些模型都免费」。 -->
              <div class="card__description model-sub">
                <template v-if="priceOf(m)">单价（元 / 百万 tokens）</template>
                <template v-else>未配置单价 · 按 0 元计</template>
              </div>
            </div>

            <div class="card__content model-prices">
              <div class="price-row">
                <span class="price-lab">输入</span>
                <span class="price-val mono" :class="{ unset: (priceOf(m)?.price_input ?? 0) <= 0 }">
                  {{ priceCell(priceOf(m)?.price_input) }}
                </span>
              </div>
              <div class="price-row">
                <span class="price-lab">输出</span>
                <span class="price-val mono" :class="{ unset: (priceOf(m)?.price_output ?? 0) <= 0 }">
                  {{ priceCell(priceOf(m)?.price_output) }}
                </span>
              </div>
              <!-- 「其中:缓存命中」这个前缀不能省：三档并排会让人以为
                   「三项相加 = 总输入成本」，而缓存命中其实是**输入的一部分**
                   （两个上游协议里 cached 都 ⊆ input），它是命中时的**折扣价**，
                   不是新增的一项。 -->
              <div class="price-row">
                <span class="price-lab">其中:缓存命中</span>
                <span class="price-val mono" :class="{ unset: (priceOf(m)?.price_cache_hit ?? 0) <= 0 }">
                  {{ priceCell(priceOf(m)?.price_cache_hit) }}
                </span>
              </div>
            </div>
          </article>
        </div>
      </template>
    </div>

    <div class="panel">
      <div class="panel-title"><span>关于这份清单</span></div>
      <ul class="notes">
        <li>
          清单由网关按你的身份自动算出，<b>本页只读</b>：能改的地方是「访问密钥」
          页的模型白名单（只能收紧）和管理员的「分组」页（按组收紧）。
        </li>
        <li v-if="isAdminUser">
          你是管理员，看到的是全部已配置的模型名，不受分组白名单约束。
        </li>
        <li v-else-if="restricted">
          你被分在一个收窄过的分组里，清单已被该分组的白名单过滤。
        </li>
        <li v-else>
          你不在任何受限制的分组里，因此这份清单就是全部已配置的模型名。
        </li>
        <li>
          密钥上如果单独设了白名单，最终范围取<b>两者取交集</b>，只会比这里更严。
        </li>
        <li>
          单价单位<b>元 / 百万 tokens</b>：输入按「未命中缓存」计，缓存命中另有
          更低的一档（留空则回退到输入价）。
        </li>
        <li>
          标「<b>未配置</b>」表示这一档还没填单价，<b>不等于免费</b> —— 未配置时
          该项按 0 元计入，实际花费请以「调用历史」里的记录为准。
        </li>
        <li class="dim">
          看不到某个模型不一定是故障：路由被停用、或模型名拼错，都会让它不出现在这份清单里。
        </li>
      </ul>
    </div>
  </main>
</template>

<style scoped>
/* ---------- 卡片式模型清单（2026-10-14） ----------
   从表格改成 HeroUI card 网格。复用全局 .card / .card__* 结构
   （styles.css:787 起，与 CredentialPanel 同一套 HeroUI 移植）。 */

/* auto-fill + minmax：卡片数随宽度自动换行，不写死列数 —— 模型从 3 个到
   80 个都能用同一套布局（写死 3 列在窄屏会把卡片挤成一条）。
   minmax 的下限 232px 是「模型名 + 三行价格」不折行的最小可用宽度。 */
.model-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(232px, 1fr));
  gap: 12px;
}
.model-card {
  gap: 10px;
  padding: 14px 16px;
  /* 描边 + 投影双层：卡片**密集并排**时,单张看投影够用,但几十张白卡
     挤在一起,4%~6% 的淡投影在相邻卡片之间互相干扰,整片看边界就糊了。
     描边是这里必需的 —— 表格用 separate + 分隔线分层,卡片没有分隔线可借,
     只能自己加。颜色走 --border 变量,亮/暗自动跟随。 */
  border: 1px solid var(--border);
}
/* 模型名可能很长（provider/model 形式），允许在连字符处折行而不是撑破卡片 */
.model-name {
  font-family: var(--mono, ui-monospace, monospace);
  font-size: 13.5px;
  line-height: 1.4;
  overflow-wrap: anywhere;
}
.model-sub {
  font-size: 12px;
  margin-top: 2px;
}
.model-prices {
  gap: 3px;
}
.price-row {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 10px;
  font-size: 12.5px;
}
.price-lab {
  color: var(--text-3);
  white-space: nowrap;
}
.price-val {
  font-weight: 600;
  /* 数字右对齐在纵向列表里读起来更整齐（tabular 让小数点对齐） */
  font-variant-numeric: tabular-nums;
  text-align: right;
}
/* 「未配置」弱化：明确写「未配置」还不够 —— 它得读起来像一条**说明**
   而不是一个价格，否则用户还是会把它当成数字。与 Settings.vue 的 .unset
   同一套口径：价格在设置页、这里在只读清单，但说的是同一件事。 */
.price-val.unset {
  color: var(--text-4);
  font-weight: 400;
}

/* 价格表表头下的单位标注（2026-10-11 前端审计）。
   表头里塞单位是因为价格表最容易被误读 —— 「3.00」在不知道单位时
   可以被当成任何东西。放在表格下方的注释里等于没写。
   卡片化后单位写进 card__description（.model-sub），此样式已无用，
   保留是给「关于这份清单」等仍可能引用 th-unit 的位置兜底。 */
.th-unit {
  font-weight: 400;
  font-size: 11px;
  white-space: nowrap;
}

/* 提示条样式沿用 History.vue 的 .notice-warn 同款（该类写在它的 scoped
   样式里，本页用不了），这里只补一条本地定义：语义是「你看到的不是全集」，
   属提示而不是错误，用 warning-soft 而非 danger。 */
.notice-warn {
  margin: 0 0 14px;
  padding: 8px 10px;
  border-radius: var(--r-thumb);
  background: var(--warning-soft);
  color: var(--warning-soft-foreground);
  font-size: 12.5px;
  line-height: 1.55;
}
.err-retry {
  margin-top: 10px;
  display: flex;
  justify-content: center;
}
.notes {
  margin: 0;
  padding-left: 18px;
  font-size: 12.5px;
  line-height: 1.85;
  color: var(--text-2);
}
.notes b { color: var(--text); }
.notes .dim { color: var(--text-3); }
</style>
