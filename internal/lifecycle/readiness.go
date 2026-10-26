// Package lifecycle owns process readiness and shutdown coordination.
package lifecycle

import (
	"context"
	"sync/atomic"
)

// Phase is the process lifecycle phase.
type Phase string

const (
	PhaseStarting Phase = "starting"
	PhaseReady    Phase = "ready"
	PhaseStopping Phase = "stopping"
	PhaseStopped  Phase = "stopped"
)

// BootstrapState is the authoritative first-run administrator state.
type BootstrapState string

const (
	BootstrapUnknown       BootstrapState = "unknown"
	BootstrapSetupRequired BootstrapState = "setup_required"
	BootstrapReady         BootstrapState = "ready"
	BootstrapFailed        BootstrapState = "failed"
)

// DimensionState is one observable readiness dimension.
type DimensionState string

const (
	DimensionOK      DimensionState = "ok"
	DimensionPending DimensionState = "pending"
	DimensionFailed  DimensionState = "failed"
)

// Readiness is the typed result projected by the public health endpoint.
// Storage, runtime, and bootstrap are independent facts; HTTP must not infer
// one from another.
type Readiness struct {
	Status    string
	ErrorCode string
	Database  DimensionState
	Runtime   DimensionState
	Bootstrap DimensionState
}

// Pinger is the storage readiness capability.
type Pinger interface {
	Ping(context.Context) error
}

// RuntimeState reports whether the published runtime lags the database.
type RuntimeState interface {
	IsDirty() bool
}

// Checker is consumed by the HTTP health projection.
type Checker interface {
	Readiness(context.Context) Readiness
}

// Manager owns the typed readiness dimensions for one process.
type Manager struct {
	db      Pinger
	runtime RuntimeState
	phase   atomic.Value
	boot    atomic.Value
	workers atomic.Int64
	joined  atomic.Bool
}

// NewManager constructs a manager before the HTTP surface starts.
func NewManager(db Pinger, runtime RuntimeState) *Manager {
	m := &Manager{db: db, runtime: runtime}
	m.phase.Store(PhaseStarting)
	m.boot.Store(BootstrapUnknown)
	return m
}

// SetPhase publishes a lifecycle phase.
func (m *Manager) SetPhase(phase Phase) {
	m.phase.Store(phase)
}

// Phase returns the last published lifecycle phase.
func (m *Manager) Phase() Phase {
	phase, _ := m.phase.Load().(Phase)
	return phase
}

// SetBootstrap publishes the verified bootstrap outcome.
func (m *Manager) SetBootstrap(state BootstrapState) {
	m.boot.Store(state)
}

// AddWorkers records long-lived goroutines owned by the lifecycle.
func (m *Manager) AddWorkers(n int) {
	m.workers.Add(int64(n))
}

// WorkerExited records a joined worker.
func (m *Manager) WorkerExited() {
	m.workers.Add(-1)
}

// MarkWorkersJoined records that every registered worker has exited.
func (m *Manager) MarkWorkersJoined() {
	m.joined.Store(true)
}

// PendingWorkers reports registered workers that have not exited. A negative
// value can only mean a bookkeeping bug and is never treated as joined.
func (m *Manager) PendingWorkers() int64 {
	n := m.workers.Load()
	if n < 0 {
		return n
	}
	return n
}

// WorkersJoined reports whether shutdown completed its join barrier.
func (m *Manager) WorkersJoined() bool {
	return m.joined.Load() && m.workers.Load() == 0
}

// Readiness evaluates every readiness dimension once.
func (m *Manager) Readiness(ctx context.Context) Readiness {
	result := Readiness{
		Status:    "ok",
		Database:  DimensionOK,
		Runtime:   DimensionOK,
		Bootstrap: DimensionOK,
	}
	if err := m.db.Ping(ctx); err != nil {
		result.Status = "unhealthy"
		result.ErrorCode = "database_unavailable"
		result.Database = DimensionFailed
	}

	phase, _ := m.phase.Load().(Phase)
	if phase == PhaseStopping || phase == PhaseStopped {
		if result.Status == "ok" {
			result.Status = "degraded"
			result.ErrorCode = "service_stopping"
		}
	}
	if phase == PhaseStopping && !m.WorkersJoined() {
		result.Runtime = DimensionPending
		result.Status = "degraded"
		result.ErrorCode = "workers_draining"
	}
	if m.runtime != nil && m.runtime.IsDirty() {
		result.Runtime = DimensionPending
		if result.Status == "ok" {
			result.Status = "degraded"
			result.ErrorCode = "runtime_lags_behind_database"
		}
	}

	bootstrap, _ := m.boot.Load().(BootstrapState)
	switch bootstrap {
	case BootstrapFailed:
		result.Bootstrap = DimensionFailed
		if result.Status == "ok" {
			result.Status = "unhealthy"
			result.ErrorCode = "bootstrap_failed"
		}
	case BootstrapSetupRequired:
		result.Bootstrap = DimensionPending
	case BootstrapUnknown:
		result.Bootstrap = DimensionPending
	}
	return result
}
