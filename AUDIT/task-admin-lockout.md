# 管理员数据面锁定（控制面 / 数据面分离）

> 需求原话：「管理员账号不能调用模型，不能创建key，管理员需要新建普通用户账户来调用API，
> 这样的逻辑，管理员账号只负责管理网关。」
>
> 实现日期：2026-10 ｜ 分支工作区：未提交（按要求不执行 `git commit`）

---

## 0. TL;DR

| 项 | 结果 |
|---|---|
| 管理员调 `/v1` | **403** + `code=admin_cannot_call_model` + `type=permission_error` |
| 管理员建 key | **403**（路由层中间件），库里不落任何数据 |
| 拦截落点 | `internal/auth.Authenticate`（**所有**数据面入口统一生效） |
| 存量管理员 key | **立即失效**（fail-fast）+ 清晰错误消息 |
| 改了 `internal/admin/` / `web/` / `internal/store/` 吗 | **没有** |
| 硬性检查 | `go build` / `go test`（14 包全绿）/ `go vet` / `gofmt` 全部通过 |

---

## 1. 鉴权链路：先查清再动手

任务要求「先查清鉴权链路再动手」。结论：

```
HTTP 请求
  └─ internal/auth.Authenticate()      ← 唯一的 key 认证点
       ├─ extractKey()                  Authorization: Bearer / X-Api-Key（刻意不支持 ?key=）
       ├─ sha256 → snapshot.KeysByHash  O(1) 索引查找
       ├─ enabled / expires_at / allowed_ips 检查
       ├─ snap.UsersByID[matched.UserID] ← 取归属用户
       └─ **本次新增**：u.Role == admin → ErrAdminCannotCallModel
```

**key 上有 UserID 吗？** 有。`access_keys.user_id` 经快照下发到 `KeySnapshot.UserID`，
**热路径保证非空**（为空/查不到用户 → `ErrKeyUnowned`，401）。

**能不能在这一层就拒绝管理员？** 能，而且是**唯一正确的落点** —— 见下。

### 为什么拦截必须收敛在 `Authenticate`，而不是 `handleIngress` 加一道检查

数据面有**多条**入口，全部只做 `auth.Authenticate`：

| 入口 | 注册位置 |
|---|---|
| `POST /v1/chat/completions`、`/v1/messages`、`/v1/responses` | `handleIngress` |
| `GET /v1/models`、`/v1/models/{model}`（三种协议别名） | `handleListModels` / `handleGetModel` |
| `/dashboard/billing/subscription`、`/v1/organization/costs`、`/v1/organization/usage/completions` | `billing.go` |

若只在 `handleIngress` 加检查，只覆盖 3 个推理端点，其余入口各自漂移。
**漂移的方向恰恰最危险**：管理员旧 key 在 `/v1/models`、billing 查询上照常通过
（billing 本就用 admin 看全量，那是**控制面**语义），只有推理端点被挡 ——
「管理员不能进数据面」这条要求实际没成立，只是没被发现。

收敛到 `Authenticate` 之后，**任何**新增数据面端点只要走鉴权就自动继承这条策略，
不需要记得再写一遍。

---

## 2. 关键设计判断：状态码与错误消息

### 2.1 选 403（我按要求自行判断，理由如下）

| 候选 | 结论 | 理由 |
|---|---|---|
| **401** | ❌ | 语义是「凭据无效，请换一把 key」。但这里**key 和账号都是好的**，只是角色不允许。换多少把管理员的 key 都没用。回 401 会让 SDK 提示「重新配置 key」（甚至自动换 key 重试）、让运维以为 key 损坏/过期而白查一圈。 |
| **403** | ✅ **采用** | 语义精确：「明确知道你是谁，但不允许」。与本仓库既有 `key_disabled` / `key_expired` / `ip_not_allowed` / `model_not_allowed` **完全一致**（都是 403 + 独立 code），不引入新的状态码方言。 |
| **404** | ❌ | 隐藏存在性能减少信息泄露，但**这里没有可枚举的攻击面** —— 调用方本来��持有这把 key 的明文，404 藏不住任何东西。代价却是管理员看到「key 不存在」，完全不知道发生了角色策略变更，与「给清晰消息」的需求正相反。 |

### 2.2 存量管理员 key：立即失效（我同意 Lead 的倾向，并确认没有更安全的做法）

- **不保留旧 key 可用**。保留等于这条「要求」被架空：存量 key 会继续消耗上游额度，
  且（改动前）还不受余额约束，同时管理员绕过控制面直接吃数据面。
