package main

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/store/kube"
)

const kubeBlockingTimeout = 5 * time.Second

// kubeBlockingRuntime serializes local observations and writes so a slow read
// cannot overwrite this process's newer confirmed command. Polling is read-only;
// every mutation has one original lifetime, final authorization and CAS attempt.
type kubeBlockingRuntime struct {
	store      *kube.Store
	toggle     *blockingstage.Toggle
	config     *config.Reloadable
	authorizer rbac.Authorizer
	gate       chan struct{}
	observed   atomic.Pointer[kube.BlockingRuntimeState]
	available  atomic.Bool
}

func newKubeBlockingRuntime(st *kube.Store, toggle *blockingstage.Toggle, cfg *config.Reloadable, authorizer rbac.Authorizer) *kubeBlockingRuntime {
	return &kubeBlockingRuntime{store: st, toggle: toggle, config: cfg, authorizer: authorizer, gate: make(chan struct{}, 1)}
}

func (r *kubeBlockingRuntime) lock(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case r.gate <- struct{}{}:
		return nil
	}
}

func (r *kubeBlockingRuntime) apply(state kube.BlockingRuntimeState) {
	old := r.observed.Load()
	// An unchanged observation must not rearm a timer already consumed locally.
	if old == nil || old.UID != state.UID || !old.Until.Equal(state.Until) {
		r.toggle.Apply(state.Until)
	}
	r.observed.Store(&state)
	r.available.Store(true)
}

func (r *kubeBlockingRuntime) refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, kubeBlockingTimeout)
	defer cancel()
	if err := r.lock(ctx); err != nil {
		return err
	}
	defer func() { <-r.gate }()
	state, err := r.store.ReadBlockingRuntime(ctx)
	if err != nil || ctx.Err() != nil {
		r.available.Store(false)
		if err != nil {
			return err
		}
		return ctx.Err()
	}
	r.apply(state)
	return nil
}

func (r *kubeBlockingRuntime) Run(ctx context.Context) error {
	for {
		_ = r.refresh(ctx)
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (r *kubeBlockingRuntime) BlockingState(ctx context.Context, now time.Time) (bool, bool, time.Time, error) {
	if err := ctx.Err(); err != nil {
		return false, false, time.Time{}, err
	}
	if !r.available.Load() || now.IsZero() {
		return false, false, time.Time{}, kubeBlockingUnavailable(nil)
	}
	configured := r.config.Current().Blocking.Enabled
	until := r.toggle.DisabledUntil()
	return configured, configured && (until.IsZero() || !now.Before(until)), until, nil
}

func kubeBlockingUnavailable(hint any) error {
	return &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable,
		Message: "cluster blocking state unavailable", Hint: hint}}
}

func (r *kubeBlockingRuntime) Command(ctx context.Context, change api.BlockingChange) (*api.BlockingState, error) {
	// Keep the original request deadline if it is earlier than this command cap.
	ctx, cancel := context.WithTimeout(ctx, kubeBlockingTimeout)
	defer cancel()
	status, err := blockingStatus(change, time.Now())
	if err != nil {
		return nil, err
	}
	until := time.Time{}
	if status.Disabled {
		until = blockingstage.Forever
		if status.DisabledUntil != nil {
			until = status.DisabledUntil.Time
		}
	}
	if err := r.lock(ctx); err != nil {
		return nil, kubeBlockingUnavailable(nil)
	}
	defer func() { <-r.gate }()
	admit := func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := auth.Authorize(ctx, r.authorizer, blockingToggleAttrs); err != nil {
			return &api.Error{Status: err.Status, Body: api.ErrorBody{Key: err.Key, Message: err.Message, Hint: err.Hint}}
		}
		return ctx.Err()
	}
	receipt, err := r.store.ChangeBlockingRuntime(ctx, until, admit)
	if err != nil {
		if !receipt.Dispatched {
			if problem, ok := err.(*api.Error); ok {
				return nil, problem
			}
		}
		if apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err) {
			return nil, &api.Error{Status: http.StatusConflict, Body: api.ErrorBody{Key: api.KeyConflict, Message: "cluster blocking state changed; this command was not applied"}}
		}
		hint := map[string]any{"dispatched": receipt.Dispatched, "outcome": "not_dispatched"}
		if receipt.Committed {
			hint["outcome"], hint["committed"] = "committed", true
		} else if receipt.Dispatched {
			hint["outcome"] = "unknown"
		}
		return nil, kubeBlockingUnavailable(hint)
	}
	// A confirmed commit remains a commit even if its original request expires.
	r.apply(receipt.After)
	if err := admit(ctx); err != nil {
		return nil, kubeBlockingUnavailable(map[string]any{"committed": true, "resourceVersion": receipt.After.ResourceVersion})
	}
	now := time.Now()
	configured, enabled, deadline, err := r.BlockingState(ctx, now)
	if err != nil {
		return nil, kubeBlockingUnavailable(map[string]any{"committed": true})
	}
	result := &api.BlockingState{Configured: configured, Blocking: enabled, Scope: "cluster"}
	if configured && !enabled && deadline.After(now) && deadline.Before(blockingstage.Forever) {
		u, remaining := deadline.UTC(), deadline.Sub(now).Seconds()
		result.Until, result.Timer = &u, &remaining
	}
	return result, nil
}
