package sqlite

import (
	"context"
	"time"
)

// lockWriterContext retains the original lifetime while queueing for wmu.
// The caller owns Unlock on success. No detached waiter can acquire the mutex
// after the original operation has already returned a refusal.
func (s *Store) lockWriterContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.wmu.TryLock() {
		return nil
	}
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if err := ctx.Err(); err != nil {
				return err
			}
			if s.wmu.TryLock() {
				return nil
			}
		}
	}
}
