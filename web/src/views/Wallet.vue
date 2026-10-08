<script setup lang="ts">
/**
 * 钱包与充值：**按身份分两种形态**的同一页。
 *
 * # 两种形态（2026-10-11）
 *
 * 管理员账户不再有余额语义 —— 它不建 key、不调 API，也被禁止给自己充值
 * （后端 AdjustBalance 已拦下）。所以管理员进来这一页不是看「我还有多少钱」，
 * 而是看**别人**的钱：全站的用户消费汇总 + 所有充值流水 + 一个充值入口。
 *
 *   - 管理员：用户消费金额（全站汇总）、充值记录、充值按钮；
 *     **没有**消耗记录（用户明确说「不要放消费记录」—— 明细在「调用历史」页，
 *     那里才有时间范围、状态/模型/密钥过滤与 CSV 导出）。
 *   - 普通用户：我的余额、我的消耗（**按天一行**，2026-10-10 由逐条明细改来）、
 *     我的充值记录。
 *
 * # 为什么普通用户的消耗改成「按天」（2026-10-10）
 *
 * 用户要回答的是「10-08 那天花了多少钱」，不是「那天第 37 次调用花了多少」。
 * 逐条明细在钱包页里有两个问题：一是 15 条上限让用户对不上账，
 * 而「对不上账」的第一反应是「网关没扣我钱」；二是它与「调用历史」页
 * 逐条明细功能重叠却少了全部排障控件，两份列表各自演化后必然对不上。
 * 逐条的诉求没有被取消，只是搬到了它该在的地方。
 *
 * 两形态共用一个组件而不是写两个页面：它们的「加载 → 错误 → 三段内容」骨架
 * 完全相同，分开写意味着骨架改一次要改两处，而两处迟早只改一处。
 *
 * # 为什么余额区要占这么大一块（普通用户形态）
 *
 * 额度（quota_tokens）与余额（balance_cents）是两件不同的事，且耗尽后的处置
 * 动作相反：额度耗尽换个 key 就行，余额耗尽会被 402 拒绝、必须充值。塌成
 * 同行的一对数字会被读成「同一个东西的两种单位」，于是用户按前者的经验处理
 * 后者 —— 换个 key 发现还是 402，只能回头找管理员。余额必须独占一屏。
 */
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, ApiFail, isAdmin, saveToken, session } from '../api'
// fmtTimeMs 仍被「充值记录」表使用（那里要的是完整时刻，不是按天）。
// 消耗记录改成按天之后**不再**用它 —— 按天表的第一列是后端给的 day 字符串，
// 用 fmtTimeMs 去格式化一个 day 会得到无意义的「NaN」，而且那个串必须
// 与后端 dayExpr 逐字相同才查得对，自己格式化等于自己造一个可能的偏差。
// statusLabel / statusBadge 随「状态」列一起移除了：按天汇总没有「这一次
// 成功/失败」可言 —— 一天里 100 次调用可能有 3 次失败，按天行只能显示请求
// 次数与费用。失败率要看得逐条，去「调用历史」页按状态筛。
import { fmtNum, fmtTokens, fmtBalance, fmtRemainder, fmtMoney, fmtTimeMs } from '../fmt'
import { toast } from '../ui'
import { rangeStart } from '../range'
import { checkPasswordStrength } from '../password'
import AppModal from '../components/AppModal.vue'
import type { Stats, TopupRecord, UsageByUserEntry, UsageGroupEntry, User } from '../types'

// ---------- 加载状态 ----------
//
// 三种状态必须可区分，不能塌成一个：
//   - loading：还在拉
//   - err：拉失败了（否则用户会读成「我一分钱都没有」）
//   - 空列表：真的没有记录
// 判据是 stats/rows 是否为 null 而不是「长度是否为 0」——
// 「没拿到」与「拿到但是空的」是两件事。
const loading = ref(true)
const err = ref('')
const stats = ref<Stats | null>(null)
/**
 * 普通用户的「消耗记录」：**按天**一行（2026-10-10 由逐条明细改成按天汇总）。
 *
 * 数据源是 by-day 端点，与总览页的日趋势图**同一个端点** —— 同一份数字在两处
 * 出现，口径天然一致（都是 SUM(cost_total)）。逐条明细改由「调用历史」页承担：
 * 那里才有时间范围、状态/模型/密钥过滤与 CSV 导出，钱包页复制一套筛选器只会
 * 让两份结论对不上。
 */
const byDay = ref<UsageGroupEntry[]>([])
/** 按天消耗是否加载失败。与整页 err 分开：它只影响那一张表。 */
const byDayErr = ref('')
/** 充值流水。两种身份都要，但内容不同（管理员=全部，普通用户=自己）。 */
const topups = ref<TopupRecord[]>([])
/** 充值流水条数，用于「共 N 条」。 */
const topupTotal = ref(0)
/** 充值流水是否加载失败。与整页 err 分开：它只影响那一张表。 */
const topupLoadErr = ref('')
/** 充值弹窗要用的用户下拉（仅管理员，按需拉取）。 */
const topupTargets = ref<User[]>([])

/**
 * 按用户汇总的消费（仅管理员）。一行一个用户，`total` 是有消费的用户总数。
 *
 * 与整页 err 分开：它只影响「消费汇总」那一张表 —— 汇总卡（全站合计）可能
 * 已经渲染出来了，不该因为这一张表失败而被一起收走。
 */
const byUser = ref<UsageByUserEntry[]>([])
const byUserTotal = ref(0)
const byUserErr = ref('')

/**
 * 消费汇总一次拉多少行。
 *
 * 100 是后端 clampLimit 的默认值，也是这里的取值 —— **不取 1000**：
 * 这张表是给人读的，一屏能读完的账号数远小于 100，而 1000 行既没人会翻完，
 * 又让「截断」在实际使用中几乎不出现（于是 total 那条提示也失去了意义）。
 *
 * 超出 100 时**不静默截断**：表尾会显示「显示前 100 / 共 N 位用户」，
 * 并引导去「调用历史」页按用户过滤。见下方模板。
 */
const BY_USER_LIMIT = 100

// 消耗记录覆盖多少天。
//
// # 按天汇总之后为什么反而**拉长**了（2026-10-10）
//
// 此前这里限制 30 天 + 最多 15 条，取的是逐条明细 —— 那时「15 条」才是实际
// 上限，30 天只是名义窗口，翻两页就没了。改成按天之后一行就是一天，
// 15 天的行数恰好是一屏多几行，仍然好读；而 30 天以前的钱如果完全不出现，
// 用户对不上账时会以为「没扣钱」。所以窗口取 90 天、按天列出 ——
// 每天一行，90 行是可以往下滑着看完的量，而钱必须能对得上。
//
// 口径用的是 range.ts 的 rangeStart（**本地自然日 0 点**起算），
// 与后端按天分桶的 dayExpr（localtime）逐字一致 —— 差一个时区偏移会让
// 「今天」这一格与后端的「今天」不是同一天，数字与图就对不上。
const USAGE_DAYS = 90

