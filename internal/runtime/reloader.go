// Package runtime owns the combined upstream-pool and snapshot rebuild.
//
// The rebuild is an explicit atomic operation: the pool and snapshot are
// prepared independently, then installed only after both preparations succeed.
package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cn-maul/rosetta-gateway/internal/config"
	"github.com/cn-maul/rosetta-gateway/internal/snapshot"
	"github.com/cn-maul/rosetta-gateway/internal/store"
	"github.com/cn-maul/rosetta-gateway/internal/upstream"
)

// Reloader serializes pool and snapshot rebuilds.
//
// The pool and snapshot are two separate atomic domains. Building both before
// installing either prevents a failed second step from leaving them divergent;
// the mutex keeps an admin-triggered reload and the background retry from
// interleaving.
type Reloader struct {
	mu        sync.Mutex
	db        *store.Store
	masterKey []byte
	pool      *upstream.Pool
	cfg       *config.Config
	logger    *slog.Logger

	// dirty means the last rebuild failed and the runtime lags the database.
	// It uses atomics because Reload holds mu for the whole rebuild while the
	// retry loop must be able to inspect and set the flag without blocking.
	dirty atomic.Bool

	// dirtySince records when dirty was first set, so persistent failures can
	// report how long the runtime has been behind.
	dirtySince atomic.Int64
}

// New constructs a reloader.
func New(db *store.Store, masterKey []byte, pool *upstream.Pool, cfg *config.Config, logger *slog.Logger) *Reloader {
	return &Reloader{db: db, masterKey: masterKey, pool: pool, cfg: cfg, logger: logger}
}

const (
	// reloadRetryInterval is the base retry interval. It is short enough that
	// disabling a key takes effect within half a minute, and long enough that
	// a persistent database failure does not flood the log.
	reloadRetryInterval = 30 * time.Second
	// reloadTimeout bounds one rebuild. Normal rebuilds are in the millisecond
	// range; this only prevents a stuck SQLite read from pinning the retry loop.
	reloadTimeout = 30 * time.Second
	// reloadRetryMaxInterval caps exponential backoff at ten minutes.
	reloadRetryMaxInterval = 10 * time.Minute
)

// MarkDirty records a failed rebuild and waits for the background retry.
func (r *Reloader) MarkDirty() {
	if r.dirty.CompareAndSwap(false, true) {
		r.dirtySince.Store(time.Now().UnixMilli())
	}
}

// markClean clears the dirty flag after a successful Reload.
func (r *Reloader) markClean() {
	r.dirty.Store(false)
	r.dirtySince.Store(0)
}

// IsDirty reports whether the runtime lags the database.
func (r *Reloader) IsDirty() bool { return r.dirty.Load() }

// WatchRetry retries dirty rebuilds until they succeed or ctx is canceled.
func (r *Reloader) WatchRetry(ctx context.Context) {
	backoff := reloadRetryInterval
	for {
		if !r.dirty.Load() {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}

		since := r.dirtySince.Load()
		if since > 0 {
			elapsed := time.Since(time.UnixMilli(since)).Round(time.Second)
			r.logger.Warn("runtime still lags behind database; retrying rebuild",
				"dirty_for", elapsed.String(),
				"next_retry_in", backoff.String())
		}

		rctx, cancel := context.WithTimeout(ctx, reloadTimeout)
		if err := r.Reload(rctx); err == nil {
			r.logger.Info("runtime caught up with database after failed reload",
				"dirty_for", time.Since(time.UnixMilli(since)).Round(time.Second).String())
			backoff = reloadRetryInterval
		} else {
			r.logger.Warn("runtime reload retry failed; will retry again",
				"error", err, "next_retry_in", backoff.String())
			backoff *= 2
			if backoff > reloadRetryMaxInterval {
				backoff = reloadRetryMaxInterval
			}
		}
		cancel()

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// Reload serially rebuilds the pool and snapshot. Any failure leaves both in
// their previous state.
func (r *Reloader) Reload(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	providers, poolFailures, err := r.pool.PrepareFromStore(ctx, r.db, r.masterKey, r.cfg)
	if err != nil {
		return fmt.Errorf("prepare upstream pool: %w", err)
	}

	type projection struct {
		reason string
		fatal  bool
	}
	bySlug := make(map[string]projection, len(poolFailures))
	for _, f := range poolFailures {
		fatal := f.Stage == "no_credentials" || f.Stage == "list_credentials"
		if prev, ok := bySlug[f.Slug]; ok && prev.fatal && !fatal {
			continue
		}
		bySlug[f.Slug] = projection{reason: f.Reason, fatal: fatal}
	}
	failures := make([]snapshot.ProviderFailure, 0, len(bySlug))
	for slug, p := range bySlug {
		failures = append(failures, snapshot.ProviderFailure{Slug: slug, Reason: p.reason})
	}
	sort.Slice(failures, func(i, j int) bool { return failures[i].Slug < failures[j].Slug })

	if len(poolFailures) > 0 && r.logger != nil {
		r.logger.Warn("some providers are not ready after reload",
			"count", len(poolFailures), "providers", len(bySlug))
	}

	snap, err := snapshot.RebuildFromDB(ctx, r.db, failures)
	if err != nil {
		return fmt.Errorf("rebuild snapshot: %w", err)
	}
	r.pool.Install(providers, liveTargetIDs(ctx, r.db))
	snapshot.Swap(snap)
	r.markClean()
	return nil
}

// liveTargetIDs returns the route-target IDs used to preserve breaker state.
// A read failure returns Known=false so Install keeps all existing states
// rather than treating an unknown set as an empty one.
func liveTargetIDs(ctx context.Context, db *store.Store) upstream.LiveTargets {
	targets, err := db.ListAllRouteTargets(ctx)
	if err != nil {
		return upstream.LiveTargets{Known: false}
	}
	ids := make(map[string]bool, len(targets))
	for _, t := range targets {
		ids[t.ID] = true
	}
	return upstream.LiveTargets{IDs: ids, Known: true}
}
