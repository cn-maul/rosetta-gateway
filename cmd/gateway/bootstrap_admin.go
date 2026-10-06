package main

import (
	"context"
	"log/slog"

	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// bootstrapAdminUsername 是引导出的管理员账号名。
//
// 固定值而不是可配置：多用户阶段还没有「建管理员」的管理界面，
// 这是唯一的入口。用户名冲突时（用户已自建了叫 admin 的账号）
// 引导会跳过并记 WARN，绝不覆盖别人的账号。
const bootstrapAdminUsername = "admin"

// ensureBootstrapAdmin 在 users 表为空时建出一个待初始化的 admin 账号。
//
// # 它解决的是什么问题
//
// 统一认证后（admin_token 与 admin_auth.json 两条通道已删除），users 表是
// **唯一**的身份来源。于是新部署面临一个空档：一张空库，没有任何账号，
// 而「建第一个账号」这个动作本身需要身份 —— 先有鸡还是先有蛋。
//
// 解法是建一个**登不进去但明确存在**的账号：
// 它的 password_hash 为空，登录校验必然失败，但 `GET /admin/api/bootstrap`
// 能据此告诉前端「请为 admin 设置密码」，而 `POST /admin/api/bootstrap`
// 负责设密码并直接签发会话。整条链路自洽，无需任何旁路凭据。
//
// # 为什么 password_hash 留空
//
// 留一个假的哈希值会让登录路径无法区分「还没设过密码」与「密码错了」，
// 用户只会收到「用户名或密码错误」然后反复重试。空串是一个**可判定**的状态。
//
// # 旧部署怎么办
//
// 存量部署升级过来时 users 表为空，会建出同样的 admin 账号。原先用
// admin_token 或 admin_auth.json 的人需要做一次「设密码」——
// 这是有意的行为变更，且是唯一一次。好处是从此只有一个身份来源。
//
// # 幂等
//
// 判定条件是「users 表为空」，不写任何标记位。重复调用是 no-op ——
// 用户自己建了账号之后就不再引导，不会把他们的库搅乱。
func ensureBootstrapAdmin(ctx context.Context, db *store.Store, logger *slog.Logger) {
	n, err := db.CountUsers(ctx)
	if err != nil {
		// 读不到用户数是个异常状态（数据库层面的问题）。此时**不做引导**：
		// 宁可让人手工建管理员，也不要在状态不明时往库里塞一个身份。
		logger.Error("bootstrap admin: cannot count users; skipping", "error", err)
		return
	}
	if n > 0 {
		return // 已有用户，不引导
	}

	u := &store.User{
		ID: generateID(),
		// PasswordHash 留空：见函数注释「为什么 password_hash 留空」。
		PasswordHash: "",
		Username:     bootstrapAdminUsername,
		DisplayName:  "管理员",
		Role:         store.RoleAdmin,
		Status:       store.UserStatusActive,
		AuthVersion:  1,
		Remark:       "首次启动自动创建；请在登录页设置密码",
	}
	if err := db.CreateUser(ctx, u); err != nil {
		// 唯一约束冲突说明并发启动时另一个实例已经建好了 —— 不是错误。
		if store.IsUniqueViolation(err) {
			logger.Info("bootstrap admin skipped: created concurrently by another instance")
			return
		}
		logger.Error("bootstrap admin: create failed", "error", err)
		return
	}

	logger.Info("created initial administrator account; set its password on first visit",
		"username", u.Username,
		"next_step", "打开管理后台的登录页会显示「首次设置密码」表单；"+
			"设置完成后即可用该账号登录（不需要任何其他凭据）")
}
