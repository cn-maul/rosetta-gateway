<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { RouterLink } from 'vue-router'
import { api, isAdmin } from '../api'
import { toast } from '../ui'
import { fmtNum, fmtTokens, fmtSpeed, fmtSec, fmtPercent, fmtMoney, fmtBalance, fmtRemainder } from '../fmt'
// 命名空间导入并改名：下面有一个组件本地的 `const range = ref<RangeKey>(...)`
//（当前选中的档位），同名会把这里的 range 遮住，导致 range.rangeBounds 被当成
// 那个 ref。改用 timeRange 既避开冲突，也顺带说明它管的是「时间区间」不是「档位」。
import * as timeRange from '../range'
import type { Stats, UsageGroupEntry, Provider } from '../types'

const err = ref('')
const loading = ref(true)
const stats = ref<Stats | null>(null)
const byDay = ref<UsageGroupEntry[]>([])
const byModel = ref<UsageGroupEntry[]>([])
const byKey = ref<UsageGroupEntry[]>([])
const byProvider = ref<UsageGroupEntry[]>([])
// 上游与模型，只为把 id 翻成人类可读的名字。
// 用量分组端点返回的 key 是 provider_id（32 位哈希），直接显示就是
// 「一串 id」—— 建映射后才能显示供应商名。
//
// 连带说明：原先这里还建了 modelsById / modelLabel 给「上游模型健康」卡用，
// 那个卡已在 2026-10-14 移除（连通率逐模型并进「可用模型」的卡片里），
// 所以 modelsById / modelLabel 一并删除。全模型列表因此也不再需要 ——
// 供应商名一张 providers 就够，少发一个 admin-only 请求。
const providers = ref<Provider[]>([])

const providerName = computed(() => {
  const m: Record<string, string> = {}
  for (const p of providers.value) m[p.id] = p.name || p.slug || p.id.slice(0, 8)
  return m
})

// ---------- 时间范围选择 ----------
// key 传给后端换算 from/to；label 显示在下拉里。
// days 为 0 表示「全部历史」（from=0）。
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
const rangeOpen = ref(false)

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

/**
 * readiness ——「我现在能调用吗」的聚合结论（2026-10-12）。
 *
 * 只回答**余额与额度**这两个维度，因为只有它们的判据在本页的 stats 里。
 * 密钥启用状态、模型白名单不在此列（见模板里的说明），
 * 所以标题一律带「就余额和额度而言」这个限定语 —— 不说满。
 *
 * 优先级：余额 > 额度。两者都不足时只报余额，因为处置动作不同
 * （余额要充值，额度换个 key 就绕过了），同时给两条会让用户先做容易的那件、
 * 然后发现还是被拒 —— 那正是余额独占一屏的理由。
 *
 * quotaTokens = 0 表示不限额，必须与 unlimited 一样显式排除，
 * 否则未配额度的用户会看到「额度已用尽」的红色结论。
 */
