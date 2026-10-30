# 前端重构方案 v2：Vue 3 → React 19 + Tailwind v4 + HeroUI v3

> 状态：草案 v2（按「彻底重建」重写） · 范围：`web/` 全量前端 · 后端 Go 与 API 契约不动
>
> v1 的核心假设是「保住现有观感」，已被否决。v2 的目标是**用 HeroUI 的体系重建这套后台**：
> 视觉以 HeroUI 默认体系为准，原有像素级观感不作为约束；每一块界面都换成语义上最合适的 HeroUI 组件；
> 手写实现（手写 CSS、手写交互原语、手写状态机）尽可能删除，交给框架。

---

## 0. 相对 v1 的三处根本改动

| | v1（作废） | v2（本方案） |
| --- | --- | --- |
| 样式 | 保留 `styles.css` 作 legacy 层，逐页收编 | **终态 `styles.css` 完全删除**；全站 Tailwind 工具类 + `@heroui/styles` 令牌；过渡期只是执行细节，有硬期限与单调递减门禁 |
| 视觉 | 与迁移前像素级一致 | **以 HeroUI 体系为准**；验收看「是否统一使用体系令牌/组件」，不看「像不像以前」 |
| 组件 | 少数换 HeroUI，多数保留原生 + legacy 类 | **所有交互原语一律 HeroUI**；只保留 1 个业务可视化组件（心跳图） |

于是 v1 里为「保观感」而生的 `.rg-*` 改名、静态对照页、逐页截图比对，全部作废。

---

## 1. 什么叫「迁移完成」（可判定的定义）

1. `web/src` 下**不存在手写 CSS 文件**（除 `theme.css` 里的 `@theme` 令牌覆盖，目标 ≤ 40 行）。
2. `web/src/**/*.tsx` 里**不存在裸交互原语**：`<button>`、`<input>`、`<select>`、`<textarea>`、
   `<table>`、`<details>`、`<dialog>` 一律为零（例外见 §6 allowlist）。
3. 颜色/间距/圆角**只允许来自体系**：`bg-surface`、`text-muted`、`border-separator`、`text-danger`…
   不允许 `oklch(...)`、`#hex`、`bg-[#...]`、`style={{ color: ... }}` 这类魔法值（主题令牌文件除外）。
4. 手写交互逻辑清零：`AppModal.vue` 的焦点陷阱/滚动锁/弹窗栈、`Tabs.vue` 的 aria id 配对、
   `DataTable.vue` 的加载/空/错三态、`confirmBox` 的 Promise 状态机 —— 全部改由 HeroUI / RAC 承担。
5. 上述 1–4 由**自动化门禁**保证（§6），不靠人自觉。

---

## 2. 技术栈（锁版本）

| 包 | 版本 | 说明 |
| --- | --- | --- |
| react / react-dom | 19.3.1 | HeroUI v3 要求 ≥19 |
| @types/react / @types/react-dom | 19.3.0 | |
| @heroui/react | 3.2.7 | 精确锁定（= 本地包版本） |
| @heroui/styles | 3.2.7 | 唯一样式来源 |
| tailwindcss / @tailwindcss/vite | 4.3.3 | HeroUI v3 强制 v4 |
| tailwind-variants | 3.3.1 | 由 @heroui/react 传递引入，**不要**装 4.x |
| react-aria / react-aria-components | 3.53.1 / 1.22.1 | @heroui/react 的 peer，必须显式装 |
| @react-aria/ssr · @react-aria/utils · @internationalized/date | 3.10.1 · 3.34.1 · 3.12.4 | 同上 |
| react-router-dom | 7.18.5 | 见 D2（**不用 8.x**：要 node ≥22.22 / react ≥19.2.7，CI 是 22.12） |
| lucide-react | 最新 | 导航/动作图标（HeroUI 只自带 15 个内部图标，见 D8） |
| @vitejs/plugin-react | 6.1.2 | peer `vite ^8` |
| vite | 8.3.x（现有） | 不动 |
| prettier-plugin-tailwindcss | 最新 | 工具类排序（HeroUI 官方仓自己就用它） |
| eslint + eslint-plugin-react-hooks + eslint-plugin-jsx-a11y | 最新 | D9：hooks 依赖与 a11y 的最低保障 |

安装：

```bash
cd web
npm rm vue vue-router @vitejs/plugin-vue vue-tsc
npm i react@19.3.1 react-dom@19.3.1 react-router-dom@7.18.5 lucide-react \
      @heroui/react@3.2.7 @heroui/styles@3.2.7 \
      react-aria@3.53.1 react-aria-components@1.22.1 \
      @react-aria/ssr@3.10.1 @react-aria/utils@3.34.1 @internationalized/date@3.12.4
npm i -D @types/react@19.3.0 @types/react-dom@19.3.0 \
      tailwindcss@4.3.3 @tailwindcss/vite@4.3.3 @vitejs/plugin-react@6.1.2 \
      prettier prettier-plugin-tailwindcss eslint eslint-plugin-react-hooks eslint-plugin-jsx-a11y
```

**HeroUI 从 npm 装，不从本地目录装**：`C:\Users\louis\Desktop\project\heroui-3.2.7` 是源码 monorepo，`exports` 指向 `./src/index.ts`、
依赖是 `workspace:*`，必须 `pnpm install && pnpm build` 才有产物。本地那份的价值是：
离线组件文档 `apps/docs/content/docs/en/react/components/**`、组件实现 `packages/react/src`、
BEM 样式 `packages/styles/**`。无外网时兜底：`pnpm build && pnpm pack` 后装 tgz。

---

## 3. 架构决策

### D1 样式体系：只有令牌 + 工具类，没有「应用的 CSS 文件」

`src/styles.css`（2 120 行）**删除**，取而代之只有一个约 40 行的 `src/theme.css`：