// 充值流水取多少条。
//
// 50 是后端 ListTopups 的默认值（defaultTopupLimit=50、上限 200）。
// 充值是**低频**事件 —— 一个月可能只有几笔 —— 所以这一页不做分页控件，
// 取够一个月的量即可。
const TOPUP_LIMIT = 50

// 请求序号守卫：与 Overview.vue / History.vue 同一套写法。
//
// load() 有三处会并发触发（初次挂载、点刷新、切页签重载），而 await 之间
// 隔着一次网络往返。没有序号守卫时，晚到的旧响应会把新数据覆盖成旧的 ——
// 表现是「余额已经充值过了，页面还显示充值前的数字」，而用户正是为这个数字来的。
let reqSeq = 0

async function load() {
  const seq = ++reqSeq
  loading.value = true
  err.value = ''
  topupLoadErr.value = ''
  byUserErr.value = ''
  byDayErr.value = ''
  // isAdmin() 读 session.me；本页在有 me 的前提下才渲染（路由守卫），所以拿得到。
  const isAdm = isAdmin()
  try {
    // 并发，彼此无依赖。
    //
    // stats 传 (0, now) 即「全部历史」：费用是按**落库当时**的单价算好后
    // 固化的 cost_total 再求和，所以改价不会改写历史，拿「全部」当总消费
    // 是准确的；且后端的 UsageSource 是「明细 ∪ 日归档」，30 天明细被剪掉
    // 之后费用不会缩水（见 stats_handler 的注释）。
    // stats 端点接受显式 from=0（它区分「显式传 0」与「没传」，见
    // queryRangeExplicit），与 history 端点同口径 —— 上一轮那个
    // 「history 把 0 当没传、回落到 7 天」的分叉已被移除，两边现在一致。
    //
    // 按天消耗用 USAGE_DAYS（不是 0），理由见该常量的注释。
    //
    // **管理员不请求 by-day**：这一格只对普通用户渲染，而 by-day 要扫归一化
    // 来源的整月数据。给不用的东西发请求没有收益。
    //
    // ⚠️ **「用户消费金额」（顶部大卡）复用 stats 而不是新增端点** ——
    // 这是本任务的设计决策（a），理由：
    //   ① stats 对管理员返回的 scope 是**全局**（callerScope 对 admin 返回空串），
    //      cost 正是「所有用户的消费合计」，就是这一格要的数字；
    //   ② 它已经实现了 UsageSource（明细 ∪ 日归档）的合并聚合，而归档那半边
    //      正是「30 天明细被剪掉后总消费不缩水」的关键。自己写一个汇总端点
    //      等于把这套口径复制一份，两份实现迟早在剪枝语义上分叉；
    //   ③ 管理员总览页本来就在拉同一个端点，两处数字必然一致 —— 新端点
    //      反而多一处「总览和钱包对不上」的可能。
    //
    // 而**下面那张「一人一行」的表用新的 by-user 端点**：按用户分组是新维度，
    // stats 给不了。两者是同一区间的两种视图（全站合计 vs 每人明细），
    // 所以它们的合计**必须相等**（同一个 UsageFilter + 同一个 cost_total 口径），
    // 界面上也把这句话写出来了。
    //
    // **now 只取一次**：顶部汇总卡（stats）与按用户的表（by-user）是同一区间
    // 的两种视图，界面上明确写了「未截断时两者应当一致」。若各调一次
    // Date.now()，两个 to 会差几毫秒 —— 恰好落在这几毫秒里的那条用量会让两个
    // 数字对不上，而那是**我自己造出来的**假不一致，管理员会照着那条提示去
    // 排查一个不存在的问题。取一个共享的 now，两者在构造上就可比。
    const now = Date.now()
    // 消耗记录的区间：rangeStart 走本地自然日 0 点，与后端 dayExpr 同口径。
    // 共享同一个 now（理由见下方注释）：顶部汇总卡与这张表必须可比。
    const dayFrom = rangeStart(USAGE_DAYS, now)
    const [s, d, t, bu] = await Promise.all([
      api.stats(0, now),
      // 管理员不需要按天表（这一格只对普通用户渲染）—— 不给不用的东西发请求。
      // 降级：by-day 挂了不该让余额整块消失，那才是用户真正要看的数字。
      isAdm
        ? Promise.resolve(null)
        : api.usageByDay(dayFrom, now).then(
            (r) => ({ ok: true as const, r }),
            (e: unknown) => ({ ok: false as const, e }),
          ),
      api.topups(TOPUP_LIMIT, 0).then(
        (r) => ({ ok: true as const, r }),
        (e: unknown) => ({ ok: false as const, e }),
      ),
      // 仅管理员：普通用户打这个端点必然 403（handler 内 requireAdmin），
      // 所以不能无条件发 —— 那会给每个普通用户制造一条假的错误态。
      isAdm
        ? api.usageByUser(0, now, BY_USER_LIMIT).then(
            (r) => ({ ok: true as const, r }),
            (e: unknown) => ({ ok: false as const, e }),
          )
        : Promise.resolve(null),
    ])
    if (seq !== reqSeq) return // 期间又发起了新请求，本响应已过时，丢弃
    stats.value = s
    // 按天消耗单独降级（理由同充值流水：它挂了不该让余额整块消失）。
    if (d === null) {
      byDay.value = []
    } else if (d.ok) {
      byDay.value = d.r
    } else {
      byDay.value = []
      byDayErr.value =
        d.e instanceof Error ? '消耗记录加载失败：' + d.e.message : '消耗记录加载失败'
    }
    // 充值流水**单独降级**：它挂了不该让余额/消费区整块消失（那两块可能
    // 已经渲染出来了），也不该被上面那个 catch 吞成一个整页错误。
    if (t.ok) {
      topups.value = t.r.records
      topupTotal.value = t.r.total
    } else {
      topups.value = []
      topupLoadErr.value =
        t.e instanceof Error ? '充值记录加载失败：' + t.e.message : '充值记录加载失败'
    }
    // 消费汇总同样单独降级（理由同上）。
    if (bu === null) {
      byUser.value = []
      byUserTotal.value = 0
    } else if (bu.ok) {
      byUser.value = bu.r.records
      byUserTotal.value = bu.r.total
    } else {
      byUser.value = []
      byUserTotal.value = 0
      byUserErr.value =
        bu.e instanceof Error ? '消费汇总加载失败：' + bu.e.message : '消费汇总加载失败'
    }
  } catch (e) {
    if (seq !== reqSeq) return
    // 401 已由 api 层清令牌并把 session.me 置空，App.vue 的 watch 会跳登录页。
    // 这里再弹一次 toast 只会盖住「重新登录」这个更该被看到的信息。
    if (e instanceof ApiFail && e.status === 401) return
    // 失败必须留下可见错误态：只弹 toast 的话，页面呈现的是「加载中…」消失后的
    // 空卡片，余额读不出来会被读成「余额是 0」—— 而那恰恰会触发 402，
    // 用户会去充值，而真实原因只是网关读库失败。与 Overview/History 同口径。
    err.value = '加载钱包数据失败：' + (e instanceof Error ? e.message : String(e))
    toast(err.value, 'err')
  } finally {
    if (seq === reqSeq) loading.value = false
  }
}

