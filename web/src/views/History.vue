<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api, isAdmin } from '../api'
import { toast } from '../ui'
import { fmtNum, fmtTokens, fmtTimeMs, fmtSec, fmtSpeed, fmtMoney, statusLabel, statusBadge } from '../fmt'
import type { AccessKey, UsageHistoryEntry, UsageHistoryPage } from '../types'

const err = ref('')
const loading = ref(true)
const rows = ref<UsageHistoryEntry[]>([])
const days = ref(7)
const pageSize = ref(20)
const page = ref(1)
// 过滤后的总条数（后端返回，不受分页影响）—— 用来算总页数
const total = ref(0)

// ---------- 排障过滤器 ----------
// 与 CSV 导出、后端 History 端点共用同一套参数（status/model/key_id，精确匹配）。
// status 的取值与表格状态徽章同一套口径（见 fmt.statusLabel）；model 是公开模型名。
const fStatus = ref('')
const fModel = ref('')
// 上一次**真正提交**过的模型名。输入过程中的 fModel 只是草稿、不参与请求；
// 失焦时与它比对，内容没变就不必刷新。
const appliedModel = ref('')
// key 过滤用下拉而非手输：key_id 是内部 id，没人背得出来。
const fKey = ref('')
const keyOptions = ref<AccessKey[]>([])

const statuses = [
  { value: 'ok', label: '正常' },
  { value: 'error', label: '失败' },
  { value: 'truncated', label: '截断' },
  { value: 'overflow', label: '超限' },
  { value: 'canceled', label: '已取消' },
]

// 组装当前过滤器：空值归一成 undefined，api 侧就不会把它拼进 query。
function filters() {
  return {
    status: fStatus.value || undefined,
    model: fModel.value.trim() || undefined,
    keyId: fKey.value || undefined,
  }
}

const ranges = [
  { label: '近 1 天', value: 1 },
  { label: '近 7 天', value: 7 },
  { label: '近 30 天', value: 30 },
]
const pageSizes = [20, 50, 100]

const totalPages = computed(() => Math.max(1, Math.ceil(total.value / pageSize.value)))

/**
 * 「速度(t/s)」格的文本。
 *
 * tps 由**后端**算好下发（理由见 types.ts 的 UsageHistoryEntry.tps 注释：
 * 公式必须与总览页的 store.GetRecentThroughput 逐字一致）。
 *
 * tps === 0 表示**没有可算的样本** —— 上游未报 usage（output_tokens=0）、
 * 非流式短请求、或 latency_ms=0 的错误请求 —— 而不是「速度为零」。
 * 所以必须显示 '—'：可算样本的 tps 恒 > 0（分子分母都 > 0 的商不可能为 0），
 * 印成 0 会被读成「这次生成极慢」，与真相相反。
 *
 * 这层判断不能省掉交给 fmtSpeed：它自身对 <=0 返回的是 '0' 而不是 '—'。
 */
function fmtTps(tps: number): string {
  return tps > 0 ? fmtSpeed(tps) : '—'
}

/**
 * 「首字/总耗时」合并格的文本，形如 `3.6s/45.1s`。
 *
 * # 单位 s 为什么在这里显式拼
 *
 * fmtSec 返回的是**裸数字**：它只做 ms→s 换算与精度收敛，不带单位
 * （原「首字(s)」「总耗时(s)」两列是把单位写在**表头**里的）。合并成一格后
 * 表头变成「首字/总耗时」、不再含单位，两个数就必须各自带 s ——
 * 否则一格里的两个数字看不出量纲（是秒？毫秒？）。
 *
 * # ttfb_ms <= 0 时只让首字那半边是 '—'
 *
 * 不整格显示 '—'：总耗时是真实存在的，丢掉它等于这一格白占一列位置。
 * 而「非流式请求没有独立首字时间」是常态（非流式的 ttfb 与总耗时同源，
 * 落库常为 0），运维真正要看的正是总耗时。所以显示 `—/45.1s`。
 *
 * 两侧都无值时（latency_ms=0 的错误请求）会得到 `—/—`，如实反映
 * 「这条记录没有任何计时数据」。
 */
