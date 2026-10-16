<script setup lang="ts">
/**
 * 可用模型（全体可见的只读清单）。每个模型一张卡片，卡内同时显示
 * **单价**与**连通率**。
 *
 * # 为什么把连通率并进这张卡（2026-10-14 页面合并）
 *
 * 合之前是两个页面：「可用模型」按**公开路由名**列价格，「连通率」按
 * **upstream_models.id** 画曲线。两者问的其实是同一个问题的两半 ——
 * 「我要用的这个模型，此刻多少钱、此刻通不通」。拆开看的代价是：判断一个
 * 故障该换档还是该降价，得先记下模型名、去另一页按 id 找、再回来对价格。
 * 现在一张卡给全，且**价格与连通率指向同一个上游模型**（后端 meta 与
 * prices 同取故障转移链的 position 0，见 group_handler.go）。
 *
 * # 连通率怎么对上号
 *
 * 探测数据按 upstream_models.id 组织，本页清单按公开名组织 —— 主键不同，
 * 靠 model-names 的 meta[i].upstream_model_id 映射。
 *
 * 映射**可能缺**，三种情况与各自该显示什么都必须区分开：
 *   - 该路由解析不出可用目标（provider/模型被禁用）→ 不画曲线，写明原因；
 *     画一条空图等于把「没配好」呈现成「探测不出问题」。
 *   - 探测端点按身份收窄后拿不到（交集为空、或未探测）→ 显示「未探测」。
 *   - 拿得到 → 画曲线。
 *
 * # 曲线仍然只在勾选的模型上画
 *
 * 一次画几十条曲线既没人看得懂，也让后端单查询变重（24 个上限）。所以默认
 * 只画**有探测记录的前 6 个**，其余模型的价格照常显示 —— 用户要看的通常是
 * 价格，不是曲线。
 *
 * # 这页和 ModelPicker 讲的不是同一件事
 *
 * ModelPicker 是**写**白名单的地方，它承载一条反直觉但关键的规则：
 * 「一个都不选 = 不限制」。本页回答的是另一个问题：**我实际能用哪些模型、
 * 多少钱、此刻通不通**。这里的清单是接口按身份收窄后的结果，不是任何人的
 * 待保存配置。
 *
 *   - 空清单**绝不能**写成「不限制」。空的可能只有两种：路由一个都没配，
 *     或组白名单与路由无交集。返回全集就会构成**静默放宽**，界面若把空说成
 *     「不限制」等于把那个失败方向又讲了一遍。
 *   - 同样不能给勾选控件。这里没有待保存的状态。
 *
 * # restricted 是必答项
 *
 * 后端只在清单**真的被组白名单收窄过**时置 true。它回答的是「你看到的
 * 是不是全部」—— 收窄过就得说。
 */
import { computed, onMounted, ref } from 'vue'
import { api, ApiFail, isAdmin } from '../api'
import type { ModelMeta, ModelPrice, ProbeHealth, ProbeTimelineBucket } from '../types'
import { fmtMoney } from '../fmt'
import { toast } from '../ui'
import AppModal from '../components/AppModal.vue'
import HeartbeatBar from '../components/HeartbeatBar.vue'

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
 * 没人会发现。
 */
const priceMap = ref(new Map<string, ModelPrice>())

/**
 * 映射表：公开名 → 上游模型行 id（探测数据的外键）。
 *
 * 同样转 Map 而不用下标，理由与 priceMap 一致 —— 而且这里错位的后果是
 * 「A 模型的健康曲线画在 B 模型卡片上」，比价格错位更难被察觉，因为两者的
 * 数字看起来都合理。
 */
const metaMap = ref(new Map<string, ModelMeta>())

// ---- 连通率 ----

const healths = ref<ProbeHealth[]>([])
/** 摘要按 upstream_models.id 组织；这里按公开名索引，卡片直接查。 */
const healthByUpstream = computed(() => {
  const m = new Map<string, ProbeHealth>()
  for (const h of healths.value) m.set(h.model_id, h)
  return m
})

