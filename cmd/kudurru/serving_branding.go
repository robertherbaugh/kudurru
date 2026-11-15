package main

import (
	"context"
	"net/http"
	"net/netip"
	"time"

	"github.com/robertherbaugh/kudurru/ee/branding"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/blockpage"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
)

// Branding reads public presentation from the SAME auth/settings owner as the
// request. Missing owners fail; license denial returns defaults with no assets.
func (s *servingRuntime) Branding(ctx context.Context) (api.BrandingInfo, error) {
	view, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return api.BrandingInfo{}, currentCommandAPIError(err)
	}
	cfg, err := s.settingsFromView(view)
	if err != nil {
		return api.BrandingInfo{}, err
	}
	now := s.opts.Now()
	allowed, until, err := view.BrandingValidity(now)
	if err != nil {
		return api.BrandingInfo{}, currentCommandAPIError(err)
	}
	return api.BrandingInfo(branding.Project(cfg.Branding, allowed, until, now)), nil
}

func (s *servingRuntime) settingsFromView(view *servingauth.View) (*config.Config, error) {
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

func brandingWriteCheck(view *servingauth.View, now time.Time) error {
	allowed, err := view.BrandingAllowed(now)
	if err != nil {
		return err
	}
	if !allowed {
		return &api.Error{Status: http.StatusPaymentRequired, Body: api.ErrorBody{Key: api.KeyLicense, Message: "custom branding requires an Enterprise license"}}
	}
	return nil
}

// captureBlockPageBranding is for the independent public block-page listener.
// It captures once, retaining that owner's settings and entitlement together.
// Failure uses the renderer's built-in brand; DNS remains unaffected.
func (s *servingRuntime) captureBlockPageBranding(ctx context.Context, cache *branding.PageCache) (*blockpage.Branding, error) {
	if s == nil {
		return nil, serving.ErrPending
	}
	lease, err := s.publisher.CaptureHTTP(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	value, ok := lease.Bundle().Component("auth")
	view, valid := value.(*servingauth.View)
	if !ok || !valid || view == nil {
		return nil, serving.ErrComponents
	}
	return s.blockPageBranding(ctx, view, cache)
}

func (s *servingRuntime) blockPageBranding(ctx context.Context, view *servingauth.View, cache *branding.PageCache) (*blockpage.Branding, error) {
	allowed, err := view.BrandingAllowed(s.opts.Now())
	if err != nil || !allowed {
		return nil, err
	}
	cfg, err := s.settingsFromView(view)
	if err != nil {
		return nil, err
	}
	prepared, err := cache.Prepare(ctx, cfg.Branding)
	if err != nil {
		return nil, err
	}
	// Preparation can cross expiry. Recheck only the original immutable bound.
	if allowed, err := view.BrandingAllowed(s.opts.Now()); err != nil || !allowed {
		return nil, err
	}
	return prepared, nil
}

type servingBrandingPreview struct {
	runtime *servingRuntime
	server  *blockpage.Server
	cache   *branding.PageCache
}

func (p servingBrandingPreview) Preview(ctx context.Context, domain string, client netip.Addr) []byte {
	doc, _ := p.CapturedPreview(ctx, domain, client)
	return doc.Body
}

func (p servingBrandingPreview) CapturedPreview(ctx context.Context, domain string, client netip.Addr) (blockpage.Document, error) {
	view, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return blockpage.Document{}, currentCommandAPIError(err)
	}
	cfg, err := p.runtime.settingsFromView(view)
	if err != nil {
		return blockpage.Document{}, err
	}
	brand, err := p.runtime.blockPageBranding(ctx, view, p.cache)
	if err != nil {
		return blockpage.Document{}, currentCommandAPIError(err)
	}
	settings := blockpage.SettingsFrom(cfg)
	requests, err := view.UnblockRequestsAllowed(p.runtime.opts.Now())
	if err != nil {
		return blockpage.Document{}, currentCommandAPIError(err)
	}
	settings.Requests = settings.Requests && requests
	return p.server.PreviewDocumentWithSettings(ctx, domain, client, settings, brand), nil
}