function fmtTtfbOverLatency(ttfbMs: number, latencyMs: number): string {
  const first = ttfbMs > 0 ? fmtSec(ttfbMs) + 's' : '—'
  const total = latencyMs > 0 ? fmtSec(latencyMs) + 's' : '—'
  return `${first}/${total}`
}

// 请求序号守卫：快速翻页 / 切时间范围会并发多个 fetch，晚到的旧响应若直接
// 写回 rows/page/total，会把新结果覆盖成过期数据（表内容与页码自相矛盾）。
// 每次请求 ++reqSeq，await 全部结束后只有「仍是最新序号」的请求才提交状态。
//
// 注意 fetchPage 内部是**两个** await（越界时会退到最后一页重取）：中途抛错
// 走的是 catch 而不是正常路径，所以「丢弃过期响应」的判断必须放在 finally 里，
// 否则一次失败的旧请求会把新请求的 loading 提前清掉、把错误态留在屏幕上。
let reqSeq = 0

// 加载态按序号分账：只有**当前**请求的完成才收尾。
// 直接用 finally { loading = false } 会让旧请求（或一次失败的翻页）把新请求的
// 加载态提前抹掉 —— 界面于是显示「加载中」消失、表格仍是上一页的内容，
// 看着像刷新成功了。
function settle(seq: number) {
  if (seq === reqSeq) loading.value = false
}

// fetchPage 取第 p 页（1 基）。
//
// 越界保护：数据变少（或切到更窄的时间范围）时当前页可能已不存在，
// 此时退到最后一页重取，而不是让用户停在一张空白表上 —— 空白表看起来
// 和「这个范围内没有记录」一模一样，会误导人。
//
// 错误态在这里**统一**处理（无论调用方是首次加载还是翻页）：
//   - 只有「仍是最新序号」的请求才写 err/toast。晚到的旧请求若失败就写错误态，
//     屏幕会在新数据已经到位之后又跳回「加载失败」，把一次早已无关紧要的
//     失败呈现成当前状态；
//   - 翻页失败同样要留下可见错误态。只弹 toast 的话，界面停在「上一页的内容
//     + 已经翻过去的页码」，看起来像翻页成功了 —— 用户会照着旧数据做判断。
async function fetchPage(p: number): Promise<void> {
  const seq = ++reqSeq
  loading.value = true
  try {
    let res = await api.usageHistory(days.value, pageSize.value, (p - 1) * pageSize.value, filters())
    let target = p
    const last = Math.max(1, Math.ceil(res.total / pageSize.value))
    if (p > last) {
      res = await api.usageHistory(days.value, pageSize.value, (last - 1) * pageSize.value, filters())
      target = last
    }
    if (seq !== reqSeq) return // 期间又发起了新请求，本响应的数据已过时，丢弃
    page.value = target
    rows.value = res.records
    total.value = res.total
    err.value = ''
  } catch (e) {
    if (seq !== reqSeq) return
    if ((e as { status?: number }).status === 401) return
    // 失败**必须留下可见的错误态**：只弹 toast 的话，页面保留空列表/
    // 空表格，呈现成「暂无数据」—— 而真实原因是请求失败了。运维会据此
    // 判断「今天没有流量」，进而排除掉网关/上游故障这个真正的方向。
    // 与 Users/Groups 的持久错误态同口径（见其 v-else-if="err"）。
    err.value = '加载调用历史失败：' + (e as Error).message
    toast('加载调用历史失败：' + (e as Error).message, 'err')
  } finally {
    settle(seq)
  }
}

// 首次加载 / 换条件后重新拉取。
function load() {
  // 记录「本次真正提交给后端的模型名」，供输入框失焦时判断内容有没有变。
  // 放在这里而不是 filters() 里，是为了让 filters() 保持纯读取 —— 它也被
  // 导出 CSV 调用，一次提交不该有两个地方改状态。
  appliedModel.value = fModel.value
  return fetchPage(page.value)
}

