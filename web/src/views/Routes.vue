<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import { toast, confirmBox } from '../ui'
import type { Provider, UpstreamModel, Route, RouteTargetInput } from '../types'
import AppModal from '../components/AppModal.vue'

const err = ref('')
const loading = ref(true)
const routes = ref<Route[]>([])
const providers = ref<Provider[]>([])
// upstream_models.id（短哈希外键）→ 模型对象；Resolve 时拿的是这个外键
const mmap = ref<Record<string, UpstreamModel>>({})

const providerMap = computed(() => {
  const m: Record<string, Provider> = {}
  for (const p of providers.value) m[p.id] = p
  return m
})

// 某个 provider 下可被路由选择的模型
function modelsOf(providerId: string): UpstreamModel[] {
  return Object.values(mmap.value).filter((m) => m.provider_id === providerId)
}

function modelLabel(routeRow: Route): string {
  const m = mmap.value[routeRow.upstream_model_id]
  if (m) return m.display_name ? `${m.display_name} (${m.model_id})` : m.model_id
  // 兜底：外键悬空时至少显示路由自身信息
  return routeRow.id
}

async function load() {
  loading.value = true
  err.value = ''
  try {
    const [r, p] = await Promise.all([api.routes(), api.providers()])
    routes.value = r
    providers.value = p
    // 一次取回全部上游模型建索引（此前对每个 provider 各发一次请求 = 1+N）
    const all = await api.allModels()
    const map: Record<string, UpstreamModel> = {}
    for (const m of all) map[m.id] = m
    mmap.value = map
  } catch (e) {
    if ((e as { status?: number }).status === 401) return
    // 失败**必须留下可见的错误态**：只弹 toast 的话，页面保留空列表/
    // 空表格，呈现成「暂无数据」—— 而真实原因是请求失败了。运维会据此
    // 判断「今天没有流量」，进而排除掉网关/上游故障这个真正的方向。
    // 与 Users/Groups 的持久错误态同口径（见其 v-else-if="err"）。
    err.value = '加载路由失败：' + (e as Error).message
    toast('加载失败：' + (e as Error).message, 'err')
  } finally {
    loading.value = false
  }
}

// ---------- 表单 ----------

interface TargetRow {
  provider_id: string
  upstream_model_id: string
  enabled: boolean
}

const form = reactive({
  open: false,
  editing: '',
  public_name: '',
  enabled: true,
  // 是否对这条路由启用故障转移。
  // 策略参数（尝试预算 / 熔断阈值 / 各类超时）是全局的，在「设置」页配置。
  failover_enabled: false,
  targets: [] as TargetRow[],
})

// 目标链加载失败的说明。非空 = 禁止保存。
//
// 为什么不能像别的展示字段那样「加载失败就留空」：保存走的是 PUT 整体替换
// （按数组顺序重排 position），一份伪造出来的单目标链写回去，等于把 position 1..N
// 全部删掉 —— 界面显示「保存成功」，实际故障转移链已经没了，且没有撤销路径。
const chainError = ref('')

/**
 * 目标链的两个「能不能保存」状态，缺一不可：
 *
 *   - chainLoading：链是**异步**取的。打开弹窗到响应回来这段窗口里，表单里
 *     只有一条由 route 主目标列拼出来的占位行 —— 此时放行保存，等于拿一份
 *     伪造的单目标链去做 PUT 整体替换。
 *   - chainReady：只有「本次打开的链确实取到了」才为真。加载失败（chainError）
 *     和「会话已失效」都停在假，永不放行。
 *
 * 光有 chainError 不够（它覆盖不到加载中），光有 chainLoading 也不够（加载
 * 结束后失败态会把它清回 false，继而放行）。两者叠起来才是「安全」。
 */
const chainLoading = ref(false)
const chainReady = ref(false)

/**
 * 打开弹窗的请求序号。
 *
 * 连点两条路由的「编辑」会并发两个 routeTargets 请求，而响应顺序不保证与点击
 * 顺序一致。没有序号时，慢到的**旧**响应会最后写入 form.targets —— 弹窗于是
 * 显示「A 路由的公开名 + B 路由的上游链」。这份串了的表单一旦保存，PUT 会把
 * B 的链整体写到 A 上（position 1..N 被重排覆盖），界面上完全看不出发生过什么。
 */
let chainSeq = 0

// 保存中：挡住回车连点造成的重复提交（每次提交都会整体替换一次链）。
const saving = ref(false)

// 链未就绪期间禁止一切改动与保存（覆盖「加载中」与「加载失败」两种情形）
const chainBlocked = computed(() => chainLoading.value || !chainReady.value || saving.value)

function blankTarget(): TargetRow {
  return { provider_id: '', upstream_model_id: '', enabled: true }
}

