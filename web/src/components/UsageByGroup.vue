<script setup lang="ts">
import DataTable from './DataTable.vue'
/**
 * 用量统计的「按维度汇总」面板 —— 上游页与模型页共用。
 *
 * 两个页的**列完全一样**，只有第一列的表头与取数端点不同：
 * 上游按 provider_id 分组、模型按 upstream_model（或模型归组名）分组。
 * 做成一个组件由 props 切换，而不是复制两份 —— 复制后加一列（比如哪天
 * 想加「缓存命中」）就只会改一处，两页的数字对不上，且没人发现。
 */
import { onMounted, ref, watch } from 'vue'
import { fmtNum, fmtTokens, fmtMoney, fmtSec, fmtSpeed } from '../fmt'
import { rangeBounds } from '../range'
import type { UsageGroupEntry } from '../types'

const props = defineProps<{
  /** 第一列表头：「上游名称」或「模型」。 */
  firstCol: string
  /**
   * 维度。决定副标题的措辞 —— 见 subLabel 的注释。
   * 不从 firstCol 反推：两个表头都可能被人改文案，用文案当语义会静默失效。
   */
  kind: 'provider' | 'model' | 'key'
  /** 取数端点。两者都返回 UsageGroupEntry[]。 */
  fetch: (from: number, to: number) => Promise<UsageGroupEntry[]>
  /** 时间范围（天），由父页面统一持有。 */
  days: number
}>()

const rows = ref<UsageGroupEntry[]>([])
const err = ref('')
const loading = ref(true)

let reqSeq = 0
async function load() {
  const seq = ++reqSeq
  loading.value = true
  err.value = ''
  try {
    // to 现算而不是缓存：页面开着跨过零点时，缓存的 to 会让最新
    // 那批数据永远落在窗口外。
    const { from, to } = rangeBounds(props.days)
    const result = await props.fetch(from, to)
    if (seq === reqSeq) rows.value = result
  } catch (e) {
    if (seq !== reqSeq) return
    // 失败必须留可见错误态：空表与「这段时间真的没有调用」在界面上
    // 长得一模一样，用户会把一次网关故障读成「没人用过这个上游」。
    err.value = '加载用量统计失败：' + (e instanceof Error ? e.message : String(e))
    rows.value = []
  } finally {
    if (seq === reqSeq) loading.value = false
  }
}

onMounted(load)
watch(() => props.days, load)

/**
 * 成功率。
 *
 * 分母用 count（全部请求）而不是 ok+err：canceled 既不是成功也不是失败，
 * 但它**确实是一次请求**，把它排除会让「100% 成功」出现在一个发起了 50 次
 * 调用、其中 40 次被客户端自己掐断的上游上 —— 那是在夸一个不可用的上游。
 */
function successRate(r: UsageGroupEntry): number {
  return r.count > 0 ? r.ok / r.count : 0
}

/** 成功率格：百分比 + 原始计数。 */
function rateText(r: UsageGroupEntry): string {
  if (r.count === 0) return '—'
  return `${Math.round(successRate(r) * 100)}%（${fmtNum(r.ok)}/${fmtNum(r.count)}）`
}

/**
 * 成功率格的配色。阈值分两档而不是「不是 100% 就变黄」：
 * 一次调用失败率 100% 和 999 次里失败 3 次，后者严重得多，但前者的
 * 百分比数字一样。配合 rateText 里的原始计数一起看，才不会误判。
 */
function rateClass(r: UsageGroupEntry): string {
  const rate = successRate(r)
  if (r.count === 0) return ''
  if (rate < 0.5) return 'bad'
  if (rate < 0.9) return 'mid'
  return ''
}

/**
 * 首字延迟格。
 *
 * n_ttfb = 0 表示这个区间里没有真正的流式请求 —— 显示「—」而不是 0 秒。
 * 后者会被读成「首字快得不需要时间」，而真相是「压根没测过」。
 */
function ttfbText(r: UsageGroupEntry): string {
  if (r.n_ttfb === 0) return '—'
  return `${fmtSec(r.avg_ttfb_ms)}s`
}

/** 速度格。同样只在有成功请求时给数，否则显示 —。 */
function tpsText(r: UsageGroupEntry): string {
  return r.tps > 0 ? fmtSpeed(r.tps) : '—'
}

/**
 * 首列显示文本。
 *
 * 三级回退：name（显示名）→ key（分组键）→ 占位文案。
 * 最后一级不是装饰：真实库里存在 provider_id 为**空串**的历史行
 * （无归属的早期记录），而 name 字段带 omitempty 会被整个省略 ——
 * 于是那行既没名字也没键，留白比任何占位符都更像「加载坏了」。
 */
