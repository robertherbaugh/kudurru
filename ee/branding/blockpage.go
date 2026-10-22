package branding

import (
	"context"
	"sync"

	"github.com/robertherbaugh/kudurru/internal/blockpage"
	"github.com/robertherbaugh/kudurru/internal/config"
)

// PageCache holds at most one exact, validated presentation. It caches no
// entitlement: callers must project the original license before every use.
// Eviction affects preparation cost only; older requests retain their value.
type PageCache struct {
	mu    sync.Mutex
	key   config.Branding
	value *blockpage.Branding
}

// Prepare returns immutable, detached block-page artwork outside serving pins.
func (c *PageCache) Prepare(ctx context.Context, b config.Branding) (*blockpage.Branding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.value != nil && c.key == b {
		return c.value, nil
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	light, err := config.BrandingPNG(b.LogoLight, 1024)
	if err != nil {
		return nil, err
	}
	dark, err := config.BrandingPNG(b.LogoDark, 1024)
	if err != nil {
		return nil, err
	}
	icon, err := config.BrandingPNG(b.Favicon, 256)
	if err != nil {
		return nil, err
	}
	prepared, err := blockpage.PrepareBranding(blockpage.BrandingInput{Name: b.Name, LogoLight: light, LogoDark: dark, Favicon: icon, AccentColor: b.AccentColor})
	if err != nil {
		return nil, err
	}
	c.key, c.value = b, prepared
	return prepared, ctx.Err()
}
