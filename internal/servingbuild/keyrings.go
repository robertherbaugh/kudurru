package servingbuild

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// KeyringOwner serializes replacement of the immutable TSIG provider with
// publication and complete command execution. It performs no secret loading;
// callers construct a validated StaticKeyring before entering Replace.
type KeyringOwner struct {
	gate     chan struct{}
	current  atomic.Pointer[keyringGeneration]
	closed   atomic.Bool
	mu       sync.Mutex
	changed  chan struct{}
	next     uint64 // protected by gate
	instance string
}

type keyringGeneration struct {
	keys     *dnsserver.StaticKeyring
	identity string
}

// KeyringCut retains immutable Go-owned keys; it needs no external close hook.
// Current is a local pointer check, so A -> B -> A never revives an older cut.
// TSIG credential identities remain the immutable keyring's content identities.
type KeyringCut struct {
	owner *KeyringOwner
	value *keyringGeneration
}

// NewKeyringOwner requires an explicit ring, including for unsigned operation.
func NewKeyringOwner(keys *dnsserver.StaticKeyring) (*KeyringOwner, error) {
	if keys == nil {
		return nil, serving.ErrComponents
	}
	var id [16]byte
	_, _ = rand.Read(id[:])
	o := &KeyringOwner{gate: make(chan struct{}, 1), changed: make(chan struct{}), instance: hex.EncodeToString(id[:])}
	o.gate <- struct{}{}
	if err := o.Replace(context.Background(), keys); err != nil {
		return nil, err
	}
	return o, nil
}

func (o *KeyringOwner) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.gate:
		if err := ctx.Err(); err != nil {
			o.gate <- struct{}{}
			return err
		}
		return nil
	}
}

// Replace installs an already prepared ring and invalidates every earlier cut.
// It waits for complete commands/publications using the same owner to finish.
func (o *KeyringOwner) Replace(ctx context.Context, keys *dnsserver.StaticKeyring) error {
	if keys == nil {
		return serving.ErrComponents
	}
	if err := o.lock(ctx); err != nil {
		return err
	}
	defer func() { o.gate <- struct{}{} }()
	if o.closed.Load() {
		return serving.ErrClosed
	}
	if o.next == math.MaxUint64 {
		return serving.ErrDependency
	}
	o.next++
	o.current.Store(&keyringGeneration{keys: keys, identity: fmt.Sprintf("tsig-local/%s/%d", o.instance, o.next)})
	o.signal()
	return nil
}

// Capture is memory-only and safe during a command or rotation.
func (o *KeyringOwner) Capture() KeyringCut {
	if o == nil || o.closed.Load() {
		return KeyringCut{}
	}
	return KeyringCut{owner: o, value: o.current.Load()}
}

// Keys returns the exact immutable provider, or nil for an empty cut.
func (c KeyringCut) Keys() *dnsserver.StaticKeyring {
	if c.value == nil {
		return nil
	}
	return c.value.keys
}

// Identity names this process-local installation, not a durable credential ID.
func (c KeyringCut) Identity() string {
	if c.value == nil {
		return ""
	}
	return c.value.identity
}

// Current never performs Store/network I/O or takes the replacement gate.
func (c KeyringCut) Current() bool {
	return c.owner != nil && c.value != nil && !c.owner.closed.Load() && c.owner.current.Load() == c.value
}

// Changes subscribes before Capture; replacement is a hint to rebuild all owners.
func (o *KeyringOwner) Changes() <-chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.changed
}

func (o *KeyringOwner) signal() {
	o.mu.Lock()
	defer o.mu.Unlock()
	close(o.changed)
	o.changed = make(chan struct{})
}

// WithCurrent pins the current installation through the entire synchronous
// callback. Call it after publication/provider/host pins and before catalog,
// transferred manager and SQLite gates. It never reenters the publisher gate.
func (o *KeyringOwner) WithCurrent(ctx context.Context, fn func(KeyringCut) error) error {
	if o == nil || ctx == nil || fn == nil {
		return serving.ErrComponents
	}
	if err := o.lock(ctx); err != nil {
		return err
	}
	defer func() { o.gate <- struct{}{} }()
	cut := o.Capture()
	if !cut.Current() {
		return serving.ErrClosed
	}
	return fn(cut)
}

// PrimaryPin composes publication -> key replacement -> SQLite exclusion.
// It supports pending background recovery without opening protected reads.
// NOTIFY stays disabled until root supplies a separately bound sender capability.
func (o *KeyringOwner) PrimaryPin(publisher *serving.Publisher) currentguard.PinFunc {
	return func(ctx context.Context, fence pr.PublicationFence, fn func(currentguard.Pinned) error) error {
		if o == nil || publisher == nil || fn == nil {
			return serving.ErrComponents
		}
		return publisher.WithWriterGate(ctx, serving.TermActivation{Term: fence.Term, Incarnation: fence.Incarnation}, func() error {
			return o.WithCurrent(ctx, func(cut KeyringCut) error {
				return fn(currentguard.Pinned{Identity: cut.Identity(), Keys: cut.Keys(), Current: cut.Current})
			})
		})
	}
}

// GuardSource adds key replacement exclusion outside the actual SQLite candidate
// barrier. Builder capture must use this SAME owner's cut and Current predicate.
// Additional independently mutable resources need their own outer guards too.
func (o *KeyringOwner) GuardSource(source serving.Source) (serving.Source, error) {
	if o == nil || source == nil {
		return nil, serving.ErrComponents
	}
	return keyringSource{Source: source, owner: o}, nil
}

type keyringSource struct {
	serving.Source
	owner *KeyringOwner
}

func (s keyringSource) PublishIfCurrent(ctx context.Context, token sqlite.GenerationToken, options sqlite.PublicationOptions, publish func() error) error {
	return s.owner.WithCurrent(ctx, func(KeyringCut) error {
		return s.Source.PublishIfCurrent(ctx, token, options, publish)
	})
}

// Close rejects new work after existing pins finish. Held cuts retain their
// immutable bytes for already captured DNS requests, but cease to be Current.
func (o *KeyringOwner) Close(ctx context.Context) error {
	if err := o.lock(ctx); err != nil {
		return err
	}
	defer func() { o.gate <- struct{}{} }()
	if o.closed.CompareAndSwap(false, true) {
		o.current.Store(nil)
		o.signal()
	}
	return nil
}