/** 用户勾选要画曲线的公开模型名。空 = 一条都不画（界面给提示，不自动补）。 */
const drawn = ref<string[]>([])
const bucketsByUpstream = ref<Record<string, ProbeTimelineBucket[]>>({})
const days = ref(7)
const granularity = ref<'hour' | 'day'>('hour')
const timelineLoading = ref(false)
/** 探测数据是否失败。**与「探测不出问题」分开**：前者要提示，后者照常显示曲线。 */
const probeErr = ref('')
const probeLoading = ref(false)

const configOpen = ref(false)

/**
 * 展示顺序 = 字典序。
 *
 * 与 ModelPicker 同样的理由：同一份集合若每次渲染顺序不同，用户的心智模型
 * 「第 3 项是哪个模型」就会漂移。
 */
const sorted = computed(() => [...models.value].filter(Boolean).sort())

/** 管理员不受分组白名单约束（后端 ModelNames 里的同一判定）。 */
const isAdminUser = computed(() => isAdmin())

function priceOf(name: string): ModelPrice | undefined {
  return priceMap.value.get(name)
}

function upstreamIdOf(name: string): string {
  return metaMap.value.get(name)?.upstream_model_id ?? ''
}

function providerOf(name: string): string {
  return metaMap.value.get(name)?.provider_name ?? ''
}

/**
 * 单价单元格：**0 显示「未配置」而不是 0.00**。
 *
 * 与 Settings.vue 的 priceCell 同一口径、同一理由：没填单价时实际计费按 0，
 * 但「算过了，就是免费」与「还没人填」是两件不同的事。
 */
function priceCell(v: number | undefined): string {
  if (v === undefined || !(v > 0)) return '未配置'
  return fmtMoney(v)
}

/** 有探测记录的公开名（按清单顺序），用于默认勾选与「全选」。 */
const withProbe = computed(() =>
  sorted.value.filter((n) => {
    const id = upstreamIdOf(n)
    return id && healthByUpstream.value.get(id)?.window_count
  }),
)

/**
 * curveFor 取某个公开模型对应的桶数组（按上游模型 id 索引）。
 *
 * 返回空数组而不是 undefined：调用点是 v-for，空数组是合法的「没有桶」，
 * 走 buildCurve 渲染出「该时段没有探测记录」而不是崩在 .length 上。
 */
function curveFor(name: string): ProbeTimelineBucket[] {
  const id = upstreamIdOf(name)
  return id ? (bucketsByUpstream.value[id] ?? []) : []
}

/** 该模型这条曲线该画在哪个桶序列上：故障转移链的主目标。
 *
 *  与 priceOf / upstreamIdOf 同源（同取 Candidates[0]），三者在后端就已经
 *  对齐，这里只是把它们指向同一条曲线。 */
function drawnIds(): string[] {
  return drawn.value.map(upstreamIdOf).filter(Boolean)
}

async function loadTimeline() {
  const ids = drawnIds()
  if (ids.length === 0) {
    bucketsByUpstream.value = {}
    return
  }
  timelineLoading.value = true
  probeErr.value = ''
  try {
    const all = await api.probeTimeline(ids, days.value, granularity.value)
    const grouped: Record<string, ProbeTimelineBucket[]> = {}
    for (const b of all) (grouped[b.model_id] ??= []).push(b)
    bucketsByUpstream.value = grouped
  } catch (e) {
    // 失败必须**可见**：曲线区会显示成「没有记录」，而那与「真的没记录」
    // 含义完全相反 —— 用户会把一次网关故障读成模型停了。
    probeErr.value = '加载连通率失败：' + (e instanceof Error ? e.message : String(e))
  } finally {
    timelineLoading.value = false
  }
}

/** 默认画前 6 个有数据的模型：一次给一屏，不至于铺满整页。
 *
 *  上限 6 是有意的 —— 后端单查询上限是 24，而一屏 24 张曲线既看不清也
 *  用不上（同一供应商的模型曲线往往同形）。 */
function defaultDrawn(): void {
  drawn.value = withProbe.value.slice(0, 6)
}

function toggleDrawn(name: string): void {
  drawn.value = drawn.value.includes(name)
    ? drawn.value.filter((x) => x !== name)
    : [...drawn.value, name]
  loadTimeline()
}

