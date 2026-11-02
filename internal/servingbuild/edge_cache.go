package servingbuild

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/rpz"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingzones"
	"github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// EdgeCache projects complete serving identity, never desired settings or a
// content digest sampled separately from the blocking/policy/list owners.
type EdgeCache struct {
	publisher      *serving.Publisher
	node, instance string
	now            func() time.Time
}

// NewEdgeCache binds one complete publisher and captured local node identity.
func NewEdgeCache(p *serving.Publisher, node string, now func() time.Time) (*EdgeCache, error) {
	if p == nil || node == "" || len(node) > 256 {
		return nil, serving.ErrComponents
	}
	var instance [32]byte
	if _, err := rand.Read(instance[:]); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &EdgeCache{publisher: p, node: node, instance: hex.EncodeToString(instance[:]), now: now}, nil
}

// State retains one protected original lease throughout projection and final
// currency checking. It never repairs a failed original with a fresh capture.
func (o *EdgeCache) State(ctx context.Context) (*internalapi.ResolverState, error) {
	if o == nil || o.publisher == nil || ctx == nil {
		return nil, serving.ErrComponents
	}
	lease, err := o.publisher.CaptureHTTP(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	state, err := o.project(lease, o.now())
	if err != nil {
		return nil, err
	}
	if err := o.publisher.CheckReadLease(ctx, lease); err != nil {
		return nil, err
	}
	return state, nil
}

// Zones retains the SAME protected original lease for serials and cache proof.
func (o *EdgeCache) Zones(ctx context.Context) (*internalapi.ZoneList, error) {
	if o == nil || o.publisher == nil || ctx == nil {
		return nil, serving.ErrComponents
	}
	lease, err := o.publisher.CaptureHTTP(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	now := o.now()
	state, err := o.project(lease, now)
	if err != nil {
		return nil, err
	}
	b := lease.Bundle()
	objectsValue, _ := b.Component("objects")
	objects, _ := objectsValue.(*zonecache.Snapshot)
	runtimeValue, _ := b.Component("runtime")
	runtime, _ := runtimeValue.(*RuntimeOwner)
	zonesValue, _ := b.Component("zones")
	zones, _ := zonesValue.(*servingzones.Prepared)
	if err := diagnosticOwners(objects, runtime, zones, b.Identity()); err != nil {
		return nil, err
	}
	cut := DiagnosticCut{lease: lease, view: diagnosticView{objects: objects.Objects, runtime: runtime, now: func() time.Time { return now }, check: func() error { return o.publisher.CheckReadLease(ctx, lease) }}}
	served, err := cut.Zones()
	if err != nil {
		return nil, err
	}
	result := &internalapi.ZoneList{Node: o.node, Resolver: state, Zones: []internalapi.Zone{}}
	for _, z := range served {
		if z.Available && (z.Type == public.ZoneTypePrimary || z.Type == public.ZoneTypeSecondary) {
			result.Zones = append(result.Zones, internalapi.Zone{Name: z.Apex, Serial: z.Serial, Type: string(z.Type)})
		}
	}
	if err := o.publisher.CheckReadLease(ctx, lease); err != nil {
		return nil, err
	}
	return result, nil
}

func (o *EdgeCache) project(lease *serving.Lease, now time.Time) (*internalapi.ResolverState, error) {
	if now.IsZero() || lease == nil || lease.Bundle() == nil {
		return nil, serving.ErrReleased
	}
	bundle := lease.Bundle()
	value, _ := bundle.Component("blocking")
	view, ok := value.(*blocking.View)
	if !ok || view == nil {
		return nil, serving.ErrComponents
	}
	_, enabled, disabledUntil, err := view.StateAt(now)
	if err != nil {
		return nil, err
	}
	value, _ = bundle.Component("edge-cache")
	timeline, ok := value.(*cacheTimeline)
	if !ok || timeline == nil {
		return nil, serving.ErrComponents
	}
	phase, boundary, available := timeline.at(now)
	until := now.Add(internalapi.ResolverLease)
	if !boundary.IsZero() && boundary.Before(until) {
		until = boundary
	}
	if disabledUntil.After(now) && disabledUntil.Before(blocking.Forever) && disabledUntil.Before(until) {
		until = disabledUntil
	}
	data, err := json.Marshal(bundle.Identity())
	if err != nil {
		return nil, serving.ErrComponents
	}
	digest := sha256.Sum256(data)
	return &internalapi.ResolverState{Version: 1, Node: o.node, Instance: o.instance, Publication: bundle.Identity().Publication, Identity: hex.EncodeToString(digest[:]), Phase: phase, ObservedAt: now.UTC(), ValidUntil: until.UTC(), BlockingEnabled: enabled, DisabledUntil: disabledUntil, Available: available, UnavailableReason: timeline.reason}, nil
}

// cacheTimeline contains only detached original time boundaries. Compilation
// happens once per candidate, outside publication locks, not on every pull.
type cacheTimeline struct {
	deadlines []time.Time
	scheduled bool
	rpz       []*rpz.Selection
	reason    string
}

func newCacheTimeline(owners *semanticOwners, inventory *InventoryOwner, zones *servingzones.Prepared) (*cacheTimeline, error) {
	t := &cacheTimeline{reason: sharedCacheBypass(owners)}
	for _, p := range owners.objects.Objects.Policies() {
		if p.Spec.Schedule != nil {
			t.scheduled = true
		}
	}
	for _, c := range inventory.Snapshot.Clients() {
		if !c.ExpiresAt.IsZero() {
			t.deadlines = append(t.deadlines, c.ExpiresAt)
		}
	}
	for _, g := range inventory.MACGrants {
		if !g.ExpiresAt.IsZero() {
			t.deadlines = append(t.deadlines, g.ExpiresAt)
		}
	}
	if len(t.deadlines) > 200000 {
		return nil, serving.ErrComponents
	}
	slices.SortFunc(t.deadlines, func(a, b time.Time) int { return a.Compare(b) })
	t.deadlines = slices.CompactFunc(t.deadlines, func(a, b time.Time) bool { return a.Equal(b) })
	var names []string
	for _, list := range owners.objects.Objects.BlockLists() {
		names = append(names, list.Namespace+"/"+list.Name)
		if list.Namespace == "" {
			names[len(names)-1] = list.Name
		}
	}
	slices.Sort(names)
	for start := 0; start < len(names) || start == 0; start += 4096 {
		end := min(start+4096, len(names))
		selection, err := zones.SelectRPZ(names[start:end])
		if err != nil {
			return nil, err
		}
		t.rpz = append(t.rpz, selection)
		if end == len(names) {
			break
		}
	}
	return t, nil
}

func (t *cacheTimeline) at(now time.Time) (string, time.Time, bool) {
	i := sort.Search(len(t.deadlines), func(i int) bool { return t.deadlines[i].After(now) })
	var until time.Time
	available := t.reason == ""
	for _, selection := range t.rpz {
		boundary, ok := selection.CacheUntil(now)
		available = available && ok
		if !boundary.IsZero() && (until.IsZero() || boundary.Before(until)) {
			until = boundary
		}
	}
	if i < len(t.deadlines) && (until.IsZero() || t.deadlines[i].Before(until)) {
		until = t.deadlines[i]
	}
	var minute int64
	if t.scheduled {
		minute = now.Unix() / 60
		next := now.Truncate(time.Minute).Add(time.Minute)
		if until.IsZero() || next.Before(until) {
			until = next
		}
	}
	return fmt.Sprintf("%d:%d", i, minute), until, available
}

// Shared dnsdist packet caches carry no exact original client/identity key.
// These configured modes may change a verdict between clients or between two
// requests in one publication. A finite publication lease cannot repair that.
func sharedCacheBypass(owners *semanticOwners) string {
	if owners == nil || owners.config == nil || owners.objects == nil {
		return "input-unavailable"
	}
	cfg := owners.config
	switch {
	case cfg.ClientNames.ReversePTR.Enabled:
		return "async-ptr"
	case cfg.DHCP.Enabled || len(cfg.DHCP.Scopes) != 0:
		return "dhcp-runtime"
	case !owners.policy.ClientIndependent():
		return "client-scope"
	case !owners.objects.Objects.Lists().ClientIndependent():
		return "rule-client-scope"
	case !zoneQueriesClientIndependent(owners.objects.Objects.Zones()):
		return "zone-client-acl"
	case len(cfg.Control.AllowedNetworks) != 0:
		return "client-acl"
	case cfg.Upstreams.ECS.Mode != "" && cfg.Upstreams.ECS.Mode != "strip":
		return "ecs"
	case cfg.Response.Rebinding.Enabled && len(cfg.Response.Rebinding.BypassNetworks) != 0:
		return "rebinding-client-scope"
	case cfg.RateLimit.Enabled:
		return "rate-limit"
	default:
		return ""
	}
}

func zoneQueriesClientIndependent(zones []*public.DNSZone) bool {
	for _, z := range zones {
		// Forward and stub routes use the resolver pool. Include other local
		// zones conservatively because CNAME/fallback paths can cross them.
		// Transfer/notify keys authorize different operations, not queries.
		if z == nil || len(z.Spec.Policy.QueryACL) != 0 {
			return false
		}
		switch z.Spec.Policy.QueryAccess {
		case "", public.AccessAllow, public.AccessDeny:
		default:
			return false
		}
	}
	return true
}
