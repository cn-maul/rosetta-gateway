# 前端 React 重构 · 移交说明

> 写给接手的新对话。三份长文档：`docs/frontend-react-migration-plan.md`（迁移总方案：架构决策、
> 组件映射表、阶段计划、Plan B 漂移控制 §12）、`docs/frontend-ux-overhaul-plan.md`（信息架构与
> 性能整改：首屏口径、骨架规范、图形化改造层）、`docs/api-audit-2026-10-31.md`（管理面接口审计）。
> 文档地图见 `docs/README.md`。本文只讲「现在在哪、怎么继续」。

## 1. 现状一句话

Vue→React 换血**已全部完成并合入本地 `main`**：13 个可渲染页面全部是真实实现（无占位件），
`ui/` 基座与 `components/` 子组件拆分到位；首屏改造（U1）、七个大页面拆分（U2）、路由懒加载 +
vendor 分包 + 偏好记忆（U3）全部落地，入口 chunk 从 1.10 MB 降到约 51 kB。
`typecheck` / `npm test` **98 条** / `build` / `legacy-audit --gate` / Go 三项嵌入守护全绿。
后端另有接口审计 13 条修复。**全部提交都只在本地 `main`，未推送 `origin`**
（准确条数用 `git rev-list --count origin/main..main` 取，别抄文档里的数字）。

## 2. 提交与标签

| 提交 | 内容 |
| --- | --- |
| `pre-react-rewrite`（tag @ `bbe9b08`） | Vue 基线；旧实现取阅：`git show pre-react-rewrite:web/src/views/X.vue` |
| `4141251` | 重构方案 v2 + legacy-audit 脚本 |
| `992ac3d` `b5163f3` | P1 换血 / P2 基座与守卫 loader |
| `c469bb9` `6a59aa5` | B1 Networks 模式模板 + 移交说明 |
| `01d7ec2` … `1745cf6` | B1–B3 全页面重建（用量、总览、模型、上游、设置、密钥、用户、钱包…） |
| `10f0d23` `fbd2d06` | P5 收尾（删占位件）+ 版本 1.7.2 → 2.0.0 |
| `35323cd` `2ddd410` | R10 异步所有权断言回归 + failover 测试排空 |
| `142a6fe` | U1 首屏仪表盘（后被 `77dabd9` 按需求重写） |
| `b9fb9f1` … `3c122a4` | U2 六处大页面拆分：价格页签、上游详情、钱包、用户与分组、密钥 |
| `8f8359a` `92734a4` | U3 路由懒加载 + vendor 分包（1.10 MB → 49 kB）/ 偏好记忆 |
| `3e5ad12` `3ea7bd3` `7018ffe` | 文档收口 / CI 与 build.ps1 接 `--gate` / 方案 §11 执行结果 |
| `e36b14b` … `c2d7e56` | **后端接口审计 13 条修复**（413 双信封、反区间、`{model...}`+JSON 404、复合索引、1+2N、单事务、ctx 查询、探测本地日界、fail-closed、by-* 加 total…） |
| `74e6006` `77dabd9` `2f087f6` | 总览页重写（只留七指标 + 用量趋势表）+ 后端补「首字后生成耗时」 |
| `ed8e126` `61d94b6` | 修上游详情崩溃（null.name）、请求日志表格缺 Table 根、账号设置挪进顶栏 |
| `4155dee` | 请求日志 Token 列箭头化、错误抽屉只讲错误并修正位置、上游详情恢复平铺 |
| `bfa14ad` | 全仓补 `isRowHeader` 修整页崩溃 + 上游模型表拆价格/思考两列 |
| `a875b7e` | 表单可辨识度（field 令牌）、添加模型两档入口（发现/手动） |
| `b37956f` `30ff072` | 思考档位落库（`usage_records.effort`）+ 端到端守护 |
| `8308a09` | 三档价格（去掉缓存写入）+ 上游详情按钮描边 + 手动添加补价 |
| `89830d6` `7a8e6d5` | 八项界面调整（箭头/导航措辞/登录主题/站点品牌/协议检测/时间格式/普通用户视图/删两按钮）；`7a8e6d5` 是后端站点品牌与协议探测 |
| `6095fae` `b793953` `8aedcfc` `bc54f30` `40df0d3` | 128K/1M 简写、路由下拉去重、空白目标行不报错、DataTable `empty` 写死、页签底槽与选中滑块 |

## 3. 已完成 / 待办

**已完成**

- 基座：`theme.css`（唯一 CSS 文件）、`store.ts`、`main.tsx`、`router.tsx`（18 条路由条目，其中
  4 条是兼容重定向）、loader 守卫（`ensureSession` / `authedLoader` / `loginLoader` / `providersLoader`）
- `ui/`：AppModal、AppDrawer、ConfirmDialog + `confirm.ts`、PageHeader、SectionCard、StatusChip、
  DataTable、SkeletonBlock、PageFallback、TokenField、BrandMark、FileImport、theme、nav、Shell、
  AppRoot、Login、PasswordSetup、`branding.ts`（站点品牌 store）、`usePersistedState.ts`