```css
@import "tailwindcss";
@import "@heroui/styles";

/* HeroUI 认 [data-theme=dark] 与 .dark；tailwind 的 dark: 默认只看系统偏好，这里对齐到属性 */
@custom-variant dark (&:where([data-theme="dark"], [data-theme="dark"] *));

@theme {
  /* HeroUI 不自带字体，中文字体栈由应用定义（只此一处） */
  --font-sans: "PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", ui-sans-serif, system-ui, sans-serif;
  --font-mono: ui-monospace, "SF Mono", Menlo, Consolas, monospace;
  /* 全站容器宽度这类常量做成令牌，避免各页写 max-w-[1180px] 之类魔法值 */
  --container-app: 1180px;
  --spacing-gutter: 1.375rem;
}
```

规则（写进 AGENTS.md / CONTRIBUTING）：
- 间距/尺寸/颜色一律工具类；**不允许**新建 `.xxx` 类名做样式；
- 唯一例外：需要跨组件约定的复合外观，做成 React 组件（D3），不是 CSS 类；
- 主题令牌如需改，只在 `theme.css` 的 `:root` 覆盖，不在组件里分散覆盖。

**Plan B 已定案（2026-10-30）：长分支不合并，不存在「新旧共存」期。**

- 分支 `feat/react-rewrite`，基线 tag `pre-react-rewrite`（@ `bbe9b08`）；`main` 保持 Vue 版可发版、可热修。
- 分支上 `styles.css` **直接删除**：没有 `@layer legacy` 垫片，也没有「未迁页面先跑起来」的阶段。代价是分支在页面迁完前不可部署，这是明确接受的。
- 分支如何保持「随时能打开看」：走 §12 的双入口脚手架 —— 分支期间 `index.html` 仍指向 Vue 入口，
  React 侧用 `index.react.html` + `src/main.react.tsx` 独立跑；全部页面迁完后一次性 flip，删 Vue 入口与 `@vitejs/plugin-vue`。
  这样既有「每步都能在浏览器里验证」，又保证**合并结果里零共存**。
- 因此本方案没有「过渡期倒计时」这种机制：`legacy-audit` 在分支上是进度看板，在合并点是硬门禁（§6）。

### D2 路由：react-router 7 数据路由，loader 取代手写守卫

现在 `router.ts` 有两段手写逻辑：`guardDecision`（判登录/角色）与 `reapplyGuard`
（探测完成后重判当前路由，因为「硬刷新不触发 beforeEach」）。**换成 loader 后两者都能删**：
loader 在**首次渲染前**执行，天然没有「刷新没跑到守卫」的窗口期。

```tsx
const requireSession = async ({ request }: LoaderFunctionArgs) => {
  const s = await loadSessionOnce()            // 复用 api.ts 的探测，进程内缓存 Promise
  const url = new URL(request.url)
  if (!s.me) {
    throw redirectTo('/login?redirect=' + encodeURIComponent(url.pathname + url.search))
  }
  if (ADMIN_PREFIXES.some(p => url.pathname.startsWith(p)) && !isAdmin()) {
    throw redirectTo('/')
  }
  return null
}

export const router = createHashRouter([
  { path: '/login', element: <Login />, handle: { title: '登录' } },
  { path: '/set-password', element: <PasswordSetup /> },
  {
    path: '/', element: <Shell />, loader: requireSession, HydrateFallback: ShellSkeleton,
    children: [
      { index: true, element: <Overview />, loader: overviewLoader, handle: { title: '网关总览' } },
      { path: 'keys', element: <AccessControl />, handle: { title: 'API 密钥' } },
      // …其余 16 条
    ],
  },
  { path: '*', element: <Navigate to='/' replace /> },
])
```

必须逐条带过去的行为（都是现有注释里记过坑的）：
- 未登录 → `/login?redirect=…`，`redirect` 只接受单斜杠开头且非 `//`（防开放重定向）；
- 已登录访问 `/login` → 按 `redirect` 回跳；
- admin-only 页面对普通用户 → 回 `/`（`/keys` 例外：普通用户可看自己的密钥）；
- `/providers?tab=routes` 等旧地址兼容跳转 → loader 里 `throw redirectTo`；
- 401 掉登录（api.ts 把 `session.me` 置空）→ 订阅 store 后 `router.navigate('/login?redirect=…')`；
- `document.title`：`handle.title` + Shell 里 `useMatches()` 统一设置（替代 `afterEach`）。

### D3 组件分层：HeroUI 原语 / 应用组合件 / 唯一自研件

**第一层：HeroUI 直接使用**（不包装、不套皮）。页面里直接
`import { Button, TextField, Table } from '@heroui/react'`。包装层越厚，「用上最新组件」的收益越低，
也会把 HeroUI 的能力（variants、BEM 定制、data-* 状态）挡在外面。

**第二层：应用组合件**（`src/ui/`，约 8 个文件，每个 ≤ 60 行，只做组合与语义约定，不含样式 hack）：

| 组件 | 作用 | 内部组成 |
| --- | --- | --- |
| `PageHeader` | 页面 h1 + 副标题 + 右侧动作区 | `Typography` + `Toolbar` |
| `SectionCard` | 带标题的内容块 | `Card.Header/Title/Description/Content` |
| `DataTable` | 列表页统一外壳：工具条 + 加载/空/错 + 表格 | `Toolbar` + `Spinner` + `Skeleton` + `EmptyState` + `Alert` + `Table` |
| `ConfirmDialog` | `confirmBox()` 的 Promise 实现（17 个调用点不变） | `AlertDialog` |
| `AppModal` | 受控 Modal（25 个调用点不变） | `Modal` |
| `AppDrawer` | 右侧详情抽屉（日志详情、上游详情） | `Drawer` |
| `FormField` | Label + Input/Select + Description + FieldError 常用组合 | `TextField` / `NumberField` / `Select` |
| `StatusChip` | 状态 → variant 映射（ok/warn/err/dim） | `Chip` |

