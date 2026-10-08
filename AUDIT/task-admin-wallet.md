# 任务报告：管理员钱包页改造（去掉管理员余额 / 充值入口收敛）

> 对应用户需求原文：
> *「admin账户完全去掉普通用户的余额功能，而是换成用户消费金额、充值记录，不要放消费记录，admin的钱包和充值页面新增一个充值按钮，点击之后可以选择用户填写金额进行充值，用户列表的充值按钮取消。管理员的充值和余额、API调用全去掉，管理员账户只有一个，只能管理网关。」*
>
> 其中「API 调用」已由另一人完成（不在本任务范围）。

**验证状态（全部通过）**

| 检查 | 结果 |
|---|---|
| `go build ./...` | exit 0 |
| `go vet ./...` | exit 0 |
| `go test ./...` | exit 0（14 个包全 ok） |
| `gofmt -l cmd internal` | 无输出 |
| `vue-tsc --noEmit` | exit 0 |
| `vite build` | exit 0 |

未跑 `sync-embed.mjs`，未 `git commit`，未回退任何已有改动。

---

## 一、逐条需求的实现

### 需求 1：管理员账户不再有余额

做了**三处**后端收口，都在「唯一的构造点」上，而不是各 handler 里手改：

1. **`GET /admin/api/me`**（`internal/admin/user_handler.go`，`Me` handler）
   管理员走**独立分支直接短路**返回，不调用 `BalanceOf`。返回 `balance_cents: 0` / `balance_unlimited: true` / `balance_remainder: 0`。
   选 `unlimited: true` 而不是「不返回该字段」或「置 0 + 不限额」的第三种组合，理由：前端 `fmtBalance(cents, unlimited)` 见到 `unlimited` 就渲染「不限」，这是现有展示层唯一能表达「不适用/不参与」的既有语义；而 `false` + `0` 会渲染成「0.00 元」加一个「已用尽」红标 —— 那是**凭空捏造的账目告警**。

2. **`toUserResponse`**（`internal/admin/user_admin_handler.go`）
   这是 `ListUsers` 与 `AdjustBalance` 共用的**唯一** DTO 构造点，在函数开头对 `role == admin` 整体返回归一值。收在这一处的理由写在注释里：漏一处就会出现「用户列表里是 0、充值响应里是真余额」这种同一账号两个答案的矛盾。

3. **引导态合成 user**（`Me` 的 `u.ID == ""` 分支）
   顺手补了 `Unlimited: true` —— 它原来吃 Go 零值 `false`，会渲染成「0.00 元 / 已用尽」。这个分支不可达，但**修好它成本为零**，而留着就是一条「哪天空 ID 变可达就显示假告警」的隐患。

**没有破坏普通用户路径**：`Me` 里新增的分支是 `if u.IsAdmin()` 提前 return，普通用户继续走原来的 `BalanceOf` + 余数逻辑，一行未改。回归由 `internal/admin` 的既有测试覆盖（全绿）。

### 需求 2：管理员钱包页换成「用户消费金额」+「充值记录」，**不要**消耗记录

`web/src/views/Wallet.vue` 按身份分两形态（详见第三节）。管理员形态：

- **顶部卡**：`用户消费金额`（全站）+ Token 消耗 / 调用次数 / `管理员余额：不适用` 三个佐证量。
- **「消费汇总」页签**：一张区间汇总表（金额/次数/Token/输入/输出/缓存/失败）+ 口径说明 + 一个**指向「调用历史」页的明细入口**。
  明细入口是刻意给的：用户要求「不要放消费记录」，但完全不给入口会让管理员**从此没有任何地方能看到明细**。调用历史页本来就是 admin 导航区里的页面，直接链接即可，不复制筛选器。
- **不含**任何逐条消耗表格。管理员路径连 `api.usageHistory()` 都不发（那是一次实打实的表扫描）。

### 需求 3：钱包页新增充值按钮（选用户 → 填金额 → 充值）

页头新增 `充值` 按钮（仅管理员渲染），点开 `AppModal`：
下拉选用户 → 填金额 → 可选备注 → 确定。

