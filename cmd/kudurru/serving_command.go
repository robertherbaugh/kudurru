package main

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// pinProviderHost runs inside the coordinator's resource exclusion. It retains
// every lower publication dependency across the entire current command, so the
// original HTTP lease cannot lose a manager/catalog/key installation between
// its final memory check and SQLite commit. Acquisition performs no network work
// inside this callback. The source wrappers keep their existing identical order.
func (s *servingRuntime) pinProviderHost(ctx context.Context, fn func() error) error {
	return s.host.PinHostCurrent(ctx, func(current func() bool) error {
		if current == nil || !current() {
			return serving.ErrDependency
		}
		return s.opts.Keyrings.WithCurrent(ctx, func(_ servingbuild.KeyringCut) error {
			return s.catalog.WithCurrent(ctx, func() error {
				return s.opts.Transferred.WithCurrent(ctx, func() error {
					if !current() {
						return serving.ErrDependency
					}
					return fn()
				})
			})
		})
	})
}

// currentCommand admits ordinary HTTP mutations under their original serving
// lease and elected writer. fn uses only the supplied concrete transaction;
// publication waits and network work belong after this method returns. Provider
// source/authority replacements use the separate exclusive resource protocol.
func (s *servingRuntime) currentCommand(ctx context.Context, attrs rbac.Attributes, fn func(context.Context, *sqlite.CommandTx, auth.Principal) error) error {
	if s == nil || ctx == nil || fn == nil {
		return serving.ErrComponents
	}
	proof, err := servinghttp.Credential(ctx)
	if err != nil {
		return err
	}
	return s.currentAuthCommand(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		principal, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, attrs, s.opts.Now)
		if err != nil {
			return err
		}
		return fn(ctx, tx, principal)
	})
}

// currentAuthCommand retains the original HTTP lease, elected writer episode,
// and complete dependency pin through the actual transaction. It grants no
// authentication or permission by itself: login/self-service callbacks must
// verify their raw proof and current account authority before reading private
// data, mutating state, or returning a deduplicated result.
func (s *servingRuntime) currentAuthCommand(ctx context.Context, fn func(context.Context, *sqlite.CommandTx, servingauth.Dependencies) error) error {
	if s == nil || ctx == nil || fn == nil {
		return serving.ErrComponents
	}
	status := s.primary.driver.Status()
	if !status.Running {
		return replication.ErrFenced
	}
	writer := s.primary.writer.active.Load()
	ctx, release, err := s.primary.writer.bind(ctx, status.Writer)
	if err != nil {
		return err
	}
	defer release()
	target := serving.TermActivation{Term: status.Writer.Term, Incarnation: status.Writer.Incarnation}
	check := func(ctx context.Context) error {
		// Read the original episode directly: its cancellation can precede
		// propagation through bind's AfterFunc to this request context.
		if !s.primary.writer.current(writer) {
			return replication.ErrFenced
		}
		if err := s.writerCurrent(ctx, target); err != nil {
			return err
		}
		return servinghttp.CheckWriter(ctx, s.publisher, target)
	}
	if err := check(ctx); err != nil {
		return err
	}
	return s.providers.WithCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		if err := check(ctx); err != nil {
			return err
		}
		if err := tx.RequireCommitCheck(check); err != nil {
			return err
		}
		return fn(ctx, tx, deps)
	})
}
