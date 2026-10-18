<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { api, isAdmin } from '../api'
import { fmtNum, fmtTokens, fmtSpeed, fmtSec, fmtPercent, fmtMoney, fmtBalance, fmtRemainder } from '../fmt'
import * as timeRange from '../range'
import type { Stats, UsageGroupEntry, Provider, Route } from '../types'
import { useQueryText } from '../composables/useQueryText'

const err = ref('')
const loading = ref(true)
const stats = ref<Stats | null>(null)
const byDay = ref<UsageGroupEntry[]>([])
const loadedAt = ref(0)
const providers = ref<Provider[]>([])
const routes = ref<Route[]>([])
const gatewayLoading = ref(true)
const providerError = ref('')
const routeError = ref('')
const gatewayAt = ref(0)
const unready = computed(() => providers.value.filter(p => p.enabled && p.ready === false))
const unknown = computed(() => providers.value.filter(p => p.enabled && typeof p.ready !== 'boolean'))
const readyCount = computed(() => providers.value.filter(p => p.enabled && p.ready === true).length)
const activeRoutes = computed(() => routes.value.filter(r => r.enabled).length)
const errorRate = computed(() => stats.value && stats.value.total_requests > 0
  ? fmtPercent(stats.value.error_count / stats.value.total_requests) + '%' : '—')

type RangeKey = '1d' | '7d' | '14d' | '1m' | '1y' | 'all'
const RANGES: { key: RangeKey; label: string; days: number }[] = [
  { key: '1d', label: '近 1 天', days: 1 },
  { key: '7d', label: '近 7 天', days: 7 },
  { key: '14d', label: '近 14 天', days: 14 },
  { key: '1m', label: '近 1 月', days: 30 },
  { key: '1y', label: '近 1 年', days: 365 },
  { key: 'all', label: '全部', days: 0 },
]
const range = ref<RangeKey>('14d')
const rangeQuery = useQueryText('range', '14d')
const selectedRange = computed<RangeKey>({
  get: () => RANGES.find(r => r.key === rangeQuery.value)?.key ?? '14d',
  set: value => { rangeQuery.value = value },
})


// 区间口径统一走 range.ts：days=1 是**今天 0 点**起算，不是滚动 24 小时。
// 以前这里写的是 to - days * 86400_000，于是下拉写着「近 1 天」、数字算的却是
// 「从现在往前 24 小时」：早上 0~8 点的调用明明算今天，却落在窗口之外。
function rangeBounds(key: RangeKey): { from: number; to: number } {
  const days = RANGES.find((r) => r.key === key)?.days ?? 14
  return timeRange.rangeBounds(days)
}

function rangeLabel(key: RangeKey): string {
  return RANGES.find((r) => r.key === key)?.label ?? ''
}

// 柱状图的一个桶。labFull / labShort 是**预先算好的展示标签**，模板不再做字符串切片
// （切片散在模板里，改桶宽时极易忘掉一处，标签就会露出原始日期串）。
type Bucket = {
  key: string // v-for 的 key（取桶内首日，天然唯一）
  count: number
  tokens: number
  labFull: string // 桌面标签：按日 MM-DD / 按月 YYYY-MM
  labShort: string // 窄屏标签：只留日或月，避免把窄列撑破
  tip: string // 悬停提示
}

// 把后端按日聚合的结果补齐成连续序列（缺的日期补 0），保证柱状图横轴连续。
// 范围 ≤ 31 天按日展示；更大范围按周/月聚合，避免横轴过密。
// lifetimeTruncated：普通用户选「全部」档时拿不到 stats.lifetime
// （该字段 admin-only），起点只能退到 byDay 的最早一天；而 byDay 未传 limit
// 时后端默认只返回 100 行，于是图上实际只有最近 100 天。
//
// 标记出来是为了让界面如实说明 —— 下拉写着「全部」而图不是全部时，
// 用户无从分辨（2026-10-11 前端审计 M1）。这是**展示层的诚实**问题，
// 不是数据错误：数字本身是对的，只是覆盖窗口比字面窄。
//
// 判据与 daySeries 里的起点推导共用同一组条件，避免两处各写一份漂移。
const lifetimeTruncated = computed(() => {
  if (range.value !== 'all') return false
  const first = stats.value?.lifetime?.first_record_at
  if (first && first > 0) return false
  return byDay.value.length > 0
})