- 下拉候选用 `topupCandidates`，**滤掉了 `role === 'admin'`**。后端 `AdjustBalance` 拒绝给自己充值，把管理员列进下拉就是让管理员选中自己然后吃一个 400 —— 界面自己造出来的死路。
- 每项后面带**现余额**（`fmtBalance`）：管理员能看着自己正在动谁的账。
- 保留了原弹窗里两条必须写在界面上的提醒：「相对调整」语义、给**不限额**用户充值会把它切成有限额。
- 金额预览沿用原逻辑，`yuanToCents` 逐字搬到本页。
- 用户清单**打开弹窗时才拉**（`openTopup`），不随页面预取 —— `api.users()` 不便宜（`ListUsers` 对每个用户各跑一次 `SumUserUsedTokens` 与 `ListAccessKeysByUser`），为偶尔打开的弹窗每次进页面都付一次不划算。

### 需求 4：用户列表的「充值」按钮取消

见第四节（删了什么 / 留了什么 / 为什么）。

### 需求 5：管理员不能给自己充值（后端拦截）

`AdjustBalance` 开头：

```go
self, ok := requireAdmin(w, r)
...
if id == "" || self.ID == id {
    writeError(w, http.StatusBadRequest,
        "管理员账户不能充值：管理员不参与计费，请为普通用户账户充值")
    return
}
```

**在后端而不是只在前端藏按钮**，三条理由（写在代码注释里）：① `requireAdmin` 已经返回了当前管理员，`self` 在手边，判定免费；② 这是**权限边界**而非界面偏好 —— 前端隐藏对 curl / SDK / 旧版前端一律无效，而 `PUT /users/{id}/balance` 是公开管理端点，管理员 id 完全可以自己构造；③ 一旦 admin 有了余额，它就会出现在对所有管理员可见的「用户」列表里，界面上出现「给自己充值」这一格，而那笔账没有任何对应消费，是纯噪声流水。

比的是 **id** 而不是用户名（`username` 是 `COLLATE NOCASE UNIQUE`，同名不可能并存，但 id 比较不依赖任何字符串口径）。空 id 的引导态合成 user 同样被拦下。

顺带清了一处重复：`AdjustBalance` 末尾原本又从 context 取了一次 `self`（`requireAdmin` 读的就是同一个），改为复用开头那个。

---

## 二、需要 Lead 决策的点

### 🔴 决策 1（阻塞需求 2）：`/admin/api/topups` 对管理员的**作用域**与需求相反

**data-auditor 的后端已落盘**（`internal/store/topup_dao.go`、`internal/admin/user_admin_handler.go:758` 的 `ListTopups`）。我核对了实际实现，它与数据表/端点形状一致，但**作用域**与用户需求相反：

```go
// ListTopups：作用域由会话身份决定，不接受请求参数指定查谁
//   - 普通用户 → 只能看到自己的；
//   - 管理员   → 看自己的那一份，而不是全站。
...
list, err := h.store.ListTopupsByUser(ctx, me.ID, limit, offset)
```

配套测试 `TestTopupHandler_AdminAlsoSeesOnlyOwn`（`internal/admin/topup_handler_test.go:190`）把这个行为**钉住了**，注释理由是「钱包页是个人账本页，不存在『看全站充值』这个需求」。

**冲突点**：用户需求明确说「充值记录」要在管理员钱包页展示，而管理员现在调这个端点拿到的是**恒空数组** —— 因为管理员不能被充值（需求 5 恰好是我这次加的拦截），所以 `me.ID` 名下永远不会有流水。**照现状联调，管理员的「充值记录」表永远是空的。**

三条路，请 Lead 选一条：

