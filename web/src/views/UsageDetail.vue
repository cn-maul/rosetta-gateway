<script setup lang="ts">
import DataTable from '../components/DataTable.vue'
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { api } from '../api'
import { toast } from '../ui'
import { fmtNum, fmtTokens, fmtTimeMs, fmtSec, fmtSpeed, fmtMoney, statusLabel, statusBadge } from '../fmt'
import type { AccessKey, UsageHistoryEntry } from '../types'
import DetailDrawer from '../components/DetailDrawer.vue'
import { parseRouteTrace } from '../routeTrace'

const props = defineProps<{ days: number }>()
const route = useRoute()
const router = useRouter()
const rows = ref<UsageHistoryEntry[]>([])
const total = ref(0)
const loading = ref(true)
const err = ref('')
const keys = ref<AccessKey[]>([])
const keyError = ref('')
const selected = ref<UsageHistoryEntry | null>(null)
const loadedPage = ref(1)
const loadedDays = ref(props.days)
const statuses = [
  { value: 'ok', label: '正常' }, { value: 'error', label: '失败' },
  { value: 'truncated', label: '截断' }, { value: 'overflow', label: '超限' }, { value: 'canceled', label: '已取消' },
]
function queryText(key: string) { return typeof route.query[key] === 'string' ? route.query[key] as string : '' }
const request = computed(() => ({
  days: props.days,
  status: statuses.some(s => s.value === queryText('status')) ? queryText('status') : '',
  model: queryText('model').trim(),
  keyId: queryText('key'),
  page: Math.max(1, Math.min(1000000, Math.floor(Number(queryText('page'))) || 1)),
  size: [20, 50, 100].includes(Number(queryText('size'))) ? Number(queryText('size')) : 20,
}))
const fStatus = ref('')
const fModel = ref('')
const fKey = ref('')
// 输入框保留草稿；实际请求、分页与 CSV 始终来自 URL 中已应用的条件。
watch(() => [request.value.status, request.value.model, request.value.keyId], () => {
  fStatus.value = request.value.status
  fModel.value = request.value.model
  fKey.value = request.value.keyId
}, { immediate: true })
const pageSize = computed({
  get: () => request.value.size,
  set: size => { void router.push({ query: { ...route.query, size: String(size), page: undefined } }) },
})
const totalPages = computed(() => Math.max(1, Math.ceil(total.value / request.value.size)))
const filters = () => ({
  status: request.value.status || undefined,
  model: request.value.model || undefined,
  keyId: request.value.keyId || undefined,
})
let reqSeq = 0
onUnmounted(() => { ++reqSeq })
async function load() {
  const seq = ++reqSeq
  const r = { ...request.value }
  loading.value = true
  err.value = ''
  try {
    const result = await api.usageHistory(r.days, r.size, (r.page - 1) * r.size, filters())
    if (seq !== reqSeq) return
    const last = Math.max(1, Math.ceil(result.total / r.size))
    if (r.page > last) {
      await router.replace({ query: { ...route.query, page: last === 1 ? undefined : String(last) } })
      return
    }
    rows.value = result.records
    total.value = result.total
    loadedPage.value = r.page
    loadedDays.value = r.days
  } catch (e) {
    if (seq === reqSeq && (e as { status?: number }).status !== 401) err.value = '请求日志加载失败：' + (e as Error).message
  } finally {
    if (seq === reqSeq) loading.value = false
  }
}
watch(() => JSON.stringify(request.value), load, { immediate: true })

async function applyFilters() {
  const before = JSON.stringify(request.value)
  await router.push({ query: {
    ...route.query, status: fStatus.value || undefined, model: fModel.value.trim() || undefined,
    key: fKey.value || undefined, page: undefined,
  } })
  if (JSON.stringify(request.value) === before) await load()
}
function onModelBlur() { if (fModel.value.trim() !== request.value.model) void applyFilters() }
function resetFilters() {
  fStatus.value = ''; fModel.value = ''; fKey.value = ''
  void applyFilters()
}
function go(page: number) {
  if (loading.value || page < 1 || page > totalPages.value) return
  void router.push({ query: { ...route.query, page: page === 1 ? undefined : String(page) } })
}
async function loadKeys() {
  keyError.value = ''
  try { keys.value = await api.keys() }
  catch { keyError.value = '密钥筛选暂不可用' }
}
onMounted(loadKeys)