function drawAll(): void {
  drawn.value = [...withProbe.value]
  loadTimeline()
}

function clearDrawn(): void {
  drawn.value = []
  loadTimeline()
}

async function loadProbes() {
  probeLoading.value = true
  try {
    // 探测摘要可能失败（后端老版本没这个端点、或权限收窄）。这不该让整页
    // 进错误态 —— 价格仍然完全可用。所以失败只记 probeErr，卡片上的连通率
    // 一行显示原因，其余照常。
    healths.value = await api.probeModels()
    probeErr.value = ''
    if (drawn.value.length === 0) defaultDrawn()
    await loadTimeline()
  } catch (e) {
    if (e instanceof ApiFail && e.status === 401) return
    probeErr.value = '加载连通率失败：' + (e instanceof Error ? e.message : String(e))
  } finally {
    probeLoading.value = false
  }
}

async function load() {
  loading.value = true
  err.value = ''
  try {
    const r = await api.modelNames()
    models.value = r.models
    restricted.value = r.restricted
    priceMap.value = new Map(r.prices.map((p) => [p.name, p]))
    metaMap.value = new Map(r.meta.map((m) => [m.name, m]))
    // 清单变了就重算默认勾选：否则会拿着上一轮的模型名去查曲线。
    defaultDrawn()
    await loadProbes()
  } catch (e) {
    if (e instanceof ApiFail && e.status === 401) return
    // 失败必须留下可见错误态：只弹 toast 的话，页面会渲染成空清单，
    // 而空清单在这页有明确含义（「你一个模型都调不了」）。
    err.value = '加载可用模型失败：' + (e instanceof Error ? e.message : String(e))
    toast('加载可用模型失败：' + (e instanceof Error ? e.message : String(e)), 'err')
  } finally {
    loading.value = false
  }
}

onMounted(load)

// ---------- 连通率条形图 ----------
//
// 原来是纯 SVG 折线（buildCurve / pxFor / pyFor 一整套），2026-10-16 换成
// HeartbeatBar：折线把「停机 5 小时」画成一条直通 100% 的斜线，故障时段
// 在图上消失；条形图每个探测桶一根胶囊，坏的直接是红的。
//
// 跨度 = days × 24 小时（与数据面同源，只换渲染不改查询）。
// 两种粒度都是 24×days 小时，差别只在后端怎么聚合，所以这里不必分支。
const HB_HOURS = days.value * 24

/** healthOf 这个公开模型对应的上游模型的健康摘要（可能 undefined）。 */
function healthOf(name: string): ProbeHealth | undefined {
  const id = upstreamIdOf(name)
  return id ? healthByUpstream.value.get(id) : undefined
}

function healthClass(h: string): string {
  switch (h) {
    case 'healthy':
      return 'badge-live'
    case 'down':
      return 'badge-off'
    case 'degraded':
      return 'badge-warn'
    default:
      return 'badge-dim'
  }
}
function healthLabel(h: string): string {
  switch (h) {
    case 'healthy':
      return '正常'
    case 'down':
      return '不可用'
    case 'degraded':
      return '不稳定'
    default:
      return '未探测'
  }
}

/** 这条连通率该显示成什么 —— 三种状态必须可区分，不能都退化成「无数据」。
 *
 *  - 路由解析不出目标 → 「无可用上游」（配置问题，不是探测问题）
 *  - 探测请求失败 → 「读取失败」（网关问题）
 *  - 没勾选画这条 → 不画曲线
 */
type LineState = 'ok' | 'no-upstream' | 'probe-failed' | 'not-probed'
function lineState(name: string): LineState {
  if (!upstreamIdOf(name)) return 'no-upstream'
  if (probeErr.value) return 'probe-failed'
  if (!healthOf(name)?.window_count) return 'not-probed'
  return 'ok'
}

/**
 * lineNote 这条连通率的一句话说明。
 *
 * 刻意给四种状态**四种不同的话**，且每种都指明「该找谁 / 该修哪里」：
 * 把它们统一成「暂无数据」会让管理员无法判断这是配置问题、网关问题，
 * 还是真的刚重启还没探测。
 */
