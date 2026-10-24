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
import { protocolLabel } from '../protocols'

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
  { value: 'timeout', label: '网关超时' },
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
const copyNotice = ref('')

/** 首字超过 10s 红，5–10s 橙，5s 内绿；没有样本保持中性。 */
function firstTokenTone(h: UsageHistoryEntry): string {
  if (h.ttfb_ms <= 0) return 'latency-none'
  if (h.ttfb_ms <= 5000) return 'latency-good'
  if (h.ttfb_ms <= 10000) return 'latency-warn'
  return 'latency-bad'
}

/** 总耗时超过 30s 红，10–30s 橙，10s 内绿；没有样本保持中性。 */
function totalTimeTone(h: UsageHistoryEntry): string {
  if (h.latency_ms <= 0) return 'latency-none'
  if (h.latency_ms <= 10000) return 'latency-good'
  if (h.latency_ms <= 30000) return 'latency-warn'
  return 'latency-bad'
}

function timingTitle(h: UsageHistoryEntry): string {
  return `首字 ${h.ttfb_ms > 0 ? fmtSec(h.ttfb_ms) + ' 秒' : '无样本'} | 总耗时 ${h.latency_ms > 0 ? fmtSec(h.latency_ms) + ' 秒' : '无样本'}`
}

function routeName(h: UsageHistoryEntry): string {
  return h.public_model || '—'
}

function upstreamModelName(h: UsageHistoryEntry): string {
  return h.upstream_model || '实际模型未记录'
}

/**
 * 实际接单的上游展示名。provider_name 是落库时固化的历史值；旧记录没有
 * 这个字段，回退到 provider_id 并明确标注，避免让人误以为那就是名字。
 */
function upstreamName(h: UsageHistoryEntry): string {
  return h.provider_name || h.provider_id || '上游未记录'
}

function upstreamNameTitle(h: UsageHistoryEntry): string {
  if (h.provider_name) return h.provider_id ? `${h.provider_name}（provider_id: ${h.provider_id}）` : h.provider_name
  if (h.provider_id) return `旧记录未固化上游名称，provider_id: ${h.provider_id}`
  return '这条记录没有归因到具体上游'
}

function upstreamPathTitle(h: UsageHistoryEntry): string {
  const steps = traceSteps(h)
  return steps.length ? traceTitle(h) : upstreamNameTitle(h)
}

function protocolPairTitle(h: UsageHistoryEntry): string {
  return `上游协议：${protocolLabel(h.upstream_protocol, '未记录')}；入站协议：${protocolLabel(h.ingress_protocol, '未记录')}`
}

function traceSteps(h: UsageHistoryEntry) {
  return parseRouteTrace(h.route_trace)
}

function traceNodeClass(step: ReturnType<typeof parseRouteTrace>[number]): string {
  if (step.outcome === 'ok') return 'path-ok'
  if (step.outcome === 'skip') return 'path-skip'
  return 'path-fail'
}

function traceTitle(h: UsageHistoryEntry): string {
  const steps = traceSteps(h)
  if (!steps.length) return '这条记录没有转发决策轨迹'
  return steps.map(step => {
    if (!step.parsed) return step.raw
    return `${step.attempt}. ${step.provider}/${step.model} · ${step.outcomeLabel}${step.reasonLabel ? ' · ' + step.reasonLabel : ''}`
  }).join('\n')
}

/**
 * 错误详情分两段：上游那句 message 与完整 raw body。老记录可能只有一段
 * 裸文本/裸 JSON，这里统一识别，交给模板分区渲染。
 */
const errorView = computed(() => {
  const value = selected.value?.error_detail?.trim() ?? ''
  if (!value) return { message: '', raw: '', rawPretty: '' }
  let message = ''
  let raw = ''
  if (value.startsWith('message: ')) {
    const rawAt = value.indexOf('\nraw: ')
    if (rawAt >= 0) {
      message = value.slice('message: '.length, rawAt)
      raw = value.slice(rawAt + '\nraw: '.length)
    } else {
      message = value.slice('message: '.length)
    }
  } else if (value.startsWith('raw: ')) {
    raw = value.slice('raw: '.length)
  } else {
    message = value
  }
  const looksJSON = message.trimStart().startsWith('{') || message.trimStart().startsWith('[')
  if (!raw && looksJSON) {
    raw = message
    message = ''
  }
  let rawPretty = raw
  if (raw) {
    try { rawPretty = JSON.stringify(JSON.parse(raw), null, 2) } catch { /* 不是合法 JSON 就保留原文 */ }
  }
  return { message, raw, rawPretty }
})

