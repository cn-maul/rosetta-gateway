# 权限边界**动态**验证报告

> 本报告的每一条结论都有**实际执行的命令 + 原始输出**支撑。不做「读代码推断」。
> 未能验证的部分在 §6 明确列出，不用推理补齐。

**验证环境**

| 项 | 值 |
|---|---|
| Go | `C:\Program Files\Go\bin\go.exe` |
| 进程内测试 | `internal/server/authz_matrix_test.go`（package `server_test`） |
| 真实进程探针 | `scripts/authz_probe/probe.ps1` + `scripts/authz_probe/dbdump/` |
| 探针隔离方式 | `ROSETTA_GW_HOME` 重定向整个状态根目录 |

**硬性验证结果（全部通过）**

| 检查 | 命令 | 结果 |
|---|---|---|
| server 包 | `go test ./internal/server/ -count=1` | `ok` (2.1s) |
| 全量 | `go test -count=1 ./...` | 14 个包全 `ok` |
| 格式 | `gofmt -l cmd internal` | 无输出 |
| 静态检查 | `go vet ./...` | exit 0 |
| 真实进程探针 | `pwsh -File scripts/authz_probe/probe.ps1` | 38 条断言全通过，exit 0 |

**残留检查（真实进程探针之后）**

- 端口 18771：`Get-NetTCPConnection -LocalPort 18771` → **空闲**
- 探针进程：已 `Kill`（脚本 `finally` 块保证，即使中途抛异常）
- 探针二进制与隔离状态目录：已删除（`gateway-probe.exe` / `dbdump.exe` / `run/`）
- `bin/data/gateway.db`：`LastWriteTime` 仍为 `8/10/2026 4:57:34`，**未被读写**
- 工作区另有一个 `gateway` 进程（pid 7500），`ExecutablePath` 指向
  `%TEMP%\rg-integration-68292852\gateway.exe` —— 属**其他 teammate** 的集成测试，
  与本任务无关，未触碰。端口 8666（`bin/config.json` 的端口）无监听。

---

## 1. 权限矩阵大表

来源：`parseAdminRoutes()` 用正则从 `cmd/gateway/main.go` 的注册处**提取**
（不手抄 —— 手抄漏一条就静默少测一条）。实测解析出 **65 条**路由。

判定分三层，这是本项目最关键的一个区分：

| 归类 | 含义 | 判据 |
|---|---|---|
| `401/auth-401` | 网关层拒绝：未登录 | `UserAuth` 中间件 |
| `403/gate-DENY` | 网关层拒绝：非白名单 + 非管理员 | `AdminGateGuard` |
| `403/handler-only-deny` | **网关层放行**，handler 自己拦下 | handler 内 `requireAdmin` |
| `200/ALLOWED` | 到达 handler | — |

**为什么必须区分后两者**：本项目的 `AdminGateGuard` 是**前缀**匹配。
`/admin/api/usage` 在白名单里，于是整个 `/admin/api/usage/**` 子树被放行 ——
`by-user`（全站账单）与 `prune`（不可逆删除）都因此**只剩 handler 内一道防线**。
把两者混成一个「403 = 拒绝」，就永远看不出这一点。

### 1.1 网关层矩阵（65 条 × 3 身份，原始输出）

