package main

import (
	"context"
	"net/http"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/unblock"
)

// UnblockCommand grants only this closed request operation. The decide grant
// includes its derived allow entry, policy references and ownership handoff;
// it never grants a general blocklists/policies write capability.
func (s *servingRuntime) UnblockCommand(ctx context.Context, target unblock.Target, d unblock.Decision) (*unblock.Outcome, error) {
	if s == nil || ctx == nil || !target.Valid() {
		return nil, &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable, Message: "current unblock command unavailable"}}
	}
	proof, err := servinghttp.Credential(ctx)
	if err != nil {
		return nil, currentCommandAPIError(err)
	}
	var out *unblock.Outcome
	err = s.currentAuthCommand(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		var err error
		out, err = s.executeUnblockCurrent(ctx, tx, deps, proof, target, d)
		return err
	})
	if err == nil {
		s.publisher.Wake()
	}
	return out, currentCommandAPIError(err)
}

// executeUnblockCurrent admits one closed delegated action using only the
// original concrete transaction, proof and immutable pinned entitlement.
func (s *servingRuntime) executeUnblockCurrent(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, target unblock.Target, d unblock.Decision) (*unblock.Outcome, error) {
	verb := "decide"
	if target.Action == unblock.Delete {
		verb = "delete"
	}
	attrs := rbac.Attributes{Resource: "unblockrequests", Verb: verb, Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Namespace: target.Namespace, Name: target.Name}
	principal, err := servingauth.ReauthorizeCurrentAllWithClock(ctx, tx, deps, proof, []rbac.Attributes{attrs}, s.opts.Now)
	if err != nil {
		return nil, err
	}
	// This immutable entitlement is held by currentAuthCommand's original host
	// pin. Its original absolute deadline is checked again at final commit.
	check := func(context.Context) error {
		now := s.opts.Now().UTC()
		if !deps.License.Requests || deps.License.RequestsFrom.IsZero() || !deps.License.RequestsUntil.After(deps.License.RequestsFrom) || now.Before(deps.License.RequestsFrom) || !now.Before(deps.License.RequestsUntil) {
			return &api.Error{Status: http.StatusPaymentRequired, Body: api.ErrorBody{Key: api.KeyLicense, Message: "unblock requests need a license key"}}
		}
		return nil
	}
	if err := check(ctx); err != nil {
		return nil, err
	}
	if err := tx.RequireCommitCheck(check); err != nil {
		return nil, err
	}
	actor := audit.Actor{Kind: string(principal.Kind), Name: principal.Name, ID: principal.ID}.String()
	d.Actor = actor
	return unblock.ExecuteCurrent(store.WithActor(ctx, actor), tx, target, d, s.opts.Now().UTC())
}

// SweepUnblock is an explicit system writer command. It binds the original
// elected episode and full dependency exclusions, without an HTTP lease or a
// human grant. Expiring a prior approval remains available after license loss.
func (s *servingRuntime) SweepUnblock(ctx context.Context) (unblock.SweepResult, error) {
	var out unblock.SweepResult
	status := s.primary.driver.Status()
	if !status.Running {
		return out, replication.ErrFenced
	}
	writer := s.primary.writer.active.Load()
	ctx, release, err := s.primary.writer.bind(ctx, status.Writer)
	if err != nil {
		return out, err
	}
	defer release()
	target := serving.TermActivation{Term: status.Writer.Term, Incarnation: status.Writer.Incarnation}
	check := func(ctx context.Context) error {
		if !s.primary.writer.current(writer) {
			return replication.ErrFenced
		}
		return s.writerCurrent(ctx, target)
	}
	if err := check(ctx); err != nil {
		return out, err
	}
	err = s.providers.WithCommand(ctx, target, func(ctx context.Context, tx *sqlite.CommandTx, _ servingauth.Dependencies) error {
		if err := check(ctx); err != nil {
			return err
		}
		if err := tx.RequireCommitCheck(check); err != nil {
			return err
		}
		var settings public.Kudurru
		if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
			return err
		}
		document, err := servingSettingsDocument(&settings)
		if err != nil {
			return err
		}
		cfg, _, err := config.ApplyClusterDocument(s.opts.Config, document)
		if err != nil {
			return err
		}
		out, err = unblock.SweepCurrent(store.WithActor(ctx, unblock.SystemActor), tx, s.opts.Now().UTC(), cfg.UnblockRequests.PendingExpiry.Std(), cfg.UnblockRequests.Retention.Std())
		return err
	})
	if err == nil {
		s.publisher.Wake()
	}
	return out, err
}
