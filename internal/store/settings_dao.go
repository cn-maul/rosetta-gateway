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
