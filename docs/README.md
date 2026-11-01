# 文档地图

仓库只保留一条文档链：根目录的 [`README.md`](../README.md) 负责介绍与入口，
其余正式文档都在本目录。过程性审计、迁移计划和整改报告在结论落地后删除；
需要追溯时从 Git 历史读取，不再堆在工作树里。

## 长期有效

| 文档 | 讲什么 | 什么时候更新 |
| --- | --- | --- |
| [`../README.md`](../README.md) | 对外介绍、能力清单、快速开始、文档入口 | 改对外能力或部署方式 |
| [`DESIGN.md`](DESIGN.md) | **唯一设计事实源**：架构、数据模型、协议、路由、计费、鉴权 | 改架构、模块、接口或持久化契约 |
| [`DOCKER.md`](DOCKER.md) | 镜像、挂载、日志、时区、备份、升级、健康检查 | 改容器或发布方式 |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | 开发环境、构建、CI 门禁、行尾与工具链约束 | 改构建链、门禁或工具版本 |
| [`MULTIUSER.md`](MULTIUSER.md) | 多用户 / 分组 / 配额模型的历史设计记录 | 默认不改；代码注释仍以它的章节号为锚点 |

## 约定

- 当前版本看 `web/package.json`、Git tag 与提交历史，不在文档里维护“最新版本”。
- 审计、迁移、整改完成后，把仍然有效的决定写进 `DESIGN.md` 或
  `CONTRIBUTING.md`；一次性报告删除，详细过程留在 Git 历史。
- 历史文档可以这样取回：

```bash
git log --oneline --all -- docs/<path>
git show <commit>^:docs/<path>
```

## 阅读顺序

1. 新人接手：根目录 `README.md` → `DESIGN.md` §1–§3 → `CONTRIBUTING.md`。
2. 改数据面：`DESIGN.md` §5–§12，再读对应包的注释与测试。
3. 改管理面或前端：`DESIGN.md` §6 与 §13，再运行完整 CI 门禁。
4. 部署与升级：`DOCKER.md`，升级前完整备份状态目录。