// me 必须走 computed 而不能抄一份局部常量：setup 只跑一次，而 session.me
// 是 loadSession() 的 await 之后才填上的。抄常量 → 整页永远按 null 渲染。
const me = computed(() => session.me)

// 身份分叉的唯一判据。两种形态的余额语义相反（管理员不限额、且永不扣费），
// 所以模板必须按它分开渲染，而不是靠某个余额数字去猜角色。
const admin = computed(() => isAdmin())

// 余额的三个态必须分开写，不能压成一个「余额」computed：
//   - 不限额：永远放行，**不消耗**用户的关注（不该和 0 元同屏等重）
//   - 有限额且 > 0：正常
//   - 有限额且 = 0：会被 402 拒绝，这是本页唯一必须显眼的红色状态
const unlimited = computed(() => me.value?.balance_unlimited ?? false)
const balanceCents = computed(() => me.value?.balance_cents ?? 0)
const remainderText = computed(() => fmtRemainder(me.value?.balance_remainder ?? 0))
const balanceEmpty = computed(() => !unlimited.value && balanceCents.value === 0)

// 总消费：全部历史的费用合计（元）。
//
// 注意这不是「这一页记录的 cost 求和」——那是最近 15 条的合计，把它叫
// 「总消费」会少算一大截，而且这个数在用户刷新一次之后就变了，
// 读起来像「总消费在变」。真正的合计只能来自 stats 的区间汇总。
//
// **管理员形态下这一个数字的含义是「所有用户的消费合计」**，因为 stats 对
// 管理员返回全局 scope（callerScope 对 admin 返回空串）。字段名不变，但标题
// 必须改（见模板）—— 否则管理员会把它读成「我消费了多少」。
const totalCost = computed(() => stats.value?.cost ?? 0)
// 累计调用次数，同上（stats 区间合计，不是本页 15 条）。
const totalCalls = computed(() => stats.value?.total_requests ?? 0)
// 均次消费：给「总消费」一个参照物，否则用户无法判断这个数字算多算少。
// 0 次调用时不能算 —— 会得到 NaN/除零，界面上显示「NaN 元」。
const avgCost = computed(() =>
  totalCalls.value > 0 ? totalCost.value / totalCalls.value : 0,
)

// ---------- 消费汇总（按用户）----------

/**
 * 一行的展示名。
 *
 * 三种情况必须分开，因为「空」不是一个能直接显示的答案：
 *   - 有用户名            → 用户名；
 *   - 用户已删除（key 非空而 user_name 空）→ 回退显示 user_id；
 *   - 无归属（key 本身为空，迁移前的无主 key 产生的用量）→ 「无归属」。
 *
 * 后两者在数据形状上都是「user_name 为空」，但含义完全不同：一个是真实存在过、
 * 现在被删掉的账号（它的消费是历史事实，要能对账），另一个是从来没有归属的
 * 历史遗留。只显示空白会让管理员把两者混为一谈，而它们的处置方式不一样。
 */
function rowName(r: UsageByUserEntry): string {
  if (r.user_name) return r.user_name
  return r.key || '无归属'
}

/**
 * 一行的标记（badge 文案 / 样式），无标记时返回 null。
 *
 * 「已删除」与「管理员」都必须显式标注，不能只靠名字空缺或角色列去意会：
 *   - **已删除**：这一行是历史账。不标注的话，管理员会去找一个已经不存在的
 *     账号，或者以为这是一条脏数据。
 *   - **管理员**：管理员现在**不能**调用模型（后端 ErrAdminCannotCallModel
 *     已拦下），所以他出现在这张表里只可能是升级前的历史用量。
 *     标注出来才不会被读成「管理员现在还在花钱」。
 *   - **无归属**（key 为空）单独一类，不与「已删除」合并 —— 见 rowName 的注释。
 */
function rowTag(r: UsageByUserEntry): { text: string; cls: string } | null {
  if (!r.key) return { text: '无归属', cls: 'badge-off' }
  if (!r.user_name) return { text: '已删除', cls: 'badge-off' }
  if (r.role === 'admin') return { text: '管理员·历史', cls: 'badge-accent' }
  return null
}

/** 有消费的用户数是否超过了拉取上限（于是这张表被截断）。 */
const byUserTruncated = computed(() => byUserTotal.value > byUser.value.length)

/**
 * 当前**显示出来的**这些行的消费合计。
 *
 * 用途是让管理员能当场核对「每人各花了多少」与顶部「全站合计」这两个层次：
 * 未截断时两者应当完全相等（同一个 from/to、同一个固化 cost_total 口径），
 * 对不上就说明有一边读错了 —— 而这正是分两张表看时最容易出现又最难发现的
 * 问题（两个数字各看都合理，只有放一起才知道错）。
 *
 * 被截断时它**小于**全站合计，所以文案必须跟着变（见模板）——
 * 否则一个偏小的合计会被当成「账对不上」。
 */
const byUserSum = computed(() => byUser.value.reduce((a, r) => a + r.cost, 0))

// ---------- 管理员充值 ----------
//
// 充值入口收敛到本页一处（原先散在「用户与分组」页每一行，见 UsersGroups.vue）。
const topupOpen = ref(false)
const topupUserId = ref('')
const topupAmount = ref<string>('')
const topupRemark = ref<string>('')
const topupErr = ref('')
const topupBusy = ref(false)

/**
 * 充值候选用户：只列**普通用户**。
 *
 * 后端 AdjustBalance 拒绝「给自己充值」（管理员没有余额语义，见其 handler），
 * 把管理员行也列进下拉会让管理员选中自己然后吃一个 400 —— 一个界面上
 * 自己造出来的死路。在选择阶段就滤掉，根本不给这个选项。
 */
const topupCandidates = computed(() => topupTargets.value.filter((u) => u.role !== 'admin'))

/** 当前选中的充值对象（用于展示现余额与「不限额」提示）。 */
const topupTarget = computed(
  () => topupCandidates.value.find((u) => u.id === topupUserId.value) ?? null,
)

/**
 * 元 → 整数分。四舍五入，与后端 store.YuanToCents 同一口径。
 *
 * 刻意在界面这一侧换算而不是把浮点元发给后端：余额是反复累加的账目，
 * 浮点的二进制表示无法精确表达十进制小数，而「发什么」必须在这里定死，
 * 否则后端换一个换算函数，同一笔充值在对账时就会差一分钱。
 *
 * （这份实现原先在 UsersGroups.vue，入口移过来时一并搬走，不留两份。）
 */
function yuanToCents(input: string): number | null {
  const n = Number(input)
  if (!Number.isFinite(n)) return null
  return Math.round(n * 100)
}

