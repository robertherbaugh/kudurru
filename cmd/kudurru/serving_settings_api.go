package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/providercoordinator"
	"github.com/robertherbaugh/kudurru/internal/auth/providerhost"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Settings derives a detached effective configuration from the request's exact
// auth/settings owner and this runtime's immutable startup host configuration.
// No live store or reload pointer participates in a leased settings read.
func (s *servingRuntime) Settings(ctx context.Context) (*config.Config, error) {
	view, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return nil, currentCommandAPIError(err)
	}
	return s.settingsFromView(view)
}

// WriteSettings accepts the API's validated next values only for named public
// keys. currentCommand rejects any intervening durable/dependency replacement,
// including no-ops, so a merge prepared from the original lease cannot overwrite
// a newer map. It never writes the legacy file or independently reloads DNS.
func (s *servingRuntime) WriteSettings(ctx context.Context, next *config.Config, changed []string) error {
	attrs := rbac.Attributes{Resource: "settings", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}
	err := s.currentCommand(ctx, attrs, func(ctx context.Context, tx *sqlite.CommandTx, _ auth.Principal) error {
		plan, err := api.PrepareCanonicalSettingsValues(ctx, tx, s.opts.Config, next, changed)
		if err != nil {
			return err
		}
		if err := s.checkSettingsBranding(ctx, tx, plan); err != nil {
			return err
		}
		return plan.Apply(ctx, tx)
	})
	if err == nil {
		s.publisher.Wake()
	}
	return currentCommandAPIError(err)
}

// PrepareSettings merges imported settings in the existing concrete command.
// It shares canonical PATCH persistence and retains the original whole view's
// branding entitlement through the enclosing transaction's final commit check.
func (s *servingRuntime) PrepareSettings(ctx context.Context, tx *sqlite.CommandTx, patch map[string]any) (*api.CanonicalSettingsPlan, error) {
	plan, err := api.PrepareCanonicalSettingsPatch(ctx, tx, s.opts.Config, patch)
	if err != nil {
		return nil, err
	}
	if err := s.checkSettingsBranding(ctx, tx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

func (s *servingRuntime) checkSettingsBranding(ctx context.Context, tx *sqlite.CommandTx, plan *api.CanonicalSettingsPlan) error {
	if !plan.Branding() {
		return nil
	}
	view, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return err
	}
	check := func(context.Context) error { return brandingWriteCheck(view, s.opts.Now()) }
	if err := check(ctx); err != nil {
		return err
	}
	return tx.RequireCommitCheck(check)
}

func servingSettingsDocument(settings *public.Kudurru) (map[string]any, error) {
	document := map[string]any{}
	for key, raw := range settings.Spec.Settings {
		var value any
		if err := json.Unmarshal(raw.Raw, &value); err != nil {
			return nil, err
		}
		document[key] = value
	}
	return document, nil
}

// currentCommandAPIError exposes stable refusal categories without returning
// private credential/provider diagnostics to a caller.
func currentCommandAPIError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, servingauth.ErrCredentials):
		return &api.Error{Status: http.StatusUnauthorized, Body: api.ErrorBody{Key: api.KeyUnauthenticated, Message: "current credential is invalid or expired"}}
	case errors.Is(err, servingauth.ErrDenied):
		return &api.Error{Status: http.StatusForbidden, Body: api.ErrorBody{Key: api.KeyForbidden, Message: "current authority does not permit this command"}}
	case errors.Is(err, replication.ErrFenced), errors.Is(err, replication.ErrReadOnly), errors.Is(err, serving.ErrPending), errors.Is(err, serving.ErrSuperseded), errors.Is(err, serving.ErrDependency), errors.Is(err, serving.ErrReleased), errors.Is(err, serving.ErrClosed), errors.Is(err, primaryruntime.ErrUnavailable),
		errors.Is(err, providercoordinator.ErrStale), errors.Is(err, providercoordinator.ErrClosed), errors.Is(err, providerresources.ErrStale), errors.Is(err, providerresources.ErrUnavailable), errors.Is(err, providerresources.ErrClosed),
		errors.Is(err, providerhost.ErrStale), errors.Is(err, providerhost.ErrContinuity), errors.Is(err, providerhost.ErrClosed), errors.Is(err, servingauth.ErrDependency):
		return &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable, Message: "current writable serving generation unavailable"}}
	default:
		return err
	}
}

var _ api.CanonicalSettings = (*servingRuntime)(nil)
var _ api.CanonicalSettingsPlanner = (*servingRuntime)(nil)
