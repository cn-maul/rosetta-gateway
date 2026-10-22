# A7 管理面 API、鉴权、会话与权限边界审计

审计日期：2026-10-20  
审计对象：`rosetta-gateway`  
审计方式：只读静态审计，结合现有测试与真实路由矩阵；使用 `xerj_search(index="rosetta-gateway-docs", ...)` 做语义检索，精确字符串与测试用 `rg`。未查询 `ax-*` 或 `*`，未修改源码或既有报告。

## 1. 结论摘要

本审计确认 **0 个 P0、5 个 P1、12 个 P2、3 个 P3**。管理面现有 81 条实际路由均有矩阵测试覆盖，未发现匿名可达受保护 handler、跨用户 key 对象级越权、普通用户读取全站账单等直接违例。主要风险集中在“写操作已提交但会话吊销/审计/附随写入失败”的降级路径，以及 `PATCH` 和部署环境的契约边界。

## 2. 路由注册与鉴权链

### 2.1 路由真相与装配

- 管理端点集中在 `cmd/gateway/main.go:370-517` 注册：`adminMux` 中具体路由 77 条，`publicAdminMux` 中公开路由 4 条。根 mux 在 `cmd/gateway/main.go:565-577` 显式挂载公开端点，其余 `/admin/api/` 五类方法统一进入 `adminAuto`。
- 公开端点只有 `POST /admin/api/login`、`GET /admin/api/session`、`GET/POST /admin/api/bootstrap`，分别见 `cmd/gateway/main.go:374-378`；其中 bootstrap POST 在 `cmd/gateway/main.go:571` 额外套 `server.AuditOnly`。
- 真实路由矩阵从 `main.go` 源码提取路由，见 `internal/server/authz_matrix_test.go:88-149`；按方法+模式去重后逐条测试匿名、普通用户、管理员，见 `internal/server/authz_matrix_test.go:417-526`。本机执行结果为“管理面权限矩阵（网关层）共 81 条路由”，全部 PASS。
- 普通用户白名单使用 `http.ServeMux.Handler` 返回的真实注册模式，而非手抄路径；未注册模式 fail-closed，见 `internal/server/user_auth.go:222-253`。管理员分支同时要求 `Role=admin` 与 `ID != ""`，见 `internal/server/user_auth.go:235-238`。
- 全局链装配顺序为 Recovery → Middleware → SecurityHeaders → CORS → RequestSizeLimit，见 `cmd/gateway/main.go:662-666`；管理鉴权位于 `adminAuto` 内，不属于该全局链。

### 2.2 路由级策略与 handler 双防线

- 管理员不能新建 key 的策略在路由包装层实现，缺身份 401、管理员 403，见 `cmd/gateway/main.go:1301-1321`；实际注册见 `cmd/gateway/main.go:443-447`。
- `GET /admin/api/usage/by-user` 与 `POST /admin/api/usage/prune` 不在普通用户白名单；handler 内仍有 `requireAdmin` 作为第二道防线，注册与注释见 `cmd/gateway/main.go:461-469`，handler 见 `internal/admin/usage_by_user_handler.go:81-100` 与 `internal/admin/usage_handler.go:944-982`。
- `config-export/export` 与 `import` 含敏感配置，handler 再次校验管理员，见 `internal/admin/config_transfer_handler.go:47-50,215-218`。

**A7-01｜P2｜管理路由授权契约由多套判定维持**

网关层的精确模式白名单、handler 内 `requireAdmin`、对象归属判断和 `callerScope` 分别维护“能否进入 / 是否管理员 / 能否操作该对象 / 能看谁的数据”。例如白名单在 `internal/server/user_auth.go:161-207`，`requireAdmin` 在 `internal/admin/user_admin_handler.go:158-172`，key 归属在 `internal/admin/key_handler.go:506-539`，用量 scope 在 `internal/admin/usage_handler.go:175-198`。当前矩阵与对象测试通过，但新增白名单端点仍要靠 handler 自证作用域。A1 已记录总体的鉴权口径分散，见 `work/audit-2026-10-20/A1-architecture.md:190-206`；A7 不重复泛化问题，只把“新增端点必须同时完成对象级收窄”列为维护约束。

## 3. 管理员认证、会话与 token 生命周期

### 3.1 正常签发与校验链

