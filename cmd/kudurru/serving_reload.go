package main

import (
	"context"
	"errors"

	"github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/config"
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
		next, err := s.captureOperationalSettings(ctx)
		if err == nil {
			paths := config.Changed(previous, next)
			if len(paths) > 0 {
				applyReload(ctx, targets, next, paths)
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
	lease, err := s.publisher.CaptureHTTP(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	owner, ok := lease.Bundle().Component("auth")
	view, typed := owner.(*servingauth.View)
	if !ok || !typed || view == nil {
		return nil, serving.ErrComponents
	}
	settings, err := view.Settings()
	if err != nil {
		return nil, err
	}
	document, err := servingSettingsDocument(settings)
	if err != nil {
		return nil, err
	}
	next, _, err := config.ApplyClusterDocument(s.opts.Config, document)
	return next, err
}
