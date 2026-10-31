# 文档地图

仓库根有三份长期文档，`docs/` 下是过程记录。本文件说明「哪份还有效、哪份只是历史」。

## 长期有效（改代码时要一起维护）

| 文档 | 讲什么 | 什么时候该动它 |
| --- | --- | --- |
| [`../DESIGN.md`](../DESIGN.md) | **唯一的设计事实源**：架构、模块职责、协议/路由/计费/鉴权规则、目录结构 | 改架构、加模块、改契约 |
| [`../MULTIUSER.md`](../MULTIUSER.md) | 多用户/配额/余额/密钥的设计与运维口径 | 动用户、配额、计费 |
| [`../README.md`](../README.md) | 对外介绍：能力清单、快速开始、部署 | 改对外能力或部署方式 |
| [`../CONTRIBUTING.md`](../CONTRIBUTING.md) | 开发环境、构建、门禁、行尾与工具链约束 | 改构建链、门禁、工具版本 |
| [`../DOCKER.md`](../DOCKER.md) | Docker 部署细节（镜像、挂载、日志、升级、健康检查） | 改镜像或容器运行方式 |

## 前端（`web/`）

| 文档 | 状态 | 内容 |
| --- | --- | --- |
| [`react-migration-handoff.md`](react-migration-handoff.md) | **现行 · 先读这份** | 现在在哪、怎么继续、每页标准模式、硬性纪律、环境坑、验证命令、组件 API 速记 |
| [`frontend-react-migration-plan.md`](frontend-react-migration-plan.md) | 现行（历史决策） | Vue→React 迁移总方案：架构决策、组件映射表、阶段计划、Plan B 漂移控制、HeroUI API 实测更正 |
| [`frontend-ux-overhaul-plan.md`](frontend-ux-overhaul-plan.md) | 已实施完毕 | 信息架构与性能整改：首屏口径、骨架规范、图形化层；§11 是执行结果 |

## 审计记录

| 文档 | 状态 | 内容 |
| --- | --- | --- |
| [`api-audit-2026-10-31.md`](api-audit-2026-10-31.md) | 现行 | 管理面 HTTP 接口审计：13 条已修 + 判定「不是 bug」+ 明确不做的项及理由 |
| [`audit-2026-10-22.md`](audit-2026-10-22.md) | 历史（2026-10-20 全量审计 + 10-21/22 修复复核） | 12 条生产 P0、50 条 P1 的判定与落地；行号是 3532 行单文件 `cmd/gateway/main.go` 时代的历史坐标 |
| [`audit-history.md`](audit-history.md) | 索引 | 四轮审计的基线 / 结论 / 修复提交一览，指向 git 历史 |

## 历史留档（已从工作树清理，需要时从 git 历史取回）

2026-11-01 做了一次仓库清理：Vue 时代的验收记录、被后续审计取代的明细报告全部删除（内容都在
git 历史里，用 `git log --diff-filter=D -- docs/<path>` 找到删除它的提交，再 `git show <commit>^:<path>` 取回）。

| 已删除 | 删除理由 |
| --- | --- |
| `docs/audit-2026-10-20/`（A1–A11 + SUMMARY，584K） | 全部指向已拆包的 3532 行 `cmd/gateway/main.go`；结论已被 `audit-2026-10-22.md` 吸收 |
| `docs/audit-fresh-2026-10-24/`（7 个，428K） | 54 条发现已全部修完（R0–R12）；`PLAN.md` 自己写着「不要用 10-20 那份当证据」 |
| `docs/network-proxy-audit-2026-10-20.md` | 结论已被 `api-audit-2026-10-31.md` 与 DESIGN.md 吸收 |
| `docs/frontend-information-architecture-review-2026-10-17.md` | Vue 3 时代的 IA 评审，页面结构已整体重写 |
| `docs/frontend-{integration,production}-acceptance-2026-10-18.md` | Vue 时代验收记录，引用已删除的 `.vue` 组件 |
| `docs/frontend-shared-components-2026-10-18.md` | 讲 `DataTable.vue` / `DetailDrawer.vue` 等已不存在的组件 |

取回任意一份：

```bash
# 找到删除它的提交
git log --oneline --diff-filter=D -- docs/audit-2026-10-20/A3-outwire.md
# 用该提交的父提交取回内容
git show <那个哈希>^:docs/audit-2026-10-20/A3-outwire.md
```

## 建议阅读顺序

1. 新人接手：`README.md` → `DESIGN.md` §1–§3（架构）→ `CONTRIBUTING.md`（怎么跑起来）
2. 改前端：`docs/react-migration-handoff.md`（现行状态与纪律）→ `docs/frontend-react-migration-plan.md`
   §4（组件映射）+ 附录（HeroUI API 实测更正）
3. 改后端接口：`DESIGN.md` 对应章节 → `docs/api-audit-2026-10-31.md`（已修什么、哪些判定不做）
4. 查历史决策为什么这么做：`git log --oneline -- <path>` + 上述审计文档