- 登录同时使用 IP 和账号两维限速，所有未登录分支调用 `failBoth`，见 `internal/admin/user_handler.go:186-305`；不存在与密码错误共用伪哈希，见 `internal/admin/user_handler.go:259-272`。
- JWT 固定 HS256、默认 8 小时、issuer/subject/uid/auth_version 均校验，见 `internal/userauth/session.go:18-23,185-279`；`Peek` 只做身份预解析，不接受先用 token 内版本自证，见 `internal/userauth/session.go:281-317`。
- 管理中间件顺序为取 token → `Peek` → 查当前用户 → active → `Verify(auth_version)`，见 `internal/server/user_auth.go:270-321`。空 token 不消耗失败额度，非空错误 token 才计数，见 `internal/server/user_auth.go:277-295`。
- 改密、管理员重置密码和 bootstrap 设密均通过带 `auth_version = auth_version + 1` 的 SQL 原子失效旧会话，见 `internal/store/user_dao.go:281-291,394-418`；改密闭环测试见 `internal/server/authz_matrix_test.go:1319-1372`。

### 3.2 A7-02｜P1｜角色/状态更新与会话失效不是原子操作

`UpdateUser` 先提交角色、状态、分组等字段，再单独执行 `BumpAuthVersion`，见 `internal/admin/user_admin_handler.go:491-506`。底层两次写分别是独立的 `UPDATE users` 与相对递增，见 `internal/store/user_dao.go:259-278,455-465`。第二步失败时，角色或禁用状态已经落库，但 handler 返回 500；旧会话在自然过期前仍可被 `auth_version` 校验通过，且外层 `AutoReload` 对 >=400 响应不审计、不置脏。现有测试只覆盖成功 bump，见 `internal/admin/user_admin_handler_test.go:113-137`，没有覆盖第二步失败。

### 3.3 A7-03｜P1｜Logout 服务端吊销失败仍返回 200

`Logout` 先清 cookie，再调用 `BumpAuthVersion`；失败时明确记录“仍回 200”，见 `internal/admin/user_handler.go:630-650`。前端 `logout()` 的 `finally` 无论响应如何都清本地 token，见 `web/src/api.ts:191-198`，因此用户会认为所有设备已登出，但被复制出的 Bearer token 在自然过期前仍有效。成功路径测试见 `internal/server/authz_matrix_test.go:1374-1400`，却没有失败路径断言。

### 3.4 A7-04｜P2｜Cookie 优先于 Bearer，错误 cookie 可遮蔽有效脚本凭据

`sessionToken` 只要发现非空 cookie 就直接返回 cookie，不再尝试 Bearer，见 `internal/server/user_auth.go:388-397`。`cookieAuthed` 同样按“非空 cookie”决定是否做同源校验，见 `internal/server/user_auth.go:371-375`。现有测试只覆盖空 cookie 回落 Bearer，见 `internal/server/csrf_middleware_test.go:257-276`；非空但失效 cookie + 有效 Bearer 的行为未测试。未证明可跨账号提权，标记「待确认」；但会制造脚本运维的确定性 401，且前端实际只发 Bearer（`web/src/api.ts:209-212,282-285`），Cookie 与 Bearer 并存时契约不清晰。

### 3.5 A7-05｜P2｜反向代理部署下无效 token 限速按代理 IP 聚合

管理鉴权失败和登录限速都只读 `RemoteAddr`，刻意不读 `X-Forwarded-For`，见 `internal/server/server.go:187-204`；README 推荐前置反向代理，见 `README.md:93-101`。在单反向代理拓扑下，10 次非空错误 token 可让所有用户共享同一 IP 冷却一分钟，见 `internal/server/user_auth.go:270-295` 与阈值定义 `internal/server/server.go:206-220`。实际部署拓扑与是否需要可信代理配置「待确认」，故列 P2；不能简单改为信任任意 XFF。

### 3.6 Cookie 与会话固定/重放边界

- Cookie 具备 `HttpOnly`、`SameSite=Strict`、`Path=/admin`，见 `internal/server/user_auth.go:399-425`；测试见 `internal/server/authz_matrix_test.go:1288-1313`。
- 未设置 `Secure` 是局域网 HTTP 的已知取舍，见 `internal/server/user_auth.go:409-410`。这不是本次新发现的漏洞，但在反代 HTTPS 后仍不设置 Secure，会让 HTTP 子资源或错误重定向扩大 Cookie 暴露面；“是否由反代终止 TLS”属于部署契约，标记「待确认」。
- 登录每次签发含随机 `jti` 的新 token，见 `internal/userauth/session.go:198-216,327-331`；未发现服务端会话固定点。`auth_version` 变更使所有设备一起失效，能抑制旧 token 重放，但没有逐 token 撤销粒度。

