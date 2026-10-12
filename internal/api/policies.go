package api

import (
	"context"
	"math"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// PolicyState is GET /policies/{namespace}/{name}/state: what the policy
// page shows above its sections.
type PolicyState struct {
	Policy  string `json:"policy" doc:"namespace/name"`
	Enabled bool   `json:"enabled" doc:"false while the kudurru.io/enabled annotation is \"false\"."`
	Active  bool   `json:"active" doc:"enabled and the schedule allows it now (an invalid schedule applies at all times)."`
	// Schedule is absent for a policy without one.
	Schedule   *PolicySchedule  `json:"schedule,omitempty"`
	Priority   int              `json:"priority"`
	ShadowedBy []string         `json:"shadowedBy,omitempty" doc:"Enabled policies (namespace/name, rank order) that outrank this one for at least one of its client groups: they decide for those clients and their settings win; the lists of all active policies still apply."`
	ManagedBy  string           `json:"managedBy,omitempty"`
	Groups     []PolicyGroup    `json:"groups" nullable:"false"`
	Lists      []PolicyList     `json:"lists" nullable:"false"`
	Services   []PolicyService  `json:"services" nullable:"false" doc:"The spec.blockedServices entries."`
	Categories []PolicyCategory `json:"categories" nullable:"false" doc:"The spec.blockedCategories entries."`
	// AllowedServices are the spec.allowedServices exceptions.
	AllowedServices []PolicyService `json:"allowedServices" nullable:"false" doc:"The spec.allowedServices entries: services of a blocked category this policy does not block."`
	// EffectiveServices is what the policy blocks now.
	EffectiveServices []PolicyEffectiveService `json:"effectiveServices" nullable:"false" doc:"Every service the policy blocks with the catalogue in service: blockedServices, then the services of blockedCategories minus allowedServices."`
	SafeSearch        PolicySafeSearch         `json:"safeSearch"`
	// UpstreamGroup is absent when the policy uses the default group.
	UpstreamGroup *PolicyUpstreamGroup `json:"upstreamGroup,omitempty"`
	BlockMode     string               `json:"blockMode" doc:"spec.blockMode, else blocking.defaultResponse; a BlockList's own response still overrides it for that list."`
	BlockTTL      string               `json:"blockTTL,omitempty"`
	Conditions    []metav1.Condition   `json:"conditions" nullable:"false"`
	// Stats24h is absent when this node stores no query log.
	Stats24h *PolicyStats24h `json:"stats24h,omitempty"`
	Took
}

// PolicySchedule is the schedule evaluated now.
type PolicySchedule struct {
	Now        bool       `json:"now" doc:"The schedule allows the policy now."`
	NextChange *time.Time `json:"nextChange,omitempty" doc:"When now flips next (within eight days)."`
	Summary    string     `json:"summary" doc:"\"Mon-Fri 08:00-17:00 (Europe/Berlin)\""`
	Error      string     `json:"error,omitempty" doc:"Why the schedule is invalid; the policy then applies at all times."`
}

// PolicyGroup is one clientGroupRefs entry.
type PolicyGroup struct {
	Ref     string `json:"ref"`
	Exists  bool   `json:"exists"`
	Enabled bool   `json:"enabled"`
	Clients int    `json:"clients" doc:"Client matchers of the group (spec.clients)."`
	Seen24h *int   `json:"seen24h,omitempty" doc:"Distinct clients this node's query log attributed to the group (as their first group) in the last 24 hours; absent without a query log."`
}

// PolicyList is one blockListRefs or allowListRefs entry.
type PolicyList struct {
	Ref            string `json:"ref"`
	Role           string `json:"role" enum:"block,allow" doc:"Which reference field names it: blockListRefs (block) or allowListRefs (allow)."`
	Mode           string `json:"mode" doc:"The list's own spec.mode (how it is applied); the role when it does not exist."`
	Exists         bool   `json:"exists"`
	Enabled        bool   `json:"enabled"`
	Ready          bool   `json:"ready"`
	Entries        int    `json:"entries"`
	SourcesEnabled int    `json:"sourcesEnabled"`
	SourcesTotal   int    `json:"sourcesTotal"`
}

// PolicyService is one blockedServices entry.
type PolicyService struct {
	ID    string `json:"id"`
	Name  string `json:"name" doc:"The catalogue name; the ID when unknown."`
	Known bool   `json:"known" doc:"The catalogue in service has the ID."`
	Rules int    `json:"rules"`
	IconRef
}

// PolicyCategory is one blockedCategories entry.
type PolicyCategory struct {
	ID       string `json:"id"`
	Name     string `json:"name" doc:"The catalogue name; the ID when unknown."`
	Known    bool   `json:"known" doc:"The catalogue in service has the category."`
	Services int    `json:"services" doc:"Services in the category now."`
	Allowed  int    `json:"allowed" doc:"Of those, the ones allowedServices excepts."`
	Icon     string `json:"icon,omitempty" doc:"A Lucide icon name."`
}

// PolicyEffectiveService is one service the policy blocks.
type PolicyEffectiveService struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Known    bool   `json:"known"`
	Rules    int    `json:"rules"`
	Category string `json:"category,omitempty"`
	Via      string `json:"via" enum:"service,category" doc:"service: named in blockedServices; category: a service of a blocked category."`
	Origin   string `json:"origin" enum:"kudurru,adguard"`
	IconRef
}

