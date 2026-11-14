package zone

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/miekg/dns"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// Client-group views (split horizon, D403).
//
// An endpoint whose kudurru.view names a ClientGroup is served only to the
// clients of that group. A zone with views is built as its default zone
// (every endpoint without a view) plus one complete variant per group: the
// default data with the group's RRsets laid over it. A view RRset replaces
// the default RRset of the same owner and type; a view CNAME replaces every
// default RRset at its owner, and any view RRset replaces a default CNAME at
// its owner. Views never remove a default name, and a client outside the
// group never sees a view-only name: it gets the default zone's NXDOMAIN or
// NODATA with the default SOA. A view name, and each empty non-terminal it
// creates, exists in the group's variant, so a default wildcard does not
// synthesize answers at those names for the group (RFC 4592 §2.2.2).
//
// A view replaces only default data its own namespace claimed: a view
// RRset that would replace another namespace's default RRset is refused as
// Duplicated, as a default endpoint repeating it would be. The view budget
// (MaxViews) is per zone and shared, in build order, by every record set
// feeding the zone.
//
// The default zone is what is transferred (AXFR/IXFR), exported, compared by
// RFC 2136 UPDATE prerequisites and published to sinks. Group data never
// leaves through those paths. The query path selects a variant with
// ForGroups, which costs no allocation and returns the default zone itself
// when the zone has no views.

// MaxViews bounds the client-group views of one zone. Each view holds a
// complete copy of the zone's names (the RRs themselves are shared), so the
// bound keeps a large zone's memory finite.
const MaxViews = 16

// ViewPublic is the publish-only view: such endpoints go to sinks and are
// never served (D29).
const ViewPublic = "public"

// clientView is one built variant of a zone.
type clientView struct {
	group string // ClientGroup ID, "namespace/name" (policy GroupIDs)
	zone  *Zone
	// sets are the view's own RRsets (the overlay), in canonical order.
	sets []RRset
}

// GroupView returns the ClientGroup an endpoint's view names, or "" when the
// endpoint has no view or the publish-only "public" view.
func GroupView(ep *v1alpha1.Endpoint) string {
	if ep == nil || ep.Kudurru == nil || ep.Kudurru.View == ViewPublic {
		return ""
	}
	return ep.Kudurru.View
}

// ViewGroupID is the ClientGroup ID a view in a record set of namespace
// refers to. ClientGroup references are namespace-local, as Policy
// clientGroupRefs are; the ID has the form of pipeline Client.GroupIDs.
func ViewGroupID(namespace, view string) string {
	if namespace == "" {
		return view
	}
	return namespace + "/" + view
}

// ForGroups returns the variant of z a client in groups (sorted
// ClientGroup IDs, pipeline Client.GroupIDs) is answered from. A client in
// several groups with views gets the view of the group whose ID sorts
// first. Without views, or for a client in none of them, it returns z.
func (z *Zone) ForGroups(groups []string) *Zone {
	if z == nil || len(z.views) == 0 || len(groups) == 0 {
		return z
	}
	for i := range z.views {
		if slices.Contains(groups, z.views[i].group) {
			return z.views[i].zone
		}
	}
	return z
}

// Base returns the default zone of a view variant, or z itself.
func (z *Zone) Base() *Zone {
	if z != nil && z.base != nil {
		return z.base
	}
	return z
}

// HasViews reports whether the zone serves any client-group view.
func (z *Zone) HasViews() bool { return z != nil && len(z.views) != 0 }

// ViewGroups returns the ClientGroup IDs the zone has views for, sorted.
func (z *Zone) ViewGroups() []string {
	if z == nil {
		return nil
	}
	out := make([]string, len(z.views))
	for i := range z.views {
		out[i] = z.views[i].group
	}
	return out
}

