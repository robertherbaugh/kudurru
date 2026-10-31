package policy

import (
	"cmp"
	"fmt"
	"net"
	"net/netip"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/responsepolicy"
)

// Names of the bootstrap defaults (internal/bootstrap seeds them).
const (
	DefaultNamespace = "default"
	DefaultGroupName = "default"
	// DefaultPolicyName is the fallback policy.
	DefaultPolicyName = "default"
)

// HostnameResolver supplies the names a client address is known by
// (reverse DNS of LAN clients, DHCP leases, an inventory integration). It
// is called on the query path, so it must answer from memory and never
// block; it returns nil when the address is unknown. Lower-case names
// without a trailing dot avoid a per-query conversion.
type HostnameResolver interface {
	Hostnames(addr netip.Addr) []string
}

// Options configures Build.
type Options struct {
	// DefaultNamespace holds ClientGroup "default" and Policy "default";
	// default "default". In Kubernetes pass the namespace given to
	// bootstrap.Options.
	DefaultNamespace string
	// Hostnames feeds the hostname and nameGlob tier; nil in v0.1.
	Hostnames HostnameResolver
	// Inventory enables MAC and captured hostname matching supplied by the
	// identify stage. It does not perform any inventory reads during Build.
	Inventory bool
	// ExceptionScope says which deny rules a @@ exception cancels; the
	// default blocking.ScopeList matches list semantics D73.
	ExceptionScope blocking.ExceptionScope
	// Services tells a blockedServices ID the catalogue does not have
	// (Diagnosis UnknownService) from one whose list is not compiled yet
	// (NotLoaded), and resolves blockedCategories to their services; nil
	// reports every uncompiled ID and every category as unknown.
	Services ServiceCatalogue
}

// ServiceCatalogue is the blocked-services catalogue in service
// (catalogue.Holder).
type ServiceCatalogue interface {
	HasService(id string) bool
	// CategoryServices returns the service IDs of a category and whether
	// it exists.
	CategoryServices(id string) ([]string, bool)
}

