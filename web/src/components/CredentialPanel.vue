<script setup lang="ts">
/**
 * 凭据面板：某个上游下的多把 API 凭据（Providers 页展开区的一列）。
 *
 * 2026-10-12 从 Providers.vue 抽出，并改用 HeroUI 的结构约定：
 *   - 外层 .card（header / title / content 的 slot 结构），不再是一个裸 div；
 *   - 表格沿用 .tbl（已按 HeroUI table.css 的手法改写）。
 *
 * 刻意**只搬展示**不加抽象：数据源与操作回调都从父组件传入（props + emit），
 * 父组件仍是唯一的写路径持有者 —— 抽组件只减少单文件长度，
 * 不引入第二处状态真相。
 */
import type { Credential, ProviderBalance, CredentialBalance } from '../types'

const props = defineProps<{
  providerId: string
  creds: Credential[]
  /** 该上游的余额查询结果；未点过「查余额」时为 undefined。 */
  balance?: ProviderBalance
  balanceBusy: boolean
}>()

const emit = defineEmits<{
  add: []
  queryBalance: []
  edit: [cred: Credential]
  remove: [cred: Credential]
}>()

// balanceLine 把一条凭据的余额压成一行。
//
// 分三个状态措辞，因为三者的后续动作互不相干：
//   - unsupported：这个上游压根没有余额查询方式 → 去它自己的后台看；
//   - error：查了但失败 → 401 换 key、超时重试；
//   - ok：拿到数 → 数字 + 时刻 + 上游附注。
// 混成一句「查询失败」会让管理员在三种完全不同的排查方向之间瞎猜。
function balanceLine(r: CredentialBalance): string {
  if (r.status === 'unsupported') return r.message || '该上游不支持余额查询'
  if (r.status === 'error') return r.message || '查询失败'
  const at = r.fetched_at ? new Date(r.fetched_at).toLocaleTimeString() : ''
  const head = `${(r.amount ?? 0).toFixed(2)} ${r.currency ?? ''}`.trim()
  return [head, at, r.detail].filter(Boolean).join(' · ')
}

function balanceClass(r: CredentialBalance): string {
  if (r.status === 'ok') return 'bal-ok'
  return r.status === 'unsupported' ? 'bal-na' : 'test-err'
}
</script>

<template>
  <div class="card">
    <div class="card__header">
      <div class="card__title">凭据（{{ creds.length }}）</div>
    </div>
    <div class="card__content">
      <!-- 操作按钮：HeroUI 的 card header 常配一组行动按钮，
           这里放在内容区顶部，与「添加/查余额」的语义相符。 -->
      <div class="card__footer">
        <button class="btn btn-sm" @click="emit('add')">添加凭据</button>
        <button
          class="btn btn-sm btn-ghost"
          :disabled="balanceBusy"
          title="实时向该上游查询每把凭据的余额（不自动刷新）"
          @click="emit('queryBalance')"
        >
          {{ balanceBusy ? '查询中…' : '查余额' }}
        </button>
      </div>

      <div v-if="creds.length === 0" class="empty sub-empty">
        无凭据 —— 上游鉴权必需
      </div>
      <table v-else class="tbl sub-tbl">
        <thead>
          <tr>
            <th>凭据</th>
            <th>上游余额</th>
            <th class="c-act">操作</th>
          </tr>
        </thead>
        <tbody>
          <!-- 权重 / 状态 / 创建时间三列已按需求移除。
               注意 weight 在后端仍然生效（负载均衡按它轮询），
               这里去掉的只是展示。 -->
          <tr v-for="c in creds" :key="c.id">
            <td>
              <div class="cell-key">
                <span class="name">{{ c.label || c.id }}</span>
                <span class="badge" :class="c.enabled ? 'badge-live' : 'badge-off'">
                  {{ c.enabled ? '启用' : '停用' }}
                </span>
              </div>
            </td>
            <!-- 余额显示在凭据行内而不是另起一表：余额挂在 key 上，
                 放远了就失去「哪一把快没钱了」这个唯一有用的读法。
                 没点过「查余额」时留白 —— 它不是 0，
                 两者必须可区分：一个是「没查」，一个是「确实没钱」。 -->
            <td>
              <div v-if="balance" class="bal-cell">
                <div
                  v-for="r in balance.results.filter((x) => x.credential_id === c.id)"
                  :key="r.credential_id"
                  class="bal-line"
                  :class="balanceClass(r)"
                >
                  {{ balanceLine(r) }}
                </div>
                <span v-if="!balance.results.some((x) => x.credential_id === c.id)" class="dim">
                  —（无结果）
                </span>
              </div>
              <span v-else class="dim">—</span>
            </td>
            <td class="c-act">
              <div class="row-actions">
                <button class="btn btn-sm btn-ghost" @click="emit('edit', c)">换钥</button>
                <button class="btn btn-sm btn-danger" @click="emit('remove', c)">删除</button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