const readiness = computed(() => {
  // # 管理员不参与「余额/额度」判定，也**不显示这条横幅**（2026-10-14）
  //
  // 管理员账户只做控制面管理，数据面明确拒绝其调用模型
  //（auth.Authenticate 返回 ErrAdminCannotCallModel）。而 admin 的
  // balance_cents 恒为 0 且非 NULL（= 有限额且没钱），所以下面那条
  // `!unlimited && cents <= 0` 必然命中，会给管理员弹出
  // 「现在发请求会被拒绝（余额已耗尽）」+「去充值」—— 既是假的
  //（管理员压根调不了，与余额无关），又把人引向错误动作。
  //
  // 这里返回 **null** 而不是一条「说明性结论」横幅：
  // 这条横幅原本是给管理员一句「去建普通用户并自行签发密钥」的指引，
  // 但它常驻显示 —— 管理员每次打开总览都要先读到一句与当下无关的
  // 状态说明，而真正的入口在侧栏「用户与分组」就在手边。常驻横幅的
  // 成本高于收益，去掉后剩下的调用路径与普通用户完全一致。
  //
  // 关键在于**必须显式短路**：让管理员继续往下走那几个判断就是上面
  // 描述的那个假警报。
  if (isAdmin()) return null

  const unlimited = stats.value?.balance_unlimited ?? false
  const cents = stats.value?.balance_cents ?? 0
  const quota = stats.value?.quota_tokens ?? 0
  const used = stats.value?.used_tokens ?? 0

  if (!unlimited && cents <= 0) {
    return {
      cls: 'is-blocked',
      title: '现在发请求会被拒绝（余额已耗尽）',
      detail: '调用会返回 402。就余额和额度而言，需先充值才能继续。',
      linkTo: '/profile',
      linkText: '去充值',
    }
  }
  if (quota > 0 && used >= quota) {
    return {
      cls: 'is-blocked',
      title: '现在发请求会被拒绝（额度已用尽）',
      detail: `已用 ${fmtTokens(used)} / 额度 ${fmtTokens(quota)} tokens。换一把未耗尽的密钥即可继续。`,
      linkTo: '/keys',
      linkText: '查看密钥',
    }
  }
  if (quota > 0 && used > 0) {
    const pct = Math.round((used / quota) * 100)
    if (pct >= 80) {
      return {
        cls: 'is-warn',
        title: `额度即将用尽（已用 ${pct}%）`,
        detail: `已用 ${fmtTokens(used)} / 额度 ${fmtTokens(quota)} tokens。`,
        linkTo: '/keys',
        linkText: '查看密钥',
      }
    }
  }
  return {
    cls: 'is-ok',
    // 「就余额和额度而言」这个限定语刻意保留：本页没有密钥启用状态
    // 与模型白名单的数据，说「可以调用」是越权的结论。
    title: '就余额和额度而言，现在可以调用',
    detail: '密钥是否启用、模型是否在你的白名单内，请分别查看密钥页与可用模型页。',
    linkTo: '',
    linkText: '',
  }
})

const topModelMax = computed(() => Math.max(1, ...byModel.value.map((e) => e.tokens)))
const topKeyMax = computed(() => Math.max(1, ...byKey.value.map((e) => e.tokens)))
const topProviderMax = computed(() => Math.max(1, ...byProvider.value.map((e) => e.tokens)))

// 请求序号守卫：快速切换时间范围会并发多个 load()，晚到的旧响应若写回状态，
// 会把新范围的数据覆盖成旧范围的（下拉显示「近 1 天」、表里却是「近 1 年」）。
// 只有「仍是最新序号」的请求才提交状态与收场 loading。
let reqSeq = 0

async function load() {
  const seq = ++reqSeq
  loading.value = true
  err.value = ''
  try {
    const { from, to } = rangeBounds(range.value)
    // 「全部」档额外要终身累计：明细被剪掉之后，byDay 的最早一天不再是
    // 真正的起点（只剩 30 天），first_record_at 才是（见 stats_handler）。
    const wantLifetime = range.value === 'all'
    // 六路并发，但**降级语义分两组**。
    //
    // 前五个是页面的骨架：任一失败整屏都拿不到数据，必须走外层 catch
    // 进错误态。否则页面呈现的是「加载中…」消失后的空卡片，运维会读成
    // 「今天没有流量」，恰好把网关/上游故障这个方向排除掉 —— 而那正是
    // 需要排查的方向。
    //
    // providers 只是把 provider_id 翻成名字用的映射，失败回退成短 id 即可，
    // 远不值得让整屏数据消失，所以它自带 .catch 降级、不参与那组语义。
    //
    // **关键**：它与前面五个是**同一个 Promise.all**，而不是排在后面
    // 单独 await（2026-10-15 性能审计）。串行写法让首屏多等一个完整 RTT，
    // 而降级语义靠 .catch 就能保证，与「是否并行」无关 —— 早先把它拆到
    // 第二个 await 是把「降级」和「串行」这两件不相干的事绑在了一起。
    const [s, d, m, k, p, provs] = await Promise.all([
      api.stats(from, to, wantLifetime),
      api.usageByDay(from, to),
      api.usageByModel(from, to),
      api.usageByKey(from, to),
      api.usageByProvider(from, to),
      api.providers().catch(() => [] as Provider[]),
    ])

    if (seq !== reqSeq) return // 期间切换了新范围，本响应已过时
    stats.value = s
    byDay.value = d
    byModel.value = m
    byKey.value = k
    byProvider.value = p
    providers.value = provs
  } catch (e) {
    if (seq !== reqSeq) return
    if ((e as { status?: number }).status === 401) return
    // 骨架那五个请求任一失败整页都拿不到数据。不留错误态的话，页面呈现的
    // 是「加载中…」消失后的空卡片，运维会读成「今天没有流量」，恰好把
    // 网关/上游故障这个方向排除掉 —— 而那正是需要排查的方向。
    // （providers 那路自带降级，不会落到这里。）
    err.value = '加载总览失败：' + (e as Error).message
    toast(err.value, 'err')
  } finally {
    if (seq === reqSeq) loading.value = false
  }
}