// 换时间范围 / 换每页条数 / 换过滤条件都必须回到第 1 页：留在原页码毫无意义，
// 而且极可能直接越界（原来在第 5 页，换成「近 1 天」后只有 1 页）。
function changeDays() {
  page.value = 1
  load()
}
function changePageSize() {
  page.value = 1
  load()
}

/**
 * 模型名过滤的提交时机。
 *
 * 下拉（状态 / 密钥）是离散选择，@change 当场提交没问题；模型名是**手输**的，
 * 边打字边提交会为每个字符发一次请求 —— 而且这些请求并发返回，虽有序号守卫
 * 兜底不至于显示错数据，但「输到一半的中间态」本身就没有意义（"gpt-4o" 打到
 * "gpt-" 时必然是 0 条），只会让表格在「暂无记录」和「有记录」之间来回闪。
 *
 * 所以输入框只把值写进 fModel（draft），真正的提交走：
 *   - 回车 / 失焦（change）
 *   - 「应用过滤」按钮
 * 「重置」则清空 draft 并立刻提交 —— 用户按它就是要「回到不过滤」这个结果。
 */
function applyFilters() {
  page.value = 1
  load()
}

/** 输入框失焦 / 回车：内容有变才提交，避免「点一下输入框就刷新一次」。
 *  比对用 trim 后的值 —— 前后空白不产生新的过滤条件，不该算作「变了」。 */
function onModelBlur() {
  if (fModel.value.trim() === appliedModel.value.trim()) return
  applyFilters()
}

function resetFilters() {
  fStatus.value = ''
  fModel.value = ''
  fKey.value = ''
  applyFilters()
}

async function go(p: number) {
  if (p < 1 || p > totalPages.value || loading.value) return
  await fetchPage(p)
}

// ---------- CSV 导出 ----------
const exporting = ref(false)

// 后端一次最多导出 1000 行（maxUsageLimit），命中上限时响应带
// X-Export-Truncated: 1 与 X-Export-Total（见 internal/admin/usage_handler.go）。
const CSV_MAX_ROWS = 1000

// 上一次导出的截断说明。非空时在过滤区常驻显示 —— 它必须比 toast 活得久：
// toast 5 秒后消失，而用户是拿着下载下来的文件去表格工具里做判断的，
// 等他打开文件时提示早没了，一个缺行的 CSV 就会被当成完整证据。
const exportNotice = ref('')

// 导出与列表同参数（时间范围 + 三个过滤器），口径由 api.ts 的 usageHistoryQuery 统一保证。
async function exportCSV() {
  if (exporting.value) return
  // 忙碌标记必须在**第一个 await 之前**置位（2026-10-10 修复的 P1）。
  // 原实现先 `await load()` 再置位，而 load() 是一次完整的
  // GET /usage/history（客户端超时 30s）—— 那段窗口里 exporting 仍是
  // false，再点一次「导出 CSV」就会并发跑两轮下载：两次 blob 下载与两次
  // <a download> 触发，而先返回的 finally 会把 exporting 清掉，
  // 于是按钮显示「可点」时其实还有一轮在跑，截断提示也会互相覆盖。
  exporting.value = true
  exportNotice.value = ''
  try {
    // 输入框里还有没提交的模型名时先落盘再导出：否则表格显示「全部模型」而 CSV
    // 只含那个尚未应用的值 —— 用户拿文件对不上屏幕，只会以为导出丢了数据。
    if (fModel.value.trim() !== appliedModel.value.trim()) {
      page.value = 1
      await load()
    }
    const r = await api.exportUsageCSV(days.value, filters())
    if (r.truncated) {
      // 「已导出多少、总共多少」都要说出来：只说「被截断了」的话，用户既
      // 不知道手上的文件有多大，也不知道该把范围缩到多小才够。
      const totalTxt = r.total === undefined ? '' : `共 ${fmtNum(r.total)} 条，`
      const omitted =
        r.total === undefined ? '' : `另有 ${fmtNum(Math.max(0, r.total - CSV_MAX_ROWS))} 条未导出，`
      const msg =
        `${totalTxt}本次只导出了最近 ${fmtNum(CSV_MAX_ROWS)} 条，${omitted}` +
        '请缩小时间范围或加过滤条件后重新导出。'
      exportNotice.value = msg
      toast(msg, 'err')
    } else {
      toast('CSV 已开始下载')
    }
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('导出失败：' + (e as Error).message, 'err')
  } finally {
    exporting.value = false
  }
}

