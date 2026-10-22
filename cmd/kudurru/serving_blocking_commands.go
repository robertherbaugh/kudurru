package main

import (
	"context"
	"errors"
	"math"
	"net/http"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	blockingstage "github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const blockingPublicationWait = 10 * time.Second

var blockingToggleAttrs = rbac.Attributes{Resource: "blocking", Verb: "toggle", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}

type blockingReceipt struct {
	Committed       bool       `json:"committed"`
	SettingsUID     string     `json:"settingsUID"`
	ResourceVersion string     `json:"resourceVersion"`
	Disabled        bool       `json:"disabled"`
	DisabledUntil   *time.Time `json:"disabledUntil,omitempty"`
	Publication     string     `json:"publication"`
}

func invalidBlocking(message string) error {
	return &api.Error{Status: http.StatusBadRequest, Body: api.ErrorBody{Key: api.KeyInvalid, Message: message}}
}

// blockingStatus resolves the deadline exactly once. metav1.Time persists whole
// seconds; relative deadlines round up, while absolute fractional input refuses.
func blockingStatus(change api.BlockingChange, now time.Time) (public.BlockingStatus, error) {
	if now.IsZero() || !now.Before(blockingstage.Forever) {
		return public.BlockingStatus{}, serving.ErrDependency
	}
	if math.IsNaN(change.TimerSeconds) || math.IsInf(change.TimerSeconds, 0) || change.TimerSeconds < 0 || !change.HasTimer && change.TimerSeconds != 0 || !change.HasUntil && !change.Until.IsZero() {
		return public.BlockingStatus{}, invalidBlocking("invalid blocking timer or presence flags")
	}
	if change.Blocking {
		return public.BlockingStatus{}, nil
	}
	out := public.BlockingStatus{Disabled: true}
	var until time.Time
	switch {
	case change.HasUntil:
		until = change.Until.UTC()
		if until.IsZero() || until.Nanosecond() != 0 || !until.After(now) || !until.Before(blockingstage.Forever) {
			return public.BlockingStatus{}, invalidBlocking("until must be a future whole UTC second before the maximum deadline")
		}
	case change.HasTimer && change.TimerSeconds > 0:
		// Compare before conversion: float64(MaxInt64) rounds beyond int64 range.
		nanos := change.TimerSeconds * float64(time.Second)
		if nanos < 1 || nanos >= float64(math.MaxInt64) {
			return public.BlockingStatus{}, invalidBlocking("timer is outside the supported duration")
		}
		until = now.Add(time.Duration(math.Ceil(nanos))).UTC()
		if until.Nanosecond() != 0 {
			until = until.Truncate(time.Second).Add(time.Second)
		}
		if !until.After(now) || !until.Before(blockingstage.Forever) {
			return public.BlockingStatus{}, invalidBlocking("timer exceeds the maximum deadline")
		}
	default:
		return out, nil
	}
	deadline := metav1.NewTime(until)
	out.DisabledUntil = &deadline
	return out, nil
}

func sameBlocking(a, b public.BlockingStatus) bool {
	if a.Disabled != b.Disabled || (a.DisabledUntil == nil) != (b.DisabledUntil == nil) {
		return false
	}
	return a.DisabledUntil == nil || a.DisabledUntil.Equal(b.DisabledUntil)
}

// BlockingCommand changes only the canonical singleton's blocking status under
// the original raw proof, writer and complete dependency pins. Publication waits
// happen after commit, outside every pin, with the same original writer binding.
func (s *servingRuntime) BlockingCommand(ctx context.Context, change api.BlockingChange) (*api.BlockingState, error) {
	if s == nil || ctx == nil {
		return nil, currentCommandAPIError(serving.ErrComponents)
	}
	proof, err := servinghttp.Credential(ctx)
	if err != nil {
		return nil, currentCommandAPIError(err)
	}
	original, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return nil, currentCommandAPIError(err)
	}
	runtime, err := servinghttp.Component[*servingbuild.RuntimeOwner](ctx, "runtime")
	if err != nil {
		return nil, currentCommandAPIError(err)
	}
	_, _, current := runtime.NotifyKeys()
	if current == nil || !current() {
		return nil, currentCommandAPIError(serving.ErrDependency)
	}
	dependency := original.DependencyIdentity()
	var receipt *public.Kudurru
	var result *api.BlockingState
	var observed time.Time
	committed, err := s.currentAuthCommandThen(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		if _, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, proof, blockingToggleAttrs, s.opts.Now); err != nil {
			return err
		}
		settings, err := blockingCommandSettings(ctx, tx)
		if err != nil {
			return err
		}
		// Validate the same-tx effective configuration, never a competing live Reload.
		document, err := servingSettingsDocument(settings)
		if err != nil {
			return err
		}
		if _, _, err := config.ApplyClusterDocument(s.opts.Config, document); err != nil {
			return err
		}
		observed = s.opts.Now()
		status, err := blockingStatus(change, observed)
		if err != nil {
			return err
		}
		if !sameBlocking(settings.Status.Blocking, status) {
			settings.Status.Blocking = status
			if err := tx.UpdateStatus(ctx, settings); err != nil {
				return err
			}
		}
		receipt, err = blockingCommandSettings(ctx, tx)
		if err != nil {
			return err
		}
		return nil
	}, func(ctx context.Context, target serving.TermActivation) error {
		s.publisher.Wake()
		var err error
		result, err = s.awaitBlockingPublication(ctx, target, receipt, dependency, current, observed)
		return err
	})
	if err != nil {
		if committed {
			hint := blockingReceipt{Committed: true, Publication: "pending"}
			if receipt != nil {
				hint.SettingsUID, hint.ResourceVersion = string(receipt.UID), receipt.ResourceVersion
				hint.Disabled = receipt.Status.Blocking.Disabled
				if receipt.Status.Blocking.DisabledUntil != nil {
					u := receipt.Status.Blocking.DisabledUntil.UTC()
					hint.DisabledUntil = &u
				}
			}
			return nil, &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable, Message: "blocking state committed; serving publication not acknowledged", Hint: hint}}
		}
		return nil, currentCommandAPIError(err)
	}
	if !committed || result == nil {
		return nil, currentCommandAPIError(serving.ErrPending)
	}
	return result, nil
}