async function openTopup() {
  topupErr.value = ''
  topupAmount.value = ''
  topupRemark.value = ''
  topupUserId.value = ''
  topupOpen.value = true
  try {
    // 用户清单在**打开弹窗时**才拉，不随页面一起拉：它只服务于这一个弹窗，
    // 无条件拉一份等于每次进钱包页都多一次 /users 请求（而那一页不便宜 ——
    // ListUsers 对每个用户各跑一次 SumUserUsedTokens 与 ListAccessKeysByUser）。
    topupTargets.value = await api.users()
  } catch (e) {
    if (e instanceof ApiFail && e.status === 401) return
    topupErr.value = e instanceof ApiFail ? e.message : '加载用户列表失败'
  }
}

async function submitTopup() {
  if (topupBusy.value) return
  topupErr.value = ''
  if (!topupUserId.value) {
    topupErr.value = '请选择要充值的用户'
    return
  }
  const cents = yuanToCents(topupAmount.value)
  if (cents === null || Number.isNaN(cents)) {
    topupErr.value = '请输入一个金额'
    return
  }
  if (cents === 0) {
    // 后端也拒 0，但这里先说清：空输入框提交上来就是空串 → 0 分，
    // 而「调平」几乎一定是手滑而不是意图。
    topupErr.value = '金额不能为 0'
    return
  }
  topupBusy.value = true
  try {
    const updated = await api.adjustUserBalance(topupUserId.value, cents)
    const verb = cents > 0 ? '充值' : '扣减'
    toast(
      `${verb}成功，${updated.username} 当前余额 ${fmtBalance(updated.balance_cents, updated.balance_unlimited)}`,
      'ok',
    )
    topupOpen.value = false
    // 充值后重新拉：充值记录多了一条，而消费汇总虽然不一定变（充值本身不
    // 产生用量），保持「拉一次就是当前真相」。
    await load()
  } catch (e) {
    topupErr.value = e instanceof ApiFail ? e.message : '操作失败'
  } finally {
    topupBusy.value = false
  }
}

// ---------- 页签 ----------
//
// 页签**按身份不同**，由 computed 决定而不是模板里 v-if 两套：页签数组本身
// 就是数据，模板只负责渲染它；两套 v-if 会让「加一个页签」变成改两处。
//
// 管理员三格：消费汇总 / 充值记录 / 改密 —— 刻意**没有**「消耗记录」格，
// 用户明确说钱包页不放消费明细（明细在「调用历史」页，那里有时间范围、
// 状态/模型/密钥过滤与 CSV 导出）。
type TabKey = 'usage' | 'topup' | 'password'
const TABS = computed<{ key: TabKey; label: string }[]>(() =>
  admin.value
    ? [
        { key: 'usage', label: '消费汇总' },
        { key: 'topup', label: '充值记录' },
        { key: 'password', label: '修改密码' },
      ]
    : [
        { key: 'usage', label: '消耗记录' },
        { key: 'topup', label: '充值记录' },
        { key: 'password', label: '修改密码' },
      ],
)
const tab = ref<TabKey>('usage')

const router = useRouter()
function gotoHistory() {
  // 显式写 name 而不是 path：History 页的路由 path 不会变，但 name 变了他就
  // 少一个能跑的入口。router-link 同样是这个理由。
  void router.push({ name: 'history' })
}

// ---------- 修改密码 ----------
const oldPwd = ref('')
const newPwd = ref('')
const confirmPwd = ref('')
const pwdBusy = ref(false)
const pwdErr = ref('')
const newPwdErr = ref('')

// 强度门槛与后端 userauth.ValidatePassword 同一套（见 password.ts）。
// 刻意与后端保持一致但**不替代**它：前端只是提前告知，真正的强度判断
// 必须在服务端做（绕过前端直接打接口是常态）。
function checkStrength(p: string) {
  return checkPasswordStrength(p)
}

async function submitPwd() {
  if (pwdBusy.value) return
  pwdErr.value = ''
  newPwdErr.value = checkStrength(newPwd.value)
  if (newPwdErr.value) return
  if (newPwd.value !== confirmPwd.value) {
    pwdErr.value = '两次输入的新密码不一致'
    return
  }
  pwdBusy.value = true
  try {
    await api.changeMyPassword(oldPwd.value, newPwd.value)
    // 后端在此递增了 auth_version，**当前这个会话已经失效**（ChangePassword
    // 还顺手清了会话 cookie）。延迟跳转的那段时间里界面还在、任何点击都会打到
    // 一个必然 401 的接口上；所以立刻清掉本地会话并跳登录页 ——
    // 改密成功与「需要重新登录」是同一件事，不该拆成两步。
    //
    // 不调 logout()：那个端点也会（按用户）递增 auth_version 并清 cookie，
    // 而我们已经处在「会话已失效」的状态下，再发一请求只会拿到 401，
    // 还会在 api.ts 的 401 分支里再清一次令牌。本地 saveToken('') 就够了。
    saveToken('')
    session.me = null
    toast('密码已更新，请用新密码重新登录', 'ok')
    oldPwd.value = newPwd.value = confirmPwd.value = ''
    // redirect 指回本页：重新登录后回到钱包页，而不是被丢到总览再自己找回来。
    await router.replace({ name: 'login', query: { redirect: '/profile' } })
  } catch (e) {
    pwdErr.value = e instanceof ApiFail ? e.message : '改密失败'
  } finally {
    pwdBusy.value = false
  }
}

