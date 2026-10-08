# 调用历史表格四项改造 — 实施报告

**任务**：调用历史新增「平均 token 速度(t/s)」列、合并「首字/总耗时」、改表头、状态列移到费用后。

**改动文件**（严格限于授权范围）
| 文件 | 改动 |
|---|---|
| `internal/admin/usage_handler.go` | 新增 `tpsFor`、`OutputTokens`/`Tps` 字段、SELECT 补列、CSV 补两列 |
| `web/src/views/History.vue` | 新增 `fmtTps` / `fmtTtfbOverLatency`、列顺序调整 |
| `web/src/types.ts` | **仅** `UsageHistoryEntry` 追加两个字段（追加式，未动其它接口） |
| `internal/admin/usage_history_tps_test.go` | 新建，7 个测试 |
| `AUDIT/task-history-tps.md` | 本文件 |

未改动任何其它文件，未跑 `sync-embed.mjs`，未 `git commit`。

---

## 一、t/s 放在前端还是后端算 → **放在后端**

**结论**：后端算，新增 `tps` 字段下发；同时下发 `output_tokens`（两者都要，理由见下）。

**理由（按重要性排序）**

1. **公式只能有一处实现。** 总览页的「平均速度」读的是
   `store.GetRecentThroughput`（`internal/store/usage_dao.go:341`）：

   ```sql
   SELECT COALESCE(SUM(output_tokens) * 1000.0 / NULLIF(SUM(latency_ms), 0), 0)
   ```

   单条记录下两个 `SUM` 各自退化，等价于 `output_tokens * 1000 / latency_ms`。
   这条公式放在后端、紧挨 `tpsFor` 的注释，全项目**只有一处**。
   若放前端，同一公式会同时存在于 Go/SQL 与 TypeScript 两边，改一处忘一处时
   两个页面静默分叉 —— 而对不上时没人知道该信哪个。这正是项目一贯反对的
   「同一件事有两个可以各自忘记的实现」。

2. **除零边界只需守一处。** `NULLIF(...,0)` 那半边保护只存在于后端。
   前端复刻就要再写一次 `latency_ms > 0` 判断。

3. **`+Inf` 会把整个响应打坏，不只是某一格显示错。** 这点实测过
   （见下面第四节），未守卫的除法产出 `+Inf`，`encoding/json` **拒绝**序列化它。

**为什么同时保留 `output_tokens` 下发**：CSV 导出与第三方调用方需要能自己复现
这一列；只给算好的 `tps` 就等于把公式锁死在网关里，拿出去的数据无法独立验证。

**口径的已知性质（不是缺陷，写进注释了）**：分母用 `latency_ms` —— **总耗时**，
含首字延迟与输入处理时间，不是纯解码时长。所以这个数系统性略低于「吐字速率」。
保持它是对的：改成分母只取 `latency - ttfb` 会让本列与总览页立刻分叉，
而分叉的代价大于「略保守」这点偏差。

**实现**（`internal/admin/usage_handler.go:469`）：

```go
func tpsFor(outputTokens, latencyMs int64) float64 {
	if outputTokens <= 0 || latencyMs <= 0 {
		return 0
	}
	return float64(outputTokens) * 1000 / float64(latencyMs)
}
```

**没有把除法写进 SQL**：SQL 里再写一遍除数意味着除零保护也要在 SQL 与 Go
两边各写一份，而这正是上面第 1 条要消灭的东西。所以 `SELECT` 补 `u.output_tokens`
之后在 Go 侧算（`usage_handler.go:591`、`:666`）。

---

## 二、`ttfb_ms = 0` 时怎么显示 → **`—/45.1s`（只让前半格变 `—`）**

**结论**：采纳 Lead 的建议，且我认为这是唯一正确的选择。

**理由**：`ttfb_ms = 0` 是**常态**而不是异常 —— 非流式请求没有独立首字时刻
（响应的 ttfb 与总耗时同源，落库常为 0，`cmd/gateway/main.go` 的
`attemptNonStream` 里 `ttfbMs = latency` 是「近似」，流式才有真正的首字）。
整格显示 `—` 会把**真实存在且运维最关心的总耗时**一起丢掉，
等于这一列白占位置。

两侧都无值时（`latency_ms = 0` 的错误请求，在触碰上游前就返回、没走计时）
自然得到 `—/—`，如实反映「这条记录没有任何计时数据」。

实测输出：

| ttfb_ms | latency_ms | 场景 | 渲染 |
|---|---|---|---|
| 3600 | 45100 | 流式，两者都有 | `3.60s/45.1s` |
| 0 | 45100 | 非流式 | `—/45.1s` |
| 0 | 0 | 错误请求 | `—/—` |
| 1200 | 0 | 理论上只有首字 | `1.20s/—` |

---