## 4. 用户、分组、API key 权限模型

### 4.1 用户管理边界

- 用户列表、创建、修改、删除、重置密码均调用 `requireAdmin`，见 `internal/admin/user_admin_handler.go:174-177,263-266,385-388,518-521,546-549`；`requireAdmin` 对空 ID 也 fail-closed，见 `internal/admin/user_admin_handler.go:158-172`。
- 禁止自删、自禁用、自降级，测试同时验证库未被改变，见 `internal/admin/scope_test.go:340-395`。
- 空密码普通用户不能升级为管理员，见 `internal/admin/user_admin_handler.go:459-469` 与测试 `internal/admin/user_admin_handler_test.go:90-110`。
- 普通用户只能改自己的密码；旧密码校验按用户 ID 限速，成功后改密并清 cookie，见 `internal/admin/user_admin_handler.go:853-922`。

### 4.2 A7-06｜P2｜管理员可给另一名管理员充值

`AdjustBalance` 只拒绝 `self.ID == id`，见 `internal/admin/user_admin_handler.go:615-644`；后续没有重新读取目标角色，直接把 `id` 交给 `AdjustBalanceWithLedger`，见 `internal/admin/user_admin_handler.go:683-697`。底层 DAO 只校验非空 userID 与用户存在，不检查角色，见 `internal/store/topup_dao.go:159-213`。现有测试只覆盖“给自己充值被拒”，见 `internal/server/authz_matrix_test.go:870-908`。这不会提升权限，但会让不参与数据面计费的管理员出现余额与充值流水，破坏“管理员不能充值”的产品边界，列为 P2。

### 4.3 分组与模型可见性

- 分组成员/白名单端点均为 admin-only，见 `cmd/gateway/main.go:501-516`；`ModelNames` 对普通用户按组白名单求交，空 ID 401、空交集返回空集，见 `internal/admin/group_handler.go:326-361`。
- 删除分组会同时阻止仍有用户或 key 的组，避免外键 SET NULL 静默放宽权限，见 `internal/store/group_dao.go:103-130`。

**A7-07｜P2｜Group PATCH 只改 name 会清空 description**

`groupRequest` 的 `Name` 与 `Description` 都是普通 string，见 `internal/admin/group_handler.go:103-106`；`Update` 对 name 做“非空才改”，却对 description 无条件赋值，见 `internal/admin/group_handler.go:164-180`。因此 `PATCH {"name":"研发二组"}` 会把原描述写成空串；现有测试只断言 name，见 `internal/admin/group_handler_test.go:83-93`。这违反项目在 `internal/admin/helpers.go:150-160` 明确的指针合并规则。

### 4.4 API key 的对象级授权

- 普通用户 key 列表按自己的 `user_id` 查询，管理员看全量，见 `internal/admin/key_handler.go:106-126`。
- PATCH/DELETE/recompute 都先读 key 再调用 `ownedByCaller`，跨用户统一 404，见 `internal/admin/key_handler.go:541-553,686-698,713-725` 与 `internal/admin/key_handler.go:506-539`。
- 跨用户 IDOR 测试覆盖改名、删除、重算，并检查库内名称、enabled、owner 未变，见 `internal/server/authz_matrix_test.go:1112-1190`。
- 普通用户只能收紧 key 的 enabled、quota、RPM、TPM、expiry、IP 白名单，见 `internal/admin/key_handler.go:384-434`；key 归属只允许管理员改变，见 `internal/admin/key_handler.go:617-681`。

**A7-08｜P2｜Key PATCH 认领分两次写**

`Update` 先调用 `UpdateAccessKey` 提交名称/限制，再调用 `ReassignAccessKey` 提交归属，见 `internal/admin/key_handler.go:670-681`；DAO 也明确将两步拆开，见 `internal/store/key_dao.go:304-343`。第二步失败时，前面的 key 配置已经落库但响应为 404/500，客户端重试时可能把“部分成功”误读成整体失败。归属校验本身正确，问题是 PATCH 原子性，列 P2。

