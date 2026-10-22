<script setup lang="ts">
/**
 * 出站网络（管理员）：一份可复用的代理地址清单。
 *
 * # 为什么它是独立一页而不是上游表单里的一个输入框
 *
 * 代理地址天然是**多个上游共用**的 —— 同一个 clash、同一台公司代理。
 * 此前它以纯文本存在每个 provider 行上，于是「改一次代理端口」要在每个
 * 用到它的上游里各改一遍，而手写 URL 极易打错（少个斜杠、端口写错）。
 * 打错不报错，只是连不上，症状是 504 —— 排查方向会先怀疑上游或网络，
 * 而不是「这三个上游共用的那串地址少了个字符」。
 *
 * 建成清单之后：地址填一次到处可选、列表一眼能看出「有哪些网络、
 * 各有多少上游在用」、改一处全部生效。
 *
 * # 直连为什么不是一条记录
 *
 * 直连没有可配置的内容（它的语义就是「不设代理」），所以它是保留 id
 * 'direct'，由上游表单作为下拉的第一项展示，不出现在这张表里 ——
 * 一个「代理地址为空」的行会让「有哪些网络」这个问题的答案多一个废话项，
 * 而它的值恒等于「直连」这一个已经存在的选项。
 *
 * 独立一页（形态同 Providers.vue）：它管的是一张**跨上游**的清单，而上游
 * 列表是按单个上游组织的 —— 把「有哪些网络」塞进某个上游的展开区，
 * 等于要求先选中一个上游才能看到全局有哪些网络可用。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import { toast, confirmBox } from '../ui'
import AppModal from '../components/AppModal.vue'
import DataTable from '../components/DataTable.vue'
import type { Network, NetworkTestResult } from '../types'
import { useNetworks } from '../composables/useNetworks'

const err = ref('')
const { networks, loading, load } = useNetworks()
// 首次进入时 useNetworks 默认是空闲态；管理页一进来就要显示加载态，
// 失败时空态也不能被误读成「确实没有网络」。
loading.value = true

/** 401 由 api 层统一处理（清令牌 + 跳登录），这里不再提示。 */
function is401(e: unknown): boolean {
  return (e as { status?: number }).status === 401
}

async function loadWithError() {
  err.value = ''
  const message = await load()
  if (!message) return
  err.value = '加载代理失败：' + message
  toast('加载代理失败：' + message, 'err')
}
onMounted(loadWithError)

// ---------- 建 / 改 ----------

const nForm = reactive({
  open: false,
  editing: '' as string,
  name: '',
  proxy_url: '',
  saving: false,
})

function openCreate() {
  nForm.editing = ''
  nForm.name = ''
  nForm.proxy_url = ''
  nForm.open = true
}

function openEdit(n: Network) {
  nForm.editing = n.id
  nForm.name = n.name
  nForm.proxy_url = n.proxy_url
  nForm.open = true
}

async function submit() {
  const name = nForm.name.trim()
  const proxyURL = nForm.proxy_url.trim()
  if (!name) {
    toast('代理名称不能为空', 'err')
    return
  }
  if (!proxyURL) {
    // 直连已经有（且不需要）一个入口，再让用户建一条空地址的「网络」
    // 只会造出第二种「不代理」的表示。
    toast('代理地址不能为空', 'err')
    return
  }
  nForm.saving = true
  try {
    if (nForm.editing) {
      await api.updateNetwork(nForm.editing, { name, proxy_url: proxyURL })
      delete testStates[nForm.editing]
      toast('代理已更新')
    } else {
      await api.createNetwork({ name, proxy_url: proxyURL })
      toast('代理已创建')
    }
    nForm.open = false
    await loadWithError()
  } catch (e) {
    if (!is401(e)) toast('保存失败：' + (e as Error).message, 'err')
  } finally {
    nForm.saving = false
  }
}

