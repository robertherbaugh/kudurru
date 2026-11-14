package domainlist

import (
	"context"
	"errors"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// writeAttempts bounds the read-modify-write retries of a write that
// loses a race with another writer.
const writeAttempts = 4

// mutated says what mutate did.
type mutated int

const (
	unchanged mutated = iota
	written
	deleted
)

// mutate applies fn to the entries of the "ui" source of the list ns/name
// and writes the list: created (mode of a) when absent and create is set,
// its legacy inline lines converted to entries first, deleted and
// detached from every policy once it holds nothing. fn returns the new
// entries and whether it changed anything.
func (s *Service) mutate(ctx context.Context, ns, name string, a Action, create bool, fn func([]v1alpha1.ListEntry) ([]v1alpha1.ListEntry, bool)) (mutated, error) {
	for attempt := 1; ; attempt++ {
		m, err := s.tryMutate(ctx, ns, name, a, create, fn)
		if !errors.Is(err, store.ErrConflict) || attempt == writeAttempts {
			return m, err
		}
	}
}

func (s *Service) tryMutate(ctx context.Context, ns, name string, a Action, create bool, fn func([]v1alpha1.ListEntry) ([]v1alpha1.ListEntry, bool)) (mutated, error) {
	key := store.Key{Kind: "BlockList", Namespace: ns, Name: name}
	bl := &v1alpha1.BlockList{}
	exists := true
	if err := s.opts.Store.Get(ctx, key, bl); errors.Is(err, store.ErrNotFound) {
		if !create {
			return unchanged, nil
		}
		exists = false
		bl = &v1alpha1.BlockList{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1alpha1.BlockListSpec{Mode: a.Mode()},
		}
	} else if err != nil {
		return unchanged, err
	}
	bl.TypeMeta = metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "BlockList"}
	owner := store.ManagedBy(bl)
	if exists && !writableOwner(owner) {
		return unchanged, &store.ErrManagedBy{Key: key, Owner: owner}
	}
	if listMode(bl) != a.Mode() {
		return unchanged, fmt.Errorf("%w: BlockList %s/%s has mode %s, not %s", ErrInvalid, ns, name, listMode(bl), a.Mode())
	}
	migrated := migrateList(bl, nil)
	idx := entriesSource(bl)
	var cur []v1alpha1.ListEntry
	if idx >= 0 {
		cur = slices.Clone(bl.Spec.Sources[idx].Entries)
		if !bl.Spec.Sources[idx].IsEnabled() {
			// A switched-off source would hide what this write does: the
			// switch moves onto its entries (they stay off unless fn
			// switches them on) and the source is switched on.
			for i := range cur {
				cur[i].Enabled = ptrFalse()
			}
			bl.Spec.Sources[idx].Enabled = nil
		}
	}
	next, changed := fn(cur)
	if !changed && !migrated {
		return unchanged, nil
	}
	switch {
	case idx >= 0 && len(next) > 0:
		bl.Spec.Sources[idx].Entries = next
	case idx >= 0:
		bl.Spec.Sources = slices.Delete(bl.Spec.Sources, idx, idx+1)
	case len(next) > 0:
		bl.Spec.Sources = append([]v1alpha1.ListSource{{Name: SourceName, Entries: next}}, bl.Spec.Sources...)
	}
	if len(bl.Spec.Sources) == 0 {
		if !exists {
			return unchanged, nil
		}
		if err := s.opts.Store.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerAPI, Force: owner == store.ManagerBootstrap}); err != nil && !errors.Is(err, store.ErrNotFound) {
			return unchanged, err
		}
		return deleted, s.detachEverywhere(ctx, ns, name)
	}
	opts := store.ApplyOptions{Manager: store.ManagerAPI, Force: owner == store.ManagerBootstrap}
	if exists {
		opts.ExpectedResourceVersion = bl.ResourceVersion
	}
	if _, err := s.opts.Store.Apply(ctx, bl, opts); err != nil {
		return unchanged, err
	}
	return written, nil
}

// entriesSource is the index of the source holding the entries: the one
// named "ui" with entries, else the first with entries; -1 for none.
func entriesSource(bl *v1alpha1.BlockList) int {
	if i := slices.IndexFunc(bl.Spec.Sources, func(s v1alpha1.ListSource) bool { return s.Name == SourceName && len(s.Entries) > 0 }); i >= 0 {
		return i
	}
	return slices.IndexFunc(bl.Spec.Sources, func(s v1alpha1.ListSource) bool { return len(s.Entries) > 0 })
}

// Provenance names the UnblockRequest whose approval holds a legacy line,
// keyed by "<list>\x00<RuleKey>", for Migrate.
type provenance map[string]string