**第三层：唯一自研**：`HeartbeatBar`（60 个时间桶的心跳条，含键盘聚焦与文字摘要）。
HeroUI 与 RAC 都没有等价物，这是业务可视化，必须自己写。目标：602 行压到 ≤ 200 行
（布局用 grid 工具类，悬停说明用 `Tooltip`，键盘用列表语义 + 现有 `heartbeatBucketTexts` 文案）。
其余 `fmt.ts` / `range.ts` / `modelPrice.ts` / `routeTrace.ts` / `importLinks.ts` /
`heartbeatText.ts` / `password.ts` / `api.ts` / `types.ts` 是业务逻辑而非手写 UI，保留。

### D4 数据加载：读用 loader，写用 action / useActionState

| 场景 | 做法 | 省掉的手写 |
| --- | --- | --- |
| 列表页/详情页首次加载 | 路由 `loader` + `useLoaderData()` | 每页 `loading`/`error`/`onMounted` 三件套（18 处） |
| 筛选变化（URL 为真相） | `useSearchParams` 写入 → 导航重跑 loader | `useQueryText`（67 行）与 5 处重复实现 |
| 手动刷新 | `useRevalidator().revalidate()` | 每页 `bump` 重建计数、`RequestGeneration` 的大部分用途 |
| 弹窗/面板按需加载 | 一个 40 行 `useAsync`（保留 `RequestGeneration` 防乱序） | 各页重复的 try/catch/loading |
| 表单提交 | `useActionState` + HeroUI `Form` + `FieldError` | `saving` 标志、`formError` 文案、手写必填校验 |

注意（易踩）：`api.ts` 顶部注释记着后端约定 —— PATCH 是**字段级部分更新**、空串/0 是合法值、
必填字段传空串会 400。HeroUI `NumberField` 的值是 `number | null`、`TextField` 是 `string`，
**「空值 → 不发该字段」的映射必须在 `api.ts` 边界统一处理**，不要在页面里各写一遍。
这是本次重构最容易造成「静默丢字段 / 误清空」的地方，需专门单测覆盖（§9 R4）。

### D5 反馈三件套：全换 HeroUI，删掉 `ui.ts`

| 现状 | 手写量 | 换成 | 调用点改法 |
| --- | --- | --- | --- |
| `toast()`（145 处） | 单例 + 5.2s 定时器 | `toast.success/danger/info/warning`；根节点放 `<Toast.Provider />` | `toast(t,'ok')` → `toast.success(t)`；`'err'` → `toast.danger`（机械替换） |
| `confirmBox()`（17 处 await） | 30 行 Promise 状态机 | `<AlertDialog>` + 模块级 resolver（`ConfirmDialog`） | 对外仍是 `Promise<boolean>`，调用点不动 |
| `AppModal`（25 处） | 174 行：焦点陷阱、背景 inert、body 滚锁、弹窗栈、层级、Esc | `<Modal isOpen onOpenChange>`（RAC 负责焦点/滚锁/栈/动画） | 对外 props 不变，调用点不动 |
| `DetailDrawer` | 19 行 | `<Drawer>` | 日志详情、上游详情 |

### D6 表单：HeroUI Form + RAC 校验，取代手写校验

```tsx
<Form onSubmit={submit} validationBehavior="native">
  <Fieldset>
    <TextField name="name" isRequired>
      <Label>名称</Label>
      <Input placeholder="例如 主线路" />
      <Description>展示给用户的名字</Description>
      <FieldError />
    </TextField>
    <NumberField name="timeout" minValue={1}>
      <Label>超时（秒）</Label>
      <Input />
    </NumberField>
    <Select name="protocol">
      <Label>协议</Label>
      <Select.Trigger><Select.Value /><Select.Indicator /></Select.Trigger>
      <Select.Popover><ListBox items={protocols}>{(x) => <ListBox.Item id={x.id}><Label>{x.label}</Label></ListBox.Item>}</ListBox></Select.Popover>
    </Select>
  </Fieldset>
  <Button type="submit" variant="primary">保存</Button>
</Form>
```

- 数字字段一律 `NumberField`（现状 22 处 `type=number`）；密码 5 处用 `TextField` + `<Input type='password'>`；
  搜索 2 处用 `SearchField`；`Settings` 那 1 处 `type=file` 用 RAC `FileTrigger`；
  复选框 7 处 → `Checkbox`，开关语义 → `Switch`；
- 字段级错误 → `FieldError`（替代 `.form-error` / `.notice-warn` 的 13 处手写块）；
- 页面级错误 → `<Alert variant='danger'>`；
- 17 个 `<form>` 全部走 `Form`，删掉手写 `e.preventDefault()` 与校验分支。

### D7 状态：一个 30 行 store，删掉 `reactive`

`api.ts` 只用到 `reactive({token})` 与 `reactive(session)`：

```ts
export function createStore<T extends object>(initial: T) {
  let state = initial
  const subs = new Set<() => void>()
  return {
    get: () => state,
    set: (patch: Partial<T>) => { state = { ...state, ...patch }; subs.forEach(f => f()) },
    subscribe: (f: () => void) => { subs.add(f); return () => subs.delete(f) },
  }
}
```

其余一律局地 `useState`。**不引入** Redux / Zustand / Jotai / TanStack Query：loaders + useAsync 已覆盖，
多一个库就多一份「框架之外的手写接线」。

### D8 图标：lucide-react 取代手写 SVG path

`App.vue` 里 10 个导航图标是手写 `d='M3 3h7v7H3z…'`，各处还有零散 inline SVG。
HeroUI 只自带 15 个内部图标（chevron、search、plus/minus、status、calendar），不够用。
统一用 `lucide-react`（HeroUI 官方文档站自己也用它）：`LayoutDashboard` `ScrollText` `ChartColumn`
`Route` `Servers` `Globe` `Users` `Wallet` `Settings` `KeyRound` `Sun` `Moon` `LogOut`。

### D9 lint 与测试

- `eslint-plugin-react-hooks`：30 个手写组件的依赖数组，`tsc` 一个都拦不住；
- `eslint-plugin-jsx-a11y`：防止在 HeroUI 组件上写反 a11y 属性；
- 保留纯 TS 单测（apiContract / dtoContract / routeTrace / asyncOwnership / importLinks）—— 与框架无关，零改动；
- `accessibilityContract.test.mjs` 改写为读 `.tsx`，并**升级**为「禁止裸原语」守门测试（§6）：
  它守的东西从「我们手写对了」变成「我们没再手写」；
