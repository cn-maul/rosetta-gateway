package store

import (
	"context"
	"time"
)

type UsageRecord struct {
	ID               string
	Ts               int64
	AccessKeyID      string
	PublicModel      string
	ProviderID       string
	UpstreamModel    string
	IngressProtocol  string
	Stream           bool
	InputTokens      int64
	OutputTokens     int64
	TotalTokens      int64
	ReasoningTokens  int64
	CachedTokens     int64
	UsageState       string
	Status           string
	HTTPStatus       int
	ErrorCode        string
	LatencyMs        int64
	TTFBMs           int64
	RequestID        string
}

func (s *Store) CreateUsageRecord(ctx context.Context, r *UsageRecord) error {
	now := time.Now().UnixMilli()
	stream := 0
	if r.Stream {
		stream = 1
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO usage_records (id, ts, access_key_id, public_model, provider_id, upstream_model, ingress_protocol, stream, input_tokens, output_tokens, total_tokens, reasoning_tokens, cached_tokens, usage_state, status, http_status, error_code, latency_ms, ttfb_ms, request_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, now, r.AccessKeyID, r.PublicModel, r.ProviderID, r.UpstreamModel, r.IngressProtocol, stream, r.InputTokens, r.OutputTokens, r.TotalTokens, r.ReasoningTokens, r.CachedTokens, r.UsageState, r.Status, r.HTTPStatus, r.ErrorCode, r.LatencyMs, r.TTFBMs, r.RequestID)
	return err
}

type UsageStats struct {
	TotalRequests int64
	TotalTokens   int64
	InputTokens   int64
	OutputTokens  int64
	ErrorCount    int64
}

func (s *Store) GetUsageStats(ctx context.Context) (*UsageStats, error) {
	var stats UsageStats
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COUNT(CASE WHEN status != 'ok' THEN 1 END) FROM usage_records`).
		Scan(&stats.TotalRequests, &stats.TotalTokens, &stats.InputTokens, &stats.OutputTokens, &stats.ErrorCount)
	if err != nil {
		return nil, err
	}
	return &stats, nil
}