**A7-09｜P2｜KeyHandler.List 无身份时走管理员分支**

`List` 仅在 `me != nil && !me.IsAdmin()` 时收窄，否则调用全量 `ListAccessKeys`，见 `internal/admin/key_handler.go:106-126`。生产链在该层之前必然注入身份，因此当前不可达；但 handler 单独复用或测试替身漏注入时会 fail-open，与 `requireAdmin`、`ownedByCaller` 的空 ID fail-closed 口径不一致。A7 标为 P2 防御性问题，不声称现有线上可越权。

### 4.5 用量、充值流水与探测数据 scope

- 用量统一由服务端 `callerScope` 决定 scope，缺失或空 ID 返回不可匹配哨兵，见 `internal/admin/usage_handler.go:175-198`；过滤条件集中在 `usageFilter` 与 `usageHistoryFilters`，见 `internal/admin/usage_handler.go:115-173,519-531`。
- `Stats` 的 lifetime 仅管理员显式请求时返回，普通用户只读自己的统计数据，见 `internal/admin/stats_handler.go:123-141,216-236`。
- Topups 不接受 query 指定 user_id，普通用户按 `me.ID` 查询，管理员跨用户；空身份 401，见 `internal/admin/user_admin_handler.go:758-843`。
- Probe Models 与 Timeline 都按组白名单收窄，空交集保持空集而不放大，见 `internal/admin/probe_handler.go:62-95,167-243`。
- 作用域黑盒测试验证普通用户看不到另一用户的账单与明细，见 `internal/server/authz_matrix_test.go:910-1110`。

## 5. PATCH 字段合并与不可变字段

### 5.1 正确的合并模式

- User PATCH 使用 `*string/*int64` 区分未提供与显式值，见 `internal/admin/user_admin_handler.go:373-382,400-506`。
- Provider PATCH 同样使用指针，`slug` 不在更新请求中，见 `internal/admin/provider_handler.go:45-67`；`slug` 创建后不可修改的契约见 `DESIGN.md:701-707`。
- Model、Route、Credential 的 PATCH 均使用指针或三态类型，例如 `internal/admin/model_handler.go:29-67,334-414`、`internal/admin/route_handler.go:33-39,158-220`、`internal/admin/credential_handler.go:21-37,157-212`。

### 5.2 A7-10｜P2｜Network PATCH 要求两个字段同时出现

`networkRequest` 两个字段虽然都是指针，但 Update 对缺失字段使用 `derefStr` 后立即把空串判为非法，见 `internal/admin/network_handler.go:61-64,97-116`。因此只 PATCH `name` 或只 PATCH `proxy_url` 都会 400，实际变成“必须整体提交”。当前前端总是同时发送两个字段，见 `web/src/views/Networks.vue:92-100`，所以暂未造成 UI 回归；契约仍是 P2。

### 5.3 A7-11｜P2｜ModelGroup PATCH 缺少 display_name 时 400

`modelGroupRequest.DisplayName` 为指针，但 Update 无条件 deref 并拒绝空值，见 `internal/admin/model_group_handler.go:74-76,104-126`。因此该端点不支持空请求或未来新增字段的纯 PATCH；前端当前总发送 display_name，见 `web/src/views/ModelGroups.vue:90-97` 与 `web/src/api.ts:500-503`。列为 P2。

### 5.4 A7-12｜P2｜Upstream model_id 可在 PATCH 中被改名，契约待确认

`modelRequest.ModelID` 为指针，Update 接受非空新值并直接写入，见 `internal/admin/model_handler.go:34-39,343-350`；DAO 的 `UPDATE upstream_models SET model_id = ?` 也会写入，见 `internal/store/model_dao.go:198-205`。而用量归档以 `(provider_id, upstream_model)` 作为不可变定位符，见 `internal/admin/usage_handler.go:785-797,813-831`。如果模型名必须作为历史定位符，改名会让历史按模型归组与后续配置的目标对不上；设计文档只明确 `providers.slug` 不可变，未明确 `upstream_models.model_id`。因此标记「待确认」，暂列 P2，不当成已确认数据错误。

## 6. CSRF、CORS、Origin

### 6.1 A7-13｜P1｜sameOrigin 未比较 scheme

