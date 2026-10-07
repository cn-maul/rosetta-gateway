package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// 设置以 JSON 值存在 app_settings 表里，key 区分不同配置块。
const settingModelDefaultsKey = "model_defaults"

// settingBootstrapCompletedKey 是「本安装已完成过首次引导」的一次性标记。
//
// 为什么需要它（而不只靠「当前是否存在空密码 admin」判定引导窗口）：
// 空密码 admin 只是引导窗口**曾经**开放的原因，不是窗口该不该开的依据。
// 曾经由管理员建出/升级出空密码 admin（bug 期间的口子，已在用户管理面堵住）
// 时，仅凭后者判定会让免鉴权的 POST /admin/api/bootstrap **永久**重开 ——
// 任何人都能给那个账号设上自己的密码并拿到 admin 会话。窗口必须绑定到
// 「本安装是否完成过引导」这个只会前进一次的事实上。
//
// 值固定为 "1"：布尔语义，不需要结构化内容。写入与 SetInitialAdminPassword
// 的条件 UPDATE 在**同一个事务**里（见 user_dao.go），「设密成功」与「窗口关闭」
// 原子生效，不存在「密码设上了但窗口还开着」的中间态。
//
// 兼容性：老库已完成引导但缺这个标记时，窗口本来就由「无空密码 admin」关着，
// 行为不变；只有 bug 期间恰好留下空密码 admin 的库会再开**最后**一次窗口，
// 设完即永久关闭。
const settingBootstrapCompletedKey = "bootstrap_completed"

// BootstrapCompleted 报告本安装是否已完成过首次引导。
// 未写过标记（含老库升级）返回 false —— 此时窗口是否开放仍由
// FindUninitializedAdmin 的旧判定兜底。
func (s *Store) BootstrapCompleted(ctx context.Context) (bool, error) {
	v, ok, err := s.getSetting(ctx, settingBootstrapCompletedKey)
	if err != nil || !ok {
		return false, err
	}
	return v == "1", nil
}

// 未显式配置时的兜底默认：探测不到模型容量时使用。
//
// 兜底值刻意取**当代主流模型的真实规格**（128K 上下文 / 64K 最大输出），
// 而不是早期保守值：既然是「探测不到才用」，就应当按当下模型的水位给，
// 否则一个没暴露 context_length 的新模型会被凭空削成 8K，用户侧表现为
// 「刚接的模型只会说 8K 字」这种难以自查的静默降级。
const (
	defaultContextWindowFallback   = 131_072 // 128K tokens
	defaultMaxOutputTokensFallback = 65_536  // 64K tokens
)

type ModelDefaults struct {
	ContextWindow   int `json:"context_window"`
	MaxOutputTokens int `json:"max_output_tokens"`
}

func (s *Store) getSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.read.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (s *Store) setSetting(ctx context.Context, key, value string) error {
	return setSettingExec(ctx, s.db, key, value)
}

func setSettingExec(ctx context.Context, ex execer, key, value string) error {
	_, err := ex.ExecContext(ctx,
		`INSERT INTO app_settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UnixMilli())
	return err
}

// SaveSettings 在一个事务里同时写入模型容量默认与运行时默认。
//
// 设置页的「保存」是一个动作的两半；分两条自动提交语句写（旧实现）会在第二条
// 失败时留下「容量已改、超时未改」的半套配置，界面回 500 但库里已部分生效，
// 用户重试前系统状态与界面显示不一致。
func (s *Store) SaveSettings(ctx context.Context, d ModelDefaults, rt RuntimeDefaults) error {
	db, err := json.Marshal(d)
	if err != nil {
		return err
	}
	rb, err := json.Marshal(rt)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := setSettingExec(ctx, tx, settingModelDefaultsKey, string(db)); err != nil {
		return err
	}
	if err := setSettingExec(ctx, tx, settingRuntimeDefaultsKey, string(rb)); err != nil {
		return err
	}
	return tx.Commit()
}

// GetModelDefaults 返回默认模型容量；未配置时给出常量兜底（不回写）。
func (s *Store) GetModelDefaults(ctx context.Context) (ModelDefaults, error) {
	d := ModelDefaults{
		ContextWindow:   defaultContextWindowFallback,
		MaxOutputTokens: defaultMaxOutputTokensFallback,
	}
	raw, ok, err := s.getSetting(ctx, settingModelDefaultsKey)
	if err != nil || !ok {
		return d, err
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return d, err
	}
	if d.ContextWindow <= 0 {
		d.ContextWindow = defaultContextWindowFallback
	}
	if d.MaxOutputTokens <= 0 {
		d.MaxOutputTokens = defaultMaxOutputTokensFallback
	}
	return d, nil
}

func (s *Store) SetModelDefaults(ctx context.Context, d ModelDefaults) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return s.setSetting(ctx, settingModelDefaultsKey, string(b))
}

// settingRuntimeDefaultsKey 是「运行时全局默认」的 app_settings 键。
// 这些参数原先只存在于 config.json（改后要重启），或散落在每条 route 上；
// 收拢到这里后可随保存即时生效（保存后前端触发 reload 重建快照）。
const settingRuntimeDefaultsKey = "runtime_defaults"

// RuntimeDefaults 是可在管理后台热改的运行时全局默认（超时 + 故障转移策略）。
// 每个字段 0 都表示「未配置」，由调用方回落到 config.json 的 defaults；
// 这样老部署升级后，config.json 里已写的值继续生效，直到在后台显式保存。
type RuntimeDefaults struct {
	UpstreamTimeoutMs         int `json:"upstream_timeout_ms"`
	StreamIdleTimeoutMs       int `json:"stream_idle_timeout_ms"`
	StreamFirstTokenTimeoutMs int `json:"stream_first_token_timeout_ms"`
	FailoverMaxTargets        int `json:"failover_max_targets"`
	FailoverFailureThreshold  int `json:"failover_failure_threshold"`
}

// GetRuntimeDefaults 返回运行时全局默认；未配置的字段留 0（调用方回落 config）。
func (s *Store) GetRuntimeDefaults(ctx context.Context) (RuntimeDefaults, error) {
	var d RuntimeDefaults
	raw, ok, err := s.getSetting(ctx, settingRuntimeDefaultsKey)
	if err != nil || !ok {
		return d, err
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		// 存储损坏时退回「全部未配置」而不是让整个启动失败 —— 与 ModelDefaults
		// 的容错口径不同是有意的：这里驱动的是转发路径，宁可回落 config 也不能中断。
		return RuntimeDefaults{}, nil
	}
	return d, nil
}

func (s *Store) SetRuntimeDefaults(ctx context.Context, d RuntimeDefaults) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return s.setSetting(ctx, settingRuntimeDefaultsKey, string(b))
}