const daySeries = computed<Bucket[]>(() => {
  const map = new Map(byDay.value.map((e) => [e.key, e]))
  const days = RANGES.find((r) => r.key === range.value)?.days ?? 14
  const now = Date.now()

  // 全部历史：起点优先用终身累计里的 first_record_at。
  //
  // 修复前这里是「byDay 里最早的那一天」—— 明细剪掉 30 天以后，byDay 只剩
  // 30 天，趋势图会凭空丢掉更早的历史（usage_totals.first_record_at 存的就是
  // 这个值 —— 「全部」档必须用终身累计口径，不能用 30 天窗口的起点）。
  // 拿不到（普通用户 / 没请求 / 无数据）时回退到旧逻辑。
  let start: Date
  if (days === 0) {
    const first = stats.value?.lifetime?.first_record_at
    if (first && first > 0) {
      start = new Date(first)
    } else {
      // 降级情形由 lifetimeTruncated 负责对外说明，此处只管算起点。
      const keys = byDay.value.map((e) => e.key).sort()
      const earliest = keys[0]
      // 兜底（连 byDay 都空）：同样用本地日历日，now 是毫秒时间戳。
      start = earliest
        ? new Date(earliest + 'T00:00:00')
        : timeRange.localMidnight(30, now)
    }
  } else {
    // 柱子的起点必须由 rangeStart() 给，**不能**在这里另算一份。
    //
    // 这两处一旦各写各的就会漂移：rangeBounds 改成「今天 0 点起」而柱子还按
    // 「24 小时前」画，就会得到「数字是今天的数据、图上却有昨天一截」——用户
    // 读图得到的结论和读数字得到的结论相反。这正是本次要修的病根，所以图和
    // 数字必须共用同一个起点函数（localMidnight 同样处理跨月/跨年/夏令时）。
    start = new Date(timeRange.rangeStart(days, now))
  }

  // 逐日补齐。日期串走 timeRange.localDayKey：与后端 store.dayExpr（localtime）
  // 同一口径，本地日历日键必须与 byDay 的 key 逐字相同，否则 map 查不到、
  // 每根柱子都被补成 0 —— 一张「全零图」比报错更难查。
  const daily: { date: string; count: number; tokens: number }[] = []
  const cursor = new Date(start.getFullYear(), start.getMonth(), start.getDate())
  const end = timeRange.localMidnight(0, now)
  while (cursor <= end) {
    const date = timeRange.localDayKey(cursor.getTime())
    const e = map.get(date)
    daily.push({ date, count: e?.count ?? 0, tokens: e?.tokens ?? 0 })
    cursor.setDate(cursor.getDate() + 1)
  }

  // 桶宽：≤31 天按日；1 年按周；全部按月。
  const bucketDays = days === 0 ? 30 : days > 31 ? 7 : 1
  if (bucketDays === 1) {
    return daily.map((d) => ({
      key: d.date,
      count: d.count,
      tokens: d.tokens,
      labFull: d.date.slice(5), // 09-30
      labShort: d.date.slice(8), // 30
      tip: d.date.slice(5),
    }))
  }

  // 周桶 / 月桶的窄标签用「N月」：一年期的横轴本来就是看月份推进，
  // 只写「日」（01/05/10…）根本读不出是哪个月。
  const monthLabel = (dateStr: string) => `${parseInt(dateStr.slice(5, 7), 10)}月`

  const out: Bucket[] = []
  const monthly = bucketDays > 7
  for (let i = 0; i < daily.length; i += bucketDays) {
    const slice = daily.slice(i, i + bucketDays)
    const first = slice[0]
    const last = slice[slice.length - 1]
    // 尾桶不足整桶宽（「近 1 年」末尾常只剩几天）：标签加 * 标记、tip 写明实际
    // 天数 —— 不加标记的话，一个只有 2 天的桶会被读成「流量腰斩」。刻意**不**
    // 按桶宽做高度归一：那会把「只有 2 天的量」放大成「一整桶的量」，数据语义就变了。
    const partial = slice.length < bucketDays
    const mark = partial ? '*' : ''
    const partialNote = partial ? `（部分桶：仅 ${slice.length} 天）` : ''
    // 月桶跨月时标签补上止月：只写首月「2026-03」会被读成整桶都落在 3 月。
    // 同年给「2026-03~04」，跨年给完整两段，避免「~04」被误读成日期里的日。
    const sameMonth = first.date.slice(0, 7) === last.date.slice(0, 7)
    let labFullMonth = first.date.slice(0, 7)
    if (!sameMonth) {
      labFullMonth +=
        first.date.slice(0, 4) === last.date.slice(0, 4)
          ? `~${last.date.slice(5, 7)}`
          : `~${last.date.slice(0, 7)}`
    }
    const m1 = parseInt(first.date.slice(5, 7), 10)
    const m2 = parseInt(last.date.slice(5, 7), 10)
    out.push({
      key: first.date,
      count: slice.reduce((a, b) => a + b.count, 0),
      tokens: slice.reduce((a, b) => a + b.tokens, 0),
      labFull: (monthly ? labFullMonth : first.date.slice(5)) + mark,
      labShort: (monthly ? (sameMonth ? monthLabel(first.date) : `${m1}~${m2}月`) : monthLabel(first.date)) + mark,
      // 月桶的 tip 也给真实日期范围（与周桶同口径），悬停即可确认桶的覆盖范围。
      tip: `${first.date.slice(5)} ~ ${last.date.slice(5)}${partialNote}`,
    })
  }
  return out
})

