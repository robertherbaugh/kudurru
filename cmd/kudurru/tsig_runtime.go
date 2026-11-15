package main

import (
	"context"
	"time"

	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

// tsigRuntime prepares complete encrypted input cuts outside DNS handling. It
// borrows its owners; only the root closes them after joining all workers.
// Baseline is the one immutable file configuration admitted at startup.
type tsigRuntime struct {
	store      *sqlite.Store
	encryption *authKeyOwner
	rings      *servingbuild.KeyringOwner
	baseline   *dnsserver.StaticKeyring
}

func newTSIGRuntime(st *sqlite.Store, encryption *authKeyOwner, rings *servingbuild.KeyringOwner, baseline *dnsserver.StaticKeyring) (*tsigRuntime, error) {
	if st == nil || encryption == nil || rings == nil || baseline == nil {
		return nil, serving.ErrComponents
	}
	return &tsigRuntime{store: st, encryption: encryption, rings: rings, baseline: baseline}, nil
}

// prepare never changes storage and never resolves secret references. A failed
// candidate leaves the coherent previous ring installed; Matches on complete
// serving capture and SAME-CommandTx checks independently exclude stale input.
func (r *tsigRuntime) prepare(ctx context.Context) error {
	cut := r.encryption.owner.Capture()
	if !cut.Current() || cut.Value() == nil {
		return serving.ErrDependency
	}
	image, err := r.store.ReadGeneration(ctx)
	if err != nil {
		return err
	}
	source, err := tsigkeys.FromImage(ctx, image)
	if err != nil {
		return err
	}
	material := cut.Value()
	sealer, err := tsigkeys.NewSealer(material.current, material.previous)
	if err != nil {
		return err
	}
	if old := r.rings.Capture().Prepared(); old != nil && old.SourceIdentity() == source.Identity() && old.EncryptionIdentity() == sealer.Identity() {
		if !cut.Current() {
			return serving.ErrDependency
		}
		return ctx.Err()
	}
	prepared, err := source.Prepare(sealer, r.baseline)
	if err != nil {
		return err
	}
	// This order is the same as complete publication/current-command guards.
	return r.encryption.owner.WithCurrent(ctx, cut, func() error {
		return r.rings.ReplacePrepared(ctx, prepared)
	})
}

func (r *tsigRuntime) Run(ctx context.Context) error {
	if r == nil || ctx == nil {
		return serving.ErrComponents
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		// Subscribe before reading so no source/key transition can be lost.
		sourceChanged, keyChanged := r.store.GenerationChanges(), r.encryption.owner.Changes()
		var retry <-chan time.Time
		if err := r.prepare(ctx); err != nil {
			// Read-only recovery is needed only after failure; stable input waits
			// for a real source/key notification instead of polling the image.
			retry = tick.C
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sourceChanged:
		case <-keyChanged:
		case <-retry:
		}
	}
}
