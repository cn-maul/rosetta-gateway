# 任务 9：侧边栏重排与路由调整（Lead）

日期：2026-10-10

## 需求原文

> 我觉得前端页面在侧边栏的顺序和内容需要调整：1、总览 2、上游与模型 3、路由
> 4、用户与分组 5、调用历史 6、设置。其中用户和分组合并到一个页面里面，
> 管理员的可用模型删掉……

## 改动

### 侧边栏（`web/src/App.vue`）

最终顺序：

```
总览                      ← 所有人
访问密钥                  ← 所有人
钱包与充值                ← 所有人（原「我的账号」，2026-10-09 已加待结算展示）
───────────────────────── 以下 admin-only
上游与模型                ← 原「上游」+「模型」两页合一（上游卡片内已有模型区）
路由
用户与分组                ← 原「用户」+「分组」两入口合一
调用历史                  ← 从 admin 区外移入
设置
```

几个需要说明的判断：

1. **「访问密钥」和「钱包与充值」留在 admin 区之外**。需求只列了 6 项（都是
   admin 视角），但普通用户只有这两个入口可用——它们如果也塞进 admin 区，
   普通用户的侧边栏会变成空白。管理员不该调 API 了，但仍然需要看自己的余额
   （不限额）与改自己的密码，所以这两项两边都显示。

2. **「调用历史」移进 admin 区**。它现在是查全站账的台账；普通用户看自己的
   消费记录应该走「钱包与充值」页，那里有精简的最近记录 + 跳转入口。

3. **「可用模型」页整体删除**（路由 `/models` + `Models.vue`）。它原本是
   「我能用哪些模型」的只读清单，注释里写着「管理员也能进，看到的是全集，
   无害」——但管理员既然不再调用 API，这份清单对ta 就是无用信息，而且它把
   **上游模型拓扑**（provider 下挂了哪些模型）直接摊在界面上，比密钥名称
   更接近生产配置。

   普通用户没有因此失去能力：他们在「访问密钥」页的可用模型选择器里能看到
   同一份数据（`ModelPicker`，同一个 `/admin/api/model-names` 端点）。

## 路由表（`web/src/router.ts`）

| path | name | 组件 | meta |
|---|---|---|---|
| `/login` | `login` | `Login.vue` | `public` |
| `/` | `overview` | `Overview.vue` | — |
| `/keys` | `keys` | `Keys.vue` | — |
| `/history` | `history` | `History.vue` | — |
| `/profile` | `profile` | **`Wallet.vue`** | `title: 钱包与充值` |
| `/users` | `users` | **`UsersGroups.vue`** | `adminOnly`, `title: 用户与分组` |
| `/providers` | `providers` | `Providers.vue` | `adminOnly` |
| `/routes` | `routes` | `Routes.vue` | `adminOnly` |
| `/settings` | `settings` | `Settings.vue` | `adminOnly` |

已删除：`/groups`（并入 `/users`）、`/models`。

### 两个刻意保留的 `profile`（钱包页）

路由的 `path` 与 `name` **保持 `/profile` / `profile` 不变**，只有展示标题改成
「钱包与充值」。原因：路由名已经被写进三处跳转——

- `router.ts:83` 守卫把非 admin 踢回 `{ name: 'profile' }`
- 登出后 `App.vue` push 回 `{ name: 'profile' }`
- 旧书签 / 外部链接指向 `/profile`

改成 `/wallet` 会让这些全部要跟着改，且老书签直接 404。展示给用户看的文字
在 `meta.title` 和侧边栏文案里，与路径无关——用户不会看到 `/profile` 这个
字符串。

## 未做的事（需要其他任务配合）

- `UsersGroups.vue` 与 `Wallet.vue` 由 `users-groups-ui` / `wallet-ui` 两位
  teammate 实现；本任务只负责接线（路由表 + 侧边栏）。
- 管理员「不能调 /v1、不能建 key」是 `admin-lockout` 的后端任务。
  **注意副作用**：管理员的「访问密钥」页届时会变成一个只能看的空页面
  （他无法再建 key，也没有历史 key 可看）。如果这影响体验，需要产品决定
  是否对 admin 隐藏整个「访问密钥」入口——我暂时保留，因为存量 admin key
  在升级后仍会存在，管理员可能需要看到并清理它们。

## 验证

```
vue-tsc --noEmit   → exit 0
```

（两位 teammate 的组件此时已落盘，所以类型检查能一起跑通。构建产物同步
`npm run sync` 由 Lead 在全部任务合并后统一执行。）