- **不用 404 藏起来**（理由同上：藏不住 + 失去清晰消息）。

### 2.3 `type` 字段可区分 —— 直接对应需求里「SDK 不要无限重试」

`internal/outwire/errorTypeFromCode` 新增一条映射：

```go
case "admin_cannot_call_model":
    return "permission_error"
```

**为什么不是 `authentication_error`**：那正是这个 switch 存在的核心诉求
（见其函数头注释「让客户端能按类型决策重试/退避」）。`authentication_error` 的标准
处置是「换一把 key / 重新认证」，而这里换一万把管理员的 key 也一样不通 ——
客户端只会无限重试并刷满日志。`permission_error` 表达的是**终态的权限边界**：
不要重试，请改权限。正确动作就是错误消息里那句话：去建普通用户。

**没有用现成的 `invalid_api_key` code**：客户端必须能把「角色不允许」与「凭据坏了」
分开 —— 这两者的处置动作完全不同（建用户 vs 换 key）。

---

## 3. 逐项改动清单

### 3.1 `internal/auth/errors.go` —— 新增错误

新增 `ErrAdminCannotCallModel`，注释里写清了**为什么与 `ErrKeyDisabled` /
`ErrUserDisabled` 分开**（三者处置动作不同，塌成一类会让管理员以为 key/账号坏了而
白排查），以及「存量 key 立即失效」的取舍。

### 3.2 `internal/auth/auth.go` —— 拦截 + 一个复用判据

- 新增 `const userRoleAdmin = "admin"`：与 `store.RoleAdmin` 字面量一致。
  **刻意不 import store**，理由与既有 `UserStatusActive` 完全相同 ——
  否则会把数据库驱动、事务、全部 DAO 拖进鉴权热路径的依赖图。
- 在 `Authenticate` 里，**「用户存在」→「用户启用」之后**插入角色判定。

**为什么排在这两处之后**：无归属/已禁用应当先报自己的错。管理员账号被禁用时
应报「用户已禁用」而不是「管理员不能调模型」——处置动作完全不同。
（已有专门用例 `TestAuthenticate_DisabledAdminReportsDisabled` 钉住。）

**为什么判「归属用户角色」而不是「key 上打个管理员标记」**：
角色是 `users` 表的权威事实，经快照下发、热路径零查库；且管理员把自己降级为
`user` 后，下一次快照重建就**自动恢复可调用**，不需要手工清理任何 key 标记。
（用例 `TestAuthenticate_DemotedAdminCanCallAgain` 钉住。）

- 新增 `Context.IsAdminOwner()`：把「这个 key 的归属用户是不是管理员」这个判据
  变成**单一实现**，供已鉴权成功的调用方复用。

### 3.3 `cmd/gateway/main.go`

- `writeAuthError` 新增 `ErrAdminCannotCallModel` 分支 → 403 + `admin_cannot_call_model`
  + 指向正确出路的英文消息（数据面协议是英文，前端/管理面才是中文）。
- `usageRecorder.charge` 的管理员短路注释更新（逻辑未变，见 §4）。
- **新增 `denyAdminKeyCreate` 中间件**，并把 `POST /admin/api/keys` 的注册包一层。

### 3.4 `internal/outwire/errors.go` —— 一条 type 映射

> ⚠️ **这个文件不在我声明的写权限清单内**，但需求明确要求「错误消息要用 `type`
> 字段可区分的分类（见 `internal/outwire/errors.go` 的 `errorTypeFromCode`）」。
> 因此**只加了本需求必需的那一条 case**，没有顺手改任何其它分支。改动量：+15 行。

### 3.5 `cmd/gateway/billing.go` —— 顺带去重

`orgCostsScope` 原本自己内联了一份「查快照判 admin」的逻辑，现改为调用
`authCtx.IsAdminOwner()`。理由：管理员判定在数据面有两处（鉴权拦截 / org 可见范围），
**同一判据必须只有一份实现**，否则将来很容易各改各的、悄悄漂移成
「拦截生效了但 org 端点没收窄」这类极难查的不一致。

---

## 4. 任务 3：余额/计费的连带影响

### 4.1 两处短路**保留**（采纳 Lead 倾向，理由如下）

| 位置 | 新语义下是否可达 | 处理 |
|---|---|---|
| `precheckBalance` 的 `if balanceExempt(authCtx.UserID) { return true }` | **否**（鉴权已拦） | **保留** |
| `usageRecorder.charge` 的 `if balanceExempt(rec.UserID) { return }` | **否**（压根不会有管理员 usage 落库） | **保留** |

