# 前端信息架构与性能整改方案（v2）

状态：**已实施完毕**（2026-10-31，U1/U2/U3 全部落地，U5 不立项；执行结果见 §11）。v1 只覆盖四个最重页面；v2 应「所有页面都够快、没有大页面」的目标，
补全全部页面加载清单、骨架屏/占位规范、图形化（进度条/圆环/迷你图）改造层。
分支基线 feat/react-rewrite @ 05dddf1。反馈来源：迁移交付后使用者复盘。

## 0. 现状诊断（问题 → 证据）

### 0.1 想看的信息第一时间看不见

- 登录后落在「网关总览」，统计范围**默认 14 天**——「今天正常吗、今天花了多少」都要手动切。
- 「异常」被动：最近失败请求要自己去 /history 筛 status；总览待办只覆盖配置问题，不覆盖「刚才有请求失败」。
- 费用信息分散：总览 KPI 是区间汇总；按天/按模型在 /usage 页签；管理员看「谁花得多」在 /wallet。

### 0.2 信息组织方式问题

- 侧栏三组按页面类型分而非使用频率：高频的「API 密钥」「账户与充值」在第三组末尾。
- 模型相关入口三处（模型与路由 / 可用模型 / 设置-价格页签）。

### 0.3 大页面慢（全部页面挂载请求盘点，已逐文件核实）

| 路由 | 挂载请求 | 重量 | 对策（§号） |
| --- | --- | --- | --- |
| / 总览 | admin: stats + usageByDay + statsOverview + providers（4 并发）；user: stats + usageByDay | 重 | 首屏拆分，stats(1d) 先答，byDay 懒加载，网关区独立骨架（§3） |
| /history 请求日志 | usageHistory(单页) + keys()（筛选项） | 轻 | 保留；补表格骨架行（§6） |
| /usage 用量页签 | 每页签一个 usageBy{Provider,Model,Key}(limit 200) | 轻 | 保留 |
| /usage?tab=groups | ModelGroups：modelGroups + allModels + providers **3 并发** | 重 | 拆：组表只要 modelGroups；allModels+providers 延到成员弹窗打开（§5） |
| /models 可用模型 | modelNames + probeModels + probeTimeline(默认前 6) | 中 | 保留（时间线已按卡展开拉）；probeModels 卡片骨架化（§6） |
| /wallet | admin: stats(0,now) + topups + usageByUser(100)；user: stats + usageByDay(90 天) + topups | 重 | stats 卡先渲染；topups/byUser/byDay 按区块懒加载 + 骨架（§5） |
| /keys | keys；modelNames/users/groups 已惰性 | 中 | 惰性结果加模块级缓存，切页签不重拉（§5） |
| /users 用户与权限 | users + groups + modelNames 并发 | 重 | modelNames 延到白名单弹窗打开（§5） |
| /providers 上游列表 | providers() | 轻 | 保留；卡片骨架 |
| /providers/:id | provider + credentials + models **3 并发** | 重 | 概览先渲染；凭据/模型按页签惰性（§5） |
| /routes 路由 | routeCatalog；routeTargets 编辑时才拉 | 轻 | 保留 |
| /networks 代理 | networks() | 轻 | 保留 |
| /settings | settings；价格页签 providers + allModels 全量 | 重 | 价格页签拆弹窗数据源 + 按供应商懒分组（§5） |
| /account | 无 | — | — |

结论：轻页面（history/usage/providers/routes/networks）只缺骨架屏；「大页面」实为 **6 处**：
总览、价格页签、上游详情、Wallet、UsersGroups、ModelGroups（groups 页签）——v1 漏了后两个。

## 1. 设计原则

