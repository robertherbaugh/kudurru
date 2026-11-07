package servingbuild

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/rpz"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/stub"
)

// TransferredRuntime binds fixed manager instances to complete capture and
// publication. It does not own their workers or perform network acquisition.
// Construct a new outer serving assembly to replace a manager instance.
type TransferredRuntime struct {
	secondary *secondary.Manager
	stub      *stub.Manager
	rpz       *rpz.Manager
}

// NewTransferredRuntime treats nil managers as explicit, fixed empty parts.
// Non-nil managers must have been initialized by their constructors.
func NewTransferredRuntime(s *secondary.Manager, t *stub.Manager, r *rpz.Manager) *TransferredRuntime {
	return &TransferredRuntime{secondary: s, stub: t, rpz: r}
}

type transferredCut struct {
	owner     *TransferredRuntime
	secondary secondary.Cut
	stub      stub.Cut
	rpz       rpz.Cut
}

func (o *TransferredRuntime) capture() transferredCut {
	return transferredCut{owner: o, secondary: o.secondary.Capture(), stub: o.stub.Capture(), rpz: o.rpz.Capture()}
}

func (c transferredCut) current() bool {
	return c.owner != nil && (c.owner.secondary == nil || c.secondary.Current()) &&
		(c.owner.stub == nil || c.stub.Current()) && (c.owner.rpz == nil || c.rpz.Current())
}

// CaptureDependencies composes the exact acquired snapshots and installation
// cursors with an existing complete dependency capture. The returned reference,
// including on failure, follows the builder's ordinary release ownership.
func (o *TransferredRuntime) CaptureDependencies(base func(context.Context, *replication.Image) (Dependencies, error)) (func(context.Context, *replication.Image) (Dependencies, error), error) {
	if o == nil || base == nil {
		return nil, serving.ErrComponents
	}
	return func(ctx context.Context, image *replication.Image) (Dependencies, error) {
		d, err := base(ctx, image)
		if err != nil {
			return d, err
		}
		c := o.capture()
		if d.Identity == "" || len(d.Identity) > 512 || d.Current == nil || !c.current() {
			return d, serving.ErrDependency
		}
		data, _ := json.Marshal([]string{"transferred-runtime/v1", d.Identity, c.secondary.Identity(), c.stub.Identity(), c.rpz.Identity()})
		digest := sha256.Sum256(data)
		d.Identity = hex.EncodeToString(digest[:])
		d.Secondary, d.Stub, d.RPZ = c.secondary.Snapshot(), c.stub.Snapshot(), c.rpz.Snapshot()
		current := d.Current
		d.Current = func() bool { return current() && c.current() }
		return d, nil
	}, nil
}

// WithCurrent excludes installation in secondary -> stub -> RPZ order through
// the entire callback. Call it after publication/provider/host/keyring pins and
// before the actual SQLite command or publication barrier. A newer pin does not
// repair an older prepared cut; that cut must still check its original Current.
func (o *TransferredRuntime) WithCurrent(ctx context.Context, fn func() error) error {
	if o == nil || ctx == nil || fn == nil {
		return serving.ErrComponents
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c := o.capture()
	if !c.current() {
		return serving.ErrDependency
	}
	run := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn()
	}
	if o.rpz != nil {
		next := run
		run = func() error { return o.rpz.WithCurrent(ctx, c.rpz, next) }
	}
	if o.stub != nil {
		next := run
		run = func() error { return o.stub.WithCurrent(ctx, c.stub, next) }
	}
	if o.secondary != nil {
		next := run
		run = func() error { return o.secondary.WithCurrent(ctx, c.secondary, next) }
	}
	return run()
}

// GuardSource belongs inside the keyring/provider/host source wrappers. It pins
// all three installations through the actual store's memory publication callback.
func (o *TransferredRuntime) GuardSource(source serving.Source) (serving.Source, error) {
	if o == nil || source == nil {
		return nil, serving.ErrComponents
	}
	return transferredSource{Source: source, owner: o}, nil
}

type transferredSource struct {
	serving.Source
	owner *TransferredRuntime
}

func (s transferredSource) PublishIfCurrent(ctx context.Context, token sqlite.GenerationToken, options sqlite.PublicationOptions, publish func() error) error {
	if publish == nil {
		return serving.ErrComponents
	}
	return s.owner.WithCurrent(ctx, func() error { return s.Source.PublishIfCurrent(ctx, token, options, publish) })
}

// PrimaryPin extends an existing publication/host/keyring pin with the same
// manager exclusion used for publication. NotifyKeys.Current relies on these
// installations, so a final memory check alone cannot authorize durable ack.
func (o *TransferredRuntime) PrimaryPin(base currentguard.PinFunc) currentguard.PinFunc {
	return func(ctx context.Context, fence pr.PublicationFence, fn func(currentguard.Pinned) error) error {
		if o == nil || base == nil || fn == nil {
			return serving.ErrComponents
		}
		return base(ctx, fence, func(p currentguard.Pinned) error {
			return o.WithCurrent(ctx, func() error { return fn(p) })
		})
	}
}

// Watch wakes the complete publisher on actual manager installations. It
// subscribes to every broadcast before waking/capturing, without polling or
// consuming another subscriber's signal. Run it alongside the manager workers.
func (o *TransferredRuntime) Watch(ctx context.Context, wake func()) error {
	if o == nil || ctx == nil || wake == nil {
		return serving.ErrComponents
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		secondary, stub, rpz := o.secondary.Changes(), o.stub.Changes(), o.rpz.Changes()
		wake()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-secondary:
		case <-stub:
		case <-rpz:
		}
	}
}