// 横轴最多显示约 12 个标签，其余隐藏。步长按实际桶数自适应 ——
// 写死 `i % 3` 在「近 1 年 / 全部」这种几十上百个桶时会挤成一团竖排碎字。
const labelStride = computed(() => Math.max(1, Math.ceil(daySeries.value.length / 12)))

const maxTokens = computed(() => Math.max(1, ...daySeries.value.map((d) => d.tokens)))

// 账号级余额与配额不等于密钥、白名单或上游的完整可用性。
const readiness = computed(() => {
  if (isAdmin() || !stats.value) return null

  const unlimited = stats.value?.balance_unlimited ?? false
  const cents = stats.value?.balance_cents ?? 0
  const quota = stats.value?.quota_tokens ?? 0
  const used = stats.value?.used_tokens ?? 0

  if (!unlimited && cents <= 0) {
    return {
      cls: 'is-blocked',
      title: '现在发请求会被拒绝（余额已耗尽）',
      detail: '调用会返回 402。就余额和额度而言，需先充值才能继续。',
      linkTo: '/wallet',
      linkText: '去充值',
    }
  }
  if (quota > 0 && used >= quota) {
    return {
      cls: 'is-blocked',
      title: '现在发请求会被拒绝（额度已用尽）',
      detail: `已用 ${fmtTokens(used)} / 额度 ${fmtTokens(quota)} tokens。请联系管理员调整账号配额，换密钥不会恢复账号额度。`,
      linkTo: '/wallet',
      linkText: '查看账号配额',
    }
  }
  if (quota > 0 && used > 0) {
    const pct = Math.round((used / quota) * 100)
    if (pct >= 80) {
      return {
        cls: 'is-warn',
        title: `额度即将用尽（已用 ${pct}%）`,
        detail: `已用 ${fmtTokens(used)} / 额度 ${fmtTokens(quota)} tokens。`,
        linkTo: '/wallet',
        linkText: '查看账号配额',
      }
    }
  }
  return {
    cls: 'is-ok',
    title: '就余额和额度而言，现在可以调用',
    detail: '密钥是否启用、模型是否在你的白名单内，请分别查看密钥页与可用模型页。',
    linkTo: '',
    linkText: '',
  }
})

let statsSeq = 0
async function loadStats() {
  const seq = ++statsSeq
  const requested = selectedRange.value
  loading.value = true
  err.value = ''
  try {
    const { from, to } = rangeBounds(requested)
    const [s, d] = await Promise.all([api.stats(from, to, requested === 'all'), api.usageByDay(from, to)])
    if (seq !== statsSeq) return
    stats.value = s
    byDay.value = d
    range.value = requested
    loadedAt.value = Date.now()
  } catch (e) {
    if (seq === statsSeq && (e as { status?: number }).status !== 401) err.value = '统计加载失败：' + (e as Error).message
  } finally {
    if (seq === statsSeq) loading.value = false
  }
}