- **P1 登录即答案**：首屏 3 秒内回答「现在正常吗 + 今天花了多少」。
- **P2 一屏一问，渐进披露**：第一屏只回答一个主问题；细节折叠、进抽屉、降第二屏。
- **P3 按需加载**：首屏可见的数据才挂载拉；弹窗、折叠区、第二屏打开时才拉。
- **P4 记住偏好**：范围/页签/筛选写 localStorage（URL 参数优先）。
- **P5 角色分层**：管理员与普通用户首屏、导航独立设计。
- **P6 占位先行**：页面骨架（布局 + Skeleton）立即渲染，数据到了再填；任何异步区域必须有「骨架 → 数据 → 错误」三态，禁止白屏等待。
- **P7 图形优先**：能一张图说清的不用文字；比例用圆环/进度条，趋势用迷你折线，状态用色点+图标；零新增依赖（HeroUI 自带组件 + 手绘 SVG）。

## 2. 首屏仪表盘重排（改 Overview.tsx，路由不动）

### 2.1 管理员首屏（自上而下）

1. **状态条**（一行四格，图形化）：上游就绪 **ProgressCircle**（X/M，异常红圈直达 /providers?status=unready）· 公开路由数 · 今日请求 **成功率圆环**（≥90% 绿 / ≥70% 黄 / else 红）· 今日花费。
2. **今日关键数字卡 ×4**：请求数 / 成功率 / Token / 费用。**默认范围 1d（今天 0 点起）**，卡内副行「近 7 天均值」对比 + 迷你趋势（sparkline）；点卡片跳明细。范围选择器保留并记忆（P4）。
3. **最近异常请求**（内嵌表）：`usageHistory(days=1, limit=8, filters={status:'error'})`，行点击开详情抽屉（从 UsageDetail 抽共享组件）；底部「查看全部失败 → /history?status=error」；无失败渲染成功态一行。
4. **第二屏**：趋势（保留现有条形图）、待办面板、当前状态五卡（配置信息降级到这里）。

### 2.2 普通用户首屏

1. **余额大卡**：余额 + 待结算 + 配额 **ProgressBar**（≥80% 警示色）。不等 byDay。
2. **消耗小结两格**：今日 / 近 7 天（请求数/Token/费用），配 sparkline。
3. **最近 10 条调用**：复用 §2.1 内嵌表，成功/失败徽章着色，行点击开抽屉。
4. **快捷操作条**：新建密钥、复制密钥、查看可用模型。

### 2.3 数据加载策略

- 统计区与网关状态区并行渲染、各自 Skeleton，互不阻塞（现状请求已分开，渲染不再等最慢的）。
- usageByDay 延到趋势区可见时才拉。
- 首屏一律 Skeleton 占位（§6），不用单点 Spinner。

## 3. 导航按频率重组（改 ui/nav.ts + Shell）

管理员：**常用**＝网关总览 / · 请求日志 /history · 用量与费用 /usage · API 密钥 /keys · 账户与充值 /wallet；
**管理配置**＝上游服务 /providers · 模型与路由 /routes · 用户与权限 /users · 代理 /networks · 系统设置 /settings。
普通用户单组：我的总览 / · 我的调用 /history · 消耗与费用 /usage · API 密钥 /keys · 余额与账单 /wallet · 可用模型 /models。
路由 URL 零变更。

## 4. 大页面拆分（全部 6 处）

1. **价格页签**（PriceSettings）：a) 弹窗数据源下沉——新增/编辑/批量弹窗打开才拉 `api.models(providerId)`；b) 列表按供应商懒分组——默认只拉 providers，选中才拉该组模型，去 allModels 全量；分页保留。（可选 U5 后端分页接口。）
2. **上游详情**（ProviderDetail）：概览挂载即渲染；`Promise.allSettled([credentials, models])` 拆成独立 `loadCreds`/`loadModels`，进入对应页签才调，各自 loading/err。
3. **Wallet**：stats 汇总卡挂载即渲染；topups / byUser / byDay 三个区块各自独立请求 + 懒加载（进视口或滚动触发），管理员 usageByUser(全历史聚合) 是最贵的一笔，放最后。
4. **UsersGroups**：users+groups 保留并发；modelNames 移入白名单弹窗打开时。
5. **ModelGroups**（/usage?tab=groups）：组表挂载只拉 modelGroups；allModels + providers 延到「成员」弹窗打开时（弹窗内本就要展示可选模型）。
6. **Keys**：keys 挂载拉；modelNames/users/groups 惰性结果缓存到模块级，切 /keys ↔ /users 不重拉。

