package main

import (
	"context"
	"errors"

	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

// runServingReload observes only complete publications. It owns no replacement
// DNS resources and never updates the legacy file/reload pointer. Subscribe
// before capture so a publication during application is observed on the next
// pass. Preparation is detached before any operational callback runs.
func (s *servingRuntime) runServingReload(ctx context.Context, targets reloadTargets, responseCache *cache.Cache) error {
	previous := s.opts.Config
	for {
		changed := s.publisher.Subscribe()
		next, logging, err := s.captureOperationalOwners(ctx)
		if err == nil {
			operational := targets
			if targets.qlOwner != nil {
				targets.qlOwner.adopt(logging, next.Querylog)
				operational.qlOwner, operational.qlAsync, operational.qlWriter = nil, nil, nil
			}
			paths := config.Changed(previous, next)
			if len(paths) > 0 {
				applyReload(ctx, operational, next, paths)
				if responseCache != nil && config.ChangedUnder(paths, "cache.maxEntries") {
					responseCache.Resize(next.Cache.MaxEntries)
				}
				if targets.edgeFlush != nil && config.ChangedUnder(paths, "control", "blocking", "response", "upstreams", "dnssec", "blockPage.ipv4", "blockPage.ipv6") {
					targets.edgeFlush.BlockResponsesChanged()
				}
				previous = next
			}
		} else if ctx.Err() == nil && !errors.Is(err, serving.ErrPending) && !transientServing(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		}
	}
}

func (s *servingRuntime) captureOperationalSettings(ctx context.Context) (*config.Config, error) {
	next, _, err := s.captureOperationalOwners(ctx)
	return next, err
}

func (s *servingRuntime) captureOperationalOwners(ctx context.Context) (*config.Config, *querylog.Captured, error) {
	lease, err := s.publisher.CaptureHTTP(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer lease.Release()
	owner, ok := lease.Bundle().Component("auth")
	view, typed := owner.(*servingauth.View)
	if !ok || !typed || view == nil {
		return nil, nil, serving.ErrComponents
	}
	settings, err := view.Settings()
	if err != nil {
		return nil, nil, err
	}
	document, err := servingSettingsDocument(settings)
	if err != nil {
		return nil, nil, err
	}
	next, _, err := config.ApplyClusterDocument(s.opts.Config, document)
	if err != nil {
		return nil, nil, err
	}
	value, ok := lease.Bundle().Component("logging")
	logging, typed := value.(*querylog.Captured)
	if !ok || !typed || logging == nil {
		return nil, nil, serving.ErrComponents
	}
	return next, logging, nil
}