async function openRoute(r?: Route) {
  const seq = ++chainSeq
  form.editing = r?.id ?? ''
  form.public_name = r?.public_name ?? ''
  form.enabled = r?.enabled ?? true
  form.failover_enabled = r?.failover_enabled ?? false
  chainError.value = ''
  // 每次打开都先回到「未就绪」：任何一次重新打开、以及任何一次并发的旧响应，
  // 在没有拿到本轮链之前都不允许保存。
  chainReady.value = false

  if (r) {
    chainLoading.value = true
    try {
      const ts = await api.routeTargets(r.id)
      if (seq !== chainSeq) return // 期间又打开了别的路由，本响应已过时，丢弃
      form.targets = ts.length
        ? ts.map((t) => ({ provider_id: t.provider_id, upstream_model_id: t.upstream_model_id, enabled: t.enabled }))
        : [{ provider_id: r.provider_id, upstream_model_id: r.upstream_model_id, enabled: true }]
      chainReady.value = true
    } catch (e) {
      if (seq !== chainSeq) return
      if ((e as { status?: number }).status === 401) {
        // 401 只代表会话失效（api 侧已清令牌、App 会送回登录页）。
        // 这里必须**关掉弹窗**：留在打开状态的话，屏幕停在一份永远加载不出来的
        // 表单上，而真正的原因（请重新登录）只在 toast 里。
        form.open = false
        return
      }
      // 载荷仍是「主目标单行」，但只用于展示；chainReady 保持假，保存被拦住。
      chainError.value =
        '无法读取该路由的上游链：' +
        (e as Error).message +
        '。为避免用不完整的数据覆盖已有配置，保存已被禁用 —— 请先刷新页面重试。'
      form.targets = [{ provider_id: r.provider_id, upstream_model_id: r.upstream_model_id, enabled: true }]
    } finally {
      // 只清自己那一次的加载态：被丢弃的旧响应不能把新一轮的加载态提前抹掉，
      // 否则「点第二条路由」的瞬间按钮就解禁了 —— 而链还在路上。
      if (seq === chainSeq) chainLoading.value = false
    }
  } else {
    // 新建：链由用户现场填，没有「读回来」这一步，直接算就绪。
    form.targets = [blankTarget()]
    chainReady.value = true
  }
  form.open = true
}

function addTarget() {
  form.targets.push(blankTarget())
}

function removeTarget(i: number) {
  if (form.targets.length <= 1) {
    toast('至少保留一个上游目标', 'err')
    return
  }
  form.targets.splice(i, 1)
}

function moveTarget(i: number, dir: -1 | 1) {
  const j = i + dir
  if (j < 0 || j >= form.targets.length) return
  const [row] = form.targets.splice(i, 1)
  form.targets.splice(j, 0, row)
}

function onTargetProviderChange(row: TargetRow) {
  // 切 provider 后原模型可能不属于它，清空让用户重选
  const m = mmap.value[row.upstream_model_id]
  if (m && m.provider_id !== row.provider_id) row.upstream_model_id = ''
}

async function submitRoute() {
  const targets = form.targets
  if (chainLoading.value) {
    toast('上游链仍在加载，请稍候', 'err')
    return
  }
  if (!chainReady.value) {
    // 与 chainError 同因：链没读回来就保存 = 用一份伪造的单目标链整体替换。
    // chainError 非空时它已经把原因写在弹窗里了，这里只补一句动作被拦下。
    toast('上游链未成功加载，已禁止保存（请先刷新页面）', 'err')
    return
  }
  if (saving.value) return
  if (!form.public_name.trim()) {
    toast('公开模型名为必填项', 'err')
    return
  }
  if (targets.length === 0 || targets.some((t) => !t.provider_id || !t.upstream_model_id)) {
    toast('每个上游目标都要选好 provider 与模型', 'err')
    return
  }

  // 链首即 route 的主目标列（后端 Create/Update 仍要求这两列非空）。
  const head = targets[0]
  const routeBody: Partial<Route> = {
    public_name: form.public_name.trim(),
    provider_id: head.provider_id,
    upstream_model_id: head.upstream_model_id,
    enabled: form.enabled,
    failover_enabled: form.failover_enabled,
  }
  const chain: RouteTargetInput[] = targets.map((t) => ({
    provider_id: t.provider_id,
    upstream_model_id: t.upstream_model_id,
    enabled: t.enabled,
  }))

  let isNew = false
  saving.value = true
  try {
    let routeId = form.editing
    if (routeId) {
      await api.updateRoute(routeId, routeBody)
    } else {
      const created = await api.createRoute(routeBody)
      routeId = created.id
      isNew = true
      // 走到这里 route 已经在库里了（后端建 route 时会种一条链首目标）。
      // 后面若写链失败，必须如实说「已建但链不完整」，不能报一句「保存失败」
      // 让用户以为什么都没发生 —— 他会再点一次「新建」，然后撞 public_name 冲突。
      form.editing = routeId
    }
    // 目标链独立于 route 基础字段整体替换（按当前顺序重排 position）。
    await api.saveRouteTargets(routeId, chain)

    form.open = false
    toast(isNew ? '路由已创建' : '路由已更新')
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) {
      // isNew=true 时 route 已落库、只是链没写全；其余情况才是真正的「保存失败」。
      const hint = isNew ? '路由已创建，但上游链写入失败：' : '保存失败：'
      toast(hint + (e as Error).message, 'err')
    }
  } finally {
    saving.value = false
  }
}