## 5. 占位与骨架系统（P6，全站适用）

- 组件：HeroUI `Skeleton` 包一层项目封装 `<SkeletonBlock w h rounded?>`（统一圆角与 shimmer 节奏），放 `ui/SkeletonBlock.tsx`。
- 表格：DataTable 增加 `skeleton` 呈现——loading&&!hasData 时渲染 N 行灰条（列数随表格），替代现单点 Spinner。
- 卡片：KpiCard / 状态条 / Wallet 三区块 / 模型卡，未加载时渲染同尺寸骨架。
- 规范：每个异步区域三态齐全（骨架 / 数据 / 错误重试）；骨架尺寸与真实内容一致，避免加载完成后跳动。

## 6. 图形化改造层（P7，零新增依赖）

| 信息 | 现状 | 改为 |
| --- | --- | --- |
| 成功率 | 文字百分比 | HeroUI `ProgressCircle` 圆环（色阶随阈值），中心放百分比数字 |
| 配额/余额 | 文字 | HeroUI `Meter` / `ProgressBar`（≥80% 警示色） |
| 缓存命中率 | 文字 | `ProgressBar` 细条 + 百分比 |
| 费用/请求趋势 | 现条形图（总览）保留 | KPI 卡内补手绘 SVG sparkline（≤60 行组件 `ui/Sparkline.tsx`，HeartbeatBar 同思路） |
| 状态 | StatusChip 保留 | 配色点/图标辅助（lucide，size 14–18） |
| 用量构成（输入/输出/缓存） | 文字 | 堆叠 `ProgressBar` 一条三段 |

手绘 SVG 只做 sparkline 与（可选）环形，复杂图表不引入图表库——保住入口体积目标。

## 7. 入口体积与偏好记忆

- vite `manualChunks` 拆 @heroui/react + react 全家桶 vendor chunk（首包预计降 300–400KB 且长缓存）；复核根入口导入的 tree-shaking。
- `usePersistedState` hook（前缀 `rosetta_gw_`）：统计范围、/usage 页签与天数、/history 条数、价格页签供应商筛选。URL 优先，离开写回。

## 8. 实施批次

| 批 | 内容 | 提交粒度 |
| --- | --- | --- |
| U1 | 首屏仪表盘（§2）+ 导航（§3）+ 骨架系统基座（§5 的 SkeletonBlock/DataTable 骨架行）+ 图形基座（§6 的 Sparkline/圆环封装） | 2–3 个提交 |
| U2 | 大页面拆分全部 6 处（§4），每处连带该页骨架与图形化 | 每处独立提交（6 个） |
| U3 | 入口体积 + 偏好记忆（§7） | 2 个提交 |
| U5 | （可选，需后端）价格/模型分页接口、双区间 stats | 后端另行评审 |

每批沿用门禁：`npm run typecheck` + node --test 全量 + `npm run build` + `legacy-audit` + 仓库根 `go test ./internal/webui/... -count=1`。

## 9. 兼容与风险

- 路由零变更；R10 断言对象（ProviderDetail 余额归属 / Models 时间线 / ModelPicker unionSelection）只拆请求时机不改归属判定。
- 业务文件（api.ts/types.ts 等）不改签名（U5 除外）。
- 风险：价格页签全局搜索收窄为「先选供应商」（备选：模型 <500 保留全量）；Keys 惰性数据弱网首开表单有一次延迟（骨架兜底）；vendor 拆分后 CSS 顺序（styles 独立入口，理论不受影响）。

## 10. 待拍板项

1. 首屏默认统计范围改「今天」？
2. 价格页签全局搜索收窄可接受？（或 <500 模型保留全量）
3. U5 后端分页接口是否立项？
4. 图形化程度：§6 全做，还是先做成功率圆环 + 配额条两处最高频的？

---

## 11. 实际执行结果（2026-10-31）

