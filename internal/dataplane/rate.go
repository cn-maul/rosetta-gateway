package dataplane

import (
	"context"
	"log/slog"

	"github.com/cn-maul/rosetta-gateway/internal/ratelimit"
	"github.com/cn-maul/rosetta-gateway/internal/store"
)

// RateCommit 把一次请求的两种预占（TPM 窗口 + 终身配额）在请求终结时收尾。
// nil 接收者安全：TPM 未启用（额度 0）时调用方可以放一个 nil ——
// 现在主路径在 TPMLimit==0 时干脆不建这个结构，Commit 就是纯 no-op。
type RateCommit struct {
	limiter  *ratelimit.Limiter
	keyID    string
	limit    int // 预占时的 tpmLimit；0 = 未启用，CommitTPM 据此直接返回
	reserved int64

	// store 用于**终身配额**的预占退回；为 nil 时跳过（不限额或测试）。
	store  *store.Store
	logger *slog.Logger
	// quotaReserved 是本次请求预占的 token 数（0 = 未预占，或已释放）。
	quotaReserved int64
	// userQuotaReserved 是本次请求在**用户级**配额上预占的 token 数
	// （0 = 未预占，或已释放）。
	//
	// 与 quotaReserved 分列而不是合并成一个计数：两级配额的判定口径不同
	// （key 级是 access_keys.quota_tokens，用户级是 users.quota_tokens），
	// 且**可能只命中一级** —— 只配了用户额度的用户，key 级预占恒为 0。
	// 合并成一个数会在「用户配了额度、key 没配」时把用户级预占错记成
	// key 级，退预占时去动一张不该动的表，或反之。
	userQuotaReserved int64
	// userID 是 userQuotaReserved 的归属用户；退预占时必须用它定位行。
	userID string
}

// Commit 用量已知时的**全量**收尾：TPM 按真实用量校正 + 退配额预占。
//
// 正常路径一律走这里，别为单一预占另起入口 —— 两种预占共用同一个 est 与
// 同一个收尾，分头调用时容易只改一处而漏掉另一处，且配额那条漏掉是
// **终身**偏差，不像 TPM 窗口会自愈。唯一允许绕开 Commit 的路径是
// usage missing（见 ReleaseQuota 的说明）。
func (rc *RateCommit) Commit(actual int64) {
	if rc == nil {
		return
	}
	rc.limiter.CommitTPM(rc.keyID, rc.limit, rc.reserved, actual)
	rc.ReleaseQuota()
}

// ReleaseQuota 只退终身配额的预占，**不碰 TPM 窗口**。
//
// 独立成方法是因为有一条真实路径必须拆开收尾：上游没报 usage
// （usage_state="missing"）时，TPM 预占要**故意保留**——Commit(0) 会全额退还
// 预占，等于不报用量的上游完全绕过 TPM；保留估算值让限速继续约束这类流量，
// 估算值随窗口翻转自然清零。但配额预占必须退掉：它是终身累计，预占按
// max_tokens 全额计，不退就等于每次 missing 请求永久吃掉数万 token 额度
// （2026-10-07 修复的 P1-3 —— 修复前 missing 时整个 Commit 被跳过）。
func (rc *RateCommit) ReleaseQuota() {
	if rc == nil || rc.store == nil {
		return
	}
	// 两级预占各自「先取后清零」，让释放天然幂等：任何路径重复收尾（或未来
	// 重构引入第二次调用）时第二次是空操作，不会把别的请求刚立起来的预占退掉。
	if rc.quotaReserved > 0 {
		reserved := rc.quotaReserved
		rc.quotaReserved = 0
		if err := rc.store.ReleaseQuota(context.Background(), rc.keyID, reserved); err != nil {
			if rc.logger != nil {
				rc.logger.Warn("release quota reservation failed",
					"key_id", rc.keyID, "reserved", reserved, "error", err)
			}
		}
	}
	if rc.userQuotaReserved > 0 {
		reserved := rc.userQuotaReserved
		rc.userQuotaReserved = 0
		if err := rc.store.ReleaseUserQuota(context.Background(), rc.userID, reserved); err != nil {
			if rc.logger != nil {
				rc.logger.Warn("release user quota reservation failed",
					"user_id", rc.userID, "reserved", reserved, "error", err)
			}
		}
	}
}

// NewRateCommit creates a request-level reservation finalizer.
func NewRateCommit(limiter *ratelimit.Limiter, keyID string, limit int, reserved int64, store *store.Store, logger *slog.Logger) *RateCommit {
	return &RateCommit{
		limiter: limiter, keyID: keyID, limit: limit, reserved: reserved,
		store: store, logger: logger,
	}
}

// SetKeyQuotaReserved records the key-level lifetime quota reservation.
func (rc *RateCommit) SetKeyQuotaReserved(reserved int64) {
	if rc != nil {
		rc.quotaReserved = reserved
	}
}

// SetUserQuotaReserved records the user-level lifetime quota reservation.
func (rc *RateCommit) SetUserQuotaReserved(userID string, reserved int64) {
	if rc != nil {
		rc.userID = userID
		rc.userQuotaReserved = reserved
	}
}
