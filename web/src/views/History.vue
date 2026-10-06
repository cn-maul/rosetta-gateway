<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api } from '../api'
import { toast } from '../ui'
import { fmtNum, fmtTokens, fmtTimeMs, fmtSec, statusLabel, statusBadge } from '../fmt'
import type { UsageHistoryEntry } from '../types'

const err = ref('')
const loading = ref(true)
const rows = ref<UsageHistoryEntry[]>([])
const days = ref(7)
const pageSize = ref(20)
const page = ref(1)
// 过滤后的总条数（后端返回，不受分页影响）—— 用来算总页数
const total = ref(0)

const ranges = [
  { label: '近 1 天', value: 1 },
  { label: '近 7 天', value: 7 },
  { label: '近 30 天', value: 30 },
]
const pageSizes = [20, 50, 100]

const totalPages = computed(() => Math.max(1, Math.ceil(total.value / pageSize.value)))

// 请求序号守卫：快速翻页 / 切时间范围会并发多个 fetch，晚到的旧响应若直接
// 写回 rows/page/total，会把新结果覆盖成过期数据（表内容与页码自相矛盾）。
// 每次请求 ++reqSeq，await 全部结束后只有「仍是最新序号」的请求才提交状态。
let reqSeq = 0

// fetchPage 取第 p 页（1 基）。
//
// 越界保护：数据变少（或切到更窄的时间范围）时当前页可能已不存在，
// 此时退到最后一页重取，而不是让用户停在一张空白表上 —— 空白表看起来
// 和「这个范围内没有记录」一模一样，会误导人。
async function fetchPage(p: number) {
  const seq = ++reqSeq
  let res = await api.usageHistory(days.value, pageSize.value, (p - 1) * pageSize.value)
  let target = p
  const last = Math.max(1, Math.ceil(res.total / pageSize.value))
  if (p > last) {
    res = await api.usageHistory(days.value, pageSize.value, (last - 1) * pageSize.value)
    target = last
  }
  if (seq !== reqSeq) return // 期间又发起了新请求，本响应的数据已过时，丢弃
  page.value = target
  rows.value = res.records
  total.value = res.total
}

async function load() {
  loading.value = true
  err.value = ''
  try {
    await fetchPage(page.value)
  } catch (e) {
    if ((e as { status?: number }).status === 401) return
    // 失败**必须留下可见的错误态**：只弹 toast 的话，页面保留空列表/
    // 空表格，呈现成「暂无数据」—— 而真实原因是请求失败了。运维会据此
    // 判断「今天没有流量」，进而排除掉网关/上游故障这个真正的方向。
    // 与 Users/Groups 的持久错误态同口径（见其 v-else-if="err"）。
    err.value = '加载调用历史失败：' + (e as Error).message
    toast('加载调用历史失败：' + (e as Error).message, 'err')
  } finally {
    loading.value = false
  }
}

// 换时间范围 / 换每页条数都必须回到第 1 页：留在原页码毫无意义，
// 而且极可能直接越界（原来在第 5 页，换成「近 1 天」后只有 1 页）。
function changeDays() {
  page.value = 1
  load()
}
function changePageSize() {
  page.value = 1
  load()
}
function go(p: number) {
  if (p < 1 || p > totalPages.value || p === page.value) return
  fetchPage(p).catch((e) => {
    if ((e as { status?: number }).status !== 401) toast('加载调用历史失败：' + (e as Error).message, 'err')
  })
}

onMounted(load)
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>调用历史</h1>
        <div class="sub">请求明细（按时间倒序，可分页）</div>
      </div>
      <div class="head-actions">
        <select v-model.number="days" class="select w-auto" @change="changeDays">
          <option v-for="r in ranges" :key="r.value" :value="r.value">{{ r.label }}</option>
        </select>
        <select v-model.number="pageSize" class="select w-auto" @change="changePageSize">
          <option v-for="s in pageSizes" :key="s" :value="s">{{ s }} 条/页</option>
        </select>
        <button class="btn" :disabled="loading" @click="load">刷新</button>
      </div>
    </div>

    <div class="panel">
      <div v-if="loading && rows.length === 0" class="loading">加载中…</div>
      <!--加载失败**必须**与「确实没有数据」在界面上可区分：把请求失败呈现成
           「暂无数据」会让运维误判为无流量，从而排除掉网关/上游故障这个方向。
           与 Users/Groups 的错误态同口径。 -->
      <div v-else-if="err" class="empty"><div class="big">⚠</div>{{ err }}</div>
      <div v-else-if="total === 0" class="empty">
        <div class="big">⌗</div>
        该时间范围内暂无调用记录
      </div>
      <template v-else>
        <div class="tbl-wrap">
          <table class="tbl">
            <thead>
              <tr>
                <th class="c-time">时间</th>
                <th>调用模型</th>
                <th>实际模型</th>
                <th>调用密钥</th>
                <th class="num-h">Tokens</th>
                <th class="num-h">首字(s)</th>
                <th class="num-h">总耗时(s)</th>
                <th class="c-st">状态</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="(h, i) in rows" :key="i">
                <td class="mono c-time">{{ fmtTimeMs(h.ts) }}</td>
                <td class="mono">{{ h.public_model || '—' }}</td>
                <td class="mono dim">{{ h.upstream_model || '—' }}</td>
                <td>{{ h.key_name || h.key_id || '—' }}</td>
                <td class="num-h">{{ fmtTokens(h.total_tokens) }}</td>
                <td class="num-h">{{ h.ttfb_ms > 0 ? fmtSec(h.ttfb_ms) : '—' }}</td>
                <td class="num-h">{{ h.latency_ms > 0 ? fmtSec(h.latency_ms) : '—' }}</td>
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
.tbl-wrap {
  overflow-x: auto;
}
.tbl {
  width: 100%;
  border-collapse: collapse;
  font-size: 13px;
}
.tbl th,
.tbl td {
  text-align: left;
  padding: 9px 12px;
  white-space: nowrap;
}
.tbl thead th {
  color: var(--text-3);
  font-weight: 500;
  font-size: 12px;
  border-bottom: 1px solid var(--separator);
}
.tbl tbody tr {
  border-bottom: 1px solid var(--separator);
}
.tbl tbody tr:last-child {
  border-bottom: none;
}
.tbl tbody tr:hover {
  background: var(--hover);
}
.c-time {
  color: var(--text-2);
}
.c-st {
  width: 72px;
}
.num-h {
  text-align: right;
}
.dim {
  color: var(--text-3);
}
/* 分页条：与表头分隔线对齐，视觉上属于表格的页脚 */
.pager {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 8px;
  flex-wrap: wrap;
  margin-top: 14px;
  padding-top: 12px;
  border-top: 1px solid var(--separator);
}
.pager-info {
  font-size: 12.5px;
  color: var(--text-2);
  margin: 0 4px;
}
.pager-info b {
  font-variant-numeric: tabular-nums;
}
</style>