function selectRange(key: RangeKey) {
  if (key === range.value) {
    rangeOpen.value = false
    return
  }
  range.value = key
  rangeOpen.value = false
  load()
}

// 点击组件外部 / 按 Esc 收起下拉。
// 只靠「选项自己 @click.stop」还不够：点空白处、切到别的控件时同样该收起。
const rangeEl = ref<HTMLElement | null>(null)

function onDocClick(e: MouseEvent) {
  if (!rangeOpen.value) return
  if (rangeEl.value && !rangeEl.value.contains(e.target as Node)) rangeOpen.value = false
}
function onKeydown(e: KeyboardEvent) {
  if (e.key === 'Escape') rangeOpen.value = false
}

onMounted(() => {
  load()
  document.addEventListener('click', onDocClick)
  document.addEventListener('keydown', onKeydown)
})
onUnmounted(() => {
  document.removeEventListener('click', onDocClick)
  document.removeEventListener('keydown', onKeydown)
})
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>总览</h1>
        <div class="sub">网关运行状态与{{ rangeLabel(range) }}用量</div>
      </div>
      <div class="head-actions">
        <!-- 时间范围下拉。
             注意 menu 上的 @click.stop：选项按钮的点击会冒泡到外层这个
             toggle 容器，导致 selectRange 里刚设好的 rangeOpen=false 又被翻回
             true —— 表现就是「选完不收起」。 -->
        <!--
          外层容器补可访问性（2026-10-11 前端审计）：
          原来只有 @click，键盘用户无法打开这个下拉（div 不可聚焦、无 role）。
          选项本身已是 <button>，所以只需让容器可聚焦 + 声明语义 +
          支持 Enter/Space 打开。Escape 关闭交给 document 监听（见脚本）。
        -->
        <div
          ref="rangeEl"
          class="range-select"
          role="button"
          tabindex="0"
          aria-haspopup="listbox"
          :aria-expanded="rangeOpen"
          @click="rangeOpen = !rangeOpen"
          @keydown.enter.prevent="rangeOpen = !rangeOpen"
          @keydown.space.prevent="rangeOpen = !rangeOpen"
        >
          <span class="range-current">{{ rangeLabel(range) }}</span>
          <span class="range-caret">▾</span>
          <div v-if="rangeOpen" class="range-menu" @click.stop role="listbox">
            <button
              v-for="r in RANGES"
              :key="r.key"
              class="range-item"
              role="option"
              :aria-selected="r.key === range"
              :class="{ active: r.key === range }"
              @click="selectRange(r.key)"
            >
              {{ r.label }}
            </button>
          </div>
        </div>
        <button class="btn" :disabled="loading" @click="load">刷新</button>
      </div>
    </div>

    <div v-if="loading && !stats" class="loading">加载中…</div>
    <!--
      stale：已经渲染过一次数据、之后刷新失败（2026-10-11 前端审计 H2）。
      原先只有 `v-else-if="err"`，而 stats 非空时它被跳过、旧数字照常渲染
      —— 用户看到的是一个「看起来完全正常」的过期页面，无法分辨数字已停更。

      与 Keys（v-else-if="err" 整块替换）/ History（常驻横幅）的差别是：
      本页的余额/费用/错误数是用户做判断的依据，静默保留旧值比直接报错更危险。
      所以这里保留数据 + 加过期标记，而不是二选一。
    -->
    <div v-else-if="err && !stats" class="empty"><div class="big">⚠</div>{{ err }}</div>
    <div v-else-if="err && stats" class="stale-bar">
      <span>⚠ 数据已过期：{{ err }}</span>
      <button class="btn btn-sm" :disabled="loading" @click="load">重试</button>
    </div>

    <template v-if="stats">
      <!--
        「我现在能调用吗」状态行（2026-10-12）。

        这一屏有 10 个指标、别处有密钥清单和模型清单，但没有一处直接
        回答用户真正要问的那句话：**现在发一个请求，会不会被拒？**
        会拒的三种原因散落在三个页面：
          - 余额耗尽 → 402（在钱包页）
          - 配额耗尽 → 429（在钱包页的额度徽章）
          - 密钥停用 → 401（在密钥页）
        用户要自己把三处拼起来才能得出结论，而拼错（以为能调）代价是
        一次失败的调试。

        这里只做**聚合**，不新增判据逻辑：三个判据都来自 stats 已有的字段，
        与钱包页、密钥页用的是同一个数字，不会两边说法不一致。

        # 刻意不覆盖的部分

        密钥是否启用、模型是否在白名单内，本页拿不到（Overview 不拉 /keys
        与 /model-names，加两个请求只为这一行不划算）。所以状态行只回答
        「**就余额和额度而言**」，并在文案里写明这个边界 —— 不说满，
        比给一个看起来完整但漏了两项判据的结论更诚实。

        # v-if="readiness" 不能省

        管理员的 readiness 是 **null**（见上面那个短路）。若这里直接读
        readiness.cls，模板会在渲染期抛「读 null 的属性」—— Vue 只在
        开发模式报这个错，生产构建下会静默渲染成一片空白，比原来更糟。
        守卫放在这里而不是脚本里，是为了让「这一段可能整体不出现」
        这件事在模板上一眼可见。
      -->
      <div v-if="readiness" class="call-readiness" :class="readiness.cls" role="status">
        <span class="cr-dot" aria-hidden="true"></span>
        <div class="cr-body">
          <div class="cr-title">{{ readiness.title }}</div>
          <div v-if="readiness.detail" class="cr-detail">{{ readiness.detail }}</div>
        </div>
        <RouterLink v-if="readiness.linkTo" :to="readiness.linkTo" class="btn btn-sm btn-ghost">
          {{ readiness.linkText }}
        </RouterLink>
      </div>

      <!--
        首次使用引导（2026-10-11 前端审计）。
        新账号第一次打开总览是一屏 0：没有密钥、没有用量、没有余额。
        三条最该做的事都能在别处做得到，但没有一条被主动指出来 ——
        用户只能自己在导航里猜「下一步该去哪」。
        只在「从来没有过调用」时出现；有数据之后自动消失，不打扰老用户。
      -->
      <div v-if="stats.total_requests === 0" class="notice-warn first-use" role="status">
        <div class="first-use-title">还没有任何调用记录。从这三步开始：</div>
        <ol class="first-use-steps">
          <!-- 「访问密钥」已并进「用户与分组」页的第三个页签（2026-10-14），
               /keys 现在是重定向。链接直接指到那一页，避免多一跳。 -->
          <li>
            到 <RouterLink to="/users" class="lnk">用户与分组</RouterLink> 建一把 API 密钥
            <span class="dim">（明文只在创建时显示一次，请当场保存）</span>
          </li>
          <li>
            到 <RouterLink to="/models" class="lnk">可用模型</RouterLink> 确认哪些模型对你开放
            <span class="dim">（分组白名单会收窄这份清单）</span>
          </li>
          <!-- admin 不显示余额告警：管理员账户不参与模型调用，其 balance_cents
             恒为 0（见 readiness 里的同一判断），提示「余额为 0 去充值」既是
             假的、也把人引向错误动作。 -->
          <li v-if="!isAdmin() && !stats.balance_unlimited && stats.balance_cents <= 0">
            余额为 0，调用会被拒绝 —— 请 <RouterLink to="/profile" class="lnk">联系管理员充值</RouterLink>
          </li>
        </ol>
      </div>

      <div class="stat-grid">
        <div class="card stat-card">
          <div class="card__title">总请求</div>
          <div class="card__description stat-v num">{{ fmtNum(stats.total_requests) }}</div>
        </div>
        <div class="card stat-card">
          <div class="card__title">总 Tokens</div>
          <div class="card__description stat-v num">{{ fmtTokens(stats.total_tokens) }}</div>
        </div>
        <!-- 费用：按每条用量**落库当时**的模型单价算好并固化（cost_total），再求和。
             所以改价不会回头改写已经发生的费用 —— 拿它和上游账单核对时，
             两边对不上的原因要去查当时的价格，而不是现在的价格。
             未配置价格的模型按 0 计，因此它不是账单，是「按各自当时的价算大概花多少」。 -->
        <div
          class="card stat-card"
          title="按各次调用当时的单价固化计算（元）；之后改价不会改写历史费用，未配置价格的模型不计费"
        >
          <div class="card__title">费用</div>
          <div class="card__description stat-v num">¥{{ fmtMoney(stats.cost) }}</div>
        </div>
        <!-- 余额：与「费用」放在一起是刻意的 —— 它们是同一件事的两面
             （花了多少 / 还剩多少），分开看总有一半说不清。
             待结算余数指「已消费但不足一分、还没从余额扣掉」的部分：
             余额按分扣减，单价低时一次调用不足一分，不显示它就会出现
             「费用在涨、余额不动」。 -->
        <div
          class="card stat-card"
          title="账户余额（元）。不足一分的消费先攒着，攒够一分后扣除"
        >
          <div class="card__title">余额</div>
          <div class="card__description stat-v num">
            {{ fmtBalance(stats.balance_cents ?? 0, stats.balance_unlimited ?? false) }}
          </div>
          <div v-if="fmtRemainder(stats.balance_remainder ?? 0)" class="card__description stat-sub">
            含 {{ fmtRemainder(stats.balance_remainder ?? 0) }} 待结算
          </div>
        </div>
        <div class="card stat-card">
          <div class="card__title">输入 Tokens</div>
          <div class="card__description stat-v num">{{ fmtTokens(stats.input_tokens) }}</div>
        </div>
        <div class="card stat-card">
          <div class="card__title">输出 Tokens</div>
          <div class="card__description stat-v num">{{ fmtTokens(stats.output_tokens) }}</div>
        </div>
        <div
          class="card stat-card"
          :title="`缓存读取 ${fmtTokens(stats.cached_tokens)} / 输入 ${fmtTokens(stats.input_tokens)} tok`"
        >
          <div class="card__title">缓存命中率</div>
          <div class="card__description stat-v num">
            {{ fmtPercent(stats.cache_hit_rate) }}<span class="unit">%</span>
          </div>
        </div>
        <div class="card stat-card">
          <div class="card__title">平均速度</div>
          <div class="card__description stat-v num">
            {{ fmtSpeed(stats.avg_tokens_per_sec) }}<span class="unit">tok/s</span>
          </div>
        </div>
        <div class="card stat-card">
          <div class="card__title">首字用时</div>
          <div class="card__description stat-v num">
            {{ fmtSec(stats.avg_ttfb_ms) }}<span class="unit">s</span>
          </div>
        </div>
        <div class="card stat-card" :class="{ 'is-err': stats.error_count > 0 }">
          <div class="card__title">错误数</div>
          <div class="card__description stat-v num">{{ fmtNum(stats.error_count) }}</div>
        </div>
      </div>

      <!-- 上游模型健康卡已移除（2026-10-14）：连通率现在逐模型显示在
           「可用模型」的每张卡片上（含价格与曲线），这一份摘要既重复、
           又只列出异常项，管理员要判断「哪个模型坏了」得再点进另一页。
           保留它会让同一份探测数据在两个地方呈现，且两处口径可能不同步。

           随之删掉的还有它的数据源：probeHealth / modelLabelForProbe /
           providers+modelsById 两个映射，以及 load 里的独立探测请求 ——
           留着就是不可达代码，而不可达代码会让下一个改这一页的人以为
           「总览也带健康信息」。modelLabelForProbe 依赖的 modelsById 只服务
           于它，一并删除。 -->

      <div class="panel">
        <div class="panel-title">
          {{ rangeLabel(range) }}用量
          <span class="hint">{{ rangeLabel(range) === '全部' ? '按月聚合' : rangeLabel(range) === '近 1 年' ? '按周聚合' : '按日聚合' }} · 悬停查看详情</span>
        </div>
        <!--
          「全部」档覆盖窗口不足时如实说明（2026-10-11 前端审计 M1）：
          普通用户拿不到 stats.lifetime（admin-only），起点退到 byDay 最早一天，
          而 byDay 后端默认只给 100 行。不说的话，下拉写着「全部」而图上只有
          最近 100 天，用户会以为更早的数据不存在。
        -->
        <div v-if="lifetimeTruncated" class="hint-note">
          仅显示最近 {{ daySeries.length }} 天的用量（更早的调用时间仅管理员可见）
        </div>
        <div class="bars">
          <div v-for="d in daySeries" :key="d.key" class="bar-col">
            <div class="bar-track">
              <div
                class="bar"
                :class="{ dim: d.tokens === 0 }"
                :style="{ height: Math.max(2, (d.tokens / maxTokens) * 100) + '%' }"
                :data-tip="`${d.tip} · ${fmtNum(d.count)} 次 · ${fmtTokens(d.tokens)} tok`"
              ></div>
            </div>
          </div>
        </div>
        <div class="bar-labels" :class="{ dense: daySeries.length > 20 }">
          <span v-for="(d, i) in daySeries" :key="d.key" class="bar-lab" :class="{ dim: i % labelStride !== 0 }">
            <span class="bar-full">{{ d.labFull }}</span>
            <span class="bar-short">{{ d.labShort }}</span>
          </span>
        </div>
      </div>

      <div class="panel">
        <div class="expand-grid is-3col">
          <div class="expand-col">
            <h4>模型用量 Top 10</h4>
            <div v-if="byModel.length === 0" class="empty">暂无用量记录</div>
            <div v-for="e in byModel" :key="e.key" class="mini-row">
              <span class="mini-main mono">{{ e.key }}</span>
              <span class="badge badge-accent num">{{ fmtNum(e.count) }} 次</span>
              <div
                class="bar"
                style="width: 72px; flex: none; border-radius: 3px"
                :style="{ height: '8px', opacity: 0.35 + 0.65 * (e.tokens / topModelMax) }"
              ></div>
              <span class="num" style="width: 64px; text-align: right; flex: none">{{ fmtTokens(e.tokens) }}</span>
            </div>
          </div>
          <div class="expand-col">
            <h4>密钥用量 Top 10</h4>
            <div v-if="byKey.length === 0" class="empty">暂无用量记录</div>
            <div v-for="e in byKey" :key="e.key" class="mini-row">
              <span class="mini-main">{{ e.name || e.key }}</span>
              <span class="badge badge-accent num">{{ fmtNum(e.count) }} 次</span>
              <div
                class="bar"
                style="width: 72px; flex: none; border-radius: 3px"
                :style="{ height: '8px', opacity: 0.35 + 0.65 * (e.tokens / topKeyMax) }"
              ></div>
              <span class="num" style="width: 64px; text-align: right; flex: none">{{ fmtTokens(e.tokens) }}</span>
            </div>
          </div>
          <div class="expand-col">
            <h4>供应商用量 Top 10</h4>
            <div v-if="byProvider.length === 0" class="empty">暂无用量记录</div>
            <div v-for="e in byProvider" :key="e.key" class="mini-row">
              <!-- e.key 是 provider_id（32 位哈希），经 providerName 翻成名字；
                   取不到时回退短 id，至少不再是那串乱码。 -->
              <span class="mini-main">{{ providerName[e.key] ?? e.key.slice(0, 8) }}</span>
              <span class="badge badge-accent num">{{ fmtNum(e.count) }} 次</span>
              <div
                class="bar"
                style="width: 72px; flex: none; border-radius: 3px"
                :style="{ height: '8px', opacity: 0.35 + 0.65 * (e.tokens / topProviderMax) }"
              ></div>
              <span class="num" style="width: 64px; text-align: right; flex: none">{{ fmtTokens(e.tokens) }}</span>
            </div>
          </div>
        </div>
      </div>
    </template>
  </main>
