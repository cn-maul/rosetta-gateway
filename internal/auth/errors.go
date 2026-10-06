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
)