async function copyError() {
  const text = selected.value?.error_detail ?? ''
  if (!text) return
  try {
    await navigator.clipboard.writeText(text)
    copyNotice.value = '已复制'
  } catch {
    copyNotice.value = '复制失败，请手动选择文本'
  }
  setTimeout(() => { copyNotice.value = '' }, 1800)
}

/**
 * 速度格：tps = 0 表示没有可计算样本（上游未报 usage / 错误请求），
 * 不是「速度为零」。所以显示 —，不能把 0 直接印出来。
 */
function speedText(h: UsageHistoryEntry): string {
  return h.tps > 0 ? fmtSpeed(h.tps) + ' t/s' : '—'
}

function modeLabel(h: UsageHistoryEntry): string {
  return h.stream ? '流式' : '非流式'
}

/**
 * 统一 Usage 的 input_tokens 是输入总量，包含缓存命中和缓存写入。
 * 页面上的“新增输入”只展示本次真正新增处理、未命中缓存的输入 token。
 */
function newInputTokens(h: UsageHistoryEntry): number {
  return Math.max(0, h.input_tokens - h.cached_tokens - h.cached_creation_tokens)
}

function tokenTitle(h: UsageHistoryEntry): string {
  return '新增输入 ' + fmtNum(newInputTokens(h)) +
    ' · 输出 ' + fmtNum(h.output_tokens) +
    ' · 缓存命中 ' + fmtNum(h.cached_tokens) +
    ' · 缓存写入 ' + fmtNum(h.cached_creation_tokens) +
    ' · 推理 ' + fmtNum(h.reasoning_tokens) +
    ' · 总计 ' + fmtNum(h.total_tokens)
}

