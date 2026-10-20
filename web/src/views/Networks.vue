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
import type { Network } from '../types'
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
  err.value = '加载网络失败：' + message
  toast('加载网络失败：' + message, 'err')
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
    toast('网络名称不能为空', 'err')
    return
  }
  if (!proxyURL) {
    // 直连已经有（且不需要）一个入口，再让用户建一条空地址的「网络」
    // 只会造出第二种「不代理」的表示。
    toast('代理地址不能为空 —— 不代理请在上游那里选「直连」', 'err')
    return
  }
  nForm.saving = true
  try {
    if (nForm.editing) {
      await api.updateNetwork(nForm.editing, { name, proxy_url: proxyURL })
      toast('网络已更新')
    } else {
      await api.createNetwork({ name, proxy_url: proxyURL })
      toast('网络已创建')
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
    title: `删除网络「${n.name}」？`,
    // 讲清后果：引用它的上游会回到直连，**不会**被删掉。删网络看起来像
    // 「让一批上游改走直连」—— 必须说，否则有人会以为是在删上游。
    body: n.provider_count > 0
      ? `正在使用它的 ${n.provider_count} 个上游会退回「直连」（上游本身不会被删除）。之后可以重新建一条同名网络再选回去。`
      : '当前没有上游使用它。',
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return
  try {
    await api.deleteNetwork(n.id)
    toast('网络已删除')
    await loadWithError()
  } catch (e) {
    if (!is401(e)) toast('删除失败：' + (e as Error).message, 'err')
  }
}

// 「直连」那一行常驻显示：它是合法且最常用的选项（新建上游的默认值），
// 但它不是一条 networks 记录 —— 不把它显式画出来，界面上就会出现
// 「这个网关只能走代理？」的误解。
const rows = computed(() => networks.value)
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h1>出站网络</h1>
        <div class="sub">可复用的代理地址清单：填一次，各上游从下拉里选 · 新建上游默认直连</div>
      </div>
      <div class="head-actions">
        <button class="btn" :disabled="loading" @click="loadWithError">刷新</button>
        <button class="btn btn-primary" @click="openCreate">新建网络</button>
      </div>
    </div>

    <p class="context-note">
      出站网络是一份<b>可复用</b>的代理地址清单：同一个代理被多个上游共用时填一次，
      上游从下拉里选，改一处全部生效。<b>新建上游默认走「直连」</b>，
      不需要代理的绝大多数上游什么都不用填。
    </p>

    <div class="direct-row">
      <span class="badge badge-live">直连</span>
      <span class="dim">不经过代理（读进程环境变量里的 <code>HTTP_PROXY</code> / <code>HTTPS_PROXY</code>）—— 保留选项，不是可编辑的网络。</span>
    </div>

    <DataTable class="panel" label="网络列表" :loading="loading" :error="err" :has-data="!!rows.length" :empty="!rows.length" @retry="loadWithError">
      <template #empty>
        <template v-if="!rows.length">还没有自建网络 —— 全部上游走「直连」即可。需要代理时点上方「新建网络」。</template>
      </template>
      <table class="tbl">
        <thead>
          <tr>
            <th>名称</th>
            <th>代理地址</th>
            <th class="num-h">使用中的上游</th>
            <th class="c-op">操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="n in rows" :key="n.id">
            <td>{{ n.name }}</td>
            <td class="mono addr">{{ n.proxy_url }}</td>
            <td class="num-h">{{ n.provider_count }}</td>
            <td class="c-op">
              <div class="row-actions">
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
      :title="nForm.editing ? '编辑网络' : '新建网络'"
      max-width="520px"
      :dismissable="false"
    >
      <form @submit.prevent="submit">
        <div class="form-grid">
          <div class="field span2">
            <label for="cgw-lNetworks-1">名称 *</label>
            <input v-model="nForm.name" class="input" placeholder="公司代理 / 本机 Clash" id="cgw-lNetworks-1"/>
            <span class="tip">上游下拉里显示的就是这个名字，全库唯一。填「直连」以外的可读名字即可。</span>
          </div>
          <div class="field span2">
            <label for="cgw-lNetworks-2">代理地址 *</label>
            <input v-model="nForm.proxy_url" class="input mono" placeholder="http://127.0.0.1:7897" autocomplete="off" id="cgw-lNetworks-2"/>
            <span class="tip">
              支持 <code>http</code> / <code>https</code> / <code>socks5</code> /
              <code>socks5h</code>（<code>socks5h</code> 让 DNS 也走代理，本地 DNS 被污染时用它）。
              保存时会校验，写错当场报错 —— 不会等到请求打过去才发现连不上。
              <br/>
              <strong>网关跑在 Docker 里时注意</strong>：<code>127.0.0.1</code>
              指的是网关容器自己，不是宿主机。代理也在容器里的话，填
              <code>http://代理容器名:端口</code>（同一 Docker 网络内用容器名互访）；
              代理在宿主机上时才用 <code>host.docker.internal</code>。
            </span>
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
.direct-row {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 0 14px;
  font-size: 13px;
  flex-wrap: wrap;
}
.addr { overflow-wrap: anywhere; }
.actions-right { display: flex; gap: 10px; }
</style>