const parsedTrace = computed(() => parseRouteTrace(selected.value?.route_trace))

function duration(h: UsageHistoryEntry) {
  const first = h.ttfb_ms > 0 ? fmtSec(h.ttfb_ms) + 's' : '—'
  const total = h.latency_ms > 0 ? fmtSec(h.latency_ms) + 's' : '—'
  return first + '/' + total
}

/**
 * 速度格：tps = 0 表示没有可计算样本（上游未报 usage / 错误请求），
 * 不是「速度为零」。所以显示 —，不能把 0 直接印出来。
 */
function speedText(h: UsageHistoryEntry): string {
  return h.tps > 0 ? fmtSpeed(h.tps) : '—'
}
const exporting = ref(false)
const exportNotice = ref('')
async function exportCSV() {
  if (exporting.value) return
  exporting.value = true
  exportNotice.value = ''
  try {
    if (fModel.value.trim() !== request.value.model || fStatus.value !== request.value.status || fKey.value !== request.value.keyId) await applyFilters()
    const result = await api.exportUsageCSV(props.days, filters())
    if (result.truncated) {
      exportNotice.value = (result.total === undefined ? '' : '共 ' + fmtNum(result.total) + ' 条，') +
        '本次仅导出最近 1,000 条。请缩小时间范围或增加筛选后重新导出。'
    } else toast('CSV 已开始下载')
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('导出失败：' + (e as Error).message, 'err')
  } finally { exporting.value = false }
}
</script>