**理由**：
1. 余额是**钱**。删掉换来的只是一点点「整洁」，却让资金正确性依赖一个**跨文件假设**
   「auth 那一层一定会先拦住」。将来某个入口绕过鉴权直达标费/预检（重构、新增数据面
   端点、内部复用）时，这条短路能立刻把管理员挡在「不读库、不计费」之外。
   失败模式从「静默超支/扣错钱」变成「被短路挡住」—— 方向明显更好。
2. 代价是**零**：`balanceExempt` 只读快照、不查库，且只在命中 admin 时 early-return，
   普通用户走原路径，**每个普通用户请求的开销不变**。

### 4.2 注释已同步

原来描述「管理员完全跳过（Lead 与用户确认）」的那几段注释已改写为
「**纵深防御，数据面上不可达**」，并写明为什么留。同时明确标注：

> 别因为数据面管理员被拦就把 `orgCostsScope` 一起关掉 ——
> 那里判 admin 是**控制面**语义（管理员在管理端看全局账），不走 `Authenticate`，**仍然可达**。

### 4.3 改了一个既有测试（必要，需 Lead 知悉）

`cmd/gateway/balance_test.go` 的 `TestBalance_AdminIsExempt` 断言的是
「管理员调 /v1 返回 200 且不扣费」—— 这正是本次需求要**废除**的行为，
留着必然红。已改写为 `TestBalance_AdminCannotReachDataPlane`：
断言管理员 → 403 `admin_cannot_call_model`，**且上游一次都没被触碰**。

> 这是我唯一改动的、非 `*_adminlock_test.go` 的既有文件。改的是测试函数体，
> 未动任何被测生产代码；不这样处理则 `go test ./...` 无法全绿。

---

## 5. 任务 2：管理员不能建 key

### 5.1 在**路由/中间件层**解决，`internal/admin/` 一行未改 ✅

`cmd/gateway/main.go`：

```go
adminMux.HandleFunc("POST /admin/api/keys", denyAdminKeyCreate(keyHandler.Create))
```

**理由**：
1. 这是**路由级策略**，只针对一个端点。写进 handler 会与 `Create` 内部那一大段
   「自助建 key 的额度封顶」逻辑缠在一起 —— 而那段的语义（谁能建、建出来多少额度）
   与本策略（谁**不允许**建）其实是两件事，混在一起反而更难读。
2. 需要的鉴权上下文在这一层**本来就有**：`AdminGateGuard`/`UserAuth` 已把登录用户
   注入 context，路由层读它即可，**无需查库**。
3. `Create` 保持「普通用户自助」原样不动，回归风险最低。

**换取的约束（已写进注释）**：本策略绑定在「main.go 注册的这条路由」上。
若将来另有代码用 `KeyHandler.Create` 组新路由，**必须**同样套上本包装。

### 5.2 身份缺失一律 401（fail-closed）

`u == nil` 或 `u.ID == ""` → 401，**绝不**等于「当作普通用户放行」。
后者等于给匿名请求开了一条建 key 的路。空 ID 在统一认证后已无构造路径，
但它一旦可达就是**静默提权**（空 ID 常被当作 admin）。

### 5.3 管理员仍保有对存量 key 的**完整治理权**

本策略只拦「新建」这一个动作。管理员照旧可以：列出全部 key、把已有 key **认领**给
某个用户（`PATCH` 的 `user_id` 路径）、禁用/启用/改配额/改分组覆盖……
这正是需求要的「管理员只负责管理网关」。

---

## 6. 安全问题与不合理之处（已按要求指出）

### 6.1 ⚠️ 存量管理员 key 会**立刻全部失效** —— 这是有意的破坏性变更

升级瞬间，所有管理员手上的 key 全部返回 403。若管理员是用它跑
ChatGPT-Next-Web / LobeChat / 内部脚本，症状是「突然全挂」。

**缓解**：403 的 message 与 code 明确指向「建普通用户」，不是含糊的 403。
**请用户确认**：是否需要在 `/admin` 界面上主动提示这条策略变更（前端不在我范围内）。

### 6.2 ⚠️ 管理员仍可把 key **认领给自己**，造出一把「死 key」

`PATCH /admin/api/keys/{id}` 带 `{"user_id":"<自己>"}` 仍可执行
（`key_handler.Update` 的认领路径，我没改、也不该改 —— 它是控制面治理能力）。
结果是一把**归属管理员**的 key，它在数据面被 §3.2 的鉴权拦截挡住。

**这不是安全漏洞**（鉴权层兜住了，没有绕过），但会**造出无法使用的死数据**，
症状与 §6.1 一样。**建议**（需前端/管理面配合，未实现）：
在 key 列表/认领下拉框里过滤掉 admin 用户，或对「认领给 admin」直接 400。