// ViewRRsets calls fn for every RRset of every view (the view's own data,
// not the default data it inherits), views in group order and RRsets in
// canonical order, until fn returns false. The RRsets are shared.
func (z *Zone) ViewRRsets(fn func(group string, set *RRset) bool) {
	if z == nil {
		return
	}
	for i := range z.views {
		for j := range z.views[i].sets {
			if !fn(z.views[i].group, &z.views[i].sets[j]) {
				return
			}
		}
	}
}

// viewType reports why records of type t cannot be in a view, or "". Views
// carry leaf data only: a delegation or a DNAME in one view would change
// which names exist below it, and DNSSEC data is never served from a view.
func viewType(t uint16) string {
	switch t {
	case dns.TypeNS, dns.TypeDNAME:
		return "a client-group view cannot hold NS or DNAME records (D403)"
	case dns.TypeSOA, dns.TypeRRSIG, dns.TypeNSEC, dns.TypeNSEC3, dns.TypeNSEC3PARAM,
		dns.TypeDNSKEY, dns.TypeDS, dns.TypeCDS, dns.TypeCDNSKEY, dns.TypeSIG, dns.TypeKEY, dns.TypeNXT:
		return "a client-group view cannot hold " + typeString(t) + " records (D403)"
	}
	return ""
}

// validViewName reports why a view value is not "public" or a ClientGroup
// name, or "".
func validViewName(view string) string {
	if view == ViewPublic {
		return ""
	}
	if errs := validation.IsDNS1123Subdomain(view); len(errs) > 0 {
		return "must be \"public\" or a ClientGroup name: " + strings.Join(errs, "; ")
	}
	return ""
}

// signed reports whether the default data holds DNSSEC records or the zone
// asks for signing. Views are not served in such a zone (D403): a view
// would need its own signatures and denial chain.
func (z *Zone) signedData() bool {
	if z.hasSignatures {
		return true
	}
	signed := false
	z.tree.walk(func(n *node) bool {
		for i := range n.sets {
			switch n.sets[i].Type {
			case dns.TypeRRSIG, dns.TypeNSEC, dns.TypeNSEC3, dns.TypeNSEC3PARAM, dns.TypeDNSKEY, dns.TypeSIG:
				signed = true
				return false
			}
		}
		return true
	})
	return signed
}

// viewOcclusion reports why the default zone hides view data at key, or "":
// a DNAME at or above the apex-side ancestors, or a delegation at or above
// key below the apex.
func (z *Zone) viewOcclusion(key string) string {
	for i := len(key); i >= len(z.key); i-- {
		if i > 0 && i < len(key) && key[i-1] != keySep {
			continue
		}
		n := z.index[key[:i]]
		if n == nil {
			continue
		}
		if i < len(key) && n.get(dns.TypeDNAME) != nil {
			return fmt.Sprintf("%s: %s is below the DNAME at %s and would never be served (RFC 6672 §2.4)", v1alpha1.ReasonOccluded, nameFromKey(key), n.name)
		}
		if n != z.apex && n.get(dns.TypeNS) != nil {
			return fmt.Sprintf("%s: %s is at or below the delegation at %s and would never be served", v1alpha1.ReasonOccluded, nameFromKey(key), n.name)
		}
	}
	return ""
}

// viewName is the pending data of one owner name in one view.
type viewName struct {
	key, name string
	sets      []RRset
	// entries are the endpoints that put data here, for errors found when
	// the zone is finished.
	entries []viewEntry
}

type viewEntry struct {
	source string
	path   *field.Path
	value  string
	t      uint16
}

// pendingView is one group's data while a zone is being built.
type pendingView struct {
	group string
	names map[string]*viewName
}

func (p *pendingView) name(key, owner string) *viewName {
	n := p.names[key]
	if n == nil {
		n = &viewName{key: key, name: owner}
		p.names[key] = n
	}
	return n
}

func (n *viewName) get(t uint16) *RRset {
	for i := range n.sets {
		if n.sets[i].Type == t {
			return &n.sets[i]
		}
	}
	return nil
}

