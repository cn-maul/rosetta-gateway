package auth

import "errors"

var (
	ErrNoKey       = errors.New("missing API key")
	ErrInvalidKey  = errors.New("invalid API key")
	ErrKeyDisabled = errors.New("API key disabled")
	// ErrUserDisabled 表示 key 有效但其归属用户已被管理员禁用。
	//
	// 刻意与 ErrKeyDisabled 分开：两者的处置不同 —— key 被禁用是「这把钥匙
	// 坏了」，用户被禁用是「这个人的门锁了」，管理面要能区分以便排查。
	ErrUserDisabled = errors.New("API key owner is disabled")
	// ErrKeyUnowned 表示 key 没有归属用户（user_id 为空，或指向已不存在的行）。
	//
	// 多用户改造的既定决策：升级后不发新 key 就不给用。存量无归属 key 走这条
	// 路径返回 401，客户端重新配一次即可 —— 明确失败好过静默放行，
	// 后者会让「谁的key 在用」彻底无从追查。
	ErrKeyUnowned = errors.New("API key has no owner; please issue a new key")

	// ErrKeyExpired 表示密钥已过有效期（P2）。
	//
	// 刻意与 ErrKeyDisabled 分开：停用是管理员的**主动**动作，过期是时间
	// 自然到达。运维排查时两者处置完全不同（一个是配置问题，一个是
	// 「该续期还是该换 key」），合成一个错误会让排障方向跑偏。
	ErrKeyExpired = errors.New("API key has expired")

	// ErrIPNotAllowed 表示请求来源不在该密钥的 IP 白名单内（P2）。
	ErrIPNotAllowed = errors.New("source IP not allowed for this API key")

	// ErrAdminCannotCallModel 表示这把 key 的归属用户是**管理员**，而管理员
	// 被禁止走数据面（/v1 调用模型）。
	//
	// # 业务语义（2026-10 需求确认）
	//
	// 「管理员账号只负责管理网关」：调用模型、消耗上游额度一律由**普通用户**
	// 完成，管理员需要先建普通用户账户、发 key 给对方来调 API。于是管理员
	// 在数据面彻底退出，只保留控制面管理权。
	//
	// # 为什么刻意与 ErrKeyDisabled / ErrUserDisabled 分开
	//
	// 三者的**处置动作完全不同**，塌成一类会让运维与管理员都走错方向：
	//   - ErrKeyDisabled     → 这把钥匙坏了，去发/换一把；
	//   - ErrUserDisabled    → 这个人的门锁了，去解禁账号；
	//   - ErrAdminCannotCallModel → 钥匙和账号**都是好的**，只是角色不允许。
	//     正确动作是「新建一个普通用户，用那个账号的 key 调模型」。
	//
	// 混进 ErrKeyDisabled/ErrUserDisabled 会让管理员以为是 key 或账号坏了，
	// 白白排查。所以单独一条错误，配套单独的状态码与 code（见 main.go 的
	// writeAuthError 与 outwire.errorTypeFromCode）。
	//
	// # 存量管理员 key 的处置：升级后**立即失效**（fail-fast）
	//
	// 若保留管理员旧 key 可用，等于「要求」被架空 —— 存量 key 会继续消耗上游
	// 额度且不受余额约束（balanceExempt），同时管理员绕过控制面直接吃数据面。
	// 立刻失效 + 清晰错误消息，让管理员明确知道「要去建普通用户」，而不是看到
	// 一个含糊的 403 去反复重试。
	ErrAdminCannotCallModel = errors.New("administrator accounts cannot call models; create a regular user account and use its API key")
)
