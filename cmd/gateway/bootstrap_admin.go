package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"

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

// bootstrapTokenEnv 是引导令牌的环境变量名。
//
// 生产推荐**显式设置**它，理由与 master.key 同类：自动生成的令牌只出现在
// 容器日志里，日志会随轮转与聚合平台留存 —— 那等于把「谁还没完成引导」
// 这条信息的攻击面从「能连到端口」扩大到「能读日志」。
const bootstrapTokenEnv = "ROSETTA_GW_BOOTSTRAP_TOKEN"

// ensureBootstrapToken 在「本安装尚未完成引导」时布置一个一次性令牌。
//
// # 为什么需要它
//
// ensureBootstrapAdmin 建出的 admin 密码为空，而完成引导的
// POST /admin/api/bootstrap 是**免鉴权**的。容器默认 listen 0.0.0.0
// （否则 `-p 8666:8666` 映射不出去），所以「谁先打开页面谁就是管理员」
// 是一个真实的抢注窗口，而不是理论风险：curl 没有 Origin 可校验，
// SameOrigin 那一层对它形同虚设。
//
// # 为什么不用「改默认 listen 为 127.0.0.1」
//
// 那样确实能关掉窗口，但会让 `-p 8666:8666` 立刻失效：容器内只听回环，
// 端口映射进来的流量到不了监听套接字。Docker 开箱部署会直接坏掉，
// 而「默认能跑起来」正是这个部署方式被大量使用的原因。
//
// # 令牌的生命周期
//
// 1. 启动时：未完成引导 → 取环境变量或生成随机值写入 app_settings；
// 2. 使用时：POST /bootstrap 必须带上它，比对在写事务内做；
// 3. 消费时：与「写密码」「置 bootstrap_completed」同一个事务清空。
//
// 三者同进同退，所以不存在「设过密但令牌还在」（攻击者仍可重放）或
// 「令牌清了但密码没设上」（管理员被永久锁在门外）这两种中间态。
func ensureBootstrapToken(ctx context.Context, db *store.Store, logger *slog.Logger) {
	done, err := db.BootstrapCompleted(ctx)
	if err != nil {
		// 读不到状态时**不**生成：宁可这次部署没有令牌（回到旧的抢注风险、
		// 但有启动日志里的 ERROR 告警），也不要往一个状态不明的库里
		// 塞一条此后无法解释的凭据。
		logger.Error("bootstrap token: cannot read completed marker; skipping",
			"error", err,
			"impact", "本次部署的引导窗口没有令牌保护，请尽快完成首次设置密码")
		return
	}
	if done {
		// 已完成引导：不再留令牌。已经完成过的安装也不该因为升级
		// 而凭空多出一段日志和一个需要处置的凭据。
		return
	}
	if tok, err := db.BootstrapToken(ctx); err != nil {
		logger.Error("bootstrap token: cannot read existing token; skipping", "error", err)
		return
	} else if tok != "" {
		// 重启时令牌已存在：**不要**覆盖、不要重复打印。
		// 覆盖会让运维手里那份失效；重复打印等于在每次重启时把同一个
		// 凭据再往日志里摊一次。
		logger.Info("bootstrap token already set; reuse the one printed at first start",
			"hint", "日志若已轮转丢失，请删除该部署的 app_settings.bootstrap_token 后重启以重新生成")
		return
	}

	token := os.Getenv(bootstrapTokenEnv)
	fromEnv := token != ""
	if fromEnv {
		// 环境变量里的令牌是**运维自己选**的值，强度只有运维自己清楚。
		// 静默接受一个 "1234" 等于把整个引导窗口的强度交给一个猜测成本
		// 接近于零的凭据 —— 那不如没设。直接拒绝，让它在部署清单里
		// 就是错的，而不是等到出事才发现。
		if err := validateBootstrapToken(token); err != nil {
			logger.Error("bootstrap token: "+bootstrapTokenEnv+" 被拒绝，引导窗口没有令牌保护",
				"error", err,
				"immediate_action", "设置一个至少 32 字节、建议 32 字节随机 hex 的令牌后重启；"+
					"或移除该环境变量，让本次启动自行生成",
				"accepted_length", bootstrapTokenMinLen)
			return
		}
	} else {
		var err error
		if token, err = newBootstrapToken(); err != nil {
			// 随机源失败 = **绝不写库**。写一个空 token 会在库里留下
			// 「配了令牌但值是空」的记录，把「状态不明」伪装成「已加固」，
			// 后面谁都说不清引导窗口到底受没受保护。
			logger.Error("bootstrap token: crypto/rand failed;引导窗口没有令牌保护",
				"error", err,
				"immediate_action", "立即完成首次设置密码，或用 "+bootstrapTokenEnv+" 显式指定令牌后重启")
			return
		}
	}
	if err := db.SetBootstrapToken(ctx, token); err != nil {
		// 写失败 = 本次部署没有令牌保护。必须显式告警，而不是静默继续：
		// 运维需要知道引导窗口此刻是**裸奔**的。
		logger.Error("bootstrap token: persist failed;引导窗口没有令牌保护",
			"error", err,
			"immediate_action", "立即在浏览器打开 /admin/ 完成首次设置密码；"+
				"在此之前不要把该端口暴露到公网")
		return
	}

	// 令牌只打一次、只进日志。绝不能进 HTTP 响应：BootstrapStatus 是
	// 免鉴权端点，把令牌放进去等于取消了这个令牌。
	logger.Warn("首次引导令牌已生成 —— 完成首次设置密码时必须填写它",
		"token", token,
		"source", map[bool]string{true: "环境变量 " + bootstrapTokenEnv, false: "本次启动随机生成"}[fromEnv],
		"how_to_use", "打开 /admin/ 的「首次设置密码」表单，粘贴此令牌后设置密码；"+
			"设置成功后本令牌立即作废",
		"recommendation", "生产环境建议显式设置 "+bootstrapTokenEnv+
			"，避免令牌只能从日志里获取")
}

// newBootstrapToken 生成 32 字节（256 bit）CSPRNG 随机值并 hex 编码。
//
// 32 字节不是随手取的：这是**唯一的**免鉴权写入口的唯一凭据，
// 强度下限就该按长期凭据给。读随机源失败时**返回 error 且不写库**
// （此时窗口仍会被启动告警覆盖），而不是回落到 time.Now()/计数器
// 这类可预测值 —— 拿一个可猜的令牌去「修」一个抢注窗口，等于没修。
//
// 更不能把空串写进库：那会留下一个「配了令牌但值是空」的记录，
// 把状态不明的窗口伪装成已加固的窗口。
func newBootstrapToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("read crypto/rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// bootstrapTokenMinLen 是引导令牌要求的最小长度（字节）。
//
// 32 字节与自动生成时的输出强度对齐 —— 自动生成的令牌是 32 字节随机
// hex（64 字符），环境变量就该至少提供同等量级的熵，而不是让「用自己
// 喜欢的口令」成为绕过强度下限的后门。
const bootstrapTokenMinLen = 32

// validateBootstrapToken 校验运维显式提供的引导令牌。
//
// 只查长度，不查字符集：口令强度无法靠字符集判断，而过度严格的
// 规则只会把人推向 `Passw0rd!` 这种「满足规则但可预测」的值。
// 长度是唯一能被机械保证的下限，剩下的交给运维与生成器。
func validateBootstrapToken(token string) error {
	if n := len(token); n < bootstrapTokenMinLen {
		return fmt.Errorf("token is %d bytes, want at least %d", n, bootstrapTokenMinLen)
	}
	return nil
}