`sameOrigin` 注释宣称比较 scheme+host，见 `internal/server/server.go:146-158`；实际只比较 `u.Host` 与 `r.Host`，见 `internal/server/server.go:161-174`。只要 `Origin` 的 scheme 不同但 host 相同，就会被误判为同源。该函数同时用于免鉴权的 bootstrap 写端点，见 `internal/admin/user_handler.go:489-496`，并由管理中间件用于 cookie 写请求，见 `internal/server/user_auth.go:323-357`。现有测试只覆盖 host 不同，见 `internal/server/csrf_middleware_test.go:167-202` 与 `internal/admin/bootstrap_handler_test.go:180-200`，没有覆盖同 host 不同 scheme。Cookie 未设置 Secure，见 `internal/server/user_auth.go:399-411`，该缺陷在 HTTP 部署下风险更实际。修复应比较规范化 scheme 与 host，或引入可信外部 origin 配置，而不是无条件信任转发头。

### 6.2 A7-14｜P2｜登录公开端点没有走 SameOrigin 门禁

登录是公开的 `POST /admin/api/login`，注册见 `cmd/gateway/main.go:374`；handler 从 `internal/admin/user_handler.go:164` 开始直接解析 JSON，未调用 `server.SameOrigin`。bootstrap POST 有显式同源判断，见 `internal/admin/user_handler.go:489-496`，但登录没有同义防线。登录响应同时写入 Cookie，见 `internal/admin/user_handler.go:297-305`。同站不同源页面可触发一次登录请求，造成登录 CSRF / 错误会话覆盖；`SameSite=Strict` 与浏览器 preflight 是现有间接防线，但没有统一 Origin 校验。由于是否依赖“所有浏览器都强制 Sec-Fetch-Site”尚待部署确认，列为 P2。

### 6.3 已验证的 CSRF/CORS 防线

- CORS 对 `/admin` 不设置 `Access-Control-Allow-Origin`，仅数据面放行，见 `internal/server/server.go:124-143`。
- 所有 JSON handler 先断言 Content-Type，见 `internal/admin/helpers.go:101-147`；完整 cookie 链测试验证 text/plain 与缺失 Content-Type 都返回 415，见 `internal/server/authz_matrix_test.go:1230-1285`。
- 同站不同源的 cookie 写请求被拒绝，测试覆盖 prune/logout/recompute 等无 body 端点，见 `internal/server/csrf_middleware_test.go:129-165`。
- 安全响应头对 `/admin` 设置 CSP、X-Frame-Options、nosniff、no-referrer、no-store，见 `internal/server/server.go:458-495`。

## 7. 审计日志与敏感信息

### 7.1 A7-15｜P1｜Bootstrap 失败不留审计

`AuditOnly` 注释明确说引导失败意味着有人猜管理员密码，应保留攻击信号，见 `internal/server/autoreload.go:187-195`；实际条件却是 `if sw.StatusCode() < 400`，见同文件 `193-195`，403/409 等失败全部跳过。错误引导令牌正是 403，handler 分支见 `internal/admin/user_handler.go:564-571`；密码弱、已完成窗口等 400/409 路径也不落审计。根 mux 只对 bootstrap POST 套 `AuditOnly`，见 `cmd/gateway/main.go:568-571`，因此这是实际生产路径，不是测试桩。

### 7.2 A7-16｜P1｜不可逆 usage prune 的 keep_days 不进入审计

Prune 从 query string 读取 `keep_days`，见 `internal/admin/usage_handler.go:944-982`；handler 注释声称该参数会进 audit，见 `internal/admin/usage_handler.go:980-981`。但 `AutoReload` 只解析请求体顶层 JSON 字段，见 `internal/server/autoreload.go:59-86,143-155`；DELETE 甚至连 body 解析分支都不走，见 `internal/server/autoreload.go:66-85,90-116`。因此 Prune 的不可逆删除参数无法从审计字段名还原。结合 A7-15，建议把“审计完整性”修为一个原子范围：失败安全事件与实际变更参数都要可追溯。

### 7.3 A7-17｜P2｜审计 actor 恒为 admin，写库失败仅 WARN

