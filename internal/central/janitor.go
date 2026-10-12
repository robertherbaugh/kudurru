package central

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// Job is one retention or housekeeping task of the Janitor; it returns how
// many rows it removed.
type Job struct {
	Name string
	Run  func(ctx context.Context, tx pgx.Tx, now time.Time) (int64, error)
}

// Janitor is the leader-only runnable that runs Jobs once at start and
// then every Interval (default 1 h), each in its own transaction. A
// failed job is logged and retried at the next tick; the database being
// away is not an error.
type Janitor struct {
	DB       *DB
	Jobs     []Job
	Interval time.Duration
	Logger   *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Run runs the jobs until ctx ends.
func (j *Janitor) Run(ctx context.Context) error {
	interval := j.Interval
	if interval <= 0 {
		interval = time.Hour
	}
	now := j.Now
	if now == nil {
		now = time.Now
	}
	log := j.Logger
	if log == nil {
		log = slog.Default()
	}
	log = log.With("component", "central")
	run := func() {
		for _, job := range j.Jobs {
			var n int64
			err := j.DB.Tx(ctx, func(tx pgx.Tx) error {
				// Housekeeping on a large database outlasts the pool's
				// statement_timeout, which is meant for reads.
				if err := NoStatementTimeout(ctx, tx); err != nil {
					return err
				}
				var err error
				n, err = job.Run(ctx, tx, now())
				return err
			})
			switch {
			case errors.Is(err, ErrUnavailable) || errors.Is(err, ErrIncompatible):
				return // Run logs the database's state
			case err != nil && ctx.Err() == nil:
				log.Warn("central housekeeping failed", "job", job.Name, "err", err)
			case n > 0:
				log.Info("central housekeeping", "job", job.Name, "rows", n)
			}
		}
	}
	run()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			run()
		}
	}
}

// NoStatementTimeout lifts the pool's statement_timeout (15 s, for reads)
// for the rest of tx: retention, purges and other bulk work.
func NoStatementTimeout(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SET LOCAL statement_timeout = 0`)
	return err
}