// The canonical singleton is infrastructure, not a user-addressed optional
// object. Its absence makes this command unavailable and never bootstraps it.
func blockingCommandSettings(ctx context.Context, tx *sqlite.CommandTx) (*public.Kudurru, error) {
	var settings public.Kudurru
	if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, serving.ErrPending
		}
		return nil, err
	}
	return &settings, nil
}

func (s *servingRuntime) awaitBlockingPublication(ctx context.Context, target serving.TermActivation, receipt *public.Kudurru, dependency string, current func() bool, observed time.Time) (*api.BlockingState, error) {
	if receipt == nil || dependency == "" || current == nil {
		return nil, serving.ErrComponents
	}
	ctx, cancel := context.WithTimeout(ctx, blockingPublicationWait)
	defer cancel()
	for {
		changed, authority := s.publisher.Subscribe(), s.publisher.AuthorityChanges()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !current() {
			return nil, serving.ErrDependency
		}
		if err := s.writerCurrent(ctx, target); err != nil {
			return nil, err
		}
		lease, err := s.publisher.CaptureHTTP(ctx)
		if err == nil {
			result, ready, captureErr := s.blockingPublication(ctx, lease, target, receipt, dependency, current, observed)
			lease.Release()
			if captureErr != nil {
				return nil, captureErr
			}
			if ready {
				return result, nil
			}
		} else if !errors.Is(err, serving.ErrPending) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		case <-authority:
		}
	}
}

func (s *servingRuntime) blockingPublication(ctx context.Context, lease *serving.Lease, target serving.TermActivation, receipt *public.Kudurru, dependency string, current func() bool, observed time.Time) (*api.BlockingState, bool, error) {
	bundle := lease.Bundle()
	if bundle == nil {
		return nil, false, serving.ErrReleased
	}
	raw, ok := bundle.Component("auth")
	auth, valid := raw.(*servingauth.View)
	if !ok || !valid || auth == nil || !auth.Base().Same(bundle.Identity().Base) {
		return nil, false, serving.ErrComponents
	}
	if auth.DependencyIdentity() != dependency || !current() {
		return nil, false, serving.ErrDependency
	}
	settings, err := auth.Settings()
	if err != nil {
		return nil, false, err
	}
	if settings.UID != receipt.UID || settings.ResourceVersion != receipt.ResourceVersion || !sameBlocking(settings.Status.Blocking, receipt.Status.Blocking) {
		return nil, false, nil // A precommit publication can still be installed.
	}
	raw, ok = bundle.Component("blocking")
	view, valid := raw.(*blockingstage.View)
	if !ok || !valid || view == nil {
		return nil, false, serving.ErrComponents
	}
	now := s.opts.Now()
	if now.IsZero() || now.Before(observed) {
		return nil, false, servingauth.ErrCredentials
	}
	configured, enabled, until, err := view.StateAt(now)
	if err != nil {
		return nil, false, err
	}
	if err := s.publisher.CheckWriterLease(ctx, lease, target); err != nil {
		if errors.Is(err, serving.ErrPending) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if !current() {
		return nil, false, serving.ErrDependency
	}
	result := &api.BlockingState{Configured: configured, Blocking: enabled, Scope: "cluster"}
	if configured && !enabled && until.After(now) && until.Before(blockingstage.Forever) {
		u, seconds := until.UTC(), until.Sub(now).Seconds()
		result.Until, result.Timer = &u, &seconds
	}
	return result, true, nil
}