async function remove(n: Network) {
  const ok = await confirmBox({
    title: `删除代理「${n.name}」？`,
    // 讲清后果：引用它的上游会回到直连，**不会**被删掉。删网络看起来像
    // 「让一批上游改走直连」—— 必须说，否则有人会以为是在删上游。
    body: n.provider_count > 0
      ? `正在使用它的 ${n.provider_count} 个上游会退回「直连」（上游本身不会被删除）。之后可以重新建一条同名代理再选回去。`
      : '当前没有上游使用它。',
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return
  try {
    await api.deleteNetwork(n.id)
    delete testStates[n.id]
    toast('代理已删除')
    await loadWithError()
  } catch (e) {
    if (!is401(e)) toast('删除失败：' + (e as Error).message, 'err')
  }
}

const rows = computed(() => networks.value)

// 连通性结果只保留在当前页面会话：它描述的是「刚才那次测试」，不是配置本身。
type TestState = { pending: boolean; result: NetworkTestResult | null }
const testStates = reactive<Record<string, TestState>>({})

function isTesting(id: string): boolean {
  return testStates[id]?.pending === true
}

function testLabel(id: string): string {
  const state = testStates[id]
  if (!state) return '未测试'
  if (state.pending) return '测试中…'
  if (state.result?.status === 'ok') return `正常 · ${state.result.latency_ms}ms`
  return '失败'
}

function testBadgeClass(id: string): string {
  const state = testStates[id]
  if (!state) return 'badge-dim'
  if (state.pending) return 'badge-warn'
  return state.result?.status === 'ok' ? 'badge-live' : 'badge-err'
}

function testTitle(id: string): string | undefined {
  return testStates[id]?.result?.message
}

async function testProxy(n: Network) {
  if (isTesting(n.id)) return
  testStates[n.id] = { pending: true, result: null }
  try {
    const result = await api.testNetwork(n.id)
    testStates[n.id] = { pending: false, result }
    if (result.status !== 'ok') toast('测试失败：' + result.message, 'err')
  } catch (e) {
    if (is401(e)) {
      delete testStates[n.id]
      return
    }
    const result: NetworkTestResult = {
      status: 'error',
      message: '管理接口调用失败：' + (e as Error).message,
      latency_ms: 0,
    }
    testStates[n.id] = { pending: false, result }
    toast('测试失败：' + (e as Error).message, 'err')
  }
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h1>代理</h1>
      </div>
      <div class="head-actions">
        <button class="btn" :disabled="loading" @click="loadWithError">刷新</button>
        <button class="btn btn-primary" @click="openCreate">新建代理</button>
      </div>
    </div>

    <DataTable class="panel" label="代理列表" :loading="loading" :error="err" :has-data="!!rows.length" :empty="!rows.length" @retry="loadWithError">
      <template #empty>
        <template v-if="!rows.length">暂无代理</template>
      </template>
      <table class="tbl">
        <thead>
          <tr>
            <th>名称</th>
            <th>代理地址</th>
            <th class="num-h">使用中的上游</th>
            <th>联通状态</th>
            <th class="c-op">操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="n in rows" :key="n.id">
            <td>{{ n.name }}</td>
            <td class="mono addr">{{ n.proxy_url }}</td>
            <td class="num-h">{{ n.provider_count }}</td>
            <td>
              <span class="badge" :class="testBadgeClass(n.id)" :title="testTitle(n.id)">
                {{ testLabel(n.id) }}
              </span>
            </td>
            <td class="c-op">
              <div class="row-actions">
                <button class="btn btn-sm btn-ghost" :disabled="isTesting(n.id)" @click="testProxy(n)">
                  {{ isTesting(n.id) ? '测试中…' : '测试' }}
                </button>
                <button class="btn btn-sm btn-ghost" @click="openEdit(n)">编辑</button>
                <button class="btn btn-sm btn-danger" @click="remove(n)">删除</button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </DataTable>

    <AppModal
      :open="nForm.open"
      :title="nForm.editing ? '编辑代理' : '新建代理'"
      max-width="520px"
      :dismissable="false"
    >
      <form @submit.prevent="submit">
        <div class="form-grid">
          <div class="field span2">
            <label for="cgw-lNetworks-1">名称 *</label>
            <input v-model="nForm.name" class="input" placeholder="公司代理 / 本机 Clash" id="cgw-lNetworks-1"/>
          </div>
          <div class="field span2">
            <label for="cgw-lNetworks-2">代理地址 *</label>
            <input v-model="nForm.proxy_url" class="input mono" placeholder="http://127.0.0.1:7897" autocomplete="off" id="cgw-lNetworks-2"/>
          </div>
        </div>
        <div class="form-actions">
          <div class="actions-right">
            <button type="button" class="btn btn-ghost" :disabled="nForm.saving" @click="nForm.open = false">取消</button>
            <button type="submit" class="btn btn-primary" :disabled="nForm.saving">{{ nForm.saving ? '保存中…' : '保存' }}</button>
          </div>
        </div>
      </form>
    </AppModal>
  </div>
</template>

<style scoped>
.addr { overflow-wrap: anywhere; }
.actions-right { display: flex; gap: 10px; }
</style>
