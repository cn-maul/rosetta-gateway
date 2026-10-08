# 项目状态交接（2026-10-10）

> **给下一个会话读这个文件就够**，不必重读全部对话。
> 上下文预算：每轮约 685K token。带这份文件（~4K）比带 20 轮对话便宜得多。

## 一、项目

`C:\Users\louis\Desktop\project\rosetta-gateway` — LLM API 网关（Go + Vue3）。
配套 SDK：`C:\Users\louis\Desktop\project\rosetta`（**同一个所有者，v1.0.1 已发**）。

## 二、当前状态：138 个文件未提交

HEAD 仍是 `d529b29`（1.4.3 时代）。全部改动在工作区，**尚未 commit**。

### 已完成并验证的改动（20 项）

| 类别 | 内容 |
|---|---|
| **P0 计费** | 扣费失败误提交幂等占位行；`balance_charges` 主键缺 `user_id` |
| **P0 性能** | 批量落库（`CreateUsageRecordBatched`），单条路径一行未改 |
| **P1 数据面** | 流式失败误报 HTTP 200；关停 panic（队列已关仍写）；上游畸形响应不可转移（SDK 侧修） |
| **P1 鉴权** | 管理员退出数据面（不能调 `/v1`、不能建 key、不能被认领 key、不能自充值）；改密限速器漏并发额度 |
| **P1 余额** | 「不足一分」整笔漏收 → 余数机制（微元累加） |
| **P2 其他** | `/usage/history` 不认 `from=0`；总请求无上限；白名单改精确匹配；`-race` 进 CI；Docker 发布加测试门禁 |
| **功能** | 侧边栏重排、用户+分组合并页、钱包页、按用户消费汇总、充值流水表、密钥筛选排序、调用历史 t/s + 首字/总耗时 |

### 关键决策（别推翻）

- **扣费失败不重试** —— 防重复收费
- **流式解不开不转移** —— 防重放已交付输出、重复计费
- **批量扣费不做** —— 实测只值 12%，折叠幂等键风险高
- **改钱单位不做** —— 实测语句次数不变，性能无收益；且用户负载仅 0.049 req/s

## 三、**未修的 5 个已知缺口**（不是遗漏，是决定或待办）

1. **欠费时余数被回滚丢弃** —— 缺功能，且 `balance_dao.go` **文件头注释还写着「余数照留」，与实现矛盾** ⚠️ 发布前应改
2. 落库与扣费两个事务 —— SIGKILL 会丢一次扣费
3. `usage_records.user_id` 在 key 改归属后漂移 —— 无对账
4. 剪枝水位非单调 —— 长期运行才显现
5. 批量扣费 —— 建议不做

## 四、发布前必做的 5 步（用户已确认按此顺序）

1. **改掉 `balance_dao.go` 文件头那句自相矛盾的注释**（约 5 分钟）
2. **commit 138 个文件** —— 分批提交，每类改动一个 commit（便于回滚）
3. **版本号 1.4.3 → 1.5.0** —— 有破坏性变更（管理员 key 失效、白名单收紧、rosetta→v1.0.1）
4. **确认 `rosetta v1.0.1` 能被正常校验拉取** ⚠️ 见下
5. **推送让 CI 跑第一次**；CI 绿了再打 tag

## 五、⚠️ 两个已知风险

### 1. rosetta v1.0.1 的 sumdb 可能未同步

拉取时 `go get` 报 `sumdb ... 404 Not Found`，用了 `GOSUMDB=off` 才装上。
`go.sum` 已记录 v1.0.1。若别人 clone 时 sumdb 仍未同步 → **装不上**。
**处理**：先重试一次 `go get github.com/cn-maul/rosetta@v1.0.1`（不带 GOSUMDB=off）验证。

### 2. CI 从未真正跑过

我只在本地验证过（`build`/`vet`/`gofmt`/全量测试/`-race` 全绿，CI 三个门禁本地对照通过）。
`docker.yml` 的测试门禁是本轮新加的，**首次执行就在发版时**。

## 六、环境事实（省去重复摸索）

### 测试

```bash
go build ./... && go vet ./... && gofmt -l cmd internal   # 全绿
go test -count=1 ./...        # 16 包全绿
CC=C:/mingw64/bin/gcc.exe go test -race -count=1 ./...   # 竞态检测（本机必须带 CC）
```

**`-race` 必须带 `CC=C:/mingw64/bin/gcc.exe`** —— 默认 PATH 上的 MinGW 在
`C:\Program Files\mingw64`，路径含空格，链接器解析不了。这是本轮发现的关键事实。

### 前端

pnpm 在此环境会失败。用 node 直接调：
```powershell
$node = "C:\Users\louis\.dsh\dsh-runtimes\dsh-primary-runtime\dependencies\node\bin\node.exe"
cd web
& $node node_modules\vue-tsc\bin\vue-tsc.js --noEmit
& $node node_modules\vite\bin\vite.js build
& $node sync-embed.mjs     # 改前端后必须跑，否则 CI 的 webui-sync 会红
```

### 代码风格（重要）

- 注释**中文、高密度、每个非显然决策都解释「为什么」**
- 测试断言要钉**行为**而非猜测值；反例：不要断言「上游没被调用」除非证明它**能**被调用
- 探测（探针）用完即删，但**结论写进注释或报告**

## 七、审计报告位置

`AUDIT/` 下 20 份，重点：
- `test-summary.md` —— **总审计结论（先读这个）**
- `test-datapath.md` / `test-billing.md` / `test-authz.md` / `test-perf.md` —— 四个方向的动态测试
- `fix-p1-perf.md` —— 性能修复（含一条对我给的 38.8× 前提的纠正）
- `AUDIT-sdk-malformed.md` 在 **rosetta 仓库**内（未提交）

每份都有「未能验证的部分」诚实清单 —— 别当它们是全覆盖。

## 八、用户的实际环境

- 团队生产网关**不在本地**运行
- 约 4200 请求/天、950M token/天（≈0.049 req/s，占实测峰值 0.0019%）
- 缓存命中率 99%，成本 421.22 元/天（其中**输出 token 占约 73%**，单价 9.0 vs 缓存输入 0.1）
- 单价：`price_input=3.0` / `price_cache_hit=0.1` / `price_output=9.0`（元/百万）