// PolicySafeSearch is the policy's safe-search setting.
type PolicySafeSearch struct {
	Enabled bool                     `json:"enabled"`
	Mode    string                   `json:"mode" enum:"strict,moderate"`
	Engines []PolicySafeSearchEngine `json:"engines" nullable:"false" doc:"Every engine the catalogue rewrites (all apply when enabled)."`
}

// PolicySafeSearchEngine is one engine safe search rewrites.
type PolicySafeSearchEngine struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	IconRef
}

// PolicyUpstreamGroup is spec.upstreamGroup.
type PolicyUpstreamGroup struct {
	Name   string `json:"name"`
	Exists bool   `json:"exists" doc:"upstreams.groups has it; queries fall back to the default group otherwise."`
}

// PolicyStats24h is the policy's traffic on this node in the last 24
// hours, from the query log rows it decided (rows since the query log
// recorded the deciding policy; bounded by querylog.retention).
type PolicyStats24h struct {
	Queries        int64                    `json:"queries"`
	Blocked        int64                    `json:"blocked"`
	BlockedPercent float64                  `json:"blockedPercent"`
	TopBlocked     []sqlitelog.PolicyDomain `json:"topBlocked" nullable:"false"`
	TopClients     []sqlitelog.PolicyClient `json:"topClients" nullable:"false"`
}

type policyStateOutput struct{ Body *PolicyState }

// registerPolicies registers the policy page route.
func (s *Server) registerPolicies() {
	op := operation(http.MethodGet, "/policies/{namespace}/{name}/state", "getPolicyState", "A policy's current state", "policies")
	op.Description = "Enabled and active now (schedule), rank against other policies, the state of every referenced group, list, service and the upstream group, " +
		"and the last 24 hours of the queries this policy decided on this node."
	register(s, op, perm{resource: "policies", verb: "read"}, s.policyState)
}

