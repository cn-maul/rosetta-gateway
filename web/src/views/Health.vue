<script setup lang="ts">
/**
 * 上游模型连通率（管理员）。按时间轴展示每个模型的探测成功率。
 *
 * 数据来自后台定时探测（internal/probe.Runner），本页只读 model_probes 表。
 * 探测请求 max_tokens=1、不经 usage_recorder，不扣任何网关用户的钱。
 *
 * 热力图用纯 CSS 条带（项目没有任何图表库，Overview 的条形图也是纯 div）：
 * 每列一个时间桶，颜色深浅 = 成功率，空白 = 该桶没探测记录（≠ 全失败）。
 */
import { computed, onMounted, ref } from 'vue'
import { api } from '../api'
import { toast } from '../ui'
import type { ProbeHealth, ProbeTimelineBucket } from '../types'

const err = ref('')
const loading = ref(true)
const healths = ref<ProbeHealth[]>([])
const selected = ref('')
const timeline = ref<ProbeTimelineBucket[]>([])
const days = ref(7)
const granularity = ref<'hour' | 'day'>('hour')
const timelineLoading = ref(false)

async function loadHealths() {
  loading.value = true
  err.value = ''
  try {
    healths.value = await api.probeModels()
    // 默认选中第一个有探测记录的模型（否则时间轴页打开是空的）。
    if (!selected.value) {
      const first = healths.value.find((h) => h.window_count > 0)
      if (first) selected.value = first.model_id
    }
    if (selected.value) await loadTimeline()
  } catch (e) {
    if ((e as { status?: number }).status === 401) return
    err.value = '加载健康数据失败：' + (e as Error).message
    toast('加载健康数据失败：' + (e as Error).message, 'err')
  } finally {
    loading.value = false
  }
}

async function loadTimeline() {
  if (!selected.value) return
  timelineLoading.value = true
  try {
    timeline.value = await api.probeTimeline(selected.value, days.value, granularity.value)
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('加载时间轴失败：' + (e as Error).message, 'err')
  } finally {
    timelineLoading.value = false
  }
}

onMounted(loadHealths)

// 健康态徽章 class（与总览/历史同一套 badge 语义）。
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

// 时间轴分桶 → 热力图列。桶起点毫秒 → 颜色档。
// 成功率 0 但 count>0 = 全失败（深红）；count=0 的行后端不返回（空白）。
function bucketClass(b: ProbeTimelineBucket): string {
  const rate = b.count > 0 ? b.ok / b.count : 0
  if (rate >= 0.8) return 'heat-ok'
  if (rate >= 0.5) return 'heat-mid'
  if (rate > 0) return 'heat-low'
  return 'heat-fail'
}