// EffectiveServices is the set of services a policy spec blocks:
// blockedServices, then the services of blockedCategories (in catalogue
// order) that allowedServices does not except, each once. unknown are
// the categories cat does not have. cat may be nil (no category
// resolves).
func EffectiveServices(spec *v1alpha1.PolicySpec, cat ServiceCatalogue) (ids, unknown []string) {
	seen := map[string]bool{}
	for _, id := range spec.BlockedServices {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(spec.BlockedCategories) == 0 {
		return ids, nil
	}
	except := map[string]bool{}
	for _, id := range spec.AllowedServices {
		except[id] = true
	}
	for _, c := range spec.BlockedCategories {
		var members []string
		ok := false
		if cat != nil {
			members, ok = cat.CategoryServices(c)
		}
		if !ok {
			unknown = append(unknown, c)
			continue
		}
		for _, id := range members {
			if !seen[id] && !except[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids, unknown
}

// MaxMACGrants bounds private captured source contributions independently of
// public matchers. Exceeding it closes these contributions, never truncates them.
const MaxMACGrants = 100000

// MACGrant is a private, source-bound contribution to an existing public group.
// It is compiled from one complete image, never persisted as a public matcher.
// The original exclusive expiry is evaluated at the request's captured time.
type MACGrant struct {
	Group, GroupUID, MAC, Source string
	ExpiresAt                    time.Time
}
type sourceMACGrant struct {
	source     string
	expires    time.Time
	membership *Membership
}

// Input is the object set an Index is built from.
type Input struct {
	ClientGroups []*v1alpha1.ClientGroup
	MACGrants    []MACGrant
	Policies     []*v1alpha1.Policy
	BlockLists   []*v1alpha1.BlockList
	// Lists are the compiled lists; nil means none compiled yet.
	Lists *blocking.Snapshot
	// Generation numbers the Index (diagnostics only).
	Generation uint64
}

// Tier says which identification tier matched a client.
type Tier uint8

// Tiers in precedence order.
const (
	TierNone Tier = iota
	TierIP
	TierMAC
	TierCIDR
	TierHostname
	TierNameGlob
	TierClientID
	TierCatchAll
	TierDefault
)

func (t Tier) String() string {
	switch t {
	case TierIP:
		return "ip"
	case TierMAC:
		return "mac"
	case TierCIDR:
		return "cidr"
	case TierHostname:
		return "hostname"
	case TierNameGlob:
		return "nameGlob"
	case TierClientID:
		return "clientID"
	case TierCatchAll:
		return "catchAll"
	case TierDefault:
		return "default"
	}
	return "none"
}

// Membership is the result of identifying a client: its groups and the
// policies they bring. Memberships are shared: never modify GroupIDs.
type Membership struct {
	// GroupIDs are the client's groups, "namespace/name", sorted; empty
	// when no group (not even the default group) exists.
	GroupIDs []string
	// Tier is the identification tier that matched.
	Tier            Tier
	set             *policySet
	rateLimitExempt bool
}

// RateLimitExempt reports the compiled exemption of the winning membership.
// It is independent of policy priority/schedules and grants no authority.
func (m *Membership) RateLimitExempt() bool { return m != nil && m.rateLimitExempt }

// Diagnosis reasons.
const (
	// ReasonTie: an IP, MAC, hostname, clientID or equal-length CIDR is
	// listed by more than one group; the client joins all of them.
	ReasonTie = "Tie"
	// ReasonInvalidMatcher: a client matcher does not parse and is ignored.
	ReasonInvalidMatcher = "InvalidMatcher"
	// ReasonUnsupported: a matcher needs a source this build lacks (MAC
	// inventory, hostname resolver).
	ReasonUnsupported = "Unsupported"
	// ReasonMissingReference: a policy names a group or list that does
	// not exist.
	ReasonMissingReference = "MissingReference"
	// ReasonNotLoaded: a referenced list exists but is not compiled yet.
	ReasonNotLoaded = "NotLoaded"
	// ReasonModeMismatch: a block list under allowListRefs or the reverse;
	// the list is applied by its own mode.
	ReasonModeMismatch = "ModeMismatch"
	// ReasonInvalidSchedule: the schedule does not parse; the policy is
	// treated as always active (fail closed).
	ReasonInvalidSchedule = "InvalidSchedule"
	// ReasonDisabled: a policy references a ClientGroup or BlockList
	// annotated kudurru.io/enabled: "false"; the reference is skipped.
	ReasonDisabled = "Disabled"
	// ReasonUnknownService: blockedServices names a service the catalogue
	// does not have; the ID is skipped.
	ReasonUnknownService = "UnknownService"
	// ReasonUnknownCategory: blockedCategories names a category the
	// catalogue does not have; the ID is skipped.
	ReasonUnknownCategory = "UnknownCategory"
)

// Diagnosis is a configuration problem found while building the Index.
// Nothing is fatal: the offending entry is ignored or applied in the safe
// direction, and the rest of the configuration keeps working.
type Diagnosis struct {
	Kind    string // ClientGroup, Policy
	Object  string // namespace/name
	Reason  string
	Message string
}

func (d Diagnosis) String() string {
	return fmt.Sprintf("%s %s: %s: %s", d.Kind, d.Object, d.Reason, d.Message)
}

// Index is the immutable identification and policy index of one snapshot.
// It is safe for concurrent use.
type Index struct {
	generation  uint64
	groupIDs    []string // sorted; index = group number
	groupSets   []*policySet
	groupExempt []bool

	ip        map[netip.Addr]*Membership
	mac       map[string]*Membership
	macGrants map[string][]sourceMACGrant
	v4, v6    cidrTable
	hostname  map[string][]int
	globs     []globMatcher
	clientID  map[string]*Membership
	catch4    *Membership
	catch6    *Membership
	def       *Membership

	hostnames HostnameResolver
	hostOne   []*Membership // per group: hostname-tier membership of that group alone
	globOne   []*Membership // per group: nameGlob-tier membership of that group alone
	dynamic   sync.Map      // tier:group-set key -> *Membership (multi-group hostname matches)
	setsMu    sync.Mutex
	sets      map[string]*policySet // group-set key -> set
	pols      []*compiledPolicy     // sorted by id
	byGroup   [][]*compiledPolicy   // candidate policies per group
	merge     func([]*compiledPolicy, bool, []string) *Decision

	fallbackPolicy *compiledPolicy
	fallback       *policySet
	none           *Decision
	lists          map[string]*ListInfo
	notLoaded      []string
	diags          []Diagnosis
}

type cidrTable struct {
	bits []int // distinct prefix lengths, longest first; never 0
	m    map[netip.Prefix]*Membership
}

func (t *cidrTable) lookup(a netip.Addr) *Membership {
	for _, b := range t.bits {
		p, err := a.Prefix(b)
		if err != nil {
			continue
		}
		if m, ok := t.m[p]; ok {
			return m
		}
	}
	return nil
}

type globMatcher struct {
	pattern string
	group   int
}

func objectID(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "/" + name
}

func isGlob(s string) bool { return strings.ContainsAny(s, "*?[") }

func normalizeHost(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// lowerHost is normalizeHost for the query path: it allocates only when
// the name is not already lower case without a trailing dot.
func lowerHost(s string) string {
	s = strings.TrimSuffix(s, ".")
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			return strings.ToLower(s)
		}
	}
	return s
}

func normalizeMAC(s string) (string, bool) {
	hw, err := net.ParseMAC(strings.TrimSpace(s))
	if err != nil {
		return "", false
	}
	return hw.String(), true
}

// builder accumulates matcher keys before memberships are interned.
type builder struct {
	opts      Options
	ix        *Index
	groupNum  map[string]int
	ip        map[netip.Addr][]int
	mac       map[string][]int
	cidr      map[netip.Prefix][]int
	clientID  map[string][]int
	catch4    []int
	catch6    []int
	members   map[string]*Membership
	byObject  map[string]*ListInfo
	needHosts []string // groups with hostname matchers
	needMAC   []string
	diag      func(kind, obj, reason, format string, args ...any)
	// disabled holds "Kind/namespace/name" of disabled objects.
	disabled map[string]bool
}

// enabledOnly drops the objects annotated kudurru.io/enabled: "false" and
// records them in disabled.
func enabledOnly[T interface {
	*v1alpha1.ClientGroup | *v1alpha1.Policy | *v1alpha1.BlockList
	metav1.Object
}](objs []T, kind string, disabled map[string]bool) []T {
	out := objs[:0:0]
	for _, o := range objs {
		if v1alpha1.IsDisabled(o) {
			disabled[kind+"/"+objectID(o.GetNamespace(), o.GetName())] = true
			continue
		}
		out = append(out, o)
	}
	return out
}

// Build compiles in into an Index. It never fails: problems become
// Diagnoses.
func Build(in Input, opts Options) *Index {
	if opts.DefaultNamespace == "" {
		opts.DefaultNamespace = DefaultNamespace
	}
	lists := in.Lists
	if lists == nil {
		lists, _ = blocking.NewSnapshot(0)
	}
	ix := &Index{
		generation: in.Generation,
		ip:         map[netip.Addr]*Membership{},
		mac:        map[string]*Membership{},
		macGrants:  map[string][]sourceMACGrant{},
		v4:         cidrTable{m: map[netip.Prefix]*Membership{}},
		v6:         cidrTable{m: map[netip.Prefix]*Membership{}},
		hostname:   map[string][]int{},
		clientID:   map[string]*Membership{},
		hostnames:  opts.Hostnames,
		sets:       map[string]*policySet{},
		lists:      map[string]*ListInfo{},
	}
	b := &builder{
		opts: opts, ix: ix, groupNum: map[string]int{},
		ip: map[netip.Addr][]int{}, mac: map[string][]int{}, cidr: map[netip.Prefix][]int{},
		clientID: map[string][]int{}, members: map[string]*Membership{}, byObject: map[string]*ListInfo{},
	}
	b.diag = func(kind, obj, reason, format string, args ...any) {
		ix.diags = append(ix.diags, Diagnosis{Kind: kind, Object: obj, Reason: reason, Message: fmt.Sprintf(format, args...)})
	}

	// kudurru.io/enabled: "false" objects are left out as if absent; a
	// policy naming a disabled list or group skips the reference quietly
	// (ReasonDisabled diagnosis, not MissingReference).
	b.disabled = map[string]bool{}
	in.ClientGroups = enabledOnly(in.ClientGroups, "ClientGroup", b.disabled)
	in.Policies = enabledOnly(in.Policies, "Policy", b.disabled)
	in.BlockLists = enabledOnly(in.BlockLists, "BlockList", b.disabled)

	groups := slices.Clone(in.ClientGroups)
	slices.SortFunc(groups, func(a, b *v1alpha1.ClientGroup) int {
		return cmp.Compare(objectID(a.Namespace, a.Name), objectID(b.Namespace, b.Name))
	})
	groups = slices.CompactFunc(groups, func(a, b *v1alpha1.ClientGroup) bool {
		return a.Namespace == b.Namespace && a.Name == b.Name
	})
	for i, g := range groups {
		id := objectID(g.Namespace, g.Name)
		ix.groupIDs = append(ix.groupIDs, id)
		ix.groupExempt = append(ix.groupExempt, g.Spec.RateLimitExempt)
		b.groupNum[id] = i
	}

	b.compileLists(in.BlockLists, lists)
	b.compilePolicies(in.Policies, lists)
	for i, g := range groups {
		b.compileGroup(i, g)
	}
	b.intern()
	if len(in.MACGrants) > MaxMACGrants {
		b.diag("ClientGroup", "", ReasonInvalidMatcher, "private inventory MAC grant capacity exceeded")
	} else {
		for _, grant := range in.MACGrants {
			n, present := b.groupNum[grant.Group]
			mac, valid := normalizeMAC(grant.MAC)
			if !opts.Inventory || !present || !valid || grant.Source == "" || len(grant.Source) > 256 || grant.ExpiresAt.IsZero() || string(groups[n].UID) != grant.GroupUID || grant.GroupUID == "" {
				b.diag("ClientGroup", grant.Group, ReasonInvalidMatcher, "private inventory MAC grant is invalid or unavailable")
				continue
			}
			ix.macGrants[mac] = append(ix.macGrants[mac], sourceMACGrant{source: grant.Source, expires: grant.ExpiresAt, membership: b.membership([]int{n}, TierMAC)})
		}
	}
	if len(b.needHosts) > 0 && opts.Hostnames == nil && !opts.Inventory {
		for _, id := range b.needHosts {
			b.diag("ClientGroup", id, ReasonUnsupported, "hostname and nameGlob matchers need a hostname source; none is configured, so they never match")
		}
	}
	if !opts.Inventory {
		for _, id := range b.needMAC {
			b.diag("ClientGroup", id, ReasonUnsupported, "mac matchers need an inventory source; none is configured")
		}
	}
	slices.SortStableFunc(ix.diags, func(a, b Diagnosis) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Object, b.Object), cmp.Compare(a.Reason, b.Reason), cmp.Compare(a.Message, b.Message))
	})
	ix.diags = slices.Compact(ix.diags)
	slices.Sort(ix.notLoaded)
	ix.notLoaded = slices.Compact(ix.notLoaded)
	return ix
}