function statusHint(h: UsageHistoryEntry): string {
  const parts = [statusLabel(h.status)]
  if (h.http_status > 0) parts.push('HTTP ' + h.http_status)
  if (h.error_code) parts.push(h.error_code)
  return parts.join(' · ')
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
  <div class="usage-detail logs-page">
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
          <thead><tr>
            <th class="time-column">时间</th><th class="mode-column">模式</th><th class="protocol-column">协议</th><th class="upstream-column">上游</th><th class="model-column">模型</th><th class="key-column">密钥</th><th class="token-column">Token 用量</th><th class="speed-column">速度</th><th class="timing-column">时间</th><th class="cost-column">费用</th><th class="status-column">状态</th>
          </tr></thead>
          <tbody><tr v-for="(h, i) in rows" :key="h.ts + ':' + i">
            <td class="mono time-column">{{ fmtTimeMs(h.ts) }}</td>
            <td class="mode-column"><span class="badge" :class="h.stream ? 'badge-accent' : 'badge-dim'">{{ modeLabel(h) }}</span></td>
            <td class="protocol-column" :title="protocolPairTitle(h)">
              <span class="protocol-stack">
                <span class="protocol-line"><span class="protocol-dir">上游</span><span class="protocol-tag mono">{{ protocolLabel(h.upstream_protocol, '未记录') }}</span></span>
                <span class="protocol-line"><span class="protocol-dir">下游</span><span class="protocol-tag mono">{{ protocolLabel(h.ingress_protocol, '未记录') }}</span></span>
              </span>
            </td>
            <td class="upstream-column" :title="upstreamPathTitle(h)">
              <span v-if="traceSteps(h).length" class="path-flow">
                <template v-for="(step, si) in traceSteps(h)" :key="si">
                  <span class="path-node" :class="traceNodeClass(step)">{{ step.parsed ? step.provider : '记录异常' }}</span>
                  <span v-if="si < traceSteps(h).length - 1" class="path-arrow" aria-hidden="true">→</span>
                </template>
              </span>
              <template v-else>
                <span class="upstream-name">{{ upstreamName(h) }}</span>
                <span v-if="!h.provider_name && h.provider_id" class="cell-note">名称未固化</span>
              </template>
            </td>
            <td class="model-column">
              <span class="mobile-log-time dim">{{ fmtTimeMs(h.ts) }}</span>
              <span class="model-stack">
                <span class="model-route mono" :title="routeName(h)">{{ routeName(h) }}</span>
                <span class="model-upstream mono" :title="upstreamModelName(h)">{{ upstreamModelName(h) }}</span>
              </span>
            </td>
            <td class="key-column"><span class="key-name" :title="h.key_name || h.key_id || '—'">{{ h.key_name || h.key_id || '—' }}</span></td>
            <td class="token-column" :title="tokenTitle(h)">
              <span class="token-top"><span class="token-label">新增输入</span><span>{{ fmtTokens(newInputTokens(h)) }}</span><span class="metric-sep">|</span><span class="token-label">输出</span><span>{{ fmtTokens(h.output_tokens) }}</span></span>
              <span class="cell-note">缓存命中 {{ fmtTokens(h.cached_tokens) }}</span>
            </td>
            <td class="speed-column">{{ speedText(h) }}</td>
            <td class="timing-column">
              <span class="timing-line" :title="timingTitle(h)">
                <span :class="firstTokenTone(h)">{{ h.ttfb_ms > 0 ? fmtSec(h.ttfb_ms) : '—' }}</span>
                <span class="metric-sep">|</span>
                <span :class="totalTimeTone(h)">{{ h.latency_ms > 0 ? fmtSec(h.latency_ms) : '—' }}</span>
              </span>
            </td>
            <td class="cost-column">¥{{ fmtMoney(h.cost) }}</td>
            <td class="status-column">
              <button class="status-button" :title="statusHint(h) + ' · 点击查看完整详情'" :aria-label="'查看' + statusLabel(h.status) + '请求详情'" @click="selected = h">
                <span class="badge" :class="statusBadge(h.status)">{{ statusLabel(h.status) }}</span>
                <span v-if="h.http_status > 0" class="cell-note">HTTP {{ h.http_status }}</span>
              </button>
            </td>
          </tr></tbody>
        </table>
      <template #footer><div class="pager"><button class="btn btn-sm" :disabled="request.page <= 1 || loading" @click="go(1)">首页</button><button class="btn btn-sm" :disabled="request.page <= 1 || loading" @click="go(request.page - 1)">上一页</button><span class="pager-info">第 {{ request.page }} / {{ totalPages }} 页</span><button class="btn btn-sm" :disabled="request.page >= totalPages || loading" @click="go(request.page + 1)">下一页</button><button class="btn btn-sm" :disabled="request.page >= totalPages || loading" @click="go(totalPages)">末页</button></div></template>
    </DataTable>

    <DetailDrawer :open="!!selected" title="请求详情" max-width="680px" @close="selected = null">
      <template v-if="selected">
        <div class="request-heading"><span class="badge" :class="statusBadge(selected.status)">{{ statusLabel(selected.status) }}</span><span class="dim">{{ fmtTimeMs(selected.ts) }} · {{ modeLabel(selected) }} · {{ protocolLabel(selected.ingress_protocol) }} → {{ protocolLabel(selected.upstream_protocol) }}</span></div>
        <dl class="request-facts">
          <div><dt>请求 ID</dt><dd class="mono">{{ selected.request_id || '未记录' }}</dd></div>
          <div><dt>上游</dt><dd><span class="fact-strong">{{ upstreamName(selected) }}</span><span v-if="selected.provider_id" class="fact-note mono">provider_id: {{ selected.provider_id }}</span></dd></div>
          <div><dt>上游协议 / 入站协议</dt><dd>{{ protocolLabel(selected.upstream_protocol, '未记录') }} / {{ protocolLabel(selected.ingress_protocol, '未记录') }}<span class="fact-note">上行是网关连接上游使用的协议；下行是客户端调用网关使用的协议。</span></dd></div>
          <div><dt>公开模型名</dt><dd class="mono">{{ selected.public_model || '—' }}</dd></div>
          <div><dt>实际模型</dt><dd class="mono">{{ selected.upstream_model || '未记录' }}</dd></div>
          <div><dt>调用密钥</dt><dd>{{ selected.key_name || selected.key_id || '未记录' }}</dd></div>
          <div><dt>Token 用量</dt><dd>新增输入 {{ fmtNum(newInputTokens(selected)) }} · 输出 {{ fmtNum(selected.output_tokens) }} · 总计 {{ fmtNum(selected.total_tokens) }}<span class="fact-note">新增输入 = 输入总量 - 缓存命中 - 缓存写入</span></dd></div>
          <div><dt>缓存命中 / 写入 / 推理</dt><dd>{{ fmtNum(selected.cached_tokens) }} / {{ fmtNum(selected.cached_creation_tokens) }} / {{ fmtNum(selected.reasoning_tokens) }} Tokens</dd></div>
          <div><dt>记录费用</dt><dd>¥{{ fmtMoney(selected.cost) }}<span class="fact-note">按调用当时单价固化；0 元不能单独说明是否配价</span></dd></div>
          <div>
            <dt>首字 / 总耗时</dt>
            <dd class="fact-timing">
              <span :class="firstTokenTone(selected)">首字 {{ selected.ttfb_ms > 0 ? fmtSec(selected.ttfb_ms) + 's' : '—' }}</span>
              <span :class="totalTimeTone(selected)">总耗时 {{ selected.latency_ms > 0 ? fmtSec(selected.latency_ms) + 's' : '—' }}</span>
            </dd>
          </div>
          <div><dt>输出速度</dt><dd>{{ selected.tps > 0 ? fmtSpeed(selected.tps) + ' tok/s' : '无可计算样本' }}</dd></div>
          <div v-if="selected.http_status > 0"><dt>HTTP 状态</dt><dd>{{ selected.http_status }}</dd></div>
          <div v-if="selected.error_code"><dt>错误码</dt><dd class="mono">{{ selected.error_code }}</dd></div>
        </dl>
        <section class="detail-section">
          <div class="section-head"><h2>转发决策轨迹</h2><span v-if="parsedTrace.length > 1" class="section-note">共 {{ parsedTrace.length }} 个决策</span></div>
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
        <section v-if="selected.status !== 'ok' || selected.error_detail || selected.error_code" class="detail-section">
          <div class="section-head"><h2>错误详情</h2><button v-if="selected.error_detail" class="btn btn-ghost btn-sm" @click="copyError">{{ copyNotice || '复制完整错误' }}</button></div>
          <div class="error-summary">
            <div><span class="error-k">HTTP</span><strong>{{ selected.http_status || '—' }}</strong></div>
            <div><span class="error-k">错误码</span><code>{{ selected.error_code || '未记录' }}</code></div>
            <div><span class="error-k">终端</span><strong>{{ parsedTrace.at(-1)?.provider || upstreamName(selected) }}</strong></div>
          </div>
          <div v-if="errorView.message" class="error-block">
            <h3>上游返回的说明</h3>
            <pre class="error-message">{{ errorView.message }}</pre>
          </div>
          <div v-if="errorView.raw" class="error-block">
            <h3>完整原始响应</h3>
            <pre class="error-raw">{{ errorView.rawPretty }}</pre>
          </div>
          <p v-if="!errorView.message && !errorView.raw" class="context-note">没有可展示的上游正文：这类失败可能在连接阶段发生，或上游未返回错误体。上面的错误码与转发轨迹仍可用于定位。</p>
        </section>
      </template>
    </DetailDrawer>
  </div>
</template>

<style scoped>
.logs-page {
  width: 100%;
  max-width: none;
  margin: 0;
}
.log-toolbar { margin-bottom: 16px; }
.log-actions, .request-heading { display: flex; justify-content: space-between; align-items: center; gap: 12px; flex-wrap: wrap; font-size: 12px; margin-bottom: 16px; }
.w-auto { width: auto; }
.logs-table { min-width: 1360px; table-layout: fixed; }
.logs-table th, .logs-table td { white-space: nowrap; }
.logs-table th, .logs-table td { text-align: left; }
/* 请求日志是逐条扫描为主，行密度优先。全局 .table-panel td 给 16px
   上下内边距是按「每格有副标题」的形态定的；本表已把副标题折进主行，
   仍沿用会让每行 ~60px，一屏少看近一半记录。 */
.logs-table th { padding-left: 12px; padding-right: 12px; }
.logs-table td { padding: 9px 12px; vertical-align: middle; }
.mobile-log-time { display: none; }
.logs-table .time-column { width: 126px; white-space: nowrap; }
.logs-table th { overflow: hidden; text-overflow: ellipsis; }
.mode-column { width: 70px; }
.protocol-column { width: 104px; }
.upstream-column { width: 190px; min-width: 0; }
.upstream-name { display: block; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 600; }
.model-column { width: 238px; min-width: 0; }
.model-stack { display: grid; min-width: 0; gap: 3px; }
.model-route, .model-upstream { display: block; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.model-route { font-weight: 650; }
.model-upstream { color: var(--muted); font-size: 11.5px; }
.path-flow { display: flex; align-items: center; gap: 4px; min-width: 0; overflow: hidden; }
.path-node { max-width: 76px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.path-arrow { color: var(--muted); flex-shrink: 0; }
.path-ok { color: var(--success-soft-foreground); }
.path-fail { color: var(--danger); }
.path-skip { color: var(--muted); }
.key-column { width: 112px; }
.key-name { max-width: 100%; overflow: hidden; text-overflow: ellipsis; display: inline-block; vertical-align: bottom; }
.cell-note { display: block; margin-top: 3px; color: var(--muted); font-size: 10.5px; line-height: 1.35; overflow: hidden; text-overflow: ellipsis; }
.protocol-stack { display: grid; gap: 3px; }
.protocol-line { display: flex; align-items: baseline; gap: 5px; min-width: 0; }
.protocol-dir { flex-shrink: 0; color: var(--muted); font-size: 10.5px; }
.protocol-tag { display: inline-block; max-width: 70px; overflow: hidden; text-overflow: ellipsis; vertical-align: bottom; color: var(--muted); }
.token-column { width: 164px; }
.token-top { display: flex; gap: 6px; white-space: nowrap; font-variant-numeric: tabular-nums; }
.token-label { color: var(--muted); font-size: 10.5px; }
.metric-sep { color: var(--separator); }
.speed-column { width: 82px; }
.timing-column { width: 96px; }
.timing-line { display: flex; gap: 6px; white-space: nowrap; font-variant-numeric: tabular-nums; }
.latency-good { color: var(--success-soft-foreground); }
.latency-warn { color: var(--warning-soft-foreground); }
.latency-bad { color: var(--danger); }
.latency-none { color: var(--muted); }
.cost-column { width: 84px; }
.status-column { width: 92px; }
.status-button { border: 0; background: transparent; padding: 0; cursor: pointer; display: inline-flex; flex-direction: column; align-items: flex-start; gap: 2px; max-width: 88px; }
.status-button:hover .badge { filter: brightness(0.96); }
.request-facts { margin: 20px 0; }
.request-facts > div { display: grid; grid-template-columns: 120px minmax(0, 1fr); gap: 16px; padding: 12px 0; border-bottom: 1px solid var(--border); }
.request-facts dt { color: var(--muted); font-size: 12px; }
.request-facts dd { margin: 0; font-size: 13px; overflow-wrap: anywhere; }
.fact-timing { display: flex; gap: 16px; flex-wrap: wrap; font-variant-numeric: tabular-nums; }
.fact-note { display: block; margin-top: 6px; font-size: 11px; color: var(--muted); }
.fact-strong { font-weight: 650; }
.detail-section { margin: 24px 0; }
.detail-section h2 { font-size: 14px; margin-bottom: 14px; }
.section-head { display: flex; justify-content: space-between; align-items: center; gap: 12px; flex-wrap: wrap; margin-bottom: 14px; }
.section-head h2 { margin: 0; }
.section-note { color: var(--muted); font-size: 11.5px; }
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
.error-summary { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 10px; margin-bottom: 14px; }
.error-summary > div { min-width: 0; padding: 10px 12px; border-radius: 9px; background: var(--surface-secondary); }
.error-k { display: block; color: var(--muted); font-size: 10.5px; margin-bottom: 4px; }
.error-summary code, .error-summary strong { overflow-wrap: anywhere; }
.error-block { margin-top: 14px; }
.error-block h3 { margin: 0 0 8px; font-size: 12px; color: var(--muted); font-weight: 600; }
.error-message, .error-raw { margin: 0; white-space: pre-wrap; overflow-wrap: anywhere; border-radius: 10px; padding: 14px; font-size: 12px; line-height: 1.7; }
.error-message { background: var(--danger-soft); color: var(--danger-soft-foreground); }
.error-raw { max-height: 52vh; overflow: auto; background: var(--surface-secondary); color: var(--text); font-family: var(--font-mono, monospace); }
.notice-warn { margin: 12px 0; padding: 12px; display: flex; flex-wrap: wrap; align-items: center; gap: 12px; border-radius: 10px; background: var(--warning-soft); color: var(--warning-soft-foreground); font-size: 12px; }
@media (max-width: 1500px) {
  .logs-table { min-width: 1360px; }
  .key-column { display: none; }
}
@media (max-width: 900px) {
  .logs-table { min-width: 920px; }
  .speed-column, .timing-column, .cost-column, .key-column { display: none; }
}
@media (max-width: 640px) {
  .logs-table { min-width: 0; table-layout: fixed; }
  .token-column, .speed-column, .timing-column, .cost-column, .protocol-column, .key-column, .time-column { display: none; }
  .mode-column { width: 64px; }
  .upstream-column { width: 34%; }
  .model-column { min-width: 0; max-width: none; }
  .model-stack { display: block; }
  .model-route, .model-upstream { display: block; max-width: none; white-space: normal; overflow-wrap: anywhere; }
  .mobile-log-time { display: block; font-size: 11px; margin-bottom: 3px; }
  .logs-table .model-column { white-space: normal; }
  .logs-table .mono { white-space: normal; overflow-wrap: anywhere; }
  .log-actions > .head-actions { flex-wrap: wrap; }
  .request-facts > div { grid-template-columns: 90px minmax(0, 1fr); gap: 10px; }
  .error-summary { grid-template-columns: 1fr; }
}
</style>