| 方案 | 做法 | 代价 |
|---|---|---|
| **A（推荐）** | 扩 `ListTopups`：管理员不加 `user_id` 过滤（返回全站），普通用户维持收窄 | 需改 `ListTopupsByUser`→新增 `ListAllTopups`，并**改掉 `TestTopupHandler_AdminAlsoSeesOnlyOwn`**（它现在断言相反）。前端零改动 |
| B | 保留现状，管理员钱包页的充值记录改用别的端点 | 需要第二个端点，且 data-auditor 的设计意图（「不存在这个需求」）被推翻 |
| C | 保留现状，管理员这一格先空着 | 需求 2 的「充值记录」实质未交付 |

**我没有自行改**：这是服务端作用域语义变更 + 需要推翻别人已钉住的测试，属于跨任务决策，且明确超出了「本任务」给我的 write 边界（用户原话只要求我改 `AdjustBalance` 与 `/me`）。

**已确认无阻塞的部分**：接口**形状**我按实际实现锁定，联调可直接跑通（见第三节末）。

### 🟡 决策 2：`/admin/api/topups` 的命名与路由位置

端点挂在 `adminMux` 上，但 `internal/server/user_auth.go` 把 `/admin/api/topups` 加进了 `userAccessiblePrefixes`（data-auditor 改的），所以普通用户可达、且拿自己的流水 —— 这个设计是对的（普通用户钱包页要看「我的充值记录」）。

但代价是：`GET /admin/api/topups` 成了普通用户可访问前缀下的端点，与「admin-only 前缀白名单」的既有原则不同（该仓库对 `usage/prune` 的处理是「白名单管不到，靠 handler 内 `requireAdmin` 把关」）。这里不需要 admin 权限所以没问题，只是**命名上**它其实是「我的充值记录 / 全站充值记录」，叫 `/topups` 略宽。无需改动，仅记录以免后来者困惑。

### 🟡 决策 3：管理员「用户消费金额」的口径确认

见第三节 (a) 的决策说明。已按「复用 stats」实现，如 Lead 认为需要一个带用户维度拆分的汇总（例如「每个用户各花了多少」），那是另一个量级的工作，需要单独排期。

---

## 三、两个设计问题的判断

### (a) 「用户消费金额」从哪来 → **复用 `GET /admin/api/stats`，不新增端点**

理由三条：

1. **语义正好对上**：`stats` 走 `callerScope`，对管理员返回**空串**（= 不加作用域过滤），所以 `stats.cost` 就是「所有用户的消费合计」—— 正是这一格要的数字。字段、接口、口径全部现成。
2. **不能重新实现聚合**：`stats` 用的是 `store.UsageSource`（明细 ∪ 日归档）的合并查询，这是「30 天明细被剪枝清掉后总消费**不缩水**」的关键。自己写一个「用户消费汇总」端点等于把这套口径复制一份，两份实现迟早在剪枝语义上分叉 —— 而那个分叉的表现是「总览说花了 1000，钱包说花了 800，且没人知道该信哪个」。
3. **重复调用是否有必要 → 没有**。管理员总览页本来就在拉同一个 `stats`，本页再调一次是同一段查询；真要省，得把结果提到全局缓存，那是另一件事（且要处理剪枝后的失效）。

补充：费用是**落库当时**按单价算好后固化的 `cost_total` 求和，改价不改写历史 —— 这正是「记账」该有的性质，也让它可以放心当「累计消费」展示。

### (b) 「充值记录」接口形状 → 已按 data-auditor 的**实际实现**锁定，不是我的推测

任务书让我「若未落盘就按推测的形状写，并在报告里写明假设」。实际情况是**已落盘**，所以我核对并对齐了真实实现，逐字对齐 `internal/store/topup_dao.go` 里 `store.Topup` 的 json tag：

```
GET /admin/api/topups?limit=50&offset=0
→ { records: [{ id, user_id, delta_cents, balance_after, was_unlimited,
                operator_id, operator_username, ts, remark, username }],
    total: number }
```

与我最初推测形状的差异（已修正进 `web/src/types.ts`）：

| 我最初写的 | 实际 | 处理 |
|---|---|---|
| 无 `operator_id` | 有 | 已补（`operator_id` 是**被充值者**之外的操作者 id，与 `user_id` 分列） |
| `total` = 分页总数 | 一致 | 保持 |
| 其余字段 | 一致 | 逐字对齐 |

