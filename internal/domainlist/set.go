package domainlist

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking/parser"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Copy is one place a logical entry is stored: an entry of the "ui"
// source of one list, or a legacy inline line not migrated yet.
type Copy struct {
	// List is the BlockList, "namespace/name".
	List string
	// Source is the source's name.
	Source string
	// Group is the ClientGroup ("namespace/name") of a group list; empty
	// for the everyone list.
	Group string
	// ManagedBy is the list's owner (store.ManagedBy).
	ManagedBy string
	// Legacy is true for an inline line (written before structured
	// entries); the next write of the list converts it.
	Legacy bool
	// Entry is the stored entry (synthesised for legacy lines).
	Entry v1alpha1.ListEntry
}

// Kinds of Entry.
const (
	// KindEntry is an entry of the Allowlist or Denylist itself.
	KindEntry = "entry"
	// KindException is an entry of another list switched off there
	// (spec.sources[].exclude): an exception that list makes for the name.
	// An excluded entry of a block list is shown on the Allowlist, one of
	// an allow list on the Denylist. Its only copy names the list and the
	// source; Value is the exclude value as written.
	KindException = "exception"
)

// Entry is one logical allow or deny entry: every copy of (namespace,
// type, value) in the action's lists of the namespace, or one list
// exception (Kind).
type Entry struct {
	// Kind is KindEntry or KindException.
	Kind      string
	Namespace string
	Action    Action
	Type      v1alpha1.EntryType
	Value     string
	// Everyone is true when the everyone list holds a copy.
	Everyone bool
	// Groups are the ClientGroups ("namespace/name") whose lists hold a
	// copy, sorted.
	Groups []string
	// Copies, the everyone list's first, then by group.
	Copies []Copy
}

// ID is the entry's API identifier.
func (e *Entry) ID() string {
	if e.Kind == KindException {
		_, list, _ := strings.Cut(e.Copies[0].List, "/")
		return ExceptionID(e.Namespace, list, e.Copies[0].Source, e.Value)
	}
	return ID(e.Namespace, e.Type, e.Value)
}

// Rule is the rule the entry compiles to; for an exception the rule of
// the list it switches off.
func (e *Entry) Rule() string {
	if e.Kind == KindException {
		return e.Value
	}
	return v1alpha1.EntryRule(e.Type, e.Value)
}

// Primary is the copy whose switch, comment and provenance the entry
// shows.
func (e *Entry) Primary() *v1alpha1.ListEntry { return &e.Copies[0].Entry }

// Enabled reports whether any copy is switched on.
func (e *Entry) Enabled() bool {
	for i := range e.Copies {
		if e.Copies[i].Entry.IsEnabled() {
			return true
		}
	}
	return false
}

// ReadOnly names the owner of a copy's list that the API may not change,
// or "".
func (e *Entry) ReadOnly() string {
	for _, c := range e.Copies {
		if !writableOwner(c.ManagedBy) {
			return c.ManagedBy
		}
	}
	return ""
}

// ListInfo describes one ui list.
type ListInfo struct {
	// List is "namespace/name".
	List   string
	Action Action
	// Group is the ClientGroup "namespace/name"; empty for everyone.
	Group string
	// Enabled is the list's kudurru.io/enabled switch.
	Enabled   bool
	Entries   int
	ManagedBy string
	// Policies reference the list, "namespace/name", sorted.
	Policies []string
}

// Unattached is a policy that should reference a list but is managed
// elsewhere, so the API did not add the reference.
type Unattached struct {
	List, Policy, ManagedBy string
}

// Set is every ui list of the namespaces read, with its entries and the
// policies that apply them, and the exceptions other lists make.
type Set struct {
	entries    map[Action][]*Entry
	Lists      []ListInfo
	Unattached []Unattached
	// ExceptionPolicies are the policies ("namespace/name") that reference
	// each list ("namespace/name") with exceptions.
	ExceptionPolicies map[string][]string
}

// Entries returns the logical entries of action, newest first, then by
// value.
func (s *Set) Entries(a Action) []*Entry { return s.entries[a] }