```
METHOD PATH                                       ANON                USER                ADMIN
GET    /admin/api/audit                           401/auth-401        403/gate-deny       200/ALLOWED
GET    /admin/api/bootstrap                       200/ALLOWED         200/ALLOWED         200/ALLOWED
POST   /admin/api/bootstrap                       200/ALLOWED         200/ALLOWED         200/ALLOWED
POST   /admin/api/config-export/export            401/auth-401        403/gate-deny       200/ALLOWED
POST   /admin/api/config-export/import            401/auth-401        403/gate-deny       200/ALLOWED
DELETE /admin/api/credentials/{id}                401/auth-401        403/gate-deny       200/ALLOWED
PATCH  /admin/api/credentials/{id}                401/auth-401        403/gate-deny       200/ALLOWED
GET    /admin/api/groups                          401/auth-401        403/gate-deny       200/ALLOWED
POST   /admin/api/groups                          401/auth-401        403/gate-deny       200/ALLOWED
DELETE /admin/api/groups/{id}                     401/auth-401        403/gate-deny       200/ALLOWED
PATCH  /admin/api/groups/{id}                     401/auth-401        403/gate-deny       200/ALLOWED
PUT    /admin/api/groups/{id}/models              401/auth-401        403/gate-deny       200/ALLOWED
GET    /admin/api/keys                            401/auth-401        200/ALLOWED         200/ALLOWED
POST   /admin/api/keys                            401/auth-401        200/ALLOWED         200/ALLOWED
DELETE /admin/api/keys/{id}                       401/auth-401        200/ALLOWED         200/ALLOWED
PATCH  /admin/api/keys/{id}                       401/auth-401        200/ALLOWED         200/ALLOWED
POST   /admin/api/keys/{id}/recompute-usage       401/auth-401        200/ALLOWED         200/ALLOWED
POST   /admin/api/login                           200/ALLOWED         200/ALLOWED         200/ALLOWED
POST   /admin/api/logout                          401/auth-401        200/ALLOWED         200/ALLOWED
GET    /admin/api/me                              401/auth-401        200/ALLOWED         200/ALLOWED
POST   /admin/api/me/password                     401/auth-401        200/ALLOWED         200/ALLOWED
GET    /admin/api/model-names                     401/auth-401        200/ALLOWED         200/ALLOWED
DELETE /admin/api/models/{id}                     401/auth-401        403/gate-deny       200/ALLOWED
PATCH  /admin/api/models/{id}                     401/auth-401        403/gate-deny       200/ALLOWED
POST   /admin/api/models/{id}/test                401/auth-401        403/gate-deny       200/ALLOWED
GET    /admin/api/providers                       401/auth-401        403/gate-deny       200/ALLOWED
POST   /admin/api/providers                       401/auth-401        403/gate-deny       200/ALLOWED
DELETE /admin/api/providers/{id}                  401/auth-401        403/gate-deny       200/ALLOWED
GET    /admin/api/providers/{id}                  401/auth-401        403/gate-deny       200/ALLOWED
PATCH  /admin/api/providers/{id}                  401/auth-401        403/gate-deny       200/ALLOWED
GET    /admin/api/providers/{id}/credentials      401/auth-401        403/gate-deny       200/ALLOWED
POST   /admin/api/providers/{id}/credentials      401/auth-401        403/gate-deny       200/ALLOWED
GET    /admin/api/providers/{id}/models           401/auth-401        403/gate-deny       200/ALLOWED
POST   /admin/api/providers/{id}/models           401/auth-401        403/gate-deny       200/ALLOWED
POST   /admin/api/providers/{id}/models/discover  401/auth-401        403/gate-deny       200/ALLOWED
POST   /admin/api/providers/{id}/models/import    401/auth-401        403/gate-deny       200/ALLOWED
POST   /admin/api/providers/{id}/test             401/auth-401        403/gate-deny       200/ALLOWED
POST   /admin/api/reload                          401/auth-401        403/gate-deny       200/ALLOWED
GET    /admin/api/routes                          401/auth-401        403/gate-deny       200/ALLOWED
POST   /admin/api/routes                          401/auth-401        403/gate-deny       200/ALLOWED
DELETE /admin/api/routes/{id}                     401/auth-401        403/gate-deny       200/ALLOWED
PATCH  /admin/api/routes/{id}                     401/auth-401        403/gate-deny       200/ALLOWED
GET    /admin/api/routes/{id}/targets             401/auth-401        403/gate-deny       200/ALLOWED
PUT    /admin/api/routes/{id}/targets             401/auth-401        403/gate-deny       200/ALLOWED
GET    /admin/api/session                         200/ALLOWED         200/ALLOWED         200/ALLOWED
GET    /admin/api/settings                        401/auth-401        403/gate-deny       200/ALLOWED
PUT    /admin/api/settings                        401/auth-401        403/gate-deny       200/ALLOWED
GET    /admin/api/stats                           401/auth-401        200/ALLOWED         200/ALLOWED
GET    /admin/api/topups                          401/auth-401        200/ALLOWED         200/ALLOWED
GET    /admin/api/upstream-models                 401/auth-401        403/gate-deny       200/ALLOWED
GET    /admin/api/usage                           401/auth-401        200/ALLOWED         200/ALLOWED
GET    /admin/api/usage/by-day                    401/auth-401        200/ALLOWED         200/ALLOWED
GET    /admin/api/usage/by-key                    401/auth-401        200/ALLOWED         200/ALLOWED
GET    /admin/api/usage/by-model                  401/auth-401        200/ALLOWED         200/ALLOWED
GET    /admin/api/usage/by-provider               401/auth-401        200/ALLOWED         200/ALLOWED
GET    /admin/api/usage/by-user                   401/auth-401        200/ALLOWED         200/ALLOWED   ← 白名单放行
GET    /admin/api/usage/history                   401/auth-401        200/ALLOWED         200/ALLOWED
GET    /admin/api/usage/history.csv               401/auth-401        200/ALLOWED         200/ALLOWED
POST   /admin/api/usage/prune                     401/auth-401        200/ALLOWED         200/ALLOWED   ← 白名单放行
GET    /admin/api/users                           401/auth-401        403/gate-deny       200/ALLOWED
POST   /admin/api/users                           401/auth-401        403/gate-deny       200/ALLOWED
DELETE /admin/api/users/{id}                      401/auth-401        403/gate-deny       200/ALLOWED
PATCH  /admin/api/users/{id}                      401/auth-401        403/gate-deny       200/ALLOWED
PUT    /admin/api/users/{id}/balance              401/auth-401        403/gate-deny       200/ALLOWED
POST   /admin/api/users/{id}/password             401/auth-401        403/gate-deny       200/ALLOWED
```