<template>
  <div class="usage-detail">
    <div class="data-toolbar log-toolbar">
      <label><span class="sr-only">请求状态</span><select v-model="fStatus" class="select" @change="applyFilters"><option value="">全部状态</option><option v-for="s in statuses" :key="s.value" :value="s.value">{{ s.label }}</option></select></label>
      <label class="search-field"><span class="sr-only">公开模型名</span><input v-model="fModel" class="input" placeholder="公开模型名（精确匹配）" @keyup.enter="onModelBlur" @change="onModelBlur" /></label>
      <label><span class="sr-only">调用密钥</span><select v-model="fKey" class="select" :disabled="!!keyError" @change="applyFilters"><option value="">全部密钥</option><option v-for="k in keys" :key="k.id" :value="k.id">{{ k.name || k.key_prefix }}</option></select></label>
      <button class="btn btn-sm" @click="applyFilters">应用</button><button class="btn btn-ghost btn-sm" @click="resetFilters">重置</button>
    </div>
    <div class="log-actions"><span class="dim">按时间倒序 · 共 {{ fmtNum(total) }} 条</span><div class="head-actions"><label><span class="sr-only">每页条数</span><select v-model.number="pageSize" class="select w-auto"><option :value="20">20 条/页</option><option :value="50">50 条/页</option><option :value="100">100 条/页</option></select></label><button class="btn btn-sm" :disabled="exporting || loading" @click="exportCSV">{{ exporting ? '导出中…' : '导出 CSV' }}</button><button class="btn btn-sm" :disabled="loading" @click="load">刷新</button></div></div>
    <p v-if="keyError" class="notice-warn" role="status">{{ keyError }}<button class="text-link" @click="loadKeys">重试</button></p>
    <p v-if="exportNotice" class="notice-warn" role="alert">{{ exportNotice }}</p>
    <DataTable class="panel table-panel" label="请求日志" :loading="loading" :error="err ? err + (rows.length ? '（近 ' + loadedDays + ' 天，第 ' + loadedPage + ' 页）' : '') : ''" :has-data="!!rows.length" :empty="!rows.length" empty-text="当前条件下没有调用记录" loading-text="正在加载请求日志…" @retry="load">
        <table class="tbl logs-table">
          <thead><tr><th>时间</th><th>状态</th><th>调用模型 / 实际模型</th><th class="key-column">调用密钥</th><th class="num-h usage-column">Tokens / 费用</th><th class="num-h speed-column">速度(t/s)</th><th class="num-h timing-column">首字 / 总耗时</th><th>详情</th></tr></thead>
          <tbody><tr v-for="(h, i) in rows" :key="h.ts + ':' + i">
            <td class="mono time-column">{{ fmtTimeMs(h.ts) }}</td>
            <td><button class="status-button" :aria-label="'查看' + statusLabel(h.status) + '请求详情'" @click="selected = h"><span class="badge" :class="statusBadge(h.status)">{{ statusLabel(h.status) }}</span></button></td>
            <td class="model-column"><span class="mobile-log-time dim">{{ fmtTimeMs(h.ts) }}</span><button class="text-link mono model-name" @click="selected = h">{{ h.public_model || '—' }}</button><span class="cell-meta mono" :title="h.upstream_model || '实际模型未记录'"> / {{ h.upstream_model || '实际模型未记录' }}</span></td>
            <td class="key-column"><span class="key-name" :title="h.key_name || h.key_id || '—'">{{ h.key_name || h.key_id || '—' }}</span><span class="cell-meta"> · {{ h.stream ? '流式' : '非流式' }}</span></td>
            <td class="num usage-column">{{ fmtTokens(h.total_tokens) }}<span class="cell-meta"> · ¥{{ fmtMoney(h.cost) }}</span></td>
            <td class="num speed-column">{{ speedText(h) }}</td>
            <td class="num timing-column">{{ duration(h) }}</td>
            <td><button class="btn btn-act" :aria-label="'查看 ' + (h.public_model || '请求') + ' 详情'" @click="selected = h">详情</button></td>
          </tr></tbody>
        </table>
      <template #footer><div class="pager"><button class="btn btn-sm" :disabled="request.page <= 1 || loading" @click="go(1)">首页</button><button class="btn btn-sm" :disabled="request.page <= 1 || loading" @click="go(request.page - 1)">上一页</button><span class="pager-info">第 {{ request.page }} / {{ totalPages }} 页</span><button class="btn btn-sm" :disabled="request.page >= totalPages || loading" @click="go(request.page + 1)">下一页</button><button class="btn btn-sm" :disabled="request.page >= totalPages || loading" @click="go(totalPages)">末页</button></div></template>
    </DataTable>

    <DetailDrawer :open="!!selected" title="请求详情" max-width="680px" @close="selected = null">
      <template v-if="selected">
        <div class="request-heading"><span class="badge" :class="statusBadge(selected.status)">{{ statusLabel(selected.status) }}</span><span class="dim">{{ fmtTimeMs(selected.ts) }} · {{ selected.stream ? '流式' : '非流式' }}</span></div>
        <dl class="request-facts">
          <div><dt>公开模型名</dt><dd class="mono">{{ selected.public_model || '—' }}</dd></div>
          <div><dt>实际模型</dt><dd class="mono">{{ selected.upstream_model || '未记录' }}</dd></div>
          <div><dt>调用密钥</dt><dd>{{ selected.key_name || selected.key_id || '未记录' }}</dd></div>
          <div><dt>总 Tokens / 输出</dt><dd>{{ fmtNum(selected.total_tokens) }} / {{ fmtNum(selected.output_tokens) }}</dd></div>
          <div><dt>记录费用</dt><dd>¥{{ fmtMoney(selected.cost) }}<span class="fact-note">按调用当时单价固化；0 元不能单独说明是否配价</span></dd></div>
          <div><dt>首字 / 总耗时</dt><dd>{{ duration(selected) }}</dd></div>
          <div><dt>输出速度</dt><dd>{{ selected.tps > 0 ? fmtSpeed(selected.tps) + ' tok/s' : '无可计算样本' }}</dd></div>
          <div v-if="selected.http_status > 0"><dt>HTTP 状态</dt><dd>{{ selected.http_status }}</dd></div>
          <div v-if="selected.error_code"><dt>错误码</dt><dd class="mono">{{ selected.error_code }}</dd></div>
        </dl>
        <section class="detail-section">
          <h2>转发决策轨迹</h2>
          <template v-if="parsedTrace.length">
            <p class="context-note">编号表示本次请求的决策顺序，包含跳过的目标。配置位置与实际调用次数可能不同。</p>
            <ol class="trace-list">
              <li v-for="(step, i) in parsedTrace" :key="i">
                <span class="trace-number">{{ step.attempt ?? i + 1 }}</span>
                <div v-if="step.parsed" class="trace-body">
                  <div class="trace-top"><strong>{{ step.provider }}</strong><span class="badge" :class="step.outcome === 'ok' ? 'badge-live' : step.outcome === 'skip' ? 'badge-dim' : 'badge-off'">{{ step.outcomeLabel }}</span></div>
                  <div class="mono trace-model">{{ step.model }}</div>
                  <div v-if="step.reasonLabel" class="trace-reason">{{ step.reasonLabel }}</div>
                  <p v-if="step.explanation" class="trace-explanation">{{ step.explanation }}</p>
                  <details class="trace-raw"><summary>原始记录</summary><code>{{ step.raw }}</code></details>
                </div>
                <div v-else class="trace-body"><p class="trace-explanation">记录格式无法解析，保留原文供排查。</p><code>{{ step.raw }}</code></div>
              </li>
            </ol>
          </template>
          <p v-else class="context-note">此记录未提供转发轨迹，无法从中还原完整尝试过程。</p>
        </section>
        <section v-if="selected.status !== 'ok' || selected.error_detail" class="detail-section"><h2>错误详情</h2><pre v-if="selected.error_detail" class="error-detail">{{ selected.error_detail }}</pre><p v-else class="context-note">没有更多错误详情：上游可能未返回正文，或原文已超出保留期。</p></section>
      </template>
    </DetailDrawer>
  </div>