### 6.3 `errorTypeFromCode` 的**既有**缺口（非本次引入，仅报告）

`key_expired` / `ip_not_allowed` / `model_not_allowed` 三个 code **没有**对应 case，
全部塌进 `default` → `type=api_error`。按该函数自己的注释，
这三个恰恰是客户端最需要区分、且**不该重试**的终态错误。

**我刻意没顺手改**：超出本任务范围，且会让本次 diff 的边界变模糊。
**建议**单独一条小改动把它们一并映射到 `permission_error` 等。

### 6.4 「管理员不得调用模型」不影响**控制面**语义

`orgCostsScope` 仍让管理员看全量费用/用量 —— 这是**控制面**（管理端看全局账），
不走数据面鉴权，与本需求不冲突。已在 §4.2 明确注释，避免后人误关。

### 6.5 关于「测试里 `hits == 0` 的假绿」

自己写第一版测试时踩到：`pricedHarness` 内部自带 `fakeGood`，
我在外面挂的计数上游压根没被调用，`hits == 0` 因**错误的原因**通过。
已改为用 `buildHarness` 把计数上游真正挂到链上。
这一点也写进了用例注释 —— 「断言『没被碰』」本身也需要证明它**能**被碰。

---

## 7. 测试

### `internal/auth/adminlock_test.go`（7 个用例）

| 用例 | 证明什么 |
|---|---|
| `RejectsAdminKey` | 管理员 key 被拒，且**返回 nil Context**（防止调用方误当放行） |
| `NormalUserStillWorks` | **反向**：普通用户照常通过，白名单判定未被破坏 |
| `Context_IsAdminOwnerMatchesLockout` | 判据与拦截**同源**；查不到用户必须判**非**管理员（fail-closed 方向） |
| `DisabledAdminReportsDisabled` | 判定**顺序**：被禁用时先报「用户已禁用」 |
| `UnownedKeyNotMisreportedAsAdmin` | 无归属 key 仍报 `ErrKeyUnowned`（存量数据形态） |
| `DemotedAdminCanCallAgain` | 降级后**无需清理任何 key 标记**即可恢复 |
| `RoleAndStatusLiteralsMatchSnapshotPackage` | 复制字面量不漂移（不同步的症状是**静默**的） |

### `cmd/gateway/adminlock_test.go`（9 个用例 / 11 个断言点）

| 用例 | 证明什么 |
|---|---|
| `AdminCannotCallV1` | 403 + code + **上游 0 次命中**（计数上游真的挂在链上） |
| `ErrorTypeIsTerminalNotAuth` | `type=permission_error`，**不是** `invalid_api_key` |
| `AllIngressProtocolsRejectAdmin` | chat / anthropic / responses **三种协议都被拒** |
| `NormalUserUnaffected` | **反向**：普通用户 200 **且照常扣费**（拦截没误伤计费链路） |
| `QueryEndpointsAlsoRejectAdmin` | billing / org 查询端点口径统一 |
| `AdminCannotCreateKey` | 403 **且库里一把新 key 都没多**（拒绝必须是干净的） |
| `NormalUserCanStillCreateKey` | **反向**：普通用户自助建 key 仍 201 + `plaintext_key` |
| `MissingIdentityIsRejected` | 无身份 → 401（fail-closed） |
| `EmptyIDIdentityIsRejected` | 空 ID → 401，且**断言请求未透传**到下游 handler |

每个行为改动都有「管理员被拒」与「普通用户不受影响」**两个方向**的用例。

---

## 8. 验证结果

```
go build ./...              ✅ 无输出
go vet ./...                ✅ 无输出
gofmt -l cmd internal       ✅ 无输出
go test ./... -count=1      ✅ 14/14 包全绿
go test ./cmd/gateway ./internal/auth -count=3   ✅ 稳定无 flake
```

```
ok  github.com/cn-maul/rosetta-gateway/cmd/gateway        12.499s
ok  github.com/cn-maul/rosetta-gateway/internal/admin      14.244s
ok  github.com/cn-maul/rosetta-gateway/internal/auth        0.055s
ok  github.com/cn-maul/rosetta-gateway/internal/crypto     0.077s
ok  github.com/cn-maul/rosetta-gateway/internal/inwire     0.104s
ok  github.com/cn-maul/rosetta-gateway/internal/outwire    0.103s
ok  github.com/cn-maul/rosetta-gateway/internal/ratelimit  0.021s
ok  github.com/cn-maul/rosetta-gateway/internal/routing    0.072s
ok  github.com/cn-maul/rosetta-gateway/internal/server     0.114s
ok  github.com/cn-maul/rosetta-gateway/internal/snapshot   0.900s
ok  github.com/cn-maul/rosetta-gateway/internal/store     22.213s
ok  github.com/cn-maul/rosetta-gateway/internal/upstream    0.665s
ok  github.com/cn-maul/rosetta-gateway/internal/userauth   0.390s
ok  github.com/cn-maul/rosetta-gateway/internal/webui      0.029s
```