⚠️ **读表须知**：本表的 `USER` 列测的是**网关层是否放行**（mux 上挂的是探针
handler，只记「到达」）。所以 `/usage/by-user` 与 `/usage/prune` 的
`200/ALLOWED` 意为「网关层放行」，**不代表普通用户真的能拿到数据** ——
它们被 handler 内的 `requireAdmin` 拦下，见 §1.2 的实测。

### 1.2 「仅剩一道防线」的两个端点（真实 handler 实测）

请求经过**真实** handler（`admin.UsageHandler.GroupByUser` / `Prune`）：

```
普通用户 GET /admin/api/usage/by-user?from=0 -> 403 body={"error":{"message":"需要管理员权限","type":"invalid_request_error"}}
普通用户 POST /admin/api/usage/prune          -> 403 (真实进程探针 A-user-prune)
管理员   GET /admin/api/usage/by-user?from=0 -> 200 body={"records":[{"key":"bob",...,"cost":0.75},{"key":"alice",...,"cost":0.25}],"total":2}
```

且 `type` 是 `invalid_request_error` —— 这正是「handler 层拒绝」的标记，
与网关层的 `auth_error` 在实测输出里**可区分**（本报告的三层判定就是这么来的）。

`TestAuthzUsageByUser_OnlyDefenceIsInsideTheHandler` 把这件事拆成两半断言，
缺一不可：① 网关层**确实放行**（否则「唯一防线」这个前提是假的，测试会因错误
的原因通过）；② handler **确实拦截**且 403 响应里不含 `alice`/`bob`/`records`/`cost`。

### 1.3 白名单镜像自校验

