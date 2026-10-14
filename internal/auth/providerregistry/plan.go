package providerregistry

import (
	"context"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// PlanInitialize is an explicit startup input. Stage refuses root loss or legacy
// config-origin authority; normal reads never call it or synthesize empty state.
func PlanInitialize(clusterID string) (*Plan, error) {
	r, err := Empty(clusterID)
	if err != nil {
		return nil, err
	}
	return &Plan{after: r, initialize: true}, nil
}

// PlanReconcile accepts a complete explicit config manifest. Existing UIDs
// survive edits; removals retain slots, and readding a removed slot gets a new UID.
// It never learns node configuration or changes authority during a read.
func PlanReconcile(before *Registry, desired []DesiredConfig, at time.Time, newUID func() (string, error)) (*Plan, error) {
	if before == nil || before.digest == "" || desired == nil || !validTime(at) {
		return nil, ErrInvalid
	}
	if len(desired) > MaxActiveSources {
		return nil, ErrBounds
	}
	budget := 0
	for _, d := range desired {
		if !validID(d.ID) {
			return nil, ErrInvalid
		}
		if err := specBudget(reflect.ValueOf(d.Spec), &budget); err != nil {
			return nil, err
		}
	}
	after := copyRegistry(before)
	p := &Plan{before: before, after: after, at: at.UTC(), retired: []ProviderIncarnation{}, changed: []ProviderIncarnation{}}
	seen := map[string]bool{}
	changed := false
	for _, d := range desired {
		if seen[d.ID] {
			return nil, ErrInvalid
		}
		seen[d.ID] = true
		// Canonical config precedence retires previously accepted object authority
		// even before a replacement config receipt has been acquired/admitted.
		for _, a := range after.authorities {
			if a.Active && a.Provider.ID == d.ID && a.Provider.Origin == servingauth.ProviderObject {
				p.retired = append(p.retired, ProviderIncarnation{a.Provider.ID, a.Provider.UID})
				if err := retire(after, a.Provider); err != nil {
					return nil, err
				}
				changed = true
			}
		}
		if _, err := oidccore.Compile(d.Spec, d.ID); err != nil {
			return nil, ErrInvalid
		}
		index := -1
		for i, s := range after.sources {
			if s.ID == d.ID {
				index = i
				break
			}
		}
		var next Source
		if index >= 0 && after.sources[index].Active {
			next = after.sources[index]
			if next.SpecDigest == specDigest(d.Spec) {
				continue
			}
			if next.Revision == math.MaxUint64 {
				return nil, ErrBounds
			}
			p.changed = append(p.changed, ProviderIncarnation{next.ID, next.UID})
			next.Revision++
			next.AuthorityKey = ""
			if err := retire(after, servingauth.ProviderRef{ID: next.ID, UID: next.UID, Origin: servingauth.ProviderConfig}); err != nil {
				return nil, err
			}
		} else {
			if newUID == nil {
				return nil, ErrInvalid
			}
			uid, err := newUID()
			if err != nil {
				return nil, ErrInvalid
			}
			if !uidPattern.MatchString(uid) {
				return nil, ErrInvalid
			}
			for _, old := range before.sources {
				if old.UID == uid {
					return nil, ErrInvalid
				}
			}
			for _, old := range before.authorities {
				if old.Provider.UID == uid {
					return nil, ErrInvalid
				}
			}
			next = Source{FormatVersion: FormatVersion, ID: d.ID, UID: uid, CreatedAt: at.UTC(), Revision: 1, Active: true}
			if index >= 0 {
				if after.sources[index].Revision == math.MaxUint64 {
					return nil, ErrBounds
				}
				next.Revision = after.sources[index].Revision + 1
			}
		}
		next.Spec = cloneSpec(d.Spec)
		next.SpecDigest = specDigest(next.Spec)
		if index < 0 {
			if len(after.sources) >= MaxSources {
				return nil, ErrBounds
			}
			after.sources = append(after.sources, next)
		} else {
			after.sources[index] = next
		}
		changed = true
	}
	for i, s := range after.sources {
		if !s.Active || seen[s.ID] {
			continue
		}
		if s.Revision == math.MaxUint64 {
			return nil, ErrBounds
		}
		p.retired = append(p.retired, ProviderIncarnation{s.ID, s.UID})
		after.sources[i].Active = false
		after.sources[i].AuthorityKey = ""
		after.sources[i].Revision++
		if err := retire(after, servingauth.ProviderRef{ID: s.ID, UID: s.UID, Origin: servingauth.ProviderConfig}); err != nil {
			return nil, err
		}
		changed = true
	}
	if err := finish(p, changed); err != nil {
		return nil, err
	}
	return p, nil
}

// PlanAcceptAuthority accepts a COMPLETE inventory of root-admitted active
// receipts for both origins. Omitted receipts become retained inactive history
// and require floors; unavailable sources may retain their unchanged receipt.
// Initial enrollment has no old authority; any changed receipt requires a floor.
// Plans are bounded and detached, not provenance for granting host capabilities.
func PlanAcceptAuthority(before *Registry, desired []DesiredAuthority, at time.Time) (*Plan, error) {
	if before == nil || before.digest == "" || desired == nil || !validTime(at) {
		return nil, ErrInvalid
	}
	if len(desired) > MaxActiveAuthorities {
		return nil, ErrBounds
	}
	budget := 0
	for _, d := range desired {
		if err := specBudget(reflect.ValueOf(d.Source.Spec), &budget); err != nil {
			return nil, err
		}
	}
	after := copyRegistry(before)
	p := &Plan{before: before, after: after, at: at.UTC(), retired: []ProviderIncarnation{}, changed: []ProviderIncarnation{}}
	seen := map[string]bool{}
	seenIDs := map[string]bool{}
	changed := false
	for _, d := range desired {
		desc := d.Source
		if !validRef(desc.Ref) || !validTime(desc.CreatedAt) || !hashPattern.MatchString(desc.SpecIdentity) || !hashPattern.MatchString(desc.PolicyIdentity) || desc.Issuer != desc.Spec.Issuer {
			return nil, ErrInvalid
		}
		policy, err := oidccore.Compile(desc.Spec, desc.Ref.ID)
		if err != nil || policy.Identity() != desc.PolicyIdentity {
			return nil, ErrInvalid
		}
		key := AuthorityKey(desc.Ref)
		if seen[key] || seenIDs[desc.Ref.ID] {
			return nil, ErrInvalid
		}
		seen[key] = true
		seenIDs[desc.Ref.ID] = true
		p.descriptors = append(p.descriptors, cloneDescriptor(desc))
		next := AuthorityRecord{FormatVersion: FormatVersion, Provider: desc.Ref, SpecIdentity: desc.SpecIdentity, Revision: 1, Active: true, Binding: d.Binding}
		if err := validAuthority(next); err != nil {
			return nil, err
		}
		index := -1
		for i, a := range after.authorities {
			if a.Provider == desc.Ref {
				index = i
				break
			}
		}
		if index >= 0 {
			old := after.authorities[index]
			if old.Active && old.SpecIdentity == next.SpecIdentity && old.Binding == next.Binding {
				continue
			}
			if old.Revision == math.MaxUint64 {
				return nil, ErrBounds
			}
			next.Revision = old.Revision + 1
			p.changed = append(p.changed, ProviderIncarnation{desc.Ref.ID, desc.Ref.UID})
			after.authorities[index] = next
		} else {
			if len(after.authorities) >= MaxAuthorities {
				return nil, ErrBounds
			}
			after.authorities = append(after.authorities, next)
		}
		if desc.Ref.Origin == servingauth.ProviderConfig {
			found := false
			for i, s := range after.sources {
				if s.ID != desc.Ref.ID {
					continue
				}
				expected, err := configDescriptor(s)
				if err != nil || !s.Active || expected.Ref != desc.Ref || expected.SpecIdentity != desc.SpecIdentity {
					return nil, ErrConflict
				}
				after.sources[i].AuthorityKey = key
				found = true
				break
			}
			if !found {
				return nil, ErrConflict
			}
		}
		changed = true
	}
	for _, a := range after.authorities {
		if !a.Active || seen[AuthorityKey(a.Provider)] {
			continue
		}
		p.retired = append(p.retired, ProviderIncarnation{a.Provider.ID, a.Provider.UID})
		if err := retire(after, a.Provider); err != nil {
			return nil, err
		}
		for i, s := range after.sources {
			if s.AuthorityKey == AuthorityKey(a.Provider) {
				after.sources[i].AuthorityKey = ""
			}
		}
		changed = true
	}
	if err := finish(p, changed); err != nil {
		return nil, err
	}
	return p, nil
}

// ConfigSources returns detached planned canonical config sources.
func (p *Plan) ConfigSources() []servingauth.ProviderSource {
	if p == nil {
		return nil
	}
	return p.after.ConfigSources()
}

// Retired returns detached floor targets for withdrawn or shadowed incarnations.
func (p *Plan) Retired() []ProviderIncarnation {
	if p == nil {
		return nil
	}
	return slices.Clone(p.retired)
}

// Changed returns detached floor targets for retained incarnation/authority changes.
func (p *Plan) Changed() []ProviderIncarnation {
	if p == nil {
		return nil
	}
	return slices.Clone(p.changed)
}

// Stage stages only registry rows in the caller's actual transaction. It checks
// current CAS, exact tx source proof and ALREADY STAGED persistent auth floors.
// Root retains admission, exclusion and authority activation after durable commit.
func (p *Plan) Stage(ctx context.Context, tx *sqlite.CommandTx) error {
	if p == nil || p.after == nil || tx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if tx.Authority().ClusterID != p.after.root.ClusterID {
		return ErrConflict
	}
	entries, err := tx.ListSections(ctx, "security")
	if err != nil {
		return err
	}
	auth, err := servingauth.DecodeSecurity(entries)
	if err != nil {
		return err
	}
	if p.initialize {
		for _, e := range entries {
			if strings.HasPrefix(e.Key, prefix) {
				return ErrManifest
			}
		}
		for _, i := range auth.ExternalIdentities {
			if i.Provider.Origin == servingauth.ProviderConfig {
				return ErrManifest
			}
		}
		for _, s := range auth.ExternalSessions {
			if s.Provider.Origin == servingauth.ProviderConfig {
				return ErrManifest
			}
		}
	} else {
		current, err := Decode(entries, p.after.root.ClusterID)
		if err != nil {
			return err
		}
		if current.Digest() != p.before.Digest() {
			return ErrConflict
		}
	}
	for _, targets := range [][]ProviderIncarnation{p.retired, p.changed} {
		for _, target := range targets {
			found := false
			for _, f := range auth.ProviderFloors {
				if f.Provider == target.ID && f.UID == target.UID && !f.ValidAfter.Before(p.at) {
					found = true
					break
				}
			}
			if !found {
				return ErrFloor
			}
		}
	}
	if len(p.descriptors) > 0 {
		actual, err := servingauth.ProviderSourcesCurrent(ctx, tx, p.after.ConfigSources())
		if err != nil {
			return err
		}
		for _, expected := range p.descriptors {
			found := false
			for _, d := range actual {
				if d.Ref == expected.Ref && d.SpecIdentity == expected.SpecIdentity && d.PolicyIdentity == expected.PolicyIdentity {
					found = true
					break
				}
			}
			if !found {
				return ErrConflict
			}
		}
	}
	encoded, err := Encode(p.after)
	if err != nil {
		return err
	}
	combined := make([]replication.Entry, 0, len(entries)+len(encoded))
	for _, e := range entries {
		if !strings.HasPrefix(e.Key, prefix) {
			combined = append(combined, e)
		}
	}
	combined = append(combined, encoded...)
	if _, err := servingauth.DecodeSecurity(combined); err != nil {
		return err
	}
	if _, err := Decode(combined, p.after.root.ClusterID); err != nil {
		return err
	}
	nextKeys := map[string]bool{}
	for _, e := range encoded {
		nextKeys[e.Key] = true
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Key, prefix) && !nextKeys[e.Key] {
			if err := tx.DeleteSection(ctx, "security", e.Key); err != nil {
				return err
			}
		}
	}
	for _, e := range encoded {
		if err := tx.PutSection(ctx, "security", e); err != nil {
			return err
		}
	}
	return nil
}