`limit` 后端钳制在 50（默认）/200（上限），前端取 50。

**一处前端与后端的语义分歧（已在代码注释标注）**：`was_unlimited` 的后端语义是「本次是否把一个不限额用户切成了有限额」（即**状态跃迁**，不是「调整前是否不限额」）。所以我在表格里没有给它单独一列 —— 单看 `balance_after` 无从判断跃迁，但把 `was_unlimited` 渲染成一个列会让管理员误以为它在描述常态。建议**等 Lead 决策 1 落地后**，在表格里对 `was_unlimited === true` 的行加一个「由不限额转为有限额」的行内标记（现在暂时只体现在备注列的语义里）。这是可以后续补的小改动，不阻塞本任务。

---

## 四、`UsersGroups.vue`：删了什么、留了什么、为什么

**删掉**：

1. 用户行的「充值」按钮（模板里那一行）。
2. `balFor` / `balAmount` / `balErr` / `balBusy` 四个状态。
3. `openBalance` / `yuanToCents` / `submitBalance` 三个函数。
4. 整个充值 `AppModal`。

**保留**：

- **`pending` / `isPending` / `setPending`** —— 这是**共享**的防连点机制，禁用（`toggleStatus`）与删除（`askDelete`）都还在用。拆掉它会让剩下两个写操作失去连点保护。原先 `submitBalance` 也用它（注释里专门解释过为什么充值要挂这个 Map 而不是另起一个标志位），所以我核对过调用点才决定保留。
- `fmtBalance` / `fmtRemainder` 的 import —— 用户表格里的**余额只读列**还在用。
- 用户列表里的**余额展示列**。充值入口走了，但「这个人还有多少钱」是管理员唯一要看的数字，与入口在不在无关。顺带：后端 `toUserResponse` 现在对 admin 行返回归一值，所以管理员自己在列表里那行会显示「不限」而不是一个从不消耗的数字。

**为什么整块删而不是留成不可达代码**：删掉按钮后那些函数**只**被充值弹窗使用，而钱包页另做了一套充值 UI —— 留着就是纯死代码。死代码的实际危害不是占体积，而是让下一个改这一页的人以为「本页也能充值」，重新接回一个已被产品取消的入口。两处注释都写明了「已移走 + 搬去了哪」，`yuanToCents` 还额外说明了为什么不做成公共 util（两处各留一份会让「换一个换算口径」的修改只改到其中一处，同一笔充值在两个界面上差一分钱）。

---

## 五、改动文件清单

| 文件 | 改动 | 备注 |
|---|---|---|
| `internal/admin/user_handler.go` | `Me` 加 admin 短路分支；引导态补 `Unlimited: true` | 与 data-auditor 共用文件，**只加分支不改原逻辑** |
| `internal/admin/user_admin_handler.go` | `toUserResponse` 加 admin 归一；`AdjustBalance` 加自充值拦截 + 去掉末尾重复取 `self` | **共用文件**，改动写在函数头部/独立分支，未触碰他人逻辑 |
| `web/src/types.ts` | 新增 `TopupRecord` / `TopupPage` | 按实际实现逐字对齐，含作用域待决注释 |
| `web/src/api.ts` | 新增 `api.topups(limit, offset)` | 含口径待决注释 |
| `web/src/views/Wallet.vue` | 大改：双身份形态 + 充值弹窗 + 充值记录表 | |
| `web/src/views/UsersGroups.vue` | 删充值入口及其死代码，保留余额只读列 | |

**与他人改动的交界**：`internal/admin/user_admin_handler.go` 与 data-auditor 共用（他加的 `BalanceRemainder` 字段与 `ListTopups` 都在同一文件）。我的两处改动分别落在 `toUserResponse` 函数头与 `AdjustBalance` 函数头，**不与他的代码行重叠**；`ListTopups` 我一行未动。`internal/store/` 与 `internal/admin/key_handler.go` 我完全没碰。