- 页面（`pages/`）：Overview、UsageStats（含 UsageDetail 子页）、Models、Providers、Routes、
  ProviderDetail、Settings、AccessControl、Wallet、AccountSettings、Networks
- 组件（`components/`）：`keys/` `price/` `provider/` `users/` `wallet/` `settings/` 六个子目录，
  弹窗/表格/工具函数/导入 hook 各自成文件；单 tsx ≤ 400 行
- 门禁：`web/tests/` **23 个用例文件 + 2 个 loader shim**（98 条断言），`web/scripts/legacy-audit.mjs`
  报告/`--gate` 两档；Go 侧 version / csp / lockfile 三项嵌入守护；CI 的 webui-sync job 与
  `build.ps1 -Audit` 都已接 `--gate`
- 后端：接口审计 13 条修复（`docs/api-audit-2026-10-31.md`），其中 by-* 统一成 `{records, total}`
  已同步改完前端

**待办（按序）**

1. **浏览器手工走查**（唯一没做过的验证）：`cd web && npm run dev`（`/admin/api` 代理到
   `bin/config.json` 的网关端口，需先起 `bin/gateway.exe`）。重点看：路由懒加载首屏骨架、偏好刷新后
   是否保留、深链 `?tab=` / `?model=` / `?range=` / `?days=` / `?size=`、暗色切换、401 静默、登出
2. **推送与发布**：`origin/main..main` 有 70 多个提交未推送。推送前跑一次
   `go test ./... -count=1`（含 `cmd/gateway` 的 e2e，约 80s）与二进制冒烟
3. **XERJ 重建索引**：`%LOCALAPPDATA%\Programs\xerj\xerj-index.ps1 .`（本机 9200 端口当前未起；
   索引里还留着已删的 `web/src/views/*.vue`，重建后才会消失）
4. **分支清理**：`feat/react-rewrite` 与 `fix/api-audit-p0` 都已合入 `main`（`main` 领先它们
   35 / 20 个提交，`git branch --merged main` 可复核），确认无遗漏后可删

## 4. 每页重建的标准模式（新增页面时照抄）

1. 需要旧语义时读 Vue 基线：`git show pre-react-rewrite:web/src/views/X.vue` —— 页面语义注释有价值
2. 数据：`api.ts` 直接调用 + `useState`；并发查询一律用 `RequestGeneration` 守卫（`asyncState.ts`），
   401 一律静默（`is401(e)`），错误走 `toast.danger` + 页内 Alert
3. 布局：PageHeader + SectionCard / DataTable + HeroUI 组件；内容区外边距由 Shell 提供
   （`max-w-app p-gutter`），页面不再自带 `.page` 容器
4. 弹窗 AppModal / 抽屉 AppDrawer；危险操作 `confirmBox({ danger: true })`
5. 状态徽章 StatusChip（`ok/err/warn/info/dim`）；图标 lucide-react（`size={18} strokeWidth={1.75} aria-hidden`）
6. 加载态：首屏骨架用 SkeletonBlock / DataTable 的 `skeletonRows`
7. 大页面拆分：弹窗自包含（表单状态、校验、提交都在弹窗内，父级只传 `open` / `onSaved`）
8. 页签：`<Tabs.ListContainer><Tabs.List>` + 每个 `<Tabs.Tab>` 内 `<Tabs.Indicator />`（缺了没有底槽
   与选中滑块）；`?tab=` 同步到 URL，**首次激活页签才拉数据**（参考 ProviderDetail 的 `subsLoadedRef`）
9. 用户选择类偏好用 `usePersistedState`（URL 优先，偏好只做默认值）
10. 表格：`<Table aria-label>` → `<Table.ScrollContainer>` → `<Table.Content>`，每张表第一列必须
    `isRowHeader`（缺了客户端直接抛 `A table must have at least one Column with the isRowHeader prop`）；
    DataTable 只负责工具条/加载/错误/空态/骨架，**不包 Table 根**，`empty` 不许写字面量
11. 每页做完跑第 7 节全部命令

## 5. 硬性纪律（violate = 返工）

- **守护测试不可红**：CSP（`index.html` 无内联脚本/事件、`theme-init.js` 同步外链）、
  `version_test`（`dist/index.html` 的入口 chunk 必须含 `2.0.0` 字面量 → 版本号渲染在 Shell 页脚，
  Shell 永远在入口依赖链上，页面懒加载不影响）、`lockfile_version_test`
  （`web/package-lock.json` 根 version == `web/package.json`，现 2.0.0）
- **反手写门禁**：tsx 无裸 `<button>/<input>/<select>/<table>`、无新 CSS 文件（仅 `theme.css`）、
  无 `.vue` 回潮。文件选择一律用 `ui/FileImport`
- **`internal/webui/dist` 必须随前端改动一起提交**：embed 与 `version_test` 读的都是它。
  改完前端一定 `npm run build && npm run sync`，否则 Go 侧测试看着绿、其实测的是上一版产物。
  `web/dist` 不入库