// addView adds one live endpoint of a client-group view. The checks that
// need only the view itself run here; occlusion by default data is checked
// when the zone is finished (finishViews).
func (b *Builder) addView(namespace, source string, path *field.Path, ep *v1alpha1.Endpoint, rrs []dns.RR, errs field.ErrorList) field.ErrorList {
	view := ep.Kudurru.View
	if why := validViewName(view); why != "" {
		return field.ErrorList{field.Invalid(path.Child("kudurru", "view"), view, why)}
	}
	if len(errs) > 0 {
		return errs
	}
	owner := rrs[0].Header().Name
	rrtype := rrs[0].Header().Rrtype
	key, _ := canonicalKey(owner) // validated by ParseEndpoint
	dnsName := path.Child("dnsName")
	if why := viewType(rrtype); why != "" {
		return field.ErrorList{field.Invalid(path.Child("recordType"), ep.RecordType, why)}
	}
	if key == b.z.key && rrtype == dns.TypeCNAME {
		return field.ErrorList{field.Invalid(dnsName, ep.DNSName, "a CNAME cannot be at the zone apex (RFC 1912 §2.4)")}
	}
	group := ViewGroupID(namespace, view)
	id := viewIdentity{identity{key, rrtype, ep.SetIdentifier}, group}
	if prev, ok := b.viewOwners[id]; ok && prev != source && !b.merge {
		return field.ErrorList{&field.Error{
			Type:     field.ErrorTypeDuplicate,
			Field:    dnsName.String(),
			BadValue: ep.DNSName,
			Detail:   fmt.Sprintf("%s: %s %s in view %s is already defined by %s", v1alpha1.ReasonDuplicated, owner, typeString(rrtype), group, prev),
		}}
	}
	p := b.views[group]
	if p == nil {
		if len(b.views) >= MaxViews {
			return field.ErrorList{field.Invalid(path.Child("kudurru", "view"), view, fmt.Sprintf("a zone serves at most %d client-group views (D403)", MaxViews))}
		}
		if b.views == nil {
			b.views = map[string]*pendingView{}
		}
		p = &pendingView{group: group, names: map[string]*viewName{}}
		b.views[group] = p
	}
	n := p.names[key]
	if n != nil {
		if rrtype == dns.TypeCNAME && n.get(dns.TypeCNAME) == nil {
			return field.ErrorList{field.Invalid(dnsName, ep.DNSName, fmt.Sprintf("%s: a CNAME cannot coexist with other records at the same name in view %s (RFC 1034 §3.6.2)", v1alpha1.ReasonInvalid, view))}
		}
		if rrtype != dns.TypeCNAME && n.get(dns.TypeCNAME) != nil {
			return field.ErrorList{field.Invalid(dnsName, ep.DNSName, fmt.Sprintf("%s: the name already has a CNAME in view %s; no other records may exist there (RFC 1034 §3.6.2)", v1alpha1.ReasonInvalid, view))}
		}
	}
	b.claimView(id, source)
	if n == nil {
		n = p.name(key, owner)
	}
	set := n.get(rrtype)
	if set == nil {
		n.sets = append(n.sets, RRset{Name: owner, Type: rrtype})
		set = &n.sets[len(n.sets)-1]
	}
	for _, rr := range rrs {
		rr.Header().Name = owner
		set.add(rr)
	}
	n.entries = append(n.entries, viewEntry{source: source, path: dnsName, value: ep.DNSName, t: rrtype})
	if rrtype == dns.TypeCNAME && len(set.RRs) > 1 {
		set.RRs = set.RRs[:1]
		set.TTL = set.RRs[0].Header().Ttl
		return field.ErrorList{field.Invalid(path.Child("targets"), ep.Targets, "a name has at most one CNAME target; this one was dropped")}
	}
	if k := ep.Kudurru; k.ExpiresAt != nil {
		if t := k.ExpiresAt.Time; b.z.nextExpiry.IsZero() || t.Before(b.z.nextExpiry) {
			b.z.nextExpiry = t
		}
	}
	return nil
}