// Find returns the entry (namespace, type, value) of action.
func (s *Set) Find(a Action, ns string, t v1alpha1.EntryType, value string) *Entry {
	for _, e := range s.entries[a] {
		if e.Kind == KindEntry && e.Namespace == ns && e.Type == t && e.Value == value {
			return e
		}
	}
	return nil
}

// FindException returns the exception (list, source, value) of ns, or nil.
func (s *Set) FindException(ns, list, source, value string) *Entry {
	for _, a := range []Action{Allow, Deny} {
		for _, e := range s.entries[a] {
			if e.Kind == KindException && e.Namespace == ns && e.Copies[0].List == ns+"/"+list && e.Copies[0].Source == source && e.Value == value {
				return e
			}
		}
	}
	return nil
}

// Load reads the ui lists of ns (every namespace when ns is empty), the
// exceptions every other list makes, and the policies that reference
// them.
func (s *Service) Load(ctx context.Context, ns string) (*Set, error) {
	var lists, others []*v1alpha1.BlockList
	if err := s.each(ctx, "BlockList", ns, func(o store.Object) {
		if bl, ok := o.(*v1alpha1.BlockList); ok {
			if a, _, ok := ParseListName(bl.Name); ok && listMode(bl) == a.Mode() {
				lists = append(lists, bl)
			} else {
				others = append(others, bl)
			}
		}
	}); err != nil {
		return nil, err
	}
	var pols []*v1alpha1.Policy
	if err := s.each(ctx, "Policy", ns, func(o store.Object) {
		if p, ok := o.(*v1alpha1.Policy); ok {
			pols = append(pols, p)
		}
	}); err != nil {
		return nil, err
	}
	set := buildSet(lists, pols)
	set.addExceptions(others, pols)
	return set, nil
}