`TestAuthzMatrix_WhitelistMirrorIsAccurate` 对每条路由用普通用户跑一次，
实测「网关层放行 ⟺ 路径命中白名单」，并把结果与测试里的镜像列表比对。
两者一致 → 通过。这防止「白名单被改动而测试镜像没跟着改」造成的
**测试与实际脱节**（那会让矩阵断言失去意义）。

---

## 2. 管理员退出数据面：四条边界（逐条实测）

真实进程探针的原始输出（`scripts/authz_probe/probe.ps1`）：

### B1 管理员不能建 key —— 403，且库里一把都没多

```
[PASS] B1-status — 管理员 POST /keys → 403
       body={"error":{"message":"管理员账号不能创建 API key：请新建一个普通用户账户，由该用户自行创建 key 后用于调用模型","type":"auth_error"}}
[PASS] B1-message — 错误消息指向正确出路（含「普通用户」）
[PASS] B1-no-side-effect — 库里 key 数 0 → 0（不得多出）
[PASS] B3-precondition — 普通用户自助建 key → 201   ← 反向：正当途径未被误伤
```

查库确认（`dbdump`，只读打开）：

```
access_keys_total=1  access_keys_owned_by_admin=0  access_keys_named_probe=0
access_keys_owned_by_user=1
[PASS] E-db-no-admin-key — 归属管理员的 key = 0 行
[PASS] E-db-no-ghost-key — 被拒的 admin-self-key-probe = 0 行
```

> **这条只能在真实进程上验**：`denyAdminKeyCreate` 定义在 `package main`
> （`cmd/gateway/main.go:1047`），外部测试包无法引用 —— 见 §6。

### B2 管理员不能给自己充值 —— 400，余额未变

```
[PASS] B2-status — 管理员给自己充值 → 400
       body={"error":{"message":"管理员账户不能充值：管理员不参与计费，请为普通用户账户充值","type":"invalid_request_error"}}
[PASS] B2-message — 错误消息说明管理员不参与计费
[PASS] B2-no-side-effect — 管理员余额 0 → 0（不得变化）
[PASS] E-db-admin-balance-unchanged — 管理员 balance_cents = 0（基线 0：B2 被拒后未变）
[PASS] E-db-no-admin-topup-row — balance_topups 中属于管理员的流水 = 0 行
[PASS] E-db-topup-count — balance_topups 共 1 行（仅普通用户那次充值）
```

最后一条同时印证了「流水表确实在写」—— 否则「没有管理员流水」会因为
流水表压根没工作而**假通过**。

### B3 管理员不能被认领 key —— 400，归属未变

```
[PASS] B3-status — 管理员把 key 认领给自己 → 400
       body={"error":{"message":"密钥不能归属管理员：管理员账号不参与 API 调用，请把 key 认领给一个普通用户","type":"invalid_request_error"}}
[PASS] B3-message — 错误消息指向认领给普通用户
[PASS] B3-no-side-effect — key 归属仍为 <普通用户 id>（不得变成管理员）
```

进程内同一断言的实现（`TestAuthzAdminBoundary_CannotBeClaimedAKey`）额外验证
**反向成功路径**：管理员把 key 认领给普通用户 → 200。

### B4 管理员不能调 /v1 —— 403 `admin_cannot_call_model`

进程内直接走真实 `auth.Authenticate`（快照驱动）：

```go
// TestAuthzAdminBoundary_CannotCallV1
管理员带 key 调 /v1：err = ErrAdminCannotCallModel     ← 精确匹配
普通用户 key 调 /v1：通过，归属 = "alice"              ← 反向
```

真实进程侧：

```
[PASS] B4-anon-v1 — 匿名调 /v1 → 401（无 key 必须 401）
[PASS] B4-user-v1-auth-passes — 普通用户 key 通过数据面鉴权
       (status=404，非 admin_cannot_call_model / 非 invalid_api_key)
       /v1 body={"error":{"message":"model \"flash\" not found","type":"invalid_request_error","code":"model_not_found"}}
```