</template>

<style>
/* 速度卡片：单位与样本说明 */
.stat-card .unit {
  font-size: 12px;
  font-weight: 500;
  margin-left: 4px;
  color: var(--text-3);
}
/* 上游模型健康卡的样式（.health-*）已随该卡一并删除（2026-10-14）——
   连通率现在逐模型显示在「可用模型」的卡片上。整块删掉而不是留着：
   不可达样式会在下一次有人「借用一下这个左边框」时被接回不相干的元素。 */
/* 时间范围下拉 */
.range-select {
  position: relative;
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 36px;
  padding: 0 14px;
  border: 1px solid var(--border);
  border-radius: var(--r-btn, 24px);
  background: var(--surface);
  cursor: pointer;
  user-select: none;
  font-size: 13px;
  color: var(--text);
  transition:
    border-color 0.15s var(--ease-smooth),
    background 0.15s var(--ease-smooth);
}
.range-select:hover {
  border-color: var(--accent);
  background: var(--accent-soft);
}
.range-current {
  min-width: 64px;
  text-align: center;
}
.range-caret {
  color: var(--text-3);
  font-size: 11px;
}
.range-menu {
  position: absolute;
  top: calc(100% + 6px);
  right: 0;
  min-width: 120px;
  background: var(--overlay);
  color: var(--overlay-foreground);
  border: 1px solid var(--border);
  border-radius: var(--r-thumb, 12px);
  box-shadow: var(--sh-overlay);
  z-index: 20;
  overflow: hidden;
  padding: 4px;
}
.range-item {
  display: block;
  width: 100%;
  text-align: left;
  padding: 8px 10px;
  border: none;
  background: transparent;
  color: var(--text);
  font-size: 13px;
  border-radius: var(--r-chip, 6px);
  cursor: pointer;
}
.range-item:hover {
  background: var(--default);
}
.range-item.active {
  background: var(--accent);
  color: var(--accent-foreground);
}
/* 三个 by-* 维度并排（expand-grid 默认两列）；窄屏沿用全局的单列降级。
   注意这里的选择器特异性高于 styles.css 里媒体查询内的 .expand-grid，
   所以窄屏降级必须在这里重写一遍。 */
