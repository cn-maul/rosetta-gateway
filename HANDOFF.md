# 项目状态交接（2026-10-10）

> **给下一个会话读这个文件就够**，不必重读全部对话。
> 上下文预算：每轮约 685K token。带这份文件（~4K）比带 20 轮对话便宜得多。

## 一、项目

`C:\Users\louis\Desktop\project\rosetta-gateway` — LLM API 网关（Go + Vue3）。
配套 SDK：`C:\Users\louis\Desktop\project\rosetta`（**同一个所有者，v1.0.1 已发**）。

## 二、当前状态：已全部提交，版本 1.5.0

本轮共 9 个提交（`d529b29` 之后），155 文件 +27762/-1406，工作区干净：

| 提交 | 内容 |
|---|---|
| `850a207` | 余数机制 + 批量落库 + 充值流水 + 按用户汇总 |
| `c32f08d` | 管理员退出数据面 + 白名单精确匹配 + 畸形响应可转移 |
| `89c6ad4` | 流式 200 误报 + 总请求预算 + 管理面能力 |
| `2e8430f` | 侧边栏重排 + 用户分组合并 + 钱包页 + 调用历史 |
| `86181d9` | rosetta v1.0.1 + CI 竞态门禁 + 发布测试门禁 + gateway.ps1 |
| `21a34f0` | 前端产物同步（CI 硬要求） |
| `60ad30b` | 权限真实进程探针 |
| `361c149` | 审计报告 + 交接文档 |

### 关键决策（别推翻）

- **扣费失败不重试** —— 防重复收费
- **流式解不开不转移** —— 防重放已交付输出、重复计费
- **批量扣费不做** —— 实测只值 12%，折叠幂等键风险高
- **改钱单位不做** —— 实测语句次数不变，性能无收益；用户负载仅 0.049 req/s
- **管理员不调 API** —— 控制面/数据面分离，只负责管理网关

## 三、**未修的 5 个已知缺口**（不是遗漏，是决定或待办）

1. **欠费时余数被回滚丢弃** —— 缺功能，注释已如实标注（勿再写「余数照留」）
2. 落库与扣费两个事务 —— SIGKILL 会丢一次扣费
3. `usage_records.user_id` 在 key 改归属后漂移 —— 无对账
4. 剪枝水位非单调 —— 长期运行才显现
5. 批量扣费 —— 建议不做

## 四、发布剩余步骤

1. ✅ 修正矛盾注释
2. ✅ 分批提交
3. ✅ 版本号 1.5.0
4. ✅ **确认 rosetta v1.0.1 能被正常校验拉取**（已用全新 GOMODCACHE + 无关消费者模块验证，sumdb 已同步，不再需要 `GOSUMDB=off`）
5. ⏭️ **推送 + CI 首次运行**；CI 绿了再打 tag `v1.5.0`

## 五、⚠️ 唯一剩余风险

**CI 从未真正跑过。** 我只在本地验证过（`build`/`vet`/`gofmt`/全量测试/`-race` 全绿，
CI 三个门禁本地对照通过）。`docker.yml` 的测试门禁是本轮新加的，**首次执行就在发版时**。
若 CI 红，先看是不是 `-race` 需要额外环境（见下）。

## 六、环境事实（省去重复摸索）

### 测试

```bash
go build ./... && go vet ./... && gofmt -l cmd internal   # 全绿
go test -count=1 ./...        # 16 包全绿
CC=C:/mingw64/bin/gcc.exe go test -race -count=1 ./...   # 竞态检测（本机必须带 CC）
```

**`-race` 必须带 `CC=C:/mingw64/bin/gcc.exe`** —— 默认 PATH 上的 MinGW 在
`C:\Program Files\mingw64`，路径含空格，链接器解析不了。这是本轮发现的关键事实。
（CI 是 ubuntu，用默认 gcc 即可。）

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
- 注释里的数字**引用常量**而非重述（如 `MicrosPerCent`）——本轮踩过：
  注释写「100_000 微元 = 1 分」而常量是 10_000
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