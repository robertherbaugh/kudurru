// Package snapshotowner pins immutable local installations through a synchronous
// publication callback. It has no knowledge of stores, serving or acquisition.
package snapshotowner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
)

// Installation admission errors.
var (
	ErrInvalid   = errors.New("snapshotowner: invalid argument")
	ErrObsolete  = errors.New("snapshotowner: obsolete installation")
	ErrExhausted = errors.New("snapshotowner: installation counter exhausted")
)

type installation[T any] struct {
	value    *T
	identity string
}

// Owner must not be copied after first use. Its zero value supports publication.
// Values and equality inputs must be immutable; prepare them before Publish.
type Owner[T any] struct {
	state    atomic.Pointer[installation[T]]
	once     sync.Once
	gate     chan struct{}
	mu       sync.Mutex
	changed  chan struct{}
	instance string
	sequence uint64 // protected by gate
}

func (o *Owner[T]) initialize() {
	o.once.Do(func() {
		var id [16]byte
		_, _ = rand.Read(id[:])
		o.instance = hex.EncodeToString(id[:])
		o.gate = make(chan struct{}, 1)
		o.gate <- struct{}{}
		o.changed = make(chan struct{})
	})
}

func (o *Owner[T]) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o.initialize()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-o.gate:
		if err := ctx.Err(); err != nil {
			o.unlock()
			return err
		}
		return nil
	}
}

func (o *Owner[T]) unlock() { o.gate <- struct{}{} }

// Publish installs a prepared value under the same gate as WithCurrent. Equal
// immutable contents retain the actual previous pointer and emit no change.
// Equality is memory-only and thread-safe; it must not call upward or perform
// I/O. An unchanged publication needs no gate, so idle scheduler polling can
// continue acquisition while root pins the installation through a transaction.
func (o *Owner[T]) Publish(ctx context.Context, value *T, equal func(*T, *T) bool) error {
	if o == nil || value == nil || equal == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// No mutation: linearize at the second load only if this exact installation
	// still exists. Otherwise join the gate and compare against its successor.
	if old := o.state.Load(); old != nil && equal(old.value, value) && o.state.Load() == old {
		return ctx.Err()
	}
	if err := o.lock(ctx); err != nil {
		return err
	}
	defer o.unlock()
	old := o.state.Load()
	if old != nil && equal(old.value, value) {
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if o.sequence == math.MaxUint64 {
		return ErrExhausted
	}
	o.sequence++
	o.state.Store(&installation[T]{value: value, identity: fmt.Sprintf("snapshot-local/%s/%d", o.instance, o.sequence)})
	o.mu.Lock()
	close(o.changed)
	o.changed = make(chan struct{})
	o.mu.Unlock()
	return nil
}

// Cut holds one exact immutable value and its local installation identity.
// Go references retain it after replacement; it has no external release hook.
type Cut[T any] struct {
	owner *Owner[T]
	state *installation[T]
}

// Capture reads one atomic installation without locking or performing I/O.
func (o *Owner[T]) Capture() Cut[T] {
	if o == nil {
		return Cut[T]{}
	}
	return Cut[T]{owner: o, state: o.state.Load()}
}

// Value returns the captured pointer, never a newer installation.
func (c Cut[T]) Value() *T {
	if c.state == nil {
		return nil
	}
	return c.state.value
}

// Identity names a local installation, not a durable data or credential version.
func (c Cut[T]) Identity() string {
	if c.state == nil {
		return ""
	}
	return c.state.identity
}

// Current checks exact installation equality without a lock, clock or I/O.
// It does not assert data freshness. A -> B -> A does not revive the older cut.
func (c Cut[T]) Current() bool {
	return c.owner != nil && c.state != nil && c.owner.state.Load() == c.state
}

// Changes broadcasts replacements by closing a channel and installing another.
// Subscribe before Capture and resubscribe after each close. Nil owners have no
// subscription; their cuts confer no admission capability.
func (o *Owner[T]) Changes() <-chan struct{} {
	if o == nil {
		return nil
	}
	o.initialize()
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.changed
}

// WithCurrent excludes replacement for the full synchronous callback. The cut
// must belong to this owner and still be installed. Cancellation while waiting
// never enters the callback. Do not reenter this gate or acquire earlier gates.
func (o *Owner[T]) WithCurrent(ctx context.Context, expected Cut[T], fn func() error) error {
	if fn == nil {
		return ErrInvalid
	}
	if o == nil || expected.owner != o || expected.state == nil {
		return ErrObsolete
	}
	if err := o.lock(ctx); err != nil {
		return err
	}
	defer o.unlock()
	if !expected.Current() {
		return ErrObsolete
	}
	return fn()
}
