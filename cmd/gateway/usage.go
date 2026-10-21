package main

import (
	"log/slog"

	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/usage"
)

// usageRecorder 保留 cmd 内既有调用名；实现和生命周期归 internal/usage。
type usageRecorder = usage.Recorder

const (
	usageStatusOK  = usage.StatusOK
	usageQueueSize = usage.QueueSize
)

func newUsageRecorder(db *store.Store, logger *slog.Logger) *usageRecorder {
	return usage.New(usage.Config{
		DB:              db,
		Logger:          logger,
		IsBalanceExempt: balanceExempt,
	})
}