// 时间轴列标签：hour 粒度显示「HH:00」，day 显示「MM-DD」。
function bucketLabel(b: ProbeTimelineBucket): string {
  const d = new Date(b.ts)
  if (granularity.value === 'day') {
    return `${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
  }
  return `${String(d.getHours()).padStart(2, '0')}:00`
}

// 时间轴列 title：悬停看该桶的成功率与延迟。
function bucketTitle(b: ProbeTimelineBucket): string {
  const rate = b.count > 0 ? Math.round((b.ok / b.count) * 100) : 0
  return `${bucketLabel(b)} · 成功率 ${rate}% (${b.ok}/${b.count}) · 平均 ${b.latency}ms`
}

// 汇总：当前选中模型的整体成功率。
const selectedSummary = computed(() => {
  const h = healths.value.find((x) => x.model_id === selected.value)
  if (!h) return null
  return h
})
</script>

<template>
  <main class="page">
    <div class="price-head">
      <div>
        <div class="price-title">上游模型连通率</div>
        <div class="tip">
          后台每 10 分钟用最小请求（max_tokens=1）探测一次各上游模型。连续失败 3 次判「不可用」，
          最近成功且窗口成功率 ≥80% 判「正常」。探测不扣网关用户的钱。
        </div>
      </div>
    </div>

    <div v-if="loading" class="loading">加载中…</div>
    <div v-else-if="err" class="empty">{{ err }}</div>
    <div v-else-if="healths.length === 0" class="empty">还没有可探测的模型 —— 请先在「上游与模型」页添加</div>

    <template v-else>
      <!-- 模型列表 + 健康态 -->
      <div class="tbl-wrap">
        <table class="tbl">
          <thead>
            <tr>
              <th>模型</th>
              <th>状态</th>
              <th class="num-h">窗口成功率</th>
              <th class="num-h">探测次数</th>
              <th class="num-h">连续失败</th>
              <th>最近失败原因</th>
              <th class="c-op">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="h in healths" :key="h.model_id" :class="{ sel: h.model_id === selected }">
              <td class="mono">{{ h.model_id }}</td>
              <td>
                <span class="badge" :class="healthClass(h.health)">{{ healthLabel(h.health) }}</span>
              </td>
              <td class="num-h">{{ h.window_count > 0 ? Math.round(h.success_rate * 100) + '%' : '—' }}</td>
              <td class="num-h">{{ h.window_count }}</td>
              <td class="num-h">{{ h.consecutive_fail }}</td>
              <td class="dim">{{ h.last_error || '—' }}</td>
              <td class="c-op">
                <button class="btn btn-sm btn-ghost" @click="selected = h.model_id; loadTimeline()">看时间轴</button>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- 时间轴 -->
      <div class="timeline-card">
        <div class="timeline-h">
          <span class="timeline-title">
            时间轴：<span class="mono">{{ selected }}</span>
            <span v-if="selectedSummary" class="dim">
              · 整体成功率 {{ selectedSummary.window_count > 0 ? Math.round(selectedSummary.success_rate * 100) + '%' : '—' }}
            </span>
          </span>
          <div class="timeline-controls">
            <select v-model="days" class="select" @change="loadTimeline">
              <option :value="7">近 7 天</option>
              <option :value="14">近 14 天</option>
              <option :value="30">近 30 天</option>
            </select>
            <select v-model="granularity" class="select" @change="loadTimeline">
              <option value="hour">按小时</option>
              <option value="day">按天</option>
            </select>
          </div>
        </div>

        <div v-if="timelineLoading" class="loading">加载时间轴…</div>
        <div v-else-if="timeline.length === 0" class="empty">该时间范围内没有探测记录</div>
        <div v-else class="heatmap">
          <div
            v-for="b in timeline"
            :key="b.ts"
            class="heat-cell"
            :class="bucketClass(b)"
            :title="bucketTitle(b)"
          >
            <span class="heat-label">{{ bucketLabel(b) }}</span>
          </div>
        </div>
        <div class="heat-legend">
          <span class="heat-key"><i class="heat-swatch heat-ok"></i>≥80%</span>
          <span class="heat-key"><i class="heat-swatch heat-mid"></i>50–80%</span>
          <span class="heat-key"><i class="heat-swatch heat-low"></i>&lt;50%</span>
          <span class="heat-key"><i class="heat-swatch heat-fail"></i>全失败</span>
        </div>
      </div>
    </template>
  </main>
</template>

<style scoped>
.price-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 14px;
  margin-bottom: 14px;
}
.price-title {
  font-size: 15px;
  font-weight: 600;
  margin-bottom: 4px;
}
.tip {
  color: var(--text-3);
  font-size: 12px;
  max-width: 680px;
  line-height: 1.5;
}
tr.sel td {
  background: var(--accent-soft);
}
.timeline-card {
  margin-top: 18px;
  padding: 16px 18px;
  border: 1px solid var(--border);
  border-radius: var(--r-card);
  background: var(--surface);
}
.timeline-h {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 14px;
  flex-wrap: wrap;
}
.timeline-title {
  font-size: 13px;
  font-weight: 600;
}
.timeline-controls {
  display: flex;
  gap: 8px;
}
.heatmap {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}
.heat-cell {
  width: 34px;
  height: 22px;
  border-radius: 4px;
  display: flex;
  align-items: center;
  justify-content: center;
  font-size: 9px;
  color: var(--text-3);
  cursor: default;
}
.heat-ok {
  background: color-mix(in oklab, var(--ok, #22c55e) 55%, transparent);
}
.heat-mid {
  background: color-mix(in oklab, #eab308 55%, transparent);
}
.heat-low {
  background: color-mix(in oklab, #f97316 55%, transparent);
}
.heat-fail {
  background: color-mix(in oklab, #ef4444 60%, transparent);
  color: #fff;
}
.heat-label {
  white-space: nowrap;
}
.heat-legend {
  display: flex;
  gap: 14px;
  margin-top: 12px;
  font-size: 11px;
  color: var(--text-3);
}
.heat-key {
  display: flex;
  align-items: center;
  gap: 4px;
}
.heat-swatch {
  width: 12px;
  height: 12px;
  border-radius: 3px;
  display: inline-block;
}
</style>