onMounted(() => {
  load()
  // 密钥下拉的选项与列表同权限（admin 全部、普通用户自己的）。
  // 拉不到只影响「按密钥过滤」这一个入口，不阻塞页面 —— 与其它惰性加载同口径。
  api
    .keys()
    .then((ks) => (keyOptions.value = ks))
    .catch(() => {
      /* 保持空选项：下拉里只剩「全部密钥」 */
    })
})
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>调用历史</h1>
        <!-- 口径随身份变化，必须写在界面上（2026-10-10 对普通用户开放本页后）：
             管理员看到全站，普通用户只看到自己的。后端按会话身份强制收窄
             （usage_handler.scopeUserID），不是前端过滤。不写明的话，
             普通用户会以为「网关没记录到我的调用」—— 而实际是他只能看到
             自己的那部分，两种情况的排查方向完全不同。 -->
        <div class="sub">
          请求明细（按时间倒序，可分页）· {{ isAdmin() ? '全站' : '仅你自己' }}的调用记录
        </div>
      </div>
      <div class="head-actions">
        <select v-model.number="days" class="select w-auto" @change="changeDays">
          <option v-for="r in ranges" :key="r.value" :value="r.value">{{ r.label }}</option>
        </select>
        <select v-model.number="pageSize" class="select w-auto" @change="changePageSize">
          <option v-for="s in pageSizes" :key="s" :value="s">{{ s }} 条/页</option>
        </select>
        <button class="btn" :disabled="exporting || loading" title="按当前时间范围与过滤条件导出 CSV" @click="exportCSV">
          {{ exporting ? '导出中…' : '导出 CSV' }}
        </button>
        <button class="btn" :disabled="loading" @click="load">刷新</button>
      </div>
    </div>

    <div class="panel">
      <!-- 排障过滤器：与「导出 CSV」共用同一组条件 -->
      <div class="filters">
        <select v-model="fStatus" class="select w-auto" @change="applyFilters">
          <option value="">全部状态</option>
          <option v-for="s in statuses" :key="s.value" :value="s.value">{{ s.label }}</option>
        </select>
        <!-- 模型名是手输的，所以不 @change 即时提交：每敲一个字符发一次请求，
             中间态（"gpt-"）必然 0 条，表格会在空与不空之间闪。提交走回车/失焦/按钮。 -->
        <input
          v-model="fModel"
          class="input"
          placeholder="按模型过滤（精确匹配）"
          @keyup.enter="applyFilters"
          @change="onModelBlur"
        />
        <select v-model="fKey" class="select w-auto" @change="applyFilters">
          <option value="">全部密钥</option>
          <option v-for="k in keyOptions" :key="k.id" :value="k.id">{{ k.name }}（{{ k.key_prefix }}）</option>
        </select>
        <button class="btn btn-sm" :disabled="loading" @click="applyFilters">应用过滤</button>
        <button class="btn btn-sm btn-ghost" :disabled="loading" @click="resetFilters">重置</button>
      </div>

      <!-- 导出被截断的说明常驻显示：toast 5 秒就没了，而判断是在打开 CSV 之后做的 -->
      <p v-if="exportNotice" class="export-notice" role="alert">{{ exportNotice }}</p>

      <div v-if="loading && rows.length === 0" class="loading">加载中…</div>
      <!--加载失败**必须**与「确实没有数据」在界面上可区分：把请求失败呈现成
           「暂无数据」会让运维误判为无流量，从而排除掉网关/上游故障这个方向。
           与 Users/Groups 的错误态同口径。 -->
      <div v-else-if="err && total === 0" class="empty"><div class="big">⚠</div>{{ err }}</div>
      <div v-else-if="total === 0" class="empty">
        <div class="big">⌗</div>
        当前条件下暂无调用记录
      </div>
      <template v-else>
        <!-- 翻页失败时表格里还有上一页的内容，所以错误走常驻横幅而不是整块替换：
             把已经读到的行藏起来，等于刚翻页失败就顺手没收了用户正在看的数据。 -->
        <p v-if="err" class="notice-warn" role="alert">{{ err }}</p>
        <div class="tbl-wrap">
          <table class="tbl">
            <thead>
              <tr>
                <th class="c-time">时间</th>
                <th>调用模型</th>
                <th>实际模型</th>
                <th>调用密钥</th>
                <th class="num-h">Tokens</th>
                <th class="num-h">速度(t/s)</th>
                <th class="num-h">首字/总耗时</th>
                <th class="num-h">费用</th>
                <th class="c-st">状态</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="(h, i) in rows" :key="i">
                <td class="mono c-time">{{ fmtTimeMs(h.ts) }}</td>
                <td class="mono">{{ h.public_model || '—' }}</td>
                <td class="mono dim">{{ h.upstream_model || '—' }}</td>
                <td>
                  {{ h.key_name || h.key_id || '—' }}
                  <!-- 流式/非流式标注：首字时间的含义随它而变（见 types 的注释） -->
                  <span class="badge badge-off" :title="h.stream
                    ? '流式：首字时间是「多久吐出第一个字」'
                    : '非流式：响应一次性返回，首字时间≈总耗时'">
                    {{ h.stream ? '流式' : '非流式' }}
                  </span>
                </td>
                <td class="num-h">{{ fmtTokens(h.total_tokens) }}</td>
                <!-- 速度：0 = 没有可算的样本（上游未报 usage / 错误请求），
                     显示 — 而不是 0（见 fmtTps 的注释）。 -->
                <td class="num-h">{{ fmtTps(h.tps) }}</td>
                <!-- 首字与总耗时合并成一格，形如 3.6s/45.1s。
                     非流式请求没有独立首字时间（ttfb_ms=0）时只让前半格变 —，
                     保留总耗时（见 fmtTtfbOverLatency 的注释）。 -->
                <td class="num-h">{{ fmtTtfbOverLatency(h.ttfb_ms, h.latency_ms) }}</td>
                <!-- 单次费用可能远低于一分（单价低时），所以用 fmtMoney 的
                     高精度档而不是两位小数 —— 否则整列会显示成「0.00 元」，
                     恰恰掩盖了「确实花了钱」这个事实。 -->
                <td class="num-h">{{ h.cost > 0 ? fmtMoney(h.cost) : '—' }}</td>
                <td class="c-st">
                  <span class="badge" :class="statusBadge(h.status)">
                    {{ statusLabel(h.status) }}
                  </span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <div class="pager">
          <button class="btn btn-sm" :disabled="page <= 1 || loading" @click="go(1)">首页</button>
          <button class="btn btn-sm" :disabled="page <= 1 || loading" @click="go(page - 1)">上一页</button>
          <span class="pager-info">
            第 <b>{{ page }}</b> / {{ totalPages }} 页 · 共 {{ fmtNum(total) }} 条
          </span>
          <button class="btn btn-sm" :disabled="page >= totalPages || loading" @click="go(page + 1)">下一页</button>
          <button class="btn btn-sm" :disabled="page >= totalPages || loading" @click="go(totalPages)">末页</button>
        </div>
      </template>
    </div>
  </main>
</template>

<style scoped>
.w-auto {
  width: auto;
}
/* 过滤行：贴 Settings 价格筛选的同款布局 */
.filters {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  margin-bottom: 14px;
}
.filters .input {
  width: 220px;
}
.c-time {
  color: var(--text-2);
}
.c-st {
  width: 72px;
}
/* 导出被截断的说明：与 .chain-error 同款告警底色，位置紧贴过滤区 —— 用户要
   知道「改哪个条件才能导出完整」，提示必须长在他改条件的地方。 */
.export-notice,
.notice-warn {
  margin: 0 0 14px;
  padding: 8px 10px;
  border-radius: var(--r-chip, 6px);
  background: var(--warning-soft);
  color: var(--warning-soft-foreground);
  font-size: 12.5px;
  line-height: 1.55;
}
</style>