let gatewaySeq = 0
async function loadGateway() {
  if (!isAdmin()) { gatewayLoading.value = false; return }
  const seq = ++gatewaySeq
  gatewayLoading.value = true
  // 当前配置独立于历史统计；任一数据源失败只把自己的状态置为未知。
  const [p, r] = await Promise.allSettled([api.providers(), api.routes()])
  if (seq !== gatewaySeq) return
  if (p.status === 'fulfilled') { providers.value = p.value; providerError.value = '' }
  else { providers.value = []; providerError.value = '上游状态读取失败，当前状态未知' }
  if (r.status === 'fulfilled') { routes.value = r.value; routeError.value = '' }
  else { routes.value = []; routeError.value = '路由配置读取失败，当前状态未知' }
  gatewayAt.value = Date.now()
  gatewayLoading.value = false
}
function refresh() { void loadStats(); void loadGateway() }
watch(selectedRange, loadStats)
onMounted(refresh)
</script>

<template>
  <div class="page overview-page">
    <div class="page-head">
      <div><h1>{{ isAdmin() ? '网关总览' : '我的总览' }}</h1><div class="sub">{{ isAdmin() ? '全站流量、请求质量与待处理配置' : '个人调用、费用与账户状态' }}</div></div>
      <div class="head-actions">
        <label class="sr-only" for="overview-range">统计时间范围</label>
        <select id="overview-range" v-model="selectedRange" class="select range-control"><option v-for="r in RANGES" :key="r.key" :value="r.key">{{ r.label }}</option></select>
        <button class="btn" :disabled="loading || gatewayLoading" @click="refresh">刷新</button>
      </div>
    </div>

    <section v-if="isAdmin()" class="panel gateway-status">
      <div class="panel-title">当前配置<span class="hint">{{ gatewayLoading ? '正在刷新…' : '更新于 ' + new Date(gatewayAt).toLocaleTimeString() }} · 与统计时间范围无关</span></div>
      <div v-if="gatewayLoading" class="status-loading" role="status">正在读取上游与路由配置…</div>
      <template v-else>
        <div class="config-summary">
          <RouterLink to="/providers"><strong>{{ providerError ? '—' : readyCount }}</strong><span>凭据就绪 / {{ providerError ? '上游总数未知' : '共 ' + providers.length + ' 个上游' }}</span></RouterLink>
          <RouterLink to="/routes"><strong>{{ routeError ? '—' : activeRoutes }}</strong><span>已启用公开模型 / {{ routeError ? '总数未知' : '共 ' + routes.length + ' 个' }}</span></RouterLink>
          <RouterLink to="/models" class="status-link">查看首选目标探测 →</RouterLink>
        </div>
        <div class="config-alerts">
          <p v-if="providerError" class="status-message warn" role="alert">{{ providerError }}<button class="text-link" @click="loadGateway">重试</button></p>
          <p v-if="routeError" class="status-message warn" role="alert">{{ routeError }}<button class="text-link" @click="loadGateway">重试</button></p>
          <div v-for="p in unready.slice(0, 3)" :key="p.id" class="status-message">
            <span class="badge badge-st-error">未就绪</span><div><strong>{{ p.name }}</strong><div class="sub-line">{{ p.ready_reason || '未建立可用凭据池，请检查凭据配置' }}</div></div><RouterLink :to="{ name: 'provider-detail', params: { id: p.id } }" class="btn btn-sm">检查凭据</RouterLink>
          </div>
          <RouterLink v-if="unready.length > 3" to="/providers?status=unready" class="text-link">查看全部 {{ unready.length }} 个未就绪上游 →</RouterLink>
          <p v-if="unknown.length" class="status-message warn">{{ unknown.length }} 个已启用上游状态未知<RouterLink to="/providers?status=unknown" class="text-link">查看</RouterLink></p>
          <p v-if="!providerError && !providers.length" class="status-message">还没有上游。先接入服务并配置凭据与模型。<RouterLink to="/providers" class="btn btn-sm">接入上游</RouterLink></p>
          <p v-if="!routeError && !routes.length" class="status-message">还没有公开模型路由，客户端暂时没有可调用的模型名。<RouterLink to="/routes" class="btn btn-sm">配置路由</RouterLink></p>
          <p v-else-if="!routeError && !activeRoutes" class="status-message warn">所有公开模型路由均已停用。<RouterLink to="/routes" class="text-link">检查路由</RouterLink></p>
          <p v-if="!providerError && providers.length && !unready.length && !unknown.length" class="context-note">已启用上游的凭据池均已就绪。此状态不代表实时推理成功，需结合模型探测与请求日志判断。</p>
        </div>
      </template>
    </section>

    <div v-if="err" class="stale-bar" role="alert"><span>{{ stats ? '保留上次成功结果 · ' : '' }}{{ err }}</span><button class="btn btn-sm" :disabled="loading" @click="loadStats">重试</button></div>
    <div v-if="loading && !stats" class="loading">正在加载统计…</div>
    <template v-if="stats">
      <div v-if="readiness" class="call-readiness" :class="readiness.cls" role="status"><span class="cr-dot" aria-hidden="true"></span><div class="cr-body"><div class="cr-title">{{ readiness.title }}</div><div class="cr-detail">{{ readiness.detail }}</div></div><RouterLink v-if="readiness.linkTo" :to="readiness.linkTo" class="btn btn-sm">{{ readiness.linkText }}</RouterLink></div>
      <div class="section-caption"><span>{{ rangeLabel(range) }} · {{ isAdmin() ? '全站统计' : '我的统计' }}</span><span class="dim">{{ loading ? '正在加载所选范围，暂显上次结果' : '更新于 ' + new Date(loadedAt).toLocaleTimeString() }}</span></div>
      <div class="overview-metrics">
        <div class="card stat-card"><div class="card__title">请求量</div><div class="stat-v num">{{ fmtNum(stats.total_requests) }}</div><div class="metric-note">所选区间内的全部调用</div></div>
        <div class="card stat-card" :class="{ 'is-err': stats.error_count > 0 }"><div class="card__title">错误率</div><div class="stat-v num">{{ errorRate }}</div><div class="metric-note">{{ fmtNum(stats.error_count) }} 次错误 / {{ fmtNum(stats.total_requests) }} 次请求</div></div>
        <div class="card stat-card"><div class="card__title">近期平均首字用时</div><div class="stat-v num">{{ stats.avg_ttfb_ms > 0 ? fmtSec(stats.avg_ttfb_ms) : '—' }}<span v-if="stats.avg_ttfb_ms > 0" class="unit">s</span></div><div class="metric-note">{{ stats.recent_performance_status === 'error' ? '近期性能读取失败，请刷新重试' : stats.ttfb_samples ? '最近 ' + stats.ttfb_samples + ' 条成功且有首字计时的请求（最多 ' + stats.recent_sample_limit + ' 条）' : '暂无成功且有首字计时的样本' }} · 不随区间变化</div></div>
        <div class="card stat-card"><div class="card__title">记录费用</div><div class="stat-v num">¥{{ fmtMoney(stats.cost) }}</div><div class="metric-note">按调用当时单价固化 · 未配价不计费</div></div>
      </div>

      <div v-if="!isAdmin()" class="personal-balance"><div><span class="dim">当前账户余额</span><strong>{{ fmtBalance(stats.balance_cents, stats.balance_unlimited) }}</strong><span v-if="fmtRemainder(stats.balance_remainder)" class="dim">含 {{ fmtRemainder(stats.balance_remainder) }} 待结算</span></div><RouterLink to="/wallet" class="text-link">余额与账单 →</RouterLink></div>
      <section class="panel trend-panel">
        <div class="panel-title">Token 用量趋势<span class="hint">{{ rangeLabel(range) }} · {{ range === 'all' ? '每桶 30 天' : range === '1y' ? '按周聚合' : '按日聚合' }}</span><RouterLink to="/usage?tab=models" class="text-link trend-more">按模型分析 →</RouterLink></div>
        <div v-if="stats.total_requests === 0" class="context-note">这个时间范围内没有请求记录。可切换时间范围，或到请求日志查看其他时段。</div>
        <div v-if="lifetimeTruncated" class="hint-note">趋势仅覆盖当前返回的最近 {{ byDay.length }} 个有记录日期，可能不含全部历史。</div>
        <div class="bars"><div v-for="d in daySeries" :key="d.key" class="bar-col"><div class="bar-track"><div class="bar" :class="{ dim: !d.tokens }" :style="{ height: Math.max(2, d.tokens / maxTokens * 100) + '%' }" :data-tip="d.tip + ' · ' + fmtNum(d.count) + ' 次 · ' + fmtTokens(d.tokens) + ' tok'"></div></div></div></div>
        <div class="bar-labels" :class="{ dense: daySeries.length > 20 }"><span v-for="(d, i) in daySeries" :key="d.key" class="bar-lab" :class="{ dim: i % labelStride !== 0 }"><span class="bar-full">{{ d.labFull }}</span><span class="bar-short">{{ d.labShort }}</span></span></div>
        <div class="usage-summary"><span>输入 <strong>{{ fmtTokens(stats.input_tokens) }}</strong></span><span>输出 <strong>{{ fmtTokens(stats.output_tokens) }}</strong></span><span>缓存命中 <strong>{{ stats.input_tokens > 0 ? fmtPercent(stats.cache_hit_rate) + '%' : '—' }}</strong></span><span>近期输出速度 <strong>{{ stats.avg_tokens_per_sec > 0 ? fmtSpeed(stats.avg_tokens_per_sec) + ' tok/s' : '—' }}</strong>（{{ stats.recent_performance_status === 'error' ? '近期性能读取失败，请刷新重试' : stats.throughput_samples ? '最近 ' + stats.throughput_samples + ' 条成功且有输出计时的请求，最多 ' + stats.recent_sample_limit + ' 条' : '暂无成功且有输出计时的样本' }}，不随区间变化）</span></div>
      </section>
      <div class="overview-shortcuts"><RouterLink to="/history">查看{{ isAdmin() ? '请求日志' : '我的调用' }} →</RouterLink><RouterLink v-if="isAdmin()" to="/routes">管理模型与路由 →</RouterLink><RouterLink v-else to="/keys">管理 API 密钥 →</RouterLink></div>
    </template>
  </div>