.expand-grid.is-3col {
  grid-template-columns: 1fr 1fr 1fr;
}
/* min-width: 0 是「长名字不再顶破右侧」的关键（2026-10-14）。
   grid 子项默认 min-width:auto —— 也就是「不许小于内容的最小宽度」，
   于是一个 40 字符的模型名（provider/model 形式）会把这一列撑到内容宽度，
   把后面的「次数徽章 + 柱条 + token 数」整体挤出面板右缘。
   而 .mini-main 的 text-overflow:ellipsis 只在**自己被约束住**时才生效，
   约束来自这一行的 min-width:0 链；少了它，text-overflow 形同虚设。

   刻意不用「在 JS 里把名字截成 22 个字 + …」：那会让 title 悬停提示、
   复制、以及「两个只差后缀的模型到底哪两个」都失去完整名字 ——
   省略号是纯视觉的，底层文本仍完整。 */
.expand-grid.is-3col > .expand-col {
  min-width: 0;
}
/* 指标卡一行五个（2026-10-10 用户要求：一行三个太占地方）。
   共 10 张卡 → 5×2 正好铺满，没有落单的空位。
   覆盖写在页面内而不是改全局 styles.css 的 .stat-grid：那个类目前只有本页
   在用，但它是全局文件、可能被别处复用；而这一页的卡片数量（10）是这里的
   事实，列数应与它配套，不该让全局样式替某一页的卡片数做决定。 */
.stat-grid {
  grid-template-columns: repeat(5, minmax(0, 1fr));
}
/* 窄屏降级要自己重写：上面的选择器特异性与 styles.css 媒体查询里的
   .stat-grid 相同（都是单类），后加载的页面样式会赢 —— 不写这段的话
   手机上也会挤成五列，卡片里的数字直接溢出。 */
@media (max-width: 1100px) {
  .stat-grid {
    grid-template-columns: repeat(3, minmax(0, 1fr));
  }
}
@media (max-width: 640px) {
  .stat-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}
@media (max-width: 900px) {
  .expand-grid.is-3col {
    grid-template-columns: 1fr;
  }
}
/* 柱状图双形态标签：桌面显示完整日期，窄屏只显示尾部 */
.bar-full {
  display: inline;
}
.bar-short {
  display: none;
}
@media (max-width: 640px) {
  .bar-full {
    display: none;
  }
  .bar-short {
    display: inline;
  }
}
</style>
