package objects

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// ErrSourceChanged refuses a RoleBinding effect whose original accepted source
// or target plan is unavailable. It never permits ordinary CRUD as a fallback.
var ErrSourceChanged = errors.New("objects: original RoleBinding source plan unavailable")

// RoleBindingEffects owns the concrete security transaction for a loader's
// RoleBindings. It confers no HTTP, provider, or elected writer authority.
// Implementations must retain the plan with WithCurrent through the entire
// command, compare its exact target, and stage object and security changes in
// the same transaction. A refused plan must not be recomputed or retried.
type RoleBindingEffects interface {
	ApplyRoleBinding(context.Context, RoleBindingPlan) (*public.RoleBinding, error)
	DeleteRoleBinding(context.Context, RoleBindingPlan) error
}

// sourceCut is one immutable accepted reconciliation input. Loader.loadMu
// excludes other passes; gate also prevents a retained plan from crossing cut
// retirement. This fences accepted input replacement, not native disk writes.
type sourceCut struct {
	ctx       context.Context
	at        time.Time
	gate      chan struct{}
	active    atomic.Bool
	declared  map[store.Key]string
	documents map[store.Key][sha256.Size]byte
	broken    map[string]bool
	prefixes  []string
}

func (p *pass) acceptSource(ctx context.Context, items []item) *sourceCut {
	c := &sourceCut{
		ctx: ctx, at: p.rep.Started.UTC(), gate: make(chan struct{}, 1),
		declared:  make(map[store.Key]string),
		documents: make(map[store.Key][sha256.Size]byte),
		broken:    make(map[string]bool, len(p.brokenSources)),
		prefixes:  append([]string(nil), p.brokenPrefixes...),
	}
	for key, source := range p.declared {
		if key.Kind == public.KindRoleBinding {
			c.declared[key] = source
		}
	}
	for source, broken := range p.brokenSources {
		c.broken[source] = broken
	}
	for _, it := range items {
		if it.key.Kind == public.KindRoleBinding {
			c.documents[it.key] = hashDocument(p.r.manager(it.doc.source), it.key, it.doc.obj)
		}
	}
	c.active.Store(true)
	c.gate <- struct{}{}
	return c
}

func (c *sourceCut) retire() {
	<-c.gate
	c.active.Store(false)
	c.gate <- struct{}{}
}

func (c *sourceCut) sourceBroken(source string) bool {
	if c.broken[source] {
		return true
	}
	for _, prefix := range c.prefixes {
		if strings.HasPrefix(source, prefix) {
			return true
		}
	}
	return false
}

// RoleBindingPlan is detached from caller maps and store objects. Its private
// source receipt cannot be decoded from a manifest or minted by a manager name.
// Getters return copies; they do not grant continuing source/writer authority.
type RoleBindingPlan struct {
	cut             *sourceCut
	key             store.Key
	before, after   *public.RoleBinding
	manager         string
	force, deleting bool
	document        [sha256.Size]byte
}

func roleBindingPlan(cut *sourceCut, key store.Key, before, after store.Object, manager string, force, deleting bool) (RoleBindingPlan, error) {
	p := RoleBindingPlan{cut: cut, key: key, manager: manager, force: force, deleting: deleting}
	if before != nil {
		binding, ok := before.(*public.RoleBinding)
		if !ok || binding == nil {
			return RoleBindingPlan{}, ErrSourceChanged
		}
		p.before = binding.DeepCopy()
	}
	if after != nil {
		binding, ok := after.(*public.RoleBinding)
		if !ok || binding == nil {
			return RoleBindingPlan{}, ErrSourceChanged
		}
		p.after = binding.DeepCopy()
	}
	if cut != nil {
		p.document = cut.documents[key]
	}
	if !p.valid() {
		return RoleBindingPlan{}, ErrSourceChanged
	}
	return p, nil
}

func (p RoleBindingPlan) valid() bool {
	if p.cut == nil || p.key.Kind != public.KindRoleBinding || p.key.Name == "" || !isFileManager(p.manager) {
		return false
	}
	if p.before != nil && (store.KeyOf(p.before) != p.key || p.before.UID == "" || p.before.ResourceVersion == "") {
		return false
	}
	if p.deleting {
		if p.before == nil || p.after != nil || p.force || store.ManagedBy(p.before) != p.manager {
			return false
		}
		_, declared := p.cut.declared[p.key]
		return !declared && !p.cut.sourceBroken(strings.TrimPrefix(p.manager, filePrefix))
	}
	source, declared := p.cut.declared[p.key]
	if !declared || Manager(source) != p.manager || p.after == nil || store.KeyOf(p.after) != p.key || p.document == ([sha256.Size]byte{}) || p.document != p.cut.documents[p.key] {
		return false
	}
	if p.before == nil {
		return !p.force
	}
	owner := store.ManagedBy(p.before)
	switch {
	case owner == p.manager || owner == "":
		return !p.force
	case owner == store.ManagerBootstrap:
		return p.force
	case isFileManager(owner):
		return p.force && !p.cut.sourceBroken(strings.TrimPrefix(owner, filePrefix))
	default:
		return false
	}
}

// Key returns the exact originally planned target.
func (p RoleBindingPlan) Key() store.Key { return p.key }

// Before returns the original target, or nil for an exact create-only plan.
func (p RoleBindingPlan) Before() *public.RoleBinding {
	if p.before == nil {
		return nil
	}
	return p.before.DeepCopy()
}

// Desired returns the original validated desired object, or nil for deletion.
func (p RoleBindingPlan) Desired() *public.RoleBinding {
	if p.after == nil {
		return nil
	}
	return p.after.DeepCopy()
}

// Manager returns the installed file source's manager.
func (p RoleBindingPlan) Manager() string { return p.manager }

// Force reports only the original loader's exact move/bootstrap decision.
func (p RoleBindingPlan) Force() bool { return p.force }

// Deleting distinguishes an orphan removal from an apply/no-op.
func (p RoleBindingPlan) Deleting() bool { return p.deleting }

// ObservedAt is the actual accepted input observation, not credential issuance
// or revocation time. The security owner observes revocation time inside its tx.
func (p RoleBindingPlan) ObservedAt() time.Time {
	if p.cut == nil {
		return time.Time{}
	}
	return p.cut.at
}

// WithCurrent pins the original accepted input across fn and its complete
// transaction/final checks. The predicate is memory-only and acquires no lock.
// Callers must not retain it after fn. Cancellation never replaces the cut.
func (p RoleBindingPlan) WithCurrent(ctx context.Context, fn func(current func() bool) error) error {
	if ctx == nil || fn == nil || !p.valid() {
		return ErrSourceChanged
	}
	c := p.cut
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return c.ctx.Err()
	case <-c.gate:
	}
	defer func() { c.gate <- struct{}{} }()
	current := func() bool { return c.active.Load() && ctx.Err() == nil && c.ctx.Err() == nil }
	if !current() {
		return ErrSourceChanged
	}
	return fn(current)
}
