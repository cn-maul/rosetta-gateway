# 公共表格、抽屉与聚合接口验收（2026-10-18）

承接 [正式运行验收](frontend-production-acceptance-2026-10-18.md)。本轮按用户要求统一公共表格和抽屉、补充后端聚合接口；路由独立页面式编辑暂缓，继续使用现有编辑弹窗。

## 已完成范围

| 对象 | 改动与边界 |
|---|---|
| 公共表格 | 新增 `web/src/components/DataTable.vue`，14 张原生表格共用具名滚动区域、键盘聚焦与局部横向滚动容器。页面继续负责列语义和行操作。路由、上游、请求日志三个主列表进一步接入公共加载、错误、空态、旧数据保留与重试状态；其余表格保留页面业务状态。 |
| 迁移页面 | Routes、Providers、UsageDetail、Keys、ModelGroups、Settings 各一张；UsersGroups 两张；Wallet 三张；CredentialPanel、UpstreamModelPanel、UsageByGroup 各一张，共 14 张。 |
| 公共抽屉 | 新增 `web/src/components/DetailDrawer.vue`，路由完整链和请求详情共用标题、关闭、底部操作栏和正文滚动。复用 AppModal 的弹窗栈、背景锁定与焦点管理；手机全屏显示。 |
| 弹窗行为 | 修复未传 dismissable 时被 Vue 转为 false 的默认值问题；详情可用 Escape 关闭。显式不可关闭的编辑表单保持其行为。无可聚焦子元素时聚焦弹窗本身，异步初始聚焦仅作用于当前栈顶。 |
| 路由数据 | 列表由三个请求改为一次读取路由目录；完整链直接使用目录内目标。编辑仍单独读取最新目标，并保留链未就绪、加载中或失败时禁止保存的保护。 |
| 近期性能 | 总览按实际样本数解释首字用时和输出速度，明确无样本、读取失败，以及不随日期区间变化的统计口径。 |

## 路由目录契约

新增管理员接口 `GET /admin/api/route-catalog`，返回 `routes`、`providers`、`models` 与毫秒时间戳 `generated_at`。

- 每条 route 保留现有字段，增加按 position 排序的完整 `targets` 和 `chain` 摘要。
- `chain.total` 是配置目标数，`enabled` 是已启用的配置目标数，`standby` 是除首个目标外的配置条目数。关闭自动转移时仍保留后备计数；这些计数不代表实时可用目标数。
- 上游仅返回选择器需要的 id、slug、name、enabled、ready 和可选 ready_reason，不包含 endpoint、proxy、凭据或余额查询配置。快照缺失时 ready 为 null，表示未知。
- 四次批量 DAO 查询后在内存关联，避免按每条路由或目标追加查询；没有新增逐路由请求。
- handler 校验管理员，真实服务鉴权链测试确认匿名 401、普通用户 403、管理员 200。

## 近期性能聚合口径

`GetRecentPerformance` 使用一条 CTE SQL 分别选取最近最多 5 条合格成功请求，返回指标及各自实际样本数。两个指标可以来自不同样本集合；普通用户仅统计自身请求，管理员统计全局。

- 输出速度：合格样本的输出 token 总和 × 1000 / 总 latency_ms。
- 首字用时：有有效首字计时的合格样本的 ttfb_ms 平均值。
- stats 保留既有平均值字段，增加 `throughput_samples`、`ttfb_samples`、`recent_sample_limit` 和 `recent_performance_status`。
- status 为 ready 且样本数为 0 表示无样本；error 表示读取失败。不能把没有样本显示为零秒或推断为性能良好。

## 自动检查

以下检查均通过：

- `npm.cmd run typecheck --prefix web`。
- `node --test web/tests/routeTrace.test.mjs`，5 项通过。
- `npm.cmd run build --prefix web`、`npm.cmd run sync --prefix web`；资源已同步到 `internal/webui/dist`，保留既有 theme-init.js 非 module 提示。
- `go test ./...`、`go vet ./...`。
- 后续补充的真实鉴权链测试 `go test ./internal/server -run TestRouteCatalogRealAuthBoundary -count=1` 通过。
- `git diff --check`。

路由目录测试覆盖目标顺序、禁用条目、关闭自动转移后的后备计数、敏感字段排除、未知就绪状态及空数组。近期性能测试覆盖超过 5 条样本、两个指标不同样本数、失败与无计时排除、用户隔离、无样本、日期窗口独立性及管理员全局范围。

## 实际浏览器验收

使用候选 `bin/gateway-common-ui-verified.exe`（版本 1.6.2），在 `127.0.0.1:18773` 运行已脱敏的隔离数据库副本 `bin/common-ui-acceptance-20261018-1`。全部 13 个上游地址均为本机回环地址、proxy 为空、定时探测关闭，没有调用外部付费模型。浏览器读取真实 Go 接口及嵌入资源。

| 场景 | 实测结果 |
|---|---|
| 目录与详情 | 9 条隔离路由正常展示，四目标链及配置计数一致；关闭自动转移的后备仍明确显示。 |
| 桌面抽屉 | 1280 × 720 下标题 70px、底栏 68px 保持可见，正文可视 556px、内容 752px，仅正文滚动。 |
| 键盘与关闭 | 初始焦点在“关闭详情”；Shift+Tab 回卷至“编辑目标链”；Escape 关闭后恢复背景滚动与交互，焦点返回原路由按钮。 |
| 手机抽屉 | 390 × 844 下整页宽度为 390px，底栏落在视口底部，动作可换行，正文单独滚动。 |
| 请求日志 | 真实副本中的错误轨迹、公共关闭栏正常；翻页显示第 2/87 页。 |
| 筛选空态 | 无匹配搜索显示空筛选，清除后恢复 9 条路由。 |
| 失败保留旧数据 | 停止隔离后端后刷新，显示网络错误、保留上次成功结果及可用重试按钮，9 条旧路由仍可见。 |
| 浏览器日志 | 停止隔离后端前，浏览器 warn/error 为空。 |

证据截图保存在 Git 忽略目录：`bin/common-ui-desktop-drawer.png`、`bin/common-ui-mobile-drawer.png`、`bin/common-ui-stale-error.png`。

## 交付状态

候选已构建并通过隔离验收，隔离服务已停止，临时浏览器页签已关闭，视口已恢复。没有修改正式配置、凭据、价格、路由或业务启用状态，没有提交 Git。

本轮没有替换正式 `bin/gateway.exe`。收尾检查时 8666 端口没有监听服务；上一份正式验收中的 PID 和探测结果是该次验收的历史记录。新版正式部署与正式运行复验尚未执行，不能将本报告的隔离结果视为已上线。