onMounted(load)
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>钱包与充值</h1>
        <!-- 副标题按身份换：管理员这一页讲的是「别人的钱」，普通用户讲的是
             「我的钱」。同一句话复用到两种身份上会有一半是错的。 -->
        <div class="sub">{{ admin ? '用户消费与充值流水' : '余额、消费与流水' }}</div>
      </div>
      <div class="head-actions">
        <!-- 充值按钮只对管理员出现。真正的拦截在后端（AdjustBalance 拒绝
             给自己充值），这里只是不给管理员看一个必然失败的入口。 -->
        <button v-if="admin" class="btn btn-primary" @click="openTopup">充值</button>
        <button class="btn" :disabled="loading" @click="load">刷新</button>
      </div>
    </div>

    <!-- 加载中：余额区是本页最该显眼的东西，骨架必须**先于**它出现，
         否则用户点进来的第一眼是「空」，与「没钱」无法区分。 -->
    <div v-if="loading && !stats" class="loading">加载中…</div>

    <!-- 加载失败：整页降级成一条错误态。刻意**不**在错误态下仍然渲染余额 ——
         拿不到余额时渲染出来的「0.00 元」会被读成「我被清零了」，
         而那是一次根本没发生过的账目事故。 -->
    <div v-else-if="err && !stats" class="empty">
      <div class="big">⚠</div>
      {{ err }}
      <div style="margin-top: 10px">余额未能在服务端读取，下面不会显示任何金额。</div>
      <button class="btn btn-primary" style="margin-top: 10px" @click="load">重新加载</button>
    </div>

    <template v-else>
      <!-- ---------- 顶部汇总卡：按身份两套 ---------- -->
      <!--
        管理员与普通用户的顶部卡**完全不同**，且这个分叉不能靠条件显示某几列
        来做：管理员没有「我的余额」这回事（后端 /me 对 admin 直接短路，
        不读余额），把它渲染成「不限」会让人以为那是某个真实规则，而实际
        是「这个概念不存在」。整块换掉更诚实。
      -->
      <div v-if="admin" class="panel wallet-head">
        <div class="wallet-grid wallet-grid-admin">
          <!-- 用户消费金额：管理员最关心的一个数字 —— 全站花了多少钱。
               口径是 stats 的**全部历史** cost 合计（明细 ∪ 日归档），
               明细被剪掉后不会缩水（见 stats_handler）。 -->
          <div class="wallet-main">
            <div class="k">用户消费金额</div>
            <div class="v num">¥{{ fmtMoney(totalCost) }}</div>
            <div class="note">
              全部历史累计，共 {{ fmtNum(totalCalls) }} 次调用
              <template v-if="totalCalls > 0">，均次 ¥{{ fmtMoney(avgCost) }}</template>。
            </div>
          </div>

          <!-- 消费汇总的三个佐证量。它们不与「金额」并列成同权重的几块 ——
               管理员来这里是查钱，不是看流量指标，所以放次位。 -->
          <div class="wallet-side">
            <div class="k">Token 消耗</div>
            <div class="v num small">{{ fmtTokens(stats?.total_tokens ?? 0) }}</div>
            <div class="note">与费用同一区间、同一数据源（输入 + 输出 + 缓存命中）。</div>
          </div>
          <!-- 这里原本还有第三格「管理员余额：不适用 + 一段解释」。
               2026-10-10 用户要求去掉：它占掉三分之一的宽度，只为了说明
               「这个概念不存在」—— 而管理员看不到任何余额入口、也不会去找它，
               这块解释是在回答一个没人问的问题。
               「管理员不参与余额计费」这条事实改由下面「管理员形态」的整体
               布局来表达（没有余额格、没有消耗记录格、只有充值台账）。 -->
          <div class="wallet-side">
            <div class="k">调用次数</div>
            <div class="v num small">{{ fmtNum(totalCalls) }}</div>
            <div class="note">与上方金额同源；单看金额无法判断量级。</div>
          </div>
        </div>
      </div>

      <!-- 普通用户的余额区 -->
      <div v-else class="panel wallet-head">
        <div class="wallet-grid">
          <!-- 当前余额：整页最突出的一块。
               不限额的显示口径**必须**走 fmtBalance —— 它看到 unlimited
               就返回「不限」。自己写 `cents/100` 的话管理员会被显示成
               「0.00 元」，而那与「余额耗尽」在界面上完全同形，处置动作却相反。 -->
          <div class="wallet-main">
            <div class="k">当前余额</div>
            <div class="v num" :class="{ 'is-err': balanceEmpty }">
              {{ unlimited ? '' : '¥' }}{{ fmtBalance(balanceCents, unlimited) }}
            </div>
            <div v-if="unlimited" class="note">该账户不受余额限制</div>
            <div v-else class="note">可用余额。耗尽后请求会被拒绝（402），需联系管理员充值。</div>
            <!-- 余额为 0 是本页**唯一**必须用危险色的状态：它对应后端真实会回的
                 402，而用户若事先不知道，就只能把「请求被拒」当成网关故障去排查。 -->
            <div v-if="balanceEmpty" class="note err-note">
              余额已用尽，现在发起请求会直接失败（HTTP 402），请先联系管理员充值。
            </div>
          </div>

          <!-- 待结算余数：与余额并排而不是塞进余额那一行。
               它回答的是「为什么余额没动」—— 余额按分扣减，单价低时单次调用
               不足一分，于是余额会长时间纹丝不动。把它藏进括号里（改造前
               Profile.vue 的写法）时，多数用户根本不会去读那个括号。
               fmtRemainder 在余数为 0 时返回空串，所以绝大多数时候它不占位。 -->
          <div class="wallet-side">
            <div class="k">待结算</div>
            <div class="v num small">{{ remainderText ? '¥' + remainderText : '—' }}</div>
            <div class="note">
              {{
                remainderText
                  ? '已消费但不足一分、尚未从余额扣除的部分，攒够一分后自动扣除。'
                  : '暂无不足一分的待结算消费。'
              }}
            </div>
          </div>

          <!-- 总消费：用户要的「总消费值」在这里。
               口径是**全部历史**的 cost 合计（stats 区间汇总，见 totalCost 注释），
               不是本页最近 15 条的求和 —— 后者会少算一大截，且每刷新一次就变，
               读起来像「总消费在变」。
               均次消费是给它的参照物：单看一个总额无法判断量级。 -->
          <div class="wallet-side">
            <div class="k">总消费</div>
            <div class="v num">¥{{ fmtMoney(totalCost) }}</div>
            <div class="note">
              <!-- 次数用 fmtNum 而不是 fmtTokens：后者是 K/M/B 紧凑档
                   （「12.3K 次调用」会被读成 token 数量）。与 Overview 页
                   的「总请求」卡片同口径。 -->
              全部历史累计，共 {{ fmtNum(totalCalls) }} 次调用
              <template v-if="totalCalls > 0">，均次 ¥{{ fmtMoney(avgCost) }}</template>。
            </div>
          </div>
        </div>
      </div>

      <!-- 额度（token）与余额是两件不同的事，**不能**和余额并排放进上面那张卡：
           额度耗尽换个 key 就行，余额耗尽必须充值。放一起会被读成「同一个东西的
           两种单位」。它在「消耗记录」页签底部以弱化的单行出现。 -->

      <!-- ---------- 页签 ---------- -->
      <div class="wallet-tabs">
        <button
          v-for="t in TABS"
          :key="t.key"
          class="wallet-tab"
          :class="{ active: tab === t.key }"
          type="button"
          @click="tab = t.key"
        >
          {{ t.label }}
        </button>
      </div>

      <!-- ---------- 消耗记录 / 消费汇总 ---------- -->
      <!--
        两个身份在这一格给的东西不同，且是**产品要求**（用户原话「不要放消费
        记录」）：管理员只要一个消费总额，不要明细；普通用户要的是「我的钱
        花到哪去了」，所以明细是他的核心需求。

        管理员明细的替代入口是「调用历史」页 —— 那里有完整台账（时间范围、
        状态/模型/密钥过滤、CSV 导出）。所以管理员这一格必须给出那个入口，
        否则「不要放消费记录」就变成了「管理员从此看不到任何明细」。
      -->
      <div v-if="tab === 'usage'" class="panel">
        <!-- 管理员：消费汇总 —— **一行一个用户，每行是该用户的总消费**。
             金额紧跟用户名放在第二列：用户要的就是「谁花了多少钱」，
             金额是这张表的主角，把它推到最右边会让人先读一堆次数/token
             才能找到要看的那个数。 -->
        <template v-if="admin">
          <div class="panel-title">
            消费汇总
            <span class="hint">全部历史 · 一行一个用户</span>
          </div>
          <div v-if="loading" class="loading">加载中…</div>
          <!-- 这张表失败**不**替换整块：顶部汇总卡（全站合计）可能已经渲染
               出来了，整块替换会把那个数字一起收走 —— 而失败只影响这一张表。
               与充值记录同口径。 -->
          <p v-else-if="byUserErr" class="notice-warn" role="alert">{{ byUserErr }}</p>
          <div v-else-if="byUser.length === 0" class="empty">
            <div class="big">⌗</div>
            暂无消费记录
            <div class="empty-detail">
              「全部历史」区间内还没有任何用户的用量记录。
              <br />
              若总览页显示有流量，说明这里的加载出了问题而不是真的没有数据 ——
              请用上方「刷新」重试。
            </div>
          </div>
          <template v-else>
            <div class="tbl-wrap">
              <table class="tbl">
                <thead>
                  <tr>
                    <th>用户</th>
                    <th class="num-h">总消费</th>
                    <th class="num-h">调用次数</th>
                    <th class="num-h">Token</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="r in byUser" :key="r.key || '__orphan__'">
                    <td>
                      <!-- 展示名走 rowName：用户被删掉时回退显示 user_id，
                           无归属用量显示「无归属」。绝不渲染成空白 ——
                           空白会被读成「没有归属」，而「已删除」是另一回事。 -->
                      <span :class="{ mono: !r.user_name }">{{ rowName(r) }}</span>
                      <!-- 标记见 rowTag：已删除 / 无归属 / 管理员历史用量。
                           三者都必须显式标注，理由在那个函数的注释里。 -->
                      <span v-if="rowTag(r)" class="badge" :class="rowTag(r)!.cls">
                        {{ rowTag(r)!.text }}
                      </span>
                    </td>
                    <!-- 费用用 fmtMoney 的高精度档：单次可能远低于一分，
                         两位小数会把小额整列显示成 0.00，掩盖「确实花了钱」。 -->
                    <td class="num-h">{{ r.cost > 0 ? '¥' + fmtMoney(r.cost) : '—' }}</td>
                    <td class="num-h">{{ fmtNum(r.count) }}</td>
                    <td class="num-h">{{ fmtTokens(r.tokens) }}</td>
                  </tr>
                </tbody>
              </table>
            </div>

            <!-- 截断提示：**必须可见**。一张被 limit 截断的表与一张完整的表
                 在界面上完全同形，而这张表是按金额读账的 —— 把「前 N 名」
                 读成「全部用户」会直接得出错误结论。与 CSV 导出的
                 X-Export-Truncated 同一个原则。 -->
            <div v-if="byUserTruncated" class="notice-warn" role="alert" style="margin-top: 14px">
              共 {{ fmtNum(byUserTotal) }} 位用户有消费，这里只显示消费最高的
              {{ fmtNum(byUser.length) }} 位。要看某位用户的逐条明细，请到
              「调用历史」页按密钥或模型过滤。
            </div>

            <div class="empty-detail" style="text-align: left">
              口径：<strong>全部历史</strong>的按用户汇总，明细已按天归档合并，
              30 天明细被清理后这些数字也不会缩水。费用是每条用量
              <strong>落库当时</strong>按模型单价算好后固化的，改价不会改写历史。
              <br />
              本表合计
              <strong>¥{{ fmtMoney(byUserSum) }}</strong>
              <!-- 未截断时本表合计必须与顶部「用户消费金额」相等（同一区间、
                   同一固化口径）。把这句话写出来，管理员才能当场发现一边读错 ——
                   两个数字各看都合理，只有放一起才知道错。 -->
              <template v-if="!byUserTruncated">
                （与上方「用户消费金额」应当一致；不一致说明有一侧读取失败）。
              </template>
              <template v-else>
                （仅含上表所示用户，因此小于上方「用户消费金额」）。
              </template>
              <!-- 次数/token 的合计保留在这里：单看金额无法判断量级，
                   而按用户的表已经逐行给出次数与 token。 -->
              全站 {{ fmtNum(totalCalls) }} 次调用、{{ fmtTokens(stats?.total_tokens ?? 0) }} token，
              失败 {{ fmtNum(stats?.error_count ?? 0) }} 次。
            </div>
            <!-- 明细的唯一入口。明细页对管理员开放（它本来就在 admin 导航区），
                 所以这里给链接而不是复制一张表 —— 复制一份筛选器会各自演化。 -->
            <div class="list-foot">
              <button class="btn btn-sm" @click="gotoHistory">查看逐条调用明细 →</button>
            </div>
          </template>
        </template>

        <!-- 普通用户：消耗记录（按天一行）
             2026-10-10 由「逐条明细」改成「按天总额」。用户要的是
             「10-08 那天花了多少」，而不是那一天的第 37 次调用花了多少 ——
             逐条的排障诉求由「调用历史」页承担（那里才有过滤与 CSV 导出）。
             两个页的数据同源（都是 cost_total），所以「按天求和 == 调用历史逐条相加」
             在构造上成立，不会出现两页对不上账。 -->
        <template v-else>
        <div class="panel-title">
          消耗记录
          <!-- 窗口写进标题：不说的话，用户会把它读成「我的全部消费」，
               而 90 天以前的不在表里。 -->
          <span class="hint">近 {{ USAGE_DAYS }} 天，按天列出</span>
        </div>

        <div v-if="loading" class="loading">加载中…</div>
        <!-- 余额区已经加载成功、只是这张表失败：走横幅而不是整块替换，
             否则用户会以为「我没花过钱」而那只是拉取失败。
             这一格必须用 byDayErr 而不是整页 err：整页 err 覆盖的是余额，
             余额比消耗更该被看见。 -->
        <p v-else-if="err" class="notice-warn" role="alert">{{ err }}</p>
        <p v-else-if="byDayErr" class="notice-warn" role="alert">{{ byDayErr }}</p>
        <div v-else-if="byDay.length === 0" class="empty">
          <div class="big">⌗</div>
          近 {{ USAGE_DAYS }} 天没有消耗记录
        </div>
        <template v-else>
          <div class="tbl-wrap">
            <table class="tbl">
              <thead>
                <tr>
                  <th>日期</th>
                  <th class="num-h">调用次数</th>
                  <th class="num-h">Tokens</th>
                  <th class="num-h">当日费用</th>
                </tr>
              </thead>
              <tbody>
                <!-- 后端 by-day 已按日期**升序**返回（groupBy 对 day 用
                     ORDER BY key ASC），所以这里不再排序：
                     再排一次既是多余的计算，也可能与后端的排序规则漂移。 -->
                <tr v-for="d in byDay" :key="d.key">
                  <td class="mono">{{ d.key }}</td>
                  <td class="num-h">{{ fmtNum(d.count) }}</td>
                  <td class="num-h">{{ fmtTokens(d.tokens) }}</td>
                  <!-- 单日费用可能远低于一分（单价低时），所以用 fmtMoney 的
                       高精度档而不是两位小数 —— 否则整列会显示成「0.00 元」，
                       恰恰掩盖了「确实花了钱」这个事实，也解释不了余额为何没动。
                       cost=0 显示「—」而不是 ¥0.00：那通常是**未配价**，
                       而「未配置」不等于「免费」（见 types.ts 的 UsageGroupEntry.cost）。 -->
                  <td class="num-h">{{ d.cost > 0 ? '¥' + fmtMoney(d.cost) : '—' }}</td>
                </tr>
              </tbody>
            </table>
          </div>

          <!-- 逐条明细在「调用历史」页：那里有时间范围、状态/模型/密钥过滤与
               CSV 导出。这一页刻意不复制那套控件 —— 两份筛选器各自演化，
               用户会拿本页的结论去调用历史页核对而对不上。 -->
          <div class="list-foot">
            <button class="btn btn-sm" @click="gotoHistory">查看逐条调用明细 →</button>
          </div>
        </template>

        <!-- 额度：弱化单行，明确它与余额无关（见上方注释）。 -->
        <div class="quota-line">
          <span class="k">Token 额度</span>
          <span class="num">
            已用 {{ fmtTokens(me?.used_tokens ?? 0) }}
            <template v-if="me && me.quota_tokens > 0">/ {{ fmtTokens(me.quota_tokens) }}</template>
            <template v-else>（不限）</template>
          </span>
          <span class="dim">额度按 token 计量，耗尽后更换密钥即可；余额按人民币计量，耗尽需充值。</span>
        </div>
        </template>
      </div>

      <!-- ---------- 充值记录 ---------- -->
      <!--
        这一格**两种身份共用一张表**，内容不同由服务端作用域决定：
        普通用户只看自己的，管理员看全部（待 Lead 决策，见报告）。
        列是同一套，所以这里只写一遍 —— 两套列定义早晚会漂移出
        「金额列对不齐」的界面。
      -->
      <div v-else-if="tab === 'topup'" class="panel">
        <div class="panel-title">
          充值记录
          <span class="hint">
            {{ admin ? '全部用户 · 最近 ' + topupTotal + ' 条' : '最近 ' + topupTotal + ' 条' }}
          </span>
        </div>

        <div v-if="loading" class="loading">加载中…</div>
        <!-- 这张表失败**不**替换整块：余额区/汇总区可能已经渲染出来了，
             整块替换会把它们一起收走，而失败只影响这一张列表。
             空态同理必须区分「没拿到」与「真的是空」。 -->
        <p v-else-if="topupLoadErr" class="notice-warn" role="alert">{{ topupLoadErr }}</p>
        <div v-else-if="topups.length === 0" class="empty">
          <div class="big">⌗</div>
          暂无充值记录
          <div class="empty-detail">
            {{ admin ? '还没有任何充值操作。' : '还没有人为你的账户充值。' }}
          </div>
        </div>
        <template v-else>
          <div class="tbl-wrap">
            <table class="tbl">
              <thead>
                <tr>
                  <th class="c-time">时间</th>
                  <!-- 用户名列**只对管理员**显示：管理员这一表是全站的，
                       「给谁充的」是首要信息；普通用户看到的永远是自己，
                       再加一列自己的名字纯属占宽度。 -->
                  <th v-if="admin">用户</th>
                  <th class="num-h">金额</th>
                  <th class="num-h">调整后余额</th>
                  <th v-if="admin">操作者</th>
                  <th>备注</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="t in topups" :key="t.id">
                  <td class="mono c-time">{{ fmtTimeMs(t.ts) }}</td>
                  <td v-if="admin" class="mono">{{ t.username || '—' }}</td>
                  <!-- 金额列的符号是这一列的全部信息量：充值与扣减走同
                       一个端点（相对调整），不带符号两者读起来一模一样。
                       扣减用弱化色，不必与充值抢注意力。 -->
                  <td class="num-h" :class="{ dim: t.delta_cents < 0 }">
                    {{ t.delta_cents > 0 ? '+' : '−' }}¥{{ fmtMoney(Math.abs(t.delta_cents) / 100) }}
                  </td>
                  <td class="num-h">¥{{ fmtMoney(t.balance_after / 100) }}</td>
                  <td v-if="admin" class="mono">{{ t.operator_username || '—' }}</td>
                  <td class="dim">{{ t.remark || '—' }}</td>
                </tr>
              </tbody>
            </table>
          </div>
          <div v-if="admin && topupTotal > topups.length" class="list-foot">
            <span class="dim">仅显示最近 {{ topups.length }} / {{ topupTotal }} 条</span>
          </div>
        </template>
      </div>

      <!-- ---------- 修改密码 ---------- -->
      <div v-else class="panel">
        <div class="panel-title">
          修改密码
          <span class="hint">修改后当前登录状态会立即失效</span>
        </div>
        <form class="form" @submit.prevent="submitPwd">
          <label class="flabel" for="op">当前密码</label>
          <input id="op" v-model="oldPwd" type="password" class="input" autocomplete="current-password" />

          <label class="flabel" for="np">新密码</label>
          <input id="np" v-model="newPwd" type="password" class="input" autocomplete="new-password" />
          <div v-if="checkStrength(newPwd)" class="fhint warn">{{ checkStrength(newPwd) }}</div>
          <div v-else-if="newPwd" class="fhint ok">强度符合要求</div>

          <label class="flabel" for="cp">确认新密码</label>
          <input id="cp" v-model="confirmPwd" type="password" class="input" autocomplete="new-password" />

          <div v-if="pwdErr" class="fhint err">{{ pwdErr }}</div>

          <button class="btn btn-primary" type="submit" :disabled="pwdBusy">
            {{ pwdBusy ? '提交中…' : '修改密码' }}
          </button>
        </form>
      </div>
    </template>

    <!-- ---------- 充值弹窗（仅管理员） ----------
         会动钱，所以两件事必须写在界面上而不是靠后端 400 兜底 ——
         ①「相对调整」的语义（填 100 是充 100，不是把余额设成 100）；
         ② 给**不限额**用户充值会把「不限」切成有限额。后者最容易被忽略：
         管理员以为在「送钱」，实际把对方从无限额度切成了一个具体的数额。

         「选择用户」用下拉而不是自由输入 id：用户名是唯一标识，输错 id 的
         结果是 404/查无此人，而下拉里每一项都带着现余额，管理员能直接看到
         自己正在动谁的账。 -->
    <AppModal :open="topupOpen" title="充值" max-width="460px" :dismissable="false">
      <label class="flabel" for="tu">充值给</label>
      <select id="tu" v-model="topupUserId" class="input">
        <option value="">请选择用户</option>
        <!-- 只显示 username（与「用户与分组」页的列表同一口径，不引入那页
             之外的第二种展示习惯）。每项带上**现余额**是这里的关键增量：
             管理员要能看着自己正在动谁的账，而不是选完再去别处查。 -->
        <option v-for="u in topupCandidates" :key="u.id" :value="u.id">
          {{ u.username }} — 当前 {{ fmtBalance(u.balance_cents, u.balance_unlimited) }}
        </option>
      </select>
      <!-- 管理员自己不在候选里（topupCandidates 已滤掉 role='admin'，
           后端 AdjustBalance 也会拒）。这里显式说一句，免得有人以为
           「为什么找不到我自己」。 -->
      <div v-if="topupCandidates.length === 0" class="fhint warn">
        还没有可充值的普通用户。管理员账户不参与余额计费，也不能给自己充值。
      </div>

      <label class="flabel" for="ta">金额（元）</label>
      <input id="ta" v-model="topupAmount" class="input" type="number" step="0.01" placeholder="例如 100" />
      <div class="fhint">
        填<strong>正数</strong>充值，填<strong>负数</strong>扣减。这是**相对调整**：填 100 是「加 100 元」，
        不是「把余额设成 100 元」—— 误操作不会清零。
      </div>

      <label class="flabel" for="tr">备注（可选）</label>
      <input id="tr" v-model="topupRemark" class="input" placeholder="充值原因、工单号…" />

      <!-- 两处必须写在界面上的提醒：不限额用户被充值会降级；预览把即将
           发生的变化说清楚，而不是让管理员提交后去列表里数位数。 -->
      <div v-if="topupTarget?.balance_unlimited" class="fhint warn">
        {{ topupTarget.username }} 当前<strong>不限额</strong>。充值会把它切换成有限额 ——
        充值后他只能使用填入的金额。
      </div>
      <div v-if="topupTarget && yuanToCents(topupAmount)" class="fhint">
        调整后约为
        <b>{{ fmtBalance(topupTarget.balance_cents + (yuanToCents(topupAmount) ?? 0), false) }}</b>
      </div>

      <div v-if="topupErr" class="fhint err">{{ topupErr }}</div>
      <div class="form-actions">
        <button class="btn" @click="topupOpen = false">取消</button>
        <button class="btn btn-primary" :disabled="topupBusy" @click="submitTopup">
          {{ topupBusy ? '处理中…' : '确定' }}
        </button>
      </div>
    </AppModal>
  </main>