// claimView records the first source of a view identity.
func (b *Builder) claimView(id viewIdentity, source string) {
	if b.viewOwners == nil {
		b.viewOwners = map[viewIdentity]string{}
	}
	if _, ok := b.viewOwners[id]; !ok {
		b.viewOwners[id] = source
	}
}

// finishViews builds the variants of the finished default zone b.z from
// the pending views. Endpoints the default data hides, and every view of a
// signed zone, are refused and reported in ViewErrors.
func (b *Builder) finishViews() {
	if len(b.views) == 0 {
		return
	}
	refuseEntry := func(e viewEntry, why string) {
		if b.viewErrs == nil {
			b.viewErrs = map[string]field.ErrorList{}
		}
		b.viewErrs[e.source] = append(b.viewErrs[e.source], field.Invalid(e.path, e.value, why))
	}
	refuse := func(n *viewName, why string) {
		for _, e := range n.entries {
			refuseEntry(e, why)
		}
	}
	owners := b.viewDefaultOwners()
	groups := make([]string, 0, len(b.views))
	for g := range b.views {
		groups = append(groups, g)
	}
	slices.Sort(groups)
	signed := b.z.signedData() || (b.spec.DNSSEC != nil && b.spec.DNSSEC.Enabled)
	var views []clientView
	for _, g := range groups {
		p := b.views[g]
		names := make([]*viewName, 0, len(p.names))
		for _, n := range p.names {
			names = append(names, n)
		}
		slices.SortFunc(names, func(x, y *viewName) int { return strings.Compare(x.key, y.key) })
		kept := names[:0]
		for _, n := range names {
			why := b.z.viewOcclusion(n.key)
			if why == "" && b.viewFence != nil {
				why = b.viewFence(n.name)
			}
			switch {
			case signed:
				refuse(n, v1alpha1.ReasonInvalid+": client-group views are not served in a DNSSEC-signed zone (D403)")
			case why != "":
				refuse(n, why)
			case b.ownSets(n, owners[n.key], refuseEntry):
				kept = append(kept, n)
			}
		}
		if len(kept) == 0 {
			continue
		}
		for _, n := range kept {
			for _, e := range n.entries {
				if n.get(e.t) == nil {
					continue // refused by ownSets
				}
				if b.viewServed == nil {
					b.viewServed = map[string]bool{}
				}
				b.viewServed[e.source] = true
			}
		}
		views = append(views, b.z.buildView(g, kept))
	}
	b.z.setViews(views)
}

// defaultOwner is a source that claimed default data of one type at a name.
type defaultOwner struct {
	t      uint16
	source string
}

// viewDefaultOwners returns, for every owner name the views use, the
// sources that claimed default data there (served or not, as Duplicated
// counts them), sorted by source and type. It scans the default identities
// once, and only when the zone has views.
func (b *Builder) viewDefaultOwners() map[string][]defaultOwner {
	keys := map[string]bool{}
	for _, p := range b.views {
		for k := range p.names {
			keys[k] = true
		}
	}
	out := map[string][]defaultOwner{}
	for id, source := range b.owners {
		if keys[id.key] {
			out[id.key] = append(out[id.key], defaultOwner{id.t, source})
		}
	}
	for _, list := range out {
		slices.SortFunc(list, func(x, y defaultOwner) int {
			if c := strings.Compare(x.source, y.source); c != 0 {
				return c
			}
			return int(x.t) - int(y.t)
		})
	}
	return out
}

// sourceScope is the namespace part of a source: everything before its
// last "/" ("namespace/name" in Build, "DNSRecordSet/namespace/name" in the
// committed primary compiler).
func sourceScope(source string) string {
	if i := strings.LastIndexByte(source, '/'); i >= 0 {
		return source[:i]
	}
	return ""
}

