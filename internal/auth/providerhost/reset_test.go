package providerhost

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestResetWithdrawalActualFloorsPreservesIncarnationsAndNoOpRollback(t *testing.T) {
	in := staticInput(t)
	in.LocalProviders = []providerregistry.DesiredConfig{{ID: "corp", Spec: spec()}}
	x := newFixture(t, in)
	addObject(t, x, "object", spec())
	command(t, x.s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return security.ReconcileProviderConfigs(ctx, tx, in.LocalProviders, x.f.now())
	})
	e := reset(t, x)
	accept(t, x, e)
	before := image(t, x.s)
	sources := registry(t, x.s).ConfigSources()
	x.f.clock.Add(int64(time.Second))
	e2 := reset(t, x)
	if e2 == e || e2 == "" {
		t.Fatal("reset failed to rotate activation")
	}
	r := registry(t, x.s)
	for _, a := range r.Authorities() {
		if a.Active {
			t.Fatal("reset left active receipt")
		}
	}
	if !reflect.DeepEqual(sources, r.ConfigSources()) {
		t.Fatal("reset altered config UID/time/spec")
	}
	state, err := servingauth.DecodeSecurity(image(t, x.s).Payload.Security)
	if err != nil || len(state.ProviderFloors) != 2 {
		t.Fatal("reset omitted BOTH-origin exact UID floors", err)
	}
	for _, f := range state.ProviderFloors {
		if f.ValidAfter.Before(x.f.now()) {
			t.Fatal("floor older than reset")
		}
	}
	if before.Generation().Same(image(t, x.s).Generation()) {
		t.Fatal("withdrawal did not commit")
	}
	snapshot := image(t, x.s)
	abort := errors.New("test precommit abort")
	original := x.host.opts.Withdraw
	x.host.opts.Withdraw = func(ctx context.Context, tx *sqlite.CommandTx, at time.Time) error {
		if err := original(ctx, tx, at); err != nil {
			return err
		}
		return tx.RequireCommitCheck(func(context.Context) error { return abort })
	}
	ctx, target := writer(t, x.s)
	if err := x.host.Reset(ctx, target); err != abort {
		t.Fatal("no-op precommit abort not honored", err)
	}
	if !snapshot.Generation().Same(image(t, x.s).Generation()) || x.host.value.Load().phase == pending {
		t.Fatal("no-op failure changed DB or installed pending")
	}
	x.host.opts.Withdraw = original
	_ = reset(t, x)
}

func TestWithdrawalPostStateDeniesNoopAndUnrelatedEdits(t *testing.T) {
	for _, mode := range []string{"no-withdrawal", "session-floor-edit"} {
		t.Run(mode, func(t *testing.T) {
			x := newFixture(t, staticInput(t))
			addObject(t, x, "object", spec())
			e := reset(t, x)
			accept(t, x, e)
			before := image(t, x.s)
			original := x.host.opts.Withdraw
			x.host.opts.Withdraw = func(ctx context.Context, tx *sqlite.CommandTx, at time.Time) error {
				if mode == "no-withdrawal" {
					return nil
				}
				if err := original(ctx, tx, at); err != nil {
					return err
				}
				s, err := security.Load(ctx, tx)
				if err != nil {
					return err
				}
				if err = s.InvalidateGrant("viewer@namespace:apps", at); err != nil {
					return err
				}
				return s.Commit(ctx)
			}
			ctx, target := writer(t, x.s)
			if err := x.host.Reset(ctx, target); !errors.Is(err, ErrWithdrawal) {
				t.Fatal("invalid withdrawal accepted", err)
			}
			if !before.Generation().Same(image(t, x.s).Generation()) {
				t.Fatal("invalid poststate committed")
			}
		})
	}
}