- 新增 `apiPatchSemantics.test.mjs`：覆盖 D4 的「空值 → 不发字段」边界（§9 R4）；
- `tests/tsResolve.mjs` 的 `vue` → stub 映射删除。

---

## 4. 组件映射总表（按 UI 语义，不按页面）

数量来自对 28 个 SFC 的全量扫描。

| 现有手写件 | 数量 | HeroUI 目标 | 说明 |
| --- | --- | --- | --- |
| `.btn` / `.btn-primary` / `.btn-ghost` / `.btn-danger` / `.btn-sm` / `.btn-act` / `.btn-icon` | 185 | `<Button>` `variant=primary/secondary/tertiary/ghost/outline/danger` + `size` / `isIconOnly` | 用语义变体，不用颜色词 |
| 纯图标按钮（主题切换、侧栏收起、行内操作） | ~20 | `<Button isIconOnly variant='ghost'>` + `<Tooltip>` | 图标来自 lucide |
| `.field` + `.flabel` + `.fhint` + `<input class='input'>` | 输入框 86 | `<TextField><Label/><Input/><Description/><FieldError/></TextField>` | 表单语义一次到位 |
| `<input type='number'>` | 约 22 | `<NumberField>` | 最小值/步进交给 RAC |
| `<input type='password'>` | 5 | `<TextField>` + `<Input type='password'>` | HeroUI 无独立密码组件 |
| `<input type='search'>` | 2 | `<SearchField>` | 含清除按钮 |
| `<input type='file'>` | 1（Settings 导入） | RAC `<FileTrigger>` | 非 HeroUI 组件，但仍属框架能力 |
| `<input type='checkbox'>` | 7 | `<Checkbox>`；开关语义用 `<Switch>` | |
| `<select>` + `<option>` | 27 / 60 | `<Select>` + `<ListBox>` | 选项来自数据，用 items + render prop |
| `<textarea>` | 1 | `<TextArea>` | |
| `<form>` | 17 | `<Form>` + `<Fieldset>` | 校验与提交语义 |
| 字段错误 `.form-error` / 页面提示 `.notice-warn` | 13 | `<FieldError>` / `<Alert variant='danger'>` | |
| 徽章 `.badge` / 状态点 | 37 | `<Chip>`（success/warning/danger/accent/default）+ `<Badge>`（计数角标） | |
| `.card` / `.panel` / `.current-card` / `.model-card` / `.rt-card` | 约 30 块 | `<Card>` + `Card.Header/Title/Description/Content/Footer`；分组底用 `<Surface>` | |
| `<table>` + `th/td` | 16 表 / 91 列 | `<Table>` + `Table.ScrollContainer/Content/Header/Column/Body/Row/Cell` | 列宽与选择交给 RAC |
| 表格工具条（`.filters` / `.log-toolbar`） | ~10 | `<Toolbar>` | |
| 加载态 `.loading` / `.table-refresh` | 16 | `<Spinner>` / `<Skeleton>` | |
| 空态 `.empty` | 28 | `<EmptyState>` | |
| `.tip` / `title=` 提示 | 47 | `<Tooltip>` | |
| `.row-menu`（`<details>` 下拉） | 若干 | `<Dropdown>` + `Dropdown.Trigger/Popover/Menu/Item` | |
| Tabs（自研 142 行） | 5 | `<Tabs>` + `Tabs.List/Tab/Panel` | aria 配对由 RAC 生成 |
| 时间范围/粒度分段筛选（`.range-control`） | ~6 | `<ToggleButtonGroup selectionMode='single'>` 或 `<Tabs variant='secondary'>` | |
| Modal（自研 174 行） | 25 | `<Modal>` | 见 D5 |
| 右侧详情抽屉 | 1 | `<Drawer>` | |
| 确认框（自研 30 行） | 17 | `<AlertDialog>` | 包成 ConfirmDialog 保持 Promise |
| Toast（自研 20 行） | 145 调用点 | `toast.success/danger` + `<Toast.Provider />` | |
| 顶栏用户标识 | 1 | `<Avatar>` + `<Dropdown>`（菜单放登出） | |
| 标题层级（h1/h2/h3） | 14 h1 + 若干 | `<Typography type='h1/h2/h3/body/body-sm'>` | 统一字号阶梯 |
| 分隔线 / 分组小标题 | 多处 | `<Separator>` / `<Header>` | |
| 额度/用量条（余额、配额、分组用量） | ~8 | `<Meter>` + `Label/Output/Track/Fill` | 比手写进度条更合适 |
| 折叠面板（Settings 的 `<details>`） | 2 | `<Disclosure>` / `<Accordion>` | |
| 长表格滚动阴影 | — | `<ScrollShadow>` | 替代手写遮罩 |
| 侧栏导航项（RouterLink + 手写 active 判定） | 23 | react-router `<NavLink>`（`isActive`）+ Tailwind + lucide | 可删掉 activeNavTo() 之类手写归一化 |
| 手写 SVG path | ~20 | `lucide-react` | |
| 心跳条（业务可视化） | 1 | **保留自研**，用工具类重写（≤200 行） | 见 D3 |

---

## 5. 页面级映射（18 条路由 + 嵌入组件）