</template>

<style scoped>
.range-control { width: auto; min-width: 110px; }
.gateway-status { margin-bottom: 24px; padding: 20px; }
.config-summary { display: flex; align-items: center; gap: 28px; padding: 8px 0 14px; flex-wrap: wrap; }
.config-summary > a { display: flex; align-items: baseline; gap: 10px; color: var(--text); text-decoration: none; }
.config-summary strong { font-size: 26px; font-variant-numeric: tabular-nums; }
.config-summary span { color: var(--muted); font-size: 13px; }
.config-summary .status-link { margin-left: auto; color: var(--accent); font-size: 13px; }
.config-alerts { padding: 0; }
.status-loading { padding: 20px; color: var(--muted); }
.status-message { display: flex; align-items: center; gap: 12px; padding: 12px; background: var(--surface-secondary); border-radius: 10px; margin: 6px 0; font-size: 13px; }
.status-message > div { min-width: 0; flex: 1; overflow-wrap: anywhere; }
.status-message > .btn, .status-message > .text-link { margin-left: auto; flex-shrink: 0; }
.warn { color: var(--warning-soft-foreground); background: var(--warning-soft); }
.section-caption { display: flex; justify-content: space-between; flex-wrap: wrap; gap: 8px; margin: 20px 0 10px; font-size: 12px; }
.overview-metrics { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 14px; margin-bottom: 22px; }
.overview-metrics .stat-card { padding: 20px; border-radius: 16px; gap: 10px; }
.metric-note { color: var(--muted); font-size: 11px; line-height: 1.6; }
.unit { font-size: 13px; margin-left: 4px; color: var(--muted); }
.personal-balance, .personal-balance > div { display: flex; align-items: center; gap: 16px; flex-wrap: wrap; }
.personal-balance { justify-content: space-between; padding: 18px 0 24px; font-size: 13px; }
.personal-balance strong { font-size: 22px; }
.trend-more { margin-left: auto; font-size: 12px; }
.usage-summary { display: flex; flex-wrap: wrap; gap: 12px 28px; padding: 18px 20px; margin-top: 14px; border-top: 1px solid var(--border); color: var(--muted); font-size: 12px; }
.usage-summary strong { color: var(--text); font-weight: 500; }
.overview-shortcuts { display: flex; flex-wrap: wrap; gap: 24px; padding: 16px 2px; font-size: 13px; }
.bar-full { display: inline; } .bar-short { display: none; }
@media (max-width: 1100px) { .overview-metrics { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
@media (max-width: 640px) {
  .config-summary { gap: 14px; padding: 8px 0 14px; }
  .config-summary > a { width: 100%; }
  .config-summary .status-link { margin-left: 0; }
  .config-alerts { padding: 0; }
  .status-message { flex-wrap: wrap; }
  .overview-metrics { gap: 10px; }
  .overview-metrics .stat-card { padding: 14px; }
  .overview-metrics .stat-v { font-size: 23px; }
  .trend-more { width: 100%; margin-left: 0; }
  .bar-full { display: none; } .bar-short { display: inline; }
}
</style>
