package providerhost

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestPinnedHostBoundedOriginalEntitlementAndCancellation(t *testing.T) {
	f := newSigned(t)
	now := time.Now().UTC().Truncate(time.Second)
	f.clock.Store(now.UnixNano())
	f.claims.IssuedAt, f.claims.NotBefore, f.claims.ExpiresAt = now.Add(-time.Minute).Unix(), now.Add(-time.Minute).Unix(), now.Add(time.Hour).Unix()
	var err error
	f.raw, err = license.Sign(f.key, "test", f.claims)
	if err != nil {
		t.Fatal(err)
	}
	s := openStore(t, filepath.Join(t.TempDir(), "state.db"), replication.Standalone)
	_ = seed(t, s, now)
	x := start(t, s, f, staticInput(t))
	_ = reset(t, x)
	await(t, func() bool {
		return x.resources.WithCurrent(t.Context(), func(*providerresources.Pinned) error { return nil }) == nil
	})
	for _, duration := range []time.Duration{time.Minute, 2 * time.Hour} {
		ctx, cancel := context.WithTimeout(t.Context(), duration)
		want, _ := ctx.Deadline()
		if until := x.host.value.Load().until; until.Before(want) {
			want = until
		}
		var escaped context.Context
		err := x.resources.WithCurrent(ctx, func(*providerresources.Pinned) error {
			return x.host.PinHostBounded(ctx, func(scope context.Context, current func() bool) error {
				escaped = scope
				deadline, finite := scope.Deadline()
				if !finite || !deadline.Equal(want) || !current() {
					t.Fatal("pinned context lost exact original/entitlement bound")
				}
				return nil
			})
		})
		cancel()
		if err != nil || !errors.Is(escaped.Err(), context.Canceled) {
			t.Fatal("host scope escaped or failed", err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	err = x.resources.WithCurrent(ctx, func(*providerresources.Pinned) error {
		return x.host.PinHostBounded(ctx, func(scope context.Context, current func() bool) error {
			cancel()
			if current() || !errors.Is(scope.Err(), context.Canceled) {
				t.Fatal("host bound detached cancellation")
			}
			return scope.Err()
		})
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
}