// compileLists records every BlockList and finds its compiled name.
func (b *builder) compileLists(bls []*v1alpha1.BlockList, lists *blocking.Snapshot) {
	for _, bl := range bls {
		id := objectID(bl.Namespace, bl.Name)
		name := id
		switch {
		case lists.List(id) != nil:
		case lists.List(bl.Name) != nil:
			name = bl.Name
		}
		mode := bl.Spec.Mode
		if mode == "" {
			mode = v1alpha1.ListMode(blocking.ModeBlock)
		}
		// Inline: every enabled source is inline or structured entries
		// (disabled ones contribute nothing to the compiled list).
		info := &ListInfo{Name: name, Object: id, Mode: mode, Response: bl.Spec.Response}
		for _, group := range bl.EffectiveClientGroupRefs() {
			info.groups = append(info.groups, objectID(bl.Namespace, group))
		}
		for i := range bl.Spec.Sources {
			s := &bl.Spec.Sources[i]
			if !s.IsEnabled() {
				continue
			}
			if len(s.Inline) == 0 && len(s.Entries) == 0 {
				info.Inline = false
				break
			}
			info.Inline = true
		}
		if c := bl.Spec.CustomIP; c != nil {
			if a, err := netip.ParseAddr(c.V4); err == nil && a.Unmap().Is4() {
				info.CustomV4 = a.Unmap()
			}
			if a, err := netip.ParseAddr(c.V6); err == nil && a.Is6() && !a.Is4In6() {
				info.CustomV6 = a
			}
		}
		if _, dup := b.ix.lists[name]; !dup {
			b.ix.lists[name] = info
		}
		b.byObject[id] = info
	}
}