// ownSets drops from n every view RRset that would replace default data
// another namespace claimed, refusing its endpoints as Duplicated, and
// reports whether any RRset is left. A view RRset replaces the default
// RRset of its type, and a CNAME on either side replaces everything at the
// name (buildView). Ownership of served names is per namespace: a view may
// replace default data of its own namespace, never another's, because the
// record set and the ClientGroup that would override it are both written in
// the overriding namespace (D403).
func (b *Builder) ownSets(n *viewName, owners []defaultOwner, refuse func(viewEntry, string)) bool {
	if len(owners) == 0 {
		return true
	}
	kept := n.sets[:0]
	for _, s := range n.sets {
		var foreign *defaultOwner
		for _, e := range n.entries {
			if e.t != s.Type {
				continue
			}
			for i := range owners {
				o := &owners[i]
				if (o.t == s.Type || s.Type == dns.TypeCNAME || o.t == dns.TypeCNAME) && sourceScope(o.source) != sourceScope(e.source) {
					foreign = o
					break
				}
			}
			if foreign != nil {
				break
			}
		}
		if foreign == nil {
			kept = append(kept, s)
			continue
		}
		why := fmt.Sprintf("%s: %s %s is already defined by %s; a client-group view replaces only default data of its own namespace (D403)", v1alpha1.ReasonDuplicated, n.name, typeString(foreign.t), foreign.source)
		for _, e := range n.entries {
			if e.t == s.Type {
				refuse(e, why)
			}
		}
	}
	n.sets = kept
	return len(kept) > 0
}

// FenceViews makes Build refuse view data that records the builder does
// not hold would hide once they are merged into the zone: on SQLite the
// dynamic (RFC 2136) records of the committed primary join the static
// records only after the compiler, so a delegation or DNSSEC data an UPDATE
// added must refuse a view endpoint here, as default data does, instead of
// failing the committed build of the whole zone (D373, D403). fence gets
// the view's owner name and returns the refusal ("Occluded: ..." or
// "Invalid: ...") or "". Call it before Build.
func (b *Builder) FenceViews(fence func(owner string) string) { b.viewFence = fence }

// ViewErrors returns the view endpoints Build refused, by source: data the
// default zone hides (below a DNAME or a delegation) and every view of a
// DNSSEC-signed zone. Valid after Build.
func (b *Builder) ViewErrors() map[string]field.ErrorList { return b.viewErrs }

// ViewServed reports whether Build serves at least one view endpoint of
// source. Valid after Build.
func (b *Builder) ViewServed(source string) bool { return b.viewServed[source] }

// buildView copies the finished default zone and lays the view's names
// over the copy. The RRs are shared with the default zone; nodes and RRset
// slices are the variant's own.
func (z *Zone) buildView(group string, names []*viewName) clientView {
	v := *z
	v.views, v.base = nil, z
	v.tree, v.index, v.records = tree{}, nil, 0
	z.tree.walk(func(n *node) bool {
		m := v.tree.upsert(n.key, n.name)
		m.sets = slices.Clone(n.sets)
		return true
	})
	v.apex = v.tree.search(z.key)
	vb := &Builder{z: &v}
	var overlay []RRset
	for _, n := range names {
		m := v.tree.search(n.key)
		if m == nil {
			m = vb.insertName(n.key, n.name)
		}
		for i := range n.sets {
			s := n.sets[i]
			s.Name = m.name
			s.normalise()
			m.sets = slices.DeleteFunc(m.sets, func(have RRset) bool {
				return have.Type == s.Type || s.Type == dns.TypeCNAME || have.Type == dns.TypeCNAME
			})
			m.sets = append(m.sets, s)
			overlay = append(overlay, s)
		}
		sortSets(m.sets)
	}
	sortSets(overlay) // stable by type within a name; names are already in key order
	slices.SortStableFunc(overlay, func(x, y RRset) int {
		kx, _ := canonicalKey(x.Name)
		ky, _ := canonicalKey(y.Name)
		return strings.Compare(kx, ky)
	})
	v.index = make(map[string]*node, v.tree.count)
	v.tree.walk(func(n *node) bool {
		v.index[n.key] = n
		for i := range n.sets {
			v.records += len(n.sets[i].RRs)
		}
		return true
	})
	v.records-- // the SOA
	return clientView{group: group, zone: &v, sets: overlay}
}