| 路由 | 现状结构 | 目标组合 |
| --- | --- | --- |
| `/` 总览 | 当前状态卡片网格 + 范围 select + 运行质量 | PageHeader + ToggleButtonGroup(范围) + Card 网格（Chip 表状态）+ SectionCard(运行质量) + Alert(告警) |
| `/history` · `/usage` | Tabs + 3× UsageByGroup | Tabs + DataTable(Table) + Meter(分组用量) |
| 日志详情 UsageDetail | 表格 + 详情抽屉 + details 原始响应 | DataTable + AppDrawer + Disclosure(原始响应) + Chip(轨迹每步状态) |
| `/models` | 筛选 + 模型卡片网格 + HeartbeatBar | SearchField + Select + Switch + Card 网格 + Chip(价格/健康) + 自研心跳条 |
| `/routes` | 表格 + 路由链 + 新建/编辑 Modal | DataTable + AppModal(Form + Select + NumberField + Switch) |
| `/providers` | 表格 + 启用开关 + 行操作 | DataTable + Switch + Dropdown(行操作) + Chip(网络/类型) |
| `/providers/:id` | 4 页签：基础/凭据/上游模型/价格 | Tabs + Form + DataTable + AppModal；页签内容拆 4 个文件（现状 1 233 行） |
| `/networks` | 表格 + 新建 Modal | DataTable + AppModal |
| `/keys` | 筛选 + 密钥表 + 新建 Modal + 模型选择器 | Toolbar(SearchField + Select) + DataTable + AppModal + AppDrawer(把 ModelPicker 改为 CheckboxGroup) |
| `/users` | Tabs + 用户表 + 分组表 + 编辑 Modal | Tabs + 2× DataTable + AppModal + CheckboxGroup |
| `/wallet` | Tabs + 余额卡 + 3 张表 + 充值 Modal | Tabs + Card+Meter(余额/配额) + 3× DataTable + AppModal |
| `/settings` | Tabs + 大量表单字段 + 维护操作（1 869 行） | Tabs + Accordion(超时/看门狗/故障转移/归档) + Form + NumberField + Alert；拆 4 个子组件文件 |
| `/login` / `/set-password` | 卡片式表单 + 品牌区 | Card + Form + TextField + Checkbox + Button + Alert(错误) |
| `/account` | 改密表单 | Form + TextField + FieldError |
| AccessControl | 页签宿主 + 角色判定 | Tabs + PageHeader（标题/副标题按页签，沿用现有语义） |
| ModelGroups（嵌入） | 表格 + 分组弹窗 | DataTable + AppModal + CheckboxGroup |
| Upstream（嵌入） | 页签 | Tabs |

结构纪律（写进 PR 模板）：
- 单个 `.tsx` **不超过 400 行**，超过就拆子组件或拆页签（现在 6 个页面在 700–1 900 行，必须拆）；
- 表单与卡片排版用 grid/flex 工具类，不写 CSS；
- 页面只做"取数 + 组合"，业务换算留在 `fmt.ts` / `modelPrice.ts` 等纯函数。

---

## 6. 反手写门禁（自动化，接进 CI）

新增 `web/scripts/legacy-audit.mjs`，两档模式：

```bash
node scripts/legacy-audit.mjs        # 报告：旧类名／裸原语／手写 CSS 存量（分支上的进度看板）
node scripts/legacy-audit.mjs --gate # 门禁：以上必须全为 0，否则 exit 1
```

统计口径：
- `styles.css` 是否存在、多少行；
- `src/**/*.tsx` 中旧类名出现次数（`btn`, `panel`, `tbl`, `field`, `flabel`, `notice-warn`, `badge`, `card`, `input`, `select` —— 按 token 边界匹配，别误伤 `Button` 之类的组件名）；
- `src/**/*.tsx` 中裸原语数量（`<button`, `<input`, `<select`, `<textarea`, `<table`, `<details`, `<dialog`）；
- `src` 下 `.css` 文件数与总行数（只允许 `theme.css`）。

allowlist（写在脚本里，逐条带理由）：目前只有 Settings 的文件导入一处 `<input type='file'>`
（RAC `FileTrigger` 内部实现仍可能裸露），以及 `HeartbeatBar` 用到的 `role=list` 语义标记（不是原语）。

另有 `tests/noHandWrittenPrimitives.test.mjs` 在 `npm test` 里守同样几条，这样即使 CI 的 job 顺序变了也不会漏。
Plan B 下它的角色是：分支上随便跑、只看趋势，**合并前必须 `--gate` 全绿**。

---

## 7. 阶段计划

估时按「1 名熟悉本项目的工程师 + AI 辅助」，含自测与回归，不含评审等待。

### P0 · 基线与安全网（0.5 天，已完成 2026-10-30）

- 已建分支 `feat/react-rewrite`、tag `pre-react-rewrite`（@ `bbe9b08`）
- 基线全绿：`npm run typecheck`、`npm test`（47 passed）、`go test ./...`；构建产物留档
- `web/scripts/legacy-audit.mjs` 已落地（报告 + `--gate` 两档），基线数字：

| 指标 | 基线值 |
| --- | --- |
| `.vue` 文件 | 28 |
| `styles.css` 行数 | 2 120 |
| 旧类名引用 | 1 943 处（484 个不同类名） |
| 裸交互原语 | 318 |
| 其它手写 CSS | 0 |

这五个数字就是本次重构要清零的对象；最重的三个文件是 Settings（275 处）、ProviderDetail（188）、UsersGroups（182）。

### P1 · 工具链与双入口骨架（1.5 天）

- 依赖替换（§2）；lockfile 重生成（**根 version 仍是 1.7.2**，lockfile 守护测试会查）
- `vite.config.ts`：加 `react()` + `tailwindcss()`，**分支期间保留 `vue()`**（双入口脚手架，§12.2；两个插件只在分支共存，P5 删）
- `tsconfig.json`：`jsx: react-jsx`；`index.html` **不动**（仍指 Vue 入口），新增 `index.react.html`（`#root` + 同一份 theme-init.js）
- `src/theme.css`（D1）、`src/main.react.tsx`（createRoot + RouterProvider + Toast.Provider，App 静态导入）、`src/store.ts`、App.tsx 骨架
- `api.ts` 去 `reactive`（其余 900 行不动）。**注意联动**：这一步会让 Vue 侧的 `session` 失去响应式，
  必须同批把 Vue 页面的读取点（`isAdmin()`、`session.me`，集中在 api.ts 导出与 App.vue）接到新 store；
  若想完全不动 Vue 侧，替代做法是 P1 另建 `api.react.ts` 给 React 用、P5 合并 —— 二选一，P1 开工时定，不要并行两套。
