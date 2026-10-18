<script setup lang="ts">
import DataTable from '../components/DataTable.vue'
/**
 * 模型归组（管理员）。把不同上游的「同一个模型」标成一组，用量统计按组名合并。
 *
 * 为什么单独一页而不是塞进 Providers 展开区：归组是**跨上游**的概念 ——
 * 一个组的成员天然分散在不同 provider 下，而 Providers 的展开区是按单个
 * provider 组织的，表达不了「把这三家各自的模型编成一组」。放这一页，
 * 左列组、右列组内成员，跨 provider 的归属一眼可见。
 *
 * 数据面不读归组（纯统计/展示层，见 store.go group_id 列注释），所以本页
 * 的任何改动都不影响路由与转发 —— 改错了也不会把流量打挂。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import { api } from '../api'
import { toast, confirmBox } from '../ui'
import AppModal from '../components/AppModal.vue'
import type { ModelGroup, Provider, UpstreamModel } from '../types'

const err = ref('')
const loading = ref(true)
const groups = ref<ModelGroup[]>([])
// 全部上游模型（跨 provider 扁平列表）：指派时按 provider 分组供选择。
const allModels = ref<UpstreamModel[]>([])
// 上游列表：只为了把 provider_id 翻成人类可读的名字（见 providerName）。
const providers = ref<Provider[]>([])

async function load() {
  loading.value = true
  err.value = ''
  try {
    const [g, m, p] = await Promise.all([api.modelGroups(), api.allModels(), api.providers()])
    groups.value = g
    allModels.value = m
    providers.value = p
  } catch (e) {
    if ((e as { status?: number }).status === 401) return
    err.value = '加载模型组失败：' + (e as Error).message
    toast('加载模型组失败：' + (e as Error).message, 'err')
  } finally {
    loading.value = false
  }
}
onMounted(load)

// providerId → 显示名。
//
// 必须来自 /providers（带真实 name/slug），不能拿 provider_id 顶上 ——
// provider_id 是 generateID() 的 32 位哈希（「7f8572482cff128dfe8bb758db4dc32f」），
// 直接显示就是用户看到的乱码。
//
// 回退顺序 name → slug → 短 id：provider 可能没填 name（表单允许留空，
// 后端回落 endpoint），这时 slug 才是人类可读的；两者都没有才截 id 前 8 位，
// 至少比 32 位哈希短。
const providerName = computed(() => {
  const map: Record<string, string> = {}
  for (const p of providers.value) {
    map[p.id] = p.name || p.slug || p.id.slice(0, 8)
  }
  return map
})

// modelName 把组 id 翻成组名（成员表里显示归属用）。
const groupNameById = computed(() => {
  const map: Record<string, string> = {}
  for (const g of groups.value) map[g.id] = g.display_name
  return map
})

// ---------- 建组 / 改名 ----------

const gForm = reactive({ open: false, editing: '' as string, display_name: '' })

function openCreate() {
  gForm.editing = ''
  gForm.display_name = ''
  gForm.open = true
}
function openRename(g: ModelGroup) {
  gForm.editing = g.id
  gForm.display_name = g.display_name
  gForm.open = true
}

async function submitGroup() {
  const name = gForm.display_name.trim()
  if (!name) {
    toast('组显示名不能为空', 'err')
    return
  }
  try {
    if (gForm.editing) {
      await api.updateModelGroup(gForm.editing, name)
      toast('组已改名')
    } else {
      const created = await api.createModelGroup(name)
      // 建完立刻进入「添加成员」，而不是让用户再点一次 —— 空组没有任何作用。
      gForm.open = false
      await load()
      openMembers(groups.value.find((g) => g.id === created.id) ?? created)
      return
    }
    gForm.open = false
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('保存失败：' + (e as Error).message, 'err')
  }
}

async function removeGroup(g: ModelGroup) {
  const ok = await confirmBox({
    title: `删除模型组「${g.display_name}」？`,
    // 讲清后果：删组**不删模型**，只是组内模型回到未归组、统计重新分开。
    // 不说清会让人以为删组等于删了一批模型（那是另一回事）。
    body: `组内 ${g.member_count} 个模型不会被删除，它们回到「未归组」，用量重新按各自名字分开统计。`,
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return
  try {
    await api.deleteModelGroup(g.id)
    toast('模型组已删除')
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('删除失败：' + (e as Error).message, 'err')
  }
}

// ---------- 成员管理（指派 / 移出）----------

const mForm = reactive({ open: false, group: null as ModelGroup | null, addIds: new Set<string>() })

function openMembers(g: ModelGroup) {
  mForm.group = g
  mForm.addIds = new Set()
  mForm.open = true
}

// 当前组成员：group_id === 本组的模型。
const currentMembers = computed(() =>
  mForm.group ? allModels.value.filter((mm) => mm.group_id === mForm.group!.id) : [],
)
// 可加入的候选：还没进**任何**组的模型。一个模型最多属一组（group_id 单列），
// 已在别组的要先把那行移出去，不能在这一页跨组「抢」。
const availableModels = computed(() =>
  allModels.value.filter((mm) => !mm.group_id),
)

function toggleAdd(id: string) {
  const next = new Set(mForm.addIds)
  if (next.has(id)) next.delete(id)
  else next.add(id)
  mForm.addIds = next
}

async function applyMembers() {
  if (!mForm.group) return
  const addIds = [...mForm.addIds]
  if (addIds.length === 0) {
    mForm.open = false
    return
  }
  try {
    await api.assignModelsToGroup(mForm.group.id, addIds)
    toast(`已把 ${addIds.length} 个模型并入「${mForm.group.display_name}」`)
    mForm.open = false
    await load()
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('指派失败：' + (e as Error).message, 'err')
  }
}

async function removeMember(mm: UpstreamModel) {
  if (!mForm.group) return
  try {
    await api.unassignModelsFromGroup([mm.id])
    toast(`已把「${mm.model_id}」移出组`)
    // 局部刷新：重取组列表 + 模型列表，currentMembers 随之更新。
    await load()
    // load 后原 group 对象换了引用，把表单里的 group 指回新的同 id 项。
    mForm.group = groups.value.find((g) => g.id === mForm.group!.id) ?? mForm.group
  } catch (e) {
    if ((e as { status?: number }).status !== 401) toast('移出失败：' + (e as Error).message, 'err')
  }
}

// labelFor 给候选/成员行一个「供应商 / 模型名」的可读标签。
function labelFor(mm: UpstreamModel): string {
  // 回退也截短 id：provider 没出现在 /providers 里时（刚删、或接口部分失败），
  // 退回完整 32 位哈希就是用户看到的那串乱码 —— 截 8 位至少还能认。
  const name = providerName.value[mm.provider_id] ?? mm.provider_id.slice(0, 8)
  return `${name} / ${mm.model_id}`
}
</script>

<template>
  <!-- 面板形态（2026-10-14 页面合并）：并进 Upstream.vue 的「模型归组」页签。
       标题 / 刷新由宿主页统一提供。

       这里的 .tip 说明**刻意保留**：它讲的是「归组只影响统计展示，不改变
       路由与转发」—— 一条容易被误解为「改了会动线上流量」的边界，而页签
       本身只有两个字「归组」，扛不住这句话。宿主页不重复它。 -->
  <div class="panel-body">
    <div class="panel-actions">
      <button class="btn btn-primary btn-sm" type="button" @click="openCreate">新建模型组</button>
    </div>

    <div class="panel">
      <div class="panel-note">
        把不同上游的「同一个模型」（叫法往往不同）编成一组，调用统计按组名合并显示。
        归组只影响统计展示，<b>不改变路由与转发</b>。一个模型最多属于一组。
      </div>

      <div v-if="loading" class="loading">加载中…</div>
      <div v-else-if="err" class="empty">{{ err }}</div>
      <div v-else-if="groups.length === 0" class="empty">
        还没有模型组 —— 点上方「新建模型组」，再把各上游的同名模型并进来
      </div>

    <DataTable v-else label="模型分组">
      <table class="tbl">
        <thead>
          <tr>
            <th>组名（统计里显示这个名字）</th>
            <th class="num-h">成员数</th>
            <th class="c-op">操作</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="g in groups" :key="g.id">
            <td>{{ g.display_name }}</td>
            <td class="num-h">{{ g.member_count }}</td>
            <td class="c-op">
              <div class="row-actions">
                <button class="btn btn-sm btn-ghost" @click="openMembers(g)">成员</button>
                <button class="btn btn-sm btn-ghost" @click="openRename(g)">改名</button>
                <button class="btn btn-sm btn-danger" @click="removeGroup(g)">删除</button>
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </DataTable>
    </div>

    <!-- 建组 / 改名 -->
    <AppModal
      :open="gForm.open"
      :title="gForm.editing ? '改名' : '新建模型组'"
      max-width="420px"
      :dismissable="false"
    >
      <form @submit.prevent="submitGroup">
        <div class="field span2">
          <label for="cgw-lModelGroups-1">组显示名 *</label>
          <input v-model="gForm.display_name" class="input" placeholder="DeepSeek V3" id="cgw-lModelGroups-1"/>
          <span class="tip">统计页与模型名会显示成这个名字，全库唯一</span>
        </div>
        <div class="form-actions">
          <div class="actions-right">
            <button type="button" class="btn btn-ghost" @click="gForm.open = false">取消</button>
            <button type="submit" class="btn btn-primary">保存</button>
          </div>
        </div>
      </form>
    </AppModal>

    <!-- 成员管理 -->
    <AppModal
      :open="mForm.open"
      :title="mForm.group ? `「${mForm.group.display_name}」的成员` : '成员'"
      max-width="560px"
      :dismissable="false"
    >
      <div class="group-members">
        <div class="gm-section">
          <div class="gm-h">当前成员（{{ currentMembers.length }}）</div>
          <div v-if="currentMembers.length === 0" class="dim gm-empty">组里还没有模型</div>
          <div v-for="mm in currentMembers" :key="mm.id" class="gm-row">
            <span class="mono gm-name">{{ labelFor(mm) }}</span>
            <button class="btn btn-sm btn-ghost" @click="removeMember(mm)">移出</button>
          </div>
        </div>

        <div class="gm-section">
          <div class="gm-h">添加到本组（勾选后点「应用」）</div>
          <div v-if="availableModels.length === 0" class="dim gm-empty">没有未归组的模型可添加</div>
          <div v-for="mm in availableModels" :key="mm.id" class="gm-row">
            <label class="gm-pick">
              <input type="checkbox" :checked="mForm.addIds.has(mm.id)" @change="toggleAdd(mm.id)"/>
              <span class="mono gm-name">{{ labelFor(mm) }}</span>
            </label>
          </div>
        </div>
      </div>
      <div class="form-actions">
        <div class="actions-right">
          <button type="button" class="btn btn-ghost" @click="mForm.open = false">关闭</button>
          <button
            type="button"
            class="btn btn-primary"
            :disabled="mForm.addIds.size === 0"
            @click="applyMembers"
          >
            应用（加 {{ mForm.addIds.size }} 个）
          </button>
        </div>
      </div>
    </AppModal>
  </div>
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
  max-width: 640px;
  line-height: 1.5;
}
.actions-right {
  display: flex;
  gap: 10px;
}
.group-members {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.gm-h {
  font-size: 13px;
  font-weight: 600;
  margin-bottom: 6px;
}
.gm-empty {
  font-size: 12px;
  padding: 4px 0;
}
.gm-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  padding: 3px 0;
}
.gm-pick {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
  cursor: pointer;
}
.gm-pick input {
  accent-color: var(--accent);
}
.gm-name {
  font-size: 13px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