审计回调构造函数把 `Actor` 固定为 `"admin"`，见 `cmd/gateway/main.go:533-551`；store 层的 `AuditEntry` 注释也承认这是已知不精确，见 `internal/store/audit_dao.go:12-20`。普通用户修改自己的 key/密码时，日志无法区分为具体用户。审计写库失败只记 WARN、不阻断也不回传可查询的失败状态，见 `cmd/gateway/main.go:548-550`。A1 已记录该架构问题，见 `work/audit-2026-10-20/A1-architecture.md:170-173,190-206`，A7 在此只保留管理面审计完整性影响，不重复展开架构修法。

### 7.4 敏感信息边界

- 审计只记录请求体顶层字段名，不记录值，避免密码与 API key 明文，见 `internal/server/autoreload.go:28-30,143-155` 与 `internal/store/audit_dao.go:12-19`。
- Provider credential 响应只回掩码，解密失败回 `****`，见 `internal/admin/credential_handler.go:223-231`；未发现普通响应包含 password_hash。
- `config-export` 必须加密且有 passphrase，见 `internal/admin/config_transfer_handler.go:37-45,47-60`。

## 8. 模块边界：admin 反向依赖 server、handler 直写 DB

### 8.1 A7-18｜P2｜admin → server 的反向依赖

`internal/admin` 大量使用 `server.UserFromContext`、`server.FailureThrottle`、`server.SetSessionCookie`、`server.SameOrigin` 等，例如 `internal/admin/user_handler.go:197,289,297,493,600,631,654`；而 `internal/server` 又通过 `AdminGateGuard` 接受 admin 的真实 mux，见 `internal/server/user_auth.go:222-253` 与 `cmd/gateway/main.go:526-531`。运行期装配掩盖了类型层面的反向依赖。A1 已详细记录依赖方向与鉴权口径分散，见 `work/audit-2026-10-20/A1-architecture.md:151-206`；A7 只补充“会话 cookie、限速和审计回调都由 server 提供的跨模块契约”，不把它重复列为新的架构发现。

### 8.2 handler 直写 DB 的检查

未发现 `internal/admin` 直接使用 `database/sql` 写调用或 `store.db.*`。管理 handler 的 SQL 读取限定为 `store.Reader().Query/QueryRow`，例如 `internal/admin/usage_handler.go:315,365,581,587,696,702,835,934`、`internal/admin/model_group_handler.go:35`。写操作均经 store DAO 方法边界完成；这是已验证的正面区域，除非将来新增原始 SQL，否则不建议为了形式统一而重构。

### 8.3 A4 / A9 交叉契约

- A4 已覆盖数据面热路径的鉴权顺序、禁用/过期/归属错误码以及管理员不能调用模型，见 `work/audit-2026-10-20/A4-hotpath-billing.md:1175-1235`。A7 新增的是管理面 key 创建策略与对象级归属，不重复 A4 的热路径门。
- A9 已覆盖前端 token/sessionStorage/401 总论，见 `work/audit-2026-10-20/A9-frontend.md:480-521`。A7 只补一个后端契约：禁用账号在鉴权链返回 403，见 `internal/server/user_auth.go:303-310`，而前端只对 401 清 token，见 `web/src/api.ts:235-245,303-308`；因此被禁用用户可能停留在管理壳中并反复收到 403。该行为是否由产品统一成 401 或由前端额外处理 403，尚未确认，A7 不重复 A9 的 401 设计评估。

## 9. 问题清单

### P0

无。

### P1

1. **A7-02 角色/状态与会话失效非原子**：`internal/admin/user_admin_handler.go:491-506`、`internal/store/user_dao.go:259-278,455-465`。写已提交而吊销失败时返回 500，旧 token 在自然过期前仍有效。
2. **A7-03 Logout 吊销失败仍 200**：`internal/admin/user_handler.go:630-650`、`web/src/api.ts:191-198`。用户被告知已登出，但服务端复制 token 仍可用。
3. **A7-13 sameOrigin 忽略 scheme**：`internal/server/server.go:146-174`、`internal/server/user_auth.go:323-357`、`internal/admin/user_handler.go:489-496`。同 host 不同 scheme 被误判同源，Cookie 又未设 Secure。
4. **A7-15 Bootstrap 失败不审计**：`internal/server/autoreload.go:187-195`、`internal/admin/user_handler.go:564-571`。错误令牌和重复初始化等攻击/误操作信号不落 audit。
5. **A7-16 usage/prune keep_days 不进入审计**：`internal/admin/usage_handler.go:944-982`、`internal/server/autoreload.go:59-155`。不可逆删除缺少参数留痕，且与 handler 注释相矛盾。

