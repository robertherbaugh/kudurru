package domainlist

import (
	"fmt"
	"slices"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ApprovalEdit is a pure edit of one approval's derived allow entry. A nil
// Desired deletes an empty list. Added gives the approval removal ownership.
type ApprovalEdit struct {
	Desired        *public.BlockList
	Changed, Added bool
}

// PlanApprovalEntry never reads or writes a Store. It preserves manual entries,
// legacy rules, disabled sources and unrelated list contents. The caller must
// derive key and entry from a current request and commit all related effects in
// its original concrete transaction.
func PlanApprovalEntry(current *public.BlockList, key store.Key, entry public.ListEntry, remove bool) (ApprovalEdit, error) {
	a, _, ok := ParseListName(key.Name)
	if !ok || a != Allow || key.Kind != "BlockList" || entry.Origin != public.OriginUnblock || entry.Request == "" {
		return ApprovalEdit{}, fmt.Errorf("%w: invalid approval list or provenance", ErrInvalid)
	}
	if current == nil && remove {
		return ApprovalEdit{}, nil
	}
	var bl *public.BlockList
	if current == nil {
		bl = &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}, Spec: public.BlockListSpec{Mode: Allow.Mode()}}
	} else {
		if store.KeyOf(current) != key {
			return ApprovalEdit{}, fmt.Errorf("%w: mismatched approval list", ErrInvalid)
		}
		owner := store.ManagedBy(current)
		if !writableOwner(owner) {
			return ApprovalEdit{}, &store.ErrManagedBy{Key: key, Owner: owner}
		}
		if current.DeletionTimestamp != nil {
			return ApprovalEdit{}, store.ErrConflict
		}
		bl = current.DeepCopy()
	}
	if listMode(bl) != Allow.Mode() {
		return ApprovalEdit{}, fmt.Errorf("%w: approval list must have allow mode", ErrInvalid)
	}
	bl.TypeMeta = metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "BlockList"}
	prov := provenance{}
	if remove {
		prov[key.Namespace+"/"+key.Name+"\x00"+RuleKey(public.EntryRule(typeOf(entry), entry.Value))] = entry.Request
	}
	migrated := migrateList(bl, prov)
	idx := entriesSource(bl)
	var entries []public.ListEntry
	if idx >= 0 {
		entries = slices.Clone(bl.Spec.Sources[idx].Entries)
		if !bl.Spec.Sources[idx].IsEnabled() {
			for i := range entries {
				entries[i].Enabled = ptrFalse()
			}
			bl.Spec.Sources[idx].Enabled = nil
		}
	}
	changed, added := false, false
	if remove {
		before := len(entries)
		entries = slices.DeleteFunc(entries, func(e public.ListEntry) bool {
			return RuleKey(public.EntryRule(typeOf(e), e.Value)) == RuleKey(public.EntryRule(typeOf(entry), entry.Value)) && e.Origin == public.OriginUnblock
		})
		changed = len(entries) != before
	} else if !slices.ContainsFunc(entries, func(e public.ListEntry) bool { return e.Value == entry.Value && typeOf(e) == typeOf(entry) }) {
		entries = append(entries, entry)
		changed, added = true, true
	}
	if !changed && !migrated {
		return ApprovalEdit{Desired: current}, nil
	}
	switch {
	case idx >= 0 && len(entries) > 0:
		bl.Spec.Sources[idx].Entries = entries
	case idx >= 0:
		bl.Spec.Sources = slices.Delete(bl.Spec.Sources, idx, idx+1)
	case len(entries) > 0:
		bl.Spec.Sources = append([]public.ListSource{{Name: SourceName, Entries: entries}}, bl.Spec.Sources...)
	}
	if len(bl.Spec.Sources) == 0 {
		bl = nil
	}
	return ApprovalEdit{Desired: bl, Changed: true, Added: added}, nil
}
