// Package branding projects Enterprise presentation settings. Core renderers
// remain available with built-in artwork when the entitlement is unavailable.
package branding

import (
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// Info is a public presentation. It never includes license identity or private
// settings. Enabled describes entitlement, including an empty customization.
type Info struct {
	Enabled     bool       `json:"enabled"`
	Name        string     `json:"name"`
	LogoLight   string     `json:"logoLight"`
	LogoDark    string     `json:"logoDark"`
	Favicon     string     `json:"favicon"`
	AccentColor string     `json:"accentColor"`
	ValidUntil  *time.Time `json:"validUntil,omitempty"`
}

// Default returns the built-in presentation without exposing saved overrides.
func Default() Info { return Info{Name: "kudurru"} }

// Project uses only explicitly captured entitlement and settings. An absent or
// expired original deadline denies custom presentation, without affecting DNS.
func Project(b config.Branding, allowed bool, until, now time.Time) Info {
	if !allowed || until.IsZero() || !now.Before(until) {
		return Default()
	}
	name := b.Name
	if name == "" {
		name = "kudurru"
	}
	return Info{Enabled: true, Name: name, LogoLight: b.LogoLight, LogoDark: b.LogoDark, Favicon: b.Favicon, AccentColor: b.AccentColor, ValidUntil: &until}
}
