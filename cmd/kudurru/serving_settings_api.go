package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

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
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// Settings derives a detached effective configuration from the request's exact
// auth/settings owner and this runtime's immutable startup host configuration.
// No live store or reload pointer participates in a leased settings read.
func (s *servingRuntime) Settings(ctx context.Context) (*config.Config, error) {
	view, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return nil, currentCommandAPIError(err)
	}
	settings, err := view.Settings()
	if err != nil {
		return nil, currentCommandAPIError(err)
	}
	document, err := servingSettingsDocument(settings)
	if err != nil {
		return nil, err
	}
	prepared, _, err := config.ApplyClusterDocument(s.opts.Config, document)
	return prepared, err
}

// WriteSettings accepts the API's validated next values only for named public
// keys. currentCommand rejects any intervening durable/dependency replacement,
// including no-ops, so a merge prepared from the original lease cannot overwrite
// a newer map. It never writes the legacy file or independently reloads DNS.
func (s *servingRuntime) WriteSettings(ctx context.Context, next *config.Config, changed []string) error {
	if next == nil || len(changed) == 0 || len(changed) > len(config.Schema) {
		return &api.Error{Status: http.StatusBadRequest, Body: api.ErrorBody{Key: api.KeyInvalid, Message: "settings change is empty or invalid"}}
	}
	flat := config.Flatten(next)
	seen := map[string]bool{}
	for _, path := range changed {
		key, ok := config.Lookup(path)
		if !ok || !config.PublicClusterKey(key) || seen[path] {
			return &api.Error{Status: http.StatusBadRequest, Body: api.ErrorBody{Key: api.KeyInvalid, Message: "settings command requires unique public cluster keys"}}
		}
		if slices.Contains(s.opts.Config.EnvLocked, path) {
			return &api.Error{Status: http.StatusConflict, Body: api.ErrorBody{Key: api.KeyEnvLocked, Message: path + " is set by the environment"}}
		}
		seen[path] = true
	}
	attrs := rbac.Attributes{Resource: "settings", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}
	err := s.currentCommand(ctx, attrs, func(ctx context.Context, tx *sqlite.CommandTx, _ auth.Principal) error {
		var settings public.Kudurru
		if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
			return err
		}
		document, err := servingSettingsDocument(&settings)
		if err != nil {
			return err
		}
		before, err := json.Marshal(document)
		if err != nil {
			return err
		}
		for _, path := range changed {
			parts := strings.Split(path, ".")
			parent := document
			for _, part := range parts[:len(parts)-1] {
				child, ok := parent[part].(map[string]any)
				if !ok {
					child = map[string]any{}
					parent[part] = child
				}
				parent = child
			}
			parent[parts[len(parts)-1]] = flat[path]
		}
		if _, _, err := config.ApplyClusterDocument(s.opts.Config, document); err != nil {
			return &api.Error{Status: http.StatusBadRequest, Body: api.ErrorBody{Key: api.KeyInvalid, Message: err.Error()}}
		}
		after, err := json.Marshal(document)
		if err != nil {
			return err
		}
		if bytes.Equal(before, after) {
			return nil // Original raw auth and final checks still run.
		}
		settings.Spec.Settings = map[string]apiextensionsv1.JSON{}
		for key, value := range document {
			raw, err := json.Marshal(value)
			if err != nil {
				return err
			}
			settings.Spec.Settings[key] = apiextensionsv1.JSON{Raw: raw}
		}
		_, err = tx.Apply(ctx, &settings, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	if err == nil {
		s.publisher.Wake()
	}
	return currentCommandAPIError(err)
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