- 同批删掉 `tests/tsResolve.mjs` 里的 `vue` → stub 映射（api.ts 不再依赖框架）
- 骨架验证：访问 `/index.react.html`，Tailwind 类生效、HeroUI Button 与 Modal 可用、hash 路由可跳、暗色 `data-theme` 生效

**门槛**：`npm run typecheck`、`npm test`、`npm run build`（Vue 入口仍可构建）、`go test ./internal/webui/...` 全绿；
`legacy-audit` 数字不变（P1 只搭架子，不消灭存量）。产物确定性与 CSP／入口 chunk 版本号的正式校验放在 P5 flip 之后。

### P2 · 设计基座与应用组合件（2.5 天）

- `src/ui/`：PageHeader、SectionCard、DataTable、ConfirmDialog、AppModal、AppDrawer、FormField、StatusChip
- `src/ui/Shell.tsx`：侧栏（NavLink + lucide + Tailwind）、顶栏（Header/Toolbar + Avatar + Dropdown 登出 + 主题切换 ToggleButton）
- ConfirmDialog 接上 Promise 版 `confirmBox`；**`ui.ts` 删除**
- 手写删除量：`ui.ts` 67 行、`AppModal.vue` 174、`DetailDrawer.vue` 19、`Tabs.vue` 142、`DataTable.vue` 41、App.vue 里的图标 path 与媒体查询约 80

**门槛**：登录 / 设密码 / 会话探测 / 守卫跳转 / 登出 全链路可用；ConfirmDialog 在删除、登出、危险操作三处手工验证。

### P3 · 全局原语清扫（2 天，机械但面广）

28 个页面批量替换，每类一个 commit，每个 commit 都要 typecheck 通过：
1. `<button>` → `<Button>`（185）
2. `<input>` / `<select>` / `<textarea>` → TextField / NumberField / Select / TextArea / SearchField（86 / 27 / 1）
3. `<form>` → `<Form>`（17）+ Checkbox / Switch（7）
4. `toast(t, 'ok'|'err')` → `toast.success/danger`（145 调用点）
5. 状态徽章 `.badge` → `<Chip>`（37）

此阶段**只换组件、不动布局**。每类完成后立即删 `styles.css` 里对应的规则组（`.btn*`、`.field/.input/.select`、`.badge`），
并跑 legacy-audit 确认数字下降。

**门槛**：legacy-audit 里按钮/输入/下拉/徽章四类归零；页面能点、能提交。视觉短时不协调是预期的，不做对齐。

### P4 · 页面重建（主工作量）

| 批次 | 页面 | 现状行数 | 目标 | 估时 |
| --- | --- | --- | --- | --- |
| B1 读为主 | Overview / UsageStats / UsageDetail / ModelGroups / Models / Networks | 2 629 | loader + DataTable + Card 网格 | 4 天 |
| B2 配置 CRUD | Providers / Routes / ProviderDetail / Settings | 4 067 | Form + Table + Modal；两个超大页拆子组件（Settings 按 Accordion 拆 4 段，ProviderDetail 按页签拆 4 文件） | 7 天 |
| B3 账户与权限 | Keys / UsersGroups / Wallet / AccessControl / AccountSettings / Login / PasswordSetup | 3 531 | Tabs + 多表 + 密钥/充值表单 | 5 天 |
| C 专用组件 | HeartbeatBar / UsageByGroup / ModelPicker / CredentialPanel / UpstreamModelPanel / Upstream | 1 691 | 心跳条重写到 ≤200 行；其余改组合件 + CheckboxGroup/Table | 3 天 |

每页固定动作：
1. 删该页 `<style scoped>` 整块（共 1 927 行，页均约 70 行）；
2. 布局改 Tailwind 工具类（grid / flex / space），不再新增类名；
3. 读数据改 loader、写数据改 `useActionState`；
4. 交互件按 §4 映射；
5. 连带删除 `styles.css` 中已无人引用的规则。

**门槛**：每批结束 `npm run build && node sync-embed.mjs` + `go test ./...`；
手工走查：能筛、能翻、能开弹窗提交、错误态能显示、纯键盘能走完一遍。

### P5 · 收尾与入口切换（2 天）

- **flip 入口**：`index.react.html` → `index.html`（`#root` + theme-init.js 原样保留）；删 `index.react.html`、Vue 入口 `main.ts`、双插件里的 `vue()`
- 删 `src/styles.css`、删 `src/**/*.vue`、移除 `vue` / `vue-router` / `vue-tsc` / `@vitejs/plugin-vue`
- `legacy-audit --gate` 接进 CI 硬门禁；CI 增加「无 .vue／无手写 CSS」断言
- 产物确定性：连续两次 `npm run build` 字节一致；`go test ./internal/webui/...`（CSP／入口 chunk 版本号／lockfile 三守）
- 同步 `build.ps1`（第 71 行 vue-tsc）、`gateway.ps1`、`.github/workflows/ci.yml` 注释、`AGENTS.md`、`DESIGN.md`（补两条新规则：禁止裸交互原语、禁止新建 CSS 文件）
- 重建 XERJ 索引：`%LOCALAPPDATA%\Programs\xerj\xerj-index.ps1 .`
- 提交入库产物 `internal/webui/dist`（此时 `webui-sync` 才允许转绿）

### P6 · 发布（0.5 天）

- 版本 1.7.2 → 2.0.0（package.json 与 lockfile 根两处同步）
- 二进制冒烟：起 `gateway.exe` → `/admin/` → 登录 → 五个核心页 → 暗色切换
- 保留 `pre-react-rewrite` tag 一个版本周期

---

## 8. 工作量

