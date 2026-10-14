<script setup lang="ts">
/**
 * 用户 / 分组 / 访问密钥（一个页面，三个页签）。
 *
 * # 为什么三者放在一起
 *
 * 这三样东西是一条**权限链上的三环**，拆开看每一样都缺上下文：
 *
 *   用户（users.group_id） → 分组（模型白名单） → 访问密钥（key 级白名单覆盖）
 *
 * 排查「这个人的 key 为什么调不通那个模型」需要同时看三处：账号有没有分组、
 * 那个组的白名单收没收、这把 key 有没有额外覆盖（覆盖只会更严，见 Keys.vue
 * 的 ModelPicker）。原来它们在三个页面上，人得在「这个人在哪个组」「这个组
 * 有哪些人」「这把 key 限了什么」之间来回跳 —— 每跳一次就得凭记忆把 id/name
 * 对上。
 *
 * 页签顺序按**排查顺序**：先看人（是谁），再看规则（他被收在哪一层），最后看
 * 具体凭据（哪把 key 额外收了多少）。这与 2026-10-10 合并「用户与分组」时定的
 * 顺序是同一条线，只是把凭据接在最后。
 *
 * # 三者互相引用，所以数据一次取完
 *
 * 用户的分组名来自分组表，分组的成员数来自服务端算好的字段 —— 分开请求会让
 * 「刚改了谁的分组」之后两张表的新鲜度对不上。
 *
 * # 页签按身份收窄
 *
 * 普通用户只看到**访问密钥**那一个页签，另两个不出现：
 *   - /admin/api/users 与 /admin/api/groups 都是 admin-only，普通用户调它们
 *     只会白吃一个 403 —— 给出入口等于给出无意义的错误；
 *   - 他们的访问密钥列表本来就只有自己（后端按 user_id 收窄），「用户」
 *     「分组」对他们没有可操作的对象。
 *
 * 所以这一页对普通用户**不带 adminOnly**：他们在侧栏点「用户与分组」进来，
 * 看到的就是自己的密钥 —— 与合并前点「访问密钥」看到的东西完全一致。
 *
 * # 刷新只重建当前页签的面板
 *
 * 刻意**不做「三页签全刷」**：切到密钥页签时刷用户表，管理员会白打一次、
 * 普通用户必然吃一个 403，而该更新的密钥列表没更新 —— 两头不讨好。
 *
 * 用「递增 key 强制重建当前面板」实现：面板重新挂载时它自己的 onMounted(load)
 * 会把该拿的数据拿全，同时卸载的旧面板不留残余加载态。
 * 这也让三个面板的数据生命周期互不牵连 —— 它们不会在同一时刻全部在飞。
 */
import { computed, ref } from 'vue'
import { isAdmin } from '../api'
import Tabs from '../components/Tabs.vue'
import UsersGroups from './UsersGroups.vue'
import Keys from './Keys.vue'

type TabKey = 'users' | 'groups' | 'keys'

/**
 * 页签。**管理员**三个，普通用户只有密钥那一个。
 *
 * 用 computed 而不是写死的常量：isAdmin() 读的是会话快照，而本组件在会话
 * 建立后才渲染（见 App.vue 的 showShell），所以读它很安全。万一将来改成
 * 登出后重判身份，computed 会跟着更新，写死的数组不会。
 */
const TABS = computed<readonly { key: TabKey; label: string }[]>(() =>
  isAdmin()
    ? [
        { key: 'users', label: '用户' },
        { key: 'groups', label: '分组' },
        { key: 'keys', label: '访问密钥' },
      ]
    : [{ key: 'keys', label: '访问密钥' }],
)

/**
 * tab 存在宿主页，不在子面板里。
 *
 * UsersGroups 面板把它的「用户 / 分组」两级页签**提升**到这一层显示：再嵌
 * 一层页签会出现「页签套页签」，切到「用户」还要再切一次才能到分组。所以那
 * 两个页签共用同一个面板，宿主用 key 区分、面板用收到的 tab 决定渲染哪半。
 */
const tab = ref<TabKey>(isAdmin() ? 'users' : 'keys')

/** 每个页签自己的副标题：宿主统一给，避免三处各写一遍标题区。 */
const SUB = computed(() => {
  switch (tab.value) {
    case 'users':
      return '账号、配额、登录状态与模型可见范围'
    case 'groups':
      return '模型白名单按组收窄 —— 空白名单表示不限制'
    default:
      return isAdmin()
        ? '客户端调用 /v1 时使用的 sk-gw- 密钥'
        : '客户端调用 /v1 时使用的 sk-gw- 密钥 · 归属恒为你自己'
  }
})

/** 刷新用的重建计数。递增即让当前面板整体重挂载。 */
const bump = ref(0)

/** panelKey 同时包含页签与重建计数：切页签会换组件，刷新会换同组件的新实例。 */
const panelKey = computed(() => `${tab.value}:${bump.value}`)

/** 当前页签该渲染哪个面板。密钥页签是 Keys，其余（用户/分组）是 UsersGroups。 */
const currentPanel = computed(() => (tab.value === 'keys' ? Keys : UsersGroups))
</script>

<template>
  <main class="page">
    <div class="page-head">
      <div>
        <h1>用户与分组</h1>
        <div class="sub">{{ SUB }}</div>
      </div>
      <div class="head-actions">
        <button class="btn" @click="bump++">刷新</button>
      </div>
    </div>

    <Tabs v-model="tab" :tabs="TABS" aria-label="用户 / 分组 / 访问密钥" />

    <!--
      v-bind 只在非密钥页签时传 tab：Keys 没有这个 prop，硬传会在
      Vue 的 fallthrough attrs 里冒出一个 tab 属性落到它的根元素上。
    -->
    <component
      :is="currentPanel"
      :key="panelKey"
      v-bind="tab === 'keys' ? {} : { tab }"
    />
  </main>
</template>

<style scoped>
.page-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 14px;
  margin-bottom: 14px;
}
.page-head h1 {
  margin: 0 0 4px;
  font-size: 17px;
}
.sub {
  color: var(--text-3);
  font-size: 12.5px;
}
</style>