## 三、⚠️ 与任务简报不符的两处（已按实际代码处理）

### 3.1 `fmtSec` **不带** `s` 后缀 —— 简报的前提不成立

简报说「注意 `fmtSec` 已经带 `s` 后缀，别再拼一个」。

**实际**（`web/src/fmt.ts:81-84`，含当前未提交改动，已验证）：

```ts
export function fmtSec(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '0'
  return fmtScaled(ms / 1000)   // 裸数字，无单位
}
```

我直接用 node 执行 `fmt.ts` 的原函数体确认：

```
3600  -> "3.60"
45100 -> "45.1"
```

单位**原先写在表头里**（`首字(s)` / `总耗时(s)`），所以函数本身不带。
合并成一格后表头变成 `首字/总耗时`、不含单位，两个数就**必须各自带 `s`** ——
否则一格里的两个数字看不出量纲（是秒？毫秒？）。

所以我在 `History.vue` 里显式拼了 `s`，并把这条理由写进了
`fmtTtfbOverLatency` 的注释。**没有改 `fmt.ts`**（不在我的写权限内；
`fmtSec` 还被 `Providers.vue:561` 与 `Overview.vue:343` 用着，后者自己补
`<span class="unit">s</span>`，改它会波及那两处）。

两种做法都自洽，我选了「不动共享文件」的那种。若 Lead 更希望把单位收进
`fmt.ts`（例如加 `fmtSecUnit`），那是一次跨文件重构，应由 Lead 决定并统一改
`Providers.vue` / `Overview.vue`。

### 3.2 `output_tokens` 是**新增**的，不是「已有」

简报说「`usageHistoryEntry` 现在只有 `total_tokens`」—— 正确。
但顺带说明：`internal/admin/usage_handler.go` 里的 `usageRecordEntry`
（`/usage` 明细端点）**本来就有** `OutputTokens` 字段，只有 history 这条链路缺。
两者是不同的结构体，不要混淆。

---

## 四、我额外发现并修掉的一个真实缺陷：`latency_ms = 0` 会把整张表打坏

这不是「顺手改」，而是本列**必然引入**的新失败模式，不处理就会上线即坏。

**问题**：直接 `output_tokens * 1000 / latency_ms` 在 `latency_ms = 0` 时得到
`+Inf`。而 `writeJSON`（`internal/admin/helpers.go:14-18`）用的是：

```go
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)          // ← 状态码与头已写出
	json.NewEncoder(w).Encode(v)   // ← 在 +Inf 上失败
}
```

`encoding/json` **拒绝**序列化 `+Inf`。实测（独立探针，已删）：

```
300*1000/0 = +Inf
json.Marshal(+Inf) -> bytes=""  err=json: unsupported value: +Inf
```

后果**不是**「某一格显示错」，而是：状态码 200 已发出、响应体**为空**，
前端 `JSON.parse` 拿到空串当场抛错 —— **整张调用历史表打不开**。
而只要库里存在**任意一条** `latency_ms = 0` 的记录就足以触发。

这一条在改造前不存在（原代码没有除法），是新增本列带来的。

**修复**：`tpsFor` 在 `outputTokens <= 0 || latencyMs <= 0` 时返回 0
（与 `GetRecentThroughput` 的 `NULLIF(SUM(latency_ms), 0)` 同一意图）。
**并且我加了专门的回归测试**，且验证过它有牙 —— 把守卫拿掉后：

```
--- FAIL: TestUsage_History_LatencyZeroDoesNotBreakWholeResponse
    响应体为空 —— 说明 Encode 在 +Inf 上失败，整张表打不开
--- FAIL: TestTpsFor_Formula/latency=0_不除零
    tpsFor(300, 0) = +Inf
--- FAIL: TestTpsFor_Formula/两者皆_0
    tpsFor(0, 0) = NaN
```

**注意 `fmtTps` 那层判断也不能省**：`fmtSpeed` 对 `<= 0` 返回的是 `'0'`
而不是 `'—'`，直接用它会把「无样本」印成 `0`，读起来像「这次生成极慢」。

---

## 五、CSV 导出：追加而非插入（一个我主动做的判断）

`ExportCSV` 的既有注释写着「列名与页面表格一一对应：导出件要能独立读懂」，
而页面新增了「速度」列。如果不给，用户拿出去**连自己算都算不了**
（缺 `output_tokens`）—— 那正是该注释要防的事。

**但列位置一动，任何按下标取值的既有消费方（脚本、Excel 模板）会静默错位。**
所以我：

- **追加** `output_tokens`、`tps` 到**末尾**，原有 11 列下标不变；
- 无样本时 `tps` 写**空串而不是 0**：CSV 是拿去做数值分析的，
  空单元格会被读成「缺失」（`AVERAGE` 等自动跳过），而 0 会被算进平均值、
  把整体速度往下拽。与页面显示 `—` 是同一个意图。

