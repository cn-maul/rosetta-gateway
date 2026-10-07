<script setup lang="ts">
/**
 * 分组与模型白名单（仅管理员可见）。
 *
 * # 两条最容易踩错的产品语义，界面必须讲清楚
 *
 * 1. **空白名单 = 不限制**，不是「什么都看不到」。
 *    新建的组默认就是空的，所以「建了组但没配模型」= 权限比不分组还宽
 *    （不分组也是不限制，但至少不会让人误以为收紧了）。列表里对空白名单
 *    的组显式标注「未限制」，并对未配置的组给一条提示。
 *
 * 2. **组里还有人、还有密钥时不能删**（后端 409）。删掉会让那批账号与密钥
 *    从「受限」变成「不受限」—— 一次删除操作等于给一组人扩权。这是**静默
 *    放宽**：账号还在、密钥还能用，只是模型白名单凭空消失了。
 *    所以两类绑定都要在界面上说清楚（列表里分别显示数量），
 *    并且任一 count > 0 时**不给「删除」这个选项**：后端一定会拒绝，
 *    让管理员先按一次确认、再收到一句 409，等于把「我早就知道会失败」
 *    包装成一次操作。后端仍然返回同一句含处置办法的文案，
 *    前端把它原样展示（并发下才可能命中）。
 *
 * # 白名单是精确匹配
 *
 * 所以模型清单直接从「路由」取（与后端校验同源），不给自由输入框 ——
 * 拼错一个字符的结果是「这个模型谁都看不到」而界面上毫无异常。
 */
import { onMounted, ref } from 'vue'
import { api, ApiFail } from '../api'
import type { Group } from '../types'
import { toast, confirmBox } from '../ui'
import AppModal from '../components/AppModal.vue'
import ModelPicker from '../components/ModelPicker.vue'

const groups = ref<Group[]>([])
/** 可选模型名，来自 /admin/api/model-names —— 与后端白名单校验**同一个数据源**。
 *
 *  刻意不用 /admin/api/routes：那是另一个来源，两处一旦漂移就会出现
 *  「界面能勾、但保存时被后端拒」的错位。 */
const modelOptions = ref<string[]>([])
const loading = ref(false)
const err = ref('')

const showForm = ref(false)
const editing = ref<Group | null>(null)
const fName = ref('')
const fDesc = ref('')
const fErr = ref('')
const fBusy = ref(false)

/** 正在编辑白名单的组；null = 未打开。 */
const modelsFor = ref<Group | null>(null)
const picked = ref<string[]>([])
const mErr = ref('')
const mBusy = ref(false)

/** 删除请求在飞行中。防连点：确认框结算前按钮仍可点，
 *  连点会给同一行发两次 DELETE，第二次必然 404。 */
const deleting = ref(false)

