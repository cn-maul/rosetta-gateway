package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// 设置以 JSON 值存在 app_settings 表里，key 区分不同配置块。
const settingModelDefaultsKey = "model_defaults"

// 未显式配置时的兜底默认：探测不到模型容量时使用。
const (
	defaultContextWindowFallback   = 8192
	defaultMaxOutputTokensFallback = 4096
)

type ModelDefaults struct {
	ContextWindow   int `json:"context_window"`
	MaxOutputTokens int `json:"max_output_tokens"`
}

func (s *Store) getSetting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM app_settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (s *Store) setSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO app_settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, time.Now().UnixMilli())
	return err
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