// resolveList finds the compiled name of the BlockList ref in ns.
func (b *builder) resolveList(lists *blocking.Snapshot, polID, ns, ref, field string, want v1alpha1.ListMode) (string, bool) {
	id := objectID(ns, ref)
	var name string
	switch {
	case lists.List(id) != nil:
		name = id
	case lists.List(ref) != nil:
		name = ref
	}
	_, known := b.byObject[id]
	if b.disabled["BlockList/"+id] {
		b.diag("Policy", polID, ReasonDisabled, "%s %q is disabled (kudurru.io/enabled: \"false\") and skipped", field, ref)
		return "", false
	}
	if name == "" {
		if known {
			b.ix.notLoaded = append(b.ix.notLoaded, id)
			b.diag("Policy", polID, ReasonNotLoaded, "%s %q is not compiled yet", field, ref)
		} else {
			b.diag("Policy", polID, ReasonMissingReference, "%s %q: no such BlockList in namespace %q", field, ref, ns)
		}
		return "", false
	}
	if l := lists.List(name); l != nil && string(l.Mode()) != string(want) {
		b.diag("Policy", polID, ReasonModeMismatch, "%s %q is a %s list; it is applied as one", field, ref, l.Mode())
	}
	return name, true
}

// resolveService finds the compiled list of a blockedServices ID: the
// catalogue's "service:<id>", compiled by the list resolver next to the
// BlockLists.
func (b *builder) resolveService(lists *blocking.Snapshot, polID, svc string) (string, bool) {
	name := catalogue.ListName(svc)
	if lists.List(name) != nil {
		return name, true
	}
	if b.opts.Services != nil && b.opts.Services.HasService(svc) {
		b.ix.notLoaded = append(b.ix.notLoaded, name)
		b.diag("Policy", polID, ReasonNotLoaded, "blockedServices %q is not compiled yet", svc)
		return "", false
	}
	b.diag("Policy", polID, ReasonUnknownService, "blockedServices %q: no such service in the catalogue (GET /api/v1/catalogue/services)", svc)
	return "", false
}

