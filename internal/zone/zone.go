package zone

import (
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/miekg/dns"
	"k8s.io/apimachinery/pkg/util/validation/field"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// Defaults applied when a DNSZone leaves a field empty. They match the CRD
// defaults so a zone built from an object that skipped defaulting (tests,
// imports) behaves the same.
const (
	DefaultTTL           = 300
	DefaultRefresh       = 3600
	DefaultRetry         = 900
	DefaultExpire        = 1209600
	DefaultMinimum       = 300
	DefaultUpstreamGroup = "default"
)

// Zone is an immutable, fully built zone. All methods are safe for
// concurrent use; a Zone is never modified after Build returns, so readers
// need no locks. Changes produce a new Zone that is swapped in through a
// Snapshot.
type Zone struct {
	name     string // lowercased absolute origin
	key      string
	typ      v1alpha1.ZoneType
	serial   uint32
	upstream string // forward zones
	access   accessPolicy
	transfer accessPolicy
	// transferKeys are the canonical names of spec.policy.transferTSIGKeyRefs.
	transferKeys []string
	notify       v1alpha1.NotifyMode
	notifyTo     []netip.AddrPort

	tree tree
	// index maps every key in tree to its node. The tree gives canonical
	// order (RRsets, export, the content hash, NSEC in v1.0); the index
	// makes the exact-match probes of Lookup one hash lookup instead of a
	// ~17-level pointer chase through a 100k-name tree (docs/m1/zone.md).
	index  map[string]*node
	apex   *node
	soa    *dns.SOA // served SOA (status.serial)
	negSOA *dns.SOA // SOA for negative answers: TTL = min(TTL, minimum)
	negNs  []dns.RR // authority section of negative answers: {negSOA}

	// cuts is set when a name below the apex owns NS or DNAME records.
	// Without them no ancestor can redirect a lookup, so Lookup may search
	// the full query name first and skip the label-by-label walk.
	cuts bool

	hash       string
	records    int
	nextExpiry time.Time
	generation int64
}

// Name returns the lowercased absolute zone name.
func (z *Zone) Name() string { return z.name }

// Type returns the zone type (primary or forward in v0.1).
func (z *Zone) Type() v1alpha1.ZoneType { return z.typ }

// Serial returns the SOA serial served for the zone (status.serial).
func (z *Zone) Serial() uint32 { return z.serial }

// Generation returns metadata.generation of the DNSZone the zone was built
// from.
func (z *Zone) Generation() int64 { return z.generation }

// UpstreamGroup returns the upstream group a forward zone sends queries to;
// empty for other zone types.
func (z *Zone) UpstreamGroup() string { return z.upstream }

// ContentHash returns the canonical content hash ("sha256:<hex>") the
// controller compares with status.contentHash to decide on a serial bump.
// It covers every served RR including the SOA fields, except the serial.
func (z *Zone) ContentHash() string { return z.hash }

// RecordCount returns the number of served RRs excluding the SOA.
func (z *Zone) RecordCount() int { return z.records }

// Names returns the number of owner names in the tree, including empty
// non-terminals.
func (z *Zone) Names() int { return z.tree.count }

// NextExpiry returns the earliest expiresAt among the records that were
// still live at build time, or the zero time. The zone cache rebuilds the
// zone at that instant so expired records leave service on time.
func (z *Zone) NextExpiry() time.Time { return z.nextExpiry }

// SOA returns a copy of the zone's SOA record, or nil for forward zones.
func (z *Zone) SOA() *dns.SOA {
	if z.soa == nil {
		return nil
	}
	return dns.Copy(z.soa).(*dns.SOA)
}

// Contains reports whether name is at or below the zone apex.
func (z *Zone) Contains(name string) bool {
	k, err := canonicalKey(name)
	return err == nil && isKeyPrefix(k, z.key)
}

// QueryAllowed applies spec.policy.queryAccess and queryACL to a client
// address (docs/architecture.md §7.1 step 6).
func (z *Zone) QueryAllowed(client netip.Addr) bool { return z.access.allowed(client) }

// TransferAllowed applies spec.policy.zoneTransfer, transferACL and
// transferTSIGKeyRefs to a transfer request (AXFR or IXFR out). The access
// mode decides on the client address exactly as queryAccess does (an empty
// mode is deny); when transferTSIGKeyRefs is non-empty the request must
// also have been signed with one of those keys. tsigKey is the verified
// key name ("" for an unsigned request), compared case-insensitively.
// Forward zones are never transferable.
func (z *Zone) TransferAllowed(client netip.Addr, tsigKey string) bool {
	if z.typ != v1alpha1.ZoneTypePrimary || !z.transfer.allowed(client) {
		return false
	}
	if len(z.transferKeys) == 0 {
		return true
	}
	return tsigKey != "" && slices.Contains(z.transferKeys, KeyName(tsigKey))
}

// TransferKeys returns the canonical names of spec.policy.transferTSIGKeyRefs
// in spec order. The first one also signs outbound NOTIFY messages.
func (z *Zone) TransferKeys() []string { return slices.Clone(z.transferKeys) }

// NotifyMode returns spec.policy.notify, "none" when unset.
func (z *Zone) NotifyMode() v1alpha1.NotifyMode { return z.notify }

// NotifyTargets returns spec.policy.notifyTargets as addresses with the
// port defaulted to 53.
func (z *Zone) NotifyTargets() []netip.AddrPort { return slices.Clone(z.notifyTo) }

// NameServer is one apex NS host and the in-zone addresses (glue) the zone
// itself publishes for it.
type NameServer struct {
	Host  string
	Addrs []netip.Addr
}

// NameServers returns the apex NS hosts in canonical order with their
// in-zone A/AAAA addresses. Hosts outside the zone have no addresses here;
// the caller resolves them.
func (z *Zone) NameServers() []NameServer {
	if z.apex == nil {
		return nil
	}
	ns := z.apex.get(dns.TypeNS)
	if ns == nil {
		return nil
	}
	out := make([]NameServer, 0, len(ns.RRs))
	for _, rr := range ns.RRs {
		host := rr.(*dns.NS).Ns
		out = append(out, NameServer{Host: host, Addrs: z.hostAddrs(host)})
	}
	return out
}

// RRsets calls fn for every RRset in canonical order (owner name, then
// type), the SOA first. Iteration stops when fn returns false. The RRsets
// are shared; fn must not modify them.
func (z *Zone) RRsets(fn func(*RRset) bool) {
	if z.soa == nil {
		return
	}
	if !fn(z.apex.get(dns.TypeSOA)) {
		return
	}
	z.tree.walk(func(n *node) bool {
		for i := range n.sets {
			if n == z.apex && n.sets[i].Type == dns.TypeSOA {
				continue
			}
			if !fn(&n.sets[i]) {
				return false
			}
		}
		return true
	})
}

// BuildOptions tune Build.
type BuildOptions struct {
	// Now is the instant expiresAt is compared with; zero means time.Now().
	Now time.Time
	// MergeUnion merges endpoints of the same name and type from different
	// record sets instead of rejecting the later one as Duplicated
	// (mergeStrategy: union, §4.2).
	MergeUnion bool
}

// Builder accumulates endpoints for one zone. It is not safe for
// concurrent use. Feed record sets in a stable order (creation time, then
// namespace/name) so that "Duplicated on the later object" is
// deterministic.
type Builder struct {
	z       *Zone
	spec    v1alpha1.DNSZoneSpec
	now     time.Time
	merge   bool
	nsNames []string
	// owners maps an endpoint identity to the source that claimed it
	// first. Endpoints that are not served (disabled, expired, view-scoped)
	// still claim their identity, so toggling one never moves ownership
	// (the controller's rule, docs/m1/controller.md).
	owners map[identity]string
	// nameSource maps an owner key to the first source that put data at
	// that name; it decides whether a CNAME conflict is Duplicated (another
	// object) or Invalid (the same object).
	nameSource map[string]string
	// dnames counts the DNAME RRsets added so far; zero skips the
	// occlusion checks.
	dnames int
	// policyErrs are the non-fatal spec.policy errors (PolicyErrors).
	policyErrs field.ErrorList
}

// identity is the duplicate-detection key of an endpoint within a zone:
// owner, type and the external-dns set identifier. Endpoints with
// different set identifiers are distinct (kudurru has no routing policies,
// so their records are served as one RRset); the same identity from a
// later source is Duplicated.
type identity struct {
	key   string
	t     uint16
	setID string
}

// NewBuilder validates the DNSZone and returns a Builder for its records.
// The errors are field paths into the DNSZone (spec.zoneName,
// spec.policy.queryACL[2], ...); when there are any the zone cannot be
// built and the Builder is nil. Zone types other than primary and forward
// are rejected until their release (secondary, stub and external in v0.2).
//
// Errors in the transfer and notify parts of spec.policy do not stop the
// zone from being served; they fail closed instead (transfers denied, a
// bad notify mode is none, bad notify targets are dropped) and are
// reported by Builder.PolicyErrors and Errors.Policy.
func NewBuilder(dz *v1alpha1.DNSZone, opts BuildOptions) (*Builder, field.ErrorList) {
	spec := dz.Spec
	specPath := field.NewPath("spec")
	var errs field.ErrorList

	origin := fqdn(spec.ZoneName)
	key, err := canonicalKey(origin)
	if _, ok := dns.IsDomainName(origin); err != nil || !ok {
		errs = append(errs, field.Invalid(specPath.Child("zoneName"), spec.ZoneName, "must be a domain name"))
	}
	switch spec.Type {
	case v1alpha1.ZoneTypePrimary, v1alpha1.ZoneTypeForward:
	default:
		errs = append(errs, field.NotSupported(specPath.Child("type"), spec.Type,
			[]v1alpha1.ZoneType{v1alpha1.ZoneTypePrimary, v1alpha1.ZoneTypeForward}))
	}
	policyPath := specPath.Child("policy")
	access, aerrs := compileAccess(spec.Policy, policyPath)
	errs = append(errs, aerrs...)
	transfer, terrs := compileTransfer(spec.Policy, policyPath)
	keys, kerrs := compileKeyRefs(spec.Policy.TransferTSIGKeyRefs, policyPath.Child("transferTSIGKeyRefs"))
	notify, targets, nerrs := compileNotify(spec.Policy, policyPath)
	var policyErrs field.ErrorList
	if len(terrs) > 0 || len(kerrs) > 0 {
		// Fail closed: a transfer policy that does not compile allows
		// nothing.
		transfer = accessPolicy{mode: v1alpha1.AccessDeny}
		policyErrs = append(append(policyErrs, terrs...), kerrs...)
	}
	policyErrs = append(policyErrs, nerrs...)

	var nsNames []string
	for i, ns := range spec.Nameservers {
		n, err := hostName(ns)
		if err != nil {
			errs = append(errs, field.Invalid(specPath.Child("nameservers").Index(i), ns, err.Error()))
			continue
		}
		nsNames = append(nsNames, n)
	}
	for i, a := range dz.Status.NameserverAddresses {
		ip, err := netip.ParseAddr(a)
		if err != nil {
			errs = append(errs, field.Invalid(field.NewPath("status", "nameserverAddresses").Index(i), a, "must be an IP address"))
			continue
		}
		access.nsAddrs = append(access.nsAddrs, ip.Unmap())
		transfer.nsAddrs = append(transfer.nsAddrs, ip.Unmap())
	}
	if len(errs) > 0 {
		return nil, errs
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	z := &Zone{
		name:         nameFromKey(key),
		key:          key,
		typ:          spec.Type,
		serial:       dz.Status.Serial,
		access:       access,
		transfer:     transfer,
		transferKeys: keys,
		notify:       notify,
		notifyTo:     targets,
		generation:   dz.Generation,
	}
	if spec.Type == v1alpha1.ZoneTypeForward {
		z.upstream = DefaultUpstreamGroup
		if spec.Forward != nil && spec.Forward.UpstreamGroup != "" {
			z.upstream = spec.Forward.UpstreamGroup
		}
	} else {
		z.apex = z.tree.upsert(key, z.name)
	}
	return &Builder{z: z, spec: spec, now: now, merge: opts.MergeUnion, nsNames: nsNames,
		owners: map[identity]string{}, nameSource: map[string]string{}, policyErrs: policyErrs}, nil
}

// PolicyErrors returns the errors in the transfer and notify parts of
// spec.policy. The zone is still built and served; the parts in error are
// disabled (see NewBuilder).
func (b *Builder) PolicyErrors() field.ErrorList { return b.policyErrs }

// hostName validates a host name used in the zone spec (nameservers, SOA
// mname/rname). Names are absolute with or without a trailing dot.
func hostName(s string) (string, error) {
	n := fqdn(s)
	if n == "." {
		return "", fmt.Errorf("must not be the root")
	}
	if _, ok := dns.IsDomainName(n); !ok {
		return "", errBadName
	}
	if _, err := canonicalKey(n); err != nil {
		return "", err
	}
	return n, nil
}

func (b *Builder) defaultTTL() uint32 {
	if b.spec.DefaultTTL > 0 {
		return b.spec.DefaultTTL
	}
	return DefaultTTL
}

// Served reports whether an endpoint is served at instant now: disabled
// records, expired records and records scoped to a view are not. Views are
// a v0.2 feature; until the rewrite stage can answer per client group an
// endpoint with any view is withheld rather than leaked to every client
// ("public" endpoints are never served by design, D29).
func Served(ep *v1alpha1.Endpoint, now time.Time) bool {
	k := ep.Kudurru
	if k == nil {
		return true
	}
	if k.Disabled || k.View != "" {
		return false
	}
	if k.ExpiresAt != nil && !now.Before(k.ExpiresAt.Time) {
		return false
	}
	return true
}

// AddRecordSet adds every endpoint of a DNSRecordSet. source identifies the
// object in Duplicated messages ("namespace/name"); errors carry paths of
// the form spec.endpoints[i].targets[j].
func (b *Builder) AddRecordSet(source string, eps []v1alpha1.Endpoint) field.ErrorList {
	var errs field.ErrorList
	base := field.NewPath("spec", "endpoints")
	for i := range eps {
		errs = append(errs, b.Add(source, base.Index(i), &eps[i])...)
	}
	return errs
}

// Add adds one endpoint. An endpoint is all-or-nothing: when any target
// fails to parse, or the endpoint conflicts with data already in the zone,
// none of its records are added and the errors explain why. Disabled,
// expired and view-scoped endpoints are skipped silently (Served).
func (b *Builder) Add(source string, path *field.Path, ep *v1alpha1.Endpoint) field.ErrorList {
	if b.z.typ != v1alpha1.ZoneTypePrimary {
		return field.ErrorList{field.Forbidden(path, fmt.Sprintf("%s zones hold no records", b.z.typ))}
	}
	if !Served(ep, b.now) {
		b.claim(source, ep)
		return nil
	}
	rrs, errs := ParseEndpoint(b.z.name, b.defaultTTL(), ep, path)
	if len(errs) > 0 {
		return errs
	}
	owner := rrs[0].Header().Name
	rrtype := rrs[0].Header().Rrtype
	key, _ := canonicalKey(owner) // validated by ParseEndpoint
	dnsName := path.Child("dnsName")

	if key == b.z.key {
		switch rrtype {
		case dns.TypeNS:
			return field.ErrorList{field.Invalid(dnsName, ep.DNSName, "apex NS records come from the DNSZone spec.nameservers")}
		case dns.TypeCNAME:
			return field.ErrorList{field.Invalid(dnsName, ep.DNSName, "a CNAME cannot be at the zone apex (RFC 1912 §2.4)")}
		}
	}
	id := identity{key, rrtype, ep.SetIdentifier}
	if prev, ok := b.owners[id]; ok && prev != source && !b.merge {
		return field.ErrorList{&field.Error{
			Type:     field.ErrorTypeDuplicate,
			Field:    dnsName.String(),
			BadValue: ep.DNSName,
			Detail:   fmt.Sprintf("%s: %s %s is already defined by %s", v1alpha1.ReasonDuplicated, owner, typeString(rrtype), prev),
		}}
	}
	n := b.z.tree.search(key)
	if n != nil {
		if rrtype == dns.TypeCNAME && !n.empty() && n.get(dns.TypeCNAME) == nil {
			return field.ErrorList{b.conflict(dnsName, ep.DNSName, key, source, "a CNAME cannot coexist with other records at the same name (RFC 1034 §3.6.2)")}
		}
		if rrtype != dns.TypeCNAME && n.get(dns.TypeCNAME) != nil {
			return field.ErrorList{b.conflict(dnsName, ep.DNSName, key, source, "the name already has a CNAME; no other records may exist there (RFC 1034 §3.6.2)")}
		}
	}
	if err := b.occlusion(dnsName, ep.DNSName, key, rrtype); err != nil {
		return field.ErrorList{err}
	}
	if _, ok := b.owners[id]; !ok {
		b.owners[id] = source
	}
	if _, ok := b.nameSource[key]; !ok {
		b.nameSource[key] = source
	}
	if n == nil {
		n = b.insertName(key, owner)
	}
	set := n.get(rrtype)
	if set == nil {
		n.sets = append(n.sets, RRset{Name: n.name, Type: rrtype})
		set = &n.sets[len(n.sets)-1]
		if rrtype == dns.TypeDNAME {
			b.dnames++
		}
	}
	for _, rr := range rrs {
		rr.Header().Name = n.name
		set.add(rr)
	}
	if rrtype == dns.TypeCNAME && len(set.RRs) > 1 {
		// Two sources merged under MergeUnion with different targets.
		set.RRs = set.RRs[:1]
		return field.ErrorList{field.Invalid(path.Child("targets"), ep.Targets, "a name has at most one CNAME target; this one was dropped")}
	}
	if k := ep.Kudurru; k != nil && k.ExpiresAt != nil {
		if t := k.ExpiresAt.Time; b.z.nextExpiry.IsZero() || t.Before(b.z.nextExpiry) {
			b.z.nextExpiry = t
		}
	}
	return nil
}

// insertName inserts owner and every missing ancestor below the apex as an
// empty non-terminal.
func (b *Builder) insertName(key, owner string) *node {
	n := b.z.tree.upsert(key, owner)
	name := owner
	k := key
	for {
		// Strip the leftmost label: drop the last label of the key.
		i := len(k) - 2
		for i >= 0 && k[i] != keySep {
			i--
		}
		k = k[:i+1]
		off, end := dns.NextLabel(name, 0)
		if end {
			break
		}
		name = name[off:]
		if len(k) <= len(b.z.key) || b.z.tree.search(k) != nil {
			break
		}
		b.z.tree.upsert(k, name)
	}
	return n
}

// Build finishes the zone: it synthesises the SOA and apex NS from the
// DNSZone, normalises every RRset (one TTL, canonical order) and computes
// the content hash. The Builder must not be used afterwards.
func (b *Builder) Build() *Zone {
	z := b.z
	if z.typ != v1alpha1.ZoneTypePrimary {
		z.hash = hashForward(z)
		return z
	}
	ttl := b.defaultTTL()
	soaSpec := v1alpha1.SOASpec{}
	if b.spec.SOA != nil {
		soaSpec = *b.spec.SOA
	}
	mname := childName("ns1", z.name)
	if len(b.nsNames) > 0 {
		mname = b.nsNames[0]
	}
	if soaSpec.MName != "" {
		if n, err := hostName(soaSpec.MName); err == nil {
			mname = n
		}
	}
	rname := childName("hostmaster", z.name)
	if soaSpec.RName != "" {
		if n, err := mailbox(soaSpec.RName); err == nil {
			rname = n
		}
	}
	z.soa = &dns.SOA{
		Hdr:     dns.RR_Header{Name: z.name, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: ttl},
		Ns:      mname,
		Mbox:    rname,
		Serial:  z.serial,
		Refresh: orDefault(soaSpec.Refresh, DefaultRefresh),
		Retry:   orDefault(soaSpec.Retry, DefaultRetry),
		Expire:  orDefault(soaSpec.Expire, DefaultExpire),
		Minttl:  orDefault(soaSpec.Minimum, DefaultMinimum),
	}
	neg := dns.Copy(z.soa).(*dns.SOA)
	neg.Hdr.Ttl = min(neg.Hdr.Ttl, neg.Minttl)
	z.negSOA = neg
	z.negNs = []dns.RR{neg}
	z.apex.sets = append(z.apex.sets, RRset{Name: z.name, Type: dns.TypeSOA, TTL: ttl, RRs: []dns.RR{z.soa}})

	if len(b.nsNames) > 0 {
		set := RRset{Name: z.name, Type: dns.TypeNS, TTL: ttl}
		for _, ns := range b.nsNames {
			set.add(&dns.NS{Hdr: dns.RR_Header{Name: z.name, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: ttl}, Ns: ns})
		}
		z.apex.sets = append(z.apex.sets, set)
	}

	z.tree.walk(func(n *node) bool {
		sortSets(n.sets)
		for i := range n.sets {
			n.sets[i].normalise()
			z.records += len(n.sets[i].RRs)
		}
		if n != z.apex && (n.get(dns.TypeNS) != nil || n.get(dns.TypeDNAME) != nil) {
			z.cuts = true
		}
		return true
	})
	z.records-- // the SOA
	z.index = make(map[string]*node, z.tree.count)
	z.tree.walk(func(n *node) bool {
		z.index[n.key] = n
		return true
	})
	nsAddrs := z.nameserverAddrs()
	z.access.nsAddrs = append(z.access.nsAddrs, nsAddrs...)
	z.transfer.nsAddrs = append(z.transfer.nsAddrs, nsAddrs...)
	z.hash = contentHash(z)
	return z
}

func orDefault(v, d uint32) uint32 {
	if v == 0 {
		return d
	}
	return v
}

// mailbox converts an SOA rname given as an e-mail address
// (hostmaster@example.com) or as a domain name into SOA RNAME form.
func mailbox(s string) (string, error) {
	// An escaped \@ is part of a label (a presentation-form RNAME as an
	// import stores it), not the e-mail separator.
	if i := lastUnescaped(s, '@'); i >= 0 {
		local, domain := s[:i], s[i+1:]
		var esc []byte
		for i := 0; i < len(local); i++ {
			if local[i] == '.' {
				esc = append(esc, '\\')
			}
			esc = append(esc, local[i])
		}
		s = string(esc) + "." + domain
	}
	return hostName(s)
}

// lastUnescaped returns the index of the last c in s that is not part of a
// presentation escape (\c, \DDD), or -1.
func lastUnescaped(s string, c byte) int {
	last := -1
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			if i+3 < len(s) && isDigit(s[i+1]) && isDigit(s[i+2]) && isDigit(s[i+3]) {
				i += 3
			} else {
				i++
			}
			continue
		}
		if s[i] == c {
			last = i
		}
	}
	return last
}