func finish(p *Plan, changed bool) error {
	if changed {
		if p.after.root.Epoch == math.MaxUint64 {
			return ErrBounds
		}
		p.after.root.Epoch++
	}
	encoded := rows(p.after)
	after, err := Decode(encoded, p.after.root.ClusterID)
	if err != nil {
		return err
	}
	p.after = after
	return nil
}
func copyRegistry(r *Registry) *Registry {
	return &Registry{root: r.root, sources: r.Sources(), authorities: r.Authorities(), digest: r.digest}
}
func retire(r *Registry, ref servingauth.ProviderRef) error {
	for i, a := range r.authorities {
		if a.Provider == ref && a.Active {
			if a.Revision == math.MaxUint64 {
				return ErrBounds
			}
			r.authorities[i].Active = false
			r.authorities[i].Revision++
		}
	}
	return nil
}
func cloneDescriptor(d servingauth.ProviderDescriptor) servingauth.ProviderDescriptor {
	d.Spec = cloneSpec(d.Spec)
	return d
}
func specBudget(v reflect.Value, budget *int) error {
	*budget += 32
	if *budget > MaxBytes {
		return ErrBounds
	}
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			return specBudget(v.Elem(), budget)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if err := specBudget(v.Field(i), budget); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if v.Len() > 64 {
			return ErrBounds
		}
		for i := 0; i < v.Len(); i++ {
			if err := specBudget(v.Index(i), budget); err != nil {
				return err
			}
		}
	case reflect.String:
		if v.Len() > oidccore.MaxStringBytes {
			return ErrBounds
		}
		*budget += 6 * v.Len()
	}
	if *budget > MaxBytes {
		return ErrBounds
	}
	return nil
}