async function load() {
  loading.value = true
  err.value = ''
  try {
    const [g, names] = await Promise.all([api.groups(), api.modelNames()])
    groups.value = g
    modelOptions.value = names.models
  } catch (e) {
    if (e instanceof ApiFail && e.status === 401) return
    err.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

function openCreate() {
  editing.value = null
  fName.value = ''
  fDesc.value = ''
  fErr.value = ''
  showForm.value = true
}

function openEdit(g: Group) {
  editing.value = g
  fName.value = g.name
  fDesc.value = g.description
  fErr.value = ''
  showForm.value = true
}

async function submitForm() {
  if (fBusy.value) return
  fErr.value = ''
  if (!fName.value.trim()) {
    fErr.value = '组名必填'
    return
  }
  fBusy.value = true
  try {
    if (editing.value) {
      await api.updateGroup(editing.value.id, { name: fName.value.trim(), description: fDesc.value.trim() })
      toast('已保存', 'ok')
    } else {
      await api.createGroup({ name: fName.value.trim(), description: fDesc.value.trim() })
      toast('已创建。注意：新组未配模型白名单，此时该组不限制模型可见范围。', 'ok')
    }
    showForm.value = false
    await load()
  } catch (e) {
    fErr.value = e instanceof ApiFail ? e.message : '保存失败'
  } finally {
    fBusy.value = false
  }
}

function openModels(g: Group) {
  modelsFor.value = g
  picked.value = [...g.models]
  mErr.value = ''
}

async function submitModels() {
  const g = modelsFor.value
  if (!g || mBusy.value) return
  mErr.value = ''
  mBusy.value = true
  try {
    await api.setGroupModels(g.id, [...picked.value])
    toast(picked.value.length === 0 ? '已清空白名单（该组不再限制模型）' : '白名单已保存', 'ok')
    modelsFor.value = null
    await load()
  } catch (e) {
    mErr.value = e instanceof ApiFail ? e.message : '保存失败'
  } finally {
    mBusy.value = false
  }
}

/** 删除被后端拒绝的两种绑定。
 *
 * users.group_id 与 access_keys.group_id 都是 ON DELETE SET NULL，
 * 两类引用**各自**都能让 DeleteGroup 回 ErrGroupNotEmpty ——
 * 只统计账号数会漏掉「组里没人、但有一把密钥指定了分组覆盖」的情形，
 * 那时界面显示「可删」，点下去却必然 409。 */
function boundCounts(g: Group): { users: number; keys: number } {
  return { users: g.member_count ?? 0, keys: g.key_count ?? 0 }
}

/** 该组现在能不能删。任一绑定 > 0 时被后端拒绝，界面因此不给入口。 */
function canDelete(g: Group): boolean {
  const c = boundCounts(g)
  return c.users === 0 && c.keys === 0
}

/** 删除按钮的 title：被挡住时必须说清是**哪一类**绑定挡的、以及怎么解。
 *
 * 只写「不能删除」会让人去翻后端文档；而真正该做的动作取决于类型：
 * 账号要迁走，密钥要解除「分组覆盖」（Keys 页的那一个字段）。 */
function deleteBlockedTitle(g: Group): string {
  const c = boundCounts(g)
  if (c.users > 0 && c.keys > 0) {
    return `该组下还有 ${c.users} 个账号和 ${c.keys} 把访问密钥。请先把账号移到别的组，并解除这些密钥的分组覆盖。`
  }
  if (c.users > 0) {
    return `该组下还有 ${c.users} 个账号。请先把他们移到别的组或移出分组，否则删除会被后端拒绝。`
  }
  if (c.keys > 0) {
    return `该组下还有 ${c.keys} 把访问密钥指定了分组覆盖。请先到「访问密钥」解除这些密钥的分组覆盖。`
  }
  return ''
}

async function askDelete(g: Group) {
  // 前置拦截，而不是「先确认再等一个 409」：后端一定会拒绝，
  // 让管理员为一个注定失败的操作多按一次确认毫无意义。
  const c = boundCounts(g)
  if (c.users > 0 || c.keys > 0) {
    toast(deleteBlockedTitle(g), 'err')
    return
  }
  if (deleting.value) return
  const ok = await confirmBox({
    title: '删除分组',
    body: `将删除「${g.name}」及其模型白名单。当前没有账号或访问密钥绑定在该组上。`,
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return
  deleting.value = true
  try {
    await api.deleteGroup(g.id)
    toast('已删除', 'ok')
    await load()
  } catch (e) {
    // 409 的文案是后端给的（含人数、密钥数与处置办法），原样展示比
    // 「删除失败」有用得多。走到这里只可能是并发：别人在我们这次 load()
    // 之后刚把账号或密钥绑了进来。
    toast(e instanceof ApiFail ? e.message : '删除失败', 'err')
  } finally {
    deleting.value = false
  }
}

onMounted(load)
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>分组</h1>
        <div class="sub">按组限定可见的模型范围</div>
      </div>
      <div class="head-actions">
        <button class="btn" :disabled="loading" @click="load">刷新</button>
        <button class="btn btn-primary" @click="openCreate">新建分组</button>
      </div>
    </div>

    <div class="panel note">
      未分组、或所在组未配白名单 → <strong>不限制</strong>模型可见范围。
      白名单与账号自己的密钥白名单<b>取交集</b>，密钥那一层只会更紧。
    </div>

    <div class="panel">
      <div v-if="loading && groups.length === 0" class="loading">加载中…</div>
      <div v-else-if="err" class="empty"><div class="big">⚠</div>{{ err }}</div>
      <div v-else-if="groups.length === 0" class="empty">
        <div class="big">◇</div>还没有分组 —— 当前所有密钥都能看到全部模型
      </div>
      <div v-else class="tbl-wrap">
        <table class="tbl">
          <thead>
            <tr>
              <th>分组</th>
              <th>说明</th>
              <th>模型白名单</th>
              <th class="c-act">操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="g in groups" :key="g.id">
              <td>
                <div class="cell-group">
                  <span class="name">{{ g.name }}</span>
                  <!-- 账号与密钥**分开显示**：两者都会阻止删除，但处置办法不同
                       （前者要迁组，后者要解除「分组覆盖」），合成一个数字
                       就没法告诉管理员该去哪改。 -->
                  <span class="badge badge-off">{{ g.member_count }} 个账号</span>
                  <span class="badge" :class="g.key_count > 0 ? 'badge-off' : 'badge-off dim-badge'">
                    {{ g.key_count }} 把密钥
                  </span>
                  <span v-if="g.models.length === 0" class="badge badge-warn">未限制模型</span>
                  <span v-else class="badge badge-live">{{ g.models.length }} 个模型</span>
                </div>
              </td>
              <td class="dim">{{ g.description || '—' }}</td>
              <td class="cell-models mono">
                {{ g.models.length === 0 ? '（未配置白名单 = 不限制）' : g.models.join('、') }}
              </td>
              <td class="c-act">
                <div class="row-actions">
                  <button class="btn btn-sm" @click="openModels(g)">配置模型</button>
                  <button class="btn btn-sm" @click="openEdit(g)">改名</button>
                  <button class="btn btn-sm btn-danger" :disabled="!canDelete(g) || deleting" :title="deleteBlockedTitle(g)" @click="askDelete(g)">删除</button>
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>

    <!-- 新建 / 改名：与其他页同一套 AppModal；表单不可点外关闭（与 Users 页同一规则）。 -->
    <AppModal :open="showForm" :title="editing ? '编辑分组' : '新建分组'" max-width="460px" :dismissable="false">
        <label class="flabel" for="gn">组名</label>
        <input id="gn" v-model="fName" class="input" placeholder="研发 / 外包 / 试用" />
        <label class="flabel" for="gd">说明</label>
        <input id="gd" v-model="fDesc" class="input" placeholder="可选" />
        <div v-if="!editing" class="fhint warn">
          新建的组默认<b>不限制</b>模型。建好后请到「配置模型」里勾选允许的模型。
        </div>
        <div v-if="fErr" class="fhint err">{{ fErr }}</div>
        <div class="form-actions">
          <button class="btn" @click="showForm = false">取消</button>
          <button class="btn btn-primary" :disabled="fBusy" @click="submitForm">
            {{ fBusy ? '保存中…' : '保存' }}
          </button>
        </div>
    </AppModal>

    <!-- 配置模型白名单：勾选状态同样不该被误点遮罩清空。 -->
    <AppModal :open="!!modelsFor" :title="modelsFor ? `模型白名单 · ${modelsFor.name}` : '模型白名单'" max-width="560px" :dismissable="false">
        <div v-if="modelOptions.length === 0" class="fhint warn">
          还没有任何路由（公开模型名）。请先到「上游与模型 / 路由」里建好。
        </div>
        <ModelPicker v-else v-model="picked" :options="modelOptions" />

        <div v-if="mErr" class="fhint err">{{ mErr }}</div>
        <div class="form-actions">
          <button class="btn" @click="modelsFor = null">取消</button>
          <button class="btn btn-primary" :disabled="mBusy" @click="submitModels">
            {{ mBusy ? '保存中…' : `保存（已选 ${picked.length}）` }}
          </button>
        </div>
    </AppModal>
  </main>
</template>

<style scoped>
.note { font-size: 12px; color: var(--muted); line-height: 1.7; }
.note strong { color: var(--foreground); }
.badge-warn { background: var(--accent-soft); color: var(--accent-soft-foreground); }
/* 0 把密钥不是「值得注意的状态」—— 弱化它，免得每行都挂两个同色徽章。 */
.dim-badge { opacity: 0.55; }
/* 表格控件与标签、提示用全局 .tbl / .input / .flabel / .fhint / .row-actions / .form-actions（styles.css），这里只留页面私有部分。 */
.cell-group { display: flex; align-items: center; gap: 6px; flex-wrap: wrap; }
.cell-group .name { font-weight: 500; }
/* 白名单列表可能很长：主战场是这张表的宽度，宁可让 tbl-wrap 出横向滚动。 */
.cell-models { max-width: 480px; }

</style>