</template>

<style scoped>
.log-toolbar { margin-bottom: 16px; }
.log-actions, .request-heading { display: flex; justify-content: space-between; align-items: center; gap: 12px; flex-wrap: wrap; font-size: 12px; margin-bottom: 16px; }
.w-auto { width: auto; }
.logs-table { min-width: 980px; }
.logs-table th, .logs-table td { white-space: nowrap; }
/* 表格内的 mono 文本**必须**在这里重新钉成 nowrap。
   全局 styles.css 有一条 `.table-panel .tbl .mono { white-space: normal;
   overflow-wrap: anywhere }`（3 个 class），而本组件 scoped 的
   `.cell-meta` / `.model-name` 只带 2 个 class + 1 个 data-v 属性，
   特异性打成平手 —— 谁生效取决于 chunk CSS 的插入顺序，而懒加载 chunk
   是**后插** <link> 的，看起来能用，实际是顺序碰巧。一旦上游模型名够长
   （实测 `nvidia/nemotron-3.5-lightning:free`，34 字符），
   `.cell-meta` 就按 normal 折成两行：省略号永远不出现（先换行再溢出），
   行高从 38px 撑到 50px —— 「单行」在长名字上直接失效。
   多带一个 class 把特异性抬到 (0,4,0)，就不必再赌注入顺序。 */
.logs-table .model-name.mono,
.logs-table .cell-meta.mono { white-space: nowrap; }
/* 请求日志是逐条扫描为主，行密度优先。全局 .table-panel td 给 16px
   上下内边距是按「每格有副标题」的形态定的；本表已把副标题折进主行，
   仍沿用会让每行 ~60px，一屏少看近一半记录。 */