// childName returns label.parent.
func childName(label, parent string) string {
	if parent == "." {
		return label + "."
	}
	return label + "." + parent
}

// sortSets orders the RRsets of one name by type.
func sortSets(sets []RRset) {
	for i := 1; i < len(sets); i++ {
		for j := i; j > 0 && sets[j].Type < sets[j-1].Type; j-- {
			sets[j], sets[j-1] = sets[j-1], sets[j]
		}
	}
}

// nameserverAddrs returns the in-zone addresses of the apex nameservers,
// used by the allowOnlyZoneNameServers access modes.
func (z *Zone) nameserverAddrs() []netip.Addr {
	var out []netip.Addr
	ns := z.apex.get(dns.TypeNS)
	if ns == nil {
		return nil
	}
	for _, rr := range ns.RRs {
		out = append(out, z.hostAddrs(rr.(*dns.NS).Ns)...)
	}
	return out
}

// hostAddrs returns the A and AAAA addresses the zone holds for host.
func (z *Zone) hostAddrs(host string) []netip.Addr {
	k, err := canonicalKey(host)
	if err != nil {
		return nil
	}
	n := z.tree.search(k)
	if n == nil {
		return nil
	}
	var out []netip.Addr
	for _, t := range []uint16{dns.TypeA, dns.TypeAAAA} {
		if s := n.get(t); s != nil {
			for _, a := range s.RRs {
				if ip, ok := addrOf(a); ok {
					out = append(out, ip)
				}
			}
		}
	}
	return out
}

