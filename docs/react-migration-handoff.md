# 前端 React 重构 · 移交说明

> 写给接手的新对话。总方案在 `docs/frontend-react-migration-plan.md`（607 行，含全部决策、
> 组件映射表、阶段计划、风险登记册与 Plan B 执行细则 §12），本文只讲「现在在哪、怎么继续」。

## 1. 现状一句话

分支 `feat/react-rewrite` 上已完成 Vue→React 的一次性换血（P0/P1/P2）与 B1 第一张页面
（Networks，作为模式模板），typecheck / 43 tests / build / Go 守护全绿；
剩余工作是把 17 个页面按同一模式重建完（P4-B1 余量 → B2 → B3 → C 组件），然后 P5 收尾、合并。

## 2. 提交与标签

| 提交 | 内容 |
| --- | --- |
| `pre-react-rewrite`（tag @ bbe9b08） | Vue 基线，旧实现全部可从这里 `git show pre-react-rewrite:web/src/<path>` 取阅 |
| `4141251` docs | 重构方案 v2 + legacy-audit 脚本 + 基线数字 |
| `992ac3d` P1 | 一次性换血：删光 .vue/styles.css/vue 依赖，React+Tailwind+HeroUI 骨架，api.ts store 化 |
| `b5163f3` P2 | src/ui 组合件 + Shell + Login/PasswordSetup 真实页 + loader 守卫 |
| `7d12cca` docs | P2 记录、组件 API 实测更正、P3 并入 P4 |
| `c469bb9` B1-WIP | ui/DataTable + pages/Networks（模式模板） |

## 3. 已完成 / 待办

**已完成**（详细清单见方案 §7 各阶段「实际结果」）：
- 依赖换血、theme.css、store.ts、main.tsx、router.tsx（18 条路由 handle 保留）
- src/ui：AppModal、AppDrawer、ConfirmDialog+confirm.ts（Promise 契约）、PageHeader、
  SectionCard、StatusChip、DataTable、theme.ts、nav.ts、Shell.tsx、AppRoot.tsx
  （useDocumentTitle + 401 watcher）、Placeholder.tsx（未重建页的临时落点）
- 守卫：loader 化（ensureSession/authedLoader/loginLoader/providersLoader），旧 reapplyGuard 已删
- 真实页面：Login、PasswordSetup、Networks
- 门禁：tests/noHandWrittenPrimitives.test.mjs（4 条）+ web/scripts/legacy-audit.mjs（报告/--gate）

**待办（按序）**：
1. **B1 余量**（4 天估）：UsageStats+ModelGroups（Tabs+Table+Meter）→ Overview（含心跳条移植，
   唯一自研组件，旧 602 行目标 ≤200）→ Models（SearchField+Card 网格+价格 Chip）→ UsageDetail（DataTable+AppDrawer+Disclosure）
2. **B2**（7 天估）：Providers / Routes / ProviderDetail(拆 4 页签文件) / Settings(拆 Accordion 4 段)
3. **B3**（5 天估）：Keys / UsersGroups / Wallet / AccessControl / AccountSettings
4. **C 组件**（3 天估）：UsageByGroup、ModelPicker(→CheckboxGroup 抽屉)、CredentialPanel、UpstreamModelPanel、Upstream
5. **P5 收尾**：删 Placeholder、CI 接 legacy-audit --gate、build.ps1/CI 注释/AGENTS.md/DESIGN.md 更新、
   XERJ 索引重建（%LOCALAPPDATA%\Programs\xerj\xerj-index.ps1 .）、提交 internal/webui/dist
6. **P6 发布**：版本 1.7.2→2.0.0（package.json+lockfile 根两处）、二进制冒烟、合并彩排（§12.5）

## 4. 每页重建的标准模式（照 Networks.tsx 抄）

1. 读旧实现：`git show pre-react-rewrite:web/src/views/X.vue`（页面语义注释很有价值，**逐条带到新代码**）
2. 读 loader：失败不 throw，返回 { data, error? }；变更后 `useRevalidator().revalidate()`
3. 布局：PageHeader + SectionCard/DataTable + HeroUI 组件（映射表见方案 §4）
4. 弹窗 AppModal / 抽屉 AppDrawer；危险操作 confirmBox({danger:true})
5. 状态徽章 StatusChip；图标 lucide-react（Icon size=18 strokeWidth=1.75 aria-hidden）
6. `api.ts` 的 401 一律静默（is401 判断），toast.success/danger 报结果
7. 每页做完：typecheck + npm test + 该页手工走查；R10 异步所有权契约补回断言
   （参考 git 历史里的 tests/asyncIntegration.test.mjs）

## 5. 硬性纪律（violate = 返工）

- **守护测试不可红**：CSP（index.html 无内联脚本/事件、theme-init.js 同步外链）、
  version_test（入口 chunk 必须含版本号字面量 → App/Shell 静态可达，页脚必须渲染 __APP_VERSION__）、
  lockfile 根 version == package.json version（现 1.7.2）
- **反手写门禁**：tsx 无裸 `<button>/<input>/<select>/<table>…`、无新 CSS 文件（仅 theme.css）、
  无 .vue 回潮；每步跑 `npm test`（内含 noHandWrittenPrimitives）
- **业务文件不动结构**：api.ts/types.ts/fmt.ts/… 只做框架相关最小改动（Plan B 漂移控制，§12.3）；
  每周 rebase main 一次；`dtoContract` 是契约哨兵
- **分支上不提交 internal/webui/dist**；web/dist 不入库（gitignore）
- 单 tsx ≤ 400 行，超出拆子组件（Settings 拆 4 段、ProviderDetail 拆 4 页签）

## 6. 环境与工具坑（本机 Windows，踩过的）

- shell 是 **cmd.exe**：参数里禁用 `<` `>` `|`（git --format、重定向都会炸）；
  多行提交消息用 `git commit -F <msgfile>`
- `go test` 需注入 env：USERPROFILE/HOME=C:\Users\louis，GOPATH=C:\Users\louis\go，
  GOMODCACHE=C:\Users\louis\go\pkg\mod，GOCACHE=C:\Users\louis\AppData\Local\go-build
- git 身份是 repo-local 的 cn-maul（原本无配置），可按需改
- 工作区永久有未提交杂物，**永远不要 add**：cmd/gateway/failover_test.go（已修改）、home/、work-r7-store-api.txt
- 构建有两条**已知无害警告**：theme-init.js "can't be bundled"（必须保留原样的外链脚本）；
  chunk >500KB（P4 路由级 lazy 后回落）
- run_code 内嵌模板字符串时内层引号用单引号；.mjs 里不能写 TS 注解

## 7. 验证命令清单（每次提交前全跑）

`cd web && npm run typecheck && npm test && npm run build && node scripts/legacy-audit.mjs`
`go test ./internal/webui/...`（P5 前它是绿的，因为它测的是 embed 里的 Vue 旧产物）
浏览器手工链路（登录→探测→守卫→登出→暗色切换）在 P2 后**尚未走查过**，建议下次先做：
`cd web && npm run dev`（/admin/api 代理到 bin/config.json 的网关端口，需先起 gateway.exe）。

## 8. 组件 API 速记（详见方案附录 §11）

Alert.status / Chip.color+variant+size / TextField 受控 value+onChange 放 TextField /
Input 只吃原生属性 / Button 无 title / Drawer.Content 用 placement+className / Modal.Dialog 有 style /
Checkbox 无 size / Menu.onAction+Dropdown.Item id。
