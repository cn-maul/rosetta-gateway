package store

import (
	"context"
	"database/sql"
	"time"
)

type Credential struct {
	ID            string
	ProviderID    string
	Label         string
	APIKeyEnc     []byte
	Enabled       bool
	Weight        int
	Status        string
	CooldownUntil int64
	LastError     string
	CreatedAt     int64
}

// credentialColumns 是 provider_credentials 两处读路径共用的列清单。
const credentialColumns = `id, provider_id, label, api_key_enc, enabled, weight, status, cooldown_until, last_error, created_at`

// scanCredential 用 sql.NullString 承接可空列 label / last_error，
// 否则列为 NULL 时扫描会直接报错。
func scanCredential(sc scanner) (Credential, error) {
	var c Credential
	var enabled int
	var label, lastError sql.NullString

	err := sc.Scan(&c.ID, &c.ProviderID, &label, &c.APIKeyEnc, &enabled,
		&c.Weight, &c.Status, &c.CooldownUntil, &lastError, &c.CreatedAt)
	if err != nil {
		return c, err
	}
	c.Enabled = enabled == 1
	c.Label = label.String
	c.LastError = lastError.String
	return c, nil
}

func (s *Store) ListCredentials(ctx context.Context, providerID string) ([]Credential, error) {
	rows, err := s.read.QueryContext(ctx,
		`SELECT `+credentialColumns+` FROM provider_credentials WHERE provider_id = ? ORDER BY created_at`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]Credential, 0)
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, c)
	}
	return result, rows.Err()
}

func (s *Store) GetCredential(ctx context.Context, id string) (*Credential, error) {
	c, err := scanCredential(s.read.QueryRowContext(ctx,
		`SELECT `+credentialColumns+` FROM provider_credentials WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) CreateCredential(ctx context.Context, c *Credential) error {
	return createCredential(ctx, s.db, c)
}

// createCredential 接受 execer，供事务内复用（见 CreateProviderWithCredential）。
func createCredential(ctx context.Context, ex execer, c *Credential) error {
	now := time.Now().UnixMilli()
	enabled := 0
	if c.Enabled {
		enabled = 1
	}
	_, err := ex.ExecContext(ctx,
		`INSERT INTO provider_credentials (id, provider_id, label, api_key_enc, enabled, weight, status, cooldown_until, last_error, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ProviderID, nullIfEmpty(c.Label), c.APIKeyEnc, enabled, c.Weight, c.Status, c.CooldownUntil, nullIfEmpty(c.LastError), now)
	// 回写时间戳，否则创建响应里的 created_at 是 0，与库里不一致。
	if err == nil {
		c.CreatedAt = now
	}
	return err
}

func (s *Store) UpdateCredential(ctx context.Context, id string, c *Credential) error {
	enabled := 0
	if c.Enabled {
		enabled = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE provider_credentials SET label = ?, api_key_enc = ?, enabled = ?, weight = ?, status = ?, cooldown_until = ?, last_error = ? WHERE id = ?`,
		nullIfEmpty(c.Label), c.APIKeyEnc, enabled, c.Weight, c.Status, c.CooldownUntil, nullIfEmpty(c.LastError), id)
	return checkAffected(res, err)
}

// SetCredentialCooldown 只更新凭据的冷却状态与冷却截止时间。
//
// 与 UpdateCredential 分开的原因：冷却是**运行时状态**，由数据面在每次
// 上游 401/5xx 时写入；它必须落库，否则任何一次 admin 写操作触发的池重建
// （Install 重新 PrepareFromStore）都会把内存里的 CooldownUntil 读回成库里的
// 0，刚被判坏的 key 立刻复活。凭据 id 不存在时返回 ErrNotFound。
func (s *Store) SetCredentialCooldown(ctx context.Context, id, status string, until time.Time) error {
	var untilMs int64
	if !until.IsZero() {
		untilMs = until.UnixMilli()
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE provider_credentials SET status = ?, cooldown_until = ? WHERE id = ?`,
		status, untilMs, id)
	return checkAffected(res, err)
}

// DeleteCredential 删除一把凭据；id 不存在时返回 ErrNotFound。
func (s *Store) DeleteCredential(ctx context.Context, id string) error {
	return deleteByID(ctx, s.db, "provider_credentials", id)
}

// HasEnabledCredential 报告该 provider 下是否已有启用的凭据。
//
// 只服务于「新建凭据默认该启用还是默认停用」这一个判断（见
// admin.CredentialHandler.Create）。走 s.read 足够：这是管理面的低频写
// 路径，读到「另一个标签页刚启用过」的滞后值，最坏结果只是新建的那把
// 默认停用 —— 与「同时只有一把启用」的不变量同向，绝不会多出第二把。
func (s *Store) HasEnabledCredential(ctx context.Context, providerID string) (bool, error) {
	var n int
	if err := s.read.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM provider_credentials WHERE provider_id = ? AND enabled = 1`,
		providerID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// EnableExclusiveCredential 把 id 设为该 provider 下**唯一**启用的一把凭据，
// 同一事务内把同 provider 的其余凭据一并停用。id 不存在或不属于该
// provider 时返回 ErrNotFound。
//
// # 为什么是「切换」而不是「拒绝」
//
// 运维换 key 的动作序列是固定的：新建一把 → 点启用 → 观察 → 停用旧的。
// 若「已有一把启用时再启用另一把」直接报错，这个序列就必须先停旧的，
// 于是中间出现一个「该上游一把都没有」的窗口，期间所有请求都会拿到
// "no healthy credentials"。让新的一把顶掉旧的则没有这个窗口：至多是
// 极短暂的两把都启用，而那只是一次权重轮询打到了旧 key —— 新 key 立刻
// 就接替下一次请求，失败面为零。
//
// # 为什么是「先全停、再开这一把」
//
// 顺序反过来（先开新的、再停旧的）会在两条语句之间留下「一把都没启用」
// 的窗口，并发请求正落在这里就整批失败。现有顺序里窗口内至多两把都启用，
// 没有任何请求会失败。
//
// 刻意**不**在这里顺带改 status / cooldown：凭据被停用不等于被熔断，
// 反过来重新启用时也不该顺手把「上次 5xx 冷却到明天」的历史抹掉。
func (s *Store) EnableExclusiveCredential(ctx context.Context, providerID, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // 已提交时是 no-op

	if _, err := tx.ExecContext(ctx,
		`UPDATE provider_credentials SET enabled = 0 WHERE provider_id = ? AND id <> ?`,
		providerID, id); err != nil {
		return err
	}
	// 带上 provider_id 是为了挡住「拿 A 上游的凭据 id 去 B 上游上启用」：
	// 那样会先把 B 的凭据全停掉（B 瞬间不可用），再因为一行都没命中而报错。
	// 数据面 GetAnyClient 只按 provider 找凭据，这种误调用会表现为
	// 「另一个上游莫名其妙挂了」。
	res, err := tx.ExecContext(ctx,
		`UPDATE provider_credentials SET enabled = 1 WHERE id = ? AND provider_id = ?`,
		id, providerID)
	if err != nil {
		return err
	}
	if err := checkAffected(res, nil); err != nil {
		return err
	}
	return tx.Commit()
}
