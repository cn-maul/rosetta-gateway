<script setup lang="ts">
import DataTable from './DataTable.vue'
/**
 * 上游模型面板：某个上游下声明的模型列表（Providers 页展开区的一列）。
 *
 * 2026-10-12 从 Providers.vue 抽出，并改用 HeroUI 的 .card slot 结构。
 * 与 CredentialPanel 同样只搬展示，写操作（测试/编辑/删除）由父组件持有。
 */
import { fmtSpeed, fmtSec, fmtPercent } from '../fmt'
import { modelPriceLine as priceLine } from '../modelPrice'
import type { UpstreamModel } from '../types'

const props = defineProps<{
  models: UpstreamModel[]
  /** 正在探测的 model id 集合。 */
  testing: Set<string>
}>()

const emit = defineEmits<{
  add: []
  test: [m: UpstreamModel]
  edit: [m: UpstreamModel]
  remove: [m: UpstreamModel]
}>()

function isTesting(id: string): boolean {
  return props.testing.has(id)
}

</script>

<template>
  <div class="card">
    <div class="card__header">
      <div class="card__title">上游模型（{{ models.length }}）</div>
    </div>
    <div class="card__content">
      <div class="card__footer">
        <button class="btn btn-sm" @click="emit('add')">添加模型</button>
      </div>

      <div v-if="models.length === 0" class="empty sub-empty">
        无模型 —— 路由必须指向一个上游模型
      </div>
      <!-- tbl-wrap 不能省：这张表有 5 列，其中 4 列是 .num-h（整段读、
           不许折行）。在详情页它只占半屏宽（与凭据表并排），不裹
           overflow-x 的话表格会撑破卡片而不是自己横向滚动，
           把右侧「操作」列顶出可视区 —— 而那是这一行唯一能点的东西。 -->
      <DataTable v-else label="上游模型">
        <table class="tbl sub-tbl">
        <thead>
          <tr>
            <th>模型</th>
            <th class="num-h">速度(tok/s)</th>
            <th class="num-h">首字(s)</th>
            <th class="num-h">成功率</th>
            <th class="c-act">操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="m in models" :key="m.id">
            <td>
              <div class="cell-key">
                <span class="name mono">{{ m.model_id }}</span>
              </div>
              <div class="sub-line price-line">价格（元/1M）：{{ priceLine(m) }}</div>
              <!-- 思考挡位。agent 工具靠 /v1/models 的 supported_reasoning
                   决定给用户开几档，那份数据就来自这一列的配置。
                   不显示的话「客户端以为支持、实际被静默降级」无从追查。 -->
              <!-- 能否思考 / 有哪几档必须一起显示：只显示挡位会让人以为
                   「配了档位就等于支持思考」，而数据面在「不支持思考」时
                   会直接把思考配置剥掉。 -->
              <div v-if="m.supports_thinking !== undefined" class="sub-line">
                思考：{{ m.supports_thinking ? '支持' : '不支持' }}
              </div>
              <div v-if="m.effort_levels?.length" class="sub-line">
                挡位：{{ m.effort_levels.join(' · ') }}
                <span v-if="m.unknown_effort_levels?.length" class="dim">
                  （未识别：{{ m.unknown_effort_levels.join('、') }}）
                </span>
              </div>
            </td>
            <td class="num-h dim">{{ m.tokens_per_sec ? fmtSpeed(m.tokens_per_sec) : '—' }}</td>
            <td class="num-h dim">{{ m.ttfb_ms ? fmtSec(m.ttfb_ms) : '—' }}</td>
            <td class="num-h">
              <span v-if="m.call_count" class="sr" :class="{ warn: (m.success_rate ?? 1) < 1 }">
                {{ fmtPercent(m.success_rate ?? 1) }}%
              </span>
              <span v-else class="dim">—</span>
            </td>
            <td class="c-act">
              <div class="row-actions">
                <!-- 真实发一次 max_tokens=1 的推理请求。与已移除的
                     provider 级测试不同，这个能证明「这个 model_id
                     现在真的能推理」（下架/无权限/名字写错都会红）。 -->
                <button
                  class="btn btn-sm btn-ghost"
                  :disabled="isTesting(m.id)"
                  title="真实调用一次（max_tokens=1）验证该模型可用"
                  @click="emit('test', m)"
                >
                  {{ isTesting(m.id) ? '测试中…' : '测试' }}
                </button>
                <button class="btn btn-sm btn-ghost" @click="emit('edit', m)">编辑</button>
                <!-- 链首置灰（2026-10-14）：后端对链首删除回 409，所以这里提前
                     拦住并把「该去哪儿改」写进 title。置灰而不是隐藏：模型确实
                     需要一个删除入口，只是这个模型现在不能删 —— 藏起来会让人
                     以为没有删除功能。 -->
                <button
                  class="btn btn-sm btn-danger"
                  :disabled="m.is_route_head"
                  :title="m.is_route_head
                    ? '该模型是某条路由的首选目标，不能直接删除 —— 请先到「路由」页改掉那条路由的首选'
                    : undefined"
                  @click="emit('remove', m)"
                >
                  删除
                </button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
      </DataTable>
    </div>
  </div>
</template>

<style scoped>
.price-line { color: var(--muted); overflow-wrap: anywhere; }
</style>
