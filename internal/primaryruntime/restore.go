package primaryruntime

import (
	"context"
	"maps"
	"slices"

	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

// Restore is an opaque complete prebuilt memory replacement, bound to its
// runtime. Root must join old writer work before preparation/installation.
type Restore struct {
	owner     *Runtime
	entries   map[string]*entry
	cut       Cut
	installed bool
}

// PrepareRestore prebuilds every durable lifecycle/head from one validated
// image outside publication/runtime locks. Desired-only placeholders are not
// durable runtime members; the complete image publisher supplies their coverage.
func (r *Runtime) PrepareRestore(p *PreparedImage) (*Restore, error) {
	if p == nil {
		return nil, ErrInvalid
	}
	if len(p.durable) > r.o.MaxZones {
		return nil, ErrCapacity
	}
	entries := make(map[string]*entry, len(p.durable))
	models := make([]*zone.Zone, 0, len(p.durable))
	ready := make(map[string]bool, len(p.durable))
	for _, original := range p.durable {
		cp := *original
		cp.member.Parents = slices.Clone(original.member.Parents)
		cp.member.Published = false
		apex := cp.member.Zone.Apex
		if entries[apex] != nil {
			return nil, ErrInvalid
		}
		entries[apex] = &cp
		models = append(models, cp.model)
		ready[apex] = p.cut.entries[apex] == original && p.cut.ready[apex]
	}
	snapshot, err := zone.NewSnapshot(p.base.Revision, true, models...)
	if err != nil {
		return nil, err
	}
	return &Restore{owner: r, entries: entries, cut: Cut{snapshot: snapshot, entries: maps.Clone(entries), ready: ready}}, nil
}

// InstallRestore does only a memory swap. Call under the exact candidate
// Store.PublishIfCurrent barrier; no compilation, store/provider calls or hooks.
// A restore is single use and cannot be installed into a different runtime.
func (r *Runtime) InstallRestore(prepared *Restore) error {
	if prepared == nil || prepared.owner != r {
		return ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if prepared.installed {
		return ErrConflict
	}
	r.entries = prepared.entries
	r.cut = prepared.cut
	r.generation++
	prepared.installed = true
	return nil
}

// Republish retries the exact installed member without repeating RR semantics.
// Root acknowledgment must match the complete eligible bundle and caller fence.
func (r *Runtime) Republish(ctx context.Context, id zk.ZoneID) error {
	r.mu.RLock()
	e := r.entries[id.Apex]
	r.mu.RUnlock()
	if e == nil || e.member.Zone != id {
		return ErrConflict
	}
	return r.publish(ctx, e)
}