.logs-table td { padding-top: 6px; padding-bottom: 6px; vertical-align: middle; }
.mobile-log-time { display: none; }
.logs-table .time-column { width: 142px; white-space: nowrap; }
.model-column { min-width: 0; max-width: 300px; }
.model-name { text-align: left; font-weight: 600; overflow-wrap: normal; }
.key-name { max-width: 160px; overflow: hidden; text-overflow: ellipsis; display: inline-block; vertical-align: bottom; }
/* max-width 只做「上限」，实际宽度由 table 布局分配；它同时是省略号的生效条件
   （inline-block + overflow:hidden + text-overflow:ellipsis 缺一不可）。
   这两个上限是一对约束，改一个必须重量另一个：1280px 视口下表格可用宽 ~997px，
   各定宽列（时间 142 + 状态 85 + 密钥 ~107 + Tokens 126 + 速度 86 + 耗时 112
   + 详情 84）已占 ~742px，留给「模型 / 实际模型」的只有 ~255px。
   实测 model-column 360 + meta 190 时，1280 视口溢出 47px（整表被推到
   980px 的 min-width 上，出现横向滚动条）；改成 300 + 190 后溢出为 0。 */
.cell-meta { color: var(--muted); font-size: 12px; overflow: hidden; text-overflow: ellipsis; display: inline-block; max-width: 190px; vertical-align: bottom; }
.speed-column { width: 86px; }
.timing-column { width: 112px; }
.status-button { border: 0; background: transparent; padding: 0; cursor: pointer; }
.request-facts { margin: 20px 0; }
.request-facts > div { display: grid; grid-template-columns: 120px minmax(0, 1fr); gap: 16px; padding: 12px 0; border-bottom: 1px solid var(--border); }
.request-facts dt { color: var(--muted); font-size: 12px; }
.request-facts dd { margin: 0; font-size: 13px; overflow-wrap: anywhere; }
.fact-note { display: block; margin-top: 6px; font-size: 11px; color: var(--muted); }
.detail-section { margin: 24px 0; }
.detail-section h2 { font-size: 14px; margin-bottom: 14px; }
.trace-list { list-style: none; padding: 0; display: grid; gap: 10px; }
.trace-list li { display: flex; gap: 12px; padding: 12px; background: var(--surface-secondary); border-radius: 10px; font-size: 12px; }
.trace-list code { min-width: 0; white-space: pre-wrap; overflow-wrap: anywhere; }
.trace-number { flex-shrink: 0; width: 24px; height: 24px; display: grid; place-items: center; border-radius: 50%; background: var(--surface); color: var(--muted); }
.trace-body { min-width: 0; flex: 1; }
.trace-top { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; overflow-wrap: anywhere; }
.trace-model { margin-top: 6px; overflow-wrap: anywhere; }
.trace-reason { line-height: 1.7; margin-top: 8px; overflow-wrap: anywhere; }
.trace-explanation { font-size: 12px; line-height: 1.7; color: var(--muted); margin: 6px 0 0; }
.trace-raw { margin-top: 8px; color: var(--muted); }
.trace-raw summary { cursor: pointer; padding: 4px 0; }
.trace-raw code { display: block; margin-top: 6px; }
.error-detail { white-space: pre-wrap; overflow-wrap: anywhere; border-radius: 10px; padding: 14px; font-size: 12px; line-height: 1.7; background: var(--danger-soft); color: var(--danger-soft-foreground); }
.notice-warn { margin: 12px 0; padding: 12px; display: flex; flex-wrap: wrap; align-items: center; gap: 12px; border-radius: 10px; background: var(--warning-soft); color: var(--warning-soft-foreground); font-size: 12px; }
@media (max-width: 1100px) { .logs-table { min-width: 850px; } .key-column { display: none; } }
@media (max-width: 640px) {
  .logs-table { min-width: 0; table-layout: fixed; }
  .usage-column, .speed-column, .timing-column { display: none; }
  .logs-table th:first-child, .logs-table .time-column, .logs-table th:last-child, .logs-table td:last-child { display: none; }
  .logs-table th:nth-child(2), .logs-table td:nth-child(2) { width: 76px; }
  .model-column { min-width: 0; max-width: none; }
  .mobile-log-time { display: block; font-size: 11px; margin-bottom: 3px; }
  .logs-table .model-column { white-space: normal; }
  .logs-table .mono { white-space: normal; overflow-wrap: anywhere; }
  .log-actions > .head-actions { flex-wrap: wrap; }
  .request-facts > div { grid-template-columns: 90px minmax(0, 1fr); gap: 10px; }
}
</style>
