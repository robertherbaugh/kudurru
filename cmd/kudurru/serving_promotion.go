package main

import (
	"context"
	"errors"

	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// withPromotionAuthorization runs inside Coordinator's queue and publication
// pin. Retain the original HTTP proof and every lower dependency through the
// actual Store CAS; neither a background worker nor a claimed principal grants
// promotion. Final checks use only original memory and time evidence.
func (s *servingRuntime) withPromotionAuthorization(ctx context.Context, req clusterreplica.PromoteRequest, apply func(func(context.Context) error, sqlite.AuditEntry) error) error {
	proof, err := servinghttp.Credential(ctx)
	if err != nil {
		return errors.Join(clusterreplica.ErrForbidden, err)
	}
	lease, err := servinghttp.CloneLease(ctx)
	if err != nil {
		return err
	}
	defer lease.Release()
	token, err := lease.SourceToken()
	if err != nil {
		return err
	}
	view, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return err
	}
	if !token.Generation().Same(req.ExpectedInstalled) || !token.Authority().Term.Same(req.ExpectedTerm) {
		return replication.ErrCAS
	}
	return s.resources.WithCurrent(ctx, func(resources *providerresources.Pinned) error {
		return s.host.PinHostCurrent(ctx, func(hostCurrent func() bool) error {
			return s.opts.Keyrings.WithCurrent(ctx, func(keys servingbuild.KeyringCut) error {
				return s.catalog.WithCurrent(ctx, func() error {
					return s.opts.Transferred.WithCurrent(ctx, func() error {
						current := func(ctx context.Context) error {
							if err := ctx.Err(); err != nil {
								return err
							}
							if hostCurrent == nil || !hostCurrent() || !keys.Current() {
								return serving.ErrDependency
							}
							if err := resources.CurrentAt(s.opts.Now()); err != nil {
								return err
							}
							if !token.Matches(s.opts.Store.Authority()) {
								return replication.ErrCAS
							}
							return s.publisher.CheckReadLease(ctx, lease)
						}
						if err := current(ctx); err != nil {
							return err
						}
						principal, decision, lifetime, err := view.AuthorizeOriginalTransition(ctx, proof, servingHTTPCluster("cluster", "promote"), s.opts.Now)
						if err != nil {
							return err
						}
						check := func(ctx context.Context) error {
							if err := lifetime.Check(ctx); err != nil {
								return err
							}
							return current(ctx)
						}
						return apply(check, sqlite.AuditEntry{Actor: string(principal.Kind) + ":" + principal.Name, Authz: decision.Path, Reason: req.Reason})
					})
				})
			})
		})
	})
}