func (s *Server) policyState(ctx context.Context, in *nsObjectInput) (*policyStateOutput, error) {
	k, _ := kindByPlural("policies")
	obj, err := s.getObject(ctx, k, in.Namespace, in.Name)
	if err != nil {
		return nil, err
	}
	p, ok := obj.(*v1alpha1.Policy)
	if !ok {
		return nil, errNotFound(in.Namespace + "/" + in.Name)
	}
	ns := p.Namespace
	groups := map[string]*v1alpha1.ClientGroup{}
	lists := map[string]*v1alpha1.BlockList{}
	var pols []*v1alpha1.Policy
	for kind, fn := range map[store.Kind]func(store.Object){
		"ClientGroup": func(o store.Object) {
			if g, ok := o.(*v1alpha1.ClientGroup); ok {
				groups[g.Name] = g
			}
		},
		"BlockList": func(o store.Object) {
			if l, ok := o.(*v1alpha1.BlockList); ok {
				lists[l.Name] = l
			}
		},
		"Policy": func(o store.Object) {
			if q, ok := o.(*v1alpha1.Policy); ok {
				pols = append(pols, q)
			}
		},
	} {
		if err := s.listAll(ctx, kind, ns, fn); err != nil {
			return nil, err
		}
	}

	now := s.opts.Now()
	cfg := s.config()
	out := &PolicyState{
		Policy: ns + "/" + p.Name, Enabled: !v1alpha1.IsDisabled(p), Priority: p.Spec.Priority,
		ShadowedBy: policy.ShadowedBy(p, pols), ManagedBy: store.ManagedBy(p),
		Groups: []PolicyGroup{}, Lists: []PolicyList{}, Services: []PolicyService{},
		Categories: []PolicyCategory{}, AllowedServices: []PolicyService{}, EffectiveServices: []PolicyEffectiveService{},
		BlockMode: string(p.Spec.BlockMode), Conditions: append([]metav1.Condition{}, p.Status.Conditions...),
	}
	out.Active = out.Enabled
	if p.Spec.Schedule != nil {
		st, err := policy.DescribeSchedule(p.Spec.Schedule, now)
		sch := &PolicySchedule{Now: st.Active, Summary: st.Summary}
		switch {
		case err != nil:
			sch.Now, sch.Summary, sch.Error = true, "invalid schedule", err.Error()
		case !st.NextChange.IsZero():
			sch.NextChange = &st.NextChange
		}
		out.Schedule = sch
		out.Active = out.Enabled && sch.Now
	}
	if out.BlockMode == "" && cfg != nil {
		out.BlockMode = cfg.Blocking.DefaultResponse
	}
	if d := p.Spec.BlockTTL.Duration; d > 0 {
		out.BlockTTL = d.String()
	}

	var seen map[string]int
	r := s.opts.Querylog
	if r != nil {
		if seen, err = r.ClientsSeenByGroup(ctx, now.Add(-24*time.Hour)); err != nil {
			s.log.Warn("policy state: clients by group", "err", err)
			seen = nil
		}
	}
	for _, ref := range p.Spec.ClientGroupRefs {
		g := PolicyGroup{Ref: ref}
		if cg := groups[ref]; cg != nil {
			g.Exists, g.Enabled, g.Clients = true, !v1alpha1.IsDisabled(cg), len(cg.Spec.Clients)
		}
		if seen != nil {
			n := seen[ns+"/"+ref]
			g.Seen24h = &n
		}
		out.Groups = append(out.Groups, g)
	}
	for _, ref := range []struct {
		role string
		refs []string
	}{{"block", p.Spec.BlockListRefs}, {"allow", p.Spec.AllowListRefs}} {
		for _, name := range ref.refs {
			out.Lists = append(out.Lists, policyListState(name, ref.role, lists[name]))
		}
	}

	cat := s.catalogue()
	policyService := func(id string) PolicyService {
		ps := PolicyService{ID: id, Name: id}
		if svc, ok := cat.Service(id); ok {
			ps.Name, ps.Known, ps.Rules, ps.IconRef = svc.Name, true, len(svc.Rules), iconRef(svc.IconSlug())
		} else {
			ps.IconRef = iconRef(catalogueIconFor(id))
		}
		return ps
	}
	for _, id := range p.Spec.BlockedServices {
		out.Services = append(out.Services, policyService(id))
	}
	allowed := map[string]bool{}
	for _, id := range p.Spec.AllowedServices {
		allowed[id] = true
		out.AllowedServices = append(out.AllowedServices, policyService(id))
	}
	for _, id := range p.Spec.BlockedCategories {
		pc := PolicyCategory{ID: id, Name: id}
		if k, ok := cat.Category(id); ok {
			ids, _ := cat.CategoryServices(id)
			pc.Name, pc.Known, pc.Services, pc.Icon = k.Name, true, len(ids), k.Icon
			for _, sid := range ids {
				if allowed[sid] {
					pc.Allowed++
				}
			}
		}
		out.Categories = append(out.Categories, pc)
	}
	explicit := map[string]bool{}
	for _, id := range p.Spec.BlockedServices {
		explicit[id] = true
	}
	ids, _ := policy.EffectiveServices(&p.Spec, cat)
	for _, id := range ids {
		ps := policyService(id)
		es := PolicyEffectiveService{ID: id, Name: ps.Name, Known: ps.Known, Rules: ps.Rules, Via: "category", Origin: catalogue.OriginKudurru, IconRef: ps.IconRef}
		if explicit[id] {
			es.Via = "service"
		}
		if svc, ok := cat.Service(id); ok {
			es.Category, es.Origin = svc.Category, svc.Origin()
		}
		out.EffectiveServices = append(out.EffectiveServices, es)
	}
	out.SafeSearch = PolicySafeSearch{Enabled: p.Spec.SafeSearch != nil && *p.Spec.SafeSearch, Mode: catalogue.ModeStrict, Engines: []PolicySafeSearchEngine{}}
	if p.Spec.SafeSearchMode == catalogue.ModeModerate {
		out.SafeSearch.Mode = catalogue.ModeModerate
	}
	for i := range cat.SafeSearch {
		e := &cat.SafeSearch[i]
		out.SafeSearch.Engines = append(out.SafeSearch.Engines, PolicySafeSearchEngine{ID: e.ID, Name: e.Name, IconRef: iconRef(e.IconSlug())})
	}
	if g := p.Spec.UpstreamGroup; g != "" {
		ug := &PolicyUpstreamGroup{Name: g}
		if cfg != nil {
			_, ug.Exists = cfg.Upstreams.Groups[g]
		}
		out.UpstreamGroup = ug
	}

	if r != nil {
		st, err := r.PolicyStats(ctx, out.Policy, sqlitelog.Window{From: now.Add(-24 * time.Hour), Until: now}, 10)
		if err != nil {
			return nil, readerError(err)
		}
		out.Stats24h = &PolicyStats24h{Queries: st.Queries, Blocked: st.Blocked, TopBlocked: st.TopBlocked, TopClients: st.TopClients}
		if st.Queries > 0 {
			out.Stats24h.BlockedPercent = math.Round(float64(st.Blocked)*1000/float64(st.Queries)) / 10
		}
	}
	return &policyStateOutput{Body: out}, nil
}

// catalogueIconFor resolves the icon of an ID the catalogue does not know
// (a service from a newer catalogue): the icon named like the ID, if any.
func catalogueIconFor(id string) string {
	if catalogue.HasIcon(id) {
		return id
	}
	return ""
}

// policyListState describes one list reference; bl is nil when the list
// does not exist.
func policyListState(ref, role string, bl *v1alpha1.BlockList) PolicyList {
	l := PolicyList{Ref: ref, Role: role, Mode: role}
	if bl == nil {
		return l
	}
	l.Exists, l.Enabled, l.Entries, l.SourcesTotal = true, !v1alpha1.IsDisabled(bl), bl.Status.Entries, len(bl.Spec.Sources)
	if bl.Spec.Mode != "" {
		l.Mode = string(bl.Spec.Mode)
	} else {
		l.Mode = string(v1alpha1ListModeBlock)
	}
	for i := range bl.Spec.Sources {
		if bl.Spec.Sources[i].IsEnabled() {
			l.SourcesEnabled++
		}
	}
	l.Ready = l.Enabled && meta.IsStatusConditionTrue(bl.Status.Conditions, v1alpha1.ConditionReady)
	return l
}