// migrateList converts the legacy inline lines of a ui list into entries
// of its "ui" source (lossless: lines that are not domain, wildcard or
// regex rules stay in an inline source, and a switched-off inline source
// gives switched-off entries). The entries take the list's creation time
// and origin "migrated", or "unblock" with the request when prov knows the
// approval that added the line. It reports whether bl changed.
func migrateList(bl *v1alpha1.BlockList, prov provenance) bool {
	_, group, ok := ParseListName(bl.Name)
	if !ok {
		return false
	}
	scopeChanged := group != "" && !slices.Equal(bl.Spec.ClientGroupRefs, []string{group})
	if scopeChanged {
		bl.Spec.ClientGroupRefs = []string{group}
	}
	var created *metav1.Time
	if !bl.CreationTimestamp.IsZero() {
		c := bl.CreationTimestamp.Rfc3339Copy()
		created = &c
	}
	idx := entriesSource(bl)
	var entries []v1alpha1.ListEntry
	if idx >= 0 {
		entries = slices.Clone(bl.Spec.Sources[idx].Entries)
	}
	changed := false
	var sources []v1alpha1.ListSource
	for i, src := range bl.Spec.Sources {
		if i == idx {
			sources = append(sources, src) // replaced below
			continue
		}
		if src.URL != "" || src.ConfigMapRef != nil || len(src.Entries) > 0 || len(src.Inline) == 0 {
			sources = append(sources, src)
			continue
		}
		var rest []string
		for _, line := range src.Inline {
			t, v, ok := FromRule(line, parser.Format(src.Format))
			if !ok {
				rest = append(rest, line)
				continue
			}
			changed = true
			if slices.ContainsFunc(entries, func(e v1alpha1.ListEntry) bool { return e.Value == v && typeOf(e) == t }) {
				continue
			}
			e := v1alpha1.ListEntry{Value: v, Type: t, Created: created, Origin: v1alpha1.OriginMigrated}
			if req := prov[bl.Namespace+"/"+bl.Name+"\x00"+RuleKey(line)]; req != "" {
				e.Origin, e.Request = v1alpha1.OriginUnblock, req
			}
			if !src.IsEnabled() {
				e.Enabled = ptrFalse()
			}
			entries = append(entries, e)
		}
		if len(rest) > 0 {
			src.Inline = rest
			if src.Name == SourceName || src.Name == "" {
				src.Name = unconvertedSource
			}
			sources = append(sources, src)
		}
	}
	if !changed {
		return scopeChanged
	}
	if idx >= 0 {
		for i := range sources {
			if sources[i].Name == bl.Spec.Sources[idx].Name && len(sources[i].Entries) > 0 {
				sources[i].Entries = entries
			}
		}
	} else if len(entries) > 0 {
		sources = append([]v1alpha1.ListSource{{Name: SourceName, Entries: entries}}, sources...)
	}
	bl.Spec.Sources = sources
	return true
}

// attachTarget references the list of tg from the policies it applies
// through and records them in res.
func (s *Service) attachTarget(ctx context.Context, a Action, ns string, tg target, res *Result) error {
	pols, err := s.policiesFor(ctx, ns, tg.group)
	if err != nil {
		return err
	}
	for _, p := range pols {
		ok, err := s.attach(ctx, a, p, tg.list)
		if err != nil {
			return err
		}
		res.attachedTo(p.Namespace+"/"+p.Name, ok)
	}
	return nil
}

// attach adds list to the allow or block references of p. ok is false
// when p is managed elsewhere and was left alone.
func (s *Service) attach(ctx context.Context, a Action, p *v1alpha1.Policy, list string) (bool, error) {
	if slices.Contains(refs(a, p), list) {
		return writableOwner(store.ManagedBy(p)), nil
	}
	for attempt := 1; ; attempt++ {
		ok, err := s.editPolicy(ctx, p.Namespace, p.Name, func(p *v1alpha1.Policy) bool {
			r := &p.Spec.BlockListRefs
			if a == Allow {
				r = &p.Spec.AllowListRefs
			}
			if slices.Contains(*r, list) {
				return false
			}
			*r = append(*r, list)
			return true
		})
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		if !errors.Is(err, store.ErrConflict) || attempt == writeAttempts {
			return ok, err
		}
	}
}

// detachEverywhere removes list from the references of every writable
// policy in ns.
func (s *Service) detachEverywhere(ctx context.Context, ns, list string) error {
	var names []string
	if err := s.each(ctx, "Policy", ns, func(o store.Object) {
		if p, ok := o.(*v1alpha1.Policy); ok && (slices.Contains(p.Spec.AllowListRefs, list) || slices.Contains(p.Spec.BlockListRefs, list)) {
			names = append(names, p.Name)
		}
	}); err != nil {
		return err
	}
	for _, n := range names {
		for attempt := 1; ; attempt++ {
			_, err := s.editPolicy(ctx, ns, n, func(p *v1alpha1.Policy) bool {
				before := len(p.Spec.AllowListRefs) + len(p.Spec.BlockListRefs)
				drop := func(r string) bool { return r == list }
				p.Spec.AllowListRefs = slices.DeleteFunc(p.Spec.AllowListRefs, drop)
				p.Spec.BlockListRefs = slices.DeleteFunc(p.Spec.BlockListRefs, drop)
				return len(p.Spec.AllowListRefs)+len(p.Spec.BlockListRefs) != before
			})
			if !errors.Is(err, store.ErrConflict) || attempt == writeAttempts {
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
				break
			}
		}
	}
	return nil
}

// editPolicy applies fn to policy ns/name and writes it when fn changed
// it. ok is false when the policy is not writable by the API.
func (s *Service) editPolicy(ctx context.Context, ns, name string, fn func(*v1alpha1.Policy) bool) (bool, error) {
	p := &v1alpha1.Policy{}
	if err := s.opts.Store.Get(ctx, store.Key{Kind: "Policy", Namespace: ns, Name: name}, p); err != nil {
		return false, err
	}
	owner := store.ManagedBy(p)
	if !writableOwner(owner) {
		return false, nil
	}
	if !fn(p) {
		return true, nil
	}
	p.TypeMeta = metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "Policy"}
	_, err := s.opts.Store.Apply(ctx, p, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: p.ResourceVersion, Force: owner == store.ManagerBootstrap})
	return true, err
}