> **一个额外的结构性结论**：在真实进程上**无法构造**「管理员带 key 调 /v1」——
> 因为 B1 堵住建 key、B3 堵住认领，管理员拿不到任何数据面凭据。
> 这比「拿到 key 后被拦」更强。该组合因此只能由进程内测试直接调
> `auth.Authenticate` 来覆盖（已覆盖）。

---

## 3. 作用域收窄：普通用户真的只看到自己

用**两个都有数据**的用户（不是「库里只有一个用户」那种假通过）。
`alice` 250_000 token / 0.25 元，`bob` 750_000 token / 0.75 元，全站 1.00 元。

### 3.1 数字证明（`TestAuthzScope_NumericIsolation`）

```
stats 实测：alice={TotalTokens:250000 TotalRequests:1 Cost:0.25}
            bob  ={TotalTokens:750000 TotalRequests:1 Cost:0.75}
            admin(全站)={TotalTokens:1000000 TotalRequests:2 Cost:1}
alice /usage 明细条数 = 1（全站 2 条）
```

断言四个数字：alice=250000/0.25、bob=750000/0.75、admin=1000000/1.00、
alice 明细 1 条。**admin 那一组是关键对照** —— 它证明数据确实都在库里，
上面的「只看到自己」不是因为表里本来就只有 alice。

### 3.2 每个白名单端点的原始响应（`TestAuthzScope_NormalUserSeesOnlyOwnData`）

```
普通用户 /admin/api/usage?from=0&limit=100      -> 200  records[…输入 250000…]          无 bob
普通用户 /admin/api/usage/by-key?from=0         -> 200  [{"key":"key-alice","count":1,"tokens":250000,"name":"key-alice"}]
普通用户 /admin/api/usage/by-model?from=0       -> 200  无 bob
普通用户 /admin/api/usage/by-provider?from=0    -> 200  无 bob
普通用户 /admin/api/usage/by-day?from=0         -> 200  无 bob
普通用户 /admin/api/usage/history?from=0        -> 200  key_name":"key-alice"            无 bob
普通用户 /admin/api/usage/history.csv?from=0    -> 200  无 key-bob
普通用户 /admin/api/usage/by-user?from=0        -> 403  {"message":"需要管理员权限"}       ← admin-only
普通用户 /admin/api/topups?limit=50             -> 200  delta_cents:10000,user_id:"alice" 无 bob/无 90000
普通用户 /admin/api/keys                        -> 200  [{"id":"key-alice","user_id":"alice"}] 无 key-bob
普通用户 /admin/api/stats?from=0                -> 200  "total_tokens":250000,"cost":0.25
```

同一条用例还断言**管理员看得到两个人**（对照）：

```
管理员 /admin/api/usage?from=0&limit=100  -> 200  records[…]（含 bob 与 alice）
管理员 /admin/api/topups?limit=50         -> 200  user_id:"bob" 与 user_id:"alice"
管理员 /admin/api/usage/by-user?from=0    -> 200  {"records":[bob 0.75, alice 0.25],"total":2}
```

### 3.3 显式越权参数被忽略

```
[PASS] C-topups-userid-ignored — 普通用户传 ?user_id=管理员 时作用域仍是自己
       (user_ids=<自己>)
```

### 3.4 IDOR：跨用户的**写**操作（`TestAuthzIDOR_CrossUserKeyMutations`）

`/admin/api/keys` 在白名单里 ⇒ 普通用户能进这个前缀下的 **PATCH / DELETE /
recompute-usage**。每个路径各发一次跨用户请求：

```
alice PATCH  /admin/api/keys/key-bob                  -> 404 {"message":"密钥不存在"}
alice DELETE /admin/api/keys/key-bob                  -> 404 {"message":"密钥不存在"}
alice POST   /admin/api/keys/key-bob/recompute-usage  -> 404 {"message":"密钥不存在"}
对照：alice 改自己的 key → 200（证明 404 是作用域判定而不是端点故障）
```

