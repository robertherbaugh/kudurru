package main

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/snapshotowner"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

// tsigAcquisition retains the original complete source/encryption/provider cut.
// It grants local cache acceptance only, never current command authority.
type tsigAcquisition struct {
	runtime    *tsigRuntime
	encryption snapshotowner.Cut[authKeyMaterial]
	ring       servingbuild.KeyringCut
	prepared   *tsigkeys.Prepared
}

func (r *tsigRuntime) captureAcquisition() dnsserver.AcquisitionKeys {
	if r == nil {
		return nil
	}
	c := &tsigAcquisition{runtime: r, encryption: r.encryption.owner.Capture(), ring: r.rings.Capture()}
	c.prepared = c.ring.Prepared()
	if !c.Current() || c.prepared == nil || c.encryption.Value() == nil {
		return nil
	}
	material := c.encryption.Value()
	sealer, err := tsigkeys.NewSealer(material.current, material.previous)
	if err != nil || sealer.Identity() != c.prepared.EncryptionIdentity() {
		return nil
	}
	return c
}

func (c *tsigAcquisition) Keys() *dnsserver.StaticKeyring { return c.ring.Keys() }
func (c *tsigAcquisition) Identity() string               { return c.ring.Identity() + "/" + c.encryption.Identity() }
func (c *tsigAcquisition) Current() bool {
	return c != nil && c.runtime != nil && c.encryption.Current() && c.ring.Current()
}

func (c *tsigAcquisition) checkSource(ctx context.Context) error {
	if !c.Current() || c.prepared == nil {
		return serving.ErrDependency
	}
	image, err := c.runtime.store.ReadGeneration(ctx)
	if err != nil {
		return err
	}
	if err := c.prepared.Matches(ctx, image); err != nil {
		return err
	}
	if !c.Current() {
		return serving.ErrDependency
	}
	return ctx.Err()
}

// WithCurrent pins encryption -> keyring before local persistence/manager gates.
// The read-only source checks bracket local.db cache writes; they are NOT one
// atomic transaction across the two DBs. A raced artifact keeps its acquired
// epoch. Complete serving publication separately matches Prepared to its actual
// durable image and binds that copy to the exact credential owner/epoch. Same-
// CommandTx validation remains mandatory for authoritative acknowledgments.
func (c *tsigAcquisition) WithCurrent(ctx context.Context, cache func() error, publish func() error) error {
	if c == nil || ctx == nil || cache == nil || !c.Current() {
		return serving.ErrDependency
	}
	return c.runtime.encryption.owner.WithCurrent(ctx, c.encryption, func() error {
		return c.runtime.rings.WithCurrent(ctx, func(current servingbuild.KeyringCut) error {
			if current.Identity() != c.ring.Identity() {
				return serving.ErrDependency
			}
			if err := c.checkSource(ctx); err != nil {
				return err
			}
			if err := cache(); err != nil {
				return err
			}
			if err := c.checkSource(ctx); err != nil {
				return err
			}
			if publish == nil {
				return nil
			}
			// publish is memory-only and checks the SAME original context at
			// its swap. No fallible source re-read follows visible acceptance.
			return publish()
		})
	})
}