| 阶段 | 估时 |
| --- | --- |
| P0 基线 | 0.5 d |
| P1 工具链与双入口骨架 | 1.5 d |
| P2 设计基座 + 组合件 + Shell | 2.5 d |
| P3 全局原语清扫 | 2 d |
| P4 页面重建（B1 4 + B2 7 + B3 5 + C 3） | 19 d |
| P5 收尾与入口切换 | 2 d |
| P6 发布 | 0.5 d |
| 漂移控制（每周 rebase main、热修 cherry-pick、合并彩排） | 1.5 d |
| **合计** | **约 29.5 人日**（建议按 34 人日排期） |

并行：B1 / B2 / B3 / C 之间几乎无耦合，但**必须等 P2 的 `src/ui/` 就位**再并行，否则各写一套组合件，第 3 周开始互相打架。
2–3 人并行下关键路径约为 P1 → P2 → P4-B2（7 天），整体约 **4 周**。

Plan B 比 Plan A 多出约 1.5 天（周 rebase / cherry-pick / 合并彩排），并且**这 4 周内 main 出不了新前端版本** —— 这是选 Plan B 时已接受的代价；
换来的是合并结果里没有任何「新旧共存」痕迹，且`main` 在分支冻结期间保持可发版。

相对 v1 的 21.5 天，多出的 8 天是「全量 Tailwind 化 + 拆超大页面 + 反手写门禁 + 长分支漂移控制」的成本；
换来的是 `styles.css` 2 120 行、scoped CSS 1 927 行、手写交互约 500 行彻底消失。

---

## 9. 风险登记册

| # | 风险 | 影响 | 对策 |
| --- | --- | --- | --- |
| R1 | 分支上长期存在「半 Vue 半 React」（双入口） | 中 | 双入口脚手架只在分支存在（§12.2），合并前必须 flip 并删除；`legacy-audit --gate` 作为合并门禁 |
| R2 | HeroUI `Table` 的排序/列配置语义与现有"服务端已排好序"的表格不同 | 中 | 只把"展示 + 行操作"迁到 Table；排序与分页仍走服务端（searchParams + loader 重取）；不为用而用 RAC 的客户端排序 |
| R3 | loader 与 401／重定向交互出错，出现跳登录死循环 | 高 | loader 只做"判定 + throw redirect"；401 只由 api.ts 处理并跳一次；redirect 目标沿用现有"单斜杠开头且非 //"校验 |
| R4 | 表单值类型变化导致 PATCH 语义漂移（空值被当 0 发、空串被静默丢弃） | 高（数据损坏） | `api.ts` 边界统一规约 `undefined / null / ''`；新增 `apiPatchSemantics.test.mjs`；每个写表单迁移时手工验证一次"清空字段" |
| R5 | Go 侧守护测试（CSP／入口 chunk 版本号／lockfile）被无意破坏 | 高（失败无声） | P1 起每次 build 后跑 `go test ./internal/webui/...`；App 静态导入、不改输出文件名、index.html 不写内联 |
| R6 | 产物跨平台不一致，CI 与本地 diff 不一致 | 中 | P1 验证两次构建字节一致；必要时在 .gitattributes 给 `internal/webui/dist/**` 加 `-text` |
| R7 | 键盘/读屏可用性回退（框架给了能力，但布局可能破坏） | 高 | 每批做一次纯键盘走查；a11y 契约测试改为"禁止裸原语"；Modal/Tabs/Drawer 一律不自研 |
| R8 | 超大页面拆分导致行为回归（Settings 1 869、ProviderDetail 1 233） | 中 | 先纯移动拆分（不改逻辑），再重构；子组件单独验收 |
| R9 | 分支冻结约 4 周，期间 main 发不出新前端版本 | 中 | 已接受（Plan B）。热修走 main 发布再 cherry-pick 到分支；冲突落在 `.vue` 时直接把修复移植到对应 React 页面（§12.4） |
| R10 | 依赖体积上升（RAC + HeroUI + lucide） | 低 | 保留路由级懒加载；lucide 具名按需导入；必要时按子路径导入 |
| R11 | main 漂移导致合并冲突（后端改动落在 `api.ts`／`types.ts`／`fmt.ts`） | 高 | 这些文件只做框架相关最小改动、保持原名与结构，让后端改动能干净合入；每周 rebase 一次；`dtoContract` 作为契约哨兵（§12.3） |

---

## 10. Definition of Done

- [ ] `web/` 下无 `.vue`、无 `styles.css`、无 vue 系依赖
- [ ] `legacy-audit --gate` 通过（旧类名 0、裸原语 0、CSS 仅 `theme.css`）
- [ ] 全站只有 `theme.css` 一个 CSS 文件（≤40 行），页面里零 `<style>`
- [ ] typecheck / test / build+sync / `go test ./...` 全绿；产物与构建一致
- [ ] 18 条路由在亮/暗 × 宽/窄 × 空/错态下符合 HeroUI 体系（组件、令牌、字号阶梯一致）
- [ ] 纯键盘可完成：登录、切页签、开关 Modal 与 Drawer（Esc／遮罩／焦点回位）、提交表单、行内操作
- [ ] 写表单的"清空字段"行为逐个手工验证（R4）
- [ ] index.html 无内联脚本／内联事件；theme-init.js 仍为同步外链
- [ ] 版本号单一来源成立（页脚／二进制／lockfile）
- [ ] AGENTS.md 补两条规则（禁止裸交互原语、禁止新建 CSS 文件）；DESIGN.md、build.ps1、CI 注释已更新
- [ ] `index.react.html` 与双入口脚手架已删除，`index.html` 指向 React 入口
- [ ] 已从最新 `main` 彩排合并一次（`feat/react-rewrite-rehearsal`）并通过全套 CI（§12.5）
- [ ] XERJ 索引已重建

---

## 11. 附录：HeroUI v3 速查（实现时对照本地包）

