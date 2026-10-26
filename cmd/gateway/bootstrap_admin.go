package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cn-maul/rosetta-gateway/internal/lifecycle"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// bootstrapAdminUsername 是引导出的管理员账号名。
//
// 固定值而不是可配置：多用户阶段还没有「建管理员」的管理界面，
// 这是唯一的入口。用户名冲突时（用户已自建了叫 admin 的账号）
// 引导会跳过并记 WARN，绝不覆盖别人的账号。
const bootstrapAdminUsername = "admin"

type adminBootstrapOutcome string

const (
	adminBootstrapSetupRequired adminBootstrapOutcome = "setup_required"
	adminBootstrapReady         adminBootstrapOutcome = "ready"
	adminBootstrapFailed        adminBootstrapOutcome = "failed"
)

type adminBootstrapStore interface {
	CountUsers(ctx context.Context) (int, error)
	CreateUser(ctx context.Context, u *store.User) error
	GetUserByUsername(ctx context.Context, username string) (*store.User, error)
	FindUninitializedAdmin(ctx context.Context) (*store.User, error)
}

// initializeAdminBootstrap is the startup hook injected into gatewayapp.Run.
// The implementation stays in this package so bootstrap tests can exercise
// the exact production function.
func initializeAdminBootstrap(ctx context.Context, db *store.Store,
	logger *slog.Logger) (lifecycle.BootstrapState, error) {
	outcome, err := ensureBootstrapAdmin(ctx, db, logger)
	if err != nil {
		return lifecycle.BootstrapFailed, err
	}
	logger.Info("admin bootstrap state resolved", "outcome", outcome)
	return lifecycle.BootstrapState(outcome), nil
}

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
// 负责设密码并直接签发会话。
//
// # 首次设置窗口
//
// 完成引导的端点是免鉴权的，且容器通常监听 0.0.0.0，因此在管理员设好密码
// 之前，局域网内能访问该端口的人都可以抢先完成设置。这是局域网自用部署
// 明确接受的风险。窗口本身仍由 bootstrap_completed 标记与原子条件 UPDATE
// 保证只开一次，设密后永久关闭。
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
func ensureBootstrapAdmin(ctx context.Context, db adminBootstrapStore,
	logger *slog.Logger) (adminBootstrapOutcome, error) {
	n, err := db.CountUsers(ctx)
	if err != nil {
		// 状态未知时既不能引导，也不能让进程假装已经就绪。
		return adminBootstrapFailed, fmt.Errorf("read bootstrap user count: %w", err)
	}
	if n > 0 {
		return resolveExistingAdminBootstrap(ctx, db)
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
		// 唯一约束冲突只在重读证明另一个实例确实建出了管理员后才可接受。
		if store.IsUniqueViolation(err) {
			return resolveConcurrentAdminBootstrap(ctx, db, err)
		}
		return adminBootstrapFailed, fmt.Errorf("create initial administrator: %w", err)
	}

	logger.Info("created initial administrator account; set its password on first visit",
		"username", u.Username,
		"next_step", "打开管理后台的登录页会显示「首次设置密码」表单；设置完成后即可登录")
	return resolveExistingAdminBootstrap(ctx, db)
}

// resolveExistingAdminBootstrap turns the existing durable user state into the
// explicit setup/ready outcome. A required post-bootstrap read failure is fatal:
// callers must not infer "ready" from an unknown identity state.
func resolveExistingAdminBootstrap(ctx context.Context, db adminBootstrapStore) (
	adminBootstrapOutcome, error) {
	pending, err := db.FindUninitializedAdmin(ctx)
	if err != nil {
		return adminBootstrapFailed, fmt.Errorf("read uninitialized administrator: %w", err)
	}
	if pending != nil {
		return adminBootstrapSetupRequired, nil
	}
	return adminBootstrapReady, nil
}

// resolveConcurrentAdminBootstrap accepts a unique create conflict only when a
// re-read proves that the expected administrator now exists. The original
// error code alone is not evidence that bootstrap completed.
func resolveConcurrentAdminBootstrap(ctx context.Context, db adminBootstrapStore,
	createErr error) (adminBootstrapOutcome, error) {
	n, err := db.CountUsers(ctx)
	if err != nil {
		return adminBootstrapFailed, fmt.Errorf(
			"verify concurrently-created administrator count after %v: %w", createErr, err)
	}
	if n == 0 {
		return adminBootstrapFailed, fmt.Errorf(
			"initial administrator insert conflicted but users table is still empty: %w", createErr)
	}
	admin, err := db.GetUserByUsername(ctx, bootstrapAdminUsername)
	if err != nil {
		return adminBootstrapFailed, fmt.Errorf(
			"verify concurrently-created administrator after %v: %w", createErr, err)
	}
	if admin == nil || admin.Role != store.RoleAdmin {
		return adminBootstrapFailed, fmt.Errorf(
			"initial administrator insert conflicted but no administrator exists: %w", createErr)
	}
	outcome, err := resolveExistingAdminBootstrap(ctx, db)
	if err != nil {
		return adminBootstrapFailed, errors.Join(createErr, err)
	}
	return outcome, nil
}
