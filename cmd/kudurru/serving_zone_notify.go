package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/servingzones"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// ZoneNotifyCommand queues only the original request's exact committed head.
// Delivery and its CAS acknowledgment remain the background sender's job.
func (s *servingRuntime) ZoneNotifyCommand(ctx context.Context, name string) (*api.NotifyResult, error) {
	if s == nil || ctx == nil || name == "" || len(name) > 253 || strings.ContainsRune(name, 0) ||
		s.primary == nil || s.primary.notifyIntent == nil {
		return nil, currentCommandAPIError(serving.ErrComponents)
	}
	proof, err := servinghttp.Credential(ctx)
	if err != nil {
		return nil, currentCommandAPIError(err)
	}
	runtime, err := servinghttp.Component[*servingbuild.RuntimeOwner](ctx, "runtime")
	if err != nil {
		return nil, currentCommandAPIError(err)
	}
	zones, err := servinghttp.Component[*servingzones.Prepared](ctx, "zones")
	if err != nil || zones == nil || runtime == nil || runtime.Primary() == nil ||
		runtime.Zones() != zones || !runtime.Primary().Base().Same(zones.Base()) {
		return nil, currentCommandAPIError(serving.ErrComponents)
	}
	id, keys, current := runtime.NotifyKeys()
	if id == "" || len(id) > 512 || keys == nil || current == nil || !current() {
		return nil, currentCommandAPIError(serving.ErrDependency)
	}
	attrs := rbac.Attributes{Resource: "zones", Verb: "transfer", Scope: rbac.Scope{Kind: rbac.ScopeZone, Name: name}}
	var result *api.NotifyResult
	var decision error
	changed := false
	_, err = s.currentAuthCommandThen(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		if _, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, attrs, s.opts.Now); err != nil {
			return err
		}
		observed := s.opts.Now()
		if observed.IsZero() || !current() {
			return serving.ErrDependency
		}
		var admitted *pr.PrimaryStatusObservation
		final := func(ctx context.Context) error {
			now := s.opts.Now()
			if ctx.Err() != nil || now.IsZero() || now.Before(observed) || !current() {
				return serving.ErrDependency
			}
			if admitted != nil && !notifyHeadAvailable(runtime, zones, *admitted, now) {
				return serving.ErrPending
			}
			return nil
		}
		if err := tx.RequireCommitCheck(final); err != nil {
			return err
		}
		var z public.DNSZone
		if err := tx.Get(ctx, store.Key{Kind: "DNSZone", Name: name}, &z); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				decision = &api.Error{Status: http.StatusNotFound, Body: api.ErrorBody{Key: api.KeyNotFound, Message: "zone not found"}}
				return nil // Final raw clocks/original writer still apply.
			}
			return err
		}
		if z.Spec.Fallback != nil {
			decision = &api.Error{Status: http.StatusUnprocessableEntity, Body: api.ErrorBody{Key: api.KeyUnprocessable, Message: "NOTIFY is unavailable for zones with missing-name fallback"}}
			return nil
		}
		if z.Spec.Type != public.ZoneTypePrimary {
			decision = &api.Error{Status: http.StatusConflict, Body: api.ErrorBody{Key: api.KeyConflict, Message: "manual NOTIFY is available for primary zones"}}
			return nil
		}
		o, ok := runtime.Primary().CommittedPrimaryStatus(name)
		if !ok || !o.ConfigMatchesDesired || !notifyHeadAvailable(runtime, zones, o, observed) {
			return serving.ErrPending
		}
		admitted = &o
		intent, err := s.primary.notifyIntent(ctx, tx, o)
		if err != nil {
			if errors.Is(err, pr.ErrConflict) || errors.Is(err, pr.ErrInvalid) || errors.Is(err, pr.ErrCapacity) ||
				errors.Is(err, zonestate.ErrConflict) || errors.Is(err, zonestate.ErrInvalid) || errors.Is(err, zonestate.ErrCapacity) || errors.Is(err, store.ErrNotFound) {
				return serving.ErrPending
			}
			return err
		}
		if intent.Head == nil {
			return serving.ErrComponents
		}
		targets := make([]string, 0)
		if z.Spec.Policy.Notify != "" && z.Spec.Policy.Notify != public.NotifyNone {
			route := zones.Zones(observed).Match(o.Member.Zone.Apex)
			configured := route.NotifyTargets()
			if len(configured) > 64 {
				return serving.ErrComponents
			}
			for _, target := range configured {
				targets = append(targets, target.String())
			}
		}
		result = &api.NotifyResult{Zone: name, Targets: targets}
		changed = intent.Changed
		return final(ctx)
	}, nil)
	if err != nil {
		return nil, currentCommandAPIError(err)
	}
	if decision != nil {
		return nil, decision
	}
	if result == nil {
		return nil, currentCommandAPIError(serving.ErrComponents)
	}
	if changed {
		s.publisher.Wake()
		s.primary.driver.Wake()
	}
	return result, nil
}

func notifyHeadAvailable(runtime *servingbuild.RuntimeOwner, zones *servingzones.Prepared, o pr.PrimaryStatusObservation, now time.Time) bool {
	if !o.ConfigMatchesDesired || !runtime.Primary().Admitted(o.Member.Zone.Apex, now) ||
		(!o.ExpiresAt.IsZero() && !now.Before(o.ExpiresAt)) {
		return false
	}
	set := zones.Zones(now)
	route := set.Match(o.Member.Zone.Apex)
	return set.Synced() && route != nil && route.Name() == o.Member.Zone.Apex &&
		route.Type() == public.ZoneTypePrimary && route.AvailableAt(now) && route.Serial() == o.Serial &&
		route.ContentHash() == o.ContentHash && route.RecordCount() == o.RecordCount
}