</template>

<style scoped>
/* 余额区：三列 —— 余额占主位，余数与总消费为辅。
   主辅比例刻意不对等（2fr : 1fr : 1fr）：用户来这一页是为了看「还剩多少」，
   另外两项是解释性信息，与主位同权重会让「不限」或一个 4 位小数的余数
   抢走注意力 —— 而 0 元余额才是本页唯一需要立刻被看见的东西。 */
.wallet-head {
  padding: 22px 24px;
}
.wallet-grid {
  display: grid;
  grid-template-columns: 2fr 1fr 1fr;
  gap: 20px;
  align-items: start;
}
/* 管理员形态：一主 + 两辅（2026-10-10 去掉「管理员余额：不适用」那一格）。
   主辅比例仍不对等（3fr : 1fr : 1fr）——「用户消费金额」是管理员来这一页的
   唯一动线，那两个佐证量只是解释性信息。 */
.wallet-grid-admin {
  grid-template-columns: 3fr 1fr 1fr;
}
.wallet-main .k,
.wallet-side .k {
  font-size: 12.5px;
  color: var(--muted);
  letter-spacing: 0.02em;
}
.wallet-main .v {
  /* 余额是整页的绝对主角：字号远大于 stat-card（总览页），
     令行内零散数字用 tabular-nums 等宽，数字跳动时不左右抖。 */
  font-size: 40px;
  font-weight: 650;
  line-height: 1.15;
  margin: 6px 0 8px;
  font-variant-numeric: tabular-nums;
  letter-spacing: -0.01em;
}
.wallet-side .v {
  font-size: 22px;
  font-weight: 600;
  line-height: 1.2;
  margin: 6px 0 8px;
  font-variant-numeric: tabular-nums;
  color: var(--foreground);
}
.note {
  font-size: 12px;
  color: var(--text-3);
  line-height: 1.6;
  max-width: 34em;
}
/* 危险色只给「余额已用尽」：它是本页唯一对应后端真实 402 的状态。
   其余说明一律用弱化色 —— 满屏红色会让真正该显眼的那一条被稀释掉。 */
