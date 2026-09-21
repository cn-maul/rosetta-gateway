<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { api } from '../api'
import { toast } from '../ui'
import { fmtNum, fmtTokens, fmtSpeed, fmtSec, fmtPercent } from '../fmt'
import type { Stats, UsageGroupEntry } from '../types'

const loading = ref(true)
const stats = ref<Stats | null>(null)
const byDay = ref<UsageGroupEntry[]>([])
const byModel = ref<UsageGroupEntry[]>([])
const byKey = ref<UsageGroupEntry[]>([])

// 把后端返回的分组补齐成连续 14 天序列（缺的日期补 0），保证柱状图横轴连续
const DAYS = 14
const daySeries = computed(() => {
  const map = new Map(byDay.value.map((e) => [e.key, e]))
  const out: { date: string; count: number; tokens: number }[] = []
  const now = new Date()
  for (let i = DAYS - 1; i >= 0; i--) {
    const d = new Date(now.getFullYear(), now.getMonth(), now.getDate() - i)
    const p = (x: number) => String(x).padStart(2, '0')
    const date = `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`
    const e = map.get(date)
    out.push({ date, count: e?.count ?? 0, tokens: e?.tokens ?? 0 })
  }
  return out
})

const maxTokens = computed(() => Math.max(1, ...daySeries.value.map((d) => d.tokens)))

const topModelMax = computed(() => Math.max(1, ...byModel.value.map((e) => e.tokens)))
const topKeyMax = computed(() => Math.max(1, ...byKey.value.map((e) => e.tokens)))

async function load() {
  loading.value = true
  try {
    const [s, d, m, k] = await Promise.all([
      api.stats(),
      api.usageByDay(DAYS),
      api.usageByModel(),
      api.usageByKey(),
    ])
    stats.value = s
    byDay.value = d
    byModel.value = m
    byKey.value = k
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('加载总览失败：' + (e as Error).message, 'err')
  } finally {
    loading.value = false
  }
}

onMounted(load)
defineExpose({ load })
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>总览</h1>
        <div class="sub">网关运行状态与近 14 天用量</div>
      </div>
      <div class="head-actions">
        <button class="btn" :disabled="loading" @click="load">刷新</button>
      </div>
    </div>

    <div v-if="loading && !stats" class="loading">加载中…</div>

    <template v-if="stats">
      <div class="stat-grid">
        <div class="stat-card">
          <div class="k">总请求</div>
          <div class="v num">{{ fmtNum(stats.total_requests) }}</div>
        </div>
        <div class="stat-card">
          <div class="k">总 Tokens</div>
          <div class="v num">{{ fmtTokens(stats.total_tokens) }}</div>
        </div>
        <div class="stat-card">
          <div class="k">输入 Tokens</div>
          <div class="v num">{{ fmtTokens(stats.input_tokens) }}</div>
        </div>
        <div class="stat-card">
          <div class="k">输出 Tokens</div>
          <div class="v num">{{ fmtTokens(stats.output_tokens) }}</div>
        </div>
        <div
          class="stat-card"
          :title="`缓存读取 ${fmtTokens(stats.cached_tokens)} / 输入 ${fmtTokens(stats.input_tokens)} tok`"
        >
          <div class="k">缓存命中率</div>
          <div class="v num">
            {{ fmtPercent(stats.cache_hit_rate) }}<span class="unit">%</span>
          </div>
        </div>
        <div class="stat-card">
          <div class="k">平均速度</div>
          <div class="v num">
            {{ fmtSpeed(stats.avg_tokens_per_sec) }}<span class="unit">tok/s</span>
          </div>
        </div>
        <div class="stat-card">
          <div class="k">首字用时</div>
          <div class="v num">
            {{ fmtSec(stats.avg_ttfb_ms) }}<span class="unit">s</span>
          </div>
        </div>
        <div class="stat-card" :class="{ 'is-err': stats.error_count > 0 }">
          <div class="k">错误数</div>
          <div class="v num">{{ fmtNum(stats.error_count) }}</div>
        </div>
      </div>

      <div class="panel">
        <div class="panel-title">近 {{ DAYS }} 天用量 <span class="hint">按日聚合 · 悬停查看详情</span></div>
        <div class="bars">
          <div v-for="d in daySeries" :key="d.date" class="bar-col">
            <div class="bar-track">
              <div
                class="bar"
                :class="{ dim: d.tokens === 0 }"
                :style="{ height: Math.max(2, (d.tokens / maxTokens) * 100) + '%' }"
                :data-tip="`${d.date.slice(5)} · ${fmtNum(d.count)} 次 · ${fmtTokens(d.tokens)} tok`"
              ></div>
            </div>
          </div>
        </div>
        <div class="bar-labels">
          <span v-for="(d, i) in daySeries" :key="d.date" class="bar-lab" :class="{ dim: i % 3 !== 0 }">
            <span class="bar-full">{{ d.date.slice(5) }}</span>
            <span class="bar-short">{{ d.date.slice(8) }}</span>
          </span>
        </div>
      </div>

      <div class="panel">
        <div class="expand-grid">
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
/* 柱状图双形态标签：桌面显示 MM-DD，窄屏只显示「日」 */
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