// addExceptions adds a KindException entry for every exclude value of a
// URL, ConfigMap or inline source of lists: on the Allowlist for block
// lists, on the Denylist for allow lists.
func (s *Set) addExceptions(lists []*v1alpha1.BlockList, pols []*v1alpha1.Policy) {
	s.ExceptionPolicies = map[string][]string{}
	slices.SortFunc(lists, func(a, b *v1alpha1.BlockList) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	for _, bl := range lists {
		a := Allow
		if listMode(bl) == "allow" {
			a = Deny
		}
		key := bl.Namespace + "/" + bl.Name
		owner := store.ManagedBy(bl)
		var exc []*Entry
		for i, src := range bl.Spec.Sources {
			if len(src.Entries) > 0 {
				continue
			}
			for _, v := range src.Exclude {
				t, _, ok := FromRule(v, parser.Format(src.Format))
				if !ok {
					t = v1alpha1.EntryExact
				}
				exc = append(exc, &Entry{
					Kind: KindException, Namespace: bl.Namespace, Action: a, Type: t, Value: v,
					Copies: []Copy{{List: key, Source: sourceName(i, src), ManagedBy: owner, Entry: v1alpha1.ListEntry{Value: v, Type: t}}},
				})
			}
		}
		if len(exc) == 0 {
			continue
		}
		for _, p := range pols {
			if p.Namespace == bl.Namespace && (slices.Contains(p.Spec.BlockListRefs, bl.Name) || slices.Contains(p.Spec.AllowListRefs, bl.Name)) {
				s.ExceptionPolicies[key] = append(s.ExceptionPolicies[key], p.Namespace+"/"+p.Name)
			}
		}
		slices.Sort(s.ExceptionPolicies[key])
		s.entries[a] = append(s.entries[a], exc...)
	}
}

func listMode(bl *v1alpha1.BlockList) v1alpha1.ListMode {
	if bl.Spec.Mode == "" {
		return "block"
	}
	return bl.Spec.Mode
}

// refs returns the policy field a list of action is referenced from.
func refs(a Action, p *v1alpha1.Policy) []string {
	if a == Allow {
		return p.Spec.AllowListRefs
	}
	return p.Spec.BlockListRefs
}

func buildSet(lists []*v1alpha1.BlockList, pols []*v1alpha1.Policy) *Set {
	slices.SortFunc(lists, func(a, b *v1alpha1.BlockList) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	set := &Set{entries: map[Action][]*Entry{}}
	byKey := map[string]*Entry{}
	for _, bl := range lists {
		a, group, _ := ParseListName(bl.Name)
		if group != "" {
			group = bl.Namespace + "/" + group
		}
		info := ListInfo{List: bl.Namespace + "/" + bl.Name, Action: a, Group: group, Enabled: !v1alpha1.IsDisabled(bl), ManagedBy: store.ManagedBy(bl)}
		for _, p := range pols {
			if p.Namespace != bl.Namespace {
				continue
			}
			if slices.Contains(refs(a, p), bl.Name) {
				info.Policies = append(info.Policies, p.Namespace+"/"+p.Name)
				continue
			}
			if writableOwner(store.ManagedBy(p)) {
				continue // attached by the next write
			}
			want := group == ""
			if !want {
				_, g, _ := strings.Cut(group, "/")
				want = slices.Contains(p.Spec.ClientGroupRefs, g)
			}
			if want {
				set.Unattached = append(set.Unattached, Unattached{List: info.List, Policy: p.Namespace + "/" + p.Name, ManagedBy: store.ManagedBy(p)})
			}
		}
		slices.Sort(info.Policies)
		created := bl.CreationTimestamp.DeepCopy()
		if created.IsZero() {
			created = nil
		}
		for _, c := range listCopies(bl, group, info.ManagedBy, created) {
			info.Entries++
			key := bl.Namespace + "\x00" + string(a) + "\x00" + string(c.Entry.Type) + "\x00" + c.Entry.Value
			e := byKey[key]
			if e == nil {
				e = &Entry{Kind: KindEntry, Namespace: bl.Namespace, Action: a, Type: c.Entry.Type, Value: c.Entry.Value}
				byKey[key] = e
				set.entries[a] = append(set.entries[a], e)
			}
			e.Copies = append(e.Copies, c)
			if c.Group == "" {
				e.Everyone = true
			} else if !slices.Contains(e.Groups, c.Group) {
				e.Groups = append(e.Groups, c.Group)
			}
		}
		set.Lists = append(set.Lists, info)
	}
	for a := range set.entries {
		for _, e := range set.entries[a] {
			slices.Sort(e.Groups)
			slices.SortStableFunc(e.Copies, func(x, y Copy) int { return cmp.Compare(x.Group, y.Group) })
		}
		slices.SortStableFunc(set.entries[a], func(x, y *Entry) int {
			return cmp.Or(compareTime(y.Primary().Created, x.Primary().Created), cmp.Compare(x.Value, y.Value), cmp.Compare(x.Namespace, y.Namespace), cmp.Compare(x.Type, y.Type))
		})
	}
	return set
}

// compareTime orders unset times first.
func compareTime(a, b *metav1.Time) int {
	var ta, tb time.Time
	if a != nil {
		ta = a.Time
	}
	if b != nil {
		tb = b.Time
	}
	return ta.Compare(tb)
}

// listCopies returns the entries of a ui list: the structured entries of
// its sources and its legacy inline lines that map to entries.
func listCopies(bl *v1alpha1.BlockList, group, owner string, created *metav1.Time) []Copy {
	var out []Copy
	seen := map[string]bool{}
	add := func(c Copy) {
		k := string(c.Entry.Type) + "\x00" + c.Entry.Value
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, c)
	}
	list := bl.Namespace + "/" + bl.Name
	for i, src := range bl.Spec.Sources {
		if src.URL != "" || src.ConfigMapRef != nil {
			continue
		}
		name := sourceName(i, src)
		for _, e := range src.Entries {
			if e.Type == "" {
				e.Type = v1alpha1.EntryExact
			}
			if !src.IsEnabled() {
				e.Enabled = ptrFalse() // the whole source is switched off
			}
			add(Copy{List: list, Source: name, Group: group, ManagedBy: owner, Entry: e})
		}
		for _, line := range src.Inline {
			t, v, ok := FromRule(line, parser.Format(src.Format))
			if !ok {
				continue
			}
			e := v1alpha1.ListEntry{Value: v, Type: t, Origin: v1alpha1.OriginMigrated, Created: created}
			if !src.IsEnabled() {
				e.Enabled = ptrFalse()
			}
			add(Copy{List: list, Source: name, Group: group, ManagedBy: owner, Legacy: true, Entry: e})
		}
	}
	return out
}