状态码是 **404 而非 403** 是刻意的（403 会告诉调用者「这把 key 存在，只是不是
你的」，本身可枚举）。每个断言后**重新查库**确认 bob 的 key
`Name`/`Enabled`/`UserID` 三个字段一个都没变，且 key 仍然存在。

---

## 4. CSRF

### 4.1 已有覆盖（未重复造）

`internal/admin/csrf_test.go` 已在 **handler 层**覆盖：`text/plain` → 415、
缺失 Content-Type → 415、坏 JSON → 400、`application/json` 及
`application/vnd.api+json` → 接受。本任务不重复。

### 4.2 本任务新补的部分

**(a) cookie 认证 + 完整链路**（`TestAuthzCSRF_CookieAuthWriteRequiresJSONContentType`）

已有测试走的是 handler 直调 + `asUser()` 注入身份；本用例走**完整中间件链**，
且凭据用 **cookie**（`rosetta_session`）而不是 `Authorization` 头 ——
cookie 是浏览器会自动附带的，那才是 CSRF 的真实形态。

```
cookie 认证 + text/plain            -> 415  ✓
cookie 认证 + 无 Content-Type       -> 415  ✓
cookie 认证 + application/json      -> 201  ✓（反向：证明链路没坏）
```

随后查库确认被 415 拒绝的请求**没有建出 key**。

**(b) SameSite 属性**（`TestAuthzCSRF_SameSiteStrictOnSessionCookie`）

```
会话 cookie: name=rosetta_session path=/admin samesite=3 httponly=true
```

`samesite=3` 即 `http.SameSiteStrictMode`。这是 CSRF 的第一道也是最强的一道
防线：跨站请求浏览器根本不附带这个 cookie，于是「跨站写操作」在到达服务端
之前就没有凭据了。Content-Type 门禁是第二道。

**(c) 同源判定**：`server.SameOrigin` 被 `BootstrapSetup` 作为**第一条语句**
调用（实测 `A-bootstrap-closed`：引导完成后再 POST → 409，窗口一次性关闭）。

**结论：未发现 CSRF 缺口。**两道防线都实测有效，且互相独立
（即便 SameSite 因老浏览器失效，Content-Type 门禁仍然拦得住
`text/plain` 这种唯一不需要预检的简单请求形态）。

---

## 5. 会话与限速

### 5.1 改密后旧会话立刻失效（`TestAuthzSession_RevokedAfterPasswordChange`）

```
改密前 /me=200，改密后同一令牌 /me=401（auth_version 栅栏生效）
```

流程走真实链路：真实签发的 JWT → 真实 `ChangePassword`（`SetUserPassword`
在 SQL 里 `auth_version + 1`）→ 同一枚令牌再打 → 401。

### 5.2 登出是**服务端吊销**（`TestAuthzSession_LogoutRevokesServerSide`）

```
[进程内]     登出前 /me=200，登出后同一令牌 /me=401
[真实进程]   D-logout-revokes — 登出前 /me=200，登出后同一令牌 /me=401
```

关键点：令牌**仍在客户端手里**（`Authorization` 头照带），但已被服务端吊销 ——
这排除了「只清了 cookie」那种假吊销。

### 5.3 限速阈值行为与常量一致

`LoginFailLimit = 10`（`server.LoginFailLimit`）。空闲来源 IP 实测：

```
[PASS] TestAuthzThrottle_InvalidTokenEventually429
       第 11 次伪造令牌被 429（阈值 LoginFailLimit=10）
```

即前 10 次 401、第 11 次 429 —— 与常量**完全一致**（不是 9 次也不是 12 次）。

### 5.4 匿名请求不计入限速（历史 bug 回归）

连发 `LoginFailLimit + 5 = 15` 个匿名请求：

```
[PASS] TestAuthzThrottle_AnonymousRequestsDoNotCount
       连发 15 个匿名请求后：全部 401（无 429），有效会话仍 200
```