### ⚠️ 一个**环境**问题（非本次改动引入）

`go test -race` 在本机**任何包都编译失败**，与代码无关：

```
ld.exe: cannot find C:/Program: No such file or directory
```

原因是 mingw 链接器无法处理 `C:\Program Files\mingw64\...` 里的**空格**。
已用未改动的 `internal/routing` 验证过，同样失败 —— 确认为**既有环境问题**。
因此竞态检测本次**未能运行**，建议在 CI（Linux）上补跑。

---

## 9. 存量数据怎么办

**不写迁移脚本。** 判断依据：

1. **鉴权层拦截是零迁移的** —— 存量管理员 key 无需改任何数据就自动失效，
   且失败得很**响**（403 + 清晰消息），不会静默。
2. 自动迁移反而危险：悄悄禁用/删除管理员的 key 会让升级过程**无感知**
   （用户以为升级后能继续用，直到某天调用才发现），比「升级即失效 + 明确报错」
   难排查得多。

**建议运维在升级后手工做**（一次性、可控）：

1. 在管理界面 **用户管理** 里建需要调 API 的普通用户。
2. 由该普通用户**自行登录**建 key（管理员不能代建，见 §5）。
3. 把旧 key 从客户端配置里换成新 key（配 `base_url` + 新 key 即可，指向不变）。
4. 旧 key 可**保留在库里**（已被拦截，不会误用），也可让管理员在 key 列表里禁用 ——
   留着更利于回溯「升级前是谁在用什么」。

**若将来希望更彻底**（本次未做，需用户确认）：
启动迁移里把 `access_keys` 中归属 admin 的行置 `enabled=0`，
与既有 `retireOrphanKeys` 的做法同源。**现在不建议做** —— 有了鉴权层拦截，
这一步只多一个「数据看起来干净」的收益，却会失去上面那条「响亮失败」的性质。

---

## 10. 需要用户 / Lead 确认的点

1. **§6.1** 存量管理员 key 升级即全失效 —— 确认接受这个破坏性变更？
   是否需要在 `/admin` 界面加一条主动提示（前端不在我范围内）？
2. **§6.2** 管理员「认领 key 给自己 → 造出死 key」—— 是否要在管理面过滤掉 admin 用户？
   （需要前端 + `internal/admin`，均不在我范围内。）
3. **§3.4** 我改了 `internal/outwire/errors.go`（超出声明写权限，但需求明确指向它），
   只加了本需求必需的 1 条 case —— **请确认这个越界可接受**。
4. **§4.3** 我改了既有的 `cmd/gateway/balance_test.go` 里一个测试函数
   （该文件属他人未提交改动）—— 改动仅限测试函数体，未碰生产代码。
5. **§6.3** `errorTypeFromCode` 的既有缺口（3 个 code 塌成 `api_error`）
   是否要单独一条改动补上？
6. **§8** `-race` 因本机 mingw 路径含空格而无法运行 —— 需要在 CI 补跑吗？

---

## 11. 改动文件总览

| 文件 | 类型 | 权限 |
|---|---|---|
| `internal/auth/auth.go` | 改 | ✅ 在范围内 |
| `internal/auth/errors.go` | 改 | ✅ 在范围内 |
| `internal/auth/adminlock_test.go` | **新增** | ✅ |
| `cmd/gateway/main.go` | 改（3 处：路由注册 / `writeAuthError` / `charge` 注释） | ✅ 在范围内 |
| `cmd/gateway/billing.go` | 改（注释 + `orgCostsScope` 去重） | ✅ 在范围内 |
| `cmd/gateway/adminlock_test.go` | **新增** | ✅ |
| `cmd/gateway/balance_test.go` | 改（1 个测试函数，见 §4.3） | ⚠️ 需确认 |
| `internal/outwire/errors.go` | 改（+1 个 case，见 §3.4） | ⚠️ 需确认 |
| `internal/admin/`、`web/`、`internal/store/`、`internal/webui/dist` | **未改** | ✅ 严守 |