func TestResetLicenseDeadlineRollsBackStagedFloorsAndOriginalLifetime(t *testing.T) {
	x := newFixture(t, staticInput(t))
	addObject(t, x, "object", spec())
	e := reset(t, x)
	accept(t, x, e)
	before := image(t, x.s)
	original := x.host.opts.Withdraw
	x.host.opts.Withdraw = func(ctx context.Context, tx *sqlite.CommandTx, at time.Time) error {
		if err := original(ctx, tx, at); err != nil {
			return err
		}
		x.f.clock.Store(time.Unix(x.f.claims.ExpiresAt, 0).Add(license.Grace).UnixNano())
		return nil
	}
	ctx, target := writer(t, x.s)
	if err := x.host.Reset(ctx, target); !errors.Is(err, ErrStale) {
		t.Fatal("deadline before actual commit did not abort", err)
	}
	if !before.Generation().Same(image(t, x.s).Generation()) {
		t.Fatal("expired reset committed floors/withdrawal")
	}
	x.host.opts.Withdraw = original
	if err := x.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Denied license still permits exact durable withdrawal; no ready resource lease.
	if err := x.host.Reset(ctx, target); err != nil {
		t.Fatal("denied withdrawal rejected", err)
	}
	for _, a := range registry(t, x.s).Authorities() {
		if a.Active {
			t.Fatal("denied withdrawal did not revoke")
		}
	}
	x.f.clock.Add(-int64(license.Grace))
	if err := x.host.ObserveLicense(t.Context()); err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithCancel(ctx)
	if err := x.host.Reset(parent, target); err != nil {
		t.Fatal(err)
	}
	held := capture(t, x)
	defer held.Release()
	cancel()
	if held.Current() {
		t.Fatal("pending outlived original writer lifetime")
	}
	if err := x.host.FenceWriter(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	if x.host.value.Load().phase == pending {
		t.Fatal("writer loss left pending")
	}
	// Old elector callback cannot fence an actual successor incarnation.
	if _, err := x.s.EnablePrimary(t.Context(), image(t, x.s).Generation()); err != nil {
		t.Fatal(err)
	}
	successor, newTarget := writer(t, x.s)
	if err := x.host.Reset(successor, newTarget); err != nil {
		t.Fatal(err)
	}
	pendingE := x.host.value.Load().entitlement
	if err := x.host.FenceWriter(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	if x.host.value.Load().entitlement != pendingE {
		t.Fatal("late old writer fenced successor")
	}
	if err := x.host.Reset(ctx, target); !errors.Is(err, replication.ErrFenced) {
		t.Fatal("old original token adopted successor", err)
	}
}

func TestFinalHookLicenseReplacementExcludedThroughActualCommitAndNoop(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "noop", true: "changed"}[changed], func(t *testing.T) {
			x := newFixture(t, staticInput(t))
			if changed {
				addObject(t, x, "object", spec())
				e := reset(t, x)
				accept(t, x, e)
			}
			original := x.host.opts.Withdraw
			atHook := make(chan struct{})
			leaveHook := make(chan struct{})
			done := make(chan error, 1)
			x.host.opts.Withdraw = func(ctx context.Context, tx *sqlite.CommandTx, at time.Time) error {
				if err := original(ctx, tx, at); err != nil {
					return err
				}
				return tx.RequireCommitCheck(func(context.Context) error { close(atHook); <-leaveHook; return nil })
			}
			ctx, target := writer(t, x.s)
			go func() { done <- x.host.Reset(ctx, target) }()
			<-atHook
			replaced := make(chan error, 1)
			started := make(chan struct{})
			go func() { close(started); replaced <- x.license.Set("", "") }()
			<-started
			select {
			case err := <-replaced:
				t.Fatal("license write escaped full command exclusion", err)
			case <-time.After(20 * time.Millisecond):
			}
			close(leaveHook)
			if err := <-done; err != nil {
				t.Fatal("pinned command failed", err)
			}
			if err := <-replaced; err != nil {
				t.Fatal(err)
			}
			if x.host.current(x.host.value.Load()) {
				t.Fatal("committed cut survived later replacement")
			}
		})
	}
}

func TestFinalHookStaticAndAuthKeyReplacementExcluded(t *testing.T) {
	for _, kind := range []string{"static", "auth-keys"} {
		t.Run(kind, func(t *testing.T) {
			x := newFixture(t, staticInput(t))
			addObject(t, x, "object", spec())
			e := reset(t, x)
			accept(t, x, e)
			in := staticInput(t)
			in.Identity = "new-static"
			s, err := PrepareStatic(t.Context(), in)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Release()
			atHook, leaveHook := make(chan struct{}), make(chan struct{})
			original := x.host.opts.Withdraw
			x.host.opts.Withdraw = func(ctx context.Context, tx *sqlite.CommandTx, at time.Time) error {
				if err := original(ctx, tx, at); err != nil {
					return err
				}
				return tx.RequireCommitCheck(func(context.Context) error { close(atHook); <-leaveHook; return nil })
			}
			done := make(chan error, 1)
			ctx, target := writer(t, x.s)
			go func() { done <- x.host.Reset(ctx, target) }()
			<-atHook
			replaced, started := make(chan error, 1), make(chan struct{})
			go func() {
				close(started)
				if kind == "static" {
					replaced <- x.host.ReplaceStatic(t.Context(), s)
					return
				}
				err := lock(t.Context(), &x.keys, true)
				if err == nil {
					x.keys.Unlock()
				}
				replaced <- err
			}()
			<-started
			select {
			case err := <-replaced:
				close(leaveHook)
				t.Fatal("replacement escaped full command exclusion", err)
			case <-time.After(20 * time.Millisecond):
			}
			close(leaveHook)
			if err := <-done; err != nil {
				t.Fatal("pinned command failed", err)
			}
			if err := <-replaced; err != nil {
				t.Fatal(err)
			}
		})
	}
}