.err-note {
  color: var(--danger);
  font-weight: 500;
}
.wallet-main .v.is-err {
  color: var(--danger);
}

/* 页签：形态对齐 Settings.vue 的 .set-tabs（胶囊分段），
   刻意不用下划线式 tab —— 那会被读成「可切换的页面」，
   而这里三格同属钱包、共享同一个标题与刷新按钮。 */
.wallet-tabs {
  display: flex;
  gap: 2px;
  width: max-content;
  padding: 3px;
  margin: 18px 0 14px;
  border-radius: var(--r-pill);
  background: var(--default);
}
.wallet-tab {
  padding: 6px 16px;
  border: none;
  border-radius: var(--r-pill);
  background: transparent;
  font-size: 13px;
  font-weight: 500;
  color: var(--muted);
  cursor: pointer;
  white-space: nowrap;
  transition:
    color 0.2s var(--ease-out-quart),
    background 0.25s var(--ease-spring);
}
.wallet-tab:hover {
  color: var(--foreground);
}
.wallet-tab.active {
  color: var(--segment-foreground);
  background: var(--segment);
  box-shadow: var(--field-shadow);
}

.c-time {
  color: var(--text-2);
}
.c-st {
  width: 72px;
}
/* 列表底部：跳转「调用历史」的口子。留白是因为上面是密排表格，
   紧贴着的按钮会被误读成表格的一行。 */