function lineNote(name: string): string {
  switch (lineState(name)) {
    case 'no-upstream':
      return '无可用上游 · 该路由没有指向已启用的上游模型'
    case 'probe-failed':
      return '读取失败 · 探测数据暂时取不到（不影响单价）'
    case 'not-probed':
      return '未探测 · 后台探测尚未覆盖到这个模型'
    default:
      return ''
  }
}
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>可用模型</h1>
        <div class="sub">你能调用的模型清单 · 单价与连通率（只读）</div>
      </div>
      <div class="head-actions">
        <select v-model="days" class="select sel" @change="loadTimeline">
          <option :value="7">近 7 天</option>
          <option :value="14">近 14 天</option>
          <option :value="30">近 30 天</option>
        </select>
        <select v-model="granularity" class="select sel" @change="loadTimeline">
          <option value="hour">按小时</option>
          <option value="day">按天</option>
        </select>
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
           在网关故障时去催管理员。 -->
      <div v-else-if="err" class="empty">
        <div class="big">⚠</div>
        {{ err }}
        <div class="err-retry">
          <button class="btn btn-sm" :disabled="loading" @click="load">重试</button>
        </div>
      </div>

      <!-- 空 = 你一把都调不了。注意这里**不写**「不限制」：那是 ModelPicker
           「一个都不选」的规则，属于配置态，与本页无关。 -->
      <div v-else-if="sorted.length === 0" class="empty">
        <div class="big">∅</div>
        目前没有可调用的模型。可能的原因：管理员尚未配置路由，或你的分组白名单与现有路由没有交集 —— 请联系管理员确认。
      </div>

      <template v-else>
        <!-- restricted 显式提示，且必须排在清单**上方**：它是读这份清单的
             前置条件（这份不是全集），写在下面会被当成脚注。 -->
        <p v-if="restricted" class="notice-warn" role="status">
          这份清单已被你的分组收窄过 —— 下面的模型就是你最多能调用的，其余模型即使知道名字也会被拒绝。
          需要更多模型请联系管理员调整分组白名单。
        </p>

        <!-- 曲线配置入口：几十个模型全画既看不清也让查询变重，所以只画
             勾选的几个（默认前 6 个有数据的）。价格不受影响，照常全列。 -->
        <div class="config-bar">
          <button class="btn btn-sm" type="button" @click="configOpen = true">
            配置显示曲线（{{ drawn.length }}）
          </button>
          <span class="dim">其余模型只显示单价</span>
        </div>

        <div class="model-grid">
          <article v-for="m in sorted" :key="m" class="card model-card">
            <div class="card__header">
              <div class="card__title model-name mono">{{ m }}</div>
              <!-- 副标题位优先放供应商名（有 meta 才有）；没有就退回单价单位。
                   两者都不该写「未配置」式的说明 —— 那是下面每行的职责。 -->
              <div class="card__description model-sub">
                <template v-if="providerOf(m)">{{ providerOf(m) }} · </template>
                单价（元 / 百万 tokens）
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
                   （两个上游协议里 cached 都 ⊆ input），它是命中时的**折扣价**。 -->
              <div class="price-row">
                <span class="price-lab">其中:缓存命中</span>
                <span class="price-val mono" :class="{ unset: (priceOf(m)?.price_cache_hit ?? 0) <= 0 }">
                  {{ priceCell(priceOf(m)?.price_cache_hit) }}
                </span>
              </div>
            </div>

            <!-- 连通率：与价格同卡，但**状态必须逐个区分** ——
                 「配置没指向上游」「网关读不到」「还没探测过」「正常」四件事
                 全都显示成空白或「无数据」的话，管理员无法判断该修哪一处。 -->
            <div class="card__footer model-health">
              <div class="health-line">
                <span class="badge" :class="healthClass(healthOf(m)?.health ?? 'unknown')">
                  {{ healthLabel(healthOf(m)?.health ?? 'unknown') }}
                </span>
                <span v-if="healthOf(m)?.window_count" class="dim health-stat">
                  近窗成功率 {{ Math.round((healthOf(m)?.success_rate ?? 0) * 100) }}%
                </span>
                <span v-else class="dim health-stat">{{ lineNote(m) }}</span>
              </div>

              <template v-if="lineState(m) === 'ok' && drawn.includes(m)">
                <div v-if="timelineLoading" class="chart-empty dim">加载中…</div>
                <template v-else>
                  <HeartbeatBar :buckets="curveFor(m)" :hours="HB_HOURS" :bucket-minutes="60" />
                </template>
              </template>
              <div v-else-if="drawn.includes(m)" class="chart-empty dim">{{ lineNote(m) }}</div>
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
        <li>
          连通率来自后台定时探测（<b>max_tokens=1</b>、不扣网关用户的钱），
          每张卡的价格与曲线<b>指向同一个上游模型</b>。
        </li>
        <li class="dim">
          看不到某个模型不一定是故障：路由被停用、或模型名拼错，都会让它不出现在这份清单里。
        </li>
      </ul>
    </div>

    <AppModal
      :open="configOpen"
      title="配置显示曲线的模型"
      max-width="620px"
      :dismissable="false"
    >
      <div class="picker-list">
        <label v-for="m in sorted" :key="m" class="picker-item">
          <input type="checkbox" :checked="drawn.includes(m)" @change="toggleDrawn(m)" />
          <span class="picker-name mono">{{ m }}</span>
          <span class="badge" :class="healthClass(healthOf(m)?.health ?? 'unknown')">
            {{ healthLabel(healthOf(m)?.health ?? 'unknown') }}
          </span>
        </label>
      </div>
      <div class="picker-hint dim">
        勾选后会在卡片里画出这段时间的连通率曲线。没勾选的模型仍然显示单价，只是不画曲线。
      </div>
      <div class="form-actions">
        <div class="actions-right">
          <button type="button" class="btn btn-sm btn-ghost" @click="drawAll">全选有数据的</button>
          <button type="button" class="btn btn-sm btn-ghost" @click="clearDrawn">清空</button>
          <button type="button" class="btn btn-primary" @click="configOpen = false">完成</button>
        </div>
      </div>
    </AppModal>
  </main>