func (b *builder) compilePolicies(pols []*v1alpha1.Policy, lists *blocking.Snapshot) {
	ix := b.ix
	ix.byGroup = make([][]*compiledPolicy, len(ix.groupIDs))
	var fallback *compiledPolicy
	for _, p := range pols {
		id := objectID(p.Namespace, p.Name)
		cp := &compiledPolicy{id: id, spec: &p.Spec, priority: p.Spec.Priority}
		var specs []responsepolicy.RewriteSpec
		for _, r := range p.Spec.Rewrites {
			specs = append(specs, responsepolicy.RewriteSpec{Name: r.Name, Type: r.Type, CNAME: r.CNAME, Targets: r.Targets})
		}
		var responseErr error
		cp.rewrites, responseErr = responsepolicy.CompileRewrites(specs)
		if responseErr == nil && p.Spec.Response != nil {
			cp.response, responseErr = responsepolicy.CompileOverrides(p.Spec.Response.DNS64Prefix, p.Spec.Response.FilterAAAA)
		}
		if responseErr != nil {
			cp.responseInvalid = true
			b.diag("Policy", id, "InvalidResponsePolicy", "%s", responseErr)
		}
		if p.Spec.Schedule != nil {
			s, err := compileSchedule(p.Spec.Schedule)
			if err != nil {
				b.diag("Policy", id, ReasonInvalidSchedule, "%v; the policy applies at all times", err)
			} else {
				cp.sched = s
			}
		}
		for _, ref := range p.Spec.AllowListRefs {
			if n, ok := b.resolveList(lists, id, p.Namespace, ref, "allowListRefs", v1alpha1.ListMode(blocking.ModeAllow)); ok {
				cp.allow = append(cp.allow, n)
			}
		}
		for _, ref := range p.Spec.BlockListRefs {
			if n, ok := b.resolveList(lists, id, p.Namespace, ref, "blockListRefs", v1alpha1.ListMode(blocking.ModeBlock)); ok {
				cp.block = append(cp.block, n)
			}
		}
		svcs, unknownCats := EffectiveServices(&p.Spec, b.opts.Services)
		for _, c := range unknownCats {
			b.diag("Policy", id, ReasonUnknownCategory, "blockedCategories %q: no such category in the catalogue (GET /api/v1/catalogue/categories)", c)
		}
		for _, svc := range svcs {
			if n, ok := b.resolveService(lists, id, svc); ok {
				cp.block = append(cp.block, n)
			}
		}
		for _, ref := range p.Spec.ClientGroupRefs {
			g, ok := b.groupNum[objectID(p.Namespace, ref)]
			if !ok && b.disabled["ClientGroup/"+objectID(p.Namespace, ref)] {
				b.diag("Policy", id, ReasonDisabled, "clientGroupRefs %q is disabled (kudurru.io/enabled: \"false\") and skipped", ref)
				continue
			}
			if !ok {
				b.diag("Policy", id, ReasonMissingReference, "clientGroupRefs %q: no such ClientGroup in namespace %q", ref, p.Namespace)
				continue
			}
			if !slices.Contains(ix.byGroup[g], cp) {
				ix.byGroup[g] = append(ix.byGroup[g], cp)
			}
		}
		ix.pols = append(ix.pols, cp)
		if p.Namespace == b.opts.DefaultNamespace && p.Name == DefaultPolicyName {
			fallback = cp
		}
	}
	for g := range ix.byGroup {
		sortPolicies(ix.byGroup[g])
	}
	scope, infos := b.opts.ExceptionScope, ix.lists
	ix.merge = func(active []*compiledPolicy, fb bool, groups []string) *Decision {
		return mergePolicies(active, fb, lists, scope, infos, groups)
	}
	ix.fallbackPolicy = fallback
	if fallback != nil {
		ix.fallback = newPolicySet([]*compiledPolicy{fallback}, true, func(active []*compiledPolicy, fb bool) *Decision {
			return ix.merge(active, fb, nil)
		})
	}
	ix.none = &Decision{lists: ix.lists}
	ix.groupSets = make([]*policySet, len(ix.groupIDs))
	for g := range ix.groupIDs {
		ix.groupSets[g] = ix.policySetFor([]int{g})
	}
}

func sortPolicies(ps []*compiledPolicy) {
	slices.SortFunc(ps, func(a, b *compiledPolicy) int {
		return cmp.Or(cmp.Compare(b.priority, a.priority), cmp.Compare(a.id, b.id))
	})
}

// mergePolicies merges active (sorted by priority) into one Decision.
func mergePolicies(active []*compiledPolicy, fallback bool, lists *blocking.Snapshot, scope blocking.ExceptionScope, infos map[string]*ListInfo, groups []string) *Decision {
	top := active[0]
	d := &Decision{Policy: top.id, Fallback: fallback, AllowOnly: top.spec.AllowOnly, lists: infos}
	var allow, block []string
	var responseOverrides []*responsepolicy.Overrides
	for _, p := range active {
		d.Rewrites = append(d.Rewrites, p.rewrites...)
		d.ResponseInvalid = d.ResponseInvalid || p.responseInvalid
		responseOverrides = append(responseOverrides, p.response)
		d.Policies = append(d.Policies, p.id)
		s := p.spec
		if d.BlockMode == "" {
			d.BlockMode = s.BlockMode
		}
		if d.BlockTTL == 0 {
			d.BlockTTL = s.BlockTTL.Duration
		}
		if d.UpstreamGroup == "" {
			d.UpstreamGroup = s.UpstreamGroup
		}
		if d.QueryLog == nil {
			d.QueryLog = s.QueryLog
		}
		if d.SafeSearch == nil {
			d.SafeSearch = s.SafeSearch
		}
		if d.SafeSearchMode == "" {
			d.SafeSearchMode = s.SafeSearchMode
		}
		allow = append(allow, p.allow...)
		block = append(block, p.block...)
	}
	refs := slices.DeleteFunc(slices.Concat(allow, block), func(name string) bool {
		info := infos[name]
		if info == nil || len(info.groups) == 0 {
			return false
		}
		return !slices.ContainsFunc(info.groups, func(group string) bool { return slices.Contains(groups, group) })
	})
	d.Response = responsepolicy.MergeOverrides(responseOverrides)
	// Select keeps the first occurrence of each name and gives every list
	// the role of its own mode; all names are known, so it cannot fail.
	d.Selection, _ = lists.Select(refs, blocking.SelectOptions{AllowOnly: d.AllowOnly, ExceptionScope: scope})
	d.filters = d.AllowOnly || len(refs) > 0 || d.SafeSearchOn()
	return d
}