.list-foot {
  margin-top: 14px;
  display: flex;
  justify-content: flex-end;
}
/* 额度单行：与余额区之间用一条分隔线隔开，读起来是「另一个东西」，
   而不是一个卡片的续行 —— 理由见模板里的注释。 */
.quota-line {
  margin-top: 18px;
  padding-top: 14px;
  border-top: 1px solid var(--separator);
  display: flex;
  align-items: baseline;
  gap: 10px;
  flex-wrap: wrap;
  font-size: 12.5px;
  color: var(--text-2);
}
.quota-line .k {
  color: var(--muted);
}
/* 空态里的补充说明：与「暂无记录」的大字拉开层级，
   它是解释而不是标题。 */
.empty-detail {
  margin-top: 10px;
  font-size: 12.5px;
  line-height: 1.7;
  color: var(--text-3);
  max-width: 46em;
  margin-left: auto;
  margin-right: auto;
}
.form {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
}
.input {
  max-width: 360px;
}
/* 本页的「warn」是密码强度建议而非错误，覆盖全局的危险色回弱化色；
   「ok」也只有本页在用，一并留着。 */
.fhint.warn {
  color: var(--muted);
}
.fhint.ok {
  color: var(--success);
}
/* 列表加载失败走横幅（与 History.vue 的 .notice-warn 同款）：整块替换会把
   已经加载成功的余额区一起收走，而失败只影响这一张列表。 */
.notice-warn {
  margin: 0 0 14px;
  padding: 8px 10px;
  border-radius: var(--r-chip, 6px);
  background: var(--warning-soft);
  color: var(--warning-soft-foreground);
  font-size: 12.5px;
  line-height: 1.55;
}
@media (max-width: 860px) {
  .wallet-grid,
  .wallet-grid-admin {
    grid-template-columns: 1fr;
    gap: 16px;
  }
  .wallet-main .v {
    font-size: 34px;
  }
}
</style>