### P2

1. **A7-01 授权契约分散**：`internal/server/user_auth.go:161-253`、`internal/admin/user_admin_handler.go:158-172`、`internal/admin/key_handler.go:506-539`、`internal/admin/usage_handler.go:175-198`。当前行为正确，但新增端点容易漏掉对象级收窄。
2. **A7-04 Cookie 优先遮蔽有效 Bearer**：`internal/server/user_auth.go:371-397`、`web/src/api.ts:209-212,282-285`。同源与认证通道契约不清晰；「待确认」是否会把浏览器和脚本的登出状态互相覆盖。
3. **A7-05 反代下错误 token 限速聚合到代理 IP**：`internal/server/server.go:187-204`、`internal/server/user_auth.go:270-295`、`README.md:93-101`。是否需要可信代理配置「待确认」。
4. **A7-06 管理员可给另一管理员充值**：`internal/admin/user_admin_handler.go:615-697`、`internal/store/topup_dao.go:159-213`。只拦自充值，目标角色未校验。
5. **A7-07 Group PATCH 清空 description**：`internal/admin/group_handler.go:103-106,164-180`、`internal/admin/group_handler_test.go:83-93`。字段合并缺陷。
6. **A7-08 Key PATCH 认领两步写**：`internal/admin/key_handler.go:670-681`、`internal/store/key_dao.go:304-343`。前一步成功、后一步失败时出现部分提交。
7. **A7-09 KeyHandler.List 无身份全量回退**：`internal/admin/key_handler.go:106-126`。生产不可达，但 handler 单独复用时 fail-open。
8. **A7-10 / A7-11 Network、ModelGroup 的 PATCH 必填耦合**：`internal/admin/network_handler.go:97-116`、`internal/admin/model_group_handler.go:104-126`。当前前端总发全字段，契约仍不一致。
9. **A7-12 Upstream model_id 可改名但定位符契约不清**：`internal/admin/model_handler.go:343-350`、`internal/store/model_dao.go:198-205`、`internal/admin/usage_handler.go:785-797,813-831`。是否允许改名「待确认」。
10. **A7-14 Login 无显式 SameOrigin 门禁**：`cmd/gateway/main.go:374`、`internal/admin/user_handler.go:164-305`。登录 CSRF 与错误会话覆盖的残余面，依赖浏览器与 Cookie 策略。
11. **A7-17 审计 actor 恒为 admin、审计失败只 WARN**：`cmd/gateway/main.go:533-551`、`internal/store/audit_dao.go:12-20`。审计无法回答“具体是谁”，且审计落库失败不进入健康状态。
12. **A7-18 admin → server 反向依赖**：`internal/admin/user_handler.go:197,289,297,493,600,631,654`、`internal/server/user_auth.go:222-253`、`cmd/gateway/main.go:526-531`。架构一致性债务。

### P3

1. **A7-19 登录限速兼容死分支**：`internal/admin/user_handler.go:186-225` 的 `th == nil` 分支在当前生产构造下不可达，但与账号限速的成功清零逻辑并列，容易被误读为两套限速可独立关闭。
2. **A7-20 `usageHandler.Prune` 注释仍写旧的前缀白名单模型**：`internal/admin/usage_handler.go:944-949` 与实际精确路由白名单 `internal/server/user_auth.go:119-207` 不一致。
3. **A7-21 `BootstrapTokenRequired` 无法区分“没有记录”和“已被消费为空”**：语义注释见 `internal/store/settings_dao.go:51-63`，虽然权威校验在事务内执行 `internal/store/user_dao.go:371-393`，但公开状态文案未来可能把异常态显示成“无需令牌”。当前状态由 `internal/admin/user_handler.go:422-441` 处理，未发现直接利用路径。

## 10. 已验证无问题的区域（含理由）