// policySetFor returns the interned candidate set of a sorted group set.
func (ix *Index) policySetFor(groups []int) *policySet {
	key := groupKey(groups)
	ix.setsMu.Lock()
	defer ix.setsMu.Unlock()
	if ps, ok := ix.sets[key]; ok {
		return ps
	}
	var pols []*compiledPolicy
	for _, g := range groups {
		for _, p := range ix.byGroup[g] {
			if !slices.Contains(pols, p) {
				pols = append(pols, p)
			}
		}
	}
	sortPolicies(pols)
	// Bind both active and fallback selections to private copied identities,
	// never the exported diagnostic Membership.GroupIDs or request fields.
	ids := make([]string, len(groups))
	for i, g := range groups {
		ids[i] = ix.groupIDs[g]
	}
	merge := func(active []*compiledPolicy, fb bool) *Decision { return ix.merge(active, fb, ids) }
	ps := newPolicySet(pols, false, merge)
	if ix.fallbackPolicy != nil {
		ps.defaultSet = newPolicySet([]*compiledPolicy{ix.fallbackPolicy}, true, merge)
	}
	ix.sets[key] = ps
	return ps
}

func groupKey(groups []int) string {
	var sb strings.Builder
	for i, g := range groups {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprint(&sb, g)
	}
	return sb.String()
}

func (b *builder) compileGroup(g int, cg *v1alpha1.ClientGroup) {
	id := b.ix.groupIDs[g]
	add := func(m map[string][]int, k string) {
		if !slices.Contains(m[k], g) {
			m[k] = append(m[k], g)
		}
	}
	hosts, macs := false, false
	for i, c := range cg.Spec.Clients {
		where := fmt.Sprintf("clients[%d]", i)
		switch {
		case c.IP != "":
			a, err := netip.ParseAddr(strings.TrimSpace(c.IP))
			if err != nil {
				b.diag("ClientGroup", id, ReasonInvalidMatcher, "%s.ip %q: %v", where, c.IP, err)
				continue
			}
			a = a.Unmap().WithZone("")
			if !slices.Contains(b.ip[a], g) {
				b.ip[a] = append(b.ip[a], g)
			}
		case c.CIDR != "":
			p, err := parseCIDR(c.CIDR)
			if err != nil {
				b.diag("ClientGroup", id, ReasonInvalidMatcher, "%s.cidr %q: %v", where, c.CIDR, err)
				continue
			}
			if p != p.Masked() {
				b.diag("ClientGroup", id, ReasonInvalidMatcher, "%s.cidr %q has host bits set; read as %s", where, c.CIDR, p.Masked())
				p = p.Masked()
			}
			switch {
			case p.Bits() == 0 && p.Addr().Is4():
				if !slices.Contains(b.catch4, g) {
					b.catch4 = append(b.catch4, g)
				}
			case p.Bits() == 0:
				if !slices.Contains(b.catch6, g) {
					b.catch6 = append(b.catch6, g)
				}
			case !slices.Contains(b.cidr[p], g):
				b.cidr[p] = append(b.cidr[p], g)
			}
		case c.MAC != "":
			m, ok := normalizeMAC(c.MAC)
			if !ok {
				b.diag("ClientGroup", id, ReasonInvalidMatcher, "%s.mac %q is not a MAC address", where, c.MAC)
				continue
			}
			add(b.mac, m)
			macs = true
		case c.ClientID != "":
			add(b.clientID, strings.ToLower(strings.TrimSpace(c.ClientID)))
		case c.Hostname != "" || c.NameGlob != "":
			pat, glob := normalizeHost(c.Hostname), false
			if c.NameGlob != "" {
				pat, glob = strings.ToLower(strings.TrimSpace(c.NameGlob)), true
			}
			if isGlob(pat) {
				glob = true
			}
			if glob {
				if _, err := path.Match(pat, ""); err != nil {
					b.diag("ClientGroup", id, ReasonInvalidMatcher, "%s glob %q: %v", where, pat, err)
					continue
				}
				b.ix.globs = append(b.ix.globs, globMatcher{pattern: strings.TrimSuffix(pat, "."), group: g})
			} else {
				add(b.ix.hostname, pat)
			}
			hosts = true
		default:
			b.diag("ClientGroup", id, ReasonInvalidMatcher, "%s sets no matcher", where)
		}
	}
	if hosts {
		b.needHosts = append(b.needHosts, id)
	}
	if macs {
		b.needMAC = append(b.needMAC, id)
	}
}

// parseCIDR accepts a prefix or a bare address (a host prefix).
func parseCIDR(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil {
		if p.Addr().Is4In6() && p.Bits() >= 96 {
			p = netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96)
		}
		return p, nil
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	a = a.Unmap().WithZone("")
	return netip.PrefixFrom(a, a.BitLen()), nil
}