- **业务文件不动结构**：`api.ts` / `types.ts` / `fmt.ts` / `range.ts` / `routeTrace.ts` 只做最小改动
- **单 tsx ≤ 400 行**，超出拆子组件
- **每个子步骤完成立刻提交**：本分支上有过一次教训 —— 未提交的落盘在会话换代时整片丢失，
  `git status` 干净、HEAD 还停在上一笔，只能从精读笔记重做
- **不要跑 `npx prettier`**：仓库没有 `.prettierrc`，默认配置会把单引号改双引号、加分号，
  300 行 diff 会膨胀到 800+ 行

## 6. 环境与工具坑（本机 Windows，踩过的）

- shell 是 **bash**；多行提交信息用 `git commit -F <msgfile>`（`home/` 被 `.gitignore` 挡，
  `git add` 时不要带它，否则整条命令 exit 1）
- `go test` 需注入 env：`USERPROFILE`/`HOME=C:\Users\louis`、`GOPATH=C:\Users\louis\go`、
  `GOMODCACHE=C:\Users\louis\go\pkg\mod`、`GOCACHE=C:\Users\louis\AppData\Local\go-build`
- **`/tmp` 在 Windows 上不可写**：二进制冒烟目录必须放工作区内（如 `.smoke/`），否则 `master.key`
  创建失败、gateway 拒绝启动；冒烟结束 `taskkill //F //IM gw.exe` + `rm -rf .smoke`
  （把 `bin/gateway.exe` 拷成 `.smoke/gw.exe` 就是为了按进程名精准清理）
- gateway 的 `config.json` 默认生成在 `ROSETTA_GW_HOME` 里**而不是 cwd** —— 冒烟要在
  `<tmp>/home/config.json` 写 `{"listen":"127.0.0.1:18xxx"}`
- 构建有一条**已知无害警告**：`theme-init.js can't be bundled without type="module"`（必须保留的
  同步外链脚本）。heroui chunk 约 484 kB，是单一最大块
- **不要用 `npx tsc -b`**：会在 `web/` 生成 `vite.config.js` / `vite.config.d.ts` / `*.tsbuildinfo` 垃圾。
  用 `npm run typecheck`（`tsc --noEmit`）
- Vite 8 是 Rolldown 构建器：分包写在 `build.rolldownOptions.output.codeSplitting.groups`，
  `output.manualChunks` 已废弃且与前者同时给会被忽略
- `node --test` 的用例路径全部走 `web/tests/`，TypeScript 源码由 `tests/tsResolve.mjs` 的
  Node loader 现场转译，所以测试可以 `import` 真实的 `.ts`

## 7. 验证命令清单（每次提交前全跑）

```bash
cd web && npm run typecheck && npm test && npm run build \
  && node scripts/legacy-audit.mjs --gate && npm run sync
cd .. && go build ./... && go test ./internal/webui/... -count=1
# 动了后端时再加：
go test ./internal/... -short -count=1 && go test ./cmd/gateway/ -count=1
```

当前基线：tsc 0 错 · tests **98/98** · build ok（入口约 50.6 kB / react 313.6 kB / heroui 484 kB）·
audit `.tsx` 58 文件、裸交互原语 0（白名单 2）、旧类名 0 · `go build ./...` ok · `go test ./internal/... -short` 全绿

## 8. 组件 API 速记（详见方案附录 §11）

Alert.status / Chip.color+variant+size / TextField 受控 `value`+`onChange` 放 TextField、Input 只吃原生属性 /
Button 无 `title`（包 `<span title>`）/ Checkbox 无 `size` / Menu.onAction+Dropdown.Item id。

后续实测补充：

- `Button variant` 只有 `primary | secondary | tertiary | ghost | outline | danger | danger-soft`；
  **`outline` 才是带描边的次按钮**（`border-width:1px; border-color:var(--border)`）
- `Drawer`：宽度必须给 `Drawer.Dialog`（`style={{ width:'100%', maxWidth:680 }}`）；写在 `Drawer.Content`
  上只会缩窄 overlay，面板会贴左
- `Tabs`：`Tabs.ListContainer` 是灰底底槽、`Tabs.Indicator` 是选中滑块，后者必须是 `Tabs.Tab` 的子元素
- `ProgressCircle` 的 `size` 只接受 `sm|md|lg` · `ProgressBar.Fill` 必须自带 `style={{ width:'…%' }}` ·
  `Skeleton` 只有 `SkeletonRoot` · `Tooltip` 需 `<Tooltip delay={300}>` 包住 Trigger/Content
- React 保留 prop `key` 不能当普通 prop 传递（`KeyEditModal` 因此把入参叫 `keyRow`）
- Select 若不写 `<Label>`，会与同排的 TextField 基线错开一行 —— 并排字段两侧都要有 Label
- 输入框默认 `--field-background` 与卡片同色、`--field-border: transparent`；已在 `theme.css` 覆盖
  为 `--surface-secondary` + `--border` + `1px`