- **路由无重复、无匿名漏网、无普通用户进入 admin-only handler**：真实路由矩阵从 `main.go` 提取 81 条，三层身份断言见 `internal/server/authz_matrix_test.go:417-526`；管理员分支还要求非空 ID，见 `internal/server/user_auth.go:235-238`。
- **未知路由 fail-closed**：普通用户只放行方法+注册模式的精确白名单，未注册模式为 403，见 `internal/server/user_auth.go:240-253`。
- **key 跨用户 IDOR 已封堵且失败无副作用**：`ownedByCaller` 对普通用户跨归属回 404，见 `internal/admin/key_handler.go:506-539`；真实链测试同时查库确认名字、enabled、owner 未变，见 `internal/server/authz_matrix_test.go:1112-1190`。
- **普通用户用量、充值流水、探测数据 scope 正确**：`callerScope` 空身份返回不可匹配哨兵，见 `internal/admin/usage_handler.go:175-198`；用量隔离测试见 `internal/server/authz_matrix_test.go:910-1110`；Topups 身份收窄见 `internal/admin/user_admin_handler.go:775-803`；Probe 白名单收窄见 `internal/admin/probe_handler.go:108-138,167-243`。
- **不能自删、自禁用、自降级，且失败不写库**：测试见 `internal/admin/scope_test.go:340-395`；空密码用户不能升级 admin，见 `internal/admin/user_admin_handler_test.go:90-110`。
- **管理员不能给自己充值，余额不变**：测试见 `internal/server/authz_matrix_test.go:870-908`；本次另发现仅“给另一名管理员充值”未拦，见 A7-06，不否认自充值保护本身。
- **Bootstrap 令牌使用一次性事务、恒定时间比较和失败关闭**：校验与密码、completed 标记、清空令牌在同一事务，见 `internal/store/user_dao.go:331-439`；事务测试见 `internal/store/user_dao_test.go:355-405`；公开响应只给 token 是否要求的布尔值，见 `internal/admin/user_handler.go:378-394`。
- **登录不存在/密码错误统一文案且做伪哈希，账号与 IP 双维限速**：见 `internal/admin/user_handler.go:186-305`；有效登录清零两道额度，见 `internal/admin/user_handler.go:288-296`。
- **改密原子失效旧 token**：`SetUserPassword` 在 SQL 内递增 `auth_version`，见 `internal/store/user_dao.go:281-291`；黑盒测试见 `internal/server/authz_matrix_test.go:1319-1372`。
- **CORS、Cookie、CSRF 的既有防线有效**：`/admin` 不放行 CORS，见 `internal/server/server.go:124-143`；Cookie 为 HttpOnly、SameSite=Strict、Path=/admin，见 `internal/server/user_auth.go:399-425`；Content-Type 门禁见 `internal/admin/helpers.go:101-147`；无 body 的跨源 cookie 写请求被拒绝，见 `internal/server/csrf_middleware_test.go:129-165`。
- **安全响应头与错误信息收敛**：CSP、点击劫持、MIME、缓存控制见 `internal/server/server.go:458-495`；store 内部错误对外仅回“服务内部错误”，见 `internal/admin/helpers.go:43-50`。
- **审计不落敏感值**：只采集顶层字段名并排序，见 `internal/server/autoreload.go:28-30,143-155`；持久化字段仅含 method/path/status/fields/remote，见 `internal/store/audit_dao.go:12-38`。
- **admin 无原始 DB 写调用**：审计检查的写路径均经 store 方法；读取使用 `Reader().Query/QueryRow`，示例见 `internal/admin/usage_handler.go:315,365,581,587,696,702,835,934` 与 `internal/admin/model_group_handler.go:35`。

## 11. 验证与报告边界

本轮运行：

```text
go test ./internal/server ./internal/admin ./internal/userauth -count=1
go test ./internal/server -run TestAuthzMatrix_GatewayLayer -count=1 -v
```

矩阵结果：`管理面权限矩阵（网关层）共 81 条路由`，全部 PASS。

报告的边界如下：

- A1 已覆盖 admin → server 依赖方向和鉴权口径总论，见 `work/audit-2026-10-20/A1-architecture.md:151-206`；A7 只补管理面具体影响。
- A4 已覆盖热路径鉴权顺序与错误码，见 `work/audit-2026-10-20/A4-hotpath-billing.md:1175-1235`；A7 不重复热路径门。
- A9 已覆盖前端 token/401 总论，见 `work/audit-2026-10-20/A9-frontend.md:480-521`；A7 只记录禁用账号后端 403 与前端仅处理 401 的交叉契约。
- 未修改任何源码、测试、构建产物或既有 A1-A9 报告；本报告是唯一新增文件。工作树基线中的 `internal/webui/dist`、`web/src/views/UsageDetail.vue`、`AGENTS.md`、`docs/` 属于审计前既有状态，未被本次操作触碰。