// membership interns the membership of a group set at a tier.
func (b *builder) membership(groups []int, tier Tier) *Membership {
	groups = slices.Clone(groups)
	slices.Sort(groups)
	key := fmt.Sprintf("%d:%s", tier, groupKey(groups))
	if m, ok := b.members[key]; ok {
		return m
	}
	m := &Membership{Tier: tier, set: b.ix.policySetFor(groups)}
	for _, g := range groups {
		m.GroupIDs = append(m.GroupIDs, b.ix.groupIDs[g])
		m.rateLimitExempt = m.rateLimitExempt || b.ix.groupExempt[g]
	}
	b.members[key] = m
	return m
}

func (b *builder) tie(what string, groups []int) {
	if len(groups) < 2 {
		return
	}
	ids := make([]string, len(groups))
	for i, g := range groups {
		ids[i] = b.ix.groupIDs[g]
	}
	slices.Sort(ids)
	for _, id := range ids {
		b.diag("ClientGroup", id, ReasonTie, "%s is also listed by %s; matching clients join all of them", what, strings.Join(slices.DeleteFunc(slices.Clone(ids), func(s string) bool { return s == id }), ", "))
	}
}

// intern turns the collected keys into shared memberships.
func (b *builder) intern() {
	ix := b.ix
	for a, gs := range b.ip {
		b.tie("ip "+a.String(), gs)
		ix.ip[a] = b.membership(gs, TierIP)
	}
	for m, gs := range b.mac {
		b.tie("mac "+m, gs)
		ix.mac[m] = b.membership(gs, TierMAC)
	}
	bits4, bits6 := map[int]bool{}, map[int]bool{}
	for p, gs := range b.cidr {
		b.tie("cidr "+p.String(), gs)
		m := b.membership(gs, TierCIDR)
		if p.Addr().Is4() {
			ix.v4.m[p] = m
			bits4[p.Bits()] = true
		} else {
			ix.v6.m[p] = m
			bits6[p.Bits()] = true
		}
	}
	ix.v4.bits = sortedBits(bits4)
	ix.v6.bits = sortedBits(bits6)
	for h, gs := range ix.hostname {
		b.tie("hostname "+h, gs)
		b.membership(gs, TierHostname)
	}
	for c, gs := range b.clientID {
		b.tie("clientID "+c, gs)
		ix.clientID[c] = b.membership(gs, TierClientID)
	}
	if len(b.catch4) > 0 {
		b.tie("cidr 0.0.0.0/0", b.catch4)
		ix.catch4 = b.membership(b.catch4, TierCatchAll)
	}
	if len(b.catch6) > 0 {
		b.tie("cidr ::/0", b.catch6)
		ix.catch6 = b.membership(b.catch6, TierCatchAll)
	}
	if g, ok := b.groupNum[objectID(b.opts.DefaultNamespace, DefaultGroupName)]; ok {
		ix.def = b.membership([]int{g}, TierDefault)
	} else {
		ix.def = &Membership{Tier: TierDefault, set: ix.policySetFor(nil)}
	}
	ix.hostOne = make([]*Membership, len(ix.groupIDs))
	ix.globOne = make([]*Membership, len(ix.groupIDs))
	for _, gs := range ix.hostname {
		for _, g := range gs {
			if ix.hostOne[g] == nil {
				ix.hostOne[g] = b.membership([]int{g}, TierHostname)
			}
		}
	}
	for _, gm := range ix.globs {
		if ix.globOne[gm.group] == nil {
			ix.globOne[gm.group] = b.membership([]int{gm.group}, TierNameGlob)
		}
	}
	// Seed the dynamic table with the build-time hostname memberships.
	for k, m := range b.members {
		if m.Tier == TierHostname || m.Tier == TierNameGlob {
			ix.dynamic.Store(k, m)
		}
	}
}

func sortedBits(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for b := range set {
		out = append(out, b)
	}
	slices.Sort(out)
	slices.Reverse(out)
	return out
}

// Generation returns Input.Generation.
func (ix *Index) Generation() uint64 { return ix.generation }

// Groups returns every ClientGroup ID, sorted.
func (ix *Index) Groups() []string { return slices.Clone(ix.groupIDs) }

// Diagnoses returns the configuration problems found by Build, sorted.
func (ix *Index) Diagnoses() []Diagnosis { return slices.Clone(ix.diags) }

// NotLoaded returns the referenced BlockLists ("namespace/name") that are
// not compiled yet; empty once every referenced list is loaded.
func (ix *Index) NotLoaded() []string { return slices.Clone(ix.notLoaded) }

// Identify finds the client's groups. It never returns nil and does not
// allocate except for a hostname-tier match that joins several groups for
// the first time.
func (ix *Index) Identify(c pipeline.Client) *Membership { return ix.IdentifyAt(c, time.Now()) }

