<script setup lang="ts">
import DataTable from './DataTable.vue'
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
import { computed } from 'vue'
import type { Credential, ProviderBalance, CredentialBalance } from '../types'

const props = defineProps<{
  providerId: string
  creds: Credential[]
  /** 该上游的余额查询结果；未点过「查余额」时为 undefined。 */
  balance?: ProviderBalance
  balanceBusy: boolean
  /** 正在提交启停的凭据 id：只给那一行的开关上防连点。 */
  toggling: string[]
  /** 该上游是否配了自定义余额查询（有 spec 走自定义，没有则走内置判定）。 */
  balanceSpecConfigured: boolean
}>()

const emit = defineEmits<{
  add: []
  queryBalance: []
  edit: [cred: Credential]
  remove: [cred: Credential]
  toggle: [cred: Credential]
}>()

// isPending 判某一行是否正在提交启停。
//
// 启停是「切换」语义（同一上游同时只启用一把），所以连点一次的后果是
// **两次相反的切换**：第一次把 A 打开（同时关掉 B），第二次又把 A 关掉。
// 用户想要的结果和最终状态完全相反，且期间请求池真的换了 key ——
// 那种「我明明点了一下启用，怎么又没了」没有任何自我修复的余地。
function isPending(id: string): boolean {
  return props.toggling.includes(id)
}

// balanceLine 把一条凭据的余额压成一行。
//
// 分三个状态措辞，因为三者的后续动作互不相干：
//   - unsupported：这个上游压根没有余额查询方式 → 去它自己的后台看；
//   - error：查了但失败 → 401 换 key、超时重试；
//   - ok：拿到数 → 数字 + 时刻 + 上游附注。
// 混成一句「查询失败」会让管理员在三种完全不同的排查方向之间瞎猜。
function balanceLine(r: CredentialBalance): string {
  if (r.status === 'unsupported') return r.message || '该上游不支持余额查询'
  if (r.status === 'unknown') {
    const at = r.fetched_at ? new Date(r.fetched_at).toLocaleTimeString() : ''
    return [r.message || '余额不足以下结论', r.detail, at].filter(Boolean).join(' · ')
  }
  if (r.status === 'error') return r.message || '查询失败'
  const at = r.fetched_at ? new Date(r.fetched_at).toLocaleTimeString() : ''
  const head = `${(r.amount ?? 0).toFixed(2)} ${r.currency ?? ''}`.trim()
  return [head, at, r.detail].filter(Boolean).join(' · ')
}

function balanceClass(r: CredentialBalance): string {
  if (r.status === 'ok') return 'bal-ok'
  if (r.status === 'unsupported') return 'bal-na'
  if (r.status === 'unknown') return 'bal-unknown'
  return 'test-err'
}

/** 只接受属于当前上游的响应；跨 provider 的旧响应一律视为没有结果。 */
const currentBalance = computed(() =>
  props.balance && props.balance.provider_id === props.providerId ? props.balance : undefined,
)

function balanceFor(credentialId: string): CredentialBalance | undefined {
  return currentBalance.value?.results.find((x) => x.credential_id === credentialId)
}

// balanceModeFor 说明这次查询走的是哪条路径。
//
// 为什么要显示：自动判定只覆盖 OpenAI 官方与 DeepSeek 两家，其余上游
// 一律「不支持」。当管理员看到「不支持」时，他需要知道的是「这是内置判定
// 的结论」还是「我配了但没匹配上」—— 两者的下一步完全不同
// （前者去填 balance_spec_json，后者去改配置）。
function balanceModeFor(): string {
  if (!currentBalance.value) return ''
  if (currentBalance.value.status !== 'unsupported') return ''
  return props.balanceSpecConfigured
    ? '按自定义配置查询，但上游没有返回可用数据 —— 检查 path / field 是否与上游文档一致'
    : '未配置余额查询方式，且内置判定不认识这个上游（内置支持 OpenAI 官方与 DeepSeek）'
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
      <!-- 「同时只启用一把」的提示必须与表格**并存**，不能写成 v-else-if。
           修复前它占着 v-else 的位置，凭据数 ≥ 2 时渲染的是这句提示、
           表格整个不渲染 —— 两把 key 从界面上消失，而后端数据完好、
           调用照常成功。用户看到的是「我加的 key 不见了」。
           它是**跨行**的约束，必须写在开关旁边而不是藏进文档。 -->
      <p v-if="creds.length > 1" class="dim sub-line">
        同一上游同时只启用一把 key：打开其中一把会自动停用其它。
      </p>
      <!-- 查询方式的说明：只在查过且拿到「不支持」时出现。它的价值在于
           区分「内置判定不认识这个上游」与「配了但没匹配上」—— 下一步动作
           完全不同（去填配置 vs 去改配置），而两者在界面上原本是同一句话。 -->
      <p v-if="balanceModeFor()" class="dim sub-line">
        {{ balanceModeFor() }}
      </p>
      <!-- tbl-wrap 不能省：操作列是 .c-act（不换行），余额列里的附注也整段读。
           在详情页这张表只占半屏宽，不裹 overflow-x 的话会把右侧的操作
           按钮顶出可视区 —— 而那是这一行唯一能点的东西。 -->
      <DataTable v-if="creds.length > 0" label="上游凭据">
        <table class="tbl sub-tbl">
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
              <div v-if="currentBalance" class="bal-cell">
                <div
                  v-if="balanceFor(c.id)"
                  class="bal-line"
                  :class="balanceClass(balanceFor(c.id)!)"
                >
                  {{ balanceLine(balanceFor(c.id)!) }}
                </div>
                <span v-else class="dim">—（无结果）</span>
              </div>
              <span v-else class="dim">—</span>
            </td>
            <td class="c-act">
              <div class="row-actions">
                <!-- 启停开关。与 Keys/Routes 两页同一套 .switch 配方，
                     放在操作列最前：它是这一行最高频的动作，而「换钥 /
                     删除」是低频且有后果的 —— 位置靠内一层就少一次误点。 -->
                <button
                  class="switch"
                  :class="{ on: c.enabled }"
                  :disabled="isPending(c.id)"
                  :title="
                    c.enabled
                      ? '停用这把 key'
                      : '启用这把 key（同一上游同时只启用一把，启用它会自动停用其它）'
                  "
                  :aria-label="c.enabled ? '停用' : '启用'"
                  @click="emit('toggle', c)"
                ></button>
                <button class="btn btn-sm btn-ghost" @click="emit('edit', c)">换钥</button>
                <button class="btn btn-sm btn-danger" @click="emit('remove', c)">删除</button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
      </DataTable>
    </div>
  </div>
</template>
