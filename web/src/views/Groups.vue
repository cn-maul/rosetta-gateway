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
 * 2. **组里还有人时不能删**（后端 409）。删掉会让那批人从「受限」变成
 *    「不受限」—— 一次删除操作等于给一组人扩权。界面上把后端的原话展示出来，
 *    而不是笼统提示「删除失败」。
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

async function askDelete(g: Group) {
  const ok = await confirmBox({
    title: '删除分组',
    body:
      g.member_count > 0
        ? `「${g.name}」下还有 ${g.member_count} 个账号，删除会被拒绝（他们的模型权限会被放大）。请先把他们移到别的组。`
        : `将删除「${g.name}」及其模型白名单。`,
    danger: true,
    confirmLabel: '删除',
  })
  if (!ok) return
  try {
    await api.deleteGroup(g.id)
    toast('已删除', 'ok')
    await load()
  } catch (e) {
    // 409 的文案是后端给的（含人数与处置办法），原样展示比「删除失败」有用得多。
    toast(e instanceof ApiFail ? e.message : '删除失败', 'err')
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
      <div v-else class="row-list">
        <div v-for="g in groups" :key="g.id" class="row">
          <div class="row-main">
            <div class="row-title">
              {{ g.name }}
              <span class="badge badge-off">{{ g.member_count }} 个账号</span>
              <span v-if="g.models.length === 0" class="badge badge-warn">未限制模型</span>
              <span v-else class="badge badge-live">{{ g.models.length }} 个模型</span>
            </div>
            <div v-if="g.description" class="row-sub">{{ g.description }}</div>
            <div class="row-sub mono">
              {{ g.models.length === 0 ? '（未配置白名单 = 不限制）' : g.models.join('、') }}
            </div>
          </div>
          <div class="row-actions">
            <button class="btn btn-sm" @click="openModels(g)">配置模型</button>
            <button class="btn btn-sm" @click="openEdit(g)">改名</button>
            <button class="btn btn-sm btn-danger" @click="askDelete(g)">删除</button>
          </div>
        </div>
      </div>
    </div>

    <!-- 新建 / 改名 -->
    <div v-if="showForm" class="modal-mask" @click.self="showForm = false">
      <div class="modal">
        <h2>{{ editing ? '编辑分组' : '新建分组' }}</h2>
        <label class="flabel" for="gn">组名</label>
        <input id="gn" v-model="fName" class="fin" placeholder="研发 / 外包 / 试用" />
        <label class="flabel" for="gd">说明</label>
        <input id="gd" v-model="fDesc" class="fin" placeholder="可选" />
        <div v-if="!editing" class="fhint warn">
          新建的组默认<b>不限制</b>模型。建好后请到「配置模型」里勾选允许的模型。
        </div>
        <div v-if="fErr" class="fhint err">{{ fErr }}</div>
        <div class="modal-actions">
          <button class="btn" @click="showForm = false">取消</button>
          <button class="btn btn-primary" :disabled="fBusy" @click="submitForm">
            {{ fBusy ? '保存中…' : '保存' }}
          </button>
        </div>
      </div>
    </div>

    <!-- 配置模型白名单 -->
    <div v-if="modelsFor" class="modal-mask" @click.self="modelsFor = null">
      <div class="modal">
        <h2>模型白名单</h2>
        <div class="sub">组「{{ modelsFor.name }}」可见的模型</div>

        <div v-if="modelOptions.length === 0" class="fhint warn">
          还没有任何路由（公开模型名）。请先到「上游与模型 / 路由」里建好。
        </div>
        <ModelPicker v-else v-model="picked" :options="modelOptions" />

        <div v-if="mErr" class="fhint err">{{ mErr }}</div>
        <div class="modal-actions">
          <button class="btn" @click="modelsFor = null">取消</button>
          <button class="btn btn-primary" :disabled="mBusy" @click="submitModels">
            {{ mBusy ? '保存中…' : `保存（已选 ${picked.length}）` }}
          </button>
        </div>
      </div>
    </div>
  </main>
</template>

<style scoped>
.note { font-size: 12px; color: var(--muted); line-height: 1.7; }
.note strong { color: var(--foreground); }
.row-actions { display: flex; gap: 6px; flex-wrap: wrap; align-items: flex-start; }
.badge-warn { background: var(--accent-soft); color: var(--accent-soft-foreground); }
.flabel { display: block; font-size: 12px; color: var(--muted); margin: 12px 0 4px; }
.fin {
  width: 100%;
  padding: 8px 10px;
  border: 1px solid var(--border);
  border-radius: var(--r-thumb);
  background: var(--background);
  color: var(--foreground);
  font-size: 14px;
}
.fin:focus-visible { outline: 2px solid var(--accent); outline-offset: 1px; }
.fhint { font-size: 12px; color: var(--muted); margin-top: 6px; }
.fhint.warn { color: var(--danger); }
.fhint.err { color: var(--danger); margin-top: 10px; }
.modal-mask {
  position: fixed; inset: 0; z-index: 50;
  background: var(--backdrop);
  display: grid; place-items: center;
  padding: 20px;
}
.modal {
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--r-sheet);
  padding: 20px;
  width: 100%;
  max-width: 460px;
  max-height: 90dvh;
  overflow-y: auto;
}
.modal h2 { margin: 0 0 4px; font-size: 15px; }
.modal-actions { display: flex; gap: 8px; justify-content: flex-end; margin-top: 20px; }
</style>