// IdentifyAt evaluates private inventory grants against the ORIGINAL request
// time and captured inventory source. No live holder or clock is consulted.
func (ix *Index) IdentifyAt(c pipeline.Client, at time.Time) *Membership {
	a := c.Addr.Addr()
	valid := a.IsValid()
	if valid {
		a = a.Unmap().WithZone("")
		if m, ok := ix.ip[a]; ok {
			return m
		}
	}
	if len(ix.mac) > 0 || len(ix.macGrants) > 0 {
		if m := ix.byMACs(c, at); m != nil {
			return m
		}
	}
	if valid {
		t := &ix.v6
		if a.Is4() {
			t = &ix.v4
		}
		if m := t.lookup(a); m != nil {
			return m
		}
		if len(ix.hostname) > 0 || len(ix.globs) > 0 {
			names := c.Names
			if !c.IdentityCaptured && ix.hostnames != nil {
				names = ix.hostnames.Hostnames(a)
			}
			if m := ix.byHostname(names); m != nil {
				return m
			}
		}
	}
	if c.ClientID != "" && len(ix.clientID) > 0 {
		if m, ok := ix.clientID[lowerHost(c.ClientID)]; ok {
			return m
		}
	}
	if valid {
		if a.Is4() && ix.catch4 != nil {
			return ix.catch4
		}
		if a.Is6() && ix.catch6 != nil {
			return ix.catch6
		}
	}
	return ix.def
}

func (ix *Index) byMACs(c pipeline.Client, at time.Time) *Membership {
	var first *Membership
	var groups []int
	add := func(m *Membership) {
		if m == nil || m == first {
			return
		}
		if first == nil {
			first = m
			return
		}
		if groups == nil {
			for _, id := range first.GroupIDs {
				if n, ok := slices.BinarySearch(ix.groupIDs, id); ok {
					groups = append(groups, n)
				}
			}
		}
		for _, id := range m.GroupIDs {
			if n, ok := slices.BinarySearch(ix.groupIDs, id); ok && !slices.Contains(groups, n) {
				groups = append(groups, n)
			}
		}
	}
	for _, raw := range c.MACs {
		mac, valid := normalizeMAC(raw)
		if !valid {
			continue
		}
		add(ix.mac[mac])
		if c.IdentityCaptured && c.InventorySource != "" && !at.IsZero() {
			for _, grant := range ix.macGrants[mac] {
				if grant.source == c.InventorySource && at.Before(grant.expires) {
					add(grant.membership)
				}
			}
		}
	}
	if groups == nil {
		return first
	}
	return ix.joinMembership(groups, TierMAC)
}

// byHostname runs the hostname tier, then the nameGlob tier.
func (ix *Index) byHostname(names []string) *Membership {
	if len(names) == 0 {
		return nil
	}
	var buf [8]int
	groups := buf[:0]
	for _, n := range names {
		for _, g := range ix.hostname[lowerHost(n)] {
			if !slices.Contains(groups, g) {
				groups = append(groups, g)
			}
		}
	}
	tier := TierHostname
	if len(groups) == 0 {
		tier = TierNameGlob
		for _, n := range names {
			n = lowerHost(n)
			for _, gm := range ix.globs {
				if ok, _ := path.Match(gm.pattern, n); ok && !slices.Contains(groups, gm.group) {
					groups = append(groups, gm.group)
				}
			}
		}
	}
	switch len(groups) {
	case 0:
		return nil
	case 1:
		if tier == TierHostname {
			return ix.hostOne[groups[0]]
		}
		return ix.globOne[groups[0]]
	}
	return ix.joinMembership(groups, tier)
}

func (ix *Index) joinMembership(groups []int, tier Tier) *Membership {
	slices.Sort(groups)
	key := fmt.Sprintf("%d:%s", tier, groupKey(groups))
	if m, ok := ix.dynamic.Load(key); ok {
		return m.(*Membership)
	}
	m := &Membership{Tier: tier, set: ix.policySetFor(groups)}
	for _, g := range groups {
		m.GroupIDs = append(m.GroupIDs, ix.groupIDs[g])
		m.rateLimitExempt = m.rateLimitExempt || ix.groupExempt[g]
	}
	actual, _ := ix.dynamic.LoadOrStore(key, m)
	return actual.(*Membership)
}

// Select returns the decision for m at now: the merged active policies
// of m's groups, else the default policy, else a Decision that applies no
// policy. It never returns nil.
func (ix *Index) Select(m *Membership, now time.Time) *Decision {
	if m != nil {
		if d := m.set.decide(now); d != nil {
			return d
		}
		if m.set != nil {
			if d := m.set.defaultSet.decide(now); d != nil {
				return d
			}
			return ix.none
		}
	}
	if d := ix.fallback.decide(now); d != nil {
		return d
	}
	return ix.none
}

// GroupDecision returns the decision a client that is only in group id
// ("namespace/name") gets at now, with the same fallback as Select; ok is
// false for an unknown group. The UI and the kudurru_blocking_enabled
// gauge use it.
func (ix *Index) GroupDecision(id string, now time.Time) (d *Decision, ok bool) {
	g, found := slices.BinarySearch(ix.groupIDs, id)
	if !found {
		return nil, false
	}
	return ix.Select(&Membership{set: ix.groupSets[g]}, now), true
}

// SelectPolicy identifies c and selects its policy at now.
func (ix *Index) SelectPolicy(c pipeline.Client, now time.Time) *Decision {
	return ix.Select(ix.Identify(c), now)
}