</template>

<style scoped>
.page-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 14px;
  margin-bottom: 14px;
}
.page-head h1 {
  margin: 0 0 4px;
  font-size: 17px;
}
.sub {
  color: var(--text-3);
  font-size: 12.5px;
}
.sel {
  width: 110px;
}
.config-bar {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 12px;
}

/* auto-fill + minmax：卡片数随宽度自动换行，不写死列数 —— 模型从 3 个到
   80 个都能用同一套布局（写死 3 列在窄屏会把卡片挤成一条）。
   minmax 的下限放宽到 288px：现在卡内多了「三行价格 + 一段曲线」，
   232px 会把曲线压成一条竖线。 */
.model-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(288px, 1fr));
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

/* 连通率区：与价格之间加一条分隔，让「这是两类信息」一眼可见。
   卡片没有分隔线可借，只能自己加（与上面的描边同理）。 */
.model-health {
  padding-top: 9px;
  border-top: 1px solid var(--border);
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.health-line {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}
.health-stat {
  font-size: 11.5px;
}
.chart-empty {
  font-size: 11.5px;
  padding: 4px 0;
}

/* 配置弹窗 */
.picker-list {
  display: flex;
  flex-direction: column;
  gap: 2px;
  max-height: 60vh;
  overflow-y: auto;
}
.picker-item {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 5px 4px;
  border-radius: 6px;
  cursor: pointer;
}
.picker-item:hover {
  background: var(--hover);
}
.picker-item input {
  accent-color: var(--accent);
  flex: none;
}
.picker-name {
  font-size: 12.5px;
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
}
.picker-hint {
  font-size: 11.5px;
  margin-top: 8px;
  line-height: 1.55;
}
.form-actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  margin-top: 14px;
  padding-top: 12px;
  border-top: 1px solid var(--border);
}
.actions-right {
  display: flex;
  gap: 8px;
}

/* 提示条：语义是「你看到的不是全集」，属提示而不是错误，用 warning-soft。 */
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