// setViews installs built variants and folds their data into the content
// hash, so a view change changes the hash (and the serial derived from it)
// like any other served change. A zone without views keeps its hash.
func (z *Zone) setViews(views []clientView) {
	if len(views) == 0 {
		return
	}
	z.views = views
	z.hash = z.servedHash()
	for i := range z.views {
		z.views[i].zone.hash = z.hash
	}
}

// servedHash recomputes the content hash from the zone's data: contentHash
// of the default data, then each view's own RRsets in group order.
func (z *Zone) servedHash() string {
	base := contentHash(z)
	if len(z.views) == 0 {
		return base
	}
	h := sha256.New()
	h.Write([]byte(base))
	var buf [10]byte
	for _, v := range z.views {
		h.Write([]byte("\x00view\x00" + v.group + "\x00"))
		for i := range v.sets {
			k, _ := canonicalKey(v.sets[i].Name)
			for _, rr := range v.sets[i].RRs {
				writeRR(h, &buf, k, rr)
			}
		}
	}
	return HashPrefix + hex.EncodeToString(h.Sum(nil))
}

// CommittedView is the committed data of one client-group view.
type CommittedView struct {
	Group   string
	Records []dns.RR
}

// addCommittedViews builds the variants of a finished committed primary.
// The data was admitted by the compiler; anything a view cannot hold is a
// structural error here.
func (z *Zone) addCommittedViews(views []CommittedView) error {
	if len(views) == 0 {
		return nil
	}
	if len(views) > MaxViews {
		return fmt.Errorf("committed primary has %d client-group views, more than %d", len(views), MaxViews)
	}
	if z.signedData() {
		return fmt.Errorf("client-group views are not served in a DNSSEC-signed zone")
	}
	var built []clientView
	for i, cv := range views {
		if cv.Group == "" || (i > 0 && cv.Group <= views[i-1].Group) || len(cv.Records) == 0 {
			return fmt.Errorf("committed client-group views must be nonempty and sorted by group")
		}
		p := &pendingView{group: cv.Group, names: map[string]*viewName{}}
		for _, original := range cv.Records {
			rr, _, err := copySecondaryRR(original)
			if err != nil {
				return fmt.Errorf("view %s: %w", cv.Group, err)
			}
			h := rr.Header()
			key, err := canonicalKey(h.Name)
			if err != nil || !dns.IsFqdn(h.Name) || !isKeyPrefix(key, z.key) || h.Class != dns.ClassINET || secondaryPseudoType(h.Rrtype) {
				return fmt.Errorf("view %s: invalid owner/class/type", cv.Group)
			}
			if why := viewType(h.Rrtype); why != "" {
				return fmt.Errorf("view %s: %s", cv.Group, why)
			}
			if why := z.viewOcclusion(key); why != "" {
				return fmt.Errorf("view %s: %s", cv.Group, why)
			}
			n := p.name(key, nameFromKey(key))
			set := n.get(h.Rrtype)
			if set == nil {
				n.sets = append(n.sets, RRset{Name: n.name, Type: h.Rrtype})
				set = &n.sets[len(n.sets)-1]
			}
			if !set.add(rr) {
				return fmt.Errorf("view %s: duplicate RR", cv.Group)
			}
		}
		names := make([]*viewName, 0, len(p.names))
		for _, n := range p.names {
			if c := n.get(dns.TypeCNAME); c != nil && (len(c.RRs) != 1 || len(n.sets) != 1 || n.key == z.key) {
				return fmt.Errorf("view %s: %s: invalid CNAME RRset", cv.Group, n.name)
			}
			names = append(names, n)
		}
		slices.SortFunc(names, func(x, y *viewName) int { return strings.Compare(x.key, y.key) })
		built = append(built, z.buildView(cv.Group, names))
	}
	z.setViews(built)
	return nil
}
