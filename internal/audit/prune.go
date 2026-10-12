package audit

import (
	"context"
	"log/slog"
	"time"
)

// Retention defaults (docs/architecture.md §11.4).
const (
	// DefaultRetention is how long stored audit events are kept: 400 days.
	DefaultRetention = 400 * 24 * time.Hour
	// DefaultPruneInterval is how often a Pruner runs.
	DefaultPruneInterval = 24 * time.Hour
)

// Pruner is a runnable that deletes stored events older than Retention
// once at start and then every Interval. A failed prune is logged and
// retried at the next tick; it never ends the runnable.
type Pruner struct {
	// Name labels the log lines ("spool", "auditlog", ...).
	Name string
	// Prune deletes the events before the cutoff and returns how many.
	Prune func(ctx context.Context, before time.Time) (int64, error)
	// Retention defaults to DefaultRetention, Interval to
	// DefaultPruneInterval.
	Retention, Interval time.Duration
	Logger              *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Run prunes until ctx ends.
func (p *Pruner) Run(ctx context.Context) error {
	retention, interval := p.Retention, p.Interval
	if retention <= 0 {
		retention = DefaultRetention
	}
	if interval <= 0 {
		interval = DefaultPruneInterval
	}
	now := p.Now
	if now == nil {
		now = time.Now
	}
	log := p.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "audit", "store", p.Name)
	prune := func() {
		before := now().Add(-retention)
		n, err := p.Prune(ctx, before)
		switch {
		case err != nil && ctx.Err() == nil:
			log.Warn("audit prune failed", "err", err)
		case n > 0:
			log.Info("audit events pruned", "deleted", n, "before", before.UTC().Format(time.RFC3339))
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