async function toggleRoute(r: Route) {
  try {
    // 只发要改的那一个字段。PATCH 是字段级部分更新，回传整个对象有两个代价：
    //   1. r 来自列表快照，可能已被别的标签页改过 —— 全量回写会静默回滚那些字段；
    //   2. 回传 provider_id/upstream_model_id 会顺带触发一次「把链首对齐到主目标」，
    //      而启停一个路由本不该动它的目标链。
    await api.updateRoute(r.id, { enabled: !r.enabled })
    toast(!r.enabled ? '已启用' : '已停用')
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('操作失败：' + (e as Error).message, 'err')
  }
}

async function removeRoute(r: Route) {
  const ok = await confirmBox({
    title: `删除路由「${r.public_name}」？`,
    body: '删除后该公开模型名立即不可用（客户端会收到 404 model_not_found）。',
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return
  try {
    await api.deleteRoute(r.id)
    toast('已删除')
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('删除失败：' + (e as Error).message, 'err')
  }
}

onMounted(load)
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>路由</h1>
        <div class="sub">公开模型名 → 有序上游链，按 position 依次尝试，命中故障自动转移</div>
      </div>
      <div class="head-actions">
        <button class="btn" :disabled="loading" @click="load">刷新</button>
        <button class="btn btn-primary" @click="openRoute()">新建路由</button>
      </div>
    </div>

    <div class="panel">
      <div v-if="loading && routes.length === 0" class="loading">加载中…</div>
      <!--加载失败**必须**与「确实没有数据」在界面上可区分：把请求失败呈现成
           「暂无数据」会让运维误判为无流量，从而排除掉网关/上游故障这个方向。
           与 Users/Groups 的错误态同口径。 -->
      <div v-else-if="err" class="empty"><div class="big">⚠</div>{{ err }}</div>
      <div v-else-if="routes.length === 0" class="empty">
        <div class="big">⇢</div>
        还没有路由，客户端将无法调用任何模型
      </div>
      <div v-else class="tbl-wrap">
        <table class="tbl">
          <thead>
            <tr>
              <th>公开模型名</th>
              <th>状态</th>
              <th>主目标</th>
              <th class="c-act">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in routes" :key="r.id">
              <td class="mono">{{ r.public_name }}</td>
              <td>
                <span class="badge" :class="r.enabled ? 'badge-live' : 'badge-off'">{{ r.enabled ? '启用' : '停用' }}</span>
                <span v-if="r.failover_enabled" class="badge">故障转移</span>
              </td>
              <td>
                {{ providerMap[r.provider_id]?.name ?? r.provider_id }} →
                <span class="mono">{{ modelLabel(r) }}</span>
                <div v-if="r.failover_enabled" class="sub-line">链式多上游，失败自动切换</div>
              </td>
              <td class="c-act">
                <div class="row-actions">
                  <button class="btn btn-sm btn-ghost" @click="openRoute(r)">编辑</button>
                  <button class="btn btn-sm btn-danger" @click="removeRoute(r)">删除</button>
                  <button class="switch" :class="{ on: r.enabled }" :title="r.enabled ? '停用' : '启用'" @click="toggleRoute(r)"></button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- 表单填一半别丢：遮罩/Esc 都不关（:dismissable=false），只能走取消/保存 -->
    <AppModal
      :open="form.open"
      :title="form.editing ? '编辑路由' : '新建路由'"
      max-width="760px"
      max-height="920px"
      :dismissable="false"
    >
      <form @submit.prevent="submitRoute">
        <div class="form-grid">
          <div class="field span2">
            <label>公开模型名 *</label>
            <input v-model="form.public_name" class="input mono" placeholder="gpt-4o" />
            <span class="tip">客户端在 /v1/chat/completions 里填的 model 名</span>
          </div>
        </div>

        <!-- 有序上游链 -->
        <div class="chain">
          <div class="chain-head">
            <label class="section-label">上游链（自上而下依次尝试）</label>
            <button type="button" class="btn btn-sm btn-ghost" :disabled="chainBlocked" @click="addTarget">+ 添加目标</button>
          </div>
          <!-- 链的加载态必须可见：否则弹窗里静静躺着一行占位主目标，看着就像
               「这条路由本来就只有一个上游」，用户会直接保存。 -->
          <p v-if="chainLoading" class="chain-loading" role="status">正在读取上游链…</p>
          <div v-for="(t, i) in form.targets" :key="i" class="chain-row">
            <span class="chain-idx">{{ i + 1 }}</span>
            <select v-model="t.provider_id" class="select" :disabled="chainBlocked" @change="onTargetProviderChange(t)">
              <option value="" disabled>选择上游</option>
              <option v-for="p in providers" :key="p.id" :value="p.id">{{ p.name }} ({{ p.slug }})</option>
            </select>
            <select v-model="t.upstream_model_id" class="select" :disabled="chainBlocked || !t.provider_id">
              <option value="" disabled>选择模型</option>
              <option v-for="m in modelsOf(t.provider_id)" :key="m.id" :value="m.id">
                {{ m.display_name ? `${m.display_name} (${m.model_id})` : m.model_id }}
              </option>
            </select>
            <label class="check-inline" title="是否在链中启用">
              <input v-model="t.enabled" type="checkbox" /> 启用
            </label>
            <div class="chain-ops">
              <button type="button" class="btn btn-sm btn-ghost" :disabled="chainBlocked || i === 0" @click="moveTarget(i, -1)">↑</button>
              <button type="button" class="btn btn-sm btn-ghost" :disabled="chainBlocked || i === form.targets.length - 1" @click="moveTarget(i, 1)">↓</button>
              <button type="button" class="btn btn-sm btn-danger" :disabled="chainBlocked" @click="removeTarget(i)">删</button>
            </div>
          </div>
        </div>

        <!-- 故障转移开关（策略参数在「设置」页统一配置） -->
        <div class="failover">
          <label class="check-line">
            <input v-model="form.failover_enabled" type="checkbox" />
            启用自动故障转移（关闭则只打链首一个目标）
          </label>
          <p v-if="form.failover_enabled" class="tip" style="margin-top: 6px">
            尝试目标数、失败熔断阈值与各类超时是<strong>全局设置</strong>，请到
            <RouterLink to="/settings">设置</RouterLink> 页调整。
          </p>
        </div>

        <!-- 「启用该路由」勾选框已去掉（2026-10-10 用户要求）。
             改为**默认创建即启用**：新建的路由本来就是拿来用的，
             先建出一个停用状态、再让管理员去列表里点一次开关，
             等于把一个必然动作拆成两步 —— 而漏掉第二步的症状是
             「路由建好了但请求全 404」，且列表上那个小小的「停用」徽章
             很容易被忽略。
             启停能力**没有消失**：列表行的开关（toggleRoute）仍然在，
             要停用随时可以停 —— 那里才是「运行中改状态」的正确位置，
             而创建表单里问「要不要启用」本质上是在问一个还没发生的事。 -->
        <p v-if="chainError" class="chain-error" role="alert">{{ chainError }}</p>

        <div class="form-actions">
          <button type="button" class="btn btn-ghost" :disabled="saving" @click="form.open = false">取消</button>
          <button type="submit" class="btn btn-primary" :disabled="chainBlocked">
            {{ saving ? '保存中…' : '保存' }}
          </button>
        </div>
      </form>
    </AppModal>
  </main>
</template>

<style scoped>
.chain {
  margin: 12px 0;
  padding: 12px;
  border: 1px solid var(--border);
  border-radius: var(--r-thumb, 12px);
}
.chain-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 8px;
}
.section-label {
  font-weight: 600;
}
.chain-row {
  display: grid;
  grid-template-columns: auto 1fr 1fr auto auto;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}
.chain-idx {
  width: 20px;
  text-align: center;
  opacity: 0.6;
}
.chain-ops {
  display: flex;
  gap: 4px;
}
.check-inline {
  white-space: nowrap;
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
.failover {
  margin: 12px 0;
  padding: 12px;
  border: 1px dashed var(--border);
  border-radius: var(--r-thumb, 12px);
}
/* 链加载中的说明：与失败提示同位置，避免加载态被读成「链就是这么短」 */
.chain-loading {
  margin: 0 0 8px;
  font-size: 13px;
  color: var(--text-3);
}
/* 链加载失败提示：warning（暖橙）是设计系统里的专用告警色 */
.chain-error {
  padding: 10px 12px;
  border-radius: var(--r-chip, 6px);
  background: var(--warning-soft);
  color: var(--warning-soft-foreground);
  font-size: 13px;
  line-height: 1.5;
}
</style>