func addrOf(rr dns.RR) (netip.Addr, bool) {
	switch r := rr.(type) {
	case *dns.A:
		ip, ok := netip.AddrFromSlice(r.A.To4())
		return ip, ok
	case *dns.AAAA:
		ip, ok := netip.AddrFromSlice(r.AAAA.To16())
		return ip.Unmap(), ok
	}
	return netip.Addr{}, false
}

// Errors collects build errors per object: the DNSZone itself and every
// DNSRecordSet by "namespace/name".
type Errors struct {
	Zone       field.ErrorList
	RecordSets map[string]field.ErrorList
	// Policy holds errors in the transfer and notify parts of spec.policy.
	// They do not stop the zone from being served (Builder.PolicyErrors).
	Policy field.ErrorList
}

// Empty reports whether there are no errors at all.
func (e Errors) Empty() bool {
	return len(e.Zone) == 0 && len(e.RecordSets) == 0 && len(e.Policy) == 0
}

// Build is the one-shot form of NewBuilder, AddRecordSet and Build. It
// returns a nil Zone only when the DNSZone itself is invalid; endpoint
// errors leave the offending endpoints out and are reported per record set.
// Record sets are added in the order given.
func Build(dz *v1alpha1.DNSZone, sets []*v1alpha1.DNSRecordSet, opts BuildOptions) (*Zone, Errors) {
	var out Errors
	b, errs := NewBuilder(dz, opts)
	if len(errs) > 0 {
		out.Zone = errs
		return nil, out
	}
	out.Policy = b.PolicyErrors()
	for _, rs := range sets {
		src := rs.Namespace + "/" + rs.Name
		if errs := b.AddRecordSet(src, rs.Spec.Endpoints); len(errs) > 0 {
			if out.RecordSets == nil {
				out.RecordSets = map[string]field.ErrorList{}
			}
			out.RecordSets[src] = append(out.RecordSets[src], errs...)
		}
	}
	return b.Build(), out
}
