package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// newServingNodeCommand owns this node's local document and actual logger level.
// Mount with the SAME Reload as the API and no legacy reload subscribers. The
// independent gate is never acquired by publication, dependency pins or tx code.
func newServingNodeCommand(runtime *servingRuntime, reload *config.Reloadable, level *slog.LevelVar) (api.NodeConfigCommand, error) {
	if runtime == nil || runtime.primary == nil || runtime.providers == nil || reload == nil || reload.Current() == nil || level == nil {
		return nil, serving.ErrComponents
	}
	node := reload.Current().Node.Name
	hostLog := config.NodeLogBaseline(reload.Current())
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return func(ctx context.Context, next *config.Config, changed []string, reset ...string) (api.NodeConfigReceipt, error) {
		var zero api.NodeConfigReceipt
		invalid := func() (api.NodeConfigReceipt, error) {
			return zero, &api.Error{Status: http.StatusBadRequest, Body: api.ErrorBody{Key: api.KeyInvalid, Message: "node command requires unique log.level/log.format leaves"}}
		}
		if ctx == nil || next == nil || len(changed) == 0 || len(changed) > 2 || len(reset) > 2 {
			return invalid()
		}
		requested := next.Log // ignore every field not explicitly requested
		paths := slices.Clone(changed)
		resets := map[string]bool{}
		for _, path := range reset {
			if resets[path] || !slices.Contains(paths, path) {
				return invalid()
			}
			resets[path] = true
		}
		seen := map[string]bool{}
		for _, path := range paths {
			if seen[path] || (path != "log.level" && path != "log.format") || (path == "log.level" && !resets[path] && len(requested.Level) > 256) || (path == "log.format" && !resets[path] && requested.Format != "json" && requested.Format != "text") {
				return invalid()
			}
			seen[path] = true
		}
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if _, err := servinghttp.Identity(ctx); err != nil {
			return zero, currentCommandAPIError(err)
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-gate:
		}
		defer func() { gate <- struct{}{} }()
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		before, err := cloneServingNodeConfig(reload.Current())
		if err != nil {
			return zero, currentCommandAPIError(err)
		}
		installed, err := cloneServingNodeConfig(before)
		if err != nil {
			return zero, err
		}
		if before.Node.Name != node {
			return zero, currentCommandAPIError(serving.ErrDependency)
		}
		for _, path := range paths {
			if !resets[path] && slices.Contains(before.EnvLocked, path) {
				return zero, &api.Error{Status: http.StatusConflict, Body: api.ErrorBody{Key: api.KeyEnvLocked, Message: path + " is set by the environment"}}
			}
			if !resets[path] && config.NodeLogLocked(before, path) {
				return zero, &api.Error{Status: http.StatusConflict, Body: api.ErrorBody{Key: api.KeyHostLocked, Message: path + " is declared in the node file"}}
			}
			if path == "log.level" {
				installed.Log.Level = requested.Level
				if resets[path] {
					installed.Log.Level = hostLog.Level
					if config.NodeLogLocked(before, path) || slices.Contains(before.EnvLocked, path) {
						installed.Log.Level = before.Log.Level
					}
				}
			} else {
				installed.Log.Format = requested.Format
				if resets[path] {
					installed.Log.Format = hostLog.Format
					if config.NodeLogLocked(before, path) || slices.Contains(before.EnvLocked, path) {
						installed.Log.Format = before.Log.Format
					}
				}
			}
		}
		if err := installed.Validate(); err != nil {
			return zero, &api.Error{Status: http.StatusBadRequest, Body: api.ErrorBody{Key: api.KeyInvalid, Message: err.Error()}}
		}
		after, err := cloneServingNodeConfig(installed)
		if err != nil {
			return zero, err
		}
		proof, err := servinghttp.Credential(ctx)
		if err != nil {
			return zero, currentCommandAPIError(err)
		}
		_, err = runtime.currentAuthCommandThen(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
			_, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, rbac.Attributes{Resource: "node", Verb: "config", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}, runtime.opts.Now)
			if err != nil {
				return err
			}
			local, err := tx.NodeLogSettings(ctx, node)
			if err != nil {
				return err
			}
			for _, path := range paths {
				if path == "log.level" {
					value := installed.Log.Level
					local.Level = &value
					if resets[path] {
						local.Level = nil
					}
				} else {
					value := installed.Log.Format
					local.Format = &value
					if resets[path] {
						local.Format = nil
					}
				}
			}
			// A previous commit may have outlived its canceled acknowledgment.
			// Reconcile all desired log leaves from this original transaction,
			// preserving explicit host/environment authority over dormant values.
			installed.Log = hostLog
			if local.Level != nil {
				installed.Log.Level = *local.Level
			}
			if local.Format != nil {
				installed.Log.Format = *local.Format
			}
			if config.NodeLogLocked(before, "log.level") || slices.Contains(before.EnvLocked, "log.level") {
				installed.Log.Level = before.Log.Level
			}
			if config.NodeLogLocked(before, "log.format") || slices.Contains(before.EnvLocked, "log.format") {
				installed.Log.Format = before.Log.Format
			}
			if err := installed.Validate(); err != nil {
				return err
			}
			after.Log = installed.Log
			return tx.PutNodeLogSettings(ctx, local)
		}, func(ctx context.Context, _ serving.TermActivation) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			// Local overlay and final admission have committed atomically. Logger
			// adoption is outside SQLite; later cancellation may return an error
			// after the durable preference or desired value was accepted.
			if seen["log.level"] || installed.Log.Level != before.Log.Level {
				level.Set(parseLevel(installed.Log.Level))
			}
			reload.Set(installed)
			return nil
		})
		if err != nil {
			return zero, currentCommandAPIError(err)
		}
		return api.NodeConfigReceipt{Before: before, After: after, Persisted: true}, nil
	}, nil
}

func cloneServingNodeConfig(source *config.Config) (*config.Config, error) {
	if source == nil {
		return nil, serving.ErrComponents
	}
	raw, err := json.Marshal(source)
	if err != nil {
		return nil, err
	}
	var out config.Config
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	out.EnvLocked, out.Source = slices.Clone(source.EnvLocked), source.Source
	config.CopyNodeLogSource(&out, source)
	return &out, nil
}