组件清单（`packages/react/src/components/`，全部可从 `@heroui/react` 具名导入）：
accordion · alert · alert-dialog · autocomplete · avatar · avatar-group · badge · breadcrumbs · button · button-group ·
calendar · card · checkbox · checkbox-group · chip · close-button · combo-box · date-field · date-picker · date-range-picker ·
description · disclosure · disclosure-group · drawer · dropdown · empty-state · error-message · field-error · fieldset · form ·
header · input · input-group · input-otp · kbd · label · link · list-box · menu · meter · modal · number-field · pagination ·
popover · progress-bar · progress-circle · radio-group · scroll-shadow · search-field · select · separator · sheet · skeleton ·
slider · spinner · surface · switch · switch-group · table · tabs · tag · tag-group · textarea · textfield · time-field · toast ·
toggle-button · toggle-button-group · toolbar · tooltip · typography

v3 与 v2 的关键差别（别用旧记忆）：
- 无 `<HeroUIProvider>`；组件是复合式（`<Card><Card.Header>`）；
- 事件用 `onPress` 而非 `onClick`（RAC 语义）；
- 样式走 Tailwind v4 + `@heroui/styles`，变体是语义名（`primary/secondary/tertiary/ghost/outline/danger`）；
- 暗色：`[data-theme=dark]` 或 `.dark`（与现有 `theme-init.js` 天然兼容）；
- 浮层层级 `--z-index-overlay: 100000`，toast 为其 +1（旧的 `--z-modal: 90` 那套随 `styles.css` 一起消失）；
- 图标只自带 15 个（chevron／search／plus／minus／status／calendar），其余自备（D8）；
- `@heroui/styles` 不声明字体，中文字体栈必须在 `theme.css` 的 `@theme` 里给。

本地资料位置：`apps/docs/content/docs/en/react/components/**`（MDX 文档，含 anatomy／props／BEM 类名／交互状态）、
`packages/react/src/components/**`（真实实现，判断受控与否的最快途径）、
`packages/styles/themes/default/variables.css`（全部令牌）、
`skills/heroui-react/SKILL.md`（取文档脚本，需联网）。

---

## 12. Plan B 执行细则（已定案 2026-10-30）

### 12.1 分支与推送纪律

- 工作分支 `feat/react-rewrite`；基线 tag `pre-react-rewrite`（@ `bbe9b08`）。
- 分支**开启 CI**（typecheck／test／build 三步），但 `webui-sync` 这个 job 在分支上不判红：
  它守的是「入库产物与源码一致」，而分支产物在 flip 之前不提交。合并时必须转绿（§12.5）。
- 分支期间**不提交** `internal/webui/dist`；P5 一次性提交新产物。
- 合并形态：**squash 合并**为一条 `feat: 前端迁移到 React 19 + Tailwind v4 + HeroUI v3`；
  完整提交历史另存 tag `react-rewrite-history`，便于日后 bisect。

### 12.2 双入口脚手架（只在分支存在，合并前删除）

目的：让分支**每一步都能在浏览器里打开验证**，同时不产生「新旧页面同入口共存」的最终状态。

| 文件 | 作用 | 生命周期 |
| --- | --- | --- |
| `index.html` | 保持不变，仍加载 Vue 入口 `src/main.ts` | 到 P5 flip 前 |
| `index.react.html` | React 入口：root 容器 + 同一份 `theme-init.js` | P1 建，P5 删 |
| `src/main.react.tsx` | `createRoot` + `RouterProvider` + `Toast.Provider` | P1 建，P5 改名 `main.tsx` |
| `vite.config.ts` | 同时挂 `vue()` 与 `react()` 两个插件 | P1 起，P5 删 `vue()` |

两个入口共享 `src/api.ts` 与 `src/types.ts`，但**不共享页面组件**（Vue 页面留在 `.vue` 里直到被替换）。
由此有一个必须提前决策的点：`api.ts` 从 `reactive` 改成 store 的那一刻，Vue 侧的响应式会失效。
两条路二选一，**P1 开工时定，不要并行两套**：

1. 同批把 Vue 侧的读取点改成新 store（`isAdmin()`、`session.me` 的读取集中在 api.ts 导出与 App.vue，改动面很小）；
2. P1 保留 `api.ts` 原样、另建 `api.react.ts` 只给 React 用，P5 合并回一个。

### 12.3 漂移控制（Plan B 的主要风险）

分支冻结约 4 周，期间 main 会继续合后端改动。三条纪律：

1. **每周 rebase 一次**到 `main`，不攒到最后 —— 攒到最后等于把冲突放到最没耐心的时刻。
2. **业务文件不改名、不改结构**：`api.ts`、`types.ts`、`fmt.ts`、`range.ts`、`modelPrice.ts`、
   `routeTrace.ts`、`importLinks.ts`、`heartbeatText.ts`、`password.ts`、`generated/*` 只做框架相关的最小改动，
   main 上这些文件的改动就能自动合入。页面文件会整份重写，冲突时直接取分支版本（分支是新实现）。
3. **契约哨兵**：每次 rebase 后跑 `npm test`（`dtoContract` 会拿 `types.ts` 与 Go DTO 逐字段比对）。
   Go 侧新增字段而前端漏改，会在这一步暴露，而不是发布后。

### 12.4 热修流程（分支冻结期间）

- main 的紧急前端修复：正常在 main 修 → 发版 → `git cherry-pick` 到分支。
- 冲突处理：冲突落在 `.vue` 时，**优先把修复移植到对应的 React 页面**（该页若已迁完），
  不要为了 cherry-pick 干净而在即将删除的 `.vue` 里重写一遍。

### 12.5 合并彩排与合并门禁

1. P5 之后、合并之前，从最新 `main` 拉 `feat/react-rewrite-rehearsal`，把分支合进去，跑**全套 CI**（含 `webui-sync`）。
2. 合并前必须全绿：
   - `node scripts/legacy-audit.mjs --gate`（旧类名 0、裸原语 0、仅 `theme.css`）
   - `npm run typecheck` / `npm test`
   - `npm run build && node sync-embed.mjs` → `git diff internal/webui/dist` 为空
   - `go test ./...`（CSP／入口 chunk 版本号／lockfile 三个守护）
   - 纯键盘走查 + 二进制冒烟（P6）
3. 合并后在 main 打 tag `post-react-rewrite`，并保留 `pre-react-rewrite` 一个版本周期用于回滚。
