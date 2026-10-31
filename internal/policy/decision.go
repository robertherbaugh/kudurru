package policy

import (
	"net/netip"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/responsepolicy"
)

// Decision is the merged policy that applies to one client at one moment.
// Decisions are precomputed and shared by every query that selects them:
// never modify one.
type Decision struct {
	// Policy is the highest-priority applied policy, "namespace/name"; empty
	// when no policy applies.
	Policy string
	// Policies are all applied policies, highest priority first.
	Policies []string
	// Fallback is true when no policy of the client's groups was active and
	// the default policy applied instead.
	Fallback bool

	// AllowOnly blocks every name no allow list matches.
	AllowOnly bool
	// BlockMode is the block response; empty means the configured
	// blocking.defaultResponse. A BlockList's own response overrides it for
	// that list's matches (ListInfo.Response).
	BlockMode v1alpha1.BlockResponse
	// BlockTTL is the TTL of block responses; zero means the configured
	// blocking.blockTTL.
	BlockTTL time.Duration
	// UpstreamGroup is the policy's upstream group, empty when unset.
	UpstreamGroup string
	// QueryLog false excludes the client from the query log; nil when
	// unset.
	QueryLog *bool
	// SafeSearch true makes the blocking stage answer the catalogue's
	// search-engine host names with their safe-search targets; nil when
	// unset.
	SafeSearch *bool
	// SafeSearchMode is strict, moderate or empty (strict).
	SafeSearchMode string
	// Rewrites are ordered by policy priority, then specificity within each
	// policy. Response overrides inherit each highest-priority explicit field.
	Rewrites []*responsepolicy.Rewrite
	Response *responsepolicy.Overrides
	// ResponseInvalid fails recursive queries closed until invalid response configuration is corrected.
	ResponseInvalid bool

	// Selection holds the policy's lists resolved against the compiled
	// lists of the same snapshot; nil when no policy applies.
	Selection *blocking.Selection
	filters   bool
	lists     map[string]*ListInfo
}

// SafeSearchOn reports whether safe search is enforced (d may be nil).
func (d *Decision) SafeSearchOn() bool { return d != nil && d.SafeSearch != nil && *d.SafeSearch }

// Applies reports whether a policy applies (d may be nil).
func (d *Decision) Applies() bool { return d != nil && d.Policy != "" }

// Filters reports whether the decision can block or rewrite anything: a
// policy applies and it names at least one list or blocked service, is
// allow-only, or enforces safe search. The blocking stage skips decisions
// that do not filter.
func (d *Decision) Filters() bool { return d != nil && d.filters }

// List returns what the Index knows about the compiled list name (a name
// from a blocking.Decision); nil for an unknown list.
func (d *Decision) List(name string) *ListInfo {
	if d == nil {
		return nil
	}
	return d.lists[name]
}

// ListInfo describes one BlockList as the blocking stage needs it.
type ListInfo struct {
	// groups is copied namespace-qualified scope from the accepted object.
	// Empty permits every membership. Only private policy-set identities use it.
	groups []string

	// Name is the list's name in the compiled blocking.Snapshot.
	Name string
	// Object is the BlockList, "namespace/name".
	Object string
	// Mode is the list's mode (block or allow).
	Mode v1alpha1.ListMode
	// Response overrides the policy's block mode for this list's matches;
	// empty when unset.
	Response v1alpha1.BlockResponse
	// CustomV4 and CustomV6 are the customIP answers; invalid when unset.
	CustomV4, CustomV6 netip.Addr
	// Inline is true when every source is inline: the user's own list,
	// whose exact and suffix hits are reported as blocked_exact.
	Inline bool
}

// FromRequest returns the decision the identify stage stored in
// req.Policy, or nil.
func FromRequest(req *pipeline.Request) *Decision {
	if req == nil {
		return nil
	}
	d, _ := req.Policy.(*Decision)
	return d
}

// compiledPolicy is one Policy prepared for merging.
type compiledPolicy struct {
	id       string
	spec     *v1alpha1.PolicySpec
	priority int
	sched    *schedule // nil: always active
	// allow and block are the resolved snapshot names of allowListRefs and
	// blockListRefs.
	allow, block    []string
	rewrites        []*responsepolicy.Rewrite
	response        *responsepolicy.Overrides
	responseInvalid bool
}

// maxEagerSched bounds the scheduled policies of one set whose
// combinations are precomputed (2^6 = 64 decisions).
const maxEagerSched = 6

// maxLazySched bounds the scheduled policies whose combinations are
// cached lazily by bit mask; beyond it decisions are merged per query.
const maxLazySched = 63

// policySet is the ordered candidate policies of one group set plus the
// precomputed decisions for every combination of its scheduled policies.
type policySet struct {
	pols       []*compiledPolicy // priority desc, id asc
	sched      []int             // indexes into pols of scheduled policies
	eager      []*Decision       // by mask, when len(sched) <= maxEagerSched
	lazy       sync.Map          // uint64 mask -> *Decision
	merge      func(active []*compiledPolicy, fallback bool) *Decision
	fallback   bool
	defaultSet *policySet // fallback bound to this same original membership
}

func newPolicySet(pols []*compiledPolicy, fallback bool, merge func([]*compiledPolicy, bool) *Decision) *policySet {
	ps := &policySet{pols: pols, merge: merge, fallback: fallback}
	for i, p := range pols {
		if p.sched != nil {
			ps.sched = append(ps.sched, i)
		}
	}
	if len(ps.sched) <= maxEagerSched {
		ps.eager = make([]*Decision, 1<<len(ps.sched))
		for mask := range ps.eager {
			ps.eager[mask] = ps.decisionFor(uint64(mask))
		}
	}
	return ps
}

// decisionFor merges the always-active policies and the scheduled ones
// selected by mask; nil when none is active.
func (ps *policySet) decisionFor(mask uint64) *Decision {
	active := make([]*compiledPolicy, 0, len(ps.pols))
	bit := 0
	for _, p := range ps.pols {
		if p.sched != nil {
			on := mask&(1<<bit) != 0
			bit++
			if !on {
				continue
			}
		}
		active = append(active, p)
	}
	if len(active) == 0 {
		return nil
	}
	return ps.merge(active, ps.fallback)
}

// decide returns the decision for now; nil when no policy of the set is
// active.
func (ps *policySet) decide(now time.Time) *Decision {
	if ps == nil || len(ps.pols) == 0 {
		return nil
	}
	if len(ps.sched) == 0 {
		return ps.eager[0]
	}
	var mask uint64
	if len(ps.sched) <= maxLazySched {
		for bit, i := range ps.sched {
			if ps.pols[i].sched.active(now) {
				mask |= 1 << bit
			}
		}
	}
	switch {
	case ps.eager != nil:
		return ps.eager[mask]
	case len(ps.sched) <= maxLazySched:
		if d, ok := ps.lazy.Load(mask); ok {
			return d.(*Decision)
		}
		d := ps.decisionFor(mask)
		ps.lazy.Store(mask, d)
		return d
	}
	// More scheduled policies than mask bits: merge per query.
	active := make([]*compiledPolicy, 0, len(ps.pols))
	for _, p := range ps.pols {
		if p.sched == nil || p.sched.active(now) {
			active = append(active, p)
		}
	}
	if len(active) == 0 {
		return nil
	}
	return ps.merge(active, ps.fallback)
}