两个方向都验了：① 匿名不触发 429；② 轰炸之后**有效令牌仍能用**
（证明限速没有误伤同 IP 的正常用户）。这条同时覆盖了
`internal/server/throttle_test.go` 的包内版本（两处独立，任一处被改坏都能发现）。

---

## 6. 未能验证的部分（如实列出）

| # | 未能验证 | 原因 | 已用何种方式替代 |
|---|---|---|---|
| 1 | `denyAdminKeyCreate` 的**进程内**验证 | 它定义在 `package main`（`cmd/gateway/main.go:1047`），外部测试包无法引用；`internal/admin` 测试包又拿不到 `server` 的私有 context key | **真实进程探针**已完整覆盖（B1 三条断言 + 查库）。这是唯一走真实进程的原因 |
| 2 | 前端（Vue）侧是否真的藏起了管理员入口 | 本任务范围是后端权限边界；前端隐藏属**体验**而非权限控制（真正的拦截在后端） | 未验证，也不应作为安全性依据 |
| 3 | 反向代理部署下 `clientIP` 的准确性 | `clientIP` 刻意不读 `X-Forwarded-For`（防伪造），代理后计数退化为「按代理 IP」。这是**已知且有意的取舍**，不是缺陷 | 未验证（需真实代理环境） |
| 4 | `SameSite` 在具体浏览器上的强制行为 | 只能断言服务端下发的 Set-Cookie 属性；浏览器是否遵守由浏览器决定 | 已断言 `samesite=3`（Strict）+ `HttpOnly` + `Path=/admin` |
| 5 | 并发条件下的作用域收窄 | 未做并发压测（本任务验证的是权限判定，不是并发正确性） | `internal/server/session_throttle_concurrency_test.go` 另有并发用例（已存在，全绿） |
| 6 | 管理员 `/v1` 边界在**真实进程**上的端到端组合 | 真实进程上管理员**拿不到**数据面 key（B1/B3 拦住了），该组合无法构造 | 进程内直接调 `auth.Authenticate` 精确断言 `ErrAdminCannotCallModel` |

### 工具侧的两处「首轮实测踩坑」（记录以免后人重踩）

1. **`/admin/api/me` 不返回 `id`**（只有 username/role/…）。
   首轮探针把 `$me.id` 当管理员 id → 空串 → B2 的 URL 变成 `/users//balance`
   （307 重定向）、B3 传了 `user_id=null`。管理员 id 必须从
   `/admin/api/users` 里按 `role` 找。**当前探针已修正并全绿。**
2. **探针把「管理员 balance_cents 应为 NULL」当成断言**，实际是 `0`
   （`ensureBootstrapAdmin` 建号时未置 `Unlimited` ⇒ 落 0 而非 NULL）。
   该 0 无害（管理员被挡在 /v1 之外，API 层又把管理员余额归一成「不限额」），
   但让一条**本该通过**的断言误报。已改为「与 B 组动作前的基线对比」——
   正确的不变量是「被拒的操作没有改动它」，与具体取值无关。

---

## 7. 发现的越权/放行不当

**未发现任何越权漏洞。**具体地说：

- 65 条管理面路由 × 3 身份 = 195 次实测，**没有一条**出现
  「普通用户拿到了 200 而语义上他不该看到」的情况；
- 白名单里 8 个前缀下的全部端点都实测了作用域收窄（§3），用**数字**证明
  普通用户看不到别人的数据；
- 白名单前缀下两个 admin-only 端点（`/usage/by-user`、`/usage/prune`）
  实测被 handler 拦下（403 + `invalid_request_error`），且 403 响应体不含数据；
- 跨用户写操作（PATCH/DELETE/recompute-usage）实测 404 + 查库确认零副作用；
- 四条管理员数据面边界全部拦住，错误消息都指向正确出路，且**失败路径无副作用**
  （查库确认）。

### 一条值得记录的**结构性风险**（不是当前缺陷）