function firstCell(r: UsageGroupEntry): string {
  // 占位文案按维度给：模型页的 key 是上游裸模型名（不会是空串），
  // 写「未知上游」在那里是错的；只有 provider_id 可能为空。
  return r.name || r.key || (props.kind === 'provider' ? '（未知上游）' : '（未命名）')
}

/**
 * 副标题：只有当**显示名与分组键不同**时才给。
 *
 * 两个维度都会命中这个条件，但含义不同，所以文案也不同：
 *   - 模型页：name 是模型归组的组名，key 是上游裸模型名 →
 *     「归组自 xxx」告诉排查者该去哪儿找这个组；
 *   - 上游页：name 是上游名、key 是 provider_id →
 *     显示「归组自 <provider_id>」是纯噪音（上游没有归组这回事），
 *     但 provider 被删后 name 为空，这时必须**回退显示 key**，
 *     否则那行只剩一格空白，看起来像「这个上游没有名字」。
 */
function subLabel(r: UsageGroupEntry): string {
  if (!r.name) return ''
  if (r.name === r.key) return ''
  // 模型页：name 是组名、key 是上游裸模型名，副标题告诉排查者去哪找这个组。
  // 上游页：name 与 key 同时存在时（provider 在、名字拿到了）不需要副标题 ——
  // provider_id 对管理员没有参考价值，摆在下面只是噪音。真正需要兜底的是
  // 「name 为空、只能显示 key」那一支，而那时 firstCell 已经显示 key 了，
  // 再在副标题重复一遍是同一串字符出现两次。
  return props.kind === 'model' ? `归组自 ${r.key}` : ''
}

function hasSub(r: UsageGroupEntry): boolean {
  return subLabel(r) !== ''
}

/** 汇总为零无法反推出未配价或免费。 */
function costTitle(r: UsageGroupEntry): string {
  return r.cost === 0 ? '记录费用为 0，可能未配置价格；仅凭汇总无法判断，需结合请求日志核对' : ''
}
</script>

<template>
  <div class="panel">
    <div class="panel-actions">
      <span class="dim">{{ rows.length >= 200 ? '前 200 项' : '共 ' + rows.length + ' 项' }} · 按用量降序</span>
      <button class="btn btn-sm" :disabled="loading" @click="load">
        {{ loading ? '加载中…' : '刷新' }}
      </button>
    </div>

    <div v-if="err" class="empty sub-empty">
      <div class="big">⚠</div>
      {{ err }}
    </div>
    <div v-else-if="loading" class="empty sub-empty">加载中…</div>
    <div v-else-if="rows.length === 0" class="empty sub-empty">该时段没有调用记录</div>

    <DataTable v-else label="用量分组统计">
      <table class="tbl">
        <thead>
          <tr>
            <th>{{ firstCol }}</th>
            <th class="num-h">请求数</th>
            <th class="num-h">总 token</th>
            <th class="num-h">花费（元）</th>
            <th class="num-h">成功率</th>
            <th class="num-h">首字 / 速度</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="r in rows" :key="r.key">
            <td>
              <div class="name mono">{{ firstCell(r) }}</div>
              <!-- 显示名与分组键不同时给出副标题：模型页是「归组自 <上游裸名>」，
                   上游页是 provider_id 兜底（provider 被删后拿不到名字）。
                   措辞由 subLabel 按维度决定，见那里的注释。 -->
              <div v-if="hasSub(r)" class="sub-line dim">{{ subLabel(r) }}</div>
            </td>
            <td class="num">{{ fmtNum(r.count) }}</td>
            <td class="num">{{ fmtTokens(r.tokens) }}</td>
            <td class="num" :title="costTitle(r)">{{ fmtMoney(r.cost) }}</td>
            <td class="num" :class="rateClass(r)">{{ rateText(r) }}</td>
            <td class="num">
              <span>{{ ttfbText(r) }}</span>
              <span class="sep">/</span>
              <span>{{ tpsText(r) }} t/s</span>
            </td>
          </tr>
        </tbody>
      </table>
    </DataTable>
  </div>
</template>

<style scoped>
.num {
  font-variant-numeric: tabular-nums;
}
/* 成功率低于 90% 才标色：低于 100% 就报警的话，几十次调用的上游会
   长期挂着黄字，真正的故障反而被淹掉 —— 一两次客户端取消不算故障。 */
.bad {
  color: var(--danger);
}
.mid {
  color: var(--warning);
}
.sep {
  color: var(--muted);
  margin: 0 5px;
}
</style>
