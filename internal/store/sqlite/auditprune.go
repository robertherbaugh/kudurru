package sqlite

import (
	"context"
	"log/slog"
	"time"
)

// Audit retention defaults (dev-docs/design-record.md §11.4).
const (
	// DefaultAuditRetention is how long audit rows are kept: 400 days.
	DefaultAuditRetention = 400 * 24 * time.Hour
	// DefaultAuditPruneInterval is how often the leader prunes.
	DefaultAuditPruneInterval = 24 * time.Hour
)

// AuditPruner is the leader-only runnable that deletes audit rows older
// than Retention once at start and then every Interval (PruneAudit). The
// hash chain stays verifiable: VerifyAudit accepts a pruned predecessor.
type AuditPruner struct {
	Store *Store
	// Retention defaults to DefaultAuditRetention, Interval to
	// DefaultAuditPruneInterval.
	Retention, Interval time.Duration
	Logger              *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Run prunes until ctx ends. A failed prune is logged and retried at the
// next tick; it never ends the runnable.
func (p *AuditPruner) Run(ctx context.Context) error {
	retention, interval := p.Retention, p.Interval
	if retention <= 0 {
		retention = DefaultAuditRetention
	}
	if interval <= 0 {
		interval = DefaultAuditPruneInterval
	}
	now := p.Now
	if now == nil {
		now = time.Now
	}
	log := p.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "audit")
	prune := func() {
		before := now().Add(-retention)
		n, err := p.Store.PruneAudit(ctx, before)
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn("audit prune failed", "err", err)
		case n > 0:
			log.Info("audit rows pruned", "deleted", n, "before", before.UTC().Format(time.RFC3339))
		}
	}
	prune()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			prune()
		}
	}
}
