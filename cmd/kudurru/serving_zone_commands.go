package main

import (
	"context"
	"net/http"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ZoneCommand executes one closed compound zone operation under the original
// HTTP proof, writer and full serving exclusions. No successor is sampled.
func (s *servingRuntime) ZoneCommand(ctx context.Context, target api.ZoneCommandTarget, prepare api.ZoneCommandPrepare) error {
	if s == nil || ctx == nil || prepare == nil || !zoneCommandTarget(target) {
		return &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable, Message: "current zone command unavailable"}}
	}
	proof, err := servinghttp.Credential(ctx)
	if err != nil {
		return currentCommandAPIError(err)
	}
	err = s.currentAuthCommand(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		var completed atomic.Bool
		check := func(context.Context) error {
			if !completed.Load() {
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
		if !zoneCommandRequirements(target, attrs) {
			return servingauth.ErrDenied
		}
		if _, err := servingauth.ReauthorizeCurrentAllWithClock(ctx, tx, deps, proof, attrs, s.opts.Now); err != nil {
			return err
		}
		if err := plan.Apply(ctx, tx); err != nil {
			return err
		}
		completed.Store(true)
		return check(ctx)
	})
	if err == nil {
		s.publisher.Wake()
	}
	return currentCommandAPIError(err)
}

func zoneCommandTarget(target api.ZoneCommandTarget) bool {
	if target.Zone == "" || len(target.Zone) > 253 {
		return false
	}
	switch target.Action {
	case api.ZoneCommandUpsertRecord, api.ZoneCommandDeleteRecord, api.ZoneCommandImport, api.ZoneCommandDeleteZone:
		return true
	default:
		return false
	}
}

func zoneCommandRequirements(target api.ZoneCommandTarget, attrs []rbac.Attributes) bool {
	if !zoneCommandTarget(target) || len(attrs) == 0 || len(attrs) > 3 {
		return false
	}
	seen := make(map[string]bool, len(attrs))
	for _, attr := range attrs {
		if attr.Namespace != "" || attr.Name != target.Zone {
			return false
		}
		key := attr.Resource + "." + attr.Verb
		if seen[key] {
			return false
		}
		seen[key] = true
		if key == "zones.write" && target.Action == api.ZoneCommandImport {
			if attr.Scope != (rbac.Scope{Kind: rbac.ScopeCluster}) {
				return false
			}
		} else if attr.Scope != (rbac.Scope{Kind: rbac.ScopeZone, Name: target.Zone}) {
			return false
		}
		switch target.Action {
		case api.ZoneCommandUpsertRecord:
			if key != "records.write" {
				return false
			}
		case api.ZoneCommandDeleteRecord:
			if key != "records.delete" {
				return false
			}
		case api.ZoneCommandImport:
			if key != "records.write" && key != "records.delete" && key != "zones.write" {
				return false
			}
		case api.ZoneCommandDeleteZone:
			if key != "zones.delete" && key != "records.delete" {
				return false
			}
		}
	}
	switch target.Action {
	case api.ZoneCommandUpsertRecord, api.ZoneCommandImport:
		return seen["records.write"]
	case api.ZoneCommandDeleteRecord:
		return seen["records.delete"]
	case api.ZoneCommandDeleteZone:
		return seen["zones.delete"]
	}
	return false
}
