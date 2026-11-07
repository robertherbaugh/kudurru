package main

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// DomainCommand admits a sealed convenience plan under the original raw proof,
// dependency exclusions and elected writer, retained through final commit.
func (s *servingRuntime) DomainCommand(ctx context.Context, target api.DomainCommandTarget, prepare api.DomainCommandPrepare) error {
	if s == nil || ctx == nil || prepare == nil || !domainCommandTarget(target) {
		return &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable, Message: "current domain command unavailable"}}
	}
	proof, err := servinghttp.Credential(ctx)
	if err != nil {
		return currentCommandAPIError(err)
	}
	err = s.currentAuthCommand(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		var complete atomic.Bool
		check := func(context.Context) error {
			if !complete.Load() {
				return servingauth.ErrDenied
			}
			return nil
		}
		if err := tx.RequireCommitCheck(check); err != nil {
			return err
		}
		plan, err := prepare(ctx, tx)
		if plan != nil {
			defer plan.Release()
		}
		if err != nil {
			return err
		}
		if !plan.PreparedFor(tx, target) {
			return servingauth.ErrDenied
		}
		attrs := plan.Requirements()
		if len(attrs) != 1 || attrs[0] != (rbac.Attributes{Resource: "blocklists", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}) {
			return servingauth.ErrDenied
		}
		if _, err := servingauth.ReauthorizeCurrentAllWithClock(ctx, tx, deps, proof, attrs, s.opts.Now); err != nil {
			return err
		}
		if err := plan.Apply(ctx, tx); err != nil {
			return err
		}
		complete.Store(true)
		return check(ctx)
	})
	if err == nil {
		s.publisher.Wake()
	}
	return currentCommandAPIError(err)
}

func domainCommandTarget(target api.DomainCommandTarget) bool {
	if target.Action != domainlist.Allow && target.Action != domainlist.Deny {
		return false
	}
	switch target.Operation {
	case "create", "patch", "delete", "batch", "add", "query":
		return true
	}
	return false
}