这两点各有一条测试钉住（含表头逐字断言）。

---

## 六、最终列顺序（已从构建产物反向确认）

```
时间 / 调用模型 / 实际模型 / 调用密钥 / Tokens / 速度(t/s) / 首字/总耗时 / 费用 / 状态
```

`vite build` 产物里 `<thead>` 的实际渲染调用：

```
"Tokens", "速度(t/s)", "首字/总耗时", "费用", "状态"
```

`<tbody>` 实际渲染调用：

```
ts, public_model, upstream_model, key+流式徽章, total_tokens, fmtTps(tps),
fmtTtfbOverLatency(ttfb_ms, latency_ms), cost, 状态徽章
```

四项需求全部落实。

---

## 七、验证结果（全部 exit 0）

| 检查 | 结果 |
|---|---|
| `go build ./...` | ✅ exit 0 |
| `go vet ./...` | ✅ exit 0 |
| `go test ./...` | ✅ 全部包 ok（含 admin 12.5s、store 19.8s） |
| `gofmt -l cmd internal` | ✅ 我改的两个文件干净 |
| `vue-tsc --noEmit` | ✅ exit 0 |
| `vite build` | ✅ exit 0（66 modules，1.64s） |

**`gofmt -l cmd internal` 现在是完全干净的（输出为空）。**

中途它曾报出 `internal/admin/usage_by_user_handler.go` —— 那是**另一个 teammate 新建的
未跟踪文件**（`git status` 显示 `??`，不在 HEAD 里），**不在我的写权限内**，我没有碰它。
在我完成收尾验证时该文件已由属主格式化，`gofmt -l` 已无输出。
我自己的三个源文件与一个测试文件始终 gofmt-clean（
`gofmt -l internal\admin\usage_handler.go internal\admin\usage_history_tps_test.go`
输出为空）。

**未跑 `sync-embed.mjs`**（按简报由 Lead 统一做）。
`vite build` 我刻意输出到 `$env:TEMP` 而不是 `web/dist`，避免在 Lead 统一同步前
改动 embed 产物 —— 所以 `internal/webui/dist/` 的删除状态（`git status` 里的 `D`）
是工作区既有状态，与本次改动无关。

---

## 八、测试清单（`internal/admin/usage_history_tps_test.go`）

| 测试 | 钉住什么 |
|---|---|
| `TestTpsFor_Formula` | 公式正确；`latency<=0` / `output<=0` 无样本返回 0；**绝不产出 `Inf`/`NaN`/负值** |
| `TestUsage_History_ExposesOutputTokensAndTps` | HTTP 层下发 `output_tokens` 与 `tps`；三种边界各自正确 |
| `TestUsage_History_TpsUsesOutputTokensNotTotal` | **分子必须是 `output_tokens`**：seed 用 input=900/output=200，用错分子会得到 275 而不是 50（差 5.5 倍），断言直接对比两个值 |
| `TestUsage_History_TpsMatchesRecentThroughputAggregate` | **与总览页口径一致**：表格逐条 tps 的耗时加权平均 == `store.GetRecentThroughput` == 手算聚合值 |
| `TestUsage_History_OutputTokensIsPresentEvenWhenZero` | 在**原始 JSON** 上断言键存在（解成 struct 后无法区分「字段缺失」与「字段是 0」，而 0 是「上游未报 usage」这个有意义的状态） |
| `TestUsage_History_LatencyZeroDoesNotBreakWholeResponse` | `latency=0` 的记录**不许**打坏整个响应；响应体非空、无 `Inf`/`NaN` 字面量、三条记录都在 |
| `TestUsage_ExportCSV_IncludesOutputTokensAndTps` | CSV 含两列；表头逐字断言（新增列在**末尾**）；正常样本写 50；无样本**留空不写 0** |

`TestUsage_History_LatencyZeroDoesNotBreakWholeResponse` 与
`TestTpsFor_Formula` 都**验证过「拿掉修国会失败」**（见第四节），不是空转的测试。

---

## 九、给 Lead 的两点提示

1. **`fmtSec` 的单位归属**：我按「不改共享文件」处理，在 `History.vue` 显式拼 `s`。
   若 Lead 想统一到 `fmt.ts`，需要一并改 `Providers.vue:561`、`Overview.vue:343`
   （见第三节 3.1）。这是本次唯一一个「简报前提与实际代码不符」的点，
   我按实际代码做了，没有改超出权限的文件。
2. **`GetRecentThroughput` 与本列的关系**已由测试钉死。若将来有人改动那条约 SQL 的
   公式（例如把分母改成 `latency - ttfb`），
   `TestUsage_History_TpsMatchesRecentThroughputAggregate` 会失败 —— 这是**有意的**，
   因为那正是「两个页面显示不同速度」的入口。