四个待拍板项当时的结论：**① 做**（首屏默认「今天」，KPI 副行给「近 7 天日均」对照）；
**② 做，但带门控**（供应商 ≥500 才先选供应商，<500 保留全量，`PRICE_PROVIDER_GATE = 500`）；
**③ 不立项**（U5 未做，价格/模型仍走现有接口）；**④ 全做**（圆环 + 进度条 + mini sparkline 都已落地）。

| 批 | 状态 | 提交 | 实际结果 |
| --- | --- | --- | --- |
| U1 | ✅ | `142a6fe` | 首屏=状态条 + KPI（含圆环/迷你折线）+ 最近请求内嵌；第二屏=趋势/待办/配置五卡（`useInView` 滚动才拉 byDay）。新增基座 `SkeletonBlock` / `Sparkline` / `RateRing` / `useInView`，`DataTable` 加骨架行，`ui/nav.ts` 按频率重排两组 |
| U2 | ✅ | `b9fb9f1` `9e93071` `ff1b460` `23a14e9` `30bedfc` `3c122a4` | 六处拆分全部完成：价格页签（弹窗下沉 + 供应商门控 + 批量列表渲染上限 300/次）、上游详情（4 弹窗独立 + 概览/凭据/模型页签化 + 首进页签才拉）、钱包（充值弹窗 + 三张表格独立 + 充值流水懒加载）、用户与分组（六弹窗两表格）、密钥（表格/弹窗/导入 hook）。ModelGroups 344 行经确认无需拆 |
| U3 | ✅ | `8f8359a` `92734a4` | 路由懒加载（除 Networks 因导出 `networksLoader` 保持静态）+ vendor 分包：入口 **1,104.44 kB → 49.53 kB**（gzip 325.16 → 16.58），react 313.58 kB、heroui 490.16 kB、页面 2.1–55.5 kB。偏好记忆 `ui/usePersistedState.ts`（前缀 `rosetta.pref.`）：总览区间、`/usage` 与 `/history` 天数（分开记）、`/history` 每页条数、模型页天数与粒度 |
| U5 | ⏭ 未做 | — | 待拍板项 ③ 结论为不立项 |

**U1 首屏仪表盘已于 2026-10-31 按需求重写（`77dabd9`）**：用户明确「总览只看多少天内的七个指标 + 用量趋势表」，于是状态条 / 配置五卡 / 待办卡 / 最近请求 / 余额卡 / 柱状图全部从总览移除（各有专页）。保留的是区间选择 + 七指标（请求数 / Token / 成功率 / 缓存命中率 / TPS / 平均首字 / 首字之后的生成耗时）+ 按天趋势表（八列同口径）。为此后端先补了指标（`74e6006`）：`/stats` 加 `avg_gen_ms` + `gen_samples`（avg_ttfb + avg_gen ≈ 平均总耗时，刻意不合成一个数），`UsageSource` 两条来源支各加 `gen_ok_sum` / `n_gen_ok` 归一列，四个 by-* 每行补 `cached_tokens` / `cache_hit_rate` / `avg_gen_ms` / `n_gen`。上一版为此新增的 `Sparkline` / `RateRing` / `useInView` / `RecentRequests` 随本次重写一并删除（git 历史可查）。

门禁基线（U1–U3 每批每步都跑）：`npm run typecheck` 0 错 · `node --test` **51/51**（新增
`persistedPrefs` 5 条）· `npm run build` ok · `legacy-audit` 裸原语 0（白名单 2）/旧类名 0 ·
`go test ./internal/webui/... -count=1` ok。另做了一次二进制冒烟：`build.ps1` 出的
`bin/gateway.exe` 在临时目录起服务后 `/admin/` 200、入口 chunk 与懒加载 chunk（Overview /
react / heroui）全部 200 且字节数与构建输出一致 —— 证明 embed 里的产物与源码同批。

两项偏离原计划，记录在此：**①** §7 说的 `manualChunks` 在 Vite 8（Rolldown）里已废弃，
实际用 `build.rolldownOptions.output.codeSplitting.groups`；**②** 偏好前缀按实现改为
`rosetta.pref.`（原文 `rosetta_gw_`），键按页面分域（`overview.range` / `history.days` /
`usage.days` / `history.size` / `models.days` / `models.granularity`）。