`userAccessiblePrefixes` 是**前缀**匹配，因此
`/admin/api/usage` 这一条白名单会**自动放行该子树下未来新增的一切端点**。
`/usage/by-user` 正是这样落进来的：它在网关层没有任何保护，唯一防线是
handler 第一行的 `requireAdmin`。

- **当前状态**：`by-user` 与 `prune` 都有 `requireAdmin`，实测有效（§1.2）；`/usage/prune` → 403 实测确认。
- **风险**：将来有人在这条前缀下**新增**端点而忘了写 `requireAdmin`
  （或忘了它属于 admin-only），网关层不会拦，也**不会报错** —— 表现为端点照常
  200，静默越权。`internal/admin/usage_by_user_test.go` 与
  `TestAuthzUsageByUser_OnlyDefenceIsInsideTheHandler` 各钉了一半，
  但都只能覆盖**已存在**的端点。

**建议**（不在本任务范围内，供 Lead 决策）：把 admin-only 的判定从「handler 里
记得写」升级为**可在网关层表达**的形式 —— 例如给 `userAccessiblePrefixes`
增加一个「例外子路径」列表（`/admin/api/usage/prune`、`/admin/api/usage/by-user`），
或让这些端点在注册时显式套一层 `requireAdminMiddleware`。这样漏写会**静默越权**
变成**编译期/注册期可见**，与本仓库既有的「白名单优于黑名单」的取舍一致。

---

## 8. 产出文件

| 文件 | 说明 |
|---|---|
| `internal/server/authz_matrix_test.go` | 主产出。进程内，`package server_test`（能 import admin，不构成依赖环 —— 已实测） |
| `scripts/authz_probe/probe.ps1` | 真实进程探针（38 条断言），用 `ROSETTA_GW_HOME` 完全隔离 |
| `scripts/authz_probe/dbdump/main.go` | 只读查库助手（本机无 sqlite3 CLI） |

测试清单（`internal/server/authz_matrix_test.go`）：

| 用例 | 覆盖 |
|---|---|
| `TestAuthzMatrix_GatewayLayer` | 65 路由 × 3 身份的网关层矩阵 |
| `TestAuthzMatrix_WhitelistMirrorIsAccurate` | 白名单镜像与实际行为一致 |
| `TestAuthzUsageByUser_OnlyDefenceIsInsideTheHandler` | 前缀白名单不管用 + handler 唯一防线 |
| `TestAuthzAdminBoundary_CannotCallV1` | 管理员 /v1 → `ErrAdminCannotCallModel` |
| `TestAuthzAdminBoundary_CannotBeClaimedAKey` | 不能认领给管理员 + 零副作用 + 反向 |
| `TestAuthzAdminBoundary_CannotTopUpSelf` | 不能自充值 + 零副作用 + 反向 |
| `TestAuthzScope_NormalUserSeesOnlyOwnData` | 11 个白名单端点的隔离 |
| `TestAuthzScope_NumericIsolation` | 数字证明（250000 / 750000 / 1000000） |
| `TestAuthzIDOR_CrossUserKeyMutations` | 跨用户写操作 404 + 查库零副作用 |
| `TestAuthzScope_AdminSeesEveryoneInTopups` | 管理员看全站流水 |
| `TestAuthzCSRF_CookieAuthWriteRequiresJSONContentType` | cookie 认证 + Content-Type 门禁 |
| `TestAuthzCSRF_SameSiteStrictOnSessionCookie` | SameSite=Strict / HttpOnly / Path |
| `TestAuthzSession_RevokedAfterPasswordChange` | auth_version 栅栏 |
| `TestAuthzSession_LogoutRevokesServerSide` | 服务端吊销 |
| `TestAuthzThrottle_AnonymousRequestsDoNotCount` | 匿名不计数（历史 bug） |
| `TestAuthzThrottle_InvalidTokenEventually429` | 阈值与 `LoginFailLimit` 一致 |
